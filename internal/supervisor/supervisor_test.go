package supervisor

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/github"
	"github.com/befeast/maestro/internal/outcome"
	"github.com/befeast/maestro/internal/state"
)

type fakeReader struct {
	issues               []github.Issue
	prs                  []github.PR
	openPRIssues         map[int]bool
	mergedPRIssues       map[int]bool
	closedIssues         map[int]bool
	mergedPRs            map[int]bool
	ciStatuses           map[int]string
	greptileOK           map[int]bool
	greptilePend         map[int]bool
	reviewVerdicts       map[int]github.ReviewGateVerdict
	mergeStates          map[int]string
	highSeverityHeadSHA  map[int]string
	highSeverityFindings map[int][]github.ReviewComment
	rateLimit            *github.RateLimitStatus
	rateLimitErr         error
	rateLimitCalls       int
	issueCalls           int
	closedIssueCalls     map[int]int
	mergedPRIssueCalls   map[int]int
	mergedPRCalls        map[int]int
	addedLabels          []string
	removedLabels        []string
	comments             []string
	addLabelErr          error
	removeLabelErr       error
	commentErr           error
}

type fakeLLM struct {
	output string
	prompt string
	calls  int
	err    error
}

func (f *fakeLLM) Complete(prompt string) (string, error) {
	f.calls++
	f.prompt = prompt
	if f.err != nil {
		return "", f.err
	}
	return f.output, nil
}

func (f *fakeReader) ListOpenIssues(labels []string) ([]github.Issue, error) {
	f.issueCalls++
	return f.issues, nil
}

func (f *fakeReader) ListOpenPRs() ([]github.PR, error) {
	return f.prs, nil
}

func (f *fakeReader) HasOpenPRForIssue(issueNumber int) (bool, error) {
	return f.openPRIssues[issueNumber], nil
}

func (f *fakeReader) HasMergedPRForIssue(issueNumber int) (bool, error) {
	if f.mergedPRIssueCalls == nil {
		f.mergedPRIssueCalls = map[int]int{}
	}
	f.mergedPRIssueCalls[issueNumber]++
	return f.mergedPRIssues[issueNumber], nil
}

func (f *fakeReader) IsIssueClosed(number int) (bool, error) {
	if f.closedIssueCalls == nil {
		f.closedIssueCalls = map[int]int{}
	}
	f.closedIssueCalls[number]++
	return f.closedIssues[number], nil
}

func (f *fakeReader) IsPRMerged(prNumber int) (bool, error) {
	if f.mergedPRCalls == nil {
		f.mergedPRCalls = map[int]int{}
	}
	f.mergedPRCalls[prNumber]++
	return f.mergedPRs[prNumber], nil
}

func (f *fakeReader) AddIssueLabel(issueNumber int, label string) error {
	if f.addLabelErr != nil {
		return f.addLabelErr
	}
	f.addedLabels = append(f.addedLabels, fmt.Sprintf("#%d:%s", issueNumber, label))
	return nil
}

func (f *fakeReader) RemoveIssueLabel(issueNumber int, label string) error {
	if f.removeLabelErr != nil {
		return f.removeLabelErr
	}
	f.removedLabels = append(f.removedLabels, fmt.Sprintf("#%d:%s", issueNumber, label))
	return nil
}

func (f *fakeReader) CommentIssue(issueNumber int, body string) error {
	if f.commentErr != nil {
		return f.commentErr
	}
	f.comments = append(f.comments, fmt.Sprintf("#%d:%s", issueNumber, body))
	return nil
}

func (f *fakeReader) PRCIStatus(prNumber int) (string, error) {
	return f.ciStatuses[prNumber], nil
}

func (f *fakeReader) PRMergeStatus(prNumber int) (string, string, error) {
	mergeable := ""
	for _, pr := range f.prs {
		if pr.Number == prNumber {
			mergeable = pr.Mergeable
			break
		}
	}
	return mergeable, f.mergeStates[prNumber], nil
}

func (f *fakeReader) PRGreptileApproved(prNumber int) (bool, bool, error) {
	if f.greptilePend[prNumber] {
		return false, true, nil
	}
	approved, ok := f.greptileOK[prNumber]
	if !ok {
		return true, false, nil
	}
	return approved, false, nil
}

func (f *fakeReader) PRReviewGateVerdict(prNumber int, streams []string) (github.ReviewGateVerdict, error) {
	if f.reviewVerdicts != nil {
		if verdict, ok := f.reviewVerdicts[prNumber]; ok {
			return verdict, nil
		}
	}
	approved, pending, err := f.PRGreptileApproved(prNumber)
	if err != nil {
		return github.ReviewGateVerdict{}, err
	}
	return github.ReviewGateVerdict{
		Passed:  approved && !pending,
		Pending: pending,
		Streams: []github.ReviewStreamVerdict{{Name: "greptile", Passed: approved, Pending: pending}},
	}, nil
}

// PRHighSeverityReviewOnHead lets the supervisor #565 auto-review-repair
// branch peek at fake P0/P1 inline findings. Returns the configured head
// SHA + findings for prNumber; missing entries are no findings.
func (f *fakeReader) PRHighSeverityReviewOnHead(prNumber int) (string, []github.ReviewComment, bool, error) {
	sha := f.highSeverityHeadSHA[prNumber]
	findings := f.highSeverityFindings[prNumber]
	return sha, findings, len(findings) > 0, nil
}

func (f *fakeReader) PRBlockingReviewFindingsOnHead(prNumber int, streams []string) (string, []github.ReviewComment, bool, error) {
	return f.PRHighSeverityReviewOnHead(prNumber)
}

func (f *fakeReader) RateLimit() (github.RateLimitStatus, error) {
	f.rateLimitCalls++
	if f.rateLimitErr != nil {
		return github.RateLimitStatus{}, f.rateLimitErr
	}
	if f.rateLimit != nil {
		return *f.rateLimit, nil
	}
	return github.RateLimitStatus{
		Core:    github.RateLimitBucket{Limit: 5000, Remaining: 5000},
		GraphQL: github.RateLimitBucket{Limit: 5000, Remaining: 5000},
	}, nil
}

func testConfig(t *testing.T) *config.Config {
	t.Helper()
	return &config.Config{
		Repo:               "owner/repo",
		StateDir:           t.TempDir(),
		MaxParallel:        1,
		MaxRetriesPerIssue: 3,
	}
}

func enableDynamicWave(cfg *config.Config) {
	enabled := true
	cfg.Supervisor.DynamicWave.Enabled = &enabled
}

func testEngine(cfg *config.Config, reader *fakeReader) *Engine {
	eng := NewEngine(cfg, reader)
	eng.now = func() time.Time { return time.Date(2026, 4, 29, 12, 0, 0, 0, time.UTC) }
	eng.pidAlive = func(pid int) bool { return true }
	eng.lookPath = func(file string) (string, error) { return file, nil }
	// Isolate the in-process enrollment dedup (#569) so each test starts
	// with an empty set and does not bleed state into sibling tests.
	eng.enrollmentTracker = newInMemoryEnrollmentTracker()
	return eng
}

func requireStuckState(t *testing.T, decision state.SupervisorDecision, code string) state.SupervisorStuckState {
	t.Helper()
	for _, stuck := range decision.StuckStates {
		if stuck.Code == code {
			return stuck
		}
	}
	t.Fatalf("stuck state %q not found in %#v", code, decision.StuckStates)
	return state.SupervisorStuckState{}
}

func TestDecide_DoesNotPollRateLimitEndpointForIdleDecision(t *testing.T) {
	cfg := testConfig(t)
	cfg.GitHubProjects.Enabled = true
	cfg.GitHubProjects.ProjectNumber = 3
	reader := &fakeReader{
		rateLimit: &github.RateLimitStatus{
			Core:    github.RateLimitBucket{Limit: 5000, Remaining: 4990, Used: 10},
			GraphQL: github.RateLimitBucket{Limit: 5000, Remaining: 0, Used: 5000, Reset: 1779052352},
		},
	}

	decision, err := testEngine(cfg, reader).Decide(state.NewState())
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}

	if reader.rateLimitCalls != 0 {
		t.Fatalf("RateLimit calls = %d, want 0; supervisor must not burn API budget to measure API budget", reader.rateLimitCalls)
	}
	for _, stuck := range decision.StuckStates {
		if stuck.Code == "github_graphql_rate_exhausted" || stuck.Code == "github_rate_budget_unknown" {
			t.Fatalf("unexpected proactive rate-budget stuck state: %+v", stuck)
		}
	}
}

func testLLMEngine(cfg *config.Config, reader *fakeReader, llm *fakeLLM) *Engine {
	cfg.Supervisor.Enabled = true
	eng := testEngine(cfg, reader)
	eng.llm = llm
	return eng
}

func testIssue(number int, title string, labels ...string) github.Issue {
	issue := github.Issue{Number: number, Title: title}
	for _, label := range labels {
		issue.Labels = append(issue.Labels, struct {
			Name string `json:"name"`
		}{Name: label})
	}
	return issue
}

func boolPtr(v bool) *bool {
	return &v
}

func withProjectStatus(issue github.Issue, status string) github.Issue {
	issue.ProjectItems = []github.IssueProjectItem{{
		Title: "Maestro",
		Status: &github.IssueProjectItemStatus{
			Name: status,
		},
	}}
	return issue
}

func TestDecide_IdleNoEligibleIssueRecommendsLabel(t *testing.T) {
	cfg := testConfig(t)
	cfg.IssueLabels = []string{"maestro-ready"}
	enableDynamicWave(cfg)
	reader := &fakeReader{issues: []github.Issue{testIssue(308, "implement supervisor")}}

	decision, err := testEngine(cfg, reader).Decide(state.NewState())
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}

	if decision.RecommendedAction != ActionLabelIssueReady {
		t.Fatalf("action = %q, want %q", decision.RecommendedAction, ActionLabelIssueReady)
	}
	if decision.Target == nil || decision.Target.Issue != 308 {
		t.Fatalf("target = %#v, want issue 308", decision.Target)
	}
	if decision.Risk != RiskMutating {
		t.Errorf("risk = %q, want %q", decision.Risk, RiskMutating)
	}
	if decision.Mode != ModeReadOnly {
		t.Errorf("mode = %q, want %q", decision.Mode, ModeReadOnly)
	}
	if decision.QueueAnalysis == nil {
		t.Fatal("QueueAnalysis is nil")
	}
	if decision.QueueAnalysis.OpenIssues != 1 || decision.QueueAnalysis.EligibleCandidates != 1 {
		t.Fatalf("queue analysis = %#v, want one open eligible candidate", decision.QueueAnalysis)
	}
	if decision.QueueAnalysis.SelectedCandidate == nil || decision.QueueAnalysis.SelectedCandidate.Number != 308 {
		t.Fatalf("selected candidate = %#v, want issue 308", decision.QueueAnalysis.SelectedCandidate)
	}
}

func TestDecide_RunningWorkerWaits(t *testing.T) {
	cfg := testConfig(t)
	reader := &fakeReader{prs: []github.PR{{Number: 55, HeadRefName: "untracked-open-pr", State: "OPEN"}}}
	st := state.NewState()
	st.Sessions["slot-1"] = &state.Session{
		IssueNumber: 42,
		IssueTitle:  "work in progress",
		Status:      state.StatusRunning,
		PID:         12345,
		StartedAt:   time.Now().UTC(),
	}

	decision, err := testEngine(cfg, reader).Decide(st)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}

	if decision.RecommendedAction != ActionWaitForRunningWorker {
		t.Fatalf("action = %q, want %q", decision.RecommendedAction, ActionWaitForRunningWorker)
	}
	if decision.Target == nil || decision.Target.Session != "slot-1" || decision.Target.Issue != 42 {
		t.Fatalf("target = %#v, want slot-1 issue 42", decision.Target)
	}
	if reader.issueCalls != 0 {
		t.Fatalf("ListOpenIssues called %d time(s), want 0 for running-worker decision", reader.issueCalls)
	}
}

func TestDecide_RetryExhaustedNeedsReview(t *testing.T) {
	cfg := testConfig(t)
	reader := &fakeReader{prs: []github.PR{{Number: 55, HeadRefName: "untracked-open-pr", State: "OPEN"}}}
	st := state.NewState()
	st.Sessions["slot-2"] = &state.Session{
		IssueNumber: 77,
		IssueTitle:  "flaky work",
		Status:      state.StatusRetryExhausted,
		StartedAt:   time.Now().UTC().Add(-time.Hour),
	}

	decision, err := testEngine(cfg, reader).Decide(st)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}

	if decision.RecommendedAction != ActionReviewRetryExhausted {
		t.Fatalf("action = %q, want %q", decision.RecommendedAction, ActionReviewRetryExhausted)
	}
	if decision.Risk != RiskApprovalGated {
		t.Errorf("risk = %q, want %q", decision.Risk, RiskApprovalGated)
	}
	if decision.Target == nil || decision.Target.Issue != 77 {
		t.Fatalf("target = %#v, want issue 77", decision.Target)
	}
	stuck := requireStuckState(t, decision, "retry_exhausted")
	if stuck.Severity != SeverityBlocked {
		t.Errorf("severity = %q, want %q", stuck.Severity, SeverityBlocked)
	}
	if stuck.SupervisorCanAct {
		t.Error("retry_exhausted should require manual action")
	}
}

func TestDecide_RetryExhaustedSkippedWhenIssueClosed(t *testing.T) {
	cfg := testConfig(t)
	reader := &fakeReader{
		closedIssues: map[int]bool{768: true},
	}
	st := state.NewState()
	st.Sessions["pan-56"] = &state.Session{
		IssueNumber: 768,
		IssueTitle:  "stale exhausted work",
		Status:      state.StatusRetryExhausted,
		StartedAt:   time.Now().UTC().Add(-2 * time.Hour),
	}

	decision, err := testEngine(cfg, reader).Decide(st)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}

	if decision.RecommendedAction == ActionReviewRetryExhausted {
		t.Fatalf("action = %q, must not recommend reviewing a stale retry-exhausted session for a closed issue", decision.RecommendedAction)
	}
	if decision.Target != nil && decision.Target.Session == "pan-56" {
		t.Fatalf("target = %#v, must not target stale session pan-56 once issue #768 is closed", decision.Target)
	}
	for _, stuck := range decision.StuckStates {
		if stuck.Code == "retry_exhausted" {
			t.Fatalf("stuck state retry_exhausted should not be reported for closed issue: %#v", stuck)
		}
	}
}

