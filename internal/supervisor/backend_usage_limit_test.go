package supervisor

import (
	"strings"
	"testing"
	"time"

	"github.com/befeast/maestro/internal/state"
)

// #805: a backend gated by an account-quota exhaustion must surface as a
// backend_usage_limit finding — "quota exhausted, failover covering" — so the
// operator sees why dispatch moved instead of discovering wedged issues. The
// default backend is blocking; others warn.
func TestDetectBackendUsageLimitStuckStates(t *testing.T) {
	cfg := testConfig(t)
	cfg.Model.Default = "codex"
	e := testEngine(cfg, &fakeReader{})
	now := e.now()

	retryAfter := now.Add(25 * time.Minute)
	st := state.NewState()
	st.BackendHealth["codex"] = state.BackendHealth{
		State:       state.BackendHealthCooldown,
		Reason:      state.BackendBlockUsageLimit,
		Pattern:     "hit_limit",
		Since:       now.Add(-3 * time.Minute),
		RetryAfter:  &retryAfter,
		LastSession: "ok-12",
	}

	findings := e.detectBackendUsageLimitStuckStates(st, now)
	if len(findings) != 1 {
		t.Fatalf("findings = %d, want 1: %+v", len(findings), findings)
	}
	finding := findings[0]
	if finding.Code != "backend_usage_limit" {
		t.Fatalf("code = %q, want backend_usage_limit", finding.Code)
	}
	if finding.Severity != SeverityBlocked {
		t.Fatalf("severity = %q, want %q (default backend quota-dead blocks new spawns)", finding.Severity, SeverityBlocked)
	}
	if !strings.Contains(finding.Summary, "usage quota") {
		t.Fatalf("summary %q should say the quota is exhausted", finding.Summary)
	}
	evidence := strings.Join(finding.Evidence, "\n")
	for _, want := range []string{"backend=codex", "signature=hit_limit", "last_session=ok-12"} {
		if !strings.Contains(evidence, want) {
			t.Errorf("evidence %q missing %q", evidence, want)
		}
	}
}

func TestDetectBackendUsageLimitStuckStates_SkipsExpiredAndOtherReasons(t *testing.T) {
	cfg := testConfig(t)
	cfg.Model.Default = "codex"
	e := testEngine(cfg, &fakeReader{})
	now := e.now()

	expired := now.Add(-time.Minute)
	st := state.NewState()
	// Cooldown elapsed: the selector will re-probe the backend, no finding.
	st.BackendHealth["codex"] = state.BackendHealth{
		State:      state.BackendHealthCooldown,
		Reason:     state.BackendBlockUsageLimit,
		RetryAfter: &expired,
	}
	// Auth failure and quota pressure have their own findings.
	st.BackendHealth["claude"] = state.BackendHealth{
		State:  state.BackendHealthCooldown,
		Reason: state.BackendBlockAuthFailure,
	}
	st.BackendHealth["opencode"] = state.BackendHealth{
		State:  state.BackendHealthCooldown,
		Reason: state.BackendBlockQuotaPressure,
	}

	if findings := e.detectBackendUsageLimitStuckStates(st, now); len(findings) != 0 {
		t.Fatalf("findings = %+v, want none", findings)
	}
}

// A non-default backend hitting its quota degrades capacity but does not
// block new spawns — it must surface as a warning, not a blocker.
func TestDetectBackendUsageLimitStuckStates_NonDefaultBackendWarns(t *testing.T) {
	cfg := testConfig(t)
	cfg.Model.Default = "claude"
	e := testEngine(cfg, &fakeReader{})
	now := e.now()

	retryAfter := now.Add(25 * time.Minute)
	st := state.NewState()
	st.BackendHealth["codex"] = state.BackendHealth{
		State:      state.BackendHealthCooldown,
		Reason:     state.BackendBlockUsageLimit,
		RetryAfter: &retryAfter,
	}

	findings := e.detectBackendUsageLimitStuckStates(st, now)
	if len(findings) != 1 {
		t.Fatalf("findings = %d, want 1: %+v", len(findings), findings)
	}
	if findings[0].Severity != SeverityWarning {
		t.Fatalf("severity = %q, want %q", findings[0].Severity, SeverityWarning)
	}
}
