package worker

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/github"
	"github.com/befeast/maestro/internal/state"
)

// SlotPrefix derives the slot prefix from the repo name ("BeFeast/panoptikon" → "pan")
func SlotPrefix(repo string) string {
	parts := strings.Split(repo, "/")
	name := parts[len(parts)-1]
	if len(name) >= 3 {
		return name[:3]
	}
	return name
}

// IsAlive checks if a PID is still running
func IsAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = proc.Signal(syscall.Signal(0))
	return err == nil
}

// TmuxSessionName returns the tmux session name for a worker slot
func TmuxSessionName(slotName string) string {
	return "maestro-" + slotName
}

// Start spawns a new worker for the given issue inside a tmux session.
// backendName selects the model backend ("claude", "codex", etc.); empty defaults to config.
func Start(cfg *config.Config, s *state.State, repo string, issue github.Issue, promptBase string, backendName string) (string, error) {
	slotName := s.NextSlotName(cfg.SessionPrefix)

	worktreePath := filepath.Join(cfg.WorktreeBase, slotName)
	branchName := fmt.Sprintf("feat/%s-%d-%s", slotName, issue.Number, slugify(issue.Title))

	// Create worktree
	log.Printf("[worker] creating worktree %s on branch %s", worktreePath, branchName)
	out, err := exec.Command("git", "-C", cfg.LocalPath,
		"worktree", "add", worktreePath, "-b", branchName).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git worktree add: %w\n%s", err, out)
	}

	// Assemble worker prompt
	prompt := assemblePrompt(promptBase, issue, worktreePath, branchName, cfg)

	// Write prompt to file
	promptFile := filepath.Join(cfg.StateDir, fmt.Sprintf("%s-prompt.md", slotName))
	if err := os.WriteFile(promptFile, []byte(prompt), 0644); err != nil {
		return "", fmt.Errorf("write prompt file: %w", err)
	}

	// Determine backend
	if backendName == "" {
		backendName = cfg.Model.Default
	}
	backendDef, ok := cfg.Model.Backends[backendName]
	if !ok {
		log.Printf("[worker] warn: backend %q not found in config, falling back to default %q", backendName, cfg.Model.Default)
		backendName = cfg.Model.Default
		backendDef, ok = cfg.Model.Backends[backendName]
		if !ok {
			return "", fmt.Errorf("backend %q (default) not found in config", backendName)
		}
	}
	backendCfg := BackendConfig{
		Cmd:       backendDef.Cmd,
		ExtraArgs: backendDef.ExtraArgs,
	}

	// Prepare log file
	logDir := state.LogDir(cfg.StateDir)
	if err := os.MkdirAll(logDir, 0755); err != nil {
		return "", fmt.Errorf("create log dir: %w", err)
	}
	logFile := filepath.Join(logDir, slotName+".log")

	// Build the worker command to generate the runner script
	workerCmd, stdinFile, err := BuildWorkerCmd(backendName, backendCfg, promptFile, worktreePath)
	if err != nil {
		return "", fmt.Errorf("build worker cmd: %w", err)
	}

	// Write runner script
	runnerPath := filepath.Join(cfg.StateDir, slotName+"-run.sh")
	var runnerContent string
	if stdinFile != "" {
		// Stdin-based backends (e.g. codex): redirect prompt file to stdin via shell
		runnerContent = fmt.Sprintf("#!/bin/bash\nexec %s < %q 2>&1 | tee -a %q\n",
			shellJoin(workerCmd.Args), stdinFile, logFile)
	} else {
		// Arg-based backends (e.g. claude): prompt is already in args
		runnerContent = fmt.Sprintf("#!/bin/bash\nexec %s 2>&1 | tee -a %q\n",
			shellJoin(workerCmd.Args), logFile)
	}
	if err := os.WriteFile(runnerPath, []byte(runnerContent), 0755); err != nil {
		return "", fmt.Errorf("write runner script: %w", err)
	}

	// Start tmux session
	tmuxName := TmuxSessionName(slotName)
	tmuxCmd := exec.Command("tmux", "new-session", "-d", "-s", tmuxName, "-c", worktreePath, "bash", runnerPath)
	if tmuxOut, err := tmuxCmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("tmux new-session: %w\n%s", err, tmuxOut)
	}

	// Get PID of the shell running inside the tmux pane
	pidOut, err := exec.Command("tmux", "list-panes", "-t", tmuxName, "-F", "#{pane_pid}").Output()
	if err != nil {
		return "", fmt.Errorf("tmux list-panes: %w", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(pidOut)))
	if err != nil {
		return "", fmt.Errorf("parse pane pid: %w", err)
	}

	log.Printf("[worker] started %s in tmux session %s (pane_pid=%d, log=%s)", slotName, tmuxName, pid, logFile)

	// Collect issue label names for runtime decisions (e.g. long-running)
	var labelNames []string
	for _, l := range issue.Labels {
		labelNames = append(labelNames, l.Name)
	}

	// Save session to state
	s.Sessions[slotName] = &state.Session{
		IssueNumber: issue.Number,
		IssueTitle:  issue.Title,
		IssueLabels: labelNames,
		Worktree:    worktreePath,
		Branch:      branchName,
		PID:         pid,
		LogFile:     logFile,
		StartedAt:   time.Now().UTC(),
		Status:      state.StatusRunning,
		Backend:     backendName,
	}

	return slotName, nil
}

