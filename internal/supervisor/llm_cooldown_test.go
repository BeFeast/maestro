package supervisor

import (
	"strings"
	"testing"
	"time"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/state"
)

func cooldownCandidatesConfig() *config.Config {
	return &config.Config{Model: config.ModelConfig{
		Default:          "claude",
		FallbackBackends: []string{"sol", "opencode"},
		Backends: map[string]config.BackendDef{
			"claude":   {Cmd: "claude"},
			"sol":      {Cmd: "codex"},
			"opencode": {Cmd: "opencode"},
		},
	}}
}

// A candidate in active BackendHealth cooldown is skipped for the cycle
// instead of re-proving the outage with a live call (the pre-fix walk landed
// on the last rung — opencode — every supervise tick while primaries cooled).
func TestSupervisorBackendCandidates_SkipsCoolingBackends(t *testing.T) {
	now := time.Now().UTC()
	retry := now.Add(30 * time.Minute)
	health := map[string]state.BackendHealth{
		"claude": {State: state.BackendHealthCooldown, Reason: state.BackendBlockProviderLimit, RetryAfter: &retry},
	}

	candidates, err := supervisorBackendCandidates(cooldownCandidatesConfig(), health, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 2 || candidates[0].name != "sol" || candidates[1].name != "opencode" {
		t.Fatalf("candidates = %+v, want [sol opencode]", candidates)
	}
}

// An elapsed cooldown no longer blocks the candidate.
func TestSupervisorBackendCandidates_ElapsedCooldownIsEligible(t *testing.T) {
	now := time.Now().UTC()
	retry := now.Add(-time.Minute)
	health := map[string]state.BackendHealth{
		"claude": {State: state.BackendHealthCooldown, Reason: state.BackendBlockProviderLimit, RetryAfter: &retry},
	}

	candidates, err := supervisorBackendCandidates(cooldownCandidatesConfig(), health, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 3 || candidates[0].name != "claude" {
		t.Fatalf("candidates = %+v, want claude first", candidates)
	}
}

// All candidates cooling → a distinct error so decideWithLLM degrades to the
// deterministic guardrail without burning a walk down the chain.
func TestSupervisorBackendCandidates_AllCoolingSkipsCycle(t *testing.T) {
	now := time.Now().UTC()
	retry := now.Add(30 * time.Minute)
	health := map[string]state.BackendHealth{}
	for _, name := range []string{"claude", "sol", "opencode"} {
		health[name] = state.BackendHealth{State: state.BackendHealthCooldown, Reason: state.BackendBlockUsageLimit, RetryAfter: &retry}
	}

	_, err := supervisorBackendCandidates(cooldownCandidatesConfig(), health, now)
	if err == nil || !strings.Contains(err.Error(), "cooling down") {
		t.Fatalf("err = %v, want all-cooling-down skip", err)
	}
}
