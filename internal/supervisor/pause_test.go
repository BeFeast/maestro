package supervisor

import (
	"strings"
	"testing"
	"time"

	"github.com/befeast/maestro/internal/github"
	"github.com/befeast/maestro/internal/state"
)

// TestDecide_PausedIdleReportsPausedNotStall verifies the #683 supervisor
// contract: a paused project with no in-flight work reports the paused state
// (ActionNone with a paused reason) instead of treating the idle project as a
// stall or recommending new work — and makes no GitHub reads to do so.
func TestDecide_PausedIdleReportsPausedNotStall(t *testing.T) {
	cfg := testConfig(t)
	reader := &fakeReader{issues: []github.Issue{{Number: 683, Title: "ready issue"}}}
	st := state.NewState()
	st.SetPaused(time.Date(2026, 6, 11, 9, 0, 0, 0, time.UTC))

	decision, err := testEngine(cfg, reader).Decide(st)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}

	if decision.RecommendedAction != ActionNone {
		t.Fatalf("action = %q, want %q while paused", decision.RecommendedAction, ActionNone)
	}
	if !strings.Contains(strings.ToLower(decision.Summary), "paused") {
		t.Fatalf("summary = %q, want it to report the paused state", decision.Summary)
	}
	if reader.issueCalls != 0 {
		t.Fatalf("ListOpenIssues called %d time(s), want 0 for a paused idle decision", reader.issueCalls)
	}
	if len(decision.StuckStates) != 0 {
		t.Fatalf("stuck states = %#v, want none while intentionally paused", decision.StuckStates)
	}
}

// TestDecide_PausedWithActiveSessionContinuesNormalFlow verifies that a
// worker in flight at pause time keeps its normal supervision: the paused
// short-circuit only applies once the project is idle, so the running
// worker's PR continues to be monitored to completion (#683 AC2).
func TestDecide_PausedWithActiveSessionContinuesNormalFlow(t *testing.T) {
	cfg := testConfig(t)
	reader := &fakeReader{}
	st := state.NewState()
	st.SetPaused(time.Now().UTC())
	st.Sessions["slot-1"] = &state.Session{
		IssueNumber: 42,
		IssueTitle:  "work in progress",
		Status:      state.StatusRunning,
		PID:         12345,
		StartedAt:   time.Now().UTC(),
	}

	decision, err := testEngine(cfg, reader).Decide(st)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}

	if decision.RecommendedAction != ActionWaitForRunningWorker {
		t.Fatalf("action = %q, want %q for an in-flight worker under pause", decision.RecommendedAction, ActionWaitForRunningWorker)
	}
}
