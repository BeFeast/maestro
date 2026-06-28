package daemon

import (
	"testing"
	"time"
)

func TestComputeSuperviseJitter(t *testing.T) {
	if d := computeSuperviseJitter(0, 0.5); d != 0 {
		t.Fatalf("non-positive interval jitter = %s, want 0", d)
	}
	if d := computeSuperviseJitter(-time.Second, 0.5); d != 0 {
		t.Fatalf("negative interval jitter = %s, want 0", d)
	}
	if d := computeSuperviseJitter(40*time.Second, 0); d != 0 {
		t.Fatalf("frac 0 jitter = %s, want 0", d)
	}
	if d := computeSuperviseJitter(40*time.Second, 0.5); d != 20*time.Second {
		t.Fatalf("frac 0.5 of 40s = %s, want 20s", d)
	}
	// A long supervise interval is clamped to the cap so a fresh daemon still
	// runs its first cycle promptly. rand.Float64 yields [0,1), so the largest
	// realizable offset stays strictly under the cap.
	if d := computeSuperviseJitter(5*time.Minute, 0.999); d >= superviseJitterCap {
		t.Fatalf("jitter %s, want < cap %s for a long interval", d, superviseJitterCap)
	}
	if d := computeSuperviseJitter(5*time.Minute, 0.5); d != superviseJitterCap/2 {
		t.Fatalf("frac 0.5 above cap = %s, want %s", d, superviseJitterCap/2)
	}
}

// TestSuperviseStartupJitter_Bounds checks the live (rng-backed) entry point
// keeps every offset within [0, min(interval, cap)) across many draws.
func TestSuperviseStartupJitter_Bounds(t *testing.T) {
	interval := 90 * time.Second // > cap, so the bound is the cap
	for i := 0; i < 1000; i++ {
		d := superviseStartupJitter(interval)
		if d < 0 || d >= superviseJitterCap {
			t.Fatalf("draw %d = %s, want within [0, %s)", i, d, superviseJitterCap)
		}
	}
	if superviseStartupJitter(0) != 0 {
		t.Fatal("non-positive interval must yield 0 jitter")
	}
}
