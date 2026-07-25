package daemon

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

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

func saveFleetQueueState(t *testing.T, dir string, running, availableSlots int, candidates ...int) {
	t.Helper()
	st := state.NewState()
	for i := 0; i < running; i++ {
		issue := 1000 + i
		st.Sessions[fmt.Sprintf("slot-%d", i+1)] = &state.Session{Status: state.StatusRunning, IssueNumber: issue}
	}
	ranked := make([]state.SupervisorIssueCandidate, 0, len(candidates))
	for _, issue := range candidates {
		ranked = append(ranked, state.SupervisorIssueCandidate{Number: issue})
	}
	decision := state.SupervisorDecision{
		CreatedAt: time.Now().UTC(),
		ProjectState: state.SupervisorProjectState{
			Running:        running,
			AvailableSlots: availableSlots,
		},
		QueueAnalysis: &state.SupervisorQueueAnalysis{
			EligibleCandidates: len(ranked),
			EligibleRanked:     ranked,
		},
	}
	st.SupervisorDecisions = append(st.SupervisorDecisions, decision)
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

func TestFleetSpawnLimiterDefersDogfoodBelowFloorForRunnableProductBacklog(t *testing.T) {
	store := &fleetConcurrencyTestStore{settings: config.FleetConcurrencySettings{MinLiveWorkers: 5, MaxLiveWorkers: 10}}
	limiter := newFleetSpawnLimiter(store)
	dogfoodDir := t.TempDir()
	productDir := t.TempDir()
	saveFleetRunningState(t, dogfoodDir, 0)
	saveFleetQueueState(t, productDir, 0, 2, 394)
	limiter.RegisterProject(dogfoodDir, factoryDogfoodRepo, time.Minute)
	limiter.RegisterProject(productDir, "BeFeast/ok-player", time.Minute)

	if _, _, ok := limiter.Reserve(dogfoodDir); ok {
		t.Fatal("dogfood reservation should defer to runnable product backlog below the fleet floor")
	}
	_, release, ok := limiter.Reserve(productDir)
	if !ok {
		t.Fatal("product reservation should consume the protected slot")
	}
	release()
}

func TestFleetSpawnLimiterDefersDogfoodForStarvedProductAtFloor(t *testing.T) {
	store := &fleetConcurrencyTestStore{settings: config.FleetConcurrencySettings{MinLiveWorkers: 2, MaxLiveWorkers: 4}}
	limiter := newFleetSpawnLimiter(store)
	dogfoodDir := t.TempDir()
	busyProductDir := t.TempDir()
	starvedProductDir := t.TempDir()
	saveFleetRunningState(t, dogfoodDir, 0)
	saveFleetRunningState(t, busyProductDir, 2)
	saveFleetQueueState(t, starvedProductDir, 0, 1, 264)
	limiter.RegisterProject(dogfoodDir, factoryDogfoodRepo, time.Minute)
	limiter.RegisterProject(busyProductDir, "example/busy-product", time.Minute)
	limiter.RegisterProject(starvedProductDir, "BeFeast/ok-folio", time.Minute)

	if _, _, ok := limiter.Reserve(dogfoodDir); ok {
		t.Fatal("dogfood reservation should defer when a runnable product queue has no live worker")
	}
}

func TestFleetSpawnLimiterAllowsDogfoodWhenProductQueueCannotDispatch(t *testing.T) {
	store := &fleetConcurrencyTestStore{settings: config.FleetConcurrencySettings{MinLiveWorkers: 5, MaxLiveWorkers: 10}}
	limiter := newFleetSpawnLimiter(store)
	dogfoodDir := t.TempDir()
	productDir := t.TempDir()
	saveFleetRunningState(t, dogfoodDir, 0)
	saveFleetQueueState(t, productDir, 0, 0, 394)
	limiter.RegisterProject(dogfoodDir, factoryDogfoodRepo, time.Minute)
	limiter.RegisterProject(productDir, "BeFeast/ok-player", time.Minute)

	_, release, ok := limiter.Reserve(dogfoodDir)
	if !ok {
		t.Fatal("dogfood should fill spare capacity when the product queue has no local dispatch slot")
	}
	release()
}

func TestFleetSpawnLimiterAllowsDogfoodWhenProductCandidateIsAlreadyClaimed(t *testing.T) {
	store := &fleetConcurrencyTestStore{settings: config.FleetConcurrencySettings{MinLiveWorkers: 5, MaxLiveWorkers: 10}}
	limiter := newFleetSpawnLimiter(store)
	dogfoodDir := t.TempDir()
	productDir := t.TempDir()
	saveFleetRunningState(t, dogfoodDir, 0)
	saveFleetQueueState(t, productDir, 0, 1, 394)
	productState, err := state.Load(productDir)
	if err != nil {
		t.Fatal(err)
	}
	productState.Sessions["ok-player-1"] = &state.Session{Status: state.StatusRunning, IssueNumber: 394}
	if err := state.Save(productDir, productState); err != nil {
		t.Fatal(err)
	}
	limiter.RegisterProject(dogfoodDir, factoryDogfoodRepo, time.Minute)
	limiter.RegisterProject(productDir, "BeFeast/ok-player", time.Minute)

	_, release, ok := limiter.Reserve(dogfoodDir)
	if !ok {
		t.Fatal("a stale product queue candidate should not suppress dogfood after it has a durable claim")
	}
	release()
}

func TestFleetSpawnLimiterAllowsDogfoodWhenProductDispatchIsHeld(t *testing.T) {
	store := &fleetConcurrencyTestStore{settings: config.FleetConcurrencySettings{MinLiveWorkers: 5, MaxLiveWorkers: 10}}
	limiter := newFleetSpawnLimiter(store)
	dogfoodDir := t.TempDir()
	productDir := t.TempDir()
	saveFleetRunningState(t, dogfoodDir, 0)
	saveFleetQueueState(t, productDir, 0, 1, 394)
	productState, err := state.Load(productDir)
	if err != nil {
		t.Fatal(err)
	}
	productState.DispatchHold = state.DispatchHold{
		Active:      true,
		ReasonClass: state.DispatchHoldBackendsCoolingDown,
	}
	if err := state.Save(productDir, productState); err != nil {
		t.Fatal(err)
	}
	limiter.RegisterProject(dogfoodDir, factoryDogfoodRepo, time.Minute)
	limiter.RegisterProject(productDir, "BeFeast/ok-player", time.Minute)

	_, release, ok := limiter.Reserve(dogfoodDir)
	if !ok {
		t.Fatal("backend-held product work is not runnable and should not suppress dogfood fill")
	}
	release()
}

func TestFleetSpawnLimiterAllowsDogfoodWhenProductQueueSignalIsStale(t *testing.T) {
	store := &fleetConcurrencyTestStore{settings: config.FleetConcurrencySettings{MinLiveWorkers: 5, MaxLiveWorkers: 10}}
	limiter := newFleetSpawnLimiter(store)
	dogfoodDir := t.TempDir()
	productDir := t.TempDir()
	saveFleetRunningState(t, dogfoodDir, 0)
	saveFleetQueueState(t, productDir, 0, 1, 394)
	productState, err := state.Load(productDir)
	if err != nil {
		t.Fatal(err)
	}
	productState.SupervisorDecisions[0].CreatedAt = time.Now().UTC().Add(-10 * time.Minute)
	if err := state.Save(productDir, productState); err != nil {
		t.Fatal(err)
	}
	limiter.RegisterProject(dogfoodDir, factoryDogfoodRepo, time.Minute)
	limiter.RegisterProject(productDir, "BeFeast/ok-player", time.Minute)

	_, release, ok := limiter.Reserve(dogfoodDir)
	if !ok {
		t.Fatal("historical queue analysis must not become a new dogfood freeze")
	}
	release()
}
