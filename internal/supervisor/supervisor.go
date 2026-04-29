package supervisor

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/github"
	"github.com/befeast/maestro/internal/mission"
	"github.com/befeast/maestro/internal/state"
)

const (
	ModeReadOnly    = "read_only"
	ModeSafeActions = "safe_actions"

	ActionNone                 = "none"
	ActionWaitForRunningWorker = "wait_for_running_worker"
	ActionWaitForCapacity      = "wait_for_capacity"
	ActionMonitorOpenPR        = "monitor_open_pr"
	ActionReviewRetryExhausted = "review_retry_exhausted"
	ActionSpawnWorker          = "spawn_worker"
	ActionLabelIssueReady      = "label_issue_ready"

	RiskSafe          = "safe"
	RiskMutating      = "mutating"
	RiskApprovalGated = "approval_gated"

	PolicyRuleRuntimeState   = "runtime_state"
	PolicyRuleOpenIssues     = "open_issues"
	PolicyRuleIssueLabels    = "issue_labels"
	PolicyRuleOrderedQueue   = "supervisor.ordered_queue"
	PolicyRuleExcludedLabels = "supervisor.excluded_labels"

	DecisionStatusRecommended = "recommended"
	DecisionStatusSucceeded   = "succeeded"
	DecisionStatusFailed      = "failed"

	MutationAddReadyLabel      = "add_ready_label"
	MutationRemoveBlockedLabel = "remove_blocked_label"
	MutationIssueComment       = config.SupervisorActionAddIssueComment

	MutationStatusPlanned   = "planned"
	MutationStatusSucceeded = "succeeded"
	MutationStatusFailed    = "failed"

	ErrorClassGitHubAPI         = "github_api"
	ErrorClassGitHubAuth        = "github_auth"
	ErrorClassGitHubNotFound    = "github_not_found"
	ErrorClassGitHubRateLimited = "github_rate_limited"
	ErrorClassUnsupportedClient = "unsupported_client"

	SeverityInfo    = "info"
	SeverityWarning = "warning"
	SeverityBlocked = "blocked"
)

// Reader is the read-only GitHub surface used by the supervisor engine.
type Reader interface {
	ListOpenIssues(labels []string) ([]github.Issue, error)
	ListOpenPRs() ([]github.PR, error)
	HasOpenPRForIssue(issueNumber int) (bool, error)
	IsIssueClosed(number int) (bool, error)
}

// Mutator is the safe GitHub write surface used for supervisor queue actions.
type Mutator interface {
	AddIssueLabel(issueNumber int, label string) error
	RemoveIssueLabel(issueNumber int, label string) error
	CommentIssue(issueNumber int, body string) error
}

type prCIStatusReader interface {
	PRCIStatus(prNumber int) (string, error)
}

type prGreptileReader interface {
	PRGreptileApproved(prNumber int) (approved bool, pending bool, err error)
}

// Engine makes deterministic supervisor decisions. It plans safe queue mutations
// and emits structured stuck-state explanations.
type Engine struct {
	cfg      *config.Config
	reader   Reader
	now      func() time.Time
	pidAlive func(pid int) bool
	stat     func(name string) (os.FileInfo, error)
	lookPath func(file string) (string, error)
}

func NewEngine(cfg *config.Config, reader Reader) *Engine {
	if reader == nil {
		reader = github.New(cfg.Repo)
	}
	return &Engine{
		cfg:      cfg,
		reader:   reader,
		now:      func() time.Time { return time.Now().UTC() },
		pidAlive: pidAlive,
		stat:     os.Stat,
		lookPath: exec.LookPath,
	}
}

// RunOnce records one supervisor decision in Maestro state and applies any safe
// queue mutations selected by the decision.
func RunOnce(cfg *config.Config, reader Reader) (state.SupervisorDecision, error) {
	st, err := state.Load(cfg.StateDir)
	if err != nil {
		return state.SupervisorDecision{}, fmt.Errorf("load state: %w", err)
	}
	if reader == nil {
		reader = github.New(cfg.Repo)
	}

	decision, err := NewEngine(cfg, reader).Decide(st)
	if err != nil {
		return state.SupervisorDecision{}, err
	}
	if len(decision.Mutations) > 0 {
		mutator, ok := reader.(Mutator)
		if !ok {
			markUnsupportedQueueAction(&decision)
		} else {
			applyQueueAction(cfg, &decision, mutator)
		}
	}
	st.RecordSupervisorDecision(decision, state.DefaultSupervisorDecisionLimit)
	if err := state.Save(cfg.StateDir, st); err != nil {
		return state.SupervisorDecision{}, fmt.Errorf("save state: %w", err)
	}
	return decision, nil
}

