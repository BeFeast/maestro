package orchestrator

import (
	"bufio"
	"context"
	"crypto/sha256"
	"fmt"
	"log"
	"os"
	"os/exec"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/github"
	"github.com/befeast/maestro/internal/mission"
	"github.com/befeast/maestro/internal/notify"
	"github.com/befeast/maestro/internal/pipeline"
	"github.com/befeast/maestro/internal/router"
	"github.com/befeast/maestro/internal/state"
	"github.com/befeast/maestro/internal/supervisor"
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
	listOpenPRsFn         func() ([]github.PR, error)
	remoteBranchExistsFn  func(branch string) (bool, error)
	createPRFn            func(title, body, base, head string) (int, error)
	hasOpenPRForIssueFn   func(issueNumber int) (bool, error)
	hasMergedPRForIssueFn func(issueNumber int) (bool, error)
	isPRMergedFn          func(prNumber int) (bool, error)

	// Testing hooks for checkSessions
	captureTmuxFn   func(session string) (string, error)
	tmuxCaptureFn   func(session string) (string, error)
	isIssueClosedFn func(issueNumber int) (bool, error)
	addIssueLabelFn func(number int, label string) error
	isRateLimitedFn func(logFile string) bool
	// workerRespawnFn / respawnWorkerFn: used by respawnWorker() for dead-worker fallback (tests set one or the other)
	workerRespawnFn func(cfg *config.Config, slotName string, sess *state.Session, repo string, issue github.Issue, promptBase string, backendName string) error
	respawnWorkerFn func(cfg *config.Config, slotName string, sess *state.Session, repo string, issue github.Issue, promptBase string, backendName string) error
	getIssueFn      func(number int) (github.Issue, error)
	// saveCheckpointFn / respawnInPlaceFn: used for soft token threshold checkpoint+respawn
	saveCheckpointFn func(sess *state.Session) (string, error)
	respawnInPlaceFn func(cfg *config.Config, slotName string, sess *state.Session, repo string, issue github.Issue, promptBase string, backendName string) error

	// Testing hooks for pipeline phase transitions
	workerStartPhaseFn func(cfg *config.Config, sess *state.Session, slotName, prompt, backendName string) error

	// Testing hooks for startNewWorkers
	listOpenIssuesFn func(labels []string) ([]github.Issue, error)
	workerStartFn    func(cfg *config.Config, s *state.State, repo string, issue github.Issue, promptBase, backend string) (string, error)

	// Cached project board field (discovered once per run cycle, nil if disabled)
	projectField *github.ProjectField

	// Mission processor (nil when missions disabled)
	missionProc *mission.Processor

	// Config hot-reload channel (nil = disabled, safe in select)
	configReloadCh <-chan *config.Config

	// Testing hooks for autoMergePRs / mergeReadyPR
	ghPRCIStatusFn              func(prNumber int) (string, error)
	ghPRGreptileApprovedFn      func(prNumber int) (approved bool, pending bool, err error)
	ghMergePRFn                 func(prNumber int) error
	ghClosePRFn                 func(prNumber int, comment string) error
	ghPRChecksOutputFn          func(prNumber int) (string, error)
	ghCollectPRReviewFeedbackFn func(prNumber int) (string, error)
	ghCloseIssueFn              func(number int, comment string) error
	workerStopFn                func(cfg *config.Config, slotName string, sess *state.Session) error
	rebaseWorktreeFn            func(worktreePath, branch string, autoResolveFiles, autoRestoreFiles []string) error
}

// New creates a new Orchestrator
func New(cfg *config.Config) *Orchestrator {
	n := notify.NewWithToken(cfg.Telegram.BotToken, cfg.Telegram.Target, cfg.Telegram.Mode, cfg.Telegram.OpenclawURL)
	if cfg.Telegram.DigestMode {
		n.SetDigestMode(true)
		log.Printf("[orch] digest mode enabled — notifications will be batched per cycle")
	}
	gh := github.New(cfg.Repo)
	o := &Orchestrator{
		cfg:      cfg,
		notifier: n,
		gh:       gh,
		router:   router.New(cfg),
		repo:     cfg.Repo,
	}
	if cfg.Missions.Enabled {
		o.missionProc = mission.NewProcessor(cfg, gh)
		log.Printf("[orch] mission mode enabled (max_children=%d, labels=%v)", cfg.Missions.MaxChildren, cfg.Missions.Labels)
	}
	return o
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

func (o *Orchestrator) listOpenPRs() ([]github.PR, error) {
	if o.listOpenPRsFn != nil {
		return o.listOpenPRsFn()
	}
	return o.gh.ListOpenPRs()
}

func (o *Orchestrator) remoteBranchExists(branch string) (bool, error) {
	if o.remoteBranchExistsFn != nil {
		return o.remoteBranchExistsFn(branch)
	}
	branch = strings.TrimSpace(branch)
	if branch == "" || o.cfg == nil || strings.TrimSpace(o.cfg.LocalPath) == "" {
		return false, nil
	}
	out, err := exec.Command("git", "-C", o.cfg.LocalPath, "ls-remote", "--exit-code", "--heads", "origin", branch).CombinedOutput()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 2 {
			return false, nil
		}
		return false, fmt.Errorf("git ls-remote --heads origin %s: %w\n%s", branch, err, out)
	}
	return true, nil
}

func (o *Orchestrator) createPR(title, body, base, head string) (int, error) {
	if o.createPRFn != nil {
		return o.createPRFn(title, body, base, head)
	}
	return o.gh.CreatePR(title, body, base, head)
}

func (o *Orchestrator) hasOpenPRForIssue(issueNumber int) (bool, error) {
	if o.hasOpenPRForIssueFn != nil {
		return o.hasOpenPRForIssueFn(issueNumber)
	}
	return o.gh.HasOpenPRForIssue(issueNumber)
}

func (o *Orchestrator) hasMergedPRForIssue(issueNumber int) (bool, error) {
	if o.hasMergedPRForIssueFn != nil {
		return o.hasMergedPRForIssueFn(issueNumber)
	}
	return o.gh.HasMergedPRForIssue(issueNumber)
}

func (o *Orchestrator) isPRMerged(prNumber int) (bool, error) {
	if o.isPRMergedFn != nil {
		return o.isPRMergedFn(prNumber)
	}
	return o.gh.IsPRMerged(prNumber)
}

func (o *Orchestrator) prCIStatus(prNumber int) (string, error) {
	if o.ghPRCIStatusFn != nil {
		return o.ghPRCIStatusFn(prNumber)
	}
	return o.gh.PRCIStatus(prNumber)
}

func (o *Orchestrator) prGreptileApproved(prNumber int) (bool, bool, error) {
	if o.ghPRGreptileApprovedFn != nil {
		return o.ghPRGreptileApprovedFn(prNumber)
	}
	return o.gh.PRGreptileApproved(prNumber)
}

func (o *Orchestrator) mergePR(prNumber int) error {
	if o.ghMergePRFn != nil {
		return o.ghMergePRFn(prNumber)
	}
	return o.gh.MergePR(prNumber)
}

func (o *Orchestrator) closePR(prNumber int, comment string) error {
	if o.ghClosePRFn != nil {
		return o.ghClosePRFn(prNumber, comment)
	}
	return o.gh.ClosePR(prNumber, comment)
}

func (o *Orchestrator) prChecksOutput(prNumber int) (string, error) {
	if o.ghPRChecksOutputFn != nil {
		return o.ghPRChecksOutputFn(prNumber)
	}
	return o.gh.PRChecksOutput(prNumber)
}

func (o *Orchestrator) collectPRReviewFeedback(prNumber int) (string, error) {
	if o.ghCollectPRReviewFeedbackFn != nil {
		return o.ghCollectPRReviewFeedbackFn(prNumber)
	}
	return o.gh.CollectPRReviewFeedback(prNumber)
}

func (o *Orchestrator) closeIssue(number int, comment string) error {
	if o.ghCloseIssueFn != nil {
		return o.ghCloseIssueFn(number, comment)
	}
	return o.gh.CloseIssue(number, comment)
}

func (o *Orchestrator) stopWorker(slotName string, sess *state.Session) error {
	if o.workerStopFn != nil {
		return o.workerStopFn(o.cfg, slotName, sess)
	}
	return worker.Stop(o.cfg, slotName, sess)
}

func (o *Orchestrator) getIssue(number int) (github.Issue, error) {
	if o.getIssueFn != nil {
		return o.getIssueFn(number)
	}
	return o.gh.GetIssue(number)
}

func (o *Orchestrator) respawnWorker(slotName string, sess *state.Session, issue github.Issue, promptBase string, backendName string) error {
	// Support both hook names for test compatibility (respawnWorkerFn = branch, workerRespawnFn = HEAD)
	if o.respawnWorkerFn != nil {
		return o.respawnWorkerFn(o.cfg, slotName, sess, o.repo, issue, promptBase, backendName)
	}
	if o.workerRespawnFn != nil {
		return o.workerRespawnFn(o.cfg, slotName, sess, o.repo, issue, promptBase, backendName)
	}
	return worker.Respawn(o.cfg, slotName, sess, o.repo, issue, promptBase, backendName)
}

func (o *Orchestrator) saveCheckpoint(sess *state.Session) (string, error) {
	if o.saveCheckpointFn != nil {
		return o.saveCheckpointFn(sess)
	}
	return worker.SaveCheckpoint(sess)
}

func (o *Orchestrator) respawnInPlace(slotName string, sess *state.Session, issue github.Issue, promptBase string, backendName string) error {
	if o.respawnInPlaceFn != nil {
		return o.respawnInPlaceFn(o.cfg, slotName, sess, o.repo, issue, promptBase, backendName)
	}
	return worker.RespawnInPlace(o.cfg, slotName, sess, o.repo, issue, promptBase, backendName)
}

func (o *Orchestrator) rebaseWorktree(worktreePath, branch string) error {
	if o.rebaseWorktreeFn != nil {
		return o.rebaseWorktreeFn(worktreePath, branch, o.cfg.AutoResolveFiles, o.cfg.AutoRestoreFiles)
	}
	return worker.RebaseWorktree(worktreePath, branch, o.cfg.AutoResolveFiles, o.cfg.AutoRestoreFiles)
}

func (o *Orchestrator) captureTmux(session string) (string, error) {
	if o.tmuxCaptureFn != nil {
		return o.tmuxCaptureFn(session)
	}
	if o.captureTmuxFn != nil {
		return o.captureTmuxFn(session)
	}
	return tmuxCapture(session)
}

func (o *Orchestrator) isIssueClosed(number int) (bool, error) {
	if o.isIssueClosedFn != nil {
		return o.isIssueClosedFn(number)
	}
	return o.gh.IsIssueClosed(number)
}

func (o *Orchestrator) addIssueLabel(number int, label string) error {
	if o.addIssueLabelFn != nil {
		return o.addIssueLabelFn(number, label)
	}
	return o.gh.AddIssueLabel(number, label)
}

func (o *Orchestrator) isRateLimited(logFile string) bool {
	if o.isRateLimitedFn != nil {
		return o.isRateLimitedFn(logFile)
	}
	return worker.IsRateLimited(logFile)
}

