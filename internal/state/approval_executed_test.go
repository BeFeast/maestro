package state

import (
	"errors"
	"testing"
	"time"
)

func mkApprovedApproval(id string) Approval {
	now := time.Now().UTC()
	return Approval{
		ID:        id,
		CreatedAt: now,
		UpdatedAt: now,
		Action:    "merge_pr",
		Status:    ApprovalStatusApproved,
		Audit: []ApprovalAudit{
			{At: now, Event: ApprovalAuditCreated},
			{At: now, Event: ApprovalAuditApproved, Actor: "cli"},
		},
	}
}

func TestListApprovedApprovals_OldestFirst(t *testing.T) {
	s := NewState()
	t0 := time.Now().UTC()
	mid := mkApprovedApproval("approval-mid")
	mid.CreatedAt = t0.Add(-30 * time.Second)
	old := mkApprovedApproval("approval-old")
	old.CreatedAt = t0.Add(-60 * time.Second)
	new_ := mkApprovedApproval("approval-new")
	new_.CreatedAt = t0
	s.Approvals = []Approval{mid, old, new_}

	got := s.ListApprovedApprovals()
	if len(got) != 3 {
		t.Fatalf("len = %d", len(got))
	}
	if got[0].ID != "approval-old" || got[1].ID != "approval-mid" || got[2].ID != "approval-new" {
		t.Fatalf("order: %s, %s, %s", got[0].ID, got[1].ID, got[2].ID)
	}
}

func TestListApprovedApprovals_FiltersOnlyApproved(t *testing.T) {
	s := NewState()
	pending := mkApprovedApproval("pending")
	pending.Status = ApprovalStatusPending
	exec := mkApprovedApproval("done")
	exec.Status = ApprovalStatusExecuted
	rejected := mkApprovedApproval("rej")
	rejected.Status = ApprovalStatusRejected
	approved := mkApprovedApproval("approved")
	s.Approvals = []Approval{pending, exec, rejected, approved}

	got := s.ListApprovedApprovals()
	if len(got) != 1 || got[0].ID != "approved" {
		t.Fatalf("got=%+v, want only 'approved'", got)
	}
}

func TestMarkApprovalExecuted_HappyPath(t *testing.T) {
	s := NewState()
	s.Approvals = []Approval{mkApprovedApproval("a1")}
	now := time.Now().UTC()
	a, err := s.MarkApprovalExecuted("a1", now, "cli", "merged PR #42")
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if a.Status != ApprovalStatusExecuted {
		t.Fatalf("status = %q", a.Status)
	}
	last := a.Audit[len(a.Audit)-1]
	if last.Event != ApprovalAuditExecuted || last.Reason != "merged PR #42" {
		t.Fatalf("last audit = %+v", last)
	}
}

func TestMarkApprovalExecuted_IsIdempotent_AgainstAlreadyExecuted(t *testing.T) {
	// concurrent CLI+supervisor scenario — second mark must NOT mutate.
	s := NewState()
	a := mkApprovedApproval("a1")
	a.Status = ApprovalStatusExecuted
	s.Approvals = []Approval{a}
	now := time.Now().UTC()

	got, err := s.MarkApprovalExecuted("a1", now, "supervisor", "rerun")
	if !errors.Is(err, ErrApprovalNotApproved) {
		t.Fatalf("err = %v, want ErrApprovalNotApproved", err)
	}
	if got.Status != ApprovalStatusExecuted {
		t.Fatalf("status drifted: %q", got.Status)
	}
}

func TestMarkApprovalExecuted_ErrApprovalNotFound(t *testing.T) {
	s := NewState()
	now := time.Now().UTC()
	_, err := s.MarkApprovalExecuted("nope", now, "cli", "")
	if !errors.Is(err, ErrApprovalNotFound) {
		t.Fatalf("err = %v, want ErrApprovalNotFound", err)
	}
}

func TestMarkApprovalExecutionFailed_HappyPath(t *testing.T) {
	s := NewState()
	s.Approvals = []Approval{mkApprovedApproval("a1")}
	now := time.Now().UTC()
	a, err := s.MarkApprovalExecutionFailed("a1", now, "cli", "boom")
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if a.Status != ApprovalStatusExecutionFailed {
		t.Fatalf("status = %q", a.Status)
	}
	if a.Audit[len(a.Audit)-1].Event != ApprovalAuditExecutionFailed {
		t.Fatalf("audit = %+v", a.Audit)
	}
}

func TestMarkApprovalExecutionSkipped_HappyPath(t *testing.T) {
	s := NewState()
	s.Approvals = []Approval{mkApprovedApproval("a1")}
	now := time.Now().UTC()
	a, err := s.MarkApprovalExecutionSkipped("a1", now, "supervisor", "manual edit required")
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if a.Status != ApprovalStatusExecutionSkipped {
		t.Fatalf("status = %q", a.Status)
	}
}

func TestMarkApprovalExecution_RefusesPendingOrExecuted(t *testing.T) {
	cases := []ApprovalStatus{
		ApprovalStatusPending,
		ApprovalStatusExecuted,
		ApprovalStatusExecutionFailed,
		ApprovalStatusExecutionSkipped,
		ApprovalStatusRejected,
		ApprovalStatusStale,
		ApprovalStatusSuperseded,
	}
	now := time.Now().UTC()
	for _, st := range cases {
		s := NewState()
		a := mkApprovedApproval("a1")
		a.Status = st
		s.Approvals = []Approval{a}
		if _, err := s.MarkApprovalExecuted("a1", now, "cli", ""); !errors.Is(err, ErrApprovalNotApproved) {
			t.Errorf("status %q: MarkApprovalExecuted err = %v, want ErrApprovalNotApproved", st, err)
		}
	}
}
