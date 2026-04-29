package supervisor

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/github"
	"github.com/befeast/maestro/internal/state"
)

type fakeReader struct {
	issues         []github.Issue
	prs            []github.PR
	openPRIssues   map[int]bool
	closedIssues   map[int]bool
	issueCalls     int
	addedLabels    []string
	removedLabels  []string
	comments       []string
	addLabelErr    error
	removeLabelErr error
	commentErr     error
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

func (f *fakeReader) IsIssueClosed(number int) (bool, error) {
	return f.closedIssues[number], nil
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

func testConfig(t *testing.T) *config.Config {
	t.Helper()
	return &config.Config{
		Repo:               "owner/repo",
		StateDir:           t.TempDir(),
		MaxParallel:        1,
		MaxRetriesPerIssue: 3,
	}
}

func testEngine(cfg *config.Config, reader *fakeReader) *Engine {
	eng := NewEngine(cfg, reader)
	eng.now = func() time.Time { return time.Date(2026, 4, 29, 12, 0, 0, 0, time.UTC) }
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

func TestDecide_IdleNoEligibleIssueRecommendsLabel(t *testing.T) {
	cfg := testConfig(t)
	cfg.IssueLabels = []string{"maestro-ready"}
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
}

func TestDecide_RunningWorkerWaits(t *testing.T) {
	cfg := testConfig(t)
	reader := &fakeReader{}
	st := state.NewState()
	st.Sessions["slot-1"] = &state.Session{
		IssueNumber: 42,
		IssueTitle:  "work in progress",
		Status:      state.StatusRunning,
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
}

func TestDecide_OrderedQueueSelectsFirstUnfinishedIssue(t *testing.T) {
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
	reader := &fakeReader{issues: []github.Issue{
		testIssue(306, "second wave", "maestro-ready"),
		testIssue(308, "done wave", "maestro-ready"),
	}}
	st := state.NewState()
	st.Sessions["slot-1"] = &state.Session{IssueNumber: 308, Status: state.StatusDone, StartedAt: time.Now().UTC()}

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

func TestRunOnceRemovesBlockedLabelWhenPolicyAllows(t *testing.T) {
	cfg := testConfig(t)
	cfg.IssueLabels = []string{"maestro-ready"}
	cfg.ExcludeLabels = []string{"blocked"}
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

func TestRunOnceUsesConfiguredSupervisorBlockedLabel(t *testing.T) {
	cfg := testConfig(t)
	cfg.IssueLabels = []string{"maestro-ready"}
	cfg.Supervisor.BlockedLabel = "waiting"
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
