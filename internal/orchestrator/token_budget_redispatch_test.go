package orchestrator

import (
	"testing"
	"time"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/github"
	"github.com/befeast/maestro/internal/state"
	"github.com/befeast/maestro/internal/worker"
)

func TestReleaseAgedTokenBudgetClaimClearsRetryBlockAndKeepsHistory(t *testing.T) {
	now := time.Date(2026, 7, 23, 10, 0, 0, 0, time.UTC)
	finished := now.Add(-tokenBudgetClaimGrace - time.Minute)
	st := state.NewState()
	sess := &state.Session{
		IssueNumber:   1006,
		Status:        state.StatusRetryExhausted,
		StartedAt:     finished.Add(-time.Hour),
		FinishedAt:    &finished,
		WorkerOutcome: worker.TokenBudgetExceededOutcome,
	}
	st.Sessions["aged"] = sess

	o := &Orchestrator{cfg: &config.Config{}}
	o.releaseAgedTokenBudgetClaim(st, sess, now)

	if !sess.ReleasedForRedispatch {
		t.Fatal("aged token-budget session was not released")
	}
	if sess.Status != state.StatusFailed || sess.WorkerOutcome != worker.TokenBudgetExceededOutcome {
		t.Fatalf("released audit history = %+v, want failed + token_budget_exceeded", sess)
	}
	if _, claimed := st.IssueClaimFor(1006); claimed {
		t.Fatal("aged token-budget issue still has a terminal claim")
	}
	if st.IssueRetryExhausted(1006) || st.FailedAttemptsForIssue(1006) != 0 {
		t.Fatal("released token-budget issue still consumes or reports an exhausted retry budget")
	}
	if status, ok := projectStatusForSession(sess, false); !ok || status != github.ProjectStatusTodo {
		t.Fatalf("released session board status = %q, %v; want Todo", status, ok)
	}
}
