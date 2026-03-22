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
	// Kill tmux session + PID (but do NOT remove worktree)
	tmuxName := TmuxSessionName(slotName)
	if out, err := exec.Command("tmux", "kill-session", "-t", tmuxName).CombinedOutput(); err != nil {
		log.Printf("[worker] tmux kill-session %s: %v (%s)", tmuxName, err, strings.TrimSpace(string(out)))
	}
	if sess.PID > 0 && IsAlive(sess.PID) {
		proc, _ := os.FindProcess(sess.PID)
		if err := proc.Kill(); err != nil {
			log.Printf("[worker] kill pid %d: %v", sess.PID, err)
		}
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
			checkpointContext = string(data)
		}
	}

	// Assemble prompt with checkpoint
	prompt := assemblePromptWithCheckpoint(promptBase, issue, sess.Worktree, sess.Branch, cfg, checkpointContext)

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
	workerCmd, stdinFile, err := BuildWorkerCmd(backendName, backendCfg, promptFile, sess.Worktree)
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
	tmuxCmd := exec.Command("tmux", "new-session", "-d", "-s", tmuxName, "-c", sess.Worktree, "bash", runnerPath)
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

	log.Printf("[worker] respawned in-place %s in tmux session %s (pane_pid=%d, log=%s)", slotName, tmuxName, pid, logFile)

	// Update session — keep worktree and branch, reset runtime fields
	sess.PID = pid
	sess.TmuxSession = tmuxName
	sess.LogFile = logFile
	sess.StartedAt = time.Now().UTC()
	sess.FinishedAt = nil
	sess.Status = state.StatusRunning
	sess.Backend = backendName
	sess.TokensUsedAttempt = 0
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
