package state

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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

func TestDonePRCount(t *testing.T) {
	s := NewState()
	s.Sessions["merged-1"] = &Session{IssueNumber: 1, Status: StatusDone, PRNumber: 10}
	s.Sessions["merged-2"] = &Session{IssueNumber: 2, Status: StatusDone, PRNumber: 11}
	s.Sessions["closed-issue"] = &Session{IssueNumber: 3, Status: StatusDone}
	s.Sessions["open-pr"] = &Session{IssueNumber: 4, Status: StatusPROpen, PRNumber: 12}

	if got := s.DonePRCount(); got != 2 {
		t.Fatalf("DonePRCount = %d, want 2", got)
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

func TestPreviousAttemptFeedback_Persistence(t *testing.T) {
	dir := t.TempDir()

	s := NewState()
	s.Sessions["slot-1"] = &Session{
		IssueNumber:             42,
		Branch:                  "feat/test",
		Status:                  StatusDead,
		StartedAt:               time.Now().UTC(),
		PreviousAttemptFeedback: "Confidence 3/5\nP2: null dereference",
	}
	s.Sessions["slot-2"] = &Session{
		IssueNumber: 43,
		Branch:      "feat/other",
		Status:      StatusRunning,
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
	if sess1.PreviousAttemptFeedback != "Confidence 3/5\nP2: null dereference" {
		t.Errorf("PreviousAttemptFeedback = %q, want Greptile feedback", sess1.PreviousAttemptFeedback)
	}

	sess2 := loaded.Sessions["slot-2"]
	if sess2 == nil {
		t.Fatal("slot-2 not found after load")
	}
	if sess2.PreviousAttemptFeedback != "" {
		t.Errorf("PreviousAttemptFeedback should be empty, got %q", sess2.PreviousAttemptFeedback)
	}
}

func TestPreviousAttemptFeedback_OmittedWhenEmpty(t *testing.T) {
	dir := t.TempDir()

	s := NewState()
	s.Sessions["slot-1"] = &Session{
		IssueNumber: 42,
		Branch:      "feat/test",
		Status:      StatusRunning,
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
	if containsString(json, "previous_attempt_feedback") {
		t.Error("previous_attempt_feedback should be omitted from JSON when empty")
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

func TestSessionAttentionFor_RetryExhaustedPRWithFailedChecks(t *testing.T) {
	sess := &Session{
		Status:          StatusRetryExhausted,
		PRNumber:        12,
		CIFailureOutput: "unit tests failed",
	}

	attention := SessionAttentionFor(sess, nil)
	if !attention.NeedsAttention {
		t.Fatal("retry-exhausted PR with failed checks should need attention")
	}
	if !containsString(attention.Reason, "checks failed") {
		t.Fatalf("reason = %q, want failed checks", attention.Reason)
	}
	if !containsString(attention.Reason, "PR #12 remains open") {
		t.Fatalf("reason = %q, want open PR", attention.Reason)
	}
	if !containsString(attention.NextAction, "Fix failing checks") {
		t.Fatalf("next action = %q, want fix checks", attention.NextAction)
	}
}

func TestSessionAttentionFor_StaleRunningWorker(t *testing.T) {
	alive := false
	sess := &Session{Status: StatusRunning, PID: 999999}

	attention := SessionAttentionFor(sess, &alive)
	if !attention.NeedsAttention {
		t.Fatal("running worker with alive=false should need attention")
	}
	if !containsString(attention.Reason, "PID is not alive") {
		t.Fatalf("reason = %q, want dead PID explanation", attention.Reason)
	}
	if !containsString(attention.NextAction, "reconciliation cycle") {
		t.Fatalf("next action = %q, want reconciliation guidance", attention.NextAction)
	}
}

func TestSessionAttentionFor_RunningWorkerAliveDoesNotNeedAttention(t *testing.T) {
	alive := true
	sess := &Session{Status: StatusRunning, PID: 1234}

	attention := SessionAttentionFor(sess, &alive)
	if attention.NeedsAttention {
		t.Fatal("running worker with alive=true should not need attention")
	}
	if attention.NextAction != "" {
		t.Fatalf("next action = %q, want empty for healthy running worker", attention.NextAction)
	}
}

func TestSessionDisplayStatusFor_ReviewFeedbackRetryLifecycle(t *testing.T) {
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	future := now.Add(5 * time.Minute)
	past := now.Add(-time.Minute)
	alive := true

	tests := []struct {
		name string
		sess *Session
		want string
	}{
		{
			name: "backoff",
			sess: &Session{
				Status:                      StatusDead,
				NextRetryAt:                 &future,
				PreviousAttemptFeedbackKind: RetryReasonReviewFeedback,
				RetryReason:                 RetryReasonReviewFeedback,
			},
			want: string(DisplayReviewRetryBackoff),
		},
		{
			name: "pending retry worker",
			sess: &Session{
				Status:                      StatusDead,
				NextRetryAt:                 &past,
				PreviousAttemptFeedbackKind: RetryReasonReviewFeedback,
				RetryReason:                 RetryReasonReviewFeedback,
			},
			want: string(DisplayReviewRetryPending),
		},
		{
			name: "running retry worker",
			sess: &Session{
				Status:      StatusRunning,
				PID:         1234,
				RetryReason: RetryReasonReviewFeedback,
			},
			want: string(DisplayReviewRetryRunning),
		},
		{
			name: "pending recheck",
			sess: &Session{
				Status:      StatusPROpen,
				PRNumber:    12,
				RetryReason: RetryReasonReviewFeedback,
			},
			want: string(DisplayReviewRetryRecheck),
		},
		{
			name: "genuine dead remains dead",
			sess: &Session{Status: StatusDead},
			want: string(StatusDead),
		},
		{
			name: "ci retry carrying review feedback remains dead",
			sess: &Session{
				Status:                      StatusDead,
				NextRetryAt:                 &future,
				PreviousAttemptFeedbackKind: RetryReasonReviewFeedback,
			},
			want: string(StatusDead),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := SessionDisplayStatusForAt(tt.sess, &alive, now)
			if got != tt.want {
				t.Fatalf("display status = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestSessionDisplayStatusFor_StaleReviewRetryWorkerStaysRunning(t *testing.T) {
	alive := false
	sess := &Session{
		Status:      StatusRunning,
		PID:         999999,
		RetryReason: RetryReasonReviewFeedback,
	}

	got := SessionDisplayStatusForAt(sess, &alive, time.Now().UTC())
	if got != string(StatusRunning) {
		t.Fatalf("display status = %q, want raw running for stale worker", got)
	}
	attention := SessionAttentionForAt(sess, &alive, time.Now().UTC())
	if !attention.NeedsAttention || !containsString(attention.Reason, "PID is not alive") {
		t.Fatalf("attention = %+v, want stale PID attention", attention)
	}
}

func TestSessionAttentionFor_ReviewFeedbackRetryCopy(t *testing.T) {
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	future := now.Add(5 * time.Minute)

	tests := []struct {
		name       string
		sess       *Session
		wantReason string
		wantAction string
	}{
		{
			name: "backoff",
			sess: &Session{
				Status:                      StatusDead,
				NextRetryAt:                 &future,
				PreviousAttemptFeedbackKind: RetryReasonReviewFeedback,
				RetryReason:                 RetryReasonReviewFeedback,
			},
			wantReason: "waiting for the retry backoff",
			wantAction: "scheduled retry worker",
		},
		{
			name: "pending recheck",
			sess: &Session{
				Status:      StatusPROpen,
				PRNumber:    12,
				RetryReason: RetryReasonReviewFeedback,
			},
			wantReason: "waiting for CI, Greptile, or the merge gate",
			wantAction: "merge gate allows it",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			attention := SessionAttentionForAt(tt.sess, nil, now)
			if attention.NeedsAttention {
				t.Fatalf("review retry lifecycle should not need attention: %+v", attention)
			}
			if !containsString(attention.Reason, tt.wantReason) {
				t.Fatalf("reason = %q, want %q", attention.Reason, tt.wantReason)
			}
			if !containsString(attention.NextAction, tt.wantAction) {
				t.Fatalf("next action = %q, want %q", attention.NextAction, tt.wantAction)
			}
		})
	}
}

func TestSessionAttentionFor_DoneReviewFeedbackIsHistorical(t *testing.T) {
	sess := &Session{
		IssueNumber:                 359,
		Status:                      StatusDone,
		PRNumber:                    375,
		PreviousAttemptFeedbackKind: RetryReasonReviewFeedback,
		RetryReason:                 RetryReasonReviewFeedback,
	}

	attention := SessionAttentionFor(sess, nil)
	if attention.NeedsAttention {
		t.Fatalf("done session with stale review feedback should not need attention: %+v", attention)
	}
	if !containsString(attention.Reason, "Issue is complete") {
		t.Fatalf("reason = %q, want completed historical status", attention.Reason)
	}
	if got := SessionDisplayStatusFor(sess, nil); got != string(StatusDone) {
		t.Fatalf("display status = %q, want done", got)
	}
}

func TestSessionLiveAt(t *testing.T) {
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	future := now.Add(10 * time.Minute)
	recent := now.Add(-LiveSessionRecentWindow + time.Minute)
	old := now.Add(-LiveSessionRecentWindow - time.Minute)

	tests := []struct {
		name string
		sess *Session
		want bool
	}{
		{
			name: "running",
			sess: &Session{Status: StatusRunning, StartedAt: old},
			want: true,
		},
		{
			name: "open PR",
			sess: &Session{Status: StatusPROpen, StartedAt: old, PRNumber: 10},
			want: true,
		},
		{
			name: "queued",
			sess: &Session{Status: StatusQueued, StartedAt: old},
			want: true,
		},
		{
			name: "review retry backoff",
			sess: &Session{
				Status:                      StatusDead,
				StartedAt:                   old,
				NextRetryAt:                 &future,
				PreviousAttemptFeedbackKind: RetryReasonReviewFeedback,
				RetryReason:                 RetryReasonReviewFeedback,
			},
			want: true,
		},
		{
			name: "retry needs attention",
			sess: &Session{Status: StatusDead, StartedAt: old, NextRetryAt: &future},
			want: true,
		},
		{
			name: "recently finished done",
			sess: &Session{Status: StatusDone, StartedAt: old, FinishedAt: &recent},
			want: true,
		},
		{
			name: "recent output on old done",
			sess: &Session{Status: StatusDone, StartedAt: old, FinishedAt: &old, LastOutputChangedAt: recent},
			want: true,
		},
		{
			name: "old done",
			sess: &Session{Status: StatusDone, StartedAt: old, FinishedAt: &old},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := SessionLiveAt(tt.sess, now); got != tt.want {
				t.Fatalf("SessionLiveAt = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestLiveSessionsAt(t *testing.T) {
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	old := now.Add(-LiveSessionRecentWindow - time.Hour)
	recent := now.Add(-time.Hour)
	s := NewState()
	s.Sessions["running"] = &Session{Status: StatusRunning, StartedAt: old}
	s.Sessions["recent-done"] = &Session{Status: StatusDone, StartedAt: old, FinishedAt: &recent}
	s.Sessions["old-done"] = &Session{Status: StatusDone, StartedAt: old, FinishedAt: &old}

	live := s.LiveSessionsAt(now)
	if len(live) != 2 {
		t.Fatalf("LiveSessionsAt len = %d, want 2", len(live))
	}
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

func TestStatusPriority(t *testing.T) {
	// running should come first
	if StatusPriority(StatusRunning) >= StatusPriority(StatusPROpen) {
		t.Error("running should have lower priority value than pr_open")
	}
	// pr_open before queued
	if StatusPriority(StatusPROpen) >= StatusPriority(StatusQueued) {
		t.Error("pr_open should have lower priority value than queued")
	}
	// queued before terminal states
	for _, terminal := range []SessionStatus{
		StatusDead, StatusFailed, StatusConflictFailed,
		StatusRetryExhausted, StatusDone,
	} {
		if StatusPriority(StatusQueued) >= StatusPriority(terminal) {
			t.Errorf("queued should have lower priority value than %q", terminal)
		}
	}
}

func TestCountByStatus_Empty(t *testing.T) {
	s := NewState()
	counts := s.CountByStatus()
	if len(counts) != 0 {
		t.Errorf("expected empty map for empty state, got %v", counts)
	}
}

func TestSupervisorDecisionPersistence(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()
	s := NewState()
	s.RecordSupervisorDecision(SupervisorDecision{
		ID:                "sup-test",
		CreatedAt:         now,
		Project:           "owner/repo",
		Mode:              "read_only",
		Status:            "succeeded",
		Summary:           "Start a worker for issue #42.",
		RecommendedAction: "spawn_worker",
		Target:            &SupervisorTarget{Issue: 42},
		Risk:              "mutating",
		Confidence:        0.84,
		Mutations: []SupervisorMutation{{
			Type:   "add_ready_label",
			Issue:  42,
			Label:  "maestro-ready",
			Status: "succeeded",
		}},
		Reasons: []string{"Issue #42 is eligible"},
		StuckStates: []SupervisorStuckState{
			{
				Code:              "no_eligible_issues",
				Severity:          "warning",
				Summary:           "No open issues match the configured ready labels.",
				Evidence:          []string{"Configured issue_labels: maestro-ready"},
				RecommendedAction: "Add one of the configured ready labels to an issue.",
				SupervisorCanAct:  true,
				Target:            &SupervisorTarget{Issue: 42},
			},
		},
		ProjectState: SupervisorProjectState{
			Sessions:       0,
			OpenIssues:     1,
			AvailableSlots: 1,
		},
		QueueAnalysis: &SupervisorQueueAnalysis{
			OpenIssues:         1,
			EligibleCandidates: 1,
			SelectedCandidate:  &SupervisorIssueCandidate{Number: 42, Title: "Start worker"},
		},
	}, DefaultSupervisorDecisionLimit)

	if err := Save(dir, s); err != nil {
		t.Fatalf("Save: %v", err)
	}
	loaded, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	latest := loaded.LatestSupervisorDecision()
	if latest == nil {
		t.Fatal("latest supervisor decision missing")
	}
	if latest.ID != "sup-test" {
		t.Fatalf("ID = %q, want sup-test", latest.ID)
	}
	if latest.Target == nil || latest.Target.Issue != 42 {
		t.Fatalf("target = %#v, want issue 42", latest.Target)
	}
	if latest.ProjectState.OpenIssues != 1 {
		t.Fatalf("open issues = %d, want 1", latest.ProjectState.OpenIssues)
	}
	if latest.Status != "succeeded" || len(latest.Mutations) != 1 || latest.Mutations[0].Label != "maestro-ready" {
		t.Fatalf("latest audit fields = %#v, want persisted status and mutation", latest)
	}
	if len(latest.StuckStates) != 1 {
		t.Fatalf("stuck states = %d, want 1", len(latest.StuckStates))
	}
	if latest.StuckStates[0].Code != "no_eligible_issues" {
		t.Fatalf("stuck state code = %q, want no_eligible_issues", latest.StuckStates[0].Code)
	}
	if latest.QueueAnalysis == nil || latest.QueueAnalysis.SelectedCandidate == nil || latest.QueueAnalysis.SelectedCandidate.Number != 42 {
		t.Fatalf("queue analysis = %#v, want selected issue 42", latest.QueueAnalysis)
	}
}

func TestSupervisorQueueAnalysisIdleReasonExplainsAllExcluded(t *testing.T) {
	analysis := &SupervisorQueueAnalysis{
		OpenIssues:         11,
		EligibleCandidates: 0,
		ExcludedIssues:     11,
		SkippedReasons: []string{
			"Issue #24 skipped by dynamic wave policy: excluded by label \"blocked\"",
		},
	}

	if got, want := analysis.IdleReason(), "Policy excluded all 11 open issues."; got != want {
		t.Fatalf("IdleReason = %q, want %q", got, want)
	}
	if got, want := analysis.TopSkippedReason(), "Issue #24 skipped by dynamic wave policy: excluded by label \"blocked\""; got != want {
		t.Fatalf("TopSkippedReason = %q, want %q", got, want)
	}

	analysis.EligibleCandidates = 1
	if got := analysis.IdleReason(); got != "" {
		t.Fatalf("IdleReason with eligible candidate = %q, want empty", got)
	}
}

func TestSupervisorQueueAnalysisIdleReasonExplainsSkipCategories(t *testing.T) {
	analysis := &SupervisorQueueAnalysis{
		OpenIssues:                    4,
		EligibleCandidates:            0,
		ExcludedIssues:                1,
		HeldIssues:                    1,
		BlockedByDependencyIssues:     1,
		NonRunnableProjectStatusCount: 1,
	}

	want := "Queue policy classified all 4 open issues: excluded=1, held/meta=1, blocked-by-dependency=1, non-runnable project status=1."
	if got := analysis.IdleReason(); got != want {
		t.Fatalf("IdleReason = %q, want %q", got, want)
	}

	analysis = &SupervisorQueueAnalysis{OpenIssues: 2, BlockedByDependencyIssues: 2}
	if got, want := analysis.IdleReason(), "Open dependencies blocked all 2 open issues."; got != want {
		t.Fatalf("IdleReason = %q, want %q", got, want)
	}
}

func TestRecordSupervisorDecisionPrunesOldRecords(t *testing.T) {
	s := NewState()
	now := time.Now().UTC()
	for i := 0; i < 5; i++ {
		s.RecordSupervisorDecision(SupervisorDecision{
			ID:        fmt.Sprintf("sup-%d", i),
			CreatedAt: now.Add(time.Duration(i) * time.Minute),
		}, 3)
	}

	if len(s.SupervisorDecisions) != 3 {
		t.Fatalf("decisions = %d, want 3", len(s.SupervisorDecisions))
	}
	if s.SupervisorDecisions[0].ID != "sup-2" {
		t.Fatalf("first retained ID = %q, want sup-2", s.SupervisorDecisions[0].ID)
	}
	latest := s.LatestSupervisorDecision()
	if latest == nil || latest.ID != "sup-4" {
		t.Fatalf("latest = %#v, want sup-4", latest)
	}
}

func TestSaveMergesIndependentConcurrentUpdates(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 5, 1, 13, 46, 2, 0, time.UTC)
	initial := NewState()
	if err := Save(dir, initial); err != nil {
		t.Fatalf("Save initial: %v", err)
	}

	runSnapshot, err := Load(dir)
	if err != nil {
		t.Fatalf("Load run snapshot: %v", err)
	}
	supervisorSnapshot, err := Load(dir)
	if err != nil {
		t.Fatalf("Load supervisor snapshot: %v", err)
	}

	decision := SupervisorDecision{
		ID:                "sup-20260501T134602.103131758Z",
		CreatedAt:         now,
		Project:           "BeFeast/maestro",
		Mode:              "read_only",
		Summary:           "Start a worker for issue #302: Prevent state lost-update.",
		RecommendedAction: "spawn_worker",
		Target:            &SupervisorTarget{Issue: 302},
		Risk:              "mutating",
		Confidence:        0.84,
		Reasons:           []string{"Issue #302 is eligible"},
	}
	approval := supervisorSnapshot.RecordPendingApprovalForDecision(decision, now)
	decision.ApprovalID = approval.ID
	supervisorSnapshot.RecordSupervisorDecision(decision, DefaultSupervisorDecisionLimit)
	if err := Save(dir, supervisorSnapshot); err != nil {
		t.Fatalf("Save supervisor snapshot: %v", err)
	}

	runSnapshot.Sessions["slot-1"] = &Session{
		IssueNumber: 17,
		IssueTitle:  "existing run-loop work",
		Status:      StatusRunning,
		StartedAt:   now,
		PID:         1234,
	}
	if err := Save(dir, runSnapshot); err != nil {
		t.Fatalf("Save stale run snapshot: %v", err)
	}

	loaded, err := Load(dir)
	if err != nil {
		t.Fatalf("Load merged state: %v", err)
	}
	if loaded.Sessions["slot-1"] == nil {
		t.Fatal("run-loop session missing after merge")
	}
	latest := loaded.LatestSupervisorDecision()
	if latest == nil || latest.ID != decision.ID || latest.Target == nil || latest.Target.Issue != 302 {
		t.Fatalf("latest decision = %#v, want supervisor decision for issue #302", latest)
	}
	loadedApproval, ok := loaded.FindApproval(approval.ID)
	if !ok {
		t.Fatalf("approval %q missing after stale run-loop save", approval.ID)
	}
	if loadedApproval.Status != ApprovalStatusPending {
		t.Fatalf("approval status = %q, want pending", loadedApproval.Status)
	}
	if _, err := loaded.ApproveApproval(approval.ID, now.Add(time.Minute), "test", "race preserved"); err != nil {
		t.Fatalf("ApproveApproval after merge: %v", err)
	}
}

func TestSaveReconcilesConcurrentSpawnApprovalWithStartedWorker(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 5, 1, 13, 46, 2, 0, time.UTC)
	initial := NewState()
	if err := Save(dir, initial); err != nil {
		t.Fatalf("Save initial: %v", err)
	}

	runSnapshot, err := Load(dir)
	if err != nil {
		t.Fatalf("Load run snapshot: %v", err)
	}
	supervisorSnapshot, err := Load(dir)
	if err != nil {
		t.Fatalf("Load supervisor snapshot: %v", err)
	}

	approval := supervisorSnapshot.RecordPendingApprovalForDecision(testApprovalDecision(now), now)
	if err := Save(dir, supervisorSnapshot); err != nil {
		t.Fatalf("Save supervisor snapshot: %v", err)
	}

	runSnapshot.Sessions["slot-1"] = &Session{
		IssueNumber: 42,
		IssueTitle:  "ready work",
		Status:      StatusRunning,
		StartedAt:   now.Add(time.Minute),
		PID:         1234,
	}
	if err := Save(dir, runSnapshot); err != nil {
		t.Fatalf("Save stale run snapshot: %v", err)
	}

	loaded, err := Load(dir)
	if err != nil {
		t.Fatalf("Load merged state: %v", err)
	}
	loadedApproval, ok := loaded.FindApproval(approval.ID)
	if !ok {
		t.Fatalf("approval %q missing after merge", approval.ID)
	}
	if loadedApproval.Status != ApprovalStatusSuperseded {
		t.Fatalf("approval status = %q, want %q", loadedApproval.Status, ApprovalStatusSuperseded)
	}
	last := loadedApproval.Audit[len(loadedApproval.Audit)-1]
	if last.Event != ApprovalAuditSuperseded || !strings.Contains(last.Reason, "worker slot-1 started for issue #42") {
		t.Fatalf("last audit = %#v, want superseded by started worker", last)
	}
	if _, err := loaded.ApproveApproval(approval.ID, now.Add(2*time.Minute), "test", "too late"); !errors.Is(err, ErrApprovalSuperseded) {
		t.Fatalf("ApproveApproval superseded err = %v, want %v", err, ErrApprovalSuperseded)
	}
}

func TestSaveRejectsConcurrentSameSessionConflict(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 5, 1, 13, 46, 2, 0, time.UTC)
	initial := NewState()
	initial.Sessions["slot-1"] = &Session{
		IssueNumber: 42,
		IssueTitle:  "same session",
		Status:      StatusRunning,
		StartedAt:   now,
		PID:         100,
	}
	if err := Save(dir, initial); err != nil {
		t.Fatalf("Save initial: %v", err)
	}

	first, err := Load(dir)
	if err != nil {
		t.Fatalf("Load first: %v", err)
	}
	second, err := Load(dir)
	if err != nil {
		t.Fatalf("Load second: %v", err)
	}

	first.Sessions["slot-1"].PID = 200
	if err := Save(dir, first); err != nil {
		t.Fatalf("Save first: %v", err)
	}
	second.Sessions["slot-1"].PID = 300
	if err := Save(dir, second); !errors.Is(err, ErrStateConflict) {
		t.Fatalf("Save second err = %v, want %v", err, ErrStateConflict)
	}

	loaded, err := Load(dir)
	if err != nil {
		t.Fatalf("Load after conflict: %v", err)
	}
	if got := loaded.Sessions["slot-1"].PID; got != 200 {
		t.Fatalf("PID = %d, want first writer value 200", got)
	}
}

func TestApprovalPendingPersistence(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 4, 29, 12, 0, 0, 0, time.UTC)
	s := NewState()
	approval := s.RecordPendingApprovalForDecision(testApprovalDecision(now), now)

	if approval.Status != ApprovalStatusPending {
		t.Fatalf("status = %q, want %q", approval.Status, ApprovalStatusPending)
	}
	if approval.Action != "spawn_worker" {
		t.Fatalf("action = %q, want spawn_worker", approval.Action)
	}
	if approval.Target == nil || approval.Target.Issue != 42 {
		t.Fatalf("target = %#v, want issue 42", approval.Target)
	}
	if approval.PayloadHash == "" {
		t.Fatal("payload hash missing")
	}
	if approval.TargetStateHash == "" {
		t.Fatal("target state hash missing")
	}
	if len(approval.Audit) != 1 || approval.Audit[0].Event != ApprovalAuditCreated {
		t.Fatalf("audit = %#v, want created event", approval.Audit)
	}

	if err := Save(dir, s); err != nil {
		t.Fatalf("Save: %v", err)
	}
	loaded, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	loadedApproval, ok := loaded.FindApproval(approval.ID)
	if !ok {
		t.Fatalf("approval %q missing after load", approval.ID)
	}
	if loadedApproval.PayloadHash != approval.PayloadHash {
		t.Fatalf("payload hash = %q, want %q", loadedApproval.PayloadHash, approval.PayloadHash)
	}
}

func TestReconcileSpawnWorkerApprovalsForStartedSession(t *testing.T) {
	now := time.Date(2026, 4, 29, 12, 0, 0, 0, time.UTC)
	s := NewState()
	matching := s.RecordPendingApprovalForDecision(testApprovalDecision(now), now)
	matchingID := matching.ID
	nonMatching := s.RecordPendingApprovalForDecision(SupervisorDecision{
		ID:                "sup-other-issue",
		CreatedAt:         now,
		Project:           "owner/repo",
		Summary:           "Start a worker for issue #43.",
		RecommendedAction: "spawn_worker",
		Target:            &SupervisorTarget{Issue: 43},
		Risk:              "mutating",
	}, now)
	nonMatchingID := nonMatching.ID
	nonSpawn := s.RecordPendingApprovalForDecision(SupervisorDecision{
		ID:                "sup-merge",
		CreatedAt:         now,
		Project:           "owner/repo",
		Summary:           "Merge PR #9.",
		RecommendedAction: "approve_merge",
		Target:            &SupervisorTarget{Issue: 42, PR: 9},
		Risk:              "mutating",
	}, now)
	nonSpawnID := nonSpawn.ID

	count := s.ReconcileSpawnWorkerApprovalsForStartedSession("slot-1", &Session{
		IssueNumber: 42,
		Status:      StatusRunning,
		StartedAt:   now.Add(time.Minute),
	}, now.Add(time.Minute))

	if count != 1 {
		t.Fatalf("reconciled approvals = %d, want 1", count)
	}
	matching, ok := s.FindApproval(matchingID)
	if !ok {
		t.Fatalf("matching approval %q missing", matchingID)
	}
	nonMatching, ok = s.FindApproval(nonMatchingID)
	if !ok {
		t.Fatalf("non-matching approval %q missing", nonMatchingID)
	}
	nonSpawn, ok = s.FindApproval(nonSpawnID)
	if !ok {
		t.Fatalf("non-spawn approval %q missing", nonSpawnID)
	}
	if matching.Status != ApprovalStatusSuperseded {
		t.Fatalf("matching status = %q, want %q", matching.Status, ApprovalStatusSuperseded)
	}
	if nonMatching.Status != ApprovalStatusPending {
		t.Fatalf("non-matching status = %q, want pending", nonMatching.Status)
	}
	if nonSpawn.Status != ApprovalStatusPending {
		t.Fatalf("non-spawn status = %q, want pending", nonSpawn.Status)
	}
	last := matching.Audit[len(matching.Audit)-1]
	if last.Event != ApprovalAuditSuperseded {
		t.Fatalf("last audit = %#v, want superseded", last)
	}
}

func TestApproveApprovalAuditsResolution(t *testing.T) {
	now := time.Date(2026, 4, 29, 12, 0, 0, 0, time.UTC)
	s := NewState()
	approval := s.RecordPendingApprovalForDecision(testApprovalDecision(now), now)

	approved, err := s.ApproveApproval(approval.DecisionID, now.Add(time.Minute), "cli", "checks green")
	if err != nil {
		t.Fatalf("ApproveApproval: %v", err)
	}
	if approved.Status != ApprovalStatusApproved {
		t.Fatalf("status = %q, want %q", approved.Status, ApprovalStatusApproved)
	}
	if len(approved.Audit) != 2 {
		t.Fatalf("audit entries = %d, want 2", len(approved.Audit))
	}
	last := approved.Audit[len(approved.Audit)-1]
	if last.Event != ApprovalAuditApproved || last.Actor != "cli" || last.Reason != "checks green" {
		t.Fatalf("last audit = %#v, want approved by cli", last)
	}
}

func TestRejectApprovalAuditsResolution(t *testing.T) {
	now := time.Date(2026, 4, 29, 12, 0, 0, 0, time.UTC)
	s := NewState()
	approval := s.RecordPendingApprovalForDecision(testApprovalDecision(now), now)

	rejected, err := s.RejectApproval(approval.ID, now.Add(time.Minute), "cli", "needs review")
	if err != nil {
		t.Fatalf("RejectApproval: %v", err)
	}
	if rejected.Status != ApprovalStatusRejected {
		t.Fatalf("status = %q, want %q", rejected.Status, ApprovalStatusRejected)
	}
	last := rejected.Audit[len(rejected.Audit)-1]
	if last.Event != ApprovalAuditRejected || last.Actor != "cli" || last.Reason != "needs review" {
		t.Fatalf("last audit = %#v, want rejected by cli", last)
	}
}

func TestApproveMissingApprovalFailsSafely(t *testing.T) {
	s := NewState()
	_, err := s.ApproveApproval("approval-missing", time.Now().UTC(), "cli", "")
	if !errors.Is(err, ErrApprovalNotFound) {
		t.Fatalf("ApproveApproval missing err = %v, want %v", err, ErrApprovalNotFound)
	}
}

func TestApproveStaleApprovalFailsSafely(t *testing.T) {
	now := time.Date(2026, 4, 29, 12, 0, 0, 0, time.UTC)
	s := NewState()
	s.Sessions["slot-1"] = &Session{IssueNumber: 77, Status: StatusRetryExhausted, PRNumber: 12}
	decision := SupervisorDecision{
		ID:                "sup-stale",
		CreatedAt:         now,
		Project:           "owner/repo",
		Mode:              "read_only",
		Summary:           "Review retry-exhausted issue #77.",
		RecommendedAction: "review_retry_exhausted",
		Target:            &SupervisorTarget{Issue: 77, PR: 12, Session: "slot-1"},
		Risk:              "approval_gated",
		Confidence:        0.93,
		Reasons:           []string{"retry budget exhausted"},
	}
	approval := s.RecordPendingApprovalForDecision(decision, now)
	s.Sessions["slot-1"].Status = StatusDone

	_, err := s.ApproveApproval(approval.ID, now.Add(time.Minute), "cli", "")
	if !errors.Is(err, ErrApprovalStale) {
		t.Fatalf("ApproveApproval stale err = %v, want %v", err, ErrApprovalStale)
	}
	if s.Approvals[0].Status != ApprovalStatusStale {
		t.Fatalf("status = %q, want %q", s.Approvals[0].Status, ApprovalStatusStale)
	}
	last := s.Approvals[0].Audit[len(s.Approvals[0].Audit)-1]
	if last.Event != ApprovalAuditStale {
		t.Fatalf("last audit = %#v, want stale event", last)
	}
}

func TestApproveChangedApprovalPayloadFailsSafely(t *testing.T) {
	now := time.Date(2026, 4, 29, 12, 0, 0, 0, time.UTC)
	s := NewState()
	approval := s.RecordPendingApprovalForDecision(testApprovalDecision(now), now)
	approval.Action = "merge_pr"

	_, err := s.ApproveApproval(approval.ID, now.Add(time.Minute), "cli", "")
	if !errors.Is(err, ErrApprovalPayloadMismatch) {
		t.Fatalf("ApproveApproval payload err = %v, want %v", err, ErrApprovalPayloadMismatch)
	}
	if s.Approvals[0].Status != ApprovalStatusStale {
		t.Fatalf("status = %q, want %q", s.Approvals[0].Status, ApprovalStatusStale)
	}
}

func testApprovalDecision(now time.Time) SupervisorDecision {
	return SupervisorDecision{
		ID:                "sup-approval",
		CreatedAt:         now,
		Project:           "owner/repo",
		Mode:              "read_only",
		Summary:           "Start a worker for issue #42: ready work",
		RecommendedAction: "spawn_worker",
		Target:            &SupervisorTarget{Issue: 42},
		Risk:              "mutating",
		Confidence:        0.84,
		Reasons:           []string{"Issue #42 is eligible", "Starting a worker mutates local worktrees"},
		ProjectState: SupervisorProjectState{
			OpenIssues:     1,
			AvailableSlots: 1,
		},
	}
}
