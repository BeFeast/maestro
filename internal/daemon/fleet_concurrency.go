package daemon

import (
	"context"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"time"

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

type fleetProjectRegistration struct {
	repo              string
	queueSignalMaxAge time.Duration
}

// fleetSpawnLimiter serializes capacity checks across every project flow. A
// successful spawn remains reserved until its running slot is visible in the
// authoritative state file, closing both the concurrent-flow race and the
// single-flow batch overshoot window.
type fleetSpawnLimiter struct {
	mu           sync.Mutex
	settings     fleetConcurrencySettingsLoader
	stateDirs    map[string]struct{}
	projects     map[string]fleetProjectRegistration
	reservations map[uint64]fleetSpawnReservation
	nextID       uint64
	loadState    func(string) (*state.State, error)
}

func newFleetSpawnLimiter(store ConfigLoader) *fleetSpawnLimiter {
	loader, _ := store.(fleetConcurrencySettingsLoader)
	return &fleetSpawnLimiter{
		settings:     loader,
		stateDirs:    make(map[string]struct{}),
		projects:     make(map[string]fleetProjectRegistration),
		reservations: make(map[uint64]fleetSpawnReservation),
		loadState:    state.Load,
	}
}

func (l *fleetSpawnLimiter) RegisterStateDir(stateDir string) {
	l.RegisterProject(stateDir, "", DefaultSuperviseInterval)
}

func (l *fleetSpawnLimiter) RegisterProject(stateDir, repo string, superviseInterval time.Duration) {
	if l == nil || strings.TrimSpace(stateDir) == "" {
		return
	}
	stateDir = strings.TrimSpace(stateDir)
	if superviseInterval <= 0 {
		superviseInterval = DefaultSuperviseInterval
	}
	l.mu.Lock()
	l.stateDirs[stateDir] = struct{}{}
	l.projects[stateDir] = fleetProjectRegistration{
		repo:              strings.TrimSpace(repo),
		queueSignalMaxAge: 2*superviseInterval + time.Minute,
	}
	l.mu.Unlock()
}

func (l *fleetSpawnLimiter) UnregisterStateDir(stateDir string) {
	if l == nil || strings.TrimSpace(stateDir) == "" {
		return
	}
	l.mu.Lock()
	delete(l.stateDirs, stateDir)
	delete(l.projects, stateDir)
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

func (l *fleetSpawnLimiter) runningLocked() (map[string]struct{}, error) {
	dirs := make([]string, 0, len(l.stateDirs))
	for dir := range l.stateDirs {
		dirs = append(dirs, dir)
	}
	sort.Strings(dirs)

	running := make(map[string]struct{})
	for _, dir := range dirs {
		st, err := l.loadState(dir)
		if err != nil {
			return nil, fmt.Errorf("load fleet state %s: %w", dir, err)
		}
		for slot, sess := range st.Sessions {
			if sess != nil && sess.Status == state.StatusRunning {
				running[fleetWorkerKey(dir, slot)] = struct{}{}
			}
		}
	}
	return running, nil
}

func (l *fleetSpawnLimiter) reconcileReservationsLocked(running map[string]struct{}) {
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
}

func (l *fleetSpawnLimiter) liveLocked() (int, error) {
	running, err := l.runningLocked()
	if err != nil {
		return 0, err
	}
	l.reconcileReservationsLocked(running)
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

// FloorStatus reports the durable StatusRunning count against
// fleet.min_live_workers. Pending spawn reservations still protect the hard
// ceiling but are not live workers and cannot mask a CRITICAL floor breach.
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
	running, err := l.runningLocked()
	if err != nil {
		return 0, settings.MinLiveWorkers, settings.MaxLiveWorkers, false, err
	}
	l.reconcileReservationsLocked(running)
	live = len(running)
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
	deferReason, err := l.dogfoodDeferralReasonLocked(stateDir, settings, live)
	if err != nil {
		l.mu.Unlock()
		log.Printf("[daemon] fleet spawn reservation fail-closed: product queue state unavailable: %v", err)
		return nil, nil, false
	}
	if deferReason != "" {
		l.mu.Unlock()
		log.Printf("[daemon] fleet spawn reservation deferred for %s: %s", factoryDogfoodRepo, deferReason)
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

const factoryDogfoodRepo = "BeFeast/maestro"

// dogfoodDeferralReasonLocked keeps the Maestro dogfood project in the
// background when product work can use the same fleet slot. It is deliberately
// narrow: no new priority scheduler or operator knob, and no preemption. The
// existing persisted supervisor decision supplies the runnable-backlog signal.
func (l *fleetSpawnLimiter) dogfoodDeferralReasonLocked(stateDir string, settings config.FleetConcurrencySettings, live int) (string, error) {
	registration, ok := l.projects[stateDir]
	if !ok || !strings.EqualFold(registration.repo, factoryDogfoodRepo) {
		return "", nil
	}

	dirs := make([]string, 0, len(l.projects))
	for dir, project := range l.projects {
		if dir == stateDir || strings.TrimSpace(project.repo) == "" || strings.EqualFold(project.repo, factoryDogfoodRepo) {
			continue
		}
		dirs = append(dirs, dir)
	}
	sort.Strings(dirs)

	for _, dir := range dirs {
		project := l.projects[dir]
		st, err := l.loadState(dir)
		if err != nil {
			return "", fmt.Errorf("load product state %s: %w", dir, err)
		}
		decision := st.LatestSupervisorDecision()
		if !freshSupervisorQueueSignal(decision, project.queueSignalMaxAge, time.Now().UTC()) ||
			!productQueueCanDispatch(st, decision) || !hasUnclaimedRunnableCandidate(st, decision) {
			continue
		}
		productLive := st.RunningSessionCount()
		if settings.MinLiveWorkers > 0 && live < settings.MinLiveWorkers {
			return fmt.Sprintf("fleet is below floor (live=%d min=%d) and product %s has runnable backlog", live, settings.MinLiveWorkers, project.repo), nil
		}
		if productLive == 0 {
			return fmt.Sprintf("product %s has runnable backlog and no live worker", project.repo), nil
		}
	}
	return "", nil
}

func freshSupervisorQueueSignal(decision *state.SupervisorDecision, maxAge time.Duration, now time.Time) bool {
	if decision == nil || decision.RecommendationDropped() || maxAge <= 0 {
		return false
	}
	observedAt := decision.CreatedAt
	if decision.LastSeenAt.After(observedAt) {
		observedAt = decision.LastSeenAt
	}
	if observedAt.IsZero() {
		return false
	}
	return !now.After(observedAt.UTC().Add(maxAge))
}

func productQueueCanDispatch(st *state.State, decision *state.SupervisorDecision) bool {
	if st == nil || decision == nil {
		return false
	}
	if st.PauseActive() || st.DrainActive() || decision.RequiresApproval {
		return false
	}
	if st.DispatchHold.Active && st.DispatchHold.ReasonClass != state.DispatchHoldAwaitingDispatch {
		return false
	}
	if decision.ProjectState.AvailableSlots > 0 {
		return true
	}
	// A decision recorded at local capacity becomes dispatchable after one of
	// those workers exits, even before the next supervisor cycle refreshes it.
	return st.RunningSessionCount() < decision.ProjectState.Running
}

func hasUnclaimedRunnableCandidate(st *state.State, decision *state.SupervisorDecision) bool {
	if st == nil || decision == nil || decision.QueueAnalysis == nil || decision.QueueAnalysis.EligibleCandidates <= 0 {
		return false
	}
	analysis := decision.QueueAnalysis
	candidates := analysis.EligibleRanked
	if len(candidates) == 0 && analysis.SelectedCandidate != nil {
		candidates = []state.SupervisorIssueCandidate{*analysis.SelectedCandidate}
	}
	for _, candidate := range candidates {
		if candidate.Number > 0 && !st.IssueInProgress(candidate.Number) && !st.IssueDone(candidate.Number) {
			return true
		}
	}
	// EligibleRanked is bounded. A larger aggregate means unlisted runnable
	// candidates remain even when every persisted identity is already claimed.
	return analysis.EligibleCandidates > len(candidates)
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
