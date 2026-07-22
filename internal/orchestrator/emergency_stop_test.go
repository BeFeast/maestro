package orchestrator

import (
	"testing"

	"github.com/befeast/maestro/internal/github"
	"github.com/befeast/maestro/internal/state"
)

// TestStartNewWorkers_EmergencyStopBlocksNewSpawns pins #840 AC ("flag set →
// spawn gate closed"): while the fleet-wide emergency halt reports active,
// startNewWorkers must not list issues or spawn any worker even with slots and
// ready issues available.
func TestStartNewWorkers_EmergencyStopBlocksNewSpawns(t *testing.T) {
	cfg := cfgWithBackends("claude", "claude")
	issues := []github.Issue{makeIssue(840, "ready issue")}

	o, started, _ := newStartWorkersOrchestrator(cfg, issues)
	listed := false
	o.listOpenIssuesFn = func(labelFilter []string) ([]github.Issue, error) {
		listed = true
		return issues, nil
	}
	o.SetEmergencyHalt(func() bool { return true })

	o.startNewWorkers(state.NewState(), 5)

	if len(*started) != 0 {
		t.Fatalf("started %d workers under emergency stop, want 0", len(*started))
	}
	if listed {
		t.Fatal("startNewWorkers listed issues under emergency stop; the gate must return before any GitHub call")
	}
}

// TestStartNewWorkers_FleetSpawnCeilingBlocksNewSpawns pins the cross-flow
// live-worker ceiling: when fleet.max_live_workers is saturated, no new
// workers spawn even with ready issues and free project slots.
func TestStartNewWorkers_FleetSpawnCeilingBlocksNewSpawns(t *testing.T) {
	cfg := cfgWithBackends("claude", "claude")
	issues := []github.Issue{makeIssue(1025, "ready issue")}

	o, started, _ := newStartWorkersOrchestrator(cfg, issues)
	listed := false
	o.listOpenIssuesFn = func(labelFilter []string) ([]github.Issue, error) {
		listed = true
		return issues, nil
	}
	o.SetFleetSpawnCeiling(func() bool { return true })

	o.startNewWorkers(state.NewState(), 5)

	if len(*started) != 0 {
		t.Fatalf("started %d workers under fleet ceiling, want 0", len(*started))
	}
	if listed {
		t.Fatal("startNewWorkers listed issues under fleet ceiling; the gate must return before any GitHub call")
	}
}

// TestStartNewWorkers_EmergencyResumeRestoresSpawns confirms that clearing the
// halt (what `maestro emergency resume` does) restores spawning on the next
// cycle — no restart, no extra state surgery.
func TestStartNewWorkers_EmergencyResumeRestoresSpawns(t *testing.T) {
	cfg := cfgWithBackends("claude", "claude")
	issues := []github.Issue{makeIssue(840, "ready issue")}

	o, started, _ := newStartWorkersOrchestrator(cfg, issues)
	halted := true
	o.SetEmergencyHalt(func() bool { return halted })

	o.startNewWorkers(state.NewState(), 5)
	if len(*started) != 0 {
		t.Fatalf("started %d workers while halted, want 0", len(*started))
	}

	halted = false
	o.startNewWorkers(state.NewState(), 5)
	if len(*started) != 1 || (*started)[0] != 840 {
		t.Fatalf("started = %v, want [840] after resume", *started)
	}
}

// TestStartNewWorkers_NilEmergencyHaltIsInert guards the default: an
// orchestrator with no emergency gate wired behaves exactly as before.
func TestStartNewWorkers_NilEmergencyHaltIsInert(t *testing.T) {
	cfg := cfgWithBackends("claude", "claude")
	issues := []github.Issue{makeIssue(840, "ready issue")}

	o, started, _ := newStartWorkersOrchestrator(cfg, issues)

	o.startNewWorkers(state.NewState(), 5)

	if len(*started) != 1 || (*started)[0] != 840 {
		t.Fatalf("started = %v, want [840] with no emergency gate wired", *started)
	}
}