// runAfterRunHook executes the after_run hook for a session (best-effort, never fatal).
func (o *Orchestrator) runAfterRunHook(sess *state.Session) {
	if o.cfg.Hooks.AfterRun == "" {
		return
	}
	env := worker.HookEnv{
		IssueID:       fmt.Sprintf("%s#%d", o.cfg.Repo, sess.IssueNumber),
		IssueNumber:   sess.IssueNumber,
		WorkspacePath: sess.Worktree,
	}
	if err := worker.RunHook(o.cfg, "after_run", o.cfg.Hooks.AfterRun, env); err != nil {
		log.Printf("[orch] after_run hook failed for issue #%d: %v", sess.IssueNumber, err)
	}
}

// nextFallbackBackend returns the next untried backend from the fallback list.
// It skips backends that are already in sess.TriedBackends or match the current backend.
// Returns "" if no fallback is available.
func (o *Orchestrator) nextFallbackBackend(sess *state.Session) string {
	fallbacks := o.cfg.Model.FallbackBackends
	if len(fallbacks) == 0 {
		return ""
	}

	tried := make(map[string]bool, len(sess.TriedBackends)+1)
	tried[sess.Backend] = true
	for _, b := range sess.TriedBackends {
		tried[b] = true
	}

	for _, fb := range fallbacks {
		if !tried[fb] {
			// Verify the backend exists in config
			if _, ok := o.cfg.Model.Backends[fb]; ok {
				return fb
			}
		}
	}
	return ""
}

// ensureProjectField discovers the project board field if not cached.
func (o *Orchestrator) ensureProjectField() {
	if !o.cfg.GitHubProjects.Enabled || o.cfg.GitHubProjects.ProjectNumber == 0 {
		return
	}
	if o.projectField != nil {
		return
	}
	pf, err := o.gh.DiscoverProject(o.cfg.GitHubProjects.ProjectNumber)
	if err != nil {
		log.Printf("[orch] discover project: %v", err)
		return
	}
	o.projectField = pf
}

// syncProject syncs an issue's status to the configured GitHub Project board.
// No-op if github_projects is not enabled.
func (o *Orchestrator) syncProject(issueNumber int, status github.ProjectStatus) {
	if !o.cfg.GitHubProjects.Enabled || o.cfg.GitHubProjects.ProjectNumber == 0 {
		return
	}
	o.ensureProjectField()
	// Map old ProjectStatus constants to real column names
	statusName := map[github.ProjectStatus]string{
		github.ProjectStatusTodo:       "Todo",
		github.ProjectStatusInProgress: "In Progress",
		github.ProjectStatusDone:       "Done",
	}[status]
	if statusName == "" {
		statusName = string(status)
	}
	o.gh.SyncIssueStatus(o.projectField, issueNumber, statusName)
}

// reconcileProjectBoard moves closed issues to Done on the project board.
// This catches issues closed externally (manual merge, GitHub auto-close, manual close).
func (o *Orchestrator) reconcileProjectBoard() {
	if !o.cfg.GitHubProjects.Enabled || o.cfg.GitHubProjects.ProjectNumber == 0 {
		return
	}
	o.ensureProjectField()
	if o.projectField == nil {
		return
	}

	items, err := o.gh.ListNonDoneProjectItems(o.projectField)
	if err != nil {
		log.Printf("[orch] reconcile project board: %v", err)
		return
	}

	for _, item := range items {
		if item.IssueClosed {
			log.Printf("[orch] reconcile: issue #%d is closed, moving to Done", item.IssueNumber)
			o.gh.SyncIssueStatus(o.projectField, item.IssueNumber, "Done")
		} else if !item.HasStatus {
			log.Printf("[orch] reconcile: issue #%d has no status, setting to Todo", item.IssueNumber)
			o.gh.SyncIssueStatus(o.projectField, item.IssueNumber, "Todo")
		}
	}
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

// retryBackoffMs computes the exponential backoff delay for a retry attempt.
// Formula: min(10000 * 2^(attempt-1), maxRetryBackoffMs).
// attempt is 1-based (the first retry is attempt 1).
func retryBackoffMs(attempt, maxRetryBackoffMs int) int {
	if attempt <= 0 {
		attempt = 1
	}
	delay := 10000 // 10 seconds base
	for i := 1; i < attempt; i++ {
		delay *= 2
		if delay > maxRetryBackoffMs {
			return maxRetryBackoffMs
		}
	}
	if delay > maxRetryBackoffMs {
		return maxRetryBackoffMs
	}
	return delay
}

// appendCIFailureContext appends a section to the worker prompt describing
// what went wrong in the previous CI run, so the new worker can fix it.
func appendCIFailureContext(promptBase, ciOutput string, attempt int) string {
	return fmt.Sprintf(`%s

---

## IMPORTANT: Previous CI Failure (Attempt %d)

A previous worker created a PR for this issue, but CI failed. The PR has been closed.
You are a retry worker — please fix the CI failures described below.

**CI output from the failed run:**
`+"```"+`
%s
`+"```"+`

Focus on fixing the CI failures while still implementing the issue requirements.
`, promptBase, attempt, ciOutput)
}

// appendReviewFeedbackContext appends a section to the worker prompt with
// code review findings from the previous failed attempt.
func appendReviewFeedbackContext(promptBase, feedback string) string {
	return fmt.Sprintf(`%s

### Code Review Findings

The following code review comments were left on the previous PR. Address ALL of these issues:

%s

IMPORTANT: Address ALL code review findings above before creating a new PR.
Do NOT repeat the same mistakes.
`, promptBase, feedback)
}

// appendRebaseConflictContext appends a section to the worker prompt with
// auto-rebase failure details so the retry worker can update the same PR branch.
func appendRebaseConflictContext(promptBase, feedback string) string {
	return fmt.Sprintf(`%s

### Rebase Conflict

Maestro tried to update the existing PR branch against origin/main, but git rebase hit conflicts.
You are a retry worker running in the same worktree and branch.

Resolve the conflicts, keep the PR focused on the original issue, run validation, commit the fix, and push to the existing PR branch.
Do NOT open a second PR.

Rebase failure details:
`+"```"+`
%s
`+"```"+`
`, promptBase, feedback)
}

// canRetryIssue returns true if the session can be retried, considering
// both the session's retry count and the global max_retries_per_issue config.
// When max_retries_per_issue is 0 (unlimited), retries are always allowed.
func (o *Orchestrator) canRetryIssue(s *state.State, sess *state.Session) bool {
	maxRetries := o.cfg.MaxRetriesPerIssue
	if maxRetries <= 0 {
		return true // unlimited retries
	}
	totalAttempts := s.FailedAttemptsForIssue(sess.IssueNumber) + sess.RetryCount
	return totalAttempts < maxRetries
}

func pendingRetryReservations(s *state.State) int {
	now := time.Now().UTC()
	count := 0
	for _, sess := range s.Sessions {
		if sess.Status == state.StatusDead && sess.NextRetryAt != nil && !now.Before(*sess.NextRetryAt) {
			count++
		}
	}
	return count
}

// respawnDueRetries checks dead sessions with a scheduled retry time and
// respawns them when the backoff period has elapsed.
func (o *Orchestrator) respawnDueRetries(s *state.State, slots int) {
	if slots <= 0 {
		if pending := pendingRetryReservations(s); pending > 0 {
			log.Printf("[orch] retry queue has %d pending session(s), but no worker slots are available", pending)
		}
		return
	}

	slotNames := make([]string, 0, len(s.Sessions))
	for slotName := range s.Sessions {
		slotNames = append(slotNames, slotName)
	}
	sort.Strings(slotNames)

	respawned := 0
	for _, slotName := range slotNames {
		if respawned >= slots {
			log.Printf("[orch] retry queue still has pending session(s), but retry slots are exhausted")
			return
		}

		sess := s.Sessions[slotName]
		if sess.Status != state.StatusDead {
			continue
		}
		if sess.NextRetryAt == nil {
			continue
		}
		if time.Now().UTC().Before(*sess.NextRetryAt) {
			log.Printf("[orch] worker %s retry %d waiting until %s",
				slotName, sess.RetryCount, sess.NextRetryAt.Format(time.RFC3339))
			continue
		}

		// Backoff elapsed — respawn the worker
		log.Printf("[orch] worker %s backoff elapsed, respawning (retry %d)", slotName, sess.RetryCount)
		sess.NextRetryAt = nil

		issue, err := o.getIssue(sess.IssueNumber)
		if err != nil {
			log.Printf("[orch] fetch issue #%d for retry: %v — marking as failed", sess.IssueNumber, err)
			sess.Status = state.StatusFailed
			now := time.Now().UTC()
			sess.FinishedAt = &now
			o.notifier.Sendf("💀 maestro: worker %s (issue #%d: %s) retry failed (could not fetch issue)",
				slotName, sess.IssueNumber, sess.IssueTitle)
			continue
		}

		promptBase := o.selectPrompt(issue)

		// If this is a CI failure retry, include CI output and review feedback
		// in the prompt so the new worker knows what went wrong.
		if sess.CIFailureOutput != "" {
			promptBase = appendCIFailureContext(promptBase, sess.CIFailureOutput, sess.RetryCount)
			sess.CIFailureOutput = "" // consumed — don't persist stale output
		}
		if sess.PreviousAttemptFeedback != "" {
			if sess.PreviousAttemptFeedbackKind == "rebase_conflict" {
				promptBase = appendRebaseConflictContext(promptBase, sess.PreviousAttemptFeedback)
			} else {
				if sess.PreviousAttemptFeedbackKind == state.RetryReasonReviewFeedback {
					sess.RetryReason = state.RetryReasonReviewFeedback
				}
				promptBase = appendReviewFeedbackContext(promptBase, sess.PreviousAttemptFeedback)
			}
			sess.PreviousAttemptFeedback = "" // consumed — don't persist stale feedback
			sess.PreviousAttemptFeedbackKind = ""
		}

		var respawnErr error
		if sess.PRNumber != 0 && sess.Worktree != "" {
			respawnErr = o.respawnInPlace(slotName, sess, issue, promptBase, sess.Backend)
		} else {
			respawnErr = o.respawnWorker(slotName, sess, issue, promptBase, sess.Backend)
		}
		if respawnErr != nil {
			log.Printf("[orch] respawn worker %s: %v — marking as failed", slotName, respawnErr)
			sess.Status = state.StatusFailed
			now := time.Now().UTC()
			sess.FinishedAt = &now
			o.notifier.Sendf("💀 maestro: worker %s (issue #%d: %s) respawn failed: %v",
				slotName, sess.IssueNumber, sess.IssueTitle, respawnErr)
			continue
		}

		o.notifier.Sendf("🔄 maestro: retrying worker %s for issue #%d: %s (attempt %d)",
			slotName, sess.IssueNumber, sess.IssueTitle, sess.RetryCount)
		respawned++
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

	// Step 2b: Respawn dead sessions whose backoff has elapsed
	retrySlots := availableSlots(o.cfg, s, len(s.ActiveSessions()))
	o.respawnDueRetries(s, retrySlots)

	// Step 3: Auto-merge green PRs
	o.autoMergePRs(s)

	// Step 4: Rebase conflicting PRs
	o.rebaseConflicts(s)

	// Step 4b: Process missions (decompose new epics, update progress)
	if o.missionProc != nil {
		issues, err := o.listOpenIssues(o.cfg.IssueLabels)
		if err != nil {
			log.Printf("[orch] list issues for missions: %v", err)
		} else {
			o.missionProc.ProcessMissions(s, issues)
		}
	}

	// Save after all checks/reconciliation
	if err := state.Save(o.cfg.StateDir, s); err != nil {
		return fmt.Errorf("save state: %w", err)
	}

	// Step 5: Start new workers for available slots
	active := len(s.ActiveSessions())
	slots := availableSlots(o.cfg, s, active)
	if reserved := pendingRetryReservations(s); reserved > 0 && slots > 0 {
		if reserved > slots {
			reserved = slots
		}
		slots -= reserved
		log.Printf("[orch] reserving %d worker slot(s) for scheduled retries", reserved)
	}
	log.Printf("[orch] active=%d max=%d available_slots=%d", active, o.cfg.MaxParallel, slots)

	if slots > 0 {
		o.startNewWorkers(s, slots)
		if err := state.Save(o.cfg.StateDir, s); err != nil {
			return fmt.Errorf("save state after workers: %w", err)
		}
	}

	// Step 6: Reconcile project board — move externally-closed issues to Done
	o.reconcileProjectBoard()

	// Flush digest buffer (no-op if digest mode is off or buffer is empty)
	if err := o.notifier.Flush(); err != nil {
		log.Printf("[orch] digest flush: %v", err)
	}

	log.Printf("[orch] === cycle done ===")
	return nil
}

// SetConfigReloadCh sets the channel used to receive hot-reloaded configs.
// A nil channel is safe (select case is never chosen).
func (o *Orchestrator) SetConfigReloadCh(ch <-chan *config.Config) {
	o.configReloadCh = ch
}

// Run loops with the given interval; if once=true, runs once and returns.
// The context can be used to stop the loop (e.g. for multi-project shutdown).
// An optional refreshCh triggers an immediate poll cycle when a value is received.
func (o *Orchestrator) Run(ctx context.Context, interval time.Duration, once bool, refreshCh <-chan struct{}) error {
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
		case <-refreshCh:
			log.Printf("[orch] refresh triggered via API")
			if err := o.RunOnce(); err != nil {
				log.Printf("[orch] run error: %v", err)
			}
		case newCfg := <-o.configReloadCh:
			o.reloadConfig(newCfg, &ticker)
		}
	}
}

