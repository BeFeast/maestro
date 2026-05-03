package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/outcome"
	"github.com/befeast/maestro/internal/server/web"
	"github.com/befeast/maestro/internal/state"
)

func TestLoadFleetProjects(t *testing.T) {
	dir := t.TempDir()
	stateDir := filepath.Join(dir, "state")
	configPath := filepath.Join(dir, "project.yaml")
	if err := os.WriteFile(configPath, []byte("repo: owner/project\nstate_dir: "+stateDir+"\nsession_prefix: prj\n"), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	fleetPath := filepath.Join(dir, "fleet.yaml")
	if err := os.WriteFile(fleetPath, []byte("projects:\n  - name: Project\n    config: project.yaml\n    dashboard_url: http://127.0.0.1:8787\n"), 0o644); err != nil {
		t.Fatalf("write fleet: %v", err)
	}

	projects, err := LoadFleetProjects(fleetPath)
	if err != nil {
		t.Fatalf("LoadFleetProjects failed: %v", err)
	}
	if len(projects) != 1 {
		t.Fatalf("projects len = %d, want 1", len(projects))
	}
	if projects[0].Name != "Project" {
		t.Fatalf("project name = %q", projects[0].Name)
	}
	if projects[0].cfg == nil || projects[0].cfg.Repo != "owner/project" {
		t.Fatalf("resolved config = %+v", projects[0].cfg)
	}
	if projects[0].DashboardURL != "http://127.0.0.1:8787" {
		t.Fatalf("dashboard url = %q", projects[0].DashboardURL)
	}
}

func TestFleetAPIAggregatesProjects(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()
	firstStateDir := filepath.Join(dir, "one")
	secondStateDir := filepath.Join(dir, "two")
	finishedOne := now.Add(-20 * time.Hour)
	startedDoneOne := finishedOne.Add(-2 * time.Hour)
	finishedTwo := now.Add(-48 * time.Hour)
	startedDoneTwo := finishedTwo.Add(-3 * time.Hour)
	saveFleetTestState(t, firstStateDir, map[string]*state.Session{
		"one-1": {
			IssueNumber:     1,
			IssueTitle:      "Build thing",
			Status:          state.StatusRunning,
			StartedAt:       now.Add(-time.Minute),
			Backend:         "opencode",
			TokensUsedTotal: 1234,
		},
		"one-2": {
			IssueNumber:     2,
			IssueTitle:      "Review thing",
			Status:          state.StatusDone,
			StartedAt:       startedDoneOne,
			FinishedAt:      &finishedOne,
			PRNumber:        12,
			TokensUsedTotal: 42000,
		},
	})
	saveFleetTestState(t, secondStateDir, map[string]*state.Session{
		"two-1": {
			IssueNumber:     3,
			IssueTitle:      "Broken thing",
			Status:          state.StatusRetryExhausted,
			StartedAt:       now.Add(-3 * time.Minute),
			PRNumber:        31,
			CIFailureOutput: "tests failed",
		},
		"two-2": {
			IssueNumber: 4,
			IssueTitle:  "Merged thing",
			Status:      state.StatusDone,
			StartedAt:   startedDoneTwo,
			FinishedAt:  &finishedTwo,
			PRNumber:    44,
		},
	})

	projects := []FleetProject{
		NewFleetProject("One", "/tmp/one.yaml", "http://127.0.0.1:8787", &config.Config{
			Repo: "owner/one",
			Outcome: outcome.Brief{
				DesiredOutcome: "One is deployed",
				RuntimeTarget:  "https://one.example.com",
				HealthcheckURL: "https://one.example.com/healthz",
			},
			StateDir:    firstStateDir,
			MaxParallel: 2,
			Server:      config.ServerConfig{ReadOnly: true},
		}),
		NewFleetProject("Two", "/tmp/two.yaml", "", &config.Config{
			Repo:        "owner/two",
			StateDir:    secondStateDir,
			MaxParallel: 1,
		}),
	}
	srv := NewFleet(projects, "127.0.0.1", 8786, true)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/fleet", nil)
	w := httptest.NewRecorder()
	srv.handleFleet(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}
	var resp fleetResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Summary.Projects != 2 || resp.Summary.Running != 1 || resp.Summary.PROpen != 0 || resp.Summary.Failed != 1 || resp.Summary.Sessions != 4 || resp.Summary.NeedsAttention != 2 {
		t.Fatalf("unexpected summary: %+v", resp.Summary)
	}
	if resp.Summary.ThroughputMerged7D != 2 {
		t.Fatalf("throughput merged 7d = %d, want 2", resp.Summary.ThroughputMerged7D)
	}
	if len(resp.Summary.ThroughputDaily7D) != 7 {
		t.Fatalf("throughput daily len = %d, want 7", len(resp.Summary.ThroughputDaily7D))
	}
	visibleAttention := 0
	for _, worker := range resp.Workers {
		if worker.NeedsAttention {
			visibleAttention++
		}
	}
	if resp.Summary.NeedsAttention != visibleAttention {
		t.Fatalf("summary attention = %d, visible attention rows = %d", resp.Summary.NeedsAttention, visibleAttention)
	}
	if len(resp.Attention) != resp.Summary.NeedsAttention {
		t.Fatalf("attention inbox len = %d, want %d", len(resp.Attention), resp.Summary.NeedsAttention)
	}
	if resp.Projects[0].Name != "One" {
		t.Fatalf("first project = %q, want One", resp.Projects[0].Name)
	}
	if len(resp.Projects[0].Active) != 2 {
		t.Fatalf("project active len = %d, want 2", len(resp.Projects[0].Active))
	}
	if !resp.Projects[0].Outcome.Configured || resp.Projects[0].Outcome.Goal != "One is deployed" || resp.Projects[0].Outcome.HealthState != outcome.HealthUnknown {
		t.Fatalf("project outcome = %+v, want configured unknown health", resp.Projects[0].Outcome)
	}
	if len(resp.Workers) != 4 {
		t.Fatalf("fleet workers len = %d, want 4", len(resp.Workers))
	}
	worker := findFleetWorker(t, resp.Workers, "one-2")
	if worker.ProjectName != "One" || worker.ProjectRepo != "owner/one" {
		t.Fatalf("worker project = %q/%q, want One/owner/one", worker.ProjectName, worker.ProjectRepo)
	}
	if worker.IssueURL != "https://github.com/owner/one/issues/2" {
		t.Fatalf("worker issue_url = %q", worker.IssueURL)
	}
	if worker.PRURL != "https://github.com/owner/one/pull/12" {
		t.Fatalf("worker pr_url = %q", worker.PRURL)
	}
	if worker.TokensUsedTotal != 42000 {
		t.Fatalf("worker tokens = %d, want 42000", worker.TokensUsedTotal)
	}
	if worker.RuntimeSeconds <= 0 {
		t.Fatalf("worker runtime_seconds = %d, want positive runtime", worker.RuntimeSeconds)
	}
	if len(worker.Actions) != 5 {
		t.Fatalf("worker actions = %d, want 5", len(worker.Actions))
	}
	for _, action := range worker.Actions {
		assertFleetReadOnlyAction(t, action)
	}
	if len(resp.Projects[0].Actions) != 2 {
		t.Fatalf("project actions = %d, want 2", len(resp.Projects[0].Actions))
	}
	for _, action := range resp.Projects[0].Actions {
		assertFleetReadOnlyAction(t, action)
		if action.Target != "One" {
			t.Fatalf("project action target = %q, want project name One", action.Target)
		}
	}
	attentionWorker := findFleetWorker(t, resp.Workers, "two-1")
	if !attentionWorker.NeedsAttention {
		t.Fatal("retry-exhausted worker should need attention")
	}
	if !contains(attentionWorker.StatusReason, "checks failed") || !contains(attentionWorker.StatusReason, "PR #31 remains open") {
		t.Fatalf("attention status_reason = %q, want failed checks and open PR", attentionWorker.StatusReason)
	}
	if !contains(attentionWorker.NextAction, "Fix failing checks") {
		t.Fatalf("attention next_action = %q, want fix checks guidance", attentionWorker.NextAction)
	}
	if resp.Projects[1].NeedsAttention != len(resp.Projects[1].Attention) {
		t.Fatalf("project attention count = %d, reasons = %d", resp.Projects[1].NeedsAttention, len(resp.Projects[1].Attention))
	}
}

func TestFleetThroughputBucketsAggregateSevenDayWindows(t *testing.T) {
	now := time.Date(2026, 5, 2, 15, 0, 0, 0, time.UTC)
	donePR := func(pr int, finishedAt time.Time) fleetWorkerState {
		return fleetWorkerState{Status: string(state.StatusDone), PRNumber: pr, FinishedAt: finishedAt.Format(time.RFC3339)}
	}
	fullWindow := make([]fleetWorkerState, 0, 7)
	for daysAgo := 6; daysAgo >= 0; daysAgo-- {
		fullWindow = append(fullWindow, donePR(100+daysAgo, now.AddDate(0, 0, -daysAgo)))
	}
	partialWindow := []fleetWorkerState{
		{Status: string(state.StatusDone), PRNumber: 10, FinishedAt: now.Add(-2 * time.Hour).Format(time.RFC3339)},
		{Status: string(state.StatusDone), PRNumber: 11, FinishedAt: now.Add(-24 * time.Hour).Format(time.RFC3339)},
		{Status: string(state.StatusDone), PRNumber: 12, FinishedAt: now.Add(-6 * 24 * time.Hour).Format(time.RFC3339)},
		{Status: string(state.StatusDone), PRNumber: 13, FinishedAt: now.Add(-8 * 24 * time.Hour).Format(time.RFC3339)},
		{Status: string(state.StatusFailed), PRNumber: 14, FinishedAt: now.Add(-time.Hour).Format(time.RFC3339)},
		{Status: string(state.StatusDone), PRNumber: 0, FinishedAt: now.Add(-time.Hour).Format(time.RFC3339)},
		{Status: string(state.StatusDone), PRNumber: 15, FinishedAt: "not-a-time"},
	}

	tests := []struct {
		name    string
		workers []fleetWorkerState
		want    []int
	}{
		{
			name: "zero data",
			want: []int{0, 0, 0, 0, 0, 0, 0},
		},
		{
			name:    "partial window",
			workers: partialWindow,
			want:    []int{1, 0, 0, 0, 0, 1, 1},
		},
		{
			name:    "full window",
			workers: fullWindow,
			want:    []int{1, 1, 1, 1, 1, 1, 1},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			buckets := newFleetThroughputBuckets(now, 7)
			addFleetThroughputSummary(buckets, tc.workers)

			if got := buckets.Counts(); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("counts = %v, want %v", got, tc.want)
			}
			wantTotal := 0
			for _, count := range tc.want {
				wantTotal += count
			}
			if buckets.Total() != wantTotal {
				t.Fatalf("total = %d, want %d", buckets.Total(), wantTotal)
			}
		})
	}
}

