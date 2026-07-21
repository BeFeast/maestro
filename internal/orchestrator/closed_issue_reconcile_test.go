package orchestrator

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/befeast/maestro/internal/approvalstore"
	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/github"
	"github.com/befeast/maestro/internal/outcome"
	"github.com/befeast/maestro/internal/state"
	"github.com/befeast/maestro/internal/statestore"
)

func TestCheckSessionsClosedIssueConvergesEverySessionLifecycle(t *testing.T) {
	now := time.Now().UTC().Add(-time.Hour)
	statuses := []state.SessionStatus{
		state.StatusRunning,
		state.StatusPROpen,
		state.StatusQueued,
		state.StatusCodeLanded,
		state.StatusDead,
		state.StatusFailed,
		state.StatusRetryExhausted,
	}
	st := state.NewState()
	for i, status := range statuses {
		slot := "slot-" + string(rune('a'+i))
		st.Sessions[slot] = &state.Session{
			IssueNumber: 900 + i,
			Status:      status,
			StartedAt:   now.Add(-time.Hour),
			FinishedAt:  &now,
		}
	}
	st.Sessions["slot-a"].PID = 4242
	st.Sessions["slot-b"].PRNumber = 32
	st.Sessions["slot-d"].PRNumber = 33

	stopped := map[string]bool{}
	o := &Orchestrator{
		cfg:           &config.Config{Repo: "owner/repo"},
		listOpenPRsFn: func() ([]github.PR, error) { return nil, nil },
		isIssueClosedFn: func(int) (bool, error) {
			return true, nil
		},
		workerStopFn: func(_ *config.Config, slot string, _ *state.Session) error {
			stopped[slot] = true
			return nil
		},
	}

	o.checkSessions(st)

	for slot, sess := range st.Sessions {
		if sess.Status != state.StatusDone || sess.IssueClosedAt == nil || !sess.ReleasedForRedispatch {
			t.Fatalf("%s did not converge to closed terminal state: %+v", slot, sess)
		}
		if claim, ok := st.IssueClaimFor(sess.IssueNumber); ok {
			t.Fatalf("%s retained closed-issue claim: %+v", slot, claim)
		}
	}
	if st.Sessions["slot-b"].PRNumber != 32 || st.Sessions["slot-d"].PRNumber != 33 {
		t.Fatalf("PR audit history changed: pr_open=%d code_landed=%d", st.Sessions["slot-b"].PRNumber, st.Sessions["slot-d"].PRNumber)
	}
	for _, slot := range []string{"slot-a", "slot-b", "slot-c"} {
		if !stopped[slot] {
			t.Fatalf("%s was not stopped during closed-issue reconciliation", slot)
		}
	}
	if stopped["slot-d"] {
		t.Fatal("historical code_landed session should not invoke worker stop")
	}
}

func TestCheckSessionsClosedIssueWaitsForProcessLeaseTeardown(t *testing.T) {
	st := state.NewState()
	st.Sessions["slot-running"] = &state.Session{
		IssueNumber:         905,
		Status:              state.StatusRunning,
		PID:                 4242,
		ProcessLeaseUnit:    "maestro-worker-test-g1.scope",
		ProcessLeaseManager: "systemd-user",
	}
	stopCalls := 0
	o := &Orchestrator{
		cfg:             &config.Config{Repo: "owner/repo"},
		listOpenPRsFn:   func() ([]github.PR, error) { return nil, nil },
		isIssueClosedFn: func(int) (bool, error) { return true, nil },
		workerStopFn: func(_ *config.Config, _ string, sess *state.Session) error {
			stopCalls++
			if stopCalls == 1 {
				return errors.New("process lease still active")
			}
			sess.ProcessLeaseUnit = ""
			sess.ProcessLeaseManager = ""
			return nil
		},
	}

	o.checkSessions(st)
	if sess := st.Sessions["slot-running"]; sess.Status != state.StatusRunning || sess.IssueClosedAt != nil || sess.ProcessLeaseUnit == "" {
		t.Fatalf("failed teardown session = %+v, want nonterminal state with lease receipt retained", sess)
	}

	o.checkSessions(st)
	if sess := st.Sessions["slot-running"]; sess.Status != state.StatusDone || sess.IssueClosedAt == nil || sess.ProcessLeaseUnit != "" {
		t.Fatalf("successful teardown session = %+v, want terminal closed state with lease released", sess)
	}
}