// Decide observes state and GitHub read-only data, then returns the next recommendation.
func (e *Engine) Decide(st *state.State) (state.SupervisorDecision, error) {
	if st == nil {
		st = state.NewState()
	}
	now := e.now().UTC()
	projectState := e.projectState(st)
	baseReasons := []string{
		fmt.Sprintf("State has %d session(s)", projectState.Sessions),
		fmt.Sprintf("%d active session(s) count against %d max parallel slot(s)", len(st.ActiveSessions()), e.cfg.MaxParallel),
		e.policySummaryReason(),
	}

	prs, err := e.reader.ListOpenPRs()
	if err != nil {
		return state.SupervisorDecision{}, fmt.Errorf("list open PRs: %w", err)
	}
	projectState.OpenPRs = len(prs)
	stuckStates := e.detectStuckStates(st, now, prs, nil, nil, nil, false)

	if slot, sess, pr, ok := sessionWithOpenPR(st, prs); ok {
		reasons := appendReasons(baseReasons,
			fmt.Sprintf("Session %s is associated with open PR #%d", slot, pr.Number),
			"No GitHub mutation is needed for supervisor mode",
		)
		decision := e.decision(now, projectState, ActionMonitorOpenPR,
			fmt.Sprintf("Session %s already has open PR #%d; monitor review, CI, or merge readiness.", slot, pr.Number),
			RiskSafe, 0.9, &state.SupervisorTarget{Issue: sess.IssueNumber, PR: pr.Number, Session: slot}, PolicyRuleRuntimeState, reasons)
		decision.StuckStates = stuckStates
		return decision, nil
	}

	if slot, sess, ok := runningSession(st); ok && e.shouldWaitForRunningWorker(st) {
		reasons := appendReasons(baseReasons,
			fmt.Sprintf("Session %s is running for issue #%d", slot, sess.IssueNumber),
			"Starting another worker is not recommended while a worker is active",
		)
		decision := e.decision(now, projectState, ActionWaitForRunningWorker,
			fmt.Sprintf("Worker %s is still running for issue #%d.", slot, sess.IssueNumber),
			RiskSafe, 0.88, &state.SupervisorTarget{Issue: sess.IssueNumber, Session: slot}, PolicyRuleRuntimeState, reasons)
		decision.StuckStates = stuckStates
		return decision, nil
	}

	if slot, sess, ok := retryExhaustedSession(st); ok {
		reasons := appendReasons(baseReasons,
			fmt.Sprintf("Session %s for issue #%d is retry_exhausted", slot, sess.IssueNumber),
			"Retry-exhausted work requires a human decision before more automation",
		)
		decision := e.decision(now, projectState, ActionReviewRetryExhausted,
			fmt.Sprintf("Issue #%d exhausted its retry budget and needs manual review.", sess.IssueNumber),
			RiskApprovalGated, 0.93, &state.SupervisorTarget{Issue: sess.IssueNumber, PR: sess.PRNumber, Session: slot}, PolicyRuleRuntimeState, reasons)
		decision.StuckStates = stuckStates
		return decision, nil
	}

	issues, err := e.reader.ListOpenIssues(nil)
	if err != nil {
		return state.SupervisorDecision{}, fmt.Errorf("list open issues: %w", err)
	}
	projectState.OpenIssues = len(issues)

	candidates, policySkipped, policyRule, err := e.policyCandidateIssues(st, issues)
	if err != nil {
		return state.SupervisorDecision{}, err
	}
	eligible, skipped, err := e.eligibleIssues(st, candidates, true)
	if err != nil {
		return state.SupervisorDecision{}, err
	}
	skipped = append(policySkipped, skipped...)
	stuckStates = e.detectStuckStates(st, now, prs, issues, eligible, skipped, true)

	if len(eligible) > 0 {
		issue := eligible[0]
		if projectState.AvailableSlots <= 0 {
			reasons := appendReasons(baseReasons,
				fmt.Sprintf("Issue #%d is eligible but no worker slot is available", issue.Number),
			)
			decision := e.decision(now, projectState, ActionWaitForCapacity,
				fmt.Sprintf("Issue #%d is eligible, but all worker slots are occupied.", issue.Number),
				RiskSafe, 0.86, &state.SupervisorTarget{Issue: issue.Number}, policyRule, reasons)
			decision.StuckStates = stuckStates
			return decision, nil
		}

		hasOpenPR, err := e.reader.HasOpenPRForIssue(issue.Number)
		if err != nil {
			return state.SupervisorDecision{}, fmt.Errorf("check open PR for issue #%d: %w", issue.Number, err)
		}
		if hasOpenPR {
			reasons := appendReasons(baseReasons,
				fmt.Sprintf("Issue #%d is eligible but GitHub already has an open PR referencing it", issue.Number),
				"Supervisor mode should not dispatch duplicate work",
			)
			decision := e.decision(now, projectState, ActionMonitorOpenPR,
				fmt.Sprintf("Issue #%d already has an open PR; monitor that PR instead of starting work.", issue.Number),
				RiskSafe, 0.87, &state.SupervisorTarget{Issue: issue.Number}, policyRule, reasons)
			decision.StuckStates = stuckStates
			return decision, nil
		}

		reasons := appendReasons(baseReasons,
			issueLabelReason(e.requiredIssueLabels()),
			fmt.Sprintf("Issue #%d is the next eligible issue", issue.Number),
			"Starting a worker would mutate local worktrees, so supervisor only records the recommendation",
		)
		decision := e.decision(now, projectState, ActionSpawnWorker,
			fmt.Sprintf("Start a worker for issue #%d: %s", issue.Number, issue.Title),
			RiskMutating, 0.84, &state.SupervisorTarget{Issue: issue.Number}, policyRule, reasons)
		decision.StuckStates = stuckStates
		return decision, nil
	}

	candidate, err := e.firstQueueActionCandidate(st, candidates)
	if err != nil {
		return state.SupervisorDecision{}, err
	}
	if candidate != nil {
		mutations := candidate.plannedMutations(e.cfg)
		reasons := appendReasons(baseReasons,
			queueLabelReason(candidate.readyLabel, candidate.blockedLabel),
			fmt.Sprintf("Issue #%d is the next queue issue eligible for safe label mutation", candidate.issue.Number),
		)
		risk := RiskMutating
		if len(mutations) > 0 {
			risk = RiskSafe
			reasons = appendReasons(reasons, "Supervisor policy allows the planned safe queue mutation")
		}
		decision := e.decision(now, projectState, ActionLabelIssueReady,
			fmt.Sprintf("Prepare issue #%d for the queue by %s.", candidate.issue.Number, plannedMutationPhrase(candidate.neededMutations())),
			risk, 0.82, &state.SupervisorTarget{Issue: candidate.issue.Number}, policyRule, reasons)
		decision.Mutations = mutations
		decision.StuckStates = stuckStates
		return decision, nil
	}

	reasons := appendReasons(baseReasons,
		fmt.Sprintf("Checked %d open issue(s)", len(issues)),
		"No worker is running, no PR needs attention, and no eligible issue is ready",
	)
	for _, reason := range firstN(skipped, 3) {
		reasons = append(reasons, reason)
	}
	decision := e.decision(now, projectState, ActionNone,
		"No action is currently recommended.", RiskSafe, 0.8, nil, policyRule, reasons)
	decision.StuckStates = stuckStates
	return decision, nil
}

func (e *Engine) decision(now time.Time, ps state.SupervisorProjectState, action, summary, risk string, confidence float64, target *state.SupervisorTarget, policyRule string, reasons []string) state.SupervisorDecision {
	reasons = appendReasons(reasons, policyRuleReason(policyRule))
	return state.SupervisorDecision{
		ID:                "sup-" + now.Format("20060102T150405.000000000Z"),
		CreatedAt:         now,
		Project:           e.cfg.Repo,
		Mode:              ModeReadOnly,
		PolicyRule:        policyRule,
		Status:            DecisionStatusRecommended,
		Summary:           summary,
		RecommendedAction: action,
		Target:            target,
		Risk:              risk,
		Confidence:        confidence,
		Reasons:           compactReasons(reasons),
		ProjectState:      ps,
	}
}

func (e *Engine) detectStuckStates(st *state.State, now time.Time, prs []github.PR, issues, eligible []github.Issue, skipped []string, issuesLoaded bool) []state.SupervisorStuckState {
	var findings []state.SupervisorStuckState
	findings = append(findings, e.detectWorkerStuckStates(st, now)...)
	findings = append(findings, e.detectPRStuckStates(st, prs)...)
	if issuesLoaded {
		findings = append(findings, e.detectQueueStuckStates(st, prs, issues, eligible, skipped)...)
		findings = append(findings, detectPolicyStuckStates(skipped)...)
	}
	findings = append(findings, e.detectEnvironmentStuckStates(st, eligible)...)
	return compactStuckStates(findings)
}