func TestFleetAPIReviewRetryLifecycleDisplay(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()
	retryAt := now.Add(10 * time.Minute)
	stateDir := filepath.Join(dir, "review-retry")
	saveFleetTestState(t, stateDir, map[string]*state.Session{
		"retry-backoff": {
			IssueNumber:                 42,
			IssueTitle:                  "Address review feedback",
			Status:                      state.StatusDead,
			StartedAt:                   now.Add(-20 * time.Minute),
			FinishedAt:                  &now,
			PRNumber:                    12,
			RetryCount:                  1,
			NextRetryAt:                 &retryAt,
			PreviousAttemptFeedbackKind: state.RetryReasonReviewFeedback,
			RetryReason:                 state.RetryReasonReviewFeedback,
		},
		"retry-recheck": {
			IssueNumber: 43,
			IssueTitle:  "Wait for recheck",
			Status:      state.StatusPROpen,
			StartedAt:   now.Add(-30 * time.Minute),
			PRNumber:    13,
			RetryCount:  1,
			RetryReason: state.RetryReasonReviewFeedback,
		},
		"ci-retry": {
			IssueNumber:                 44,
			IssueTitle:                  "Retry failing checks",
			Status:                      state.StatusDead,
			StartedAt:                   now.Add(-40 * time.Minute),
			FinishedAt:                  &now,
			RetryCount:                  1,
			NextRetryAt:                 &retryAt,
			PreviousAttemptFeedbackKind: state.RetryReasonReviewFeedback,
			CIFailureOutput:             "checks failed",
		},
	})

	srv := NewFleet([]FleetProject{
		NewFleetProject("ReviewRetry", "/tmp/review-retry.yaml", "", &config.Config{
			Repo:        "owner/review-retry",
			StateDir:    stateDir,
			MaxParallel: 2,
		}),
	}, "127.0.0.1", 8786, true)
	resp := srv.snapshot()

	backoff := findFleetWorker(t, resp.Workers, "retry-backoff")
	if backoff.DisplayStatus != string(state.DisplayReviewRetryBackoff) {
		t.Fatalf("backoff display_status = %q, want review retry backoff", backoff.DisplayStatus)
	}
	if backoff.NeedsAttention {
		t.Fatal("review retry backoff should not need fleet attention")
	}
	if !contains(backoff.StatusReason, "waiting for the retry backoff") || !contains(backoff.NextAction, "scheduled retry worker") {
		t.Fatalf("backoff why = %q / %q, want retry worker wording", backoff.StatusReason, backoff.NextAction)
	}

	recheck := findFleetWorker(t, resp.Workers, "retry-recheck")
	if recheck.DisplayStatus != string(state.DisplayReviewRetryRecheck) {
		t.Fatalf("recheck display_status = %q, want review retry recheck", recheck.DisplayStatus)
	}
	if !contains(recheck.StatusReason, "waiting for CI, Greptile, or the merge gate") {
		t.Fatalf("recheck status_reason = %q, want CI/Greptile/merge gate wording", recheck.StatusReason)
	}
	ciRetry := findFleetWorker(t, resp.Workers, "ci-retry")
	if ciRetry.DisplayStatus != "" {
		t.Fatalf("ci retry display_status = %q, want raw dead state", ciRetry.DisplayStatus)
	}
	if !ciRetry.NeedsAttention || !contains(ciRetry.StatusReason, "retry is scheduled") {
		t.Fatalf("ci retry why = %q / attention %v, want dead retry guidance", ciRetry.StatusReason, ciRetry.NeedsAttention)
	}

	project := findFleetProject(t, resp.Projects, "ReviewRetry")
	if project.Failed != 1 || resp.Summary.Failed != 1 {
		t.Fatalf("failed counts = project %d fleet %d, want only CI retry counted", project.Failed, resp.Summary.Failed)
	}
	if project.NeedsAttention != 1 || resp.Summary.NeedsAttention != 1 {
		t.Fatalf("attention counts = project %d fleet %d, want only CI retry attention", project.NeedsAttention, resp.Summary.NeedsAttention)
	}
}

func TestFleetAPISuppressesResolvedStaleReviewFeedback(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()
	finished := now.Add(-5 * time.Minute)
	stateDir := filepath.Join(dir, "resolved-review-feedback")
	st := state.NewState()
	st.Sessions["merged-done"] = &state.Session{
		IssueNumber:                 359,
		IssueTitle:                  "Merged review feedback",
		Status:                      state.StatusDone,
		StartedAt:                   now.Add(-2 * time.Hour),
		FinishedAt:                  &finished,
		PRNumber:                    375,
		PreviousAttemptFeedbackKind: state.RetryReasonReviewFeedback,
		RetryReason:                 state.RetryReasonReviewFeedback,
	}
	st.Sessions["open-feedback"] = &state.Session{
		IssueNumber:                 360,
		IssueTitle:                  "Open review feedback",
		Status:                      state.StatusPROpen,
		StartedAt:                   now.Add(-time.Hour),
		PRNumber:                    376,
		PreviousAttemptFeedbackKind: state.RetryReasonReviewFeedback,
	}
	st.RecordSupervisorDecision(state.SupervisorDecision{
		ID:        "sup-review-feedback",
		CreatedAt: now,
		Project:   "owner/resolved-review-feedback",
		StuckStates: []state.SupervisorStuckState{
			{
				Code:              "stale_review_feedback",
				Severity:          "blocked",
				Summary:           "Issue #359 has review feedback, but no worker is currently fixing it.",
				RecommendedAction: "Respawn a worker with the saved review feedback or resolve the feedback manually.",
				Target:            &state.SupervisorTarget{Issue: 359, PR: 375, Session: "merged-done"},
			},
			{
				Code:              "stale_review_feedback",
				Severity:          "blocked",
				Summary:           "Issue #360 has review feedback, but no worker is currently fixing it.",
				RecommendedAction: "Respawn a worker with the saved review feedback or resolve the feedback manually.",
				Target:            &state.SupervisorTarget{Issue: 360, PR: 376, Session: "open-feedback"},
			},
		},
	}, state.DefaultSupervisorDecisionLimit)
	if err := state.Save(stateDir, st); err != nil {
		t.Fatalf("save state: %v", err)
	}
	cfg := &config.Config{Repo: "owner/resolved-review-feedback", StateDir: stateDir, MaxParallel: 2}

	single := buildStateResponse(cfg, st)
	singleDone := findSessionInfo(t, single.All, "merged-done")
	if singleDone.NeedsAttention || singleDone.DisplayStatus != "" || !contains(singleDone.StatusReason, "Issue is complete") {
		t.Fatalf("single-project done session = %+v, want neutral historical status", singleDone)
	}
	singleOpen := findSessionInfo(t, single.All, "open-feedback")
	if !singleOpen.NeedsAttention || !contains(singleOpen.StatusReason, "review feedback") {
		t.Fatalf("single-project open feedback = %+v, want attention", singleOpen)
	}

	srv := NewFleet([]FleetProject{
		NewFleetProject("ResolvedReviewFeedback", "/tmp/resolved-review-feedback.yaml", "", cfg),
	}, "127.0.0.1", 8786, true)
	resp := srv.snapshot()

	doneWorker := findFleetWorker(t, resp.Workers, "merged-done")
	if doneWorker.NeedsAttention || doneWorker.DisplayStatus != "" || !contains(doneWorker.StatusReason, "Issue is complete") {
		t.Fatalf("fleet done worker = %+v, want neutral historical status", doneWorker)
	}
	openWorker := findFleetWorker(t, resp.Workers, "open-feedback")
	if !openWorker.NeedsAttention || !contains(openWorker.StatusReason, "review feedback") {
		t.Fatalf("fleet open feedback worker = %+v, want attention", openWorker)
	}
	project := findFleetProject(t, resp.Projects, "ResolvedReviewFeedback")
	if project.NeedsAttention != 1 || resp.Summary.NeedsAttention != 1 || len(resp.Attention) != 1 {
		t.Fatalf("attention counts = project %d fleet %d inbox %d, want only open feedback", project.NeedsAttention, resp.Summary.NeedsAttention, len(resp.Attention))
	}
	if resp.Attention[0].Slot != "open-feedback" {
		t.Fatalf("attention inbox = %+v, want only open-feedback", resp.Attention)
	}
}

func TestFleetAPIIncludesQueueSnapshotMetadata(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()
	excludedStateDir := filepath.Join(dir, "excluded")
	candidateStateDir := filepath.Join(dir, "candidate")

	excludedState := state.NewState()
	excludedState.RecordSupervisorDecision(state.SupervisorDecision{
		ID:                "sup-excluded",
		CreatedAt:         now,
		Project:           "owner/excluded",
		Summary:           "No issue is currently eligible under the dynamic wave policy.",
		RecommendedAction: "none",
		Risk:              "safe",
		PolicyRule:        "supervisor.dynamic_wave",
		QueueAnalysis: &state.SupervisorQueueAnalysis{
			PolicyRule:                    "supervisor.dynamic_wave",
			OpenIssues:                    4,
			EligibleCandidates:            0,
			ExcludedIssues:                1,
			HeldIssues:                    1,
			BlockedByDependencyIssues:     1,
			NonRunnableProjectStatusCount: 1,
			SkippedReasons: []string{
				"Issue #24 skipped by dynamic wave policy: excluded by label \"blocked\"",
				"Issue #25 skipped by dynamic wave policy: held/meta: mission parent issue",
				"Issue #26 skipped by dynamic wave policy: blocked by dependency: open issue(s) #12",
				"Issue #27 skipped by dynamic wave policy: project status \"In Progress\" is not runnable",
			},
		},
	}, state.DefaultSupervisorDecisionLimit)
	if err := state.Save(excludedStateDir, excludedState); err != nil {
		t.Fatalf("save excluded state: %v", err)
	}

	candidateState := state.NewState()
	candidateState.RecordSupervisorDecision(state.SupervisorDecision{
		ID:                "sup-candidate",
		CreatedAt:         now,
		Project:           "owner/candidate",
		Summary:           "Start a worker for issue #309.",
		RecommendedAction: "spawn_worker",
		Risk:              "mutating",
		PolicyRule:        "supervisor.dynamic_wave",
		QueueAnalysis: &state.SupervisorQueueAnalysis{
			PolicyRule:         "supervisor.dynamic_wave",
			OpenIssues:         3,
			EligibleCandidates: 2,
			ExcludedIssues:     1,
			SelectedCandidate: &state.SupervisorIssueCandidate{
				Number: 309,
				Title:  "Selected fleet card candidate",
			},
		},
	}, state.DefaultSupervisorDecisionLimit)
	if err := state.Save(candidateStateDir, candidateState); err != nil {
		t.Fatalf("save candidate state: %v", err)
	}

	srv := NewFleet([]FleetProject{
		NewFleetProject("Excluded", "/tmp/excluded.yaml", "", &config.Config{
			Repo:        "owner/excluded",
			StateDir:    excludedStateDir,
			MaxParallel: 1,
		}),
		NewFleetProject("Candidate", "/tmp/candidate.yaml", "", &config.Config{
			Repo:        "owner/candidate",
			StateDir:    candidateStateDir,
			MaxParallel: 1,
		}),
	}, "127.0.0.1", 8786, true)
	resp := srv.snapshot()

	excluded := findFleetProject(t, resp.Projects, "Excluded")
	if excluded.QueueSnapshot == nil {
		t.Fatal("excluded project queue snapshot is nil")
	}
	if excluded.QueueSnapshot.Open != 4 || excluded.QueueSnapshot.Eligible != 0 || excluded.QueueSnapshot.Excluded != 1 || excluded.QueueSnapshot.Held != 1 || excluded.QueueSnapshot.BlockedByDependency != 1 || excluded.QueueSnapshot.NonRunnableProjectStatusCount != 1 {
		t.Fatalf("excluded queue snapshot = %+v, want classified skipped counts", excluded.QueueSnapshot)
	}
	if !contains(excluded.QueueSnapshot.IdleReason, "Queue policy classified all 4 open issues") || !contains(excluded.QueueSnapshot.IdleReason, "blocked-by-dependency=1") {
		t.Fatalf("idle reason = %q, want classified explanation", excluded.QueueSnapshot.IdleReason)
	}
	if !contains(excluded.QueueSnapshot.TopSkippedReason, "excluded by label") {
		t.Fatalf("top skipped reason = %q, want excluded label reason", excluded.QueueSnapshot.TopSkippedReason)
	}
	if excluded.Supervisor.Latest == nil || excluded.Supervisor.Latest.QueueAnalysis == nil || excluded.Supervisor.Latest.QueueAnalysis.OpenIssues != 4 || excluded.Supervisor.Latest.QueueAnalysis.HeldIssues != 1 {
		t.Fatalf("supervisor latest queue analysis = %#v, want exposed analysis", excluded.Supervisor.Latest)
	}

	candidate := findFleetProject(t, resp.Projects, "Candidate")
	if candidate.QueueSnapshot == nil || candidate.QueueSnapshot.SelectedCandidate == nil || candidate.QueueSnapshot.SelectedCandidate.Number != 309 {
		t.Fatalf("candidate queue snapshot = %+v, want selected issue #309", candidate.QueueSnapshot)
	}
	if candidate.QueueSnapshot.IdleReason != "" {
		t.Fatalf("candidate idle reason = %q, want empty when eligible", candidate.QueueSnapshot.IdleReason)
	}
}

