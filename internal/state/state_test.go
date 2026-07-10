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

func TestBackendTaskTypePersistence(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC)

	s := NewState()
	s.Sessions["slot-1"] = &Session{
		IssueNumber: 42,
		Status:      StatusRunning,
		StartedAt:   now,
		Backend:     "claude",
		BackendSelection: &BackendSelection{
			SelectedBackend: "claude",
			SelectionReason: "auto",
			TaskType:        "vision",
		},
		Attribution: []BackendAttribution{
			{
				Backend:   "claude",
				TaskType:  "vision",
				StartedAt: now,
				Reason:    "initial_spawn",
			},
		},
	}

	if err := Save(dir, s); err != nil {
		t.Fatalf("save: %v", err)
	}
	loaded, err := Load(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	sess := loaded.Sessions["slot-1"]
	if sess == nil {
		t.Fatal("slot-1 not found after load")
	}
	if sess.BackendSelection == nil || sess.BackendSelection.TaskType != "vision" {
		t.Fatalf("BackendSelection = %+v, want task_type vision", sess.BackendSelection)
	}
	if len(sess.Attribution) != 1 || sess.Attribution[0].TaskType != "vision" {
		t.Fatalf("Attribution = %+v, want task_type vision", sess.Attribution)
	}
}

func TestDonePRCount(t *testing.T) {
	s := NewState()
	s.Sessions["merged-1"] = &Session{IssueNumber: 1, Status: StatusDone, PRNumber: 10}
	s.Sessions["merged-2"] = &Session{IssueNumber: 2, Status: StatusDone, PRNumber: 11}
	s.Sessions["code-landed"] = &Session{IssueNumber: 5, Status: StatusCodeLanded, PRNumber: 13}
	s.Sessions["closed-issue"] = &Session{IssueNumber: 3, Status: StatusDone}
	s.Sessions["open-pr"] = &Session{IssueNumber: 4, Status: StatusPROpen, PRNumber: 12}

	if got := s.DonePRCount(); got != 3 {
		t.Fatalf("DonePRCount = %d, want 3", got)
	}
}

func TestProjectStatusSynced(t *testing.T) {
	s := NewState()
	if s.ProjectStatusSynced(42, "in_progress") {
		t.Fatal("ProjectStatusSynced should be false before mark")
	}
	syncedAt := time.Now().UTC()
	s.MarkProjectStatusSynced(42, "in_progress", syncedAt)
	if !s.ProjectStatusSynced(42, "in_progress") {
		t.Fatal("ProjectStatusSynced should be true for recorded status")
	}
	if s.ProjectStatusSynced(42, "in_review") {
		t.Fatal("ProjectStatusSynced should be false for a different status")
	}
	if got := s.ProjectStatusSync[42].SyncedAt; !got.Equal(syncedAt) {
		t.Fatalf("SyncedAt = %s, want %s", got, syncedAt)
	}
}

