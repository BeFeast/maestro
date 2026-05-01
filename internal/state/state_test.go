package state

import (
	"errors"
	"fmt"
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