func TestFleetAPIOperatorStateExplainsZeroRunningActiveWork(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()
	monitorStateDir := filepath.Join(dir, "monitor")
	candidateStateDir := filepath.Join(dir, "candidate")

	monitorState := state.NewState()
	monitorState.Sessions["pr-1"] = &state.Session{
		IssueNumber: 42,
		IssueTitle:  "Review PR",
		Status:      state.StatusPROpen,
		StartedAt:   now.Add(-10 * time.Minute),
		PRNumber:    12,
	}
	monitorState.RecordSupervisorDecision(state.SupervisorDecision{
		ID:                "sup-monitor",
		CreatedAt:         now,
		Project:           "owner/monitor",
		Summary:           "Monitor PR #12 until checks and review gates pass.",
		RecommendedAction: "monitor_open_pr",
		Risk:              "safe",
		Target:            &state.SupervisorTarget{Issue: 42, PR: 12, Session: "pr-1"},
	}, state.DefaultSupervisorDecisionLimit)
	if err := state.Save(monitorStateDir, monitorState); err != nil {
		t.Fatalf("save monitor state: %v", err)
	}

	candidateState := state.NewState()
	candidateState.RecordSupervisorDecision(state.SupervisorDecision{
		ID:                "sup-candidate",
		CreatedAt:         now,
		Project:           "owner/candidate",
		Summary:           "Start a worker for issue #309.",
		RecommendedAction: "spawn_worker",
		Risk:              "mutating",
		Target:            &state.SupervisorTarget{Issue: 309},
		QueueAnalysis: &state.SupervisorQueueAnalysis{
			OpenIssues:         3,
			EligibleCandidates: 2,
			SelectedCandidate: &state.SupervisorIssueCandidate{
				Number: 309,
				Title:  "Selected fleet candidate",
			},
		},
	}, state.DefaultSupervisorDecisionLimit)
	if err := state.Save(candidateStateDir, candidateState); err != nil {
		t.Fatalf("save candidate state: %v", err)
	}

	srv := NewFleet([]FleetProject{
		NewFleetProject("Monitor", "/tmp/monitor.yaml", "", &config.Config{
			Repo:        "owner/monitor",
			StateDir:    monitorStateDir,
			MaxParallel: 1,
			Outcome:     outcome.Brief{DesiredOutcome: "Monitor outcome"},
		}),
		NewFleetProject("Candidate", "/tmp/candidate.yaml", "", &config.Config{
			Repo:        "owner/candidate",
			StateDir:    candidateStateDir,
			MaxParallel: 1,
			Outcome:     outcome.Brief{DesiredOutcome: "Candidate outcome"},
		}),
	}, "127.0.0.1", 8786, true)

	resp := srv.snapshot()
	if resp.Summary.Running != 0 || resp.Summary.Active != 2 || resp.Summary.MonitoringPR != 1 || resp.Summary.DispatchPending != 1 {
		t.Fatalf("summary = %+v, want zero running but two active operator states", resp.Summary)
	}

	monitor := findFleetProject(t, resp.Projects, "Monitor")
	if monitor.OperatorState.Kind != "monitoring_pr" || monitor.OperatorState.PRNumber != 12 || monitor.OperatorState.IssueNumber != 42 {
		t.Fatalf("monitor operator state = %+v, want monitoring PR #12 for issue #42", monitor.OperatorState)
	}
	monitorHTML := renderFleetProjectRailState(monitor)
	if contains(monitorHTML, "0/1 running") || !contains(monitorHTML, "Monitoring PR") {
		t.Fatalf("monitor rail state should explain PR monitoring without raw running counter, got:\n%s", monitorHTML)
	}

	candidate := findFleetProject(t, resp.Projects, "Candidate")
	if candidate.OperatorState.Kind != "pending_dispatch" || candidate.OperatorState.IssueNumber != 309 {
		t.Fatalf("candidate operator state = %+v, want pending dispatch for issue #309", candidate.OperatorState)
	}
	candidateHTML := renderFleetProjectRailState(candidate)
	if contains(candidateHTML, "0/1 running") || !contains(candidateHTML, "Dispatch pending") {
		t.Fatalf("candidate rail state should explain pending dispatch without raw running counter, got:\n%s", candidateHTML)
	}
}

func TestFleetOperatorBriefNamesSingleHighestPriorityAction(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()
	staleWorkerStateDir := filepath.Join(dir, "stale-worker")
	noEligibleStateDir := filepath.Join(dir, "no-eligible")
	runningStateDir := filepath.Join(dir, "running")

	saveFleetTestState(t, staleWorkerStateDir, map[string]*state.Session{
		"stale-1": {
			IssueNumber: 42,
			IssueTitle:  "Stale worker",
			Status:      state.StatusRunning,
			StartedAt:   now.Add(-12 * time.Minute),
			PID:         0,
		},
	})
	noEligibleState := state.NewState()
	noEligibleState.RecordSupervisorDecision(state.SupervisorDecision{
		ID:                "sup-no-eligible",
		CreatedAt:         now,
		Project:           "owner/no-eligible",
		Summary:           "No issue is eligible under policy.",
		RecommendedAction: "none",
		Risk:              "safe",
		QueueAnalysis: &state.SupervisorQueueAnalysis{
			OpenIssues:         2,
			EligibleCandidates: 0,
			ExcludedIssues:     2,
		},
	}, state.DefaultSupervisorDecisionLimit)
	if err := state.Save(noEligibleStateDir, noEligibleState); err != nil {
		t.Fatalf("save no eligible state: %v", err)
	}
	saveFleetTestState(t, runningStateDir, map[string]*state.Session{
		"run-1": {
			IssueNumber: 77,
			IssueTitle:  "Running worker",
			Status:      state.StatusRunning,
			StartedAt:   now.Add(-time.Minute),
			PID:         os.Getpid(),
		},
	})

	srv := NewFleet([]FleetProject{
		NewFleetProject("NoEligible", "/tmp/no-eligible.yaml", "", &config.Config{
			Repo:        "owner/no-eligible",
			StateDir:    noEligibleStateDir,
			MaxParallel: 1,
			Outcome:     outcome.Brief{DesiredOutcome: "No eligible outcome"},
		}),
		NewFleetProject("Running", "/tmp/running.yaml", "", &config.Config{
			Repo:        "owner/running",
			StateDir:    runningStateDir,
			MaxParallel: 1,
			Outcome:     outcome.Brief{DesiredOutcome: "Running outcome"},
		}),
		NewFleetProject("StaleWorker", "/tmp/stale-worker.yaml", "", &config.Config{
			Repo:        "owner/stale-worker",
			StateDir:    staleWorkerStateDir,
			MaxParallel: 1,
			Outcome:     outcome.Brief{DesiredOutcome: "Stale worker outcome"},
		}),
	}, "127.0.0.1", 8786, true)

	resp := srv.snapshot()
	brief := resp.OperatorBrief
	if !brief.ActionRequired || brief.Kind != "stale_worker" || brief.Project != "StaleWorker" {
		t.Fatalf("operator brief = %+v, want stale worker action for StaleWorker", brief)
	}
	if brief.IssueNumber != 42 || brief.Session != "stale-1" || !contains(brief.Reason, "PID is not alive") {
		t.Fatalf("operator brief target/reason = %+v, want issue/session/dead PID", brief)
	}
	for _, want := range []string{"Global brief: action required", "StaleWorker", "issue #42", "session stale-1", "Reason:"} {
		if !contains(brief.Sentence, want) {
			t.Fatalf("operator brief sentence = %q, want %q", brief.Sentence, want)
		}
	}
	if contains(brief.Sentence, "issue #77") {
		t.Fatalf("operator brief should name one highest-priority action, got %q", brief.Sentence)
	}
	if resp.Summary.StaleWorkers != 1 || resp.Summary.NoEligibleIssues != 1 {
		t.Fatalf("summary = %+v, want stale worker and no eligible counts", resp.Summary)
	}
}

func TestFleetOperatorBriefPrioritizesPendingApproval(t *testing.T) {
	now := time.Now().UTC()
	brief := buildFleetOperatorBrief([]fleetProjectState{{
		Name: "BlockedQueue",
		OperatorState: fleetOperatorState{
			Kind:        "no_eligible_issues",
			Tone:        "attention",
			Label:       "No eligible issues",
			Summary:     "No issue is eligible under policy.",
			NextAction:  "Adjust labels or policy so a worker can run.",
			IssueNumber: 15,
		},
	}}, []fleetApprovalState{{
		ProjectName: "ApprovalProject",
		Status:      string(state.ApprovalStatusPending),
		Summary:     "Supervisor approval is waiting for operator review.",
		PRNumber:    44,
		Session:     "approval-44",
		createdAt:   now.Add(-time.Minute),
		updatedAt:   now,
	}}, now)

	if !brief.ActionRequired || brief.Kind != "approval_pending" || brief.Project != "ApprovalProject" {
		t.Fatalf("operator brief = %+v, want pending approval action for ApprovalProject", brief)
	}
	if brief.PRNumber != 44 || brief.Session != "approval-44" {
		t.Fatalf("operator brief target = %+v, want PR #44 and approval-44 session", brief)
	}
	if !contains(brief.NextAction, "Approve or reject") {
		t.Fatalf("operator brief next action = %q, want approval guidance", brief.NextAction)
	}
	if contains(brief.Sentence, "Next:") {
		t.Fatalf("operator brief sentence = %q, should leave next action for structured rendering", brief.Sentence)
	}
}

func TestHighestPriorityPendingFleetApprovalSelectsNewestPending(t *testing.T) {
	now := time.Now().UTC()
	selected := highestPriorityPendingFleetApproval([]fleetApprovalState{
		{
			ProjectName: "OldApproval",
			ID:          "old",
			Status:      string(state.ApprovalStatusPending),
			createdAt:   now.Add(-2 * time.Hour),
			updatedAt:   now.Add(-2 * time.Hour),
		},
		{
			ProjectName: "ApprovedApproval",
			ID:          "approved",
			Status:      string(state.ApprovalStatusApproved),
			createdAt:   now,
			updatedAt:   now,
		},
		{
			ProjectName: "NewApproval",
			ID:          "new",
			Status:      string(state.ApprovalStatusPending),
			createdAt:   now.Add(-time.Hour),
			updatedAt:   now.Add(-5 * time.Minute),
		},
	})

	if selected == nil || selected.ProjectName != "NewApproval" {
		t.Fatalf("selected approval = %+v, want newest pending approval", selected)
	}
}

func TestFleetProjectOperatorStateDistinguishesBriefStates(t *testing.T) {
	now := time.Now().UTC()
	configuredOutcome := outcome.Status{Configured: true, Goal: "Runtime is healthy", HealthState: outcome.HealthHealthy}

	dispatch := buildFleetProjectOperatorState(fleetProjectState{
		Name:    "Dispatch",
		Repo:    "owner/dispatch",
		Outcome: configuredOutcome,
		Supervisor: supervisorInfo{Latest: &supervisorDecisionInfo{
			CreatedAt:         now,
			Status:            "failed",
			ErrorClass:        "github_api",
			Summary:           "Supervisor queue action failed for issue #9.",
			RecommendedAction: "label_issue_ready",
			Target:            &state.SupervisorTarget{Issue: 9},
		}},
	})
	if dispatch.Kind != "dispatch_failure" || dispatch.IssueNumber != 9 {
		t.Fatalf("dispatch operator state = %+v, want dispatch failure for issue #9", dispatch)
	}

	drift := buildFleetProjectOperatorState(fleetProjectState{
		Name: "Runtime",
		Outcome: outcome.Status{
			Configured:  true,
			Goal:        "Runtime is healthy",
			HealthState: outcome.HealthFailing,
		},
		QueueSnapshot: &fleetQueueSnapshot{Open: 0},
	})
	if drift.Kind != "outcome_drift" || !contains(drift.Summary, "failing") {
		t.Fatalf("drift operator state = %+v, want runtime outcome drift", drift)
	}

	blocked := buildFleetProjectOperatorState(fleetProjectState{
		Name:          "BlockedQueue",
		Outcome:       configuredOutcome,
		QueueSnapshot: &fleetQueueSnapshot{Open: 3, Eligible: 0, Excluded: 3, IdleReason: "Policy excluded all 3 open issues."},
	})
	if blocked.Kind != "no_eligible_issues" || !contains(blocked.Summary, "Policy excluded") {
		t.Fatalf("blocked queue operator state = %+v, want no eligible issues", blocked)
	}

	idle := buildFleetProjectOperatorState(fleetProjectState{
		Name:          "Idle",
		Outcome:       configuredOutcome,
		QueueSnapshot: &fleetQueueSnapshot{Open: 0, IdleReason: "No open issues are available."},
	})
	if idle.Kind != "idle" || idle.Label != "Healthy idle" {
		t.Fatalf("idle operator state = %+v, want healthy idle", idle)
	}
}

