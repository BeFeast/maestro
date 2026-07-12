package orchestrator

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/befeast/maestro/internal/approvalstore"
	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/notify"
	"github.com/befeast/maestro/internal/outcome"
	"github.com/befeast/maestro/internal/state"
)

const actionSpawnRepairWorker = "spawn_repair_worker"

func repairApproval(id string, issue, pr int, status state.ApprovalStatus, now time.Time) state.Approval {
	return state.Approval{
		ID:        id,
		Action:    actionSpawnRepairWorker,
		Target:    &state.SupervisorTarget{Issue: issue, PR: pr},
		Status:    status,
		CreatedAt: now,
		UpdatedAt: now,
		Audit:     []state.ApprovalAudit{{At: now, Event: state.ApprovalAuditCreated}},
	}
}

func approvalStatus(t *testing.T, s *state.State, id string) state.ApprovalStatus {
	t.Helper()
	a, ok := s.FindApproval(id)
	if !ok {
		t.Fatalf("approval %q vanished", id)
	}
	return a.Status
}

// TestReconcileResolvedRepairApprovals_DoneSessionSelfHeals is the #866
// regression: even when the edge-triggered post-merge stale call was lost (the
// live #858 incident left the approval pending with only its `created` audit
// event), the standing reconciler stales the moot spawn_repair_worker approval
// on the next cycle because the target issue's session is already done. Unrelated
// approvals — a repair approval for an actively-worked issue and a spawn_worker
// approval — are untouched, and the pass is idempotent.
func TestReconcileResolvedRepairApprovals_DoneSessionSelfHeals(t *testing.T) {
	now := time.Date(2026, 7, 10, 9, 47, 0, 0, time.UTC)
	o := &Orchestrator{cfg: &config.Config{Repo: "owner/repo"}, notifier: &notify.Notifier{}}

	s := state.NewState()
	s.Sessions = map[string]*state.Session{
		"sup-305": {IssueNumber: 858, Status: state.StatusDone, PRNumber: 864},
		"sup-400": {IssueNumber: 900, Status: state.StatusRunning},
	}
	s.Approvals = []state.Approval{
		repairApproval("ap-repair-858", 858, 864, state.ApprovalStatusPending, now),
		repairApproval("ap-repair-900", 900, 0, state.ApprovalStatusPending, now),
		{ID: "ap-spawn-858", Action: "spawn_worker", Target: &state.SupervisorTarget{Issue: 858}, Status: state.ApprovalStatusPending, CreatedAt: now,
			Audit: []state.ApprovalAudit{{At: now, Event: state.ApprovalAuditCreated}}},
	}

	o.reconcileResolvedRepairApprovals(s)

	if got := approvalStatus(t, s, "ap-repair-858"); got != state.ApprovalStatusStale {
		t.Fatalf("repair approval for done issue = %q, want stale", got)
	}
	moot, _ := s.FindApproval("ap-repair-858")
	last := moot.Audit[len(moot.Audit)-1]
	if last.Event != state.ApprovalAuditStale || !strings.Contains(last.Reason, "session done") {
		t.Fatalf("stale audit = {%q,%q}, want stale reason mentioning session done", last.Event, last.Reason)
	}
	if got := approvalStatus(t, s, "ap-repair-900"); got != state.ApprovalStatusPending {
		t.Fatalf("repair approval for actively-worked issue = %q, want pending (untouched)", got)
	}
	if got := approvalStatus(t, s, "ap-spawn-858"); got != state.ApprovalStatusPending {
		t.Fatalf("spawn_worker approval = %q, want pending (only spawn_repair_worker reconciled)", got)
	}

	// Idempotent: a second cycle changes nothing and appends no new audit entry.
	auditLen := len(moot.Audit)
	o.reconcileResolvedRepairApprovals(s)
	if got := approvalStatus(t, s, "ap-repair-858"); got != state.ApprovalStatusStale {
		t.Fatalf("after re-run status = %q, want stale", got)
	}
	moot2, _ := s.FindApproval("ap-repair-858")
	if len(moot2.Audit) != auditLen {
		t.Fatalf("re-run appended audit entries (%d -> %d); reconcile must be idempotent", auditLen, len(moot2.Audit))
	}
}

