package orchestrator

import (
	"bufio"
	"context"
	"crypto/sha256"
	"fmt"
	"log"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/github"
	"github.com/befeast/maestro/internal/notify"
	"github.com/befeast/maestro/internal/router"
	"github.com/befeast/maestro/internal/state"
	"github.com/befeast/maestro/internal/versioning"
	"github.com/befeast/maestro/internal/worker"
)

// Orchestrator coordinates all agent sessions
type Orchestrator struct {
	cfg                   *config.Config
	notifier              *notify.Notifier
	gh                    *github.Client
	router                *router.Router
	repo                  string
	promptBase            string
	bugPromptBase         string
	enhancementPromptBase string
	pidAliveFn            func(pid int) bool
	tmuxSessionExistsFn   func(name string) bool
}

// New creates a new Orchestrator
func New(cfg *config.Config) *Orchestrator {
	n := notify.NewWithToken(cfg.Telegram.BotToken, cfg.Telegram.Target, cfg.Telegram.OpenclawURL)
	if cfg.Telegram.DigestMode {
		n.SetDigestMode(true)
		log.Printf("[orch] digest mode enabled — notifications will be batched per cycle")
	}
	return &Orchestrator{
		cfg:      cfg,
		notifier: n,
		gh:       github.New(cfg.Repo),
		router:   router.New(cfg),
		repo:     cfg.Repo,
	}
}

func (o *Orchestrator) pidAlive(pid int) bool {
	if o.pidAliveFn != nil {
		return o.pidAliveFn(pid)
	}
	return worker.IsAlive(pid)
}

func (o *Orchestrator) tmuxSessionExists(name string) bool {
	if o.tmuxSessionExistsFn != nil {
		return o.tmuxSessionExistsFn(name)
	}
	if name == "" {
		return false
	}
	return exec.Command("tmux", "has-session", "-t", name).Run() == nil
}

func readLastLines(path string, limit int) (string, error) {
	if limit <= 0 {
		return "", nil
	}
	if strings.TrimSpace(path) == "" {
		return "", fmt.Errorf("empty log file path")
	}

	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	lines := make([]string, 0, limit)
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
		if len(lines) > limit {
			lines = lines[1:]
		}
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}
	if len(lines) == 0 {
		return "(log file is empty)", nil
	}
	return strings.Join(lines, "\n"), nil
}

func tmuxCapture(session string) (string, error) {
	if strings.TrimSpace(session) == "" {
		return "", fmt.Errorf("empty tmux session")
	}
	out, err := exec.Command("tmux", "capture-pane", "-t", session, "-p").Output()
	if err != nil {
		return "", err
	}
	return string(out), nil
}

func hashOutput(output string) string {
	const tailLines = 50

	lines := strings.Split(output, "\n")
	if len(lines) > tailLines {
		lines = lines[len(lines)-tailLines:]
	}
	tail := strings.Join(lines, "\n")
	sum := sha256.Sum256([]byte(tail))
	return fmt.Sprintf("%x", sum[:])
}

func countSilentTimeoutKillsForIssue(s *state.State, issueNumber int) int {
	count := 0
	for _, sess := range s.Sessions {
		if sess.IssueNumber == issueNumber && sess.LastNotifiedStatus == "silent_timeout" {
			count++
		}
	}
	return count
}

// LoadPromptBase reads the worker prompt template from config or a provided path.
// Priority: 1) explicit promptPath arg, 2) cfg.WorkerPrompt, 3) built-in fallback.
// Also loads optional bug_prompt and enhancement_prompt from config.
func (o *Orchestrator) LoadPromptBase(promptPath string) error {
	if promptPath == "" {
		promptPath = o.cfg.WorkerPrompt
	}
	if promptPath == "" {
		log.Printf("[orch] warn: no worker_prompt configured, using built-in fallback")
		o.promptBase = "You are a coding agent. Implement the given issue in the provided worktree."
		return nil
	}
	data, err := os.ReadFile(promptPath)
	if err != nil {
		log.Printf("[orch] warn: could not read prompt base from %s: %v", promptPath, err)
		o.promptBase = "You are a coding agent. Implement the given issue in the provided worktree."
		return nil
	}
	o.promptBase = string(data)
	log.Printf("[orch] loaded prompt base from %s (%d bytes)", promptPath, len(data))

	// Load optional per-issue-type prompts
	if o.cfg.BugPrompt != "" {
		if data, err := os.ReadFile(o.cfg.BugPrompt); err != nil {
			log.Printf("[orch] warn: could not read bug_prompt from %s: %v", o.cfg.BugPrompt, err)
		} else {
			o.bugPromptBase = string(data)
			log.Printf("[orch] loaded bug_prompt from %s (%d bytes)", o.cfg.BugPrompt, len(data))
		}
	}
	if o.cfg.EnhancementPrompt != "" {
		if data, err := os.ReadFile(o.cfg.EnhancementPrompt); err != nil {
			log.Printf("[orch] warn: could not read enhancement_prompt from %s: %v", o.cfg.EnhancementPrompt, err)
		} else {
			o.enhancementPromptBase = string(data)
			log.Printf("[orch] loaded enhancement_prompt from %s (%d bytes)", o.cfg.EnhancementPrompt, len(data))
		}
	}

	return nil
}

