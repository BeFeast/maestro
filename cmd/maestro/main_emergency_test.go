package main

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/befeast/maestro/internal/emergencystore"
)

// TestEmergencyCmd_StopLLMAndResume exercises the #840 CLI verbs end-to-end
// against a real unified DB: stop-llm writes the persistent switch the daemon
// honors, and resume clears it. --store is emptied so the test never touches the
// host's real config store.
func TestEmergencyCmd_StopLLMAndResume(t *testing.T) {
	db := filepath.Join(t.TempDir(), "maestro.db")

	emergencyCmd([]string{"stop-llm", "--db", db, "--store", "", "--actor", "tester", "--reason", "runaway burn"})

	store, err := emergencystore.Open(db)
	if err != nil {
		t.Fatalf("open switch db: %v", err)
	}
	st, err := store.Get(context.Background())
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if st.Level != emergencystore.LevelLLMStopped {
		t.Fatalf("level = %q after stop-llm, want llm_stopped", st.Level)
	}
	if st.Actor != "tester" || st.Reason != "runaway burn" {
		t.Fatalf("actor/reason = %q/%q, want tester/runaway burn", st.Actor, st.Reason)
	}
	if st.Since.IsZero() {
		t.Fatal("since not stamped by stop-llm")
	}
	store.Close()

	emergencyCmd([]string{"resume", "--db", db, "--store", "", "--actor", "tester"})

	store, err = emergencystore.Open(db)
	if err != nil {
		t.Fatalf("reopen switch db: %v", err)
	}
	defer store.Close()
	st, _ = store.Get(context.Background())
	if st.Active() {
		t.Fatalf("switch still active after resume: %+v", st)
	}
}

// TestEmergencyCmd_StopAll writes the whole-fleet level.
func TestEmergencyCmd_StopAll(t *testing.T) {
	db := filepath.Join(t.TempDir(), "maestro.db")
	emergencyCmd([]string{"stop-all", "--db", db, "--store", ""})

	store, err := emergencystore.Open(db)
	if err != nil {
		t.Fatalf("open switch db: %v", err)
	}
	defer store.Close()
	st, _ := store.Get(context.Background())
	if st.Level != emergencystore.LevelAllStopped {
		t.Fatalf("level = %q after stop-all, want all_stopped", st.Level)
	}
}
