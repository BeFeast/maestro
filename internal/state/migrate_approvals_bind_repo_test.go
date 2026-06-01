package state

import (
	"testing"
	"time"
)

// #489: MigrateApprovalsBindRepo back-fills the Repo stamp on in-flight
// approvals that predate the cross-project guard. Terminal-status
// approvals are not retroactively rewritten — their audit trail is final.

func TestMigrateApprovalsBindRepo_StampsInFlightApprovals(t *testing.T) {
	s := NewState()
	now := time.Now().UTC()
	s.Approvals = []Approval{
		{ID: "a1", Status: ApprovalStatusPending, CreatedAt: now},
		{ID: "a2", Status: ApprovalStatusApproved, CreatedAt: now},
		{ID: "a3", Status: ApprovalStatusAwaitingDispatch, CreatedAt: now},
	}

	n := s.MigrateApprovalsBindRepo("BeFeast/maestro")
	if n != 3 {
		t.Fatalf("migrated = %d, want 3", n)
	}
	for _, a := range s.Approvals {
		if a.Repo != "BeFeast/maestro" {
			t.Fatalf("approval %s repo = %q, want stamped", a.ID, a.Repo)
		}
	}
}

func TestMigrateApprovalsBindRepo_LeavesTerminalApprovalsAlone(t *testing.T) {
	s := NewState()
	terminal := []ApprovalStatus{
		ApprovalStatusExecuted,
		ApprovalStatusExecutionFailed,
		ApprovalStatusExecutionSkipped,
		ApprovalStatusRejected,
		ApprovalStatusStale,
		ApprovalStatusSuperseded,
	}
	for i, st := range terminal {
		s.Approvals = append(s.Approvals, Approval{
			ID:     "t" + string(rune('1'+i)),
			Status: st,
		})
	}

	n := s.MigrateApprovalsBindRepo("BeFeast/maestro")
	if n != 0 {
		t.Fatalf("migrated = %d, want 0 (terminal records must not be rewritten)", n)
	}
	for _, a := range s.Approvals {
		if a.Repo != "" {
			t.Fatalf("terminal approval %s was rewritten with repo=%q", a.ID, a.Repo)
		}
	}
}

func TestMigrateApprovalsBindRepo_PreservesAlreadyStamped(t *testing.T) {
	s := NewState()
	s.Approvals = []Approval{
		{ID: "a1", Status: ApprovalStatusPending, Repo: "BeFeast/scribe-service"},
		{ID: "a2", Status: ApprovalStatusPending},
	}

	n := s.MigrateApprovalsBindRepo("BeFeast/maestro")
	if n != 1 {
		t.Fatalf("migrated = %d, want 1 (a1 already stamped)", n)
	}
	if s.Approvals[0].Repo != "BeFeast/scribe-service" {
		t.Fatalf("a1 repo = %q, must not be overwritten", s.Approvals[0].Repo)
	}
	if s.Approvals[1].Repo != "BeFeast/maestro" {
		t.Fatalf("a2 repo = %q, want stamped", s.Approvals[1].Repo)
	}
}

func TestMigrateApprovalsBindRepo_NoRepo_NoOp(t *testing.T) {
	s := NewState()
	s.Approvals = []Approval{{ID: "a1", Status: ApprovalStatusPending}}
	if n := s.MigrateApprovalsBindRepo("   "); n != 0 {
		t.Fatalf("migrated = %d, want 0 when repo is blank", n)
	}
	if s.Approvals[0].Repo != "" {
		t.Fatalf("a1 repo = %q, want empty when repo arg is blank", s.Approvals[0].Repo)
	}
}

func TestMigrateApprovalsBindRepo_Idempotent(t *testing.T) {
	s := NewState()
	s.Approvals = []Approval{{ID: "a1", Status: ApprovalStatusPending}}
	first := s.MigrateApprovalsBindRepo("BeFeast/maestro")
	second := s.MigrateApprovalsBindRepo("BeFeast/maestro")
	if first != 1 || second != 0 {
		t.Fatalf("first=%d second=%d, want 1 then 0 (idempotent)", first, second)
	}
}

func TestMigrateApprovalsBindRepo_NilState_NoOp(t *testing.T) {
	var s *State
	if n := s.MigrateApprovalsBindRepo("BeFeast/maestro"); n != 0 {
		t.Fatalf("nil receiver migrated = %d, want 0", n)
	}
}