func TestCheckSessionsClosedIssueWaitsForLegacyTeardownError(t *testing.T) {
	st := state.NewState()
	st.Sessions["slot-running"] = &state.Session{
		IssueNumber: 906,
		Status:      state.StatusRunning,
		PID:         4242,
		TmuxSession: "maestro-slot-running",
	}
	stopCalls := 0
	o := &Orchestrator{
		cfg:             &config.Config{Repo: "owner/repo"},
		listOpenPRsFn:   func() ([]github.PR, error) { return nil, nil },
		isIssueClosedFn: func(int) (bool, error) { return true, nil },
		workerStopFn: func(_ *config.Config, _ string, sess *state.Session) error {
			stopCalls++
			if stopCalls == 1 {
				// Reproduce a legacy/controller stop that clears receipts before
				// returning an ambiguous teardown failure.
				sess.PID = 0
				sess.TmuxSession = ""
				return errors.New("legacy process teardown uncertain")
			}
			return nil
		},
	}

	o.checkSessions(st)
	if sess := st.Sessions["slot-running"]; sess.Status != state.StatusRunning || sess.IssueClosedAt != nil || sess.ReleasedForRedispatch {
		t.Fatalf("failed legacy teardown session = %+v, want nonterminal ownership retained", sess)
	}

	o.checkSessions(st)
	if sess := st.Sessions["slot-running"]; sess.Status != state.StatusDone || sess.IssueClosedAt == nil || !sess.ReleasedForRedispatch {
		t.Fatalf("successful retry session = %+v, want terminal closed state", sess)
	}
}

func TestCheckSessionsClosedIssuePreservesDeliveryOutcomeAndProjectApproval(t *testing.T) {
	deployedAt := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)
	checkedAt := deployedAt.Add(time.Hour)
	st := state.NewState()
	st.OutcomeHealth = &outcome.HealthCheckResult{
		CheckedAt: checkedAt,
		State:     outcome.HealthFailing,
		Signal:    "healthcheck_command",
		Summary:   "runtime still failing",
	}
	st.Sessions["txc-17"] = &state.Session{
		IssueNumber:          5,
		Status:               state.StatusCodeLanded,
		PRNumber:             33,
		DeploymentFinishedAt: &deployedAt,
	}
	st.Approvals = []state.Approval{
		{
			ID:     "deploy-33",
			Action: state.ApprovalActionDeployProject,
			Status: state.ApprovalStatusPending,
			Target: &state.SupervisorTarget{Issue: 5, PR: 33, Session: "txc-17"},
			Delivery: &state.DeliveryPayload{
				Project: "owner/repo", Repo: "owner/repo", Issue: 5, PR: 33,
			},
		},
		{
			ID:     "deploy-failed-33",
			Action: state.ApprovalActionDeployProject,
			Status: state.ApprovalStatusExecutionFailed,
			Target: &state.SupervisorTarget{Issue: 5, PR: 33, Session: "txc-17"},
			Delivery: &state.DeliveryPayload{
				Project: "owner/repo", Repo: "owner/repo", Issue: 5, PR: 33,
			},
		},
		{
			ID:     "merge-33",
			Action: config.SupervisorActionMergePR,
			Status: state.ApprovalStatusPending,
			Target: &state.SupervisorTarget{Issue: 5, PR: 33, Session: "txc-17"},
		},
		{
			ID:     "restart-txc-17",
			Action: config.SupervisorActionRestartWorker,
			Status: state.ApprovalStatusApproved,
			Target: &state.SupervisorTarget{Issue: 5, PR: 33, Session: "txc-17"},
		},
		{
			ID:     "close-batch",
			Action: config.SupervisorActionCloseIssueBatch,
			Status: state.ApprovalStatusApproved,
			Target: &state.SupervisorTarget{Issues: []state.SupervisorIssueTarget{{Issue: 5, PR: 33}, {Issue: 6, PR: 34}}},
		},
	}
	o := &Orchestrator{
		cfg:             &config.Config{Repo: "owner/repo", Outcome: outcome.Brief{RequiresDeploy: true}},
		listOpenPRsFn:   func() ([]github.PR, error) { return nil, nil },
		isIssueClosedFn: func(int) (bool, error) { return true, nil },
	}

	o.checkSessions(st)

	sess := st.Sessions["txc-17"]
	if sess.Status != state.StatusDone || sess.PRNumber != 33 || !sess.PRMerged || sess.DeploymentFinishedAt == nil || !sess.DeploymentFinishedAt.Equal(deployedAt) {
		t.Fatalf("closed delivery session lost audit history: %+v", sess)
	}
	if st.OutcomeHealth == nil || st.OutcomeHealth.State != outcome.HealthFailing || st.OutcomeHealth.Summary != "runtime still failing" {
		t.Fatalf("project outcome was hidden or rewritten: %+v", st.OutcomeHealth)
	}
	if got := approvalStatus(t, st, "deploy-33"); got != state.ApprovalStatusPending {
		t.Fatalf("project delivery approval = %q, want pending", got)
	}
	if got := approvalStatus(t, st, "deploy-failed-33"); got != state.ApprovalStatusExecutionFailed {
		t.Fatalf("failed project delivery approval = %q, want execution_failed", got)
	}
	for _, id := range []string{"merge-33", "restart-txc-17", "close-batch"} {
		if got := approvalStatus(t, st, id); got != state.ApprovalStatusStale {
			t.Fatalf("issue-scoped approval %s = %q, want stale", id, got)
		}
	}
	remainingBatch := 0
	for i := range st.Approvals {
		approval := &st.Approvals[i]
		if approval.ID == "close-batch" || approval.Action != config.SupervisorActionCloseIssueBatch || approval.Status != state.ApprovalStatusPending || approval.Target == nil {
			continue
		}
		remainingBatch++
		if len(approval.Target.Issues) != 1 || approval.Target.Issues[0].Issue != 6 || approval.Target.Issues[0].PR != 34 {
			t.Fatalf("reissued close batch = %+v, want only issue #6 / PR #34", approval)
		}
	}
	if remainingBatch != 1 {
		t.Fatalf("reissued close batches = %d, want 1", remainingBatch)
	}
}

