package state

import (
	"errors"
	"testing"
	"time"
)

// TestSupersedeApproval_PendingToSuperseded verifies the happy path for the
// operator-driven bulk-supersede endpoint (#533 spec gap 7): a pending
// approval transitions to "superseded" with an audit entry stamped by the
// operator.
func TestSupersedeApproval_PendingToSuperseded(t *testing.T) {
	st := NewState()
	now := time.Now().UTC()
	dec := SupervisorDecision{
		ID:                "dec-1",
		CreatedAt:         now,
		Project:           "p",
		Mode:              "test",
		Summary:           "x",
		RecommendedAction: "merge_pr",
		Target:            &SupervisorTarget{PR: 7},
		Risk:              "high",
		RequiresApproval:  true,
	}
	a := st.RecordPendingApprovalForDecision(dec, now)
	if a == nil {
		t.Fatal("RecordPendingApprovalForDecision returned nil")
	}

	got, err := st.SupersedeApproval(a.ID, now.Add(time.Second), "oleg", "newer mint landed")
	if err != nil {
		t.Fatalf("SupersedeApproval err = %v", err)
	}
	if got.Status != ApprovalStatusSuperseded {
		t.Fatalf("status = %q, want superseded", got.Status)
	}

	var sawSupersede bool
	for _, ev := range got.Audit {
		if ev.Event == ApprovalAuditSuperseded && ev.Actor == "oleg" && ev.Reason == "newer mint landed" {
			sawSupersede = true
		}
	}
	if !sawSupersede {
		t.Fatalf("audit lacks operator-stamped supersede event: %+v", got.Audit)
	}
}

// TestSupersedeApproval_AwaitingDispatchAccepted verifies that approvals in
// the awaiting_dispatch state (spawn_worker + open_child_issue, post-#515)
// can also be operator-superseded — the dispatcher loop owns the actual
// side effect, but the operator may dismiss a stale dispatch.
func TestSupersedeApproval_AwaitingDispatchAccepted(t *testing.T) {
	st := NewState()
	now := time.Now().UTC()
	dec := SupervisorDecision{
		ID:                "dec-2",
		CreatedAt:         now,
		RecommendedAction: "spawn_worker",
		Target:            &SupervisorTarget{Issue: 42},
		Risk:              "medium",
		RequiresApproval:  true,
		Project:           "p",
		Mode:              "test",
	}
	a := st.RecordPendingApprovalForDecision(dec, now)
	if _, err := st.ApproveApproval(a.ID, now, "cli", ""); err != nil {
		t.Fatalf("approve: %v", err)
	}
	if _, err := st.MarkApprovalAwaitingDispatch(a.ID, now, "cli", "spawn_worker awaiting"); err != nil {
		t.Fatalf("mark awaiting: %v", err)
	}

	got, err := st.SupersedeApproval(a.ID, now.Add(time.Second), "oleg", "stale")
	if err != nil {
		t.Fatalf("SupersedeApproval err = %v", err)
	}
	if got.Status != ApprovalStatusSuperseded {
		t.Fatalf("status = %q, want superseded", got.Status)
	}
}

// TestSupersedeApproval_NotFound verifies a missing id surfaces
// ErrApprovalNotFound so the bulk handler can report it under errors,
// not skipped.
func TestSupersedeApproval_NotFound(t *testing.T) {
	st := NewState()
	if _, err := st.SupersedeApproval("nope", time.Now().UTC(), "x", ""); !errors.Is(err, ErrApprovalNotFound) {
		t.Fatalf("err = %v, want ErrApprovalNotFound", err)
	}
}

// TestSupersedeApproval_AlreadyApprovedReturnsNotPending verifies that
// committing-but-not-yet-executed approvals (status=approved) refuse
// supersede — the executor pipeline owns their terminal transition.
func TestSupersedeApproval_AlreadyApprovedReturnsNotPending(t *testing.T) {
	st := NewState()
	now := time.Now().UTC()
	dec := SupervisorDecision{
		ID:                "dec-3",
		CreatedAt:         now,
		RecommendedAction: "merge_pr",
		Target:            &SupervisorTarget{PR: 1},
		Risk:              "high",
		RequiresApproval:  true,
		Project:           "p",
		Mode:              "test",
	}
	a := st.RecordPendingApprovalForDecision(dec, now)
	if _, err := st.ApproveApproval(a.ID, now, "cli", ""); err != nil {
		t.Fatalf("approve: %v", err)
	}
	if _, err := st.SupersedeApproval(a.ID, now.Add(time.Second), "oleg", "too late"); !errors.Is(err, ErrApprovalNotPending) {
		t.Fatalf("err = %v, want ErrApprovalNotPending", err)
	}
}

// TestSupersedeApproval_AlreadySuperseded is idempotent: a second call
// returns ErrApprovalSuperseded so the bulk handler records "skipped"
// instead of double-stamping audit.
func TestSupersedeApproval_AlreadySuperseded(t *testing.T) {
	st := NewState()
	now := time.Now().UTC()
	dec := SupervisorDecision{
		ID:                "dec-4",
		CreatedAt:         now,
		RecommendedAction: "merge_pr",
		Target:            &SupervisorTarget{PR: 9},
		Risk:              "high",
		RequiresApproval:  true,
		Project:           "p",
		Mode:              "test",
	}
	a := st.RecordPendingApprovalForDecision(dec, now)
	if _, err := st.SupersedeApproval(a.ID, now, "oleg", ""); err != nil {
		t.Fatalf("first supersede: %v", err)
	}
	if _, err := st.SupersedeApproval(a.ID, now.Add(time.Second), "oleg", ""); !errors.Is(err, ErrApprovalSuperseded) {
		t.Fatalf("err = %v, want ErrApprovalSuperseded", err)
	}
}