// selectPrompt returns the appropriate prompt template for an issue based on its labels.
// Priority: bug label → bug_prompt, enhancement label → enhancement_prompt, fallback → worker_prompt.
func (o *Orchestrator) selectPrompt(issue github.Issue) string {
	if o.bugPromptBase != "" && github.HasLabel(issue, []string{"bug"}) {
		return o.bugPromptBase
	}
	if o.enhancementPromptBase != "" && github.HasLabel(issue, []string{"enhancement"}) {
		return o.enhancementPromptBase
	}
	return o.promptBase
}

// RunOnce executes one orchestration cycle
func (o *Orchestrator) RunOnce() error {
	s, err := state.Load(o.cfg.StateDir)
	if err != nil {
		return fmt.Errorf("load state: %w", err)
	}

	log.Printf("[orch] === cycle start — %d sessions in state ===", len(s.Sessions))

	// Step 1: Reconcile stale running sessions before scheduling/slot calculation.
	reconciled := o.reconcileRunningSessions(s)

	// Persist immediately when reconciliation changes state, so slot calculation
	// always sees healed state on disk.
	if reconciled {
		if err := state.Save(o.cfg.StateDir, s); err != nil {
			return fmt.Errorf("save state after reconcile: %w", err)
		}
	}

	// Step 2: Check running sessions for dead processes / stale / closed issues
	o.checkSessions(s)

	// Step 3: Auto-merge green PRs
	o.autoMergePRs(s)

	// Step 4: Rebase conflicting PRs
	o.rebaseConflicts(s)

	// Save after all checks/reconciliation
	if err := state.Save(o.cfg.StateDir, s); err != nil {
		return fmt.Errorf("save state: %w", err)
	}

	// Step 5: Start new workers for available slots
	active := len(s.ActiveSessions())
	slots := o.cfg.MaxParallel - active
	log.Printf("[orch] active=%d max=%d available_slots=%d", active, o.cfg.MaxParallel, slots)

	if slots > 0 {
		o.startNewWorkers(s, slots)
		if err := state.Save(o.cfg.StateDir, s); err != nil {
			return fmt.Errorf("save state after workers: %w", err)
		}
	}

	// Flush digest buffer (no-op if digest mode is off or buffer is empty)
	if err := o.notifier.Flush(); err != nil {
		log.Printf("[orch] digest flush: %v", err)
	}

	log.Printf("[orch] === cycle done ===")
	return nil
}

// Run loops with the given interval; if once=true, runs once and returns.
// The context can be used to stop the loop (e.g. for multi-project shutdown).
func (o *Orchestrator) Run(ctx context.Context, interval time.Duration, once bool) error {
	if err := o.RunOnce(); err != nil {
		log.Printf("[orch] run error: %v", err)
	}
	if once {
		return nil
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			log.Printf("[orch] shutting down (%s)", o.repo)
			return nil
		case <-ticker.C:
			if err := o.RunOnce(); err != nil {
				log.Printf("[orch] run error: %v", err)
			}
		}
	}
}