func (e *Engine) detectWorkerStuckStates(st *state.State, now time.Time) []state.SupervisorStuckState {
	var findings []state.SupervisorStuckState
	for _, slot := range sortedSessionNames(st) {
		sess := st.Sessions[slot]
		if sess == nil {
			continue
		}
		target := &state.SupervisorTarget{Issue: sess.IssueNumber, PR: sess.PRNumber, Session: slot}

		if sess.Status == state.StatusRunning {
			if sess.PID <= 0 {
				findings = append(findings, stuckState("dead_running_pid", SeverityBlocked,
					fmt.Sprintf("Worker %s is marked running, but no live process is recorded.", slot),
					"Run a Maestro reconciliation cycle or inspect the worker before dispatching more work.", true, target,
					fmt.Sprintf("Session %s status=running pid=%d", slot, sess.PID)))
			} else if !e.pidAlive(sess.PID) {
				findings = append(findings, stuckState("dead_running_pid", SeverityBlocked,
					fmt.Sprintf("Worker %s is marked running, but PID %d is not alive.", slot, sess.PID),
					"Run a Maestro reconciliation cycle so the session can be marked dead and retried if eligible.", true, target,
					fmt.Sprintf("Session %s status=running pid=%d alive=false", slot, sess.PID)))
			}

			maxMinutes := e.cfg.MaxRuntimeMinutes
			if sess.LongRunning {
				maxMinutes *= 2
			}
			if maxMinutes > 0 {
				maxRuntime := time.Duration(maxMinutes) * time.Minute
				if age := now.Sub(sess.StartedAt); age > maxRuntime {
					findings = append(findings, stuckState("worker_timeout", SeverityBlocked,
						fmt.Sprintf("Worker %s exceeded the configured runtime limit.", slot),
						"Stop the timed-out worker and decide whether to retry or split the issue.", true, target,
						fmt.Sprintf("Runtime %s exceeds limit %s", age.Round(time.Second), maxRuntime)))
				}
			}

			if e.cfg.WorkerSilentTimeoutMinutes > 0 && !sess.LastOutputChangedAt.IsZero() {
				timeout := time.Duration(e.cfg.WorkerSilentTimeoutMinutes) * time.Minute
				if silentFor := now.Sub(sess.LastOutputChangedAt); silentFor > timeout {
					findings = append(findings, stuckState("stale_worker_logs", SeverityBlocked,
						fmt.Sprintf("Worker %s has not produced new output within the silent timeout.", slot),
						"Restart or stop the silent worker so the issue can continue.", true, target,
						fmt.Sprintf("Last output changed %s ago; timeout is %s", silentFor.Round(time.Second), timeout)))
				}
			}
		}

		if sess.Status == state.StatusRetryExhausted {
			findings = append(findings, stuckState("retry_exhausted", SeverityBlocked,
				fmt.Sprintf("Issue #%d exhausted its retry budget.", sess.IssueNumber),
				"Review the failed attempts, adjust the issue or retry budget, then restart intentionally.", false, target,
				fmt.Sprintf("Session %s status=retry_exhausted retry_count=%d", slot, sess.RetryCount)))
		}

		if sess.PreviousAttemptFeedbackKind == "review_feedback" && sess.Status != state.StatusRunning {
			canAct := sess.Status == state.StatusDead && sess.NextRetryAt != nil
			findings = append(findings, stuckState("stale_review_feedback", SeverityBlocked,
				fmt.Sprintf("Issue #%d has review feedback, but no worker is currently fixing it.", sess.IssueNumber),
				"Respawn a worker with the saved review feedback or resolve the feedback manually.", canAct, target,
				fmt.Sprintf("Session %s status=%s previous_feedback_kind=review_feedback", slot, sess.Status)))
		}
	}
	return findings
}

func (e *Engine) detectPRStuckStates(st *state.State, prs []github.PR) []state.SupervisorStuckState {
	byNumber := make(map[int]github.PR, len(prs))
	byBranch := make(map[string]github.PR, len(prs))
	for _, pr := range prs {
		byNumber[pr.Number] = pr
		if strings.TrimSpace(pr.HeadRefName) != "" {
			byBranch[pr.HeadRefName] = pr
		}
	}

	ciStatuses := make(map[int]string)
	if ciReader, ok := e.reader.(prCIStatusReader); ok {
		for _, pr := range prs {
			status, err := ciReader.PRCIStatus(pr.Number)
			if err == nil {
				ciStatuses[pr.Number] = status
			}
		}
	}

	var findings []state.SupervisorStuckState
	seenPRs := make(map[int]struct{})
	for _, slot := range sortedSessionNames(st) {
		sess := st.Sessions[slot]
		if sess == nil {
			continue
		}
		pr, found := openPRForSession(sess, byNumber, byBranch)
		target := &state.SupervisorTarget{Issue: sess.IssueNumber, PR: sess.PRNumber, Session: slot}

		if sess.PRNumber > 0 && !found && sessionCanStillBlockProgress(sess.Status) {
			findings = append(findings, stuckState("closed_pr_with_active_session", SeverityBlocked,
				fmt.Sprintf("Session %s records PR #%d, but that PR is not open.", slot, sess.PRNumber),
				"Reconcile the session with the closed PR state before starting duplicate work.", true, target,
				fmt.Sprintf("Session %s status=%s recorded_pr=%d", slot, sess.Status, sess.PRNumber),
				fmt.Sprintf("Open PRs observed: %d", len(prs))))
			continue
		}
		if !found {
			continue
		}
		if sess.PRNumber == 0 {
			target.PR = pr.Number
		}
		if _, ok := seenPRs[pr.Number]; ok {
			continue
		}
		seenPRs[pr.Number] = struct{}{}

		if pr.IsDraft {
			findings = append(findings, stuckState("draft_pr", SeverityInfo,
				fmt.Sprintf("PR #%d is still a draft.", pr.Number),
				"Mark the PR ready for review when implementation is complete.", false, target,
				fmt.Sprintf("PR #%d isDraft=true", pr.Number)))
		}

		switch strings.ToUpper(strings.TrimSpace(pr.Mergeable)) {
		case "CONFLICTING":
			findings = append(findings, stuckState("unmergeable_pr", SeverityBlocked,
				fmt.Sprintf("PR #%d has merge conflicts.", pr.Number),
				"Rebase or resolve conflicts before the PR can merge.", e.cfg.AutoRebase, target,
				fmt.Sprintf("PR #%d mergeable=CONFLICTING", pr.Number)))
		case "UNKNOWN":
			findings = append(findings, stuckState("unmergeable_pr", SeverityWarning,
				fmt.Sprintf("PR #%d mergeability is unknown.", pr.Number),
				"Wait for GitHub to finish computing mergeability or refresh the PR state.", true, target,
				fmt.Sprintf("PR #%d mergeable=UNKNOWN", pr.Number)))
		}

		ciStatus := ciStatuses[pr.Number]
		if ciStatus == "failure" {
			findings = append(findings, stuckState("failing_checks", SeverityBlocked,
				fmt.Sprintf("PR #%d has failing checks.", pr.Number),
				"Capture the failing check output and retry the worker if the retry budget allows.", true, target,
				fmt.Sprintf("PR #%d checks=failure", pr.Number)))
		}

		if e.cfg.ReviewGate == "greptile" && (ciStatus == "" || ciStatus == "success") {
			if greptileReader, ok := e.reader.(prGreptileReader); ok {
				approved, pending, err := greptileReader.PRGreptileApproved(pr.Number)
				if err == nil {
					switch {
					case pending:
						findings = append(findings, stuckState("greptile_pending", SeverityInfo,
							fmt.Sprintf("PR #%d is waiting for Greptile review.", pr.Number),
							"Wait for Greptile to finish before merging.", true, target,
							fmt.Sprintf("PR #%d greptile=pending", pr.Number)))
					case !approved:
						findings = append(findings, stuckState("greptile_not_approved", SeverityBlocked,
							fmt.Sprintf("PR #%d is not approved by Greptile.", pr.Number),
							"Address Greptile feedback or disable the Greptile review gate for this project.", e.cfg.AutoRetryReviewFeedback, target,
							fmt.Sprintf("PR #%d greptile=not_approved", pr.Number)))
					}
				}
			}
		}
	}
	return findings
}

