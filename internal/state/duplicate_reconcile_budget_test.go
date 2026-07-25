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

// Codex review catch (P1): the queue paths consult IssueRetryExhausted BEFORE
// FailedAttemptsForIssue, so excluding an outcome from the count alone leaves
// the issue blocked the moment one such session carries retry_exhausted status.
// Live ok-player #627: two duplicate-dispatch siblings held exactly that status.
func TestIssueRetryExhausted_IgnoresDuplicateDispatchReconcile(t *testing.T) {
	s := NewState()
	s.Sessions["a"] = retiredSibling(627, WorkerOutcomeDuplicateDispatchReconciled)
	s.Sessions["b"] = retiredSibling(627, WorkerOutcomeDuplicateDispatchReconciled)

	if s.IssueRetryExhausted(627) {
		t.Fatal("retired duplicate siblings must not mark the issue retry-exhausted")
	}
	if got := s.FailedAttemptsForIssue(627); got != 0 {
		t.Fatalf("FailedAttemptsForIssue = %d, want 0", got)
	}
}

// A governor stop is not exhaustion either: raising the budget must un-block it.
func TestIssueRetryExhausted_IgnoresTokenBudgetStops(t *testing.T) {
	s := NewState()
	s.Sessions["a"] = retiredSibling(628, string(DisplayTokenBudgetExceeded))

	if s.IssueRetryExhausted(628) {
		t.Fatal("a token-budget stop marked retry_exhausted must not block the issue forever")
	}
}

// Genuine exhaustion still blocks.
func TestIssueRetryExhausted_RealExhaustionStillBlocks(t *testing.T) {
	s := NewState()
	s.Sessions["a"] = retiredSibling(700, "")

	if !s.IssueRetryExhausted(700) {
		t.Fatal("a genuinely exhausted issue must still be blocked")
	}
}

// A transient backend block is not exhaustion.
func TestIssueRetryExhausted_IgnoresRateLimitedSessions(t *testing.T) {
	s := NewState()
	sess := retiredSibling(701, "")
	sess.RateLimitHit = true
	s.Sessions["a"] = sess

	if s.IssueRetryExhausted(701) {
		t.Fatal("a rate-limited session must not mark the issue retry-exhausted")
	}
}
