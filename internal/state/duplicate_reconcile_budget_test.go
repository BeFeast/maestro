package state

import (
	"testing"
	"time"
)

func retiredSibling(issue int, outcome string) *Session {
	now := time.Now().UTC()
	return &Session{
		IssueNumber:   issue,
		Status:        StatusRetryExhausted,
		WorkerOutcome: outcome,
		StartedAt:     now.Add(-time.Hour),
		FinishedAt:    &now,
	}
}

// Live 2026-07-23 (ok-player #627): Maestro's own duplicate-dispatch cleanup
// retired two sibling sessions, and those retirements counted against the
// per-issue retry budget. With a single genuine failure the issue still read
// 3/3 attempts and stopped being dispatched entirely.
func TestFailedAttemptsForIssue_ExcludesDuplicateDispatchReconcile(t *testing.T) {
	s := NewState()
	s.Sessions["a"] = retiredSibling(627, WorkerOutcomeDuplicateDispatchReconciled)
	s.Sessions["b"] = retiredSibling(627, WorkerOutcomeDuplicateDispatchReconciled)
	s.Sessions["c"] = retiredSibling(627, "") // one real failed attempt

	if got := s.FailedAttemptsForIssue(627); got != 1 {
		t.Fatalf("FailedAttemptsForIssue = %d, want 1 — Maestro retiring its own duplicate siblings is bookkeeping, not failed attempts", got)
	}
}

// Real failures still count, so the retry budget keeps protecting the fleet.
func TestFailedAttemptsForIssue_StillCountsRealFailures(t *testing.T) {
	s := NewState()
	s.Sessions["a"] = retiredSibling(700, "")
	s.Sessions["b"] = retiredSibling(700, WorkerOutcomeRepeatedUnexpectedExit)

	if got := s.FailedAttemptsForIssue(700); got != 2 {
		t.Fatalf("FailedAttemptsForIssue = %d, want 2", got)
	}
}
