package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/befeast/maestro/internal/config"
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
			Status:          state.StatusPROpen,
			StartedAt:       now.Add(-2 * time.Minute),
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
	})

	projects := []FleetProject{
		NewFleetProject("One", "/tmp/one.yaml", "http://127.0.0.1:8787", &config.Config{
			Repo:        "owner/one",
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
	if resp.Summary.Projects != 2 || resp.Summary.Running != 1 || resp.Summary.PROpen != 1 || resp.Summary.Failed != 1 || resp.Summary.Sessions != 3 || resp.Summary.NeedsAttention != 2 {
		t.Fatalf("unexpected summary: %+v", resp.Summary)
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
	if len(resp.Workers) != 3 {
		t.Fatalf("fleet workers len = %d, want 3", len(resp.Workers))
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
			PolicyRule:         "supervisor.dynamic_wave",
			OpenIssues:         11,
			EligibleCandidates: 0,
			ExcludedIssues:     11,
			SkippedReasons: []string{
				"Issue #24 skipped by dynamic wave policy: excluded by label \"blocked\"",
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
	if excluded.QueueSnapshot.Open != 11 || excluded.QueueSnapshot.Eligible != 0 || excluded.QueueSnapshot.Excluded != 11 {
		t.Fatalf("excluded queue snapshot = %+v, want open=11 eligible=0 excluded=11", excluded.QueueSnapshot)
	}
	if !contains(excluded.QueueSnapshot.IdleReason, "Policy excluded all 11 open issues") {
		t.Fatalf("idle reason = %q, want all-excluded explanation", excluded.QueueSnapshot.IdleReason)
	}
	if !contains(excluded.QueueSnapshot.TopSkippedReason, "excluded by label") {
		t.Fatalf("top skipped reason = %q, want excluded label reason", excluded.QueueSnapshot.TopSkippedReason)
	}
	if excluded.Supervisor.Latest == nil || excluded.Supervisor.Latest.QueueAnalysis == nil || excluded.Supervisor.Latest.QueueAnalysis.OpenIssues != 11 {
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
	if resp.Summary.Approvals != 4 || resp.Summary.ApprovalsPending != 1 || resp.Summary.ApprovalsStale != 1 || resp.Summary.ApprovalsApproved != 1 || resp.Summary.ApprovalsRejected != 1 {
		t.Fatalf("approval summary = %+v, want one per lifecycle status", resp.Summary)
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
	body := w.Body.String()
	if ct := w.Header().Get("Content-Type"); ct != "text/html; charset=utf-8" {
		t.Errorf("content-type = %q, want text/html", ct)
	}
	for _, want := range []string{
		"Maestro Fleet",
		"/api/v1/fleet",
		"/api/v1/fleet/worker",
		"project-tabs",
		"approval-inbox",
		"approval-list",
		"approval-summary",
		"attention-inbox",
		"attention-list",
		"attention-summary",
		"fleet-workers-body",
		"worker-detail",
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
		"<details class=\"approval-history\"' + (historyWasOpen ? ' open' : '') + '>",
		"const historyWasOpen = historyDetails ? historyDetails.open : false;",
		"(historyWasOpen ? ' open' : '')",
		".approval-card.approval-stale { border-left-color: var(--line);",
		".a-stale { color: var(--muted);",
		"renderAttentionInbox",
		"attentionFromData",
		"if (!Array.isArray(data.attention) && Array.isArray(data.workers))",
		"No projects need attention right now",
		"renderWorkerDetail",
		"renderProject",
		"issueSummaryHTML",
		"project-worker-status { width: 124px;",
		"issue-main",
		"issue-title",
		"Why Attention",
		"Why Not Running",
		"Queue Snapshot",
		"queueSnapshotHTML",
		"queue-snapshot",
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
	for _, oldAlarm := range []string{
		".approval-card.approval-stale { border-left-color: var(--bad);",
		".a-stale { color: var(--bad);",
	} {
		if contains(body, oldAlarm) {
			t.Fatalf("dashboard should not render stale approval history with alarming styling %q", oldAlarm)
		}
	}
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
	st := state.NewState()
	for name, sess := range sessions {
		st.Sessions[name] = sess
	}
	if err := state.Save(dir, st); err != nil {
		t.Fatalf("save state: %v", err)
	}
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
