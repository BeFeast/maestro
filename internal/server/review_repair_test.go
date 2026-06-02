package server

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/befeast/maestro/internal/state"
)

// #565: HTTP enqueue for spawn_review_repair must (a) accept the verb
// (no 400 "unknown action"), (b) require pr_number, (c) record a
// pending approval bound to the supervisor target so the cautious gate
// path is reusable from the dashboard.
func TestApprovalAction_SpawnReviewRepair_Enqueues202(t *testing.T) {
	cfg, dir := approvalEnqueueCfg(t)
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
	if got := st.Approvals[0].Action; got != "spawn_review_repair" {
		t.Fatalf("approval action = %q, want spawn_review_repair", got)
	}
	if got := st.Approvals[0].Target; got == nil || got.PR != 564 || got.Issue != 442 {
		t.Fatalf("target = %+v, want PR=564 issue=442", got)
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
