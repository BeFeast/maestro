package daemon

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/state"
)

// This fixture exercises capacity ownership, not real worker/provider launches.
// Each product's max_parallel=1 is enforced separately by its orchestrator.
func TestReturnPilotTwoProductsAndBoundedCandidate(t *testing.T) {
	store := &fleetConcurrencyTestStore{settings: config.FleetConcurrencySettings{MinLiveWorkers: 2, MaxLiveWorkers: 3}}
	products := []struct {
		repo string
		dir  string
	}{{"BeFeast/hedroom", t.TempDir()}, {"BeFeast/halenote", t.TempDir()}}
	candidate := t.TempDir()
	saveFleetQueueState(t, candidate, 0, 1, 900)
	for _, p := range products {
		saveFleetQueueState(t, p.dir, 0, 1, 1000)
	}
	newLimiter := func() *fleetSpawnLimiter {
		l := newFleetSpawnLimiter(store)
		for _, p := range products {
			l.RegisterProject(p.dir, p.repo, time.Minute)
		}
		l.RegisterProject(candidate, factoryDogfoodRepo, time.Minute)
		return l
	}
	limiter := newLimiter()
	if _, release, ok := limiter.Reserve(candidate); ok {
		release()
		t.Fatal("candidate took capacity before runnable products")
	}

	// Both project flows race for their first slot. Pending permits count
	// against the fleet ceiling before either session is durable.
	var wg sync.WaitGroup
	admitted := make(chan string, len(products))
	for _, p := range products {
		wg.Add(1)
		go func(dir string) {
			defer wg.Done()
			commit, _, ok := limiter.Reserve(dir)
			if ok {
				commit("slot-1")
				admitted <- dir
			}
		}(p.dir)
	}
	wg.Wait()
	close(admitted)
	if len(admitted) != 2 {
		t.Fatalf("product reservations=%d, want 2", len(admitted))
	}
	if _, release, ok := limiter.Reserve(candidate); ok {
		release()
		t.Fatal("candidate ran while product launches were still unconfirmed")
	}
	for dir := range admitted {
		saveFleetQueueState(t, dir, 1, 0, 1000)
	}

	// A failed candidate spawn releases its permit. Parallel attempts at the
	// remaining slot admit exactly one winner, without preempting products.
	_, release, ok := limiter.Reserve(candidate)
	if !ok {
		t.Fatal("candidate denied after both products became durable")
	}
	release()
	winners := make(chan func(string), 16)
	for i := 0; i < cap(winners); i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			commit, _, ok := limiter.Reserve(candidate)
			if ok {
				winners <- commit
			}
		}()
	}
	wg.Wait()
	close(winners)
	if len(winners) != 1 {
		t.Fatalf("candidate permits=%d, want exactly 1", len(winners))
	}
	for commit := range winners {
		commit("slot-1")
	}
	saveFleetQueueState(t, candidate, 1, 0, 900)
	for _, l := range []*fleetSpawnLimiter{limiter, newLimiter()} {
		if !l.CeilingReached() {
			t.Fatal("three durable workers did not occupy the fleet ceiling")
		}
		for _, dir := range []string{products[0].dir, products[1].dir, candidate} {
			if _, release, ok := l.Reserve(dir); ok {
				release()
				t.Fatal("extra worker admitted across durable restart")
			}
		}
	}

	// One confirmed terminal worker is held. Its neighbor remains running;
	// the empty capacity remains available to permitted work after restart.
	failed, err := state.Load(products[0].dir)
	if err != nil {
		t.Fatal(err)
	}
	failed.Sessions["slot-1"].Status = state.StatusFailed
	failed.DispatchHold = state.DispatchHold{Active: true, ReasonClass: state.DispatchHoldBackendsCoolingDown}
	if err := state.Save(products[0].dir, failed); err != nil {
		t.Fatal(err)
	}
	limiter = newLimiter()
	_, release, ok = limiter.Reserve(products[1].dir)
	if !ok {
		t.Fatal("terminal held product retained a global capacity slot")
	}
	release() // Capacity proof only: the neighbor's local cap still applies.
	for _, dir := range []string{products[1].dir, candidate} {
		st, err := state.Load(dir)
		if err != nil || st.RunningSessionCount() != 1 {
			t.Fatalf("neighbor changed: state=%v, error=%v", st, err)
		}
	}
}

func TestReturnPilotHeldProductDoesNotFreezeRunnableNeighbor(t *testing.T) {
	for _, heldIndex := range []int{0, 1} {
		t.Run(fmt.Sprintf("held-product-%d", heldIndex), func(t *testing.T) {
			store := &fleetConcurrencyTestStore{settings: config.FleetConcurrencySettings{MinLiveWorkers: 2, MaxLiveWorkers: 3}}
			limiter := newFleetSpawnLimiter(store)
			dirs := []string{t.TempDir(), t.TempDir()}
			for i, repo := range []string{"BeFeast/hedroom", "BeFeast/halenote"} {
				saveFleetQueueState(t, dirs[i], 0, 1, 1000)
				limiter.RegisterProject(dirs[i], repo, time.Minute)
			}
			held, err := state.Load(dirs[heldIndex])
			if err != nil {
				t.Fatal(err)
			}
			held.DispatchHold = state.DispatchHold{Active: true, ReasonClass: state.DispatchHoldBackendsCoolingDown}
			if err := state.Save(dirs[heldIndex], held); err != nil {
				t.Fatal(err)
			}
			candidate := t.TempDir()
			saveFleetRunningState(t, candidate, 0)
			limiter.RegisterProject(candidate, factoryDogfoodRepo, time.Minute)
			if _, release, ok := limiter.Reserve(candidate); ok {
				release()
				t.Fatal("candidate displaced the remaining runnable product")
			}
			ready := dirs[1-heldIndex]
			commit, _, ok := limiter.Reserve(ready)
			if !ok {
				t.Fatal("one product's hold froze its neighbor")
			}
			commit("slot-1")
			saveFleetQueueState(t, ready, 1, 0, 1000)
			_, release, ok := limiter.Reserve(candidate)
			if !ok {
				t.Fatal("held product froze spare candidate capacity")
			}
			release()
		})
	}
}
