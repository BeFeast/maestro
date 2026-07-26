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

	if got := s.ConsecutiveTokenBudgetKillsForIssue(628, 0); got != 3 {
		t.Fatalf("streak = %d, want 3", got)
	}
	if got := s.ConsecutiveTokenBudgetKillsForIssue(999, 0); got != 0 {
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

	if got := s.ConsecutiveTokenBudgetKillsForIssue(628, 0); got != 0 {
		t.Fatalf("streak = %d, want 0 — the most recent ending was not a budget stop", got)
	}
}

// A session that produced a PR is progress, never part of a futile streak.
func TestConsecutiveTokenBudgetKillsForIssue_IgnoresSessionsWithPR(t *testing.T) {
	now := time.Now().UTC()
	s := NewState()
	s.Sessions["a"] = budgetSession(628, string(DisplayTokenBudgetExceeded), StatusFailed, now.Add(-30*time.Minute), 0)
	s.Sessions["b"] = budgetSession(628, string(DisplayTokenBudgetExceeded), StatusFailed, now.Add(-10*time.Minute), 4242)

	if got := s.ConsecutiveTokenBudgetKillsForIssue(628, 0); got != 1 {
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

	if got := s.ConsecutiveTokenBudgetKillsForIssue(628, 0); got != 1 {
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

// budgetKill builds a PR-less budget stop that records the ceiling it hit.
func budgetKill(issue int, finished time.Time, ceiling, observed int) *Session {
	sess := budgetSession(issue, string(DisplayTokenBudgetExceeded), StatusFailed, finished, 0)
	sess.TokenBudgetMaxTokens = ceiling
	sess.TokenBudgetTokensAttempt = observed
	return sess
}

// #1124: raising worker_max_tokens above the ceiling that produced the stops
// retires them. Without this the hold could never clear, because the only thing
// that reset the streak was a dispatch the hold itself blocked.
func TestConsecutiveTokenBudgetKillsForIssue_RaisedBudgetRetiresStaleKills(t *testing.T) {
	now := time.Now().UTC()
	s := NewState()
	s.Sessions["a"] = budgetKill(628, now.Add(-30*time.Minute), 120000, 157000)
	s.Sessions["b"] = budgetKill(628, now.Add(-20*time.Minute), 120000, 158000)

	if got := s.ConsecutiveTokenBudgetKillsForIssue(628, 120000); got != 2 {
		t.Fatalf("streak at the unchanged budget = %d, want 2", got)
	}
	if got := s.ConsecutiveTokenBudgetKillsForIssue(628, 400000); got != 0 {
		t.Fatalf("streak after the budget raise = %d, want 0 — the wall those stops hit no longer exists", got)
	}
}

// Stops recorded at the current ceiling are live evidence and still hold.
func TestConsecutiveTokenBudgetKillsForIssue_StopsAtCurrentCeilingStillCount(t *testing.T) {
	now := time.Now().UTC()
	s := NewState()
	s.Sessions["a"] = budgetKill(628, now.Add(-30*time.Minute), 400000, 430000)
	s.Sessions["b"] = budgetKill(628, now.Add(-20*time.Minute), 400000, 440000)

	if got := s.ConsecutiveTokenBudgetKillsForIssue(628, 400000); got != 2 {
		t.Fatalf("streak = %d, want 2 — these stops hit the budget that is configured now", got)
	}
}

// Sessions recorded before #1124 have no ceiling field; their observed usage is
// the lower bound that proves which side of the current budget they fall on.
// This is the shape of the live frozen issues (ok-folio #280, ok-player #628).
func TestConsecutiveTokenBudgetKillsForIssue_LegacyKillsUseObservedUsage(t *testing.T) {
	now := time.Now().UTC()
	s := NewState()
	s.Sessions["a"] = budgetKill(628, now.Add(-30*time.Minute), 0, 157000)
	s.Sessions["b"] = budgetKill(628, now.Add(-20*time.Minute), 0, 158000)

	if got := s.ConsecutiveTokenBudgetKillsForIssue(628, 120000); got != 2 {
		t.Fatalf("legacy streak at the old budget = %d, want 2", got)
	}
	if got := s.ConsecutiveTokenBudgetKillsForIssue(628, 400000); got != 0 {
		t.Fatalf("legacy streak after the raise = %d, want 0", got)
	}
}
