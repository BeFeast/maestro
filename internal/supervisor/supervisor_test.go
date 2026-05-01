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
	issues         []github.Issue
	prs            []github.PR
	openPRIssues   map[int]bool
	mergedPRIssues map[int]bool
	closedIssues   map[int]bool
	mergedPRs      map[int]bool
	ciStatuses     map[int]string
	greptileOK     map[int]bool
	greptilePend   map[int]bool
	issueCalls     int
	addedLabels    []string
	removedLabels  []string
	comments       []string
	addLabelErr    error
	removeLabelErr error
	commentErr     error
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
	return f.mergedPRIssues[issueNumber], nil
}

func (f *fakeReader) IsIssueClosed(number int) (bool, error) {
	return f.closedIssues[number], nil
}

func (f *fakeReader) IsPRMerged(prNumber int) (bool, error) {
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
	reader := &fakeReader{}
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
	reader := &fakeReader{}
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

func TestDecide_RetryExhaustedOpenGreenPRExplainsMergeEligibility(t *testing.T) {
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

	if decision.RecommendedAction != ActionMonitorOpenPR {
		t.Fatalf("action = %q, want %q", decision.RecommendedAction, ActionMonitorOpenPR)
	}
	if !strings.Contains(strings.ToLower(decision.Summary), "retry exhausted") || !strings.Contains(strings.ToLower(decision.Summary), "merge") {
		t.Fatalf("summary = %q, want retry exhausted merge eligibility", decision.Summary)
	}
	stuck := requireStuckState(t, decision, "retry_exhausted_open_pr")
	if stuck.Severity != SeverityInfo {
		t.Errorf("severity = %q, want %q", stuck.Severity, SeverityInfo)
	}
	if !strings.Contains(strings.Join(stuck.Evidence, "\n"), "checks=success") {
		t.Fatalf("evidence = %#v, want checks=success", stuck.Evidence)
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

			findings := testEngine(testConfig(t), tt.reader).detectWorkerStuckStates(st, now)

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

func TestDecideWithLLM_DetectorDisagreementRejected(t *testing.T) {
	cfg := testConfig(t)
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

	_, err := testLLMEngine(cfg, reader, llm).Decide(st)
	if err == nil || !strings.Contains(err.Error(), "disagrees with deterministic guardrail") {
		t.Fatalf("Decide error = %v, want detector disagreement", err)
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
