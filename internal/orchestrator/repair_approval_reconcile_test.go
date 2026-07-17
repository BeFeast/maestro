package orchestrator

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/befeast/maestro/internal/approvalstore"
	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/github"
	"github.com/befeast/maestro/internal/notify"
	"github.com/befeast/maestro/internal/outcome"
	"github.com/befeast/maestro/internal/state"
	"github.com/befeast/maestro/internal/supervisor"
)

// TestAwaitingRepairDispatchReservesAndRespawnsOriginalSession is the #892
// incident regression: PR #7 is retained on txc-1, its approved repair is
// awaiting dispatch, and the ready issue is visible to the normal poll. The
// dispatcher must repair txc-1 in place, consume the approval, and never call
// the fresh slot allocator. Persist/reload then proves the completed dispatch
// is idempotent.
func TestAwaitingRepairDispatchReservesAndRespawnsOriginalSession(t *testing.T) {
	cfg := cfgWithBackends("codex", "codex")
	cfg.StateDir = t.TempDir()
	issues := []github.Issue{makeIssue(1, "repair PR #7", "maestro-ready")}
	o, freshStarts, _ := newStartWorkersOrchestrator(cfg, issues)
	o.hasOpenPRForIssueFn = func(issue int) (bool, error) { return issue == 1, nil }
	respawns := 0
	o.respawnInPlaceFn = func(cfg *config.Config, slot string, sess *state.Session, repo string, issue github.Issue, prompt, backend string) error {
		respawns++
		if slot != "txc-1" {
			t.Fatalf("respawn slot = %q, want txc-1", slot)
		}
		sess.Status = state.StatusRunning
		sess.PID = 7001
		return nil
	}

	now := time.Date(2026, 7, 13, 9, 3, 20, 0, time.UTC)
	s := state.NewState()
	s.Sessions["txc-1"] = &state.Session{
		IssueNumber: 1,
		IssueTitle:  "repair PR #7",
		Status:      state.StatusDead,
		PRNumber:    7,
		Worktree:    "/work/txc-1",
		Branch:      "feat/txc-1-1-repair",
		Backend:     "codex",
	}
	s.Approvals = []state.Approval{repairApproval("repair-1", 1, 7, state.ApprovalStatusAwaitingDispatch, now)}
	s.Approvals[0].Target.Session = "txc-1"

	o.startNewWorkers(s, 1)

	if respawns != 1 {
		t.Fatalf("in-place respawns = %d, want 1", respawns)
	}
	if len(*freshStarts) != 0 || len(s.Sessions) != 1 {
		t.Fatalf("fresh starts=%v sessions=%v, want only original txc-1", *freshStarts, s.Sessions)
	}
	if got := approvalStatus(t, s, "repair-1"); got != state.ApprovalStatusSuperseded {
		t.Fatalf("repair approval = %q, want consumed/superseded", got)
	}
	if err := state.Save(cfg.StateDir, s); err != nil {
		t.Fatalf("save dispatched state: %v", err)
	}
	reloaded, err := state.Load(cfg.StateDir)
	if err != nil {
		t.Fatalf("reload dispatched state: %v", err)
	}
	o.startNewWorkers(reloaded, 1)
	if respawns != 1 || len(*freshStarts) != 0 || len(reloaded.Sessions) != 1 {
		t.Fatalf("reload re-dispatched work: respawns=%d fresh=%v sessions=%v", respawns, *freshStarts, reloaded.Sessions)
	}
}

