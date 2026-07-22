package state

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestIssueClaimFor_AwaitingRepairApprovalReservesOriginalSession(t *testing.T) {
	s := NewState()
	s.Sessions["txc-1"] = &Session{IssueNumber: 1, Status: StatusDead, PRNumber: 7, Worktree: "/work/txc-1"}
	s.Approvals = []Approval{{
		ID:     "repair-1",
		Action: approvalActionSpawnRepairWorker,
		Status: ApprovalStatusAwaitingDispatch,
		Target: &SupervisorTarget{Issue: 1, PR: 7, Session: "txc-1"},
	}}

	claim, ok := s.IssueClaimFor(1)
	if !ok {
		t.Fatal("awaiting repair approval must reserve issue #1")
	}
	if claim.Kind != IssueClaimRepairDispatch || claim.Session != "txc-1" || claim.PRNumber != 7 || claim.ApprovalID != "repair-1" {
		t.Fatalf("claim = %+v, want repair reservation for txc-1 / PR #7", claim)
	}
	if !strings.Contains(claim.Reason, "txc-1") {
		t.Fatalf("reason = %q, want reserved session", claim.Reason)
	}
	if !s.IssueInProgress(1) {
		t.Fatal("awaiting repair reservation must make the issue in progress")
	}
}

func TestActiveIssueClaims_ExposesCompetingSameIssueSession(t *testing.T) {
	s := NewState()
	s.Sessions["txc-1"] = &Session{IssueNumber: 1, Status: StatusDead, PRNumber: 7}
	s.Sessions["txc-2"] = &Session{IssueNumber: 1, Status: StatusRunning}
	s.Approvals = []Approval{{
		ID:     "repair-1",
		Action: approvalActionSpawnRepairWorker,
		Status: ApprovalStatusAwaitingDispatch,
		Target: &SupervisorTarget{Issue: 1, PR: 7, Session: "txc-1"},
	}}

	claims := s.ActiveIssueClaims()
	if len(claims) != 2 {
		t.Fatalf("claims = %+v, want repair reservation plus competing txc-2", claims)
	}
	if claims[0].ApprovalID != "repair-1" || claims[1].Session != "txc-2" {
		t.Fatalf("claims = %+v, want repair claim followed by competing session", claims)
	}
}

