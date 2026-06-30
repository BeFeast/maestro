package state

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestRunningSessions_OnlyCountsRunning(t *testing.T) {
	s := NewState()
	s.Sessions["a"] = &Session{Status: StatusRunning}
	s.Sessions["b"] = &Session{Status: StatusRunning}
	s.Sessions["c"] = &Session{Status: StatusPROpen} // active but no live worker
	s.Sessions["d"] = &Session{Status: StatusDone}
	s.Sessions["e"] = &Session{Status: StatusDead}

	if got := s.RunningSessionCount(); got != 2 {
		t.Fatalf("RunningSessionCount() = %d, want 2 (pr_open/done/dead must not count)", got)
	}
	if got := len(s.RunningSessions()); got != 2 {
		t.Fatalf("len(RunningSessions()) = %d, want 2", got)
	}
}

func TestSetAndClearSpawnDrain(t *testing.T) {
	s := NewState()
	if s.DrainActive() {
		t.Fatal("new state should not be draining")
	}

	at := time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC)
	s.SetSpawnDrain(at)
	if !s.DrainActive() {
		t.Fatal("DrainActive() false after SetSpawnDrain")
	}
	if !s.SpawnDrainAt.Equal(at) {
		t.Fatalf("SpawnDrainAt = %v, want %v", s.SpawnDrainAt, at)
	}
	if !s.SpawnDrainClearAt.IsZero() {
		t.Fatalf("SpawnDrainClearAt = %v, want zero while drain is active", s.SpawnDrainClearAt)
	}

	later := at.Add(time.Minute)
	s.ClearSpawnDrain(later)
	if s.DrainActive() {
		t.Fatal("DrainActive() true after ClearSpawnDrain")
	}
	if !s.SpawnDrainAt.IsZero() {
		t.Fatalf("SpawnDrainAt = %v, want zero after clear", s.SpawnDrainAt)
	}
	if !s.SpawnDrainClearAt.Equal(later) {
		t.Fatalf("SpawnDrainClearAt = %v, want %v after clear", s.SpawnDrainClearAt, later)
	}
}

func TestSpawnDrainJSONOmitsInactiveDrainAt(t *testing.T) {
	s := NewState()
	at := time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC)
	s.SetSpawnDrain(at)
	active, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshal active state: %v", err)
	}
	if !strings.Contains(string(active), `"spawn_drain_at"`) {
		t.Fatalf("active state JSON = %s, want spawn_drain_at", active)
	}

	s.ClearSpawnDrain(at.Add(time.Minute))
	cleared, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshal cleared state: %v", err)
	}
	if strings.Contains(string(cleared), `"spawn_drain_at"`) {
		t.Fatalf("cleared state JSON = %s, want no spawn_drain_at", cleared)
	}
	if !strings.Contains(string(cleared), `"spawn_drain_clear_at"`) {
		t.Fatalf("cleared state JSON = %s, want spawn_drain_clear_at", cleared)
	}
}

func TestSpawnDrain_SurvivesSaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s := NewState()
	at := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	s.SetSpawnDrain(at)
	if err := Save(dir, s); err != nil {
		t.Fatalf("save: %v", err)
	}

	reloaded, err := Load(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !reloaded.DrainActive() {
		t.Fatal("drain flag lost across save/load")
	}
	if !reloaded.SpawnDrainAt.Equal(at) {
		t.Fatalf("SpawnDrainAt = %v, want %v after round trip", reloaded.SpawnDrainAt, at)
	}
}

// TestMergeSpawnDrain_LatestWriteWins covers the concurrent-save path: a drain
// request must survive an orchestrator save that does not carry the flag (older
// timestamp), and a fresh clear must not be undone by a stale set.
func TestMergeSpawnDrain_LatestWriteWins(t *testing.T) {
	t0 := time.Date(2026, 6, 1, 9, 0, 0, 0, time.UTC)

	t.Run("fresh drain beats stale current", func(t *testing.T) {
		base := NewState()
		current := NewState() // orchestrator's on-disk snapshot, no drain
		ours := NewState()
		ours.SetSpawnDrain(t0.Add(time.Minute)) // drain CLI's newer write

		merged, err := mergeStateSnapshots(base, current, ours)
		if err != nil {
			t.Fatalf("merge: %v", err)
		}
		if !merged.DrainActive() {
			t.Fatal("fresh drain request lost to stale concurrent save")
		}
	})

	t.Run("fresh clear beats stale set", func(t *testing.T) {
		base := NewState()
		current := NewState()
		current.ClearSpawnDrain(t0.Add(2 * time.Minute)) // daemon's fresh clear
		ours := NewState()
		ours.SetSpawnDrain(t0) // a stale set

		merged, err := mergeStateSnapshots(base, current, ours)
		if err != nil {
			t.Fatalf("merge: %v", err)
		}
		if merged.DrainActive() {
			t.Fatal("stale set undid a fresh clear")
		}
		if !merged.SpawnDrainAt.IsZero() {
			t.Fatalf("SpawnDrainAt = %v, want zero after merged clear", merged.SpawnDrainAt)
		}
		if !merged.SpawnDrainClearAt.Equal(t0.Add(2 * time.Minute)) {
			t.Fatalf("SpawnDrainClearAt = %v, want fresh clear timestamp", merged.SpawnDrainClearAt)
		}
	})
}

func TestNormalizeMigratesInactiveSpawnDrainAtToClearAt(t *testing.T) {
	clearAt := time.Date(2026, 6, 1, 10, 30, 0, 0, time.UTC)
	s := NewState()
	s.SpawnDrain = false
	s.SpawnDrainAt = clearAt
	s.normalize()

	if !s.SpawnDrainAt.IsZero() {
		t.Fatalf("SpawnDrainAt = %v, want zero for inactive drain", s.SpawnDrainAt)
	}
	if !s.SpawnDrainClearAt.Equal(clearAt) {
		t.Fatalf("SpawnDrainClearAt = %v, want migrated clear timestamp %v", s.SpawnDrainClearAt, clearAt)
	}
}