func (e *Engine) detectQueueStuckStates(st *state.State, prs []github.PR, issues, eligible []github.Issue, skipped []string) []state.SupervisorStuckState {
	if len(issues) == 0 {
		if len(st.ActiveSessions()) == 0 && len(prs) == 0 {
			return []state.SupervisorStuckState{stuckState("no_open_issues", SeverityInfo,
				"No open issues are available for Maestro.",
				"Open a GitHub issue or wait for new work to enter the queue.", false, nil,
				"Open issues observed: 0")}
		}
		return nil
	}
	if len(eligible) > 0 {
		return nil
	}

	missingLabelCount := countSkipped(skipped, "missing configured ready label")
	excludedCount := countSkipped(skipped, "excluded by configured label")
	var findings []state.SupervisorStuckState

	if len(e.cfg.IssueLabels) > 0 && missingLabelCount > 0 {
		evidence := append([]string{
			fmt.Sprintf("Configured issue_labels: %s", strings.Join(e.cfg.IssueLabels, ", ")),
			fmt.Sprintf("Open issues observed: %d", len(issues)),
		}, firstEvidence(skipped)...)
		findings = append(findings, stuckState("no_eligible_issues", SeverityWarning,
			"No open issues match the configured ready labels.",
			"Add one of the configured ready labels to an issue or update issue_labels in config.", true, firstMissingLabelTarget(issues, e.cfg.IssueLabels),
			evidence...))
	}

	if excludedCount == len(issues) {
		findings = append(findings, stuckState("all_eligible_issues_excluded", SeverityWarning,
			"Every open issue is excluded by policy labels.",
			"Remove an exclude label from an issue or update exclude_labels in config.", false, nil,
			fmt.Sprintf("Configured exclude_labels: %s", strings.Join(e.cfg.ExcludeLabels, ", ")),
			fmt.Sprintf("Open issues observed: %d", len(issues))))
	}

	if len(skipped) > 0 {
		findings = append(findings, stuckState("ordered_queue_exhausted", SeverityInfo,
			"The ordered issue queue was checked, but every issue was skipped.",
			"Review skipped reasons and make one issue eligible for dispatch.", false, nil,
			append([]string{fmt.Sprintf("Skipped issues: %d", len(skipped))}, firstEvidence(skipped)...)...))
	}

	return findings
}

func detectPolicyStuckStates(skipped []string) []state.SupervisorStuckState {
	var findings []state.SupervisorStuckState
	for _, reason := range firstN(skipped, 3) {
		if !policySkipReason(reason) {
			continue
		}
		findings = append(findings, stuckState("issue_excluded_by_policy", SeverityInfo,
			"An issue was skipped because of Supervisor policy.",
			"Change the issue labels/type or adjust Maestro policy config if the issue should run.", false, targetFromSkipReason(reason), reason))
	}
	return findings
}

func (e *Engine) detectEnvironmentStuckStates(st *state.State, eligible []github.Issue) []state.SupervisorStuckState {
	var findings []state.SupervisorStuckState
	if shouldCheckRuntimeEnvironment(st, eligible) {
		findings = append(findings, e.detectPromptStuckStates()...)
		if missingCLI := e.detectMissingCLI(); missingCLI != nil {
			findings = append(findings, *missingCLI)
		}
	}

	for _, slot := range sortedSessionNames(st) {
		sess := st.Sessions[slot]
		if sess == nil || strings.TrimSpace(sess.Worktree) == "" || strings.TrimSpace(e.cfg.WorktreeBase) == "" {
			continue
		}
		if !pathWithinBase(sess.Worktree, e.cfg.WorktreeBase) {
			findings = append(findings, stuckState("unexpected_worktree_path", SeverityWarning,
				fmt.Sprintf("Session %s uses a worktree outside the configured worktree base.", slot),
				"Move the worktree under worktree_base or update worktree_base to the intended storage location.", false,
				&state.SupervisorTarget{Issue: sess.IssueNumber, PR: sess.PRNumber, Session: slot},
				fmt.Sprintf("worktree=%s", sess.Worktree),
				fmt.Sprintf("worktree_base=%s", e.cfg.WorktreeBase)))
		}
	}

	return findings
}

func (e *Engine) detectPromptStuckStates() []state.SupervisorStuckState {
	paths := []struct {
		name string
		path string
	}{
		{name: "worker_prompt", path: e.cfg.WorkerPrompt},
		{name: "bug_prompt", path: e.cfg.BugPrompt},
		{name: "enhancement_prompt", path: e.cfg.EnhancementPrompt},
		{name: "pipeline.planner.prompt", path: e.cfg.Pipeline.Planner.Prompt},
		{name: "pipeline.validator.prompt", path: e.cfg.Pipeline.Validator.Prompt},
	}
	for i, path := range e.cfg.PromptSections {
		paths = append(paths, struct {
			name string
			path string
		}{name: fmt.Sprintf("prompt_sections[%d]", i), path: path})
	}

	var findings []state.SupervisorStuckState
	for _, item := range paths {
		path := strings.TrimSpace(item.path)
		if path == "" {
			continue
		}
		if _, err := e.stat(path); err != nil {
			code := "missing_prompt"
			severity := SeverityWarning
			summary := fmt.Sprintf("Configured prompt file for %s is not readable.", item.name)
			if os.IsPermission(err) {
				code = "permission_denied"
				severity = SeverityBlocked
				summary = fmt.Sprintf("Configured prompt file for %s cannot be read due to permissions.", item.name)
			} else if os.IsNotExist(err) {
				summary = fmt.Sprintf("Configured prompt file for %s does not exist.", item.name)
			}
			findings = append(findings, stuckState(code, severity, summary,
				"Fix the prompt path or file permissions in Maestro config before dispatching more workers.", false, nil,
				fmt.Sprintf("%s=%s", item.name, path)))
		}
	}
	return findings
}