func TestDecide_RetryExhaustedSkippedWhenIssueHasMergedWinningPR(t *testing.T) {
	cfg := testConfig(t)
	reader := &fakeReader{
		mergedPRIssues: map[int]bool{768: true},
	}
	st := state.NewState()
	st.Sessions["pan-56"] = &state.Session{
		IssueNumber: 768,
		IssueTitle:  "stale exhausted work, winner merged",
		Status:      state.StatusRetryExhausted,
		StartedAt:   time.Now().UTC().Add(-2 * time.Hour),
	}

	decision, err := testEngine(cfg, reader).Decide(st)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}

	if decision.RecommendedAction == ActionReviewRetryExhausted {
		t.Fatalf("action = %q, must not recommend reviewing a stale retry-exhausted session once a winning PR is merged", decision.RecommendedAction)
	}
	if decision.Target != nil && decision.Target.Session == "pan-56" {
		t.Fatalf("target = %#v, must not target stale session pan-56 once issue #768 has a merged winner", decision.Target)
	}
	for _, stuck := range decision.StuckStates {
		if stuck.Code == "retry_exhausted" {
			t.Fatalf("stuck state retry_exhausted should not be reported for issue resolved by merged PR: %#v", stuck)
		}
	}
}

func TestDecide_RetryExhaustedSkippedWhenSessionPRIsMerged(t *testing.T) {
	cfg := testConfig(t)
	reader := &fakeReader{
		mergedPRs: map[int]bool{820: true},
	}
	st := state.NewState()
	st.Sessions["pan-56"] = &state.Session{
		IssueNumber: 768,
		IssueTitle:  "merged work tagged retry_exhausted",
		Status:      state.StatusRetryExhausted,
		PRNumber:    820,
		StartedAt:   time.Now().UTC().Add(-2 * time.Hour),
	}

	decision, err := testEngine(cfg, reader).Decide(st)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}

	if decision.RecommendedAction == ActionReviewRetryExhausted {
		t.Fatalf("action = %q, must not recommend reviewing a retry-exhausted session whose own PR has merged", decision.RecommendedAction)
	}
	if decision.Target != nil && decision.Target.Session == "pan-56" {
		t.Fatalf("target = %#v, must not target session pan-56 once its PR #820 is merged", decision.Target)
	}
}

func TestDecide_RetryExhaustedSkippedWhenNewerSessionForIssueIsLive(t *testing.T) {
	cfg := testConfig(t)
	reader := &fakeReader{}
	now := time.Date(2026, 4, 29, 12, 0, 0, 0, time.UTC)
	finished := now.Add(-20 * time.Minute)
	st := state.NewState()
	st.Sessions["scr-528"] = &state.Session{
		IssueNumber: 528,
		IssueTitle:  "stale exhausted work",
		Status:      state.StatusRetryExhausted,
		StartedAt:   now.Add(-time.Hour),
		FinishedAt:  &finished,
	}
	st.Sessions["scr-532"] = &state.Session{
		IssueNumber: 528,
		IssueTitle:  "fresh respawn",
		Status:      state.StatusRunning,
		PID:         12345,
		StartedAt:   now.Add(-10 * time.Minute),
	}

	decision, err := testEngine(cfg, reader).Decide(st)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}

	if decision.RecommendedAction == ActionReviewRetryExhausted {
		t.Fatalf("action = %q, must not review stale retry_exhausted once same issue has a newer live session", decision.RecommendedAction)
	}
	if decision.Target != nil && decision.Target.Session == "scr-528" {
		t.Fatalf("target = %#v, must not target stale session scr-528", decision.Target)
	}
	for _, stuck := range decision.StuckStates {
		if stuck.Code == "retry_exhausted" && stuck.Target != nil && stuck.Target.Session == "scr-528" {
			t.Fatalf("stale retry_exhausted stuck state reported: %#v", stuck)
		}
	}
}

func TestDecide_RetryExhaustedResolutionCachedPerDecisionCycle(t *testing.T) {
	cfg := testConfig(t)
	reader := &fakeReader{}
	st := state.NewState()
	st.Sessions["pan-56"] = &state.Session{
		IssueNumber: 768,
		IssueTitle:  "stale exhausted work checked in two places",
		Status:      state.StatusRetryExhausted,
		StartedAt:   time.Now().UTC().Add(-2 * time.Hour),
	}

	if _, err := testEngine(cfg, reader).Decide(st); err != nil {
		t.Fatalf("Decide: %v", err)
	}

	if got := reader.closedIssueCalls[768]; got > 1 {
		t.Fatalf("IsIssueClosed(#768) called %d times in one decision cycle; want at most 1", got)
	}
	if got := reader.mergedPRIssueCalls[768]; got > 1 {
		t.Fatalf("HasMergedPRForIssue(#768) called %d times in one decision cycle; want at most 1", got)
	}
}

// #512 (Phase 1.6): a retry-exhausted session whose PR is fully ready
// (mergeable, CI green, review gate off) should now get an actionable
// merge_pr decision rather than the old monitor_open_pr loop. The
// retry-exhausted stuck-state info is still attached.
func TestDecide_RetryExhaustedOpenGreenPRRecommendsMerge(t *testing.T) {
	cfg := testConfig(t)
	cfg.ReviewGate = "none"
	reader := &fakeReader{
		prs:        []github.PR{{Number: 88, HeadRefName: "feat/retry-green", Mergeable: "MERGEABLE"}},
		ciStatuses: map[int]string{88: "success"},
	}
	st := state.NewState()
	st.Sessions["slot-2"] = &state.Session{
		IssueNumber: 77,
		IssueTitle:  "green work",
		Status:      state.StatusRetryExhausted,
		Branch:      "feat/retry-green",
		PRNumber:    88,
		StartedAt:   time.Now().UTC().Add(-time.Hour),
	}

	decision, err := testEngine(cfg, reader).Decide(st)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}

	if decision.RecommendedAction != ActionMergePR {
		t.Fatalf("action = %q, want %q", decision.RecommendedAction, ActionMergePR)
	}
	if decision.Risk != RiskMutating {
		t.Fatalf("risk = %q, want %q (cautious gate must require approval for merge_pr)", decision.Risk, RiskMutating)
	}
	if decision.Target == nil || decision.Target.PR != 88 {
		t.Fatalf("target = %#v, want PR=88", decision.Target)
	}
	stuck := requireStuckState(t, decision, "retry_exhausted_open_pr")
	if stuck.Severity != SeverityInfo {
		t.Errorf("severity = %q, want %q", stuck.Severity, SeverityInfo)
	}
}

// #512: PR has all gates green and a normal (non-retry-exhausted) session
// → planner recommends merge_pr with mutating risk (cautious gate gates
// the actual merge, but the recommendation surfaces a real button).
func TestDecide_OpenPR_AllGreen_RecommendsMerge(t *testing.T) {
	cfg := testConfig(t)
	cfg.ReviewGate = "none"
	reader := &fakeReader{
		prs:        []github.PR{{Number: 99, HeadRefName: "feat/all-green", Mergeable: "MERGEABLE"}},
		ciStatuses: map[int]string{99: "success"},
	}
	st := state.NewState()
	st.Sessions["slot-1"] = &state.Session{
		IssueNumber: 200,
		IssueTitle:  "ready PR",
		Status:      state.StatusPROpen,
		Branch:      "feat/all-green",
		PRNumber:    99,
		StartedAt:   time.Now().UTC().Add(-30 * time.Minute),
	}

	decision, err := testEngine(cfg, reader).Decide(st)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if decision.RecommendedAction != ActionMergePR {
		t.Fatalf("action = %q, want merge_pr", decision.RecommendedAction)
	}
	if decision.Risk != RiskMutating {
		t.Fatalf("risk = %q, want mutating", decision.Risk)
	}
}

// Regression: a session whose PREVIOUS attempt was retried on review
// feedback (PreviousAttemptFeedbackKind=review_feedback) used to wedge a
// now-green PR forever — detectWorkerStuckStates raised
// stale_review_feedback[blocked], openPRNeedsRepair short-circuited to
// spawn_repair_worker, and the executor refuses that verb (not in the
// registry). Once the feedback is addressed (PR green + mergeable), the
// merge_pr rule (#512) must win.
func TestDecide_StaleReviewFeedbackButGreenPRRecommendsMerge(t *testing.T) {
	cfg := testConfig(t)
	cfg.ReviewGate = "none"
	reader := &fakeReader{
		prs:          []github.PR{{Number: 548, HeadRefName: "feat/stale-review-green", State: "OPEN", Mergeable: "MERGEABLE"}},
		ciStatuses:   map[int]string{548: "success"},
		mergedPRs:    map[int]bool{548: false},
		closedIssues: map[int]bool{532: false},
	}
	st := state.NewState()
	st.Sessions["slot-1"] = &state.Session{
		IssueNumber:                 532,
		IssueTitle:                  "project card actionable",
		Status:                      state.StatusRetryExhausted,
		Branch:                      "feat/stale-review-green",
		PRNumber:                    548,
		PreviousAttemptFeedbackKind: state.RetryReasonReviewFeedback,
		StartedAt:                   time.Now().UTC().Add(-time.Hour),
	}

	decision, err := testEngine(cfg, reader).Decide(st)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if decision.RecommendedAction != ActionMergePR {
		t.Fatalf("action = %q, want %q (green mergeable PR with addressed review feedback must merge, not loop on spawn_repair_worker)", decision.RecommendedAction, ActionMergePR)
	}
	if decision.Risk != RiskMutating {
		t.Fatalf("risk = %q, want %q", decision.Risk, RiskMutating)
	}
	if decision.Target == nil || decision.Target.PR != 548 {
		t.Fatalf("target = %#v, want PR=548", decision.Target)
	}
}

// #556: a retry-exhausted session with an open PR and unaddressed review
// feedback must NOT recommend `spawn_repair_worker`. That verb is not in
// the executor's action registry, so the supervisor's at-mint guard
// refuses it every cycle (logged as "refusing to mint approval"), which
// is the dogfood loop the issue calls out. The decision must fall
// through to a registry-supported / non-mutating recommendation such as
// `monitor_open_pr`.
func TestDecide_RetryExhaustedUnaddressedFeedback_DoesNotRecommendSpawnRepairWorker(t *testing.T) {
	cfg := testConfig(t)
	cfg.ReviewGate = "none"
	reader := &fakeReader{
		prs:          []github.PR{{Number: 555, HeadRefName: "feat/sup-92", State: "OPEN", Mergeable: "MERGEABLE"}},
		ciStatuses:   map[int]string{555: "success"},
		mergedPRs:    map[int]bool{555: false},
		closedIssues: map[int]bool{535: false},
	}
	st := state.NewState()
	st.Sessions["sup-92"] = &state.Session{
		IssueNumber:                 535,
		IssueTitle:                  "unaddressed P1 review",
		Status:                      state.StatusRetryExhausted,
		Branch:                      "feat/sup-92",
		PRNumber:                    555,
		RetryCount:                  2,
		LastNotifiedStatus:          "review_retry_exhausted",
		PreviousAttemptFeedbackKind: state.RetryReasonReviewFeedback,
		PreviousAttemptFeedback:     "## Review Feedback\n\ninternal/foo.go:42 P1: nil pointer panic",
		StartedAt:                   time.Now().UTC().Add(-2 * time.Hour),
	}

	decision, err := testEngine(cfg, reader).Decide(st)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}

	if decision.RecommendedAction == ActionSpawnRepairWorker {
		t.Fatalf("action = %q, must NOT recommend spawn_repair_worker for retry_exhausted (verb is refused at mint, leads to dogfood loop)", decision.RecommendedAction)
	}
}

// #512: CI is still pending → planner stays on monitor_open_pr with a
// summary that names the actual blocker, not the old aspirational text.
func TestDecide_OpenPR_CIPending_StaysMonitor(t *testing.T) {
	cfg := testConfig(t)
	cfg.ReviewGate = "none"
	reader := &fakeReader{
		prs:        []github.PR{{Number: 100, HeadRefName: "feat/ci-pending", Mergeable: "MERGEABLE"}},
		ciStatuses: map[int]string{100: "pending"},
	}
	st := state.NewState()
	st.Sessions["slot-1"] = &state.Session{
		IssueNumber: 201,
		Status:      state.StatusPROpen,
		Branch:      "feat/ci-pending",
		PRNumber:    100,
		StartedAt:   time.Now().UTC().Add(-10 * time.Minute),
	}

	decision, err := testEngine(cfg, reader).Decide(st)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if decision.RecommendedAction != ActionMonitorOpenPR {
		t.Fatalf("action = %q, want monitor_open_pr (CI pending must NOT trigger merge_pr)", decision.RecommendedAction)
	}
	if !strings.Contains(strings.ToLower(decision.Summary), "ci status=pending") {
		t.Fatalf("summary = %q, want a substring naming CI status pending", decision.Summary)
	}
}

// #512: PR is conflicting / dirty → planner stays on monitor_open_pr.
// (The repair branch above already catches drafts, so we exercise
// the post-repair-check fall-through here.)
func TestDecide_OpenPR_Conflicting_StaysMonitor(t *testing.T) {
	cfg := testConfig(t)
	cfg.ReviewGate = "none"
	reader := &fakeReader{
		prs:        []github.PR{{Number: 101, HeadRefName: "feat/dirty", Mergeable: "CONFLICTING"}},
		ciStatuses: map[int]string{101: "success"},
	}
	st := state.NewState()
	st.Sessions["slot-1"] = &state.Session{
		IssueNumber: 202,
		Status:      state.StatusPROpen,
		Branch:      "feat/dirty",
		PRNumber:    101,
		StartedAt:   time.Now().UTC().Add(-10 * time.Minute),
	}

	decision, err := testEngine(cfg, reader).Decide(st)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if decision.RecommendedAction != ActionMonitorOpenPR {
		t.Fatalf("action = %q, want monitor_open_pr (CONFLICTING must NOT merge)", decision.RecommendedAction)
	}
	if !strings.Contains(decision.Summary, "CONFLICTING") {
		t.Fatalf("summary = %q, want a substring naming CONFLICTING", decision.Summary)
	}
}

