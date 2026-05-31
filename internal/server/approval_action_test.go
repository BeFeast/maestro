package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/state"
)

// approvalEnqueueCfg returns a config bound to a fresh per-test state dir,
// suitable for dispatchApprovalAction tests.
func approvalEnqueueCfg(t *testing.T) (*config.Config, string) {
	t.Helper()
	dir := t.TempDir()
	cfg := newSafeActionTestCfg()
	cfg.StateDir = dir
	return cfg, dir
}

func postApprovalAction(t *testing.T, srv *Server, payload string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/actions", bytes.NewBufferString(payload))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.handleAction(w, req)
	return w
}

func loadStateAt(t *testing.T, dir string) *state.State {
	t.Helper()
	st, err := state.Load(dir)
	if err != nil {
		t.Fatalf("load state at %q: %v", dir, err)
	}
	return st
}

// --- enqueue happy paths -----------------------------------------------------

func TestApprovalAction_MergePR_Enqueues202(t *testing.T) {
	cfg, dir := approvalEnqueueCfg(t)
	gh := &fakeActionGH{}
	auditCalls := 0
	srv := New(cfg, nil)
	srv.SetActionDeps(gh, func(actor, action, target, reason string) error {
		auditCalls++
		if action != "enqueue:merge_pr" {
			t.Errorf("audit action = %q, want enqueue:merge_pr", action)
		}
		return nil
	})

	w := postApprovalAction(t, srv, `{"action_id":"merge_pr","pr_number":42,"issue_number":99,"actor":"oleg","reason":"PR is green, gates clear"}`)
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%s", w.Code, w.Body.String())
	}

	var resp approvalEnqueueResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.OK || resp.ActionID != "merge_pr" || resp.ApprovalID == "" || resp.Status != "pending" {
		t.Fatalf("response = %+v", resp)
	}
	if auditCalls != 1 {
		t.Fatalf("audit calls = %d, want 1", auditCalls)
	}

	// gh client must NOT have been touched: enqueue must not execute.
	if len(gh.addLabelCalls)+len(gh.removeLabelCalls)+len(gh.commentCalls) != 0 {
		t.Fatalf("gh was unexpectedly called during enqueue")
	}

	// state on disk has exactly one pending approval for merge_pr.
	st := loadStateAt(t, dir)
	if len(st.Approvals) != 1 {
		t.Fatalf("approvals = %d, want 1", len(st.Approvals))
	}
	got := st.Approvals[0]
	if got.Status != state.ApprovalStatusPending {
		t.Fatalf("approval status = %q, want pending", got.Status)
	}
	if got.Action != "merge_pr" {
		t.Fatalf("approval action = %q, want merge_pr", got.Action)
	}
	if got.Target == nil || got.Target.PR != 42 {
		t.Fatalf("approval target = %+v, want PR=42", got.Target)
	}
	// state file should exist.
	if _, err := filepath.Abs(filepath.Join(dir, "state.json")); err != nil {
		t.Fatalf("state path: %v", err)
	}
}

func TestApprovalAction_CloseIssue_Enqueues202(t *testing.T) {
	cfg, dir := approvalEnqueueCfg(t)
	srv := New(cfg, nil)
	srv.SetActionDeps(&fakeActionGH{}, nil)

	w := postApprovalAction(t, srv, `{"action_id":"close_issue","issue_number":7,"reason":"obsolete"}`)
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%s", w.Code, w.Body.String())
	}
	st := loadStateAt(t, dir)
	if len(st.Approvals) != 1 || st.Approvals[0].Action != "close_issue" || st.Approvals[0].Target.Issue != 7 {
		t.Fatalf("state.Approvals = %+v", st.Approvals)
	}
}

func TestApprovalAction_DeleteWorktree_Enqueues202(t *testing.T) {
	cfg, dir := approvalEnqueueCfg(t)
	srv := New(cfg, nil)
	srv.SetActionDeps(&fakeActionGH{}, nil)

	w := postApprovalAction(t, srv, `{"action_id":"delete_worktree","slot":"sup-77","reason":"stale codex"}`)
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%s", w.Code, w.Body.String())
	}
	st := loadStateAt(t, dir)
	if len(st.Approvals) != 1 || st.Approvals[0].Action != "delete_worktree" || st.Approvals[0].Target.Session != "sup-77" {
		t.Fatalf("state.Approvals = %+v", st.Approvals)
	}
}

func TestApprovalAction_ChangeGlobalConfig_Enqueues202(t *testing.T) {
	cfg, dir := approvalEnqueueCfg(t)
	srv := New(cfg, nil)
	srv.SetActionDeps(&fakeActionGH{}, nil)

	w := postApprovalAction(t, srv, `{"action_id":"change_global_config","reason":"swap default backend"}`)
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%s", w.Code, w.Body.String())
	}
	st := loadStateAt(t, dir)
	if len(st.Approvals) != 1 || st.Approvals[0].Action != "change_global_config" {
		t.Fatalf("state.Approvals = %+v", st.Approvals)
	}
}

