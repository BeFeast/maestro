package state

import (
	"testing"
	"time"
)

func budgetSession(issue int, outcome string, status SessionStatus, finished time.Time, pr int) *Session {
	f := finished
	return &Session{
		IssueNumber:   issue,
		PRNumber:      pr,
		Status:        status,
		WorkerOutcome: outcome,
		StartedAt:     finished.Add(-2 * time.Minute),
		FinishedAt:    &f,
	}
}

// Consecutive PR-less budget stops accumulate; the streak counts only the most
// recent run.
func TestConsecutiveTokenBudgetKillsForIssue_CountsRecentRun(t *testing.T) {
	now := time.Now().UTC()
	s := NewState()
	s.Sessions["a"] = budgetSession(628, string(DisplayTokenBudgetExceeded), StatusFailed, now.Add(-30*time.Minute), 0)
	s.Sessions["b"] = budgetSession(628, string(DisplayTokenBudgetExceeded), StatusFailed, now.Add(-20*time.Minute), 0)
	s.Sessions["c"] = budgetSession(628, string(DisplayTokenBudgetExceeded), StatusFailed, now.Add(-10*time.Minute), 0)

	if got := s.ConsecutiveTokenBudgetKillsForIssue(628); got != 3 {
		t.Fatalf("streak = %d, want 3", got)
	}
	if got := s.ConsecutiveTokenBudgetKillsForIssue(999); got != 0 {
		t.Fatalf("streak for unrelated issue = %d, want 0", got)
	}
}

// A newer non-budget ending breaks the streak: the budget was not the wall for
// the latest attempt, so dispatch must not stay held.
func TestConsecutiveTokenBudgetKillsForIssue_NonBudgetEndingBreaksStreak(t *testing.T) {
	now := time.Now().UTC()
	s := NewState()
	s.Sessions["a"] = budgetSession(628, string(DisplayTokenBudgetExceeded), StatusFailed, now.Add(-30*time.Minute), 0)
	s.Sessions["b"] = budgetSession(628, string(DisplayTokenBudgetExceeded), StatusFailed, now.Add(-20*time.Minute), 0)
	s.Sessions["c"] = budgetSession(628, "", StatusDead, now.Add(-5*time.Minute), 0)

	if got := s.ConsecutiveTokenBudgetKillsForIssue(628); got != 0 {
		t.Fatalf("streak = %d, want 0 — the most recent ending was not a budget stop", got)
	}
}

// A session that produced a PR is progress, never part of a futile streak.
func TestConsecutiveTokenBudgetKillsForIssue_IgnoresSessionsWithPR(t *testing.T) {
	now := time.Now().UTC()
	s := NewState()
	s.Sessions["a"] = budgetSession(628, string(DisplayTokenBudgetExceeded), StatusFailed, now.Add(-30*time.Minute), 0)
	s.Sessions["b"] = budgetSession(628, string(DisplayTokenBudgetExceeded), StatusFailed, now.Add(-10*time.Minute), 4242)

	if got := s.ConsecutiveTokenBudgetKillsForIssue(628); got != 1 {
		t.Fatalf("streak = %d, want 1 (the PR-bearing session is excluded)", got)
	}
}

// Running sessions are not endings and must not enter the streak.
func TestConsecutiveTokenBudgetKillsForIssue_IgnoresRunning(t *testing.T) {
	now := time.Now().UTC()
	s := NewState()
	s.Sessions["a"] = budgetSession(628, string(DisplayTokenBudgetExceeded), StatusFailed, now.Add(-30*time.Minute), 0)
	live := budgetSession(628, "", StatusRunning, now, 0)
	live.FinishedAt = nil
	s.Sessions["b"] = live

	if got := s.ConsecutiveTokenBudgetKillsForIssue(628); got != 1 {
		t.Fatalf("streak = %d, want 1", got)
	}
}

// The budget stop still must NOT burn the per-issue retry budget (#805 rule).
func TestFailedAttemptsForIssue_StillExcludesBudgetStops(t *testing.T) {
	now := time.Now().UTC()
	s := NewState()
	s.Sessions["a"] = budgetSession(628, string(DisplayTokenBudgetExceeded), StatusFailed, now.Add(-30*time.Minute), 0)
	s.Sessions["b"] = budgetSession(628, string(DisplayTokenBudgetExceeded), StatusFailed, now.Add(-20*time.Minute), 0)

	if got := s.FailedAttemptsForIssue(628); got != 0 {
		t.Fatalf("FailedAttemptsForIssue = %d, want 0 — budget stops are a governor, not work failures", got)
	}
}
