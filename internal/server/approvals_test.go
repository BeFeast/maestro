package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/befeast/maestro/internal/approvalstore"
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

func TestHandleApproval_DeliveryMirrorSaveFailureAcknowledgesAuthoritativeCommit(t *testing.T) {
	srv, dir := srvWithStateDir(t)
	now := time.Now().UTC()
	st := state.NewState()
	approval := st.RecordDeliveryApproval(state.DeliveryPayload{
		Project: "owner/repo", Repo: "owner/repo",
		MergedSHA:    "0123456789abcdef0123456789abcdef01234567",
		ConfigDigest: "sha256:approved", ExpiresAt: now.Add(time.Hour),
	}, now)
	if err := state.Save(dir, st); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(t.TempDir(), "approvals.db")
	store, err := approvalstore.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.PutDelivery(context.Background(), approval, approvalstore.RowBinding{
		Project: "owner/repo", Repo: "owner/repo", StateDir: dir,
	}, now); err != nil {
		t.Fatal(err)
	}
	if err := srv.SetApprovalStore(approvalstore.ModeJSON, dbPath); err != nil {
		t.Fatal(err)
	}

	const privatePath = "/home/private-user/secret-vault/state.json.tmp"
	originalSave := saveApprovalDecisionState
	saveApprovalDecisionState = func(string, *state.State) error {
		return &os.PathError{Op: "rename", Path: privatePath, Err: errors.New("injected mirror failure")}
	}
	defer func() { saveApprovalDecisionState = originalSave }()
	var logs bytes.Buffer
	oldLogWriter := log.Writer()
	log.SetOutput(&logs)
	defer log.SetOutput(oldLogWriter)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/approvals/"+approval.ID+"/approve", nil)
	w := httptest.NewRecorder()
	srv.handleApproval(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want committed success; body=%s", w.Code, w.Body.String())
	}
	var resp approvalDecisionResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.OK || resp.Approval == nil || resp.Approval.Status != state.ApprovalStatusApproved || resp.Warning == "" {
		t.Fatalf("response = %+v, want approved with mirror warning", resp)
	}
	if resp.Warning != "approval committed; "+state.DeliveryMirrorReconciliationPending {
		t.Fatalf("warning = %q, want the path-free mirror warning", resp.Warning)
	}
	if strings.Contains(w.Body.String(), privatePath) || strings.Contains(logs.String(), privatePath) ||
		strings.Contains(w.Body.String(), "private-user") || strings.Contains(logs.String(), "private-user") {
		t.Fatalf("private state path leaked in API/log surface: body=%q log=%q", w.Body.String(), logs.String())
	}

	// The losing/retry decision path also attempts to persist the authoritative
	// status before returning 409. Its mirror failure must preserve that conflict
	// semantics without exposing the same PathError.
	retryReq := httptest.NewRequest(http.MethodPost, "/api/v1/approvals/"+approval.ID+"/approve", nil)
	retryW := httptest.NewRecorder()
	srv.handleApproval(retryW, retryReq)
	if retryW.Code != http.StatusConflict {
		t.Fatalf("retry status = %d, want authoritative conflict; body=%s", retryW.Code, retryW.Body.String())
	}
	if strings.Contains(retryW.Body.String(), privatePath) || strings.Contains(logs.String(), privatePath) ||
		strings.Contains(retryW.Body.String(), "private-user") || strings.Contains(logs.String(), "private-user") {
		t.Fatalf("private state path leaked on failed-decision mirror path: body=%q log=%q", retryW.Body.String(), logs.String())
	}
	authoritative, err := store.Get(context.Background(), dir, approval.ID)
	if err != nil {
		t.Fatal(err)
	}
	if authoritative.Status != state.ApprovalStatusApproved {
		t.Fatalf("authoritative status = %q, want approved", authoritative.Status)
	}
	if _, err := store.ClaimDeliveryExecuting(context.Background(), dir, approval.ID, "sha256:approved", now.Add(time.Second), "daemon-a", "claim"); err != nil {
		t.Fatalf("first execution claim: %v", err)
	}
	if _, err := store.ClaimDeliveryExecuting(context.Background(), dir, approval.ID, "sha256:approved", now.Add(2*time.Second), "daemon-b", "claim"); !errors.Is(err, state.ErrApprovalNotApproved) {
		t.Fatalf("second execution claim err = %v, want no replay", err)
	}
}

// TestHandleApproval_ApproveAfterChurningCycle covers #750 criteria 1/3: the
// SPA /approvals approve path must execute without a "stale" race when the
// pending approval was read a supervise cycle earlier and the bound session's
// runtime snapshot churned in between. The id stays the live, approvable
// record across the cycle.
func TestHandleApproval_ApproveAfterChurningCycle(t *testing.T) {
	srv, dir := srvWithStateDir(t)
	a := enqueuedApproval(t, dir, "merge_pr", &state.SupervisorTarget{PR: 748, Issue: 700})

	// Simulate one supervise cycle landing between the operator reading the id
	// and clicking approve: a bound session churns, MarkStaleApprovals runs at
	// the cycle top, and the unchanged decision is re-emitted.
	st, _ := state.Load(dir)
	now := time.Now().UTC()
	st.Sessions["slot-1"] = &state.Session{IssueNumber: 700, PRNumber: 748, Status: state.StatusRetryExhausted, RetryCount: 5}
	st.MarkStaleApprovals(now)
	reemit := state.SupervisorDecision{
		ID:                "synthetic-merge_pr-cycle2",
		CreatedAt:         now,
		Project:           "owner/repo",
		Summary:           "test enqueue",
		RecommendedAction: "merge_pr",
		Target:            &state.SupervisorTarget{PR: 748, Issue: 700},
		Risk:              "high",
		RequiresApproval:  true,
	}
	reemitted := st.RecordPendingApprovalForDecision(reemit, now)
	if reemitted.ID != a.ID {
		t.Fatalf("approval id churned across cycle: %q -> %q", a.ID, reemitted.ID)
	}
	if err := state.Save(dir, st); err != nil {
		t.Fatalf("save: %v", err)
	}

	// Approve the id read before the cycle — must succeed, not 409 stale.
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
