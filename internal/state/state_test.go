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

func TestRebaseAttempted_Persistence(t *testing.T) {
	dir := t.TempDir()

	s := NewState()
	s.Sessions["slot-1"] = &Session{
		IssueNumber:     42,
		Branch:          "feat/test",
		Status:          StatusConflictFailed,
		StartedAt:       time.Now().UTC(),
		RebaseAttempted: true,
	}
	s.Sessions["slot-2"] = &Session{
		IssueNumber:     43,
		Branch:          "feat/other",
		Status:          StatusPROpen,
		StartedAt:       time.Now().UTC(),
		RebaseAttempted: false,
	}

	if err := Save(dir, s); err != nil {
		t.Fatalf("save: %v", err)
	}

	loaded, err := Load(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if !loaded.Sessions["slot-1"].RebaseAttempted {
		t.Error("slot-1: RebaseAttempted should be true after load")
	}
	if loaded.Sessions["slot-2"].RebaseAttempted {
		t.Error("slot-2: RebaseAttempted should be false after load")
	}
}

func TestRebaseAttempted_OmittedWhenFalse(t *testing.T) {
	dir := t.TempDir()

	s := NewState()
	s.Sessions["slot-1"] = &Session{
		IssueNumber: 42,
		Branch:      "feat/test",
		Status:      StatusPROpen,
		StartedAt:   time.Now().UTC(),
	}

	if err := Save(dir, s); err != nil {
		t.Fatalf("save: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatalf("read file: %v", err)
	}
	if containsString(string(data), "rebase_attempted") {
		t.Error("rebase_attempted should be omitted from JSON when false")
	}
}

func TestRebaseAttempted_BackwardCompatibility(t *testing.T) {
	dir := t.TempDir()

	oldJSON := `{
  "sessions": {
    "slot-1": {
      "issue_number": 42,
      "branch": "feat/test",
      "status": "conflict_failed",
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

	if loaded.Sessions["slot-1"].RebaseAttempted {
		t.Error("RebaseAttempted should default to false for old state files")
	}
}

func TestIssueInProgress_QueuedCountsAsInProgress(t *testing.T) {
	s := NewState()
	s.Sessions["slot-1"] = &Session{IssueNumber: 100, Status: StatusQueued}

	if !s.IssueInProgress(100) {
		t.Error("IssueInProgress should return true for queued session")
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
		{StatusRetryExhausted, true},
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

// --- retry exhaustion tests ---

func TestFailedAttemptsForIssue(t *testing.T) {
	now := time.Now().UTC()
	s := NewState()
	s.Sessions["slot-1"] = &Session{IssueNumber: 42, Status: StatusDead, PRNumber: 0}
	s.Sessions["slot-2"] = &Session{IssueNumber: 42, Status: StatusFailed, PRNumber: 0}
	s.Sessions["slot-3"] = &Session{IssueNumber: 42, Status: StatusDone, PRNumber: 10}                   // success — not counted
	s.Sessions["slot-4"] = &Session{IssueNumber: 42, Status: StatusDead, PRNumber: 5}                    // has PR — not counted
	s.Sessions["slot-5"] = &Session{IssueNumber: 42, Status: StatusRetryExhausted, PRNumber: 0}          // counted
	s.Sessions["slot-6"] = &Session{IssueNumber: 99, Status: StatusDead, PRNumber: 0}                    // different issue
	s.Sessions["slot-7"] = &Session{IssueNumber: 42, Status: StatusRunning, PRNumber: 0, StartedAt: now} // running — not counted
	s.Sessions["slot-8"] = &Session{IssueNumber: 42, Status: StatusConflictFailed, PRNumber: 0}          // conflict — not counted

	if got := s.FailedAttemptsForIssue(42); got != 3 {
		t.Errorf("FailedAttemptsForIssue(42) = %d, want 3", got)
	}
	if got := s.FailedAttemptsForIssue(99); got != 1 {
		t.Errorf("FailedAttemptsForIssue(99) = %d, want 1", got)
	}
	if got := s.FailedAttemptsForIssue(100); got != 0 {
		t.Errorf("FailedAttemptsForIssue(100) = %d, want 0", got)
	}
}

func TestIssueRetryExhausted(t *testing.T) {
	s := NewState()
	s.Sessions["slot-1"] = &Session{IssueNumber: 42, Status: StatusDead}
	s.Sessions["slot-2"] = &Session{IssueNumber: 42, Status: StatusRetryExhausted}
	s.Sessions["slot-3"] = &Session{IssueNumber: 99, Status: StatusFailed}

	if !s.IssueRetryExhausted(42) {
		t.Error("IssueRetryExhausted(42) should be true")
	}
	if s.IssueRetryExhausted(99) {
		t.Error("IssueRetryExhausted(99) should be false")
	}
}

func TestMarkIssueRetryExhausted(t *testing.T) {
	now := time.Now().UTC()
	old := now.Add(-1 * time.Hour)

	s := NewState()
	s.Sessions["slot-1"] = &Session{IssueNumber: 42, Status: StatusDead, FinishedAt: &old}
	s.Sessions["slot-2"] = &Session{IssueNumber: 42, Status: StatusFailed, FinishedAt: &now} // most recent
	s.Sessions["slot-3"] = &Session{IssueNumber: 42, Status: StatusDone, PRNumber: 10}       // not eligible

	s.MarkIssueRetryExhausted(42)

	// The most recent dead/failed session (slot-2) should be marked
	if s.Sessions["slot-2"].Status != StatusRetryExhausted {
		t.Errorf("slot-2 status = %q, want %q", s.Sessions["slot-2"].Status, StatusRetryExhausted)
	}
	// slot-1 should remain dead
	if s.Sessions["slot-1"].Status != StatusDead {
		t.Errorf("slot-1 status = %q, want %q", s.Sessions["slot-1"].Status, StatusDead)
	}
}

func TestMarkIssueRetryExhausted_NoSessions(t *testing.T) {
	s := NewState()
	// Should not panic when no matching sessions exist
	s.MarkIssueRetryExhausted(42)
}

func TestCountByStatus(t *testing.T) {
	s := NewState()
	s.Sessions["slot-1"] = &Session{IssueNumber: 1, Status: StatusRunning}
	s.Sessions["slot-2"] = &Session{IssueNumber: 2, Status: StatusRunning}
	s.Sessions["slot-3"] = &Session{IssueNumber: 3, Status: StatusPROpen}
	s.Sessions["slot-4"] = &Session{IssueNumber: 4, Status: StatusQueued}
	s.Sessions["slot-5"] = &Session{IssueNumber: 5, Status: StatusDone}   // terminal — excluded
	s.Sessions["slot-6"] = &Session{IssueNumber: 6, Status: StatusFailed} // terminal — excluded

	counts := s.CountByStatus()

	if counts[StatusRunning] != 2 {
		t.Errorf("running = %d, want 2", counts[StatusRunning])
	}
	if counts[StatusPROpen] != 1 {
		t.Errorf("pr_open = %d, want 1", counts[StatusPROpen])
	}
	if counts[StatusQueued] != 1 {
		t.Errorf("queued = %d, want 1", counts[StatusQueued])
	}
	if counts[StatusDone] != 0 {
		t.Errorf("done = %d, want 0 (terminal states excluded)", counts[StatusDone])
	}
	if counts[StatusFailed] != 0 {
		t.Errorf("failed = %d, want 0 (terminal states excluded)", counts[StatusFailed])
	}
}

func TestCountByStatus_Empty(t *testing.T) {
	s := NewState()
	counts := s.CountByStatus()
	if len(counts) != 0 {
		t.Errorf("expected empty map for empty state, got %v", counts)
	}
}

func TestIsMissionParent(t *testing.T) {
	s := NewState()
	s.Missions[100] = &Mission{
		ParentIssue: 100,
		ChildIssues: []int{101, 102},
		Status:      MissionStatusActive,
	}

	if !s.IsMissionParent(100) {
		t.Error("expected issue 100 to be a mission parent")
	}
	if s.IsMissionParent(101) {
		t.Error("expected issue 101 to not be a mission parent")
	}
}

func TestIsMissionChild(t *testing.T) {
	s := NewState()
	s.Missions[100] = &Mission{
		ParentIssue: 100,
		ChildIssues: []int{101, 102, 103},
		Status:      MissionStatusActive,
	}

	if parent := s.IsMissionChild(101); parent != 100 {
		t.Errorf("expected issue 101 parent to be 100, got %d", parent)
	}
	if parent := s.IsMissionChild(102); parent != 100 {
		t.Errorf("expected issue 102 parent to be 100, got %d", parent)
	}
	if parent := s.IsMissionChild(999); parent != 0 {
		t.Errorf("expected issue 999 to not be a mission child, got parent %d", parent)
	}
}

func TestMissionPersistence(t *testing.T) {
	dir := t.TempDir()

	s := NewState()
	now := time.Now().UTC()
	s.Missions[50] = &Mission{
		ParentIssue: 50,
		ParentTitle: "Epic: build feature X",
		ChildIssues: []int{51, 52, 53},
		Status:      MissionStatusActive,
		CreatedAt:   now,
	}

	if err := Save(dir, s); err != nil {
		t.Fatalf("Save: %v", err)
	}

	loaded, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	m, ok := loaded.Missions[50]
	if !ok {
		t.Fatal("mission 50 not found after load")
	}
	if m.ParentIssue != 50 {
		t.Errorf("ParentIssue = %d, want 50", m.ParentIssue)
	}
	if m.ParentTitle != "Epic: build feature X" {
		t.Errorf("ParentTitle = %q, want %q", m.ParentTitle, "Epic: build feature X")
	}
	if len(m.ChildIssues) != 3 {
		t.Errorf("ChildIssues len = %d, want 3", len(m.ChildIssues))
	}
	if m.Status != MissionStatusActive {
		t.Errorf("Status = %q, want %q", m.Status, MissionStatusActive)
	}
}