// reconcileRunningSessions self-heals stale "running" sessions.
// If a session is marked running but either its PID is dead/missing OR its tmux session
// is missing, the session is marked dead and its PID/tmux fields are cleared.
func (o *Orchestrator) reconcileRunningSessions(s *state.State) bool {
	reconciled := false
	for slotName, sess := range s.Sessions {
		if sess.Status != state.StatusRunning {
			continue
		}

		tmuxName := sess.TmuxSession
		if tmuxName == "" {
			tmuxName = worker.TmuxSessionName(slotName)
		}

		var reasons []string
		if sess.PID <= 0 {
			reasons = append(reasons, "pid missing")
		} else if !o.pidAlive(sess.PID) {
			reasons = append(reasons, fmt.Sprintf("pid %d dead", sess.PID))
		}

		if !o.tmuxSessionExists(tmuxName) {
			reasons = append(reasons, fmt.Sprintf("tmux session %q missing", tmuxName))
		}

		if len(reasons) == 0 {
			continue
		}

		oldPID := sess.PID
		oldTmux := tmuxName
		sess.Status = state.StatusDead
		sess.PID = 0
		sess.TmuxSession = ""
		now := time.Now().UTC()
		sess.FinishedAt = &now
		reconciled = true

		log.Printf("[orch] reconcile: %s running->dead (%s); pid=%d tmux=%q",
			slotName, strings.Join(reasons, ", "), oldPID, oldTmux)
	}
	return reconciled
}

