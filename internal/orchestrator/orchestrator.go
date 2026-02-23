package orchestrator

import (
	"fmt"
	"log"
	"os"
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
}

// New creates a new Orchestrator
func New(cfg *config.Config) *Orchestrator {
	return &Orchestrator{
		cfg:      cfg,
		notifier: notify.NewWithToken(cfg.Telegram.BotToken, cfg.Telegram.Target, cfg.Telegram.OpenclawURL),
		gh:       github.New(cfg.Repo),
		router:   router.New(cfg),
		repo:     cfg.Repo,
	}
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

	// Step 1: Check running sessions for dead processes / stale / closed issues
	o.checkSessions(s)

	// Step 2: Auto-merge green PRs
	o.autoMergePRs(s)

	// Step 3: Rebase conflicting PRs
	o.rebaseConflicts(s)

	// Save after all checks
	if err := state.Save(o.cfg.StateDir, s); err != nil {
		return fmt.Errorf("save state: %w", err)
	}

	// Step 4: Start new workers for available slots
	active := len(s.ActiveSessions())
	slots := o.cfg.MaxParallel - active
	log.Printf("[orch] active=%d max=%d available_slots=%d", active, o.cfg.MaxParallel, slots)

	if slots > 0 {
		o.startNewWorkers(s, slots)
		if err := state.Save(o.cfg.StateDir, s); err != nil {
			return fmt.Errorf("save state after workers: %w", err)
		}
	}

	log.Printf("[orch] === cycle done ===")
	return nil
}

// Run loops with the given interval; if once=true, runs once and returns
func (o *Orchestrator) Run(interval time.Duration, once bool) error {
	if err := o.RunOnce(); err != nil {
		log.Printf("[orch] run error: %v", err)
	}
	if once {
		return nil
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for range ticker.C {
		if err := o.RunOnce(); err != nil {
			log.Printf("[orch] run error: %v", err)
		}
	}
	return nil
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
				} else {
					log.Printf("[orch] worker %s (pid=%d) is dead", slotName, sess.PID)
					sess.Status = state.StatusDead
					now := time.Now().UTC()
					sess.FinishedAt = &now
					o.notifier.Sendf("⚠️ maestro: worker %s (issue #%d: %s) process died.\nCheck log: %s",
						slotName, sess.IssueNumber, sess.IssueTitle, sess.LogFile)
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

			// Check if worker exceeded max runtime — kill if so
			maxMinutes := o.cfg.MaxRuntimeMinutes
			if sess.LongRunning {
				maxMinutes *= 2
			}
			maxRuntime := time.Duration(maxMinutes) * time.Minute
			age := time.Since(sess.StartedAt)
			if age > maxRuntime {
				log.Printf("[orch] worker %s exceeded max runtime (%dm), killing", slotName, maxMinutes)
				worker.Stop(o.cfg, slotName, sess)
				sess.Status = state.StatusDead
				now := time.Now().UTC()
				sess.FinishedAt = &now
				o.notifier.Sendf("💀 maestro: worker for #%d killed after %dm (max runtime exceeded)",
					sess.IssueNumber, int(age.Minutes()))
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
			log.Printf("[orch] merging PR #%d (branch %s)", pr.Number, sess.Branch)
			if err := o.gh.MergePR(pr.Number); err != nil {
				log.Printf("[orch] merge PR #%d: %v", pr.Number, err)
				o.notifier.Sendf("❌ maestro: failed to merge PR #%d (%s): %v", pr.Number, sess.Branch, err)
			} else {
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
			}
		case "failure":
			o.notifier.Sendf("❌ maestro: CI failing for PR #%d (%s, issue #%d)", pr.Number, sess.Branch, sess.IssueNumber)
		}
	}
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