func (e *Engine) detectMissingCLI() *state.SupervisorStuckState {
	backendName := strings.TrimSpace(e.cfg.Model.Default)
	if backendName == "" {
		backendName = "claude"
	}
	backendDef := e.cfg.Model.Backends[backendName]
	binary := commandBinary(backendDef.Cmd, backendName)
	if binary == "" {
		return nil
	}
	if _, err := e.lookPath(binary); err != nil {
		finding := stuckState("missing_cli", SeverityBlocked,
			fmt.Sprintf("Default backend CLI %q is not available.", binary),
			"Install the backend CLI or update model.default/model.backends in config.", false, nil,
			fmt.Sprintf("model.default=%s", backendName),
			fmt.Sprintf("cmd=%s", binary))
		return &finding
	}
	return nil
}

func (e *Engine) projectState(st *state.State) state.SupervisorProjectState {
	counts := st.CountByStatus()
	return state.SupervisorProjectState{
		Sessions:       len(st.Sessions),
		Running:        counts[state.StatusRunning],
		PROpen:         counts[state.StatusPROpen],
		Queued:         counts[state.StatusQueued],
		RetryExhausted: countSessions(st, state.StatusRetryExhausted),
		AvailableSlots: availableSlots(e.cfg, st),
	}
}

func (e *Engine) policyCandidateIssues(st *state.State, issues []github.Issue) ([]github.Issue, []string, string, error) {
	if !e.cfg.Supervisor.OrderedQueueActive() {
		return issues, nil, e.defaultPolicyRule(), nil
	}
	if err := validateOrderedQueueIssues(e.cfg.Supervisor.OrderedQueue.Issues); err != nil {
		return nil, nil, "", err
	}
	issueByNumber := make(map[int]github.Issue, len(issues))
	for _, issue := range issues {
		issueByNumber[issue.Number] = issue
	}
	var skipped []string
	for _, issueNumber := range e.cfg.Supervisor.OrderedQueue.Issues {
		if st.IssueDone(issueNumber) {
			skipped = append(skipped, fmt.Sprintf("Issue #%d skipped by supervisor.ordered_queue: already completed in state", issueNumber))
			continue
		}
		closed, err := e.reader.IsIssueClosed(issueNumber)
		if err != nil {
			return nil, nil, "", fmt.Errorf("check ordered queue issue #%d: %w", issueNumber, err)
		}
		if closed {
			skipped = append(skipped, fmt.Sprintf("Issue #%d skipped by supervisor.ordered_queue: issue is closed", issueNumber))
			continue
		}
		issue, ok := issueByNumber[issueNumber]
		if !ok {
			return nil, append(skipped, fmt.Sprintf("Issue #%d is first unfinished in supervisor.ordered_queue but was not returned by open issue listing", issueNumber)), PolicyRuleOrderedQueue, nil
		}
		return []github.Issue{issue}, skipped, PolicyRuleOrderedQueue, nil
	}
	return nil, append(skipped, "No unfinished issue remains in supervisor.ordered_queue"), PolicyRuleOrderedQueue, nil
}

func validateOrderedQueueIssues(issues []int) error {
	seen := make(map[int]struct{}, len(issues))
	for i, issueNumber := range issues {
		if issueNumber <= 0 {
			return fmt.Errorf("supervisor ordered_queue issue at index %d must be a positive issue number", i)
		}
		if _, ok := seen[issueNumber]; ok {
			return fmt.Errorf("supervisor ordered_queue issue at index %d duplicates issue #%d", i, issueNumber)
		}
		seen[issueNumber] = struct{}{}
	}
	return nil
}

func (e *Engine) defaultPolicyRule() string {
	if len(e.requiredIssueLabels()) > 0 {
		return PolicyRuleIssueLabels
	}
	return PolicyRuleOpenIssues
}

func (e *Engine) shouldWaitForRunningWorker(st *state.State) bool {
	if e.cfg.Supervisor.OneAtATime {
		return true
	}
	return availableSlots(e.cfg, st) <= 0
}

type queueActionCandidate struct {
	issue         github.Issue
	readyLabel    string
	blockedLabel  string
	addReady      bool
	removeBlocked bool
}

func (c queueActionCandidate) neededMutations() []state.SupervisorMutation {
	var mutations []state.SupervisorMutation
	if c.addReady {
		mutations = append(mutations, state.SupervisorMutation{
			Type:   MutationAddReadyLabel,
			Issue:  c.issue.Number,
			Label:  c.readyLabel,
			Status: MutationStatusPlanned,
		})
	}
	if c.removeBlocked {
		mutations = append(mutations, state.SupervisorMutation{
			Type:   MutationRemoveBlockedLabel,
			Issue:  c.issue.Number,
			Label:  c.blockedLabel,
			Status: MutationStatusPlanned,
		})
	}
	return mutations
}

func (c queueActionCandidate) plannedMutations(cfg *config.Config) []state.SupervisorMutation {
	needed := c.neededMutations()
	mutations := make([]state.SupervisorMutation, 0, len(needed))
	for _, mutation := range needed {
		if safeActionAllowed(cfg, mutation.Type) {
			mutations = append(mutations, mutation)
		}
	}
	return mutations
}

func safeActionAllowed(cfg *config.Config, action string) bool {
	if cfg == nil {
		return false
	}
	return cfg.Supervisor.AllowsSafeAction(action)
}

func (e *Engine) firstQueueActionCandidate(st *state.State, issues []github.Issue) (*queueActionCandidate, error) {
	readyLabel := e.readyLabel()
	blockedLabel := e.blockedLabel()
	if readyLabel == "" && blockedLabel == "" {
		return nil, nil
	}

	for _, issue := range issues {
		hasReadyLabel := readyLabel == "" || github.HasLabel(issue, []string{readyLabel})
		hasBlockedLabel := blockedLabel != "" && github.HasLabel(issue, []string{blockedLabel})
		addReady := readyLabel != "" && !hasReadyLabel && !supervisorMutationSucceeded(st, issue.Number, MutationAddReadyLabel, readyLabel)
		removeBlocked := hasBlockedLabel && !supervisorMutationSucceeded(st, issue.Number, MutationRemoveBlockedLabel, blockedLabel)
		candidate := queueActionCandidate{
			issue:         issue,
			readyLabel:    readyLabel,
			blockedLabel:  blockedLabel,
			addReady:      addReady,
			removeBlocked: removeBlocked,
		}
		if !candidate.addReady && !candidate.removeBlocked {
			continue
		}

		reason, err := e.issueQueueSkipReason(st, issue, blockedLabel)
		if err != nil {
			return nil, err
		}
		if reason != "" {
			continue
		}
		return &candidate, nil
	}
	return nil, nil
}

func supervisorMutationSucceeded(st *state.State, issueNumber int, mutationType, label string) bool {
	if st == nil {
		return false
	}
	for _, decision := range st.SupervisorDecisions {
		for _, mutation := range decision.Mutations {
			if mutation.Status != MutationStatusSucceeded {
				continue
			}
			if mutation.Issue == issueNumber && mutation.Type == mutationType && strings.EqualFold(mutation.Label, label) {
				return true
			}
		}
	}
	return false
}

