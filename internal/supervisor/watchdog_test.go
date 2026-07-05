package supervisor

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/befeast/maestro/internal/state"
)

func TestWatchdogKick(t *testing.T) {
	dir := t.TempDir()

	interval := 30 * time.Millisecond
	old := time.Now().UTC().Add(-1 * time.Hour)

	st := state.NewState()
	st.LastRunOnceAt = old
	if err := state.Save(dir, st); err != nil {
		t.Fatalf("save initial state: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	kickCh := make(chan struct{}, 10)
	done := make(chan struct{})
	go func() {
		Watchdog(ctx, "test", dir, interval, kickCh)
		close(done)
	}()

	// Wait past startup grace (3*interval) + at least 2 stuck ticks.
	time.Sleep(8 * interval)

	select {
	case <-kickCh:
	default:
		t.Fatal("expected at least one kick after 2 consecutive stuck ticks, got none")
	}

	// Stop the watchdog so our recovery save doesn't race with
	// persistSupervisorStuck (which also calls state.Save and can cause
	// the 3-way merge to lose our LastRunOnceAt update).
	cancel()
	<-done

	// Re-seed state with a future LastRunOnceAt to simulate recovery.
	// Must use Load+modify+Save to match the loadedHash and avoid the
	// 3-way merge (which doesn't merge LastRunOnceAt).
	st, err := state.Load(dir)
	if err != nil {
		t.Fatalf("load post-watchdog state: %v", err)
	}
	st.LastRunOnceAt = time.Now().UTC().Add(time.Hour)
	if err := state.Save(dir, st); err != nil {
		t.Fatalf("save recovered state: %v", err)
	}

	// Start a fresh watchdog.
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	kickCh2 := make(chan struct{}, 10)
	done2 := make(chan struct{})
	go func() {
		Watchdog(ctx2, "test", dir, interval, kickCh2)
		close(done2)
	}()

	time.Sleep(6 * interval)

	select {
	case <-kickCh2:
		t.Fatal("watchdog kicked again after recovery — consecutiveStuck was not reset")
	default:
	}
}

func TestWatchdogNoKickOnFreshStart(t *testing.T) {
	dir := t.TempDir()

	interval := 30 * time.Millisecond
	st := state.NewState()
	st.LastRunOnceAt = time.Now().UTC().Add(time.Hour)
	if err := state.Save(dir, st); err != nil {
		t.Fatalf("save state: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	kickCh := make(chan struct{}, 10)
	done := make(chan struct{})
	go func() {
		Watchdog(ctx, "test", dir, interval, kickCh)
		close(done)
	}()

	time.Sleep(6 * interval)

	select {
	case <-kickCh:
		t.Fatal("watchdog kicked despite fresh LastRunOnceAt (future timestamp)")
	default:
	}
}

func TestWatchdogNilKickCh(t *testing.T) {
	dir := t.TempDir()

	interval := 20 * time.Millisecond
	st := state.NewState()
	st.LastRunOnceAt = time.Now().UTC().Add(-1 * time.Hour)
	if err := state.Save(dir, st); err != nil {
		t.Fatalf("save state: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		Watchdog(ctx, "test", dir, interval, nil)
		close(done)
	}()

	time.Sleep(5 * interval)
	cancel()
	<-done
}

func TestWatchdogKickZeroLastRunOnceAt(t *testing.T) {
	dir := t.TempDir()
	interval := 30 * time.Millisecond
	st := state.NewState()
	if err := state.Save(dir, st); err != nil {
		t.Fatalf("save state: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	kickCh := make(chan struct{}, 10)
	done := make(chan struct{})
	go func() {
		Watchdog(ctx, "test", dir, interval, kickCh)
		close(done)
	}()

	time.Sleep(8 * interval)

	select {
	case <-kickCh:
	default:
		t.Fatal("expected kick for zero LastRunOnceAt after consecutive stuck ticks")
	}
}

func TestWatchdogKickMissingStateFile(t *testing.T) {
	dir := filepath.Join(os.TempDir(), "maestro-test-watchdog-missing-"+t.Name())
	defer os.RemoveAll(dir)

	interval := 20 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var kicked int32
	kickCh := make(chan struct{}, 10)
	done := make(chan struct{})
	go func() {
		Watchdog(ctx, "test", dir, interval, kickCh)
		close(done)
	}()

	go func() {
		for range kickCh {
			atomic.AddInt32(&kicked, 1)
		}
	}()

	time.Sleep(5 * interval)
	cancel()
	<-done
	_ = kicked
}

func TestConsecutiveStuckResetAfterPartialRecovery(t *testing.T) {
	dir := t.TempDir()
	interval := 30 * time.Millisecond
	old := time.Now().UTC().Add(-1 * time.Hour)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	kickCh := make(chan struct{}, 10)
	done := make(chan struct{})
	go func() {
		Watchdog(ctx, "test", dir, interval, kickCh)
		close(done)
	}()

	// Wait past startup grace.
	time.Sleep(5 * interval)

	// Make state stale.
	st := state.NewState()
	st.LastRunOnceAt = old
	state.Save(dir, st)
	time.Sleep(3 * interval) // 2+ stuck ticks → kick

	// Drain any kick.
	_ = len(kickCh) > 0 && <-kickCh != struct{}{}

	// Stop the watchdog to avoid concurrent state.Save during recovery.
	cancel()
	<-done

	// Stamp a future timestamp to reset counter.
	// Use Load+modify+Save to match loadedHash (avoids 3-way merge).
	st, _ = state.Load(dir)
	st.LastRunOnceAt = time.Now().UTC().Add(time.Hour)
	state.Save(dir, st)

	// Start fresh.
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	kickCh2 := make(chan struct{}, 10)
	done2 := make(chan struct{})
	go func() {
		Watchdog(ctx2, "test", dir, interval, kickCh2)
		close(done2)
	}()

	// Wait long enough for a few ticks; should stay clean.
	time.Sleep(4 * interval)

	// Make stale again — save well before the next tick so the watchdog
	// is guaranteed to see it. Use Load+modify+Save to match loadedHash.
	st, _ = state.Load(dir)
	st.LastRunOnceAt = old
	state.Save(dir, st)

	// Wait for startup grace (3*interval) + 2 stuck ticks + margin.
	time.Sleep(6 * interval)

	select {
	case <-kickCh2:
	default:
		t.Fatal("expected kick after 2 consecutive stuck ticks following a reset")
	}
}

// TestWatchdogClearsSupervisorStuckOnRecovery verifies that when the
// watchdog detects a fresh LastRunOnceAt it clears the SupervisorStuck
// flag (#816).
func TestWatchdogClearsSupervisorStuckOnRecovery(t *testing.T) {
	dir := t.TempDir()
	interval := 30 * time.Millisecond

	st := state.NewState()
	st.LastRunOnceAt = time.Now().UTC().Add(-1 * time.Hour)
	st.SupervisorStuck = true
	st.SupervisorStuckReason = "synthetic stuck for test"
	if err := state.Save(dir, st); err != nil {
		t.Fatalf("save initial stuck state: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		Watchdog(ctx, "test", dir, interval, nil)
		close(done)
	}()

	// Wait past startup grace so the watchdog loads state.
	time.Sleep(4 * interval)

	// Stamp a fresh LastRunOnceAt to simulate recovery.
	// Use Load+modify+Save to match loadedHash.
	st2, err := state.Load(dir)
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	st2.LastRunOnceAt = time.Now().UTC().Add(time.Minute)
	if err := state.Save(dir, st2); err != nil {
		t.Fatalf("save recovered state: %v", err)
	}

	// Wait for the watchdog to tick and see the fresh timestamp.
	time.Sleep(3 * interval)

	cancel()
	<-done

	// Verify SupervisorStuck was cleared.
	st3, err := state.Load(dir)
	if err != nil {
		t.Fatalf("load final state: %v", err)
	}
	if st3.SupervisorStuck {
		t.Fatalf("SupervisorStuck still true after recovery; watchdog should have cleared it")
	}
	if st3.SupervisorStuckReason != "" {
		t.Fatalf("SupervisorStuckReason=%q; should be cleared on recovery", st3.SupervisorStuckReason)
	}
}
