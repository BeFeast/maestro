package daemon

import (
	"context"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/state"
)

type fleetConcurrencySettingsLoader interface {
	FleetConcurrencySettings(context.Context) (config.FleetConcurrencySettings, error)
}

type fleetSpawnReservation struct {
	stateDir string
	slot     string
}

// fleetSpawnLimiter serializes capacity checks across every project flow. A
// successful spawn remains reserved until its running slot is visible in the
// authoritative state file, closing both the concurrent-flow race and the
// single-flow batch overshoot window.
type fleetSpawnLimiter struct {
	mu           sync.Mutex
	settings     fleetConcurrencySettingsLoader
	stateDirs    map[string]struct{}
	reservations map[uint64]fleetSpawnReservation
	nextID       uint64
	loadState    func(string) (*state.State, error)
}

func newFleetSpawnLimiter(store ConfigLoader) *fleetSpawnLimiter {
	loader, _ := store.(fleetConcurrencySettingsLoader)
	return &fleetSpawnLimiter{
		settings:     loader,
		stateDirs:    make(map[string]struct{}),
		reservations: make(map[uint64]fleetSpawnReservation),
		loadState:    state.Load,
	}
}

func (l *fleetSpawnLimiter) RegisterStateDir(stateDir string) {
	if l == nil || strings.TrimSpace(stateDir) == "" {
		return
	}
	l.mu.Lock()
	l.stateDirs[stateDir] = struct{}{}
	l.mu.Unlock()
}

func (l *fleetSpawnLimiter) UnregisterStateDir(stateDir string) {
	if l == nil || strings.TrimSpace(stateDir) == "" {
		return
	}
	l.mu.Lock()
	delete(l.stateDirs, stateDir)
	for id, reservation := range l.reservations {
		if reservation.stateDir == stateDir {
			delete(l.reservations, id)
		}
	}
	l.mu.Unlock()
}

func (l *fleetSpawnLimiter) settingsLocked() (config.FleetConcurrencySettings, error) {
	if l.settings == nil {
		return config.ResolveFleetConcurrencySettings(nil)
	}
	return l.settings.FleetConcurrencySettings(context.Background())
}

func fleetWorkerKey(stateDir, slot string) string {
	return stateDir + "\x00" + slot
}

func (l *fleetSpawnLimiter) liveLocked() (int, error) {
	dirs := make([]string, 0, len(l.stateDirs))
	for dir := range l.stateDirs {
		dirs = append(dirs, dir)
	}
	sort.Strings(dirs)

	running := make(map[string]struct{})
	for _, dir := range dirs {
		st, err := l.loadState(dir)
		if err != nil {
			return 0, fmt.Errorf("load fleet state %s: %w", dir, err)
		}
		for slot, sess := range st.Sessions {
			if sess != nil && sess.Status == state.StatusRunning {
				running[fleetWorkerKey(dir, slot)] = struct{}{}
			}
		}
	}

	// Once a committed reservation appears as a running state session, the
	// durable state count replaces it. Pending or not-yet-persisted commits stay
	// reserved, so no other flow can consume the same global slot.
	for id, reservation := range l.reservations {
		if reservation.slot == "" {
			continue
		}
		if _, ok := running[fleetWorkerKey(reservation.stateDir, reservation.slot)]; ok {
			delete(l.reservations, id)
		}
	}
	return len(running) + len(l.reservations), nil
}

func (l *fleetSpawnLimiter) CeilingReached() bool {
	if l == nil {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	settings, err := l.settingsLocked()
	if err != nil {
		log.Printf("[daemon] fleet spawn ceiling fail-closed: settings unavailable: %v", err)
		return true
	}
	if settings.MaxLiveWorkers == 0 {
		return false
	}
	live, err := l.liveLocked()
	if err != nil {
		log.Printf("[daemon] fleet spawn ceiling fail-closed: live-worker count unavailable: %v", err)
		return true
	}
	if live >= settings.MaxLiveWorkers {
		log.Printf("[daemon] fleet spawn ceiling reached: live=%d min=%d max=%d", live, settings.MinLiveWorkers, settings.MaxLiveWorkers)
		return true
	}
	return false
}

// FloorStatus reports current live worker count against fleet.min_live_workers.
// below=true means live < min (CRITICAL floor breach, #1106).
func (l *fleetSpawnLimiter) FloorStatus() (live, min, max int, below bool, err error) {
	if l == nil {
		return 0, 0, 0, false, nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	settings, err := l.settingsLocked()
	if err != nil {
		return 0, 0, 0, false, err
	}
	live, err = l.liveLocked()
	if err != nil {
		return 0, settings.MinLiveWorkers, settings.MaxLiveWorkers, false, err
	}
	min = settings.MinLiveWorkers
	max = settings.MaxLiveWorkers
	below = min > 0 && live < min
	return live, min, max, below, nil
}

func (l *fleetSpawnLimiter) Reserve(stateDir string) (commit func(string), release func(), ok bool) {
	if l == nil {
		return func(string) {}, func() {}, true
	}
	l.mu.Lock()
	settings, err := l.settingsLocked()
	if err != nil {
		l.mu.Unlock()
		log.Printf("[daemon] fleet spawn reservation fail-closed: settings unavailable: %v", err)
		return nil, nil, false
	}
	if settings.MaxLiveWorkers == 0 {
		l.mu.Unlock()
		return func(string) {}, func() {}, true
	}
	if strings.TrimSpace(stateDir) == "" {
		l.mu.Unlock()
		log.Printf("[daemon] fleet spawn reservation fail-closed: project state_dir is empty")
		return nil, nil, false
	}
	l.stateDirs[stateDir] = struct{}{}
	live, err := l.liveLocked()
	if err != nil {
		l.mu.Unlock()
		log.Printf("[daemon] fleet spawn reservation fail-closed: live-worker count unavailable: %v", err)
		return nil, nil, false
	}
	if live >= settings.MaxLiveWorkers {
		l.mu.Unlock()
		return nil, nil, false
	}
	l.nextID++
	id := l.nextID
	l.reservations[id] = fleetSpawnReservation{stateDir: stateDir}
	l.mu.Unlock()

	commit = func(slot string) {
		l.mu.Lock()
		reservation, exists := l.reservations[id]
		if exists {
			reservation.slot = strings.TrimSpace(slot)
			l.reservations[id] = reservation
		}
		l.mu.Unlock()
	}
	release = func() {
		l.mu.Lock()
		delete(l.reservations, id)
		l.mu.Unlock()
	}
	return commit, release, true
}

func (d *Daemon) fleetSpawnCeilingReached() bool {
	return d.spawnLimiter != nil && d.spawnLimiter.CeilingReached()
}

func (d *Daemon) reserveFleetSpawn(stateDir string) (func(string), func(), bool) {
	if d.spawnLimiter == nil {
		return func(string) {}, func() {}, true
	}
	return d.spawnLimiter.Reserve(stateDir)
}
