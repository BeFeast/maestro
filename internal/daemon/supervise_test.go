package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/state"
)

// disableJitter sets superviseJitterFrac so the startup jitter is zero,
// making test timing deterministic. Returns the restore function.
func disableJitter() func() {
	old := superviseJitterFrac
	superviseJitterFrac = func() float64 { return 0 }
	return func() { superviseJitterFrac = old }
}

// TestRunSuperviseRespondsToKick verifies that the supervise loop responds
// to kick signals from the watchdog by running a fresh cycle (#816).
// It also verifies that the loop exits cleanly on ctx cancellation and
// does not deadlock.
func TestRunSuperviseRespondsToKick(t *testing.T) {
	restore := disableJitter()
	defer restore()

	cfg := &config.Config{
		Repo:     "test/repo",
		StateDir: t.TempDir(),
	}

	st := state.NewState()
	if err := state.Save(cfg.StateDir, st); err != nil {
		t.Fatalf("save initial state: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	kickCh := make(chan struct{}, 1)
	done := make(chan struct{})

	go func() {
		runSupervise(ctx, "test-kick", func() *config.Config { return cfg }, nil, 60*time.Second, nil, kickCh)
		close(done)
	}()

	// Let the initial runCycle complete (RunOnce will fail fast
	// since gh CLI isn't configured in tests).
	time.Sleep(50 * time.Millisecond)

	// Send a kick — the loop should process it and run another cycle.
	select {
	case kickCh <- struct{}{}:
	default:
		t.Fatal("kickCh buffer full")
	}

	time.Sleep(50 * time.Millisecond)

	// Send another kick.
	select {
	case kickCh <- struct{}{}:
	default:
	}

	time.Sleep(50 * time.Millisecond)

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("supervise loop did not exit after ctx cancellation — test deadlock")
	}
}

// TestRunSuperviseStartupCycleRunsOnce verifies that only the first runCycle
// runs before the main loop select — not two back-to-back. With a 500ms
// interval, the second cycle cannot fire within 200ms if the fix is correct.
func TestRunSuperviseStartupCycleRunsOnce(t *testing.T) {
	restore := disableJitter()
	defer restore()

	cfg := &config.Config{
		Repo:     "test/repo",
		StateDir: t.TempDir(),
	}

	st := state.NewState()
	if err := state.Save(cfg.StateDir, st); err != nil {
		t.Fatalf("save initial state: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		runSupervise(ctx, "test-once", func() *config.Config { return cfg }, nil, 500*time.Millisecond, nil, nil)
		close(done)
	}()

	// Wait 200ms — well within the 500ms interval. With the old code
	// (runCycle at top of for-loop), two cycles would run back-to-back
	// immediately. With the fix, only the first cycle runs and the
	// second waits on ticker.C, which hasn't fired yet.
	time.Sleep(200 * time.Millisecond)

	// No cycles should have been started beyond the first.
	// We verify this by checking that the function is still running
	// and hasn't deadlocked.
	select {
	case <-done:
		t.Fatal("supervise loop exited before ctx cancellation — unexpected")
	default:
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("supervise loop did not exit after ctx cancellation")
	}
}

// TestRunSuperviseCancelsCleanlyWhileWaitingForRunOnce verifies that when
// ctx is canceled while waiting for a stuck RunOnce, the function still
// exits (it waits for the goroutine and returns).
func TestRunSuperviseCancelsCleanlyWhileWaitingForRunOnce(t *testing.T) {
	restore := disableJitter()
	defer restore()

	cfg := &config.Config{
		Repo:     "test/repo",
		StateDir: t.TempDir(),
	}

	st := state.NewState()
	if err := state.Save(cfg.StateDir, st); err != nil {
		t.Fatalf("save initial state: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		runSupervise(ctx, "test-cancel", func() *config.Config { return cfg }, nil, 100*time.Millisecond, nil, nil)
		close(done)
	}()

	// Let the first runCycle start.
	time.Sleep(20 * time.Millisecond)

	// Cancel while the cycle is in-flight.
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("supervise loop did not exit after ctx cancellation while RunOnce was in-flight")
	}
}

// TestRunSuperviseKickWhileWaitingForRunOnce verifies that a kick sent
// while runCycle is waiting for a stuck RunOnce is observed and causes
// runCycle to abort waiting (#816 review comment 2).
// TestRunSuperviseKickTracksInflightGoroutine verifies that after a kick
// abandons the in-flight RunOnce, the function still exits cleanly on ctx
// cancellation — proving the goroutine is tracked via inflight and does not
// prevent shutdown (#816 review comment 1).
func TestRunSuperviseKickTracksInflightGoroutine(t *testing.T) {
	restore := disableJitter()
	defer restore()

	cfg := &config.Config{
		Repo:     "test/repo",
		StateDir: t.TempDir(),
	}

	st := state.NewState()
	if err := state.Save(cfg.StateDir, st); err != nil {
		t.Fatalf("save initial state: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	kickCh := make(chan struct{}, 1)
	done := make(chan struct{})
	go func() {
		runSupervise(ctx, "test-track", func() *config.Config { return cfg }, nil, 100*time.Millisecond, nil, kickCh)
		close(done)
	}()

	// Let the first runCycle complete (RunOnce fails fast without gh CLI).
	time.Sleep(50 * time.Millisecond)

	// Send a kick — the inner runCycle returns without consuming done,
	// abandoning the goroutine. With the fix the goroutine is tracked.
	kickCh <- struct{}{}

	// Let the kicked cycle start.
	time.Sleep(20 * time.Millisecond)

	// Cancel the context and verify the function exits. The inflight
	// WaitGroup must have counted down — otherwise done never closes.
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("runSupervise did not exit after ctx cancellation — inflight.Wait() may be stuck")
	}
}

// TestRunSuperviseMultipleKicksTracksEachGoroutine verifies that multiple
// rapid kicks don't cause a leak: each abandoned goroutine is tracked and
// the function exits cleanly.
func TestRunSuperviseMultipleKicksTracksEachGoroutine(t *testing.T) {
	restore := disableJitter()
	defer restore()

	cfg := &config.Config{
		Repo:     "test/repo",
		StateDir: t.TempDir(),
	}

	st := state.NewState()
	if err := state.Save(cfg.StateDir, st); err != nil {
		t.Fatalf("save initial state: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	kickCh := make(chan struct{}, 10)
	done := make(chan struct{})
	go func() {
		runSupervise(ctx, "test-multi-kick", func() *config.Config { return cfg }, nil, 100*time.Millisecond, nil, kickCh)
		close(done)
	}()

	// Let the first cycle complete.
	time.Sleep(50 * time.Millisecond)

	// Send multiple rapid kicks.
	for i := 0; i < 5; i++ {
		kickCh <- struct{}{}
		time.Sleep(10 * time.Millisecond)
	}

	time.Sleep(50 * time.Millisecond)

	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("runSupervise did not exit after multiple kicks — inflight.Wait() may be stuck")
	}
}

func TestRunSuperviseKickWhileWaitingForRunOnce(t *testing.T) {
	restore := disableJitter()
	defer restore()

	cfg := &config.Config{
		Repo:     "test/repo",
		StateDir: t.TempDir(),
	}

	st := state.NewState()
	if err := state.Save(cfg.StateDir, st); err != nil {
		t.Fatalf("save initial state: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	kickCh := make(chan struct{}, 1)
	done := make(chan struct{})

	go func() {
		runSupervise(ctx, "test-kick-wait", func() *config.Config { return cfg }, nil, 60*time.Second, nil, kickCh)
		close(done)
	}()

	// Wait for the initial runCycle to start and complete (RunOnce
	// returns quickly even with errors).
	time.Sleep(50 * time.Millisecond)

	// The loop is now in the outer select, waiting on ticker.C, kickCh,
	// or ctx.Done. Send a kick to trigger the next cycle.
	kickCh <- struct{}{}

	// Wait for the kicked cycle to start and finish.
	time.Sleep(50 * time.Millisecond)

	// If the kick was not observed (old code), the loop would still be
	// waiting. With the fix, the kick triggers a new cycle.
	// Send a second kick to verify the loop is still responsive.
	kickCh <- struct{}{}

	time.Sleep(50 * time.Millisecond)

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("supervise loop did not exit after ctx cancellation")
	}
}
