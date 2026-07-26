package supervisor

import (
	"context"
	"testing"
	"time"

	"github.com/befeast/maestro/internal/state"
)

// TestWatchdogKicksOnlyAfterAConfirmedWedge pins the self-heal trigger (#1119):
// the watchdog escalates from "warn" to "ask the supervise loop to cancel its
// cycle" only once a second consecutive tick still sees a stale LastRunOnceAt,
// so one slow-but-progressing cycle is never interrupted.
func TestWatchdogKicksOnlyAfterAConfirmedWedge(t *testing.T) {
	dir := t.TempDir()
	st := state.NewState()
	st.LastRunOnceAt = time.Now().UTC().Add(-time.Hour)
	if err := state.Save(dir, st); err != nil {
		t.Fatalf("save state: %v", err)
	}

	interval := 25 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	kickCh := make(chan struct{}, 1)
	go Watchdog(ctx, "wedged", dir, interval, kickCh)

	// Startup grace is 3*interval, then two stuck ticks are needed.
	select {
	case <-kickCh:
	case <-time.After(3 * time.Second):
		t.Fatal("watchdog never kicked despite a permanently stale LastRunOnceAt")
	}
}

// TestWatchdogDoesNotKickWhileCyclesLand verifies the watchdog stays quiet — and
// resets its stuck streak — while the supervise loop keeps stamping fresh cycles.
func TestWatchdogDoesNotKickWhileCyclesLand(t *testing.T) {
	dir := t.TempDir()
	interval := 25 * time.Millisecond

	stamp := func() {
		st, err := state.Load(dir)
		if err != nil {
			t.Fatalf("load state: %v", err)
		}
		st.LastRunOnceAt = time.Now().UTC()
		if err := state.Save(dir, st); err != nil {
			t.Fatalf("save state: %v", err)
		}
	}
	stamp()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	kickCh := make(chan struct{}, 1)
	go Watchdog(ctx, "healthy", dir, interval, kickCh)

	deadline := time.Now().Add(10 * interval)
	for time.Now().Before(deadline) {
		select {
		case <-kickCh:
			t.Fatal("watchdog kicked a healthy supervise loop")
		default:
		}
		stamp()
		time.Sleep(interval / 2)
	}
}

// TestWatchdogNilKickChannelStaysWarnOnly covers the single-project CLI wiring:
// it passes no kick channel, so the watchdog must keep warning without panicking
// on the nil send.
func TestWatchdogNilKickChannelStaysWarnOnly(t *testing.T) {
	dir := t.TempDir()
	st := state.NewState()
	st.LastRunOnceAt = time.Now().UTC().Add(-time.Hour)
	if err := state.Save(dir, st); err != nil {
		t.Fatalf("save state: %v", err)
	}

	interval := 20 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		Watchdog(ctx, "cli", dir, interval, nil)
	}()

	time.Sleep(8 * interval)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("watchdog did not return after ctx cancellation")
	}

	loaded, err := state.Load(dir)
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	if !loaded.SupervisorStuck {
		t.Error("watchdog with a nil kick channel must still persist SupervisorStuck")
	}
}