// --- enqueue error paths ----------------------------------------------------

func TestApprovalAction_MergePR_RequiresPRNumber(t *testing.T) {
	cfg, dir := approvalEnqueueCfg(t)
	srv := New(cfg, nil)
	srv.SetActionDeps(&fakeActionGH{}, nil)

	w := postApprovalAction(t, srv, `{"action_id":"merge_pr","issue_number":1}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", w.Code, w.Body.String())
	}
	if st := loadStateAt(t, dir); len(st.Approvals) != 0 {
		t.Fatalf("approvals leaked on bad request: %+v", st.Approvals)
	}
}

func TestApprovalAction_CloseIssue_RequiresIssueNumber(t *testing.T) {
	cfg, _ := approvalEnqueueCfg(t)
	srv := New(cfg, nil)
	srv.SetActionDeps(&fakeActionGH{}, nil)

	w := postApprovalAction(t, srv, `{"action_id":"close_issue"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

func TestApprovalAction_DeleteWorktree_RequiresSlot(t *testing.T) {
	cfg, _ := approvalEnqueueCfg(t)
	srv := New(cfg, nil)
	srv.SetActionDeps(&fakeActionGH{}, nil)

	w := postApprovalAction(t, srv, `{"action_id":"delete_worktree"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

func TestApprovalAction_ChangeGlobalConfig_RequiresReason(t *testing.T) {
	cfg, _ := approvalEnqueueCfg(t)
	srv := New(cfg, nil)
	srv.SetActionDeps(&fakeActionGH{}, nil)

	w := postApprovalAction(t, srv, `{"action_id":"change_global_config"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

func TestApprovalAction_NoStateDirReturns500(t *testing.T) {
	cfg := newSafeActionTestCfg() // no StateDir set
	srv := New(cfg, nil)
	srv.SetActionDeps(&fakeActionGH{}, nil)

	w := postApprovalAction(t, srv, `{"action_id":"merge_pr","pr_number":1}`)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", w.Code)
	}
}

func TestApprovalAction_ReadOnlyShortCircuits(t *testing.T) {
	cfg, dir := approvalEnqueueCfg(t)
	cfg.Server.ReadOnly = true
	srv := New(cfg, nil)
	srv.SetActionDeps(&fakeActionGH{}, nil)

	w := postApprovalAction(t, srv, `{"action_id":"merge_pr","pr_number":1}`)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", w.Code)
	}
	if st := loadStateAt(t, dir); len(st.Approvals) != 0 {
		t.Fatalf("approvals recorded under --read-only: %+v", st.Approvals)
	}
}

// Two enqueues for the same (action, target) coalesce into ONE pending
// approval (Phase 1.1 at-mint dedup, 2026-05-31). HTTP callers can re-POST
// safely; idempotency is enforced at the state-write boundary, not by
// callers reading the returned approval_id and stopping.
//
// This was the dogfood "approvals storm" root cause — the same
// (action=spawn_worker, target.issue=471) was minted 56 times in 12h
// because RecordPendingApprovalForDecision had no dedup at all.
func TestApprovalAction_SecondEnqueueDedupsToSameApproval(t *testing.T) {
	cfg, dir := approvalEnqueueCfg(t)
	srv := New(cfg, nil)
	srv.SetActionDeps(&fakeActionGH{}, nil)

	w1 := postApprovalAction(t, srv, `{"action_id":"merge_pr","pr_number":1}`)
	w2 := postApprovalAction(t, srv, `{"action_id":"merge_pr","pr_number":1}`)
	if w1.Code != http.StatusAccepted || w2.Code != http.StatusAccepted {
		t.Fatalf("statuses = %d, %d", w1.Code, w2.Code)
	}
	st := loadStateAt(t, dir)
	if len(st.Approvals) != 1 {
		t.Fatalf("approvals = %d, want 1 (dedup must coalesce identical re-enqueue)", len(st.Approvals))
	}
}

// Different targets (different PR numbers) for the same action MUST stay
// distinct. Dedup is keyed on (Action, Target), not Action alone.
func TestApprovalAction_DifferentTargetsCoexist(t *testing.T) {
	cfg, dir := approvalEnqueueCfg(t)
	srv := New(cfg, nil)
	srv.SetActionDeps(&fakeActionGH{}, nil)

	w1 := postApprovalAction(t, srv, `{"action_id":"merge_pr","pr_number":1}`)
	w2 := postApprovalAction(t, srv, `{"action_id":"merge_pr","pr_number":2}`)
	if w1.Code != http.StatusAccepted || w2.Code != http.StatusAccepted {
		t.Fatalf("statuses = %d, %d", w1.Code, w2.Code)
	}
	st := loadStateAt(t, dir)
	if len(st.Approvals) != 2 {
		t.Fatalf("approvals = %d, want 2 (different targets must NOT dedup)", len(st.Approvals))
	}
	if st.Approvals[0].ID == st.Approvals[1].ID {
		t.Fatalf("approvals share the same ID: %s", st.Approvals[0].ID)
	}
}