// checkSessions inspects all sessions and updates their status
func (o *Orchestrator) checkSessions(s *state.State) {
	// Fetch open PRs once for the whole check cycle
	prs, prErr := o.gh.ListOpenPRs()
	branchToPR := make(map[string]github.PR)
	if prErr != nil {
		log.Printf("[orch] list PRs (check): %v", prErr)
	} else {
		for _, pr := range prs {
			branchToPR[pr.HeadRefName] = pr
		}
	}

	for slotName, sess := range s.Sessions {
		switch sess.Status {
		case state.StatusDone, state.StatusDead, state.StatusConflictFailed, state.StatusFailed:
			// Terminal states — cleanup old worktrees after 1h
			if sess.FinishedAt != nil && time.Since(*sess.FinishedAt) > 1*time.Hour && sess.Worktree != "" {
				if _, err := os.Stat(sess.Worktree); err == nil {
					log.Printf("[orch] cleaning up stale worktree for %s (finished %s ago)", slotName, time.Since(*sess.FinishedAt).Round(time.Minute))
					worker.Stop(o.cfg, slotName, sess)
					sess.Worktree = "" // Mark as cleaned
				}
			}
			continue
		}

		// Check if issue is now closed (only for running sessions)
		if sess.Status == state.StatusRunning {
			closed, err := o.gh.IsIssueClosed(sess.IssueNumber)
			if err != nil {
				log.Printf("[orch] check issue #%d: %v", sess.IssueNumber, err)
			} else if closed {
				log.Printf("[orch] issue #%d closed, stopping worker %s", sess.IssueNumber, slotName)
				worker.Stop(o.cfg, slotName, sess)
				sess.Status = state.StatusDone
				now := time.Now().UTC()
				sess.FinishedAt = &now
				continue
			}

			// Check if process is still alive
			if sess.PID > 0 && !worker.IsAlive(sess.PID) {
				// Check if there's an open PR for this branch BEFORE marking dead
				if pr, found := branchToPR[sess.Branch]; found {
					log.Printf("[orch] worker %s exited but PR #%d is open — transitioning to pr_open", slotName, pr.Number)
					sess.Status = state.StatusPROpen
					sess.PRNumber = pr.Number
					o.notifier.Sendf("🔀 maestro: worker %s completed, PR #%d open for issue #%d (%s)",
						slotName, pr.Number, sess.IssueNumber, sess.IssueTitle)
				} else if sess.RetryCount < 1 {
					// First failure — retry with fresh worktree
					log.Printf("[orch] worker %s (pid=%d) died, retrying (attempt %d)", slotName, sess.PID, sess.RetryCount+1)
					sess.RetryCount++

					issue, err := o.gh.GetIssue(sess.IssueNumber)
					if err != nil {
						log.Printf("[orch] fetch issue #%d for retry: %v — marking as failed", sess.IssueNumber, err)
						sess.Status = state.StatusFailed
						now := time.Now().UTC()
						sess.FinishedAt = &now
						o.notifier.Sendf("💀 maestro: worker %s (issue #%d: %s) died and retry failed (could not fetch issue)",
							slotName, sess.IssueNumber, sess.IssueTitle)
						continue
					}

					promptBase := o.selectPrompt(issue)
					if err := worker.Respawn(o.cfg, slotName, sess, o.repo, issue, promptBase, sess.Backend); err != nil {
						log.Printf("[orch] respawn worker %s: %v — marking as failed", slotName, err)
						sess.Status = state.StatusFailed
						now := time.Now().UTC()
						sess.FinishedAt = &now
						o.notifier.Sendf("💀 maestro: worker %s (issue #%d: %s) died and respawn failed: %v",
							slotName, sess.IssueNumber, sess.IssueTitle, err)
						continue
					}

					o.notifier.Sendf("🔄 maestro: retrying worker %s for issue #%d: %s (attempt %d)",
						slotName, sess.IssueNumber, sess.IssueTitle, sess.RetryCount)
				} else {
					// Already retried — mark as permanently failed
					log.Printf("[orch] worker %s (pid=%d) permanently failed after %d retries", slotName, sess.PID, sess.RetryCount)
					if err := o.gh.AddIssueLabel(sess.IssueNumber, "blocked"); err != nil {
						log.Printf("[orch] warn: could not label issue #%d as blocked: %v", sess.IssueNumber, err)
					}
					sess.Status = state.StatusFailed
					now := time.Now().UTC()
					sess.FinishedAt = &now
					o.notifier.Sendf("💀 maestro: worker %s (issue #%d: %s) permanently failed after %d retry.\nCheck log: %s",
						slotName, sess.IssueNumber, sess.IssueTitle, sess.RetryCount, sess.LogFile)
				}
				continue
			}

			// Check if running session has opened a PR (worker still alive)
			if pr, found := branchToPR[sess.Branch]; found {
				log.Printf("[orch] worker %s opened PR #%d while still running — transitioning to pr_open", slotName, pr.Number)
				sess.Status = state.StatusPROpen
				sess.PRNumber = pr.Number
				o.notifier.Sendf("🔀 maestro: worker %s opened PR #%d for issue #%d (%s)",
					slotName, pr.Number, sess.IssueNumber, sess.IssueTitle)
				continue
			}

			// Silent worker detection: compare tmux pane output hash over time.
			if o.cfg.WorkerSilentTimeoutMinutes > 0 {
				tmuxName := sess.TmuxSession
				if tmuxName == "" {
					tmuxName = worker.TmuxSessionName(slotName)
				}

				output, err := tmuxCapture(tmuxName)
				if err != nil {
					log.Printf("[orch] warn: tmux capture-pane failed for %s (%s): %v", slotName, tmuxName, err)
				} else {
					hash := hashOutput(output)
					now := time.Now().UTC()

					if sess.LastOutputHash == "" || sess.LastOutputChangedAt.IsZero() || hash != sess.LastOutputHash {
						sess.LastOutputHash = hash
						sess.LastOutputChangedAt = now
					} else {
						timeout := time.Duration(o.cfg.WorkerSilentTimeoutMinutes) * time.Minute
						if time.Since(sess.LastOutputChangedAt) > timeout {
							log.Printf("[orch] worker %s silent for >%dm, killing", slotName, o.cfg.WorkerSilentTimeoutMinutes)
							if err := worker.Stop(o.cfg, slotName, sess); err != nil {
								log.Printf("[orch] warn: could not stop silent worker %s: %v", slotName, err)
							}

							sess.Status = state.StatusDead
							sess.LastNotifiedStatus = "silent_timeout"
							sess.FinishedAt = &now

							if countSilentTimeoutKillsForIssue(s, sess.IssueNumber) > 1 {
								if err := o.gh.AddIssueLabel(sess.IssueNumber, "blocked"); err != nil {
									log.Printf("[orch] warn: could not label issue #%d as blocked: %v", sess.IssueNumber, err)
								}
							}

							o.notifier.Sendf("⏱️ maestro: worker %s (issue #%d) killed — no output for %d minutes",
								slotName, sess.IssueNumber, o.cfg.WorkerSilentTimeoutMinutes)
							continue
						}
					}
				}
			}

			// Check if worker exceeded max runtime — hard fail (no retry) with diagnostics
			maxMinutes := o.cfg.MaxRuntimeMinutes
			if sess.LongRunning {
				maxMinutes *= 2
			}
			maxRuntime := time.Duration(maxMinutes) * time.Minute
			age := time.Since(sess.StartedAt)
			if age > maxRuntime {
				log.Printf("[orch] worker %s exceeded max runtime (%dm), killing and marking failed", slotName, maxMinutes)

				logTail, err := readLastLines(sess.LogFile, 20)
				if err != nil {
					log.Printf("[orch] warn: could not read log tail for %s (%s): %v", slotName, sess.LogFile, err)
					logTail = fmt.Sprintf("(could not read log file %s: %v)", sess.LogFile, err)
				}

				if err := worker.Stop(o.cfg, slotName, sess); err != nil {
					log.Printf("[orch] warn: could not stop timed-out worker %s: %v", slotName, err)
				}
				if err := o.gh.AddIssueLabel(sess.IssueNumber, "blocked"); err != nil {
					log.Printf("[orch] warn: could not label issue #%d as blocked: %v", sess.IssueNumber, err)
				}
				sess.Status = state.StatusFailed
				now := time.Now().UTC()
				sess.FinishedAt = &now

				o.notifier.Sendf("⏱️ maestro: worker %s (issue #%d: %s) timed out after %d min.\nLast log lines:\n%s",
					slotName, sess.IssueNumber, sess.IssueTitle, maxMinutes, logTail)
			}
		}
	}
}

