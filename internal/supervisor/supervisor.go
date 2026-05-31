package supervisor

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/befeast/maestro/internal/approver"
	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/github"
	"github.com/befeast/maestro/internal/mission"
	"github.com/befeast/maestro/internal/outcome"
	"github.com/befeast/maestro/internal/state"
	"github.com/befeast/maestro/internal/worker"
)

const (
	ModeReadOnly    = "read_only"
	ModeSafeActions = "safe_actions"

	ActionNone                 = "none"
	ActionWaitForRunningWorker = "wait_for_running_worker"
	ActionWaitForCapacity      = "wait_for_capacity"
	ActionWaitForOrderedQueue  = "wait_for_ordered_queue"
	ActionMonitorOpenPR        = "monitor_open_pr"
	ActionReviewRetryExhausted = "review_retry_exhausted"
	ActionCheckOutcomeHealth   = "check_outcome_health"
	ActionNotifyRed            = "notify_red"
	ActionSpawnWorker          = "spawn_worker"
	ActionSpawnRepairWorker    = "spawn_repair_worker"
	ActionLabelIssueReady      = "label_issue_ready"
	ActionMergePR              = "merge_pr"
	// ActionOpenChildIssue asks the supervisor (or operator) to create the
	// next concrete child issue from an open handoff/epic when no runnable
	// issue remains. Approval-gated in v1; the safe-action executor for
	// `open_child_issue` is wired up by a follow-up PR.
	ActionOpenChildIssue = "open_child_issue"
	// ActionPreflightFailed is emitted when a configured preflight gate
	// fails. The supervisor refuses to recommend spawn/open-child until the
	// operator clears the failure.
	ActionPreflightFailed = "preflight_failed"

	RiskSafe          = "safe"
	RiskMutating      = "mutating"
	RiskApprovalGated = "approval_gated"

	PolicyRuleRuntimeState   = "runtime_state"
	PolicyRuleOpenIssues     = "open_issues"
	PolicyRuleIssueLabels    = "issue_labels"
	PolicyRuleOrderedQueue   = "supervisor.ordered_queue"
	PolicyRuleDynamicWave    = "supervisor.dynamic_wave"
	PolicyRuleExcludedLabels = "supervisor.excluded_labels"

	DecisionStatusRecommended = "recommended"
	DecisionStatusSucceeded   = "succeeded"
	DecisionStatusFailed      = "failed"

	MutationAddReadyLabel      = "add_ready_label"
	MutationRemoveReadyLabel   = "remove_ready_label"
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
	HasMergedPRForIssue(issueNumber int) (bool, error)
	IsIssueClosed(number int) (bool, error)
	IsPRMerged(prNumber int) (bool, error)
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

// prMergeableReader fetches the per-PR mergeable state via the
// SINGLE-PR endpoint (`gh api repos/.../pulls/{N}`) which triggers
// GitHub's mergeability computation. The LIST endpoint (used by
// ListOpenPRs) always returns mergeable=null, so reading
// `pr.Mergeable` from a list result is always UNKNOWN. #543.
type prMergeableReader interface {
	PRMergeable(prNumber int) (string, error)
}

// PreflightResult is the outcome of running a configured preflight command.
// Ok=true means the gate passed and the supervisor may continue dispatching
// spawn/open-child actions. Ok=false carries a human-readable Reason that is
// surfaced in the stuck-state evidence and CLI output. Output is the
// trimmed combined output of the configured command (optional, for audit).
type PreflightResult struct {
	Ok       bool
	Reason   string
	ExitCode int
	Output   string
}

// PreflightRunner runs the configured supervisor preflight command. Tests
// inject a fake runner; production wires a shell executor via the default
// engine factory.
type PreflightRunner func(command string) PreflightResult

// Engine makes deterministic supervisor decisions. It plans safe queue mutations
// and emits structured stuck-state explanations.
type Engine struct {
	cfg       *config.Config
	reader    Reader
	llm       LLMClient
	now       func() time.Time
	pidAlive  func(pid int) bool
	stat      func(name string) (os.FileInfo, error)
	lookPath  func(file string) (string, error)
	preflight PreflightRunner
}

func NewEngine(cfg *config.Config, reader Reader) *Engine {
	if reader == nil {
		reader = github.New(cfg.Repo)
	}
	eng := &Engine{
		cfg:       cfg,
		reader:    reader,
		now:       func() time.Time { return time.Now().UTC() },
		pidAlive:  pidAlive,
		stat:      os.Stat,
		lookPath:  exec.LookPath,
		preflight: defaultPreflightRunner,
	}
	if cfg != nil && cfg.Supervisor.Enabled {
		eng.llm = NewBackendLLMClient(cfg)
	}
	return eng
}

// defaultPreflightRunner shells out to `bash -c <command>` and returns
// Ok=true when the command exits 0. Empty command is treated as
// "no gate configured" → Ok=true.
func defaultPreflightRunner(command string) PreflightResult {
	command = strings.TrimSpace(command)
	if command == "" {
		return PreflightResult{Ok: true}
	}
	cmd := exec.Command("bash", "-lc", command)
	out, err := cmd.CombinedOutput()
	exit := 0
	if err != nil {
		exit = -1
		if ee, ok := err.(*exec.ExitError); ok {
			exit = ee.ExitCode()
		}
	}
	trimmed := strings.TrimSpace(string(out))
	if err != nil {
		reason := fmt.Sprintf("preflight command exited with %d", exit)
		if trimmed != "" {
			reason = fmt.Sprintf("%s: %s", reason, firstLine(trimmed))
		}
		return PreflightResult{Ok: false, Reason: reason, ExitCode: exit, Output: trimmed}
	}
	return PreflightResult{Ok: true, ExitCode: exit, Output: trimmed}
}

func firstLine(s string) string {
	if idx := strings.IndexAny(s, "\r\n"); idx >= 0 {
		return strings.TrimSpace(s[:idx])
	}
	return strings.TrimSpace(s)
}

// RunOnce records one supervisor decision in Maestro state and applies any safe
// queue mutations selected by the decision.
func RunOnce(cfg *config.Config, reader Reader) (state.SupervisorDecision, error) {
	st, err := state.Load(cfg.StateDir)
	if err != nil {
		return state.SupervisorDecision{}, fmt.Errorf("load state: %w", err)
	}
	st.MarkStaleApprovals(time.Now().UTC())
	if reader == nil {
		reader = github.New(cfg.Repo)
	}
	// NOTE: approval execution must NOT run in dry-run mode (greptile
	// P1 on #480) — the side effects are real GH/fs operations and the
	// state.Save that records the executed/failed transition lives
	// inside the !DryRun guard below, so dry-run would also re-execute
	// the same approvals on every cycle. We move the call there.
	recordOutcomeHealth(cfg, st)

	decision, err := NewEngine(cfg, reader).Decide(st)
	if err != nil {
		return state.SupervisorDecision{}, err
	}
	if !cfg.Supervisor.DryRun {
		// Execute any approvals that were transitioned to status=approved
		// outside this loop (CLI approve already executes inline; this
		// pass catches web-driven approves and replays after a daemon
		// restart). Lives inside the dry-run guard so the resulting
		// state transitions are persisted by the state.Save below.
		executeApprovedApprovals(cfg, st, reader)
		if decisionRequiresApproval(cfg, decision) {
			// #505: gate the mint on the executor registry. A verb the
			// executor cannot handle would otherwise pile up
			// execution_failed records every cycle (live evidence:
			// spawn_repair_worker on dogfood 2026-05-30, 4 stuck records
			// before the operator noticed). Refuse at mint time so the
			// failure surfaces loudly in the journal instead of silently
			// in state.json.
			if !approver.IsKnownApprovalAction(decision.RecommendedAction) {
				log.Printf("[supervisor] refusing to mint approval: action %q is not in the executor registry; supported actions = %v", decision.RecommendedAction, approver.KnownApprovalActionList())
			} else {
				approval := st.RecordPendingApprovalForDecision(decision, decision.CreatedAt)
				decision.ApprovalID = approval.ID
			}
		}
		if len(decision.Mutations) > 0 && decision.Risk == RiskSafe {
			mutator, ok := reader.(Mutator)
			if !ok {
				markUnsupportedQueueAction(&decision)
			} else {
				applyQueueAction(cfg, &decision, mutator)
			}
		}
		st.RecordSupervisorDecision(decision, state.DefaultSupervisorDecisionLimit)
		// Phase 1.2 (#499): stamp the last-run heartbeat just before save
		// so the watchdog goroutine in cmd/maestro can see this cycle
		// completed. Also clear any stale SupervisorStuck flag set by a
		// previous watchdog tick — a successful RunOnce is the only
		// signal that unwedges the daemon.
		st.LastRunOnceAt = time.Now().UTC()
		st.SupervisorStuck = false
		st.SupervisorStuckReason = ""
		if err := state.Save(cfg.StateDir, st); err != nil {
			return state.SupervisorDecision{}, fmt.Errorf("save state: %w", err)
		}
	}
	return decision, nil
}

func recordOutcomeHealth(cfg *config.Config, st *state.State) {
	if cfg == nil || st == nil {
		return
	}
	if !cfg.Outcome.Configured() || !cfg.Outcome.HasHealthSignal() {
		return
	}
	result := outcome.Checker{}.Check(context.Background(), cfg.Outcome)
	st.OutcomeHealth = &result
}

// Decide observes state and GitHub read-only data, then returns the next recommendation.
func (e *Engine) Decide(st *state.State) (state.SupervisorDecision, error) {
	var decision state.SupervisorDecision
	var err error
	if e.cfg.Supervisor.Enabled {
		decision, err = e.decideWithLLM(st)
	} else {
		decision, err = e.decideDeterministic(st)
	}
	if err != nil {
		return state.SupervisorDecision{}, err
	}
	outcomeStatus := e.outcomeStatus(st)
	decision.Outcome = &outcomeStatus
	// Stamp the project repo on every decision so RecordPendingApprovalForDecision
	// carries it onto the Approval (#489). Defends against cross-project
	// mutation if Executor wiring ever drifts.
	if decision.Repo == "" {
		decision.Repo = strings.TrimSpace(e.cfg.Repo)
	}
	return decision, nil
}

func (e *Engine) decideDeterministic(st *state.State) (state.SupervisorDecision, error) {
	if st == nil {
		st = state.NewState()
	}
	now := e.now().UTC()
	projectState := e.projectState(st)
	outcomeStatus := e.outcomeStatus(st)
	baseReasons := []string{
		fmt.Sprintf("State has %d session(s)", projectState.Sessions),
		fmt.Sprintf("%d active session(s) count against %d max parallel slot(s)", len(st.ActiveSessions()), e.cfg.MaxParallel),
		e.policySummaryReason(),
		outcomeDecisionReason(outcomeStatus),
	}

	prs, err := e.reader.ListOpenPRs()
	if err != nil {
		return state.SupervisorDecision{}, fmt.Errorf("list open PRs: %w", err)
	}
	projectState.OpenPRs = len(prs)
	cache := newResolutionCache(e.reader)
	stuckStates := e.detectStuckStates(st, now, prs, nil, nil, nil, false, cache)

	if slot, sess, pr, ok := e.sessionWithOpenPR(st, prs, cache); ok {
		if e.openPRNeedsRepair(st, stuckStates, slot, sess, pr) {
			reasons := appendReasons(baseReasons,
				fmt.Sprintf("Session %s is associated with open PR #%d, but the PR is not progressing", slot, pr.Number),
				"The worker is not actively repairing this PR while it is draft, failing checks, blocked by review, or the live outcome is still failing",
				"Starting a repair worker would mutate local worktrees, so supervisor records an explicit repair recommendation",
			)
			decision := e.decision(st, now, projectState, ActionSpawnRepairWorker,
				fmt.Sprintf("Start a repair worker for issue #%d; PR #%d is not enough to prove the live outcome.", sess.IssueNumber, pr.Number),
				RiskMutating, 0.9, &state.SupervisorTarget{Issue: sess.IssueNumber, PR: pr.Number, Session: slot}, PolicyRuleRuntimeState, reasons)
			decision.StuckStates = stuckStates
			return decision, nil
		}
		// #512 (Phase 1.6): if the PR is actually ready to merge — not draft,
		// mergeable, CI green, Greptile approved (when configured) — recommend
		// merge_pr (risk=mutating, cautious-gate gates approval). Without this
		// rule, every CLEAN/SUCCESS PR sat in pr_open forever while supervisor
		// emitted monitor_open_pr loops; the reasoning even claimed "Maestro will
		// merge when the merge gate allows it" — aspirational, never implemented.
		if ready, mergeReasons := e.openPRReadyToMerge(slot, sess, pr); ready {
			summary := fmt.Sprintf("Merge PR #%d for issue #%d — checks green, mergeable, review gates passed.", pr.Number, sess.IssueNumber)
			reasons := appendReasons(baseReasons, mergeReasons...)
			decision := e.decision(st, now, projectState, ActionMergePR,
				summary,
				RiskMutating, 0.9, &state.SupervisorTarget{Issue: sess.IssueNumber, PR: pr.Number, Session: slot}, PolicyRuleRuntimeState, reasons)
			decision.StuckStates = stuckStates
			return decision, nil
		}

		// PR exists but is not yet merge-ready. Stay on monitor_open_pr but
		// surface the actual blocker (CI pending, review missing, draft, etc.)
		// in the summary instead of claiming auto-merge will happen.
		monitorReasons := e.monitorOpenPRReasons(slot, sess, pr)
		summary := fmt.Sprintf("Monitor PR #%d for issue #%d: %s", pr.Number, sess.IssueNumber, summarizeMonitorReasons(monitorReasons))
		reasons := appendReasons(baseReasons,
			fmt.Sprintf("Session %s is associated with open PR #%d", slot, pr.Number),
			"No GitHub mutation is needed for supervisor mode",
		)
		reasons = appendReasons(reasons, monitorReasons...)
		if sess.Status == state.StatusRetryExhausted {
			reasons = appendReasons(reasons,
				fmt.Sprintf("Session %s is retry_exhausted but still has open PR #%d", slot, pr.Number),
				"Retry exhaustion does not block normal PR merge flow when checks and review gates pass",
			)
		}
		decision := e.decision(st, now, projectState, ActionMonitorOpenPR,
			summary,
			RiskSafe, 0.9, &state.SupervisorTarget{Issue: sess.IssueNumber, PR: pr.Number, Session: slot}, PolicyRuleRuntimeState, reasons)
		decision.StuckStates = stuckStates
		return decision, nil
	}

	if slot, sess, ok := runningSession(st); ok && e.shouldWaitForRunningWorker(st) {
		reasons := appendReasons(baseReasons,
			fmt.Sprintf("Session %s is running for issue #%d", slot, sess.IssueNumber),
			"Starting another worker is not recommended while a worker is active",
		)
		decision := e.decision(st, now, projectState, ActionWaitForRunningWorker,
			fmt.Sprintf("Worker %s is still running for issue #%d.", slot, sess.IssueNumber),
			RiskSafe, 0.88, &state.SupervisorTarget{Issue: sess.IssueNumber, Session: slot}, PolicyRuleRuntimeState, reasons)
		decision.StuckStates = stuckStates
		return decision, nil
	}

	if !e.cfg.Supervisor.OrderedQueueActive() && !e.cfg.Supervisor.DynamicWave.Active() {
		if slot, sess, ok := e.retryExhaustedSession(st, cache); ok {
			reasons := appendReasons(baseReasons,
				fmt.Sprintf("Session %s for issue #%d is retry_exhausted", slot, sess.IssueNumber),
				"Retry-exhausted work requires a human decision before more automation",
			)
			decision := e.decision(st, now, projectState, ActionReviewRetryExhausted,
				fmt.Sprintf("Issue #%d exhausted its retry budget and needs manual review.", sess.IssueNumber),
				RiskApprovalGated, 0.93, &state.SupervisorTarget{Issue: sess.IssueNumber, PR: sess.PRNumber, Session: slot}, PolicyRuleRuntimeState, reasons)
			decision.StuckStates = stuckStates
			return decision, nil
		}
	}

	if stuck := noOutcomeProgressStuckState(stuckStates); stuck != nil && len(st.ActiveSessions()) == 0 {
		reasons := appendReasons(baseReasons,
			stuck.Summary,
			fmt.Sprintf("Open PRs observed: %d; PR presence does not prove the live outcome is fixed", len(prs)),
			"Supervisor remains read-only; it recommends outcome verification but does not run deploy or runtime commands",
		)
		decision := e.decision(st, now, projectState, ActionCheckOutcomeHealth,
			"Verify outcome health before starting more issue work.",
			RiskSafe, 0.86, nil, PolicyRuleRuntimeState, reasons)
		decision.StuckStates = stuckStates
		return decision, nil
	}

	issues, err := e.reader.ListOpenIssues(nil)
	if err != nil {
		return state.SupervisorDecision{}, fmt.Errorf("list open issues: %w", err)
	}
	projectState.OpenIssues = len(issues)

	if slot, sess, issue, ok, err := e.retryExhaustedRepairCandidate(st, issues, cache); err != nil {
		return state.SupervisorDecision{}, err
	} else if ok {
		reasons := appendReasons(baseReasons,
			fmt.Sprintf("Session %s for issue #%d is retry_exhausted with no usable PR", slot, sess.IssueNumber),
			"Ready-labeled issue is still open and has no dependency/blocking label reason",
			"Retry exhaustion may have moved the project item to Blocked, but supervisor can start one intentional repair worker instead of dead-ending the queue",
			"Starting a repair worker would mutate local worktrees, so supervisor records an explicit repair recommendation",
		)
		decision := e.decision(st, now, projectState, ActionSpawnRepairWorker,
			fmt.Sprintf("Start a repair worker for retry-exhausted issue #%d: %s", issue.Number, issue.Title),
			RiskMutating, 0.88, &state.SupervisorTarget{Issue: issue.Number, Session: slot}, PolicyRuleRuntimeState, reasons)
		decision.StuckStates = stuckStates
		return decision, nil
	}

	policyResult, err := e.policyCandidateIssues(st, issues)
	if err != nil {
		return state.SupervisorDecision{}, err
	}
	if policyResult.dynamicWave {
		return e.decideDynamicWave(st, now, projectState, baseReasons, prs, issues, policyResult, cache)
	}
	candidates := policyResult.candidates
	policySkipped := policyResult.skipped
	policyRule := policyResult.policyRule
	eligible, skipped, err := e.eligibleIssues(st, candidates, true)
	if err != nil {
		return state.SupervisorDecision{}, err
	}
	skipped = append(policySkipped, skipped...)
	stuckStates = e.detectStuckStates(st, now, prs, issues, eligible, skipped, true, cache)
	analysis := supervisorQueueAnalysis(policyRule, len(issues), eligible, skipped)
	withAnalysis := func(decision state.SupervisorDecision) state.SupervisorDecision {
		decision.QueueAnalysis = analysis
		return decision
	}

	if len(eligible) > 0 {
		issue := eligible[0]
		if projectState.AvailableSlots <= 0 {
			reasons := appendReasons(baseReasons,
				fmt.Sprintf("Issue #%d is eligible but no worker slot is available", issue.Number),
			)
			decision := e.decision(st, now, projectState, ActionWaitForCapacity,
				fmt.Sprintf("Issue #%d is eligible, but all worker slots are occupied.", issue.Number),
				RiskSafe, 0.86, &state.SupervisorTarget{Issue: issue.Number}, policyRule, reasons)
			decision.StuckStates = stuckStates
			return withAnalysis(decision), nil
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
			decision := e.decision(st, now, projectState, ActionMonitorOpenPR,
				fmt.Sprintf("Issue #%d already has an open PR; monitor that PR instead of starting work.", issue.Number),
				RiskSafe, 0.87, &state.SupervisorTarget{Issue: issue.Number}, policyRule, reasons)
			decision.StuckStates = stuckStates
			return withAnalysis(decision), nil
		}

		// Race protection: refuse to recommend a spawn for an issue that
		// has already been closed since the listing snapshot (post-merge
		// race) or whose session is already Done/code_landed.
		if cache.isIssueClosed(issue.Number) {
			reasons := appendReasons(baseReasons,
				fmt.Sprintf("Issue #%d closed between listing and dispatch; refusing to spawn a duplicate worker", issue.Number),
			)
			decision := e.decision(st, now, projectState, ActionNone,
				fmt.Sprintf("Issue #%d closed after listing; supervisor will re-evaluate on the next cycle.", issue.Number),
				RiskSafe, 0.9, &state.SupervisorTarget{Issue: issue.Number}, policyRule, reasons)
			decision.StuckStates = stuckStates
			return withAnalysis(decision), nil
		}
		if e.sessionRecentlyDoneForIssue(st, issue.Number) {
			reasons := appendReasons(baseReasons,
				fmt.Sprintf("Issue #%d already has a Done/code-landed session in state; refusing to spawn another worker", issue.Number),
			)
			decision := e.decision(st, now, projectState, ActionNone,
				fmt.Sprintf("Issue #%d already reached Done in state; supervisor refuses to dispatch a duplicate worker.", issue.Number),
				RiskSafe, 0.9, &state.SupervisorTarget{Issue: issue.Number}, policyRule, reasons)
			decision.StuckStates = stuckStates
			return withAnalysis(decision), nil
		}

		reasons := appendReasons(baseReasons,
			issueLabelReason(e.requiredIssueLabels()),
			fmt.Sprintf("Issue #%d is the next eligible issue", issue.Number),
			outcomeIssueReason(outcomeStatus, issue),
			"Starting a worker would mutate local worktrees, so supervisor only records the recommendation",
		)
		if preflight, ok := e.evaluatePreflight(false, true); !ok {
			stuckStates = append(stuckStates, preflightFailedStuckState(preflight, "spawn_worker"))
			blockedReasons := appendReasons(reasons,
				"Preflight gate failed; refusing to recommend a worker spawn",
				"Preflight: "+preflight.Reason,
			)
			decision := e.decision(st, now, projectState, ActionPreflightFailed,
				fmt.Sprintf("Preflight blocked worker spawn for issue #%d: %s", issue.Number, preflight.Reason),
				RiskApprovalGated, 0.9, &state.SupervisorTarget{Issue: issue.Number}, policyRule, blockedReasons)
			decision.StuckStates = stuckStates
			return withAnalysis(decision), nil
		}
		decision := e.decision(st, now, projectState, ActionSpawnWorker,
			fmt.Sprintf("Start a worker for issue #%d: %s", issue.Number, issue.Title),
			RiskMutating, 0.84, &state.SupervisorTarget{Issue: issue.Number}, policyRule, reasons)
		decision.StuckStates = stuckStates
		return withAnalysis(decision), nil
	}

	if policyRule == PolicyRuleOrderedQueue && len(candidates) == 1 {
		issue := candidates[0]
		hasOpenPR, err := e.reader.HasOpenPRForIssue(issue.Number)
		if err != nil {
			return state.SupervisorDecision{}, fmt.Errorf("check open PR for issue #%d: %w", issue.Number, err)
		}
		if hasOpenPR {
			reasons := appendReasons(baseReasons,
				fmt.Sprintf("Issue #%d is the first unfinished ordered issue", issue.Number),
				fmt.Sprintf("Issue #%d already has an open PR", issue.Number),
				"Ordered queue will not label or dispatch later issues while this PR is in review",
			)
			decision := e.decision(st, now, projectState, ActionMonitorOpenPR,
				fmt.Sprintf("Ordered queue is paused at issue #%d because it already has an open PR.", issue.Number),
				RiskSafe, 0.9, &state.SupervisorTarget{Issue: issue.Number}, policyRule, reasons)
			decision.StuckStates = stuckStates
			if analysis.SelectedCandidate == nil {
				analysis.SelectedCandidate = supervisorIssueCandidate(issue)
			}
			return withAnalysis(decision), nil
		}
	}

	candidate, err := e.firstQueueActionCandidate(st, candidates)
	if err != nil {
		return state.SupervisorDecision{}, err
	}
	if candidate != nil {
		if analysis.SelectedCandidate == nil {
			analysis.SelectedCandidate = supervisorIssueCandidate(candidate.issue)
		}
		mutations := candidate.plannedMutations(e.cfg)
		reasons := appendReasons(baseReasons,
			queueLabelReason(candidate.readyLabel, candidate.blockedLabel),
			fmt.Sprintf("Issue #%d is the next queue issue eligible for safe label mutation", candidate.issue.Number),
			outcomeIssueReason(outcomeStatus, candidate.issue),
		)
		risk := RiskMutating
		if len(mutations) > 0 {
			risk = RiskSafe
			reasons = appendReasons(reasons, "Supervisor policy allows the planned safe queue mutation")
		}
		decision := e.decision(st, now, projectState, ActionLabelIssueReady,
			fmt.Sprintf("Prepare issue #%d for the queue by %s.", candidate.issue.Number, plannedMutationPhrase(candidate.neededMutations())),
			risk, 0.82, &state.SupervisorTarget{Issue: candidate.issue.Number}, policyRule, reasons)
		decision.Mutations = mutations
		decision.StuckStates = stuckStates
		return withAnalysis(decision), nil
	}

	if policyRule == PolicyRuleOrderedQueue && len(candidates) == 1 {
		issue := candidates[0]
		if pauseReason := orderedQueuePauseReason(skipped, issue.Number); pauseReason != "" {
			action := ActionWaitForOrderedQueue
			risk := RiskSafe
			confidence := 0.88
			if strings.Contains(pauseReason, "retry limit exhausted") {
				action = ActionReviewRetryExhausted
				risk = RiskApprovalGated
				confidence = 0.93
			}
			reasons := appendReasons(baseReasons,
				fmt.Sprintf("Issue #%d is the first unfinished ordered issue", issue.Number),
				pauseReason,
				"Ordered queue will not advance until this issue is done or explicitly overridden",
			)
			decision := e.decision(st, now, projectState, action,
				fmt.Sprintf("Ordered queue is paused at issue #%d: %s.", issue.Number, pauseReason),
				risk, confidence, &state.SupervisorTarget{Issue: issue.Number}, policyRule, reasons)
			decision.StuckStates = stuckStates
			if analysis.SelectedCandidate == nil {
				analysis.SelectedCandidate = supervisorIssueCandidate(issue)
			}
			return withAnalysis(decision), nil
		}
	}

	reasons := appendReasons(baseReasons,
		fmt.Sprintf("Checked %d open issue(s)", len(issues)),
		"No worker is running, no PR needs attention, and no eligible issue is ready",
	)
	for _, reason := range firstN(skipped, 3) {
		reasons = append(reasons, reason)
	}
	decision := e.decision(st, now, projectState, ActionNone,
		"No action is currently recommended.", RiskSafe, 0.8, nil, policyRule, reasons)
	decision.StuckStates = stuckStates
	return withAnalysis(decision), nil
}

func (e *Engine) decideDynamicWave(st *state.State, now time.Time, projectState state.SupervisorProjectState, baseReasons []string, prs []github.PR, issues []github.Issue, result policyCandidateResult, cache *resolutionCache) (state.SupervisorDecision, error) {
	candidates := result.candidates
	analysis := result.analysis
	outcomeStatus := e.outcomeStatus(st)
	if analysis == nil {
		analysis = &state.SupervisorQueueAnalysis{PolicyRule: PolicyRuleDynamicWave, OpenIssues: len(issues), EligibleCandidates: len(candidates), SkippedReasons: firstN(result.skipped, 5)}
		if len(candidates) > 0 {
			analysis.SelectedCandidate = supervisorIssueCandidate(candidates[0])
		}
	}
	stuckStates := e.detectStuckStates(st, now, prs, issues, candidates, result.skipped, true, cache)
	withAnalysis := func(decision state.SupervisorDecision) state.SupervisorDecision {
		decision.QueueAnalysis = analysis
		decision.StuckStates = stuckStates
		return decision
	}

	if len(candidates) == 0 {
		reasons := appendReasons(baseReasons,
			fmt.Sprintf("Dynamic wave checked %d open issue(s)", len(issues)),
			fmt.Sprintf("Dynamic wave found %d eligible candidate(s), %d excluded issue(s), %d held/meta issue(s), %d blocked-by-dependency issue(s), and %d issue(s) in non-runnable project status", analysis.EligibleCandidates, analysis.ExcludedIssues, analysis.HeldIssues, analysis.BlockedByDependencyIssues, analysis.NonRunnableProjectStatusCount),
		)
		for _, reason := range firstN(result.skipped, 3) {
			reasons = append(reasons, reason)
		}

		// Handoff planner v1: when an open handoff/epic remains but no
		// runnable issue is eligible, the supervisor must not return
		// `none` and silently idle overnight. Promote the queue-
		// exhausted finding to an actionable warning and recommend
		// opening the next concrete child issue (approval-gated).
		if planner := e.handoffPlannerCandidate(issues); planner != nil {
			stuckStates = append(stuckStates, handoffEpicNeedsChildStuckState(planner))
			plannerReasons := appendReasons(reasons,
				fmt.Sprintf("Open handoff/epic #%d (%s) has no runnable child issue", planner.Number, strings.TrimSpace(planner.Title)),
				"Supervisor must own continuation: recommend opening the next child issue instead of idling on `none`",
			)
			if preflight, ok := e.evaluatePreflight(true, false); !ok {
				stuckStates = append(stuckStates, preflightFailedStuckState(preflight, "open_child_issue"))
				blockedReasons := appendReasons(plannerReasons,
					"Preflight gate failed; refusing to recommend opening a new child issue",
					"Preflight: "+preflight.Reason,
				)
				decision := e.decision(st, now, projectState, ActionPreflightFailed,
					fmt.Sprintf("Preflight blocked handoff continuation for epic #%d: %s", planner.Number, preflight.Reason),
					RiskApprovalGated, 0.9, &state.SupervisorTarget{Issue: planner.Number}, PolicyRuleDynamicWave, blockedReasons)
				return withAnalysis(decision), nil
			}
			decision := e.decision(st, now, projectState, ActionOpenChildIssue,
				fmt.Sprintf("Open the next child issue from handoff epic #%d: %s", planner.Number, strings.TrimSpace(planner.Title)),
				RiskApprovalGated, 0.85, &state.SupervisorTarget{Issue: planner.Number}, PolicyRuleDynamicWave, plannerReasons)
			return withAnalysis(decision), nil
		}

		decision := e.decision(st, now, projectState, ActionNone,
			"No issue is currently eligible under the dynamic wave policy.", RiskSafe, 0.8, nil, PolicyRuleDynamicWave, reasons)
		return withAnalysis(decision), nil
	}

	issue := candidates[0]
	if projectState.AvailableSlots <= 0 {
		reasons := appendReasons(baseReasons,
			fmt.Sprintf("Dynamic wave selected issue #%d", issue.Number),
			fmt.Sprintf("Issue #%d is eligible but no worker slot is available", issue.Number),
		)
		decision := e.decision(st, now, projectState, ActionWaitForCapacity,
			fmt.Sprintf("Issue #%d is eligible, but all worker slots are occupied.", issue.Number),
			RiskSafe, 0.86, &state.SupervisorTarget{Issue: issue.Number}, PolicyRuleDynamicWave, reasons)
		return withAnalysis(decision), nil
	}

	hasOpenPR, err := e.reader.HasOpenPRForIssue(issue.Number)
	if err != nil {
		return state.SupervisorDecision{}, fmt.Errorf("check open PR for issue #%d: %w", issue.Number, err)
	}
	if hasOpenPR {
		reasons := appendReasons(baseReasons,
			fmt.Sprintf("Dynamic wave selected issue #%d", issue.Number),
			fmt.Sprintf("Issue #%d already has an open PR", issue.Number),
			"Supervisor mode should not dispatch duplicate work",
		)
		decision := e.decision(st, now, projectState, ActionMonitorOpenPR,
			fmt.Sprintf("Issue #%d already has an open PR; monitor that PR instead of starting work.", issue.Number),
			RiskSafe, 0.87, &state.SupervisorTarget{Issue: issue.Number}, PolicyRuleDynamicWave, reasons)
		return withAnalysis(decision), nil
	}

	queueCandidate := e.dynamicQueueActionCandidate(st, issue, issues)
	if queueCandidate != nil {
		mutations := queueCandidate.plannedMutations(e.cfg)
		reasons := appendReasons(baseReasons,
			queueLabelReason(queueCandidate.readyLabel, ""),
			fmt.Sprintf("Dynamic wave selected issue #%d by priority and issue number", issue.Number),
			outcomeIssueReason(outcomeStatus, issue),
		)
		risk := RiskMutating
		if len(mutations) > 0 {
			risk = RiskSafe
			reasons = appendReasons(reasons, "Supervisor policy allows the planned safe queue mutation")
		}
		decision := e.decision(st, now, projectState, ActionLabelIssueReady,
			fmt.Sprintf("Prepare issue #%d for the dynamic wave by %s.", issue.Number, plannedMutationPhrase(queueCandidate.neededMutations())),
			risk, 0.82, &state.SupervisorTarget{Issue: issue.Number}, PolicyRuleDynamicWave, reasons)
		decision.Mutations = mutations
		return withAnalysis(decision), nil
	}

	if !matchesRequiredLabels(issue, e.requiredIssueLabels()) {
		reasons := appendReasons(baseReasons,
			fmt.Sprintf("Dynamic wave selected issue #%d", issue.Number),
			"Selected issue is waiting for a ready label mutation to appear in GitHub issue data",
		)
		for _, reason := range firstN(result.skipped, 3) {
			reasons = append(reasons, reason)
		}
		decision := e.decision(st, now, projectState, ActionNone,
			"No action is currently recommended while the selected issue waits for its ready label.", RiskSafe, 0.8, &state.SupervisorTarget{Issue: issue.Number}, PolicyRuleDynamicWave, reasons)
		return withAnalysis(decision), nil
	}

	// Race protection: refuse to recommend spawn for an issue that has
	// already been closed since the issue listing snapshot was taken
	// (post-merge close race). Cache hits are cheap because the broader
	// supervisor cycle reuses the same resolutionCache.
	if cache != nil && cache.isIssueClosed(issue.Number) {
		reasons := appendReasons(baseReasons,
			fmt.Sprintf("Dynamic wave selected issue #%d", issue.Number),
			fmt.Sprintf("Issue #%d closed between listing and dispatch; refusing to spawn a duplicate worker", issue.Number),
		)
		decision := e.decision(st, now, projectState, ActionNone,
			fmt.Sprintf("Issue #%d closed after listing; supervisor will re-evaluate on the next cycle.", issue.Number),
			RiskSafe, 0.9, &state.SupervisorTarget{Issue: issue.Number}, PolicyRuleDynamicWave, reasons)
		return withAnalysis(decision), nil
	}
	if e.sessionRecentlyDoneForIssue(st, issue.Number) {
		reasons := appendReasons(baseReasons,
			fmt.Sprintf("Dynamic wave selected issue #%d", issue.Number),
			fmt.Sprintf("Issue #%d already has a Done/code-landed session in state; refusing to spawn another worker", issue.Number),
		)
		decision := e.decision(st, now, projectState, ActionNone,
			fmt.Sprintf("Issue #%d already reached Done in state; supervisor refuses to dispatch a duplicate worker.", issue.Number),
			RiskSafe, 0.9, &state.SupervisorTarget{Issue: issue.Number}, PolicyRuleDynamicWave, reasons)
		return withAnalysis(decision), nil
	}

	reasons := appendReasons(baseReasons,
		issueLabelReason(e.requiredIssueLabels()),
		fmt.Sprintf("Dynamic wave selected issue #%d by priority and issue number", issue.Number),
		outcomeIssueReason(outcomeStatus, issue),
		"Starting a worker would mutate local worktrees, so supervisor only records the recommendation",
	)
	if preflight, ok := e.evaluatePreflight(false, true); !ok {
		stuckStates = append(stuckStates, preflightFailedStuckState(preflight, "spawn_worker"))
		blockedReasons := appendReasons(reasons,
			"Preflight gate failed; refusing to recommend a worker spawn",
			"Preflight: "+preflight.Reason,
		)
		decision := e.decision(st, now, projectState, ActionPreflightFailed,
			fmt.Sprintf("Preflight blocked worker spawn for issue #%d: %s", issue.Number, preflight.Reason),
			RiskApprovalGated, 0.9, &state.SupervisorTarget{Issue: issue.Number}, PolicyRuleDynamicWave, blockedReasons)
		return withAnalysis(decision), nil
	}
	decision := e.decision(st, now, projectState, ActionSpawnWorker,
		fmt.Sprintf("Start a worker for issue #%d: %s", issue.Number, issue.Title),
		RiskMutating, 0.84, &state.SupervisorTarget{Issue: issue.Number}, PolicyRuleDynamicWave, reasons)
	return withAnalysis(decision), nil
}

func (e *Engine) decision(st *state.State, now time.Time, ps state.SupervisorProjectState, action, summary, risk string, confidence float64, target *state.SupervisorTarget, policyRule string, reasons []string) state.SupervisorDecision {
	reasons = appendReasons(reasons, policyRuleReason(policyRule))
	outcomeStatus := e.outcomeStatus(st)
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
		RequiresApproval:  risk == RiskApprovalGated,
		Outcome:           &outcomeStatus,
		ProjectState:      ps,
	}
}

