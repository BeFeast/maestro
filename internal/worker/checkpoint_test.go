package worker

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/github"
	"github.com/befeast/maestro/internal/state"
)

func TestBeginSessionAttemptClearsPriorProjectionAndPreservesHistory(t *testing.T) {
	ended := time.Date(2026, 7, 17, 11, 5, 0, 0, time.UTC)
	started := ended.Add(time.Hour)
	sess := &state.Session{
		IssueNumber:          346,
		PRNumber:             335,
		Worktree:             "/tmp/kept-worktree",
		Branch:               "feat/kept-branch",
		Backend:              "claude",
		Model:                "claude-fable-5",
		Status:               state.StatusDead,
		FinishedAt:           &ended,
		WorkerEndedAt:        &ended,
		CostUSDBackend:       1.25,
		UsageTokensWatermark: 1_730_413,
		TokensUsedAttempt:    1_730_413,
		TokensUsedTotal:      1_730_413,
		WorkerOutcome:        "failed",
		Attribution: []state.BackendAttribution{{
			Backend: "claude", Model: "claude-fable-5", StartedAt: ended.Add(-time.Minute),
		}},
	}
	cfg := &config.Config{Model: config.ModelConfig{Backends: map[string]config.BackendDef{
		"sol": {Provider: "openai", Model: "gpt-5.6-sol", Effort: "high"},
	}}}

	beginSessionAttempt(cfg, sess, "sol", "in_place_respawn", "in_place_respawn", started)

	if sess.Status != state.StatusRunning || sess.Backend != "sol" || !sess.StartedAt.Equal(started) {
		t.Fatalf("live attempt = status %q backend %q start %v", sess.Status, sess.Backend, sess.StartedAt)
	}
	if sess.FinishedAt != nil || sess.WorkerEndedAt != nil || sess.Model != "" || sess.CostUSDBackend != 0 {
		t.Fatalf("stale projection retained: finished=%v ended=%v model=%q cost=%v", sess.FinishedAt, sess.WorkerEndedAt, sess.Model, sess.CostUSDBackend)
	}
	if sess.TokensUsedAttempt != 0 || sess.UsageTokensWatermark != 0 || sess.WorkerOutcome != "" {
		t.Fatalf("attempt counters not reset: attempt=%d watermark=%d outcome=%q", sess.TokensUsedAttempt, sess.UsageTokensWatermark, sess.WorkerOutcome)
	}
	if sess.TokensUsedTotal != 1_730_413 || sess.Worktree != "/tmp/kept-worktree" || sess.Branch != "feat/kept-branch" || sess.PRNumber != 335 {
		t.Fatalf("cumulative/session identity changed: total=%d worktree=%q branch=%q PR=%d", sess.TokensUsedTotal, sess.Worktree, sess.Branch, sess.PRNumber)
	}
	if len(sess.Attribution) != 2 || sess.Attribution[0].EndedAt == nil || sess.Attribution[1].Backend != "sol" || sess.Attribution[1].Model != "gpt-5.6-sol" || sess.Attribution[1].EndedAt != nil {
		t.Fatalf("attribution = %+v", sess.Attribution)
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
