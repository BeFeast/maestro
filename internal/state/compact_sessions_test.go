package state

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCompactSessions_KeepsNewest20OfFiftyDone(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	s := NewState()
	for i := 0; i < 50; i++ {
		finished := now.Add(-time.Duration(i+30) * 24 * time.Hour) // all older than 7d
		started := finished.Add(-time.Hour)
		slot := fmt.Sprintf("done-%02d", i)
		s.Sessions[slot] = &Session{
			IssueNumber: i + 1,
			Status:      StatusDone,
			StartedAt:   started,
			FinishedAt:  &finished,
		}
	}
	s.Sessions["running"] = &Session{
		IssueNumber: 999,
		Status:      StatusRunning,
		StartedAt:   now.Add(-time.Hour),
	}

	archive := filepath.Join(t.TempDir(), "sessions-archive.jsonl")
	policy := SessionRetentionPolicy{KeepLast: 20, MinAge: 7 * 24 * time.Hour, ArchiveFile: archive}

	res, err := s.CompactSessions(policy, now)
	if err != nil {
		t.Fatalf("CompactSessions: %v", err)
	}
	if res.Removed != 30 {
		t.Errorf("Removed=%d, want 30", res.Removed)
	}
	if res.Archived != 30 {
		t.Errorf("Archived=%d, want 30", res.Archived)
	}
	if _, ok := s.Sessions["running"]; !ok {
		t.Error("running session must not be touched")
	}
	if len(s.Sessions) != 21 {
		t.Errorf("remaining sessions=%d, want 21 (20 done + 1 running)", len(s.Sessions))
	}
	// The 20 kept slots are the newest by FinishedAt — done-00..done-19.
	for i := 0; i < 20; i++ {
		slot := fmt.Sprintf("done-%02d", i)
		if _, ok := s.Sessions[slot]; !ok {
			t.Errorf("expected %s kept", slot)
		}
	}
	for i := 20; i < 50; i++ {
		slot := fmt.Sprintf("done-%02d", i)
		if _, ok := s.Sessions[slot]; ok {
			t.Errorf("expected %s pruned", slot)
		}
	}

	// Verify archive content is one JSON record per line.
	f, err := os.Open(archive)
	if err != nil {
		t.Fatalf("open archive: %v", err)
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	lines := 0
	for scanner.Scan() {
		var rec archivedSessionRecord
		if err := json.Unmarshal(scanner.Bytes(), &rec); err != nil {
			t.Fatalf("decode archive line: %v", err)
		}
		if rec.Session == nil || rec.Slot == "" {
			t.Errorf("archive record missing fields: %+v", rec)
		}
		lines++
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scan archive: %v", err)
	}
	if lines != 30 {
		t.Errorf("archive lines=%d, want 30", lines)
	}
}

func TestCompactSessions_ActiveSessionsUntouched(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	s := NewState()
	veryOld := now.Add(-90 * 24 * time.Hour)
	for _, status := range []SessionStatus{StatusRunning, StatusPROpen, StatusQueued} {
		slot := "active-" + string(status)
		s.Sessions[slot] = &Session{
			IssueNumber: 1,
			Status:      status,
			StartedAt:   veryOld,
		}
	}

	res, err := s.CompactSessions(SessionRetentionPolicy{KeepLast: 0, MinAge: 0}, now)
	if err != nil {
		t.Fatalf("CompactSessions: %v", err)
	}
	if res.Removed != 0 {
		t.Errorf("Removed=%d, want 0", res.Removed)
	}
	if len(s.Sessions) != 3 {
		t.Errorf("len(Sessions)=%d, want 3 (no active session pruned)", len(s.Sessions))
	}
}

func TestCompactSessions_AgeFloorKeepsRecent(t *testing.T) {
	// 30 done sessions all within the last 24h: nothing should be pruned
	// because every one of them is younger than MinAge, even though the
	// count window is 20.
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	s := NewState()
	for i := 0; i < 30; i++ {
		finished := now.Add(-time.Duration(i) * time.Minute)
		started := finished.Add(-time.Hour)
		slot := fmt.Sprintf("done-%02d", i)
		s.Sessions[slot] = &Session{
			IssueNumber: i + 1,
			Status:      StatusDone,
			StartedAt:   started,
			FinishedAt:  &finished,
		}
	}

	res, err := s.CompactSessions(SessionRetentionPolicy{KeepLast: 20, MinAge: 7 * 24 * time.Hour}, now)
	if err != nil {
		t.Fatalf("CompactSessions: %v", err)
	}
	if res.Removed != 0 {
		t.Errorf("Removed=%d, want 0 (age floor protects all)", res.Removed)
	}
	if len(s.Sessions) != 30 {
		t.Errorf("len(Sessions)=%d, want 30", len(s.Sessions))
	}
}

func TestCompactSessions_CountFloorKeepsSmallSet(t *testing.T) {
	// Only 5 done sessions, all 30 days old. Count floor of 20 protects
	// every session even though age floor would otherwise let them be
	// pruned.
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	s := NewState()
	for i := 0; i < 5; i++ {
		finished := now.Add(-30 * 24 * time.Hour).Add(-time.Duration(i) * time.Hour)
		started := finished.Add(-time.Hour)
		slot := fmt.Sprintf("done-%d", i)
		s.Sessions[slot] = &Session{
			IssueNumber: i + 1,
			Status:      StatusDone,
			StartedAt:   started,
			FinishedAt:  &finished,
		}
	}

	res, err := s.CompactSessions(SessionRetentionPolicy{KeepLast: 20, MinAge: 7 * 24 * time.Hour}, now)
	if err != nil {
		t.Fatalf("CompactSessions: %v", err)
	}
	if res.Removed != 0 {
		t.Errorf("Removed=%d, want 0 (count floor protects all)", res.Removed)
	}
	if len(s.Sessions) != 5 {
		t.Errorf("len(Sessions)=%d, want 5", len(s.Sessions))
	}
}

func TestCompactSessions_Idempotent(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	s := NewState()
	for i := 0; i < 25; i++ {
		finished := now.Add(-time.Duration(i+30) * 24 * time.Hour)
		started := finished.Add(-time.Hour)
		slot := fmt.Sprintf("done-%02d", i)
		s.Sessions[slot] = &Session{
			IssueNumber: i + 1,
			Status:      StatusDone,
			StartedAt:   started,
			FinishedAt:  &finished,
		}
	}

	archive := filepath.Join(t.TempDir(), "sessions-archive.jsonl")
	policy := SessionRetentionPolicy{KeepLast: 20, MinAge: 7 * 24 * time.Hour, ArchiveFile: archive}

	first, err := s.CompactSessions(policy, now)
	if err != nil {
		t.Fatalf("CompactSessions first: %v", err)
	}
	if first.Removed != 5 {
		t.Errorf("first Removed=%d, want 5", first.Removed)
	}

	second, err := s.CompactSessions(policy, now)
	if err != nil {
		t.Fatalf("CompactSessions second: %v", err)
	}
	if second.Removed != 0 {
		t.Errorf("second Removed=%d, want 0 (idempotent)", second.Removed)
	}
	if second.Archived != 0 {
		t.Errorf("second Archived=%d, want 0", second.Archived)
	}
}

func TestCompactSessions_AllTerminalStatusesEligible(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	veryOld := now.Add(-90 * 24 * time.Hour)
	statuses := []SessionStatus{
		StatusDone,
		StatusFailed,
		StatusConflictFailed,
		StatusDead,
		StatusRetryExhausted,
		StatusCodeLanded,
	}

	s := NewState()
	for i, status := range statuses {
		finished := veryOld.Add(-time.Duration(i) * time.Hour)
		slot := fmt.Sprintf("slot-%s", status)
		s.Sessions[slot] = &Session{
			IssueNumber: i + 1,
			Status:      status,
			StartedAt:   finished.Add(-time.Hour),
			FinishedAt:  &finished,
		}
	}

	// KeepLast=0 + MinAge=7d: every entry is older than 7d and beyond
	// the count floor → every eligible status is pruned.
	res, err := s.CompactSessions(SessionRetentionPolicy{KeepLast: 0, MinAge: 7 * 24 * time.Hour}, now)
	if err != nil {
		t.Fatalf("CompactSessions: %v", err)
	}
	if res.Removed != len(statuses) {
		t.Errorf("Removed=%d, want %d (all retention-eligible statuses pruned)", res.Removed, len(statuses))
	}
	if len(s.Sessions) != 0 {
		t.Errorf("len(Sessions)=%d, want 0", len(s.Sessions))
	}
}

func TestCompactSessions_NoFinishedAtFallsBackToStartedAt(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	s := NewState()
	s.Sessions["dead-no-finish"] = &Session{
		IssueNumber: 1,
		Status:      StatusDead,
		StartedAt:   now.Add(-30 * 24 * time.Hour),
		FinishedAt:  nil,
	}

	res, err := s.CompactSessions(SessionRetentionPolicy{KeepLast: 0, MinAge: 7 * 24 * time.Hour}, now)
	if err != nil {
		t.Fatalf("CompactSessions: %v", err)
	}
	if res.Removed != 1 {
		t.Errorf("Removed=%d, want 1", res.Removed)
	}
}

func TestCompactSessions_ArchiveDisabled(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	s := NewState()
	finished := now.Add(-30 * 24 * time.Hour)
	s.Sessions["done-1"] = &Session{
		IssueNumber: 1,
		Status:      StatusDone,
		StartedAt:   finished.Add(-time.Hour),
		FinishedAt:  &finished,
	}

	res, err := s.CompactSessions(SessionRetentionPolicy{KeepLast: 0, MinAge: 7 * 24 * time.Hour}, now)
	if err != nil {
		t.Fatalf("CompactSessions: %v", err)
	}
	if res.Removed != 1 {
		t.Errorf("Removed=%d, want 1", res.Removed)
	}
	if res.Archived != 0 {
		t.Errorf("Archived=%d, want 0 (archive disabled)", res.Archived)
	}
}
