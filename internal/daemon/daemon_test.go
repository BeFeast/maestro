package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/server"
)

// fakeLoader is an in-memory ConfigLoader so the daemon lifecycle can be
// exercised without a real SQLite config store.
type fakeLoader struct {
	cfgs []*config.Config
	err  error
}

func (f fakeLoader) LoadAll(ctx context.Context) ([]*config.Config, error) {
	return f.cfgs, f.err
}

func testConfig(t *testing.T, repo string) *config.Config {
	t.Helper()
	return &config.Config{
		Repo:        repo,
		StateDir:    t.TempDir(),
		MaxParallel: 1,
	}
}

// loopTracker records per-loop start/stop so tests can assert flows ran and
// drained without reaching GitHub.
type loopTracker struct {
	started int64
	stopped int64
}

func (lt *loopTracker) loop(ctx context.Context, cfg *config.Config, opts Options) {
	atomic.AddInt64(&lt.started, 1)
	<-ctx.Done()
	atomic.AddInt64(&lt.stopped, 1)
}

// newTestDaemon builds a daemon with stub loops (no GitHub) over the given
// configs.
func newTestDaemon(loader ConfigLoader, run, supervise func(context.Context, *config.Config, Options)) *Daemon {
	d := New(loader, Options{Host: "127.0.0.1", Port: 0})
	if run != nil {
		d.runLoop = run
	}
	if supervise != nil {
		d.superviseLoop = supervise
	}
	return d
}

func TestRunStartsFlowPerProjectAndAggregatesFleet(t *testing.T) {
	cfgs := []*config.Config{
		testConfig(t, "owner/alpha"),
		testConfig(t, "owner/beta"),
		testConfig(t, "owner/gamma"),
	}
	var runTracker, supTracker loopTracker
	d := newTestDaemon(fakeLoader{cfgs: cfgs}, runTracker.loop, supTracker.loop)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()

	// Wait for the fleet to be built.
	fleet := waitForFleet(t, d)

	// /api/v1/fleet must show every project (acceptance criterion).
	req := httptest.NewRequest(http.MethodGet, "/api/v1/fleet", nil)
	rec := httptest.NewRecorder()
	fleet.HandlerForTest().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/fleet = %d, want 200", rec.Code)
	}
	var resp struct {
		Projects []json.RawMessage `json:"projects"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode fleet response: %v", err)
	}
	if len(resp.Projects) != 3 {
		t.Fatalf("fleet projects = %d, want 3", len(resp.Projects))
	}

	// Each flow ran both loops.
	if got := atomic.LoadInt64(&runTracker.started); got != 3 {
		t.Fatalf("run loops started = %d, want 3", got)
	}
	if got := atomic.LoadInt64(&supTracker.started); got != 3 {
		t.Fatalf("supervise loops started = %d, want 3", got)
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

	// Every loop drained on shutdown.
	if got := atomic.LoadInt64(&runTracker.stopped); got != 3 {
		t.Fatalf("run loops stopped = %d, want 3", got)
	}
	if got := atomic.LoadInt64(&supTracker.stopped); got != 3 {
		t.Fatalf("supervise loops stopped = %d, want 3", got)
	}
}

func TestRunSkipsDuplicateFleetName(t *testing.T) {
	// Two configs deriving the same fleet name (same repo) — the second must
	// be skipped, not silently overwrite the first.
	cfgs := []*config.Config{
		testConfig(t, "owner/dup"),
		testConfig(t, "owner/dup"),
	}
	var run, sup loopTracker
	d := newTestDaemon(fakeLoader{cfgs: cfgs}, run.loop, sup.loop)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()

	waitForFleet(t, d)

	d.mu.Lock()
	flows := len(d.flows)
	d.mu.Unlock()
	if flows != 1 {
		t.Fatalf("flows = %d, want 1 (duplicate name skipped)", flows)
	}
	if got := atomic.LoadInt64(&run.started); got != 1 {
		t.Fatalf("run loops started = %d, want 1", got)
	}

	cancel()
	<-done
}

func TestRunEmptyStoreReturnsError(t *testing.T) {
	d := New(fakeLoader{cfgs: nil}, Options{Port: 0})
	if err := d.Run(context.Background()); err == nil {
		t.Fatal("Run with empty store: want error, got nil")
	}
}

func TestStartStopFlowDrains(t *testing.T) {
	cfg := testConfig(t, "owner/solo")
	var run, sup loopTracker
	d := newTestDaemon(fakeLoader{cfgs: []*config.Config{cfg}}, run.loop, sup.loop)

	proj := newFleetProject(cfg)
	flow := d.startFlow(context.Background(), proj)

	// Both loops should be running.
	waitFor(t, func() bool { return atomic.LoadInt64(&run.started) == 1 && atomic.LoadInt64(&sup.started) == 1 })

	d.stopFlow(flow.name)

	select {
	case <-flow.done:
	case <-time.After(2 * time.Second):
		t.Fatal("flow did not drain after stopFlow")
	}
	if atomic.LoadInt64(&run.stopped) != 1 || atomic.LoadInt64(&sup.stopped) != 1 {
		t.Fatalf("loops did not drain: run.stopped=%d sup.stopped=%d", run.stopped, sup.stopped)
	}

	// Flow is deregistered.
	d.mu.Lock()
	_, ok := d.flows[flow.name]
	d.mu.Unlock()
	if ok {
		t.Fatal("flow still registered after stopFlow")
	}

	// stopFlow on an unknown name is a no-op.
	d.stopFlow("does-not-exist")
}

func TestFlowPanicContained(t *testing.T) {
	// A panic in one loop must be contained: the daemon (and the other loop)
	// survive. This is the "one bad project doesn't kill the daemon" guarantee
	// at the goroutine level (#756).
	cfg := testConfig(t, "owner/panicky")
	var supStarted, supStopped int64
	supervise := func(ctx context.Context, c *config.Config, o Options) {
		atomic.AddInt64(&supStarted, 1)
		<-ctx.Done()
		atomic.AddInt64(&supStopped, 1)
	}
	run := func(ctx context.Context, c *config.Config, o Options) {
		panic("boom")
	}
	d := newTestDaemon(fakeLoader{cfgs: []*config.Config{cfg}}, run, supervise)

	proj := newFleetProject(cfg)
	flow := d.startFlow(context.Background(), proj)

	// The supervise loop keeps running despite the run loop's panic.
	waitFor(t, func() bool { return atomic.LoadInt64(&supStarted) == 1 })

	d.stopFlow(flow.name)
	select {
	case <-flow.done:
	case <-time.After(2 * time.Second):
		t.Fatal("flow did not drain after a contained panic")
	}
	if atomic.LoadInt64(&supStopped) != 1 {
		t.Fatal("supervise loop did not drain after the run loop panicked")
	}
}

func waitForFleet(t *testing.T, d *Daemon) *server.FleetServer {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if f := d.Fleet(); f != nil {
			return f
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("fleet was not built within deadline")
	return nil
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("condition not met within deadline")
}
