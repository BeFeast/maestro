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

// Start spawns a new worker for the given issue
func Start(cfg *config.Config, s *state.State, repo string, issue github.Issue, promptBase string) (string, error) {
	prefix := SlotPrefix(cfg.Repo)
	slotName := s.NextSlotName(prefix)

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
	promptFile := filepath.Join(state.StateDir(repo), fmt.Sprintf("%s-prompt.md", slotName))
	if err := os.WriteFile(promptFile, []byte(prompt), 0644); err != nil {
		return "", fmt.Errorf("write prompt file: %w", err)
	}

	// Prepare log file
	logDir := state.LogDir(repo)
	if err := os.MkdirAll(logDir, 0755); err != nil {
		return "", fmt.Errorf("create log dir: %w", err)
	}
	logFile := filepath.Join(logDir, slotName+".log")

	// Build claude command
	claudeArgs := []string{
		"--dangerously-skip-permissions",
		"-p", prompt,
	}

	cmd := exec.Command(cfg.ClaudeCmd, claudeArgs...)
	cmd.Dir = worktreePath

	// Redirect stdin from /dev/null
	devNull, err := os.Open(os.DevNull)
	if err != nil {
		return "", fmt.Errorf("open /dev/null: %w", err)
	}
	cmd.Stdin = devNull

	// Redirect stdout/stderr to log file
	lf, err := os.Create(logFile)
	if err != nil {
		return "", fmt.Errorf("create log file: %w", err)
	}
	cmd.Stdout = lf
	cmd.Stderr = lf

	// Detach from parent process group
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

	if err := cmd.Start(); err != nil {
		lf.Close()
		return "", fmt.Errorf("start claude: %w", err)
	}

	pid := cmd.Process.Pid
	log.Printf("[worker] started %s (pid=%d, log=%s)", slotName, pid, logFile)

	// Save session to state
	s.Sessions[slotName] = &state.Session{
		IssueNumber: issue.Number,
		IssueTitle:  issue.Title,
		Worktree:    worktreePath,
		Branch:      branchName,
		PID:         pid,
		LogFile:     logFile,
		StartedAt:   time.Now().UTC(),
		Status:      state.StatusRunning,
	}

	// Don't wait — let it run in background
	go func() {
		cmd.Wait()
		devNull.Close()
		lf.Close()
	}()

	return slotName, nil
}

// Stop kills a worker and removes its worktree
func Stop(cfg *config.Config, slotName string, sess *state.Session) error {
	// Kill process if alive
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