// autoMergePRs checks open PRs and merges ones with green CI
func (o *Orchestrator) autoMergePRs(s *state.State) {
	prs, err := o.gh.ListOpenPRs()
	if err != nil {
		log.Printf("[orch] list PRs: %v", err)
		return
	}

	// Build branch → PR map
	branchToPR := make(map[string]github.PR)
	for _, pr := range prs {
		branchToPR[pr.HeadRefName] = pr
	}

	type mergeCandidate struct {
		slotName string
		sess     *state.Session
		pr       github.PR
	}

	ready := make([]mergeCandidate, 0)

	for slotName, sess := range s.Sessions {
		if sess.Status != state.StatusPROpen {
			continue
		}

		pr, found := branchToPR[sess.Branch]
		if !found {
			log.Printf("[orch] no open PR found for branch %s (slot %s) — assuming merged/closed", sess.Branch, slotName)
			sess.Status = state.StatusDone
			now := time.Now().UTC()
			sess.FinishedAt = &now
			continue
		}

		if sess.PRNumber == 0 {
			sess.PRNumber = pr.Number
		}

		// Check CI
		ciStatus, err := o.gh.PRCIStatus(pr.Number)
		if err != nil {
			log.Printf("[orch] CI status for PR #%d: %v", pr.Number, err)
			continue
		}

		log.Printf("[orch] PR #%d (%s) CI=%s", pr.Number, sess.Branch, ciStatus)

		switch ciStatus {
		case "success":
			// Reset notification status when CI goes green
			sess.LastNotifiedStatus = ""
			sess.NotifiedCIFail = false // backward compat

			greptileOK, greptilePending, err := o.gh.PRGreptileApproved(pr.Number)
			if err != nil {
				log.Printf("[orch] greptile check PR #%d: %v", pr.Number, err)
				continue // skip this cycle, try next
			}
			if greptilePending {
				log.Printf("[orch] PR #%d waiting for Greptile review", pr.Number)
				continue // not ready yet
			}
			if !greptileOK {
				log.Printf("[orch] PR #%d not approved by Greptile, labeling blocked", pr.Number)
				if err := o.gh.AddIssueLabel(sess.IssueNumber, "blocked"); err != nil {
					log.Printf("[orch] warn: could not label issue #%d as blocked: %v", sess.IssueNumber, err)
				}
				continue
			}

			ready = append(ready, mergeCandidate{slotName: slotName, sess: sess, pr: pr})
		case "failure":
			// Only notify CI failure once — dedup via LastNotifiedStatus
			if sess.LastNotifiedStatus != "ci_failure" {
				o.notifier.Sendf("❌ maestro: CI failing for PR #%d (%s, issue #%d)", pr.Number, sess.Branch, sess.IssueNumber)
				sess.LastNotifiedStatus = "ci_failure"
				sess.NotifiedCIFail = true // backward compat
			}
		}
	}

	if len(ready) == 0 {
		return
	}

	sort.Slice(ready, func(i, j int) bool {
		return ready[i].pr.Number < ready[j].pr.Number
	})

	strategy := o.mergeStrategy()
	if strategy == "parallel" {
		for _, candidate := range ready {
			if o.mergeReadyPR(candidate.slotName, candidate.sess, candidate.pr) {
				s.LastMergeAt = time.Now().UTC()
			}
		}
		return
	}

	interval := o.mergeInterval()
	if !s.LastMergeAt.IsZero() {
		sinceLastMerge := time.Since(s.LastMergeAt)
		if sinceLastMerge < interval {
			log.Printf("[orch] sequential merge mode: waiting for merge interval (%s remaining), skipping %d ready PR(s)", (interval - sinceLastMerge).Round(time.Second), len(ready))
			return
		}
	}

	candidate := ready[0]
	merged := o.mergeReadyPR(candidate.slotName, candidate.sess, candidate.pr)
	if merged {
		s.LastMergeAt = time.Now().UTC()
	}
	if len(ready) > 1 {
		log.Printf("[orch] sequential merge mode: deferring %d additional ready PR(s) to next cycle", len(ready)-1)
	}
}

