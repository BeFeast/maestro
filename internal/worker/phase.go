package worker

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/state"
)

// StartPhase launches a new worker session for a pipeline phase in an existing worktree.
// Unlike Start, this does NOT create a new worktree or branch — it reuses the session's
// existing workspace. The session is updated in place with a new PID and status.
func StartPhase(cfg *config.Config, sess *state.Session, slotName, prompt, backendName string) error {
	if sess.Worktree == "" {
		return fmt.Errorf("session %s has no worktree", slotName)
	}

	// Kill any leftover tmux session from the previous phase
	tmuxName := TmuxSessionName(slotName)
	exec.Command("tmux", "kill-session", "-t", tmuxName).CombinedOutput()

	// Write prompt to file
	promptFile := fmt.Sprintf("%s/%s-prompt.md", cfg.StateDir, slotName)
	if err := os.WriteFile(promptFile, []byte(prompt), 0644); err != nil {
		return fmt.Errorf("write prompt file: %w", err)
	}

	// Determine backend
	if backendName == "" {
		backendName = cfg.Model.Default
	}
	backendDef, ok := cfg.Model.Backends[backendName]
	if !ok {
		log.Printf("[worker] warn: backend %q not found, falling back to default %q", backendName, cfg.Model.Default)
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
	logFile := fmt.Sprintf("%s/%s-%s.log", logDir, slotName, sess.Phase)

	// Build the worker command
	workerCmd, stdinFile, err := BuildWorkerCmd(backendName, backendCfg, promptFile, sess.Worktree)
	if err != nil {
		return fmt.Errorf("build worker cmd: %w", err)
	}

	// Write runner script
	runnerPath := fmt.Sprintf("%s/%s-run.sh", cfg.StateDir, slotName)
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
	hookEnv := HookEnv{
		IssueID:       fmt.Sprintf("%s#%d", cfg.Repo, sess.IssueNumber),
		IssueNumber:   sess.IssueNumber,
		WorkspacePath: sess.Worktree,
	}
	if err := RunHook(cfg, "before_run", cfg.Hooks.BeforeRun, hookEnv); err != nil {
		return fmt.Errorf("before_run hook: %w", err)
	}

	// Start tmux session
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

	log.Printf("[worker] started phase %s for %s in tmux %s (pane_pid=%d)", sess.Phase, slotName, tmuxName, pid)

	// Update session in place
	sess.PID = pid
	sess.TmuxSession = tmuxName
	sess.LogFile = logFile
	sess.StartedAt = time.Now().UTC()
	sess.FinishedAt = nil
	sess.Status = state.StatusRunning
	sess.Backend = backendName
	sess.LastOutputHash = ""
	sess.LastOutputChangedAt = time.Time{}

	return nil
}