func (e *Engine) eligibleIssues(st *state.State, issues []github.Issue, requireLabels bool) ([]github.Issue, []string, error) {
	var eligible []github.Issue
	var skipped []string
	requiredLabels := e.requiredIssueLabels()
	for _, issue := range issues {
		if requireLabels && !matchesRequiredLabels(issue, requiredLabels) {
			skipped = append(skipped, fmt.Sprintf("Issue #%d skipped: missing configured ready label", issue.Number))
			continue
		}
		reason, err := e.issueSkipReason(st, issue)
		if err != nil {
			return nil, nil, err
		}
		if reason != "" {
			skipped = append(skipped, fmt.Sprintf("Issue #%d skipped: %s", issue.Number, reason))
			continue
		}
		eligible = append(eligible, issue)
	}
	return eligible, skipped, nil
}

func (e *Engine) issueSkipReason(st *state.State, issue github.Issue) (string, error) {
	return e.issueSkipReasonWithExcludeLabels(st, issue, e.excludeLabels(), "")
}

func (e *Engine) issueQueueSkipReason(st *state.State, issue github.Issue, blockedLabel string) (string, error) {
	return e.issueSkipReasonWithExcludeLabels(st, issue, excludeLabelsExcept(e.excludeLabels(), blockedLabel), blockedLabel)
}

func (e *Engine) issueSkipReasonWithExcludeLabels(st *state.State, issue github.Issue, excludeLabels []string, ignoredBlockedLabel string) (string, error) {
	if st.IssueInProgress(issue.Number) {
		return "already in progress", nil
	}
	if st.IssueDone(issue.Number) {
		return "already completed in state", nil
	}
	if st.IssueRetryExhausted(issue.Number) {
		return "retry limit exhausted", nil
	}
	if e.cfg.MaxRetriesPerIssue > 0 && st.FailedAttemptsForIssue(issue.Number) >= e.cfg.MaxRetriesPerIssue {
		return "retry limit exhausted", nil
	}
	if st.IsMissionParent(issue.Number) {
		return "mission parent issue", nil
	}
	if e.cfg.Missions.Enabled && mission.IsMissionIssue(issue, e.cfg.Missions.Labels) && !st.IsMissionChild(issue.Number) {
		return "mission issue awaits decomposition", nil
	}
	if github.HasLabel(issue, excludeLabels) {
		return "excluded by configured label", nil
	}
	if blockedLabel := strings.TrimSpace(e.cfg.Supervisor.BlockedLabel); blockedLabel != "" && !strings.EqualFold(blockedLabel, ignoredBlockedLabel) && github.HasLabel(issue, []string{blockedLabel}) {
		return "blocked by supervisor policy label", nil
	}
	if github.HasLabel(issue, excludeLabelsExcept(e.policyExcludedLabels(), ignoredBlockedLabel)) {
		return "excluded by supervisor policy label", nil
	}
	if len(e.cfg.BlockerPatterns) > 0 {
		blockers := github.FindBlockers(issue.Body, e.cfg.BlockerPatterns)
		openBlockers, err := e.openBlockers(blockers)
		if err != nil {
			return "", err
		}
		if len(openBlockers) > 0 {
			return fmt.Sprintf("blocked by open issue(s) %s", issueRefs(openBlockers)), nil
		}
	}
	return "", nil
}

func (e *Engine) openBlockers(blockers []int) ([]int, error) {
	var open []int
	for _, blocker := range blockers {
		closed, err := e.reader.IsIssueClosed(blocker)
		if err != nil {
			return nil, fmt.Errorf("check blocker #%d: %w", blocker, err)
		}
		if !closed {
			open = append(open, blocker)
		}
	}
	return open, nil
}

func sessionWithOpenPR(st *state.State, prs []github.PR) (string, *state.Session, github.PR, bool) {
	branchToPR := make(map[string]github.PR, len(prs))
	numberToPR := make(map[int]github.PR, len(prs))
	for _, pr := range prs {
		branchToPR[pr.HeadRefName] = pr
		numberToPR[pr.Number] = pr
	}
	for _, slot := range sortedSessionNames(st) {
		sess := st.Sessions[slot]
		if sess == nil {
			continue
		}
		if sess.Branch != "" {
			if pr, ok := branchToPR[sess.Branch]; ok {
				return slot, sess, pr, true
			}
		}
		if sess.PRNumber > 0 {
			if pr, ok := numberToPR[sess.PRNumber]; ok {
				return slot, sess, pr, true
			}
		}
	}
	return "", nil, github.PR{}, false
}

func runningSession(st *state.State) (string, *state.Session, bool) {
	for _, slot := range sortedSessionNames(st) {
		sess := st.Sessions[slot]
		if sess != nil && sess.Status == state.StatusRunning {
			return slot, sess, true
		}
	}
	return "", nil, false
}

func retryExhaustedSession(st *state.State) (string, *state.Session, bool) {
	for _, slot := range sortedSessionNames(st) {
		sess := st.Sessions[slot]
		if sess != nil && sess.Status == state.StatusRetryExhausted {
			return slot, sess, true
		}
	}
	return "", nil, false
}