func TestIssueClaimFor_RetainsOpenPRMaintenanceAcrossReload(t *testing.T) {
	dir := t.TempDir()
	s := NewState()
	s.Sessions["txc-1"] = &Session{IssueNumber: 1, Status: StatusRetryExhausted, PRNumber: 7, Worktree: "/work/txc-1"}
	if err := Save(dir, s); err != nil {
		t.Fatalf("save: %v", err)
	}
	reloaded, err := Load(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	claim, ok := reloaded.IssueClaimFor(1)
	if !ok || claim.Kind != IssueClaimOpenPRMaintenance || claim.Session != "txc-1" {
		t.Fatalf("claim after reload = %+v, %v", claim, ok)
	}

	// Explicit release is the only state-level path that makes a closed-PR
	// terminal attempt selectable again.
	reloaded.Sessions["txc-1"].ReleasedForRedispatch = true
	if _, ok := reloaded.IssueClaimFor(1); ok {
		t.Fatal("released session must not retain an open-PR maintenance claim")
	}
}

func TestIssueClaimFor_DeadScheduledRetryRemainsReserved(t *testing.T) {
	next := time.Now().UTC().Add(time.Minute)
	s := NewState()
	s.Sessions["txc-1"] = &Session{IssueNumber: 1, Status: StatusDead, NextRetryAt: &next}

	claim, ok := s.IssueClaimFor(1)
	if !ok || claim.Kind != IssueClaimScheduledRetry {
		t.Fatalf("claim = %+v, %v, want scheduled retry", claim, ok)
	}
}

func TestIssueClaimFor_TerminalSafetyOutcomeSurvivesWorktreeCleanup(t *testing.T) {
	for _, outcome := range []string{string(DisplayTokenBudgetExceeded), WorkerOutcomeRepeatedUnexpectedExit} {
		t.Run(outcome, func(t *testing.T) {
			s := NewState()
			s.Sessions["txc-1"] = &Session{
				IssueNumber:   1,
				Status:        StatusFailed,
				WorkerOutcome: outcome,
				// Worktree is intentionally empty: automatic cleanup already ran.
			}

			claim, ok := s.IssueClaimFor(1)
			if !ok || claim.Kind != IssueClaimTerminalFailure || claim.Session != "txc-1" {
				t.Fatalf("claim = %+v, %v, want durable terminal-failure claim", claim, ok)
			}
			if !strings.Contains(claim.Reason, outcome) {
				t.Fatalf("reason = %q, want durable outcome %q", claim.Reason, outcome)
			}

			s.Sessions["txc-1"].ReleasedForRedispatch = true
			if _, ok := s.IssueClaimFor(1); ok {
				t.Fatal("explicit release must drop the terminal-failure claim")
			}
		})
	}
}

func TestIssueClaimFor_DonePRRemainsReservedUntilExplicitRelease(t *testing.T) {
	s := NewState()
	finishedAt := time.Now().UTC()
	s.Sessions["ok-player-274"] = &Session{IssueNumber: 365, Status: StatusDone, PRNumber: 370, FinishedAt: &finishedAt}

	claim, ok := s.IssueClaimFor(365)
	if !ok || claim.Kind != IssueClaimTerminalReconcile || claim.Session != "ok-player-274" || claim.PRNumber != 370 {
		t.Fatalf("claim = %+v, %v; want terminal reconciliation lease", claim, ok)
	}
	if !s.IssueInProgress(365) {
		t.Fatal("done PR must keep issue reserved through close-issue reconciliation")
	}

	s.Sessions["ok-player-274"].ReleasedForRedispatch = true
	if _, ok := s.IssueClaimFor(365); ok {
		t.Fatal("explicitly released done session must not retain terminal reconciliation lease")
	}
}

func TestFreshDispatchClaim_ContendsThenRenewsSameCanonicalIdentity(t *testing.T) {
	s := NewState()
	now := time.Date(2026, 7, 21, 21, 0, 0, 0, time.UTC)
	claim, acquired, err := s.ClaimFreshDispatch(394, "ok-player", "lease-a", 10*time.Minute, now)
	if err != nil || !acquired {
		t.Fatalf("initial claim: acquired=%t err=%v", acquired, err)
	}
	claim.Branch = "feat/ok-player-1-394-canonical"
	claim.Worktree = filepath.Join("/worktrees", claim.Slot)

	contended, acquired, err := s.ClaimFreshDispatch(394, "ok-player", "lease-b", 10*time.Minute, now.Add(31*time.Second))
	if err != nil || acquired {
		t.Fatalf("contending claim: acquired=%t err=%v", acquired, err)
	}
	if contended.Slot != claim.Slot || contended.LeaseID != "lease-a" || contended.ContentionCount != 1 || contended.LastContendedAt.IsZero() {
		t.Fatalf("contended claim = %+v", contended)
	}

	renewed, acquired, err := s.ClaimFreshDispatch(394, "ok-player", "lease-c", 10*time.Minute, now.Add(11*time.Minute))
	if err != nil || !acquired {
		t.Fatalf("renew claim: acquired=%t err=%v", acquired, err)
	}
	if renewed.Slot != claim.Slot || renewed.Branch != claim.Branch || renewed.Worktree != claim.Worktree {
		t.Fatalf("renewed identity changed: before=%+v after=%+v", claim, renewed)
	}
	if renewed.LeaseGeneration != 2 || renewed.LeaseID != "lease-c" || s.NextSlot != 2 {
		t.Fatalf("renewed lease = %+v next_slot=%d", renewed, s.NextSlot)
	}
}

func TestFreshDispatchClaim_CompletionCreatesSessionAndRetainsEvidence(t *testing.T) {
	s := NewState()
	now := time.Date(2026, 7, 21, 21, 0, 0, 0, time.UTC)
	claim, acquired, err := s.ClaimFreshDispatch(394, "ok-player", "lease-a", 10*time.Minute, now)
	if err != nil || !acquired {
		t.Fatalf("claim: acquired=%t err=%v", acquired, err)
	}
	claim.Branch = "feat/ok-player-1-394-canonical"
	claim.Worktree = filepath.Join("/worktrees", claim.Slot)
	sess := &Session{
		IssueNumber: 394,
		IssueTitle:  "canonical",
		Worktree:    claim.Worktree,
		Branch:      claim.Branch,
		PID:         4242,
		TmuxSession: "maestro-" + claim.Slot,
		StartedAt:   now.Add(time.Minute),
		Status:      StatusRunning,
	}
	if err := s.CompleteFreshDispatch(394, "lease-a", sess, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, active := s.FreshDispatchClaimFor(394); active {
		t.Fatal("completed claim remained active")
	}
	evidence := s.FreshDispatchClaims[394]
	if evidence == nil || evidence.Status != FreshDispatchClaimStatusCompleted || evidence.CompletedAt.IsZero() || evidence.SessionStartedAt.IsZero() {
		t.Fatalf("completed evidence = %+v", evidence)
	}
	if got := s.Sessions[claim.Slot]; got == nil || got.IssueNumber != 394 || got.PID != 4242 {
		t.Fatalf("persisted session = %+v", got)
	}
	claimView, ok := s.IssueClaimFor(394)
	if !ok || claimView.Kind != IssueClaimImplementation || claimView.Session != claim.Slot {
		t.Fatalf("issue claim after completion = %+v, %t", claimView, ok)
	}
}

func TestFreshDispatchClaim_SupersedeReleasesExactLease(t *testing.T) {
	s := NewState()
	now := time.Date(2026, 7, 21, 21, 0, 0, 0, time.UTC)
	claim, acquired, err := s.ClaimFreshDispatch(394, "ok-player", "lease-a", 10*time.Minute, now)
	if err != nil || !acquired {
		t.Fatalf("claim: acquired=%t err=%v", acquired, err)
	}
	claim.Branch = "feat/ok-player-1-394-canonical"
	claim.Worktree = filepath.Join("/worktrees", claim.Slot)

	if err := s.SupersedeFreshDispatch(394, "stale-lease", "start_failed", now.Add(time.Minute)); err == nil {
		t.Fatal("stale lease owner superseded the active claim")
	}
	if claim.Status != FreshDispatchClaimStatusClaimed {
		t.Fatalf("claim status after stale supersede = %q, want claimed", claim.Status)
	}

	completedAt := now.Add(2 * time.Minute)
	if err := s.SupersedeFreshDispatch(394, "lease-a", "start_failed", completedAt); err != nil {
		t.Fatal(err)
	}
	if _, active := s.FreshDispatchClaimFor(394); active {
		t.Fatal("superseded claim remained active")
	}
	if _, claimed := s.IssueClaimFor(394); claimed {
		t.Fatal("superseded lease still appeared as an active issue claim")
	}
	if claim.Status != FreshDispatchClaimStatusSuperseded || claim.TerminalReason != "start_failed" {
		t.Fatalf("superseded claim = %+v", claim)
	}
	if !claim.CompletedAt.Equal(completedAt) || !claim.UpdatedAt.Equal(completedAt) || !claim.LeaseExpiresAt.IsZero() {
		t.Fatalf("superseded timestamps = %+v", claim)
	}

	next, acquired, err := s.ClaimFreshDispatch(394, "ok-player", "lease-b", 10*time.Minute, completedAt.Add(time.Minute))
	if err != nil || !acquired {
		t.Fatalf("redispatch claim: acquired=%t err=%v", acquired, err)
	}
	if next != claim || next.Slot != "ok-player-1" || next.Branch != "feat/ok-player-1-394-canonical" || next.Worktree != filepath.Join("/worktrees", "ok-player-1") {
		t.Fatalf("redispatch identity changed = %+v", next)
	}
	if next.Status != FreshDispatchClaimStatusClaimed || next.LeaseID != "lease-b" || next.LeaseGeneration != 2 || s.NextSlot != 2 {
		t.Fatalf("redispatch lease = %+v next_slot=%d", next, s.NextSlot)
	}
	if !next.CompletedAt.IsZero() || !next.SessionStartedAt.IsZero() || next.TerminalReason != "" {
		t.Fatalf("redispatch retained terminal evidence = %+v", next)
	}
}

func TestFreshDispatchClaim_StaleOwnerCannotSupersedeRenewedLease(t *testing.T) {
	s := NewState()
	now := time.Date(2026, 7, 21, 21, 0, 0, 0, time.UTC)
	claim, _, _ := s.ClaimFreshDispatch(394, "ok-player", "lease-a", time.Minute, now)
	renewed, acquired, err := s.ClaimFreshDispatch(394, "ok-player", "lease-b", time.Minute, now.Add(2*time.Minute))
	if err != nil || !acquired {
		t.Fatalf("renew claim: acquired=%t err=%v", acquired, err)
	}
	if err := s.SupersedeFreshDispatch(394, "lease-a", "start_failed", now.Add(3*time.Minute)); err == nil {
		t.Fatal("stale owner superseded renewed lease")
	}
	if renewed.Status != FreshDispatchClaimStatusClaimed || renewed.LeaseID != "lease-b" || renewed.LeaseGeneration != 2 {
		t.Fatalf("renewed lease changed = %+v (initial %+v)", renewed, claim)
	}
}

func TestFreshDispatchClaim_ConcurrentSaveKeepsContentionAndCompletion(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 7, 21, 21, 0, 0, 0, time.UTC)
	seed := NewState()
	claim, _, _ := seed.ClaimFreshDispatch(394, "ok-player", "lease-a", 10*time.Minute, now)
	claim.Branch = "feat/ok-player-1-394-canonical"
	claim.Worktree = filepath.Join(dir, claim.Slot)
	if err := Save(dir, seed); err != nil {
		t.Fatal(err)
	}

	owner, _ := Load(dir)
	contender, _ := Load(dir)
	if _, acquired, err := contender.ClaimFreshDispatch(394, "ok-player", "lease-b", 10*time.Minute, now.Add(time.Minute)); err != nil || acquired {
		t.Fatalf("contender: acquired=%t err=%v", acquired, err)
	}
	if err := Save(dir, contender); err != nil {
		t.Fatal(err)
	}
	sess := &Session{IssueNumber: 394, Worktree: claim.Worktree, Branch: claim.Branch, PID: 44, StartedAt: now, Status: StatusRunning}
	if err := owner.CompleteFreshDispatch(394, "lease-a", sess, now.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := Save(dir, owner); err != nil {
		t.Fatal(err)
	}
	loaded, _ := Load(dir)
	evidence := loaded.FreshDispatchClaims[394]
	if evidence.Status != FreshDispatchClaimStatusCompleted || evidence.ContentionCount != 1 {
		t.Fatalf("merged evidence = %+v", evidence)
	}
	if loaded.Sessions[claim.Slot] == nil {
		t.Fatal("completed session lost during merge")
	}
}

func TestFreshDispatchClaim_ConcurrentSaveKeepsSupersededAtEqualTimestamp(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 7, 21, 21, 0, 0, 0, time.UTC)
	seed := NewState()
	if _, acquired, err := seed.ClaimFreshDispatch(394, "ok-player", "lease-a", 10*time.Minute, now); err != nil || !acquired {
		t.Fatalf("seed claim: acquired=%t err=%v", acquired, err)
	}
	if err := Save(dir, seed); err != nil {
		t.Fatal(err)
	}

	owner, _ := Load(dir)
	contender, _ := Load(dir)
	terminalAt := now.Add(time.Minute)
	if err := owner.SupersedeFreshDispatch(394, "lease-a", "start_failed", terminalAt); err != nil {
		t.Fatal(err)
	}
	if _, acquired, err := contender.ClaimFreshDispatch(394, "ok-player", "lease-b", 10*time.Minute, terminalAt); err != nil || acquired {
		t.Fatalf("contender: acquired=%t err=%v", acquired, err)
	}
	if err := Save(dir, contender); err != nil {
		t.Fatal(err)
	}
	if err := Save(dir, owner); err != nil {
		t.Fatal(err)
	}

	loaded, _ := Load(dir)
	claim := loaded.FreshDispatchClaims[394]
	if claim.Status != FreshDispatchClaimStatusSuperseded || claim.TerminalReason != "start_failed" || claim.ContentionCount != 1 {
		t.Fatalf("merged superseded evidence = %+v", claim)
	}
}

func TestFreshDispatchClaim_ReconcilesCrashAfterSessionSave(t *testing.T) {
	s := NewState()
	now := time.Date(2026, 7, 21, 21, 0, 0, 0, time.UTC)
	claim, _, _ := s.ClaimFreshDispatch(394, "ok-player", "lease-a", 10*time.Minute, now)
	claim.Branch = "feat/ok-player-1-394-canonical"
	claim.Worktree = filepath.Join("/worktrees", claim.Slot)
	s.Sessions[claim.Slot] = &Session{
		IssueNumber: 394,
		Worktree:    claim.Worktree,
		Branch:      claim.Branch,
		StartedAt:   now.Add(time.Minute),
		Status:      StatusRunning,
	}
	if got := s.ReconcileFreshDispatchClaims(now.Add(2 * time.Minute)); got != 1 {
		t.Fatalf("reconciled = %d, want 1", got)
	}
	if claim.Status != FreshDispatchClaimStatusCompleted || claim.TerminalReason != "session_persisted" || claim.SessionStartedAt.IsZero() {
		t.Fatalf("reconciled claim = %+v", claim)
	}
}
