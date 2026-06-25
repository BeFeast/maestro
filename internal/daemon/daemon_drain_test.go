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