func (e *Engine) detectStuckStates(st *state.State, now time.Time, prs []github.PR, issues, eligible []github.Issue, skipped []string, issuesLoaded bool, cache *resolutionCache) []state.SupervisorStuckState {
	var findings []state.SupervisorStuckState
	findings = append(findings, e.detectWorkerStuckStates(st, now, cache)...)
	findings = append(findings, e.detectPRStuckStates(st, prs, cache)...)
	if issuesLoaded {
		findings = append(findings, e.detectQueueStuckStates(st, prs, issues, eligible, skipped)...)
		findings = append(findings, detectPolicyStuckStates(skipped)...)
	}
	findings = append(findings, e.detectEnvironmentStuckStates(st, eligible)...)
	findings = append(findings, e.detectOutcomeStuckStates(st)...)
	return compactStuckStates(findings)
}

func (e *Engine) outcomeStatus(st *state.State) outcome.Status {
	if e == nil || e.cfg == nil {
		return outcome.StatusFor(outcome.Brief{}, 0, time.Time{})
	}
	mergedPRs := 0
	lastMergeAt := time.Time{}
	if st != nil {
		mergedPRs = st.DonePRCount()
		lastMergeAt = st.LastMergeAt
	}
	if st != nil && st.OutcomeHealth != nil {
		return outcome.StatusFor(e.cfg.Outcome, mergedPRs, lastMergeAt, *st.OutcomeHealth)
	}
	return outcome.StatusFor(e.cfg.Outcome, mergedPRs, lastMergeAt)
}

