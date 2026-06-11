package orchestrator

import (
	"testing"
	"time"

	"github.com/befeast/maestro/internal/github"
	"github.com/befeast/maestro/internal/state"
)

// TestStartNewWorkers_PausedBlocksNewSpawns verifies the #683 operator-pause
// gate: while state.Paused is set, startNewWorkers must not list issues or
// spawn any worker, even when slots and ready issues are available.
func TestStartNewWorkers_PausedBlocksNewSpawns(t *testing.T) {
	cfg := cfgWithBackends("claude", "claude")
	issues := []github.Issue{makeIssue(683, "ready issue")}

	o, started, _ := newStartWorkersOrchestrator(cfg, issues)
	listed := false
	o.listOpenIssuesFn = func(labelFilter []string) ([]github.Issue, error) {
		listed = true
		return issues, nil
	}

	s := state.NewState()
	s.SetPaused(time.Now().UTC())

	o.startNewWorkers(s, 5)

	if len(*started) != 0 {
		t.Fatalf("started %d workers while paused, want 0", len(*started))
	}
	if listed {
		t.Fatal("startNewWorkers listed issues while paused; pause must skip issue selection before any GitHub call")
	}
}

// TestStartNewWorkers_ResumeRestoresSpawns verifies that clearing the pause
// flag (what `maestro resume` does) restores issue selection on the very next
// cycle — no restart, no extra state surgery.
func TestStartNewWorkers_ResumeRestoresSpawns(t *testing.T) {
	cfg := cfgWithBackends("claude", "claude")
	issues := []github.Issue{makeIssue(683, "ready issue")}

	o, started, _ := newStartWorkersOrchestrator(cfg, issues)

	s := state.NewState()
	s.SetPaused(time.Now().UTC().Add(-time.Minute))
	s.ClearPaused(time.Now().UTC())

	o.startNewWorkers(s, 5)

	if len(*started) != 1 || (*started)[0] != 683 {
		t.Fatalf("started = %v, want [683] after resume", *started)
	}
}

// TestStartNewWorkers_PausedDoesNotTouchInFlightWorker verifies #683 AC2: a
// worker that is running at pause time is left alone — pause only stops new
// selection; it never kills or drains the in-flight session.
func TestStartNewWorkers_PausedDoesNotTouchInFlightWorker(t *testing.T) {
	cfg := cfgWithBackends("claude", "claude")
	issues := []github.Issue{makeIssue(683, "ready issue")}

	o, started, _ := newStartWorkersOrchestrator(cfg, issues)

	s := state.NewState()
	s.Sessions["prj-1"] = &state.Session{Status: state.StatusRunning, IssueNumber: 600}
	s.SetPaused(time.Now().UTC())

	o.startNewWorkers(s, 5)

	if len(*started) != 0 {
		t.Fatalf("started %d workers while paused, want 0", len(*started))
	}
	sess := s.Sessions["prj-1"]
	if sess == nil || sess.Status != state.StatusRunning {
		t.Fatalf("in-flight session mutated under pause: %+v", sess)
	}
}

// TestClearSpawnDrainOnStartup_LeavesPauseSet verifies the restart contract
// (#683 AC4): the startup drain clear must not lift an operator pause — only
// `maestro resume` does. Pause persists across `systemctl --user restart`.
func TestClearSpawnDrainOnStartup_LeavesPauseSet(t *testing.T) {
	cfg := cfgWithBackends("claude", "claude")
	cfg.StateDir = t.TempDir()

	s := state.NewState()
	s.SetSpawnDrain(time.Now().UTC().Add(-time.Minute))
	s.SetPaused(time.Now().UTC().Add(-time.Minute))
	if err := state.Save(cfg.StateDir, s); err != nil {
		t.Fatalf("save paused+drained state: %v", err)
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
	if !reloaded.PauseActive() {
		t.Fatal("pause flag lost across startup; pause must survive a unit restart")
	}
}