func TestDecide_DeadRunningPIDExplained(t *testing.T) {
	cfg := testConfig(t)
	reader := &fakeReader{}
	st := state.NewState()
	st.Sessions["slot-1"] = &state.Session{
		IssueNumber: 91,
		IssueTitle:  "lost worker",
		Status:      state.StatusRunning,
		PID:         424242,
		StartedAt:   time.Now().UTC().Add(-time.Hour),
	}
	eng := testEngine(cfg, reader)
	eng.pidAlive = func(pid int) bool { return false }

	decision, err := eng.Decide(st)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}

	stuck := requireStuckState(t, decision, "dead_running_pid")
	if stuck.Severity != SeverityBlocked {
		t.Errorf("severity = %q, want %q", stuck.Severity, SeverityBlocked)
	}
	if !stuck.SupervisorCanAct {
		t.Error("dead running PID should be automatically reconcilable")
	}
}

func TestDecide_StaleWorkerLogsExplained(t *testing.T) {
	cfg := testConfig(t)
	cfg.WorkerSilentTimeoutMinutes = 10
	reader := &fakeReader{}
	st := state.NewState()
	st.Sessions["slot-1"] = &state.Session{
		IssueNumber:         92,
		IssueTitle:          "silent worker",
		Status:              state.StatusRunning,
		PID:                 12345,
		StartedAt:           time.Date(2026, 4, 29, 10, 0, 0, 0, time.UTC),
		LastOutputChangedAt: time.Date(2026, 4, 29, 11, 40, 0, 0, time.UTC),
	}

	decision, err := testEngine(cfg, reader).Decide(st)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}

	stuck := requireStuckState(t, decision, "stale_worker_logs")
	if stuck.Target == nil || stuck.Target.Session != "slot-1" {
		t.Fatalf("target = %#v, want slot-1", stuck.Target)
	}
}

func TestDetectWorkerStuckStates_SuppressesResolvedReviewFeedback(t *testing.T) {
	now := time.Date(2026, 4, 29, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name   string
		reader *fakeReader
		sess   *state.Session
	}{
		{
			name:   "done session",
			reader: &fakeReader{mergedPRs: map[int]bool{375: true}},
			sess: &state.Session{
				IssueNumber:                 359,
				Status:                      state.StatusDone,
				PRNumber:                    375,
				PreviousAttemptFeedbackKind: state.RetryReasonReviewFeedback,
			},
		},
		{
			name:   "merged PR",
			reader: &fakeReader{mergedPRs: map[int]bool{375: true}},
			sess: &state.Session{
				IssueNumber:                 359,
				Status:                      state.StatusDead,
				PRNumber:                    375,
				PreviousAttemptFeedbackKind: state.RetryReasonReviewFeedback,
			},
		},
		{
			name:   "retry exhausted merged PR",
			reader: &fakeReader{mergedPRs: map[int]bool{375: true}},
			sess: &state.Session{
				IssueNumber:                 359,
				Status:                      state.StatusRetryExhausted,
				PRNumber:                    375,
				PreviousAttemptFeedbackKind: state.RetryReasonReviewFeedback,
			},
		},
		{
			name:   "closed issue",
			reader: &fakeReader{closedIssues: map[int]bool{359: true}},
			sess: &state.Session{
				IssueNumber:                 359,
				Status:                      state.StatusDead,
				PRNumber:                    375,
				PreviousAttemptFeedbackKind: state.RetryReasonReviewFeedback,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := state.NewState()
			st.Sessions["slot-1"] = tt.sess

			eng := testEngine(testConfig(t), tt.reader)
			findings := eng.detectWorkerStuckStates(st, now, newResolutionCache(eng.reader))

			for _, stuck := range findings {
				if stuck.Code == "stale_review_feedback" {
					t.Fatalf("resolved review feedback should not create stale_review_feedback: %#v", findings)
				}
			}
		})
	}
}

func TestDetectWorkerStuckStates_OpenReviewFeedbackNeedsAttention(t *testing.T) {
	cfg := testConfig(t)
	reader := &fakeReader{
		prs:          []github.PR{{Number: 376, State: "OPEN"}},
		mergedPRs:    map[int]bool{376: false},
		closedIssues: map[int]bool{360: false},
	}
	st := state.NewState()
	st.Sessions["slot-1"] = &state.Session{
		IssueNumber:                 360,
		Status:                      state.StatusPROpen,
		PRNumber:                    376,
		PreviousAttemptFeedbackKind: state.RetryReasonReviewFeedback,
	}

	decision, err := testEngine(cfg, reader).Decide(st)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}

	stuck := requireStuckState(t, decision, "stale_review_feedback")
	if stuck.Severity != SeverityBlocked || stuck.Target == nil || stuck.Target.PR != 376 {
		t.Fatalf("stale review feedback stuck state = %#v, want blocked PR #376", stuck)
	}
}

func TestDecide_ClosedPRWithActiveSessionExplained(t *testing.T) {
	cfg := testConfig(t)
	reader := &fakeReader{}
	st := state.NewState()
	st.Sessions["slot-1"] = &state.Session{
		IssueNumber: 93,
		IssueTitle:  "closed pr",
		Status:      state.StatusPROpen,
		PRNumber:    17,
		Branch:      "feat/closed-pr",
		StartedAt:   time.Now().UTC().Add(-time.Hour),
	}

	decision, err := testEngine(cfg, reader).Decide(st)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}

	stuck := requireStuckState(t, decision, "closed_pr_with_active_session")
	if stuck.Target == nil || stuck.Target.PR != 17 {
		t.Fatalf("target = %#v, want PR 17", stuck.Target)
	}
}

func TestDecide_FailingChecksExplained(t *testing.T) {
	cfg := testConfig(t)
	reader := &fakeReader{
		prs:        []github.PR{{Number: 31, HeadRefName: "feat/checks", State: "OPEN", Mergeable: "MERGEABLE"}},
		ciStatuses: map[int]string{31: "failure"},
	}
	st := state.NewState()
	st.Sessions["slot-1"] = &state.Session{
		IssueNumber: 94,
		IssueTitle:  "failing checks",
		Status:      state.StatusPROpen,
		PRNumber:    31,
		Branch:      "feat/checks",
		StartedAt:   time.Now().UTC().Add(-time.Hour),
	}

	decision, err := testEngine(cfg, reader).Decide(st)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}

	stuck := requireStuckState(t, decision, "failing_checks")
	if stuck.Severity != SeverityBlocked {
		t.Errorf("severity = %q, want %q", stuck.Severity, SeverityBlocked)
	}
}

func TestDecide_DeadSessionWithDraftFailingPRRecommendsRepairWorker(t *testing.T) {
	cfg := testConfig(t)
	reader := &fakeReader{
		prs: []github.PR{{
			Number:      31,
			HeadRefName: "feat/checks",
			State:       "OPEN",
			Mergeable:   "MERGEABLE",
			IsDraft:     true,
		}},
		ciStatuses: map[int]string{31: "failure"},
	}
	st := state.NewState()
	st.Sessions["slot-1"] = &state.Session{
		IssueNumber: 94,
		IssueTitle:  "failing checks",
		Status:      state.StatusDead,
		PRNumber:    31,
		Branch:      "feat/checks",
		StartedAt:   time.Now().UTC().Add(-time.Hour),
	}

	decision, err := testEngine(cfg, reader).Decide(st)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}

	if decision.RecommendedAction != ActionSpawnRepairWorker {
		t.Fatalf("action = %q, want %q", decision.RecommendedAction, ActionSpawnRepairWorker)
	}
	if decision.Risk != RiskMutating {
		t.Fatalf("risk = %q, want %q", decision.Risk, RiskMutating)
	}
	if decision.Target == nil || decision.Target.Issue != 94 || decision.Target.PR != 31 || decision.Target.Session != "slot-1" {
		t.Fatalf("target = %#v, want issue 94 PR 31 slot-1", decision.Target)
	}
	if !strings.Contains(strings.ToLower(decision.Summary), "repair worker") {
		t.Fatalf("summary = %q, want repair worker", decision.Summary)
	}
}

func TestDecide_RetryExhaustedReadyIssueSpawnsRepairWorkerEvenWhenProjectBlocked(t *testing.T) {
	cfg := testConfig(t)
	cfg.IssueLabels = []string{"maestro-ready"}
	enableDynamicWave(cfg)
	reader := &fakeReader{issues: []github.Issue{
		withProjectStatus(testIssue(808, "duplicate settings entry", "maestro-ready"), "Blocked"),
	}}
	st := state.NewState()
	st.Sessions["pan-72"] = &state.Session{
		IssueNumber: 808,
		IssueTitle:  "duplicate settings entry",
		Status:      state.StatusRetryExhausted,
		StartedAt:   time.Now().UTC().Add(-time.Hour),
	}

	decision, err := testEngine(cfg, reader).Decide(st)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}

	if decision.RecommendedAction != ActionSpawnRepairWorker {
		t.Fatalf("action = %q, want %q", decision.RecommendedAction, ActionSpawnRepairWorker)
	}
	if decision.RequiresApproval {
		t.Fatalf("RequiresApproval = true, want false for policy-allowed repair spawn")
	}
	if decision.Target == nil || decision.Target.Issue != 808 || decision.Target.Session != "pan-72" {
		t.Fatalf("target = %#v, want issue 808 pan-72", decision.Target)
	}
	rationale := strings.Join(decision.Reasons, "\n")
	if !strings.Contains(rationale, "retry_exhausted") || !strings.Contains(rationale, "Blocked") {
		t.Fatalf("reasons = %q, want retry_exhausted and Blocked rationale", rationale)
	}
}

func TestDecide_PRExistsForSessionMonitorsPR(t *testing.T) {
	cfg := testConfig(t)
	reader := &fakeReader{prs: []github.PR{{Number: 12, HeadRefName: "mae-1-42-fix", State: "OPEN"}}}
	st := state.NewState()
	st.Sessions["slot-1"] = &state.Session{
		IssueNumber: 42,
		IssueTitle:  "fix bug",
		Status:      state.StatusDead,
		Branch:      "mae-1-42-fix",
		StartedAt:   time.Now().UTC().Add(-time.Hour),
	}

	decision, err := testEngine(cfg, reader).Decide(st)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}

	if decision.RecommendedAction != ActionMonitorOpenPR {
		t.Fatalf("action = %q, want %q", decision.RecommendedAction, ActionMonitorOpenPR)
	}
	if decision.Target == nil || decision.Target.PR != 12 || decision.Target.Session != "slot-1" {
		t.Fatalf("target = %#v, want PR 12 for slot-1", decision.Target)
	}
	if reader.issueCalls != 0 {
		t.Fatalf("ListOpenIssues called %d time(s), want 0 for PR-session decision", reader.issueCalls)
	}
}

func TestDecide_EligibleIssueRecommendsSpawn(t *testing.T) {
	cfg := testConfig(t)
	cfg.IssueLabels = []string{"maestro-ready"}
	reader := &fakeReader{issues: []github.Issue{testIssue(42, "ready work", "maestro-ready")}}

	decision, err := testEngine(cfg, reader).Decide(state.NewState())
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}

	if decision.RecommendedAction != ActionSpawnWorker {
		t.Fatalf("action = %q, want %q", decision.RecommendedAction, ActionSpawnWorker)
	}
	if decision.Target == nil || decision.Target.Issue != 42 {
		t.Fatalf("target = %#v, want issue 42", decision.Target)
	}
	if decision.Risk != RiskMutating {
		t.Errorf("risk = %q, want %q", decision.Risk, RiskMutating)
	}
}

func TestDecide_OutcomeRationaleNamesGoalAndIssue(t *testing.T) {
	cfg := testConfig(t)
	cfg.IssueLabels = []string{"maestro-ready"}
	cfg.Outcome = outcome.Brief{
		DesiredOutcome: "Maestro dogfood dashboard runs unattended",
		RuntimeTarget:  "http://127.0.0.1:8786",
	}
	reader := &fakeReader{issues: []github.Issue{testIssue(42, "wire outcome status", "maestro-ready")}}

	decision, err := testEngine(cfg, reader).Decide(state.NewState())
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if decision.Outcome == nil || !decision.Outcome.Configured || decision.Outcome.Goal != cfg.Outcome.DesiredOutcome {
		t.Fatalf("Outcome = %#v, want configured dogfood context", decision.Outcome)
	}
	reasons := strings.Join(decision.Reasons, "\n")
	if !strings.Contains(reasons, "Outcome: Maestro dogfood dashboard runs unattended") {
		t.Fatalf("reasons = %q, want current outcome", reasons)
	}
	if !strings.Contains(reasons, "Issue #42") || !strings.Contains(reasons, "toward Maestro dogfood dashboard runs unattended") {
		t.Fatalf("reasons = %q, want issue-to-outcome rationale", reasons)
	}
}

func TestDecide_NoOutcomeProgressRecommendsRuntimeCheck(t *testing.T) {
	cfg := testConfig(t)
	cfg.Outcome = outcome.Brief{
		DesiredOutcome: "Hosted app responds to users",
		RuntimeTarget:  "https://app.example.com",
		HealthcheckURL: "https://app.example.com/healthz",
	}
	reader := &fakeReader{}
	now := time.Date(2026, 4, 29, 12, 0, 0, 0, time.UTC)
	st := state.NewState()
	st.LastMergeAt = now.Add(-10 * time.Minute)
	st.Sessions["done-1"] = &state.Session{IssueNumber: 1, IssueTitle: "first", Status: state.StatusDone, PRNumber: 10, StartedAt: now.Add(-2 * time.Hour)}
	st.Sessions["done-2"] = &state.Session{IssueNumber: 2, IssueTitle: "second", Status: state.StatusDone, PRNumber: 11, StartedAt: now.Add(-time.Hour)}

	decision, err := testEngine(cfg, reader).Decide(st)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if decision.RecommendedAction != ActionCheckOutcomeHealth {
		t.Fatalf("action = %q, want %q", decision.RecommendedAction, ActionCheckOutcomeHealth)
	}
	if decision.Risk != RiskSafe {
		t.Fatalf("risk = %q, want safe read-only recommendation", decision.Risk)
	}
	stuck := requireStuckState(t, decision, state.StuckNoOutcomeProgress)
	if stuck.SupervisorCanAct {
		t.Fatal("no_outcome_progress should not mutate deploy/runtime state")
	}
	if !strings.Contains(stuck.Summary, "Hosted app responds to users") || !strings.Contains(stuck.Summary, "unknown") {
		t.Fatalf("stuck summary = %q, want outcome and unknown health", stuck.Summary)
	}
	if reader.issueCalls != 0 {
		t.Fatalf("ListOpenIssues called %d time(s), want runtime check before more issue throughput", reader.issueCalls)
	}
}

