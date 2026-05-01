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
		"renderAttentionInbox",
		"attentionFromData",
		"pending · ",
		"stale · ",
		"approved · ",
		"rejected",
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
		"next_action",
		"sortWorkers",
		"filteredWorkers",
		"URLSearchParams",
		"renderActions",
		"Approval-gated controls",
	} {
		if !contains(body, want) {
			t.Fatalf("dashboard should contain %q", want)
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
	if !action.Mutating || !action.RequiresApproval || !action.Disabled {
		t.Fatalf("action %+v should be disabled mutating approval affordance", action)
	}
	if !contains(action.DisabledReason, "Read-only mode") {
		t.Fatalf("disabled reason = %q, want read-only explanation", action.DisabledReason)
	}
	if action.Method != http.MethodPost || action.Endpoint != "/api/v1/fleet/actions" {
		t.Fatalf("action endpoint = %s %s, want POST /api/v1/fleet/actions", action.Method, action.Endpoint)
	}
}
