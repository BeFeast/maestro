package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/befeast/maestro/internal/config"
)

// fakeActionGH records calls + lets tests stub errors.
type fakeActionGH struct {
	addLabelCalls    []labelCall
	removeLabelCalls []labelCall
	commentCalls     []commentCall

	addLabelErr    error
	removeLabelErr error
	commentErr     error
}

type labelCall struct {
	issue int
	label string
}
type commentCall struct {
	issue int
	body  string
}

func (f *fakeActionGH) AddIssueLabel(issue int, label string) error {
	f.addLabelCalls = append(f.addLabelCalls, labelCall{issue: issue, label: label})
	return f.addLabelErr
}
func (f *fakeActionGH) RemoveIssueLabel(issue int, label string) error {
	f.removeLabelCalls = append(f.removeLabelCalls, labelCall{issue: issue, label: label})
	return f.removeLabelErr
}
func (f *fakeActionGH) CommentIssue(issue int, body string) error {
	f.commentCalls = append(f.commentCalls, commentCall{issue: issue, body: body})
	return f.commentErr
}

func newSafeActionTestCfg() *config.Config {
	return &config.Config{
		Repo: "owner/repo",
		Server: config.ServerConfig{
			Port:     8788,
			ReadOnly: false,
		},
		Supervisor: config.SupervisorConfig{
			ReadyLabel:   "maestro-ready",
			BlockedLabel: "blocked",
		},
	}
}

func postAction(t *testing.T, srv *Server, payload string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/actions", bytes.NewBufferString(payload))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.handleAction(w, req)
	return w
}

