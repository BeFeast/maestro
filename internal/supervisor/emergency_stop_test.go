package supervisor

import (
	"context"
	"testing"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/github"
	"github.com/befeast/maestro/internal/state"
)

// validSpawnLLM is an LLM decision that agrees with the deterministic guardrail
// for a single ready issue, so the normal (non-emergency) path succeeds and
// records exactly one backend call.
func validSpawnLLM() *fakeLLM {
	return &fakeLLM{output: `{
  "summary": "Issue #42 is ready to feed; no worker is running.",
  "recommended_action": "spawn_worker",
  "target": {"issue": 42},
  "risk": "mutating",
  "confidence": 0.87,
  "reasons": ["ordered queue points to #42", "no active worker"],
  "requires_approval": true
}`}
}

// TestDecide_EmergencyLLMHalt_SkipsLLM pins #840 AC: with the fleet-wide LLM
// stop active, Decide never invokes the supervisor backend (decideWithLLM is not
// reached) and falls back to the deterministic decision — while the SAME engine
// config, with the halt cleared, does call the LLM. Proving both directions in
// one test guards against the gate being wired but inert.
func TestDecide_EmergencyLLMHalt_SkipsLLM(t *testing.T) {
	newSetup := func() (*config.Config, *fakeReader, *fakeLLM) {
		cfg := testConfig(t)
		cfg.IssueLabels = []string{"maestro-ready"}
		reader := &fakeReader{issues: []github.Issue{testIssue(42, "ready work", "maestro-ready")}}
		return cfg, reader, validSpawnLLM()
	}

	// Baseline: LLM enabled, no emergency → the backend is called once.
	cfg, reader, llm := newSetup()
	if _, err := testLLMEngine(cfg, reader, llm).Decide(state.NewState()); err != nil {
		t.Fatalf("baseline Decide: %v", err)
	}
	if llm.calls != 1 {
		t.Fatalf("baseline LLM calls = %d, want 1 (LLM path active without emergency)", llm.calls)
	}

	// Emergency LLM stop active → decideWithLLM must not be reached.
	cfg, reader, llm = newSetup()
	eng := testLLMEngine(cfg, reader, llm)
	eng.SetEmergencyLLMHalt(true)
	decision, err := eng.Decide(state.NewState())
	if err != nil {
		t.Fatalf("emergency Decide: %v", err)
	}
	if llm.calls != 0 {
		t.Fatalf("LLM calls = %d under emergency stop, want 0 (decideWithLLM must not run)", llm.calls)
	}
	if decision.RecommendedAction == "" {
		t.Fatal("emergency Decide produced no deterministic decision")
	}
}

// TestRunOnce_WithEmergencyLLMHalt threads the option through the package-level
// RunOnce (the seam the daemon uses) and confirms the injected LLM client is
// never called even though Supervisor.Enabled is true.
func TestRunOnce_WithEmergencyLLMHalt(t *testing.T) {
	cfg := testConfig(t)
	cfg.Supervisor.Enabled = true
	cfg.IssueLabels = []string{"maestro-ready"}
	cfg.Supervisor.DryRun = true // no side effects / state.Save churn
	reader := &fakeReader{issues: []github.Issue{testIssue(42, "ready work", "maestro-ready")}}

	if err := state.Save(cfg.StateDir, state.NewState()); err != nil {
		t.Fatalf("seed state: %v", err)
	}

	// RunOnce builds its own engine + backend client; with the halt set it must
	// not shell out. A real backend call would fail (no backend configured), so a
	// clean deterministic decision is itself evidence the LLM path was skipped.
	decision, err := RunOnce(context.Background(), cfg, reader, WithEmergencyLLMHalt(true))
	if err != nil {
		t.Fatalf("RunOnce with emergency halt: %v", err)
	}
	if decision.RecommendedAction == "" {
		t.Fatal("RunOnce produced no decision under emergency halt")
	}
}