func TestFleetVerdictCoversHeaderStates(t *testing.T) {
	now := time.Now().UTC()
	tests := []struct {
		name      string
		sessions  map[string]*state.Session
		decisions []state.SupervisorDecision
		wantTone  string
		wantText  []string
	}{
		{
			name: "healthy idle by policy",
			decisions: []state.SupervisorDecision{{
				ID:                "sup-healthy-idle",
				CreatedAt:         now,
				Project:           "owner/healthy-idle",
				Summary:           "No open issues match the configured ready labels.",
				RecommendedAction: "none",
				Risk:              "safe",
				QueueAnalysis: &state.SupervisorQueueAnalysis{
					OpenIssues:         1,
					EligibleCandidates: 0,
					ExcludedIssues:     1,
				},
			}},
			wantTone: "healthy",
			wantText: []string{"Supervisor healthy.", "No worker is running by policy.", "No item needs attention."},
		},
		{
			name: "busy running worker",
			sessions: map[string]*state.Session{
				"busy-1": {
					IssueNumber: 11,
					IssueTitle:  "Build busy thing",
					Status:      state.StatusRunning,
					StartedAt:   now.Add(-time.Minute),
					PID:         os.Getpid(),
				},
			},
			decisions: []state.SupervisorDecision{{
				ID:                "sup-busy",
				CreatedAt:         now,
				Project:           "owner/busy",
				Summary:           "Worker is already running.",
				RecommendedAction: "wait_for_worker",
				Risk:              "safe",
			}},
			wantTone: "busy",
			wantText: []string{"Supervisor healthy.", "1 worker is running.", "No item needs attention."},
		},
		{
			name: "attention required",
			sessions: map[string]*state.Session{
				"dead-1": {
					IssueNumber: 12,
					IssueTitle:  "Dead worker",
					Status:      state.StatusDead,
					StartedAt:   now.Add(-2 * time.Minute),
				},
			},
			decisions: []state.SupervisorDecision{{
				ID:                "sup-attention",
				CreatedAt:         now,
				Project:           "owner/attention",
				Summary:           "Worker needs reconciliation.",
				RecommendedAction: "wait_for_reconciliation",
				Risk:              "safe",
			}},
			wantTone: "attention",
			wantText: []string{"Supervisor healthy.", "No worker is running.", "1 item needs attention."},
		},
		{
			name: "daemon down stale heartbeat",
			decisions: []state.SupervisorDecision{{
				ID:                "sup-stale",
				CreatedAt:         now.Add(-fleetSupervisorHeartbeatStaleAfter - time.Minute),
				Project:           "owner/stale",
				Summary:           "No worker slot is available.",
				RecommendedAction: "none",
				Risk:              "safe",
			}},
			wantTone: "daemon-down",
			wantText: []string{"Supervisor heartbeat lost", "Last safe action was", "No worker is running.", "No item needs attention."},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			stateDir := filepath.Join(dir, "state")
			saveFleetTestSnapshot(t, stateDir, tt.sessions, tt.decisions)
			srv := NewFleet([]FleetProject{
				NewFleetProject("Project", "/tmp/project.yaml", "", &config.Config{
					Repo:        "owner/project",
					StateDir:    stateDir,
					MaxParallel: 1,
				}),
			}, "127.0.0.1", 8786, true)

			resp := srv.snapshot()
			if resp.Verdict.Tone != tt.wantTone {
				t.Fatalf("verdict tone = %q, want %q; sentence=%q", resp.Verdict.Tone, tt.wantTone, resp.Verdict.Sentence)
			}
			for _, want := range tt.wantText {
				if !contains(resp.Verdict.Sentence, want) {
					t.Fatalf("verdict sentence = %q, want %q", resp.Verdict.Sentence, want)
				}
			}
		})
	}
}

func TestFleetVerdictDoesNotTreatProjectFreshnessStaleAsHeartbeatStale(t *testing.T) {
	now := time.Now().UTC()
	latest := &supervisorDecisionInfo{CreatedAt: now}
	resp := fleetResponse{
		Summary: fleetSummary{Projects: 1, Stale: 1},
		Projects: []fleetProjectState{{
			Supervisor: supervisorInfo{Latest: latest},
		}},
	}

	verdict := buildFleetVerdict(resp, now)
	if verdict.Tone != "attention" {
		t.Fatalf("verdict tone = %q, want attention; sentence=%q", verdict.Tone, verdict.Sentence)
	}
	if contains(verdict.Sentence, "heartbeat stale") || contains(verdict.Sentence, "heartbeat lost") {
		t.Fatalf("verdict sentence = %q, should not label stale snapshots as stale heartbeat", verdict.Sentence)
	}
	for _, want := range []string{"Supervisor healthy.", "1 project snapshot is stale.", "1 item needs attention."} {
		if !contains(verdict.Sentence, want) {
			t.Fatalf("verdict sentence = %q, want %q", verdict.Sentence, want)
		}
	}
}

func TestFleetIdleByPolicyRequiresEveryIdleProjectReason(t *testing.T) {
	policyIdle := fleetProjectState{QueueSnapshot: &fleetQueueSnapshot{IdleReason: "Policy excluded all 1 open issue."}}
	alsoPolicyIdle := fleetProjectState{QueueSnapshot: &fleetQueueSnapshot{IdleReason: "No open issues are available."}}

	if !fleetIdleByPolicy([]fleetProjectState{policyIdle, alsoPolicyIdle}) {
		t.Fatal("fleetIdleByPolicy = false, want true when every idle project has a policy reason")
	}
	if fleetIdleByPolicy([]fleetProjectState{policyIdle, {}}) {
		t.Fatal("fleetIdleByPolicy = true, want false when another idle project lacks a policy reason")
	}
}

func TestFleetAPISurfacesProjectErrorsAndStaleFreshness(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()
	healthyStateDir := filepath.Join(dir, "healthy")
	staleStateDir := filepath.Join(dir, "stale")
	brokenStateDir := filepath.Join(dir, "broken")
	finished := now.Add(-2 * time.Minute)
	saveFleetTestState(t, healthyStateDir, map[string]*state.Session{
		"healthy-1": {
			IssueNumber: 1,
			IssueTitle:  "Healthy done worker",
			Status:      state.StatusDone,
			StartedAt:   now.Add(-10 * time.Minute),
			FinishedAt:  &finished,
		},
	})
	saveFleetTestState(t, staleStateDir, map[string]*state.Session{
		"stale-1": {
			IssueNumber: 2,
			IssueTitle:  "Stale done worker",
			Status:      state.StatusDone,
			StartedAt:   now.Add(-20 * time.Minute),
			FinishedAt:  &finished,
		},
	})
	staleAt := now.Add(-fleetProjectStaleAfter - time.Minute)
	if err := os.Chtimes(state.StatePath(staleStateDir), staleAt, staleAt); err != nil {
		t.Fatalf("make state stale: %v", err)
	}
	if err := os.MkdirAll(brokenStateDir, 0o755); err != nil {
		t.Fatalf("create broken state dir: %v", err)
	}
	if err := os.WriteFile(state.StatePath(brokenStateDir), []byte("{not-json"), 0o644); err != nil {
		t.Fatalf("write broken state: %v", err)
	}

	srv := NewFleet([]FleetProject{
		NewFleetProject("Healthy", "/tmp/healthy.yaml", "", &config.Config{
			Repo:        "owner/healthy",
			StateDir:    healthyStateDir,
			MaxParallel: 1,
		}),
		NewFleetProject("Stale", "/tmp/stale.yaml", "", &config.Config{
			Repo:        "owner/stale",
			StateDir:    staleStateDir,
			MaxParallel: 1,
		}),
		NewFleetProject("Broken", "/tmp/broken.yaml", "", &config.Config{
			Repo:        "owner/broken",
			StateDir:    brokenStateDir,
			MaxParallel: 1,
		}),
	}, "127.0.0.1", 8786, true)

	resp := srv.snapshot()
	if resp.RefreshedAt == "" {
		t.Fatal("fleet response should include refreshed_at")
	}
	if resp.Summary.Projects != 3 || resp.Summary.Stale != 1 || resp.Summary.Errors != 1 {
		t.Fatalf("summary = %+v, want 3 projects, 1 stale, 1 error", resp.Summary)
	}
	healthy := findFleetProject(t, resp.Projects, "Healthy")
	if healthy.Error != "" || healthy.Sessions != 1 {
		t.Fatalf("healthy project = %+v, want rendered without error", healthy)
	}
	if healthy.Freshness.SnapshotAt == "" || healthy.Freshness.Stale {
		t.Fatalf("healthy freshness = %+v, want fresh snapshot metadata", healthy.Freshness)
	}
	stale := findFleetProject(t, resp.Projects, "Stale")
	if !stale.Freshness.Stale || stale.Freshness.SnapshotAgeSeconds <= int64(fleetProjectStaleAfter/time.Second) {
		t.Fatalf("stale freshness = %+v, want stale snapshot metadata", stale.Freshness)
	}
	if !contains(stale.Freshness.Reason, "stale after") {
		t.Fatalf("stale reason = %q, want threshold explanation", stale.Freshness.Reason)
	}
	broken := findFleetProject(t, resp.Projects, "Broken")
	if broken.Error == "" || broken.Freshness.StateUpdatedAt == "" {
		t.Fatalf("broken project = %+v, want load error with state timestamp", broken)
	}
}

func TestFleetProjectFreshnessUsesRawAgeForStaleThreshold(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()
	stateDir := filepath.Join(dir, "state")
	saveFleetTestState(t, stateDir, nil)

	staleAt := now.Add(-fleetProjectStaleAfter - 100*time.Millisecond)
	if err := os.Chtimes(state.StatePath(stateDir), staleAt, staleAt); err != nil {
		t.Fatalf("make state barely stale: %v", err)
	}

	freshness := fleetProjectFreshnessForState(stateDir, nil, now)
	if freshness.SnapshotAgeSeconds != int64(fleetProjectStaleAfter/time.Second) {
		t.Fatalf("snapshot age seconds = %d, want rounded threshold", freshness.SnapshotAgeSeconds)
	}
	if !freshness.Stale {
		t.Fatalf("freshness = %+v, want stale based on raw age", freshness)
	}
}

