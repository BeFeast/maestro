package state

import (
	"testing"
	"time"
)

func TestSetAndClearPaused(t *testing.T) {
	s := NewState()
	if s.PauseActive() {
		t.Fatal("new state should not be paused")
	}

	at := time.Date(2026, 6, 11, 10, 0, 0, 0, time.UTC)
	s.SetPaused(at)
	if !s.PauseActive() {
		t.Fatal("PauseActive() false after SetPaused")
	}
	if !s.PausedAt.Equal(at) {
		t.Fatalf("PausedAt = %v, want %v", s.PausedAt, at)
	}

	later := at.Add(time.Minute)
	s.ClearPaused(later)
	if s.PauseActive() {
		t.Fatal("PauseActive() true after ClearPaused")
	}
	if !s.PausedAt.Equal(later) {
		t.Fatalf("PausedAt = %v, want %v after clear", s.PausedAt, later)
	}
}

// TestPaused_SurvivesSaveLoadRoundTrip is the restart-persistence guarantee
// (#683 AC4): unlike the drain flag, pause is never cleared on startup, so a
// `systemctl --user restart` of both project units must reload it intact.
func TestPaused_SurvivesSaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s := NewState()
	at := time.Date(2026, 6, 11, 12, 0, 0, 0, time.UTC)
	s.SetPaused(at)
	if err := Save(dir, s); err != nil {
		t.Fatalf("save: %v", err)
	}

	reloaded, err := Load(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !reloaded.PauseActive() {
		t.Fatal("paused flag lost across save/load")
	}
	if !reloaded.PausedAt.Equal(at) {
		t.Fatalf("PausedAt = %v, want %v after round trip", reloaded.PausedAt, at)
	}
}

// TestMergePaused_LatestWriteWins covers the concurrent-save path: a pause
// request must survive an orchestrator save that does not carry the flag
// (older timestamp), and a fresh resume must not be undone by a stale set.
func TestMergePaused_LatestWriteWins(t *testing.T) {
	t0 := time.Date(2026, 6, 11, 9, 0, 0, 0, time.UTC)

	t.Run("fresh pause beats stale current", func(t *testing.T) {
		base := NewState()
		current := NewState() // orchestrator's on-disk snapshot, not paused
		ours := NewState()
		ours.SetPaused(t0.Add(time.Minute)) // pause CLI's newer write

		merged, err := mergeStateSnapshots(base, current, ours)
		if err != nil {
			t.Fatalf("merge: %v", err)
		}
		if !merged.PauseActive() {
			t.Fatal("fresh pause request lost to stale concurrent save")
		}
	})

	t.Run("fresh resume beats stale set", func(t *testing.T) {
		base := NewState()
		current := NewState()
		current.ClearPaused(t0.Add(2 * time.Minute)) // resume CLI's fresh clear
		ours := NewState()
		ours.SetPaused(t0) // a stale set

		merged, err := mergeStateSnapshots(base, current, ours)
		if err != nil {
			t.Fatalf("merge: %v", err)
		}
		if merged.PauseActive() {
			t.Fatal("stale set undid a fresh resume")
		}
	})
}