func sortedSessionNames(st *state.State) []string {
	names := make([]string, 0, len(st.Sessions))
	for name := range st.Sessions {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func availableSlots(cfg *config.Config, st *state.State) int {
	maxParallel := cfg.MaxParallel
	active := len(st.ActiveSessions())
	slots := maxParallel - active
	if limit, ok := cfg.MaxConcurrentByState["running"]; ok && limit > 0 {
		runningSlots := limit - st.CountByStatus()[state.StatusRunning]
		if runningSlots < slots {
			slots = runningSlots
		}
	}
	if slots < 0 {
		return 0
	}
	return slots
}

func countSessions(st *state.State, status state.SessionStatus) int {
	count := 0
	for _, sess := range st.Sessions {
		if sess != nil && sess.Status == status {
			count++
		}
	}
	return count
}

func matchesRequiredLabels(issue github.Issue, labels []string) bool {
	if len(labels) == 0 {
		return true
	}
	return github.HasLabel(issue, labels)
}

func (e *Engine) requiredIssueLabels() []string {
	labels := append([]string(nil), e.cfg.IssueLabels...)
	readyLabel := strings.TrimSpace(e.cfg.Supervisor.ReadyLabel)
	if readyLabel == "" {
		return labels
	}
	for _, label := range labels {
		if strings.EqualFold(label, readyLabel) {
			return labels
		}
	}
	return append(labels, readyLabel)
}

func (e *Engine) policySummaryReason() string {
	mode := strings.TrimSpace(e.cfg.Supervisor.Mode)
	if mode == "" {
		mode = "cautious"
	}
	parts := []string{
		fmt.Sprintf("mode=%s", mode),
	}
	if e.cfg.Supervisor.Enabled {
		parts = append(parts, "enabled=true")
	}
	if e.cfg.Supervisor.OrderedQueueActive() {
		parts = append(parts, fmt.Sprintf("ordered_queue=%d issue(s)", len(e.cfg.Supervisor.OrderedQueue.Issues)))
	}
	if excludedLabels := e.policyExcludedLabels(); len(excludedLabels) > 0 {
		parts = append(parts, "excluded_labels="+strings.Join(excludedLabels, ","))
	}
	if len(e.cfg.Supervisor.SafeActions) > 0 {
		parts = append(parts, "safe_actions="+strings.Join(e.cfg.Supervisor.SafeActions, ","))
	}
	if len(e.cfg.Supervisor.ApprovalRequired) > 0 {
		parts = append(parts, "approval_required="+strings.Join(e.cfg.Supervisor.ApprovalRequired, ","))
	}
	return "Supervisor policy: " + strings.Join(parts, "; ")
}

func (e *Engine) policyExcludedLabels() []string {
	if e.cfg.Supervisor.ExcludedLabels == nil && len(e.cfg.Supervisor.AllowIssueTypes) == 0 {
		return []string{"epic", "meta"}
	}
	return e.cfg.Supervisor.ExcludedLabels
}

func policyRuleReason(policyRule string) string {
	if strings.TrimSpace(policyRule) == "" {
		return ""
	}
	return "Policy rule: " + policyRule
}

func issueLabelReason(labels []string) string {
	if len(labels) == 0 {
		return "Config has no issue label filter"
	}
	return "Config requires one of issue_labels: " + strings.Join(labels, ", ")
}

func (e *Engine) readyLabel() string {
	if label := strings.TrimSpace(e.cfg.Supervisor.ReadyLabel); label != "" {
		return label
	}
	for _, label := range e.cfg.IssueLabels {
		if label = strings.TrimSpace(label); label != "" {
			return label
		}
	}
	return ""
}

func (e *Engine) blockedLabel() string {
	if label := strings.TrimSpace(e.cfg.Supervisor.BlockedLabel); label != "" {
		return label
	}
	for _, label := range e.cfg.ExcludeLabels {
		label = strings.TrimSpace(label)
		if strings.EqualFold(label, "blocked") {
			return label
		}
	}
	return ""
}

func (e *Engine) excludeLabels() []string {
	labels := append([]string(nil), e.cfg.ExcludeLabels...)
	blockedLabel := strings.TrimSpace(e.cfg.Supervisor.BlockedLabel)
	if blockedLabel != "" && !hasLabelName(labels, blockedLabel) {
		labels = append(labels, blockedLabel)
	}
	return labels
}

func hasLabelName(labels []string, target string) bool {
	target = strings.TrimSpace(target)
	for _, label := range labels {
		if strings.EqualFold(strings.TrimSpace(label), target) {
			return true
		}
	}
	return false
}

func queueLabelReason(readyLabel, blockedLabel string) string {
	var parts []string
	if readyLabel != "" {
		parts = append(parts, "ready label: "+readyLabel)
	}
	if blockedLabel != "" {
		parts = append(parts, "blocked label: "+blockedLabel)
	}
	if len(parts) == 0 {
		return "No supervisor queue labels are configured"
	}
	return "Supervisor queue labels configured (" + strings.Join(parts, ", ") + ")"
}

func excludeLabelsExcept(labels []string, except string) []string {
	except = strings.TrimSpace(except)
	if except == "" {
		return labels
	}
	filtered := make([]string, 0, len(labels))
	for _, label := range labels {
		if strings.EqualFold(strings.TrimSpace(label), except) {
			continue
		}
		filtered = append(filtered, label)
	}
	return filtered
}

func plannedMutationPhrase(mutations []state.SupervisorMutation) string {
	descriptions := make([]string, 0, len(mutations))
	for _, mutation := range mutations {
		descriptions = append(descriptions, mutationDescription(mutation))
	}
	return strings.Join(descriptions, " and ")
}

func mutationDescription(mutation state.SupervisorMutation) string {
	switch mutation.Type {
	case MutationAddReadyLabel:
		return fmt.Sprintf("adding `%s`", mutation.Label)
	case MutationRemoveBlockedLabel:
		return fmt.Sprintf("removing `%s`", mutation.Label)
	case MutationIssueComment:
		return "adding an issue comment"
	default:
		return mutation.Type
	}
}

func issueRefs(numbers []int) string {
	refs := make([]string, len(numbers))
	for i, n := range numbers {
		refs[i] = fmt.Sprintf("#%d", n)
	}
	return strings.Join(refs, ", ")
}

func applyQueueAction(cfg *config.Config, decision *state.SupervisorDecision, mutator Mutator) {
	decision.Mode = ModeSafeActions
	decision.Status = DecisionStatusSucceeded

	completed := make([]string, 0, len(decision.Mutations))
	for i := range decision.Mutations {
		mutation := decision.Mutations[i]
		if err := applyQueueMutation(mutator, mutation); err != nil {
			markQueueActionFailed(decision, i, classifyGitHubError(err))
			return
		}
		decision.Mutations[i].Status = MutationStatusSucceeded
		completed = append(completed, completedMutationPhrase(mutation))
	}

	if cfg.Supervisor.QueueComments && safeActionAllowed(cfg, config.SupervisorActionAddIssueComment) && len(completed) > 0 && decision.Target != nil && decision.Target.Issue > 0 {
		comment := state.SupervisorMutation{
			Type:   MutationIssueComment,
			Issue:  decision.Target.Issue,
			Status: MutationStatusPlanned,
		}
		decision.Mutations = append(decision.Mutations, comment)
		commentIndex := len(decision.Mutations) - 1
		if err := mutator.CommentIssue(decision.Target.Issue, queueActionComment(completed)); err != nil {
			markQueueActionFailed(decision, commentIndex, classifyGitHubError(err))
			return
		}
		decision.Mutations[commentIndex].Status = MutationStatusSucceeded
	}
}

func applyQueueMutation(mutator Mutator, mutation state.SupervisorMutation) error {
	switch mutation.Type {
	case MutationAddReadyLabel:
		return mutator.AddIssueLabel(mutation.Issue, mutation.Label)
	case MutationRemoveBlockedLabel:
		return mutator.RemoveIssueLabel(mutation.Issue, mutation.Label)
	default:
		return fmt.Errorf("unsupported queue mutation %q", mutation.Type)
	}
}

func markUnsupportedQueueAction(decision *state.SupervisorDecision) {
	decision.Mode = ModeSafeActions
	decision.Status = DecisionStatusFailed
	decision.ErrorClass = ErrorClassUnsupportedClient
	decision.Summary = "Supervisor queue action could not run because the GitHub client does not support safe mutations."
	for i := range decision.Mutations {
		if decision.Mutations[i].Status == MutationStatusPlanned {
			decision.Mutations[i].Status = MutationStatusFailed
			decision.Mutations[i].ErrorClass = ErrorClassUnsupportedClient
			break
		}
	}
	decision.Reasons = appendReasons(decision.Reasons, "Supervisor queue mutation failed with error class: "+ErrorClassUnsupportedClient)
}

func markQueueActionFailed(decision *state.SupervisorDecision, mutationIndex int, errorClass string) {
	decision.Status = DecisionStatusFailed
	decision.ErrorClass = errorClass
	if mutationIndex >= 0 && mutationIndex < len(decision.Mutations) {
		decision.Mutations[mutationIndex].Status = MutationStatusFailed
		decision.Mutations[mutationIndex].ErrorClass = errorClass
	}
	issue := 0
	if decision.Target != nil {
		issue = decision.Target.Issue
	}
	if issue > 0 {
		decision.Summary = fmt.Sprintf("Supervisor queue action failed for issue #%d (%s).", issue, errorClass)
	} else {
		decision.Summary = fmt.Sprintf("Supervisor queue action failed (%s).", errorClass)
	}
	decision.Reasons = appendReasons(decision.Reasons, "Supervisor queue mutation failed with error class: "+errorClass)
}

func classifyGitHubError(err error) string {
	if err == nil {
		return ""
	}
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "rate limit") || strings.Contains(msg, "secondary rate"):
		return ErrorClassGitHubRateLimited
	case strings.Contains(msg, "not found") || strings.Contains(msg, "404"):
		return ErrorClassGitHubNotFound
	case strings.Contains(msg, "unauthorized") || strings.Contains(msg, "authentication") || strings.Contains(msg, "permission") || strings.Contains(msg, "403") || strings.Contains(msg, "401"):
		return ErrorClassGitHubAuth
	default:
		return ErrorClassGitHubAPI
	}
}

