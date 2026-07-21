package daemon

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/state"
)

// TestDrainSetsSpawnDrainAndReturnsWhenIdle covers the SIGTERM in-process drain
// (#761): Drain stamps SpawnDrain on every live flow so no new workers are
// claimed, then — because no in-flight workers remain — returns promptly without
// waiting out the timeout.
func TestDrainSetsSpawnDrainAndReturnsWhenIdle(t *testing.T) {
	old := drainPollInterval
	drainPollInterval = 5 * time.Millisecond
	defer func() { drainPollInterval = old }()

	a := testConfig(t, "owner/alpha")
	b := testConfig(t, "owner/beta")
	d := New(fakeLoader{cfgs: nil}, Options{})
	d.flows[flowKey(a)] = &projectFlow{name: "alpha", key: flowKey(a), cfg: a}
	d.flows[flowKey(b)] = &projectFlow{name: "beta", key: flowKey(b), cfg: b}

	done := make(chan struct{})
	go func() {
		// A long timeout that must NOT be reached: both flows are idle.
		d.Drain(context.Background(), 10*time.Minute)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Drain did not return promptly for idle flows")
	}

	for name, dir := range map[string]string{"alpha": a.StateDir, "beta": b.StateDir} {
		s, err := state.Load(dir)
		if err != nil {
			t.Fatalf("load %s state: %v", name, err)
		}
		if !s.DrainActive() {
			t.Fatalf("flow %s: SpawnDrain not set after Drain", name)
		}
	}
}

// TestDrainWaitsForWorkersThenAbortsOnCancel covers the wait phase: with an
// in-flight worker that never finishes, Drain blocks, and a second signal (ctx
// cancellation) aborts the wait so the operator can force shutdown.
func TestDrainWaitsForWorkersThenAbortsOnCancel(t *testing.T) {
	old := drainPollInterval
	drainPollInterval = 5 * time.Millisecond
	defer func() { drainPollInterval = old }()

	cfg := testConfig(t, "owner/alpha")
	d := New(fakeLoader{cfgs: nil}, Options{})
	d.flows[flowKey(cfg)] = &projectFlow{name: "alpha", key: flowKey(cfg), cfg: cfg}

	// Seed one in-flight worker so the wait loop never observes running==0.
	seed := &state.State{Sessions: map[string]*state.Session{
		"alpha-1": {Status: state.StatusRunning},
	}}
	if err := state.Save(cfg.StateDir, seed); err != nil {
		t.Fatalf("seed running state: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		d.Drain(ctx, 10*time.Minute)
		close(done)
	}()

	// The worker never finishes, so Drain must still be waiting.
	select {
	case <-done:
		t.Fatal("Drain returned despite an in-flight worker still running")
	case <-time.After(60 * time.Millisecond):
	}

	// SpawnDrain was still requested in phase 1.
	if s, err := state.Load(cfg.StateDir); err != nil || !s.DrainActive() {
		t.Fatalf("SpawnDrain not set during wait (err=%v)", err)
	}

	// Second signal → ctx cancel → Drain aborts the wait.
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Drain did not abort after ctx cancellation")
	}
}

// TestStopAllTimesOutOnHangingFlow verifies that stopAll does not block
// indefinitely when a flow's goroutines ignore context cancellation. It should
// return within shutdownFlowTimeout and log a warning, not hang (#817).
func TestStopAllTimesOutOnHangingFlow(t *testing.T) {
	old := shutdownFlowTimeout
	shutdownFlowTimeout = 50 * time.Millisecond
	defer func() { shutdownFlowTimeout = old }()

	d := New(fakeLoader{cfgs: nil}, Options{})

	// A flow whose done channel never closes — the loop never exits.
	neverDone := make(chan struct{})
	_, cancel := context.WithCancel(context.Background())
	flow := &projectFlow{
		name:   "hanger",
		key:    "hanger",
		cfg:    testConfig(t, "owner/hanger"),
		cancel: cancel,
		done:   neverDone,
	}
	// Cancel so flow.cancel() works without panic; the done channel stays open.
	cancel()
	d.flows[flow.key] = flow

	// stopAll must return within ~shutdownFlowTimeout, not hang.
	done := make(chan struct{})
	go func() {
		d.stopAll()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("stopAll did not return within shutdownFlowTimeout for a hanging flow")
	}
}

