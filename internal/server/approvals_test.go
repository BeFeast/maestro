package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/state"
)

func enqueuedApproval(t *testing.T, dir, action string, target *state.SupervisorTarget) *state.Approval {
	t.Helper()
	st, err := state.Load(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	now := time.Now().UTC()
	dec := state.SupervisorDecision{
		ID:                "synthetic-" + action,
		CreatedAt:         now,
		Project:           "owner/repo",
		Mode:              "test",
		Summary:           "test enqueue",
		RecommendedAction: action,
		Target:            target,
		Risk:              "high",
		Confidence:        1.0,
		RequiresApproval:  true,
	}
	approval := st.RecordPendingApprovalForDecision(dec, now)
	if err := state.Save(dir, st); err != nil {
		t.Fatalf("save: %v", err)
	}
	return approval
}

func srvWithStateDir(t *testing.T) (*Server, string) {
	t.Helper()
	dir := t.TempDir()
	cfg := &config.Config{
		Repo:     "owner/repo",
		StateDir: dir,
		Server:   config.ServerConfig{ReadOnly: false, Port: 8788},
	}
	return New(cfg, nil), dir
}

// --- happy paths ------------------------------------------------------------

func TestHandleApproval_Approve_HappyPath(t *testing.T) {
	srv, dir := srvWithStateDir(t)
	a := enqueuedApproval(t, dir, "merge_pr", &state.SupervisorTarget{PR: 12})

	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/approvals/"+a.ID+"/approve",
		bytes.NewBufferString(`{"actor":"oleg","reason":"green"}`))
	w := httptest.NewRecorder()
	srv.handleApproval(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var resp approvalDecisionResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.OK || resp.Approval == nil || resp.Approval.Status != state.ApprovalStatusApproved {
		t.Fatalf("response = %+v", resp)
	}
	st, _ := state.Load(dir)
	if st.Approvals[0].Status != state.ApprovalStatusApproved {
		t.Fatalf("disk status = %q", st.Approvals[0].Status)
	}
}

func TestHandleApproval_Reject_HappyPath(t *testing.T) {
	srv, dir := srvWithStateDir(t)
	a := enqueuedApproval(t, dir, "close_issue", &state.SupervisorTarget{Issue: 7})

	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/approvals/"+a.ID+"/reject",
		bytes.NewBufferString(`{"actor":"oleg","reason":"do not close"}`))
	w := httptest.NewRecorder()
	srv.handleApproval(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	st, _ := state.Load(dir)
	if st.Approvals[0].Status != state.ApprovalStatusRejected {
		t.Fatalf("disk status = %q", st.Approvals[0].Status)
	}
}

// --- error paths ------------------------------------------------------------

func TestHandleApproval_NotFound_404(t *testing.T) {
	srv, _ := srvWithStateDir(t)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/approvals/nope/approve", nil)
	w := httptest.NewRecorder()
	srv.handleApproval(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
}

func TestHandleApproval_AlreadyApproved_409(t *testing.T) {
	srv, dir := srvWithStateDir(t)
	a := enqueuedApproval(t, dir, "merge_pr", &state.SupervisorTarget{PR: 1})
	// pre-approve
	st, _ := state.Load(dir)
	if _, err := st.ApproveApproval(a.ID, time.Now().UTC(), "cli", "first"); err != nil {
		t.Fatalf("pre-approve: %v", err)
	}
	if err := state.Save(dir, st); err != nil {
		t.Fatalf("save: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/approvals/"+a.ID+"/approve", nil)
	w := httptest.NewRecorder()
	srv.handleApproval(w, req)
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", w.Code)
	}
}

func TestHandleApproval_BadRoute_404(t *testing.T) {
	srv, _ := srvWithStateDir(t)
	for _, p := range []string{
		"/api/v1/approvals/",
		"/api/v1/approvals//approve",
		"/api/v1/approvals/foo",
		"/api/v1/approvals/foo/banana",
		"/api/v1/approvals/foo/approve/extra",
	} {
		req := httptest.NewRequest(http.MethodPost, p, nil)
		w := httptest.NewRecorder()
		srv.handleApproval(w, req)
		if w.Code != http.StatusNotFound {
			t.Errorf("path %q: status = %d, want 404", p, w.Code)
		}
	}
}

func TestHandleApproval_GetIs405(t *testing.T) {
	srv, dir := srvWithStateDir(t)
	a := enqueuedApproval(t, dir, "merge_pr", &state.SupervisorTarget{PR: 1})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/approvals/"+a.ID+"/approve", nil)
	w := httptest.NewRecorder()
	srv.handleApproval(w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", w.Code)
	}
}

func TestHandleApproval_ReadOnly_403(t *testing.T) {
	srv, dir := srvWithStateDir(t)
	srv.cfg.Server.ReadOnly = true
	a := enqueuedApproval(t, dir, "merge_pr", &state.SupervisorTarget{PR: 1})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/approvals/"+a.ID+"/approve", nil)
	w := httptest.NewRecorder()
	srv.handleApproval(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", w.Code)
	}
	st, _ := state.Load(dir)
	if st.Approvals[0].Status != state.ApprovalStatusPending {
		t.Fatalf("status changed under read-only: %q", st.Approvals[0].Status)
	}
}

func TestHandleApproval_NoStateDir_500(t *testing.T) {
	cfg := &config.Config{Repo: "owner/repo", Server: config.ServerConfig{ReadOnly: false}}
	srv := New(cfg, nil)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/approvals/nonexistent/approve", nil)
	w := httptest.NewRecorder()
	srv.handleApproval(w, req)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", w.Code)
	}
}

// --- fleet variant ----------------------------------------------------------

func newFleetForApprovalTest(t *testing.T) (*FleetServer, string, string) {
	t.Helper()
	dir := t.TempDir()
	projectName := "scribe-service"
	cfg := &config.Config{
		Repo:     "owner/scribe-service",
		StateDir: dir,
		Server:   config.ServerConfig{ReadOnly: false},
	}
	proj := NewFleetProject(projectName, "/tmp/scribe.yaml", "", cfg)
	srv := NewFleet([]FleetProject{proj}, "127.0.0.1", 0, false)
	return srv, projectName, dir
}

func TestHandleFleetApproval_Approve_HappyPath(t *testing.T) {
	srv, projectName, dir := newFleetForApprovalTest(t)
	a := enqueuedApproval(t, dir, "merge_pr", &state.SupervisorTarget{PR: 1})

	url := fmt.Sprintf("/api/v1/fleet/approvals/%s/approve?project=%s", a.ID, projectName)
	req := httptest.NewRequest(http.MethodPost, url, bytes.NewBufferString(`{"actor":"web"}`))
	w := httptest.NewRecorder()
	srv.handleFleetApproval(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	st, _ := state.Load(dir)
	if st.Approvals[0].Status != state.ApprovalStatusApproved {
		t.Fatalf("status = %q", st.Approvals[0].Status)
	}
}

func TestHandleFleetApproval_MissingProject_400(t *testing.T) {
	srv, _, dir := newFleetForApprovalTest(t)
	a := enqueuedApproval(t, dir, "merge_pr", &state.SupervisorTarget{PR: 1})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/fleet/approvals/"+a.ID+"/approve", nil)
	w := httptest.NewRecorder()
	srv.handleFleetApproval(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

func TestHandleFleetApproval_UnknownProject_404(t *testing.T) {
	srv, _, dir := newFleetForApprovalTest(t)
	a := enqueuedApproval(t, dir, "merge_pr", &state.SupervisorTarget{PR: 1})
	url := "/api/v1/fleet/approvals/" + a.ID + "/approve?project=ghost"
	req := httptest.NewRequest(http.MethodPost, url, nil)
	w := httptest.NewRecorder()
	srv.handleFleetApproval(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
}