func completedMutationPhrase(mutation state.SupervisorMutation) string {
	switch mutation.Type {
	case MutationAddReadyLabel:
		return fmt.Sprintf("added `%s`", mutation.Label)
	case MutationRemoveBlockedLabel:
		return fmt.Sprintf("removed `%s`", mutation.Label)
	default:
		return mutation.Type
	}
}

func queueActionComment(actions []string) string {
	return "Maestro queue action: " + strings.Join(actions, "; ") + "."
}

func appendReasons(base []string, extra ...string) []string {
	reasons := append([]string(nil), base...)
	reasons = append(reasons, extra...)
	return compactReasons(reasons)
}

func compactReasons(reasons []string) []string {
	seen := make(map[string]struct{}, len(reasons))
	compact := make([]string, 0, len(reasons))
	for _, reason := range reasons {
		reason = strings.TrimSpace(reason)
		if reason == "" {
			continue
		}
		if _, ok := seen[reason]; ok {
			continue
		}
		seen[reason] = struct{}{}
		compact = append(compact, reason)
	}
	return compact
}

func firstN(values []string, n int) []string {
	if len(values) <= n {
		return values
	}
	return values[:n]
}

func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return proc.Signal(syscall.Signal(0)) == nil
}

func stuckState(code, severity, summary, recommendedAction string, supervisorCanAct bool, target *state.SupervisorTarget, evidence ...string) state.SupervisorStuckState {
	return state.SupervisorStuckState{
		Code:              code,
		Severity:          severity,
		Summary:           summary,
		Evidence:          compactReasons(evidence),
		RecommendedAction: recommendedAction,
		SupervisorCanAct:  supervisorCanAct,
		Target:            target,
	}
}

func compactStuckStates(findings []state.SupervisorStuckState) []state.SupervisorStuckState {
	seen := make(map[string]struct{}, len(findings))
	compact := make([]state.SupervisorStuckState, 0, len(findings))
	for _, finding := range findings {
		finding.Code = strings.TrimSpace(finding.Code)
		finding.Severity = strings.TrimSpace(finding.Severity)
		finding.Summary = strings.TrimSpace(finding.Summary)
		finding.RecommendedAction = strings.TrimSpace(finding.RecommendedAction)
		finding.Evidence = compactReasons(finding.Evidence)
		if finding.Code == "" || finding.Summary == "" {
			continue
		}
		key := finding.Code + "|" + supervisorTargetKey(finding.Target) + "|" + finding.Summary
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		compact = append(compact, finding)
	}
	return compact
}

func supervisorTargetKey(target *state.SupervisorTarget) string {
	if target == nil {
		return ""
	}
	return fmt.Sprintf("issue=%d/pr=%d/session=%s", target.Issue, target.PR, target.Session)
}

func openPRForSession(sess *state.Session, byNumber map[int]github.PR, byBranch map[string]github.PR) (github.PR, bool) {
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

func sessionCanStillBlockProgress(status state.SessionStatus) bool {
	switch status {
	case state.StatusRunning, state.StatusPROpen, state.StatusQueued, state.StatusFailed, state.StatusDead, state.StatusRetryExhausted:
		return true
	}
	return false
}

func countSkipped(skipped []string, contains string) int {
	count := 0
	for _, reason := range skipped {
		if strings.Contains(reason, contains) {
			count++
		}
	}
	return count
}

func firstEvidence(values []string) []string {
	return firstN(values, 3)
}

func firstMissingLabelTarget(issues []github.Issue, labels []string) *state.SupervisorTarget {
	for _, issue := range issues {
		if !matchesRequiredLabels(issue, labels) {
			return &state.SupervisorTarget{Issue: issue.Number}
		}
	}
	return nil
}

func policySkipReason(reason string) bool {
	return strings.Contains(reason, "excluded by configured label") ||
		strings.Contains(reason, "mission parent issue") ||
		strings.Contains(reason, "mission issue awaits decomposition") ||
		strings.Contains(reason, "blocked by open issue")
}

func targetFromSkipReason(reason string) *state.SupervisorTarget {
	var issue int
	if _, err := fmt.Sscanf(reason, "Issue #%d", &issue); err == nil && issue > 0 {
		return &state.SupervisorTarget{Issue: issue}
	}
	return nil
}

func shouldCheckRuntimeEnvironment(st *state.State, eligible []github.Issue) bool {
	if len(eligible) > 0 {
		return true
	}
	for _, sess := range st.Sessions {
		if sess == nil {
			continue
		}
		if sess.Status == state.StatusQueued || (sess.Status == state.StatusDead && sess.NextRetryAt != nil) {
			return true
		}
	}
	return false
}

func pathWithinBase(path, base string) bool {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return true
	}
	absBase, err := filepath.Abs(base)
	if err != nil {
		return true
	}
	rel, err := filepath.Rel(absBase, absPath)
	if err != nil {
		return true
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator)))
}

func commandBinary(cmd, fallback string) string {
	cmd = strings.TrimSpace(cmd)
	if cmd == "" {
		cmd = strings.TrimSpace(fallback)
	}
	fields := strings.Fields(cmd)
	if len(fields) == 0 {
		return ""
	}
	return fields[0]
}
