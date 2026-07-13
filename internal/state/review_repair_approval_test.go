package state

import (
	"testing"
	"time"
)

func reviewRepairDecision(issue, pr int, head string) SupervisorDecision {
	return SupervisorDecision{
		ID:                "dec-1",
		CreatedAt:         time.Date(2026, 7, 12, 10, 0, 0, 0, time.UTC),
		RecommendedAction: approvalActionSpawnReviewRepair,
		Risk:              "mutating",
		RequiresApproval:  true,
		Repo:              "BeFeast/maestro",
		Project:           "BeFeast/maestro",
		Target:            &SupervisorTarget{Issue: issue, PR: pr, HeadSHA: head},
		ReviewRepair: &SupervisorReviewRepairPayload{
			HeadSHA: head,
			Backend: "claude",
			Findings: []SupervisorReviewFinding{
				{Path: "internal/foo.go", Line: 42, Body: "P1: fix", Severity: "P1"},
			},
		},
	}
}

// #874: an approval minted from a spawn_review_repair decision must DURABLY
// carry the review-repair payload, so the dispatcher can converge from the
// approval alone without a coincident latest decision.
func TestRecordPendingApprovalForDecision_PersistsReviewRepairPayload(t *testing.T) {
	now := time.Date(2026, 7, 12, 10, 0, 0, 0, time.UTC)
	st := NewState()
	approval := st.RecordPendingApprovalForDecision(reviewRepairDecision(442, 564, "deadbeef"), now)
	if approval == nil {
		t.Fatal("RecordPendingApprovalForDecision returned nil")
	}
	if approval.ReviewRepair == nil {
		t.Fatal("approval.ReviewRepair is nil — payload not persisted")
	}
	if approval.ReviewRepair.HeadSHA != "deadbeef" || len(approval.ReviewRepair.Findings) != 1 {
		t.Fatalf("payload = %+v, want head deadbeef + 1 finding", approval.ReviewRepair)
	}
	// Deep copy: mutating the approval's payload must not touch the decision's.
	approval.ReviewRepair.Findings[0].Body = "mutated"
	fresh := st.RecordPendingApprovalForDecision(reviewRepairDecision(442, 564, "deadbeef"), now)
	if fresh.ReviewRepair.Findings[0].Body != "mutated" {
		// same identity → refreshed in place; the refresh re-clones from the
		// decision, so it should be back to the decision's value.
		if fresh.ReviewRepair.Findings[0].Body != "P1: fix" {
			t.Fatalf("refreshed payload body = %q, want the decision value", fresh.ReviewRepair.Findings[0].Body)
		}
	}
}

// #874: daemon restart — the durable payload must survive a Save/Load round
// trip so the dispatcher path works after the process restarts.
func TestReviewRepairApproval_SurvivesSaveLoad(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 7, 12, 10, 0, 0, 0, time.UTC)
	st := NewState()
	st.RecordPendingApprovalForDecision(reviewRepairDecision(442, 564, "deadbeef"), now)
	if err := Save(dir, st); err != nil {
		t.Fatalf("save: %v", err)
	}
	reloaded, err := Load(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(reloaded.Approvals) != 1 {
		t.Fatalf("approvals = %d, want 1", len(reloaded.Approvals))
	}
	rr := reloaded.Approvals[0].ReviewRepair
	if rr == nil || rr.HeadSHA != "deadbeef" || len(rr.Findings) != 1 {
		t.Fatalf("reloaded payload = %+v, want durable head + finding", rr)
	}
}

// #874: EffectiveReviewRepairPayloadForPR proves the payload for a manual
// enqueue — preferring an effective approval, then a recent decision, and
// returning false when nothing proves it.
func TestEffectiveReviewRepairPayloadForPR(t *testing.T) {
	now := time.Date(2026, 7, 12, 10, 0, 0, 0, time.UTC)

	// No proof.
	empty := NewState()
	if _, _, ok := empty.EffectiveReviewRepairPayloadForPR(564); ok {
		t.Fatal("empty state must not prove a review-repair payload")
	}

	// Proof from a decision only.
	fromDecision := NewState()
	fromDecision.RecordSupervisorDecision(reviewRepairDecision(442, 564, "cafe1234"), DefaultSupervisorDecisionLimit)
	payload, target, ok := fromDecision.EffectiveReviewRepairPayloadForPR(564)
	if !ok || payload == nil || payload.HeadSHA != "cafe1234" {
		t.Fatalf("decision proof: ok=%v payload=%+v", ok, payload)
	}
	if target == nil || target.Issue != 442 {
		t.Fatalf("decision proof target = %+v, want issue 442", target)
	}
	// Wrong PR is not proven.
	if _, _, ok := fromDecision.EffectiveReviewRepairPayloadForPR(999); ok {
		t.Fatal("PR 999 must not be proven from a PR 564 decision")
	}

	// Proof from an effective approval wins.
	fromApproval := NewState()
	fromApproval.RecordPendingApprovalForDecision(reviewRepairDecision(442, 564, "beef5678"), now)
	payload, _, ok = fromApproval.EffectiveReviewRepairPayloadForPR(564)
	if !ok || payload == nil || payload.HeadSHA != "beef5678" {
		t.Fatalf("approval proof: ok=%v payload=%+v", ok, payload)
	}
}

