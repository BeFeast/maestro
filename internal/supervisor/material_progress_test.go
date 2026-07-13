package supervisor

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/progress"
	"github.com/befeast/maestro/internal/state"
)

func TestCollectMaterialProgressSignals_PreDeliveryLiveWorker(t *testing.T) {
	st := state.NewState()
	st.Sessions["s1"] = &state.Session{
		IssueNumber:    10,
		Status:         state.StatusRunning,
		PID:            4242,
		TmuxSession:    "mae-10",
		Branch:         "feat/x",
		LastOutputHash: "hash-a",
	}
	now := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)

	set, phase := collectMaterialProgressSignals(st, now)
	if phase != progress.PhasePreDelivery {
		t.Fatalf("phase = %q, want pre_delivery", phase)
	}
	present := set.Present()
	if len(present) == 0 {
		t.Fatalf("no signals collected for a live worker")
	}
	// Process/tmux and terminal signals must be present for a live worker.
	kinds := map[progress.SignalKind]bool{}
	for _, s := range present {
		kinds[s.Kind] = true
	}
	if !kinds[progress.SignalProcessTmux] {
		t.Errorf("process/tmux signal missing")
	}
	if !kinds[progress.SignalTerminalCheckpoint] {
		t.Errorf("terminal/checkpoint signal missing")
	}
}

// Advancing any single field changes the combined identity (progress);
// an unchanged snapshot keeps it (candidate stall).
func TestCollectMaterialProgressSignals_Sensitivity(t *testing.T) {
	now := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
	mk := func(hash string) progress.SignalSet {
		st := state.NewState()
		st.Sessions["s1"] = &state.Session{IssueNumber: 1, Status: state.StatusRunning, PID: 1, TmuxSession: "t", LastOutputHash: hash}
		set, _ := collectMaterialProgressSignals(st, now)
		return set
	}
	if mk("a").Combined() == mk("b").Combined() {
		t.Fatalf("advancing terminal hash did not change combined identity")
	}
	if mk("a").Combined() != mk("a").Combined() {
		t.Fatalf("identical state produced unstable identity")
	}
}

// A dead-only project (no live worker) is not a pre-delivery worker stall:
// there is nothing to retry, so the phase is delivery_pending.
func TestCollectMaterialProgressSignals_NoLiveWorker(t *testing.T) {
	st := state.NewState()
	st.Sessions["s1"] = &state.Session{IssueNumber: 3, Status: state.StatusDone, PRNumber: 7}
	now := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)

	_, phase := collectMaterialProgressSignals(st, now)
	if phase != progress.PhaseDeliveryPending {
		t.Fatalf("phase = %q, want delivery_pending (no live worker)", phase)
	}
}

// A quiet-but-active worker (git/branch advances while terminal output is
// frozen) never trips the watchdog across many evaluation cycles.
func TestRecordMaterialProgress_QuietButActiveThroughDeadline(t *testing.T) {
	st := state.NewState()
	budget := 20 * time.Minute
	now := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
	sess := &state.Session{IssueNumber: 5, Status: state.StatusRunning, PID: 9, TmuxSession: "mae-5", LastOutputHash: "frozen"}
	st.Sessions["s1"] = sess

	for i := 0; i < 8; i++ {
		now = now.Add(30 * time.Minute)
		// Terminal output frozen; the worker keeps committing (PR head moves).
		sess.PRNumber = 100 + i
		observed, phase := collectMaterialProgressSignals(st, now)
		dec := st.RecordMaterialProgress(observed, phase, budget, time.Minute, now)
		if dec.Acted() {
			t.Fatalf("cycle %d: watchdog acted on an actively-progressing worker: %q", i, dec.Action)
		}
	}
}

// A genuinely frozen pre-delivery worker trips the watchdog after the budget,
// and the decision is durable across a Save/Load restart with a stable deadline.
func TestRecordMaterialProgress_FrozenWorkerStallSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	st := state.NewState()
	budget := 20 * time.Minute
	start := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
	st.Sessions["s1"] = &state.Session{IssueNumber: 5, Status: state.StatusRunning, PID: 9, TmuxSession: "mae-5", LastOutputHash: "frozen", Branch: "feat/x"}

	// First evaluation establishes the watermark.
	observed, phase := collectMaterialProgressSignals(st, start)
	st.RecordMaterialProgress(observed, phase, budget, time.Minute, start)
	deadline := st.MaterialProgress.Deadline()

	if err := state.Save(dir, st); err != nil {
		t.Fatalf("Save: %v", err)
	}
	reloaded, err := state.Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := reloaded.MaterialProgress.Deadline(); !got.Equal(deadline) {
		t.Fatalf("deadline after restart = %s, want %s", got, deadline)
	}

	// Same frozen worker, past the deadline → stop_and_retry recovery.
	past := start.Add(budget + time.Minute)
	observed, phase = collectMaterialProgressSignals(reloaded, past)
	dec := reloaded.RecordMaterialProgress(observed, phase, budget, time.Minute, past)
	if dec.Action != progress.ActionStopAndRetry {
		t.Fatalf("frozen worker past deadline action = %q, want stop_and_retry", dec.Action)
	}
	if !dec.Deadline.Equal(deadline) {
		t.Fatalf("deadline drifted across restart: got %s, want %s", dec.Deadline, deadline)
	}
}