func TestBackendHealthPersistence(t *testing.T) {
	dir := t.TempDir()
	since := time.Date(2026, 5, 22, 12, 0, 0, 0, time.UTC)
	retryAfter := since.Add(2 * time.Hour)

	s := NewState()
	s.BackendHealth["claude"] = BackendHealth{
		State:       BackendHealthCooldown,
		Reason:      BackendBlockProviderLimit,
		Pattern:     "limit for today",
		Since:       since,
		RetryAfter:  &retryAfter,
		LastSession: "scr-54",
	}

	if err := Save(dir, s); err != nil {
		t.Fatalf("save: %v", err)
	}
	loaded, err := Load(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	health := loaded.BackendHealth["claude"]
	if health.State != BackendHealthCooldown || health.Reason != BackendBlockProviderLimit {
		t.Fatalf("health = %+v, want provider-limit cooldown", health)
	}
	if health.RetryAfter == nil || !health.RetryAfter.Equal(retryAfter) {
		t.Fatalf("retry_after = %v, want %s", health.RetryAfter, retryAfter)
	}
}

// #600: a backend that opened a PR after the cooldown was recorded must
// be cleared by ReconcileBackendHealth — the dashboard should show it as
// healthy, not "auto-recovery pending".
func TestReconcileBackendHealth_ClearsAfterPRSuccess(t *testing.T) {
	s := NewState()
	since := time.Date(2026, 5, 30, 18, 30, 0, 0, time.UTC)
	s.BackendHealth["claude"] = BackendHealth{
		State:       BackendHealthCooldown,
		Reason:      BackendBlockProviderLimit,
		Since:       since,
		LastSession: "sup-83",
	}
	s.Sessions["sup-128"] = &Session{
		IssueNumber: 600,
		Backend:     "claude",
		Status:      StatusPROpen,
		StartedAt:   since.Add(48 * time.Hour),
		PRNumber:    599,
	}

	now := since.Add(72 * time.Hour)
	if !ReconcileBackendHealth(s, now) {
		t.Fatal("ReconcileBackendHealth should report a change when a PR-evidence session post-dates the cooldown")
	}
	if _, ok := s.BackendHealth["claude"]; ok {
		t.Fatalf("BackendHealth[claude] should be cleared, got %+v", s.BackendHealth["claude"])
	}
}

// #600: an elapsed RetryAfter must clear the cooldown — the selector
// already treats this backend as available, so the panel must too.
func TestReconcileBackendHealth_ClearsAfterElapsedRetryAfter(t *testing.T) {
	s := NewState()
	since := time.Date(2026, 5, 29, 23, 0, 0, 0, time.UTC)
	retryAfter := since.Add(2 * time.Hour)
	s.BackendHealth["codex"] = BackendHealth{
		State:      BackendHealthCooldown,
		Reason:     BackendBlockProviderLimit,
		Since:      since,
		RetryAfter: &retryAfter,
	}

	now := retryAfter.Add(48 * time.Hour)
	if !ReconcileBackendHealth(s, now) {
		t.Fatal("ReconcileBackendHealth should clear cooldown when RetryAfter has elapsed")
	}
	if _, ok := s.BackendHealth["codex"]; ok {
		t.Fatal("BackendHealth[codex] should be cleared after RetryAfter elapses")
	}
}

// #600: a cooldown with no RetryAfter must be capped by MaxBackendCooldownTTL
// so a transient provider limit cannot render as "auto-recovery pending"
// forever.
func TestReconcileBackendHealth_ClearsAfterMaxTTL(t *testing.T) {
	s := NewState()
	since := time.Now().UTC().Add(-MaxBackendCooldownTTL - time.Hour)
	s.BackendHealth["claude"] = BackendHealth{
		State:  BackendHealthCooldown,
		Reason: BackendBlockProviderLimit,
		Since:  since,
	}

	if !ReconcileBackendHealth(s, time.Now().UTC()) {
		t.Fatal("ReconcileBackendHealth should clear cooldown after max TTL elapses")
	}
	if _, ok := s.BackendHealth["claude"]; ok {
		t.Fatal("BackendHealth[claude] should be cleared after max-cooldown TTL")
	}
}

// #600: an active cooldown whose RetryAfter is still in the future must
// be preserved so the selector keeps blocking the backend.
func TestReconcileBackendHealth_KeepsActiveCooldown(t *testing.T) {
	s := NewState()
	now := time.Now().UTC()
	retryAfter := now.Add(30 * time.Minute)
	s.BackendHealth["claude"] = BackendHealth{
		State:      BackendHealthCooldown,
		Reason:     BackendBlockProviderLimit,
		Since:      now.Add(-5 * time.Minute),
		RetryAfter: &retryAfter,
	}

	if ReconcileBackendHealth(s, now) {
		t.Fatal("ReconcileBackendHealth should not clear an active cooldown")
	}
	if got, ok := s.BackendHealth["claude"]; !ok || got.State != BackendHealthCooldown {
		t.Fatalf("BackendHealth[claude] = %+v, want active cooldown", got)
	}
}

// #600: a PR-success that pre-dates the cooldown must not clear it —
// only later successes prove recovery.
func TestReconcileBackendHealth_IgnoresOlderSuccess(t *testing.T) {
	s := NewState()
	since := time.Date(2026, 5, 30, 18, 0, 0, 0, time.UTC)
	s.BackendHealth["claude"] = BackendHealth{
		State:  BackendHealthCooldown,
		Reason: BackendBlockProviderLimit,
		Since:  since,
	}
	s.Sessions["sup-50"] = &Session{
		Backend:   "claude",
		Status:    StatusPROpen,
		StartedAt: since.Add(-24 * time.Hour),
		PRNumber:  500,
	}

	now := since.Add(1 * time.Hour)
	if ReconcileBackendHealth(s, now) {
		t.Fatal("ReconcileBackendHealth should not clear when only older successes exist")
	}
}

// #600 review feedback: a session that landed in StatusDone via external
// issue closure (no PR produced) must NOT count as backend recovery —
// the backend may still be rate-limited and we have no evidence it
// produced useful output.
func TestReconcileBackendHealth_IgnoresExternallyClosedDone(t *testing.T) {
	s := NewState()
	since := time.Date(2026, 5, 30, 18, 0, 0, 0, time.UTC)
	s.BackendHealth["claude"] = BackendHealth{
		State:  BackendHealthCooldown,
		Reason: BackendBlockProviderLimit,
		Since:  since,
	}
	// Session started after the cooldown, but issue was closed externally
	// without the backend ever producing a PR.
	s.Sessions["sup-77"] = &Session{
		Backend:   "claude",
		Status:    StatusDone,
		StartedAt: since.Add(1 * time.Hour),
		PRNumber:  0,
	}

	now := since.Add(2 * time.Hour)
	if ReconcileBackendHealth(s, now) {
		t.Fatal("ReconcileBackendHealth must not clear cooldown for externally-closed sessions without PR evidence")
	}
	if _, ok := s.BackendHealth["claude"]; !ok {
		t.Fatal("BackendHealth[claude] should still be present")
	}
}

// #600 review feedback: even a session that reached StatusPROpen must
// have PRNumber > 0 to count as PR evidence; a zero PRNumber means the
// PR signal never landed and we should not trust it as proof.
func TestReconcileBackendHealth_RequiresNonZeroPRNumber(t *testing.T) {
	s := NewState()
	since := time.Date(2026, 5, 30, 18, 0, 0, 0, time.UTC)
	s.BackendHealth["claude"] = BackendHealth{
		State:  BackendHealthCooldown,
		Reason: BackendBlockProviderLimit,
		Since:  since,
	}
	s.Sessions["sup-99"] = &Session{
		Backend:   "claude",
		Status:    StatusPROpen,
		StartedAt: since.Add(1 * time.Hour),
		PRNumber:  0,
	}

	now := since.Add(2 * time.Hour)
	if ReconcileBackendHealth(s, now) {
		t.Fatal("ReconcileBackendHealth must not clear cooldown when PRNumber is zero")
	}
}

// #600: ReconcileBackendHealth is a no-op on a nil State or empty map.
func TestReconcileBackendHealth_HandlesNilAndEmpty(t *testing.T) {
	if ReconcileBackendHealth(nil, time.Now()) {
		t.Fatal("nil state should return false")
	}
	s := NewState()
	if ReconcileBackendHealth(s, time.Now()) {
		t.Fatal("empty BackendHealth map should return false")
	}
}

func TestBackendHealthMergeKeepsLatest(t *testing.T) {
	base := NewState()
	current := NewState()
	ours := NewState()
	current.BackendHealth["claude"] = BackendHealth{State: BackendHealthCooldown, Reason: BackendBlockProviderLimit, Since: time.Date(2026, 5, 22, 10, 0, 0, 0, time.UTC)}
	ours.BackendHealth["claude"] = BackendHealth{State: BackendHealthAvailable, Reason: "manual_clear", Since: time.Date(2026, 5, 22, 11, 0, 0, 0, time.UTC)}

	merged, err := mergeStateSnapshots(base, current, ours)
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	if got := merged.BackendHealth["claude"]; got.State != BackendHealthAvailable || got.Reason != "manual_clear" {
		t.Fatalf("merged claude health = %+v, want latest ours value", got)
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

func TestIssueInProgress_CodeLandedCountsAsInProgress(t *testing.T) {
	s := NewState()
	s.Sessions["slot-1"] = &Session{IssueNumber: 101, Status: StatusCodeLanded, PRNumber: 12}

	if !s.IssueInProgress(101) {
		t.Error("IssueInProgress should return true for code_landed session")
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
		{StatusCodeLanded, false},
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
	s.Sessions["old-code-landed"] = &Session{
		IssueNumber: 4,
		Status:      StatusCodeLanded,
		StartedAt:   old.Add(-time.Hour),
		FinishedAt:  &old,
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
	if _, ok := s.Sessions["old-code-landed"]; !ok {
		t.Error("old-code-landed should still exist (not terminal)")
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
	s.Sessions["slot-3"] = &Session{IssueNumber: 42, Status: StatusDone, PRNumber: 10}                    // success — not counted
	s.Sessions["slot-4"] = &Session{IssueNumber: 42, Status: StatusDead, PRNumber: 5}                     // has PR — not counted
	s.Sessions["slot-5"] = &Session{IssueNumber: 42, Status: StatusRetryExhausted, PRNumber: 0}           // counted
	s.Sessions["slot-6"] = &Session{IssueNumber: 99, Status: StatusDead, PRNumber: 0}                     // different issue
	s.Sessions["slot-7"] = &Session{IssueNumber: 42, Status: StatusRunning, PRNumber: 0, StartedAt: now}  // running — not counted
	s.Sessions["slot-8"] = &Session{IssueNumber: 42, Status: StatusConflictFailed, PRNumber: 0}           // conflict — not counted
	s.Sessions["slot-9"] = &Session{IssueNumber: 42, Status: StatusDead, PRNumber: 0, RateLimitHit: true} // rate-limited — not counted (#466)

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

func TestSessionDisplayStatusFor_BackendRateLimited(t *testing.T) {
	now := time.Date(2026, 5, 30, 12, 0, 0, 0, time.UTC)
	reset := time.Date(2026, 5, 30, 20, 13, 0, 0, time.UTC)
	future := now.Add(5 * time.Minute)

	tests := []struct {
		name string
		sess *Session
		want string
	}{
		{
			name: "dead with provider limit and no retry scheduled",
			sess: &Session{
				Status:               StatusDead,
				RateLimitHit:         true,
				ProviderLimitBackend: "codex",
				ProviderLimitResetAt: &reset,
			},
			want: string(DisplayBackendRateLimited),
		},
		{
			name: "retry_exhausted purely from provider limit",
			sess: &Session{
				Status:               StatusRetryExhausted,
				RateLimitHit:         true,
				ProviderLimitBackend: "codex",
			},
			want: string(DisplayBackendRateLimited),
		},
		{
			name: "scheduled retry takes precedence over provider limit",
			sess: &Session{
				Status:               StatusDead,
				RateLimitHit:         true,
				ProviderLimitBackend: "codex",
				NextRetryAt:          &future,
			},
			want: string(StatusDead),
		},
		{
			name: "rate-limit flag without backend is not surfaced",
			sess: &Session{
				Status:       StatusDead,
				RateLimitHit: true,
			},
			want: string(StatusDead),
		},
		{
			name: "generic dead session is unaffected",
			sess: &Session{
				Status: StatusDead,
			},
			want: string(StatusDead),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := SessionDisplayStatusForAt(tt.sess, nil, now); got != tt.want {
				t.Fatalf("display status = %q, want %q", got, tt.want)
			}
		})
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

// TestSessionAttentionActionableAt pins the #566 TTL semantics:
// dead/failed/conflict_failed sessions without a scheduled retry age out
// of attention after FleetAttentionTTL; retry_exhausted with an open PR
// stays actionable regardless of age; live-status sessions are always
// actionable.
func TestSessionAttentionActionableAt(t *testing.T) {
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	fresh := now.Add(-1 * time.Hour)
	stale := now.Add(-FleetAttentionTTL - time.Hour)
	future := now.Add(15 * time.Minute)

	tests := []struct {
		name string
		sess *Session
		want bool
	}{
		{
			name: "running session is always actionable",
			sess: &Session{Status: StatusRunning, StartedAt: stale},
			want: true,
		},
		{
			name: "pr_open session is always actionable",
			sess: &Session{Status: StatusPROpen, StartedAt: stale, PRNumber: 1},
			want: true,
		},
		{
			name: "dead with scheduled retry is actionable",
			sess: &Session{Status: StatusDead, StartedAt: stale, FinishedAt: &stale, NextRetryAt: &future},
			want: true,
		},
		{
			name: "fresh dead session is actionable",
			sess: &Session{Status: StatusDead, StartedAt: fresh, FinishedAt: &fresh},
			want: true,
		},
		{
			name: "stale dead session ages out",
			sess: &Session{Status: StatusDead, StartedAt: stale, FinishedAt: &stale},
			want: false,
		},
		{
			name: "stale failed session ages out",
			sess: &Session{Status: StatusFailed, StartedAt: stale, FinishedAt: &stale},
			want: false,
		},
		{
			name: "stale conflict_failed session ages out",
			sess: &Session{Status: StatusConflictFailed, StartedAt: stale, FinishedAt: &stale},
			want: false,
		},
		{
			name: "retry_exhausted with open PR never ages out",
			sess: &Session{Status: StatusRetryExhausted, StartedAt: stale, FinishedAt: &stale, PRNumber: 564},
			want: true,
		},
		{
			name: "retry_exhausted without PR ages out",
			sess: &Session{Status: StatusRetryExhausted, StartedAt: stale, FinishedAt: &stale},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := SessionAttentionActionableAt(tt.sess, now); got != tt.want {
				t.Fatalf("SessionAttentionActionableAt = %v, want %v", got, tt.want)
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
	s.Sessions["recent-code-landed"] = &Session{Status: StatusCodeLanded, StartedAt: old, FinishedAt: &recent}

	live := s.LiveSessionsAt(now)
	if len(live) != 3 {
		t.Fatalf("LiveSessionsAt len = %d, want 3", len(live))
	}
}

func TestCountByStatus(t *testing.T) {
	s := NewState()
	s.Sessions["slot-1"] = &Session{IssueNumber: 1, Status: StatusRunning}
	s.Sessions["slot-2"] = &Session{IssueNumber: 2, Status: StatusRunning}
	s.Sessions["slot-3"] = &Session{IssueNumber: 3, Status: StatusPROpen}
	s.Sessions["slot-4"] = &Session{IssueNumber: 4, Status: StatusQueued}
	s.Sessions["slot-5"] = &Session{IssueNumber: 5, Status: StatusDone}       // terminal — excluded
	s.Sessions["slot-6"] = &Session{IssueNumber: 6, Status: StatusFailed}     // terminal — excluded
	s.Sessions["slot-7"] = &Session{IssueNumber: 7, Status: StatusCodeLanded} // non-terminal — included

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
	if counts[StatusCodeLanded] != 1 {
		t.Errorf("code_landed = %d, want 1", counts[StatusCodeLanded])
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
	// queued before code_landed
	if StatusPriority(StatusQueued) >= StatusPriority(StatusCodeLanded) {
		t.Error("queued should have lower priority value than code_landed")
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

// 2026-05-31: at-mint dedup tests. The dogfood "approvals storm" was 56
// duplicate spawn_worker pending approvals on issue #471 over 12h because
// RecordPendingApprovalForDecision had no dedup at all and just appended.
// These three tests pin the new contract.

func TestRecordPendingApprovalDedupsIdenticalReEmit(t *testing.T) {
	now := time.Date(2026, 5, 31, 12, 0, 0, 0, time.UTC)
	s := NewState()

	first := s.RecordPendingApprovalForDecision(testApprovalDecision(now), now)
	if first == nil {
		t.Fatal("first approval was nil")
	}
	firstID := first.ID

	// Identical re-emit one minute later (same action+target, target state
	// snapshot unchanged): supervisor recommended spawn_worker for #42 again
	// because nothing has changed downstream. Should NOT create a duplicate.
	later := now.Add(1 * time.Minute)
	dup := s.RecordPendingApprovalForDecision(testApprovalDecision(later), later)
	if dup == nil {
		t.Fatal("dedup return must not be nil — should be the existing approval")
	}
	if dup.ID != firstID {
		t.Fatalf("dedup ID = %q, want %q (returned a fresh approval — duplicate created)", dup.ID, firstID)
	}

	pending := 0
	for _, a := range s.Approvals {
		if a.Status == ApprovalStatusPending {
			pending++
		}
	}
	if pending != 1 {
		t.Fatalf("pending count = %d, want 1 (storm prevention failed)", pending)
	}
	if got := len(s.Approvals); got != 1 {
		t.Fatalf("len(s.Approvals) = %d, want 1 (no duplicate appended)", got)
	}
	if got := len(dup.Audit); got != 1 || dup.Audit[0].Event != ApprovalAuditCreated {
		t.Fatalf("audit on existing approval mutated: %+v", dup.Audit)
	}
}

// #750: re-evaluating the same (action, target) decision after the volatile
// target-state snapshot moved must UPDATE THE LIVE APPROVAL IN PLACE under its
// stable id — NOT supersede it and re-mint a sibling. The pre-#750 behaviour
// (asserted by the old TestRecordPendingApprovalSupersedesOnTargetStateChange)
// is exactly the churn that made the CLI/SPA approve path race a moving target
// every supervise cycle.
func TestRecordPendingApprovalUpdatesInPlaceOnTargetStateChange(t *testing.T) {
	now := time.Date(2026, 5, 31, 12, 0, 0, 0, time.UTC)
	s := NewState()

	first := s.RecordPendingApprovalForDecision(testApprovalDecision(now), now)
	if first == nil {
		t.Fatal("first approval was nil")
	}
	firstID := first.ID
	firstTargetHash := first.TargetStateHash

	// Add a session for the same issue — this changes the target snapshot
	// (ApprovalTargetStateHash includes session state for matching slots).
	s.Sessions = map[string]*Session{
		"sup-1": {
			IssueNumber: 42,
			Status:      StatusRunning,
			Branch:      "feat/sup-1-42-x",
			PRNumber:    0,
		},
	}

	// Sanity: TargetStateHash for the same target must now differ.
	freshHash := s.ApprovalTargetStateHash(testApprovalDecision(now).Target)
	if freshHash == firstTargetHash {
		t.Fatalf("ApprovalTargetStateHash should differ after session add: both were %q", freshHash)
	}

	later := now.Add(2 * time.Minute)
	// A fresh per-cycle decision id must NOT change the content-addressed
	// approval id — the id keys on (action, target), not decision.ID.
	freshDecision := testApprovalDecision(later)
	freshDecision.ID = "sup-approval-2"
	freshDecision.Summary = "Start a worker for issue #42: ready work (re-evaluated)"
	second := s.RecordPendingApprovalForDecision(freshDecision, later)
	if second == nil {
		t.Fatal("second approval was nil")
	}
	if second.ID != firstID {
		t.Fatalf("approval id churned after target-state change: got %q, want stable %q", second.ID, firstID)
	}

	// Still exactly one approval, still pending, no superseded sibling.
	if got := len(s.Approvals); got != 1 {
		t.Fatalf("len(approvals) = %d, want 1 (no churn sibling minted)", got)
	}
	if second.Status != ApprovalStatusPending {
		t.Fatalf("status = %q, want pending", second.Status)
	}
	for _, a := range second.Audit {
		if a.Event == ApprovalAuditSuperseded {
			t.Fatalf("approval must not be superseded on target-state churn; audit = %+v", second.Audit)
		}
	}

	// Updated in place: target-state snapshot and re-derived content refreshed,
	// UpdatedAt bumped, CreatedAt preserved, PayloadHash self-consistent.
	if second.TargetStateHash != freshHash {
		t.Fatalf("TargetStateHash = %q, want refreshed %q", second.TargetStateHash, freshHash)
	}
	if !second.CreatedAt.Equal(first.CreatedAt) {
		t.Fatalf("CreatedAt moved: got %s, want %s", second.CreatedAt, first.CreatedAt)
	}
	if !second.UpdatedAt.After(first.CreatedAt) {
		t.Fatalf("UpdatedAt = %s, want bumped past CreatedAt %s", second.UpdatedAt, first.CreatedAt)
	}
	if second.Summary != freshDecision.Summary {
		t.Fatalf("summary = %q, want refreshed %q", second.Summary, freshDecision.Summary)
	}
	if second.ComputePayloadHash() != second.PayloadHash {
		t.Fatalf("PayloadHash not refreshed in place: stored %q, computed %q", second.PayloadHash, second.ComputePayloadHash())
	}

	// The id remains approvable after the in-place update.
	if _, err := s.ApproveApproval(firstID, later.Add(time.Minute), "cli", "go"); err != nil {
		t.Fatalf("ApproveApproval after in-place update: %v", err)
	}
}

// TestRecordPendingApprovalStableAcrossSupervisorCycles drives several
// supervise cycles over an unchanged merge_pr decision while the bound
// session's volatile runtime snapshot (status, retry count, next-retry time)
// churns every cycle, and asserts the approval id is stable and still
// approvable on the last cycle (#750 acceptance criteria 1, 2, 5). Before
// #750 each cycle re-minted a fresh id keyed on the per-cycle decision
// id/timestamp and staled the prior one, so `supervise approve <id>` always
// raced a moving target.
func TestRecordPendingApprovalStableAcrossSupervisorCycles(t *testing.T) {
	now := time.Date(2026, 6, 20, 12, 0, 0, 0, time.UTC)
	s := NewState()
	s.Sessions["slot-1"] = &Session{
		IssueNumber: 700,
		Status:      StatusRetryExhausted,
		PRNumber:    748,
		RetryCount:  3,
	}
	decisionFor := func(cycle int, at time.Time) SupervisorDecision {
		return SupervisorDecision{
			// Production mints a UNIQUE per-cycle decision id; the stable
			// approval id must NOT depend on it.
			ID:                fmt.Sprintf("sup-cycle-%d", cycle),
			CreatedAt:         at,
			Project:           "owner/repo",
			Summary:           "Merge PR #748 (retry-exhausted hand-off).",
			RecommendedAction: "merge_pr",
			Target:            &SupervisorTarget{PR: 748, Issue: 700},
			Risk:              "mutating",
			Reasons:           []string{"PR #748 is green"},
		}
	}

	var ids []string
	for cycle := 0; cycle < 4; cycle++ {
		at := now.Add(time.Duration(cycle) * time.Minute)
		// Cycle top: the supervisor stales drifted approvals before re-emit.
		s.MarkStaleApprovals(at)
		approval := s.RecordPendingApprovalForDecision(decisionFor(cycle, at), at)
		if approval == nil {
			t.Fatalf("cycle %d: approval was nil", cycle)
		}
		if approval.Status != ApprovalStatusPending {
			t.Fatalf("cycle %d: status = %q, want pending", cycle, approval.Status)
		}
		ids = append(ids, approval.ID)
		// Churn the bound session's runtime snapshot, the way a retry-exhausted
		// worker's NextRetryAt / RetryCount move every supervise cycle.
		next := at.Add(30 * time.Minute)
		s.Sessions["slot-1"].NextRetryAt = &next
		s.Sessions["slot-1"].RetryCount = 3 + cycle
	}

	for i, id := range ids {
		if id != ids[0] {
			t.Fatalf("approval id churned: cycle %d id = %q, cycle 0 id = %q", i, id, ids[0])
		}
	}
	// Exactly one approval ever existed — no stale/superseded siblings.
	if len(s.Approvals) != 1 {
		t.Fatalf("len(approvals) = %d, want 1 (no churn siblings)", len(s.Approvals))
	}

	// The id read a cycle earlier is still approvable on the last cycle.
	approved, err := s.ApproveApproval(ids[0], now.Add(5*time.Minute), "cli", "land hand-off")
	if err != nil {
		t.Fatalf("ApproveApproval(stable id) after %d cycles: %v", len(ids), err)
	}
	if approved.Status != ApprovalStatusApproved {
		t.Fatalf("approved status = %q, want approved", approved.Status)
	}
}

func TestRecordPendingApprovalDifferentActionsCoexist(t *testing.T) {
	// Two pending approvals with the SAME target but DIFFERENT actions
	// must coexist — the dedup is keyed on (Action, Target), not Target alone.
	now := time.Date(2026, 5, 31, 12, 0, 0, 0, time.UTC)
	s := NewState()

	spawn := s.RecordPendingApprovalForDecision(testApprovalDecision(now), now)
	if spawn == nil {
		t.Fatal("spawn approval was nil")
	}

	closeDecision := SupervisorDecision{
		ID:                "sup-close-42",
		CreatedAt:         now,
		Project:           "owner/repo",
		Mode:              "read_only",
		Summary:           "Close issue #42 (resolved)",
		RecommendedAction: "close_issue",
		Target:            &SupervisorTarget{Issue: 42},
		Risk:              "high",
	}
	closeApp := s.RecordPendingApprovalForDecision(closeDecision, now)
	if closeApp == nil {
		t.Fatal("close approval was nil")
	}
	if closeApp.ID == spawn.ID {
		t.Fatal("close action wrongly deduped against spawn action; they share Target but Action differs")
	}

	if got := len(s.Approvals); got != 2 {
		t.Fatalf("len(s.Approvals) = %d, want 2 (different actions must coexist)", got)
	}
}

// #515: dedup must coalesce against an awaiting_dispatch record too.
// Scenario: supervisor mints a pending; operator approves it; executor
// transitions it to awaiting_dispatch (spawn_worker side-effect on the
// dispatcher loop). Before the dispatcher ticks, supervisor cycles and
// would re-recommend spawn_worker for the same issue. Without #515,
// dedup only scanned pending and a fresh duplicate was minted. With
// #515, the dedup loop also checks awaiting_dispatch and returns the
// existing record, suppressing the duplicate.
func TestRecordPendingApprovalDedupsAgainstAwaitingDispatch(t *testing.T) {
	now := time.Date(2026, 5, 31, 16, 18, 0, 0, time.UTC)
	s := NewState()

	first := s.RecordPendingApprovalForDecision(testApprovalDecision(now), now)
	if first == nil {
		t.Fatal("first approval was nil")
	}
	firstID := first.ID

	// Simulate the executor transition to awaiting_dispatch.
	first.Status = ApprovalStatusAwaitingDispatch

	// Supervisor cycles 30s later, would re-recommend the same target
	// (different decision ID — like supervisor mints fresh decisions
	// per cycle).
	later := now.Add(30 * time.Second)
	freshDecision := testApprovalDecision(later)
	freshDecision.ID = "sup-approval-2"
	dup := s.RecordPendingApprovalForDecision(freshDecision, later)
	if dup == nil {
		t.Fatal("dedup return must not be nil — should be the awaiting_dispatch existing approval")
	}
	if dup.ID != firstID {
		t.Fatalf("dedup ID = %q, want %q (race window: fresh duplicate minted while awaiting_dispatch was effective)", dup.ID, firstID)
	}
	if got := len(s.Approvals); got != 1 {
		t.Fatalf("len(s.Approvals) = %d, want 1 (no fresh pending duplicate)", got)
	}
	if dup.Status != ApprovalStatusAwaitingDispatch {
		t.Fatalf("returned approval status = %q, want %q (must keep awaiting_dispatch unchanged)", dup.Status, ApprovalStatusAwaitingDispatch)
	}
}

// #771 follow-up: a pending spawn_repair_worker approval must auto-stale when
// its issue is resolved (orchestrator auto-closed it after a verified merge),
// so it stops lingering as a Past-SLA red flag on the dashboard
// (dogfood #773/#774/#775). Only the repair approval for the resolved issue is
// touched — a repair approval for a different issue, and a spawn_worker approval
// for the same issue, are left pending.
func TestMarkSpawnRepairWorkerApprovalsStaleForResolvedIssue(t *testing.T) {
	now := time.Date(2026, 6, 25, 12, 0, 0, 0, time.UTC)
	s := NewState()
	s.Approvals = []Approval{
		{ID: "ap-repair-759", Action: approvalActionSpawnRepairWorker, Target: &SupervisorTarget{Issue: 759, PR: 773}, Status: ApprovalStatusPending, CreatedAt: now},
		{ID: "ap-repair-800", Action: approvalActionSpawnRepairWorker, Target: &SupervisorTarget{Issue: 800, PR: 801}, Status: ApprovalStatusPending, CreatedAt: now},
		{ID: "ap-spawn-759", Action: approvalActionSpawnWorker, Target: &SupervisorTarget{Issue: 759}, Status: ApprovalStatusPending, CreatedAt: now},
	}

	count := s.MarkSpawnRepairWorkerApprovalsStaleForResolvedIssue(759, now.Add(time.Minute))
	if count != 1 {
		t.Fatalf("staled count = %d, want 1", count)
	}

	statusOf := func(id string) ApprovalStatus {
		a, ok := s.FindApproval(id)
		if !ok {
			t.Fatalf("approval %q vanished", id)
		}
		return a.Status
	}
	if got := statusOf("ap-repair-759"); got != ApprovalStatusStale {
		t.Fatalf("repair approval for resolved issue = %q, want %q", got, ApprovalStatusStale)
	}
	if got := statusOf("ap-repair-800"); got != ApprovalStatusPending {
		t.Fatalf("repair approval for a different issue = %q, want pending (untouched)", got)
	}
	if got := statusOf("ap-spawn-759"); got != ApprovalStatusPending {
		t.Fatalf("spawn_worker approval = %q, want pending (only spawn_repair_worker is staled)", got)
	}
}

// #866: the reason-carrying core stales approved/awaiting_dispatch repair
// approvals too, records the supplied terminal-outcome reason in the audit
// trail, returns the staled approvals for per-approval journaling, and is
// idempotent (a second call finds nothing left to stale).
func TestStaleSpawnRepairWorkerApprovalsForResolvedIssue(t *testing.T) {
	now := time.Date(2026, 7, 10, 9, 47, 0, 0, time.UTC)
	s := NewState()
	s.Approvals = []Approval{
		{ID: "ap-repair-858", Action: approvalActionSpawnRepairWorker, Target: &SupervisorTarget{Issue: 858, PR: 864}, Status: ApprovalStatusPending, CreatedAt: now},
		{ID: "ap-repair-858b", Action: approvalActionSpawnRepairWorker, Target: &SupervisorTarget{Issue: 858, PR: 864}, Status: ApprovalStatusAwaitingDispatch, CreatedAt: now},
		{ID: "ap-repair-900", Action: approvalActionSpawnRepairWorker, Target: &SupervisorTarget{Issue: 900}, Status: ApprovalStatusPending, CreatedAt: now},
	}

	reason := "issue #858 resolved (verified merge) — repair worker moot"
	staled := s.StaleSpawnRepairWorkerApprovalsForResolvedIssue(858, now.Add(time.Minute), reason)
	if len(staled) != 2 {
		t.Fatalf("staled = %d approvals, want 2 (pending + awaiting_dispatch for issue 858)", len(staled))
	}
	for _, ap := range staled {
		if ap.Status != ApprovalStatusStale {
			t.Fatalf("returned approval %s status = %q, want stale", ap.ID, ap.Status)
		}
		last := ap.Audit[len(ap.Audit)-1]
		if last.Event != ApprovalAuditStale || last.Reason != reason {
			t.Fatalf("approval %s last audit = {%q,%q}, want {stale,%q}", ap.ID, last.Event, last.Reason, reason)
		}
	}
	if got, _ := s.FindApproval("ap-repair-900"); got.Status != ApprovalStatusPending {
		t.Fatalf("unrelated issue approval = %q, want pending (untouched)", got.Status)
	}

	// Idempotent: nothing left active for issue 858.
	if again := s.StaleSpawnRepairWorkerApprovalsForResolvedIssue(858, now.Add(2*time.Minute), reason); len(again) != 0 {
		t.Fatalf("second reconcile staled %d approvals, want 0 (idempotent)", len(again))
	}
}

// #866: ActiveSpawnRepairWorkerApprovalIssues enumerates only issues that still
// carry an active repair approval, distinct and sorted, ignoring terminal
// approvals and non-repair actions.
func TestActiveSpawnRepairWorkerApprovalIssues(t *testing.T) {
	now := time.Date(2026, 7, 10, 9, 47, 0, 0, time.UTC)
	s := NewState()
	s.Approvals = []Approval{
		{ID: "a1", Action: approvalActionSpawnRepairWorker, Target: &SupervisorTarget{Issue: 858}, Status: ApprovalStatusPending, CreatedAt: now},
		{ID: "a2", Action: approvalActionSpawnRepairWorker, Target: &SupervisorTarget{Issue: 858}, Status: ApprovalStatusAwaitingDispatch, CreatedAt: now},
		{ID: "a3", Action: approvalActionSpawnRepairWorker, Target: &SupervisorTarget{Issue: 700}, Status: ApprovalStatusApproved, CreatedAt: now},
		{ID: "a4", Action: approvalActionSpawnRepairWorker, Target: &SupervisorTarget{Issue: 999}, Status: ApprovalStatusStale, CreatedAt: now},
		{ID: "a5", Action: approvalActionSpawnWorker, Target: &SupervisorTarget{Issue: 500}, Status: ApprovalStatusPending, CreatedAt: now},
	}

	got := s.ActiveSpawnRepairWorkerApprovalIssues()
	want := []int{700, 858}
	if len(got) != len(want) {
		t.Fatalf("active issues = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("active issues = %v, want %v", got, want)
		}
	}
}

// #515: ReconcileSpawnWorkerApprovalsForStartedSession must supersede
// awaiting_dispatch records too, not just pending ones — once the
// worker actually starts, the awaiting record has done its job.
func TestReconcileSpawnWorkerApprovalsSupersedesAwaitingDispatch(t *testing.T) {
	now := time.Date(2026, 5, 31, 16, 18, 0, 0, time.UTC)
	s := NewState()
	a := s.RecordPendingApprovalForDecision(testApprovalDecision(now), now)
	if a == nil {
		t.Fatal("approval was nil")
	}
	a.Status = ApprovalStatusAwaitingDispatch

	sess := &Session{
		IssueNumber: 42,
		Status:      StatusRunning,
		PRNumber:    0,
		Branch:      "feat/sup-1-42-x",
		// StartedAt > approval.CreatedAt — matches the gate inside
		// spawnWorkerApprovalMatchesSession (only sessions started
		// AFTER the approval are considered "spawned by it").
		StartedAt: now.Add(30 * time.Second),
	}
	s.Sessions = map[string]*Session{"sup-1": sess}

	count := s.ReconcileSpawnWorkerApprovalsForStartedSession("sup-1", sess, now.Add(time.Minute))
	if count != 1 {
		t.Fatalf("reconciled count = %d, want 1 (must supersede awaiting_dispatch)", count)
	}
	if got := s.Approvals[0].Status; got != ApprovalStatusSuperseded {
		t.Fatalf("approval status = %q, want %q", got, ApprovalStatusSuperseded)
	}
}

// #515 follow-up: MarkApprovalAwaitingDispatch transitions an approved
// approval to awaiting_dispatch with an audit entry, mirroring the
// existing MarkApprovalExecuted/Failed/Skipped helpers. Idempotency
// guarantee: not-approved input returns ErrApprovalNotApproved.
func TestMarkApprovalAwaitingDispatch_TransitionsApprovedToAwaiting(t *testing.T) {
	now := time.Date(2026, 5, 31, 17, 0, 0, 0, time.UTC)
	s := NewState()
	a := s.RecordPendingApprovalForDecision(testApprovalDecision(now), now)
	if a == nil {
		t.Fatal("approval was nil")
	}
	a.Status = ApprovalStatusApproved

	updated, err := s.MarkApprovalAwaitingDispatch(a.ID, now.Add(time.Minute), "supervisor", "test reason")
	if err != nil {
		t.Fatalf("MarkApprovalAwaitingDispatch: %v", err)
	}
	if updated.Status != ApprovalStatusAwaitingDispatch {
		t.Fatalf("status = %q, want awaiting_dispatch", updated.Status)
	}
	if got := updated.Audit[len(updated.Audit)-1].Event; got != ApprovalAuditAwaitingDispatch {
		t.Fatalf("last audit event = %q, want awaiting_dispatch", got)
	}
	if got := updated.Audit[len(updated.Audit)-1].Reason; got != "test reason" {
		t.Fatalf("audit reason = %q, want %q", got, "test reason")
	}
}

func TestMarkApprovalAwaitingDispatch_RefusesNonApproved(t *testing.T) {
	now := time.Date(2026, 5, 31, 17, 0, 0, 0, time.UTC)
	s := NewState()
	a := s.RecordPendingApprovalForDecision(testApprovalDecision(now), now)
	// Status stays Pending; helper must refuse.
	_, err := s.MarkApprovalAwaitingDispatch(a.ID, now, "supervisor", "x")
	if err != ErrApprovalNotApproved {
		t.Fatalf("err = %v, want ErrApprovalNotApproved", err)
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

// #750: target-state drift between mint and approve must NOT stale the
// approval. The pre-#750 behaviour (asserted by the old
// TestApproveStaleApprovalFailsSafely) returned ErrApprovalStale here, which
// is precisely why `supervise approve <freshest-id>` raced a moving target on
// the dogfood supervisor. The executor re-validates runtime preconditions at
// execute time, so the approve gate keys only on the unchanged decision
// identity. Genuine payload-identity changes still fail safely — see
// TestApproveChangedApprovalPayloadFailsSafely.
func TestApproveSucceedsAcrossTargetStateDrift(t *testing.T) {
	now := time.Date(2026, 4, 29, 12, 0, 0, 0, time.UTC)
	s := NewState()
	s.Sessions["slot-1"] = &Session{IssueNumber: 77, Status: StatusRetryExhausted, PRNumber: 12}
	decision := SupervisorDecision{
		ID:                "sup-stable",
		CreatedAt:         now,
		Project:           "owner/repo",
		Mode:              "read_only",
		Summary:           "Merge PR #12 for issue #77.",
		RecommendedAction: "merge_pr",
		Target:            &SupervisorTarget{Issue: 77, PR: 12, Session: "slot-1"},
		Risk:              "mutating",
		Confidence:        0.93,
		Reasons:           []string{"PR is green"},
	}
	approval := s.RecordPendingApprovalForDecision(decision, now)
	// The bound session's runtime state moves before the operator approves.
	s.Sessions["slot-1"].Status = StatusDone

	approved, err := s.ApproveApproval(approval.ID, now.Add(time.Minute), "cli", "land it")
	if err != nil {
		t.Fatalf("ApproveApproval across target-state drift: %v, want success", err)
	}
	if approved.Status != ApprovalStatusApproved {
		t.Fatalf("status = %q, want approved", approved.Status)
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

func TestSessionStale_DeadIdleAndClosedPRWithMissingWorktree(t *testing.T) {
	now := time.Now().UTC()
	finished := now.Add(-30 * time.Hour)
	sess := &Session{
		IssueNumber: 101,
		Status:      StatusDead,
		StartedAt:   finished.Add(-time.Hour),
		FinishedAt:  &finished,
		Worktree:    "/tmp/missing-worktree",
		PRNumber:    42,
	}
	policy := StaleSessionPolicy{
		Enabled:                true,
		IdleAfter:              24 * time.Hour,
		RequireWorktreeMissing: true,
	}
	worktreeExists := func(path string) bool { return false }

	audit, stale := SessionStale(sess, now, policy, worktreeExists)
	if !stale {
		t.Fatalf("session should be stale: %+v", audit)
	}
	if audit.IssueNumber != 101 || audit.PRNumber != 42 {
		t.Fatalf("audit target = %+v, want issue=101 pr=42", audit)
	}
	if audit.IdleSeconds < int64(24*time.Hour/time.Second) {
		t.Fatalf("audit idle seconds = %d, want >= 24h", audit.IdleSeconds)
	}
	if audit.Reason == "" {
		t.Fatalf("audit reason should be populated")
	}
}

func TestSessionStale_ActiveWorktreeKeepsSessionLive(t *testing.T) {
	now := time.Now().UTC()
	finished := now.Add(-30 * time.Hour)
	sess := &Session{
		IssueNumber: 102,
		Status:      StatusDead,
		StartedAt:   finished.Add(-time.Hour),
		FinishedAt:  &finished,
		Worktree:    "/tmp/active-worktree",
	}
	policy := StaleSessionPolicy{
		Enabled:                true,
		IdleAfter:              24 * time.Hour,
		RequireWorktreeMissing: true,
	}
	worktreeExists := func(path string) bool { return true }

	if _, stale := SessionStale(sess, now, policy, worktreeExists); stale {
		t.Fatalf("session with present worktree must not be marked stale")
	}
}

func TestSessionStale_RecentSessionIsNotStaleEvenWithMissingWorktree(t *testing.T) {
	now := time.Now().UTC()
	finished := now.Add(-3 * time.Hour)
	sess := &Session{
		IssueNumber: 103,
		Status:      StatusDead,
		StartedAt:   finished.Add(-time.Hour),
		FinishedAt:  &finished,
		Worktree:    "/tmp/missing",
	}
	policy := StaleSessionPolicy{
		Enabled:                true,
		IdleAfter:              24 * time.Hour,
		RequireWorktreeMissing: true,
	}
	worktreeExists := func(path string) bool { return false }

	if _, stale := SessionStale(sess, now, policy, worktreeExists); stale {
		t.Fatalf("session within idle window must not be reclassified")
	}
}

func TestSessionStale_RunningSessionIsNeverStale(t *testing.T) {
	now := time.Now().UTC()
	sess := &Session{
		IssueNumber: 104,
		Status:      StatusRunning,
		StartedAt:   now.Add(-72 * time.Hour),
		Worktree:    "/tmp/missing",
	}
	policy := StaleSessionPolicy{
		Enabled:                true,
		IdleAfter:              24 * time.Hour,
		RequireWorktreeMissing: true,
	}
	if _, stale := SessionStale(sess, now, policy, func(string) bool { return false }); stale {
		t.Fatalf("running session must never be marked stale")
	}
}

func TestSessionStale_ScheduledRetryIsNotStale(t *testing.T) {
	now := time.Now().UTC()
	finished := now.Add(-30 * time.Hour)
	retry := now.Add(15 * time.Minute)
	sess := &Session{
		IssueNumber: 105,
		Status:      StatusDead,
		StartedAt:   finished.Add(-time.Hour),
		FinishedAt:  &finished,
		NextRetryAt: &retry,
		Worktree:    "/tmp/missing",
	}
	policy := StaleSessionPolicy{
		Enabled:                true,
		IdleAfter:              24 * time.Hour,
		RequireWorktreeMissing: true,
	}
	if _, stale := SessionStale(sess, now, policy, func(string) bool { return false }); stale {
		t.Fatalf("session with pending retry must not be reclassified")
	}
}

func TestReconcileStaleSessions_IsIdempotent(t *testing.T) {
	now := time.Now().UTC()
	finished := now.Add(-30 * time.Hour)
	st := NewState()
	st.Sessions["slot-stale"] = &Session{
		IssueNumber: 201,
		Status:      StatusDead,
		StartedAt:   finished.Add(-time.Hour),
		FinishedAt:  &finished,
		Worktree:    "/tmp/missing-1",
	}
	st.Sessions["slot-live"] = &Session{
		IssueNumber: 202,
		Status:      StatusRunning,
		StartedAt:   now.Add(-1 * time.Minute),
	}
	policy := StaleSessionPolicy{
		Enabled:                true,
		IdleAfter:              24 * time.Hour,
		RequireWorktreeMissing: true,
	}
	worktreeExists := func(string) bool { return false }

	first := st.ReconcileStaleSessions(now, policy, worktreeExists)
	second := st.ReconcileStaleSessions(now, policy, worktreeExists)
	if len(first) != 1 {
		t.Fatalf("first pass audits = %d, want 1", len(first))
	}
	if len(second) != 1 || second[0].Slot != first[0].Slot {
		t.Fatalf("second pass audits = %v, want identical to first", second)
	}
	if len(st.Sessions) != 2 {
		t.Fatalf("state should not be mutated; sessions = %d", len(st.Sessions))
	}
}

func TestSessionStale_LinkedMergedPRDismissesRegardlessOfIdle(t *testing.T) {
	now := time.Now().UTC()
	finished := now.Add(-15 * time.Minute) // well below IdleAfter window
	sess := &Session{
		IssueNumber: 347,
		PRNumber:    396,
		Status:      StatusRetryExhausted,
		Branch:      "feat/sup-46-347-confirmation-dialog",
		StartedAt:   finished.Add(-5 * time.Minute),
		FinishedAt:  &finished,
		Worktree:    "/tmp/sup-46",
	}
	policy := StaleSessionPolicy{
		Enabled:                true,
		IdleAfter:              24 * time.Hour,
		RequireWorktreeMissing: true,
		MergedPRDismisses:      true,
		PRStateForBranchPR: func(b string, pr int) string {
			if b == "feat/sup-46-347-confirmation-dialog" && pr == 396 {
				return "MERGED"
			}
			return ""
		},
	}
	worktreeExists := func(string) bool { return true } // worktree present and idle window unmet — old policy would not fire

	audit, stale := SessionStale(sess, now, policy, worktreeExists)
	if !stale {
		t.Fatalf("session with MERGED linked PR should be reconciled regardless of idle/worktree")
	}
	if audit.Reason != MergedPRReason {
		t.Fatalf("audit reason = %q, want %q", audit.Reason, MergedPRReason)
	}
	if audit.IssueNumber != 347 || audit.PRNumber != 396 {
		t.Fatalf("audit target = %+v, want issue=347 pr=396", audit)
	}
}

func TestSessionStale_NoLinkedPRFollowsLegacyPolicy(t *testing.T) {
	now := time.Now().UTC()
	finished := now.Add(-30 * time.Hour)
	sess := &Session{
		IssueNumber: 101,
		Status:      StatusDead,
		Branch:      "feat/sup-44-101-no-pr-yet",
		StartedAt:   finished.Add(-time.Hour),
		FinishedAt:  &finished,
		Worktree:    "/tmp/missing-44",
	}
	policy := StaleSessionPolicy{
		Enabled:                true,
		IdleAfter:              24 * time.Hour,
		RequireWorktreeMissing: true,
		MergedPRDismisses:      true,
		PRStateForBranchPR:     func(string, int) string { return "" }, // no link known
	}
	worktreeExists := func(string) bool { return false }

	audit, stale := SessionStale(sess, now, policy, worktreeExists)
	if !stale {
		t.Fatalf("session with no linked PR but stale-by-idle should still reconcile via legacy policy")
	}
	if audit.Reason == MergedPRReason {
		t.Fatalf("audit reason should reflect legacy policy, got %q", audit.Reason)
	}

	// And: a session within the idle window with no linked PR is not dismissed.
	freshFinished := now.Add(-5 * time.Minute)
	fresh := &Session{
		IssueNumber: 102,
		Status:      StatusDead,
		Branch:      "feat/sup-44-102",
		StartedAt:   freshFinished.Add(-time.Hour),
		FinishedAt:  &freshFinished,
		Worktree:    "/tmp/missing-fresh",
	}
	if _, stale := SessionStale(fresh, now, policy, worktreeExists); stale {
		t.Fatalf("session within idle window and no linked PR must not be reconciled")
	}
}

func TestSessionStale_LinkedOpenPRIsNotDismissed(t *testing.T) {
	now := time.Now().UTC()
	finished := now.Add(-15 * time.Minute)
	sess := &Session{
		IssueNumber: 200,
		PRNumber:    500,
		Status:      StatusRetryExhausted,
		Branch:      "feat/sup-1-200-still-open",
		StartedAt:   finished.Add(-time.Hour),
		FinishedAt:  &finished,
		Worktree:    "/tmp/sup-1",
	}
	policy := StaleSessionPolicy{
		Enabled:                true,
		IdleAfter:              24 * time.Hour,
		RequireWorktreeMissing: true,
		MergedPRDismisses:      true,
		PRStateForBranchPR:     func(string, int) string { return "OPEN" },
	}
	if _, stale := SessionStale(sess, now, policy, func(string) bool { return true }); stale {
		t.Fatalf("session whose linked PR is OPEN must not be dismissed by the merged-PR path")
	}
}

// TestSessionStale_MergedPRRequiresPRNumberMatch guards against false
// dismissals after issue re-open: a session on the same branch as a
// previously-merged PR but with its own (different) PRNumber must not be
// dismissed by the merged-PR path. Lookup is keyed by (branch, PRNumber).
func TestSessionStale_MergedPRRequiresPRNumberMatch(t *testing.T) {
	now := time.Now().UTC()
	finished := now.Add(-15 * time.Minute)
	sess := &Session{
		IssueNumber: 347,
		PRNumber:    200, // new PR after issue re-opened
		Status:      StatusRetryExhausted,
		Branch:      "feat/sup-46-347-confirmation-dialog",
		StartedAt:   finished.Add(-time.Hour),
		FinishedAt:  &finished,
		Worktree:    "/tmp/sup-46",
	}
	policy := StaleSessionPolicy{
		Enabled:                true,
		IdleAfter:              24 * time.Hour,
		RequireWorktreeMissing: true,
		MergedPRDismisses:      true,
		// Branch matches but only the OLD PR (100) is merged.
		PRStateForBranchPR: func(b string, pr int) string {
			if b == "feat/sup-46-347-confirmation-dialog" && pr == 100 {
				return "MERGED"
			}
			return ""
		},
	}
	if _, stale := SessionStale(sess, now, policy, func(string) bool { return true }); stale {
		t.Fatalf("session whose own PR (200) is not merged must not be dismissed by branch reuse")
	}
}

// TestSessionStale_MergedPRSkippedWhenPRNumberZero ensures sessions
// without a recorded PRNumber are never dismissed via the merged-PR
// path. Without a PRNumber there is no way to distinguish a stale match
// from a live retry on the same branch.
func TestSessionStale_MergedPRSkippedWhenPRNumberZero(t *testing.T) {
	now := time.Now().UTC()
	finished := now.Add(-15 * time.Minute)
	sess := &Session{
		IssueNumber: 347,
		PRNumber:    0, // unknown
		Status:      StatusRetryExhausted,
		Branch:      "feat/sup-46-347-confirmation-dialog",
		StartedAt:   finished.Add(-time.Hour),
		FinishedAt:  &finished,
		Worktree:    "/tmp/sup-46",
	}
	called := false
	policy := StaleSessionPolicy{
		Enabled:                true,
		IdleAfter:              24 * time.Hour,
		RequireWorktreeMissing: true,
		MergedPRDismisses:      true,
		PRStateForBranchPR: func(string, int) string {
			called = true
			return "MERGED"
		},
	}
	if _, stale := SessionStale(sess, now, policy, func(string) bool { return true }); stale {
		t.Fatalf("session with PRNumber=0 must not be dismissed by merged-PR path")
	}
	if called {
		t.Fatalf("lookup must not be called when PRNumber=0 — there is nothing to match")
	}
}

func TestSessionStale_MergedPRDismissesDisabledIgnoresLookup(t *testing.T) {
	now := time.Now().UTC()
	finished := now.Add(-15 * time.Minute)
	sess := &Session{
		IssueNumber: 347,
		PRNumber:    396,
		Status:      StatusRetryExhausted,
		Branch:      "feat/sup-46-347-confirmation-dialog",
		StartedAt:   finished.Add(-5 * time.Minute),
		FinishedAt:  &finished,
		Worktree:    "/tmp/sup-46",
	}
	policy := StaleSessionPolicy{
		Enabled:                true,
		IdleAfter:              24 * time.Hour,
		RequireWorktreeMissing: true,
		MergedPRDismisses:      false, // disabled — must match legacy behavior
		PRStateForBranchPR:     func(string, int) string { return "MERGED" },
	}
	if _, stale := SessionStale(sess, now, policy, func(string) bool { return true }); stale {
		t.Fatalf("merged_pr_dismisses=false must disable the linked-PR path")
	}
}

func TestReconcileStaleSessions_LinkedMergedPRIsIdempotent(t *testing.T) {
	now := time.Now().UTC()
	finished := now.Add(-15 * time.Minute)
	st := NewState()
	st.Sessions["sup-46"] = &Session{
		IssueNumber: 347,
		PRNumber:    396,
		Status:      StatusRetryExhausted,
		Branch:      "feat/sup-46-347-confirmation-dialog",
		StartedAt:   finished.Add(-time.Hour),
		FinishedAt:  &finished,
		Worktree:    "/tmp/sup-46",
	}
	st.Sessions["sup-99"] = &Session{
		IssueNumber: 999,
		Status:      StatusRunning,
		StartedAt:   now.Add(-time.Minute),
	}
	lookup := func(b string, pr int) string {
		if b == "feat/sup-46-347-confirmation-dialog" && pr == 396 {
			return "MERGED"
		}
		return ""
	}
	policy := StaleSessionPolicy{
		Enabled:                true,
		IdleAfter:              24 * time.Hour,
		RequireWorktreeMissing: true,
		MergedPRDismisses:      true,
		PRStateForBranchPR:     lookup,
	}
	first := st.ReconcileStaleSessions(now, policy, func(string) bool { return true })
	second := st.ReconcileStaleSessions(now, policy, func(string) bool { return true })
	if len(first) != 1 || first[0].Slot != "sup-46" || first[0].Reason != MergedPRReason {
		t.Fatalf("first pass = %+v, want one sup-46 audit with merged-PR reason", first)
	}
	if len(second) != 1 || second[0].Slot != first[0].Slot || second[0].Reason != first[0].Reason {
		t.Fatalf("second pass = %+v, want identical to first", second)
	}
	if len(st.Sessions) != 2 {
		t.Fatalf("state must not be mutated; sessions = %d", len(st.Sessions))
	}
}

func TestReconcileStaleSessions_DisabledReturnsNothing(t *testing.T) {
	now := time.Now().UTC()
	finished := now.Add(-30 * time.Hour)
	st := NewState()
	st.Sessions["slot-1"] = &Session{
		IssueNumber: 1,
		Status:      StatusDead,
		FinishedAt:  &finished,
		StartedAt:   finished.Add(-time.Hour),
		Worktree:    "/tmp/missing",
	}
	policy := StaleSessionPolicy{Enabled: false, IdleAfter: time.Hour, RequireWorktreeMissing: true}
	if got := st.ReconcileStaleSessions(now, policy, func(string) bool { return false }); len(got) != 0 {
		t.Fatalf("disabled policy returned %d audits, want 0", len(got))
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