// A newer dynamic-wave candidate must not head-of-line block an exact-session
// repair that was already approved and moved to awaiting_dispatch. Fresh issue
// selection remains single-candidate; the repair is maintenance of an existing
// identity and survives the owned-ready filter.
func TestOwnedReadyFilterKeepsAwaitingExactSessionRepair(t *testing.T) {
	cfg := cfgWithBackends("codex", "codex")
	dynamicWaveEnabled := true
	cfg.Supervisor.DynamicWave.Enabled = &dynamicWaveEnabled
	cfg.Supervisor.DynamicWave.OwnsReadyLabel = true
	o, _, _ := newStartWorkersOrchestrator(cfg, nil)

	now := time.Date(2026, 7, 17, 19, 0, 0, 0, time.UTC)
	s := state.NewState()
	s.RecordSupervisorDecision(state.SupervisorDecision{
		ID:                "fresh-392",
		CreatedAt:         now,
		PolicyRule:        supervisor.PolicyRuleDynamicWave,
		RecommendedAction: supervisor.ActionSpawnWorker,
		Target:            &state.SupervisorTarget{Issue: 392},
		QueueAnalysis: &state.SupervisorQueueAnalysis{
			SelectedCandidate: &state.SupervisorIssueCandidate{Number: 392},
		},
	}, state.DefaultSupervisorDecisionLimit)
	repair := repairApproval("repair-390", 390, 0, state.ApprovalStatusAwaitingDispatch, now.Add(-time.Minute))
	repair.Target.Session = "ok-player-287"
	s.Approvals = append(s.Approvals, repair)

	issues := []github.Issue{
		makeIssue(390, "recover canonical worker", "ok-player-ready"),
		makeIssue(391, "unselected fresh work", "ok-player-ready"),
		makeIssue(392, "selected P0", "ok-player-ready"),
	}
	got := o.applySupervisorOwnedReadyFilter(s, issues)
	if len(got) != 2 || got[0].Number != 390 || got[1].Number != 392 {
		t.Fatalf("filtered issues = %+v, want exact repair #390 plus selected #392", got)
	}
}

// A competing same-issue worker is preserved and blocks the approved repair.
// The dispatcher must not kill/remove either worktree and must make the exact
// obsolete approval terminal so it cannot retry forever.
func TestAwaitingRepairDispatchRefusesCompetingWorker(t *testing.T) {
	cfg := cfgWithBackends("codex", "codex")
	issues := []github.Issue{makeIssue(1, "repair PR #7", "maestro-ready")}
	o, freshStarts, _ := newStartWorkersOrchestrator(cfg, issues)
	o.hasOpenPRForIssueFn = func(issue int) (bool, error) { return issue == 1, nil }
	respawns := 0
	o.respawnInPlaceFn = func(*config.Config, string, *state.Session, string, github.Issue, string, string) error {
		respawns++
		return nil
	}

	now := time.Date(2026, 7, 13, 9, 5, 15, 0, time.UTC)
	s := state.NewState()
	s.Sessions["txc-1"] = &state.Session{IssueNumber: 1, Status: state.StatusDead, PRNumber: 7, Worktree: "/work/txc-1"}
	s.Sessions["txc-2"] = &state.Session{IssueNumber: 1, Status: state.StatusRunning, Worktree: "/work/txc-2", PID: 7002}
	s.Approvals = []state.Approval{repairApproval("repair-1", 1, 7, state.ApprovalStatusAwaitingDispatch, now)}
	s.Approvals[0].Target.Session = "txc-1"

	o.startNewWorkers(s, 1)

	if respawns != 0 || len(*freshStarts) != 0 {
		t.Fatalf("dispatch occurred despite competitor: respawns=%d fresh=%v", respawns, *freshStarts)
	}
	if len(s.Sessions) != 2 || s.Sessions["txc-2"].Worktree != "/work/txc-2" {
		t.Fatalf("competing material changed: %+v", s.Sessions)
	}
	if got := approvalStatus(t, s, "repair-1"); got != state.ApprovalStatusStale {
		t.Fatalf("repair approval = %q, want stale after canonical claim wins", got)
	}
}

