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
