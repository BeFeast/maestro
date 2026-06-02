package orchestrator

import (
	"testing"
	"time"

	"github.com/befeast/maestro/internal/github"
	"github.com/befeast/maestro/internal/state"
)

// TestReconcileRecordsWorkerEndedAndPROpened verifies that a session moving
// running -> pr_open via the reconcile path captures the agent-exit moment
// (#426). Without WorkerEndedAt + PROpenedAt the API cannot distinguish
// agent runtime from PR/CI waiting once `code_landed` / `done` rewrite
// FinishedAt.
func TestReconcileRecordsWorkerEndedAndPROpened(t *testing.T) {
	s := state.NewState()
	s.Sessions["mae-1"] = &state.Session{
		IssueNumber: 426,
		IssueTitle:  "runtime breakdown",
		Status:      state.StatusRunning,
		PID:         4242,
		TmuxSession: "maestro-mae-1",
		Branch:      "feat/runtime-breakdown",
		StartedAt:   time.Now().UTC().Add(-3 * time.Minute),
	}

	openPRs := []github.PR{{Number: 426, HeadRefName: "feat/runtime-breakdown"}}

	o := &Orchestrator{
		pidAliveFn:          func(pid int) bool { return false },
		tmuxSessionExistsFn: func(name string) bool { return false },
		listOpenPRsFn:       func() ([]github.PR, error) { return openPRs, nil },
	}

	if !o.reconcileRunningSessions(s) {
		t.Fatal("expected reconciliation to report changes")
	}

	sess := s.Sessions["mae-1"]
	if sess.Status != state.StatusPROpen {
		t.Fatalf("status = %q, want pr_open", sess.Status)
	}
	if sess.WorkerEndedAt == nil {
		t.Fatal("WorkerEndedAt must be set when worker exits — without it, agent runtime is unrecoverable once FinishedAt is rewritten")
	}
	if sess.PROpenedAt == nil {
		t.Fatal("PROpenedAt must be set on first transition into pr_open")
	}
	if sess.FinishedAt == nil {
		t.Fatal("FinishedAt must also be set (workflow-level timestamp)")
	}
	if !sess.WorkerEndedAt.Equal(*sess.PROpenedAt) {
		t.Errorf("worker_ended_at=%v differs from pr_opened_at=%v on the same reconcile pass; expected the same `now` to be used",
			*sess.WorkerEndedAt, *sess.PROpenedAt)
	}
}

// TestPROpenToDoneDoesNotMoveWorkerEndedAt mirrors the scribe-service scenario
// from #426: the worker exits quickly, the session sits in pr_open for over an
// hour waiting on CI/Greptile/merge, and is finally marked done by a later
// orchestrator pass. The dashboard must still be able to tell that the agent
// only ran for the early part — WorkerEndedAt must not slide to the
// done-transition time.
func TestPROpenToDoneDoesNotMoveWorkerEndedAt(t *testing.T) {
	startedAt := time.Date(2026, 5, 21, 14, 57, 24, 0, time.UTC)
	workerEnded := startedAt.Add(3 * time.Minute)

	sess := &state.Session{
		IssueNumber:   106,
		Status:        state.StatusPROpen,
		Branch:        "feat/scr-46",
		StartedAt:     startedAt,
		WorkerEndedAt: &workerEnded,
		PROpenedAt:    &workerEnded,
		FinishedAt:    &workerEnded,
		PRNumber:      115,
	}

	// Simulate the markDoneAfterOutcomePass / markCodeLanded path setting
	// FinishedAt a second time (much later) AND calling MarkWorkerEnded
	// (idempotently) — WorkerEndedAt must remain pinned to the first call.
	doneAt := time.Date(2026, 5, 21, 16, 43, 27, 0, time.UTC)
	sess.FinishedAt = &doneAt
	state.MarkWorkerEnded(sess, doneAt)
	sess.Status = state.StatusDone

	if sess.WorkerEndedAt == nil {
		t.Fatal("WorkerEndedAt unexpectedly cleared")
	}
	if !sess.WorkerEndedAt.Equal(workerEnded) {
		t.Errorf("WorkerEndedAt slid to %v; want pinned to %v", *sess.WorkerEndedAt, workerEnded)
	}

	// Agent ran for 3 minutes; full workflow elapsed for 1h46m3s. The PR/CI
	// waiting attributable to orchestration latency is the delta.
	workerDur := sess.WorkerEndedAt.Sub(sess.StartedAt)
	workflowDur := sess.FinishedAt.Sub(sess.StartedAt)
	if workerDur != 3*time.Minute {
		t.Errorf("agent runtime = %v, want 3m", workerDur)
	}
	if workflowDur != 1*time.Hour+46*time.Minute+3*time.Second {
		t.Errorf("workflow elapsed = %v, want 1h46m3s", workflowDur)
	}
	if workflowDur-workerDur < time.Hour {
		t.Errorf("pr/ci waiting = %v, want > 1h (the actual orchestration latency)", workflowDur-workerDur)
	}
}