func TestDecide_FailingOutcomeImmediatelyBlocksFalseGreen(t *testing.T) {
	cfg := testConfig(t)
	cfg.Outcome = outcome.Brief{
		DesiredOutcome: "Hosted app responds to users",
		RuntimeTarget:  "https://app.example.com",
		HealthcheckURL: "https://app.example.com/healthz",
	}
	reader := &fakeReader{prs: []github.PR{{Number: 55, HeadRefName: "untracked-open-pr", State: "OPEN"}}}
	now := time.Date(2026, 4, 29, 12, 0, 0, 0, time.UTC)
	st := state.NewState()
	st.OutcomeHealth = &outcome.HealthCheckResult{
		CheckedAt: now.Add(-time.Minute),
		Signal:    "healthcheck_url",
		State:     outcome.HealthFailing,
		Summary:   "GET returned 500",
	}

	decision, err := testEngine(cfg, reader).Decide(st)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}

	if decision.RecommendedAction != ActionCheckOutcomeHealth {
		t.Fatalf("action = %q, want %q", decision.RecommendedAction, ActionCheckOutcomeHealth)
	}
	stuck := requireStuckState(t, decision, state.StuckNoOutcomeProgress)
	if stuck.Severity != SeverityBlocked {
		t.Fatalf("severity = %q, want %q", stuck.Severity, SeverityBlocked)
	}
	if !strings.Contains(stuck.Summary, "failing") {
		t.Fatalf("stuck summary = %q, want failing outcome", stuck.Summary)
	}
	if reader.issueCalls != 0 {
		t.Fatalf("ListOpenIssues called %d time(s), want failing outcome gate before issue throughput", reader.issueCalls)
	}
	reasons := strings.Join(decision.Reasons, "\n")
	if !strings.Contains(reasons, "Open PRs observed: 1") {
		t.Fatalf("reasons = %q, want open PRs to be treated as insufficient proof", reasons)
	}
}

func TestDecide_IgnoresOpenPRWhenIssueAlreadyClosed(t *testing.T) {
	cfg := testConfig(t)
	reader := &fakeReader{
		prs:          []github.PR{{Number: 769, HeadRefName: "codex/old-pr", IsDraft: true, State: "OPEN"}},
		closedIssues: map[int]bool{767: true},
	}
	st := state.NewState()
	st.Sessions["pan-12"] = &state.Session{
		IssueNumber: 767,
		IssueTitle:  "already closed",
		Status:      state.StatusPROpen,
		PRNumber:    769,
		Branch:      "codex/old-pr",
	}

	decision, err := testEngine(cfg, reader).Decide(st)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if decision.RecommendedAction == ActionSpawnRepairWorker || decision.RecommendedAction == ActionMonitorOpenPR {
		t.Fatalf("action = %q, want no open-PR work for closed issue", decision.RecommendedAction)
	}
	for _, stuck := range decision.StuckStates {
		if stuck.Target != nil && stuck.Target.Issue == 767 {
			t.Fatalf("stuck state targets closed issue: %+v", stuck)
		}
	}
}

func TestDecideDeterministic_OutcomeUsesStateMergeHistory(t *testing.T) {
	cfg := testConfig(t)
	cfg.Outcome = outcome.Brief{
		DesiredOutcome: "Hosted app responds to users",
		HealthcheckURL: "https://app.example.com/healthz",
	}
	now := time.Date(2026, 4, 29, 12, 0, 0, 0, time.UTC)
	st := state.NewState()
	st.LastMergeAt = now
	st.Sessions["done-1"] = &state.Session{IssueNumber: 1, IssueTitle: "first", Status: state.StatusDone, PRNumber: 10}
	st.Sessions["done-2"] = &state.Session{IssueNumber: 2, IssueTitle: "second", Status: state.StatusDone, PRNumber: 11}

	decision, err := testEngine(cfg, &fakeReader{}).decideDeterministic(st)
	if err != nil {
		t.Fatalf("decideDeterministic: %v", err)
	}
	if decision.Outcome == nil {
		t.Fatal("Outcome = nil, want state-aware outcome")
	}
	if decision.Outcome.MergedPRs != 2 || decision.Outcome.LastMergeAt == "" {
		t.Fatalf("Outcome = %+v, want merge history from state", decision.Outcome)
	}
}

func TestDecide_HealthyOutcomeAllowsIssueWorkAfterMerges(t *testing.T) {
	cfg := testConfig(t)
	cfg.IssueLabels = []string{"maestro-ready"}
	cfg.Outcome = outcome.Brief{
		DesiredOutcome: "Hosted app responds to users",
		HealthcheckURL: "https://app.example.com/healthz",
	}
	reader := &fakeReader{issues: []github.Issue{testIssue(42, "next outcome step", "maestro-ready")}}
	now := time.Date(2026, 4, 29, 12, 0, 0, 0, time.UTC)
	st := state.NewState()
	st.LastMergeAt = now.Add(-10 * time.Minute)
	st.OutcomeHealth = &outcome.HealthCheckResult{
		CheckedAt: now.Add(-time.Minute),
		Signal:    "healthcheck_url",
		State:     outcome.HealthHealthy,
		Summary:   "GET returned 200 OK",
	}
	st.Sessions["done-1"] = &state.Session{IssueNumber: 1, IssueTitle: "first", Status: state.StatusDone, PRNumber: 10}
	st.Sessions["done-2"] = &state.Session{IssueNumber: 2, IssueTitle: "second", Status: state.StatusDone, PRNumber: 11}

	decision, err := testEngine(cfg, reader).Decide(st)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if decision.RecommendedAction != ActionSpawnWorker {
		t.Fatalf("action = %q, want %q", decision.RecommendedAction, ActionSpawnWorker)
	}
	if decision.Outcome == nil || decision.Outcome.HealthState != outcome.HealthHealthy {
		t.Fatalf("Outcome = %+v, want healthy persisted outcome", decision.Outcome)
	}
	for _, stuck := range decision.StuckStates {
		if stuck.Code == state.StuckNoOutcomeProgress {
			t.Fatalf("unexpected no_outcome_progress stuck state: %+v", stuck)
		}
	}
}

func TestDecide_EmptyStateNoAction(t *testing.T) {
	cfg := testConfig(t)
	reader := &fakeReader{}

	decision, err := testEngine(cfg, reader).Decide(state.NewState())
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}

	if decision.RecommendedAction != ActionNone {
		t.Fatalf("action = %q, want %q", decision.RecommendedAction, ActionNone)
	}
	if decision.Target != nil {
		t.Fatalf("target = %#v, want nil", decision.Target)
	}
	if decision.ProjectState.OpenIssues != 0 || decision.ProjectState.OpenPRs != 0 {
		t.Fatalf("project state = %#v, want no open issues or PRs", decision.ProjectState)
	}
}

func TestRunOnceRecordsDecision(t *testing.T) {
	cfg := testConfig(t)
	reader := &fakeReader{}

	decision, err := RunOnce(cfg, reader)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if decision.RecommendedAction != ActionNone {
		t.Fatalf("action = %q, want %q", decision.RecommendedAction, ActionNone)
	}

	st, err := state.Load(cfg.StateDir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	latest := st.LatestSupervisorDecision()
	if latest == nil {
		t.Fatal("latest supervisor decision missing")
	}
	if latest.ID != decision.ID {
		t.Fatalf("latest ID = %q, want %q", latest.ID, decision.ID)
	}
	if len(st.Approvals) != 0 {
		t.Fatalf("approvals = %d, want 0 for safe action", len(st.Approvals))
	}
}

// Phase 1.2 (#499): a successful RunOnce stamps state.LastRunOnceAt
// (used by the supervise watchdog goroutine to detect silent loop wedges).
func TestRunOnce_StampsLastRunOnceAt(t *testing.T) {
	cfg := testConfig(t)
	reader := &fakeReader{}

	before := time.Now().UTC()
	if _, err := RunOnce(cfg, reader); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	after := time.Now().UTC()

	st, err := state.Load(cfg.StateDir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if st.LastRunOnceAt.IsZero() {
		t.Fatal("LastRunOnceAt is zero after a successful RunOnce — watchdog will fire on a healthy daemon")
	}
	if st.LastRunOnceAt.Before(before) || st.LastRunOnceAt.After(after.Add(time.Second)) {
		t.Fatalf("LastRunOnceAt=%s is outside the call window [%s, %s]",
			st.LastRunOnceAt, before, after)
	}
}

// Phase 1.2 (#499): a successful RunOnce clears any prior SupervisorStuck
// flag set by the watchdog. This is the only signal that unwedges the
// daemon — a healthy cycle is recovery.
func TestRunOnce_ClearsSupervisorStuckFlag(t *testing.T) {
	cfg := testConfig(t)
	reader := &fakeReader{}

	// Pre-seed state with SupervisorStuck = true (as the watchdog would).
	st, err := state.Load(cfg.StateDir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	st.SupervisorStuck = true
	st.SupervisorStuckReason = "synthetic stuck for test"
	if err := state.Save(cfg.StateDir, st); err != nil {
		t.Fatalf("Save: %v", err)
	}

	if _, err := RunOnce(cfg, reader); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	st, err = state.Load(cfg.StateDir)
	if err != nil {
		t.Fatalf("Load 2: %v", err)
	}
	if st.SupervisorStuck {
		t.Fatal("SupervisorStuck=true after a successful RunOnce; healthy cycle must clear the flag")
	}
	if st.SupervisorStuckReason != "" {
		t.Fatalf("SupervisorStuckReason=%q; should be cleared", st.SupervisorStuckReason)
	}
}

func TestRunOnceRecordsOutcomeHealth(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	cfg := testConfig(t)
	cfg.Outcome = outcome.Brief{
		DesiredOutcome: "Hosted app responds to users",
		HealthcheckURL: server.URL,
	}
	reader := &fakeReader{}

	decision, err := RunOnce(cfg, reader)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if decision.Outcome == nil || decision.Outcome.HealthState != outcome.HealthHealthy {
		t.Fatalf("decision outcome = %+v, want healthy", decision.Outcome)
	}

	st, err := state.Load(cfg.StateDir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if st.OutcomeHealth == nil || st.OutcomeHealth.State != outcome.HealthHealthy {
		t.Fatalf("stored outcome health = %+v, want healthy", st.OutcomeHealth)
	}
}

func TestRunOnceRecordsPendingApprovalForRiskyDecision(t *testing.T) {
	cfg := testConfig(t)
	cfg.IssueLabels = []string{"maestro-ready"}
	reader := &fakeReader{issues: []github.Issue{testIssue(42, "ready work", "maestro-ready")}}

	decision, err := RunOnce(cfg, reader)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if decision.RecommendedAction != ActionSpawnWorker {
		t.Fatalf("action = %q, want %q", decision.RecommendedAction, ActionSpawnWorker)
	}
	if decision.ApprovalID == "" {
		t.Fatal("decision approval ID missing")
	}

	st, err := state.Load(cfg.StateDir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(st.Approvals) != 1 {
		t.Fatalf("approvals = %d, want 1", len(st.Approvals))
	}
	approval := st.Approvals[0]
	if approval.ID != decision.ApprovalID {
		t.Fatalf("approval ID = %q, want %q", approval.ID, decision.ApprovalID)
	}
	if approval.DecisionID != decision.ID {
		t.Fatalf("decision ID = %q, want %q", approval.DecisionID, decision.ID)
	}
	if approval.Status != state.ApprovalStatusPending {
		t.Fatalf("status = %q, want %q", approval.Status, state.ApprovalStatusPending)
	}
	if approval.Action != ActionSpawnWorker {
		t.Fatalf("approval action = %q, want %q", approval.Action, ActionSpawnWorker)
	}
	if approval.Target == nil || approval.Target.Issue != 42 {
		t.Fatalf("target = %#v, want issue 42", approval.Target)
	}
}

func TestRunOnceDecisionSurvivesStaleRunLoopSave(t *testing.T) {
	cfg := testConfig(t)
	cfg.IssueLabels = []string{"maestro-ready"}
	runSnapshot, err := state.Load(cfg.StateDir)
	if err != nil {
		t.Fatalf("Load run snapshot: %v", err)
	}

	reader := &fakeReader{issues: []github.Issue{testIssue(302, "Prevent state lost-update", "maestro-ready")}}
	decision, err := RunOnce(cfg, reader)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if decision.RecommendedAction != ActionSpawnWorker || decision.ApprovalID == "" {
		t.Fatalf("decision = %#v, want approval-gated spawn_worker", decision)
	}

	runSnapshot.Sessions["slot-1"] = &state.Session{
		IssueNumber: 17,
		IssueTitle:  "run-loop reconciliation",
		Status:      state.StatusRunning,
		StartedAt:   time.Now().UTC(),
		PID:         1234,
	}
	if err := state.Save(cfg.StateDir, runSnapshot); err != nil {
		t.Fatalf("Save stale run snapshot: %v", err)
	}

	st, err := state.Load(cfg.StateDir)
	if err != nil {
		t.Fatalf("Load merged state: %v", err)
	}
	latest := st.LatestSupervisorDecision()
	if latest == nil || latest.ID != decision.ID || latest.Target == nil || latest.Target.Issue != 302 {
		t.Fatalf("latest decision = %#v, want decision for issue #302", latest)
	}
	approval, ok := st.FindApproval(decision.ApprovalID)
	if !ok {
		t.Fatalf("approval %q missing after stale run-loop save", decision.ApprovalID)
	}
	if approval.Status != state.ApprovalStatusPending {
		t.Fatalf("approval status = %q, want pending", approval.Status)
	}
	if _, err := st.ApproveApproval(decision.ApprovalID, time.Now().UTC(), "test", "race preserved"); err != nil {
		t.Fatalf("ApproveApproval after race: %v", err)
	}
}

func TestDecide_OrderedQueueAdvancesAfterClosedIssue(t *testing.T) {
	cfg := testConfig(t)
	cfg.IssueLabels = []string{"maestro-ready"}
	cfg.Supervisor.OrderedQueue = config.SupervisorOrderedQueueConfig{Enabled: true, Issues: []int{308, 306}}
	reader := &fakeReader{issues: []github.Issue{
		testIssue(306, "second wave", "maestro-ready"),
		testIssue(308, "first wave", "maestro-ready"),
	}}

	decision, err := testEngine(cfg, reader).Decide(state.NewState())
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}

	if decision.RecommendedAction != ActionSpawnWorker {
		t.Fatalf("action = %q, want %q", decision.RecommendedAction, ActionSpawnWorker)
	}
	if decision.Target == nil || decision.Target.Issue != 308 {
		t.Fatalf("target = %#v, want issue 308", decision.Target)
	}
	if decision.PolicyRule != PolicyRuleOrderedQueue {
		t.Fatalf("PolicyRule = %q, want %q", decision.PolicyRule, PolicyRuleOrderedQueue)
	}
}

func TestDecide_OrderedQueueSkipsCompletedIssue(t *testing.T) {
	cfg := testConfig(t)
	cfg.IssueLabels = []string{"maestro-ready"}
	cfg.Supervisor.OrderedQueue = config.SupervisorOrderedQueueConfig{Enabled: true, Issues: []int{308, 306}}
	reader := &fakeReader{
		issues: []github.Issue{
			testIssue(306, "second wave", "maestro-ready"),
			testIssue(308, "done wave", "maestro-ready"),
		},
		mergedPRs: map[int]bool{77: true},
	}
	st := state.NewState()
	st.Sessions["slot-1"] = &state.Session{IssueNumber: 308, Status: state.StatusDone, PRNumber: 77, StartedAt: time.Now().UTC()}

	decision, err := testEngine(cfg, reader).Decide(st)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}

	if decision.Target == nil || decision.Target.Issue != 306 {
		t.Fatalf("target = %#v, want issue 306", decision.Target)
	}
}

func TestDecide_OrderedQueueMissingLabelTargetsQueueHead(t *testing.T) {
	cfg := testConfig(t)
	cfg.IssueLabels = []string{"maestro-ready"}
	cfg.Supervisor.OrderedQueue = config.SupervisorOrderedQueueConfig{Enabled: true, Issues: []int{308, 306}}
	reader := &fakeReader{issues: []github.Issue{
		testIssue(306, "ready later", "maestro-ready"),
		testIssue(308, "missing label"),
	}}

	decision, err := testEngine(cfg, reader).Decide(state.NewState())
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}

	if decision.RecommendedAction != ActionLabelIssueReady {
		t.Fatalf("action = %q, want %q", decision.RecommendedAction, ActionLabelIssueReady)
	}
	if decision.Target == nil || decision.Target.Issue != 308 {
		t.Fatalf("target = %#v, want issue 308", decision.Target)
	}
}

func TestDecide_SupervisorExcludedLabelsSkipIssue(t *testing.T) {
	cfg := testConfig(t)
	cfg.IssueLabels = []string{"maestro-ready"}
	cfg.Supervisor.ExcludedLabels = []string{"epic"}
	reader := &fakeReader{issues: []github.Issue{
		testIssue(1, "epic", "maestro-ready", "epic"),
		testIssue(2, "regular", "maestro-ready"),
	}}

	decision, err := testEngine(cfg, reader).Decide(state.NewState())
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}

	if decision.Target == nil || decision.Target.Issue != 2 {
		t.Fatalf("target = %#v, want issue 2", decision.Target)
	}
}

func TestDecide_SupervisorBlockedLabelSkipsIssue(t *testing.T) {
	cfg := testConfig(t)
	cfg.IssueLabels = []string{"maestro-ready"}
	cfg.Supervisor.BlockedLabel = "blocked"
	reader := &fakeReader{issues: []github.Issue{
		testIssue(1, "blocked", "maestro-ready", "blocked"),
		testIssue(2, "regular", "maestro-ready"),
	}}

	decision, err := testEngine(cfg, reader).Decide(state.NewState())
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}

	if decision.Target == nil || decision.Target.Issue != 2 {
		t.Fatalf("target = %#v, want issue 2", decision.Target)
	}
}

