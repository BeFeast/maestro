package supervisor

import (
	"strings"
	"testing"
	"time"

	"github.com/befeast/maestro/internal/state"
)

// #693: a backend gated by an auth/credential failure must surface as a
// backend_auth_failure finding so the operator sees the backend is down
// (expired/invalidated OAuth token) instead of discovering it later as
// wedged issues. The default backend is blocking; others warn.
func TestDetectBackendAuthFailureStuckStates(t *testing.T) {
	cfg := testConfig(t)
	cfg.Model.Default = "claude"
	e := testEngine(cfg, &fakeReader{})
	now := e.now()

	retryAfter := now.Add(8 * time.Minute)
	st := state.NewState()
	st.BackendHealth["claude"] = state.BackendHealth{
		State:       state.BackendHealthCooldown,
		Reason:      state.BackendBlockAuthFailure,
		Pattern:     "failed_to_authenticate",
		Since:       now.Add(-2 * time.Minute),
		RetryAfter:  &retryAfter,
		LastSession: "kar-43",
	}

	findings := e.detectBackendAuthFailureStuckStates(st, now)
	if len(findings) != 1 {
		t.Fatalf("findings = %d, want 1: %+v", len(findings), findings)
	}
	finding := findings[0]
	if finding.Code != "backend_auth_failure" {
		t.Fatalf("code = %q, want backend_auth_failure", finding.Code)
	}
	if finding.Severity != SeverityBlocked {
		t.Fatalf("severity = %q, want %q (default backend down blocks new spawns)", finding.Severity, SeverityBlocked)
	}
	evidence := strings.Join(finding.Evidence, "\n")
	for _, want := range []string{"backend=claude", "signature=failed_to_authenticate", "last_session=kar-43"} {
		if !strings.Contains(evidence, want) {
			t.Errorf("evidence %q missing %q", evidence, want)
		}
	}
}

func TestDetectBackendAuthFailureStuckStates_SkipsExpiredAndOtherReasons(t *testing.T) {
	cfg := testConfig(t)
	cfg.Model.Default = "claude"
	e := testEngine(cfg, &fakeReader{})
	now := e.now()

	expired := now.Add(-time.Minute)
	st := state.NewState()
	// Cooldown elapsed: the selector will re-probe the backend, no finding.
	st.BackendHealth["claude"] = state.BackendHealth{
		State:      state.BackendHealthCooldown,
		Reason:     state.BackendBlockAuthFailure,
		RetryAfter: &expired,
	}
	// Provider rate limit is not an auth failure.
	st.BackendHealth["codex"] = state.BackendHealth{
		State:  state.BackendHealthCooldown,
		Reason: state.BackendBlockProviderLimit,
	}

	if findings := e.detectBackendAuthFailureStuckStates(st, now); len(findings) != 0 {
		t.Fatalf("findings = %+v, want none", findings)
	}
}

// A non-default backend failing auth degrades capacity but does not block
// new spawns — it must surface as a warning, not a blocker.
func TestDetectBackendAuthFailureStuckStates_NonDefaultBackendWarns(t *testing.T) {
	cfg := testConfig(t)
	cfg.Model.Default = "claude"
	e := testEngine(cfg, &fakeReader{})
	now := e.now()

	retryAfter := now.Add(8 * time.Minute)
	st := state.NewState()
	st.BackendHealth["codex"] = state.BackendHealth{
		State:      state.BackendHealthCooldown,
		Reason:     state.BackendBlockAuthFailure,
		Pattern:    "http_401",
		RetryAfter: &retryAfter,
	}

	findings := e.detectBackendAuthFailureStuckStates(st, now)
	if len(findings) != 1 {
		t.Fatalf("findings = %d, want 1: %+v", len(findings), findings)
	}
	if findings[0].Severity != SeverityWarning {
		t.Fatalf("severity = %q, want %q", findings[0].Severity, SeverityWarning)
	}
}