func outcomeDecisionReason(status outcome.Status) string {
	if !status.Configured {
		return "No outcome brief is configured; issue throughput alone is not enough to prove project progress"
	}
	goal := strings.TrimSpace(status.Goal)
	if goal == "" {
		goal = "configured runtime outcome"
	}
	if strings.TrimSpace(status.RuntimeTarget) != "" {
		return fmt.Sprintf("Outcome: %s; runtime target: %s", goal, status.RuntimeTarget)
	}
	return "Outcome: " + goal
}

func outcomeIssueReason(status outcome.Status, issue github.Issue) string {
	if !status.Configured {
		return "No outcome brief explains why this issue advances a runtime goal"
	}
	goal := strings.TrimSpace(status.Goal)
	if goal == "" {
		goal = "the configured runtime outcome"
	}
	title := strings.TrimSpace(issue.Title)
	if title == "" {
		return fmt.Sprintf("Issue #%d is selected because it is the next policy-eligible step toward %s", issue.Number, goal)
	}
	return fmt.Sprintf("Issue #%d (%s) is selected because it is the next policy-eligible step toward %s", issue.Number, title, goal)
}

func (e *Engine) detectOutcomeStuckStates(st *state.State) []state.SupervisorStuckState {
	status := e.outcomeStatus(st)
	if !status.Configured {
		return []state.SupervisorStuckState{stuckState(state.StuckMissingOutcomeBrief, SeverityWarning,
			"No outcome brief is configured for this project.",
			"Add an outcome brief with the desired runtime goal, target, health signal, and non-goals before treating issue throughput as success.", false, nil,
			"Outcome brief configured: false")}
	}
	switch status.HealthState {
	case outcome.HealthFailing:
		if !status.FailRequiresVisibleWork {
			return nil
		}
		return []state.SupervisorStuckState{e.outcomeProgressStuckState(st, status, false)}
	case outcome.HealthUnknown, outcome.HealthUnmonitored:
		if st == nil || st.DonePRCount() < 2 {
			return nil
		}
		return []state.SupervisorStuckState{e.outcomeProgressStuckState(st, status, false)}
	default:
		return nil
	}
}

