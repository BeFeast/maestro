package worker

import (
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/pipeline"
	"github.com/befeast/maestro/internal/state"
	"github.com/befeast/maestro/internal/tmuxsession"
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
	tmuxsession.KillSession(tmuxName)

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
	backendCfg := workerBackendConfig(backendDef)
	backendCfg.TokenBudget = cfg.WorkerMaxTokens
	if err := validateLiveTokenBudget(backendName, backendCfg); err != nil {
		return err
	}
	// #841/#900: thread the phase role's effort override into the worker argv via
	// the existing tier-effort path (claude --effort, codex -c
	// model_reasoning_effort; gemini drops it).
	if effort := pipeline.EffortForPhase(cfg, sess.Phase); effort != "" {
		backendCfg.TierEffort = effort
	}

	hookSetup, err := setupWorkerToolHooks(cfg.StateDir, sess.Worktree, resolveBackendKind(backendName, backendCfg), cfg.Hooks)
	if err != nil {
		return fmt.Errorf("setup worker tool hooks: %w", err)
	}
	prompt += workerToolHookPromptSection(cfg.Hooks, backendName, hookSetup)

	// Write prompt to file
	promptFile := fmt.Sprintf("%s/%s-prompt.md", cfg.StateDir, slotName)
	if err := writePromptFile(promptFile, prompt); err != nil {
		return fmt.Errorf("write prompt file: %w", err)
	}

	// Prepare log file
	logDir := state.LogDir(cfg.StateDir)
	if err := ensureWorkerLogDir(logDir); err != nil {
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
	split := streamSplitForBackend(backendName, backendCfg, logFile)
	if err := writeWorkerRunnerScript(cfg.StateDir, runnerPath, workerCmd.Args, stdinFile, logFile, sess.Worktree, split); err != nil {
		return err
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

	log.Printf("[worker] started phase %s for %s in tmux %s (pane_pid=%d)", sess.Phase, slotName, tmuxName, pid)

	// Update session in place
	sess.PID = pid
	sess.TmuxSession = tmuxName
	sess.LogFile = logFile
	beginSessionAttempt(cfg, sess, backendName, "phase_transition", "phase_transition", time.Now())
	sess.LastOutputHash = ""
	sess.LastOutputChangedAt = time.Time{}
	sess.LastNotifiedStatus = ""

	return nil
}
