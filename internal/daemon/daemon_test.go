package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/server"
	"github.com/befeast/maestro/internal/state"
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

func TestRunSkipsDuplicateFlowIdentity(t *testing.T) {
	// Two configs that resolve to the same flow identity (same StateDir) are a
	// true duplicate — the second must be skipped, not silently overwrite the
	// first in the flows registry.
	shared := testConfig(t, "owner/dup")
	dup := testConfig(t, "owner/dup")
	dup.StateDir = shared.StateDir
	cfgs := []*config.Config{shared, dup}
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
		t.Fatalf("flows = %d, want 1 (duplicate flow identity skipped)", flows)
	}
	if got := atomic.LoadInt64(&run.started); got != 1 {
		t.Fatalf("run loops started = %d, want 1", got)
	}

	cancel()
	<-done
}

// #764 P2: two distinct repos that share a basename ("api") must BOTH get a
// flow. The old dedup keyed on the repo-basename fleet name, so org-b/api was
// silently skipped — its orchestrator and supervisor never started.
func TestRunSameBasenameDistinctReposBothStart(t *testing.T) {
	cfgs := []*config.Config{
		testConfig(t, "org-a/api"),
		testConfig(t, "org-b/api"),
	}
	var run, sup loopTracker
	d := newTestDaemon(fakeLoader{cfgs: cfgs}, run.loop, sup.loop)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()

	waitForFleet(t, d)
	waitFor(t, func() bool { return atomic.LoadInt64(&run.started) == 2 })

	d.mu.Lock()
	flows := len(d.flows)
	d.mu.Unlock()
	if flows != 2 {
		t.Fatalf("flows = %d, want 2 (same-basename distinct repos both start)", flows)
	}
	if got := atomic.LoadInt64(&sup.started); got != 2 {
		t.Fatalf("supervise loops started = %d, want 2", got)
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

	proj := server.NewFleetProjectWithGitHub(cfg)
	flow := d.startFlow(context.Background(), proj)

	// Both loops should be running.
	waitFor(t, func() bool { return atomic.LoadInt64(&run.started) == 1 && atomic.LoadInt64(&sup.started) == 1 })

	d.stopFlow(flow.key)

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
	_, ok := d.flows[flow.key]
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

	proj := server.NewFleetProjectWithGitHub(cfg)
	flow := d.startFlow(context.Background(), proj)

	// The supervise loop keeps running despite the run loop's panic.
	waitFor(t, func() bool { return atomic.LoadInt64(&supStarted) == 1 })

	d.stopFlow(flow.key)
	select {
	case <-flow.done:
	case <-time.After(2 * time.Second):
		t.Fatal("flow did not drain after a contained panic")
	}
	if atomic.LoadInt64(&supStopped) != 1 {
		t.Fatal("supervise loop did not drain after the run loop panicked")
	}
}

// #764 P1: a fleet bind failure (port already held) must surface immediately —
// Run returns the error on its own, without waiting for ctx cancellation /
// SIGTERM. Otherwise the daemon runs every flow with no web endpoint until the
// operator kills it.
func TestRunSurfacesFleetBindErrorImmediately(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port

	cfg := testConfig(t, "owner/bind")
	var run, sup loopTracker
	d := New(fakeLoader{cfgs: []*config.Config{cfg}}, Options{Host: "127.0.0.1", Port: port})
	d.runLoop = run.loop
	d.superviseLoop = sup.loop

	// ctx is intentionally never cancelled within the deadline.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Run returned nil; want the fleet bind error surfaced immediately")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return the bind error without ctx cancellation (#764 P1)")
	}
}

// #764 P3: a non-positive interval must be clamped to the safe default with a
// warn, never silently honored (a 0 run-interval panics time.NewTicker; a 0
// supervise-interval kills the loop + watchdog while the flow reads healthy).
func TestNewClampsNonPositiveIntervals(t *testing.T) {
	d := New(fakeLoader{}, Options{RunInterval: 0, SuperviseInterval: -1})
	if d.opts.RunInterval != DefaultRunInterval {
		t.Fatalf("RunInterval = %s, want clamp to %s", d.opts.RunInterval, DefaultRunInterval)
	}
	if d.opts.SuperviseInterval != DefaultSuperviseInterval {
		t.Fatalf("SuperviseInterval = %s, want clamp to %s", d.opts.SuperviseInterval, DefaultSuperviseInterval)
	}

	// A positive operator value is honored verbatim.
	d2 := New(fakeLoader{}, Options{RunInterval: 3 * time.Minute, SuperviseInterval: 90 * time.Second})
	if d2.opts.RunInterval != 3*time.Minute || d2.opts.SuperviseInterval != 90*time.Second {
		t.Fatalf("positive intervals not honored: run=%s supervise=%s", d2.opts.RunInterval, d2.opts.SuperviseInterval)
	}
}

// #764 P2: the watchdog goroutine must join the flow WaitGroup so stopFlow does
// not return until it has exited. A stub watchdog records start/stop; after
// stopFlow returns, stop must already be observed.
func TestStopFlowDrainsWatchdog(t *testing.T) {
	cfg := testConfig(t, "owner/wd")
	var run, sup loopTracker
	d := New(fakeLoader{cfgs: []*config.Config{cfg}}, Options{Port: 0, SuperviseInterval: time.Minute})
	d.runLoop = run.loop
	d.superviseLoop = sup.loop

	var wdStarted, wdStopped int64
	var gotName, gotStateDir string
	d.watchdogLoop = func(ctx context.Context, name, stateDir string, interval time.Duration) {
		gotName, gotStateDir = name, stateDir
		atomic.AddInt64(&wdStarted, 1)
		<-ctx.Done()
		atomic.AddInt64(&wdStopped, 1)
	}

	proj := server.NewFleetProjectWithGitHub(cfg)
	flow := d.startFlow(context.Background(), proj)
	waitFor(t, func() bool { return atomic.LoadInt64(&wdStarted) == 1 })

	d.stopFlow(flow.key)

	// stopFlow waits on flow.done, which only closes once the watchdog
	// goroutine has also exited — so the stop must already be visible here,
	// with no further wait.
	if got := atomic.LoadInt64(&wdStopped); got != 1 {
		t.Fatalf("watchdog not drained by stopFlow: wdStopped=%d, want 1", got)
	}
	// The watchdog is told which project it watches (#764 P2 log identity).
	if gotName != flow.name {
		t.Fatalf("watchdog name = %q, want %q", gotName, flow.name)
	}
	if gotStateDir != cfg.StateDir {
		t.Fatalf("watchdog stateDir = %q, want %q", gotStateDir, cfg.StateDir)
	}
}

// #764: the daemon supervise loop logs each cycle's SupervisorDecision so the
// journal keeps the structural "why" trail the CLI printed.
func TestLogSupervisorDecisionEmitsStructuredLine(t *testing.T) {
	var buf bytes.Buffer
	old := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(old)

	cfg := &config.Config{SessionPrefix: "acme"}
	logSupervisorDecision(cfg, state.SupervisorDecision{
		RecommendedAction: "merge_pr",
		Status:            "ready",
		Risk:              "low",
		Confidence:        0.92,
		RequiresApproval:  true,
		ApprovalID:        "appr-1",
		Summary:           "PR #12 is green",
	})

	out := buf.String()
	for _, want := range []string{"acme", "supervise decision", "action=merge_pr", "requires_approval=true", "approval=appr-1", "PR #12 is green"} {
		if !strings.Contains(out, want) {
			t.Fatalf("decision log %q missing %q", out, want)
		}
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
