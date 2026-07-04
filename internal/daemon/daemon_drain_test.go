package daemon

import (
	"context"
	"testing"
	"time"

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