func (e *Engine) outcomeProgressStuckState(st *state.State, status outcome.Status, canAct bool) state.SupervisorStuckState {
	goal := strings.TrimSpace(status.Goal)
	if goal == "" {
		goal = "the configured outcome"
	}
	health := status.HealthState
	if health == "" {
		health = outcome.HealthUnknown
	}
	donePRs := 0
	if st != nil {
		donePRs = st.DonePRCount()
	}
	summary := fmt.Sprintf("Runtime outcome health is %s for %s.", health, goal)
	if health != outcome.HealthFailing {
		summary = fmt.Sprintf("Workers have merged PRs, but runtime outcome health is still %s for %s.", health, goal)
	}
	return stuckState(state.StuckNoOutcomeProgress, SeverityBlocked, summary,
		"Run the configured deployment status or healthcheck, then prioritize deploy/runtime fixes before dispatching more issue work.", canAct, nil,
		fmt.Sprintf("done_pr_sessions=%d", donePRs),
		fmt.Sprintf("health_state=%s", health))
}

func noOutcomeProgressStuckState(stuckStates []state.SupervisorStuckState) *state.SupervisorStuckState {
	for i := range stuckStates {
		if stuckStates[i].Code == state.StuckNoOutcomeProgress {
			return &stuckStates[i]
		}
	}
	return nil
}

func (e *Engine) detectWorkerStuckStates(st *state.State, now time.Time, cache *resolutionCache) []state.SupervisorStuckState {
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

		if sess.Status == state.StatusRetryExhausted && sess.PRNumber == 0 && !e.retryExhaustedSessionResolvedOnGitHub(sess, cache) {
			findings = append(findings, stuckState("retry_exhausted", SeverityBlocked,
				fmt.Sprintf("Issue #%d exhausted its retry budget.", sess.IssueNumber),
				"Review the failed attempts, adjust the issue or retry budget, then restart intentionally.", false, target,
				fmt.Sprintf("Session %s status=retry_exhausted retry_count=%d", slot, sess.RetryCount)))
		}

		if sess.PreviousAttemptFeedbackKind == state.RetryReasonReviewFeedback && e.staleReviewFeedbackNeedsAttention(sess) {
			canAct := sess.Status == state.StatusDead && sess.NextRetryAt != nil
			findings = append(findings, stuckState("stale_review_feedback", SeverityBlocked,
				fmt.Sprintf("Issue #%d has review feedback, but no worker is currently fixing it.", sess.IssueNumber),
				"Respawn a worker with the saved review feedback or resolve the feedback manually.", canAct, target,
				fmt.Sprintf("Session %s status=%s previous_feedback_kind=review_feedback", slot, sess.Status)))
		}
	}
	return findings
}

func (e *Engine) staleReviewFeedbackNeedsAttention(sess *state.Session) bool {
	if sess == nil || sess.Status == state.StatusRunning || sess.Status == state.StatusDone {
		return false
	}
	if sess.PRNumber > 0 {
		merged, err := e.reader.IsPRMerged(sess.PRNumber)
		if err == nil && merged {
			return false
		}
	}
	if sess.IssueNumber > 0 {
		closed, err := e.reader.IsIssueClosed(sess.IssueNumber)
		if err == nil && closed {
			return false
		}
	}
	return true
}

func (e *Engine) detectPRStuckStates(st *state.State, prs []github.PR, cache *resolutionCache) []state.SupervisorStuckState {
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
		if e.sessionResolvedOnGitHub(sess, cache) {
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
		if sess.Status == state.StatusRetryExhausted {
			checks := ciStatus
			if checks == "" {
				checks = "unknown"
			}
			severity := SeverityWarning
			recommended := "Refresh the PR status; if checks and review gates pass, the PR remains eligible for normal merge flow."
			switch checks {
			case "success":
				severity = SeverityInfo
				recommended = "No retry is needed if review gates pass; keep the PR in normal merge flow."
			case "pending":
				severity = SeverityInfo
				recommended = "Wait for checks to finish; if they pass and no actionable review feedback remains, merge normally."
			case "failure":
				severity = SeverityBlocked
				recommended = "Fix failing checks or retry intentionally before this PR can merge."
			}
			findings = append(findings, stuckState("retry_exhausted_open_pr", severity,
				fmt.Sprintf("Issue #%d is retry exhausted, but PR #%d is still open; checks=%s.", sess.IssueNumber, pr.Number, checks),
				recommended, true, target,
				fmt.Sprintf("Session %s status=retry_exhausted pr=%d checks=%s", slot, pr.Number, checks)))
		}
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
	excludedCount := countSkipped(skipped, "excluded by configured label") + countSkipped(skipped, "skipped by dynamic wave policy: excluded")
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
		severity := SeverityInfo
		summary := "The ordered issue queue was checked, but every issue was skipped."
		recommended := "Review skipped reasons and make one issue eligible for dispatch."
		canAct := false
		if e.openHandoffEpicPresent(issues) {
			severity = SeverityWarning
			summary = "The ordered/dynamic queue is exhausted but an open handoff epic remains."
			recommended = "Open the next concrete child issue from the handoff epic, or unblock the held one, so the supervisor can dispatch work."
			canAct = true
		}
		findings = append(findings, stuckState("ordered_queue_exhausted", severity,
			summary, recommended, canAct, nil,
			append([]string{fmt.Sprintf("Skipped issues: %d", len(skipped))}, firstEvidence(skipped)...)...))
	}

	return findings
}

