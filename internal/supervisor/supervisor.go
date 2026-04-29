package supervisor

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/github"
	"github.com/befeast/maestro/internal/mission"
	"github.com/befeast/maestro/internal/state"
)

const (
	ModeReadOnly = "read_only"

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
)

// Reader is the read-only GitHub surface used by the supervisor engine.
type Reader interface {
	ListOpenIssues(labels []string) ([]github.Issue, error)
	ListOpenPRs() ([]github.PR, error)
	HasOpenPRForIssue(issueNumber int) (bool, error)
	IsIssueClosed(number int) (bool, error)
}

// Engine makes deterministic read-only supervisor decisions.
type Engine struct {
	cfg    *config.Config
	reader Reader
	now    func() time.Time
}

func NewEngine(cfg *config.Config, reader Reader) *Engine {
	if reader == nil {
		reader = github.New(cfg.Repo)
	}
	return &Engine{
		cfg:    cfg,
		reader: reader,
		now:    func() time.Time { return time.Now().UTC() },
	}
}

// RunOnce records one read-only supervisor decision in Maestro state.
func RunOnce(cfg *config.Config, reader Reader) (state.SupervisorDecision, error) {
	st, err := state.Load(cfg.StateDir)
	if err != nil {
		return state.SupervisorDecision{}, fmt.Errorf("load state: %w", err)
	}

	decision, err := NewEngine(cfg, reader).Decide(st)
	if err != nil {
		return state.SupervisorDecision{}, err
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
	}

	prs, err := e.reader.ListOpenPRs()
	if err != nil {
		return state.SupervisorDecision{}, fmt.Errorf("list open PRs: %w", err)
	}
	projectState.OpenPRs = len(prs)

	if slot, sess, pr, ok := sessionWithOpenPR(st, prs); ok {
		reasons := appendReasons(baseReasons,
			fmt.Sprintf("Session %s is associated with open PR #%d", slot, pr.Number),
			"No GitHub mutation is needed for supervisor mode",
		)
		return e.decision(now, projectState, ActionMonitorOpenPR,
			fmt.Sprintf("Session %s already has open PR #%d; monitor review, CI, or merge readiness.", slot, pr.Number),
			RiskSafe, 0.9, &state.SupervisorTarget{Issue: sess.IssueNumber, PR: pr.Number, Session: slot}, reasons), nil
	}

	if slot, sess, ok := runningSession(st); ok {
		reasons := appendReasons(baseReasons,
			fmt.Sprintf("Session %s is running for issue #%d", slot, sess.IssueNumber),
			"Starting another worker is not recommended while a worker is active",
		)
		return e.decision(now, projectState, ActionWaitForRunningWorker,
			fmt.Sprintf("Worker %s is still running for issue #%d.", slot, sess.IssueNumber),
			RiskSafe, 0.88, &state.SupervisorTarget{Issue: sess.IssueNumber, Session: slot}, reasons), nil
	}

	if slot, sess, ok := retryExhaustedSession(st); ok {
		reasons := appendReasons(baseReasons,
			fmt.Sprintf("Session %s for issue #%d is retry_exhausted", slot, sess.IssueNumber),
			"Retry-exhausted work requires a human decision before more automation",
		)
		return e.decision(now, projectState, ActionReviewRetryExhausted,
			fmt.Sprintf("Issue #%d exhausted its retry budget and needs manual review.", sess.IssueNumber),
			RiskApprovalGated, 0.93, &state.SupervisorTarget{Issue: sess.IssueNumber, PR: sess.PRNumber, Session: slot}, reasons), nil
	}

	issues, err := e.reader.ListOpenIssues(nil)
	if err != nil {
		return state.SupervisorDecision{}, fmt.Errorf("list open issues: %w", err)
	}
	projectState.OpenIssues = len(issues)

	eligible, skipped, err := e.eligibleIssues(st, issues, true)
	if err != nil {
		return state.SupervisorDecision{}, err
	}

	if len(eligible) > 0 {
		issue := eligible[0]
		if projectState.AvailableSlots <= 0 {
			reasons := appendReasons(baseReasons,
				fmt.Sprintf("Issue #%d is eligible but no worker slot is available", issue.Number),
			)
			return e.decision(now, projectState, ActionWaitForCapacity,
				fmt.Sprintf("Issue #%d is eligible, but all worker slots are occupied.", issue.Number),
				RiskSafe, 0.86, &state.SupervisorTarget{Issue: issue.Number}, reasons), nil
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
			return e.decision(now, projectState, ActionMonitorOpenPR,
				fmt.Sprintf("Issue #%d already has an open PR; monitor that PR instead of starting work.", issue.Number),
				RiskSafe, 0.87, &state.SupervisorTarget{Issue: issue.Number}, reasons), nil
		}

		reasons := appendReasons(baseReasons,
			issueLabelReason(e.cfg.IssueLabels),
			fmt.Sprintf("Issue #%d is the next eligible issue", issue.Number),
			"Starting a worker would mutate local worktrees, so supervisor only records the recommendation",
		)
		return e.decision(now, projectState, ActionSpawnWorker,
			fmt.Sprintf("Start a worker for issue #%d: %s", issue.Number, issue.Title),
			RiskMutating, 0.84, &state.SupervisorTarget{Issue: issue.Number}, reasons), nil
	}

	if len(e.cfg.IssueLabels) > 0 {
		unlabeled, err := e.firstIssueMissingRequiredLabel(st, issues)
		if err != nil {
			return state.SupervisorDecision{}, err
		}
		if unlabeled != nil {
			reasons := appendReasons(baseReasons,
				issueLabelReason(e.cfg.IssueLabels),
				fmt.Sprintf("Issue #%d is open but does not have a configured ready label", unlabeled.Number),
			)
			return e.decision(now, projectState, ActionLabelIssueReady,
				fmt.Sprintf("No eligible issues because none have the configured ready label; issue #%d is next in the open queue.", unlabeled.Number),
				RiskMutating, 0.82, &state.SupervisorTarget{Issue: unlabeled.Number}, reasons), nil
		}
	}

	reasons := appendReasons(baseReasons,
		fmt.Sprintf("Checked %d open issue(s)", len(issues)),
		"No worker is running, no PR needs attention, and no eligible issue is ready",
	)
	for _, reason := range firstN(skipped, 3) {
		reasons = append(reasons, reason)
	}
	return e.decision(now, projectState, ActionNone,
		"No action is currently recommended.", RiskSafe, 0.8, nil, reasons), nil
}

