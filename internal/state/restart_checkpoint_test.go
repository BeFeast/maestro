package state

import (
	"testing"
	"time"
)

// #877: the restart-checkpoint marker the daemon stamps on an in-flight worker
// at shutdown must be durable across the process restart — it lives in the same
// state file the next daemon loads, so its reconcile can resume the session in
// place exactly once instead of a false running->dead.
func TestRestartCheckpointAt_SurvivesSaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	at := time.Date(2026, 7, 12, 18, 30, 0, 0, time.UTC)

	s := NewState()
	s.Sessions["sup-310"] = &Session{
		IssueNumber:         310,
		Status:              StatusRunning,
		Worktree:            "/wt/sup-310",
		Branch:              "feat/sup-310",
		RestartCheckpointAt: &at,
		CheckpointFile:      "/wt/sup-310/CHECKPOINT.md",
	}
	if err := Save(dir, s); err != nil {
		t.Fatalf("save: %v", err)
	}

	reloaded, err := Load(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	got := reloaded.Sessions["sup-310"]
	if got == nil {
		t.Fatal("session lost across save/load")
	}
	if got.RestartCheckpointAt == nil {
		t.Fatal("RestartCheckpointAt lost across save/load")
	}
	if !got.RestartCheckpointAt.Equal(at) {
		t.Fatalf("RestartCheckpointAt = %v, want %v", got.RestartCheckpointAt, at)
	}
	if got.CheckpointFile != "/wt/sup-310/CHECKPOINT.md" {
		t.Fatalf("CheckpointFile = %q, want the saved path", got.CheckpointFile)
	}
}

// A session with no restart checkpoint must serialize the marker as omitted (it
// is omitempty), so the common case adds no noise to the state file.
func TestRestartCheckpointAt_OmittedWhenUnset(t *testing.T) {
	dir := t.TempDir()
	s := NewState()
	s.Sessions["sup-1"] = &Session{IssueNumber: 1, Status: StatusRunning}
	if err := Save(dir, s); err != nil {
		t.Fatalf("save: %v", err)
	}
	reloaded, err := Load(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := reloaded.Sessions["sup-1"]; got == nil || got.RestartCheckpointAt != nil {
		t.Fatalf("RestartCheckpointAt = %v, want nil for an unmarked session", reloaded.Sessions["sup-1"].RestartCheckpointAt)
	}
}
