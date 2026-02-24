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

func TestLastNotifiedStatus_Persistence(t *testing.T) {
	dir := t.TempDir()

	s := NewState()
	s.Sessions["slot-1"] = &Session{
		IssueNumber:        42,
		Branch:             "feat/test",
		Status:             StatusPROpen,
		PRNumber:           10,
		StartedAt:          time.Now().UTC(),
		LastNotifiedStatus: "ci_failure",
	}
	s.Sessions["slot-2"] = &Session{
		IssueNumber: 43,
		Branch:      "feat/other",
		Status:      StatusPROpen,
		PRNumber:    11,
		StartedAt:   time.Now().UTC(),
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
	if sess1.LastNotifiedStatus != "ci_failure" {
		t.Errorf("slot-1: LastNotifiedStatus = %q, want %q", sess1.LastNotifiedStatus, "ci_failure")
	}

	sess2 := loaded.Sessions["slot-2"]
	if sess2 == nil {
		t.Fatal("slot-2 not found after load")
	}
	if sess2.LastNotifiedStatus != "" {
		t.Errorf("slot-2: LastNotifiedStatus = %q, want empty", sess2.LastNotifiedStatus)
	}
}

func TestLastNotifiedStatus_OmittedWhenEmpty(t *testing.T) {
	dir := t.TempDir()

	s := NewState()
	s.Sessions["slot-1"] = &Session{
		IssueNumber: 42,
		Branch:      "feat/test",
		Status:      StatusPROpen,
		PRNumber:    10,
		StartedAt:   time.Now().UTC(),
	}

	if err := Save(dir, s); err != nil {
		t.Fatalf("save: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatalf("read file: %v", err)
	}

	json := string(data)
	if containsString(json, "last_notified_status") {
		t.Error("last_notified_status should be omitted from JSON when empty")
	}
}

func TestLastNotifiedStatus_BackwardCompatibility(t *testing.T) {
	dir := t.TempDir()

	// State file without last_notified_status (simulating old state)
	oldJSON := `{
  "sessions": {
    "slot-1": {
      "issue_number": 42,
      "branch": "feat/test",
      "status": "pr_open",
      "pr_number": 10,
      "started_at": "2025-01-01T00:00:00Z",
      "notified_ci_fail": true
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
	if sess.LastNotifiedStatus != "" {
		t.Errorf("LastNotifiedStatus should default to empty for old state files, got %q", sess.LastNotifiedStatus)
	}
	// Old NotifiedCIFail should still load correctly
	if !sess.NotifiedCIFail {
		t.Error("NotifiedCIFail should still load from old state files")
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

func TestIsTerminal(t *testing.T) {
	tests := []struct {
		status SessionStatus
		want   bool
	}{
		{StatusQueued, false},
		{StatusRunning, false},
		{StatusPROpen, false},
		{StatusDone, true},
		{StatusFailed, true},
		{StatusConflictFailed, true},
		{StatusDead, true},
	}
	for _, tt := range tests {
		if got := IsTerminal(tt.status); got != tt.want {
			t.Errorf("IsTerminal(%q) = %v, want %v", tt.status, got, tt.want)
		}
	}
}

func TestCompletedSessions_Empty(t *testing.T) {
	s := NewState()
	if got := s.CompletedSessions(); len(got) != 0 {
		t.Errorf("expected 0 completed sessions, got %d", len(got))
	}
}

func TestCompletedSessions_FiltersAndSorts(t *testing.T) {
	now := time.Now().UTC()
	t1 := now.Add(-3 * time.Hour)
	t2 := now.Add(-1 * time.Hour)
	t3 := now.Add(-2 * time.Hour)

	s := NewState()
	s.Sessions["slot-1"] = &Session{
		IssueNumber: 1,
		Status:      StatusDone,
		StartedAt:   now.Add(-4 * time.Hour),
		FinishedAt:  &t1,
	}
	s.Sessions["slot-2"] = &Session{
		IssueNumber: 2,
		Status:      StatusRunning, // should be excluded
		StartedAt:   now.Add(-2 * time.Hour),
	}
	s.Sessions["slot-3"] = &Session{
		IssueNumber: 3,
		Status:      StatusDead,
		StartedAt:   now.Add(-3 * time.Hour),
		FinishedAt:  &t2,
	}
	s.Sessions["slot-4"] = &Session{
		IssueNumber: 4,
		Status:      StatusConflictFailed,
		StartedAt:   now.Add(-5 * time.Hour),
		FinishedAt:  &t3,
	}

	completed := s.CompletedSessions()
	if len(completed) != 3 {
		t.Fatalf("expected 3 completed, got %d", len(completed))
	}

	// Should be sorted by FinishedAt descending: slot-3 (1h), slot-4 (2h), slot-1 (3h)
	if completed[0].IssueNumber != 3 {
		t.Errorf("first should be issue 3 (most recent), got %d", completed[0].IssueNumber)
	}
	if completed[1].IssueNumber != 4 {
		t.Errorf("second should be issue 4, got %d", completed[1].IssueNumber)
	}
	if completed[2].IssueNumber != 1 {
		t.Errorf("third should be issue 1 (oldest), got %d", completed[2].IssueNumber)
	}
}

func TestPruneOldSessions(t *testing.T) {
	now := time.Now().UTC()
	old := now.Add(-40 * 24 * time.Hour)
	recent := now.Add(-5 * 24 * time.Hour)

	s := NewState()
	s.Sessions["old-done"] = &Session{
		IssueNumber: 1,
		Status:      StatusDone,
		StartedAt:   old.Add(-time.Hour),
		FinishedAt:  &old,
	}
	s.Sessions["recent-done"] = &Session{
		IssueNumber: 2,
		Status:      StatusDone,
		StartedAt:   recent.Add(-time.Hour),
		FinishedAt:  &recent,
	}
	s.Sessions["running"] = &Session{
		IssueNumber: 3,
		Status:      StatusRunning,
		StartedAt:   old, // old but running — should NOT be pruned
	}

	maxAge := 30 * 24 * time.Hour
	pruned := s.PruneOldSessions(maxAge)

	if pruned != 1 {
		t.Errorf("expected 1 pruned, got %d", pruned)
	}
	if _, ok := s.Sessions["old-done"]; ok {
		t.Error("old-done should have been pruned")
	}
	if _, ok := s.Sessions["recent-done"]; !ok {
		t.Error("recent-done should still exist")
	}
	if _, ok := s.Sessions["running"]; !ok {
		t.Error("running should still exist (not terminal)")
	}
}

func TestPruneOldSessions_NoFinishedAt(t *testing.T) {
	// Edge case: terminal session without FinishedAt falls back to StartedAt
	old := time.Now().UTC().Add(-40 * 24 * time.Hour)

	s := NewState()
	s.Sessions["dead-no-finish"] = &Session{
		IssueNumber: 1,
		Status:      StatusDead,
		StartedAt:   old,
		FinishedAt:  nil, // no FinishedAt
	}

	pruned := s.PruneOldSessions(30 * 24 * time.Hour)
	if pruned != 1 {
		t.Errorf("expected 1 pruned, got %d", pruned)
	}
}
