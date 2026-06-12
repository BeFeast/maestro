package state

import (
	"testing"
	"time"
)

// #704: session token deltas accrue into the backend's rolling quota
// windows; the window anchors at the first tokens observed.
func TestAccrueBackendQuotaUsage_AccruesDeltas(t *testing.T) {
	now := time.Date(2026, 6, 12, 12, 0, 0, 0, time.UTC)
	s := NewState()
	s.Sessions["sup-1"] = &Session{Backend: "claude", TokensUsedTotal: 1000}
	s.Sessions["sup-2"] = &Session{Backend: "claude", TokensUsedTotal: 500}
	s.Sessions["sup-3"] = &Session{Backend: "codex", TokensUsedTotal: 200}

	if !AccrueBackendQuotaUsage(s, now) {
		t.Fatal("expected accrual to report changes")
	}
	claude := s.BackendQuotaUsage["claude"]
	if claude == nil || claude.WindowTokens != 1500 || claude.WeekTokens != 1500 {
		t.Fatalf("claude usage = %+v, want 1500 window/week tokens", claude)
	}
	if claude.WindowStartedAt == nil || !claude.WindowStartedAt.Equal(now) {
		t.Fatalf("window anchor = %v, want %v (first use)", claude.WindowStartedAt, now)
	}
	if codex := s.BackendQuotaUsage["codex"]; codex == nil || codex.WindowTokens != 200 {
		t.Fatalf("codex usage = %+v, want 200 window tokens", codex)
	}

	// Second pass without new tokens accrues nothing further.
	AccrueBackendQuotaUsage(s, now.Add(time.Minute))
	if got := s.BackendQuotaUsage["claude"].WindowTokens; got != 1500 {
		t.Fatalf("window tokens after no-op pass = %d, want 1500 (delta accounting)", got)
	}

	// New tokens on an existing session accrue only the delta.
	s.Sessions["sup-1"].TokensUsedTotal = 1300
	AccrueBackendQuotaUsage(s, now.Add(2*time.Minute))
	if got := s.BackendQuotaUsage["claude"].WindowTokens; got != 1800 {
		t.Fatalf("window tokens after delta = %d, want 1800", got)
	}
}

// The 5h session window and the 7d weekly window roll independently:
// once a window's duration elapses its counter zeroes, and the next
// delta re-anchors it.
func TestAccrueBackendQuotaUsage_RollsExpiredWindows(t *testing.T) {
	start := time.Date(2026, 6, 12, 12, 0, 0, 0, time.UTC)
	s := NewState()
	s.Sessions["sup-1"] = &Session{Backend: "claude", TokensUsedTotal: 1000}
	AccrueBackendQuotaUsage(s, start)

	// 5h+ later the session window has expired but the week has not.
	afterWindow := start.Add(BackendQuotaWindow + time.Minute)
	if !AccrueBackendQuotaUsage(s, afterWindow) {
		t.Fatal("expected window roll to report changes")
	}
	usage := s.BackendQuotaUsage["claude"]
	if usage.WindowTokens != 0 || usage.WindowStartedAt != nil {
		t.Fatalf("usage after window expiry = %+v, want zeroed window", usage)
	}
	if usage.WeekTokens != 1000 || usage.WeekStartedAt == nil {
		t.Fatalf("usage after window expiry = %+v, want weekly counter intact", usage)
	}

	// New tokens re-anchor the session window at their own timestamp.
	s.Sessions["sup-1"].TokensUsedTotal = 1400
	AccrueBackendQuotaUsage(s, afterWindow.Add(time.Minute))
	usage = s.BackendQuotaUsage["claude"]
	if usage.WindowTokens != 400 || usage.WindowStartedAt == nil {
		t.Fatalf("usage after re-anchor = %+v, want 400 window tokens", usage)
	}
	if usage.WeekTokens != 1400 {
		t.Fatalf("week tokens = %d, want 1400", usage.WeekTokens)
	}
}

// A token counter that goes backwards (manual state surgery) re-baselines
// without panicking or un-counting window usage.
func TestAccrueBackendQuotaUsage_RebaselinesOnCounterReset(t *testing.T) {
	now := time.Date(2026, 6, 12, 12, 0, 0, 0, time.UTC)
	s := NewState()
	s.Sessions["sup-1"] = &Session{Backend: "claude", TokensUsedTotal: 1000}
	AccrueBackendQuotaUsage(s, now)

	s.Sessions["sup-1"].TokensUsedTotal = 100
	AccrueBackendQuotaUsage(s, now.Add(time.Minute))
	if got := s.Sessions["sup-1"].QuotaTokensAccounted; got != 100 {
		t.Fatalf("accounted = %d, want re-baselined to 100", got)
	}
	if got := s.BackendQuotaUsage["claude"].WindowTokens; got != 1000 {
		t.Fatalf("window tokens = %d, want 1000 (no un-counting)", got)
	}
}