// A newer active session for an issue must take precedence over an older done
// session. Otherwise the standing reconciler can stale the approval controlling
// the active repair and make legitimate work disappear.
func TestReconcileResolvedRepairApprovals_ActiveSessionOutranksOlderDone(t *testing.T) {
	now := time.Date(2026, 7, 10, 9, 47, 0, 0, time.UTC)
	o := &Orchestrator{
		cfg:      &config.Config{Repo: "owner/repo"},
		notifier: &notify.Notifier{},
		isIssueClosedFn: func(issue int) (bool, error) {
			t.Fatalf("active issue #%d must not fall through to a GitHub closed-state check", issue)
			return false, nil
		},
	}

	s := state.NewState()
	s.Sessions = map[string]*state.Session{
		"sup-old": {IssueNumber: 858, Status: state.StatusDone, PRNumber: 864},
		"sup-new": {IssueNumber: 858, Status: state.StatusRunning},
	}
	s.Approvals = []state.Approval{
		repairApproval("ap-repair-858-active", 858, 864, state.ApprovalStatusPending, now),
	}

	o.reconcileResolvedRepairApprovals(s)

	if got := approvalStatus(t, s, "ap-repair-858-active"); got != state.ApprovalStatusPending {
		t.Fatalf("repair approval with active session = %q, want pending", got)
	}
}

// TestReconcileResolvedRepairApprovals_ExternallyClosedIssue covers the
// externally-closed reconciliation path: an issue with no done session but
// closed on GitHub is reconciled; an issue that is still open (a failed session
// awaiting a legitimate repair decision) is left pending.
func TestReconcileResolvedRepairApprovals_ExternallyClosedIssue(t *testing.T) {
	now := time.Date(2026, 7, 10, 9, 47, 0, 0, time.UTC)
	closedIssues := map[int]bool{858: true, 700: false}
	o := &Orchestrator{
		cfg:      &config.Config{Repo: "owner/repo"},
		notifier: &notify.Notifier{},
		isIssueClosedFn: func(issue int) (bool, error) {
			return closedIssues[issue], nil
		},
	}

	s := state.NewState()
	// #700 has a failed session and is still open — repair approval is legitimate.
	s.Sessions = map[string]*state.Session{
		"sup-700": {IssueNumber: 700, Status: state.StatusRetryExhausted},
	}
	s.Approvals = []state.Approval{
		repairApproval("ap-repair-858", 858, 0, state.ApprovalStatusPending, now), // no session, closed externally
		repairApproval("ap-repair-700", 700, 0, state.ApprovalStatusPending, now), // failed session, still open
	}

	o.reconcileResolvedRepairApprovals(s)

	if got := approvalStatus(t, s, "ap-repair-858"); got != state.ApprovalStatusStale {
		t.Fatalf("repair approval for externally-closed issue = %q, want stale", got)
	}
	moot, _ := s.FindApproval("ap-repair-858")
	if last := moot.Audit[len(moot.Audit)-1]; !strings.Contains(last.Reason, "closed") {
		t.Fatalf("stale reason = %q, want it to mention the issue closed", last.Reason)
	}
	if got := approvalStatus(t, s, "ap-repair-700"); got != state.ApprovalStatusPending {
		t.Fatalf("repair approval for still-open issue = %q, want pending (untouched)", got)
	}
}

// TestVerifyOutcomeAfterMerge_ReconcilesRepairApproval reproduces the full #858
// sequence through the edge path: pending spawn_repair_worker approval → PR
// merges → outcome verifier passes → issue closes. The session moves to done and
// the repair approval is reconciled to stale in the same cycle.
func TestVerifyOutcomeAfterMerge_ReconcilesRepairApproval(t *testing.T) {
	now := time.Date(2026, 7, 10, 8, 31, 0, 0, time.UTC)
	closed := false
	o := &Orchestrator{
		cfg:            &config.Config{Repo: "owner/repo"},
		notifier:       &notify.Notifier{},
		outcomeCheckFn: healthyOutcome(),
		isIssueClosedFn: func(int) (bool, error) {
			return closed, nil
		},
		ghCloseIssueFn: func(int, string) error {
			closed = true
			return nil
		},
	}

	sess := &state.Session{IssueNumber: 858, Status: state.StatusCodeLanded, PRNumber: 864}
	s := state.NewState()
	s.Sessions = map[string]*state.Session{"sup-305": sess}
	s.Approvals = []state.Approval{
		repairApproval("ap-repair-858", 858, 864, state.ApprovalStatusPending, now),
	}

	o.verifyOutcomeAfterMerge(s, sess, 864)

	if sess.Status != state.StatusDone {
		t.Fatalf("session status = %q, want done after verified merge", sess.Status)
	}
	if got := approvalStatus(t, s, "ap-repair-858"); got != state.ApprovalStatusStale {
		t.Fatalf("repair approval after verified merge = %q, want stale", got)
	}
}

