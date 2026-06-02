package server

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/befeast/maestro/internal/state"
)

// auditAlwaysFails returns the same error for every audit append.
// Used to exercise the audit-fail-closed contract (#491).
func auditAlwaysFails(actor, action, target, reason string) error {
	return errors.New("audit fs hiccup: ENOSPC")
}

func TestSafeAction_AuditFailureAbortsBeforeGH(t *testing.T) {
	gh := &fakeActionGH{}
	srv := New(newSafeActionTestCfg(), nil)
	srv.SetActionDeps(gh, auditAlwaysFails)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/actions",
		bytes.NewBufferString(`{"action_id":"add_ready_label","issue_number":42}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.handleAction(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (audit-fail-closed); body=%s", w.Code, w.Body.String())
	}
	if len(gh.addLabelCalls) != 0 {
		t.Fatalf("gh.AddIssueLabel was called %v — must NOT fire when audit fails", gh.addLabelCalls)
	}
	if !strings.Contains(w.Body.String(), "audit write failed") {
		t.Fatalf("response body did not surface the audit-failure reason: %s", w.Body.String())
	}
}

func TestSafeAction_AuditFailureBlocksAllSafeVerbs(t *testing.T) {
	for _, tc := range []struct {
		actionID string
		body     string
	}{
		{"add_ready_label", `{"action_id":"add_ready_label","issue_number":1}`},
		{"remove_ready_label", `{"action_id":"remove_ready_label","issue_number":2}`},
		{"remove_blocked_label", `{"action_id":"remove_blocked_label","issue_number":3}`},
		{"add_issue_comment", `{"action_id":"add_issue_comment","issue_number":4,"body":"hello"}`},
	} {
		gh := &fakeActionGH{}
		srv := New(newSafeActionTestCfg(), nil)
		srv.SetActionDeps(gh, auditAlwaysFails)

		req := httptest.NewRequest(http.MethodPost, "/api/v1/actions",
			bytes.NewBufferString(tc.body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		srv.handleAction(w, req)

		if w.Code != http.StatusInternalServerError {
			t.Errorf("verb %s: status = %d, want 500", tc.actionID, w.Code)
		}
		if len(gh.addLabelCalls)+len(gh.removeLabelCalls)+len(gh.commentCalls) != 0 {
			t.Errorf("verb %s: gh was called despite audit failure", tc.actionID)
		}
	}
}

func TestApprovalEnqueue_AuditFailureLeavesStateUnchanged(t *testing.T) {
	cfg, dir := approvalEnqueueCfg(t)
	gh := &fakeActionGH{}
	srv := New(cfg, nil)
	srv.SetActionDeps(gh, auditAlwaysFails)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/actions",
		bytes.NewBufferString(`{"action_id":"merge_pr","pr_number":42}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.handleAction(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body=%s", w.Code, w.Body.String())
	}
	// Most importantly: state on DISK must have NO approval written.
	st, err := state.Load(dir)
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	if len(st.Approvals) != 0 {
		t.Fatalf("on-disk approvals = %d, want 0 (audit-fail must roll back persistence)", len(st.Approvals))
	}
}

func TestSafeAction_AuditNilStillExecutes(t *testing.T) {
	// Defensive: when the server was wired without an audit recorder
	// (audit == nil), actions still execute normally — the fail-closed
	// guard fires only when an audit recorder IS configured and FAILS.
	gh := &fakeActionGH{}
	srv := New(newSafeActionTestCfg(), nil)
	srv.SetActionDeps(gh, nil) // no audit

	req := httptest.NewRequest(http.MethodPost, "/api/v1/actions",
		bytes.NewBufferString(`{"action_id":"add_ready_label","issue_number":42}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.handleAction(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (no audit configured -> action proceeds)", w.Code)
	}
	if len(gh.addLabelCalls) != 1 {
		t.Fatalf("gh not called with audit=nil: %v", gh.addLabelCalls)
	}
}
