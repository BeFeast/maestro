package supervisor

import (
	"strings"
	"testing"
	"time"

	"github.com/befeast/maestro/internal/state"
)

// #713: a backend gated because its configured model is unavailable must
// surface as a backend_model_unavailable finding — distinct from
// backend_auth_failure — so the operator swaps the model id rather than
// chasing credentials. The default backend going dark blocks new spawns.
func TestDetectBackendModelUnavailableStuckStates(t *testing.T) {
	cfg := testConfig(t)
	cfg.Model.Default = "claude"
	e := testEngine(cfg, &fakeReader{})
	now := e.now()

	retryAfter := now.Add(8 * time.Minute)
	st := state.NewState()
	st.BackendHealth["claude"] = state.BackendHealth{
		State:       state.BackendHealthCooldown,
		Reason:      state.BackendBlockModelUnavailable,
		Pattern:     "model_access_denied",
		Since:       now.Add(-2 * time.Minute),
		RetryAfter:  &retryAfter,
		LastSession: "kar-65",
	}

	findings := e.detectBackendModelUnavailableStuckStates(st, now)
	if len(findings) != 1 {
		t.Fatalf("findings = %d, want 1: %+v", len(findings), findings)
	}
	finding := findings[0]
	if finding.Code != "backend_model_unavailable" {
		t.Fatalf("code = %q, want backend_model_unavailable", finding.Code)
	}
	if finding.Severity != SeverityBlocked {
		t.Fatalf("severity = %q, want %q (default backend's model gone blocks new spawns)", finding.Severity, SeverityBlocked)
	}
	evidence := strings.Join(finding.Evidence, "\n")
	for _, want := range []string{"backend=claude", "signature=model_access_denied", "last_session=kar-65"} {
		if !strings.Contains(evidence, want) {
			t.Errorf("evidence %q missing %q", evidence, want)
		}
	}
}

// The auth and model-unavailable detectors must not poach each other's gates:
// an auth_failure cooldown produces no model-unavailable finding, and an
// elapsed cooldown self-clears.
func TestDetectBackendModelUnavailableStuckStates_SkipsExpiredAndOtherReasons(t *testing.T) {
	cfg := testConfig(t)
	cfg.Model.Default = "claude"
	e := testEngine(cfg, &fakeReader{})
	now := e.now()

	expired := now.Add(-time.Minute)
	st := state.NewState()
	// Cooldown elapsed: the selector will re-probe the backend, no finding.
	st.BackendHealth["claude"] = state.BackendHealth{
		State:      state.BackendHealthCooldown,
		Reason:     state.BackendBlockModelUnavailable,
		RetryAfter: &expired,
	}
	// An auth failure belongs to the auth detector, not this one.
	st.BackendHealth["codex"] = state.BackendHealth{
		State:  state.BackendHealthCooldown,
		Reason: state.BackendBlockAuthFailure,
	}

	if findings := e.detectBackendModelUnavailableStuckStates(st, now); len(findings) != 0 {
		t.Fatalf("findings = %+v, want none", findings)
	}
	// And the auth gate must not leak into the model finding stream.
	if findings := e.detectBackendAuthFailureStuckStates(st, now); len(findings) != 1 {
		t.Fatalf("auth findings = %d, want 1 (codex auth gate)", len(findings))
	}
}

// A non-default backend whose model is gone degrades capacity but does not
// block new spawns — it must surface as a warning, not a blocker.
func TestDetectBackendModelUnavailableStuckStates_NonDefaultBackendWarns(t *testing.T) {
	cfg := testConfig(t)
	cfg.Model.Default = "claude"
	e := testEngine(cfg, &fakeReader{})
	now := e.now()

	retryAfter := now.Add(8 * time.Minute)
	st := state.NewState()
	st.BackendHealth["codex"] = state.BackendHealth{
		State:      state.BackendHealthCooldown,
		Reason:     state.BackendBlockModelUnavailable,
		Pattern:    "http_404",
		RetryAfter: &retryAfter,
	}

	findings := e.detectBackendModelUnavailableStuckStates(st, now)
	if len(findings) != 1 {
		t.Fatalf("findings = %d, want 1: %+v", len(findings), findings)
	}
	if findings[0].Severity != SeverityWarning {
		t.Fatalf("severity = %q, want %q", findings[0].Severity, SeverityWarning)
	}
}
