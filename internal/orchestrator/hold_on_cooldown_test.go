package orchestrator

import (
	"testing"
	"time"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/github"
	"github.com/befeast/maestro/internal/router"
	"github.com/befeast/maestro/internal/state"
)

func holdTestOrchestrator(enabled bool, maxWaitMinutes int) *Orchestrator {
	cfg := &config.Config{
		Repo: "owner/repo",
		Model: config.ModelConfig{
			Default:          "claude",
			FallbackBackends: []string{"sol", "kimi"},
			Backends: map[string]config.BackendDef{
				"claude": {Cmd: "claude"},
				"sol":    {Cmd: "codex"},
				"kimi":   {Cmd: "kimi"},
			},
			HoldOnCooldown: config.HoldOnCooldownConfig{
				Enabled:        enabled,
				MaxWaitMinutes: maxWaitMinutes,
			},
		},
	}
	return &Orchestrator{cfg: cfg, router: router.New(cfg)}
}

func coolingState(backend, reason string, retryAfter *time.Time, now time.Time) *state.State {
	s := state.NewState()
	s.BackendHealth = map[string]state.BackendHealth{
		backend: {
			State:      state.BackendHealthCooldown,
			Reason:     reason,
			Since:      now.Add(-time.Minute),
			RetryAfter: retryAfter,
		},
	}
	return s
}

// A quota cooldown with a reset inside the hold window must hold fresh
// dispatch — even though later fallback rungs are healthy — and report the
// hold expiry so the caller can pause until the window reopens.
func TestResolveDispatchBackend_HoldsForQuotaCooldownWithinWindow(t *testing.T) {
	now := time.Now().UTC()
	reset := now.Add(30 * time.Minute)
	o := holdTestOrchestrator(true, 0)
	s := coolingState("claude", state.BackendBlockProviderLimit, &reset, now)

	decision, ok, retryAt := o.resolveDispatchBackend(s, makeIssue(1, "hold me"), now)
	if ok {
		t.Fatalf("dispatchable = true, want hold (decision=%+v)", decision)
	}
	if decision.Reason != selectionReasonHoldOnCooldown {
		t.Fatalf("reason = %q, want %q", decision.Reason, selectionReasonHoldOnCooldown)
	}
	if retryAt == nil || !retryAt.Equal(reset) {
		t.Fatalf("retryAt = %v, want %v", retryAt, reset)
	}
}

// A reset beyond max_wait (weekly cap) must keep today's cascade: the next
// healthy rung is substituted.
func TestResolveDispatchBackend_CascadesWhenResetBeyondWindow(t *testing.T) {
	now := time.Now().UTC()
	reset := now.Add(48 * time.Hour)
	o := holdTestOrchestrator(true, 60)
	s := coolingState("claude", state.BackendBlockProviderLimit, &reset, now)

	decision, ok, _ := o.resolveDispatchBackend(s, makeIssue(1, "cascade"), now)
	if !ok {
		t.Fatal("dispatchable = false, want cascade to a healthy rung")
	}
	if decision.Backend != "sol" {
		t.Fatalf("backend = %q, want sol", decision.Backend)
	}
	if decision.Reason != selectionReasonDispatchBlockedFallback {
		t.Fatalf("reason = %q, want %q", decision.Reason, selectionReasonDispatchBlockedFallback)
	}
}

// An auth failure is not a quota wait — cascading must proceed even with the
// hold feature enabled and the reset nearby.
func TestResolveDispatchBackend_CascadesOnAuthFailure(t *testing.T) {
	now := time.Now().UTC()
	reset := now.Add(5 * time.Minute)
	o := holdTestOrchestrator(true, 0)
	s := coolingState("claude", state.BackendBlockAuthFailure, &reset, now)

	decision, ok, _ := o.resolveDispatchBackend(s, makeIssue(1, "auth"), now)
	if !ok || decision.Backend != "sol" {
		t.Fatalf("decision = %+v ok=%v, want cascade to sol", decision, ok)
	}
}

