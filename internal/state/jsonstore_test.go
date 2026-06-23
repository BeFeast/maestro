package state

import (
	"os"
	"reflect"
	"testing"
	"time"
)

// TestJSONStoreRoundTrip confirms NewJSONStore is a 1:1 wrapper over the
// package-level Load/Save: a State saved through the Store loads back through
// the Store with identical contents, and reads identically via the free
// function on the same directory (same file, same format).
func TestJSONStoreRoundTrip(t *testing.T) {
	dir := t.TempDir()
	store := NewJSONStore(dir)

	s := NewState()
	s.Sessions["slot-1"] = &Session{
		IssueNumber: 42,
		Branch:      "feat/test",
		Status:      StatusPROpen,
		PRNumber:    10,
		StartedAt:   time.Now().UTC().Truncate(time.Second),
	}
	if err := store.Save(s); err != nil {
		t.Fatalf("store.Save: %v", err)
	}

	loaded, err := store.Load()
	if err != nil {
		t.Fatalf("store.Load: %v", err)
	}
	sess := loaded.Sessions["slot-1"]
	if sess == nil {
		t.Fatal("slot-1 not found after Store round-trip")
	}
	if sess.IssueNumber != 42 || sess.PRNumber != 10 || sess.Status != StatusPROpen {
		t.Fatalf("session mismatch after round-trip: %+v", sess)
	}

	// Same file, same format: the free function reads the same bytes the
	// Store wrote, and produces an equivalent State.
	viaFree, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !reflect.DeepEqual(loaded.Sessions, viaFree.Sessions) {
		t.Fatalf("Store and free-function Load disagree:\n store=%#v\n free =%#v", loaded.Sessions, viaFree.Sessions)
	}
}

// TestJSONStoreLoadEmpty confirms loading from a directory with no state file
// returns a fresh State (no error), matching the free function's behavior.
func TestJSONStoreLoadEmpty(t *testing.T) {
	dir := t.TempDir()
	store := NewJSONStore(dir)

	s, err := store.Load()
	if err != nil {
		t.Fatalf("store.Load on empty dir: %v", err)
	}
	if s == nil || s.Sessions == nil {
		t.Fatal("expected a fresh initialized State from empty dir")
	}
	if len(s.Sessions) != 0 {
		t.Fatalf("expected no sessions, got %d", len(s.Sessions))
	}

	// No state file should have been created by a Load.
	if _, statErr := os.Stat(StatePath(dir)); !os.IsNotExist(statErr) {
		t.Fatalf("Load should not create the state file; stat err = %v", statErr)
	}
}