func TestDecide_ConfigExcludeLabelsStillHonored(t *testing.T) {
	cfg := testConfig(t)
	cfg.IssueLabels = []string{"maestro-ready"}
	cfg.ExcludeLabels = []string{"blocked"}
	reader := &fakeReader{issues: []github.Issue{
		testIssue(1, "blocked", "maestro-ready", "blocked"),
		testIssue(2, "regular", "maestro-ready"),
	}}

	decision, err := testEngine(cfg, reader).Decide(state.NewState())
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}

	if decision.Target == nil || decision.Target.Issue != 2 {
		t.Fatalf("target = %#v, want issue 2", decision.Target)
	}
}

func TestDecide_SupervisorReadyLabelActsAsRequiredLabel(t *testing.T) {
	cfg := testConfig(t)
	cfg.Supervisor.ReadyLabel = "maestro-ready"
	dynamicEnabled := false
	cfg.Supervisor.DynamicWave.Enabled = &dynamicEnabled
	reader := &fakeReader{issues: []github.Issue{
		testIssue(1, "missing"),
		testIssue(2, "ready", "maestro-ready"),
	}}

	decision, err := testEngine(cfg, reader).Decide(state.NewState())
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}

	if decision.Target == nil || decision.Target.Issue != 2 {
		t.Fatalf("target = %#v, want issue 2", decision.Target)
	}
	if decision.PolicyRule != PolicyRuleIssueLabels {
		t.Fatalf("PolicyRule = %q, want %q", decision.PolicyRule, PolicyRuleIssueLabels)
	}
}

func TestDecide_DynamicWaveSortsByPriorityThenIssueNumber(t *testing.T) {
	cfg := testConfig(t)
	cfg.IssueLabels = []string{"maestro-ready"}
	enableDynamicWave(cfg)
	reader := &fakeReader{issues: []github.Issue{
		testIssue(30, "p2 work", "p2"),
		testIssue(20, "p0 work", "P0"),
		testIssue(10, "p0 lower number", "p0"),
	}}

	decision, err := testEngine(cfg, reader).Decide(state.NewState())
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}

	if decision.RecommendedAction != ActionLabelIssueReady {
		t.Fatalf("action = %q, want %q", decision.RecommendedAction, ActionLabelIssueReady)
	}
	if decision.Target == nil || decision.Target.Issue != 10 {
		t.Fatalf("target = %#v, want issue 10", decision.Target)
	}
	if decision.QueueAnalysis == nil || decision.QueueAnalysis.SelectedCandidate == nil {
		t.Fatalf("queue analysis = %#v, want selected candidate", decision.QueueAnalysis)
	}
	if got := decision.QueueAnalysis.SelectedCandidate.PriorityLabel; !strings.EqualFold(got, "p0") {
		t.Fatalf("priority label = %q, want p0", got)
	}
}

func TestRunOnceDynamicWaveAddsReadyOnlyToBestCandidateAndCleansStale(t *testing.T) {
	cfg := testConfig(t)
	cfg.IssueLabels = []string{"maestro-ready"}
	enableDynamicWave(cfg)
	cfg.Supervisor.DynamicWave.OwnsReadyLabel = true
	cfg.Supervisor.SafeActions = []string{config.SupervisorActionAddReadyLabel}
	reader := &fakeReader{issues: []github.Issue{
		testIssue(10, "stale ready", "maestro-ready", "p3"),
		testIssue(20, "best candidate", "p0"),
	}}

	decision, err := RunOnce(cfg, reader)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	if decision.Status != DecisionStatusSucceeded {
		t.Fatalf("status = %q, want %q", decision.Status, DecisionStatusSucceeded)
	}
	if decision.Target == nil || decision.Target.Issue != 20 {
		t.Fatalf("target = %#v, want issue 20", decision.Target)
	}
	if got, want := strings.Join(reader.addedLabels, ","), "#20:maestro-ready"; got != want {
		t.Fatalf("added labels = %q, want %q", got, want)
	}
	if got, want := strings.Join(reader.removedLabels, ","), "#10:maestro-ready"; got != want {
		t.Fatalf("removed labels = %q, want %q", got, want)
	}
	if len(decision.Mutations) != 2 {
		t.Fatalf("mutations = %#v, want add selected and remove stale", decision.Mutations)
	}
}

// #577 regression: when the most recent dynamic-wave candidate exhausted
// retries without ever producing a PR, the wave must skip that issue and
// select the next eligible candidate so max_parallel=1 does not halt the
// queue waiting for the dead session to reconcile. The retry-exhausted-
// repair-candidate path is suppressed here by signalling the failed issue
// as merged-elsewhere (the scenario from #577: a prior PR landed via
// `Refs #N`), forcing supervisor to fall through to dynamic-wave eval.
func TestDecide_DynamicWaveSkipsNoPRRetryExhaustedAndAdvances(t *testing.T) {
	cfg := testConfig(t)
	cfg.IssueLabels = []string{"maestro-ready"}
	enableDynamicWave(cfg)
	reader := &fakeReader{
		issues: []github.Issue{
			testIssue(488, "already-done candidate", "maestro-ready", "p0"),
			testIssue(490, "next eligible candidate", "maestro-ready", "p1"),
		},
		mergedPRIssues: map[int]bool{488: true},
	}
	st := state.NewState()
	st.Sessions["sup-114"] = &state.Session{
		IssueNumber: 488,
		IssueTitle:  "already-done candidate",
		Branch:      "feat/sup-114",
		Status:      state.StatusRetryExhausted,
		PRNumber:    0,
		StartedAt:   time.Now().UTC().Add(-time.Hour),
	}

	decision, err := testEngine(cfg, reader).Decide(st)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}

	if decision.QueueAnalysis == nil || decision.QueueAnalysis.SelectedCandidate == nil {
		t.Fatalf("queue analysis = %#v, want selected candidate (got action=%q)", decision.QueueAnalysis, decision.RecommendedAction)
	}
	if got := decision.QueueAnalysis.SelectedCandidate.Number; got != 490 {
		t.Fatalf("selected candidate = #%d, want #490 (next eligible after #488 retry_exhausted no-PR)", got)
	}
	foundSkipReason := false
	for _, reason := range decision.QueueAnalysis.SkippedReasons {
		if strings.Contains(reason, "#488") && strings.Contains(reason, "retry limit exhausted") {
			foundSkipReason = true
			break
		}
	}
	if !foundSkipReason {
		t.Fatalf("expected #488 retry_exhausted skip reason in %#v", decision.QueueAnalysis.SkippedReasons)
	}
}

func TestDecide_DynamicWaveClassifiesTitleEpicAsHeld(t *testing.T) {
	cfg := testConfig(t)
	cfg.IssueLabels = []string{"maestro-ready"}
	enableDynamicWave(cfg)
	reader := &fakeReader{issues: []github.Issue{
		testIssue(1, "Epic: parent work", "p0"),
		testIssue(2, "regular work", "p1"),
	}}

	decision, err := testEngine(cfg, reader).Decide(state.NewState())
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}

	if decision.Target == nil || decision.Target.Issue != 2 {
		t.Fatalf("target = %#v, want issue 2", decision.Target)
	}
	if decision.QueueAnalysis == nil || decision.QueueAnalysis.HeldIssues != 1 || decision.QueueAnalysis.ExcludedIssues != 0 {
		t.Fatalf("queue analysis = %#v, want one held issue and zero excluded issues", decision.QueueAnalysis)
	}
	if len(decision.QueueAnalysis.SkippedReasons) == 0 || !strings.Contains(decision.QueueAnalysis.SkippedReasons[0], "title indicates epic") {
		t.Fatalf("skipped reasons = %#v, want title epic reason", decision.QueueAnalysis.SkippedReasons)
	}
}

func TestDecide_DynamicWaveDoesNotBlockChildOnNegatedDependencyProse(t *testing.T) {
	cfg := testConfig(t)
	cfg.IssueLabels = []string{"maestro-ready"}
	cfg.BlockerPatterns = []string{`depends on.*?#(\d+)`}
	enableDynamicWave(cfg)
	epic := testIssue(307, "Epic: parent work", "epic")
	child := testIssue(310, "child work", "maestro-ready")
	child.Body = "Depends on #307 sibling work but is independently mergeable."
	reader := &fakeReader{issues: []github.Issue{epic, child}}

	decision, err := testEngine(cfg, reader).Decide(state.NewState())
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}

	if decision.Target == nil || decision.Target.Issue != 310 {
		t.Fatalf("target = %#v, want child issue 310", decision.Target)
	}
	if decision.QueueAnalysis == nil || decision.QueueAnalysis.BlockedByDependencyIssues != 0 {
		t.Fatalf("queue analysis = %#v, want no dependency-blocked issue", decision.QueueAnalysis)
	}
}

func TestDecide_DynamicWaveDoesNotCountParentEpicAsBlocker(t *testing.T) {
	cfg := testConfig(t)
	cfg.IssueLabels = []string{"maestro-ready"}
	cfg.BlockerPatterns = []string{`blocked by.*?#(\d+)`}
	enableDynamicWave(cfg)
	epic := testIssue(307, "tracking parent", "epic")
	child := testIssue(310, "child work", "maestro-ready")
	child.Body = "Blocked by #307."
	reader := &fakeReader{issues: []github.Issue{epic, child}}

	decision, err := testEngine(cfg, reader).Decide(state.NewState())
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}

	if decision.Target == nil || decision.Target.Issue != 310 {
		t.Fatalf("target = %#v, want child issue 310", decision.Target)
	}
	if decision.QueueAnalysis == nil || decision.QueueAnalysis.BlockedByDependencyIssues != 0 {
		t.Fatalf("queue analysis = %#v, want parent epic excluded from blockers", decision.QueueAnalysis)
	}
}

