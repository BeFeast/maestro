package orchestrator

import (
	"testing"
	"time"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/github"
	"github.com/befeast/maestro/internal/state"
)

func TestCheckSessionsReleasesDoneMergedOpenIssueAfterGrace(t *testing.T) {
	cfg := &config.Config{StateDir: t.TempDir()}
	finished := time.Now().UTC().Add(-terminalReconcileGrace - time.Minute)
	sess := &state.Session{
		IssueNumber: 1103,
		Status:      state.StatusDone,
		PRNumber:    1104,
		PRMerged:    true,
		StartedAt:   finished.Add(-time.Hour),
		FinishedAt:  &finished,
	}
	st := state.NewState()
	st.Sessions["sup-1103"] = sess

	mergeReads := 0
	o := &Orchestrator{
		cfg:           cfg,
		listOpenPRsFn: func() ([]github.PR, error) { return nil, nil },
		isIssueClosedFn: func(int) (bool, error) {
			return false, nil
		},
		isPRMergedFn: func(pr int) (bool, error) {
			mergeReads++
			return pr == 1104, nil
		},
	}

	o.checkSessions(st)

	if mergeReads != 1 {
		t.Fatalf("merged PR reads = %d, want 1", mergeReads)
	}
	if !sess.ReleasedForRedispatch {
		t.Fatal("done+merged session was not released after the open-issue grace")
	}
	if sess.WorkerOutcome != state.WorkerOutcomeMergedPRIssueStillOpen {
		t.Fatalf("WorkerOutcome = %q, want %q", sess.WorkerOutcome, state.WorkerOutcomeMergedPRIssueStillOpen)
	}
	if st.IssueInProgress(1103) {
		t.Fatal("released terminal session still holds the issue claim")
	}
	if st.IssueDone(1103) {
		t.Fatal("released session for an open issue still reports IssueDone")
	}

	reloaded, err := state.Load(cfg.StateDir)
	if err != nil {
		t.Fatalf("load eagerly persisted release: %v", err)
	}
	if got := reloaded.Sessions["sup-1103"]; got == nil || !got.ReleasedForRedispatch {
		t.Fatalf("persisted session = %+v, want ReleasedForRedispatch", got)
	}
}

func TestCheckSessionsKeepsDoneMergedClaimDuringCloseGrace(t *testing.T) {
	finished := time.Now().UTC().Add(-terminalReconcileGrace + time.Minute)
	sess := &state.Session{
		IssueNumber: 1103,
		Status:      state.StatusDone,
		PRNumber:    1104,
		PRMerged:    true,
		StartedAt:   finished.Add(-time.Hour),
		FinishedAt:  &finished,
	}
	st := state.NewState()
	st.Sessions["sup-1103"] = sess

	mergeReads := 0
	o := &Orchestrator{
		cfg:           &config.Config{},
		listOpenPRsFn: func() ([]github.PR, error) { return nil, nil },
		isIssueClosedFn: func(int) (bool, error) {
			return false, nil
		},
		isPRMergedFn: func(int) (bool, error) {
			mergeReads++
			return true, nil
		},
	}

	o.checkSessions(st)

	if mergeReads != 0 {
		t.Fatalf("merged PR reads = %d, want 0 before grace expires", mergeReads)
	}
	if sess.ReleasedForRedispatch {
		t.Fatal("terminal claim released before close grace expired")
	}
	if !st.IssueInProgress(1103) {
		t.Fatal("done+merged session must retain terminal reconciliation claim during grace")
	}
}

func TestReconcileSessionsRetriesTodoForReleasedDoneOpenIssue(t *testing.T) {
	cfg := &config.Config{
		GitHubProjects: config.GitHubProjectsConfig{Enabled: true, ProjectNumber: 3},
	}
	var gotIssue int
	var gotStatus github.ProjectStatus
	o := &Orchestrator{
		cfg: cfg,
		rateLimitFn: func() (github.RateLimitStatus, error) {
			return github.RateLimitStatus{GraphQL: github.RateLimitBucket{Limit: 5000, Remaining: 5000}}, nil
		},
		syncProjectFn: func(issue int, status github.ProjectStatus) bool {
			gotIssue, gotStatus = issue, status
			return true
		},
	}
	st := state.NewState()
	st.Sessions["sup-1103"] = &state.Session{
		IssueNumber:           1103,
		Status:                state.StatusDone,
		PRNumber:              1104,
		ReleasedForRedispatch: true,
		WorkerOutcome:         state.WorkerOutcomeMergedPRIssueStillOpen,
	}
	st.MarkProjectStatusSynced(1103, string(github.ProjectStatusAwaitingClose), time.Now().UTC())

	if !o.reconcileSessionsToProjectBoard(st) {
		t.Fatal("released done session did not repair its runnable project status")
	}
	if gotIssue != 1103 || gotStatus != github.ProjectStatusTodo {
		t.Fatalf("project sync = issue #%d status %q, want issue #1103 status %q", gotIssue, gotStatus, github.ProjectStatusTodo)
	}
	if !st.ProjectStatusSynced(1103, string(github.ProjectStatusTodo)) {
		t.Fatal("Todo repair was not recorded in durable project sync state")
	}
}
