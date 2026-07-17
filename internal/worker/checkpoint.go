package worker

import (
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/github"
	"github.com/befeast/maestro/internal/state"
	"github.com/befeast/maestro/internal/tmuxsession"
)

// WorktreeDirty reports whether a retained worker worktree contains any
// tracked, staged, or untracked changes. Recovery paths use this before a
// provider transition so completed work is checkpointed instead of being
// discarded by the fresh-worktree Respawn path.
func WorktreeDirty(worktree string) (bool, error) {
	if strings.TrimSpace(worktree) == "" {
		return false, nil
	}
	out, err := exec.Command("git", "-C", worktree, "status", "--porcelain=v1", "--untracked-files=all").CombinedOutput()
	if err != nil {
		return false, fmt.Errorf("inspect worktree before recovery: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)) != "", nil
}

// rotateWorkerAttemptLog preserves the previous attempt while ensuring all
// backend-failure classifiers see only the current attempt. Without this, a
// Fable 429 left in a shared append-only log can be re-read after a successful
// Opus fallback and falsely attributed to Opus during dead-session recovery.
func rotateWorkerAttemptLog(logFile string) error {
	if strings.TrimSpace(logFile) == "" {
		return nil
	}
	suffix := fmt.Sprintf(".attempt-%d", time.Now().UTC().UnixNano())
	for _, path := range []string{logFile, logFile + ".jsonl"} {
		if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return fmt.Errorf("stat previous attempt log %s: %w", path, err)
		}
		if err := os.Rename(path, path+suffix); err != nil {
			return fmt.Errorf("rotate previous attempt log %s: %w", path, err)
		}
	}
	return nil
}

// SaveCheckpoint captures the worker's progress and writes a CHECKPOINT.md
// file to the worktree. Returns the path to the checkpoint file.
func SaveCheckpoint(sess *state.Session) (string, error) {
	if sess.Worktree == "" {
		return "", fmt.Errorf("session has no worktree")
	}

	var sections []string
	sections = append(sections, "# Checkpoint")
	sections = append(sections, fmt.Sprintf("\nSaved at: %s", time.Now().UTC().Format(time.RFC3339)))
	sections = append(sections, fmt.Sprintf("Tokens used (attempt): %d", sess.TokensUsedAttempt))
	sections = append(sections, fmt.Sprintf("Tokens used (total): %d", sess.TokensUsedTotal))

	// Capture branch commits (relative to origin/main)
	if out, err := exec.Command("git", "-C", sess.Worktree,
		"log", "--oneline", "origin/main..HEAD").CombinedOutput(); err == nil {
		commits := strings.TrimSpace(string(out))
		if commits != "" {
			sections = append(sections, "\n## Commits made\n```\n"+commits+"\n```")
		}
	}

	// Capture diff stat for uncommitted changes
	if out, err := exec.Command("git", "-C", sess.Worktree,
		"diff", "--stat").CombinedOutput(); err == nil {
		diff := strings.TrimSpace(string(out))
		if diff != "" {
			sections = append(sections, "\n## Uncommitted changes\n```\n"+diff+"\n```")
		}
	}

	// Capture staged changes stat
	if out, err := exec.Command("git", "-C", sess.Worktree,
		"diff", "--cached", "--stat").CombinedOutput(); err == nil {
		staged := strings.TrimSpace(string(out))
		if staged != "" {
			sections = append(sections, "\n## Staged changes\n```\n"+staged+"\n```")
		}
	}

	// Read last 30 lines of log for context
	if sess.LogFile != "" {
		if tail, err := readTailLines(sess.LogFile, 30); err == nil && tail != "" {
			sections = append(sections, "\n## Last worker output\n```\n"+tail+"\n```")
		}
	}

	content := strings.Join(sections, "\n")
	checkpointPath := filepath.Join(sess.Worktree, "CHECKPOINT.md")
	if err := os.WriteFile(checkpointPath, []byte(content+"\n"), 0644); err != nil {
		return "", fmt.Errorf("write checkpoint: %w", err)
	}

	log.Printf("[worker] checkpoint saved to %s (%d bytes)", checkpointPath, len(content))
	return checkpointPath, nil
}

// RespawnInPlace stops the current worker and restarts it in the same worktree
// with checkpoint context included in the prompt. Unlike Respawn, this preserves
// the existing worktree with all committed and staged code.
func RespawnInPlace(cfg *config.Config, slotName string, sess *state.Session, repo string, issue github.Issue, promptBase string, backendName string) error {
	// Kill tmux session + process subtree (but do NOT remove worktree). Reaping
	// the whole descendant tree — not just the recorded pane PID — ensures
	// worker grandchildren that reparented away (e.g. headless Chrome) do not
	// leak across a respawn.
	tmuxName := TmuxSessionName(slotName)
	if out, err := tmuxsession.KillSession(tmuxName); err != nil {
		log.Printf("[worker] tmux kill-session %s: %v (%s)", tmuxName, err, strings.TrimSpace(string(out)))
	}
	if sess.PID > 0 && IsAlive(sess.PID) {
		KillProcessTree(sess.PID)
	}

	// Run after_run hook (non-fatal)
	if cfg.Hooks.AfterRun != "" {
		hookEnv := HookEnv{
			IssueID:       fmt.Sprintf("%s#%d", repo, issue.Number),
			IssueNumber:   issue.Number,
			WorkspacePath: sess.Worktree,
		}
		if err := RunHook(cfg, "after_run", cfg.Hooks.AfterRun, hookEnv); err != nil {
			log.Printf("[worker] after_run hook failed: %v", err)
		}
	}

	// Read checkpoint content if it exists
	checkpointContext := ""
	if sess.CheckpointFile != "" {
		if data, err := os.ReadFile(sess.CheckpointFile); err == nil {
			checkpointContext = sanitizePromptUTF8(string(data))
		}
	}

	// Determine backend
	if backendName == "" {
		backendName = cfg.Model.Default
	}
	backendDef, ok := cfg.Model.Backends[backendName]
	if !ok {
		backendName = cfg.Model.Default
		backendDef, ok = cfg.Model.Backends[backendName]
		if !ok {
			return fmt.Errorf("backend %q (default) not found in config", backendName)
		}
	}
	backendCfg := workerBackendConfig(backendDef)
	backendCfg.TokenBudget = cfg.WorkerMaxTokens
	if err := validateLiveTokenBudget(backendName, backendCfg); err != nil {
		return err
	}

	hookSetup, err := setupWorkerToolHooks(cfg.StateDir, sess.Worktree, resolveBackendKind(backendName, backendCfg), cfg.Hooks)
	if err != nil {
		return fmt.Errorf("setup worker tool hooks: %w", err)
	}

	// Assemble prompt with checkpoint
	prompt := assemblePromptWithCheckpoint(promptBase, issue, sess.Worktree, sess.Branch, cfg, checkpointContext)
	prompt += subagentHintPromptSection(backendDef.SubagentHint)
	prompt += workerToolHookPromptSection(cfg.Hooks, backendName, hookSetup)

	// Write prompt to file
	promptFile := filepath.Join(cfg.StateDir, fmt.Sprintf("%s-prompt.md", slotName))
	if err := writePromptFile(promptFile, prompt); err != nil {
		return fmt.Errorf("write prompt file: %w", err)
	}

	// Prepare log file
	logDir := state.LogDir(cfg.StateDir)
	if err := ensureWorkerLogDir(logDir); err != nil {
		return fmt.Errorf("create log dir: %w", err)
	}
	logFile := filepath.Join(logDir, slotName+".log")
	if err := rotateWorkerAttemptLog(logFile); err != nil {
		return err
	}

	// Build the worker command
	workerCmd, stdinFile, err := BuildWorkerCmd(backendName, backendCfg, promptFile, sess.Worktree)
	if err != nil {
		return fmt.Errorf("build worker cmd: %w", err)
	}

	// Write runner script
	runnerPath := filepath.Join(cfg.StateDir, slotName+"-run.sh")
	split := streamSplitForBackend(backendName, backendCfg, logFile)
	if err := writeWorkerRunnerScript(cfg.StateDir, runnerPath, workerCmd.Args, stdinFile, logFile, sess.Worktree, split); err != nil {
		return err
	}

	// Run before_run hook (fatal on failure)
	hookEnv := HookEnv{
		IssueID:       fmt.Sprintf("%s#%d", repo, issue.Number),
		IssueNumber:   issue.Number,
		WorkspacePath: sess.Worktree,
	}
	if err := RunHook(cfg, "before_run", cfg.Hooks.BeforeRun, hookEnv); err != nil {
		return fmt.Errorf("before_run hook: %w", err)
	}

	// Start tmux session in existing worktree
	if tmuxOut, err := tmuxsession.StartDetached(tmuxName, sess.Worktree, runnerPath); err != nil {
		return fmt.Errorf("tmux new-session: %w\n%s", err, tmuxOut)
	}

	// Get PID
	pidOut, err := tmuxsession.PanePID(tmuxName)
	if err != nil {
		return fmt.Errorf("tmux list-panes: %w", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(pidOut)))
	if err != nil {
		return fmt.Errorf("parse pane pid: %w", err)
	}

	log.Printf("[worker] respawned in-place %s in tmux session %s (pane_pid=%d, log=%s)", slotName, tmuxName, pid, logFile)

	// Update session — keep worktree and branch, reset runtime fields
	sess.PID = pid
	sess.TmuxSession = tmuxName
	sess.LogFile = logFile
	// #513/#931: start a new attempt projection while preserving the same
	// worktree/session identity and cumulative attribution history.
	beginSessionAttempt(cfg, sess, backendName, "in_place_respawn", "in_place_respawn", time.Now())
	sess.NotifiedCIFail = false
	sess.LastNotifiedStatus = ""
	sess.LastOutputHash = ""
	sess.LastOutputChangedAt = time.Time{}

	return nil
}

// assemblePromptWithCheckpoint builds a prompt that includes checkpoint context
// from a previous worker session that hit the soft token threshold.
func assemblePromptWithCheckpoint(base string, issue github.Issue, worktreePath, branchName string, cfg *config.Config, checkpoint string) string {
	prompt := assemblePrompt(base, issue, worktreePath, branchName, cfg)
	if checkpoint == "" {
		return prompt
	}

	return prompt + fmt.Sprintf(`

---

## Previous Session Checkpoint

This task was previously worked on by another agent session that ran out of token budget.
The worktree already contains code changes from the previous session. Review the checkpoint
below, examine the existing code changes, and continue where the previous session left off.
Do NOT redo work that is already done — focus on what remains.

%s
`, checkpoint)
}

// readTailLines reads the last n lines from a file.
func readTailLines(path string, n int) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	lines := strings.Split(string(data), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n"), nil
}
