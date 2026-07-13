package daemon

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/befeast/maestro/internal/state"
)

// #877: at shutdown the daemon must checkpoint every worker still in-flight so
// the next daemon resumes the same session in place instead of a false
// running->dead. This exercises checkpointInFlightForRestart against a real
// state dir + on-disk worktree: the running worker is stamped with a resumable
// marker and a CHECKPOINT.md, while terminal sessions are left untouched.
func TestCheckpointInFlightForRestart_MarksRunningWorkers(t *testing.T) {
	stateDir := t.TempDir()
	worktree := t.TempDir() // dirty worktree that survives the restart

	s := state.NewState()
	s.Sessions["sup-310"] = &state.Session{
		IssueNumber: 310,
		Status:      state.StatusRunning,
		Worktree:    worktree,
		Branch:      "feat/sup-310",
	}
	s.Sessions["done-1"] = &state.Session{IssueNumber: 9, Status: state.StatusDone, Worktree: worktree}
	if err := state.Save(stateDir, s); err != nil {
		t.Fatalf("save: %v", err)
	}

	d := &Daemon{}
	d.checkpointInFlightForRestart(time.Now().Add(time.Hour), []string{stateDir})

	reloaded, err := state.Load(stateDir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	running := reloaded.Sessions["sup-310"]
	if running.RestartCheckpointAt == nil {
		t.Fatal("running worker was not stamped with RestartCheckpointAt")
	}
	if running.Status != state.StatusRunning {
		t.Fatalf("checkpoint must not change status: got %q", running.Status)
	}
	if running.CheckpointFile == "" {
		t.Fatal("CheckpointFile should point at the written CHECKPOINT.md")
	}
	if _, err := os.Stat(filepath.Join(worktree, "CHECKPOINT.md")); err != nil {
		t.Fatalf("CHECKPOINT.md not written to the worktree: %v", err)
	}
	if done := reloaded.Sessions["done-1"]; done.RestartCheckpointAt != nil {
		t.Fatal("a terminal (done) session must not be checkpointed for resume")
	}
}

// A second shutdown pass (both the ctx.Done and fleetErr teardown paths can
// run) must be idempotent: an already-stamped session keeps its original marker
// timestamp rather than being re-checkpointed.
func TestCheckpointInFlightForRestart_Idempotent(t *testing.T) {
	stateDir := t.TempDir()
	worktree := t.TempDir()
	earlier := time.Date(2026, 7, 12, 18, 0, 0, 0, time.UTC)

	s := state.NewState()
	s.Sessions["sup-311"] = &state.Session{
		IssueNumber:         311,
		Status:              state.StatusRunning,
		Worktree:            worktree,
		RestartCheckpointAt: &earlier,
	}
	if err := state.Save(stateDir, s); err != nil {
		t.Fatalf("save: %v", err)
	}

	d := &Daemon{}
	d.checkpointInFlightForRestart(time.Now().Add(time.Hour), []string{stateDir})

	reloaded, err := state.Load(stateDir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	got := reloaded.Sessions["sup-311"]
	if got.RestartCheckpointAt == nil || !got.RestartCheckpointAt.Equal(earlier) {
		t.Fatalf("marker re-stamped: got %v, want unchanged %v", got.RestartCheckpointAt, earlier)
	}
}

// #877 review comment 1: the default drain can consume most of the unit's stop
// deadline, leaving little time for this pass. The correctness-critical marker
// (a single field + state write) must therefore always be stamped, while the
// git-heavy CHECKPOINT.md context blob is best-effort and dropped once the
// budget is spent — so a marked worker is never left unmarked at SIGKILL.
func TestCheckpointInFlightForRestart_PastDeadlineStampsMarkerWithoutCheckpointFile(t *testing.T) {
	stateDir := t.TempDir()
	worktree := t.TempDir()

	s := state.NewState()
	s.Sessions["sup-310"] = &state.Session{
		IssueNumber: 310,
		Status:      state.StatusRunning,
		Worktree:    worktree,
		Branch:      "feat/sup-310",
	}
	if err := state.Save(stateDir, s); err != nil {
		t.Fatalf("save: %v", err)
	}

	d := &Daemon{}
	// Deadline already elapsed: skip CHECKPOINT.md, still stamp the marker.
	d.checkpointInFlightForRestart(time.Now().Add(-time.Second), []string{stateDir})

	reloaded, err := state.Load(stateDir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	got := reloaded.Sessions["sup-310"]
	if got.RestartCheckpointAt == nil {
		t.Fatal("marker must be stamped even when the checkpoint budget is spent")
	}
	if got.CheckpointFile != "" {
		t.Fatalf("CHECKPOINT.md must be skipped past the budget, got CheckpointFile=%q", got.CheckpointFile)
	}
	if _, statErr := os.Stat(filepath.Join(worktree, "CHECKPOINT.md")); !os.IsNotExist(statErr) {
		t.Fatalf("no CHECKPOINT.md should be written past the budget: %v", statErr)
	}
}

// #877 review comment 2: stopAll abandons a flow whose cycle outruns its stop
// timeout, so that still-live flow can Save concurrently with this pass. The
// pass's 3-way-merged save must let the two writes COEXIST — a concurrent flow
// save to a DIFFERENT session must neither drop this pass's marker nor be
// clobbered by it.
func TestCheckpointInFlightForRestart_CoexistsWithConcurrentFlowSave(t *testing.T) {
	stateDir := t.TempDir()
	worktree := t.TempDir()

	s := state.NewState()
	s.Sessions["sup-310"] = &state.Session{IssueNumber: 310, Status: state.StatusRunning, Worktree: worktree}
	// A second in-flight worker with no worktree: the checkpoint pass skips it, so
	// only the flow (below) mutates it — modelling a concurrent per-session write.
	s.Sessions["sup-311"] = &state.Session{IssueNumber: 311, Status: state.StatusRunning}
	if err := state.Save(stateDir, s); err != nil {
		t.Fatalf("save: %v", err)
	}

	// A flow loaded state before the pass and is about to advance sup-311.
	flow, err := state.Load(stateDir)
	if err != nil {
		t.Fatalf("load flow: %v", err)
	}
	flow.Sessions["sup-311"].Status = state.StatusPROpen

	d := &Daemon{}
	d.checkpointInFlightForRestart(time.Now().Add(time.Hour), []string{stateDir})

	// The timed-out flow saves AFTER the pass stamped sup-310's marker.
	if err := state.Save(stateDir, flow); err != nil {
		t.Fatalf("concurrent flow save must merge cleanly, got %v", err)
	}

	reloaded, err := state.Load(stateDir)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if reloaded.Sessions["sup-310"].RestartCheckpointAt == nil {
		t.Fatal("the concurrent flow save dropped the restart-checkpoint marker on sup-310")
	}
	if got := reloaded.Sessions["sup-311"].Status; got != state.StatusPROpen {
		t.Fatalf("the checkpoint pass clobbered the flow's newer sup-311 state: status=%q, want pr_open", got)
	}
}

// #877 review comment 2 (same-session): if a timed-out flow already moved the
// checkpoint's target session to a terminal state and saved, a later pass must
// YIELD — it must not re-stamp a resumable marker on a session the flow finished.
func TestCheckpointInFlightForRestart_YieldsToTerminalSession(t *testing.T) {
	stateDir := t.TempDir()
	worktree := t.TempDir()

	s := state.NewState()
	s.Sessions["sup-310"] = &state.Session{IssueNumber: 310, Status: state.StatusRunning, Worktree: worktree}
	if err := state.Save(stateDir, s); err != nil {
		t.Fatalf("save: %v", err)
	}

	// The flow finished sup-310 (opened a PR) and persisted it before the pass.
	flow, err := state.Load(stateDir)
	if err != nil {
		t.Fatalf("load flow: %v", err)
	}
	flow.Sessions["sup-310"].Status = state.StatusPROpen
	if err := state.Save(stateDir, flow); err != nil {
		t.Fatalf("flow save: %v", err)
	}

	d := &Daemon{}
	d.checkpointInFlightForRestart(time.Now().Add(time.Hour), []string{stateDir})

	reloaded, err := state.Load(stateDir)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if reloaded.Sessions["sup-310"].RestartCheckpointAt != nil {
		t.Fatal("checkpoint must not stamp a marker on a session the flow already moved to a terminal state")
	}
}

// A running session whose worktree no longer exists cannot be resumed in place,
// so it is left unmarked for the normal reconcile/retry path.
func TestCheckpointInFlightForRestart_SkipsMissingWorktree(t *testing.T) {
	stateDir := t.TempDir()

	s := state.NewState()
	s.Sessions["sup-312"] = &state.Session{
		IssueNumber: 312,
		Status:      state.StatusRunning,
		Worktree:    filepath.Join(t.TempDir(), "gone"),
	}
	if err := state.Save(stateDir, s); err != nil {
		t.Fatalf("save: %v", err)
	}

	d := &Daemon{}
	d.checkpointInFlightForRestart(time.Now().Add(time.Hour), []string{stateDir})

	reloaded, err := state.Load(stateDir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := reloaded.Sessions["sup-312"]; got.RestartCheckpointAt != nil {
		t.Fatal("a session with a missing worktree must not be marked for in-place resume")
	}
}