// The live #940 soak regression: an old awaiting-dispatch approval referenced
// a session that no longer existed. The dispatcher refused it every minute but
// left it active for days. One failed exact-reservation validation must stale
// the approval, preserve every other approval, and make later cycles no-ops.
func TestAwaitingRepairDispatchMissingReservedSessionBecomesStale(t *testing.T) {
	cfg := cfgWithBackends("codex", "codex")
	issues := []github.Issue{makeIssue(877, "repair PR #891", "maestro-ready")}
	o, freshStarts, _ := newStartWorkersOrchestrator(cfg, issues)
	o.hasOpenPRForIssueFn = func(issue int) (bool, error) { return issue == 877, nil }

	now := time.Date(2026, 7, 17, 20, 52, 47, 0, time.UTC)
	s := state.NewState()
	missing := repairApproval("repair-missing", 877, 891, state.ApprovalStatusAwaitingDispatch, now)
	missing.Target.Session = "sup-316"
	unrelated := repairApproval("repair-unrelated", 900, 904, state.ApprovalStatusAwaitingDispatch, now)
	unrelated.Target.Session = "sup-347"
	s.Approvals = []state.Approval{missing, unrelated}

	o.startNewWorkers(s, 1)
	o.startNewWorkers(s, 1)

	if len(*freshStarts) != 0 || len(s.Sessions) != 0 {
		t.Fatalf("invalid exact repair created work: starts=%v sessions=%v", *freshStarts, s.Sessions)
	}
	if got := approvalStatus(t, s, "repair-missing"); got != state.ApprovalStatusStale {
		t.Fatalf("missing-session approval = %q, want stale", got)
	}
	if got := approvalStatus(t, s, "repair-unrelated"); got != state.ApprovalStatusAwaitingDispatch {
		t.Fatalf("unrelated approval = %q, want awaiting_dispatch", got)
	}
	approval, _ := s.FindApproval("repair-missing")
	if last := approval.Audit[len(approval.Audit)-1]; last.Event != state.ApprovalAuditStale || !strings.Contains(last.Reason, "sup-316") {
		t.Fatalf("stale audit = {%q,%q}, want exact missing-session reason", last.Event, last.Reason)
	}
}

// A valid exact repair must not lose to a lexically earlier approval-derived
// claim whose own session is missing. Reconcile the invalid sibling first,
// then dispatch and consume the valid approval on its canonical worktree.
func TestAwaitingRepairDispatchValidReservationOutranksInvalidSiblingApproval(t *testing.T) {
	cfg := cfgWithBackends("codex", "codex")
	issues := []github.Issue{makeIssue(877, "repair PR #891", "maestro-ready")}
	o, freshStarts, _ := newStartWorkersOrchestrator(cfg, issues)
	o.hasOpenPRForIssueFn = func(issue int) (bool, error) { return issue == 877, nil }
	respawns := 0
	o.respawnInPlaceFn = func(_ *config.Config, slot string, sess *state.Session, _ string, _ github.Issue, _, _ string) error {
		respawns++
		if slot != "sup-valid" {
			t.Fatalf("respawn slot = %q, want sup-valid", slot)
		}
		sess.Status = state.StatusRunning
		return nil
	}

	now := time.Date(2026, 7, 17, 21, 1, 32, 0, time.UTC)
	s := state.NewState()
	s.Sessions["sup-valid"] = &state.Session{IssueNumber: 877, Status: state.StatusDead, PRNumber: 891, Worktree: "/work/sup-valid"}
	valid := repairApproval("z-valid", 877, 891, state.ApprovalStatusAwaitingDispatch, now)
	valid.Target.Session = "sup-valid"
	invalid := repairApproval("a-invalid", 877, 891, state.ApprovalStatusAwaitingDispatch, now)
	invalid.Target.Session = "sup-missing"
	// Selection uses durable insertion order while claims are sorted by ID,
	// reproducing the review-reported ordering edge.
	s.Approvals = []state.Approval{valid, invalid}

	o.startNewWorkers(s, 1)

	if respawns != 1 || len(*freshStarts) != 0 || len(s.Sessions) != 1 {
		t.Fatalf("valid repair did not dispatch exactly once: respawns=%d fresh=%v sessions=%v", respawns, *freshStarts, s.Sessions)
	}
	if got := approvalStatus(t, s, "z-valid"); got != state.ApprovalStatusSuperseded {
		t.Fatalf("valid approval = %q, want consumed/superseded", got)
	}
	if got := approvalStatus(t, s, "a-invalid"); got != state.ApprovalStatusStale {
		t.Fatalf("invalid sibling approval = %q, want stale", got)
	}
}

