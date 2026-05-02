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
	"github.com/befeast/maestro/internal/pipeline"
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

	// Run after_create hook
	hookEnv := HookEnv{
		IssueID:       fmt.Sprintf("%s#%d", cfg.Repo, issue.Number),
		IssueNumber:   issue.Number,
		WorkspacePath: worktreePath,
	}
	if err := RunHook(cfg, "after_create", cfg.Hooks.AfterCreate, hookEnv); err != nil {
		log.Printf("[worker] after_create hook failed: %v", err)
	}

	// Generate validation contract in worktree (if enabled and not already present from hook)
	if cfg.ValidationContract {
		if _, err := os.Stat(filepath.Join(worktreePath, "VALIDATION.md")); os.IsNotExist(err) {
			if _, err := GenerateValidationContract(issue, worktreePath); err != nil {
				log.Printf("[worker] validation contract generation failed: %v", err)
			} else {
				log.Printf("[worker] generated VALIDATION.md in %s", worktreePath)
			}
		} else {
			log.Printf("[worker] VALIDATION.md already present (from hook), skipping generation")
		}
	}

	// Run GSD pre-worker pipeline (research, plan validation, test mapping)
	pipelineResult := pipeline.RunGSD(cfg, worktreePath, issue.Number, issue.Title, issue.Body)
	if section := pipelineResult.PromptSection(); section != "" {
		promptBase = promptBase + section
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
		Cmd:        backendDef.Cmd,
		ExtraArgs:  backendDef.ExtraArgs,
		PromptMode: backendDef.PromptMode,
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
	if err := writeWorkerRunnerScript(cfg.StateDir, runnerPath, workerCmd.Args, stdinFile, logFile, worktreePath); err != nil {
		return "", err
	}

	// Run before_run hook (fatal on failure)
	if err := RunHook(cfg, "before_run", cfg.Hooks.BeforeRun, hookEnv); err != nil {
		return "", fmt.Errorf("before_run hook: %w", err)
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

	// Save session to state
	startedAt := time.Now().UTC()
	s.Sessions[slotName] = &state.Session{
		IssueNumber: issue.Number,
		IssueTitle:  issue.Title,
		Worktree:    worktreePath,
		Branch:      branchName,
		PID:         pid,
		TmuxSession: tmuxName,
		LogFile:     logFile,
		StartedAt:   startedAt,
		Status:      state.StatusRunning,
		Backend:     backendName,
	}
	s.ReconcileSpawnWorkerApprovalsForStartedSession(slotName, s.Sessions[slotName], startedAt)

	return slotName, nil
}

// Respawn cleans up a dead worker and restarts it in the same slot with a fresh worktree.
// The session is updated in place with new PID, worktree, branch, and timestamps.
func Respawn(cfg *config.Config, slotName string, sess *state.Session, repo string, issue github.Issue, promptBase string, backendName string) error {
	// Clean up old worker (tmux session, process, worktree)
	Stop(cfg, slotName, sess)

	// Delete old local branch (ignore errors — branch may not exist locally)
	exec.Command("git", "-C", cfg.LocalPath, "branch", "-D", sess.Branch).CombinedOutput()

	// Create fresh worktree with new branch
	worktreePath := filepath.Join(cfg.WorktreeBase, slotName)
	branchName := fmt.Sprintf("feat/%s-%d-%s", slotName, issue.Number, slugify(issue.Title))

	log.Printf("[worker] respawn: creating worktree %s on branch %s", worktreePath, branchName)
	out, err := exec.Command("git", "-C", cfg.LocalPath,
		"worktree", "add", worktreePath, "-b", branchName).CombinedOutput()
	if err != nil {
		return fmt.Errorf("git worktree add: %w\n%s", err, out)
	}

	// Run after_create hook
	hookEnv := HookEnv{
		IssueID:       fmt.Sprintf("%s#%d", cfg.Repo, issue.Number),
		IssueNumber:   issue.Number,
		WorkspacePath: worktreePath,
	}
	if err := RunHook(cfg, "after_create", cfg.Hooks.AfterCreate, hookEnv); err != nil {
		log.Printf("[worker] after_create hook failed: %v", err)
	}

	// Generate validation contract in worktree (if enabled and not already present from hook)
	if cfg.ValidationContract {
		if _, err := os.Stat(filepath.Join(worktreePath, "VALIDATION.md")); os.IsNotExist(err) {
			if _, err := GenerateValidationContract(issue, worktreePath); err != nil {
				log.Printf("[worker] validation contract generation failed: %v", err)
			} else {
				log.Printf("[worker] generated VALIDATION.md in %s", worktreePath)
			}
		} else {
			log.Printf("[worker] VALIDATION.md already present (from hook), skipping generation")
		}
	}

	// Run GSD pre-worker pipeline (research, plan validation, test mapping)
	pipelineResult := pipeline.RunGSD(cfg, worktreePath, issue.Number, issue.Title, issue.Body)
	if section := pipelineResult.PromptSection(); section != "" {
		promptBase = promptBase + section
	}

	// Assemble worker prompt
	prompt := assemblePrompt(promptBase, issue, worktreePath, branchName, cfg)

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
	if err := writeWorkerRunnerScript(cfg.StateDir, runnerPath, workerCmd.Args, stdinFile, logFile, worktreePath); err != nil {
		return err
	}

	// Run before_run hook (fatal on failure)
	if err := RunHook(cfg, "before_run", cfg.Hooks.BeforeRun, hookEnv); err != nil {
		return fmt.Errorf("before_run hook: %w", err)
	}

	// Start tmux session
	tmuxName := TmuxSessionName(slotName)
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

	log.Printf("[worker] respawned %s in tmux session %s (pane_pid=%d, log=%s)", slotName, tmuxName, pid, logFile)

	// Update session in place
	sess.Worktree = worktreePath
	sess.Branch = branchName
	sess.PID = pid
	sess.TmuxSession = tmuxName
	sess.LogFile = logFile
	sess.StartedAt = time.Now().UTC()
	sess.FinishedAt = nil
	sess.Status = state.StatusRunning
	sess.PRNumber = 0
	sess.Backend = backendName
	sess.NotifiedCIFail = false
	sess.LastNotifiedStatus = ""
	sess.LastOutputHash = ""
	sess.LastOutputChangedAt = time.Time{}
	sess.TokensUsedAttempt = 0
	// CIFailureOutput and PreviousAttemptFeedback are normally cleared by
	// respawnDueRetries before this call, but cleared here defensively in
	// case Respawn is called from other paths.
	sess.CIFailureOutput = ""
	sess.PreviousAttemptFeedback = ""
	sess.PreviousAttemptFeedbackKind = ""
	sess.CheckpointFile = ""

	return nil
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

	// Run before_remove hook
	if sess.Worktree != "" {
		hookEnv := HookEnv{
			IssueID:       fmt.Sprintf("%s#%d", cfg.Repo, sess.IssueNumber),
			IssueNumber:   sess.IssueNumber,
			WorkspacePath: sess.Worktree,
		}
		if err := RunHook(cfg, "before_remove", cfg.Hooks.BeforeRemove, hookEnv); err != nil {
			log.Printf("[worker] before_remove hook failed: %v", err)
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

// CleanupResult describes the outcome of cleaning up a single session's worktree.
type CleanupResult struct {
	SlotName    string
	IssueNumber int
	Worktree    string
	Removed     bool
	Error       error
}

// CleanupWorktrees removes worktrees for all terminal sessions that still have
// a worktree directory on disk. Returns results for each session processed.
func CleanupWorktrees(cfg *config.Config, s *state.State) []CleanupResult {
	var results []CleanupResult
	for slotName, sess := range s.Sessions {
		if !state.IsTerminal(sess.Status) {
			continue
		}
		if sess.Worktree == "" {
			continue
		}
		if _, err := os.Stat(sess.Worktree); os.IsNotExist(err) {
			// Worktree dir already gone — just clear the field
			sess.Worktree = ""
			continue
		}
		err := RemoveWorktree(cfg.LocalPath, sess.Worktree)
		r := CleanupResult{
			SlotName:    slotName,
			IssueNumber: sess.IssueNumber,
			Worktree:    sess.Worktree,
		}
		if err != nil {
			r.Error = err
			log.Printf("[worker] cleanup worktree %s for %s: %v", sess.Worktree, slotName, err)
		} else {
			r.Removed = true
			sess.Worktree = ""
			log.Printf("[worker] cleaned up worktree %s for %s", r.Worktree, slotName)
		}
		results = append(results, r)
	}
	return results
}

// RemoveWorktree removes a git worktree directory.
// Returns nil if the worktree was removed or doesn't exist.
func RemoveWorktree(localPath, worktreePath string) error {
	if worktreePath == "" {
		return nil
	}
	if _, err := os.Stat(worktreePath); os.IsNotExist(err) {
		return nil
	}
	out, err := exec.Command("git", "-C", localPath,
		"worktree", "remove", "--force", worktreePath).CombinedOutput()
	if err != nil {
		return fmt.Errorf("git worktree remove %s: %w\n%s", worktreePath, err, out)
	}
	return nil
}

// RebaseWorktree runs git fetch + rebase in the worktree.
// If rebase hits conflicts in known shared files, it auto-resolves by keeping
// both sides, continues the rebase, then force-pushes the branch.
// autoResolveFiles is the list of file paths (relative to repo root) that may
// be auto-resolved by keeping both sides; it comes from cfg.AutoResolveFiles.
// autoRestoreFiles is the list of dirty disposable paths that may be restored
// before rebasing; it comes from cfg.AutoRestoreFiles.
func RebaseWorktree(worktreePath, branch string, autoResolveFiles, autoRestoreFiles []string) error {
	if strings.TrimSpace(worktreePath) == "" {
		return fmt.Errorf("empty worktree path")
	}
	if strings.TrimSpace(branch) == "" {
		return fmt.Errorf("empty branch")
	}

	if _, err := runGit(worktreePath, "fetch", "origin"); err != nil {
		return err
	}
	if err := restoreAllowedDirtyFiles(worktreePath, autoRestoreFiles); err != nil {
		return err
	}
	if dirty, err := worktreeDirty(worktreePath); err != nil {
		return err
	} else if dirty != "" {
		return fmt.Errorf("worktree has uncommitted changes after auto_restore_files; refusing rebase:\n%s", dirty)
	}

	if _, rebaseErr := runGit(worktreePath, "rebase", "origin/main"); rebaseErr != nil {
		if _, resolveErr := continueRebaseWithAutoResolvedConflicts(worktreePath, autoResolveFiles); resolveErr != nil {
			if _, abortErr := runGit(worktreePath, "rebase", "--abort"); abortErr != nil {
				return fmt.Errorf("rebase failed: %v; auto-resolve failed: %v; additionally failed to abort rebase: %v", rebaseErr, resolveErr, abortErr)
			}
			return fmt.Errorf("rebase failed: %v; auto-resolve failed: %v", rebaseErr, resolveErr)
		}
	}

	if _, err := runGit(worktreePath, "push", "--force-with-lease", "origin", branch); err != nil {
		return err
	}

	return nil
}

func restoreAllowedDirtyFiles(worktreePath string, autoRestoreFiles []string) error {
	paths := normalizedGitPaths(autoRestoreFiles)
	if len(paths) == 0 {
		return nil
	}
	args := append([]string{"status", "--porcelain", "--untracked-files=all", "--"}, paths...)
	dirty, err := runGit(worktreePath, args...)
	if err != nil {
		return err
	}
	cleanArgs := append([]string{"clean", "-fdxn", "--"}, paths...)
	cleanPreview, err := runGit(worktreePath, cleanArgs...)
	if err != nil {
		return err
	}
	if strings.TrimSpace(dirty) == "" && strings.TrimSpace(cleanPreview) == "" {
		return nil
	}

	log.Printf("[worker] restoring allowed dirty files before rebase in %s: %s", worktreePath, strings.Join(paths, ", "))
	trackedPaths, err := trackedGitPaths(worktreePath, paths)
	if err != nil {
		return err
	}
	if len(trackedPaths) > 0 {
		args = append([]string{"restore", "--"}, trackedPaths...)
		if _, err := runGit(worktreePath, args...); err != nil {
			return err
		}
	}
	args = append([]string{"clean", "-fdx", "--"}, paths...)
	if _, err := runGit(worktreePath, args...); err != nil {
		return err
	}
	return nil
}

func trackedGitPaths(worktreePath string, paths []string) ([]string, error) {
	args := append([]string{"ls-files", "--"}, paths...)
	out, err := runGit(worktreePath, args...)
	if err != nil {
		return nil, err
	}
	lines := strings.Split(out, "\n")
	tracked := make([]string, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		tracked = append(tracked, toSlash(line))
	}
	return tracked, nil
}

func worktreeDirty(worktreePath string) (string, error) {
	out, err := runGit(worktreePath, "status", "--porcelain")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

func normalizedGitPaths(paths []string) []string {
	seen := make(map[string]struct{}, len(paths))
	normalized := make([]string, 0, len(paths))
	for _, path := range paths {
		path = strings.TrimSpace(path)
		if path == "" {
			continue
		}
		path = toSlash(path)
		if _, ok := seen[path]; ok {
			continue
		}
		seen[path] = struct{}{}
		normalized = append(normalized, path)
	}
	return normalized
}

func runGit(worktreePath string, args ...string) (string, error) {
	cmdArgs := append([]string{"-C", worktreePath}, args...)
	out, err := exec.Command("git", cmdArgs...).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %w\n%s", strings.Join(args, " "), err, out)
	}
	return string(out), nil
}

func continueRebaseWithAutoResolvedConflicts(worktreePath string, autoResolveFiles []string) ([]string, error) {
	autoResolveSet := make(map[string]struct{}, len(autoResolveFiles))
	for _, f := range autoResolveFiles {
		autoResolveSet[f] = struct{}{}
	}

	for {
		conflicted, err := conflictedFiles(worktreePath)
		if err != nil {
			return nil, err
		}
		if len(conflicted) == 0 {
			return nil, fmt.Errorf("rebase failed without conflicted files")
		}

		var resolved []string
		var unresolved []string
		for _, file := range conflicted {
			if _, ok := autoResolveSet[toSlash(file)]; !ok {
				unresolved = append(unresolved, file)
				continue
			}

			fullPath := filepath.Join(worktreePath, filepath.FromSlash(file))
			if err := resolveConflictFileKeepBothSides(fullPath); err != nil {
				return nil, fmt.Errorf("resolve %s: %w", file, err)
			}
			if _, err := runGit(worktreePath, "add", "--", file); err != nil {
				return nil, err
			}
			resolved = append(resolved, file)
		}

		if len(unresolved) > 0 {
			return nil, fmt.Errorf("unresolvable conflicts in files: %s", strings.Join(unresolved, ", "))
		}
		if len(resolved) == 0 {
			return nil, fmt.Errorf("rebase has conflicts but none are auto-resolvable")
		}

		if _, err := runGit(worktreePath, "-c", "core.editor=true", "rebase", "--continue"); err != nil {
			nextConflicted, diffErr := conflictedFiles(worktreePath)
			if diffErr != nil {
				return nil, fmt.Errorf("git rebase --continue failed: %w (and could not inspect next conflicts: %v)", err, diffErr)
			}
			if len(nextConflicted) == 0 {
				return nil, fmt.Errorf("git rebase --continue failed: %w", err)
			}
			// There are more conflict rounds; resolve in next loop.
			continue
		}

		return resolved, nil
	}
}

func conflictedFiles(worktreePath string) ([]string, error) {
	out, err := runGit(worktreePath, "diff", "--name-only", "--diff-filter=U")
	if err != nil {
		return nil, err
	}
	lines := strings.Split(out, "\n")
	files := make([]string, 0, len(lines))
	for _, l := range lines {
		l = strings.TrimSpace(l)
		if l == "" {
			continue
		}
		files = append(files, toSlash(l))
	}
	return files, nil
}

func toSlash(path string) string {
	return strings.ReplaceAll(path, "\\", "/")
}

func resolveConflictFileKeepBothSides(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read file: %w", err)
	}

	resolved, changed, err := keepBothSides(string(data))
	if err != nil {
		return err
	}
	if !changed {
		return fmt.Errorf("no conflict markers found")
	}

	fi, statErr := os.Stat(path)
	mode := os.FileMode(0644)
	if statErr == nil {
		mode = fi.Mode()
	}
	if err := os.WriteFile(path, []byte(resolved), mode); err != nil {
		return fmt.Errorf("write resolved file: %w", err)
	}
	return nil
}

func keepBothSides(content string) (string, bool, error) {
	const (
		stateNormal = iota
		stateOurs
		stateBase
		stateTheirs
	)

	lines := strings.Split(content, "\n")
	out := make([]string, 0, len(lines))
	ours := make([]string, 0)
	theirs := make([]string, 0)
	state := stateNormal
	hadConflict := false

	for _, line := range lines {
		switch state {
		case stateNormal:
			if strings.HasPrefix(line, "<<<<<<<") {
				hadConflict = true
				ours = ours[:0]
				theirs = theirs[:0]
				state = stateOurs
				continue
			}
			out = append(out, line)
		case stateOurs:
			switch {
			case strings.HasPrefix(line, "|||||||"):
				state = stateBase
			case strings.HasPrefix(line, "======="):
				state = stateTheirs
			default:
				ours = append(ours, line)
			}
		case stateBase:
			if strings.HasPrefix(line, "=======") {
				state = stateTheirs
			}
		case stateTheirs:
			if strings.HasPrefix(line, ">>>>>>>") {
				out = append(out, ours...)
				out = append(out, theirs...)
				state = stateNormal
				continue
			}
			theirs = append(theirs, line)
		}
	}

	if state != stateNormal {
		return "", hadConflict, fmt.Errorf("unterminated conflict markers")
	}
	return strings.Join(out, "\n"), hadConflict, nil
}

// shellJoin quotes args for shell safety in generated scripts.
func shellJoin(args []string) string {
	quoted := make([]string, len(args))
	for i, a := range args {
		// Simple quoting: wrap in single quotes, escape existing single quotes
		if strings.ContainsAny(a, " \t\n'\"\\$`!#&|;(){}[]<>?*~") {
			a = shellQuote(a)
		}
		quoted[i] = a
	}
	return strings.Join(quoted, " ")
}

func shellQuote(arg string) string {
	return "'" + strings.ReplaceAll(arg, "'", "'\\''") + "'"
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

// readValidationContract reads VALIDATION.md from the worktree root.
// Returns the file content or empty string if the file doesn't exist.
func readValidationContract(worktreePath string) string {
	data, err := os.ReadFile(filepath.Join(worktreePath, "VALIDATION.md"))
	if err != nil {
		return ""
	}
	return string(data)
}

// assemblePrompt builds the final worker prompt.
// If the base template contains {{ISSUE_NUMBER}} placeholders, it performs
// template substitution. Otherwise it falls back to appending a task block.
//
// Additional behavior:
//   - If {{VALIDATION_CONTRACT}} is in the template, replaces it with VALIDATION.md
//     content (or a fallback message if the file is missing)
//   - If VALIDATION.md exists but no placeholder is present, appends the contract
//   - Loads and appends any prompt section files from cfg.PromptSections
func assemblePrompt(base string, issue github.Issue, worktreePath, branchName string, cfg *config.Config) string {
	// Load validation contract from worktree (if present)
	validationContract := readValidationContract(worktreePath)

	if strings.Contains(base, "{{ISSUE_NUMBER}}") {
		// Template-style substitution
		replacements := []string{
			"{{ISSUE_NUMBER}}", fmt.Sprintf("%d", issue.Number),
			"{{ISSUE_TITLE}}", issue.Title,
			"{{ISSUE_BODY}}", issue.Body,
			"{{BRANCH}}", branchName,
			"{{WORKTREE}}", worktreePath,
			"{{REPO}}", cfg.Repo,
		}

		// Handle {{VALIDATION_CONTRACT}} placeholder
		contractInlined := false
		if strings.Contains(base, "{{VALIDATION_CONTRACT}}") {
			contractInlined = true
			contract := validationContract
			if contract == "" {
				contract = "_No VALIDATION.md found in worktree. Define your own acceptance criteria from the issue requirements before implementing._"
			}
			replacements = append(replacements, "{{VALIDATION_CONTRACT}}", contract)
		}

		r := strings.NewReplacer(replacements...)
		result := r.Replace(base) + workerPRReferenceSafetyPromptSection(issue.Number) + workerSearchSafetyPromptSection(worktreePath)
		return appendSectionsAndValidation(result, cfg.PromptSections, validationContract, contractInlined)
	}

	// Legacy: append task block after base prompt
	result := fmt.Sprintf(`%s

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
5. Before committing or opening a PR, check for accidental secrets and generated artifacts. Do NOT commit or mention API keys, bearer tokens, oauth tokens, bot tokens, env values, raw config dumps, or diagnostic logs. Do NOT commit temp/debug artifacts such as tmp/, _tmp/, *.log, *.logs, *.test, or *.test.json unless the issue explicitly requires them.
6. Keep the PR body minimal and safe. Use: gh pr create --repo %s --title "%s" --body "Refs #%d". Do NOT use GitHub auto-closing keywords (`+"`Closes`, `Fixes`, `Resolves`"+`, or variants) for deployment/runtime/operator-verification issues. Do NOT paste logs, doctor output, env dumps, or secret-bearing snippets into the PR body or comments.
7. After creating the PR, you are done. Do NOT merge it yourself.

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
	result += workerPRReferenceSafetyPromptSection(issue.Number)
	result += workerSearchSafetyPromptSection(worktreePath)
	return appendSectionsAndValidation(result, cfg.PromptSections, validationContract, false)
}

// appendSectionsAndValidation appends prompt section files and the validation
// contract (if not already inlined via placeholder) to the prompt.
func appendSectionsAndValidation(prompt string, sectionPaths []string, validationContract string, contractAlreadyInlined bool) string {
	var b strings.Builder
	b.WriteString(prompt)

	// Append prompt sections
	for _, path := range sectionPaths {
		data, err := os.ReadFile(path)
		if err != nil {
			log.Printf("[worker] warn: could not read prompt section %s: %v", path, err)
			continue
		}
		b.WriteString("\n\n---\n\n")
		b.WriteString(string(data))
	}

	// Append validation contract if present and not already inlined
	if validationContract != "" && !contractAlreadyInlined {
		b.WriteString("\n\n---\n\n")
		b.WriteString(validationContract)
	}

	return b.String()
}

// SlotNameFromPID finds a slot name by PID string (for display)
func SlotNameFromPID(pid int) string {
	return strconv.Itoa(pid)
}
