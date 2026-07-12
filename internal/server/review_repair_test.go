package server

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/befeast/maestro/internal/state"
)

// seedProvenReviewRepair writes a supervisor decision carrying a review-repair
// payload for (issue, pr, head) into the state at dir, so a manual enqueue can
// prove the payload (#874). Mirrors what the supervisor records once it
// observes blocking review findings on a green, mergeable, retry-exhausted PR.
func seedProvenReviewRepair(t *testing.T, dir string, issue, pr int, head string) {
	t.Helper()
	st, err := state.Load(dir)
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	st.RecordSupervisorDecision(state.SupervisorDecision{
		ID:                "dec-proof",
		CreatedAt:         time.Now().UTC(),
		RecommendedAction: "spawn_review_repair",
		Target:            &state.SupervisorTarget{Issue: issue, PR: pr, HeadSHA: head},
		ReviewRepair: &state.SupervisorReviewRepairPayload{
			HeadSHA: head,
			Backend: "claude",
			Findings: []state.SupervisorReviewFinding{
				{Path: "internal/foo.go", Line: 42, Body: "P1: fix this", Severity: "P1"},
			},
		},
	}, state.DefaultSupervisorDecisionLimit)
	if err := state.Save(dir, st); err != nil {
		t.Fatalf("save state: %v", err)
	}
}

// #565/#874: HTTP enqueue for spawn_review_repair, when the supervisor has
// already proven blocking findings on the PR head, records a pending approval
// that DURABLY carries the review-repair payload (head SHA + findings), so the
// orchestrator dispatcher can converge without a coincident latest decision.
func TestApprovalAction_SpawnReviewRepair_Enqueues202WithDurablePayload(t *testing.T) {
	cfg, dir := approvalEnqueueCfg(t)
	seedProvenReviewRepair(t, dir, 442, 564, "deadbeefcafe")

	srv := New(cfg, nil)
	srv.SetActionDeps(&fakeActionGH{}, nil)

	w := postApprovalAction(t, srv, `{"action_id":"spawn_review_repair","pr_number":564,"issue_number":442,"reason":"Greptile P1 unresolved"}`)
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%s", w.Code, w.Body.String())
	}
	var resp approvalEnqueueResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.ActionID != "spawn_review_repair" {
		t.Fatalf("action_id = %q, want spawn_review_repair", resp.ActionID)
	}

	st, err := state.Load(dir)
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	if len(st.Approvals) != 1 {
		t.Fatalf("approvals = %d, want 1", len(st.Approvals))
	}
	a := st.Approvals[0]
	if a.Action != "spawn_review_repair" {
		t.Fatalf("approval action = %q, want spawn_review_repair", a.Action)
	}
	if a.Target == nil || a.Target.PR != 564 || a.Target.Issue != 442 {
		t.Fatalf("target = %+v, want PR=564 issue=442", a.Target)
	}
	if a.Target.HeadSHA != "deadbeefcafe" {
		t.Fatalf("target head = %q, want the proven head deadbeefcafe (idempotency key)", a.Target.HeadSHA)
	}
	if a.ReviewRepair == nil {
		t.Fatal("approval.ReviewRepair is nil — the durable payload was not persisted; the dispatcher would have no path")
	}
	if a.ReviewRepair.HeadSHA != "deadbeefcafe" || len(a.ReviewRepair.Findings) != 1 {
		t.Fatalf("payload = %+v, want head deadbeefcafe with 1 finding", a.ReviewRepair)
	}
}

// #874: a manual spawn_review_repair enqueue with NO proven payload must be
// rejected before any mutation — otherwise the approval enters awaiting_dispatch
// with no dispatcher path (the live sup-307 / #866 wedge). The rejection is a
// 409 with a precise reason and leaves state untouched.
func TestApprovalAction_SpawnReviewRepair_RejectsWhenUnproven(t *testing.T) {
	cfg, dir := approvalEnqueueCfg(t)
	srv := New(cfg, nil)
	srv.SetActionDeps(&fakeActionGH{}, nil)

	w := postApprovalAction(t, srv, `{"action_id":"spawn_review_repair","pr_number":564,"issue_number":442}`)
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (no proven payload); body=%s", w.Code, w.Body.String())
	}

	st, err := state.Load(dir)
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	if len(st.Approvals) != 0 {
		t.Fatalf("approvals = %d, want 0 — a rejected enqueue must not mutate state", len(st.Approvals))
	}
}

// spawn_review_repair without pr_number must 400 — the dispatcher
// needs the PR to spawn the scoped worker.
func TestApprovalAction_SpawnReviewRepair_RequiresPRNumber(t *testing.T) {
	cfg, _ := approvalEnqueueCfg(t)
	srv := New(cfg, nil)
	srv.SetActionDeps(&fakeActionGH{}, nil)
	w := postApprovalAction(t, srv, `{"action_id":"spawn_review_repair","issue_number":442}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", w.Code, w.Body.String())
	}
}
