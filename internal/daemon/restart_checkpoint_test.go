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
	d.checkpointInFlightForRestart([]string{stateDir})

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
	d.checkpointInFlightForRestart([]string{stateDir})

	reloaded, err := state.Load(stateDir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	got := reloaded.Sessions["sup-311"]
	if got.RestartCheckpointAt == nil || !got.RestartCheckpointAt.Equal(earlier) {
		t.Fatalf("marker re-stamped: got %v, want unchanged %v", got.RestartCheckpointAt, earlier)
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
	d.checkpointInFlightForRestart([]string{stateDir})

	reloaded, err := state.Load(stateDir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := reloaded.Sessions["sup-312"]; got.RestartCheckpointAt != nil {
		t.Fatal("a session with a missing worktree must not be marked for in-place resume")
	}
}