func TestDecide_DynamicWaveClassifiesAllSkipCategories(t *testing.T) {
	cfg := testConfig(t)
	cfg.BlockerPatterns = []string{`blocked by #(\d+)`}
	enableDynamicWave(cfg)
	reader := &fakeReader{issues: []github.Issue{
		testIssue(1, "excluded", "wontfix"),
		testIssue(2, "mission parent"),
		{Number: 3, Title: "blocked by dependency", Body: "blocked by #100"},
		withProjectStatus(testIssue(4, "already started"), "In Progress"),
	}}
	st := state.NewState()
	st.Missions[2] = &state.Mission{ParentIssue: 2, Status: "active"}

	decision, err := testEngine(cfg, reader).Decide(st)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}

	if decision.RecommendedAction != ActionNone {
		t.Fatalf("action = %q, want %q", decision.RecommendedAction, ActionNone)
	}
	if decision.QueueAnalysis == nil {
		t.Fatal("QueueAnalysis is nil")
	}
	if got, want := decision.QueueAnalysis.ExcludedIssues, 1; got != want {
		t.Fatalf("excluded issues = %d, want %d", got, want)
	}
	if got, want := decision.QueueAnalysis.HeldIssues, 1; got != want {
		t.Fatalf("held issues = %d, want %d", got, want)
	}
	if got, want := decision.QueueAnalysis.BlockedByDependencyIssues, 1; got != want {
		t.Fatalf("blocked-by-dependency issues = %d, want %d", got, want)
	}
	if got, want := decision.QueueAnalysis.NonRunnableProjectStatusCount, 1; got != want {
		t.Fatalf("non-runnable issues = %d, want %d", got, want)
	}
	rationale := strings.Join(decision.Reasons, "\n")
	for _, want := range []string{"1 excluded issue", "1 held/meta issue", "1 blocked-by-dependency issue", "1 issue(s) in non-runnable project status"} {
		if !strings.Contains(rationale, want) {
			t.Fatalf("rationale = %q, want %q", rationale, want)
		}
	}
}

func TestSupervisorQueueAnalysisCountsBlockedPolicyLabelAsExcluded(t *testing.T) {
	analysis := supervisorQueueAnalysis("supervisor.default", 1, nil, []string{
		"Issue #24 skipped: blocked by supervisor policy label",
	})

	if analysis.ExcludedIssues != 1 {
		t.Fatalf("excluded issues = %d, want 1", analysis.ExcludedIssues)
	}
	if got, want := analysis.IdleReason(), "Policy excluded all 1 open issue."; got != want {
		t.Fatalf("IdleReason = %q, want %q", got, want)
	}
}

func TestDecide_DynamicWaveSkipsNonRunnableProjectStatus(t *testing.T) {
	cfg := testConfig(t)
	cfg.IssueLabels = []string{"maestro-ready"}
	enableDynamicWave(cfg)
	reader := &fakeReader{issues: []github.Issue{
		withProjectStatus(testIssue(1, "already started", "p0"), "In Progress"),
		withProjectStatus(testIssue(2, "ready work", "p1"), "Ready"),
	}}

	decision, err := testEngine(cfg, reader).Decide(state.NewState())
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}

	if decision.Target == nil || decision.Target.Issue != 2 {
		t.Fatalf("target = %#v, want issue 2", decision.Target)
	}
	if decision.QueueAnalysis == nil || decision.QueueAnalysis.NonRunnableProjectStatusCount != 1 {
		t.Fatalf("queue analysis = %#v, want one non-runnable project status", decision.QueueAnalysis)
	}
	if len(decision.QueueAnalysis.SkippedReasons) == 0 || !strings.Contains(decision.QueueAnalysis.SkippedReasons[0], "project status") {
		t.Fatalf("skipped reasons = %#v, want project status reason", decision.QueueAnalysis.SkippedReasons)
	}
}

func TestDecide_DynamicWaveSupportsConfiguredRunnableProjectStatus(t *testing.T) {
	cfg := testConfig(t)
	cfg.IssueLabels = []string{"maestro-ready"}
	enableDynamicWave(cfg)
	cfg.Supervisor.DynamicWave.RunnableProjectStatuses = []string{"Selected"}
	reader := &fakeReader{issues: []github.Issue{
		withProjectStatus(testIssue(1, "todo is not configured", "p0"), "Todo"),
		withProjectStatus(testIssue(2, "selected work", "p1"), "Selected"),
	}}

	decision, err := testEngine(cfg, reader).Decide(state.NewState())
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}

	if decision.Target == nil || decision.Target.Issue != 2 {
		t.Fatalf("target = %#v, want issue 2", decision.Target)
	}
	if decision.QueueAnalysis == nil || decision.QueueAnalysis.NonRunnableProjectStatusCount != 1 {
		t.Fatalf("queue analysis = %#v, want one non-runnable project status", decision.QueueAnalysis)
	}
}

func TestDecide_DynamicWaveQueueAnalysisCarriesRankedEligibleAndSkippedCandidates(t *testing.T) {
	cfg := testConfig(t)
	cfg.IssueLabels = []string{"maestro-ready"}
	enableDynamicWave(cfg)
	reader := &fakeReader{issues: []github.Issue{
		testIssue(2, "ready work", "p1"),
		testIssue(1, "urgent work", "p0"),
		testIssue(3, "low work", "p3"),
		testIssue(50, "Epic: umbrella tracking", "p1"),
	}}

	decision, err := testEngine(cfg, reader).Decide(state.NewState())
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	q := decision.QueueAnalysis
	if q == nil {
		t.Fatalf("queue analysis is nil")
	}

	// Eligible set is ranked by priority (P0<P1<P2<P3) then issue number.
	if q.SelectedCandidate == nil || q.SelectedCandidate.Number != 1 {
		t.Fatalf("selected candidate = %#v, want issue #1", q.SelectedCandidate)
	}
	gotOrder := make([]int, 0, len(q.EligibleRanked))
	for _, c := range q.EligibleRanked {
		gotOrder = append(gotOrder, c.Number)
	}
	wantOrder := []int{1, 2, 3}
	if len(gotOrder) != len(wantOrder) {
		t.Fatalf("eligible ranked = %v, want %v", gotOrder, wantOrder)
	}
	for i := range wantOrder {
		if gotOrder[i] != wantOrder[i] {
			t.Fatalf("eligible ranked = %v, want %v", gotOrder, wantOrder)
		}
	}
	if q.EligibleRanked[0].PriorityLabel != "p0" {
		t.Fatalf("eligible ranked[0] priority = %q, want p0", q.EligibleRanked[0].PriorityLabel)
	}

	// The held epic is surfaced as a structured skipped candidate.
	var epic *state.SupervisorSkippedCandidate
	for i := range q.SkippedCandidates {
		if q.SkippedCandidates[i].Number == 50 {
			epic = &q.SkippedCandidates[i]
			break
		}
	}
	if epic == nil {
		t.Fatalf("skipped candidates = %#v, want issue #50", q.SkippedCandidates)
	}
	if epic.Category != string(dynamicSkipHeldMeta) {
		t.Fatalf("skipped #50 category = %q, want %q", epic.Category, dynamicSkipHeldMeta)
	}
	if epic.PriorityLabel != "p1" {
		t.Fatalf("skipped #50 priority = %q, want p1", epic.PriorityLabel)
	}
	if !strings.Contains(strings.ToLower(epic.Reason), "epic") {
		t.Fatalf("skipped #50 reason = %q, want epic reason", epic.Reason)
	}
}

