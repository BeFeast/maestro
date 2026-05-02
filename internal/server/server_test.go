package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/outcome"
	"github.com/befeast/maestro/internal/server/web"
	"github.com/befeast/maestro/internal/state"
)

func setupTestServer(t *testing.T) (*Server, *config.Config) {
	t.Helper()
	dir := t.TempDir()

	cfg := &config.Config{
		Repo: "test/repo",
		Outcome: outcome.Brief{
			DesiredOutcome: "Test repo is healthy",
			RuntimeTarget:  "https://test.example.com",
			HealthcheckURL: "https://test.example.com/healthz",
		},
		MaxParallel: 3,
		StateDir:    dir,
		Server:      config.ServerConfig{Port: 8765},
	}

	// Write test state
	st := state.NewState()
	now := time.Now().UTC()
	logFile := filepath.Join(dir, "logs", "slot-1.log")
	if err := os.MkdirAll(filepath.Dir(logFile), 0755); err != nil {
		t.Fatalf("create log dir: %v", err)
	}
	if err := os.WriteFile(logFile, []byte("line one\nline two\nline three\n"), 0644); err != nil {
		t.Fatalf("write test log: %v", err)
	}
	st.Sessions["slot-1"] = &state.Session{
		IssueNumber:     42,
		IssueTitle:      "Fix bug",
		Status:          state.StatusRunning,
		Backend:         "claude",
		Branch:          "feat/slot-1-42-fix-bug",
		Worktree:        "/tmp/worktrees/slot-1",
		StartedAt:       now.Add(-10 * time.Minute),
		TokensUsedTotal: 5000,
		PID:             999999,
		LogFile:         logFile,
	}
	finished := now.Add(-5 * time.Minute)
	st.Sessions["slot-2"] = &state.Session{
		IssueNumber:     43,
		IssueTitle:      "Add feature",
		Status:          state.StatusPROpen,
		Backend:         "codex",
		Branch:          "feat/slot-2-43-add-feature",
		Worktree:        "/tmp/worktrees/slot-2",
		StartedAt:       now.Add(-30 * time.Minute),
		FinishedAt:      &finished,
		PRNumber:        10,
		TokensUsedTotal: 8000,
	}
	st.Sessions["slot-3"] = &state.Session{
		IssueNumber:     44,
		IssueTitle:      "Refactor code",
		Status:          state.StatusDone,
		Backend:         "claude",
		Branch:          "feat/slot-3-44-refactor-code",
		StartedAt:       now.Add(-1 * time.Hour),
		FinishedAt:      &finished,
		PRNumber:        11,
		TokensUsedTotal: 3000,
	}
	st.RecordSupervisorDecision(state.SupervisorDecision{
		ID:                "sup-test",
		CreatedAt:         now,
		Project:           "test/repo",
		Mode:              "read_only",
		Summary:           "No eligible issues.",
		RecommendedAction: "none",
		Risk:              "safe",
		Confidence:        0.8,
		StuckStates: []state.SupervisorStuckState{
			{
				Code:              "no_eligible_issues",
				Severity:          "warning",
				Summary:           "No open issues match the configured ready labels.",
				RecommendedAction: "Add a ready label or update config.",
				SupervisorCanAct:  true,
			},
		},
	}, state.DefaultSupervisorDecisionLimit)

	if err := state.Save(dir, st); err != nil {
		t.Fatalf("save test state: %v", err)
	}

	refreshCh := make(chan struct{}, 1)
	srv := New(cfg, refreshCh)
	return srv, cfg
}