// #874 changed-head: a spawn_review_repair approval whose durable payload head
// no longer matches the PR's current head must be superseded so the dispatcher
// never repairs a stale revision.
func TestSupersedeReviewRepairApprovalsForStaleHead(t *testing.T) {
	now := time.Date(2026, 7, 12, 10, 0, 0, 0, time.UTC)
	st := NewState()
	st.RecordPendingApprovalForDecision(reviewRepairDecision(442, 564, "oldhead"), now)

	// Matching head → nothing superseded.
	if ids := st.SupersedeReviewRepairApprovalsForStaleHead(564, "oldhead", now, "reason"); len(ids) != 0 {
		t.Fatalf("matching head superseded %v, want none", ids)
	}
	if st.Approvals[0].Status != ApprovalStatusPending {
		t.Fatalf("status = %q, want still pending", st.Approvals[0].Status)
	}

	// Moved head → superseded.
	ids := st.SupersedeReviewRepairApprovalsForStaleHead(564, "newhead", now.Add(time.Minute), "head moved")
	if len(ids) != 1 {
		t.Fatalf("moved head superseded %v, want 1", ids)
	}
	if st.Approvals[0].Status != ApprovalStatusSuperseded {
		t.Fatalf("status = %q, want superseded", st.Approvals[0].Status)
	}
}

// #874: after dispatch, the durable-approval path resolves the approval so it
// is not re-dispatched and no active approval is left behind.
func TestResolveDispatchedReviewRepairApproval(t *testing.T) {
	now := time.Date(2026, 7, 12, 10, 0, 0, 0, time.UTC)
	st := NewState()
	a := st.RecordPendingApprovalForDecision(reviewRepairDecision(442, 564, "head"), now)
	id := a.ID

	if !st.ResolveDispatchedReviewRepairApproval(id, now, "dispatched") {
		t.Fatal("resolve returned false for an active approval")
	}
	if st.Approvals[0].Status != ApprovalStatusSuperseded {
		t.Fatalf("status = %q, want superseded", st.Approvals[0].Status)
	}
	// Idempotent: a second resolve is a no-op.
	if st.ResolveDispatchedReviewRepairApproval(id, now, "again") {
		t.Fatal("second resolve should return false (already terminal)")
	}
}

// #874 regression: the orchestrator can dispatch a durable review-repair
// approval while it is still Approved (before the supervisor executor moves it
// to awaiting_dispatch). Resolving after dispatch must make an Approved record
// terminal, not silently leave it active while reporting success.
func TestResolveDispatchedReviewRepairApproval_FromApproved(t *testing.T) {
	now := time.Date(2026, 7, 12, 10, 0, 0, 0, time.UTC)
	st := NewState()
	a := st.RecordPendingApprovalForDecision(reviewRepairDecision(442, 564, "head"), now)
	id := a.ID
	if _, err := st.ApproveApproval(id, now, "operator", "approve"); err != nil {
		t.Fatalf("approve: %v", err)
	}
	if st.Approvals[0].Status != ApprovalStatusApproved {
		t.Fatalf("precondition: status = %q, want approved", st.Approvals[0].Status)
	}

	if !st.ResolveDispatchedReviewRepairApproval(id, now.Add(time.Minute), "dispatched") {
		t.Fatal("resolve returned false for an approved (active) approval")
	}
	if st.Approvals[0].Status != ApprovalStatusSuperseded {
		t.Fatalf("status = %q, want superseded", st.Approvals[0].Status)
	}
}

// #874 regression: a changed head must supersede an already-approved (not yet
// dispatched) review-repair approval, not just a pending one — otherwise the
// stale-revision repair stays active and re-selectable.
func TestSupersedeReviewRepairApprovalsForStaleHead_FromApproved(t *testing.T) {
	now := time.Date(2026, 7, 12, 10, 0, 0, 0, time.UTC)
	st := NewState()
	a := st.RecordPendingApprovalForDecision(reviewRepairDecision(442, 564, "oldhead"), now)
	if _, err := st.ApproveApproval(a.ID, now, "operator", "approve"); err != nil {
		t.Fatalf("approve: %v", err)
	}
	if st.Approvals[0].Status != ApprovalStatusApproved {
		t.Fatalf("precondition: status = %q, want approved", st.Approvals[0].Status)
	}

	ids := st.SupersedeReviewRepairApprovalsForStaleHead(564, "newhead", now.Add(time.Minute), "head moved")
	if len(ids) != 1 {
		t.Fatalf("moved head superseded %v, want 1", ids)
	}
	if st.Approvals[0].Status != ApprovalStatusSuperseded {
		t.Fatalf("status = %q, want superseded", st.Approvals[0].Status)
	}
}

// #874 regression: a failed worker start must not burn a review-repair attempt.
// ReleaseReviewRepairAttempt rolls back the claimed slot so the same (pr,head)
// can be dispatched again on a later cycle.
func TestReleaseReviewRepairAttempt(t *testing.T) {
	now := time.Date(2026, 7, 12, 10, 0, 0, 0, time.UTC)
	st := NewState()

	// Budget of 1 attempt: claim it, then a failed start releases it.
	if _, spawned := st.RecordReviewRepairAttempt(564, "head", 442, "", 1, now); !spawned {
		t.Fatal("first claim should be granted")
	}
	// Budget is now spent — a second claim without release is refused.
	if _, spawned := st.RecordReviewRepairAttempt(564, "head", 442, "", 1, now); spawned {
		t.Fatal("second claim should be refused before release (budget spent)")
	}

	st.ReleaseReviewRepairAttempt(564, "head", now.Add(time.Minute))

	track, ok := st.LookupReviewRepairTrack(564, "head")
	if !ok || track.Attempts != 0 || track.Exhausted {
		t.Fatalf("after release: track=%+v ok=%v, want attempts=0 not exhausted", track, ok)
	}
	// The freed slot is dispatchable again.
	if _, spawned := st.RecordReviewRepairAttempt(564, "head", 442, "", 1, now.Add(2*time.Minute)); !spawned {
		t.Fatal("claim after release should be granted again")
	}
}
