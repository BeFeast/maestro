package daemon

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/supervisor"
	"github.com/google/uuid"
)

type auxiliaryTestStore struct {
	fleetConcurrencyTestStore
	dirs map[string]bool
}

func (s *auxiliaryTestStore) RememberAuxiliaryStateDir(_ context.Context, dir string) error {
	if s.dirs == nil {
		s.dirs = map[string]bool{}
	}
	s.dirs[dir] = true
	return nil
}
func (s *auxiliaryTestStore) AuxiliaryStateDirs(context.Context) ([]string, error) {
	var dirs []string
	for dir := range s.dirs {
		dirs = append(dirs, dir)
	}
	return dirs, nil
}

func auxLaunch(t *testing.T, dir, id string) {
	t.Helper()
	root := filepath.Join(dir, "supervisor-consultations")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(map[string]any{"identity": supervisor.ConsultationIdentity{ID: id, Role: "supervisor"}, "intent_id": uuid.NewString()})
	if err := os.WriteFile(filepath.Join(root, "launch.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
}
func TestAuxiliaryCeilingContentionRestartAndWorkerFloor(t *testing.T) {
	store := &auxiliaryTestStore{fleetConcurrencyTestStore: fleetConcurrencyTestStore{settings: config.FleetConcurrencySettings{MinLiveWorkers: 2, MaxLiveWorkers: 3, MaxAuxiliaryRuns: 2}}}
	dirs := []string{t.TempDir(), t.TempDir()}
	for _, dir := range dirs {
		if err := os.Chmod(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	newLimiter := func() *fleetSpawnLimiter {
		l := newFleetSpawnLimiter(store)
		for _, dir := range dirs {
			saveFleetRunningState(t, dir, 1)
			l.RegisterStateDir(dir)
		}
		return l
	}
	l := newLimiter()
	var wg sync.WaitGroup
	winners := make(chan struct {
		dir, id string
		release func()
	}, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := uuid.NewString()
			dir := dirs[i%2]
			release, err := l.ReserveAuxiliary(dir, id)
			if err == nil {
				winners <- struct {
					dir, id string
					release func()
				}{dir, id, release}
			}
		}(i)
	}
	wg.Wait()
	close(winners)
	if len(winners) != 2 {
		t.Fatalf("aux permits=%d, want2", len(winners))
	}
	live, min, max, below, err := l.FloorStatus()
	if err != nil || live != 2 || min != 2 || max != 3 || below {
		t.Fatalf("aux changed worker floor: %d %d %d %t %v", live, min, max, below, err)
	}
	// Simulate one known terminal completion and one unresolved launch. The
	// durable marker replaces the in-memory permit after controller restart.
	n := 0
	for w := range winners {
		if n == 0 {
			auxLaunch(t, w.dir, w.id)
		}
		w.release()
		n++
	}
	restarted := newLimiter()
	release, err := restarted.ReserveAuxiliary(dirs[0], uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := restarted.ReserveAuxiliary(dirs[1], uuid.NewString()); err == nil {
		t.Fatal("unresolved durable auxiliary launch lost after restart")
	}
	release()
}
func TestAuxiliaryReleaseAndReceiptFailureHold(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	store := &auxiliaryTestStore{fleetConcurrencyTestStore: fleetConcurrencyTestStore{settings: config.FleetConcurrencySettings{MaxAuxiliaryRuns: 1}}}
	l := newFleetSpawnLimiter(store)
	l.RegisterStateDir(dir)
	release, err := l.ReserveAuxiliary(dir, uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	release()
	release, err = l.ReserveAuxiliary(dir, uuid.NewString())
	if err != nil {
		t.Fatal("failed prelaunch did not release", err)
	}
	release()
	root := filepath.Join(dir, "supervisor-consultations")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "current.json"), []byte("broken"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := l.ReserveAuxiliary(dir, uuid.NewString()); err == nil {
		t.Fatal("unreadable occupancy admitted new run")
	}
}