// reloadConfig applies non-destructive config changes at runtime.
// Fields that require a restart (repo, model.default) are logged as warnings.
func (o *Orchestrator) reloadConfig(newCfg *config.Config, ticker **time.Ticker) {
	old := o.cfg
	var changed []string

	// Restart-required fields — warn only, do not apply
	if newCfg.Repo != old.Repo {
		log.Printf("[orch] config reload: repo changed (%s → %s) — requires restart", old.Repo, newCfg.Repo)
	}
	if newCfg.Model.Default != old.Model.Default {
		log.Printf("[orch] config reload: model.default changed (%s → %s) — requires restart", old.Model.Default, newCfg.Model.Default)
	}

	// Hot-reloadable fields
	if newCfg.MaxParallel != old.MaxParallel {
		changed = append(changed, fmt.Sprintf("max_parallel: %d→%d", old.MaxParallel, newCfg.MaxParallel))
		o.cfg.MaxParallel = newCfg.MaxParallel
	}
	if newCfg.MaxRuntimeMinutes != old.MaxRuntimeMinutes {
		changed = append(changed, fmt.Sprintf("max_runtime_minutes: %d→%d", old.MaxRuntimeMinutes, newCfg.MaxRuntimeMinutes))
		o.cfg.MaxRuntimeMinutes = newCfg.MaxRuntimeMinutes
	}
	if newCfg.MaxRetriesPerIssue != old.MaxRetriesPerIssue {
		changed = append(changed, fmt.Sprintf("max_retries_per_issue: %d→%d", old.MaxRetriesPerIssue, newCfg.MaxRetriesPerIssue))
		o.cfg.MaxRetriesPerIssue = newCfg.MaxRetriesPerIssue
	}
	if newCfg.WorkerSilentTimeoutMinutes != old.WorkerSilentTimeoutMinutes {
		changed = append(changed, fmt.Sprintf("worker_silent_timeout_minutes: %d→%d", old.WorkerSilentTimeoutMinutes, newCfg.WorkerSilentTimeoutMinutes))
		o.cfg.WorkerSilentTimeoutMinutes = newCfg.WorkerSilentTimeoutMinutes
	}
	if newCfg.WorkerMaxTokens != old.WorkerMaxTokens {
		changed = append(changed, fmt.Sprintf("worker_max_tokens: %d→%d", old.WorkerMaxTokens, newCfg.WorkerMaxTokens))
		o.cfg.WorkerMaxTokens = newCfg.WorkerMaxTokens
	}
	if !strSliceEqual(newCfg.IssueLabels, old.IssueLabels) {
		changed = append(changed, fmt.Sprintf("issue_labels: %v→%v", old.IssueLabels, newCfg.IssueLabels))
		o.cfg.IssueLabels = newCfg.IssueLabels
	}
	if !strSliceEqual(newCfg.ExcludeLabels, old.ExcludeLabels) {
		changed = append(changed, fmt.Sprintf("exclude_labels: %v→%v", old.ExcludeLabels, newCfg.ExcludeLabels))
		o.cfg.ExcludeLabels = newCfg.ExcludeLabels
	}
	if !reflect.DeepEqual(newCfg.Supervisor, old.Supervisor) {
		changed = append(changed, "supervisor policy")
		o.cfg.Supervisor = newCfg.Supervisor
	}
	if newCfg.MergeStrategy != old.MergeStrategy {
		changed = append(changed, fmt.Sprintf("merge_strategy: %s→%s", old.MergeStrategy, newCfg.MergeStrategy))
		o.cfg.MergeStrategy = newCfg.MergeStrategy
	}
	if newCfg.MergeIntervalSeconds != old.MergeIntervalSeconds {
		changed = append(changed, fmt.Sprintf("merge_interval_seconds: %d→%d", old.MergeIntervalSeconds, newCfg.MergeIntervalSeconds))
		o.cfg.MergeIntervalSeconds = newCfg.MergeIntervalSeconds
	}
	if newCfg.ReviewGate != old.ReviewGate {
		changed = append(changed, fmt.Sprintf("review_gate: %s→%s", old.ReviewGate, newCfg.ReviewGate))
		o.cfg.ReviewGate = newCfg.ReviewGate
	}
	if newCfg.AutoRetryReviewFeedback != old.AutoRetryReviewFeedback {
		changed = append(changed, fmt.Sprintf("auto_retry_review_feedback: %v→%v", old.AutoRetryReviewFeedback, newCfg.AutoRetryReviewFeedback))
		o.cfg.AutoRetryReviewFeedback = newCfg.AutoRetryReviewFeedback
	}
	if newCfg.AutoRetryRebaseConflicts != old.AutoRetryRebaseConflicts {
		changed = append(changed, fmt.Sprintf("auto_retry_rebase_conflicts: %v→%v", old.AutoRetryRebaseConflicts, newCfg.AutoRetryRebaseConflicts))
		o.cfg.AutoRetryRebaseConflicts = newCfg.AutoRetryRebaseConflicts
	}
	if newCfg.DeployCmd != old.DeployCmd {
		changed = append(changed, fmt.Sprintf("deploy_cmd: %q→%q", old.DeployCmd, newCfg.DeployCmd))
		o.cfg.DeployCmd = newCfg.DeployCmd
	}
	if newCfg.DeployTimeoutMinutes != old.DeployTimeoutMinutes {
		changed = append(changed, fmt.Sprintf("deploy_timeout_minutes: %d→%d", old.DeployTimeoutMinutes, newCfg.DeployTimeoutMinutes))
		o.cfg.DeployTimeoutMinutes = newCfg.DeployTimeoutMinutes
	}
	if newCfg.AutoRebase != old.AutoRebase {
		changed = append(changed, fmt.Sprintf("auto_rebase: %v→%v", old.AutoRebase, newCfg.AutoRebase))
		o.cfg.AutoRebase = newCfg.AutoRebase
	}

	// Reload prompt files if paths changed
	if newCfg.WorkerPrompt != old.WorkerPrompt {
		changed = append(changed, fmt.Sprintf("worker_prompt: %q→%q", old.WorkerPrompt, newCfg.WorkerPrompt))
		o.cfg.WorkerPrompt = newCfg.WorkerPrompt
		o.LoadPromptBase("")
	}
	if newCfg.BugPrompt != old.BugPrompt {
		changed = append(changed, fmt.Sprintf("bug_prompt: %q→%q", old.BugPrompt, newCfg.BugPrompt))
		o.cfg.BugPrompt = newCfg.BugPrompt
		o.LoadPromptBase("")
	}
	if newCfg.EnhancementPrompt != old.EnhancementPrompt {
		changed = append(changed, fmt.Sprintf("enhancement_prompt: %q→%q", old.EnhancementPrompt, newCfg.EnhancementPrompt))
		o.cfg.EnhancementPrompt = newCfg.EnhancementPrompt
		o.LoadPromptBase("")
	}

	// Poll interval change — reset the ticker
	if newCfg.PollIntervalSeconds != old.PollIntervalSeconds && newCfg.PollIntervalSeconds > 0 {
		newInterval := time.Duration(newCfg.PollIntervalSeconds) * time.Second
		changed = append(changed, fmt.Sprintf("poll_interval_seconds: %d→%d", old.PollIntervalSeconds, newCfg.PollIntervalSeconds))
		o.cfg.PollIntervalSeconds = newCfg.PollIntervalSeconds
		(*ticker).Reset(newInterval)
	}

	// Hot-reload hooks config
	if newCfg.Hooks != old.Hooks {
		changed = append(changed, "hooks")
		o.cfg.Hooks = newCfg.Hooks
	}

	if len(changed) == 0 {
		log.Printf("[orch] config reloaded — no effective changes")
		return
	}
	log.Printf("[orch] config reloaded — changed: %s", strings.Join(changed, ", "))
}

func strSliceEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// reconcileRunningSessions self-heals stale "running" sessions.
// If a session is marked running but either its PID is dead/missing OR its tmux session
// is missing, the session is transitioned to a terminal state.
//
// Before marking a session dead, it checks whether the worker already opened a PR
// for its branch. If a PR exists, the session transitions to pr_open instead of dead.
// This prevents the infinite-spawn loop where reconcile kills a session whose worker
// had successfully created a PR before the tmux session was cleaned up.
func (o *Orchestrator) reconcileRunningSessions(s *state.State) bool {
	// Fetch open PRs once — used to rescue sessions where the worker exited
	// after creating a PR (process/tmux gone, but PR is already open on GitHub).
	prs, prErr := o.listOpenPRs()
	branchToPR := make(map[string]github.PR)
	if prErr != nil {
		log.Printf("[orch] reconcile: warn — could not list PRs: %v (will mark stale sessions dead)", prErr)
	} else {
		for _, pr := range prs {
			branchToPR[pr.HeadRefName] = pr
		}
	}

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

		// Worker process/session is gone. Before marking dead, check whether it
		// already opened a PR. If so, transition to pr_open — the worker succeeded.
		// Without this check, reconcile would mark the session dead, causing
		// IssueInProgress to return false and startNewWorkers to spawn a duplicate.
		if pr, found := branchToPR[sess.Branch]; found {
			log.Printf("[orch] reconcile: %s running->pr_open (PR #%d already open for branch %q; %s)",
				slotName, pr.Number, sess.Branch, strings.Join(reasons, ", "))
			sess.Status = state.StatusPROpen
			sess.PRNumber = pr.Number
			sess.PID = 0
			sess.TmuxSession = ""
			now := time.Now().UTC()
			sess.FinishedAt = &now
			reconciled = true
			continue
		}
		if prErr == nil {
			if prNumber, ok := o.tryCreatePRForPushedBranch(slotName, sess, reasons); ok {
				log.Printf("[orch] reconcile: %s running->pr_open (auto-created PR #%d for pushed branch %q; %s)",
					slotName, prNumber, sess.Branch, strings.Join(reasons, ", "))
				reconciled = true
				continue
			}
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

func (o *Orchestrator) tryCreatePRForPushedBranch(slotName string, sess *state.Session, reasons []string) (int, bool) {
	branch := strings.TrimSpace(sess.Branch)
	if branch == "" {
		return 0, false
	}
	exists, err := o.remoteBranchExists(branch)
	if err != nil {
		log.Printf("[orch] reconcile: could not check remote branch %q for %s: %v", branch, slotName, err)
		return 0, false
	}
	if !exists {
		return 0, false
	}

	title := autoCreatedPRTitle(sess)
	body := autoCreatedPRBody(sess, branch, reasons)
	prNumber, err := o.createPR(title, body, "main", branch)
	if err != nil {
		log.Printf("[orch] reconcile: could not auto-create PR for %s branch %q: %v", slotName, branch, err)
		return 0, false
	}

	sess.Status = state.StatusPROpen
	sess.PRNumber = prNumber
	sess.PID = 0
	sess.TmuxSession = ""
	now := time.Now().UTC()
	sess.FinishedAt = &now
	if o.notifier != nil {
		o.notifier.Sendf("🔀 maestro: worker %s pushed branch %s and exited before opening a PR; auto-created PR #%d for issue #%d (%s)",
			slotName, branch, prNumber, sess.IssueNumber, sess.IssueTitle)
	}
	return prNumber, true
}

func autoCreatedPRTitle(sess *state.Session) string {
	title := strings.TrimSpace(sess.IssueTitle)
	if title == "" {
		title = "Maestro worker result"
	}
	suffix := fmt.Sprintf(" (#%d)", sess.IssueNumber)
	if !strings.Contains(title, suffix) {
		title += suffix
	}
	if len(title) > 180 {
		title = strings.TrimSpace(title[:180-len(suffix)]) + suffix
	}
	return title
}

func autoCreatedPRBody(sess *state.Session, branch string, reasons []string) string {
	reasonText := strings.TrimSpace(strings.Join(reasons, ", "))
	if reasonText == "" {
		reasonText = "worker process exited before PR creation was observed"
	}
	return fmt.Sprintf(`Refs #%d

Maestro auto-created this PR because the worker pushed branch %s but exited before opening a pull request.

Observed worker state: %s.
`, sess.IssueNumber, branch, reasonText)
}

// checkSessions inspects all sessions and updates their status
func (o *Orchestrator) checkSessions(s *state.State) {
	// Fetch open PRs once for the whole check cycle
	prs, prErr := o.listOpenPRs()
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
		case state.StatusDone, state.StatusDead, state.StatusConflictFailed, state.StatusFailed, state.StatusRetryExhausted:
			// Zombie cleanup: if the underlying issue is closed, transition to done.
			// This prevents conflict_failed/failed/dead/retry_exhausted sessions from lingering
			// indefinitely when their issues are closed externally (#187).
			if sess.Status != state.StatusDone {
				done := false
				if sess.PRNumber > 0 {
					merged, err := o.isPRMerged(sess.PRNumber)
					if err != nil {
						log.Printf("[orch] check PR #%d merged: %v", sess.PRNumber, err)
					} else if merged {
						log.Printf("[orch] PR #%d merged, transitioning zombie session %s from %s to done", sess.PRNumber, slotName, sess.Status)
						done = true
					}
				}
				closed, err := o.isIssueClosed(sess.IssueNumber)
				if err != nil {
					log.Printf("[orch] check issue #%d: %v", sess.IssueNumber, err)
				} else if closed {
					log.Printf("[orch] issue #%d closed, transitioning zombie session %s from %s to done", sess.IssueNumber, slotName, sess.Status)
					done = true
				}
				if done {
					o.syncProject(sess.IssueNumber, github.ProjectStatusDone)
					sess.Status = state.StatusDone
					if sess.FinishedAt == nil {
						now := time.Now().UTC()
						sess.FinishedAt = &now
					}
				}
			}
			// Terminal states — cleanup old worktrees after 1h
			// Use StartedAt as fallback when FinishedAt is nil (orphaned sessions)
			// to preserve the grace period for recently-killed workers.
			nilAndOld := sess.FinishedAt == nil && !sess.StartedAt.IsZero() && time.Since(sess.StartedAt) > 1*time.Hour
			finishedAndOld := sess.FinishedAt != nil && time.Since(*sess.FinishedAt) > 1*time.Hour
			if sess.Worktree != "" && (nilAndOld || finishedAndOld) {
				if _, err := os.Stat(sess.Worktree); err == nil {
					if sess.FinishedAt != nil {
						log.Printf("[orch] cleaning up stale worktree for %s (finished %s ago)", slotName, time.Since(*sess.FinishedAt).Round(time.Minute))
					} else {
						log.Printf("[orch] cleaning up orphaned worktree for %s (started %s ago, no finishedAt)", slotName, time.Since(sess.StartedAt).Round(time.Minute))
					}
					worker.Stop(o.cfg, slotName, sess)
					sess.Worktree = "" // Mark as cleaned
				}
			}
			continue
		}

		// Check if issue is closed for pr_open/queued sessions —
		// free the worker slot when the issue no longer needs work (#187).
		if sess.Status == state.StatusPROpen || sess.Status == state.StatusQueued {
			closed, err := o.isIssueClosed(sess.IssueNumber)
			if err != nil {
				log.Printf("[orch] check issue #%d: %v", sess.IssueNumber, err)
			} else if closed {
				log.Printf("[orch] issue #%d closed, transitioning %s from %s to done", sess.IssueNumber, slotName, sess.Status)
				o.syncProject(sess.IssueNumber, github.ProjectStatusDone)
				o.stopWorker(slotName, sess)
				sess.Status = state.StatusDone
				now := time.Now().UTC()
				sess.FinishedAt = &now
				continue
			}
		}

		// Check if issue is now closed (only for running sessions)
		if sess.Status == state.StatusRunning {
			closed, err := o.isIssueClosed(sess.IssueNumber)
			if err != nil {
				log.Printf("[orch] check issue #%d: %v", sess.IssueNumber, err)
			} else if closed {
				log.Printf("[orch] issue #%d closed, stopping worker %s", sess.IssueNumber, slotName)
				o.syncProject(sess.IssueNumber, github.ProjectStatusDone)
				o.stopWorker(slotName, sess)
				sess.Status = state.StatusDone
				now := time.Now().UTC()
				sess.FinishedAt = &now
				continue
			}

			// Check if process is still alive
			if sess.PID > 0 && !o.pidAlive(sess.PID) {
				// Pipeline phase transition: if this is a pipeline session,
				// try to advance to the next phase before falling through
				// to normal dead-worker handling.
				if pipeline.IsEnabled(o.cfg) && o.advancePipeline(slotName, sess) {
					continue
				}

				// Worker process died — run after_run hook
				o.runAfterRunHook(sess)

				// Check if there's an open PR for this branch BEFORE marking dead
				if pr, found := branchToPR[sess.Branch]; found {
					log.Printf("[orch] worker %s exited but PR #%d is open — transitioning to pr_open", slotName, pr.Number)
					sess.Status = state.StatusPROpen
					sess.PRNumber = pr.Number
					o.notifier.Sendf("🔀 maestro: worker %s completed, PR #%d open for issue #%d (%s)",
						slotName, pr.Number, sess.IssueNumber, sess.IssueTitle)
				} else if o.isRateLimited(sess.LogFile) && o.nextFallbackBackend(sess) != "" {
					// Rate-limit detected — try next fallback backend
					nextBackend := o.nextFallbackBackend(sess)
					log.Printf("[orch] worker %s (backend=%s) hit rate limit, falling back to %s",
						slotName, sess.Backend, nextBackend)

					issue, err := o.getIssue(sess.IssueNumber)
					if err != nil {
						log.Printf("[orch] fetch issue #%d for fallback: %v — marking as failed", sess.IssueNumber, err)
						sess.Status = state.StatusFailed
						now := time.Now().UTC()
						sess.FinishedAt = &now
						o.notifier.Sendf("💀 maestro: worker %s (issue #%d: %s) rate-limited and fallback failed (could not fetch issue)",
							slotName, sess.IssueNumber, sess.IssueTitle)
						continue
					}

					sess.TriedBackends = append(sess.TriedBackends, sess.Backend)
					promptBase := o.selectPrompt(issue)
					if err := o.respawnWorker(slotName, sess, issue, promptBase, nextBackend); err != nil {
						log.Printf("[orch] fallback respawn worker %s with %s: %v — marking as failed", slotName, nextBackend, err)
						sess.Status = state.StatusFailed
						now := time.Now().UTC()
						sess.FinishedAt = &now
						o.notifier.Sendf("💀 maestro: worker %s (issue #%d: %s) rate-limited and fallback to %s failed: %v",
							slotName, sess.IssueNumber, sess.IssueTitle, nextBackend, err)
						continue
					}

					o.notifier.Sendf("🔄 maestro: worker %s (issue #%d) rate-limited on %s, switched to %s",
						slotName, sess.IssueNumber, sess.TriedBackends[len(sess.TriedBackends)-1], nextBackend)
				} else if o.canRetryIssue(s, sess) {
					// Schedule retry with exponential backoff (respects max_retries_per_issue)
					sess.RetryCount++
					backoffMs := retryBackoffMs(sess.RetryCount, o.cfg.MaxRetryBackoffMs)
					retryAt := time.Now().UTC().Add(time.Duration(backoffMs) * time.Millisecond)
					sess.NextRetryAt = &retryAt
					sess.Status = state.StatusDead
					now := time.Now().UTC()
					sess.FinishedAt = &now
					log.Printf("[orch] worker %s (pid=%d) died, scheduling retry %d in %dms",
						slotName, sess.PID, sess.RetryCount, backoffMs)
					o.notifier.Sendf("🔄 maestro: worker %s (issue #%d: %s) died, retry %d scheduled in %ds",
						slotName, sess.IssueNumber, sess.IssueTitle, sess.RetryCount, backoffMs/1000)
				} else {
					// Retry limit reached — mark as permanently failed
					log.Printf("[orch] worker %s (pid=%d) permanently failed after %d retries", slotName, sess.PID, sess.RetryCount)
					// auto-label blocked disabled
					o.syncProject(sess.IssueNumber, github.ProjectStatusTodo)
					sess.Status = state.StatusFailed
					now := time.Now().UTC()
					sess.FinishedAt = &now
					o.notifier.Sendf("💀 maestro: worker %s (issue #%d: %s) permanently failed after %d retries.\nCheck log: %s",
						slotName, sess.IssueNumber, sess.IssueTitle, sess.RetryCount, sess.LogFile)
				}
				continue
			}

			// Check if running session has opened a PR (worker still alive)
			if pr, found := branchToPR[sess.Branch]; found {
				if sess.PRNumber == pr.Number {
					// In-place review/CI retries intentionally keep working on an
					// already-open PR. Do not transition back to pr_open until the
					// worker exits, otherwise the fixer is interrupted mid-run.
				} else {
					log.Printf("[orch] worker %s opened PR #%d while still running — transitioning to pr_open", slotName, pr.Number)
					sess.Status = state.StatusPROpen
					sess.PRNumber = pr.Number
					o.notifier.Sendf("🔀 maestro: worker %s opened PR #%d for issue #%d (%s)",
						slotName, pr.Number, sess.IssueNumber, sess.IssueTitle)
					continue
				}
			}

			// Capture tmux pane output for token tracking, rate-limit detection,
			// silent-timeout detection, and token-limit enforcement.
			{
				tmuxName := sess.TmuxSession
				if tmuxName == "" {
					tmuxName = worker.TmuxSessionName(slotName)
				}

				output, err := o.captureTmux(tmuxName)
				if err != nil {
					log.Printf("[orch] warn: tmux capture-pane failed for %s (%s): %v", slotName, tmuxName, err)
				} else {
					// --- Token tracking ---
					if tokens := worker.ParseTokensFromOutput(output); tokens > sess.TokensUsedAttempt {
						delta := tokens - sess.TokensUsedAttempt
						sess.TokensUsedAttempt = tokens
						sess.TokensUsedTotal += delta
						log.Printf("[orch] %s tokens_used updated: attempt=%d total=%d", slotName, tokens, sess.TokensUsedTotal)
					}

					// --- Rate-limit detection ---
					if !sess.RateLimitHit && sess.LastNotifiedStatus != "rate_limit" {
						if hit, pattern := worker.DetectRateLimit(output); hit {
							log.Printf("[orch] worker %s hit rate limit (pattern=%s), stopping", slotName, pattern)
							o.runAfterRunHook(sess)
							if err := o.stopWorker(slotName, sess); err != nil {
								log.Printf("[orch] warn: could not stop rate-limited worker %s: %v", slotName, err)
							}
							sess.RateLimitHit = true

							// Attempt fallback: respawn with next fallback backend if configured
							if fallback := o.nextFallbackBackend(sess); fallback != "" {
								issue, fetchErr := o.getIssue(sess.IssueNumber)
								if fetchErr != nil {
									log.Printf("[orch] fetch issue #%d for rate-limit fallback: %v — marking dead", sess.IssueNumber, fetchErr)
									sess.Status = state.StatusDead
									sess.LastNotifiedStatus = "rate_limit"
									now := time.Now().UTC()
									sess.FinishedAt = &now
									o.notifier.Sendf("⚠️ maestro: worker %s (issue #%d) hit rate limit (%s); fallback failed (could not fetch issue)",
										slotName, sess.IssueNumber, pattern)
									continue
								}

								sess.TriedBackends = append(sess.TriedBackends, sess.Backend)
								promptBase := o.selectPrompt(issue)
								if respawnErr := o.respawnWorker(slotName, sess, issue, promptBase, fallback); respawnErr != nil {
									log.Printf("[orch] rate-limit fallback respawn %s: %v — marking dead", slotName, respawnErr)
									sess.Status = state.StatusDead
									sess.LastNotifiedStatus = "rate_limit"
									now := time.Now().UTC()
									sess.FinishedAt = &now
									o.notifier.Sendf("⚠️ maestro: worker %s (issue #%d) hit rate limit (%s); fallback to %s failed: %v",
										slotName, sess.IssueNumber, pattern, fallback, respawnErr)
									continue
								}

								log.Printf("[orch] rate-limit fallback: respawned %s with backend %s", slotName, fallback)
								o.notifier.Sendf("🔄 maestro: worker %s (issue #%d) hit rate limit — falling back to %s",
									slotName, sess.IssueNumber, fallback)
								continue
							}

							// No fallback available — mark dead
							sess.Status = state.StatusDead
							sess.LastNotifiedStatus = "rate_limit"
							now := time.Now().UTC()
							sess.FinishedAt = &now
							o.notifier.Sendf("⚠️ maestro: worker %s (issue #%d) hit rate limit (%s); no fallback configured",
								slotName, sess.IssueNumber, pattern)
							continue
						}
					}

					// --- Soft token threshold: checkpoint + respawn ---
					if o.cfg.WorkerMaxTokens > 0 && o.cfg.SoftTokenThreshold() > 0 && sess.CheckpointFile == "" {
						softLimit := int(float64(o.cfg.WorkerMaxTokens) * o.cfg.SoftTokenThreshold())
						if sess.TokensUsedAttempt >= softLimit {
							log.Printf("[orch] worker %s hit soft token threshold (%d >= %d), checkpointing",
								slotName, sess.TokensUsedAttempt, softLimit)

							// Save checkpoint
							cpFile, cpErr := o.saveCheckpoint(sess)
							if cpErr != nil {
								log.Printf("[orch] warn: checkpoint save failed for %s: %v — will hit hard limit", slotName, cpErr)
							} else {
								sess.CheckpointFile = cpFile

								// Fetch issue for prompt assembly
								issue, fetchErr := o.getIssue(sess.IssueNumber)
								if fetchErr != nil {
									log.Printf("[orch] fetch issue #%d for checkpoint respawn: %v — will hit hard limit", sess.IssueNumber, fetchErr)
								} else {
									promptBase := o.selectPrompt(issue)
									if respawnErr := o.respawnInPlace(slotName, sess, issue, promptBase, sess.Backend); respawnErr != nil {
										log.Printf("[orch] checkpoint respawn %s failed: %v — will hit hard limit", slotName, respawnErr)
									} else {
										log.Printf("[orch] checkpoint respawn complete for %s", slotName)
										o.notifier.Sendf("🔄 maestro: worker %s (issue #%d) hit soft token threshold (%s tokens) — checkpointed and respawned",
											slotName, sess.IssueNumber, worker.FormatTokens(softLimit))
										continue
									}
								}
							}
						}
					}

					// --- Token limit enforcement ---
					if o.cfg.WorkerMaxTokens > 0 && sess.TokensUsedAttempt > o.cfg.WorkerMaxTokens && sess.LastNotifiedStatus != "token_limit" {
						log.Printf("[orch] worker %s exceeded token limit (%d > %d), killing",
							slotName, sess.TokensUsedAttempt, o.cfg.WorkerMaxTokens)
						o.runAfterRunHook(sess)
						if err := o.stopWorker(slotName, sess); err != nil {
							log.Printf("[orch] warn: could not stop token-limit worker %s: %v", slotName, err)
						}
						now := time.Now().UTC()
						sess.Status = state.StatusDead
						sess.LastNotifiedStatus = "token_limit"
						sess.FinishedAt = &now
						o.notifier.Sendf("⚠️ Worker %s (issue #%d) exceeded token limit: %s tokens used (attempt), %s total",
							slotName, sess.IssueNumber, worker.FormatTokens(sess.TokensUsedAttempt), worker.FormatTokens(sess.TokensUsedTotal))
						continue
					}

					// --- Silent worker detection ---
					if o.cfg.WorkerSilentTimeoutMinutes > 0 {
						hash := hashOutput(output)
						now := time.Now().UTC()

						if sess.LastOutputHash == "" || sess.LastOutputChangedAt.IsZero() || hash != sess.LastOutputHash {
							sess.LastOutputHash = hash
							sess.LastOutputChangedAt = now
						} else {
							timeout := time.Duration(o.cfg.WorkerSilentTimeoutMinutes) * time.Minute
							if time.Since(sess.LastOutputChangedAt) > timeout {
								log.Printf("[orch] worker %s silent for >%dm, killing", slotName, o.cfg.WorkerSilentTimeoutMinutes)
								o.runAfterRunHook(sess)
								if err := o.stopWorker(slotName, sess); err != nil {
									log.Printf("[orch] warn: could not stop silent worker %s: %v", slotName, err)
								}

								// Count previous silent-timeout kills before updating this session,
								// so the current kill is not included in the count.
								prevSilentKills := countSilentTimeoutKillsForIssue(s, sess.IssueNumber)

								sess.Status = state.StatusDead
								sess.LastNotifiedStatus = "silent_timeout"
								sess.FinishedAt = &now

								if prevSilentKills > 0 {
									// auto-label blocked disabled
								}

								o.notifier.Sendf("⏱️ maestro: worker %s (issue #%d) killed — no output for %d minutes",
									slotName, sess.IssueNumber, o.cfg.WorkerSilentTimeoutMinutes)
								continue
							}
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

				o.runAfterRunHook(sess)
				if err := o.stopWorker(slotName, sess); err != nil {
					log.Printf("[orch] warn: could not stop timed-out worker %s: %v", slotName, err)
				}
				// auto-label blocked disabled
				o.syncProject(sess.IssueNumber, github.ProjectStatusTodo)
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
	prs, err := o.listOpenPRs()
	if err != nil {
		log.Printf("[orch] list PRs: %v", err)
		return
	}

	// Build branch/number → PR maps
	branchToPR := make(map[string]github.PR)
	numberToPR := make(map[int]github.PR)
	for _, pr := range prs {
		branchToPR[pr.HeadRefName] = pr
		numberToPR[pr.Number] = pr
	}

	type mergeCandidate struct {
		slotName string
		sess     *state.Session
		pr       github.PR
	}

	ready := make([]mergeCandidate, 0)

	for slotName, sess := range s.Sessions {
		if !mergeFlowEligibleStatus(sess) {
			continue
		}

		pr, found := mergeFlowPRForSession(sess, branchToPR, numberToPR)
		if !found {
			if sess.Status == state.StatusRetryExhausted {
				log.Printf("[orch] retry_exhausted session %s records PR #%d, but no open PR was found — waiting for reconciliation", slotName, sess.PRNumber)
				continue
			}
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
		ciStatus, err := o.prCIStatus(pr.Number)
		if err != nil {
			log.Printf("[orch] CI status for PR #%d: %v", pr.Number, err)
			continue
		}

		log.Printf("[orch] PR #%d (%s) CI=%s", pr.Number, sess.Branch, ciStatus)

		switch ciStatus {
		case "success":
			// Reset CI-failure notification state when CI goes green. Keep
			// review retry-exhausted markers so actionable feedback does not
			// re-notify on every orchestration cycle.
			if sess.LastNotifiedStatus == "ci_failure" || sess.LastNotifiedStatus == "ci_retry_exhausted" {
				sess.LastNotifiedStatus = ""
			}
			sess.NotifiedCIFail = false // backward compat

			if o.cfg.AutoRetryReviewFeedback {
				reviewFeedback, err := o.collectPRReviewFeedback(pr.Number)
				if err != nil {
					log.Printf("[orch] warn: could not collect review feedback for PR #%d: %v", pr.Number, err)
				} else if strings.TrimSpace(reviewFeedback) != "" {
					log.Printf("[orch] PR #%d has review feedback; scheduling retry", pr.Number)
					o.handleReviewFeedbackRetry(s, slotName, sess, pr, reviewFeedback)
					continue
				}
			}

			if o.reviewGate() == "none" {
				ready = append(ready, mergeCandidate{slotName: slotName, sess: sess, pr: pr})
				continue
			}

			greptileOK, greptilePending, err := o.prGreptileApproved(pr.Number)
			if err != nil {
				log.Printf("[orch] greptile check PR #%d: %v", pr.Number, err)
				continue // skip this cycle, try next
			}
			if greptilePending {
				log.Printf("[orch] PR #%d waiting for Greptile review", pr.Number)
				continue // not ready yet
			}
			if !greptileOK {
				log.Printf("[orch] PR #%d not approved by Greptile", pr.Number)
				// auto-label blocked disabled
				continue
			}

			ready = append(ready, mergeCandidate{slotName: slotName, sess: sess, pr: pr})
		case "failure":
			if sess.Status == state.StatusQueued {
				sess.Status = state.StatusPROpen
			}
			// Auto-retry on CI failure: close the PR, capture CI output, and schedule retry
			if sess.LastNotifiedStatus != "ci_failure" && sess.LastNotifiedStatus != "ci_retry_exhausted" {
				sess.NotifiedCIFail = true // backward compat

				o.handleCIFailureRetry(s, slotName, sess, pr)
			}
		case "pending":
			if sess.Status == state.StatusQueued {
				sess.Status = state.StatusPROpen
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

func mergeFlowEligibleStatus(sess *state.Session) bool {
	if sess == nil {
		return false
	}
	switch sess.Status {
	case state.StatusPROpen, state.StatusQueued:
		return true
	case state.StatusRetryExhausted:
		return sess.PRNumber > 0 || strings.TrimSpace(sess.Branch) != ""
	default:
		return false
	}
}

func mergeFlowPRForSession(sess *state.Session, byBranch map[string]github.PR, byNumber map[int]github.PR) (github.PR, bool) {
	if sess == nil {
		return github.PR{}, false
	}
	if sess.PRNumber > 0 {
		if pr, ok := byNumber[sess.PRNumber]; ok {
			return pr, true
		}
	}
	if strings.TrimSpace(sess.Branch) != "" {
		if pr, ok := byBranch[sess.Branch]; ok {
			return pr, true
		}
	}
	return github.PR{}, false
}

// handleReviewFeedbackRetry schedules a retry worker with review feedback in
// its prompt. When the PR worktree is still available, keep the PR open and
// respawn in place so the fixer pushes updates to the same PR.
func (o *Orchestrator) handleReviewFeedbackRetry(s *state.State, slotName string, sess *state.Session, pr github.PR, reviewFeedback string) {
	maxRetries := o.cfg.MaxRetriesPerIssue
	totalAttempts := s.FailedAttemptsForIssue(sess.IssueNumber) + sess.RetryCount

	if maxRetries > 0 && totalAttempts >= maxRetries {
		log.Printf("[orch] review feedback on PR #%d — retry limit reached (%d/%d) for issue #%d",
			pr.Number, totalAttempts, maxRetries, sess.IssueNumber)
		alreadyNotified := sess.LastNotifiedStatus == "review_retry_exhausted"
		s.MarkIssueRetryExhausted(sess.IssueNumber)
		o.syncProject(sess.IssueNumber, github.ProjectStatusTodo)
		sess.Status = state.StatusRetryExhausted
		sess.NextRetryAt = nil
		sess.LastNotifiedStatus = "review_retry_exhausted"
		now := time.Now().UTC()
		sess.FinishedAt = &now
		if !alreadyNotified {
			o.notifier.Sendf("💀 maestro: review feedback on PR #%d (issue #%d: %s) — retry limit exhausted (%d attempts)",
				pr.Number, sess.IssueNumber, sess.IssueTitle, totalAttempts)
		}
		return
	}

	if sess.Worktree == "" {
		closeComment := fmt.Sprintf("Code review feedback detected, but the PR worktree is unavailable — maestro is closing this PR and respawning a worker to address it (attempt %d).\n\nReview feedback:\n\n%s",
			sess.RetryCount+1, reviewFeedback)
		if err := o.closePR(pr.Number, closeComment); err != nil {
			log.Printf("[orch] warn: could not close PR #%d after review feedback: %v — skipping retry", pr.Number, err)
			return
		}
		log.Printf("[orch] closed PR #%d due to review feedback (worktree unavailable)", pr.Number)
		sess.PRNumber = 0
	} else {
		log.Printf("[orch] keeping PR #%d open and respawning %s in place to address review feedback", pr.Number, slotName)
		sess.PRNumber = pr.Number
	}

	sess.CIFailureOutput = ""
	sess.PreviousAttemptFeedback = reviewFeedback
	sess.PreviousAttemptFeedbackKind = state.RetryReasonReviewFeedback
	sess.RetryReason = state.RetryReasonReviewFeedback

	sess.RetryCount++
	backoffMs := retryBackoffMs(sess.RetryCount, o.cfg.MaxRetryBackoffMs)
	retryAt := time.Now().UTC().Add(time.Duration(backoffMs) * time.Millisecond)
	sess.NextRetryAt = &retryAt
	sess.Status = state.StatusDead
	now := time.Now().UTC()
	sess.FinishedAt = &now

	log.Printf("[orch] review feedback on PR #%d — scheduling retry %d in %dms for issue #%d",
		pr.Number, sess.RetryCount, backoffMs, sess.IssueNumber)
	o.notifier.Sendf("🔄 maestro: review feedback on PR #%d (issue #%d: %s), in-place retry %d scheduled in %ds",
		pr.Number, sess.IssueNumber, sess.IssueTitle, sess.RetryCount, backoffMs/1000)
}

// handleCIFailureRetry closes the failed PR, captures CI output, cleans up,
// and schedules a retry for the worker (respecting max_retries_per_issue).
func (o *Orchestrator) handleCIFailureRetry(s *state.State, slotName string, sess *state.Session, pr github.PR) {
	maxRetries := o.cfg.MaxRetriesPerIssue
	totalAttempts := s.FailedAttemptsForIssue(sess.IssueNumber) + sess.RetryCount

	if maxRetries > 0 && totalAttempts >= maxRetries {
		log.Printf("[orch] CI failure on PR #%d — retry limit reached (%d/%d) for issue #%d",
			pr.Number, totalAttempts, maxRetries, sess.IssueNumber)
		alreadyNotified := sess.LastNotifiedStatus == "ci_retry_exhausted"
		// auto-label blocked disabled
		s.MarkIssueRetryExhausted(sess.IssueNumber)
		o.syncProject(sess.IssueNumber, github.ProjectStatusTodo)
		sess.Status = state.StatusRetryExhausted
		sess.NextRetryAt = nil
		sess.LastNotifiedStatus = "ci_retry_exhausted"
		now := time.Now().UTC()
		sess.FinishedAt = &now
		if !alreadyNotified {
			o.notifier.Sendf("💀 maestro: CI failing on PR #%d (issue #%d: %s) — retry limit exhausted (%d attempts)",
				pr.Number, sess.IssueNumber, sess.IssueTitle, totalAttempts)
		}
		return
	}

	// Capture CI failure output before closing the PR
	ciOutput, err := o.prChecksOutput(pr.Number)
	if err != nil {
		log.Printf("[orch] warn: could not capture CI output for PR #%d: %v", pr.Number, err)
		ciOutput = "(CI output unavailable)"
	}

	// Collect Greptile review feedback before closing the PR
	reviewFeedback, err := o.collectPRReviewFeedback(pr.Number)
	if err != nil {
		log.Printf("[orch] warn: could not collect review feedback for PR #%d: %v", pr.Number, err)
	}

	// Close the failed PR with an explanation
	closeComment := fmt.Sprintf("CI failed — maestro is closing this PR and respawning a new worker to retry (attempt %d).\n\nCI output:\n```\n%s\n```",
		sess.RetryCount+1, ciOutput)
	if err := o.closePR(pr.Number, closeComment); err != nil {
		log.Printf("[orch] warn: could not close PR #%d: %v — skipping retry", pr.Number, err)
		return
	}
	log.Printf("[orch] closed PR #%d due to CI failure", pr.Number)

	// Clean up the worktree
	o.stopWorker(slotName, sess)
	sess.Worktree = ""

	// Store CI failure output and review feedback for the next worker
	sess.CIFailureOutput = ciOutput
	sess.PreviousAttemptFeedback = reviewFeedback
	if strings.TrimSpace(reviewFeedback) != "" {
		sess.PreviousAttemptFeedbackKind = "review_feedback"
	} else {
		sess.PreviousAttemptFeedbackKind = ""
	}

	// Schedule retry with exponential backoff
	sess.RetryCount++
	backoffMs := retryBackoffMs(sess.RetryCount, o.cfg.MaxRetryBackoffMs)
	retryAt := time.Now().UTC().Add(time.Duration(backoffMs) * time.Millisecond)
	sess.NextRetryAt = &retryAt
	sess.Status = state.StatusDead
	sess.PRNumber = 0
	now := time.Now().UTC()
	sess.FinishedAt = &now

	log.Printf("[orch] CI failure on PR #%d — scheduling retry %d in %dms for issue #%d",
		pr.Number, sess.RetryCount, backoffMs, sess.IssueNumber)
	o.notifier.Sendf("🔄 maestro: CI failed on PR #%d (issue #%d: %s), retry %d scheduled in %ds",
		pr.Number, sess.IssueNumber, sess.IssueTitle, sess.RetryCount, backoffMs/1000)
}

func (o *Orchestrator) reviewGate() string {
	switch strings.ToLower(strings.TrimSpace(o.cfg.ReviewGate)) {
	case "none", "off", "disabled":
		return "none"
	default:
		return "greptile"
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
	if err := o.mergePR(pr.Number); err != nil {
		log.Printf("[orch] merge PR #%d: %v", pr.Number, err)

		// If the branch is behind main (not conflicting, just outdated), auto-rebase
		if strings.Contains(err.Error(), "not up to date") && o.cfg.AutoRebase {
			log.Printf("[orch] PR #%d branch is behind main, auto-rebasing %s", pr.Number, slotName)
			if rebaseErr := o.rebaseWorktree(sess.Worktree, sess.Branch); rebaseErr != nil {
				log.Printf("[orch] auto-rebase failed for %s: %v", slotName, rebaseErr)
				o.markUnresolvableConflict(slotName, sess, pr.Number, rebaseErr)
			} else {
				o.markRebaseQueued(slotName, sess, pr.Number)
			}
			return false
		}

		// Only notify merge failure once per PR
		if sess.LastNotifiedStatus != "merge_failed" {
			o.notifier.Sendf("❌ maestro: failed to merge PR #%d (%s): %v", pr.Number, sess.Branch, err)
			sess.LastNotifiedStatus = "merge_failed"
		}
		return false
	}

	log.Printf("[orch] merged PR #%d ✓", pr.Number)
	o.syncProject(sess.IssueNumber, github.ProjectStatusDone)
	if err := o.closeIssue(sess.IssueNumber, fmt.Sprintf("Implemented by PR #%d (auto-merged by maestro).", pr.Number)); err != nil {
		log.Printf("[orch] warning: failed to close issue #%d: %v", sess.IssueNumber, err)
	}
	sess.Status = state.StatusDone
	now := time.Now().UTC()
	sess.FinishedAt = &now

	if o.cfg.ShouldCleanupWorktrees() {
		log.Printf("[orch] cleaning up worktree for %s after merge", slotName)
		o.stopWorker(slotName, sess)
		sess.Worktree = "" // Mark as cleaned
	} else {
		log.Printf("[orch] skipping worktree cleanup for %s (cleanup_worktrees_on_merge=false)", slotName)
	}

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

// runDeployCmd executes the configured deploy command with a configurable timeout.
func (o *Orchestrator) runDeployCmd(prNumber int) error {
	timeout := time.Duration(o.cfg.DeployTimeoutMinutes) * time.Minute
	log.Printf("[orch] running deploy command after PR #%d merge (timeout %dm): %s", prNumber, o.cfg.DeployTimeoutMinutes, o.cfg.DeployCmd)

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "bash", "-c", o.cfg.DeployCmd)
	cmd.Dir = o.cfg.LocalPath
	out, err := cmd.CombinedOutput()
	if len(out) > 0 {
		log.Printf("[orch] deploy output:\n%s", out)
	}
	if ctx.Err() == context.DeadlineExceeded {
		return fmt.Errorf("deploy command timed out after %d minutes", o.cfg.DeployTimeoutMinutes)
	}
	if err != nil {
		return fmt.Errorf("deploy command failed: %w\n%s", err, out)
	}
	return nil
}

// rebaseConflicts handles branch conflicts in two phases:
//  1. Auto-rebase (if enabled)
//  2. Label issue as blocked + keep session in conflict_failed permanently
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
		pr, hasPR := branchToPR[sess.Branch]

		switch sess.Status {
		case state.StatusPROpen:
			if !hasPR {
				continue
			}
			mergeable, err := o.gh.PRMergeable(pr.Number)
			if err != nil {
				log.Printf("[orch] mergeable PR #%d: %v", pr.Number, err)
				continue
			}
			if mergeable != "CONFLICTING" {
				continue
			}

			if !o.cfg.AutoRebase {
				log.Printf("[orch] PR #%d has conflicts for %s, auto_rebase disabled", pr.Number, slotName)
				o.markUnresolvableConflict(slotName, sess, pr.Number, fmt.Errorf("auto_rebase disabled"))
				continue
			}

			log.Printf("[orch] PR #%d has conflicts, auto-rebasing %s", pr.Number, slotName)
			if err := o.rebaseWorktree(sess.Worktree, sess.Branch); err != nil {
				log.Printf("[orch] rebase failed for %s: %v", slotName, err)
				o.handleRebaseConflictRetry(s, slotName, sess, pr.Number, err)
				continue
			}
			o.markRebaseQueued(slotName, sess, pr.Number)

		case state.StatusConflictFailed:
			if !o.cfg.AutoRebase || sess.RebaseAttempted {
				continue
			}
			if !hasPR {
				log.Printf("[orch] conflict_failed session %s has no open PR, skipping auto-rebase", slotName)
				continue
			}

			log.Printf("[orch] retrying auto-rebase for conflict_failed session %s (PR #%d)", slotName, pr.Number)
			if err := o.rebaseWorktree(sess.Worktree, sess.Branch); err != nil {
				log.Printf("[orch] rebase retry failed for %s: %v", slotName, err)
				o.handleRebaseConflictRetry(s, slotName, sess, pr.Number, err)
				continue
			}
			o.markRebaseQueued(slotName, sess, pr.Number)
		}
	}
}

func (o *Orchestrator) markRebaseQueued(slotName string, sess *state.Session, prNumber int) {
	log.Printf("[orch] rebase succeeded for %s", slotName)
	sess.Status = state.StatusQueued
	sess.RebaseAttempted = true
	sess.FinishedAt = nil
	sess.PRNumber = prNumber
	sess.NotifiedCIFail = false
	sess.LastNotifiedStatus = ""
	o.notifier.Sendf("🔄 maestro: rebased %s (PR #%d) successfully; session moved to queued", slotName, prNumber)
}

func (o *Orchestrator) handleRebaseConflictRetry(s *state.State, slotName string, sess *state.Session, prNumber int, cause error) {
	if !o.cfg.AutoRetryRebaseConflicts {
		o.markUnresolvableConflict(slotName, sess, prNumber, cause)
		return
	}

	maxRetries := o.cfg.MaxRetriesPerIssue
	totalAttempts := s.FailedAttemptsForIssue(sess.IssueNumber) + sess.RetryCount
	if maxRetries > 0 && totalAttempts >= maxRetries {
		log.Printf("[orch] rebase conflict on PR #%d — retry limit reached (%d/%d) for issue #%d",
			prNumber, totalAttempts, maxRetries, sess.IssueNumber)
		s.MarkIssueRetryExhausted(sess.IssueNumber)
		o.syncProject(sess.IssueNumber, github.ProjectStatusTodo)
		sess.Status = state.StatusRetryExhausted
		sess.NextRetryAt = nil
		sess.LastNotifiedStatus = "rebase_conflict_retry_exhausted"
		now := time.Now().UTC()
		sess.FinishedAt = &now
		o.notifier.Sendf("💀 maestro: rebase conflict on PR #%d (issue #%d: %s) — retry limit exhausted (%d attempts)",
			prNumber, sess.IssueNumber, sess.IssueTitle, totalAttempts)
		return
	}

	if sess.Worktree == "" {
		closeComment := fmt.Sprintf("Auto-rebase hit conflicts, but the PR worktree is unavailable — maestro is closing this PR and respawning a worker to resolve the conflict from a fresh branch (attempt %d).\n\nRebase failure:\n\n```\n%s\n```",
			sess.RetryCount+1, rebaseConflictFeedback(prNumber, cause))
		if err := o.closePR(prNumber, closeComment); err != nil {
			log.Printf("[orch] warn: could not close PR #%d after rebase conflict: %v — marking conflict_failed", prNumber, err)
			o.markUnresolvableConflict(slotName, sess, prNumber, cause)
			return
		}
		log.Printf("[orch] closed PR #%d due to rebase conflict (worktree unavailable)", prNumber)
		sess.PRNumber = 0
	} else {
		log.Printf("[orch] keeping PR #%d open and respawning %s in place to resolve rebase conflicts", prNumber, slotName)
		sess.PRNumber = prNumber
	}

	sess.CIFailureOutput = ""
	sess.PreviousAttemptFeedback = rebaseConflictFeedback(prNumber, cause)
	sess.PreviousAttemptFeedbackKind = "rebase_conflict"

	sess.RetryCount++
	backoffMs := retryBackoffMs(sess.RetryCount, o.cfg.MaxRetryBackoffMs)
	retryAt := time.Now().UTC().Add(time.Duration(backoffMs) * time.Millisecond)
	sess.NextRetryAt = &retryAt
	sess.Status = state.StatusDead
	sess.RebaseAttempted = true
	now := time.Now().UTC()
	sess.FinishedAt = &now

	log.Printf("[orch] rebase conflict on PR #%d — scheduling retry %d in %dms for issue #%d",
		prNumber, sess.RetryCount, backoffMs, sess.IssueNumber)
	o.notifier.Sendf("🔄 maestro: rebase conflict on PR #%d (issue #%d: %s), in-place retry %d scheduled in %ds",
		prNumber, sess.IssueNumber, sess.IssueTitle, sess.RetryCount, backoffMs/1000)
}

func rebaseConflictFeedback(prNumber int, cause error) string {
	msg := "(rebase failure unavailable)"
	if cause != nil {
		msg = strings.TrimSpace(cause.Error())
	}
	if len(msg) > 8000 {
		msg = msg[:8000] + "\n... (truncated)"
	}
	return fmt.Sprintf("PR #%d failed to rebase onto origin/main.\n\n%s", prNumber, msg)
}

func (o *Orchestrator) markUnresolvableConflict(slotName string, sess *state.Session, prNumber int, cause error) {
	if err := o.addIssueLabel(sess.IssueNumber, "blocked"); err != nil {
		log.Printf("[orch] warn: could not label issue #%d as blocked: %v", sess.IssueNumber, err)
	}
	if cause != nil {
		log.Printf("[orch] conflict for %s is unresolvable: %v", slotName, cause)
	}

	sess.Status = state.StatusConflictFailed
	sess.RebaseAttempted = true
	sess.PRNumber = prNumber
	now := time.Now().UTC()
	sess.FinishedAt = &now
	o.notifier.Sendf("⚠️ Worker %s (issue #%d) has unresolvable conflicts — manual intervention needed", slotName, sess.IssueNumber)
}

// findOpenBlockers returns the subset of blocker issue numbers that are still open.
func (o *Orchestrator) findOpenBlockers(blockers []int) []int {
	var open []int
	for _, num := range blockers {
		closed, err := o.isIssueClosed(num)
		if err != nil {
			// If we can't determine the state, assume it's open (safe default)
			log.Printf("[orch] warn: could not check blocker #%d: %v (assuming open)", num, err)
			open = append(open, num)
			continue
		}
		if !closed {
			open = append(open, num)
		}
	}
	return open
}

// resolveBackend determines which backend to use for the given issue.
// Delegates to router.ResolveBackend which applies 3-tier priority:
//  1. model:<backend> label on the issue (highest priority)
//  2. Auto-routing via LLM (if routing.mode == "auto")
//  3. Default backend from config
func (o *Orchestrator) resolveBackend(issue github.Issue) string {
	name, _ := o.router.ResolveBackend(issue)
	return name
}

// availableSlots calculates how many new workers can be started, considering
// both the global max_parallel limit and per-state limits from max_concurrent_by_state.
// New workers enter the "running" state, so the "running" per-state limit is applied.
func availableSlots(cfg *config.Config, s *state.State, active int) int {
	slots := cfg.MaxParallel - active
	if slots <= 0 {
		return 0
	}

	// Apply per-state limit for "running" — new workers enter running state
	if limit, ok := cfg.MaxConcurrentByState["running"]; ok && limit > 0 {
		statusCounts := s.CountByStatus()
		runningCount := statusCounts[state.StatusRunning]
		runningSlots := limit - runningCount
		if runningSlots < slots {
			log.Printf("[orch] per-state limit: running=%d max_running=%d (capped from %d to %d slots)",
				runningCount, limit, slots, runningSlots)
			slots = runningSlots
		}
	}

	if slots < 0 {
		return 0
	}
	return slots
}

// startNewWorkers picks eligible issues and starts workers for them
func (o *Orchestrator) listOpenIssues(labels []string) ([]github.Issue, error) {
	if o.listOpenIssuesFn != nil {
		return o.listOpenIssuesFn(labels)
	}
	return o.gh.ListOpenIssues(labels)
}

func (o *Orchestrator) startWorker(s *state.State, issue github.Issue, promptBase, backend string) (string, error) {
	if o.workerStartFn != nil {
		return o.workerStartFn(o.cfg, s, o.repo, issue, promptBase, backend)
	}
	return worker.Start(o.cfg, s, o.repo, issue, promptBase, backend)
}

func (o *Orchestrator) orderedQueueIssueDone(s *state.State, issueNumber int) (bool, string, error) {
	queue := o.cfg.Supervisor.OrderedQueue
	if queue.IsDone(issueNumber) {
		return true, "policy done override", nil
	}

	closed, err := o.isIssueClosed(issueNumber)
	if err != nil {
		return false, "", fmt.Errorf("check issue closed: %w", err)
	}
	if closed {
		return true, "issue closed", nil
	}

	merged, err := o.hasMergedPRForIssue(issueNumber)
	if err != nil {
		return false, "", fmt.Errorf("check merged PR for issue: %w", err)
	}
	if merged {
		return true, "linked PR merged", nil
	}

	for _, slotName := range sortedStateSessionNames(s) {
		sess := s.Sessions[slotName]
		if sess == nil || sess.IssueNumber != issueNumber || sess.Status != state.StatusDone || sess.PRNumber <= 0 {
			continue
		}
		merged, err := o.isPRMerged(sess.PRNumber)
		if err != nil {
			return false, "", fmt.Errorf("check PR #%d merged: %w", sess.PRNumber, err)
		}
		if merged {
			return true, fmt.Sprintf("session %s is done with merged PR #%d", slotName, sess.PRNumber), nil
		}
	}

	return false, "", nil
}

func (o *Orchestrator) orderedQueueIssueNumberPauseReason(s *state.State, issueNumber int) string {
	if s.IssueInProgress(issueNumber) {
		return fmt.Sprintf("issue #%d already has an active session", issueNumber)
	}

	if hasOpenPR, err := o.hasOpenPRForIssue(issueNumber); err != nil {
		return fmt.Sprintf("could not check open PRs for issue #%d: %v", issueNumber, err)
	} else if hasOpenPR {
		return fmt.Sprintf("issue #%d already has an open PR", issueNumber)
	}

	if s.IssueRetryExhausted(issueNumber) {
		return fmt.Sprintf("issue #%d is retry-exhausted", issueNumber)
	}
	if o.cfg.MaxRetriesPerIssue > 0 {
		failed := s.FailedAttemptsForIssue(issueNumber)
		if failed >= o.cfg.MaxRetriesPerIssue {
			if !s.IssueRetryExhausted(issueNumber) {
				s.MarkIssueRetryExhausted(issueNumber)
				o.notifier.Sendf("⚠️ Issue #%d hit max retries (%d) — needs manual review",
					issueNumber, o.cfg.MaxRetriesPerIssue)
			}
			return fmt.Sprintf("issue #%d exhausted retries (%d/%d attempts)", issueNumber, failed, o.cfg.MaxRetriesPerIssue)
		}
	}

	return ""
}

func (o *Orchestrator) orderedQueueIssuePauseReason(s *state.State, issue github.Issue) string {
	if s.IsMissionParent(issue.Number) {
		return fmt.Sprintf("issue #%d is a mission parent", issue.Number)
	}
	if o.cfg.Missions.Enabled && mission.IsMissionIssue(issue, o.cfg.Missions.Labels) && !s.IsMissionChild(issue.Number) {
		return fmt.Sprintf("issue #%d is a mission issue awaiting decomposition", issue.Number)
	}
	if github.HasLabel(issue, o.cfg.ExcludeLabels) {
		return fmt.Sprintf("issue #%d is excluded by configured label", issue.Number)
	}
	if len(o.cfg.BlockerPatterns) > 0 {
		blockers := github.FindBlockers(issue.Body, o.cfg.BlockerPatterns)
		if len(blockers) > 0 {
			openBlockers := o.findOpenBlockers(blockers)
			if len(openBlockers) > 0 {
				return fmt.Sprintf("issue #%d is blocked by open issue(s) %v", issue.Number, openBlockers)
			}
		}
	}
	return ""
}

func (o *Orchestrator) applyOrderedQueueFilter(s *state.State, issues []github.Issue) ([]github.Issue, bool) {
	queue := o.cfg.Supervisor.OrderedQueue
	if !queue.Active() {
		return issues, false
	}

	openByNumber := make(map[int]github.Issue, len(issues))
	for _, issue := range issues {
		openByNumber[issue.Number] = issue
	}

	for _, issueNumber := range queue.Issues {
		done, reason, err := o.orderedQueueIssueDone(s, issueNumber)
		if err != nil {
			log.Printf("[orch] ordered queue paused at issue #%d: %v", issueNumber, err)
			return nil, true
		}
		if done {
			log.Printf("[orch] ordered queue skipping issue #%d: %s", issueNumber, reason)
			continue
		}

		if reason := o.orderedQueueIssueNumberPauseReason(s, issueNumber); reason != "" {
			log.Printf("[orch] ordered queue paused: %s", reason)
			return nil, true
		}

		issue, ok := openByNumber[issueNumber]
		if !ok {
			log.Printf("[orch] ordered queue paused at issue #%d: issue is not open or does not match issue_labels", issueNumber)
			return nil, true
		}

		if reason := o.orderedQueueIssuePauseReason(s, issue); reason != "" {
			log.Printf("[orch] ordered queue paused: %s", reason)
			return nil, true
		}

		return []github.Issue{issue}, true
	}

	log.Printf("[orch] ordered queue complete: all configured issues are done")
	return nil, true
}

func (o *Orchestrator) supervisorOwnsDynamicReadyLabel() bool {
	return o.cfg != nil && o.cfg.Supervisor.DynamicWave.Active() && o.cfg.Supervisor.DynamicWave.OwnsReadyLabel
}

func (o *Orchestrator) supervisorOwnedReadySelectedIssue(s *state.State) (int, bool) {
	if !o.supervisorOwnsDynamicReadyLabel() || s == nil {
		return 0, false
	}
	decision := s.LatestSupervisorDecision()
	if decision == nil || decision.PolicyRule != supervisor.PolicyRuleDynamicWave {
		return 0, false
	}
	if decision.QueueAnalysis != nil && decision.QueueAnalysis.SelectedCandidate != nil && decision.QueueAnalysis.SelectedCandidate.Number > 0 {
		return decision.QueueAnalysis.SelectedCandidate.Number, true
	}
	if decision.Target != nil && decision.Target.Issue > 0 {
		return decision.Target.Issue, true
	}
	return 0, false
}

func (o *Orchestrator) applySupervisorOwnedReadyFilter(s *state.State, issues []github.Issue) []github.Issue {
	if !o.supervisorOwnsDynamicReadyLabel() || len(issues) == 0 {
		return issues
	}

	selected, ok := o.supervisorOwnedReadySelectedIssue(s)
	if !ok {
		for _, issue := range issues {
			log.Printf("[orch] skipping issue #%d: supervisor-owned ready label has no selected dynamic-wave candidate yet", issue.Number)
		}
		return nil
	}

	filtered := make([]github.Issue, 0, 1)
	for _, issue := range issues {
		if issue.Number == selected {
			filtered = append(filtered, issue)
			continue
		}
		log.Printf("[orch] skipping issue #%d: not supervisor-selected candidate #%d for supervisor-owned ready label", issue.Number, selected)
	}
	if len(filtered) == 0 {
		log.Printf("[orch] supervisor-owned ready label selected issue #%d, but it is not currently returned by issue_labels", selected)
	}
	return filtered
}

func sortedStateSessionNames(s *state.State) []string {
	names := make([]string, 0, len(s.Sessions))
	for name := range s.Sessions {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func (o *Orchestrator) startNewWorkers(s *state.State, slots int) {
	issues, err := o.listOpenIssues(o.cfg.IssueLabels)
	if err != nil {
		log.Printf("[orch] list issues: %v", err)
		return
	}
	if filtered, ordered := o.applyOrderedQueueFilter(s, issues); ordered {
		issues = filtered
	} else {
		issues = o.applySupervisorOwnedReadyFilter(s, issues)
	}

	started := 0
	for _, issue := range issues {
		if s.IssueInProgress(issue.Number) {
			continue
		}
		if s.IssueDone(issue.Number) {
			closed, err := o.isIssueClosed(issue.Number)
			if err != nil {
				log.Printf("[orch] warn: could not verify closed issue #%d before dispatch: %v", issue.Number, err)
			} else if closed {
				log.Printf("[orch] skipping issue #%d: already closed with completed session", issue.Number)
				o.syncProject(issue.Number, github.ProjectStatusDone)
				continue
			}
		}

		// Skip mission parent issues — they are decomposed, not dispatched directly
		if s.IsMissionParent(issue.Number) {
			continue
		}

		// Skip issues that carry a mission/epic label (they should be decomposed first)
		if o.cfg.Missions.Enabled && mission.IsMissionIssue(issue, o.cfg.Missions.Labels) && !s.IsMissionChild(issue.Number) {
			continue
		}

		if github.HasLabel(issue, o.cfg.ExcludeLabels) {
			log.Printf("[orch] skipping issue #%d (excluded label)", issue.Number)
			continue
		}

		// Check retry limit: skip issues that have exhausted their retry budget
		if o.cfg.MaxRetriesPerIssue > 0 {
			failed := s.FailedAttemptsForIssue(issue.Number)
			if failed >= o.cfg.MaxRetriesPerIssue {
				if !s.IssueRetryExhausted(issue.Number) {
					// First time hitting the limit — mark, label, and notify
					s.MarkIssueRetryExhausted(issue.Number)
					// auto-label blocked disabled
					o.notifier.Sendf("⚠️ Issue #%d hit max retries (%d) — needs manual review",
						issue.Number, o.cfg.MaxRetriesPerIssue)
				}
				log.Printf("[orch] skipping issue #%d: retry limit exhausted (%d/%d attempts)",
					issue.Number, failed, o.cfg.MaxRetriesPerIssue)
				continue
			}
		}

		// Check for open blockers: skip if any referenced blocking issues are still open
		if len(o.cfg.BlockerPatterns) > 0 {
			blockers := github.FindBlockers(issue.Body, o.cfg.BlockerPatterns)
			if len(blockers) > 0 {
				openBlockers := o.findOpenBlockers(blockers)
				if len(openBlockers) > 0 {
					log.Printf("[orch] skipping issue #%d: blocked by open issues %v", issue.Number, openBlockers)
					continue
				}
			}
		}

		// No available slots — sync remaining eligible issues as backlog/todo.
		// This check is intentionally before hasOpenPRForIssue to avoid making
		// a GitHub API call per backlogged issue when all slots are full.
		if started >= slots {
			o.syncProject(issue.Number, github.ProjectStatusTodo)
			continue
		}

		// Safety net: check GitHub directly for any open PR referencing this issue.
		// This guards against the race where reconcileRunningSessions marked a session
		// dead before checkSessions could detect its PR and transition it to pr_open.
		if hasOpenPR, err := o.hasOpenPRForIssue(issue.Number); err != nil {
			log.Printf("[orch] warn: could not check open PRs for issue #%d: %v", issue.Number, err)
		} else if hasOpenPR {
			log.Printf("[orch] skipping issue #%d: open PR already exists", issue.Number)
			continue
		}

		// Determine initial phase and backend
		initialPhase := pipeline.InitialPhase(o.cfg)
		var backendName string
		var promptBase string
		if initialPhase != state.PhaseNone && initialPhase != state.PhaseImplement {
			// Pipeline mode with planner — use planner backend and raw template
			// (worker.Start → assemblePrompt will substitute {{WORKTREE}} etc.)
			backendName = pipeline.BackendForPhase(o.cfg, initialPhase)
			promptBase = pipeline.PromptTemplateForPhase(o.cfg, initialPhase)
		} else {
			// Normal mode or pipeline starting at implement — use standard resolution
			backendName = o.resolveBackend(issue)
			promptBase = o.selectPrompt(issue)
			if initialPhase == state.PhaseImplement {
				// Pipeline mode but no planner — add pipeline preamble
				preamble := pipeline.ImplementerPreamble(&state.Session{})
				promptBase = preamble + "\n" + promptBase
			}
		}

		// Detect long-running label
		longRunning := false
		for _, label := range issue.Labels {
			if strings.EqualFold(label.Name, "long-running") {
				longRunning = true
				break
			}
		}

		log.Printf("[orch] starting worker for issue #%d: %s (backend=%s, phase=%s, long_running=%v)", issue.Number, issue.Title, backendName, initialPhase, longRunning)
		slotName, err := o.startWorker(s, issue, promptBase, backendName)
		if err != nil {
			log.Printf("[orch] start worker for issue #%d: %v", issue.Number, err)
			o.notifier.Sendf("❌ maestro: failed to start worker for issue #%d (%s): %v",
				issue.Number, issue.Title, err)
			continue
		}

		if longRunning {
			s.Sessions[slotName].LongRunning = true
		}
		if initialPhase != state.PhaseNone {
			s.Sessions[slotName].Phase = initialPhase
		}
		o.syncProject(issue.Number, github.ProjectStatusInProgress)
		o.notifier.Sendf("🚀 maestro: started worker %s for issue #%d: %s", slotName, issue.Number, issue.Title)
		started++
	}

	if started == 0 {
		log.Printf("[orch] no new workers started (%d issues checked)", len(issues))
	}
}
