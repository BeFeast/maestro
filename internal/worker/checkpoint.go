package worker

import (
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
)

const checkpointFile = "CHECKPOINT.md"

// SaveCheckpoint commits any pending work in the worktree and writes a
// CHECKPOINT.md file summarizing the progress. Returns the checkpoint
// content that was written.
func SaveCheckpoint(worktreePath string, tokensUsed int) (string, error) {
	// Stage all changes
	if out, err := exec.Command("git", "-C", worktreePath, "add", "-A").CombinedOutput(); err != nil {
		log.Printf("[checkpoint] git add failed: %v\n%s", err, out)
		// Non-fatal: there may be nothing to stage
	}

	// Get a summary of what's been done (staged diff stat)
	diffStat, _ := exec.Command("git", "-C", worktreePath, "diff", "--cached", "--stat").CombinedOutput()

	// Get list of commits on this branch that aren't on main
	commitLog, _ := exec.Command("git", "-C", worktreePath, "log", "--oneline", "origin/main..HEAD").CombinedOutput()

	// Build checkpoint content
	checkpoint := fmt.Sprintf(`# Checkpoint

This checkpoint was automatically created when the worker reached the soft token threshold.
The previous session used approximately %d tokens.

## Commits so far
%s

## Staged changes (diff stat)
%s

## Instructions for continuation

You are continuing work on this issue. The previous worker session made the progress
described above before being checkpointed. Review the existing code changes, read the
issue description, and continue where the previous session left off.

Do NOT redo work that has already been completed. Focus on what remains.
`, tokensUsed, strings.TrimSpace(string(commitLog)), strings.TrimSpace(string(diffStat)))

	// Write CHECKPOINT.md
	cpPath := filepath.Join(worktreePath, checkpointFile)
	if err := os.WriteFile(cpPath, []byte(checkpoint), 0644); err != nil {
		return "", fmt.Errorf("write %s: %w", checkpointFile, err)
	}

	// Commit everything including CHECKPOINT.md
	exec.Command("git", "-C", worktreePath, "add", "-A").CombinedOutput()
	if out, err := exec.Command("git", "-C", worktreePath,
		"commit", "-m", "checkpoint: save progress before token refresh").CombinedOutput(); err != nil {
		log.Printf("[checkpoint] git commit: %v\n%s", err, out)
		// Non-fatal: maybe nothing to commit
	}

	return checkpoint, nil
}