func TestStopAllUsesOneGlobalDeadlineForTwelveHangingFlows(t *testing.T) {
	old := shutdownFlowTimeout
	shutdownFlowTimeout = 5 * time.Second
	defer func() { shutdownFlowTimeout = old }()

	d := New(fakeLoader{cfgs: nil}, Options{})
	neverDone := make(chan struct{})
	defer close(neverDone)
	for i := 0; i < 12; i++ {
		_, cancel := context.WithCancel(context.Background())
		flow := &projectFlow{
			name:   fmt.Sprintf("flow-%02d", i),
			key:    fmt.Sprintf("flow-%02d", i),
			cfg:    testConfig(t, fmt.Sprintf("owner/flow-%02d", i)),
			cancel: cancel,
			done:   neverDone,
		}
		d.flows[flow.key] = flow
	}

	started := time.Now()
	d.stopAllUntil(time.Now().Add(75 * time.Millisecond))
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("12 hanging flows serialized shutdown for %v; want one shared ~75ms deadline", elapsed)
	}
	if len(d.flows) != 0 {
		t.Fatalf("flows still registered after detach: %d", len(d.flows))
	}
}

// TestDrainReturnsAfterDefaultTimeoutWithStuckWorkers verifies that Drain
// respects the bounded default timeout (DefaultDrainTimeout) when in-flight
// workers never finish (#817).
func TestDrainReturnsAfterDefaultTimeoutWithStuckWorkers(t *testing.T) {
	old := drainPollInterval
	drainPollInterval = 5 * time.Millisecond
	defer func() { drainPollInterval = old }()

	cfg := testConfig(t, "owner/stuck")
	d := New(fakeLoader{cfgs: nil}, Options{})
	d.flows[flowKey(cfg)] = &projectFlow{name: "stuck", key: flowKey(cfg), cfg: cfg}

	// Seed an in-flight worker that never finishes.
	seed := &state.State{Sessions: map[string]*state.Session{
		"stuck-1": {Status: state.StatusRunning},
	}}
	if err := state.Save(cfg.StateDir, seed); err != nil {
		t.Fatalf("seed running state: %v", err)
	}

	start := time.Now()
	// Use a very short timeout so the test completes quickly.
	d.Drain(context.Background(), 100*time.Millisecond)
	elapsed := time.Since(start)

	if elapsed > 5*time.Second {
		t.Fatalf("Drain took %v with a 100ms timeout, should have returned near the deadline", elapsed)
	}
}

