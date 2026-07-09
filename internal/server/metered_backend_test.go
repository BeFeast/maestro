package server

import (
	"testing"
	"time"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/state"
)

// TestFleetMeteredBackendOperatorState pins #838 AC: a supervisor decision
// carrying the metered stuck state surfaces as an error-tone, top-priority
// "Metered backend" operator state on Mission Control.
func TestFleetMeteredBackendOperatorState(t *testing.T) {
	now := time.Now().UTC()

	op := buildFleetProjectOperatorState(fleetProjectState{
		Name: "Dogfood",
		Repo: "befeast/maestro",
		Supervisor: supervisorInfo{Latest: &supervisorDecisionInfo{
			CreatedAt:         now,
			RecommendedAction: "none",
			StuckStates: []state.SupervisorStuckState{{
				Code:              state.StuckSupervisorMeteredBackend,
				Severity:          "blocked",
				Summary:           `Supervisor LLM disabled: backend "fireworks" is metered (per-token) and supervisor.allow_metered_backend is not set; running deterministic-only.`,
				RecommendedAction: "Point supervisor.backend at a flat/subscription backend, or set supervisor.allow_metered_backend: true.",
			}},
		}},
	})

	if op.Kind != "metered_backend" {
		t.Errorf("Kind = %q, want metered_backend", op.Kind)
	}
	if op.Tone != "error" {
		t.Errorf("Tone = %q, want error", op.Tone)
	}
	if op.Label != "Metered backend" {
		t.Errorf("Label = %q, want \"Metered backend\"", op.Label)
	}
	if op.Summary == "" || op.NextAction == "" {
		t.Errorf("Summary/NextAction must be populated: %+v", op)
	}
	if !fleetOperatorKindNeedsAction(op.Kind) {
		t.Error("metered_backend must be an operator-action kind")
	}
	if got := fleetNextActionCTAForProject(op.Kind, op); got != "Fix metered supervisor backend" {
		t.Errorf("CTA = %q, want \"Fix metered supervisor backend\"", got)
	}
	// The kind must escalate the fleet verdict to attention.
	var summary fleetSummary
	addFleetOperatorSummary(&summary, op)
	if summary.MeteredBackend != 1 {
		t.Fatalf("summary.MeteredBackend = %d, want 1", summary.MeteredBackend)
	}
	if tone := fleetVerdictTone(summary, &supervisorDecisionInfo{CreatedAt: now}, now); tone != "attention" {
		t.Errorf("verdict tone = %q, want attention", tone)
	}
}

// TestBuildFleetEffectiveConfig_MeteredOptInVisible pins #838 AC: the supervisor
// opt-in and the per-backend metered classification are visible in the fleet
// effective_config surface.
func TestBuildFleetEffectiveConfig_MeteredOptInVisible(t *testing.T) {
	cfg := &config.Config{
		Model: config.ModelConfig{
			Default: "claude",
			Backends: map[string]config.BackendDef{
				"claude":    {Cmd: "claude"},
				"fireworks": {Cmd: "fw", PricingClass: config.PricingClassMetered},
			},
		},
		Supervisor: config.SupervisorConfig{Backend: "fireworks"},
		Routing:    config.RoutingConfig{Mode: "auto", RouterModel: "claude"},
	}

	eff := buildFleetEffectiveConfig(cfg)
	gate := eff.SupervisorGate
	if gate.AllowMeteredBackend {
		t.Error("AllowMeteredBackend should be false by default")
	}
	if !gate.MeteredBackendRefused || gate.MeteredBackend != "fireworks" {
		t.Errorf("expected refusal for fireworks, got refused=%v backend=%q", gate.MeteredBackendRefused, gate.MeteredBackend)
	}

	var fireworks *fleetEffectiveBackendConfig
	for i := range eff.ModelPolicy.Backends {
		if eff.ModelPolicy.Backends[i].Name == "fireworks" {
			fireworks = &eff.ModelPolicy.Backends[i]
		}
	}
	if fireworks == nil || !fireworks.Metered || fireworks.PricingClass != "metered" {
		t.Fatalf("fireworks backend must surface metered classification: %+v", fireworks)
	}

	// Opt-in clears the refusal in the surfaced config.
	cfg.Supervisor.AllowMeteredBackend = true
	eff = buildFleetEffectiveConfig(cfg)
	if !eff.SupervisorGate.AllowMeteredBackend || eff.SupervisorGate.MeteredBackendRefused {
		t.Errorf("opt-in must show allow=true, refused=false: %+v", eff.SupervisorGate)
	}
}
