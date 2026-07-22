package supervisor

import (
	"testing"
	"time"

	"github.com/befeast/maestro/internal/github"
	"github.com/befeast/maestro/internal/state"
	"github.com/befeast/maestro/internal/worker"
)

func TestDynamicWaveHistoricalDoneSessionRemainsEligible(t *testing.T) {
	cfg := testConfig(t)
	enableDynamicWave(cfg)
	issue := testIssue(1103, "open ready issue", "maestro-ready")
	st := state.NewState()
	st.Sessions["historical"] = &state.Session{
		IssueNumber:   1103,
		Status:        state.StatusDone,
		WorkerOutcome: "duplicate_dispatch_reconciled",
	}

	result, err := testEngine(cfg, &fakeReader{}).dynamicWaveCandidateIssues(st, []github.Issue{issue}, nil)
	if err != nil {
		t.Fatalf("dynamicWaveCandidateIssues: %v", err)
	}
	if len(result.candidates) != 1 || result.candidates[0].Number != 1103 {
		t.Fatalf("candidates = %+v, want open issue #1103 eligible", result.candidates)
	}
	if result.analysis == nil || result.analysis.EligibleCandidates != 1 {
		t.Fatalf("queue analysis = %+v, want eligible=1", result.analysis)
	}
}

func TestTokenBudgetExceededSessionIgnoresReleasedSessions(t *testing.T) {
	now := time.Now().UTC()
	st := state.NewState()
	st.Sessions["older-active"] = &state.Session{
		IssueNumber:   1,
		StartedAt:     now.Add(-time.Hour),
		WorkerOutcome: worker.TokenBudgetExceededOutcome,
	}
	st.Sessions["newer-released"] = &state.Session{
		IssueNumber:           2,
		StartedAt:             now,
		WorkerOutcome:         worker.TokenBudgetExceededOutcome,
		ReleasedForRedispatch: true,
	}

	slot, sess, ok := tokenBudgetExceededSession(st)
	if !ok || slot != "older-active" || sess != st.Sessions["older-active"] {
		t.Fatalf("selection = (%q, %+v, %v), want only unreleased older-active", slot, sess, ok)
	}

	st.Sessions["older-active"].ReleasedForRedispatch = true
	if slot, sess, ok := tokenBudgetExceededSession(st); ok || slot != "" || sess != nil {
		t.Fatalf("all-released selection = (%q, %+v, %v), want none", slot, sess, ok)
	}
}
