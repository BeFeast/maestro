package worker

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/github"
	"github.com/befeast/maestro/internal/state"
)

func TestStartOrReconcileTmuxSession_UncertainSpawnAdoptsExactSessionWithoutReplay(t *testing.T) {
	originalSpawn := runTmuxNewSession
	originalRead := readTmuxPaneIdentity
	t.Cleanup(func() {
		runTmuxNewSession = originalSpawn
		readTmuxPaneIdentity = originalRead
	})
	spawnCalls := 0
	runTmuxNewSession = func(tmuxName, worktree, runnerPath string) ([]byte, error) {
		spawnCalls++
		return nil, errors.New("command result uncertain")
	}
	readTmuxPaneIdentity = func(tmuxName string) (int, string, error) {
		return 202, "/worktrees/slot-1", nil
	}

	pid, err := startOrReconcileTmuxSession("maestro-slot-1", "/worktrees/slot-1", "/state/slot-1-run.sh", 101)
	if err != nil || pid != 202 {
		t.Fatalf("reconciled spawn: pid=%d err=%v", pid, err)
	}
	if spawnCalls != 1 {
		t.Fatalf("runner spawn calls = %d, want exactly 1", spawnCalls)
	}
}

func TestStartOrReconcileTmuxSession_RejectsDifferentSessionIdentity(t *testing.T) {
	originalSpawn := runTmuxNewSession
	originalRead := readTmuxPaneIdentity
	t.Cleanup(func() {
		runTmuxNewSession = originalSpawn
		readTmuxPaneIdentity = originalRead
	})
	spawnCalls := 0
	runTmuxNewSession = func(tmuxName, worktree, runnerPath string) ([]byte, error) {
		spawnCalls++
		return nil, errors.New("command result uncertain")
	}
	readTmuxPaneIdentity = func(tmuxName string) (int, string, error) {
		return 303, "/worktrees/different-slot", nil
	}

	if _, err := startOrReconcileTmuxSession("maestro-slot-1", "/worktrees/slot-1", "/state/slot-1-run.sh", 101); err == nil {
		t.Fatal("different tmux/worktree identity was adopted")
	}
	if spawnCalls != 1 {
		t.Fatalf("runner spawn calls = %d, want exactly 1", spawnCalls)
	}
}

func TestStartOrReconcileTmuxSession_ObservesAfterConfirmedSpawnWithoutReplay(t *testing.T) {
	originalSpawn := runTmuxNewSession
	originalRead := readTmuxPaneIdentity
	t.Cleanup(func() {
		runTmuxNewSession = originalSpawn
		readTmuxPaneIdentity = originalRead
	})
	spawnCalls := 0
	readCalls := 0
	runTmuxNewSession = func(tmuxName, worktree, runnerPath string) ([]byte, error) {
		spawnCalls++
		return nil, nil
	}
	readTmuxPaneIdentity = func(tmuxName string) (int, string, error) {
		readCalls++
		if readCalls < 3 {
			return 0, "", errors.New("pane not visible yet")
		}
		return 404, "/worktrees/slot-1", nil
	}

	pid, err := startOrReconcileTmuxSession("maestro-slot-1", "/worktrees/slot-1", "/state/slot-1-run.sh", 0)
	if err != nil || pid != 404 {
		t.Fatalf("observed spawn: pid=%d err=%v", pid, err)
	}
	if spawnCalls != 1 || readCalls != 3 {
		t.Fatalf("spawn/read calls = %d/%d, want 1/3", spawnCalls, readCalls)
	}
}