// #966 production-topology regression: twelve flows, four isolated in-flight
// workers, and one flow callback that ignores cancellation. Fleet stays reachable
// while the worker drain is active; the global deadline then detaches the hung
// flow, stamps every surviving worker, and returns without a second signal.
func TestTwelveFlowShutdownKeepsHealthDuringDrainAndDetachesHungFlow(t *testing.T) {
	oldPoll, oldFlowTimeout, oldCheckpointFn := drainPollInterval, shutdownFlowTimeout, checkpointFn
	drainPollInterval = 5 * time.Millisecond
	shutdownFlowTimeout = 5 * time.Second
	checkpointFn = func(*state.Session) (string, error) { return "", nil }
	defer func() {
		drainPollInterval = oldPoll
		shutdownFlowTimeout = oldFlowTimeout
		checkpointFn = oldCheckpointFn
	}()

	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve fleet port: %v", err)
	}
	port := probe.Addr().(*net.TCPAddr).Port
	probe.Close()

	cfgs := make([]*config.Config, 0, 12)
	for i := 0; i < 12; i++ {
		cfg := testConfig(t, fmt.Sprintf("owner/flow-%02d", i))
		cfgs = append(cfgs, cfg)
		if i < 4 {
			worktree := t.TempDir()
			s := state.NewState()
			s.Sessions[fmt.Sprintf("slot-%02d", i)] = &state.Session{
				IssueNumber: 900 + i,
				Status:      state.StatusRunning,
				PID:         7000 + i,
				TmuxSession: fmt.Sprintf("maestro-slot-%02d", i),
				Worktree:    worktree,
			}
			if err := state.Save(cfg.StateDir, s); err != nil {
				t.Fatalf("seed worker %d: %v", i, err)
			}
		}
	}

	releaseHung := make(chan struct{})
	d := New(fakeLoader{cfgs: cfgs}, Options{Host: "127.0.0.1", Port: port})
	d.runLoop = func(ctx context.Context, cfg *config.Config, _ Options, _ <-chan *config.Config) {
		if cfg.Repo == "owner/flow-11" {
			<-releaseHung // deliberately ignores ctx until the test releases it
			return
		}
		<-ctx.Done()
	}
	d.superviseLoop = func(ctx context.Context, _ string, _ func() *config.Config, _ Options) { <-ctx.Done() }
	d.watchdogLoop = func(context.Context, string, string, time.Duration) {}
	d.materialProgressLoop = func(context.Context, string, func() *config.Config) {}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()
	url := fmt.Sprintf("http://127.0.0.1:%d/api/v1/fleet", port)
	client := &http.Client{Timeout: 200 * time.Millisecond}
	waitFor(t, func() bool {
		resp, err := client.Get(url)
		if err != nil {
			return false
		}
		resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	})

	shutdownDeadline := time.Now().Add(300 * time.Millisecond)
	d.SetShutdownDeadline(shutdownDeadline)
	drainDone := make(chan struct{})
	go func() {
		d.DrainUntil(context.Background(), shutdownDeadline.Add(-100*time.Millisecond))
		close(drainDone)
	}()

	// All four running workers keep the drain active. The old daemon must continue
	// serving health during this window rather than dropping the port immediately.
	time.Sleep(50 * time.Millisecond)
	resp, err := client.Get(url)
	if err != nil {
		t.Fatalf("fleet health unavailable during drain: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("fleet health during drain = %d, want 200", resp.StatusCode)
	}

	select {
	case <-drainDone:
	case <-time.After(time.Second):
		t.Fatal("bounded worker drain did not return")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned error: %v", err)
		}
	case <-time.After(time.Until(shutdownDeadline) + 300*time.Millisecond):
		t.Fatal("daemon did not detach the hung flow by the global shutdown deadline")
	}
	close(releaseHung)

	if _, err := client.Get(url); err == nil {
		t.Fatal("fleet health still reachable after process handoff")
	}
	for i := 0; i < 4; i++ {
		s, err := state.Load(cfgs[i].StateDir)
		if err != nil {
			t.Fatalf("reload worker %d: %v", i, err)
		}
		sess := s.Sessions[fmt.Sprintf("slot-%02d", i)]
		if sess.RestartCheckpointAt == nil {
			t.Fatalf("worker %d missing restart checkpoint marker", i)
		}
		if sess.PID != 7000+i {
			t.Fatalf("worker %d PID changed during shutdown: got %d want %d", i, sess.PID, 7000+i)
		}
	}

	// The replacement daemon can bind the same fleet port immediately after the
	// bounded handoff; health recovery does not wait for an external second signal.
	replacement := New(fakeLoader{cfgs: cfgs}, Options{Host: "127.0.0.1", Port: port})
	replacement.runLoop = func(ctx context.Context, _ *config.Config, _ Options, _ <-chan *config.Config) { <-ctx.Done() }
	replacement.superviseLoop = func(ctx context.Context, _ string, _ func() *config.Config, _ Options) { <-ctx.Done() }
	replacement.watchdogLoop = func(context.Context, string, string, time.Duration) {}
	replacement.materialProgressLoop = func(context.Context, string, func() *config.Config) {}
	replacementCtx, stopReplacement := context.WithCancel(context.Background())
	replacementDone := make(chan error, 1)
	go func() { replacementDone <- replacement.Run(replacementCtx) }()
	waitFor(t, func() bool {
		resp, err := client.Get(url)
		if err != nil {
			return false
		}
		resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	})
	stopReplacement()
	select {
	case err := <-replacementDone:
		if err != nil {
			t.Fatalf("replacement Run returned error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("replacement daemon did not stop")
	}
}
