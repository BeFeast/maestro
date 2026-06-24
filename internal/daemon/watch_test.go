package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/befeast/maestro/internal/config"
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

func newWatchDaemon(store ConfigLoader, run func(context.Context, *config.Config, Options, <-chan *config.Config), supervise func(context.Context, string, *config.Config, Options)) *Daemon {
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
	sup := func(ctx context.Context, name string, cfg *config.Config, opts Options) { <-ctx.Done() }
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
