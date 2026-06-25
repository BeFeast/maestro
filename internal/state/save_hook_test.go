package state

import (
	"testing"
)

// TestSaveHookFiresOnSave proves Save invokes the registered write-through hook
// with the persisted snapshot (#760): this is the single chokepoint that lets the
// daemon mirror EVERY save — orchestrator/supervisor/watchdog/server — into the
// shared maestro.db, not just the fleet read path.
func TestSaveHookFiresOnSave(t *testing.T) {
	dir := t.TempDir()
	var gotDir string
	var gotSessions int
	SetSaveHook(func(stateDir string, s *State) {
		gotDir = stateDir
		gotSessions = len(s.Sessions)
	})
	t.Cleanup(func() { SetSaveHook(nil) })

	st := NewState()
	st.Sessions["1"] = &Session{IssueNumber: 7, Status: StatusRunning}
	if err := Save(dir, st); err != nil {
		t.Fatalf("save: %v", err)
	}
	if gotDir != dir {
		t.Fatalf("hook got state dir %q, want %q", gotDir, dir)
	}
	if gotSessions != 1 {
		t.Fatalf("hook saw %d sessions, want 1", gotSessions)
	}
}

// TestSaveHookNilIsNoop asserts a cleared hook leaves Save on the JSON-only path.
func TestSaveHookNilIsNoop(t *testing.T) {
	SetSaveHook(nil)
	dir := t.TempDir()
	st := NewState()
	if err := Save(dir, st); err != nil {
		t.Fatalf("save: %v", err)
	}
}
