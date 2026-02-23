package state

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestNotifiedCIFail_Persistence(t *testing.T) {
	dir := t.TempDir()

	// Create state with NotifiedCIFail set
	s := NewState()
	s.Sessions["slot-1"] = &Session{
		IssueNumber:    42,
		Branch:         "feat/test",
		Status:         StatusPROpen,
		PRNumber:       10,
		StartedAt:      time.Now().UTC(),
		NotifiedCIFail: true,
	}
	s.Sessions["slot-2"] = &Session{
		IssueNumber:    43,
		Branch:         "feat/other",
		Status:         StatusPROpen,
		PRNumber:       11,
		StartedAt:      time.Now().UTC(),
		NotifiedCIFail: false,
	}

	// Save
	if err := Save(dir, s); err != nil {
		t.Fatalf("save: %v", err)
	}

	// Load and verify
	loaded, err := Load(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	sess1 := loaded.Sessions["slot-1"]
	if sess1 == nil {
		t.Fatal("slot-1 not found after load")
	}
	if !sess1.NotifiedCIFail {
		t.Error("slot-1: NotifiedCIFail should be true after load")
	}

	sess2 := loaded.Sessions["slot-2"]
	if sess2 == nil {
		t.Fatal("slot-2 not found after load")
	}
	if sess2.NotifiedCIFail {
		t.Error("slot-2: NotifiedCIFail should be false after load")
	}
}

func TestNotifiedCIFail_OmittedWhenFalse(t *testing.T) {
	dir := t.TempDir()

	s := NewState()
	s.Sessions["slot-1"] = &Session{
		IssueNumber:    42,
		Branch:         "feat/test",
		Status:         StatusPROpen,
		PRNumber:       10,
		StartedAt:      time.Now().UTC(),
		NotifiedCIFail: false,
	}

	if err := Save(dir, s); err != nil {
		t.Fatalf("save: %v", err)
	}

	// Read raw JSON and verify the field is omitted
	data, err := os.ReadFile(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatalf("read file: %v", err)
	}

	json := string(data)
	if containsString(json, "notified_ci_fail") {
		t.Error("notified_ci_fail should be omitted from JSON when false")
	}
}

func TestNotifiedCIFail_BackwardCompatibility(t *testing.T) {
	dir := t.TempDir()

	// Write a state file without the NotifiedCIFail field (simulating old state)
	oldJSON := `{
  "sessions": {
    "slot-1": {
      "issue_number": 42,
      "branch": "feat/test",
      "status": "pr_open",
      "pr_number": 10,
      "started_at": "2025-01-01T00:00:00Z"
    }
  },
  "next_slot": 2
}`
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte(oldJSON), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}

	// Load should succeed and default NotifiedCIFail to false
	loaded, err := Load(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	sess := loaded.Sessions["slot-1"]
	if sess == nil {
		t.Fatal("slot-1 not found")
	}
	if sess.NotifiedCIFail {
		t.Error("NotifiedCIFail should default to false for old state files")
	}
}

func TestRetryCount_Persistence(t *testing.T) {
	dir := t.TempDir()

	s := NewState()
	s.Sessions["slot-1"] = &Session{
		IssueNumber: 42,
		Branch:      "feat/test",
		Status:      StatusRunning,
		StartedAt:   time.Now().UTC(),
		RetryCount:  1,
	}
	s.Sessions["slot-2"] = &Session{
		IssueNumber: 43,
		Branch:      "feat/other",
		Status:      StatusRunning,
		StartedAt:   time.Now().UTC(),
		RetryCount:  0,
	}

	if err := Save(dir, s); err != nil {
		t.Fatalf("save: %v", err)
	}

	loaded, err := Load(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	sess1 := loaded.Sessions["slot-1"]
	if sess1 == nil {
		t.Fatal("slot-1 not found after load")
	}
	if sess1.RetryCount != 1 {
		t.Errorf("slot-1: RetryCount = %d, want 1", sess1.RetryCount)
	}

	sess2 := loaded.Sessions["slot-2"]
	if sess2 == nil {
		t.Fatal("slot-2 not found after load")
	}
	if sess2.RetryCount != 0 {
		t.Errorf("slot-2: RetryCount = %d, want 0", sess2.RetryCount)
	}
}

func TestRetryCount_OmittedWhenZero(t *testing.T) {
	dir := t.TempDir()

	s := NewState()
	s.Sessions["slot-1"] = &Session{
		IssueNumber: 42,
		Branch:      "feat/test",
		Status:      StatusRunning,
		StartedAt:   time.Now().UTC(),
		RetryCount:  0,
	}

	if err := Save(dir, s); err != nil {
		t.Fatalf("save: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatalf("read file: %v", err)
	}

	json := string(data)
	if containsString(json, "retry_count") {
		t.Error("retry_count should be omitted from JSON when zero")
	}
}

func TestRetryCount_BackwardCompatibility(t *testing.T) {
	dir := t.TempDir()

	// State file without retry_count (simulating old state)
	oldJSON := `{
  "sessions": {
    "slot-1": {
      "issue_number": 42,
      "branch": "feat/test",
      "status": "running",
      "started_at": "2025-01-01T00:00:00Z"
    }
  },
  "next_slot": 2
}`
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte(oldJSON), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}

	loaded, err := Load(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	sess := loaded.Sessions["slot-1"]
	if sess == nil {
		t.Fatal("slot-1 not found")
	}
	if sess.RetryCount != 0 {
		t.Errorf("RetryCount should default to 0 for old state files, got %d", sess.RetryCount)
	}
}

func containsString(s, substr string) bool {
	return len(s) >= len(substr) && searchString(s, substr)
}

func searchString(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