func (e *Engine) decision(now time.Time, ps state.SupervisorProjectState, action, summary, risk string, confidence float64, target *state.SupervisorTarget, reasons []string) state.SupervisorDecision {
	return state.SupervisorDecision{
		ID:                "sup-" + now.Format("20060102T150405.000000000Z"),
		CreatedAt:         now,
		Project:           e.cfg.Repo,
		Mode:              ModeReadOnly,
		Summary:           summary,
		RecommendedAction: action,
		Target:            target,
		Risk:              risk,
		Confidence:        confidence,
		Reasons:           compactReasons(reasons),
		ProjectState:      ps,
	}
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

func (e *Engine) eligibleIssues(st *state.State, issues []github.Issue, requireLabels bool) ([]github.Issue, []string, error) {
	var eligible []github.Issue
	var skipped []string
	for _, issue := range issues {
		if requireLabels && !matchesRequiredLabels(issue, e.cfg.IssueLabels) {
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

func (e *Engine) firstIssueMissingRequiredLabel(st *state.State, issues []github.Issue) (*github.Issue, error) {
	for i := range issues {
		issue := &issues[i]
		if matchesRequiredLabels(*issue, e.cfg.IssueLabels) {
			continue
		}
		reason, err := e.issueSkipReason(st, *issue)
		if err != nil {
			return nil, err
		}
		if reason == "" {
			return issue, nil
		}
	}
	return nil, nil
}

func (e *Engine) issueSkipReason(st *state.State, issue github.Issue) (string, error) {
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
	if github.HasLabel(issue, e.cfg.ExcludeLabels) {
		return "excluded by configured label", nil
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
			if sess.Status == state.StatusPROpen {
				return slot, sess, github.PR{Number: sess.PRNumber, HeadRefName: sess.Branch, State: "OPEN", Title: sess.IssueTitle}, true
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

func issueLabelReason(labels []string) string {
	if len(labels) == 0 {
		return "Config has no issue label filter"
	}
	return "Config requires one of issue_labels: " + strings.Join(labels, ", ")
}

func issueRefs(numbers []int) string {
	refs := make([]string, len(numbers))
	for i, n := range numbers {
		refs[i] = fmt.Sprintf("#%d", n)
	}
	return strings.Join(refs, ", ")
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