func TestSaveCheckpoint_WritesFile(t *testing.T) {
	tmpDir := t.TempDir()

	sess := &state.Session{
		IssueNumber:       42,
		Worktree:          tmpDir,
		TokensUsedAttempt: 85000,
		TokensUsedTotal:   120000,
		LogFile:           "",
	}

	cpPath, err := SaveCheckpoint(sess)
	if err != nil {
		t.Fatalf("SaveCheckpoint: %v", err)
	}

	if cpPath != filepath.Join(tmpDir, "CHECKPOINT.md") {
		t.Fatalf("checkpoint path = %q, want %q", cpPath, filepath.Join(tmpDir, "CHECKPOINT.md"))
	}

	data, err := os.ReadFile(cpPath)
	if err != nil {
		t.Fatalf("read checkpoint: %v", err)
	}

	content := string(data)
	if !strings.Contains(content, "# Checkpoint") {
		t.Error("checkpoint missing header")
	}
	if !strings.Contains(content, "Tokens used (attempt): 85000") {
		t.Errorf("checkpoint missing attempt token count, got:\n%s", content)
	}
	if !strings.Contains(content, "Tokens used (total): 120000") {
		t.Errorf("checkpoint missing total token count, got:\n%s", content)
	}
}

func TestSaveCheckpoint_NoWorktree(t *testing.T) {
	sess := &state.Session{
		IssueNumber: 42,
	}

	_, err := SaveCheckpoint(sess)
	if err == nil {
		t.Fatal("expected error for session with no worktree")
	}
}

func TestSaveCheckpoint_WithLogFile(t *testing.T) {
	tmpDir := t.TempDir()

	logFile := filepath.Join(tmpDir, "test.log")
	logContent := "line1\nline2\nline3\nlast line of output\n"
	if err := os.WriteFile(logFile, []byte(logContent), 0644); err != nil {
		t.Fatalf("write log: %v", err)
	}

	sess := &state.Session{
		IssueNumber:       10,
		Worktree:          tmpDir,
		TokensUsedAttempt: 50000,
		LogFile:           logFile,
	}

	cpPath, err := SaveCheckpoint(sess)
	if err != nil {
		t.Fatalf("SaveCheckpoint: %v", err)
	}

	data, err := os.ReadFile(cpPath)
	if err != nil {
		t.Fatalf("read checkpoint: %v", err)
	}

	content := string(data)
	if !strings.Contains(content, "Last worker output") {
		t.Error("checkpoint missing worker output section")
	}
	if !strings.Contains(content, "last line of output") {
		t.Errorf("checkpoint missing log tail, got:\n%s", content)
	}
}

func TestAssemblePromptWithCheckpoint_NoCheckpoint(t *testing.T) {
	iss := github.Issue{Number: 1, Title: "test", Body: "body"}
	cfg := &config.Config{Repo: "owner/repo"}

	result := assemblePromptWithCheckpoint("base prompt", iss, "/tmp/wt", "feat/branch", cfg, "")
	if strings.Contains(result, "Previous Session Checkpoint") {
		t.Error("should not contain checkpoint section when checkpoint is empty")
	}
}

func TestAssemblePromptWithCheckpoint_WithCheckpoint(t *testing.T) {
	iss := github.Issue{Number: 1, Title: "test", Body: "body"}
	cfg := &config.Config{Repo: "owner/repo"}

	checkpoint := "# Checkpoint\nTokens used: 80000\n## Commits made\nabc123 feat: stuff"
	result := assemblePromptWithCheckpoint("base prompt", iss, "/tmp/wt", "feat/branch", cfg, checkpoint)
	if !strings.Contains(result, "Previous Session Checkpoint") {
		t.Error("should contain checkpoint section header")
	}
	if !strings.Contains(result, "abc123 feat: stuff") {
		t.Error("should contain checkpoint content")
	}
	if !strings.Contains(result, "continue where the previous session left off") {
		t.Error("should contain continuation instructions")
	}
}

func TestReadTailLines(t *testing.T) {
	tmpDir := t.TempDir()
	f := filepath.Join(tmpDir, "test.txt")

	content := "line1\nline2\nline3\nline4\nline5\n"
	if err := os.WriteFile(f, []byte(content), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}

	result, err := readTailLines(f, 3)
	if err != nil {
		t.Fatalf("readTailLines: %v", err)
	}

	lines := strings.Split(result, "\n")
	// "line1\nline2\nline3\nline4\nline5\n" splits into ["line1","line2","line3","line4","line5",""]
	// last 3: ["line4", "line5", ""]
	if len(lines) != 3 {
		t.Fatalf("got %d lines, want 3: %v", len(lines), lines)
	}
}