// Stop kills a worker and removes its worktree
func Stop(cfg *config.Config, slotName string, sess *state.Session) error {
	// Try to kill the tmux session first (covers tmux-spawned workers)
	tmuxName := TmuxSessionName(slotName)
	if out, err := exec.Command("tmux", "kill-session", "-t", tmuxName).CombinedOutput(); err != nil {
		log.Printf("[worker] tmux kill-session %s: %v (%s)", tmuxName, err, strings.TrimSpace(string(out)))
	}

	// Kill process if alive (fallback for pre-tmux workers)
	if sess.PID > 0 && IsAlive(sess.PID) {
		proc, _ := os.FindProcess(sess.PID)
		if err := proc.Kill(); err != nil {
			log.Printf("[worker] kill pid %d: %v", sess.PID, err)
		}
	}

	// Remove worktree
	if sess.Worktree != "" {
		out, err := exec.Command("git", "-C", cfg.LocalPath,
			"worktree", "remove", "--force", sess.Worktree).CombinedOutput()
		if err != nil {
			log.Printf("[worker] remove worktree %s: %v\n%s", sess.Worktree, err, out)
		}
	}

	return nil
}

// RebaseWorktree runs git fetch + rebase in the worktree
func RebaseWorktree(worktreePath, branch string) error {
	cmds := [][]string{
		{"git", "fetch", "origin"},
		{"git", "rebase", "origin/main"},
		{"git", "push", "--force-with-lease", "origin", branch},
	}
	for _, c := range cmds {
		out, err := exec.Command(c[0], c[1:]...).CombinedOutput()
		if err != nil {
			return fmt.Errorf("%s: %w\n%s", strings.Join(c, " "), err, out)
		}
	}
	return nil
}

// shellJoin quotes args for shell safety in generated scripts.
func shellJoin(args []string) string {
	quoted := make([]string, len(args))
	for i, a := range args {
		// Simple quoting: wrap in single quotes, escape existing single quotes
		if strings.ContainsAny(a, " \t\n'\"\\$`!#&|;(){}[]<>?*~") {
			a = "'" + strings.ReplaceAll(a, "'", "'\\''") + "'"
		}
		quoted[i] = a
	}
	return strings.Join(quoted, " ")
}

func slugify(title string) string {
	title = strings.ToLower(title)
	var b strings.Builder
	for _, r := range title {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
		} else if r == ' ' || r == '-' || r == '_' {
			b.WriteRune('-')
		}
	}
	s := b.String()
	// Trim trailing dashes and limit length
	s = strings.Trim(s, "-")
	if len(s) > 40 {
		s = s[:40]
	}
	return s
}

// assemblePrompt builds the final worker prompt.
// If the base template contains {{ISSUE_NUMBER}} placeholders, it performs
// template substitution. Otherwise it falls back to appending a task block.
func assemblePrompt(base string, issue github.Issue, worktreePath, branchName string, cfg *config.Config) string {
	if strings.Contains(base, "{{ISSUE_NUMBER}}") {
		// Template-style substitution
		r := strings.NewReplacer(
			"{{ISSUE_NUMBER}}", fmt.Sprintf("%d", issue.Number),
			"{{ISSUE_TITLE}}", issue.Title,
			"{{ISSUE_BODY}}", issue.Body,
			"{{BRANCH}}", branchName,
			"{{WORKTREE}}", worktreePath,
			"{{REPO}}", cfg.Repo,
		)
		return r.Replace(base)
	}

	// Legacy: append task block after base prompt
	return fmt.Sprintf(`%s

---

## Your Current Task

**Issue #%d: %s**

**Repository:** %s
**Worktree path:** %s

### Issue Description
%s

---

## Instructions

1. Read and understand the issue above.
2. Implement the required changes in the worktree at: %s
3. Write tests if applicable.
4. Commit your changes with a clear message.
5. Push the branch and create a PR using: gh pr create --repo %s --title "%s" --body "Closes #%d"
6. After creating the PR, you are done. Do NOT merge it yourself.

Important: Always run cargo fmt --all before committing if this is a Rust project.
Always rebase on origin/main immediately before creating the PR.
`,
		base,
		issue.Number, issue.Title,
		cfg.Repo,
		worktreePath,
		issue.Body,
		worktreePath,
		cfg.Repo,
		issue.Title,
		issue.Number,
	)
}

// SlotNameFromPID finds a slot name by PID string (for display)
func SlotNameFromPID(pid int) string {
	return strconv.Itoa(pid)
}