func TestEvaluateBackendQuota(t *testing.T) {
	now := time.Date(2026, 6, 12, 12, 0, 0, 0, time.UTC)
	anchor := now.Add(-time.Hour)
	usage := &BackendQuotaUsage{
		WindowStartedAt: &anchor,
		WindowTokens:    900,
		WeekStartedAt:   &anchor,
		WeekTokens:      900,
	}

	status := EvaluateBackendQuota(usage, 1000, 10000, 0.85, now)
	if !status.Pressured {
		t.Fatalf("status = %+v, want pressured (90%% >= 85%%)", status)
	}
	if status.WindowPercent != 90 || status.WeekPercent != 9 {
		t.Fatalf("percents = %v/%v, want 90/9", status.WindowPercent, status.WeekPercent)
	}
	wantReset := anchor.Add(BackendQuotaWindow)
	if status.PressureResetAt == nil || !status.PressureResetAt.Equal(wantReset) {
		t.Fatalf("pressure reset = %v, want %v (window reset)", status.PressureResetAt, wantReset)
	}

	// Below threshold: not pressured.
	below := EvaluateBackendQuota(&BackendQuotaUsage{WindowStartedAt: &anchor, WindowTokens: 100}, 1000, 0, 0.85, now)
	if below.Pressured {
		t.Fatalf("status = %+v, want not pressured at 10%%", below)
	}

	// Expired window reads as empty without mutating the counters.
	expired := EvaluateBackendQuota(usage, 1000, 0, 0.85, now.Add(BackendQuotaWindow))
	if expired.Pressured || expired.WindowUsedTokens != 0 {
		t.Fatalf("status after expiry = %+v, want empty window", expired)
	}
	if usage.WindowTokens != 900 {
		t.Fatal("EvaluateBackendQuota must not mutate stored counters")
	}

	// Nil usage (no tokens observed yet) is a valid empty status.
	if empty := EvaluateBackendQuota(nil, 1000, 1000, 0.85, now); empty.Pressured || empty.WindowPercent != 0 {
		t.Fatalf("nil usage status = %+v, want empty", empty)
	}
}

// When both windows are over threshold, pressure relieves only when the
// later one resets.
func TestEvaluateBackendQuota_BothWindowsPressured(t *testing.T) {
	now := time.Date(2026, 6, 12, 12, 0, 0, 0, time.UTC)
	anchor := now.Add(-time.Hour)
	usage := &BackendQuotaUsage{
		WindowStartedAt: &anchor,
		WindowTokens:    900,
		WeekStartedAt:   &anchor,
		WeekTokens:      9000,
	}
	status := EvaluateBackendQuota(usage, 1000, 10000, 0.85, now)
	wantReset := anchor.Add(BackendQuotaWeek)
	if status.PressureResetAt == nil || !status.PressureResetAt.Equal(wantReset) {
		t.Fatalf("pressure reset = %v, want %v (weekly reset is later)", status.PressureResetAt, wantReset)
	}
}

// #704: a quota_pressure cooldown is predictive, not a malfunction — PR
// evidence must not clear it (the backend keeps landing PRs while
// pressured). RetryAfter elapsing still clears it.
func TestReconcileBackendHealth_QuotaPressureSurvivesPRSuccess(t *testing.T) {
	now := time.Date(2026, 6, 12, 12, 0, 0, 0, time.UTC)
	retryAfter := now.Add(time.Hour)
	s := NewState()
	s.BackendHealth["claude"] = BackendHealth{
		State:      BackendHealthCooldown,
		Reason:     BackendBlockQuotaPressure,
		Since:      now.Add(-time.Hour),
		RetryAfter: &retryAfter,
	}
	s.Sessions["sup-1"] = &Session{
		Backend:   "claude",
		Status:    StatusPROpen,
		PRNumber:  42,
		StartedAt: now.Add(-30 * time.Minute),
	}

	if ReconcileBackendHealth(s, now) {
		t.Fatal("quota_pressure entry must survive PR evidence")
	}
	if _, ok := s.BackendHealth["claude"]; !ok {
		t.Fatal("quota_pressure entry was removed")
	}

	// Same evidence clears an auth_failure cooldown (existing behaviour).
	s.BackendHealth["claude"] = BackendHealth{
		State:      BackendHealthCooldown,
		Reason:     BackendBlockAuthFailure,
		Since:      now.Add(-time.Hour),
		RetryAfter: &retryAfter,
	}
	if !ReconcileBackendHealth(s, now) {
		t.Fatal("auth_failure entry should clear on PR evidence")
	}

	// RetryAfter elapsing clears quota pressure too.
	past := now.Add(-time.Minute)
	s.BackendHealth["claude"] = BackendHealth{
		State:      BackendHealthCooldown,
		Reason:     BackendBlockQuotaPressure,
		RetryAfter: &past,
	}
	if !ReconcileBackendHealth(s, now) {
		t.Fatal("elapsed quota_pressure entry should clear")
	}
}