func TestRunOnceLabelsNextIssueReadyAndComments(t *testing.T) {
	cfg := testConfig(t)
	cfg.IssueLabels = []string{"maestro-ready"}
	cfg.Supervisor.SafeActions = []string{config.SupervisorActionAddReadyLabel, config.SupervisorActionAddIssueComment}
	cfg.Supervisor.QueueComments = true
	reader := &fakeReader{issues: []github.Issue{testIssue(308, "implement supervisor")}}

	decision, err := RunOnce(cfg, reader)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	if decision.RecommendedAction != ActionLabelIssueReady {
		t.Fatalf("action = %q, want %q", decision.RecommendedAction, ActionLabelIssueReady)
	}
	if decision.Status != DecisionStatusSucceeded {
		t.Fatalf("status = %q, want %q", decision.Status, DecisionStatusSucceeded)
	}
	if decision.Mode != ModeSafeActions {
		t.Fatalf("mode = %q, want %q", decision.Mode, ModeSafeActions)
	}
	if got, want := strings.Join(reader.addedLabels, ","), "#308:maestro-ready"; got != want {
		t.Fatalf("added labels = %q, want %q", got, want)
	}
	if len(reader.comments) != 1 || !strings.Contains(reader.comments[0], "maestro-ready") {
		t.Fatalf("comments = %#v, want one ready-label comment", reader.comments)
	}
	if len(decision.Mutations) != 2 {
		t.Fatalf("mutations = %#v, want label + comment", decision.Mutations)
	}
	for _, mutation := range decision.Mutations {
		if mutation.Status != MutationStatusSucceeded {
			t.Fatalf("mutation %#v status = %q, want %q", mutation, mutation.Status, MutationStatusSucceeded)
		}
	}

	st, err := state.Load(cfg.StateDir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	latest := st.LatestSupervisorDecision()
	if latest == nil || latest.Status != DecisionStatusSucceeded || len(latest.Mutations) != 2 {
		t.Fatalf("latest decision = %#v, want succeeded decision with mutations", latest)
	}

	second, err := RunOnce(cfg, reader)
	if err != nil {
		t.Fatalf("second RunOnce: %v", err)
	}
	if len(second.Mutations) != 0 {
		t.Fatalf("second mutations = %#v, want none", second.Mutations)
	}
	if len(reader.addedLabels) != 1 || len(reader.comments) != 1 {
		t.Fatalf("added labels = %#v comments = %#v, want no duplicate queue action", reader.addedLabels, reader.comments)
	}
}

func TestRunOnceOrderedQueueRemovesBlockedLabelWhenPolicyAllows(t *testing.T) {
	cfg := testConfig(t)
	cfg.IssueLabels = []string{"maestro-ready"}
	cfg.ExcludeLabels = []string{"blocked"}
	cfg.Supervisor.OrderedQueue = config.SupervisorOrderedQueueConfig{Enabled: true, Issues: []int{42}}
	cfg.Supervisor.SafeActions = []string{config.SupervisorActionRemoveBlockedLabel}
	reader := &fakeReader{issues: []github.Issue{testIssue(42, "was blocked", "maestro-ready", "blocked")}}

	decision, err := RunOnce(cfg, reader)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	if decision.Status != DecisionStatusSucceeded {
		t.Fatalf("status = %q, want %q", decision.Status, DecisionStatusSucceeded)
	}
	if len(reader.addedLabels) != 0 {
		t.Fatalf("added labels = %#v, want none", reader.addedLabels)
	}
	if got, want := strings.Join(reader.removedLabels, ","), "#42:blocked"; got != want {
		t.Fatalf("removed labels = %q, want %q", got, want)
	}
	if len(decision.Mutations) != 1 || decision.Mutations[0].Type != MutationRemoveBlockedLabel {
		t.Fatalf("mutations = %#v, want one blocked-label removal", decision.Mutations)
	}
}

func TestRunOnceOrderedQueueUsesConfiguredSupervisorBlockedLabel(t *testing.T) {
	cfg := testConfig(t)
	cfg.IssueLabels = []string{"maestro-ready"}
	cfg.Supervisor.BlockedLabel = "waiting"
	cfg.Supervisor.OrderedQueue = config.SupervisorOrderedQueueConfig{Enabled: true, Issues: []int{42}}
	cfg.Supervisor.SafeActions = []string{config.SupervisorActionRemoveBlockedLabel}
	reader := &fakeReader{issues: []github.Issue{testIssue(42, "waiting work", "maestro-ready", "waiting")}}

	decision, err := RunOnce(cfg, reader)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	if decision.Status != DecisionStatusSucceeded {
		t.Fatalf("status = %q, want %q", decision.Status, DecisionStatusSucceeded)
	}
	if got, want := strings.Join(reader.removedLabels, ","), "#42:waiting"; got != want {
		t.Fatalf("removed labels = %q, want %q", got, want)
	}
	if len(reader.addedLabels) != 0 {
		t.Fatalf("added labels = %#v, want none", reader.addedLabels)
	}
}

func TestRunOnceDynamicWaveNeverRemovesBlockedLabel(t *testing.T) {
	cfg := testConfig(t)
	cfg.IssueLabels = []string{"maestro-ready"}
	enableDynamicWave(cfg)
	cfg.Supervisor.BlockedLabel = "blocked"
	cfg.Supervisor.SafeActions = []string{config.SupervisorActionAddReadyLabel, config.SupervisorActionRemoveBlockedLabel}
	reader := &fakeReader{issues: []github.Issue{
		testIssue(1, "blocked high priority", "maestro-ready", "blocked", "p0"),
		testIssue(2, "regular", "p1"),
	}}

	decision, err := RunOnce(cfg, reader)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	if decision.Target == nil || decision.Target.Issue != 2 {
		t.Fatalf("target = %#v, want issue 2", decision.Target)
	}
	if got, want := strings.Join(reader.addedLabels, ","), "#2:maestro-ready"; got != want {
		t.Fatalf("added labels = %q, want %q", got, want)
	}
	if len(reader.removedLabels) != 0 {
		t.Fatalf("removed labels = %#v, want no blocked removal in dynamic mode", reader.removedLabels)
	}
}

func TestRunOnceDoesNotRemoveBlockedLabelWithOpenBlocker(t *testing.T) {
	cfg := testConfig(t)
	cfg.IssueLabels = []string{"maestro-ready"}
	cfg.ExcludeLabels = []string{"blocked"}
	cfg.Supervisor.SafeActions = []string{config.SupervisorActionRemoveBlockedLabel}
	cfg.BlockerPatterns = []string{`blocked by #(\d+)`}
	issue := testIssue(42, "blocked work", "maestro-ready", "blocked")
	issue.Body = "blocked by #10"
	reader := &fakeReader{
		issues:       []github.Issue{issue},
		closedIssues: map[int]bool{10: false},
	}

	decision, err := RunOnce(cfg, reader)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	if decision.RecommendedAction != ActionNone {
		t.Fatalf("action = %q, want %q", decision.RecommendedAction, ActionNone)
	}
	if len(reader.removedLabels) != 0 || len(decision.Mutations) != 0 {
		t.Fatalf("removed labels = %#v mutations = %#v, want no mutation", reader.removedLabels, decision.Mutations)
	}
}

func TestRunOnceRunningWorkerDoesNotLabelAtCapacity(t *testing.T) {
	cfg := testConfig(t)
	cfg.IssueLabels = []string{"maestro-ready"}
	cfg.Supervisor.SafeActions = []string{config.SupervisorActionAddReadyLabel}
	st := state.NewState()
	st.Sessions["slot-1"] = &state.Session{
		IssueNumber: 42,
		IssueTitle:  "work in progress",
		Status:      state.StatusRunning,
		StartedAt:   time.Now().UTC(),
	}
	if err := state.Save(cfg.StateDir, st); err != nil {
		t.Fatalf("Save: %v", err)
	}
	reader := &fakeReader{issues: []github.Issue{testIssue(308, "next")}}

	decision, err := RunOnce(cfg, reader)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	if decision.RecommendedAction != ActionWaitForRunningWorker {
		t.Fatalf("action = %q, want %q", decision.RecommendedAction, ActionWaitForRunningWorker)
	}
	if len(reader.addedLabels) != 0 || len(decision.Mutations) != 0 {
		t.Fatalf("added labels = %#v mutations = %#v, want no mutation", reader.addedLabels, decision.Mutations)
	}
	if reader.issueCalls != 0 {
		t.Fatalf("ListOpenIssues called %d time(s), want 0", reader.issueCalls)
	}
}

func TestRunOnceAlreadyReadyDoesNotDuplicateQueueAction(t *testing.T) {
	cfg := testConfig(t)
	cfg.IssueLabels = []string{"maestro-ready"}
	cfg.Supervisor.SafeActions = []string{config.SupervisorActionAddReadyLabel, config.SupervisorActionAddIssueComment}
	reader := &fakeReader{issues: []github.Issue{testIssue(42, "ready work", "maestro-ready")}}

	decision, err := RunOnce(cfg, reader)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	if decision.RecommendedAction != ActionSpawnWorker {
		t.Fatalf("action = %q, want %q", decision.RecommendedAction, ActionSpawnWorker)
	}
	if len(reader.addedLabels) != 0 || len(reader.comments) != 0 || len(decision.Mutations) != 0 {
		t.Fatalf("labels = %#v comments = %#v mutations = %#v, want no queue mutation", reader.addedLabels, reader.comments, decision.Mutations)
	}
}

func TestRunOnceGitHubFailureRecordsFailedMutation(t *testing.T) {
	cfg := testConfig(t)
	cfg.IssueLabels = []string{"maestro-ready"}
	cfg.Supervisor.SafeActions = []string{config.SupervisorActionAddReadyLabel}
	reader := &fakeReader{
		issues:      []github.Issue{testIssue(308, "implement supervisor")},
		addLabelErr: errors.New("boom"),
	}

	decision, err := RunOnce(cfg, reader)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	if decision.Status != DecisionStatusFailed {
		t.Fatalf("status = %q, want %q", decision.Status, DecisionStatusFailed)
	}
	if decision.ErrorClass != ErrorClassGitHubAPI {
		t.Fatalf("error class = %q, want %q", decision.ErrorClass, ErrorClassGitHubAPI)
	}
	if len(decision.Mutations) != 1 || decision.Mutations[0].Status != MutationStatusFailed || decision.Mutations[0].ErrorClass != ErrorClassGitHubAPI {
		t.Fatalf("mutations = %#v, want failed github_api mutation", decision.Mutations)
	}

	st, err := state.Load(cfg.StateDir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	latest := st.LatestSupervisorDecision()
	if latest == nil || latest.Status != DecisionStatusFailed || latest.ErrorClass != ErrorClassGitHubAPI {
		t.Fatalf("latest decision = %#v, want failed github_api decision", latest)
	}
}

func TestDecide_OrderedQueueSelectsFirstUnfinishedIssue(t *testing.T) {
	cfg := testConfig(t)
	cfg.IssueLabels = []string{"maestro-ready"}
	cfg.Supervisor.OrderedQueue = config.SupervisorOrderedQueueConfig{
		Enabled: true,
		Issues:  []int{308, 306},
	}
	reader := &fakeReader{
		issues:       []github.Issue{testIssue(306, "second", "maestro-ready")},
		closedIssues: map[int]bool{308: true},
	}

	decision, err := testEngine(cfg, reader).Decide(state.NewState())
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}

	if decision.RecommendedAction != ActionSpawnWorker {
		t.Fatalf("action = %q, want %q", decision.RecommendedAction, ActionSpawnWorker)
	}
	if decision.Target == nil || decision.Target.Issue != 306 {
		t.Fatalf("target = %#v, want issue 306", decision.Target)
	}
}

func TestOrderedQueueIssueDone_ClosedIssueWaitsForOutcomeWhenRequired(t *testing.T) {
	cfg := testConfig(t)
	cfg.Outcome = outcome.Brief{
		DesiredOutcome:      "Live app works",
		VerifierCommand:     "check-live",
		PassRequiredForDone: boolPtr(true),
	}
	reader := &fakeReader{closedIssues: map[int]bool{308: true}}
	st := state.NewState()
	st.OutcomeHealth = &outcome.HealthCheckResult{
		CheckedAt: time.Now().UTC(),
		State:     outcome.HealthFailing,
		Signal:    "healthcheck_command",
		Summary:   "live verifier failed",
	}

	done, reason, err := testEngine(cfg, reader).orderedQueueIssueDone(st, 308)
	if err != nil {
		t.Fatalf("orderedQueueIssueDone: %v", err)
	}
	if done {
		t.Fatalf("done = true, want false while outcome is failing")
	}
	if !strings.Contains(reason, "outcome health is not verified") {
		t.Fatalf("reason = %q, want outcome gate reason", reason)
	}
}

func TestOrderedQueueIssueDone_MergedPRWaitsForOutcomeWhenRequired(t *testing.T) {
	cfg := testConfig(t)
	cfg.Outcome = outcome.Brief{
		DesiredOutcome:      "Live app works",
		VerifierCommand:     "check-live",
		PassRequiredForDone: boolPtr(true),
	}
	reader := &fakeReader{mergedPRIssues: map[int]bool{308: true}}
	st := state.NewState()
	st.LastMergeAt = time.Now().UTC().Add(-time.Minute)
	st.OutcomeHealth = &outcome.HealthCheckResult{
		CheckedAt: time.Now().UTC(),
		State:     outcome.HealthFailing,
		Signal:    "healthcheck_command",
		Summary:   "live verifier failed",
	}

	done, reason, err := testEngine(cfg, reader).orderedQueueIssueDone(st, 308)
	if err != nil {
		t.Fatalf("orderedQueueIssueDone: %v", err)
	}
	if done {
		t.Fatalf("done = true, want false while outcome is failing")
	}
	if !strings.Contains(reason, "outcome health is not verified") {
		t.Fatalf("reason = %q, want outcome gate reason", reason)
	}
}

func TestOrderedQueueIssueDone_MergedPRAllowedAfterOutcomePass(t *testing.T) {
	cfg := testConfig(t)
	cfg.Outcome = outcome.Brief{
		DesiredOutcome:      "Live app works",
		VerifierCommand:     "check-live",
		PassRequiredForDone: boolPtr(true),
	}
	reader := &fakeReader{mergedPRIssues: map[int]bool{308: true}}
	st := state.NewState()
	st.LastMergeAt = time.Now().UTC().Add(-time.Minute)
	st.OutcomeHealth = &outcome.HealthCheckResult{
		CheckedAt: time.Now().UTC(),
		State:     outcome.HealthHealthy,
		Signal:    "healthcheck_command",
		Summary:   "live verifier passed",
	}

	done, reason, err := testEngine(cfg, reader).orderedQueueIssueDone(st, 308)
	if err != nil {
		t.Fatalf("orderedQueueIssueDone: %v", err)
	}
	if !done {
		t.Fatalf("done = false, want true after outcome pass")
	}
	if reason != "linked PR merged" {
		t.Fatalf("reason = %q, want linked PR merged", reason)
	}
}

func TestDynamicWaveDoneStateDoesNotSkipWhenOutcomeNotVerified(t *testing.T) {
	cfg := testConfig(t)
	cfg.Outcome = outcome.Brief{
		DesiredOutcome:      "Live app works",
		VerifierCommand:     "check-live",
		PassRequiredForDone: boolPtr(true),
	}
	st := state.NewState()
	st.Sessions["slot-1"] = &state.Session{IssueNumber: 42, Status: state.StatusDone, PRNumber: 12}
	st.OutcomeHealth = &outcome.HealthCheckResult{
		CheckedAt: time.Now().UTC(),
		State:     outcome.HealthFailing,
		Signal:    "healthcheck_command",
		Summary:   "live verifier failed",
	}

	issue := testIssue(42, "done issue", "maestro-ready")
	reason, _, err := testEngine(cfg, &fakeReader{}).dynamicWaveSkipReason(st, issue, []github.Issue{issue})
	if err != nil {
		t.Fatalf("dynamicWaveSkipReason: %v", err)
	}
	if reason != "" {
		t.Fatalf("reason = %q, want no done-state skip until outcome passes", reason)
	}
}

func TestDecide_OrderedQueueDoesNotLabelNextIssueWhileCurrentHasOpenPR(t *testing.T) {
	cfg := testConfig(t)
	cfg.IssueLabels = []string{"maestro-ready"}
	cfg.Supervisor.OrderedQueue = config.SupervisorOrderedQueueConfig{
		Enabled: true,
		Issues:  []int{308, 306},
	}
	reader := &fakeReader{
		issues:       []github.Issue{testIssue(308, "current"), testIssue(306, "next")},
		openPRIssues: map[int]bool{308: true},
	}

	decision, err := testEngine(cfg, reader).Decide(state.NewState())
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}

	if decision.RecommendedAction != ActionMonitorOpenPR {
		t.Fatalf("action = %q, want %q", decision.RecommendedAction, ActionMonitorOpenPR)
	}
	if decision.Target == nil || decision.Target.Issue != 308 {
		t.Fatalf("target = %#v, want issue 308", decision.Target)
	}
}

func TestDecide_OrderedQueuePausesOnBlockedIssue(t *testing.T) {
	cfg := testConfig(t)
	cfg.IssueLabels = []string{"maestro-ready"}
	cfg.BlockerPatterns = []string{`blocked by #(\d+)`}
	cfg.Supervisor.OrderedQueue = config.SupervisorOrderedQueueConfig{
		Enabled: true,
		Issues:  []int{308, 306},
	}
	reader := &fakeReader{
		issues: []github.Issue{
			{Number: 308, Title: "blocked", Body: "blocked by #100", Labels: []struct {
				Name string `json:"name"`
			}{{Name: "maestro-ready"}}},
			testIssue(306, "next", "maestro-ready"),
		},
		closedIssues: map[int]bool{100: false},
	}

	decision, err := testEngine(cfg, reader).Decide(state.NewState())
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}

	if decision.RecommendedAction != ActionWaitForOrderedQueue {
		t.Fatalf("action = %q, want %q", decision.RecommendedAction, ActionWaitForOrderedQueue)
	}
	if decision.Target == nil || decision.Target.Issue != 308 {
		t.Fatalf("target = %#v, want issue 308", decision.Target)
	}
	if !strings.Contains(decision.Summary, "blocked") {
		t.Fatalf("summary %q should explain blocked queue", decision.Summary)
	}
}

func TestDecide_OrderedQueuePausesOnRetryExhaustedIssue(t *testing.T) {
	cfg := testConfig(t)
	cfg.IssueLabels = []string{"maestro-ready"}
	cfg.MaxRetriesPerIssue = 2
	cfg.Supervisor.OrderedQueue = config.SupervisorOrderedQueueConfig{
		Enabled: true,
		Issues:  []int{308, 306},
	}
	reader := &fakeReader{
		issues: []github.Issue{testIssue(308, "flaky", "maestro-ready"), testIssue(306, "next", "maestro-ready")},
	}
	st := state.NewState()
	for i := 0; i < 2; i++ {
		finished := time.Now().UTC().Add(-time.Duration(i+1) * time.Hour)
		st.Sessions[time.Duration(i).String()] = &state.Session{
			IssueNumber: 308,
			Status:      state.StatusDead,
			FinishedAt:  &finished,
		}
	}

	decision, err := testEngine(cfg, reader).Decide(st)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}

	if decision.RecommendedAction != ActionReviewRetryExhausted {
		t.Fatalf("action = %q, want %q", decision.RecommendedAction, ActionReviewRetryExhausted)
	}
	if decision.Target == nil || decision.Target.Issue != 308 {
		t.Fatalf("target = %#v, want issue 308", decision.Target)
	}
}

func TestDecide_OrderedQueueAdvancesAfterMergedPR(t *testing.T) {
	cfg := testConfig(t)
	cfg.IssueLabels = []string{"maestro-ready"}
	cfg.Supervisor.OrderedQueue = config.SupervisorOrderedQueueConfig{
		Enabled: true,
		Issues:  []int{308, 306},
	}
	reader := &fakeReader{
		issues:         []github.Issue{testIssue(306, "next", "maestro-ready")},
		mergedPRIssues: map[int]bool{308: true},
	}

	decision, err := testEngine(cfg, reader).Decide(state.NewState())
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}

	if decision.RecommendedAction != ActionSpawnWorker {
		t.Fatalf("action = %q, want %q", decision.RecommendedAction, ActionSpawnWorker)
	}
	if decision.Target == nil || decision.Target.Issue != 306 {
		t.Fatalf("target = %#v, want issue 306", decision.Target)
	}
}

func TestDecide_OrderedQueueAdvancesAfterDoneSessionWithMergedPR(t *testing.T) {
	cfg := testConfig(t)
	cfg.IssueLabels = []string{"maestro-ready"}
	cfg.Supervisor.OrderedQueue = config.SupervisorOrderedQueueConfig{
		Enabled: true,
		Issues:  []int{308, 306},
	}
	reader := &fakeReader{
		issues:    []github.Issue{testIssue(306, "next", "maestro-ready")},
		mergedPRs: map[int]bool{77: true},
	}
	st := state.NewState()
	st.Sessions["slot-1"] = &state.Session{IssueNumber: 308, Status: state.StatusDone, PRNumber: 77}

	decision, err := testEngine(cfg, reader).Decide(st)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}

	if decision.RecommendedAction != ActionSpawnWorker {
		t.Fatalf("action = %q, want %q", decision.RecommendedAction, ActionSpawnWorker)
	}
	if decision.Target == nil || decision.Target.Issue != 306 {
		t.Fatalf("target = %#v, want issue 306", decision.Target)
	}
}

