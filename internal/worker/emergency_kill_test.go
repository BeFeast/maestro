package worker

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/state"
)

func TestKillRunningSessionsMarksDeadWithoutTouchingFinished(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{StateDir: dir, Repo: "BeFeast/test"}
	now := time.Now().UTC()
	s := &state.State{Sessions: map[string]*state.Session{
		"run-1": {Status: state.StatusRunning, IssueNumber: 1, StartedAt: now},
		"done":  {Status: state.StatusDone, IssueNumber: 2, StartedAt: now, FinishedAt: &now},
	}}
	if err := state.Save(dir, s); err != nil {
		t.Fatal(err)
	}
	killed := KillRunningSessions(cfg)
	if killed != 1 {
		t.Fatalf("killed=%d want 1", killed)
	}
	got, err := state.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got.Sessions["run-1"].Status != state.StatusDead {
		t.Fatalf("run-1 status=%s want dead", got.Sessions["run-1"].Status)
	}
	if got.Sessions["done"].Status != state.StatusDone {
		t.Fatalf("done status mutated: %s", got.Sessions["done"].Status)
	}
	_ = filepath.Base(dir)
}

// A session whose StopProcess fails while its runtime is provably still alive
// must NOT be marked dead: a lying StatusDead makes resume-time redispatch
// spawn a duplicate worker for the same issue while the survivor keeps
// running (#1150).
func TestKillRunningSessionsLeavesAliveSessionRunningOnFailedStop(t *testing.T) {
	survivor := spawnSleeper(t)
	dir := t.TempDir()
	cfg := &config.Config{StateDir: dir, Repo: "BeFeast/test"}
	now := time.Now().UTC()
	s := &state.State{Sessions: map[string]*state.Session{
		// WorkerLeaseID without ProcessLeaseUnit makes StopProcess fail with
		// "worker scratch receipt exists without a durable process lease",
		// while the recorded PID is a live process owned by this test.
		"stuck": {Status: state.StatusRunning, IssueNumber: 3, StartedAt: now,
			PID: survivor.Process.Pid, WorkerLeaseID: "orphaned-receipt"},
	}}
	if err := state.Save(dir, s); err != nil {
		t.Fatal(err)
	}
	if killed := KillRunningSessions(cfg); killed != 0 {
		t.Fatalf("killed=%d want 0 (stop failed, runtime alive)", killed)
	}
	got, err := state.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got.Sessions["stuck"].Status != state.StatusRunning {
		t.Fatalf("stuck status=%s want running (must not lie about a live worker)", got.Sessions["stuck"].Status)
	}
	if !IsAlive(survivor.Process.Pid) {
		t.Fatalf("survivor pid %d unexpectedly dead", survivor.Process.Pid)
	}
}
