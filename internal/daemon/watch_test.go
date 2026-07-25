package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/state"
)

// fakeWatchStore is an in-memory store that satisfies both ConfigLoader (so it
// can be handed to New) and configwatch.ProjectStore (so the daemon enables the
// diff-loop). Each Set bumps the project's updated_at, mirroring UpsertProject.
type fakeWatchStore struct {
	mu     sync.Mutex
	cfgs   map[string]*config.Config
	stamps map[string]time.Time
	clock  time.Time
}

func newFakeWatchStore() *fakeWatchStore {
	return &fakeWatchStore{
		cfgs:   map[string]*config.Config{},
		stamps: map[string]time.Time{},
		clock:  time.Unix(1_700_000_000, 0).UTC(),
	}
}

func (f *fakeWatchStore) Set(name string, cfg *config.Config) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.clock = f.clock.Add(time.Second)
	f.cfgs[name] = cfg
	f.stamps[name] = f.clock
}

func (f *fakeWatchStore) Delete(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.cfgs, name)
	delete(f.stamps, name)
}

func (f *fakeWatchStore) Load(ctx context.Context, name string) (*config.Config, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	cfg, ok := f.cfgs[name]
	if !ok {
		return nil, context.Canceled
	}
	return cfg, nil
}

func (f *fakeWatchStore) LoadAll(ctx context.Context) ([]*config.Config, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]*config.Config, 0, len(f.cfgs))
	for _, cfg := range f.cfgs {
		out = append(out, cfg)
	}
	return out, nil
}

func (f *fakeWatchStore) ProjectsFingerprint(ctx context.Context) (map[string]time.Time, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make(map[string]time.Time, len(f.stamps))
	for k, v := range f.stamps {
		out[k] = v
	}
	return out, nil
}

func newWatchDaemon(store ConfigLoader, run func(context.Context, *config.Config, Options, <-chan *config.Config), supervise func(context.Context, string, func() *config.Config, Options)) *Daemon {
	d := New(store, Options{Host: "127.0.0.1", Port: 0, WatchStore: true, WatchStoreInterval: 25 * time.Millisecond})
	if run != nil {
		d.runLoop = run
	}
	if supervise != nil {
		d.superviseLoop = supervise
	}
	d.watchdogLoop = func(ctx context.Context, name, stateDir string, interval time.Duration) {}
	return d
}