// Multiple stale approvals can suppress one another for the same missing
// sibling session. Reconciliation must peel the whole chain before deciding
// whether the selected canonical repair has a competing claim.
func TestAwaitingRepairDispatchValidReservationOutranksMultipleInvalidSiblingApprovals(t *testing.T) {
	cfg := cfgWithBackends("codex", "codex")
	issues := []github.Issue{makeIssue(877, "repair PR #891", "maestro-ready")}
	o, freshStarts, _ := newStartWorkersOrchestrator(cfg, issues)
	o.hasOpenPRForIssueFn = func(issue int) (bool, error) { return issue == 877, nil }
	respawns := 0
	o.respawnInPlaceFn = func(_ *config.Config, slot string, sess *state.Session, _ string, _ github.Issue, _, _ string) error {
		respawns++
		if slot != "sup-valid" {
			t.Fatalf("respawn slot = %q, want sup-valid", slot)
		}
		sess.Status = state.StatusRunning
		return nil
	}

	now := time.Date(2026, 7, 17, 21, 35, 0, 0, time.UTC)
	s := state.NewState()
	s.Sessions["sup-valid"] = &state.Session{IssueNumber: 877, Status: state.StatusDead, PRNumber: 891, Worktree: "/work/sup-valid"}
	valid := repairApproval("z-valid", 877, 891, state.ApprovalStatusAwaitingDispatch, now)
	valid.Target.Session = "sup-valid"
	invalidA := repairApproval("a-invalid", 877, 891, state.ApprovalStatusAwaitingDispatch, now)
	invalidA.Target.Session = "sup-missing"
	invalidB := repairApproval("b-invalid", 877, 891, state.ApprovalStatusAwaitingDispatch, now)
	invalidB.Target.Session = "sup-missing"
	s.Approvals = []state.Approval{valid, invalidA, invalidB}

	o.startNewWorkers(s, 1)

	if respawns != 1 || len(*freshStarts) != 0 || len(s.Sessions) != 1 {
		t.Fatalf("valid repair did not dispatch exactly once: respawns=%d fresh=%v sessions=%v", respawns, *freshStarts, s.Sessions)
	}
	if got := approvalStatus(t, s, "z-valid"); got != state.ApprovalStatusSuperseded {
		t.Fatalf("valid approval = %q, want consumed/superseded", got)
	}
	for _, id := range []string{"a-invalid", "b-invalid"} {
		if got := approvalStatus(t, s, id); got != state.ApprovalStatusStale {
			t.Fatalf("invalid sibling approval %s = %q, want stale", id, got)
		}
	}
}

// Staling an approval can reveal the session claim that the approval had been
// suppressing. If that real session is still running, it remains canonical and
// must block the selected repair even though its old approval named a stale PR.
func TestAwaitingRepairDispatchRechecksSessionRevealedByStaleSiblingApproval(t *testing.T) {
	cfg := cfgWithBackends("codex", "codex")
	issues := []github.Issue{makeIssue(877, "repair PR #891", "maestro-ready")}
	o, freshStarts, _ := newStartWorkersOrchestrator(cfg, issues)
	o.hasOpenPRForIssueFn = func(issue int) (bool, error) { return issue == 877, nil }
	respawns := 0
	o.respawnInPlaceFn = func(_ *config.Config, _ string, _ *state.Session, _ string, _ github.Issue, _, _ string) error {
		respawns++
		return nil
	}

	now := time.Date(2026, 7, 17, 21, 10, 12, 0, time.UTC)
	s := state.NewState()
	s.Sessions["sup-valid"] = &state.Session{IssueNumber: 877, Status: state.StatusDead, PRNumber: 891, Worktree: "/work/sup-valid"}
	s.Sessions["sup-running"] = &state.Session{IssueNumber: 877, Status: state.StatusRunning, PRNumber: 891, Worktree: "/work/sup-running", PID: 9911}
	selected := repairApproval("z-selected", 877, 891, state.ApprovalStatusAwaitingDispatch, now)
	selected.Target.Session = "sup-valid"
	staleSibling := repairApproval("a-stale-pr", 877, 890, state.ApprovalStatusAwaitingDispatch, now)
	staleSibling.Target.Session = "sup-running"
	s.Approvals = []state.Approval{selected, staleSibling}

	o.startNewWorkers(s, 1)

	if respawns != 0 || len(*freshStarts) != 0 || len(s.Sessions) != 2 {
		t.Fatalf("dispatch overlapped revealed running session: respawns=%d fresh=%v sessions=%v", respawns, *freshStarts, s.Sessions)
	}
	if got := approvalStatus(t, s, "a-stale-pr"); got != state.ApprovalStatusStale {
		t.Fatalf("stale-PR sibling approval = %q, want stale", got)
	}
	if got := approvalStatus(t, s, "z-selected"); got != state.ApprovalStatusStale {
		t.Fatalf("selected approval = %q, want stale after revealed running session wins", got)
	}
	if got := s.Sessions["sup-running"].Status; got != state.StatusRunning {
		t.Fatalf("revealed canonical session status = %q, want running", got)
	}
}