func TestFleetAPIIncludesApprovalInboxMetadata(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()
	stateDir := filepath.Join(dir, "approvals")
	st := state.NewState()
	st.Sessions["slot-pending"] = &state.Session{
		IssueNumber: 42,
		IssueTitle:  "Pending approval target",
		Status:      state.StatusRunning,
		StartedAt:   now.Add(-2 * time.Hour),
		PRNumber:    7,
	}
	st.Sessions["slot-stale"] = &state.Session{
		IssueNumber: 43,
		IssueTitle:  "Stale approval target",
		Status:      state.StatusRunning,
		StartedAt:   now.Add(-3 * time.Hour),
	}

	pending := st.RecordPendingApprovalForDecision(state.SupervisorDecision{
		ID:                "approval-pending",
		CreatedAt:         now.Add(-15 * time.Minute),
		Project:           "owner/approvals",
		Mode:              "active",
		Summary:           "Spawn a worker for issue #42.",
		RecommendedAction: "spawn_worker",
		Target:            &state.SupervisorTarget{Issue: 42, Session: "slot-pending"},
		Risk:              "approval_gated",
		Reasons:           []string{"Issue #42 is eligible"},
	}, now.Add(-15*time.Minute))
	approved := st.RecordPendingApprovalForDecision(state.SupervisorDecision{
		ID:                "approval-approved",
		CreatedAt:         now.Add(-30 * time.Minute),
		Project:           "owner/approvals",
		Summary:           "Merge PR #8.",
		RecommendedAction: "approve_merge",
		Target:            &state.SupervisorTarget{PR: 8},
		Risk:              "mutating",
	}, now.Add(-30*time.Minute))
	if _, err := st.ApproveApproval(approved.ID, now.Add(-20*time.Minute), "test", "covered by test"); err != nil {
		t.Fatalf("ApproveApproval: %v", err)
	}
	rejected := st.RecordPendingApprovalForDecision(state.SupervisorDecision{
		ID:                "approval-rejected",
		CreatedAt:         now.Add(-40 * time.Minute),
		Project:           "owner/approvals",
		Summary:           "Mark issue #44 blocked.",
		RecommendedAction: "mark_issue_blocked",
		Target:            &state.SupervisorTarget{Issue: 44},
		Risk:              "mutating",
	}, now.Add(-40*time.Minute))
	if _, err := st.RejectApproval(rejected.ID, now.Add(-25*time.Minute), "test", "covered by test"); err != nil {
		t.Fatalf("RejectApproval: %v", err)
	}
	stale := st.RecordPendingApprovalForDecision(state.SupervisorDecision{
		ID:                "approval-stale",
		CreatedAt:         now.Add(-50 * time.Minute),
		Project:           "owner/approvals",
		Summary:           "Start stale worker.",
		RecommendedAction: "spawn_worker",
		Target:            &state.SupervisorTarget{Issue: 43, Session: "slot-stale"},
		Risk:              "approval_gated",
	}, now.Add(-50*time.Minute))
	st.Sessions["slot-stale"].PRNumber = 9
	st.MarkStaleApprovals(now.Add(-10 * time.Minute))
	if err := state.Save(stateDir, st); err != nil {
		t.Fatalf("save state: %v", err)
	}

	srv := NewFleet([]FleetProject{
		NewFleetProject("Approvals", "/tmp/approvals.yaml", "http://127.0.0.1:8789", &config.Config{
			Repo:        "owner/approvals",
			StateDir:    stateDir,
			MaxParallel: 2,
		}),
	}, "127.0.0.1", 8786, true)
	resp := srv.snapshot()

	if len(resp.Approvals) != 4 {
		t.Fatalf("fleet approvals len = %d, want 4", len(resp.Approvals))
	}
	if len(resp.Projects) != 1 || len(resp.Projects[0].Approvals) != 4 {
		t.Fatalf("project approvals = %+v, want 4 approvals", resp.Projects)
	}
	if resp.Summary.Approvals != 1 || resp.Summary.ApprovalsPending != 1 || resp.Summary.ApprovalsHistorical != 3 || resp.Summary.ApprovalsStale != 1 || resp.Summary.ApprovalsApproved != 1 || resp.Summary.ApprovalsRejected != 1 {
		t.Fatalf("approval summary = %+v, want one active and three historical approvals", resp.Summary)
	}
	if resp.Projects[0].ApprovalSummary[string(state.ApprovalStatusPending)] != 1 || resp.Projects[0].ApprovalSummary[string(state.ApprovalStatusStale)] != 1 {
		t.Fatalf("project approval summary = %+v, want pending and stale counts", resp.Projects[0].ApprovalSummary)
	}
	if resp.Approvals[0].ID != pending.ID || resp.Approvals[1].ID != stale.ID {
		t.Fatalf("approval order = %q, %q; want pending then stale", resp.Approvals[0].ID, resp.Approvals[1].ID)
	}

	approval := findFleetApproval(t, resp.Approvals, pending.ID)
	if approval.ProjectName != "Approvals" || approval.ProjectRepo != "owner/approvals" || approval.DashboardURL == "" {
		t.Fatalf("approval project metadata = %+v", approval)
	}
	if approval.IssueNumber != 42 || approval.IssueURL != "https://github.com/owner/approvals/issues/42" {
		t.Fatalf("approval issue metadata = %+v", approval)
	}
	if approval.PRNumber != 7 || approval.PRURL != "https://github.com/owner/approvals/pull/7" {
		t.Fatalf("approval PR metadata = %+v", approval)
	}
	if approval.Session != "slot-pending" || approval.SessionStatus != string(state.StatusRunning) {
		t.Fatalf("approval session metadata = %+v", approval)
	}
	if approval.Status != string(state.ApprovalStatusPending) || approval.Action != "spawn_worker" || approval.Risk != "approval_gated" || approval.Summary == "" {
		t.Fatalf("approval lifecycle metadata = %+v", approval)
	}
	if approval.CreatedAge == "" || approval.UpdatedAge == "" || approval.CreatedAgeSeconds <= 0 || approval.UpdatedAgeSeconds <= 0 {
		t.Fatalf("approval ages = %+v, want populated age fields", approval)
	}
	if len(approval.TargetLinks) != 3 {
		t.Fatalf("approval target links = %+v, want issue, PR, and session links", approval.TargetLinks)
	}

	staleApproval := findFleetApproval(t, resp.Approvals, stale.ID)
	if staleApproval.Status != string(state.ApprovalStatusStale) {
		t.Fatalf("stale approval status = %q, want stale", staleApproval.Status)
	}
}

func TestFleetApprovalSummaryCountsOnlyActivePendingApprovals(t *testing.T) {
	var summary fleetSummary
	for _, status := range []string{
		string(state.ApprovalStatusPending),
		string(state.ApprovalStatusSuperseded),
		string(state.ApprovalStatusStale),
		string(state.ApprovalStatusApproved),
		string(state.ApprovalStatusRejected),
	} {
		addFleetApprovalSummary(&summary, status)
	}

	if summary.Approvals != 1 || summary.ApprovalsPending != 1 {
		t.Fatalf("active approval summary = %+v, want one pending active approval", summary)
	}
	if summary.ApprovalsHistorical != 4 || summary.ApprovalsSuperseded != 1 || summary.ApprovalsStale != 1 || summary.ApprovalsApproved != 1 || summary.ApprovalsRejected != 1 {
		t.Fatalf("historical approval summary = %+v, want one per historical status", summary)
	}
}

func TestFleetApprovalTargetReplacesStaleSessionWithMatchedSession(t *testing.T) {
	st := state.NewState()
	st.Sessions["slot-new"] = &state.Session{
		IssueNumber: 42,
		PRNumber:    7,
		Status:      state.StatusRunning,
	}

	issue, pr, session, sessionStatus := fleetApprovalTarget(st, &state.SupervisorTarget{
		Issue:   42,
		Session: "slot-old",
	})

	if issue != 42 || pr != 7 || session != "slot-new" || sessionStatus != string(state.StatusRunning) {
		t.Fatalf("target metadata = issue:%d pr:%d session:%q status:%q, want matched current session", issue, pr, session, sessionStatus)
	}
}

func TestFleetAttentionInboxOrdersBySeverityAndFreshness(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()
	stateDir := filepath.Join(dir, "finance")
	saveFleetTestState(t, stateDir, map[string]*state.Session{
		"fin-running": {
			IssueNumber: 306,
			IssueTitle:  "Finance stale-running worker with a title long enough to exercise compact inbox layout",
			Status:      state.StatusRunning,
			StartedAt:   now.Add(-12 * time.Minute),
			Backend:     "opencode",
		},
		"fin-pr": {
			IssueNumber: 307,
			IssueTitle:  "Waiting PR state missing its pull request number",
			Status:      state.StatusPROpen,
			StartedAt:   now.Add(-1 * time.Minute),
		},
		"fin-retry": {
			IssueNumber:     308,
			IssueTitle:      "Retry exhausted with failed checks",
			Status:          state.StatusRetryExhausted,
			StartedAt:       now.Add(-30 * time.Minute),
			PRNumber:        88,
			CIFailureOutput: "go test failed",
		},
		"fin-dead": {
			IssueNumber: 309,
			IssueTitle:  "Dead worker needs reconciliation",
			Status:      state.StatusDead,
			StartedAt:   now.Add(-5 * time.Minute),
		},
	})

	srv := NewFleet([]FleetProject{
		NewFleetProject("finance", "/tmp/finance.yaml", "http://127.0.0.1:8788", &config.Config{
			Repo:        "owner/finance",
			StateDir:    stateDir,
			MaxParallel: 4,
		}),
	}, "127.0.0.1", 8786, true)
	resp := srv.snapshot()

	if len(resp.Attention) != 4 {
		t.Fatalf("attention inbox len = %d, want 4", len(resp.Attention))
	}
	gotSlots := make([]string, 0, len(resp.Attention))
	for _, worker := range resp.Attention {
		gotSlots = append(gotSlots, worker.Slot)
	}
	wantSlots := []string{"fin-dead", "fin-retry", "fin-running", "fin-pr"}
	for i, want := range wantSlots {
		if gotSlots[i] != want {
			t.Fatalf("attention order = %v, want %v", gotSlots, wantSlots)
		}
	}

	stale := findFleetWorker(t, resp.Attention, "fin-running")
	if stale.ProjectName != "finance" || stale.DashboardURL == "" {
		t.Fatalf("stale worker project/link = %+v", stale)
	}
	if stale.IssueNumber != 306 || stale.IssueURL != "https://github.com/owner/finance/issues/306" {
		t.Fatalf("stale worker issue metadata = %+v", stale)
	}
	if stale.Status != string(state.StatusRunning) || !stale.NeedsAttention {
		t.Fatalf("stale worker status/attention = %q/%v", stale.Status, stale.NeedsAttention)
	}
	if !contains(stale.StatusReason, "PID is not alive") || !contains(stale.NextAction, "reconciliation cycle") {
		t.Fatalf("stale worker why/next = %q/%q", stale.StatusReason, stale.NextAction)
	}
	if stale.RuntimeSeconds <= 0 || stale.Runtime == "" {
		t.Fatalf("stale worker age = %q/%d, want populated", stale.Runtime, stale.RuntimeSeconds)
	}

	retry := findFleetWorker(t, resp.Attention, "fin-retry")
	if retry.PRNumber != 88 || retry.PRURL != "https://github.com/owner/finance/pull/88" {
		t.Fatalf("retry PR metadata = %d/%q", retry.PRNumber, retry.PRURL)
	}
}

func TestFleetAttentionSeverityChecksStatusText(t *testing.T) {
	worker := fleetWorkerState{Status: "blocked_waiting"}
	if got := fleetAttentionSeverity(worker); got != 0 {
		t.Fatalf("blocked status severity = %d, want 0", got)
	}
}

func TestFleetWorkersIncludeAllActiveRows(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()
	stateDir := filepath.Join(dir, "state")
	sessions := make(map[string]*state.Session)
	for i := 1; i <= 7; i++ {
		slot := "one-" + strconv.Itoa(i)
		sessions[slot] = &state.Session{
			IssueNumber: i,
			IssueTitle:  "Worker",
			Status:      state.StatusRunning,
			StartedAt:   now.Add(-time.Duration(i) * time.Minute),
		}
	}
	saveFleetTestState(t, stateDir, sessions)

	srv := NewFleet([]FleetProject{
		NewFleetProject("One", "/tmp/one.yaml", "", &config.Config{
			Repo:        "owner/one",
			StateDir:    stateDir,
			MaxParallel: 7,
		}),
	}, "127.0.0.1", 8786, true)
	resp := srv.snapshot()

	if len(resp.Projects) != 1 {
		t.Fatalf("projects len = %d, want 1", len(resp.Projects))
	}
	if len(resp.Projects[0].Active) != 6 {
		t.Fatalf("project card active len = %d, want capped 6", len(resp.Projects[0].Active))
	}
	if len(resp.Workers) != 7 {
		t.Fatalf("fleet workers len = %d, want all 7", len(resp.Workers))
	}
	if resp.Summary.NeedsAttention != 7 {
		t.Fatalf("needs attention = %d, want 7", resp.Summary.NeedsAttention)
	}
}