func TestHandleState(t *testing.T) {
	srv, _ := setupTestServer(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/state", nil)
	w := httptest.NewRecorder()
	srv.handleState(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}

	var resp stateResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if resp.Repo != "test/repo" {
		t.Errorf("repo = %q, want %q", resp.Repo, "test/repo")
	}
	if resp.MaxParallel != 3 {
		t.Errorf("max_parallel = %d, want 3", resp.MaxParallel)
	}
	if !resp.Outcome.Configured || resp.Outcome.Goal != "Test repo is healthy" || resp.Outcome.HealthState != outcome.HealthUnknown {
		t.Fatalf("outcome = %+v, want configured unknown health", resp.Outcome)
	}
	if len(resp.Running) != 1 {
		t.Errorf("running sessions = %d, want 1", len(resp.Running))
	}
	if len(resp.All) != 3 {
		t.Errorf("all sessions = %d, want 3", len(resp.All))
	}
	if len(resp.PROpen) != 1 {
		t.Errorf("pr_open sessions = %d, want 1", len(resp.PROpen))
	}
	if resp.TokenTotals.Total != 16000 {
		t.Errorf("total tokens = %d, want 16000", resp.TokenTotals.Total)
	}
	if resp.TokenTotals.Active != 13000 {
		t.Errorf("active tokens = %d, want 13000", resp.TokenTotals.Active)
	}
	if resp.Running[0].IssueURL != "https://github.com/test/repo/issues/42" {
		t.Errorf("issue_url = %q", resp.Running[0].IssueURL)
	}
	if resp.Running[0].Alive == nil || *resp.Running[0].Alive {
		t.Fatalf("running worker should expose alive=false for dead test PID")
	}
	if !resp.Running[0].NeedsAttention {
		t.Error("running worker with alive=false should need attention")
	}
	if !contains(resp.Running[0].StatusReason, "PID is not alive") {
		t.Errorf("status_reason = %q, want dead PID hint", resp.Running[0].StatusReason)
	}
	if !contains(resp.Running[0].NextAction, "reconciliation cycle") {
		t.Errorf("next_action = %q, want reconciliation guidance", resp.Running[0].NextAction)
	}
	if resp.PROpen[0].PRURL != "https://github.com/test/repo/pull/10" {
		t.Errorf("pr_url = %q", resp.PROpen[0].PRURL)
	}
	if len(resp.StuckStates) != 1 || resp.StuckStates[0].Code != "no_eligible_issues" {
		t.Fatalf("stuck_states = %#v, want latest no_eligible_issues", resp.StuckStates)
	}
	if resp.SupervisorLatest == nil || len(resp.SupervisorLatest.StuckStates) != 1 {
		t.Fatalf("supervisor_latest stuck states missing: %#v", resp.SupervisorLatest)
	}
	if !resp.Supervisor.HasRun {
		t.Fatal("supervisor.has_run = false, want true")
	}
	if resp.Supervisor.Latest == nil || resp.Supervisor.Latest.ID != "sup-test" {
		t.Fatalf("supervisor.latest = %#v, want sup-test", resp.Supervisor.Latest)
	}
	if len(resp.Supervisor.Latest.StuckReasons) != 1 {
		t.Fatalf("supervisor latest stuck reasons = %#v, want one", resp.Supervisor.Latest.StuckReasons)
	}
}

func TestHandleStateReviewRetryLifecycleDisplay(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{
		Repo:        "test/repo",
		MaxParallel: 3,
		StateDir:    dir,
	}
	now := time.Now().UTC()
	retryAt := now.Add(10 * time.Minute)
	st := state.NewState()
	st.Sessions["slot-backoff"] = &state.Session{
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
	}
	st.Sessions["slot-recheck"] = &state.Session{
		IssueNumber: 43,
		IssueTitle:  "Wait for recheck",
		Status:      state.StatusPROpen,
		StartedAt:   now.Add(-30 * time.Minute),
		PRNumber:    13,
		RetryCount:  1,
		RetryReason: state.RetryReasonReviewFeedback,
	}
	st.Sessions["slot-ci-retry"] = &state.Session{
		IssueNumber:                 44,
		IssueTitle:                  "Retry failing checks",
		Status:                      state.StatusDead,
		StartedAt:                   now.Add(-40 * time.Minute),
		FinishedAt:                  &now,
		RetryCount:                  1,
		NextRetryAt:                 &retryAt,
		PreviousAttemptFeedbackKind: state.RetryReasonReviewFeedback,
		CIFailureOutput:             "checks failed",
	}
	if err := state.Save(dir, st); err != nil {
		t.Fatalf("save state: %v", err)
	}

	resp := buildStateResponse(cfg, st)
	backoff := findSessionInfo(t, resp.All, "slot-backoff")
	if backoff.DisplayStatus != string(state.DisplayReviewRetryBackoff) {
		t.Fatalf("backoff display_status = %q, want review retry backoff", backoff.DisplayStatus)
	}
	if backoff.NeedsAttention {
		t.Fatal("review retry backoff should not need attention")
	}
	if !contains(backoff.StatusReason, "waiting for the retry backoff") || !contains(backoff.NextAction, "scheduled retry worker") {
		t.Fatalf("backoff why = %q / %q, want retry worker wording", backoff.StatusReason, backoff.NextAction)
	}
	recheck := findSessionInfo(t, resp.All, "slot-recheck")
	if recheck.DisplayStatus != string(state.DisplayReviewRetryRecheck) {
		t.Fatalf("recheck display_status = %q, want review retry recheck", recheck.DisplayStatus)
	}
	if !contains(recheck.StatusReason, "waiting for CI, Greptile, or the merge gate") {
		t.Fatalf("recheck status_reason = %q, want CI/Greptile/merge gate wording", recheck.StatusReason)
	}
	ciRetry := findSessionInfo(t, resp.All, "slot-ci-retry")
	if ciRetry.DisplayStatus != "" {
		t.Fatalf("ci retry display_status = %q, want raw dead state", ciRetry.DisplayStatus)
	}
	if !ciRetry.NeedsAttention || !contains(ciRetry.StatusReason, "retry is scheduled") {
		t.Fatalf("ci retry why = %q / attention %v, want dead retry guidance", ciRetry.StatusReason, ciRetry.NeedsAttention)
	}
	if resp.Summary[string(state.StatusDead)] != 1 {
		t.Fatalf("summary dead = %d, want CI retry counted as dead", resp.Summary[string(state.StatusDead)])
	}
	if resp.Summary[string(state.DisplayReviewRetryBackoff)] != 1 {
		t.Fatalf("summary review_retry_backoff = %d, want 1", resp.Summary[string(state.DisplayReviewRetryBackoff)])
	}
}

func TestHandleState_ReadOnlyActionsDisabled(t *testing.T) {
	srv, cfg := setupTestServer(t)
	cfg.Server.ReadOnly = true

	req := httptest.NewRequest(http.MethodGet, "/api/v1/state", nil)
	w := httptest.NewRecorder()
	srv.handleState(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}
	var resp stateResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !resp.ReadOnly {
		t.Fatal("read_only = false, want true")
	}
	if len(resp.Actions) != 2 {
		t.Fatalf("project actions = %d, want 2", len(resp.Actions))
	}
	for _, action := range resp.Actions {
		assertReadOnlyAction(t, action)
	}

	worker := findSessionInfo(t, resp.All, "slot-2")
	if len(worker.Actions) != 5 {
		t.Fatalf("worker actions = %d, want 5", len(worker.Actions))
	}
	for _, action := range worker.Actions {
		assertReadOnlyAction(t, action)
	}
	approve := findControlAction(t, worker.Actions, "approve_merge")
	if approve.PRNumber != 10 {
		t.Fatalf("approve_merge pr = %d, want 10", approve.PRNumber)
	}
	workerWithoutPR := findSessionInfo(t, resp.All, "slot-1")
	approveWithoutPR := findControlAction(t, workerWithoutPR.Actions, "approve_merge")
	assertReadOnlyAction(t, approveWithoutPR)
	if !contains(approveWithoutPR.DisabledReason, "no PR") {
		t.Fatalf("approve_merge without PR reason = %q, want target-specific no-PR explanation", approveWithoutPR.DisabledReason)
	}
}

func TestHandleStateSupervisorRationale(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{
		Repo:        "test/repo",
		MaxParallel: 3,
		StateDir:    dir,
		Supervisor: config.SupervisorConfig{
			OrderedQueue: config.SupervisorOrderedQueueConfig{Issues: []int{42, 43}},
		},
		Server: config.ServerConfig{Port: 8765},
	}
	now := time.Date(2026, 4, 29, 12, 0, 0, 0, time.UTC)
	st := state.NewState()
	st.Sessions["slot-1"] = &state.Session{
		IssueNumber: 42,
		IssueTitle:  "first PR",
		Status:      state.StatusPROpen,
		StartedAt:   now.Add(-2 * time.Hour),
		PRNumber:    12,
	}
	st.Sessions["slot-2"] = &state.Session{
		IssueNumber: 43,
		IssueTitle:  "second PR",
		Status:      state.StatusQueued,
		StartedAt:   now.Add(-1 * time.Hour),
		PRNumber:    20,
	}
	st.RecordSupervisorDecision(state.SupervisorDecision{
		ID:                "sup-safe",
		CreatedAt:         now.Add(-30 * time.Minute),
		Project:           "test/repo",
		Mode:              "read_only",
		Summary:           "Session slot-1 already has open PR #12.",
		RecommendedAction: "monitor_open_pr",
		Target:            &state.SupervisorTarget{Issue: 42, PR: 12, Session: "slot-1"},
		Risk:              "safe",
		Confidence:        0.9,
		Reasons:           []string{"Session slot-1 is associated with open PR #12"},
		ProjectState:      state.SupervisorProjectState{Sessions: 2, PROpen: 1, Queued: 1, OpenPRs: 2},
	}, state.DefaultSupervisorDecisionLimit)
	st.RecordSupervisorDecision(state.SupervisorDecision{
		ID:                "sup-latest",
		CreatedAt:         now,
		Project:           "test/repo",
		Mode:              "read_only",
		Summary:           "Issue #43 exhausted its retry budget and needs manual review.",
		RecommendedAction: "review_retry_exhausted",
		Target:            &state.SupervisorTarget{Issue: 43, PR: 20, Session: "slot-2"},
		Risk:              "approval_gated",
		Confidence:        0.93,
		Reasons: []string{
			"Session slot-2 for issue #43 is retry_exhausted",
			"Retry-exhausted work requires a human decision before more automation",
		},
		ProjectState: state.SupervisorProjectState{Sessions: 2, PROpen: 1, Queued: 1, OpenPRs: 2},
	}, state.DefaultSupervisorDecisionLimit)
	if err := state.Save(dir, st); err != nil {
		t.Fatalf("save state: %v", err)
	}

	srv := New(cfg, make(chan struct{}, 1))
	req := httptest.NewRequest(http.MethodGet, "/api/v1/state", nil)
	w := httptest.NewRecorder()
	srv.handleState(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}
	var resp stateResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if !resp.Supervisor.HasRun {
		t.Fatal("supervisor.has_run = false, want true")
	}
	if resp.Supervisor.Latest == nil {
		t.Fatal("supervisor.latest missing")
	}
	if resp.Supervisor.Latest.RecommendedAction != "review_retry_exhausted" {
		t.Fatalf("latest action = %q", resp.Supervisor.Latest.RecommendedAction)
	}
	if resp.Supervisor.Latest.OperatorSentence != "Reviewing failed feedback on PR #20 before retrying work." {
		t.Fatalf("latest operator sentence = %q", resp.Supervisor.Latest.OperatorSentence)
	}
	if len(resp.Supervisor.Latest.StuckReasons) != 2 {
		t.Fatalf("stuck reasons = %d, want 2", len(resp.Supervisor.Latest.StuckReasons))
	}
	if !hasTargetLink(resp.Supervisor.Latest.TargetLinks, "issue", "https://github.com/test/repo/issues/43") {
		t.Fatalf("latest target links = %#v, want issue link", resp.Supervisor.Latest.TargetLinks)
	}
	if !hasTargetLink(resp.Supervisor.Latest.TargetLinks, "pr", "https://github.com/test/repo/pull/20") {
		t.Fatalf("latest target links = %#v, want PR link", resp.Supervisor.Latest.TargetLinks)
	}
	if resp.Supervisor.Latest.Queue == nil || !resp.Supervisor.Latest.Queue.Enabled || resp.Supervisor.Latest.Queue.Position != 2 || resp.Supervisor.Latest.Queue.Total != 2 {
		t.Fatalf("queue = %#v, want position 2 of 2", resp.Supervisor.Latest.Queue)
	}
	if resp.Supervisor.LastSafeAction == nil || resp.Supervisor.LastSafeAction.Action != "monitor_open_pr" {
		t.Fatalf("last safe action = %#v", resp.Supervisor.LastSafeAction)
	}
	if resp.Supervisor.LastSafeAction.OperatorSentence != "Watching PR #12 until checks and review pass." {
		t.Fatalf("last safe operator sentence = %q", resp.Supervisor.LastSafeAction.OperatorSentence)
	}
	if len(resp.Supervisor.ApprovalActions) != 1 || !resp.Supervisor.ApprovalActions[0].Disabled {
		t.Fatalf("approval actions = %#v, want one disabled action", resp.Supervisor.ApprovalActions)
	}
	if resp.SupervisorLatest == nil || resp.SupervisorLatest.ID != "sup-latest" {
		t.Fatalf("legacy supervisor_latest = %#v, want sup-latest", resp.SupervisorLatest)
	}
}

func TestSupervisorOperatorSentence(t *testing.T) {
	tests := []struct {
		name    string
		action  string
		summary string
		target  *state.SupervisorTarget
		want    string
	}{
		{
			name:   "none",
			action: "none",
			want:   "Skipped this tick; no safe action was selected.",
		},
		{
			name:   "monitor open pr",
			action: "monitor_open_pr",
			target: &state.SupervisorTarget{Issue: 42, PR: 12},
			want:   "Watching PR #12 until checks and review pass.",
		},
		{
			name:   "spawn worker",
			action: "spawn_worker",
			target: &state.SupervisorTarget{Issue: 42},
			want:   "Starting a worker for issue #42.",
		},
		{
			name:   "wait for worker",
			action: "wait_for_running_worker",
			target: &state.SupervisorTarget{Session: "bot-7"},
			want:   "Waiting for worker bot-7 to finish.",
		},
		{
			name:   "outcome health",
			action: "check_outcome_health",
			want:   "Checking runtime outcome health before sending more work.",
		},
		{
			name:    "unknown keeps raw action",
			action:  "custom_new_action",
			summary: "worker paused for external system",
			want:    "Supervisor reported custom_new_action; worker paused for external system",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := supervisorOperatorSentence(tt.action, tt.summary, tt.target)
			if got != tt.want {
				t.Fatalf("sentence = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestHandleStateMapsSupervisorAttentionToWorker(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{Repo: "test/repo", MaxParallel: 2, StateDir: dir, Server: config.ServerConfig{Port: 8765}}
	now := time.Date(2026, 4, 29, 12, 0, 0, 0, time.UTC)
	st := state.NewState()
	st.Sessions["slot-1"] = &state.Session{
		IssueNumber:     77,
		IssueTitle:      "fix checks",
		Status:          state.StatusRetryExhausted,
		StartedAt:       now.Add(-time.Hour),
		PRNumber:        31,
		CIFailureOutput: "go test failed",
	}
	st.RecordSupervisorDecision(state.SupervisorDecision{
		ID:                "sup-checks",
		CreatedAt:         now,
		Project:           "test/repo",
		Mode:              "read_only",
		Summary:           "Issue #77 is retry exhausted, but PR #31 is still open; checks=failure.",
		RecommendedAction: "review_retry_exhausted",
		Target:            &state.SupervisorTarget{Issue: 77, PR: 31, Session: "slot-1"},
		Risk:              "approval_gated",
		Confidence:        0.93,
		StuckStates: []state.SupervisorStuckState{
			{
				Code:              "retry_exhausted_open_pr",
				Severity:          "blocked",
				Summary:           "Issue #77 is retry exhausted, but PR #31 is still open; checks=failure.",
				RecommendedAction: "Fix failing checks or retry intentionally before this PR can merge.",
				Target:            &state.SupervisorTarget{Issue: 77, PR: 31, Session: "slot-1"},
			},
		},
	}, state.DefaultSupervisorDecisionLimit)
	if err := state.Save(dir, st); err != nil {
		t.Fatalf("save state: %v", err)
	}

	srv := New(cfg, make(chan struct{}, 1))
	req := httptest.NewRequest(http.MethodGet, "/api/v1/state", nil)
	w := httptest.NewRecorder()
	srv.handleState(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}
	var resp stateResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(resp.All) != 1 || !resp.All[0].NeedsAttention {
		t.Fatalf("worker attention mapping missing: %#v", resp.All)
	}
	if !contains(resp.All[0].StatusReason, "PR #31 is still open") || !contains(resp.All[0].StatusReason, "checks=failure") {
		t.Fatalf("status_reason = %q, want open PR and failing checks", resp.All[0].StatusReason)
	}
	if !contains(resp.All[0].NextAction, "Fix failing checks") {
		t.Fatalf("next_action = %q, want fix checks guidance", resp.All[0].NextAction)
	}
}

func TestApplySupervisorAttentionSessionTargetDoesNotFallback(t *testing.T) {
	infos := []sessionInfo{
		{Slot: "slot-1", IssueNumber: 77, PRNumber: 31},
		{Slot: "slot-2", IssueNumber: 77, PRNumber: 31},
	}
	decision := &state.SupervisorDecision{
		StuckStates: []state.SupervisorStuckState{
			{
				Code:              "failing_checks",
				Severity:          "blocked",
				Summary:           "Slot 1 checks failed",
				RecommendedAction: "Fix slot 1 checks.",
				Target:            &state.SupervisorTarget{Issue: 77, PR: 31, Session: "slot-1"},
			},
		},
	}

	applySupervisorAttention(infos, decision)

	if !infos[0].NeedsAttention || infos[0].StatusReason != "Slot 1 checks failed" {
		t.Fatalf("slot-1 attention = %#v, want targeted supervisor reason", infos[0])
	}
	if infos[1].NeedsAttention || infos[1].StatusReason != "" || infos[1].NextAction != "" {
		t.Fatalf("slot-2 attention = %#v, want no session-targeted fallback", infos[1])
	}
}

func TestApplySupervisorAttentionSuppressesResolvedStaleReviewFeedback(t *testing.T) {
	infos := []sessionInfo{
		{
			Slot:           "slot-1",
			IssueNumber:    359,
			PRNumber:       375,
			Status:         string(state.StatusDone),
			StatusReason:   "Issue is complete; PR merged or issue was closed and the session is terminal.",
			NeedsAttention: false,
		},
	}
	decision := &state.SupervisorDecision{
		StuckStates: []state.SupervisorStuckState{
			{
				Code:              "stale_review_feedback",
				Severity:          "blocked",
				Summary:           "Issue #359 has review feedback, but no worker is currently fixing it.",
				RecommendedAction: "Respawn a worker with the saved review feedback or resolve the feedback manually.",
				Target:            &state.SupervisorTarget{Issue: 359, PR: 375, Session: "slot-1"},
			},
		},
	}

	applySupervisorAttention(infos, decision)

	if infos[0].NeedsAttention {
		t.Fatalf("resolved stale feedback should not need attention: %#v", infos[0])
	}
	if !contains(infos[0].StatusReason, "Issue is complete") || infos[0].NextAction != "" {
		t.Fatalf("resolved reason/action = %q/%q, want neutral historical status", infos[0].StatusReason, infos[0].NextAction)
	}
}

func TestApplySupervisorAttentionDoesNotOverwriteTerminalPRTargetedStaleReviewFeedback(t *testing.T) {
	tests := []struct {
		name   string
		status state.SessionStatus
	}{
		{name: "dead", status: state.StatusDead},
		{name: "retry exhausted", status: state.StatusRetryExhausted},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			infos := []sessionInfo{
				{
					Slot:           "slot-1",
					IssueNumber:    359,
					PRNumber:       375,
					Status:         string(tt.status),
					StatusReason:   "Existing terminal session state.",
					NextAction:     "Handle the terminal session state.",
					NeedsAttention: true,
				},
			}
			decision := &state.SupervisorDecision{
				StuckStates: []state.SupervisorStuckState{
					{
						Code:              "stale_review_feedback",
						Severity:          "blocked",
						Summary:           "Issue #359 has review feedback, but no worker is currently fixing it.",
						RecommendedAction: "Respawn a worker with the saved review feedback or resolve the feedback manually.",
						Target:            &state.SupervisorTarget{Issue: 359, PR: 375, Session: "slot-1"},
					},
				},
			}

			applySupervisorAttention(infos, decision)

			if infos[0].StatusReason != "Existing terminal session state." || infos[0].NextAction != "Handle the terminal session state." {
				t.Fatalf("terminal reason/action = %q/%q, want existing session state", infos[0].StatusReason, infos[0].NextAction)
			}
			if !infos[0].NeedsAttention {
				t.Fatalf("terminal session attention should be preserved: %#v", infos[0])
			}
		})
	}
}

func TestApplySupervisorAttentionKeepsOpenReviewFeedbackAttention(t *testing.T) {
	infos := []sessionInfo{
		{Slot: "slot-1", IssueNumber: 360, PRNumber: 376, Status: string(state.StatusPROpen)},
	}
	decision := &state.SupervisorDecision{
		StuckStates: []state.SupervisorStuckState{
			{
				Code:              "stale_review_feedback",
				Severity:          "blocked",
				Summary:           "Issue #360 has review feedback, but no worker is currently fixing it.",
				RecommendedAction: "Respawn a worker with the saved review feedback or resolve the feedback manually.",
				Target:            &state.SupervisorTarget{Issue: 360, PR: 376, Session: "slot-1"},
			},
		},
	}

	applySupervisorAttention(infos, decision)

	if !infos[0].NeedsAttention {
		t.Fatalf("open review feedback should still need attention: %#v", infos[0])
	}
	if !contains(infos[0].StatusReason, "review feedback") || !contains(infos[0].NextAction, "Respawn a worker") {
		t.Fatalf("attention reason/action = %q/%q, want review feedback guidance", infos[0].StatusReason, infos[0].NextAction)
	}
}

func TestApplySupervisorAttentionSkipsInformationalStuckStates(t *testing.T) {
	baseReason := "State says running, but the worker PID is not alive."
	baseAction := "Run a Maestro reconciliation cycle."
	infos := []sessionInfo{
		{Slot: "slot-1", IssueNumber: 77, PRNumber: 31, NeedsAttention: true, StatusReason: baseReason, NextAction: baseAction},
	}
	decision := &state.SupervisorDecision{
		StuckStates: []state.SupervisorStuckState{
			{
				Code:              "draft_pr",
				Severity:          "info",
				Summary:           "PR is still a draft.",
				RecommendedAction: "Wait for the author to mark the PR ready.",
				Target:            &state.SupervisorTarget{Issue: 77, PR: 31, Session: "slot-1"},
			},
		},
	}

	applySupervisorAttention(infos, decision)

	if infos[0].StatusReason != baseReason || infos[0].NextAction != baseAction {
		t.Fatalf("attention reason/action = %q/%q, want base dead-PID reason/action", infos[0].StatusReason, infos[0].NextAction)
	}
}

func TestApplySupervisorAttentionUsesLaterAttentionStuckState(t *testing.T) {
	infos := []sessionInfo{
		{Slot: "slot-1", IssueNumber: 77, PRNumber: 31, NeedsAttention: true, StatusReason: "Base reason.", NextAction: "Base action."},
	}
	decision := &state.SupervisorDecision{
		StuckStates: []state.SupervisorStuckState{
			{
				Code:              "draft_pr",
				Severity:          "info",
				Summary:           "PR is still a draft.",
				RecommendedAction: "Wait for the author to mark the PR ready.",
				Target:            &state.SupervisorTarget{Issue: 77, PR: 31, Session: "slot-1"},
			},
			{
				Code:              "failing_checks",
				Severity:          "blocked",
				Summary:           "Checks are failing.",
				RecommendedAction: "Fix failing checks.",
				Target:            &state.SupervisorTarget{Issue: 77, PR: 31, Session: "slot-1"},
			},
		},
	}

	applySupervisorAttention(infos, decision)

	if infos[0].StatusReason != "Checks are failing." || infos[0].NextAction != "Fix failing checks." {
		t.Fatalf("attention reason/action = %q/%q, want later blocked stuck state", infos[0].StatusReason, infos[0].NextAction)
	}
}

func TestHandleState_IncludesApprovals(t *testing.T) {
	srv, cfg := setupTestServer(t)
	now := time.Date(2026, 4, 29, 12, 0, 0, 0, time.UTC)
	st, err := state.Load(cfg.StateDir)
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	approval := st.RecordPendingApprovalForDecision(state.SupervisorDecision{
		ID:                "sup-approval",
		CreatedAt:         now,
		Project:           "test/repo",
		Mode:              "read_only",
		Summary:           "Start a worker for issue #45.",
		RecommendedAction: "spawn_worker",
		Target:            &state.SupervisorTarget{Issue: 45},
		Risk:              "mutating",
		Confidence:        0.84,
		Reasons:           []string{"Issue #45 is eligible"},
	}, now)
	if err := state.Save(cfg.StateDir, st); err != nil {
		t.Fatalf("save state: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/state", nil)
	w := httptest.NewRecorder()
	srv.handleState(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}
	var resp stateResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(resp.Approvals) != 1 {
		t.Fatalf("approvals = %d, want 1", len(resp.Approvals))
	}
	if resp.Approvals[0].ID != approval.ID {
		t.Fatalf("approval ID = %q, want %q", resp.Approvals[0].ID, approval.ID)
	}
	if resp.Approvals[0].Status != state.ApprovalStatusPending {
		t.Fatalf("approval status = %q, want %q", resp.Approvals[0].Status, state.ApprovalStatusPending)
	}
}

func TestHandleState_MethodNotAllowed(t *testing.T) {
	srv, _ := setupTestServer(t)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/state", nil)
	w := httptest.NewRecorder()
	srv.handleState(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusMethodNotAllowed)
	}
}

func TestHandleWorkers(t *testing.T) {
	srv, _ := setupTestServer(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/workers", nil)
	w := httptest.NewRecorder()
	srv.handleWorkers(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}

	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	total := int(resp["total"].(float64))
	if total != 3 {
		t.Errorf("total workers = %d, want 3", total)
	}

	workers := resp["workers"].([]interface{})
	if len(workers) != 3 {
		t.Errorf("workers array len = %d, want 3", len(workers))
	}
}

func TestHandleLog(t *testing.T) {
	srv, _ := setupTestServer(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/logs/slot-1?lines=2", nil)
	w := httptest.NewRecorder()
	srv.handleLog(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}

	var resp logResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Slot != "slot-1" {
		t.Errorf("slot = %q, want slot-1", resp.Slot)
	}
	if contains(resp.Text, "line one") {
		t.Error("tail should not include older lines beyond requested limit")
	}
	if !contains(resp.Text, "line two") || !contains(resp.Text, "line three") {
		t.Errorf("tail text = %q, want last two lines", resp.Text)
	}
}

func TestHandleLog_NotFound(t *testing.T) {
	srv, _ := setupTestServer(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/logs/missing", nil)
	w := httptest.NewRecorder()
	srv.handleLog(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusNotFound)
	}
}

func TestHandleLog_MethodNotAllowed(t *testing.T) {
	srv, _ := setupTestServer(t)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/logs/slot-1", nil)
	w := httptest.NewRecorder()
	srv.handleLog(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusMethodNotAllowed)
	}
}

func TestHandleIssue_Found(t *testing.T) {
	srv, _ := setupTestServer(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/42", nil)
	w := httptest.NewRecorder()
	srv.handleIssue(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}

	var resp issueResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if resp.IssueNumber != 42 {
		t.Errorf("issue_number = %d, want 42", resp.IssueNumber)
	}
	if len(resp.Sessions) != 1 {
		t.Errorf("sessions = %d, want 1", len(resp.Sessions))
	}
	if resp.Sessions[0].IssueTitle != "Fix bug" {
		t.Errorf("issue_title = %q, want %q", resp.Sessions[0].IssueTitle, "Fix bug")
	}
}

func TestHandleIssue_NotFound(t *testing.T) {
	srv, _ := setupTestServer(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/999", nil)
	w := httptest.NewRecorder()
	srv.handleIssue(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusNotFound)
	}
}

func TestHandleIssue_InvalidNumber(t *testing.T) {
	srv, _ := setupTestServer(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/abc", nil)
	w := httptest.NewRecorder()
	srv.handleIssue(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

func TestHandleRefresh(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{
		Repo:     "test/repo",
		StateDir: dir,
		Server:   config.ServerConfig{Port: 8765},
	}

	refreshCh := make(chan struct{}, 1)
	srv := New(cfg, refreshCh)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/refresh", nil)
	w := httptest.NewRecorder()
	srv.handleRefresh(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}

	// Check channel received signal
	select {
	case <-refreshCh:
		// ok
	default:
		t.Error("refresh channel did not receive signal")
	}
}

func TestHandleRefresh_AlreadyPending(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{
		Repo:     "test/repo",
		StateDir: dir,
		Server:   config.ServerConfig{Port: 8765},
	}

	refreshCh := make(chan struct{}, 1)
	refreshCh <- struct{}{} // pre-fill the channel
	srv := New(cfg, refreshCh)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/refresh", nil)
	w := httptest.NewRecorder()
	srv.handleRefresh(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}

	var resp map[string]string
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["status"] != "refresh already pending" {
		t.Errorf("status = %q, want %q", resp["status"], "refresh already pending")
	}
}

func TestHandleRefresh_MethodNotAllowed(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{
		Repo:     "test/repo",
		StateDir: dir,
		Server:   config.ServerConfig{Port: 8765},
	}

	refreshCh := make(chan struct{}, 1)
	srv := New(cfg, refreshCh)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/refresh", nil)
	w := httptest.NewRecorder()
	srv.handleRefresh(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusMethodNotAllowed)
	}
}

func TestHandleRefresh_ReadOnly(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{
		Repo:     "test/repo",
		StateDir: dir,
		Server:   config.ServerConfig{Port: 8765, ReadOnly: true},
	}

	refreshCh := make(chan struct{}, 1)
	srv := New(cfg, refreshCh)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/refresh", nil)
	w := httptest.NewRecorder()
	srv.handleRefresh(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusForbidden)
	}
	select {
	case <-refreshCh:
		t.Error("refresh channel should not receive signal in read-only mode")
	default:
	}
}

func TestHandleAction_ReadOnlyRejectsMutation(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{
		Repo:     "test/repo",
		StateDir: dir,
		Server:   config.ServerConfig{Port: 8765, ReadOnly: true},
	}
	st := state.NewState()
	st.Sessions["slot-1"] = &state.Session{
		IssueNumber: 42,
		IssueTitle:  "Fix bug",
		Status:      state.StatusRunning,
		StartedAt:   time.Now().UTC(),
	}
	if err := state.Save(dir, st); err != nil {
		t.Fatalf("save state: %v", err)
	}
	statePath := filepath.Join(dir, "state.json")
	before, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("read state before: %v", err)
	}

	srv := New(cfg, make(chan struct{}, 1))
	req := httptest.NewRequest(http.MethodPost, "/api/v1/actions", bytes.NewBufferString(`{"action_id":"restart_worker","slot":"slot-1"}`))
	w := httptest.NewRecorder()
	srv.handleAction(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusForbidden)
	}
	if !contains(w.Body.String(), "read-only") {
		t.Fatalf("response = %q, want read-only explanation", w.Body.String())
	}
	after, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("read state after: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("read-only action endpoint changed state")
	}
}

func TestHandleAction_NotImplementedWhenWritable(t *testing.T) {
	srv, _ := setupTestServer(t)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/actions", bytes.NewBufferString(`{"action_id":"stop_worker","slot":"slot-1"}`))
	w := httptest.NewRecorder()
	srv.handleAction(w, req)

	if w.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusNotImplemented)
	}
	if !contains(w.Body.String(), "approval-backed") {
		t.Fatalf("response = %q, want approval-backed explanation", w.Body.String())
	}
}

func TestHandleDashboard(t *testing.T) {
	srv, _ := setupTestServer(t)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	srv.handleDashboard(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}

	body := w.Body.String() + web.MustReadStatic("tokens.css") + web.MustReadStatic("dashboard.js") + web.MustReadStatic("dashboard.css")
	if ct := w.Header().Get("Content-Type"); ct != "text/html; charset=utf-8" {
		t.Errorf("content-type = %q, want text/html", ct)
	}
	if !contains(body, "test/repo") {
		t.Error("dashboard should contain repo name")
	}
	if !contains(body, "api/v1/logs") {
		t.Error("dashboard should include log API polling")
	}
	if !contains(body, "Workers") {
		t.Error("dashboard should render worker table shell")
	}
	if !contains(body, "status-note") {
		t.Error("dashboard should include status explanation block")
	}
	if !contains(body, "supervisor-panel") || !contains(body, "renderSupervisor") {
		t.Error("dashboard should include supervisor rationale panel")
	}
	if !contains(body, "supervisorOperatorSentence") || !contains(body, "supervisor-raw-action") || !contains(body, "Skipped this tick") {
		t.Error("dashboard should render operator-facing supervisor sentences with raw action diagnostics")
	}
	if !contains(body, "outcome-panel") || !contains(body, "renderOutcome") || !contains(body, "No outcome brief configured") {
		t.Error("dashboard should include outcome status panel")
	}
	if !contains(body, "No Supervisor has run yet") {
		t.Error("dashboard should include supervisor empty state text")
	}
	if !contains(body, "issue_url") || !contains(body, "pr_url") {
		t.Error("dashboard should render GitHub issue/PR links from API fields")
	}
	if !contains(body, "issueSummaryHTML") || !contains(body, "issue-main") || !contains(body, "issue-title") {
		t.Error("dashboard should keep issue links visible while truncating long titles")
	}
	if !contains(body, "renderWorkerActions") || !contains(body, "actionDetailHTML") || !contains(body, "manual approval required") {
		t.Error("dashboard should render disabled approval-gated action affordances")
	}
	for _, want := range []string{"<html data-theme=\"light\">", "/static/tokens.css", "/static/components.css", "/static/status-icons.svg", "/static/maestro-mark.svg", "/static/favicon-32.png", "/static/apple-touch-icon-180.png", "/static/og-1200x630.png", "Inter Tight", "JetBrains Mono", "#059669", "#0891b2", "color-scheme: light"} {
		if !contains(body, want) {
			t.Fatalf("dashboard design tokens should contain %q", want)
		}
	}
	for _, want := range []string{"Scope", "Target", "Approval", "Disabled", "replace(/^Would\\s+/i"} {
		if !contains(body, want) {
			t.Fatalf("dashboard action guardrails should contain %q", want)
		}
	}
}

func TestBrandAssetsEmbedded(t *testing.T) {
	svg := []byte(web.MustReadStatic("maestro-mark.svg"))
	if !bytes.Contains(svg, []byte("<svg")) || !bytes.Contains(svg, []byte("Maestro")) {
		t.Fatalf("maestro mark svg is not embedded or malformed")
	}
	for _, name := range []string{"favicon-16.png", "favicon-32.png", "favicon-48.png", "apple-touch-icon-180.png", "og-1200x630.png"} {
		data := []byte(web.MustReadStatic(name))
		if !bytes.HasPrefix(data, []byte("\x89PNG\r\n\x1a\n")) {
			t.Fatalf("%s is not an embedded PNG", name)
		}
	}
}

func TestComponentPrimitivesEmbedded(t *testing.T) {
	components := web.MustReadStatic("components.css")
	for _, want := range []string{".pill--ok", ".pill--warn", ".pill--bad", ".pill--idle", ".pill--policy", ".card", ".section", ".caption", ".status-icon"} {
		if !contains(components, want) {
			t.Fatalf("components.css should contain %q", want)
		}
	}
	sprite := web.MustReadStatic("status-icons.svg")
	for _, want := range []string{"status-ready", "status-running", "status-waiting", "status-blocked", "status-merged", "status-stale", "status-daemon", "status-queued"} {
		if !contains(sprite, `id="`+want+`"`) {
			t.Fatalf("status icon sprite should contain %q", want)
		}
	}
}

func TestGitHubURLs(t *testing.T) {
	if got := githubIssueURL("owner/repo", 42); got != "https://github.com/owner/repo/issues/42" {
		t.Errorf("githubIssueURL() = %q", got)
	}
	if got := githubPRURL("owner/repo", 10); got != "https://github.com/owner/repo/pull/10" {
		t.Errorf("githubPRURL() = %q", got)
	}
	if got := githubIssueURL("not-a-repo", 42); got != "" {
		t.Errorf("githubIssueURL(invalid) = %q, want empty", got)
	}
}

func TestHandleDashboard_NotFoundForOtherPaths(t *testing.T) {
	srv, _ := setupTestServer(t)

	req := httptest.NewRequest(http.MethodGet, "/nonexistent", nil)
	w := httptest.NewRecorder()
	srv.handleDashboard(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusNotFound)
	}
}

func TestHandleState_EmptyState(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{
		Repo:        "test/repo",
		MaxParallel: 5,
		StateDir:    dir,
		Server:      config.ServerConfig{Port: 8765},
	}

	refreshCh := make(chan struct{}, 1)
	srv := New(cfg, refreshCh)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/state", nil)
	w := httptest.NewRecorder()
	srv.handleState(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}

	var resp stateResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if len(resp.Running) != 0 {
		t.Errorf("running = %d, want 0", len(resp.Running))
	}
	if resp.TokenTotals.Total != 0 {
		t.Errorf("total tokens = %d, want 0", resp.TokenTotals.Total)
	}
	if resp.Supervisor.HasRun {
		t.Error("supervisor.has_run = true, want false")
	}
	if resp.Supervisor.EmptyState == "" {
		t.Error("supervisor empty state should explain that no supervisor has run")
	}
	if resp.Supervisor.Latest != nil {
		t.Fatalf("supervisor.latest = %#v, want nil", resp.Supervisor.Latest)
	}
}

func TestStartDisabledPort(t *testing.T) {
	cfg := &config.Config{
		Repo:   "test/repo",
		Server: config.ServerConfig{Port: 0},
	}
	refreshCh := make(chan struct{}, 1)
	srv := New(cfg, refreshCh)

	// Start should return nil immediately when port is 0
	err := srv.Start(nil)
	if err != nil {
		t.Errorf("expected nil error for disabled port, got %v", err)
	}
}

func TestEscapeHTML(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"hello", "hello"},
		{"<script>", "&lt;script&gt;"},
		{"a & b", "a &amp; b"},
		{`"quoted"`, "&quot;quoted&quot;"},
	}
	for _, tt := range tests {
		got := escapeHTML(tt.in)
		if got != tt.want {
			t.Errorf("escapeHTML(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestStripANSI(t *testing.T) {
	got := stripANSI("\x1b[0mhello \x1b[90mgrey\x1b[0m")
	if got != "hello grey" {
		t.Errorf("stripANSI() = %q, want hello grey", got)
	}
}

func TestHandleState_InvalidStateDir(t *testing.T) {
	cfg := &config.Config{
		Repo:     "test/repo",
		StateDir: filepath.Join(os.TempDir(), "nonexistent-dir-12345", "nested"),
	}

	// Write a corrupt state file
	os.MkdirAll(cfg.StateDir, 0755)
	os.WriteFile(filepath.Join(cfg.StateDir, "state.json"), []byte("{invalid"), 0644)

	refreshCh := make(chan struct{}, 1)
	srv := New(cfg, refreshCh)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/state", nil)
	w := httptest.NewRecorder()
	srv.handleState(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusInternalServerError)
	}

	// cleanup
	os.RemoveAll(cfg.StateDir)
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsSubstr(s, substr))
}