func (o *Orchestrator) mergeStrategy() string {
	strategy := strings.ToLower(strings.TrimSpace(o.cfg.MergeStrategy))
	if strategy == "parallel" {
		return "parallel"
	}
	return "sequential"
}

func (o *Orchestrator) mergeInterval() time.Duration {
	seconds := o.cfg.MergeIntervalSeconds
	if seconds <= 0 {
		seconds = 30
	}
	return time.Duration(seconds) * time.Second
}

func (o *Orchestrator) mergeReadyPR(slotName string, sess *state.Session, pr github.PR) bool {
	log.Printf("[orch] merging PR #%d (branch %s)", pr.Number, sess.Branch)
	if err := o.gh.MergePR(pr.Number); err != nil {
		log.Printf("[orch] merge PR #%d: %v", pr.Number, err)
		// Only notify merge failure once per PR
		if sess.LastNotifiedStatus != "merge_failed" {
			o.notifier.Sendf("❌ maestro: failed to merge PR #%d (%s): %v", pr.Number, sess.Branch, err)
			sess.LastNotifiedStatus = "merge_failed"
		}
		return false
	}

	log.Printf("[orch] merged PR #%d ✓", pr.Number)
	if err := o.gh.CloseIssue(sess.IssueNumber, fmt.Sprintf("Implemented by PR #%d (auto-merged by maestro).", pr.Number)); err != nil {
		log.Printf("[orch] warning: failed to close issue #%d: %v", sess.IssueNumber, err)
	}
	sess.Status = state.StatusDone
	now := time.Now().UTC()
	sess.FinishedAt = &now
	worker.Stop(o.cfg, slotName, sess)
	o.notifier.Sendf("✅ maestro: merged PR #%d for issue #%d (%s)", pr.Number, sess.IssueNumber, sess.IssueTitle)

	// Auto version bump
	if o.cfg.Versioning.Enabled {
		if err := versioning.Run(o.cfg, o.gh, pr.Number); err != nil {
			log.Printf("[orch] version bump for PR #%d: %v", pr.Number, err)
			o.notifier.Sendf("⚠️ maestro: version bump failed for PR #%d: %v", pr.Number, err)
		} else {
			o.notifier.Sendf("🏷️ maestro: version bumped after PR #%d merge", pr.Number)
		}
	}

	// Deploy hook
	if o.cfg.DeployCmd != "" {
		if err := o.runDeployCmd(pr.Number); err != nil {
			log.Printf("[orch] deploy command failed for PR #%d: %v", pr.Number, err)
			o.notifier.Sendf("⚠️ maestro: deploy failed after PR #%d merge: %v", pr.Number, err)
		} else {
			o.notifier.Sendf("🚀 maestro: deploy succeeded after PR #%d merge", pr.Number)
		}
	}

	return true
}

// runDeployCmd executes the configured deploy command with a 5-minute timeout.
func (o *Orchestrator) runDeployCmd(prNumber int) error {
	log.Printf("[orch] running deploy command after PR #%d merge: %s", prNumber, o.cfg.DeployCmd)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	cmd := exec.CommandContext(ctx, "bash", "-c", o.cfg.DeployCmd)
	cmd.Dir = o.cfg.LocalPath
	out, err := cmd.CombinedOutput()
	if len(out) > 0 {
		log.Printf("[orch] deploy output:\n%s", out)
	}
	if ctx.Err() == context.DeadlineExceeded {
		return fmt.Errorf("deploy command timed out after 5 minutes")
	}
	if err != nil {
		return fmt.Errorf("deploy command failed: %w\n%s", err, out)
	}
	return nil
}