// A live worker that only edits files in its worktree (no terminal output, no
// branch/PR change, no commit) must keep advancing the watermark via bounded
// filesystem evidence, so the watchdog never records a false stall against
// ongoing work (#887 review: worktree edits must not be invisible).
func TestCollectMaterialProgressSignals_QuietFileEditsAdvanceWatermark(t *testing.T) {
	wt := t.TempDir()
	if err := os.MkdirAll(filepath.Join(wt, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(wt, ".git", "HEAD"), []byte("ref: refs/heads/feat/x\n"), 0o644)
	_ = os.WriteFile(filepath.Join(wt, ".git", "index"), []byte("idx"), 0o644)
	src := filepath.Join(wt, "main.go")
	if err := os.WriteFile(src, []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	st := state.NewState()
	// Frozen terminal output, no branch, no PR — only file edits distinguish
	// the worker from a genuine stall.
	st.Sessions["s1"] = &state.Session{IssueNumber: 7, Status: state.StatusRunning, PID: 1, TmuxSession: "mae-7", LastOutputHash: "frozen", Worktree: wt}

	budget := 20 * time.Minute
	now := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
	mt := now
	if err := os.Chtimes(src, mt, mt); err != nil {
		t.Fatal(err)
	}
	observed, phase := collectMaterialProgressSignals(st, now)
	st.RecordMaterialProgress(observed, phase, budget, time.Minute, now)

	for i := 1; i <= 3; i++ {
		now = now.Add(30 * time.Minute) // each cycle is past the 20m budget
		mt = mt.Add(30 * time.Minute)
		if err := os.Chtimes(src, mt, mt); err != nil { // the worker edited a file
			t.Fatal(err)
		}
		observed, phase = collectMaterialProgressSignals(st, now)
		dec := st.RecordMaterialProgress(observed, phase, budget, time.Minute, now)
		if dec.Acted() {
			t.Fatalf("cycle %d: watchdog acted on a worker still editing files: %q", i, dec.Action)
		}
	}
}

// The worktree probe must not blind the watchdog to a genuine stall: a live
// worker whose worktree stops changing (no file edits) still trips the watchdog
// after the budget.
func TestCollectMaterialProgressSignals_IdleWorktreeStillStalls(t *testing.T) {
	wt := t.TempDir()
	src := filepath.Join(wt, "main.go")
	if err := os.WriteFile(src, []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	frozenMtime := time.Date(2026, 7, 13, 11, 0, 0, 0, time.UTC)
	if err := os.Chtimes(src, frozenMtime, frozenMtime); err != nil {
		t.Fatal(err)
	}

	st := state.NewState()
	st.Sessions["s1"] = &state.Session{IssueNumber: 7, Status: state.StatusRunning, PID: 1, TmuxSession: "mae-7", LastOutputHash: "frozen", Worktree: wt, Branch: "feat/x"}
	budget := 20 * time.Minute
	now := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
	observed, phase := collectMaterialProgressSignals(st, now)
	st.RecordMaterialProgress(observed, phase, budget, time.Minute, now)

	now = now.Add(budget + time.Minute)
	observed, phase = collectMaterialProgressSignals(st, now)
	dec := st.RecordMaterialProgress(observed, phase, budget, time.Minute, now)
	if dec.Action != progress.ActionStopAndRetry {
		t.Fatalf("truly idle worktree should still stall: got %q", dec.Action)
	}
}

// The supervisor must honor the watchdog's own evaluation cadence: a fast
// supervisor loop that cycles inside the interval must not re-evaluate the
// watchdog every cycle (#887 review: evaluation cadence must not be ignored).
func TestRecordMaterialProgress_RespectsEvalCadence(t *testing.T) {
	cfg := &config.Config{}
	cfg.StalledProgressWatchdog.EvalIntervalSecs = 60 // 1-minute watchdog cadence
	st := state.NewState()
	st.Sessions["s1"] = &state.Session{IssueNumber: 1, Status: state.StatusRunning, PID: 1, TmuxSession: "t", LastOutputHash: "a"}

	t0 := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
	recordMaterialProgress(cfg, st, t0)
	if st.MaterialProgress == nil {
		t.Fatal("first evaluation recorded no watermark")
	}
	firstEval := st.MaterialProgress.LastEvaluatedAt

	// A second supervisor cycle 10s later is inside the 60s cadence → skip.
	recordMaterialProgress(cfg, st, t0.Add(10*time.Second))
	if !st.MaterialProgress.LastEvaluatedAt.Equal(firstEval) {
		t.Fatalf("watchdog re-evaluated inside its cadence: last eval moved to %s", st.MaterialProgress.LastEvaluatedAt)
	}

	// A cycle past the cadence re-evaluates.
	recordMaterialProgress(cfg, st, t0.Add(90*time.Second))
	if st.MaterialProgress.LastEvaluatedAt.Equal(firstEval) {
		t.Fatalf("watchdog failed to re-evaluate after its cadence elapsed")
	}
}