func fleetProjectNames(t *testing.T, d *Daemon) []string {
	t.Helper()
	fleet := waitForFleet(t, d)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/fleet", nil)
	rec := httptest.NewRecorder()
	fleet.HandlerForTest().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/fleet = %d, want 200", rec.Code)
	}
	var resp struct {
		Projects []struct {
			Name string `json:"name"`
		} `json:"projects"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode fleet response: %v", err)
	}
	names := make([]string, 0, len(resp.Projects))
	for _, p := range resp.Projects {
		names = append(names, p.Name)
	}
	return names
}

// The diff-loop must add a flow for a project that appears in the store and
// drain one for a project that disappears — both reflected in /api/v1/fleet
// without a daemon restart (#757 acceptance).
func TestWatchStoreLoopHotAddsAndRemoves(t *testing.T) {
	store := newFakeWatchStore()
	store.Set("alpha", testConfig(t, "owner/alpha"))

	var run, sup loopTracker
	d := newWatchDaemon(store, run.loop, sup.superviseLoop)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()

	// Initial project is present.
	waitForNames(t, d, "alpha")
	if got := atomic.LoadInt64(&run.started); got != 1 {
		t.Fatalf("run loops started = %d, want 1", got)
	}

	// Hot-add beta.
	store.Set("beta", testConfig(t, "owner/beta"))
	waitForNames(t, d, "alpha", "beta")
	waitFor(t, func() bool { return atomic.LoadInt64(&run.started) == 2 })

	// Hot-remove alpha; beta survives, other flows untouched.
	store.Delete("alpha")
	waitForNames(t, d, "beta")
	waitFor(t, func() bool { return atomic.LoadInt64(&run.stopped) >= 1 })

	d.mu.Lock()
	flows := len(d.flows)
	d.mu.Unlock()
	if flows != 1 {
		t.Fatalf("flows = %d, want 1 after hot-remove", flows)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after ctx cancellation")
	}
}

func TestWatchStoreEmptyFleetWaitsForFirstProject(t *testing.T) {
	store := newFakeWatchStore()
	var run, sup loopTracker
	d := newWatchDaemon(store, run.loop, sup.superviseLoop)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()

	if names := fleetProjectNames(t, d); len(names) != 0 {
		t.Fatalf("initial fleet projects = %v, want empty", names)
	}
	if got := atomic.LoadInt64(&run.started); got != 0 {
		t.Fatalf("flows started before first row = %d, want 0", got)
	}

	store.Set("alpha", testConfig(t, "owner/alpha"))
	waitForNames(t, d, "alpha")
	waitFor(t, func() bool { return atomic.LoadInt64(&run.started) == 1 })

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after cancellation")
	}
}

// A store-backed flow must receive a non-nil reload channel so an edited config
// row hot-reloads its orchestrator (#757). A nil channel would silently disable
// reload.
func TestWatchStoreWiresReloadChannel(t *testing.T) {
	store := newFakeWatchStore()
	store.Set("alpha", testConfig(t, "owner/alpha"))

	var gotReload int32 // 1 if the run loop saw a non-nil reload channel
	run := func(ctx context.Context, cfg *config.Config, opts Options, reloadCh <-chan *config.Config) {
		if reloadCh != nil {
			atomic.StoreInt32(&gotReload, 1)
		}
		<-ctx.Done()
	}
	sup := func(ctx context.Context, name string, getCfg func() *config.Config, opts Options) { <-ctx.Done() }
	d := newWatchDaemon(store, run, sup)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()

	waitForFleet(t, d)
	waitFor(t, func() bool { return atomic.LoadInt32(&gotReload) == 1 })

	cancel()
	<-done
}

// #884: a config-store write that changes only max_live_workers must reach the
// already-running orchestrator loop within one watch interval. The same flow
// must survive both an effective no-op write and the capacity reconfiguration:
// cancelling it would stop every in-flight worker owned by that flow.
func TestWatchStoreMaxLiveWorkersOnlyReconfiguresRunningOrchestrator(t *testing.T) {
	store := newFakeWatchStore()
	cfg := testConfig(t, "owner/alpha")
	cfg.MaxParallel = 1
	store.Set("alpha", cfg)

	capacityState := state.NewState()
	capacityState.Sessions["gate"] = &state.Session{Status: state.StatusPROpen}

	var started, stopped, reloads int64
	var availableSlots, separated int64
	publishCapacity := func(current *config.Config) {
		cap := capacityState.Capacity(state.CapacityInput{
			MaxParallel:          current.MaxParallel,
			MaxLiveWorkers:       current.MaxLiveWorkers,
			MaxConcurrentByState: current.MaxConcurrentByState,
		})
		atomic.StoreInt64(&availableSlots, int64(cap.AvailableSlots))
		if cap.Separated {
			atomic.StoreInt64(&separated, 1)
		} else {
			atomic.StoreInt64(&separated, 0)
		}
	}
	run := func(ctx context.Context, current *config.Config, opts Options, reloadCh <-chan *config.Config) {
		atomic.AddInt64(&started, 1)
		defer atomic.AddInt64(&stopped, 1)
		publishCapacity(current)
		for {
			select {
			case <-ctx.Done():
				return
			case next := <-reloadCh:
				if next == nil {
					continue
				}
				publishCapacity(next)
				atomic.AddInt64(&reloads, 1)
			}
		}
	}
	sup := func(ctx context.Context, name string, getCfg func() *config.Config, opts Options) {
		<-ctx.Done()
	}
	d := newWatchDaemon(store, run, sup)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()

	waitForNames(t, d, "alpha")
	waitFor(t, func() bool { return atomic.LoadInt64(&started) == 1 })
	waitFor(t, func() bool { return atomic.LoadInt64(&availableSlots) == 0 })
	if got := atomic.LoadInt64(&separated); got != 0 {
		t.Fatalf("initial separated = %d, want 0", got)
	}
	// Let the per-project watcher seed its initial fingerprint before advancing
	// updated_at; otherwise an unusually slow goroutine start could absorb the
	// first write into its baseline instead of emitting it.
	time.Sleep(2 * d.opts.WatchStoreInterval)

	// A timestamp-only/no-op store write still produces a reload event, but it
	// must not replace or cancel the running flow.
	noOp := *cfg
	store.Set("alpha", &noOp)
	waitFor(t, func() bool { return atomic.LoadInt64(&reloads) >= 1 })
	if got := atomic.LoadInt64(&started); got != 1 {
		t.Fatalf("run loops started after no-op reload = %d, want 1", got)
	}
	if got := atomic.LoadInt64(&stopped); got != 0 {
		t.Fatalf("run loops stopped after no-op reload = %d, want 0", got)
	}

	// Change only max_live_workers. With one PR gate and max_parallel=1, the
	// legacy model has no slot; separated accounting must expose one immediately.
	edited := noOp
	edited.MaxLiveWorkers = 1
	store.Set("alpha", &edited)
	waitFor(t, func() bool {
		return atomic.LoadInt64(&reloads) >= 2 &&
			atomic.LoadInt64(&separated) == 1 &&
			atomic.LoadInt64(&availableSlots) == 1
	})
	waitFor(t, func() bool { return fleetProjectLiveWorkerLimit(t, d, "alpha") == 1 })
	if got := atomic.LoadInt64(&started); got != 1 {
		t.Fatalf("run loops started after effective reload = %d, want 1", got)
	}
	if got := atomic.LoadInt64(&stopped); got != 0 {
		t.Fatalf("run loops stopped after effective reload = %d, want 0", got)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after cancellation")
	}
	if got := atomic.LoadInt64(&stopped); got != 1 {
		t.Fatalf("run loops stopped after daemon shutdown = %d, want 1", got)
	}
}

func TestReloadPumpCoalescesToLatestConfigWhenOrchestratorChannelIsFull(t *testing.T) {
	base := testConfig(t, "owner/alpha")
	base.MaxParallel = 1
	base.MaxLiveWorkers = 1
	flow := &projectFlow{name: "alpha", cfg: base, holder: newConfigHolder(base)}
	watchCh := make(chan *config.Config, 1)
	orchCh := make(chan *config.Config, 1)
	stale := *base
	stale.MaxParallel = 2
	stale.MaxLiveWorkers = 2
	orchCh <- &stale // orchestrator is mid-cycle; its one-slot queue is full
	latest := *base
	latest.MaxParallel = 9
	latest.MaxLiveWorkers = 3
	watchCh <- &latest
	close(watchCh)

	d := &Daemon{}
	d.runReloadPump(context.Background(), flow, watchCh, orchCh)
	got := <-orchCh
	if got.MaxParallel != 9 {
		t.Fatalf("queued reload max_parallel = %d, want latest value 9", got.MaxParallel)
	}
	if got.MaxLiveWorkers != 3 {
		t.Fatalf("queued reload max_live_workers = %d, want latest value 3", got.MaxLiveWorkers)
	}
	if held := flow.holder.Load().MaxParallel; held != 9 {
		t.Fatalf("holder max_parallel = %d, want 9", held)
	}
	if held := flow.holder.Load().MaxLiveWorkers; held != 3 {
		t.Fatalf("holder max_live_workers = %d, want 3", held)
	}
}

func TestReloadPumpLocalPathChangeIsAtomicRestartBoundary(t *testing.T) {
	base := testConfig(t, "owner/alpha")
	base.LocalPath = "/srv/alpha-old"
	base.MaxParallel = 1
	flow := &projectFlow{name: "alpha", cfg: base, holder: newConfigHolder(base)}
	watchCh := make(chan *config.Config, 1)
	orchCh := make(chan *config.Config, 1)

	// One store edit changes both a hot field and LocalPath. None of it may be
	// partially published: the supervisor/Fleet holder and the orchestrator must
	// keep one coherent old snapshot until the flow is restarted.
	edited := *base
	edited.LocalPath = "/srv/alpha-new"
	edited.MaxParallel = 9
	watchCh <- &edited
	close(watchCh)

	var logs bytes.Buffer
	oldWriter := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(oldWriter) })

	d := &Daemon{}
	d.runReloadPump(context.Background(), flow, watchCh, orchCh)

	held := flow.holder.Load()
	if held.LocalPath != base.LocalPath || held.MaxParallel != base.MaxParallel {
		t.Fatalf("holder partially reloaded restart-boundary edit: local_path=%q max_parallel=%d", held.LocalPath, held.MaxParallel)
	}
	select {
	case got := <-orchCh:
		t.Fatalf("orchestrator received partial restart-boundary reload: %+v", got)
	default:
	}
	if !strings.Contains(logs.String(), "restart required") || !strings.Contains(logs.String(), "local_path") {
		t.Fatalf("restart boundary was not reported clearly: %q", logs.String())
	}
}

// #768: a config-store edit must reach the supervise loop AND the /api/v1/fleet
// dashboard snapshot live — not only the orchestrator (the #757 limitation).
// The supervise loop reads the flow's current config each cycle through the
// shared holder; the dashboard is rebuilt from the same config on reload. The
// flow is never restarted (same StateDir → same flow identity).
func TestWatchStoreLiveReloadReachesSupervisorAndDashboard(t *testing.T) {
	store := newFakeWatchStore()
	cfg := testConfig(t, "owner/alpha")
	cfg.MaxParallel = 1
	cfg.MaxLiveWorkers = 1
	store.Set("alpha", cfg)

	// The supervise stub polls the holder via getCfg() and records the latest
	// MaxParallel/MaxLiveWorkers it observed, modelling the real loop reading
	// config each cycle.
	var seenMax int64
	var seenLiveLimit int64
	run := func(ctx context.Context, c *config.Config, opts Options, reloadCh <-chan *config.Config) {
		<-ctx.Done()
	}
	sup := func(ctx context.Context, name string, getCfg func() *config.Config, opts Options) {
		tick := time.NewTicker(3 * time.Millisecond)
		defer tick.Stop()
		for {
			current := getCfg()
			atomic.StoreInt64(&seenMax, int64(current.MaxParallel))
			atomic.StoreInt64(&seenLiveLimit, int64(current.MaxLiveWorkers))
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
			}
		}
	}
	d := newWatchDaemon(store, run, sup)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()

	waitForNames(t, d, "alpha")
	waitFor(t, func() bool { return atomic.LoadInt64(&seenMax) == 1 })
	waitFor(t, func() bool { return atomic.LoadInt64(&seenLiveLimit) == 1 })
	if got := fleetProjectMaxParallel(t, d, "alpha"); got != 1 {
		t.Fatalf("dashboard max_parallel = %d, want 1 at startup", got)
	}
	if got := fleetProjectLiveWorkerLimit(t, d, "alpha"); got != 1 {
		t.Fatalf("dashboard live_worker_limit = %d, want 1 at startup", got)
	}

	// Live edit: same StateDir keeps the same flow; hot capacity fields change.
	edited := *cfg
	edited.MaxParallel = 7
	edited.MaxLiveWorkers = 3
	store.Set("alpha", &edited)

	// The supervise loop observes the new config through the holder...
	waitFor(t, func() bool { return atomic.LoadInt64(&seenMax) == 7 })
	waitFor(t, func() bool { return atomic.LoadInt64(&seenLiveLimit) == 3 })
	// ...and the dashboard snapshot reflects it too — both without a restart.
	waitFor(t, func() bool { return fleetProjectMaxParallel(t, d, "alpha") == 7 })
	waitFor(t, func() bool { return fleetProjectLiveWorkerLimit(t, d, "alpha") == 3 })

	// No new flow was started: the edit reloaded the existing one in place.
	d.mu.Lock()
	flows := len(d.flows)
	d.mu.Unlock()
	if flows != 1 {
		t.Fatalf("flows = %d, want 1 (edit must reload in place, not restart)", flows)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after ctx cancellation")
	}
}

// fleetProjectMaxParallel reads max_parallel for the named project from the live
// /api/v1/fleet snapshot, or -1 when the project is absent.
func fleetProjectMaxParallel(t *testing.T, d *Daemon, name string) int {
	t.Helper()
	fleet := waitForFleet(t, d)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/fleet", nil)
	rec := httptest.NewRecorder()
	fleet.HandlerForTest().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/fleet = %d, want 200", rec.Code)
	}
	var resp struct {
		Projects []struct {
			Name        string `json:"name"`
			MaxParallel int    `json:"max_parallel"`
		} `json:"projects"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode fleet response: %v", err)
	}
	for _, p := range resp.Projects {
		if p.Name == name {
			return p.MaxParallel
		}
	}
	return -1
}

