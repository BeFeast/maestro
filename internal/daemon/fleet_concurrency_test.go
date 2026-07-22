package daemon

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/state"
)

type fleetConcurrencyTestStore struct {
	settings config.FleetConcurrencySettings
	err      error
}

func (s *fleetConcurrencyTestStore) LoadAll(context.Context) ([]*config.Config, error) {
	return nil, nil
}

func (s *fleetConcurrencyTestStore) FleetConcurrencySettings(context.Context) (config.FleetConcurrencySettings, error) {
	if s.err != nil {
		return config.FleetConcurrencySettings{}, s.err
	}
	return s.settings, nil
}

func saveFleetRunningState(t *testing.T, dir string, running int) {
	t.Helper()
	st := state.NewState()
	for i := 0; i < running; i++ {
		slot := fmt.Sprintf("slot-%d", i+1)
		st.Sessions[slot] = &state.Session{Status: state.StatusRunning, IssueNumber: i + 1}
	}
	if err := state.Save(dir, st); err != nil {
		t.Fatalf("Save: %v", err)
	}
}

func TestFleetSpawnLimiterReservationPreventsBatchAndCrossFlowOvershoot(t *testing.T) {
	store := &fleetConcurrencyTestStore{settings: config.FleetConcurrencySettings{MinLiveWorkers: 5, MaxLiveWorkers: 10}}
	limiter := newFleetSpawnLimiter(store)
	dirA := t.TempDir()
	dirB := t.TempDir()
	saveFleetRunningState(t, dirA, 9)
	saveFleetRunningState(t, dirB, 0)
	limiter.RegisterStateDir(dirA)
	limiter.RegisterStateDir(dirB)

	commit, _, ok := limiter.Reserve(dirB)
	if !ok {
		t.Fatal("first reservation should consume the final fleet slot")
	}
	commit("slot-new")
	if _, _, ok := limiter.Reserve(dirA); ok {
		t.Fatal("second reservation exceeded fleet.max_live_workers")
	}
	if !limiter.CeilingReached() {
		t.Fatal("ceiling should include a successful spawn not yet visible on disk")
	}
}

func TestFleetSpawnLimiterReconcilesCommittedReservationWithState(t *testing.T) {
	store := &fleetConcurrencyTestStore{settings: config.FleetConcurrencySettings{MinLiveWorkers: 1, MaxLiveWorkers: 2}}
	limiter := newFleetSpawnLimiter(store)
	dir := t.TempDir()
	saveFleetRunningState(t, dir, 0)
	limiter.RegisterStateDir(dir)

	commit, _, ok := limiter.Reserve(dir)
	if !ok {
		t.Fatal("reservation failed")
	}
	commit("slot-1")
	saveFleetRunningState(t, dir, 1)

	_, release, ok := limiter.Reserve(dir)
	if !ok {
		t.Fatal("persisted worker was double-counted with its committed reservation")
	}
	release()
}

func TestFleetSpawnLimiterFailsClosedWhenStateCannotBeCounted(t *testing.T) {
	store := &fleetConcurrencyTestStore{settings: config.FleetConcurrencySettings{MinLiveWorkers: 5, MaxLiveWorkers: 10}}
	limiter := newFleetSpawnLimiter(store)
	limiter.RegisterStateDir(t.TempDir())
	limiter.loadState = func(string) (*state.State, error) {
		return nil, errors.New("state unavailable")
	}

	if !limiter.CeilingReached() {
		t.Fatal("uncertain fleet state must close the fast gate")
	}
	if _, _, ok := limiter.Reserve("state-dir"); ok {
		t.Fatal("uncertain fleet state must reject atomic reservations")
	}
}