// openHandoffEpicPresent reports whether the supervisor sees at least one
// open issue that looks like a handoff epic or carries one of the
// configured handoff source labels. Always false when the planner is
// disabled, so existing projects keep the original info-level behaviour.
func (e *Engine) openHandoffEpicPresent(issues []github.Issue) bool {
	if e == nil || e.cfg == nil || !e.cfg.Supervisor.HandoffPlanner.Active() {
		return false
	}
	labels := e.cfg.Supervisor.HandoffPlanner.EffectiveSourceLabels()
	for _, issue := range issues {
		if isHandoffSource(issue, labels) {
			return true
		}
	}
	return false
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

func decisionRequiresApproval(cfg *config.Config, decision state.SupervisorDecision) bool {
	if decision.RecommendedAction == ActionNone || decision.Risk == RiskSafe {
		return false
	}
	if cfg == nil {
		return true
	}
	// #545: operator-configured approval-gated mutating verbs (merge_pr,
	// close_issue, delete_worktree, change_global_config) live in
	// ApprovalRequired (yaml approval_required), NOT ApprovalRequiredActions.
	// They are canonical verbs and must gate minting of the matching decision;
	// without this loop the cautious gate silently dropped every merge_pr.
	for _, action := range cfg.Supervisor.ApprovalRequired {
		if canonicalAction(action) == decision.RecommendedAction {
			return true
		}
	}
	if cfg.Supervisor.ApprovalRequiredActions == nil {
		return true
	}
	for _, action := range cfg.Supervisor.ApprovalRequiredActions {
		if canonicalAction(action) == decision.RecommendedAction {
			return true
		}
	}
	return false
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

type policyCandidateResult struct {
	candidates  []github.Issue
	skipped     []string
	policyRule  string
	dynamicWave bool
	analysis    *state.SupervisorQueueAnalysis
}

type dynamicSkipCategory string

const (
	dynamicSkipOther         dynamicSkipCategory = "other"
	dynamicSkipExcluded      dynamicSkipCategory = "excluded"
	dynamicSkipHeldMeta      dynamicSkipCategory = "held_meta"
	dynamicSkipBlockedDep    dynamicSkipCategory = "blocked_by_dependency"
	dynamicSkipProjectStatus dynamicSkipCategory = "project_status"
)

func (e *Engine) policyCandidateIssues(st *state.State, issues []github.Issue) (policyCandidateResult, error) {
	if !e.cfg.Supervisor.OrderedQueueActive() {
		if e.cfg.Supervisor.DynamicWave.Active() {
			return e.dynamicWaveCandidateIssues(st, issues, nil)
		}
		return policyCandidateResult{candidates: issues, policyRule: e.defaultPolicyRule()}, nil
	}
	if err := validateOrderedQueueIssues(e.cfg.Supervisor.OrderedQueue.Issues); err != nil {
		return policyCandidateResult{}, err
	}
	issueByNumber := make(map[int]github.Issue, len(issues))
	for _, issue := range issues {
		issueByNumber[issue.Number] = issue
	}
	var skipped []string
	for _, issueNumber := range e.cfg.Supervisor.OrderedQueue.Issues {
		done, reason, err := e.orderedQueueIssueDone(st, issueNumber)
		if err != nil {
			return policyCandidateResult{}, fmt.Errorf("check ordered queue issue #%d: %w", issueNumber, err)
		}
		if done {
			skipped = append(skipped, fmt.Sprintf("Issue #%d skipped by supervisor.ordered_queue: %s", issueNumber, reason))
			continue
		}
		issue, ok := issueByNumber[issueNumber]
		if !ok {
			return policyCandidateResult{skipped: append(skipped, fmt.Sprintf("Issue #%d is first unfinished in supervisor.ordered_queue but was not returned by open issue listing", issueNumber)), policyRule: PolicyRuleOrderedQueue}, nil
		}
		return policyCandidateResult{candidates: []github.Issue{issue}, skipped: skipped, policyRule: PolicyRuleOrderedQueue}, nil
	}
	skipped = append(skipped, "No unfinished issue remains in supervisor.ordered_queue")
	if e.cfg.Supervisor.DynamicWave.Active() {
		return e.dynamicWaveCandidateIssues(st, issues, skipped)
	}
	return policyCandidateResult{skipped: skipped, policyRule: PolicyRuleOrderedQueue}, nil
}

func (e *Engine) dynamicWaveCandidateIssues(st *state.State, issues []github.Issue, prefixSkipped []string) (policyCandidateResult, error) {
	skipped := append([]string(nil), prefixSkipped...)
	analysis := &state.SupervisorQueueAnalysis{
		PolicyRule: PolicyRuleDynamicWave,
		OpenIssues: len(issues),
	}

	candidates := make([]github.Issue, 0, len(issues))
	for _, issue := range issues {
		reason, category, err := e.dynamicWaveSkipReason(st, issue)
		if err != nil {
			return policyCandidateResult{}, err
		}
		if reason != "" {
			switch category {
			case dynamicSkipExcluded:
				analysis.ExcludedIssues++
			case dynamicSkipHeldMeta:
				analysis.HeldIssues++
			case dynamicSkipBlockedDep:
				analysis.BlockedByDependencyIssues++
			case dynamicSkipProjectStatus:
				analysis.NonRunnableProjectStatusCount++
			}
			skipped = append(skipped, fmt.Sprintf("Issue #%d skipped by dynamic wave policy: %s", issue.Number, reason))
			continue
		}
		candidates = append(candidates, issue)
	}

	sortDynamicWaveCandidates(candidates)
	analysis.EligibleCandidates = len(candidates)
	if len(candidates) > 0 {
		analysis.SelectedCandidate = supervisorIssueCandidate(candidates[0])
	}
	analysis.SkippedReasons = firstN(skipped, 5)

	return policyCandidateResult{
		candidates:  candidates,
		skipped:     skipped,
		policyRule:  PolicyRuleDynamicWave,
		dynamicWave: true,
		analysis:    analysis,
	}, nil
}

func (e *Engine) dynamicWaveSkipReason(st *state.State, issue github.Issue) (string, dynamicSkipCategory, error) {
	if st.IssueInProgress(issue.Number) {
		return "already in progress", dynamicSkipOther, nil
	}
	if st.IssueDone(issue.Number) {
		if !e.canTreatIssueDoneForOutcome(st) {
			return "", dynamicSkipOther, nil
		}
		return "already completed in state", dynamicSkipOther, nil
	}
	if st.IssueRetryExhausted(issue.Number) {
		return "retry limit exhausted", dynamicSkipOther, nil
	}
	if e.cfg.MaxRetriesPerIssue > 0 && st.FailedAttemptsForIssue(issue.Number) >= e.cfg.MaxRetriesPerIssue {
		return "retry limit exhausted", dynamicSkipOther, nil
	}
	if st.IsMissionParent(issue.Number) {
		return heldMetaSkipReason("mission parent issue"), dynamicSkipHeldMeta, nil
	}
	if e.cfg.Missions.Enabled && mission.IsMissionIssue(issue, e.cfg.Missions.Labels) && !st.IsMissionChild(issue.Number) {
		return heldMetaSkipReason("mission issue awaits decomposition"), dynamicSkipHeldMeta, nil
	}
	if titleLooksEpic(issue.Title) {
		return heldMetaSkipReason("title indicates epic"), dynamicSkipHeldMeta, nil
	}
	if label, ok := firstMatchingIssueLabel(issue, heldMetaLabels()); ok {
		return heldMetaSkipReason(fmt.Sprintf("label %q", label)), dynamicSkipHeldMeta, nil
	}
	if label, ok := firstMatchingIssueLabel(issue, e.dynamicWaveExcludedLabels()); ok {
		return fmt.Sprintf("excluded by label %q", label), dynamicSkipExcluded, nil
	}
	if status, ok := e.nonRunnableProjectStatus(issue); ok {
		return fmt.Sprintf("project status %q is not runnable", status), dynamicSkipProjectStatus, nil
	}
	if len(e.cfg.BlockerPatterns) > 0 {
		blockers := github.FindBlockers(issue.Body, e.cfg.BlockerPatterns)
		openBlockers, err := e.openBlockers(blockers)
		if err != nil {
			return "", dynamicSkipOther, err
		}
		if len(openBlockers) > 0 {
			return blockedByDependencySkipReason(openBlockers), dynamicSkipBlockedDep, nil
		}
	}
	return "", dynamicSkipOther, nil
}

func (e *Engine) orderedQueueIssueDone(st *state.State, issueNumber int) (bool, string, error) {
	queue := e.cfg.Supervisor.OrderedQueue
	if queue.IsDone(issueNumber) {
		return true, "policy done override", nil
	}

	closed, err := e.reader.IsIssueClosed(issueNumber)
	if err != nil {
		return false, "", fmt.Errorf("check issue closed: %w", err)
	}
	if closed {
		if !e.canTreatIssueDoneForOutcome(st) {
			return false, "issue is closed but outcome health is not verified", nil
		}
		return true, "issue is closed", nil
	}

	for _, slot := range sortedSessionNames(st) {
		sess := st.Sessions[slot]
		if sess == nil || sess.IssueNumber != issueNumber || (sess.Status != state.StatusDone && sess.Status != state.StatusCodeLanded) || sess.PRNumber <= 0 {
			continue
		}
		merged, err := e.reader.IsPRMerged(sess.PRNumber)
		if err != nil {
			return false, "", fmt.Errorf("check PR #%d merged: %w", sess.PRNumber, err)
		}
		if merged {
			reason := fmt.Sprintf("session %s is %s with merged PR #%d", slot, sess.Status, sess.PRNumber)
			if !e.canTreatIssueDoneForOutcome(st) {
				return false, reason + " but outcome health is not verified", nil
			}
			return true, reason, nil
		}
	}

	merged, err := e.reader.HasMergedPRForIssue(issueNumber)
	if err != nil {
		return false, "", fmt.Errorf("check merged PR for issue: %w", err)
	}
	if merged {
		if !e.canTreatIssueDoneForOutcome(st) {
			return false, "linked PR merged but outcome health is not verified", nil
		}
		return true, "linked PR merged", nil
	}

	return false, "", nil
}

func (e *Engine) canTreatIssueDoneForOutcome(st *state.State) bool {
	if e == nil || e.cfg == nil || !e.cfg.Outcome.PassRequiredForDoneEnabled() {
		return true
	}
	return e.outcomeStatus(st).HealthState == outcome.HealthHealthy
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
	issue           github.Issue
	readyLabel      string
	blockedLabel    string
	addReady        bool
	removeReadyFrom []github.Issue
	removeBlocked   bool
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
	for _, issue := range c.removeReadyFrom {
		mutations = append(mutations, state.SupervisorMutation{
			Type:   MutationRemoveReadyLabel,
			Issue:  issue.Number,
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
		if queueMutationAllowed(cfg, mutation) {
			mutations = append(mutations, mutation)
		}
	}
	return mutations
}

func queueMutationAllowed(cfg *config.Config, mutation state.SupervisorMutation) bool {
	if safeActionAllowed(cfg, mutation.Type) {
		return true
	}
	return mutation.Type == MutationRemoveReadyLabel &&
		cfg != nil &&
		cfg.Supervisor.DynamicWave.OwnsReadyLabel &&
		safeActionAllowed(cfg, MutationAddReadyLabel)
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

func (e *Engine) dynamicQueueActionCandidate(st *state.State, selected github.Issue, openIssues []github.Issue) *queueActionCandidate {
	readyLabel := e.readyLabel()
	if readyLabel == "" {
		return nil
	}

	hasReadyLabel := github.HasLabel(selected, []string{readyLabel})
	candidate := queueActionCandidate{
		issue:      selected,
		readyLabel: readyLabel,
		addReady:   !hasReadyLabel && !supervisorMutationSucceeded(st, selected.Number, MutationAddReadyLabel, readyLabel),
	}

	if e.cfg.Supervisor.DynamicWave.OwnsReadyLabel {
		for _, issue := range openIssues {
			if issue.Number == selected.Number || !github.HasLabel(issue, []string{readyLabel}) {
				continue
			}
			if supervisorMutationSucceeded(st, issue.Number, MutationRemoveReadyLabel, readyLabel) {
				continue
			}
			candidate.removeReadyFrom = append(candidate.removeReadyFrom, issue)
		}
		sort.Slice(candidate.removeReadyFrom, func(i, j int) bool {
			return candidate.removeReadyFrom[i].Number < candidate.removeReadyFrom[j].Number
		})
	}

	if !candidate.addReady && len(candidate.removeReadyFrom) == 0 {
		return nil
	}
	return &candidate
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

func (e *Engine) issueRepairSkipReason(st *state.State, issue github.Issue) (string, error) {
	if st.IssueInProgress(issue.Number) {
		return "already in progress", nil
	}
	if st.IssueDone(issue.Number) {
		return "already completed in state", nil
	}
	if st.IsMissionParent(issue.Number) {
		return heldMetaSkipReason("mission parent issue"), nil
	}
	if e.cfg.Missions.Enabled && mission.IsMissionIssue(issue, e.cfg.Missions.Labels) && !st.IsMissionChild(issue.Number) {
		return heldMetaSkipReason("mission issue awaits decomposition"), nil
	}
	policyExcludedLabels := e.policyExcludedLabels()
	if label, ok := firstMatchingIssueLabel(issue, heldMetaLabels()); ok && (hasLabelName(e.excludeLabels(), label) || hasLabelName(policyExcludedLabels, label)) {
		return heldMetaSkipReason(fmt.Sprintf("label %q", label)), nil
	}
	if _, ok := firstMatchingIssueLabel(issue, e.excludeLabels()); ok {
		return "excluded by configured label", nil
	}
	if blockedLabel := strings.TrimSpace(e.cfg.Supervisor.BlockedLabel); blockedLabel != "" && github.HasLabel(issue, []string{blockedLabel}) {
		return "blocked by supervisor policy label", nil
	}
	if _, ok := firstMatchingIssueLabel(issue, policyExcludedLabels); ok {
		return "excluded by supervisor policy label", nil
	}
	if len(e.cfg.BlockerPatterns) > 0 {
		blockers := github.FindBlockers(issue.Body, e.cfg.BlockerPatterns)
		openBlockers, err := e.openBlockers(blockers)
		if err != nil {
			return "", err
		}
		if len(openBlockers) > 0 {
			return blockedByDependencySkipReason(openBlockers), nil
		}
	}
	return "", nil
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
		return heldMetaSkipReason("mission parent issue"), nil
	}
	if e.cfg.Missions.Enabled && mission.IsMissionIssue(issue, e.cfg.Missions.Labels) && !st.IsMissionChild(issue.Number) {
		return heldMetaSkipReason("mission issue awaits decomposition"), nil
	}
	policyExcludedLabels := excludeLabelsExcept(e.policyExcludedLabels(), ignoredBlockedLabel)
	if label, ok := firstMatchingIssueLabel(issue, heldMetaLabels()); ok && (hasLabelName(excludeLabels, label) || hasLabelName(policyExcludedLabels, label)) {
		return heldMetaSkipReason(fmt.Sprintf("label %q", label)), nil
	}
	if _, ok := firstMatchingIssueLabel(issue, excludeLabels); ok {
		return "excluded by configured label", nil
	}
	if blockedLabel := strings.TrimSpace(e.cfg.Supervisor.BlockedLabel); blockedLabel != "" && !strings.EqualFold(blockedLabel, ignoredBlockedLabel) && github.HasLabel(issue, []string{blockedLabel}) {
		return "blocked by supervisor policy label", nil
	}
	if _, ok := firstMatchingIssueLabel(issue, policyExcludedLabels); ok {
		return "excluded by supervisor policy label", nil
	}
	if len(e.cfg.BlockerPatterns) > 0 {
		blockers := github.FindBlockers(issue.Body, e.cfg.BlockerPatterns)
		openBlockers, err := e.openBlockers(blockers)
		if err != nil {
			return "", err
		}
		if len(openBlockers) > 0 {
			return blockedByDependencySkipReason(openBlockers), nil
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

func (e *Engine) sessionWithOpenPR(st *state.State, prs []github.PR, cache *resolutionCache) (string, *state.Session, github.PR, bool) {
	branchToPR := make(map[string]github.PR, len(prs))
	numberToPR := make(map[int]github.PR, len(prs))
	for _, pr := range prs {
		branchToPR[pr.HeadRefName] = pr
		numberToPR[pr.Number] = pr
	}
	for _, slot := range sortedSessionNames(st) {
		sess := st.Sessions[slot]
		if sess == nil || e.sessionResolvedOnGitHub(sess, cache) {
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

func (e *Engine) openPRNeedsRepair(st *state.State, stuckStates []state.SupervisorStuckState, slot string, sess *state.Session, pr github.PR) bool {
	if sess == nil {
		return false
	}
	if e.hasLiveRunningSessionForIssue(st, sess.IssueNumber) {
		return false
	}
	if availableSlots(e.cfg, st) <= 0 {
		return false
	}
	if pr.IsDraft {
		return true
	}
	for _, stuck := range stuckStates {
		if stuck.Target != nil {
			if stuck.Target.Session != "" && stuck.Target.Session != slot {
				continue
			}
			if stuck.Target.PR != 0 && stuck.Target.PR != pr.Number {
				continue
			}
		}
		switch stuck.Code {
		case "failing_checks", "greptile_not_approved", "stale_review_feedback":
			return true
		case "retry_exhausted_open_pr":
			return stuck.Severity == SeverityBlocked
		case state.StuckNoOutcomeProgress:
			return e.outcomeStatus(st).HealthState == outcome.HealthFailing
		}
	}
	return false
}

// openPRReadyToMerge returns (true, reasons[]) when a session's open PR can
// be safely merged: not a draft, mergeable, CI status success, Greptile
// approved (when the reader exposes that signal). The bool is false on
// any blocker; reasons is nil in that case (the caller falls through to
// monitor_open_pr with a different reason set).
//
// Strict: an UNKNOWN mergeable, "pending" CI, or "unknown" review state
// blocks the merge. We never speculate — a missing signal is treated as
// "not ready" so we don't merge a PR whose state we couldn't read.
func (e *Engine) openPRReadyToMerge(slot string, sess *state.Session, pr github.PR) (bool, []string) {
	if sess == nil {
		return false, nil
	}
	if pr.IsDraft {
		return false, nil
	}
	// #543: GitHub LIST endpoint never populates `mergeable`; fetch via
	// single-PR endpoint when reader supports it. Without this every
	// PR.Mergeable read here was "" → "UNKNOWN" → merge_pr never
	// recommended.
	mergeable := strings.ToUpper(strings.TrimSpace(pr.Mergeable))
	if mr, ok := e.reader.(prMergeableReader); ok {
		if fresh, err := mr.PRMergeable(pr.Number); err == nil {
			mergeable = strings.ToUpper(strings.TrimSpace(fresh))
		}
	}
	if mergeable != "MERGEABLE" {
		return false, nil
	}

	// CI must be green. PRCIStatus is an optional reader interface; when
	// the reader does not implement it we conservatively refuse to merge.
	ciReader, ok := e.reader.(prCIStatusReader)
	if !ok {
		return false, nil
	}
	ciStatus, err := ciReader.PRCIStatus(pr.Number)
	if err != nil {
		return false, nil
	}
	if strings.ToLower(strings.TrimSpace(ciStatus)) != "success" {
		return false, nil
	}

	// Greptile gate — when the reader exposes Greptile signal, require
	// approved=true and pending=false. Missing reader -> proceed (some
	// projects don't use Greptile and the cautious gate still requires
	// human approval before merge_pr executes).
	if greptileReader, ok := e.reader.(prGreptileReader); ok {
		approved, pending, err := greptileReader.PRGreptileApproved(pr.Number)
		if err != nil {
			return false, nil
		}
		if pending {
			return false, nil
		}
		if !approved {
			return false, nil
		}
	}

	reasons := []string{
		fmt.Sprintf("PR #%d is not draft", pr.Number),
		fmt.Sprintf("PR #%d mergeable=%s", pr.Number, mergeable),
		fmt.Sprintf("PR #%d CI status=success", pr.Number),
	}
	if _, ok := e.reader.(prGreptileReader); ok {
		reasons = append(reasons, fmt.Sprintf("PR #%d Greptile review approved", pr.Number))
	}
	return true, reasons
}

// monitorOpenPRReasons returns a short list of human-readable reasons
// describing WHY a PR is not yet merge-ready, used in the monitor_open_pr
// summary. Mirrors the gates checked by openPRReadyToMerge but produces
// honest text instead of a single "monitor" word.
func (e *Engine) monitorOpenPRReasons(slot string, sess *state.Session, pr github.PR) []string {
	var reasons []string
	if pr.IsDraft {
		reasons = append(reasons, "PR is still a draft")
	}
	// #543: refetch via single-PR endpoint; LIST endpoint always
	// returns mergeable=null so the diagnostic was always "unknown".
	mergeable := strings.ToUpper(strings.TrimSpace(pr.Mergeable))
	if mr, ok := e.reader.(prMergeableReader); ok {
		if fresh, err := mr.PRMergeable(pr.Number); err == nil {
			mergeable = strings.ToUpper(strings.TrimSpace(fresh))
		}
	}
	if mergeable != "MERGEABLE" {
		if mergeable == "" {
			reasons = append(reasons, "PR mergeable state is unknown")
		} else {
			reasons = append(reasons, fmt.Sprintf("PR mergeable=%s", mergeable))
		}
	}
	if ciReader, ok := e.reader.(prCIStatusReader); ok {
		if status, err := ciReader.PRCIStatus(pr.Number); err == nil {
			lc := strings.ToLower(strings.TrimSpace(status))
			if lc != "success" {
				reasons = append(reasons, fmt.Sprintf("CI status=%s", status))
			}
		} else {
			reasons = append(reasons, "CI status could not be read")
		}
	}
	if greptileReader, ok := e.reader.(prGreptileReader); ok {
		if approved, pending, err := greptileReader.PRGreptileApproved(pr.Number); err == nil {
			if pending {
				reasons = append(reasons, "Greptile review pending")
			} else if !approved {
				reasons = append(reasons, "Greptile review not approved")
			}
		}
	}
	if len(reasons) == 0 {
		reasons = append(reasons, "merge gate not yet evaluated this cycle")
	}
	return reasons
}

// summarizeMonitorReasons joins reasons into a single short phrase for the
// SupervisorDecision.Summary field. Keeps the dashboard readable.
func summarizeMonitorReasons(reasons []string) string {
	if len(reasons) == 0 {
		return "waiting"
	}
	return strings.Join(reasons, "; ")
}

func (e *Engine) hasLiveRunningSessionForIssue(st *state.State, issueNumber int) bool {
	if st == nil || issueNumber <= 0 {
		return false
	}
	for _, sess := range st.Sessions {
		if sess == nil || sess.IssueNumber != issueNumber || sess.Status != state.StatusRunning {
			continue
		}
		if sess.PID == 0 || e.pidAlive(sess.PID) {
			return true
		}
	}
	return false
}

func (e *Engine) retryExhaustedSession(st *state.State, cache *resolutionCache) (string, *state.Session, bool) {
	for _, slot := range sortedSessionNames(st) {
		sess := st.Sessions[slot]
		if sess == nil || sess.Status != state.StatusRetryExhausted {
			continue
		}
		if e.retryExhaustedSessionResolvedOnGitHub(sess, cache) {
			continue
		}
		return slot, sess, true
	}
	return "", nil, false
}

func (e *Engine) retryExhaustedRepairCandidate(st *state.State, issues []github.Issue, cache *resolutionCache) (string, *state.Session, github.Issue, bool, error) {
	if availableSlots(e.cfg, st) <= 0 {
		return "", nil, github.Issue{}, false, nil
	}
	issueByNumber := make(map[int]github.Issue, len(issues))
	for _, issue := range issues {
		issueByNumber[issue.Number] = issue
	}
	for _, slot := range sortedSessionNames(st) {
		sess := st.Sessions[slot]
		if sess == nil || sess.Status != state.StatusRetryExhausted || sess.PRNumber > 0 {
			continue
		}
		if e.hasLiveRunningSessionForIssue(st, sess.IssueNumber) || e.retryExhaustedSessionResolvedOnGitHub(sess, cache) {
			continue
		}
		issue, ok := issueByNumber[sess.IssueNumber]
		if !ok || !matchesRequiredLabels(issue, e.requiredIssueLabels()) {
			continue
		}
		if hasOpenPR, err := e.reader.HasOpenPRForIssue(issue.Number); err != nil {
			return "", nil, github.Issue{}, false, fmt.Errorf("check open PR for issue #%d: %w", issue.Number, err)
		} else if hasOpenPR {
			continue
		}
		if reason, err := e.issueRepairSkipReason(st, issue); err != nil {
			return "", nil, github.Issue{}, false, err
		} else if reason != "" {
			continue
		}
		return slot, sess, issue, true, nil
	}
	return "", nil, github.Issue{}, false, nil
}

// retryExhaustedSessionResolvedOnGitHub returns true when the underlying issue
// is closed or has been resolved by a merged PR, so the supervisor should
// ignore the stale retry-exhausted session record instead of recommending
// review or worker spawning for the already-resolved issue.
func (e *Engine) retryExhaustedSessionResolvedOnGitHub(sess *state.Session, cache *resolutionCache) bool {
	return e.sessionResolvedOnGitHub(sess, cache)
}

func (e *Engine) sessionResolvedOnGitHub(sess *state.Session, cache *resolutionCache) bool {
	if sess == nil {
		return false
	}
	if cache == nil {
		cache = newResolutionCache(e.reader)
	}
	if sess.PRNumber > 0 && cache.isPRMerged(sess.PRNumber) {
		return true
	}
	if sess.IssueNumber <= 0 {
		return false
	}
	if cache.isIssueClosed(sess.IssueNumber) {
		return true
	}
	if cache.hasMergedPRForIssue(sess.IssueNumber) {
		return true
	}
	return false
}

// resolutionCache memoizes the read-only GitHub lookups used to detect that a
// retry-exhausted session has already been resolved upstream. Within a single
// supervisor decision cycle the same issue or PR may be inspected from
// multiple call sites (e.g. detectWorkerStuckStates and retryExhaustedSession);
// the cache ensures each underlying API call is issued at most once.
type resolutionCache struct {
	reader           Reader
	issueClosed      map[int]bool
	mergedPRForIssue map[int]bool
	prMerged         map[int]bool
}

func newResolutionCache(reader Reader) *resolutionCache {
	return &resolutionCache{
		reader:           reader,
		issueClosed:      make(map[int]bool),
		mergedPRForIssue: make(map[int]bool),
		prMerged:         make(map[int]bool),
	}
}

func (c *resolutionCache) isIssueClosed(number int) bool {
	if c == nil || c.reader == nil || number <= 0 {
		return false
	}
	if v, ok := c.issueClosed[number]; ok {
		return v
	}
	closed, err := c.reader.IsIssueClosed(number)
	if err != nil {
		return false
	}
	c.issueClosed[number] = closed
	return closed
}

func (c *resolutionCache) hasMergedPRForIssue(number int) bool {
	if c == nil || c.reader == nil || number <= 0 {
		return false
	}
	if v, ok := c.mergedPRForIssue[number]; ok {
		return v
	}
	merged, err := c.reader.HasMergedPRForIssue(number)
	if err != nil {
		return false
	}
	c.mergedPRForIssue[number] = merged
	return merged
}

func (c *resolutionCache) isPRMerged(prNumber int) bool {
	if c == nil || c.reader == nil || prNumber <= 0 {
		return false
	}
	if v, ok := c.prMerged[prNumber]; ok {
		return v
	}
	merged, err := c.reader.IsPRMerged(prNumber)
	if err != nil {
		return false
	}
	c.prMerged[prNumber] = merged
	return merged
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

func (e *Engine) dynamicWaveExcludedLabels() []string {
	labels := []string{"blocked", "wontfix", "question", "duplicate", "invalid"}
	labels = append(labels, e.cfg.ExcludeLabels...)
	labels = append(labels, e.policyExcludedLabels()...)
	if blockedLabel := strings.TrimSpace(e.cfg.Supervisor.BlockedLabel); blockedLabel != "" {
		labels = append(labels, blockedLabel)
	}
	return uniqueLabelNames(labels)
}

func heldMetaLabels() []string {
	return []string{"epic", "meta"}
}

func heldMetaSkipReason(reason string) string {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return "held/meta"
	}
	return "held/meta: " + reason
}

func blockedByDependencySkipReason(openBlockers []int) string {
	return fmt.Sprintf("blocked by dependency: open issue(s) %s", issueRefs(openBlockers))
}

func uniqueLabelNames(labels []string) []string {
	unique := make([]string, 0, len(labels))
	seen := make(map[string]struct{}, len(labels))
	for _, label := range labels {
		label = strings.TrimSpace(label)
		if label == "" {
			continue
		}
		key := strings.ToLower(label)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		unique = append(unique, label)
	}
	return unique
}

func firstMatchingIssueLabel(issue github.Issue, labels []string) (string, bool) {
	for _, issueLabel := range issue.Labels {
		for _, excluded := range labels {
			if strings.EqualFold(strings.TrimSpace(issueLabel.Name), strings.TrimSpace(excluded)) {
				return issueLabel.Name, true
			}
		}
	}
	return "", false
}

func titleLooksEpic(title string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(title)), "epic:")
}

// handoffPlannerCandidate returns the open issue that should be treated as
// the parent handoff epic for the next supervisor-owned child issue. v1
// picks the lowest-numbered open issue that is either title-prefixed
// "epic:" or labeled with one of the configured source labels. Returns nil
// when the planner is disabled or no candidate exists.
func (e *Engine) handoffPlannerCandidate(issues []github.Issue) *github.Issue {
	if e == nil || e.cfg == nil || !e.cfg.Supervisor.HandoffPlanner.Active() {
		return nil
	}
	labels := e.cfg.Supervisor.HandoffPlanner.EffectiveSourceLabels()
	openChildren := e.cfg.Supervisor.HandoffPlanner.MaxOpenChildren
	if openChildren > 0 {
		nonHandoff := 0
		for _, issue := range issues {
			if !isHandoffSource(issue, labels) {
				nonHandoff++
			}
		}
		if nonHandoff >= openChildren {
			return nil
		}
	}

	sorted := append([]github.Issue(nil), issues...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Number < sorted[j].Number })
	for i := range sorted {
		issue := sorted[i]
		if isHandoffSource(issue, labels) {
			return &issue
		}
	}
	return nil
}

// isHandoffSource reports whether the issue is a handoff/epic parent under
// the configured source labels OR by an `epic:` title prefix.
func isHandoffSource(issue github.Issue, labels []string) bool {
	if titleLooksEpic(issue.Title) {
		return true
	}
	if _, ok := firstMatchingIssueLabel(issue, labels); ok {
		return true
	}
	return false
}

// evaluatePreflight runs the configured preflight command, if any, and
// returns the result. requireForCreate/requireForSpawn control which
// per-context gate applies. When no command is configured (or the relevant
// require_* flag is false), this returns Ok=true with no work.
func (e *Engine) evaluatePreflight(requireForCreate, requireForSpawn bool) (PreflightResult, bool) {
	if e == nil || e.cfg == nil {
		return PreflightResult{Ok: true}, true
	}
	handoff := e.cfg.Supervisor.HandoffPlanner
	// Pick the most specific preflight command available: planner-scoped
	// first (when the caller is the planner), then the supervisor-wide
	// fallback.
	cmd := ""
	if requireForCreate && strings.TrimSpace(handoff.PreflightCommand) != "" && handoff.RequirePreflightBeforeCreate {
		cmd = handoff.PreflightCommand
	} else if requireForSpawn && strings.TrimSpace(handoff.PreflightCommand) != "" && handoff.RequirePreflightBeforeSpawn {
		cmd = handoff.PreflightCommand
	}
	if cmd == "" && strings.TrimSpace(e.cfg.Supervisor.PreflightCommand) != "" {
		// Top-level preflight always applies to both gates.
		cmd = e.cfg.Supervisor.PreflightCommand
	}
	if cmd == "" {
		return PreflightResult{Ok: true}, true
	}
	runner := e.preflight
	if runner == nil {
		runner = defaultPreflightRunner
	}
	res := runner(cmd)
	return res, res.Ok
}

func handoffEpicNeedsChildStuckState(epic *github.Issue) state.SupervisorStuckState {
	target := &state.SupervisorTarget{}
	if epic != nil {
		target.Issue = epic.Number
	}
	summary := "An open handoff/epic remains but no runnable child issue is eligible."
	if epic != nil {
		summary = fmt.Sprintf("Open handoff/epic #%d (%s) has no runnable child issue.", epic.Number, strings.TrimSpace(epic.Title))
	}
	return stuckState(state.StuckHandoffEpicNeedsChild, SeverityWarning, summary,
		"Open the next concrete child issue from the handoff epic so the supervisor can dispatch work.", true, target,
		"queue_eligible=0",
		"handoff_epic_open=true")
}

func preflightFailedStuckState(result PreflightResult, action string) state.SupervisorStuckState {
	return stuckState(state.StuckPreflightFailed, SeverityBlocked,
		fmt.Sprintf("Preflight gate failed; refusing to recommend %s.", action),
		"Fix the failing preflight check (toolchain, auth, asset checksum, runtime reachability) before the supervisor can dispatch.", false, nil,
		"preflight_ok=false",
		fmt.Sprintf("preflight_reason=%s", strings.TrimSpace(result.Reason)),
		fmt.Sprintf("preflight_exit=%d", result.ExitCode))
}

// sessionRecentlyDoneForIssue reports whether the in-memory state already
// records a Done or code_landed session for the given issue. Used as a race
// guard between marking an issue Done and the dispatch loop picking the same
// issue up again before the GitHub closure is observed.
func (e *Engine) sessionRecentlyDoneForIssue(st *state.State, issueNumber int) bool {
	if st == nil || issueNumber <= 0 {
		return false
	}
	for _, sess := range st.Sessions {
		if sess == nil || sess.IssueNumber != issueNumber {
			continue
		}
		if sess.Status == state.StatusDone || sess.Status == state.StatusCodeLanded {
			return true
		}
	}
	return false
}

func (e *Engine) nonRunnableProjectStatus(issue github.Issue) (string, bool) {
	for _, item := range issue.ProjectItems {
		if item.Status == nil {
			continue
		}
		status := strings.TrimSpace(item.Status.Name)
		if status == "" || projectStatusIsRunnable(status, e.runnableProjectStatuses()) {
			continue
		}
		return status, true
	}
	return "", false
}

func (e *Engine) runnableProjectStatuses() []string {
	if e == nil || e.cfg == nil {
		return defaultRunnableProjectStatuses()
	}
	configured := trimNonEmpty(e.cfg.Supervisor.DynamicWave.RunnableProjectStatuses)
	if len(configured) > 0 {
		return configured
	}
	return defaultRunnableProjectStatuses()
}

func defaultRunnableProjectStatuses() []string {
	return []string{"Todo", "To Do", "Ready", "Backlog", "New"}
}

func projectStatusIsRunnable(status string, runnable []string) bool {
	normalized := normalizeProjectStatus(status)
	for _, candidate := range runnable {
		if normalizeProjectStatus(candidate) == normalized {
			return true
		}
	}
	return false
}

func normalizeProjectStatus(status string) string {
	fields := strings.Fields(strings.ToLower(strings.TrimSpace(status)))
	return strings.Join(fields, " ")
}

func trimNonEmpty(values []string) []string {
	trimmed := make([]string, 0, len(values))
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			trimmed = append(trimmed, value)
		}
	}
	return trimmed
}

func firstProjectStatus(issue github.Issue) string {
	for _, item := range issue.ProjectItems {
		if item.Status == nil {
			continue
		}
		if status := strings.TrimSpace(item.Status.Name); status != "" {
			return status
		}
	}
	return ""
}

func sortDynamicWaveCandidates(issues []github.Issue) {
	sort.SliceStable(issues, func(i, j int) bool {
		leftPriority, _ := issuePriority(issues[i])
		rightPriority, _ := issuePriority(issues[j])
		if leftPriority != rightPriority {
			return leftPriority < rightPriority
		}
		return issues[i].Number < issues[j].Number
	})
}

func issuePriority(issue github.Issue) (int, string) {
	bestRank := 4
	bestLabel := ""
	for _, label := range issue.Labels {
		switch strings.ToLower(strings.TrimSpace(label.Name)) {
		case "p0":
			return 0, label.Name
		case "p1":
			if bestRank > 1 {
				bestRank = 1
				bestLabel = label.Name
			}
		case "p2":
			if bestRank > 2 {
				bestRank = 2
				bestLabel = label.Name
			}
		case "p3":
			if bestRank > 3 {
				bestRank = 3
				bestLabel = label.Name
			}
		}
	}
	return bestRank, bestLabel
}

func supervisorIssueCandidate(issue github.Issue) *state.SupervisorIssueCandidate {
	_, priorityLabel := issuePriority(issue)
	return &state.SupervisorIssueCandidate{
		Number:        issue.Number,
		Title:         RedactSensitive(issue.Title),
		Labels:        issueLabelNames(issue),
		PriorityLabel: priorityLabel,
		ProjectStatus: firstProjectStatus(issue),
	}
}

func supervisorQueueAnalysis(policyRule string, openIssues int, eligible []github.Issue, skipped []string) *state.SupervisorQueueAnalysis {
	analysis := &state.SupervisorQueueAnalysis{
		PolicyRule:                    policyRule,
		OpenIssues:                    openIssues,
		EligibleCandidates:            len(eligible),
		ExcludedIssues:                countQueueExcludedReasons(skipped),
		HeldIssues:                    countQueueHeldReasons(skipped),
		BlockedByDependencyIssues:     countQueueBlockedByDependencyReasons(skipped),
		NonRunnableProjectStatusCount: countQueueNonRunnableReasons(skipped),
		SkippedReasons:                firstN(skipped, 5),
	}
	if len(eligible) > 0 {
		analysis.SelectedCandidate = supervisorIssueCandidate(eligible[0])
	}
	return analysis
}

func countQueueExcludedReasons(skipped []string) int {
	count := 0
	for _, reason := range skipped {
		lower := strings.ToLower(reason)
		if strings.Contains(lower, "excluded by configured label") ||
			strings.Contains(lower, "excluded by supervisor policy label") ||
			strings.Contains(lower, "blocked by supervisor policy label") ||
			strings.Contains(lower, "skipped by dynamic wave policy: excluded") {
			count++
		}
	}
	return count
}

func countQueueHeldReasons(skipped []string) int {
	count := 0
	for _, reason := range skipped {
		lower := strings.ToLower(reason)
		if strings.Contains(lower, "held/meta") ||
			strings.Contains(lower, "mission parent issue") ||
			strings.Contains(lower, "mission issue awaits decomposition") ||
			strings.Contains(lower, "title indicates epic") {
			count++
		}
	}
	return count
}

func countQueueBlockedByDependencyReasons(skipped []string) int {
	count := 0
	for _, reason := range skipped {
		lower := strings.ToLower(reason)
		if strings.Contains(lower, "blocked by dependency") || strings.Contains(lower, "blocked by open issue") {
			count++
		}
	}
	return count
}

func countQueueNonRunnableReasons(skipped []string) int {
	count := 0
	for _, reason := range skipped {
		lower := strings.ToLower(reason)
		if strings.Contains(lower, "project status") && strings.Contains(lower, "not runnable") {
			count++
		}
	}
	return count
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
	if e.cfg.Supervisor.DynamicWave.Active() {
		parts = append(parts, "dynamic_wave=true")
		if e.cfg.Supervisor.DynamicWave.OwnsReadyLabel {
			parts = append(parts, "owns_ready_label=true")
		}
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
	case MutationRemoveReadyLabel:
		if mutation.Issue > 0 {
			return fmt.Sprintf("removing stale `%s` from issue #%d", mutation.Label, mutation.Issue)
		}
		return fmt.Sprintf("removing stale `%s`", mutation.Label)
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
	case MutationRemoveReadyLabel:
		return mutator.RemoveIssueLabel(mutation.Issue, mutation.Label)
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
	case MutationRemoveReadyLabel:
		return fmt.Sprintf("removed stale `%s` from issue #%d", mutation.Label, mutation.Issue)
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
		strings.Contains(reason, "skipped by dynamic wave policy") ||
		strings.Contains(reason, "held/meta") ||
		strings.Contains(reason, "mission parent issue") ||
		strings.Contains(reason, "mission issue awaits decomposition") ||
		strings.Contains(reason, "blocked by dependency") ||
		strings.Contains(reason, "blocked by open issue")
}

func orderedQueuePauseReason(skipped []string, issueNumber int) string {
	prefix := fmt.Sprintf("Issue #%d skipped: ", issueNumber)
	for _, reason := range skipped {
		if strings.HasPrefix(reason, prefix) {
			pauseReason := strings.TrimSpace(strings.TrimPrefix(reason, prefix))
			if strings.Contains(pauseReason, "missing configured ready label") {
				return ""
			}
			return pauseReason
		}
	}
	return ""
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

// executeApprovedApprovals runs any approvals currently in status=approved
// through the approver.Executor and persists the resulting state
// transitions. Failures are logged but do not abort the supervisor cycle —
// each approval gets its own audit entry so an operator can see what
// happened.
func executeApprovedApprovals(cfg *config.Config, st *state.State, reader Reader) {
	approvals := st.ListApprovedApprovals()
	if len(approvals) == 0 {
		return
	}
	// approver.Executor only needs MergePR/CloseIssue from the GH client.
	// Reader (the broader supervisor surface) is satisfied by *github.Client,
	// which also satisfies approver.GitHubClient. We assert the narrower
	// interface — when it fails (mocks/tests/Mutator-only), we still try to
	// fall back to a fresh github.New(cfg.Repo).
	var gh approver.GitHubClient
	if candidate, ok := reader.(approver.GitHubClient); ok {
		gh = candidate
	} else {
		gh = github.New(cfg.Repo)
	}
	ex := &approver.Executor{
		GH:        gh,
		Worktrees: approver.WorktreeRemoverFunc(worker.RemoveWorktree),
		Cfg:       cfg,
		Sessions:  approver.SessionLookupFunc(st.SessionAt),
	}
	for _, a := range approvals {
		res := ex.Execute(a)
		now := time.Now().UTC()
		switch res.Status {
		case state.ApprovalStatusExecuted:
			if _, err := st.MarkApprovalExecuted(a.ID, now, "supervisor", res.Summary); err != nil {
				fmt.Fprintf(os.Stderr, "[supervisor] mark approval %s executed: %v\n", a.ID, err)
			}
		case state.ApprovalStatusExecutionSkipped:
			if _, err := st.MarkApprovalExecutionSkipped(a.ID, now, "supervisor", res.Summary); err != nil {
				fmt.Fprintf(os.Stderr, "[supervisor] mark approval %s skipped: %v\n", a.ID, err)
			}
		case state.ApprovalStatusAwaitingDispatch:
			// #515 follow-up: spawn_worker / open_child_issue executor
			// returns AwaitingDispatch so dedup keeps treating the
			// approval as effective until the dispatcher loop resolves
			// it. Without this case the default branch flipped these
			// to execution_failed and the next supervisor cycle minted
			// a duplicate pending — exact zombie loop seen on dogfood
			// 2026-05-31 17:45–17:55 UTC for issue #487.
			if _, err := st.MarkApprovalAwaitingDispatch(a.ID, now, "supervisor", res.Summary); err != nil {
				fmt.Fprintf(os.Stderr, "[supervisor] mark approval %s awaiting_dispatch: %v\n", a.ID, err)
			}
		default:
			msg := res.Summary
			if msg == "" && res.Err != nil {
				msg = res.Err.Error()
			}
			if _, err := st.MarkApprovalExecutionFailed(a.ID, now, "supervisor", msg); err != nil {
				fmt.Fprintf(os.Stderr, "[supervisor] mark approval %s failed: %v\n", a.ID, err)
			}
		}
	}
}