func TestFleetWorkersIncludeRecentlyCompletedDoneRows(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()
	stateDir := filepath.Join(dir, "state")
	finished := now.Add(-15 * time.Minute)
	saveFleetTestState(t, stateDir, map[string]*state.Session{
		"one-1": {
			IssueNumber: 1,
			IssueTitle:  "Done thing",
			Status:      state.StatusDone,
			StartedAt:   now.Add(-45 * time.Minute),
			FinishedAt:  &finished,
		},
	})

	srv := NewFleet([]FleetProject{
		NewFleetProject("One", "/tmp/one.yaml", "", &config.Config{
			Repo:        "owner/one",
			StateDir:    stateDir,
			MaxParallel: 1,
		}),
	}, "127.0.0.1", 8786, true)
	resp := srv.snapshot()

	if len(resp.Workers) != 1 {
		t.Fatalf("fleet workers len = %d, want recently completed worker", len(resp.Workers))
	}
	if resp.Workers[0].Status != string(state.StatusDone) {
		t.Fatalf("worker status = %q, want done", resp.Workers[0].Status)
	}
}

func TestFleetWorkersKeepHistoricalSessionsSearchableButOutOfDefaultScope(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()
	old := now.Add(-72 * time.Hour)
	stateDir := filepath.Join(dir, "state")
	sessions := make(map[string]*state.Session)
	for i := 1; i <= 55; i++ {
		finished := old.Add(-time.Duration(i) * time.Minute)
		slot := "hist-" + strconv.Itoa(i)
		sessions[slot] = &state.Session{
			IssueNumber: 1000 + i,
			IssueTitle:  "Historical done session",
			Status:      state.StatusDone,
			StartedAt:   finished.Add(-30 * time.Minute),
			FinishedAt:  fleetTimePtr(finished),
		}
	}
	recentFinished := now.Add(-15 * time.Minute)
	retryAt := now.Add(30 * time.Minute)
	sessions["live-running"] = &state.Session{
		IssueNumber: 1,
		IssueTitle:  "Running worker",
		Status:      state.StatusRunning,
		StartedAt:   now.Add(-time.Hour),
	}
	sessions["live-pr"] = &state.Session{
		IssueNumber: 2,
		IssueTitle:  "Open PR worker",
		Status:      state.StatusPROpen,
		StartedAt:   now.Add(-2 * time.Hour),
		PRNumber:    22,
	}
	sessions["live-retry"] = &state.Session{
		IssueNumber: 3,
		IssueTitle:  "Retry worker",
		Status:      state.StatusDead,
		StartedAt:   old,
		FinishedAt:  fleetTimePtr(old.Add(time.Hour)),
		NextRetryAt: &retryAt,
	}
	sessions["live-recent"] = &state.Session{
		IssueNumber: 4,
		IssueTitle:  "Recently completed worker",
		Status:      state.StatusDone,
		StartedAt:   now.Add(-45 * time.Minute),
		FinishedAt:  &recentFinished,
	}
	saveFleetTestState(t, stateDir, sessions)

	srv := NewFleet([]FleetProject{
		NewFleetProject("One", "/tmp/one.yaml", "", &config.Config{
			Repo:        "owner/one",
			StateDir:    stateDir,
			MaxParallel: 4,
		}),
	}, "127.0.0.1", 8786, true)
	resp := srv.snapshot()

	if len(resp.Workers) != 59 {
		t.Fatalf("fleet workers len = %d, want all 59 searchable sessions", len(resp.Workers))
	}
	project := findFleetProject(t, resp.Projects, "One")
	if project.Sessions != 59 {
		t.Fatalf("project sessions = %d, want 59", project.Sessions)
	}
	if len(project.Active) != 4 {
		t.Fatalf("project active len = %d, want live default set", len(project.Active))
	}

	defaultVisible := 0
	visibleAttention := 0
	historical := 0
	for _, worker := range resp.Workers {
		if worker.Live || worker.NeedsAttention {
			defaultVisible++
			if worker.NeedsAttention {
				visibleAttention++
			}
		} else {
			historical++
		}
	}
	if defaultVisible != 4 || historical != 55 {
		t.Fatalf("default/history counts = %d/%d, want 4/55", defaultVisible, historical)
	}
	if resp.Summary.NeedsAttention != visibleAttention {
		t.Fatalf("summary attention = %d, visible default attention = %d", resp.Summary.NeedsAttention, visibleAttention)
	}

	oldWorker := findFleetWorker(t, resp.Workers, "hist-1")
	if oldWorker.Live || oldWorker.NeedsAttention {
		t.Fatalf("old historical worker = %+v, want searchable but outside default live scope", oldWorker)
	}
	if !findFleetWorker(t, resp.Workers, "live-recent").Live {
		t.Fatal("recently changed done worker should stay in the default live scope")
	}
}

func TestFleetWorkerDetailIncludesMetadataAndLog(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()
	stateDir := filepath.Join(dir, "state")
	logFile := filepath.Join(dir, "logs", "one-1.log")
	if err := os.MkdirAll(filepath.Dir(logFile), 0o755); err != nil {
		t.Fatalf("create log dir: %v", err)
	}
	if err := os.WriteFile(logFile, []byte("line one\n\x1b[31mline two\x1b[0m\nline three\n"), 0o644); err != nil {
		t.Fatalf("write log: %v", err)
	}
	saveFleetTestState(t, stateDir, map[string]*state.Session{
		"one-1": {
			IssueNumber:     1,
			IssueTitle:      "Build thing",
			Status:          state.StatusRunning,
			StartedAt:       now.Add(-10 * time.Minute),
			Backend:         "opencode",
			Worktree:        filepath.Join(dir, "worktree"),
			Branch:          "maestro/one-1",
			PID:             999999,
			LogFile:         logFile,
			TokensUsedTotal: 1234,
		},
	})
	srv := NewFleet([]FleetProject{
		NewFleetProject("One", "/tmp/one.yaml", "http://127.0.0.1:8787", &config.Config{
			Repo:        "owner/one",
			StateDir:    stateDir,
			MaxParallel: 1,
		}),
	}, "127.0.0.1", 8786, true)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/fleet/worker?project=One&slot=one-1&lines=2", nil)
	w := httptest.NewRecorder()
	srv.handleFleetWorker(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}
	var resp fleetWorkerDetailResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	worker := resp.Worker
	if worker.ProjectName != "One" || worker.ProjectRepo != "owner/one" || worker.DashboardURL == "" {
		t.Fatalf("worker project metadata = %+v", worker)
	}
	if worker.Worktree == "" || worker.Branch != "maestro/one-1" {
		t.Fatalf("worker worktree/branch = %q/%q", worker.Worktree, worker.Branch)
	}
	if worker.Alive == nil || *worker.Alive {
		t.Fatalf("running worker should distinguish alive=false, got %#v", worker.Alive)
	}
	if !worker.NeedsAttention || !contains(worker.StatusReason, "PID is not alive") {
		t.Fatalf("worker attention reason = %q attention=%v", worker.StatusReason, worker.NeedsAttention)
	}
	if !worker.HasLog || !resp.Log.Available {
		t.Fatalf("log availability worker=%v log=%+v", worker.HasLog, resp.Log)
	}
	if contains(resp.Log.Text, "line one") || contains(resp.Log.Text, "\x1b") {
		t.Fatalf("log text should be tailed and ANSI-stripped: %q", resp.Log.Text)
	}
	if !contains(resp.Log.Text, "line two") || !contains(resp.Log.Text, "line three") {
		t.Fatalf("log text = %q, want recent lines", resp.Log.Text)
	}
	if resp.Log.Lines != 2 {
		t.Fatalf("log lines = %d, want actual tailed line count 2", resp.Log.Lines)
	}
}

func TestFleetWorkerDetailReportsActualLogLineCount(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()
	stateDir := filepath.Join(dir, "state")
	logFile := filepath.Join(dir, "logs", "one-1.log")
	if err := os.MkdirAll(filepath.Dir(logFile), 0o755); err != nil {
		t.Fatalf("create log dir: %v", err)
	}
	if err := os.WriteFile(logFile, []byte("line one\nline two\nline three\n"), 0o644); err != nil {
		t.Fatalf("write log: %v", err)
	}
	saveFleetTestState(t, stateDir, map[string]*state.Session{
		"one-1": {
			IssueNumber: 1,
			IssueTitle:  "Build thing",
			Status:      state.StatusDone,
			StartedAt:   now.Add(-10 * time.Minute),
			LogFile:     logFile,
		},
	})
	srv := NewFleet([]FleetProject{
		NewFleetProject("One", "/tmp/one.yaml", "", &config.Config{
			Repo:        "owner/one",
			StateDir:    stateDir,
			MaxParallel: 1,
		}),
	}, "127.0.0.1", 8786, true)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/fleet/worker?project=One&slot=one-1&lines=260", nil)
	w := httptest.NewRecorder()
	srv.handleFleetWorker(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}
	var resp fleetWorkerDetailResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Log.Lines != 3 {
		t.Fatalf("log lines = %d, want actual returned line count 3", resp.Log.Lines)
	}
}

func TestFleetWorkerDetailExplainsUnavailableLog(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()
	stateDir := filepath.Join(dir, "state")
	saveFleetTestState(t, stateDir, map[string]*state.Session{
		"one-1": {
			IssueNumber: 1,
			IssueTitle:  "Done thing",
			Status:      state.StatusDone,
			StartedAt:   now.Add(-20 * time.Minute),
		},
	})
	srv := NewFleet([]FleetProject{
		NewFleetProject("One", "/tmp/one.yaml", "", &config.Config{
			Repo:        "owner/one",
			StateDir:    stateDir,
			MaxParallel: 1,
		}),
	}, "127.0.0.1", 8786, true)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/fleet/worker?project=One&slot=one-1", nil)
	w := httptest.NewRecorder()
	srv.handleFleetWorker(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}
	var resp fleetWorkerDetailResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Worker.Status != string(state.StatusDone) {
		t.Fatalf("worker status = %q, want done", resp.Worker.Status)
	}
	if resp.Log.Available || resp.Log.Reason == "" {
		t.Fatalf("log should be unavailable with a reason: %+v", resp.Log)
	}
}

