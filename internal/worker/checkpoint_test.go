package worker

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/github"
	"github.com/befeast/maestro/internal/state"
	"github.com/befeast/maestro/internal/tmuxsession"
)

var checkpointTestLease = tmuxsession.ProcessLease{
	Unit:    "maestro-worker-0123456789abcdef0123456789abcdef-g1.scope",
	Manager: tmuxsession.ProcessLeaseManagerUser,
}

func TestStartOrReconcileTmuxSession_UncertainSpawnAdoptsExactSessionWithoutReplay(t *testing.T) {
	originalSpawn := runTmuxNewSession
	originalRead := readTmuxPaneIdentity
	t.Cleanup(func() {
		runTmuxNewSession = originalSpawn
		readTmuxPaneIdentity = originalRead
	})
	spawnCalls := 0
	runTmuxNewSession = func(tmuxName, worktree, runnerPath string, lease tmuxsession.ProcessLease) ([]byte, error) {
		spawnCalls++
		return nil, errors.New("command result uncertain")
	}
	readTmuxPaneIdentity = func(tmuxName string) (int, string, error) {
		return 202, "/worktrees/slot-1", nil
	}

	pid, err := startOrReconcileTmuxSession("maestro-slot-1", "/worktrees/slot-1", "/state/slot-1-run.sh", checkpointTestLease, 101)
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
	runTmuxNewSession = func(tmuxName, worktree, runnerPath string, lease tmuxsession.ProcessLease) ([]byte, error) {
		spawnCalls++
		return nil, errors.New("command result uncertain")
	}
	readTmuxPaneIdentity = func(tmuxName string) (int, string, error) {
		return 303, "/worktrees/different-slot", nil
	}

	if _, err := startOrReconcileTmuxSession("maestro-slot-1", "/worktrees/slot-1", "/state/slot-1-run.sh", checkpointTestLease, 101); err == nil {
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
	runTmuxNewSession = func(tmuxName, worktree, runnerPath string, lease tmuxsession.ProcessLease) ([]byte, error) {
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

	pid, err := startOrReconcileTmuxSession("maestro-slot-1", "/worktrees/slot-1", "/state/slot-1-run.sh", checkpointTestLease, 0)
	if err != nil || pid != 404 {
		t.Fatalf("observed spawn: pid=%d err=%v", pid, err)
	}
	if spawnCalls != 1 || readCalls != 3 {
		t.Fatalf("spawn/read calls = %d/%d, want 1/3", spawnCalls, readCalls)
	}
}

func TestRestoreMissingWorktreePreservesExistingBranchHead(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	base := filepath.Join(root, "worktrees")
	if out, err := exec.Command("git", "init", "-b", "main", repo).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	for _, args := range [][]string{
		{"-C", repo, "config", "user.email", "test@example.com"},
		{"-C", repo, "config", "user.name", "Test"},
	} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(repo, "base.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"-C", repo, "add", "base.txt"}, {"-C", repo, "commit", "-m", "base"}, {"-C", repo, "branch", "feat/ok-player-277-346"}} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	want, err := exec.Command("git", "-C", repo, "rev-parse", "refs/heads/feat/ok-player-277-346").Output()
	if err != nil {
		t.Fatal(err)
	}
	worktree := filepath.Join(base, "ok-player-277")
	if err := RestoreMissingWorktree(repo, base, "ok-player-277", worktree, "feat/ok-player-277-346"); err != nil {
		t.Fatalf("RestoreMissingWorktree: %v", err)
	}
	got, err := exec.Command("git", "-C", worktree, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(got)) != strings.TrimSpace(string(want)) {
		t.Fatalf("restored HEAD = %s, want %s", got, want)
	}
}

func TestRestoreMissingWorktreeRejectsNonCanonicalPath(t *testing.T) {
	err := RestoreMissingWorktree(t.TempDir(), t.TempDir(), "ok-player-277", filepath.Join(t.TempDir(), "other"), "feat/branch")
	if err == nil || !strings.Contains(err.Error(), "not deterministic slot path") {
		t.Fatalf("error = %v, want deterministic path rejection", err)
	}
}

func TestRestoreMissingWorktreePreservesOrphanedDirectoryAndRecreatesCheckout(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	base := filepath.Join(root, "worktrees")
	runBranchGit(t, root, "init", "-b", "main", repo)
	runBranchGit(t, repo, "config", "user.email", "test@example.com")
	runBranchGit(t, repo, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(repo, "base.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runBranchGit(t, repo, "add", "base.txt")
	runBranchGit(t, repo, "commit", "-m", "base")
	runBranchGit(t, repo, "branch", "feat/canonical")

	worktree := filepath.Join(base, "ok-player-277")
	if err := os.MkdirAll(worktree, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worktree, "preserved-artifact.txt"), []byte("keep me\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := RestoreMissingWorktree(repo, base, "ok-player-277", worktree, "feat/canonical"); err != nil {
		t.Fatalf("RestoreMissingWorktree: %v", err)
	}
	if got := strings.TrimSpace(runBranchGit(t, worktree, "branch", "--show-current")); got != "feat/canonical" {
		t.Fatalf("restored branch = %q, want feat/canonical", got)
	}
	backups, err := filepath.Glob(worktree + ".orphaned-*")
	if err != nil || len(backups) != 1 {
		t.Fatalf("orphan backups = %v, %v; want exactly one", backups, err)
	}
	if got, err := os.ReadFile(filepath.Join(backups[0], "preserved-artifact.txt")); err != nil || string(got) != "keep me\n" {
		t.Fatalf("orphaned content was not preserved: %q, %v", got, err)
	}
}

func TestGitWorktreeUsabilityRequiresExactLinkedCheckout(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	runBranchGit(t, root, "init", "-b", "main", repo)
	runBranchGit(t, repo, "config", "user.email", "test@example.com")
	runBranchGit(t, repo, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(repo, "base.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runBranchGit(t, repo, "add", "base.txt")
	runBranchGit(t, repo, "commit", "-m", "base")
	runBranchGit(t, repo, "branch", "feat/valid")

	valid := filepath.Join(root, "valid")
	runBranchGit(t, repo, "worktree", "add", valid, "feat/valid")
	if !isGitWorktreeForRepo(repo, valid) {
		t.Fatal("linked worktree was not recognized as usable")
	}

	outer := filepath.Join(root, "outer")
	runBranchGit(t, root, "init", "-b", "main", outer)
	nested := filepath.Join(outer, "worktrees", "slot")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	if isGitWorktreeForRepo(repo, nested) {
		t.Fatal("nested directory inherited parent repository usability")
	}

	unrelated := filepath.Join(root, "unrelated")
	runBranchGit(t, root, "init", "-b", "main", unrelated)
	if isGitWorktreeForRepo(repo, unrelated) {
		t.Fatal("unrelated repository was accepted as the canonical checkout")
	}
}

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

	result := assemblePromptWithCheckpoint("base prompt", iss, "/tmp/wt", "feat/branch", cfg, "", "")
	if strings.Contains(result, "Previous Session Checkpoint") {
		t.Error("should not contain checkpoint section when checkpoint is empty")
	}
}

func TestAssemblePromptWithCheckpoint_WithCheckpoint(t *testing.T) {
	iss := github.Issue{Number: 1, Title: "test", Body: "body"}
	cfg := &config.Config{Repo: "owner/repo"}

	checkpoint := "# Checkpoint\nTokens used: 80000\n## Commits made\nabc123 feat: stuff"
	result := assemblePromptWithCheckpoint("base prompt", iss, "/tmp/wt", "feat/branch", cfg, checkpoint, "/state/slot-1/CHECKPOINT.md")
	if !strings.Contains(result, "Previous Session Checkpoint") {
		t.Error("should contain checkpoint section header")
	}
	if !strings.Contains(result, "abc123 feat: stuff") {
		t.Error("should contain checkpoint content")
	}
	if !strings.Contains(result, "Checkpoint source: /state/slot-1/CHECKPOINT.md") {
		t.Error("should record the checkpoint source")
	}
	if !strings.Contains(result, "AUTHORITATIVE (revision "+continuationRevision("base prompt")) {
		t.Error("should record the fresh continuation revision behind a precedence marker")
	}
}

// TestAssemblePromptWithCheckpoint_FreshPayloadOutranksStaleCheckpoint is the
// #973 regression: a completed prior worker log ending in a terminal stop message
// must not appear as an authoritative instruction, and the fresh continuation
// payload must be emitted after the checkpoint behind an explicit precedence
// marker so it wins recency.
func TestAssemblePromptWithCheckpoint_FreshPayloadOutranksStaleCheckpoint(t *testing.T) {
	iss := github.Issue{Number: 973, Title: "continuation", Body: "add tests, amend push"}
	cfg := &config.Config{Repo: "owner/repo"}

	checkpoint := strings.Join([]string{
		"# Checkpoint",
		"## Last worker output",
		"PR already opened. Ready for review — stopping as instructed.",
		"All done, nothing more to do.",
	}, "\n")
	freshBase := "MAESTRO_FRESH_CONTINUATION_MARKER: add the missing regression test and amend the push"

	result := assemblePromptWithCheckpoint(freshBase, iss, "/tmp/wt", "feat/branch", cfg, checkpoint, "/state/CHECKPOINT.md")

	cpIdx := strings.Index(result, "Previous Session Checkpoint")
	freshIdx := strings.Index(result, "MAESTRO_FRESH_CONTINUATION_MARKER")
	precIdx := strings.Index(result, "Current continuation requirements — AUTHORITATIVE")
	if cpIdx < 0 || freshIdx < 0 || precIdx < 0 {
		t.Fatalf("missing sections: checkpoint=%d fresh=%d precedence=%d", cpIdx, freshIdx, precIdx)
	}
	// Ordering: checkpoint context, then precedence marker, then fresh payload.
	if !(cpIdx < precIdx && precIdx < freshIdx) {
		t.Fatalf("ordering wrong: checkpoint=%d precedence=%d fresh=%d (want checkpoint < precedence < fresh)", cpIdx, precIdx, freshIdx)
	}
	// The stale terminal sign-off lines must be annotated as superseded, never
	// left as bare authoritative instructions.
	for _, stale := range []string{
		"PR already opened. Ready for review — stopping as instructed.",
		"All done, nothing more to do.",
	} {
		marked := supersededDirectiveMarker + stale
		if !strings.Contains(result, marked) {
			t.Errorf("stale terminal line not annotated as superseded:\n%q", stale)
		}
	}
}

func TestSanitizeCheckpointTerminalDirectives(t *testing.T) {
	neutralized := []string{
		"PR already opened, stopping.",
		"The pull request has already been opened.",
		"Ready for review.",
		"Stopping as instructed by the task.",
		"You are done — exiting now.",
		"All done, nothing left to do.",
		"Task complete.",
		"I'll stop now.",
	}
	for _, line := range neutralized {
		got := sanitizeCheckpointTerminalDirectives(line)
		if !strings.HasPrefix(got, supersededDirectiveMarker) {
			t.Errorf("terminal directive not annotated: %q -> %q", line, got)
		}
	}

	preserved := []string{
		"Refactored the token accounting helper.",
		"Added a regression test for the parser.",
		"Committed abc123 with the fix.",
	}
	for _, line := range preserved {
		if got := sanitizeCheckpointTerminalDirectives(line); got != line {
			t.Errorf("benign line altered: %q -> %q", line, got)
		}
	}

	if got := sanitizeCheckpointTerminalDirectives(""); got != "" {
		t.Errorf("empty checkpoint should stay empty, got %q", got)
	}
}

// TestAssemblePromptWithCheckpoint_TokenBudgetRecoveryPreserved covers acceptance
// #5: a genuine token-budget respawn still carries its checkpoint context and the
// guidance to skip already-committed work.
func TestAssemblePromptWithCheckpoint_TokenBudgetRecoveryPreserved(t *testing.T) {
	iss := github.Issue{Number: 42, Title: "big feature", Body: "implement it"}
	cfg := &config.Config{Repo: "owner/repo"}

	checkpoint := "# Checkpoint\nTokens used (attempt): 190000\n## Commits made\n```\ndef456 feat: partial work\n```"
	result := assemblePromptWithCheckpoint("continue the feature", iss, "/tmp/wt", "feat/branch", cfg, checkpoint, "/state/CHECKPOINT.md")

	for _, want := range []string{
		"def456 feat: partial work",
		"avoid redoing",
		"same worktree and the same",
	} {
		if !strings.Contains(result, want) {
			t.Errorf("token-budget recovery prompt missing %q", want)
		}
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