func containsSubstr(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

func hasTargetLink(links []targetLinkInfo, kind, url string) bool {
	for _, link := range links {
		if link.Kind == kind && link.URL == url {
			return true
		}
	}
	return false
}

func findSessionInfo(t *testing.T, sessions []sessionInfo, slot string) sessionInfo {
	t.Helper()
	for _, session := range sessions {
		if session.Slot == slot {
			return session
		}
	}
	t.Fatalf("session %q not found in %+v", slot, sessions)
	return sessionInfo{}
}

func findControlAction(t *testing.T, actions []controlAction, id string) controlAction {
	t.Helper()
	for _, action := range actions {
		if action.ID == id {
			return action
		}
	}
	t.Fatalf("action %q not found in %+v", id, actions)
	return controlAction{}
}

func assertReadOnlyAction(t *testing.T, action controlAction) {
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
	if !action.Mutating || !action.RequiresApproval {
		t.Fatalf("action %+v should be mutating and approval-required", action)
	}
	if action.ApprovalPolicy != controlApprovalPolicyManual {
		t.Fatalf("approval policy = %q, want %q", action.ApprovalPolicy, controlApprovalPolicyManual)
	}
	if !action.Disabled {
		t.Fatalf("action %+v should be disabled", action)
	}
	if !contains(action.DisabledReason, "Read-only mode") {
		t.Fatalf("disabled reason = %q, want read-only explanation", action.DisabledReason)
	}
	if action.Method != http.MethodPost || action.Endpoint != "/api/v1/actions" {
		t.Fatalf("action endpoint = %s %s, want POST /api/v1/actions", action.Method, action.Endpoint)
	}
}
