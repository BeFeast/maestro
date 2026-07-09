package supervisor

import (
	"fmt"

	"github.com/befeast/maestro/internal/state"
)

// meteredBackendRefused reports whether this engine must skip the supervisor
// LLM path this cycle because its resolved backend is metered (per-token) and
// the project has not set supervisor.allow_metered_backend (#838). It only
// applies when the LLM path would otherwise run: the supervisor is enabled and
// the fleet-wide emergency LLM stop is not already forcing deterministic-only.
func (e *Engine) meteredBackendRefused() (backend string, refused bool) {
	if e == nil || e.cfg == nil {
		return "", false
	}
	if !e.cfg.Supervisor.Enabled || e.emergencyLLMHalt {
		return "", false
	}
	return e.cfg.SupervisorMeteredRefusal()
}

// meteredBackendStuckState builds the red stuck state surfaced when the
// supervisor LLM path is refused for a metered backend (#838). Severity=blocked
// so Mission Control renders it as an attention/red badge, and the evidence
// points the operator at both remediation paths.
func meteredBackendStuckState(backend string) state.SupervisorStuckState {
	return state.SupervisorStuckState{
		Code:     state.StuckSupervisorMeteredBackend,
		Severity: SeverityBlocked,
		Summary:  fmt.Sprintf("Supervisor LLM disabled: backend %q is metered (per-token) and supervisor.allow_metered_backend is not set; running deterministic-only.", backend),
		Evidence: []string{
			fmt.Sprintf("Resolved supervisor backend %q is declared pricing_class: metered", backend),
			"Always-on supervise cycles on a per-token backend burn cost making 'do nothing' decisions (incident 2026-07-09)",
			"Set supervisor.allow_metered_backend: true to opt in, or point supervisor.backend at a flat/subscription backend",
		},
		RecommendedAction: "Point supervisor.backend at a flat/subscription backend, or set supervisor.allow_metered_backend: true to accept per-token cost.",
		SupervisorCanAct:  false,
	}
}