func TestDecide_OrderedQueueAdvancesAfterCodeLandedSessionWithMergedPR(t *testing.T) {
	cfg := testConfig(t)
	cfg.IssueLabels = []string{"maestro-ready"}
	cfg.Supervisor.OrderedQueue = config.SupervisorOrderedQueueConfig{
		Enabled: true,
		Issues:  []int{308, 306},
	}
	reader := &fakeReader{
		issues:    []github.Issue{testIssue(306, "next", "maestro-ready")},
		mergedPRs: map[int]bool{77: true},
	}
	st := state.NewState()
	st.Sessions["slot-1"] = &state.Session{IssueNumber: 308, Status: state.StatusCodeLanded, PRNumber: 77}

	decision, err := testEngine(cfg, reader).Decide(st)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}

	if decision.RecommendedAction != ActionSpawnWorker {
		t.Fatalf("action = %q, want %q", decision.RecommendedAction, ActionSpawnWorker)
	}
	if decision.Target == nil || decision.Target.Issue != 306 {
		t.Fatalf("target = %#v, want issue 306", decision.Target)
	}
}

func TestDecide_OrderedQueueAdvancesAfterPolicyOverride(t *testing.T) {
	cfg := testConfig(t)
	cfg.IssueLabels = []string{"maestro-ready"}
	cfg.Supervisor.OrderedQueue = config.SupervisorOrderedQueueConfig{
		Enabled:    true,
		Issues:     []int{308, 306},
		DoneIssues: []int{308},
	}
	reader := &fakeReader{issues: []github.Issue{testIssue(306, "next", "maestro-ready")}}

	decision, err := testEngine(cfg, reader).Decide(state.NewState())
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}

	if decision.RecommendedAction != ActionSpawnWorker {
		t.Fatalf("action = %q, want %q", decision.RecommendedAction, ActionSpawnWorker)
	}
	if decision.Target == nil || decision.Target.Issue != 306 {
		t.Fatalf("target = %#v, want issue 306", decision.Target)
	}
}

func TestRunOnceDryRunDoesNotRecordDecision(t *testing.T) {
	cfg := testConfig(t)
	cfg.Supervisor.DryRun = true
	reader := &fakeReader{}

	if _, err := RunOnce(cfg, reader); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	st, err := state.Load(cfg.StateDir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if latest := st.LatestSupervisorDecision(); latest != nil {
		t.Fatalf("latest supervisor decision = %#v, want nil for dry run", latest)
	}
}

func TestDecideWithLLM_ValidDecision(t *testing.T) {
	cfg := testConfig(t)
	cfg.IssueLabels = []string{"maestro-ready"}
	issue := testIssue(42, "ready work", "maestro-ready")
	issue.Body = "Implement this. SERVICE_TOKEN=redact-me"
	reader := &fakeReader{issues: []github.Issue{issue}}
	llm := &fakeLLM{output: `{
  "summary": "Issue #42 is ready to feed; no worker is running.",
  "recommended_action": "spawn_worker",
  "target": {"issue": 42},
  "risk": "mutating",
  "confidence": 0.87,
  "reasons": ["ordered queue points to #42", "no active worker"],
  "requires_approval": true
}`}
	st := state.NewState()
	logPath := filepath.Join(t.TempDir(), "worker.log")
	if err := os.WriteFile(logPath, []byte("Authorization: redact-me\nAPI_KEY=redact-me\n"), 0644); err != nil {
		t.Fatal(err)
	}
	st.Sessions["slot-dead"] = &state.Session{
		IssueNumber: 99,
		IssueTitle:  "previous failure",
		Status:      state.StatusDead,
		LogFile:     logPath,
		StartedAt:   time.Now().UTC().Add(-time.Hour),
	}

	decision, err := testLLMEngine(cfg, reader, llm).Decide(st)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}

	if llm.calls != 1 {
		t.Fatalf("LLM calls = %d, want 1", llm.calls)
	}
	if decision.RecommendedAction != ActionSpawnWorker {
		t.Fatalf("action = %q, want %q", decision.RecommendedAction, ActionSpawnWorker)
	}
	if decision.Summary != "Issue #42 is ready to feed; no worker is running." {
		t.Fatalf("summary = %q", decision.Summary)
	}
	if !decision.RequiresApproval {
		t.Fatal("RequiresApproval = false, want true")
	}
	if decision.Target == nil || decision.Target.Issue != 42 {
		t.Fatalf("target = %#v, want issue 42", decision.Target)
	}
	for _, secret := range []string{"SERVICE_TOKEN=redact-me", "Authorization: redact-me", "API_KEY=redact-me"} {
		if strings.Contains(llm.prompt, secret) {
			t.Fatalf("prompt contained unredacted secret %q", secret)
		}
	}
	if !strings.Contains(llm.prompt, "ordered_queue_state") || !strings.Contains(llm.prompt, "[REDACTED") {
		t.Fatalf("prompt did not include expected redacted state packet: %s", llm.prompt)
	}
}

func TestDecideWithLLM_UnknownActionRejected(t *testing.T) {
	cfg := testConfig(t)
	cfg.IssueLabels = []string{"maestro-ready"}
	reader := &fakeReader{issues: []github.Issue{testIssue(42, "ready work", "maestro-ready")}}
	llm := &fakeLLM{output: `{
  "summary": "Delete the repo.",
  "recommended_action": "delete_repo",
  "target": {"issue": 42},
  "risk": "mutating",
  "confidence": 0.9,
  "reasons": ["not allowed"],
  "requires_approval": true
}`}

	_, err := testLLMEngine(cfg, reader, llm).Decide(state.NewState())
	if err == nil || !strings.Contains(err.Error(), "not allowed") {
		t.Fatalf("Decide error = %v, want not allowed", err)
	}
}

func TestDecideWithLLM_ApprovalRequiredActionRejectedWithoutApproval(t *testing.T) {
	cfg := testConfig(t)
	cfg.IssueLabels = []string{"maestro-ready"}
	reader := &fakeReader{issues: []github.Issue{testIssue(42, "ready work", "maestro-ready")}}
	llm := &fakeLLM{output: `{
  "summary": "Issue #42 is ready to feed.",
  "recommended_action": "spawn_worker",
  "target": {"issue": 42},
  "risk": "mutating",
  "confidence": 0.87,
  "reasons": ["ordered queue points to #42"],
  "requires_approval": false
}`}

	_, err := testLLMEngine(cfg, reader, llm).Decide(state.NewState())
	if err == nil || !strings.Contains(err.Error(), "requires approval") {
		t.Fatalf("Decide error = %v, want requires approval", err)
	}
}

func TestDecideWithLLM_MalformedOutputRejected(t *testing.T) {
	cfg := testConfig(t)
	cfg.IssueLabels = []string{"maestro-ready"}
	reader := &fakeReader{issues: []github.Issue{testIssue(42, "ready work", "maestro-ready")}}
	llm := &fakeLLM{output: `not json`}

	_, err := testLLMEngine(cfg, reader, llm).Decide(state.NewState())
	if err == nil || !strings.Contains(err.Error(), "invalid JSON contract") {
		t.Fatalf("Decide error = %v, want invalid JSON contract", err)
	}
}

// #689: an LLM-vs-guardrail disagreement is a decision-layer condition, not
// a process fault. When the guardrail side is risk=safe, the deterministic
// decision wins the tie-break and the cycle succeeds with a
// guardrail_conflict stuck state instead of an error (which used to exit
// rc=1 and put systemd in a crash-loop).
//
// #837: wait_for_running_worker is a pure safe, mutation-free decision, which
// now short-circuits the LLM. always_consult_llm=true keeps the LLM in the loop
// so this conflict-resolution path is still exercised.
func TestDecideWithLLM_DetectorDisagreementResolvesToDeterministicSafeSide(t *testing.T) {
	cfg := testConfig(t)
	cfg.Supervisor.AlwaysConsultLLM = true
	reader := &fakeReader{issues: []github.Issue{testIssue(42, "ready work")}}
	llm := &fakeLLM{output: `{
  "summary": "Start a new worker anyway.",
  "recommended_action": "spawn_worker",
  "target": {"issue": 42},
  "risk": "mutating",
  "confidence": 0.87,
  "reasons": ["LLM wants more work"],
  "requires_approval": true
}`}
	st := state.NewState()
	st.Sessions["slot-1"] = &state.Session{
		IssueNumber: 77,
		IssueTitle:  "already running",
		Status:      state.StatusRunning,
		StartedAt:   time.Now().UTC(),
	}

	decision, err := testLLMEngine(cfg, reader, llm).Decide(st)
	if err != nil {
		t.Fatalf("Decide: %v (disagreement must resolve, not fail the cycle — #689)", err)
	}
	if decision.RecommendedAction != ActionWaitForRunningWorker {
		t.Fatalf("action = %q, want deterministic %q (safe guardrail side wins the tie-break)", decision.RecommendedAction, ActionWaitForRunningWorker)
	}
	if decision.Risk != RiskSafe {
		t.Fatalf("risk = %q, want safe", decision.Risk)
	}
	stuck := requireStuckState(t, decision, state.StuckGuardrailConflict)
	if stuck.Severity != SeverityWarning {
		t.Errorf("severity = %q, want warning", stuck.Severity)
	}
	if !strings.Contains(stuck.Summary, "disagrees with deterministic guardrail") {
		t.Errorf("summary = %q, want it to name the disagreement", stuck.Summary)
	}
}

func TestDecideWithLLM_AddReadyLabelAliasAccepted(t *testing.T) {
	cfg := testConfig(t)
	cfg.IssueLabels = []string{"maestro-ready"}
	reader := &fakeReader{issues: []github.Issue{testIssue(308, "needs label")}}
	llm := &fakeLLM{output: `{
  "summary": "Issue #308 is ready to label.",
  "recommended_action": "add_ready_label",
  "target": {"issue": 308},
  "risk": "mutating",
  "confidence": 0.82,
  "reasons": ["no eligible issue has the configured ready label"],
  "requires_approval": true
}`}

	decision, err := testLLMEngine(cfg, reader, llm).Decide(state.NewState())
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if decision.RecommendedAction != ActionLabelIssueReady {
		t.Fatalf("action = %q, want canonical %q", decision.RecommendedAction, ActionLabelIssueReady)
	}
}

// #430: a spawn_worker recommendation that the cautious gate will mint a
// pending approval for must report decision.RequiresApproval=true in
// `maestro supervise --once --json`. Before the fix the field was wired
// to `risk == RiskApprovalGated` only, so a RiskMutating decision (the
// common case for spawn_worker) reported requires_approval=false while
// the supervisor silently minted a pending approval. The operator then
// saw "Approval ... approved. No risky action was executed." with no
// indication the verb needed approval at all. RunOnce must reconcile
// the JSON flag with the actual mint predicate (decisionRequiresApproval).
func TestRunOnce_SpawnWorker_RecommendedActionRequiresApprovalReflectsGate(t *testing.T) {
	cfg := testConfig(t)
	cfg.IssueLabels = []string{"maestro-ready"}
	cfg.Supervisor.ApprovalRequiredActions = []string{
		ActionSpawnWorker,
		ActionLabelIssueReady,
	}
	reader := &fakeReader{issues: []github.Issue{testIssue(119, "fix CI flake", "maestro-ready")}}

	decision, err := RunOnce(cfg, reader)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	if decision.RecommendedAction != ActionSpawnWorker {
		t.Fatalf("action = %q, want %q", decision.RecommendedAction, ActionSpawnWorker)
	}
	if decision.Status != DecisionStatusRecommended {
		t.Fatalf("status = %q, want %q (action must not be reported as executed)", decision.Status, DecisionStatusRecommended)
	}
	if !decision.RequiresApproval {
		t.Fatal("RequiresApproval = false, want true — JSON would otherwise claim no approval is needed while supervisor silently mints a pending approval (#430)")
	}
	if decision.ApprovalID == "" {
		t.Fatal("ApprovalID is empty, want a minted approval id so the operator can address it")
	}

	st, err := state.Load(cfg.StateDir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(st.Approvals) != 1 {
		t.Fatalf("approvals = %d, want 1 pending approval", len(st.Approvals))
	}
	a := st.Approvals[0]
	if a.Action != ActionSpawnWorker {
		t.Fatalf("approval action = %q, want %q", a.Action, ActionSpawnWorker)
	}
	if a.Status != state.ApprovalStatusPending {
		t.Fatalf("approval status = %q, want %q", a.Status, state.ApprovalStatusPending)
	}
	if a.Target == nil || a.Target.Issue != 119 {
		t.Fatalf("approval target = %#v, want issue 119", a.Target)
	}
}

// #430: when ApprovalRequiredActions explicitly excludes spawn_worker (the
// operator opted into autonomous execution of spawn_worker), the supervisor
// must NOT report requires_approval=true. The flag tracks reality: an
// autonomous spawn_worker (still recommended-only inside the supervisor —
// the orchestrator dispatcher owns the actual worker.Start) must surface
// as a non-approval-gated action so the operator does not chase a phantom
// approval.
func TestRunOnce_SpawnWorker_AutonomousModeReportsNoApprovalRequired(t *testing.T) {
	cfg := testConfig(t)
	cfg.IssueLabels = []string{"maestro-ready"}
	// Empty-but-non-nil list: the operator has explicitly listed which
	// actions require approval, and spawn_worker is intentionally absent.
	cfg.Supervisor.ApprovalRequiredActions = []string{ActionMergePR}
	reader := &fakeReader{issues: []github.Issue{testIssue(119, "fix CI flake", "maestro-ready")}}

	decision, err := RunOnce(cfg, reader)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	if decision.RecommendedAction != ActionSpawnWorker {
		t.Fatalf("action = %q, want %q", decision.RecommendedAction, ActionSpawnWorker)
	}
	if decision.RequiresApproval {
		t.Fatal("RequiresApproval = true, want false when spawn_worker is not in ApprovalRequiredActions")
	}

	st, err := state.Load(cfg.StateDir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	for _, a := range st.Approvals {
		if a.Action == ActionSpawnWorker {
			t.Fatalf("approval minted for spawn_worker = %#v, want none when not gated", a)
		}
	}
}