// A repair approval is authority for the state that was reviewed, not a
// timeless bypass. If the operator adds blocked before dispatch, the current
// guard wins, the approval becomes stale, and neither an in-place nor a fresh
// worker may start even though the canonical PR remains open.
func TestAwaitingRepairDispatchBlockedBeforeExecutionDoesNotSpawn(t *testing.T) {
	cfg := cfgWithBackends("codex", "codex")
	cfg.ExcludeLabels = []string{"blocked"}
	issues := []github.Issue{makeIssue(331, "canonical PR #335", "maestro-ready", "blocked")}
	o, freshStarts, _ := newStartWorkersOrchestrator(cfg, issues)
	o.hasOpenPRForIssueFn = func(issue int) (bool, error) { return issue == 331, nil }
	respawns := 0
	o.respawnInPlaceFn = func(*config.Config, string, *state.Session, string, github.Issue, string, string) error {
		respawns++
		return nil
	}

	now := time.Date(2026, 7, 17, 9, 18, 38, 0, time.UTC)
	s := state.NewState()
	s.Sessions["ok-player-247"] = &state.Session{
		IssueNumber: 331,
		Status:      state.StatusPROpen,
		PRNumber:    335,
		Worktree:    "/work/ok-player-247",
	}
	s.Approvals = []state.Approval{repairApproval("repair-331", 331, 335, state.ApprovalStatusAwaitingDispatch, now)}
	s.Approvals[0].Target.Session = "ok-player-247"

	o.startNewWorkers(s, 1)

	if respawns != 0 || len(*freshStarts) != 0 || len(s.Sessions) != 1 {
		t.Fatalf("blocked repair dispatched: respawns=%d fresh=%v sessions=%v", respawns, *freshStarts, s.Sessions)
	}
	if got := approvalStatus(t, s, "repair-331"); got != state.ApprovalStatusStale {
		t.Fatalf("repair approval = %q, want stale after current blocked guard", got)
	}
}

