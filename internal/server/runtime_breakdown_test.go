package server

import (
	"testing"
	"time"

	"github.com/befeast/maestro/internal/state"
)

// TestRuntimeBreakdownSeparatesWorkerFromWorkflow reproduces the scribe-service
// #115 / #106 scenario captured in #426: a session that ran for ~3 minutes as a
// coding agent, then waited for PR / CI / Greptile / merge for ~1h43m, and was
// finally marked done. The original dashboard reported `runtime=1h46m3s` which
// conflated agent execution with orchestration waiting.
//
// The expectation is that worker_runtime stays at the agent's wall-clock and
// workflow_runtime is the total session elapsed — PR/CI waiting is exposed
// separately so the operator can see the orchestration latency rather than
// blame the coding backend.
func TestRuntimeBreakdownSeparatesWorkerFromWorkflow(t *testing.T) {
	startedAt := time.Date(2026, 5, 21, 14, 57, 24, 0, time.UTC)
	workerEndedAt := startedAt.Add(3 * time.Minute)
	finishedAt := time.Date(2026, 5, 21, 16, 43, 27, 0, time.UTC) // 1h46m3s after start
	prOpenedAt := workerEndedAt

	sess := &state.Session{
		IssueNumber:   106,
		IssueTitle:    "scribe-service: fix",
		Status:        state.StatusDone,
		Backend:       "claude",
		PRNumber:      115,
		Branch:        "feat/scr-46",
		StartedAt:     startedAt,
		WorkerEndedAt: &workerEndedAt,
		PROpenedAt:    &prOpenedAt,
		FinishedAt:    &finishedAt,
	}

	info := makeSessionInfo("test/repo", "scr-46", sess)

	if info.WorkerRuntimeSeconds != 180 {
		t.Errorf("worker_runtime_seconds = %d, want 180 (3m of actual agent wall-clock, not the 1h46m3s workflow elapsed)",
			info.WorkerRuntimeSeconds)
	}
	if info.WorkflowRuntimeSeconds != int64(finishedAt.Sub(startedAt)/time.Second) {
		t.Errorf("workflow_runtime_seconds = %d, want %d", info.WorkflowRuntimeSeconds, int64(finishedAt.Sub(startedAt)/time.Second))
	}
	// PR/CI waiting must equal workflow - worker.
	wantPRWait := info.WorkflowRuntimeSeconds - info.WorkerRuntimeSeconds
	if info.PROpenRuntimeSeconds != wantPRWait {
		t.Errorf("pr_open_runtime_seconds = %d, want %d (workflow - agent)", info.PROpenRuntimeSeconds, wantPRWait)
	}
	if info.WorkerEndedAt == "" {
		t.Errorf("worker_ended_at not exposed; operator can't explain the long elapsed time without a state transition timestamp")
	}
	if info.PROpenedAt == "" {
		t.Errorf("pr_opened_at not exposed; operator can't see when PR/CI waiting started")
	}
	// Legacy Runtime field must keep equivalence to workflow elapsed so historical
	// rows stay understandable and the API stays backwards-compatible.
	if info.RuntimeSeconds != info.WorkflowRuntimeSeconds {
		t.Errorf("legacy runtime_seconds=%d, workflow_runtime_seconds=%d — they should agree on total workflow elapsed",
			info.RuntimeSeconds, info.WorkflowRuntimeSeconds)
	}
}

// TestRuntimeBreakdownRunningSessionWorkerRuntimeIsLiveWallClock confirms that
// an in-flight session whose worker has not yet ended still reports
// worker_runtime as the agent's live wall-clock, not zero.
func TestRuntimeBreakdownRunningSessionWorkerRuntimeIsLiveWallClock(t *testing.T) {
	startedAt := time.Now().UTC().Add(-12 * time.Minute)
	sess := &state.Session{
		IssueNumber: 200,
		IssueTitle:  "in flight",
		Status:      state.StatusRunning,
		Backend:     "claude",
		Branch:      "feat/in-flight",
		StartedAt:   startedAt,
	}

	info := makeSessionInfo("test/repo", "slot-x", sess)

	if info.WorkerRuntimeSeconds < 600 || info.WorkerRuntimeSeconds > 900 {
		t.Errorf("worker_runtime_seconds = %d, want ~720 (running session wall-clock)", info.WorkerRuntimeSeconds)
	}
	if info.WorkflowRuntimeSeconds < info.WorkerRuntimeSeconds {
		t.Errorf("workflow=%d worker=%d — workflow should be >= worker", info.WorkflowRuntimeSeconds, info.WorkerRuntimeSeconds)
	}
	if info.PROpenRuntime != "" {
		t.Errorf("pr_open_runtime should be empty before PR opens, got %q", info.PROpenRuntime)
	}
}

// TestMarkWorkerEndedIsIdempotent guards against later status transitions
// (pr_open -> code_landed -> done) sliding WorkerEndedAt forward and
// undoing the agent-vs-workflow distinction.
func TestMarkWorkerEndedIsIdempotent(t *testing.T) {
	first := time.Date(2026, 5, 21, 15, 0, 24, 0, time.UTC)
	second := time.Date(2026, 5, 21, 16, 43, 27, 0, time.UTC)
	sess := &state.Session{IssueNumber: 1, Status: state.StatusRunning, StartedAt: first.Add(-time.Hour)}

	state.MarkWorkerEnded(sess, first)
	state.MarkWorkerEnded(sess, second) // simulates code_landed → done writing FinishedAt again

	if sess.WorkerEndedAt == nil {
		t.Fatal("WorkerEndedAt should be set after first call")
	}
	if !sess.WorkerEndedAt.Equal(first) {
		t.Errorf("WorkerEndedAt = %v, want first call %v (idempotent)", *sess.WorkerEndedAt, first)
	}
}

// TestMarkPROpenedIsIdempotent guards against pr_open -> running review-retry
// loops resetting the PR-open timestamp.
func TestMarkPROpenedIsIdempotent(t *testing.T) {
	first := time.Date(2026, 5, 21, 15, 0, 24, 0, time.UTC)
	second := first.Add(30 * time.Minute)
	sess := &state.Session{IssueNumber: 1, Status: state.StatusPROpen}

	state.MarkPROpened(sess, first)
	state.MarkPROpened(sess, second)

	if sess.PROpenedAt == nil {
		t.Fatal("PROpenedAt should be set after first call")
	}
	if !sess.PROpenedAt.Equal(first) {
		t.Errorf("PROpenedAt = %v, want %v (idempotent across review-retry oscillation)", *sess.PROpenedAt, first)
	}
}