// rebaseConflicts finds PRs with conflicts and rebases them
func (o *Orchestrator) rebaseConflicts(s *state.State) {
	prs, err := o.gh.ListOpenPRs()
	if err != nil {
		log.Printf("[orch] list PRs (rebase): %v", err)
		return
	}

	branchToPR := make(map[string]github.PR)
	for _, pr := range prs {
		branchToPR[pr.HeadRefName] = pr
	}

	for slotName, sess := range s.Sessions {
		if sess.Status != state.StatusPROpen {
			continue
		}
		pr, found := branchToPR[sess.Branch]
		if !found {
			continue
		}

		mergeable, err := o.gh.PRMergeable(pr.Number)
		if err != nil {
			log.Printf("[orch] mergeable PR #%d: %v", pr.Number, err)
			continue
		}

		if mergeable == "CONFLICTING" {
			log.Printf("[orch] PR #%d has conflicts, rebasing %s", pr.Number, slotName)
			if err := worker.RebaseWorktree(sess.Worktree, sess.Branch); err != nil {
				log.Printf("[orch] rebase failed for %s: %v", slotName, err)
				sess.Status = state.StatusConflictFailed
				now := time.Now().UTC()
				sess.FinishedAt = &now
				o.notifier.Sendf("❌ maestro: rebase failed for %s (issue #%d: %s)\n%v",
					slotName, sess.IssueNumber, sess.IssueTitle, err)
			} else {
				log.Printf("[orch] rebase succeeded for %s", slotName)
				o.notifier.Sendf("🔄 maestro: rebased %s (PR #%d) successfully", slotName, pr.Number)
			}
		}
	}
}

// startNewWorkers picks eligible issues and starts workers for them
func (o *Orchestrator) startNewWorkers(s *state.State, slots int) {
	issues, err := o.gh.ListOpenIssues(o.cfg.IssueLabels)
	if err != nil {
		log.Printf("[orch] list issues: %v", err)
		return
	}

	started := 0
	for _, issue := range issues {
		if started >= slots {
			break
		}

		if s.IssueInProgress(issue.Number) {
			continue
		}

		if github.HasLabel(issue, o.cfg.ExcludeLabels) {
			log.Printf("[orch] skipping issue #%d (excluded label)", issue.Number)
			continue
		}

		// Detect model: label for backend selection (label takes precedence) and long-running label
		backendName := ""
		longRunning := false
		for _, label := range issue.Labels {
			if strings.HasPrefix(label.Name, "model:") {
				if name := strings.TrimPrefix(label.Name, "model:"); name != "" {
					backendName = name
					log.Printf("[router] issue #%d → %s (label override)", issue.Number, backendName)
				}
			}
			if strings.EqualFold(label.Name, "long-running") {
				longRunning = true
			}
		}

		// If no label, try auto-routing via LLM
		if backendName == "" && o.cfg.Routing.Mode == "auto" {
			routedBackend, reason, err := o.router.Route(issue)
			if err != nil {
				log.Printf("[router] issue #%d: error %v — using default", issue.Number, err)
			} else {
				log.Printf("[router] issue #%d → %s (%s)", issue.Number, routedBackend, reason)
			}
			backendName = routedBackend
		}

		// Fall back to default
		if backendName == "" {
			backendName = o.cfg.Model.Default
		}

		promptBase := o.selectPrompt(issue)
		log.Printf("[orch] starting worker for issue #%d: %s (backend=%s, long_running=%v)", issue.Number, issue.Title, backendName, longRunning)
		slotName, err := worker.Start(o.cfg, s, o.repo, issue, promptBase, backendName)
		if err != nil {
			log.Printf("[orch] start worker for issue #%d: %v", issue.Number, err)
			o.notifier.Sendf("❌ maestro: failed to start worker for issue #%d (%s): %v",
				issue.Number, issue.Title, err)
			continue
		}

		if longRunning {
			s.Sessions[slotName].LongRunning = true
		}
		o.notifier.Sendf("🚀 maestro: started worker %s for issue #%d: %s", slotName, issue.Number, issue.Title)
		started++
	}

	if started == 0 {
		log.Printf("[orch] no new workers started (%d issues checked)", len(issues))
	}
}
