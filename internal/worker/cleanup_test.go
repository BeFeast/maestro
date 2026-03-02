package worker

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/state"
)

func TestCleanupWorktrees_RemovesTerminalSessionWorktrees(t *testing.T) {
	// Create fake worktree directories
	tmpDir := t.TempDir()
	wt1 := filepath.Join(tmpDir, "wt1")
	wt2 := filepath.Join(tmpDir, "wt2")
	os.MkdirAll(wt1, 0755)
	os.MkdirAll(wt2, 0755)

	cfg := &config.Config{
		Repo:      "owner/repo",
		LocalPath: tmpDir,
	}

	s := state.NewState()
	s.Sessions["slot-1"] = &state.Session{
		IssueNumber: 100,
		Status:      state.StatusDone,
		Worktree:    wt1,
	}
	s.Sessions["slot-2"] = &state.Session{
		IssueNumber: 101,
		Status:      state.StatusDead,
		Worktree:    wt2,
	}
	// Running session should be skipped
	s.Sessions["slot-3"] = &state.Session{
		IssueNumber: 102,
		Status:      state.StatusRunning,
		Worktree:    filepath.Join(tmpDir, "wt3"),
	}

	results := CleanupWorktrees(cfg, s)

	// Note: actual git worktree remove will fail since these aren't real worktrees,
	// but we verify that the function attempts cleanup on the right sessions.
	if len(results) != 2 {
		t.Fatalf("expected 2 cleanup results, got %d", len(results))
	}

	// Running session should not be touched
	runSess := s.Sessions["slot-3"]
	if runSess.Worktree == "" {
		t.Error("running session worktree should not be cleared")
	}
}

func TestCleanupWorktrees_SkipsAlreadyCleanedSessions(t *testing.T) {
	cfg := &config.Config{
		Repo:      "owner/repo",
		LocalPath: "/tmp",
	}

	s := state.NewState()
	s.Sessions["slot-1"] = &state.Session{
		IssueNumber: 100,
		Status:      state.StatusDone,
		Worktree:    "", // Already cleaned
	}

	results := CleanupWorktrees(cfg, s)

	if len(results) != 0 {
		t.Fatalf("expected 0 cleanup results for already-cleaned sessions, got %d", len(results))
	}
}

func TestCleanupWorktrees_ClearsWorktreeFieldForMissingDirs(t *testing.T) {
	cfg := &config.Config{
		Repo:      "owner/repo",
		LocalPath: "/tmp",
	}

	s := state.NewState()
	s.Sessions["slot-1"] = &state.Session{
		IssueNumber: 100,
		Status:      state.StatusDone,
		Worktree:    "/nonexistent/path/that/does/not/exist",
	}

	results := CleanupWorktrees(cfg, s)

	// Directory doesn't exist, so it should silently clear the field
	if len(results) != 0 {
		t.Fatalf("expected 0 cleanup results for missing dirs, got %d", len(results))
	}
	if s.Sessions["slot-1"].Worktree != "" {
		t.Errorf("Worktree should be cleared for nonexistent directory, got %q", s.Sessions["slot-1"].Worktree)
	}
}

func TestCleanupWorktrees_HandlesAllTerminalStatuses(t *testing.T) {
	tmpDir := t.TempDir()

	terminalStatuses := []state.SessionStatus{
		state.StatusDone,
		state.StatusFailed,
		state.StatusConflictFailed,
		state.StatusDead,
	}

	cfg := &config.Config{
		Repo:      "owner/repo",
		LocalPath: tmpDir,
	}

	s := state.NewState()
	for i, status := range terminalStatuses {
		wt := filepath.Join(tmpDir, fmt.Sprintf("wt-%d", i))
		os.MkdirAll(wt, 0755)
		s.Sessions[fmt.Sprintf("slot-%d", i)] = &state.Session{
			IssueNumber: 100 + i,
			Status:      status,
			Worktree:    wt,
		}
	}

	results := CleanupWorktrees(cfg, s)

	// All 4 terminal sessions should be attempted
	if len(results) != 4 {
		t.Fatalf("expected 4 cleanup results, got %d", len(results))
	}
}
