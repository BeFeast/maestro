package supervisor

import (
	"testing"
	"time"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/state"
)

func bookkeepingRetiredSession(issue int, outcome string) *state.Session {
	now := time.Now().UTC()
	return &state.Session{
		IssueNumber:   issue,
		Status:        state.StatusRetryExhausted,
		WorkerOutcome: outcome,
		StartedAt:     now.Add(-time.Hour),
		FinishedAt:    &now,
	}
}

// Codex review catch (P1): correcting the two retry gates was not enough.
// retryExhaustedSession, retryExhaustedRepairCandidate and the stuck-state scan
// each act on the raw retry_exhausted status, so a sibling Maestro retired
// while reconciling its own duplicate dispatch still drove repair spawns and a
// Blocked finding — the same class of bug one layer down.
func TestRetryExhaustedSession_IgnoresBookkeepingRetirements(t *testing.T) {
	e := &Engine{cfg: &config.Config{}}
	cache := newResolutionCache(nil)

	for _, tc := range []struct {
		name    string
		outcome string
	}{
		{"duplicate dispatch reconciled", state.WorkerOutcomeDuplicateDispatchReconciled},
		{"token budget stop", string(state.DisplayTokenBudgetExceeded)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := state.NewState()
			st.Sessions["a"] = bookkeepingRetiredSession(627, tc.outcome)

			if slot, _, ok := e.retryExhaustedSession(st, cache); ok {
				t.Fatalf("slot %q reported as retry-exhausted: %s is control-plane "+
					"bookkeeping, not evidence the implementation failed", slot, tc.outcome)
			}
		})
	}
}

// A genuine failure still surfaces — the filter must not silence real exhaustion.
func TestRetryExhaustedSession_StillReportsGenuineFailure(t *testing.T) {
	e := &Engine{cfg: &config.Config{}}
	st := state.NewState()
	st.Sessions["a"] = bookkeepingRetiredSession(627, "")

	if _, _, ok := e.retryExhaustedSession(st, newResolutionCache(nil)); !ok {
		t.Fatal("a genuinely exhausted session must still be reported")
	}
}

// A rate-limited session is a transient backend block, not a failed attempt.
func TestRetryExhaustedSession_IgnoresRateLimitedSession(t *testing.T) {
	e := &Engine{cfg: &config.Config{}}
	st := state.NewState()
	sess := bookkeepingRetiredSession(627, "")
	sess.RateLimitHit = true
	st.Sessions["a"] = sess

	if _, _, ok := e.retryExhaustedSession(st, newResolutionCache(nil)); ok {
		t.Fatal("a rate-limited session must not count as retry exhaustion")
	}
}
