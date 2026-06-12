package orchestrator

import (
	"testing"
	"time"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/router"
	"github.com/befeast/maestro/internal/state"
)

func quotaDispatchConfig(windowTokens int) *config.Config {
	cfg := cfgWithBackends("claude", "claude", "codex")
	cfg.Model.FallbackBackends = []string{"codex"}
	claude := cfg.Model.Backends["claude"]
	claude.Quota = config.BackendQuota{WindowTokens: windowTokens}
	cfg.Model.Backends["claude"] = claude
	return cfg
}

func stateWithClaudeTokens(tokens int) *state.State {
	s := state.NewState()
	s.Sessions["sup-1"] = &state.Session{
		IssueNumber:     704,
		Backend:         "claude",
		Status:          state.StatusRunning,
		TokensUsedTotal: tokens,
	}
	return s
}

// #704 acceptance 2: with estimated usage above the threshold, the quota
// reconcile gates the backend and a fresh dispatch selects the fallback;
// below the threshold the default backend is used.
func TestReconcileBackendQuota_AboveThreshold_DispatchPrefersFallback(t *testing.T) {
	cfg := quotaDispatchConfig(1000)
	o := &Orchestrator{cfg: cfg, router: router.New(cfg)}
	now := time.Now().UTC()
	s := stateWithClaudeTokens(900) // 90% of the 5h window, threshold 85%

	if !o.reconcileBackendQuota(s, now) {
		t.Fatal("expected quota reconcile to report changes")
	}
	health, ok := s.BackendHealth["claude"]
	if !ok || health.Reason != state.BackendBlockQuotaPressure {
		t.Fatalf("health = %+v ok=%v, want quota_pressure cooldown", health, ok)
	}
	wantReset := now.Add(state.BackendQuotaWindow)
	if health.RetryAfter == nil || !health.RetryAfter.Equal(wantReset) {
		t.Fatalf("RetryAfter = %v, want window reset %v", health.RetryAfter, wantReset)
	}

	decision, dispatchable, _ := o.resolveDispatchBackend(s, makeIssue(704, "quota telemetry"), now)
	if !dispatchable {
		t.Fatal("dispatchable = false, want fallback substitution")
	}
	if decision.Backend != "codex" || decision.Reason != selectionReasonDispatchBlockedFallback {
		t.Fatalf("decision = %+v, want codex/%s", decision, selectionReasonDispatchBlockedFallback)
	}
}

func TestReconcileBackendQuota_BelowThreshold_DefaultBackendUsed(t *testing.T) {
	cfg := quotaDispatchConfig(1000)
	o := &Orchestrator{cfg: cfg, router: router.New(cfg)}
	now := time.Now().UTC()
	s := stateWithClaudeTokens(500) // 50% < 85%

	o.reconcileBackendQuota(s, now)
	if _, ok := s.BackendHealth["claude"]; ok {
		t.Fatalf("health = %+v, want no gate below threshold", s.BackendHealth["claude"])
	}

	decision, dispatchable, _ := o.resolveDispatchBackend(s, makeIssue(704, "quota telemetry"), now)
	if !dispatchable || decision.Backend != "claude" || decision.Reason != router.ReasonDefault {
		t.Fatalf("decision = %+v dispatchable=%v, want default claude", decision, dispatchable)
	}
}

// #704 acceptance 3 (self-clear): once the 5h window rolls over, the
// pressure gate is removed and dispatch returns to the default backend.
func TestReconcileBackendQuota_ClearsAfterWindowReset(t *testing.T) {
	cfg := quotaDispatchConfig(1000)
	o := &Orchestrator{cfg: cfg, router: router.New(cfg)}
	start := time.Now().UTC()
	s := stateWithClaudeTokens(900)

	o.reconcileBackendQuota(s, start)
	if _, ok := s.BackendHealth["claude"]; !ok {
		t.Fatal("expected quota_pressure gate at 90%")
	}

	after := start.Add(state.BackendQuotaWindow + time.Minute)
	if !o.reconcileBackendQuota(s, after) {
		t.Fatal("expected reconcile to clear the gate after window reset")
	}
	if _, ok := s.BackendHealth["claude"]; ok {
		t.Fatalf("health = %+v, want gate cleared after reset", s.BackendHealth["claude"])
	}

	decision, dispatchable, _ := o.resolveDispatchBackend(s, makeIssue(704, "quota telemetry"), after)
	if !dispatchable || decision.Backend != "claude" {
		t.Fatalf("decision = %+v dispatchable=%v, want default claude after reset", decision, dispatchable)
	}
}

// The episode anchor (Since) survives re-reconciles so the supervisor
// finding stays one-per-episode while usage keeps climbing.
func TestReconcileBackendQuota_PreservesEpisodeSince(t *testing.T) {
	cfg := quotaDispatchConfig(1000)
	o := &Orchestrator{cfg: cfg, router: router.New(cfg)}
	start := time.Now().UTC()
	s := stateWithClaudeTokens(900)

	o.reconcileBackendQuota(s, start)
	since := s.BackendHealth["claude"].Since

	s.Sessions["sup-1"].TokensUsedTotal = 950
	o.reconcileBackendQuota(s, start.Add(10*time.Minute))
	if got := s.BackendHealth["claude"].Since; !got.Equal(since) {
		t.Fatalf("Since = %v, want stable episode anchor %v", got, since)
	}
}

// A cooldown owned by another failure class is never overwritten by (or
// cleared as) a quota gate — auth/provider cooldowns carry their own
// recovery semantics.
func TestReconcileBackendQuota_DoesNotClobberAuthCooldown(t *testing.T) {
	cfg := quotaDispatchConfig(1000)
	o := &Orchestrator{cfg: cfg, router: router.New(cfg)}
	now := time.Now().UTC()
	s := stateWithClaudeTokens(900)
	retryAfter := now.Add(8 * time.Minute)
	s.BackendHealth["claude"] = state.BackendHealth{
		State:      state.BackendHealthCooldown,
		Reason:     state.BackendBlockAuthFailure,
		Since:      now,
		RetryAfter: &retryAfter,
	}

	o.reconcileBackendQuota(s, now)
	if got := s.BackendHealth["claude"].Reason; got != state.BackendBlockAuthFailure {
		t.Fatalf("Reason = %q, want auth_failure preserved", got)
	}
}

// A backend without quota config is never gated regardless of usage.
func TestReconcileBackendQuota_NoQuotaConfig_NoGate(t *testing.T) {
	cfg := cfgWithBackends("claude", "claude", "codex")
	o := &Orchestrator{cfg: cfg, router: router.New(cfg)}
	now := time.Now().UTC()
	s := stateWithClaudeTokens(10_000_000)

	o.reconcileBackendQuota(s, now)
	if _, ok := s.BackendHealth["claude"]; ok {
		t.Fatalf("health = %+v, want none without quota config", s.BackendHealth["claude"])
	}
}
