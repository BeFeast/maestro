package supervisor

import (
	"testing"
	"time"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/github"
	"github.com/befeast/maestro/internal/state"
)

type fakeReader struct {
	issues       []github.Issue
	prs          []github.PR
	openPRIssues map[int]bool
	closedIssues map[int]bool
	issueCalls   int
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
