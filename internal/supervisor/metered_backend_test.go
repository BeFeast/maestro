package supervisor

import (
	"testing"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/github"
	"github.com/befeast/maestro/internal/state"
)

// meteredCfg builds a supervisor config whose resolved backend is metered
// (per-token). The fakeLLM in testLLMEngine stands in for the backend call, so
// a non-zero call count means the LLM path ran.
func meteredCfg(t *testing.T) *config.Config {
	t.Helper()
	cfg := testConfig(t)
	cfg.IssueLabels = []string{"maestro-ready"}
	cfg.Model = config.ModelConfig{
		Default: "claude",
		Backends: map[string]config.BackendDef{
			"claude":    {Cmd: "claude"},
			"fireworks": {Cmd: "fw", PricingClass: config.PricingClassMetered},
		},
	}
	cfg.Supervisor.Backend = "fireworks"
	return cfg
}

// TestDecide_MeteredBackend_NoOptIn_SkipsLLM pins the #838 acceptance: a metered
// supervisor backend without supervisor.allow_metered_backend takes the
// deterministic-only path (the backend is never called) and the decision
// carries the red metered stuck state.
func TestDecide_MeteredBackend_NoOptIn_SkipsLLM(t *testing.T) {
	cfg := meteredCfg(t)
	reader := &fakeReader{issues: []github.Issue{testIssue(42, "ready work", "maestro-ready")}}
	llm := validSpawnLLM()

	decision, err := testLLMEngine(cfg, reader, llm).Decide(state.NewState())
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if llm.calls != 0 {
		t.Fatalf("LLM calls = %d, want 0 (metered backend without opt-in must skip the LLM path)", llm.calls)
	}
	if decision.RecommendedAction == "" {
		t.Fatal("metered refusal produced no deterministic decision")
	}
	stuck := requireStuckState(t, decision, state.StuckSupervisorMeteredBackend)
	if stuck.Severity != SeverityBlocked {
		t.Fatalf("metered stuck severity = %q, want %q", stuck.Severity, SeverityBlocked)
	}
}

// TestDecide_MeteredBackend_OptIn_RunsLLM proves the opt-in restores the LLM
// path: with supervisor.allow_metered_backend the backend is called and no
// metered stuck state is attached.
func TestDecide_MeteredBackend_OptIn_RunsLLM(t *testing.T) {
	cfg := meteredCfg(t)
	cfg.Supervisor.AllowMeteredBackend = true
	reader := &fakeReader{issues: []github.Issue{testIssue(42, "ready work", "maestro-ready")}}
	llm := validSpawnLLM()

	decision, err := testLLMEngine(cfg, reader, llm).Decide(state.NewState())
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if llm.calls != 1 {
		t.Fatalf("LLM calls = %d, want 1 (opt-in must restore the LLM path)", llm.calls)
	}
	for _, stuck := range decision.StuckStates {
		if stuck.Code == state.StuckSupervisorMeteredBackend {
			t.Fatal("opt-in must not attach the metered stuck state")
		}
	}
}

// TestDecide_FlatBackend_RunsLLM proves a flat/subscription backend is never
// gated: the LLM path runs and no metered stuck state is attached.
func TestDecide_FlatBackend_RunsLLM(t *testing.T) {
	cfg := meteredCfg(t)
	cfg.Supervisor.Backend = "claude" // flat / unset pricing class
	reader := &fakeReader{issues: []github.Issue{testIssue(42, "ready work", "maestro-ready")}}
	llm := validSpawnLLM()

	decision, err := testLLMEngine(cfg, reader, llm).Decide(state.NewState())
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if llm.calls != 1 {
		t.Fatalf("LLM calls = %d, want 1 (flat backend must run the LLM path)", llm.calls)
	}
	for _, stuck := range decision.StuckStates {
		if stuck.Code == state.StuckSupervisorMeteredBackend {
			t.Fatal("flat backend must not attach the metered stuck state")
		}
	}
}