func TestFleetDashboard(t *testing.T) {
	srv := NewFleet(nil, "127.0.0.1", 8786, true)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	srv.handleFleetDashboard(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}
	body := w.Body.String() + web.MustReadStatic("tokens.css") + web.MustReadStatic("fleet.js") + web.MustReadStatic("fleet.css")
	if ct := w.Header().Get("Content-Type"); ct != "text/html; charset=utf-8" {
		t.Errorf("content-type = %q, want text/html", ct)
	}
	for _, want := range []string{
		"Maestro Fleet",
		"/api/v1/fleet",
		"/api/v1/fleet/worker",
		"<html data-theme=\"light\">",
		"/static/tokens.css",
		"/static/components.css",
		"/static/status-icons.svg",
		"/static/maestro-mark.svg",
		"/static/favicon-32.png",
		"/static/apple-touch-icon-180.png",
		"/static/og-1200x630.png",
		"Inter Tight",
		"JetBrains Mono",
		"#059669",
		"#0891b2",
		"color-scheme: light",
		"fleet-initial-state",
		"project-rail",
		"project-rail-body",
		"project-filter",
		"project-segments",
		"project-count-all",
		"project-count-running",
		"project-count-attention",
		"project-count-idle",
		"mode-pill",
		"fleet-refresh",
		"stat-label",
		"Projects",
		"Running",
		"Issue throughput",
		"merged · last 7d",
		"stat-sparkline-empty",
		"projectIsUnconfigured",
		"project-row--unconfigured",
		"rail-state-unconfigured",
		"badge-setup",
		"Set up &rarr;",
		"operator_sentence",
		"supervisorOperatorSentence",
		"supervisorDecisionMetaText",
		"Last activity",
		"Open",
		"fleet-verdict",
		"renderFleetVerdict",
		"verdict-healthy",
		"verdict-daemon-down",
		"approval-inbox",
		"approval-list",
		"approval-summary",
		"attention-inbox",
		"attention-list",
		"attention-summary",
		"fleet-workers-body",
		"worker-detail",
		"worker-drawer",
		"worker-detail-backdrop",
		"worker-detail-close",
		"worker-drawer-open",
		"closeWorkerDetail",
		"resolveWorkerQuery",
		"params.get(\"worker\")",
		"overflow-wrap: anywhere",
		"worker-controls",
		"worker-filter",
		"status-filter",
		"backend-filter",
		"pr-filter",
		"worker-sort",
		"sort-direction",
		"renderFleetWorkers",
		"renderApprovalInbox",
		"approvalsFromData",
		"approvalInboxSummaryText",
		"No active approvals need review.",
		"historical approval",
		"approval-list-compact",
		"Audit history",
		"approvalHistoryCountText",
		"approval-audit-link",
		"approval-history-link-card",
		"Open full approval audit",
		"Safe recommendation",
		"Start worker",
		".approval-card.approval-stale { border-left-color: var(--line);",
		".approval-card.approval-superseded { border-left-color: var(--line);",
		".a-stale { color: var(--muted);",
		".a-superseded { color: var(--muted);",
		"counts.superseded",
		"renderAttentionInbox",
		"attentionFromData",
		"if (!Array.isArray(data.attention) && Array.isArray(data.workers))",
		"No projects need attention right now",
		"renderProjectRail",
		"projectRailRowHTML",
		"projectOpenRailHTML",
		"needs-you-rail",
		"renderNeedsYouRail",
		"needs-you-item",
		"fleet-verdict-headline",
		"stat-value",
		"workerControlsEl.hidden",
		"project-diagnostics-note",
		"This is the raw inspector layer.",
		"projectSearchText",
		"renderWorkerDetail",
		"renderProject",
		"issueSummaryHTML",
		"project-worker-status { width: 124px;",
		"issue-main",
		"issue-title",
		"Why Attention",
		"Why Not Running",
		"Queue Snapshot",
		"Outcome Status",
		"outcomeHTML",
		"No outcome brief configured",
		"queueSnapshotHTML",
		"queue-snapshot",
		"held/meta",
		"blocked-deps",
		"blocked by open dependencies",
		"next_action",
		"sortWorkers",
		"filteredWorkers",
		"URLSearchParams",
		"Last refresh",
		"projectFreshnessHTML",
		"badge-stale",
		"State error",
		"renderActions",
		"actionDetailHTML",
		"manual approval required",
		"Scope",
		"Target",
		"Approval",
		"Disabled",
		"replace(/^Would\\s+/i",
		"Approval-gated controls",
	} {
		if !contains(body, want) {
			t.Fatalf("dashboard should contain %q", want)
		}
	}
	for _, unwanted := range []string{`id="project-tabs"`, `class="project-tabs"`, "renderProjectTabs"} {
		if contains(body, unwanted) {
			t.Fatalf("dashboard should not render project tab navigation %q", unwanted)
		}
	}
	for _, oldAlarm := range []string{
		".approval-card.approval-stale { border-left-color: var(--bad);",
		".a-stale { color: var(--bad);",
	} {
		if contains(body, oldAlarm) {
			t.Fatalf("dashboard should not render stale approval history with alarming styling %q", oldAlarm)
		}
	}
}

func TestFleetDashboardRendersHistoryCollapseControls(t *testing.T) {
	body := fleetDashboardBody(t)
	for _, want := range []string{
		"function historySummaryRowHTML(workers)",
		"class=\"history-row\"",
		"data-history-scope=\"recent\"",
		"history collapsed",
		"hasWorkerDrilldownFilters",
		"worker.live === true",
		"Search or switch scope to inspect every session.",
	} {
		if !contains(body, want) {
			t.Fatalf("dashboard history collapse renderer should contain %q", want)
		}
	}
}

func TestFleetDashboardCanClearProjectWorkerScope(t *testing.T) {
	body := fleetDashboardBody(t)
	for _, want := range []string{
		`id="worker-project-reset"`,
		"Show all projects",
		"workerProjectResetEl.hidden = !projectScoped",
		`workerProjectResetEl.addEventListener("click", clearWorkerProjectScope)`,
	} {
		if !contains(body, want) {
			t.Fatalf("dashboard worker scope reset should contain %q", want)
		}
	}

	clearScope := dashboardSnippet(t, body, "function clearWorkerProjectScope()", "projectFilterEl.addEventListener")
	for _, want := range []string{
		`fleetState.selectedProject = "all";`,
		"updateQueryState();",
		"renderFleetWorkers();",
	} {
		if !contains(clearScope, want) {
			t.Fatalf("clear project scope handler should contain %q in:\n%s", want, clearScope)
		}
	}
}

func TestFleetDashboardRendersReadOnlySearchPalette(t *testing.T) {
	body := fleetDashboardBody(t)
	for _, want := range []string{
		`id="fleet-search-trigger"`,
		`aria-controls="fleet-search-dialog"`,
		`id="fleet-search-dialog"`,
		`role="dialog"`,
		`id="fleet-search-input"`,
		`id="fleet-search-results"`,
		`role="listbox"`,
		"Cmd/Ctrl K",
		"Project slug, session slot, issue #, PR #, or dashboard",
		"No write actions run from search.",
		"buildFleetSearchIndex",
		"scoreFleetSearchResult",
		"selectFleetSearchResult",
		"fleet-search-open",
	} {
		if !contains(body, want) {
			t.Fatalf("dashboard search palette should contain %q", want)
		}
	}
}

func TestFleetDashboardSearchIndexUsesLoadedFleetData(t *testing.T) {
	body := fleetDashboardBody(t)
	indexSnippet := dashboardSnippet(t, body, "function buildFleetSearchIndex()", "function fuzzySearchMatch")
	for _, want := range []string{
		"for (const project of fleetState.projects || [])",
		"for (const worker of fleetState.workers || [])",
		"for (const approval of fleetState.approvals || [])",
		`kind: "Project"`,
		`kind: "Dashboard"`,
		`kind: "Session"`,
		`kind: "Issue"`,
		`kind: "PR"`,
		"project.dashboard_url",
		"const url = searchProjectURL(project);",
		"worker.slot",
		"worker.issue_number",
		"worker.pr_number",
		`searchNumberAliases("issue", worker.issue_number)`,
		`searchNumberAliases("pr", worker.pr_number)`,
		"approval.issue_number",
		"approval.pr_number",
	} {
		if !contains(indexSnippet, want) {
			t.Fatalf("search index should contain %q in:\n%s", want, indexSnippet)
		}
	}
}

func TestFleetDashboardSearchRanksDefaultResultsBeforeLimit(t *testing.T) {
	body := fleetDashboardBody(t)
	searchSnippet := dashboardSnippet(t, body, "function searchFleetResults(query)", "function searchResultID")
	for _, want := range []string{
		"const limit = searchTerms(query).length ? 12 : 10;",
		"scoreFleetSearchResult(result, query)",
		".sort((left, right) => {",
		".slice(0, limit)",
	} {
		if !contains(searchSnippet, want) {
			t.Fatalf("search results should contain %q in:\n%s", want, searchSnippet)
		}
	}
	if contains(searchSnippet, "if (!searchTerms(query).length) return index.slice(0, 10);") {
		t.Fatalf("default search results should be ranked before truncating in:\n%s", searchSnippet)
	}
}

func TestFleetDashboardSearchKeyboardAndSelectionAreReadOnly(t *testing.T) {
	body := fleetDashboardBody(t)
	for _, want := range []string{
		"function isSearchShortcut(event)",
		"(event.metaKey || event.ctrlKey)",
		`toLowerCase() === "k"`,
		"openSearchPalette();",
		`event.key === "ArrowDown"`,
		`event.key === "ArrowUp"`,
		`event.key === "Enter"`,
		`event.key === "Escape"`,
	} {
		if !contains(body, want) {
			t.Fatalf("search keyboard support should contain %q", want)
		}
	}

	inputKeydownSnippet := dashboardSnippet(t, body, `searchInputEl.addEventListener("keydown"`, `projectFilterEl.addEventListener`)
	if !contains(inputKeydownSnippet, "event.stopPropagation();") {
		t.Fatalf("search input Escape handler should stop propagation in:\n%s", inputKeydownSnippet)
	}

	selectionSnippet := dashboardSnippet(t, body, "function openSearchURL(url)", "function workerSearchText(worker)")
	for _, want := range []string{
		`window.open(target, "_blank", "noopener,noreferrer")`,
		"selectWorker(result.project, result.slot)",
		"scopeSearchProject(result.project)",
	} {
		if !contains(selectionSnippet, want) {
			t.Fatalf("search selection should contain %q in:\n%s", want, selectionSnippet)
		}
	}
	for _, unwanted := range []string{"fetch(", "/api/v1/fleet/actions", "renderActions", "action-btn", http.MethodPost} {
		if contains(selectionSnippet, unwanted) {
			t.Fatalf("search selection should not expose write behavior %q in:\n%s", unwanted, selectionSnippet)
		}
	}
}

func TestFleetDashboardServerRendersProjectRailFixtures(t *testing.T) {
	for _, tc := range []struct {
		name     string
		projects int
	}{
		{name: "zero", projects: 0},
		{name: "one", projects: 1},
		{name: "three", projects: 3},
		{name: "twelve", projects: 12},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := fleetDashboardBodyWithProjects(t, fleetDashboardFixtureProjects(t, tc.projects))
			rail := dashboardSnippet(t, body, `<tbody id="project-rail-body">`, `</tbody>`)

			for _, want := range []string{"Project", "State", "Queue", "PR", "Outcome", "Last activity", "Open"} {
				if !contains(body, want) {
					t.Fatalf("dashboard rail should contain column %q", want)
				}
			}
			if !contains(body, `id="fleet-initial-state"`) {
				t.Fatal("dashboard should embed the initial fleet snapshot for client hydration")
			}

			rows := strings.Count(rail, `class="project-rail-row`)
			if rows != tc.projects {
				t.Fatalf("server-rendered project rail rows = %d, want %d in:\n%s", rows, tc.projects, rail)
			}
			if tc.projects == 0 {
				if !contains(rail, "project-rail-empty") || !contains(rail, "No configured projects") {
					t.Fatalf("empty rail should render an explicit empty state, got:\n%s", rail)
				}
				return
			}

			for i := 1; i <= tc.projects; i++ {
				name := "Project " + strconv.Itoa(i)
				if !contains(rail, name) {
					t.Fatalf("rail should include %q in:\n%s", name, rail)
				}
			}
			for _, want := range []string{"ready", "Project 1 outcome", "Open", "&rarr;"} {
				if !contains(rail, want) {
					t.Fatalf("rail should communicate %q in:\n%s", want, rail)
				}
			}
			if tc.projects >= 10 && !contains(body, "project-rail-scroll") {
				t.Fatal("10+ project fixture should render inside the scrollable rail container")
			}
		})
	}
}

func TestFleetDashboardRendersUnconfiguredProjectAsSetupState(t *testing.T) {
	stateDir := t.TempDir()
	saveFleetTestSnapshot(t, stateDir, map[string]*state.Session{}, nil)
	projectConfig := &config.Config{
		Repo:        "owner/setup-needed",
		StateDir:    stateDir,
		MaxParallel: 1,
		Server:      config.ServerConfig{ReadOnly: true},
	}
	srv := NewFleet([]FleetProject{
		NewFleetProject("Setup Needed", "/tmp/setup-needed.yaml", "http://127.0.0.1:8787", projectConfig),
	}, "127.0.0.1", 8786, true)
	snapshot := srv.snapshot()
	project := findFleetProject(t, snapshot.Projects, "Setup Needed")

	if !fleetProjectUnconfigured(project) {
		t.Fatalf("project should be treated as unconfigured: %+v", project.Outcome)
	}
	row := renderFleetProjectRailRow(project)
	for _, want := range []string{
		"project-row--unconfigured",
		"project-row-unconfigured",
		"rail-state-unconfigured",
		"setup",
		"No outcome brief configured",
		"Set up &rarr;",
		"setup-link",
	} {
		if !contains(row, want) {
			t.Fatalf("unconfigured rail row should contain %q, got:\n%s", want, row)
		}
	}
	if outcomeHTML := renderFleetProjectRailOutcome(project); contains(outcomeHTML, `<span class="pill`) {
		t.Fatalf("unconfigured outcome rail should not render a health pill, got:\n%s", outcomeHTML)
	}
}