// fleetProjectLiveWorkerLimit reads live_worker_limit for the named project
// from the live /api/v1/fleet snapshot, or -1 when the project is absent.
func fleetProjectLiveWorkerLimit(t *testing.T, d *Daemon, name string) int {
	t.Helper()
	fleet := waitForFleet(t, d)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/fleet", nil)
	rec := httptest.NewRecorder()
	fleet.HandlerForTest().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/fleet = %d, want 200", rec.Code)
	}
	var resp struct {
		Projects []struct {
			Name            string `json:"name"`
			LiveWorkerLimit int    `json:"live_worker_limit"`
		} `json:"projects"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode fleet response: %v", err)
	}
	for _, p := range resp.Projects {
		if p.Name == name {
			return p.LiveWorkerLimit
		}
	}
	return -1
}

func waitForNames(t *testing.T, d *Daemon, want ...string) {
	t.Helper()
	wantSet := make(map[string]bool, len(want))
	for _, w := range want {
		wantSet[w] = true
	}
	deadline := time.Now().Add(2 * time.Second)
	var last []string
	for time.Now().Before(deadline) {
		last = fleetProjectNames(t, d)
		if len(last) == len(wantSet) {
			ok := true
			for _, n := range last {
				if !wantSet[n] {
					ok = false
					break
				}
			}
			if ok {
				return
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("fleet names = %v, want %v", last, want)
}

// #757: when one project is removed and a different project sharing its repo
// basename is added in the SAME reconcile tick, the re-add must reclaim the
// freed fleet name — not get a slug/numeric-suffixed one because the removed
// flow's name lingered in the diff-loop's takenNames snapshot.
func TestReconcileStoreSameTickRemoveAddReclaimsName(t *testing.T) {
	store := newFakeWatchStore()
	var run, sup loopTracker
	d := newWatchDaemon(store, run.loop, sup.superviseLoop)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Tick 1: project "old" (repo org-a/api) starts and takes fleet name "api".
	store.Set("old", testConfig(t, "org-a/api"))
	d.reconcileStore(ctx, map[string]time.Time{"old": time.Unix(1, 0)}, map[string]bool{})
	if got := storeFlowName(d, "old"); got != "api" {
		t.Fatalf("old fleet name = %q, want \"api\"", got)
	}

	// Tick 2 (one reconcile): "old" removed, "new" (same repo basename, distinct
	// StateDir) added. The freed name "api" must be reclaimed.
	store.Delete("old")
	store.Set("new", testConfig(t, "org-a/api"))
	d.reconcileStore(ctx, map[string]time.Time{"new": time.Unix(2, 0)}, map[string]bool{})

	if got := storeFlowName(d, "new"); got != "api" {
		t.Fatalf("re-added fleet name = %q, want reclaimed \"api\" (takenNames/takenKeys not freed on remove)", got)
	}
	cancel()
}

// storeFlowName returns the fleet display name of the running flow whose
// config-store name is storeName, or "" if none.
func storeFlowName(d *Daemon, storeName string) string {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, flow := range d.flows {
		if flow.storeName == storeName {
			return flow.name
		}
	}
	return ""
}
