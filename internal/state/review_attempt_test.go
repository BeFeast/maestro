package state

import (
	"errors"
	"testing"
)

func TestDurableUpdateSyncFailureReturnsNoGrantAndRetainsIntent(t *testing.T) {
	dir := t.TempDir()
	if err := Save(dir, NewState()); err != nil {
		t.Fatal(err)
	}
	failure := errors.New("fixture directory sync failure")
	called := false
	err := updateState(dir, func(st *State) error {
		st.ReviewAttempts = map[string]ReviewAttemptTrack{"scope": {Revision: 1, Attempts: []ReviewAttempt{{ID: "fixture", Outcome: "launch_intent"}}}}
		return nil
	}, func(path string) error {
		called = true
		// This seam runs after rename, while the same state flock is held.
		st, _, readErr := readStateFile(StatePath(path))
		if readErr != nil {
			t.Fatal(readErr)
		}
		if st.ReviewAttempts["scope"].Revision != 1 {
			t.Fatal("sync happened before mutation")
		}
		return failure
	})
	if !called || !errors.Is(err, failure) {
		t.Fatalf("sync error not propagated: %v", err)
	}
	st, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if st.ReviewAttempts["scope"].Attempts[0].Outcome != "launch_intent" {
		t.Fatal("uncertain claim disappeared")
	}
	if err := UpdateDurable(dir, func(st *State) error { st.NextSlot++; return nil }); err != nil {
		t.Fatal(err)
	}
}