func TestVerifyOutcomeAfterMerge_KeepsRepairApprovalForActiveSameIssue(t *testing.T) {
	now := time.Date(2026, 7, 10, 8, 31, 0, 0, time.UTC)
	closed := false
	o := &Orchestrator{
		cfg:            &config.Config{Repo: "owner/repo"},
		notifier:       &notify.Notifier{},
		outcomeCheckFn: healthyOutcome(),
		isIssueClosedFn: func(int) (bool, error) {
			return closed, nil
		},
		ghCloseIssueFn: func(int, string) error {
			closed = true
			return nil
		},
	}

	landed := &state.Session{IssueNumber: 858, Status: state.StatusCodeLanded, PRNumber: 864}
	active := &state.Session{IssueNumber: 858, Status: state.StatusRunning}
	s := state.NewState()
	s.Sessions = map[string]*state.Session{"sup-old": landed, "sup-active": active}
	s.Approvals = []state.Approval{
		repairApproval("ap-repair-858-active-edge", 858, 864, state.ApprovalStatusPending, now),
	}

	o.verifyOutcomeAfterMerge(s, landed, 864)

	if landed.Status != state.StatusDone {
		t.Fatalf("landed session status = %q, want done", landed.Status)
	}
	if got := approvalStatus(t, s, "ap-repair-858-active-edge"); got != state.ApprovalStatusPending {
		t.Fatalf("repair approval with active same-issue session = %q, want pending", got)
	}
}

func TestReconcileCodeLandedSessions_KeepsRepairApprovalForActiveSameIssue(t *testing.T) {
	now := time.Date(2026, 7, 10, 8, 31, 0, 0, time.UTC)
	o := &Orchestrator{
		cfg: &config.Config{
			Repo: "owner/repo",
			Outcome: outcome.Brief{
				DesiredOutcome:      "Live app works",
				VerifierCommand:     "check-live",
				PassRequiredForDone: boolPtr(true),
			},
		},
		notifier:       &notify.Notifier{},
		outcomeCheckFn: healthyOutcome(),
		isPRMergedFn: func(pr int) (bool, error) {
			return pr == 864, nil
		},
		isIssueClosedFn: func(int) (bool, error) { return false, nil },
		ghCloseIssueFn:  func(int, string) error { return nil },
	}

	landed := &state.Session{IssueNumber: 858, Status: state.StatusCodeLanded, PRNumber: 864}
	active := &state.Session{IssueNumber: 858, Status: state.StatusRunning}
	s := state.NewState()
	s.Sessions = map[string]*state.Session{"sup-old": landed, "sup-active": active}
	s.Approvals = []state.Approval{
		repairApproval("ap-repair-858-active-reconcile", 858, 864, state.ApprovalStatusPending, now),
	}

	o.reconcileCodeLandedSessions(s)

	if landed.Status != state.StatusDone {
		t.Fatalf("landed session status = %q, want done", landed.Status)
	}
	if got := approvalStatus(t, s, "ap-repair-858-active-reconcile"); got != state.ApprovalStatusPending {
		t.Fatalf("repair approval with active same-issue session = %q, want pending", got)
	}
}

// TestReconcileMootRepairApprovals_MirrorsToSQLite proves acceptance criterion
// #10: when --approvals-store sqlite is active, the moot-approval stale
// transition is persisted consistently in BOTH the JSON state and the SQLite
// approval store, so a later approve/reject claim cannot act on a row the JSON
// state already retired.
func TestReconcileMootRepairApprovals_MirrorsToSQLite(t *testing.T) {
	now := time.Date(2026, 7, 10, 9, 47, 0, 0, time.UTC)
	stateDir := t.TempDir()
	store, err := approvalstore.Open(filepath.Join(t.TempDir(), "maestro.db"))
	if err != nil {
		t.Fatalf("open approvals store: %v", err)
	}
	t.Cleanup(func() { store.Close() })

	ap := repairApproval("ap-repair-858", 858, 864, state.ApprovalStatusPending, now)
	ap.PayloadHash = ap.ComputePayloadHash()
	rb := approvalstore.RowBinding{Project: "owner/repo", Repo: "owner/repo", StateDir: stateDir}
	if _, err := store.Put(context.Background(), &ap, rb); err != nil {
		t.Fatalf("seed approval into sqlite: %v", err)
	}

	o := &Orchestrator{
		cfg:              &config.Config{Repo: "owner/repo", StateDir: stateDir},
		repo:             "owner/repo",
		notifier:         &notify.Notifier{},
		approvalsBinding: approvalstore.Binding{Mode: approvalstore.ModeSQLite, Handle: store},
	}
	s := state.NewState()
	s.Approvals = []state.Approval{ap}

	n := o.reconcileMootRepairApprovals(s, 858, now, "issue #858 resolved (verified merge) — repair worker moot")
	if n != 1 {
		t.Fatalf("reconciled %d approvals, want 1", n)
	}
	if got := approvalStatus(t, s, "ap-repair-858"); got != state.ApprovalStatusStale {
		t.Fatalf("JSON state status = %q, want stale", got)
	}
	sqliteRow, err := store.Get(context.Background(), stateDir, "ap-repair-858")
	if err != nil {
		t.Fatalf("get sqlite row: %v", err)
	}
	if sqliteRow.Status != state.ApprovalStatusStale {
		t.Fatalf("SQLite status = %q, want stale (must agree with JSON state)", sqliteRow.Status)
	}
}