// RespawnWithCheckpoint saves a checkpoint in the current worktree, stops the
// worker, and restarts it on the same branch with the checkpoint context
// included in the prompt.
func RespawnWithCheckpoint(cfg *config.Config, slotName string, sess *state.Session, repo string, issue github.Issue, promptBase string, backendName string) error {
	// Save checkpoint in current worktree before tearing it down
	checkpoint, err := SaveCheckpoint(sess.Worktree, sess.TokensUsed)
	if err != nil {
		log.Printf("[checkpoint] save failed for %s: %v — proceeding with respawn anyway", slotName, err)
	}

	oldBranch := sess.Branch

	// Kill the old worker's tmux session (but don't remove worktree yet)
	tmuxName := TmuxSessionName(slotName)
	if out, err := exec.Command("tmux", "kill-session", "-t", tmuxName).CombinedOutput(); err != nil {
		log.Printf("[checkpoint] tmux kill-session %s: %v (%s)", tmuxName, err, strings.TrimSpace(string(out)))
	}
	if sess.PID > 0 && IsAlive(sess.PID) {
		if proc, err := os.FindProcess(sess.PID); err == nil {
			proc.Kill()
		}
	}

	// Run before_remove hook
	if sess.Worktree != "" {
		hookEnv := HookEnv{
			IssueID:       fmt.Sprintf("%s#%d", cfg.Repo, sess.IssueNumber),
			IssueNumber:   sess.IssueNumber,
			WorkspacePath: sess.Worktree,
		}
		if err := RunHook(cfg, "before_remove", cfg.Hooks.BeforeRemove, hookEnv); err != nil {
			log.Printf("[checkpoint] before_remove hook failed: %v", err)
		}
	}

	// Remove old worktree (branch and commits are preserved in repo)
	if sess.Worktree != "" {
		out, err := exec.Command("git", "-C", cfg.LocalPath,
			"worktree", "remove", "--force", sess.Worktree).CombinedOutput()
		if err != nil {
			log.Printf("[checkpoint] remove worktree %s: %v\n%s", sess.Worktree, err, out)
		}
	}

	// Create new worktree from the EXISTING branch (no -b flag)
	worktreePath := filepath.Join(cfg.WorktreeBase, slotName)
	log.Printf("[checkpoint] creating worktree %s from existing branch %s", worktreePath, oldBranch)
	out, err := exec.Command("git", "-C", cfg.LocalPath,
		"worktree", "add", worktreePath, oldBranch).CombinedOutput()
	if err != nil {
		return fmt.Errorf("git worktree add (checkpoint): %w\n%s", err, out)
	}

	// Run after_create hook
	hookEnv := HookEnv{
		IssueID:       fmt.Sprintf("%s#%d", cfg.Repo, issue.Number),
		IssueNumber:   issue.Number,
		WorkspacePath: worktreePath,
	}
	if err := RunHook(cfg, "after_create", cfg.Hooks.AfterCreate, hookEnv); err != nil {
		log.Printf("[checkpoint] after_create hook failed: %v", err)
	}

	// Build prompt with checkpoint context
	prompt := assemblePrompt(promptBase, issue, worktreePath, oldBranch, cfg)
	if checkpoint != "" {
		prompt = prompt + "\n\n---\n\n## Checkpoint from previous session\n\n" + checkpoint +
			"\n\nIMPORTANT: Read CHECKPOINT.md in your worktree for full context. " +
			"Continue where the previous session left off. Do NOT redo completed work.\n"
	}

	// Write prompt to file
	promptFile := filepath.Join(cfg.StateDir, fmt.Sprintf("%s-prompt.md", slotName))
	if err := os.WriteFile(promptFile, []byte(prompt), 0644); err != nil {
		return fmt.Errorf("write prompt file: %w", err)
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
	backendCfg := BackendConfig{
		Cmd:        backendDef.Cmd,
		ExtraArgs:  backendDef.ExtraArgs,
		PromptMode: backendDef.PromptMode,
	}

	// Prepare log file
	logDir := state.LogDir(cfg.StateDir)
	if err := os.MkdirAll(logDir, 0755); err != nil {
		return fmt.Errorf("create log dir: %w", err)
	}
	logFile := filepath.Join(logDir, slotName+".log")

	// Build the worker command
	workerCmd, stdinFile, err := BuildWorkerCmd(backendName, backendCfg, promptFile, worktreePath)
	if err != nil {
		return fmt.Errorf("build worker cmd: %w", err)
	}

	// Write runner script
	runnerPath := filepath.Join(cfg.StateDir, slotName+"-run.sh")
	var runnerContent string
	if stdinFile != "" {
		runnerContent = fmt.Sprintf("#!/bin/bash\nexec %s < %q 2>&1 | tee -a %q\n",
			shellJoin(workerCmd.Args), stdinFile, logFile)
	} else {
		runnerContent = fmt.Sprintf("#!/bin/bash\nexec %s 2>&1 | tee -a %q\n",
			shellJoin(workerCmd.Args), logFile)
	}
	if err := os.WriteFile(runnerPath, []byte(runnerContent), 0755); err != nil {
		return fmt.Errorf("write runner script: %w", err)
	}

	// Run before_run hook
	if err := RunHook(cfg, "before_run", cfg.Hooks.BeforeRun, hookEnv); err != nil {
		return fmt.Errorf("before_run hook: %w", err)
	}

	// Start tmux session
	tmuxName = TmuxSessionName(slotName)
	tmuxCmd := exec.Command("tmux", "new-session", "-d", "-s", tmuxName, "-c", worktreePath, "bash", runnerPath)
	if tmuxOut, err := tmuxCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("tmux new-session: %w\n%s", err, tmuxOut)
	}

	// Get PID
	pidOut, err := exec.Command("tmux", "list-panes", "-t", tmuxName, "-F", "#{pane_pid}").Output()
	if err != nil {
		return fmt.Errorf("tmux list-panes: %w", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(pidOut)))
	if err != nil {
		return fmt.Errorf("parse pane pid: %w", err)
	}

	log.Printf("[checkpoint] respawned %s with checkpoint (pane_pid=%d, branch=%s)", slotName, pid, oldBranch)

	// Update session in place — reset token counters for fresh budget
	sess.Worktree = worktreePath
	sess.Branch = oldBranch
	sess.PID = pid
	sess.TmuxSession = tmuxName
	sess.LogFile = logFile
	sess.StartedAt = time.Now().UTC()
	sess.FinishedAt = nil
	sess.Status = state.StatusRunning
	sess.Backend = backendName
	sess.NotifiedCIFail = false
	sess.LastNotifiedStatus = ""
	sess.LastOutputHash = ""
	sess.LastOutputChangedAt = time.Time{}
	sess.TokensUsed = 0
	sess.SoftThresholdNotified = false

	return nil
}

// ReadCheckpoint reads the CHECKPOINT.md file from a worktree, if it exists.
// Returns empty string if no checkpoint file is found.
func ReadCheckpoint(worktreePath string) string {
	data, err := os.ReadFile(filepath.Join(worktreePath, checkpointFile))
	if err != nil {
		return ""
	}
	return string(data)
}