func TestClosedIssueReconciliationRetainsStillOpenPRTruth(t *testing.T) {
	st := state.NewState()
	st.Sessions["slot-pr"] = &state.Session{
		IssueNumber: 907,
		Status:      state.StatusPROpen,
		PRNumber:    44,
	}
	o := &Orchestrator{
		cfg:             &config.Config{Repo: "owner/repo"},
		listOpenPRsFn:   func() ([]github.PR, error) { return []github.PR{{Number: 44}}, nil },
		isIssueClosedFn: func(int) (bool, error) { return true, nil },
		isPRMergedFn:    func(int) (bool, error) { return false, nil },
		workerStopFn:    func(*config.Config, string, *state.Session) error { return nil },
	}

	o.checkSessions(st)
	sess := st.Sessions["slot-pr"]
	if sess.Status != state.StatusDone || sess.IssueClosedAt == nil || sess.PRMerged {
		t.Fatalf("closed issue/open PR session = %+v, want done issue lifecycle with PR still open", sess)
	}
}

func TestClosedIssueReconciliationMirrorsJSONAndSQLiteTogether(t *testing.T) {
	root := t.TempDir()
	stateDir := filepath.Join(root, "state")
	store, err := statestore.Open(filepath.Join(root, "maestro.db"))
	if err != nil {
		t.Fatalf("open state store: %v", err)
	}
	defer store.Close()
	var mirrorErr error
	state.SetSaveHook(func(dir string, snapshot *state.State) {
		mirrorErr = store.ImportState(context.Background(), statestore.RowBinding{
			Project: "owner/repo", Repo: "owner/repo", StateDir: dir,
		}, snapshot)
	})
	t.Cleanup(func() { state.SetSaveHook(nil) })

	deployedAt := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)
	st := state.NewState()
	st.Sessions["txc-17"] = &state.Session{
		IssueNumber: 5, Status: state.StatusCodeLanded, PRNumber: 33,
		DeploymentFinishedAt: &deployedAt,
	}
	if err := state.Save(stateDir, st); err != nil {
		t.Fatalf("seed JSON state: %v", err)
	}
	o := &Orchestrator{
		cfg:             &config.Config{Repo: "owner/repo", StateDir: stateDir},
		listOpenPRsFn:   func() ([]github.PR, error) { return nil, nil },
		isIssueClosedFn: func(int) (bool, error) { return true, nil },
	}
	o.checkSessions(st)
	if err := state.Save(stateDir, st); err != nil {
		t.Fatalf("save reconciled JSON state: %v", err)
	}
	if mirrorErr != nil {
		t.Fatalf("mirror reconciled state: %v", mirrorErr)
	}

	jsonState, err := state.Load(stateDir)
	if err != nil {
		t.Fatalf("load JSON state: %v", err)
	}
	sqliteSessions, err := store.Sessions(context.Background(), stateDir)
	if err != nil {
		t.Fatalf("load SQLite sessions: %v", err)
	}
	jsonSession := jsonState.Sessions["txc-17"]
	sqliteSession := sqliteSessions["txc-17"]
	if jsonSession == nil || sqliteSession == nil {
		t.Fatalf("missing reconciled session: JSON=%+v SQLite=%+v", jsonSession, sqliteSession)
	}
	for source, sess := range map[string]*state.Session{"JSON": jsonSession, "SQLite": sqliteSession} {
		if sess.Status != state.StatusDone || sess.IssueClosedAt == nil || sess.PRNumber != 33 || sess.DeploymentFinishedAt == nil || !sess.DeploymentFinishedAt.Equal(deployedAt) {
			t.Fatalf("%s session did not converge with audit history: %+v", source, sess)
		}
	}
}

