package state

import (
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
