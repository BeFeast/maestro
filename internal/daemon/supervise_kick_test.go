package daemon

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/state"
	"github.com/befeast/maestro/internal/supervisor"
)

// wedgeCycles replaces the supervise loop's cycle entry point with a fake that
// blocks, so a wedged cycle can be reproduced without a real GitHub/LLM stall.
// It returns the started-cycle counter. honorCancel=false models the worst case
// the loop must survive: a cycle that never observes its context at all.
func wedgeCycles(t *testing.T, honorCancel bool) (started *atomic.Int64, cancelled *atomic.Int64) {
	t.Helper()
	started = &atomic.Int64{}
	cancelled = &atomic.Int64{}
	// Closed on cleanup so the deliberately-ignored cycles cannot outlive the test.
	release := make(chan struct{})
	restore := superviseRunOnce
	superviseRunOnce = func(ctx context.Context, _ *config.Config, _ supervisor.Reader, _ ...supervisor.RunOption) (state.SupervisorDecision, error) {
		started.Add(1)
		if honorCancel {
			select {
			case <-ctx.Done():
				cancelled.Add(1)
				return state.SupervisorDecision{}, ctx.Err()
			case <-release:
				return state.SupervisorDecision{}, nil
			}
		}
		<-release
		return state.SupervisorDecision{}, nil
	}
	t.Cleanup(func() {
		superviseRunOnce = restore
		close(release)
	})
	return started, cancelled
}

// waitForCycles blocks until the loop has started at least want cycles.
func waitForCycles(t *testing.T, started *atomic.Int64, want int64, why string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for started.Load() < want {
		if time.Now().After(deadline) {
			t.Fatalf("%s: %d cycle(s) started, want %d", why, started.Load(), want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// superviseTestConfig builds a throwaway flow config and pins the startup
// jitter to zero so the first cycle runs immediately.
func superviseTestConfig(t *testing.T) *config.Config {
	t.Helper()
	restore := superviseJitterFrac
	superviseJitterFrac = func() float64 { return 0 }
	t.Cleanup(func() { superviseJitterFrac = restore })
	return &config.Config{Repo: "owner/wedged", StateDir: t.TempDir()}
}

// TestRunSuperviseKickCancelsWedgedCycleAndRunsAFreshOne pins the self-heal the
// earlier attempt (#820) claimed and did not deliver: a kick must CANCEL the
// wedged cycle, not wait on it. The interval is long enough that the ticker
// cannot fire during the test, so every cycle observed here is kick-driven.
func TestRunSuperviseKickCancelsWedgedCycleAndRunsAFreshOne(t *testing.T) {
	started, cancelled := wedgeCycles(t, true)
	cfg := superviseTestConfig(t)
	kickCh := make(chan struct{}, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		runSupervise(ctx, "kick", func() *config.Config { return cfg }, nil, time.Hour, "", nil, kickCh)
	}()

	waitForCycles(t, started, 1, "first cycle never started")

	// Kick #1 interrupts the wedged cycle.
	kickCh <- struct{}{}
	deadline := time.Now().Add(3 * time.Second)
	for cancelled.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the kick did not cancel the wedged cycle — it only waited on it, which is the #820 no-op")
		}
		time.Sleep(5 * time.Millisecond)
	}

	// Kick #2 proves the loop is alive and runs a fresh cycle afterwards.
	kickCh <- struct{}{}
	waitForCycles(t, started, 2, "the loop did not run a fresh cycle after the kick")

	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("runSupervise did not return after ctx cancellation")
	}
}

// TestRunSuperviseKickDoesNotWaitOnACycleThatIgnoresCancellation covers the
// pathological case: the cycle never observes its context. The kick must still
// return promptly and the loop must keep running fresh cycles, because a wedge
// that cancellation cannot reach is exactly when the self-heal has to work.
func TestRunSuperviseKickDoesNotWaitOnACycleThatIgnoresCancellation(t *testing.T) {
	restoreGrace := superviseCycleCancelGrace
	superviseCycleCancelGrace = 50 * time.Millisecond
	t.Cleanup(func() { superviseCycleCancelGrace = restoreGrace })

	started, _ := wedgeCycles(t, false)
	cfg := superviseTestConfig(t)
	kickCh := make(chan struct{}, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		runSupervise(ctx, "kick-ignored", func() *config.Config { return cfg }, nil, time.Hour, "", nil, kickCh)
	}()

	waitForCycles(t, started, 1, "first cycle never started")
	kickCh <- struct{}{}
	kickCh <- struct{}{}
	waitForCycles(t, started, 2, "the loop stayed blocked on a cycle that ignores cancellation")

	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("runSupervise did not return after ctx cancellation")
	}
}

// TestRunSuperviseShutdownDoesNotBlockOnWedgedCycle pins the other half of the
// contract: flow shutdown cancels the cycle and, if it does not unwind within
// the grace, abandons it. Before this, shutdown waited on the wedged goroutine
// and the whole flow hung with it.
func TestRunSuperviseShutdownDoesNotBlockOnWedgedCycle(t *testing.T) {
	restoreGrace := superviseCycleCancelGrace
	superviseCycleCancelGrace = 50 * time.Millisecond
	t.Cleanup(func() { superviseCycleCancelGrace = restoreGrace })

	started, _ := wedgeCycles(t, false)
	cfg := superviseTestConfig(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		runSupervise(ctx, "shutdown", func() *config.Config { return cfg }, nil, time.Hour, "", nil, nil)
	}()

	waitForCycles(t, started, 1, "first cycle never started")
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("runSupervise blocked on a wedged cycle during shutdown")
	}
}
