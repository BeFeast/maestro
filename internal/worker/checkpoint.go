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

const tmuxSpawnReconcileAttempts = 3

var (
	runTmuxNewSession = func(tmuxName, worktree, runnerPath string) ([]byte, error) {
		return exec.Command("tmux", "new-session", "-d", "-s", tmuxName, "-c", worktree, "bash", runnerPath).CombinedOutput()
	}
	readTmuxPaneIdentity = func(tmuxName string) (int, string, error) {
		out, err := exec.Command("tmux", "list-panes", "-t", tmuxName, "-F", "#{pane_pid}\t#{pane_current_path}").Output()
		if err != nil {
			return 0, "", err
		}
		parts := strings.SplitN(strings.TrimSpace(string(out)), "\t", 2)
		if len(parts) != 2 {
			return 0, "", fmt.Errorf("unexpected tmux pane identity")
		}
		pid, err := strconv.Atoi(strings.TrimSpace(parts[0]))
		if err != nil || pid <= 0 {
			return 0, "", fmt.Errorf("invalid tmux pane pid")
		}
		return pid, filepath.Clean(strings.TrimSpace(parts[1])), nil
	}
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
	// Kill tmux session + process subtree (but do NOT remove worktree). Reaping
	// the whole descendant tree — not just the recorded pane PID — ensures
	// worker grandchildren that reparented away (e.g. headless Chrome) do not
	// leak across a respawn.
	tmuxName := TmuxSessionName(slotName)
	if out, err := exec.Command("tmux", "kill-session", "-t", tmuxName).CombinedOutput(); err != nil {
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

	// Start exactly once, then reconcile the result by exact tmux session,
	// pane PID, and worktree. A command transport error may arrive after tmux
	// created the runner; observing that exact session adopts it instead of
	// replaying the runner command and creating two live workers.
	pid, err := startOrReconcileTmuxSession(tmuxName, sess.Worktree, runnerPath, sess.PID)
	if err != nil {
		return err
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
	// #513: in-place respawn (post-checkpoint resume) — record a new
	// segment so the timeline shows where the previous segment ended
	// and the new one began.
	recordBackendAttribution(cfg, sess, backendName, "in_place_respawn", "in_place_respawn", time.Now())
	sess.TokensUsedAttempt = 0
	sess.WorkerOutcome = ""
	sess.NotifiedCIFail = false
	sess.LastNotifiedStatus = ""
	sess.LastOutputHash = ""
	sess.LastOutputChangedAt = time.Time{}

	return nil
}

func startOrReconcileTmuxSession(tmuxName, worktree, runnerPath string, previousPID int) (int, error) {
	spawnOut, spawnErr := runTmuxNewSession(tmuxName, worktree, runnerPath)
	wantWorktree := filepath.Clean(worktree)
	var observeErr error
	for i := 0; i < tmuxSpawnReconcileAttempts; i++ {
		pid, paneWorktree, err := readTmuxPaneIdentity(tmuxName)
		if err != nil {
			observeErr = err
			continue
		}
		if paneWorktree != wantWorktree {
			return 0, fmt.Errorf("tmux spawn identity mismatch")
		}
		if spawnErr != nil && previousPID > 0 && pid == previousPID {
			return 0, fmt.Errorf("tmux spawn left the previous worker session alive")
		}
		return pid, nil
	}
	if spawnErr != nil {
		return 0, fmt.Errorf("tmux new-session: %w\n%s", spawnErr, spawnOut)
	}
	return 0, fmt.Errorf("tmux list-panes after spawn: %w", observeErr)
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