// With the feature disabled the pre-existing cascade behavior is untouched.
func TestResolveDispatchBackend_FeatureOffKeepsCascade(t *testing.T) {
	now := time.Now().UTC()
	reset := now.Add(30 * time.Minute)
	o := holdTestOrchestrator(false, 0)
	s := coolingState("claude", state.BackendBlockProviderLimit, &reset, now)

	decision, ok, _ := o.resolveDispatchBackend(s, makeIssue(1, "off"), now)
	if !ok || decision.Backend != "sol" {
		t.Fatalf("decision = %+v ok=%v, want cascade to sol with hold disabled", decision, ok)
	}
}

// A cooldown with no stated reset cannot be waited for; cascade.
func TestResolveDispatchBackend_CascadesWhenResetUnknown(t *testing.T) {
	now := time.Now().UTC()
	o := holdTestOrchestrator(true, 0)
	s := coolingState("claude", state.BackendBlockUsageLimit, nil, now)

	decision, ok, _ := o.resolveDispatchBackend(s, makeIssue(1, "unknown"), now)
	if !ok || decision.Backend != "sol" {
		t.Fatalf("decision = %+v ok=%v, want cascade when no RetryAfter", decision, ok)
	}
}

// The live-failure fallover must hold too: selectBackendFallback returns no
// selection (callers park / preserve budget) and records the hold expiry.
func TestSelectBackendFallback_HoldsInsteadOfCascading(t *testing.T) {
	now := time.Now().UTC()
	reset := now.Add(90 * time.Minute)
	o := holdTestOrchestrator(true, 0)
	s := coolingState("claude", state.BackendBlockUsageLimit, &reset, now)
	sess := &state.Session{Backend: "claude", IssueNumber: 7}

	selection := o.selectBackendFallback(s, sess, now, selectionReasonUsageLimitFallback)
	if selection.SelectedBackend != "" {
		t.Fatalf("SelectedBackend = %q, want empty (hold)", selection.SelectedBackend)
	}
	if selection.SelectionReason != selectionReasonHoldOnCooldown {
		t.Fatalf("SelectionReason = %q, want %q", selection.SelectionReason, selectionReasonHoldOnCooldown)
	}
	if selection.HoldUntil != reset.UTC().Format(time.RFC3339) {
		t.Fatalf("HoldUntil = %q, want %s", selection.HoldUntil, reset.UTC().Format(time.RFC3339))
	}
	if earliest := earliestCandidateRetry(selection); earliest == nil || !earliest.Equal(reset.Truncate(time.Second)) {
		t.Fatalf("earliestCandidateRetry = %v, want %v", earliest, reset.Truncate(time.Second))
	}
}

// Feature off: the fallover walks the chain exactly as before.
func TestSelectBackendFallback_FeatureOffWalksChain(t *testing.T) {
	now := time.Now().UTC()
	reset := now.Add(90 * time.Minute)
	o := holdTestOrchestrator(false, 0)
	s := coolingState("claude", state.BackendBlockUsageLimit, &reset, now)
	sess := &state.Session{Backend: "claude", IssueNumber: 7}

	selection := o.selectBackendFallback(s, sess, now, selectionReasonUsageLimitFallback)
	if selection.SelectedBackend != "sol" {
		t.Fatalf("SelectedBackend = %q, want sol", selection.SelectedBackend)
	}
}

// The dashboard hold surface: dispatchHoldForCycle reports hold_on_cooldown
// when the route default is held while other rungs stay healthy.
func TestDispatchHoldForCycle_ReportsHoldOnCooldown(t *testing.T) {
	now := time.Now().UTC()
	reset := now.Add(45 * time.Minute)
	o := holdTestOrchestrator(true, 0)
	s := coolingState("claude", state.BackendBlockQuotaPressure, &reset, now)

	hold := o.dispatchHoldForCycle(s, state.Capacity{}, 3, now)
	if !hold.Active || hold.ReasonClass != state.DispatchHoldOnCooldown {
		t.Fatalf("hold = %+v, want active hold_on_cooldown", hold)
	}
}

var _ = github.Issue{} // keep import stable if makeIssue moves
