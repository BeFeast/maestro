package daemon

import (
	"fmt"
	"testing"
	"time"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/state"
)

func TestEvaluateFloorBreach_ConfirmsThenRecovers(t *testing.T) {
	dir := t.TempDir()
	if err := state.Save(dir, state.NewState()); err != nil {
		t.Fatal(err)
	}
	store := &fleetConcurrencyTestStore{settings: config.FleetConcurrencySettings{MinLiveWorkers: 5, MaxLiveWorkers: 10}}
	limiter := newFleetSpawnLimiter(store)
	limiter.RegisterStateDir(dir)

	d := &Daemon{spawnLimiter: limiter}
	streak := 0
	d.evaluateFloorBreach(&streak)
	if streak != 1 {
		t.Fatalf("streak after first sample = %d, want 1", streak)
	}
	d.evaluateFloorBreach(&streak)
	if streak != 2 {
		t.Fatalf("streak after confirm = %d, want 2", streak)
	}

	running := state.NewState()
	for i := 1; i <= 5; i++ {
		running.Sessions[fmt.Sprintf("sup-%d", i)] = &state.Session{IssueNumber: i, Status: state.StatusRunning, StartedAt: time.Now().UTC()}
	}
	if err := state.Save(dir, running); err != nil {
		t.Fatal(err)
	}
	d.evaluateFloorBreach(&streak)
	if streak != 0 {
		t.Fatalf("streak after recover = %d, want 0", streak)
	}
}

func TestFloorStatus_BelowMin(t *testing.T) {
	dir := t.TempDir()
	if err := state.Save(dir, state.NewState()); err != nil {
		t.Fatal(err)
	}
	store := &fleetConcurrencyTestStore{settings: config.FleetConcurrencySettings{MinLiveWorkers: 5, MaxLiveWorkers: 10}}
	limiter := newFleetSpawnLimiter(store)
	limiter.RegisterStateDir(dir)
	live, min, max, below, err := limiter.FloorStatus()
	if err != nil {
		t.Fatal(err)
	}
	if live != 0 || min != 5 || max != 10 || !below {
		t.Fatalf("FloorStatus = live=%d min=%d max=%d below=%v", live, min, max, below)
	}
}
