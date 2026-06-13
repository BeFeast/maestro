package supervisor

import (
	"strings"
	"testing"
	"time"

	"github.com/befeast/maestro/internal/state"
)

// #713: a backend gated because its configured model is unavailable must
// surface as a backend_model_unavailable finding — distinct from
// backend_auth_failure — so the operator reaches for the right fix (swap the
// model id, not refresh creds). The default backend's model going dark blocks;
// others warn.
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
		Pattern:     "model_no_access",
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
		t.Fatalf("severity = %q, want %q (default backend's model down blocks new spawns)", finding.Severity, SeverityBlocked)
	}
	evidence := strings.Join(finding.Evidence, "\n")
	for _, want := range []string{"backend=claude", "signature=model_no_access", "last_session=kar-65"} {
		if !strings.Contains(evidence, want) {
			t.Errorf("evidence %q missing %q", evidence, want)
		}
	}
}

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
	// An auth failure is a different finding, not model-unavailable.
	st.BackendHealth["codex"] = state.BackendHealth{
		State:  state.BackendHealthCooldown,
		Reason: state.BackendBlockAuthFailure,
	}

	if findings := e.detectBackendModelUnavailableStuckStates(st, now); len(findings) != 0 {
		t.Fatalf("findings = %+v, want none", findings)
	}
}

// A non-default backend whose model is unavailable degrades capacity but does
// not block new spawns — it must surface as a warning, not a blocker.
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