func TestSafeAction_AddReadyLabel_Executes(t *testing.T) {
	gh := &fakeActionGH{}
	auditCalls := 0
	srv := New(newSafeActionTestCfg(), nil)
	srv.SetActionDeps(gh, func(actor, action, target, reason string) error {
		auditCalls++
		if action != config.SupervisorActionAddReadyLabel {
			t.Errorf("audit action = %q, want %q", action, config.SupervisorActionAddReadyLabel)
		}
		if !strings.Contains(target, "#42") {
			t.Errorf("audit target = %q, want to mention #42", target)
		}
		return nil
	})

	w := postAction(t, srv, `{"action_id":"add_ready_label","issue_number":42}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if len(gh.addLabelCalls) != 1 || gh.addLabelCalls[0].issue != 42 || gh.addLabelCalls[0].label != "maestro-ready" {
		t.Fatalf("addLabelCalls = %+v", gh.addLabelCalls)
	}
	if auditCalls != 1 {
		t.Fatalf("audit recorded %d times, want 1", auditCalls)
	}
	var resp safeActionResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if !resp.OK || resp.ActionID != "add_ready_label" {
		t.Fatalf("response = %+v", resp)
	}
}

func TestSafeAction_RemoveReadyLabel_Executes(t *testing.T) {
	gh := &fakeActionGH{}
	srv := New(newSafeActionTestCfg(), nil)
	srv.SetActionDeps(gh, nil)

	w := postAction(t, srv, `{"action_id":"remove_ready_label","issue_number":99}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	if len(gh.removeLabelCalls) != 1 || gh.removeLabelCalls[0].issue != 99 || gh.removeLabelCalls[0].label != "maestro-ready" {
		t.Fatalf("removeLabelCalls = %+v", gh.removeLabelCalls)
	}
}

func TestSafeAction_RemoveBlockedLabel_Executes(t *testing.T) {
	gh := &fakeActionGH{}
	srv := New(newSafeActionTestCfg(), nil)
	srv.SetActionDeps(gh, nil)

	w := postAction(t, srv, `{"action_id":"remove_blocked_label","issue_number":7}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	if len(gh.removeLabelCalls) != 1 || gh.removeLabelCalls[0].label != "blocked" {
		t.Fatalf("removeLabelCalls = %+v", gh.removeLabelCalls)
	}
}

func TestSafeAction_AddIssueComment_Executes(t *testing.T) {
	gh := &fakeActionGH{}
	srv := New(newSafeActionTestCfg(), nil)
	srv.SetActionDeps(gh, nil)

	w := postAction(t, srv, `{"action_id":"add_issue_comment","issue_number":12,"body":"hello"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	if len(gh.commentCalls) != 1 || gh.commentCalls[0].issue != 12 || gh.commentCalls[0].body != "hello" {
		t.Fatalf("commentCalls = %+v", gh.commentCalls)
	}
}

func TestSafeAction_AddIssueComment_RequiresBody(t *testing.T) {
	gh := &fakeActionGH{}
	srv := New(newSafeActionTestCfg(), nil)
	srv.SetActionDeps(gh, nil)

	w := postAction(t, srv, `{"action_id":"add_issue_comment","issue_number":12}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", w.Code, w.Body.String())
	}
	if len(gh.commentCalls) != 0 {
		t.Fatalf("commentCalls = %+v, want none", gh.commentCalls)
	}
}

func TestSafeAction_RequiresIssueNumber(t *testing.T) {
	gh := &fakeActionGH{}
	srv := New(newSafeActionTestCfg(), nil)
	srv.SetActionDeps(gh, nil)

	w := postAction(t, srv, `{"action_id":"add_ready_label"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

func TestSafeAction_UIVerbsTranslate_MarkReadyExecutesDirectly(t *testing.T) {
	// #567: mark_issue_ready now translates to add_ready_label and
	// executes synchronously through the safe-action dispatcher — the
	// 501 stub is gone.
	gh := &fakeActionGH{}
	srv := New(newSafeActionTestCfg(), nil)
	srv.SetActionDeps(gh, nil)

	w := postAction(t, srv, `{"action_id":"mark_issue_ready","issue_number":42}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if len(gh.addLabelCalls) != 1 || gh.addLabelCalls[0].issue != 42 || gh.addLabelCalls[0].label != "maestro-ready" {
		t.Fatalf("addLabelCalls = %+v, want one add of maestro-ready to #42", gh.addLabelCalls)
	}
}

func TestSafeAction_UIVerbsTranslate_MarkBlockedExecutesDirectly(t *testing.T) {
	// #567: mark_issue_blocked translates to add_blocked_label.
	gh := &fakeActionGH{}
	srv := New(newSafeActionTestCfg(), nil)
	srv.SetActionDeps(gh, nil)

	w := postAction(t, srv, `{"action_id":"mark_issue_blocked","issue_number":7}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if len(gh.addLabelCalls) != 1 || gh.addLabelCalls[0].issue != 7 || gh.addLabelCalls[0].label != "blocked" {
		t.Fatalf("addLabelCalls = %+v, want one add of blocked to #7", gh.addLabelCalls)
	}
}

func TestSafeAction_AddBlockedLabel_Executes(t *testing.T) {
	// #567: new safe verb backing mark_issue_blocked.
	gh := &fakeActionGH{}
	srv := New(newSafeActionTestCfg(), nil)
	srv.SetActionDeps(gh, nil)

	w := postAction(t, srv, `{"action_id":"add_blocked_label","issue_number":99}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if len(gh.addLabelCalls) != 1 || gh.addLabelCalls[0].label != "blocked" {
		t.Fatalf("addLabelCalls = %+v, want one add of blocked", gh.addLabelCalls)
	}
}

func TestSafeAction_UIVerbsTranslate_ApprovalVerbsDoNotExecuteSafely(t *testing.T) {
	// #567: approve_merge / restart_worker / stop_worker are approval-
	// gated. The safe dispatcher must NOT call gh; the approval
	// dispatcher takes over. The single-project handler has no stateDir
	// in the test cfg, so the approval enqueue returns 500 (not 501
	// "not implemented"). The important contract: no gh side effect
	// fires, and the response is no longer the legacy 501 stub.
	gh := &fakeActionGH{}
	srv := New(newSafeActionTestCfg(), nil)
	srv.SetActionDeps(gh, nil)

	for _, payload := range []string{
		`{"action_id":"approve_merge","issue_number":1,"pr_number":11}`,
		`{"action_id":"restart_worker","issue_number":1,"slot":"slot-1"}`,
		`{"action_id":"stop_worker","issue_number":1,"slot":"slot-1"}`,
	} {
		w := postAction(t, srv, payload)
		if w.Code == http.StatusNotImplemented {
			t.Fatalf("payload %s: status = 501; want the legacy 'not implemented' stub to be gone (#567)", payload)
		}
		if len(gh.addLabelCalls) != 0 || len(gh.removeLabelCalls) != 0 || len(gh.commentCalls) != 0 {
			t.Fatalf("payload %s: approval-gated verb leaked a gh call: add=%v rm=%v cmt=%v", payload, gh.addLabelCalls, gh.removeLabelCalls, gh.commentCalls)
		}
	}
}

func TestSafeAction_UnknownActionID_Rejected(t *testing.T) {
	gh := &fakeActionGH{}
	srv := New(newSafeActionTestCfg(), nil)
	srv.SetActionDeps(gh, nil)

	w := postAction(t, srv, `{"action_id":"do_a_barrel_roll","issue_number":1}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", w.Code, w.Body.String())
	}
}

// TestHandleAction_Never501 pins the #475 contract: the action endpoints
// are wired through dispatchSafeAction + dispatchApprovalAction, so no
// payload (known verb, unknown verb, empty verb, malformed) may surface
// the legacy "approval-backed action endpoints are not implemented yet"
// 501 stub. Unknown verbs return 4xx, server-side bugs return 5xx, but
// 501 is gone.
func TestHandleAction_Never501(t *testing.T) {
	gh := &fakeActionGH{}
	srv := New(newSafeActionTestCfg(), nil)
	srv.SetActionDeps(gh, nil)

	for _, payload := range []string{
		``,
		`{}`,
		`{"action_id":""}`,
		`{"action_id":"do_a_barrel_roll"}`,
		`{"action_id":"do_a_barrel_roll","issue_number":1}`,
		`{"action_id":"add_ready_label","issue_number":42}`,
	} {
		w := postAction(t, srv, payload)
		if w.Code == http.StatusNotImplemented {
			t.Errorf("payload %q: status = 501; the legacy 'not implemented' stub must be gone (#475)", payload)
		}
		if strings.Contains(w.Body.String(), "not implemented yet") {
			t.Errorf("payload %q: body still surfaces legacy stub text: %s", payload, w.Body.String())
		}
	}
}

func TestSafeAction_ReadOnlyStillWins(t *testing.T) {
	// Even with safe-action wiring in place, --read-only must keep returning
	// 403 — no behavior change in the default deployment.
	cfg := newSafeActionTestCfg()
	cfg.Server.ReadOnly = true
	gh := &fakeActionGH{}
	srv := New(cfg, nil)
	srv.SetActionDeps(gh, nil)

	w := postAction(t, srv, `{"action_id":"add_ready_label","issue_number":42}`)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", w.Code)
	}
	if len(gh.addLabelCalls) != 0 {
		t.Fatalf("addLabelCalls = %+v, want none (read-only must short-circuit)", gh.addLabelCalls)
	}
}

func TestSafeAction_GHFailureSurfacesAsBadGateway(t *testing.T) {
	gh := &fakeActionGH{addLabelErr: errors.New("403 from github")}
	srv := New(newSafeActionTestCfg(), nil)
	srv.SetActionDeps(gh, nil)

	w := postAction(t, srv, `{"action_id":"add_ready_label","issue_number":42}`)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502; body=%s", w.Code, w.Body.String())
	}
}

func TestSafeAction_NoGHClientReturns500(t *testing.T) {
	srv := New(newSafeActionTestCfg(), nil) // no SetActionDeps
	w := postAction(t, srv, `{"action_id":"add_ready_label","issue_number":42}`)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body=%s", w.Code, w.Body.String())
	}
}
