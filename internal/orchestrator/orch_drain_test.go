package orchestrator

import (
	"testing"
	"time"

	"github.com/befeast/maestro/internal/github"
	"github.com/befeast/maestro/internal/state"
)

// TestStartNewWorkers_DrainBlocksNewSpawns verifies the #541 graceful-drain
// gate: while state.SpawnDrain is set, startNewWorkers must not list issues or
// spawn any worker, even when slots and ready issues are available.
func TestStartNewWorkers_DrainBlocksNewSpawns(t *testing.T) {
	cfg := cfgWithBackends("claude", "claude")
	issues := []github.Issue{makeIssue(541, "ready issue")}

	o, started, _ := newStartWorkersOrchestrator(cfg, issues)
	listed := false
	o.listOpenIssuesFn = func(labelFilter []string) ([]github.Issue, error) {
		listed = true
		return issues, nil
	}

	s := state.NewState()
	s.SetSpawnDrain(time.Now().UTC())

	o.startNewWorkers(s, 5)

	if len(*started) != 0 {
		t.Fatalf("started %d workers while draining, want 0", len(*started))
	}
	if listed {
		t.Fatal("startNewWorkers listed issues while draining; drain must stop claiming work before any GitHub call")
	}
}

// TestStartNewWorkers_NoDrainStillSpawns is a control: without a drain set the
// same wiring spawns the ready issue, proving the gate (not the harness) is
// what blocks spawns in the drain test above.
func TestStartNewWorkers_NoDrainStillSpawns(t *testing.T) {
	cfg := cfgWithBackends("claude", "claude")
	issues := []github.Issue{makeIssue(541, "ready issue")}

	o, started, _ := newStartWorkersOrchestrator(cfg, issues)

	s := state.NewState()
	o.startNewWorkers(s, 5)

	if len(*started) != 1 || (*started)[0] != 541 {
		t.Fatalf("started = %v, want [541] when not draining", *started)
	}
}

// TestClearSpawnDrainOnStartup_ClearsLeftoverFlag verifies that a freshly
// started daemon lifts a leftover drain flag so it resumes normal spawning.
func TestClearSpawnDrainOnStartup_ClearsLeftoverFlag(t *testing.T) {
	cfg := cfgWithBackends("claude", "claude")
	cfg.StateDir = t.TempDir()

	s := state.NewState()
	s.SetSpawnDrain(time.Now().UTC().Add(-time.Minute))
	if err := state.Save(cfg.StateDir, s); err != nil {
		t.Fatalf("save drained state: %v", err)
	}

	o := &Orchestrator{cfg: cfg}
	o.clearSpawnDrainOnStartup()

	reloaded, err := state.Load(cfg.StateDir)
	if err != nil {
		t.Fatalf("reload state: %v", err)
	}
	if reloaded.DrainActive() {
		t.Fatal("drain flag still set after clearSpawnDrainOnStartup")
	}
}

// TestClearSpawnDrainOnStartup_NoopWhenNotDraining verifies the startup clear
// is a no-op (no spurious state write churn) when no drain is set.
func TestClearSpawnDrainOnStartup_NoopWhenNotDraining(t *testing.T) {
	cfg := cfgWithBackends("claude", "claude")
	cfg.StateDir = t.TempDir()

	s := state.NewState()
	if err := state.Save(cfg.StateDir, s); err != nil {
		t.Fatalf("save state: %v", err)
	}

	o := &Orchestrator{cfg: cfg}
	o.clearSpawnDrainOnStartup()

	reloaded, err := state.Load(cfg.StateDir)
	if err != nil {
		t.Fatalf("reload state: %v", err)
	}
	if reloaded.DrainActive() {
		t.Fatal("drain unexpectedly active after no-op startup clear")
	}
}