// A blocked issue normally loses its ready label and therefore never enters
// startNewWorkers. The standing reconciler must still retire any delayed
// classic or review-repair authority so it cannot execute on a later cycle.
func TestReconcileGuardedRepairApprovals_BlockedWithoutReadyLabel(t *testing.T) {
	now := time.Date(2026, 7, 17, 9, 22, 0, 0, time.UTC)
	o := &Orchestrator{
		cfg:      &config.Config{Repo: "owner/repo", ExcludeLabels: []string{"blocked"}},
		notifier: &notify.Notifier{},
		getIssueFn: func(issue int) (github.Issue, error) {
			if issue != 331 {
				t.Fatalf("get issue = %d, want 331", issue)
			}
			return makeIssue(331, "canonical PR #335", "blocked"), nil
		},
	}
	s := state.NewState()
	s.Sessions["ok-player-247"] = &state.Session{
		IssueNumber: 331,
		Status:      state.StatusPROpen,
		PRNumber:    335,
		Worktree:    "/work/ok-player-247",
	}
	classic := repairApproval("repair-331", 331, 335, state.ApprovalStatusAwaitingDispatch, now)
	classic.Target.Session = "ok-player-247"
	review := repairApproval("review-repair-331", 331, 335, state.ApprovalStatusApproved, now)
	review.Action = supervisor.ActionSpawnReviewRepair
	review.Target.Session = "ok-player-247"
	pending := repairApproval("pending-repair-331", 331, 335, state.ApprovalStatusPending, now)
	pending.Target.Session = "ok-player-247"
	s.Approvals = []state.Approval{classic, review, pending}

	o.reconcileGuardedRepairApprovals(s)

	for _, id := range []string{"repair-331", "review-repair-331", "pending-repair-331"} {
		if got := approvalStatus(t, s, id); got != state.ApprovalStatusStale {
			t.Fatalf("approval %s = %q, want stale", id, got)
		}
	}
}

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
// on the next cycle because GitHub confirms the target issue is closed. A local
// done token alone is not authoritative (#949). Unrelated
// approvals — a repair approval for an actively-worked issue and a spawn_worker
// approval — are untouched, and the pass is idempotent.
func TestReconcileResolvedRepairApprovals_DoneSessionSelfHeals(t *testing.T) {
	now := time.Date(2026, 7, 10, 9, 47, 0, 0, time.UTC)
	o := &Orchestrator{
		cfg:      &config.Config{Repo: "owner/repo"},
		notifier: &notify.Notifier{},
		isIssueClosedFn: func(issue int) (bool, error) {
			return issue == 858, nil
		},
	}

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
	if last.Event != state.ApprovalAuditStale || !strings.Contains(last.Reason, "closed") {
		t.Fatalf("stale audit = {%q,%q}, want stale reason mentioning authoritative issue close", last.Event, last.Reason)
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

// A false local done token must not erase repair authority while GitHub still
// shows an open issue and no merged linked PR (#949).
func TestReconcileResolvedRepairApprovals_FalseDoneDoesNotStale(t *testing.T) {
	now := time.Date(2026, 7, 17, 19, 22, 0, 0, time.UTC)
	o := &Orchestrator{
		cfg:                   &config.Config{Repo: "owner/repo"},
		notifier:              &notify.Notifier{},
		isIssueClosedFn:       func(int) (bool, error) { return false, nil },
		hasMergedPRForIssueFn: func(int) (bool, error) { return false, nil },
	}
	s := state.NewState()
	s.Sessions["ok-player-273"] = &state.Session{
		IssueNumber: 345,
		Status:      state.StatusDone,
		PRNumber:    389,
	}
	s.Approvals = []state.Approval{
		repairApproval("repair-345", 345, 388, state.ApprovalStatusAwaitingDispatch, now),
	}

	o.reconcileResolvedRepairApprovals(s)

	if got := approvalStatus(t, s, "repair-345"); got != state.ApprovalStatusAwaitingDispatch {
		t.Fatalf("approval = %q, want awaiting_dispatch while issue remains open and unmerged", got)
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

// A terminal-failure session must also outrank the done-session shortcut. The
// GitHub issue is the authority in this ambiguous history: if it is open the
// repair remains actionable; if it is closed the approval is safely staled.
func TestReconcileResolvedRepairApprovals_FailedSessionOutranksOlderDone(t *testing.T) {
	now := time.Date(2026, 7, 10, 9, 47, 0, 0, time.UTC)
	for _, tc := range []struct {
		name        string
		issueClosed bool
		want        state.ApprovalStatus
	}{
		{name: "open issue keeps repair pending", issueClosed: false, want: state.ApprovalStatusPending},
		{name: "closed issue stales repair", issueClosed: true, want: state.ApprovalStatusStale},
	} {
		t.Run(tc.name, func(t *testing.T) {
			checks := 0
			o := &Orchestrator{
				cfg:      &config.Config{Repo: "owner/repo"},
				notifier: &notify.Notifier{},
				isIssueClosedFn: func(issue int) (bool, error) {
					checks++
					return tc.issueClosed, nil
				},
			}

			s := state.NewState()
			s.Sessions = map[string]*state.Session{
				"sup-old-done":   {IssueNumber: 858, Status: state.StatusDone, PRNumber: 864},
				"sup-new-failed": {IssueNumber: 858, Status: state.StatusRetryExhausted, PRNumber: 867},
			}
			s.Approvals = []state.Approval{
				repairApproval("ap-repair-858-after-failure", 858, 867, state.ApprovalStatusPending, now),
			}

			o.reconcileResolvedRepairApprovals(s)

			if checks != 1 {
				t.Fatalf("GitHub issue checks = %d, want 1", checks)
			}
			if got := approvalStatus(t, s, "ap-repair-858-after-failure"); got != tc.want {
				t.Fatalf("repair approval = %q, want %q", got, tc.want)
			}
		})
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