func TestFleetDashboardProjectRailPlaceholdersAreNotReplacedFromProjectData(t *testing.T) {
	snapshot := fleetResponse{
		Projects: []fleetProjectState{{
			Name:         "{{FLEET_INITIAL_STATE}}",
			Repo:         "{{FLEET_PROJECT_RAIL_SUMMARY}}",
			ConfigPath:   "{{FLEET_PROJECT_RAIL_ROWS}}",
			DashboardURL: "http://127.0.0.1:8787",
			MaxParallel:  1,
			Outcome: outcome.Status{
				Configured:  true,
				Goal:        "{{FLEET_PROJECT_RAIL_SUMMARY}}",
				HealthState: outcome.HealthUnknown,
			},
			QueueSnapshot: &fleetQueueSnapshot{Open: 1, Eligible: 1},
			Freshness:     fleetProjectFreshness{SnapshotAge: "1m0s"},
		}},
	}
	body, err := renderFleetDashboardHTML(snapshot)
	if err != nil {
		t.Fatalf("render dashboard: %v", err)
	}

	summary := dashboardSnippet(t, body, `<div class="section-note" id="project-rail-summary">`, `</div>`)
	if !contains(summary, "1 project · 0 active · 0 attention") {
		t.Fatalf("summary placeholder was not replaced correctly, got:\n%s", summary)
	}
	rail := dashboardSnippet(t, body, `<tbody id="project-rail-body">`, `</tbody>`)
	if !contains(rail, "{{FLEET_INITIAL_STATE}}") || !contains(rail, "{{FLEET_PROJECT_RAIL_SUMMARY}}") {
		t.Fatalf("rail should preserve placeholder-like project text as data, got:\n%s", rail)
	}

	startMarker := `<script type="application/json" id="fleet-initial-state">`
	script := dashboardSnippet(t, body, startMarker, `</script>`)
	var decoded fleetResponse
	if err := json.Unmarshal([]byte(strings.TrimPrefix(script, startMarker)), &decoded); err != nil {
		t.Fatalf("initial state should remain valid JSON: %v\n%s", err, script)
	}
	if len(decoded.Projects) != 1 || decoded.Projects[0].Name != "{{FLEET_INITIAL_STATE}}" {
		t.Fatalf("initial state project data changed: %+v", decoded.Projects)
	}
}

func TestFleetDashboardServesFleetPath(t *testing.T) {
	srv := NewFleet(nil, "127.0.0.1", 8786, true)
	req := httptest.NewRequest(http.MethodGet, "/fleet", nil)
	w := httptest.NewRecorder()
	srv.handleFleetDashboard(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}
	if !contains(w.Body.String(), "Projects") {
		t.Fatal("/fleet should serve the fleet dashboard")
	}
}

func TestFleetDashboardServesApprovalAuditPath(t *testing.T) {
	srv := NewFleet(nil, "127.0.0.1", 8786, true)
	req := httptest.NewRequest(http.MethodGet, "/approvals/audit", nil)
	w := httptest.NewRecorder()
	srv.handleFleetApprovalAudit(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}
	if !contains(w.Body.String(), "Historical Approvals") || !contains(w.Body.String(), "Back to Fleet") {
		t.Fatalf("approval audit should render dedicated audit page, got:\n%s", w.Body.String())
	}
}

func TestFleetDashboardReadOnlyProjectControlsRenderQuietNote(t *testing.T) {
	body := fleetDashboardBody(t)
	readOnlyBranch := dashboardSnippet(t, body,
		"if (project.read_only === true || fleetState.readOnly)",
		"return '<div class=\"project-actions\"><div class=\"label\">Approval-gated controls</div>'")

	if !contains(readOnlyBranch, "Write controls disabled in read-only mode.") {
		t.Fatalf("read-only project controls should render disabled-write footer, got:\n%s", readOnlyBranch)
	}
	if !contains(readOnlyBranch, "project-actions-readonly") {
		t.Fatalf("read-only project controls should use project-actions-readonly class, got:\n%s", readOnlyBranch)
	}
	for _, unwanted := range []string{"action-btn", "<button", "renderActions("} {
		if contains(readOnlyBranch, unwanted) {
			t.Fatalf("read-only project controls should not render button-like controls %q in:\n%s", unwanted, readOnlyBranch)
		}
	}
}

func TestFleetDashboardWritableProjectControlsKeepApprovalDiagnostics(t *testing.T) {
	body := fleetDashboardBody(t)
	writableBranch := dashboardSnippet(t, body,
		"return '<div class=\"project-actions\"><div class=\"label\">Approval-gated controls</div>'",
		"function projectFreshnessHTML")

	for _, want := range []string{
		"Approval-gated controls",
		"renderActions(project.actions || [], { details: false })",
	} {
		if !contains(writableBranch, want) {
			t.Fatalf("writable project controls should preserve %q in:\n%s", want, writableBranch)
		}
	}
}

func fleetDashboardBody(t *testing.T) string {
	t.Helper()
	return fleetDashboardBodyWithProjects(t, nil)
}

func fleetDashboardBodyWithProjects(t *testing.T, projects []FleetProject) string {
	t.Helper()
	srv := NewFleet(projects, "127.0.0.1", 8786, true)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	srv.handleFleetDashboard(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}
	return w.Body.String() + web.MustReadStatic("tokens.css") + web.MustReadStatic("fleet.js") + web.MustReadStatic("fleet.css")
}

func fleetDashboardFixtureProjects(t *testing.T, count int) []FleetProject {
	t.Helper()
	if count == 0 {
		return nil
	}
	dir := t.TempDir()
	now := time.Now().UTC()
	projects := make([]FleetProject, 0, count)
	for i := 1; i <= count; i++ {
		idx := strconv.Itoa(i)
		name := "Project " + idx
		stateDir := filepath.Join(dir, "project-"+idx)
		status := state.StatusDone
		prNumber := 0
		if i%2 == 0 {
			status = state.StatusPROpen
			prNumber = 100 + i
		}
		if i%3 == 0 {
			status = state.StatusRunning
		}
		sessions := map[string]*state.Session{
			"slot-" + idx: {
				IssueNumber: i,
				IssueTitle:  "Issue " + idx,
				Status:      status,
				StartedAt:   now.Add(-time.Duration(i) * time.Minute),
				PRNumber:    prNumber,
			},
		}
		decisions := []state.SupervisorDecision{{
			ID:                "decision-" + idx,
			CreatedAt:         now.Add(-time.Duration(i) * time.Minute),
			Summary:           "Queue snapshot for " + name,
			RecommendedAction: "none",
			Risk:              "low",
			QueueAnalysis: &state.SupervisorQueueAnalysis{
				OpenIssues:                    i + 2,
				EligibleCandidates:            1,
				ExcludedIssues:                i % 3,
				HeldIssues:                    i % 2,
				BlockedByDependencyIssues:     i % 4,
				NonRunnableProjectStatusCount: i % 2,
				SelectedCandidate: &state.SupervisorIssueCandidate{
					Number: i,
					Title:  "Issue " + idx,
				},
			},
		}}
		saveFleetTestSnapshot(t, stateDir, sessions, decisions)
		projects = append(projects, NewFleetProject(name, "/tmp/project-"+idx+".yaml", "http://127.0.0.1:878"+idx, &config.Config{
			Repo:        "owner/project-" + idx,
			StateDir:    stateDir,
			MaxParallel: 2,
			Outcome: outcome.Brief{
				DesiredOutcome: name + " outcome",
				RuntimeTarget:  "https://project-" + idx + ".example.com",
			},
			Server: config.ServerConfig{ReadOnly: true},
		}))
	}
	return projects
}

func dashboardSnippet(t *testing.T, body, startMarker, endMarker string) string {
	t.Helper()
	start := strings.Index(body, startMarker)
	if start < 0 {
		t.Fatalf("dashboard missing start marker %q", startMarker)
	}
	rest := body[start:]
	end := strings.Index(rest, endMarker)
	if end < 0 {
		t.Fatalf("dashboard missing end marker %q after %q", endMarker, startMarker)
	}
	return rest[:end]
}

func TestFleetActionReadOnlyRejectsMutation(t *testing.T) {
	srv := NewFleet(nil, "127.0.0.1", 8786, true)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/fleet/actions", nil)
	w := httptest.NewRecorder()
	srv.handleFleetAction(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusForbidden)
	}
	if !contains(w.Body.String(), "read-only") {
		t.Fatalf("response = %q, want read-only explanation", w.Body.String())
	}
}

func TestFleetActionProjectReadOnlyRejectsMutation(t *testing.T) {
	srv := NewFleet([]FleetProject{
		NewFleetProject("One", "/tmp/one.yaml", "", &config.Config{
			Repo:   "owner/one",
			Server: config.ServerConfig{ReadOnly: true},
		}),
	}, "127.0.0.1", 8786, false)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/fleet/actions", bytes.NewBufferString(`{"action_id":"restart_worker","project":"One","slot":"one-1"}`))
	w := httptest.NewRecorder()
	srv.handleFleetAction(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusForbidden)
	}
	if !contains(w.Body.String(), "read-only") {
		t.Fatalf("response = %q, want read-only explanation", w.Body.String())
	}
}

func findFleetWorker(t *testing.T, workers []fleetWorkerState, slot string) fleetWorkerState {
	t.Helper()
	for _, worker := range workers {
		if worker.Slot == slot {
			return worker
		}
	}
	t.Fatalf("worker %q not found in %+v", slot, workers)
	return fleetWorkerState{}
}

func findFleetProject(t *testing.T, projects []fleetProjectState, name string) fleetProjectState {
	t.Helper()
	for _, project := range projects {
		if project.Name == name {
			return project
		}
	}
	t.Fatalf("project %q not found in %+v", name, projects)
	return fleetProjectState{}
}

func findFleetApproval(t *testing.T, approvals []fleetApprovalState, id string) fleetApprovalState {
	t.Helper()
	for _, approval := range approvals {
		if approval.ID == id {
			return approval
		}
	}
	t.Fatalf("approval %q not found in %+v", id, approvals)
	return fleetApprovalState{}
}

func saveFleetTestState(t *testing.T, dir string, sessions map[string]*state.Session) {
	t.Helper()
	saveFleetTestSnapshot(t, dir, sessions, nil)
}

func saveFleetTestSnapshot(t *testing.T, dir string, sessions map[string]*state.Session, decisions []state.SupervisorDecision) {
	t.Helper()
	st := state.NewState()
	for name, sess := range sessions {
		st.Sessions[name] = sess
	}
	for _, decision := range decisions {
		st.RecordSupervisorDecision(decision, state.DefaultSupervisorDecisionLimit)
	}
	if err := state.Save(dir, st); err != nil {
		t.Fatalf("save state: %v", err)
	}
}

func fleetTimePtr(t time.Time) *time.Time {
	return &t
}

func assertFleetReadOnlyAction(t *testing.T, action controlAction) {
	t.Helper()
	wantLabels := map[string]string{
		"restart_worker":     "Restart",
		"stop_worker":        "Stop",
		"mark_issue_ready":   "Mark ready",
		"mark_issue_blocked": "Mark blocked",
		"approve_merge":      "Approve merge",
	}
	if want, ok := wantLabels[action.ID]; ok && action.Label != want {
		t.Fatalf("action %s label = %q, want %q", action.ID, action.Label, want)
	}
	if len(action.Label) > len("Approve merge") {
		t.Fatalf("action %s label = %q, want concise non-wrapping label", action.ID, action.Label)
	}
	if action.Description == "" {
		t.Fatalf("action %+v should describe the disabled operation", action)
	}
	if action.Scope == "" || action.Target == "" {
		t.Fatalf("action %+v should include scope and target metadata", action)
	}
	if !action.Mutating || !action.RequiresApproval || !action.Disabled {
		t.Fatalf("action %+v should be disabled mutating approval affordance", action)
	}
	if action.ApprovalPolicy != controlApprovalPolicyManual {
		t.Fatalf("approval policy = %q, want %q", action.ApprovalPolicy, controlApprovalPolicyManual)
	}
	if !contains(action.DisabledReason, "Read-only mode") {
		t.Fatalf("disabled reason = %q, want read-only explanation", action.DisabledReason)
	}
	if action.Method != http.MethodPost || action.Endpoint != "/api/v1/fleet/actions" {
		t.Fatalf("action endpoint = %s %s, want POST /api/v1/fleet/actions", action.Method, action.Endpoint)
	}
}
