package supervisor

import (
	"strings"
	"testing"
	"time"

	"github.com/befeast/maestro/internal/state"
)

// #704: a backend gated by quota pressure surfaces as a single
// backend_quota_pressure warning so the operator sees why fresh
// dispatches moved to fallback backends.
func TestDetectBackendQuotaPressureStuckStates(t *testing.T) {
	cfg := testConfig(t)
	cfg.Model.Default = "claude"
	e := testEngine(cfg, &fakeReader{})
	now := e.now()

	resetAt := now.Add(3 * time.Hour)
	st := state.NewState()
	st.BackendHealth["claude"] = state.BackendHealth{
		State:      state.BackendHealthCooldown,
		Reason:     state.BackendBlockQuotaPressure,
		Pattern:    "window 91% / week 34% (threshold 85%)",
		Since:      now.Add(-10 * time.Minute),
		RetryAfter: &resetAt,
	}

	findings := e.detectBackendQuotaPressureStuckStates(st, now)
	if len(findings) != 1 {
		t.Fatalf("findings = %d, want 1: %+v", len(findings), findings)
	}
	finding := findings[0]
	if finding.Code != "backend_quota_pressure" {
		t.Fatalf("code = %q, want backend_quota_pressure", finding.Code)
	}
	if finding.Severity != SeverityWarning {
		t.Fatalf("severity = %q, want %q (dispatch reroutes; nothing is blocked)", finding.Severity, SeverityWarning)
	}
	evidence := strings.Join(finding.Evidence, "\n")
	for _, want := range []string{"backend=claude", "usage=window 91%", "reset_at="} {
		if !strings.Contains(evidence, want) {
			t.Errorf("evidence %q missing %q", evidence, want)
		}
	}
}

// One finding per pressure episode: the orchestrator keeps Since/summary
// stable across cycles, so repeated detection compacts to one entry, and
// no finding survives once the window reset has passed (self-clear).
func TestDetectBackendQuotaPressureStuckStates_OncePerEpisodeAndSelfClears(t *testing.T) {
	cfg := testConfig(t)
	e := testEngine(cfg, &fakeReader{})
	now := e.now()

	resetAt := now.Add(time.Hour)
	st := state.NewState()
	st.BackendHealth["claude"] = state.BackendHealth{
		State:      state.BackendHealthCooldown,
		Reason:     state.BackendBlockQuotaPressure,
		Since:      now.Add(-time.Hour),
		RetryAfter: &resetAt,
	}

	first := e.detectBackendQuotaPressureStuckStates(st, now)
	second := e.detectBackendQuotaPressureStuckStates(st, now.Add(10*time.Minute))
	combined := compactStuckStates(append(first, second...))
	if len(combined) != 1 {
		t.Fatalf("compacted findings = %d, want 1 per episode: %+v", len(combined), combined)
	}

	// Past the reset the finding is gone even before the orchestrator
	// removes the gate entry.
	if findings := e.detectBackendQuotaPressureStuckStates(st, resetAt.Add(time.Minute)); len(findings) != 0 {
		t.Fatalf("findings after reset = %+v, want none", findings)
	}

	// And once the gate entry is cleared, nothing is emitted.
	delete(st.BackendHealth, "claude")
	if findings := e.detectBackendQuotaPressureStuckStates(st, now); len(findings) != 0 {
		t.Fatalf("findings after clear = %+v, want none", findings)
	}
}

// Other cooldown classes (auth failure, provider limit) are not quota
// pressure and must not double-report here.
func TestDetectBackendQuotaPressureStuckStates_SkipsOtherReasons(t *testing.T) {
	cfg := testConfig(t)
	e := testEngine(cfg, &fakeReader{})
	now := e.now()

	st := state.NewState()
	st.BackendHealth["claude"] = state.BackendHealth{
		State:  state.BackendHealthCooldown,
		Reason: state.BackendBlockAuthFailure,
	}
	st.BackendHealth["codex"] = state.BackendHealth{
		State:  state.BackendHealthCooldown,
		Reason: state.BackendBlockProviderLimit,
	}

	if findings := e.detectBackendQuotaPressureStuckStates(st, now); len(findings) != 0 {
		t.Fatalf("findings = %+v, want none", findings)
	}
}