func TestClosedIssueApprovalMirrorRetriesAfterJSONAlreadyStale(t *testing.T) {
	now := time.Date(2026, 7, 15, 14, 28, 15, 0, time.UTC)
	stateDir := t.TempDir()
	store, err := approvalstore.Open(filepath.Join(t.TempDir(), "maestro.db"))
	if err != nil {
		t.Fatalf("open approval store: %v", err)
	}
	t.Cleanup(func() { store.Close() })

	approval := state.Approval{
		ID:        "merge-33",
		CreatedAt: now.Add(-time.Hour),
		UpdatedAt: now.Add(-time.Hour),
		Action:    config.SupervisorActionMergePR,
		Target:    &state.SupervisorTarget{Issue: 5, PR: 33, Session: "txc-17"},
		Summary:   "Merge PR #33",
		Risk:      "approval_gated",
		Status:    state.ApprovalStatusPending,
		Repo:      "owner/repo",
		Project:   "owner/repo",
	}
	approval.PayloadHash = approval.ComputePayloadHash()
	if _, err := store.Put(context.Background(), &approval, approvalstore.RowBinding{Project: "owner/repo", Repo: "owner/repo", StateDir: stateDir}); err != nil {
		t.Fatalf("seed approval store: %v", err)
	}

	st := state.NewState()
	st.Sessions["txc-17"] = &state.Session{IssueNumber: 5, Status: state.StatusCodeLanded, PRNumber: 33}
	st.Approvals = []state.Approval{approval}
	o := &Orchestrator{
		cfg:              &config.Config{Repo: "owner/repo", StateDir: stateDir},
		repo:             "owner/repo",
		approvalsBinding: approvalstore.Binding{Mode: approvalstore.ModeSQLite, DBPath: t.TempDir()},
	}

	if !o.reconcileClosedIssueSession(st, "txc-17", st.Sessions["txc-17"], now) {
		t.Fatal("first closed-issue reconciliation did not transition JSON state")
	}
	if got := approvalStatus(t, st, "merge-33"); got != state.ApprovalStatusStale {
		t.Fatalf("JSON approval status = %q, want stale", got)
	}
	if row, err := store.Get(context.Background(), stateDir, "merge-33"); err != nil || row.Status != state.ApprovalStatusPending {
		t.Fatalf("SQLite row after forced mirror failure = row %+v err %v, want pending", row, err)
	}

	o.approvalsBinding = approvalstore.Binding{Mode: approvalstore.ModeSQLite, Handle: store}
	if !o.reconcileClosedIssueSession(st, "txc-17", st.Sessions["txc-17"], now.Add(time.Minute)) {
		t.Fatal("second closed-issue reconciliation did not complete")
	}
	row, err := store.Get(context.Background(), stateDir, "merge-33")
	if err != nil {
		t.Fatalf("load retried SQLite row: %v", err)
	}
	if row.Status != state.ApprovalStatusStale {
		t.Fatalf("SQLite approval status after retry = %q, want stale", row.Status)
	}
}

func TestOrderedQueueClosedIssueAdvancesWithoutOutcomeOrLabelChurn(t *testing.T) {
	st := state.NewState()
	st.OutcomeHealth = &outcome.HealthCheckResult{State: outcome.HealthFailing}
	o := &Orchestrator{
		cfg: &config.Config{
			Repo: "owner/repo",
			Outcome: outcome.Brief{
				DesiredOutcome:      "runtime is healthy",
				HealthcheckCommand:  "check-runtime",
				PassRequiredForDone: boolPtr(true),
			},
		},
		isIssueClosedFn: func(int) (bool, error) { return true, nil },
		hasMergedPRForIssueFn: func(int) (bool, error) {
			t.Fatal("closed issue must advance before merged-PR or outcome gates")
			return false, nil
		},
		addIssueLabelFn: func(int, string) error {
			t.Fatal("closed issue reconciliation must not churn ready/blocked labels")
			return nil
		},
	}

	done, reason, err := o.orderedQueueIssueDone(st, 5)
	if err != nil {
		t.Fatalf("orderedQueueIssueDone: %v", err)
	}
	if !done || reason != "issue closed" {
		t.Fatalf("ordered queue result = done %v reason %q, want terminal closed issue", done, reason)
	}
}
