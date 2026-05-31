package main

import (
	"errors"
	"testing"
	"time"
)

// fakeClock advances only when sleep is called, making drainWait fully
// deterministic and instant under test.
type fakeClock struct {
	now time.Time
}

func (c *fakeClock) Now() time.Time        { return c.now }
func (c *fakeClock) Sleep(d time.Duration) { c.now = c.now.Add(d) }

func TestDrainWait_ReturnsWhenWorkersFinish(t *testing.T) {
	clk := &fakeClock{now: time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)}
	// 3 -> 3 -> 1 -> 0 across successive observations.
	counts := []int{3, 3, 1, 0}
	idx := 0
	runningCount := func() (int, error) {
		v := counts[idx]
		if idx < len(counts)-1 {
			idx++
		}
		return v, nil
	}

	var observed []int
	err := drainWait(runningCount, 10*time.Minute, time.Second, clk.Now, clk.Sleep, func(r int) {
		observed = append(observed, r)
	})
	if err != nil {
		t.Fatalf("drainWait returned %v, want nil once workers reached 0", err)
	}
	if got := observed[len(observed)-1]; got != 0 {
		t.Fatalf("last observed running = %d, want 0", got)
	}
	// Should have slept 3 times (after 3, 3, 1) before observing 0.
	wantElapsed := 3 * time.Second
	if elapsed := clk.now.Sub(time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)); elapsed != wantElapsed {
		t.Fatalf("elapsed = %v, want %v", elapsed, wantElapsed)
	}
}

func TestDrainWait_ReturnsImmediatelyWhenNoWorkers(t *testing.T) {
	clk := &fakeClock{now: time.Unix(0, 0).UTC()}
	calls := 0
	runningCount := func() (int, error) {
		calls++
		return 0, nil
	}

	err := drainWait(runningCount, time.Hour, time.Second, clk.Now, clk.Sleep, nil)
	if err != nil {
		t.Fatalf("drainWait = %v, want nil with no running workers", err)
	}
	if calls != 1 {
		t.Fatalf("runningCount called %d times, want 1 (no polling needed)", calls)
	}
	if !clk.now.Equal(time.Unix(0, 0).UTC()) {
		t.Fatal("drainWait slept despite zero workers")
	}
}

func TestDrainWait_TimesOutWithWorkersStillRunning(t *testing.T) {
	clk := &fakeClock{now: time.Unix(0, 0).UTC()}
	runningCount := func() (int, error) {
		return 2, nil // never drains
	}

	err := drainWait(runningCount, 10*time.Second, 3*time.Second, clk.Now, clk.Sleep, nil)
	if !errors.Is(err, errDrainTimeout) {
		t.Fatalf("drainWait = %v, want errDrainTimeout", err)
	}
	// Must not overshoot the deadline.
	if elapsed := clk.now.Sub(time.Unix(0, 0).UTC()); elapsed > 10*time.Second {
		t.Fatalf("elapsed = %v overshot the 10s timeout", elapsed)
	}
}

func TestDrainWait_PropagatesCountError(t *testing.T) {
	clk := &fakeClock{now: time.Unix(0, 0).UTC()}
	wantErr := errors.New("boom")
	runningCount := func() (int, error) {
		return 0, wantErr
	}

	err := drainWait(runningCount, time.Hour, time.Second, clk.Now, clk.Sleep, nil)
	if !errors.Is(err, wantErr) {
		t.Fatalf("drainWait = %v, want %v", err, wantErr)
	}
}
