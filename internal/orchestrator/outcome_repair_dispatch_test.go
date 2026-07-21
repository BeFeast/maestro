package orchestrator

import (
	"testing"
	"time"

	"github.com/befeast/maestro/internal/github"
	"github.com/befeast/maestro/internal/outcome"
	"github.com/befeast/maestro/internal/state"
	"github.com/befeast/maestro/internal/supervisor"
)

func TestStartNewWorkers_OutcomeRepairMarkerBypassesRedOutcomeReadyHold(t *testing.T) {
	cfg := cfgWithBackends("codex", "codex")
	cfg.IssueLabels = []string{"maestro-ready"}
	dynamicWaveEnabled := true
	cfg.Supervisor.DynamicWave.Enabled = &dynamicWaveEnabled
	cfg.Supervisor.DynamicWave.OwnsReadyLabel = true

	issue := makeIssue(901, "repair delivery infrastructure", "maestro-ready")
	issue.Body = "repair evidence\n<!-- maestro:outcome-repair fingerprint=0123456789abcdef -->"
	o, started, _ := newStartWorkersOrchestrator(cfg, []github.Issue{issue})

	now := time.Now().UTC()
	s := state.NewState()
	s.OutcomeHealth = &outcome.HealthCheckResult{CheckedAt: now, State: outcome.HealthFailing}
	s.OutcomeRecovery = &outcome.RecoveryState{Status: outcome.RecoveryStatusCapped, CappedAt: now, UpdatedAt: now}
	s.RecordSupervisorDecision(state.SupervisorDecision{
		ID:                "red-outcome-hold",
		CreatedAt:         now,
		PolicyRule:        supervisor.PolicyRuleRuntimeState,
		RecommendedAction: supervisor.ActionCheckOutcomeHealth,
	}, state.DefaultSupervisorDecisionLimit)

	o.startNewWorkers(s, 1)
	if len(*started) != 1 || (*started)[0] != 901 {
		t.Fatalf("started = %v, want marker-classified outcome repair issue #901", *started)
	}
}
