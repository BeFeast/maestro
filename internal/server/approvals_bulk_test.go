package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/befeast/maestro/internal/state"
)

// --- bulk reject ------------------------------------------------------------

// TestBulkApproval_Reject_AppliesToMultiple verifies #533 spec gap 7: the
// multi-id bulk endpoint rejects all named pending approvals in one
// state.Save and reports each id under Items, with counts matching.
func TestBulkApproval_Reject_AppliesToMultiple(t *testing.T) {
	srv, dir := srvWithStateDir(t)
	a := enqueuedApproval(t, dir, "merge_pr", &state.SupervisorTarget{PR: 1})
	b := enqueuedApproval(t, dir, "close_issue", &state.SupervisorTarget{Issue: 2})
	c := enqueuedApproval(t, dir, "spawn_worker", &state.SupervisorTarget{Issue: 3})

	body := fmt.Sprintf(`{"ids":[%q,%q,%q],"verb":"reject","actor":"oleg","reason":"all junk"}`, a.ID, b.ID, c.ID)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/approvals/bulk", bytes.NewBufferString(body))
	w := httptest.NewRecorder()
	srv.handleApproval(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var resp approvalBulkResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if !resp.OK || resp.Applied != 3 || resp.Errors != 0 || resp.Skipped != 0 {
		t.Fatalf("response counts = applied=%d skipped=%d errors=%d ok=%v",
			resp.Applied, resp.Skipped, resp.Errors, resp.OK)
	}
	if len(resp.Items) != 3 {
		t.Fatalf("items = %d, want 3", len(resp.Items))
	}
	for _, item := range resp.Items {
		if item.Status != "ok" {
			t.Fatalf("item %s status = %q, want ok", item.ID, item.Status)
		}
	}

	st, _ := state.Load(dir)
	for _, ap := range st.Approvals {
		if ap.Status != state.ApprovalStatusRejected {
			t.Fatalf("approval %s status = %q on disk, want rejected", ap.ID, ap.Status)
		}
		// Audit must record the operator action so the audit log can
		// reconstruct who bulk-rejected.
		var sawReject bool
		for _, ev := range ap.Audit {
			if ev.Event == state.ApprovalAuditRejected && ev.Actor == "oleg" && ev.Reason == "all junk" {
				sawReject = true
			}
		}
		if !sawReject {
			t.Fatalf("approval %s audit lacks reject event with operator identity/reason: %+v",
				ap.ID, ap.Audit)
		}
	}
}

// TestBulkApproval_Supersede_AppliesToMultiple is the parallel test for the
// supersede verb: bulk-supersede dismisses N pending cards onto status
// "superseded" without rejecting them.
func TestBulkApproval_Supersede_AppliesToMultiple(t *testing.T) {
	srv, dir := srvWithStateDir(t)
	a := enqueuedApproval(t, dir, "merge_pr", &state.SupervisorTarget{PR: 1})
	b := enqueuedApproval(t, dir, "close_issue", &state.SupervisorTarget{Issue: 2})

	body := fmt.Sprintf(`{"ids":[%q,%q],"verb":"supersede","reason":"newer mint"}`, a.ID, b.ID)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/approvals/bulk", bytes.NewBufferString(body))
	w := httptest.NewRecorder()
	srv.handleApproval(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	var resp approvalBulkResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if resp.Applied != 2 || resp.Errors != 0 {
		t.Fatalf("counts = %+v", resp)
	}
	st, _ := state.Load(dir)
	for _, ap := range st.Approvals {
		if ap.Status != state.ApprovalStatusSuperseded {
			t.Fatalf("approval %s status = %q, want superseded", ap.ID, ap.Status)
		}
	}
}

// TestBulkApproval_MixedTerminalAndPending_SkipsTerminalNotErrors verifies
// the dashboard's "operator re-submits after partial apply" scenario:
// already-rejected approvals come back as status="skipped", not as errors,
// and pending ones still flip. The whole batch returns 200.
func TestBulkApproval_MixedTerminalAndPending_SkipsTerminalNotErrors(t *testing.T) {
	srv, dir := srvWithStateDir(t)
	stale := enqueuedApproval(t, dir, "merge_pr", &state.SupervisorTarget{PR: 1})
	fresh := enqueuedApproval(t, dir, "close_issue", &state.SupervisorTarget{Issue: 2})

	// Pre-reject the first one — the bulk call must skip it gracefully.
	st, _ := state.Load(dir)
	if _, err := st.RejectApproval(stale.ID, time.Now().UTC(), "cli", "earlier"); err != nil {
		t.Fatalf("pre-reject: %v", err)
	}
	if err := state.Save(dir, st); err != nil {
		t.Fatalf("save: %v", err)
	}

	body := fmt.Sprintf(`{"ids":[%q,%q],"verb":"reject"}`, stale.ID, fresh.ID)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/approvals/bulk", bytes.NewBufferString(body))
	w := httptest.NewRecorder()
	srv.handleApproval(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	var resp approvalBulkResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Applied != 1 || resp.Skipped != 1 || resp.Errors != 0 {
		t.Fatalf("counts wrong: %+v", resp)
	}
	if !resp.OK {
		t.Fatalf("ok = false despite a successful apply; resp = %+v", resp)
	}
}

// TestBulkApproval_AllErrors_ReturnsBadRequest verifies an all-bogus-id
// batch surfaces as 4xx so the dashboard renders an error banner instead
// of a green toast.
func TestBulkApproval_AllErrors_ReturnsBadRequest(t *testing.T) {
	srv, _ := srvWithStateDir(t)
	body := `{"ids":["nope-1","nope-2"],"verb":"reject"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/approvals/bulk", bytes.NewBufferString(body))
	w := httptest.NewRecorder()
	srv.handleApproval(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (all-errors batch)", w.Code)
	}
}

// TestBulkApproval_EmptyIDs_400 protects against a UI bug that submits an
// empty list — easier to surface 400 than to silently no-op.
func TestBulkApproval_EmptyIDs_400(t *testing.T) {
	srv, _ := srvWithStateDir(t)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/approvals/bulk", bytes.NewBufferString(`{"ids":[],"verb":"reject"}`))
	w := httptest.NewRecorder()
	srv.handleApproval(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

// TestBulkApproval_UnknownVerb_400 protects against typos and gives the
// caller a clear error so a fat-fingered curl does not silently succeed.
func TestBulkApproval_UnknownVerb_400(t *testing.T) {
	srv, _ := srvWithStateDir(t)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/approvals/bulk",
		bytes.NewBufferString(`{"ids":["x"],"verb":"approve"}`))
	w := httptest.NewRecorder()
	srv.handleApproval(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (approve is not a bulk verb)", w.Code)
	}
}

// TestBulkApproval_ReadOnly_403 verifies the read-only gate still wins:
// even with valid ids and verb, a read-only deployment refuses the bulk
// mutation.
func TestBulkApproval_ReadOnly_403(t *testing.T) {
	srv, dir := srvWithStateDir(t)
	a := enqueuedApproval(t, dir, "merge_pr", &state.SupervisorTarget{PR: 1})
	srv.cfg.Server.ReadOnly = true

	body := fmt.Sprintf(`{"ids":[%q],"verb":"reject"}`, a.ID)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/approvals/bulk", bytes.NewBufferString(body))
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

// TestBulkApproval_GetIs405 verifies non-POST methods are rejected
// upfront (the bulk endpoint is mutating).
func TestBulkApproval_GetIs405(t *testing.T) {
	srv, _ := srvWithStateDir(t)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/approvals/bulk", nil)
	w := httptest.NewRecorder()
	srv.handleApproval(w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", w.Code)
	}
}

// TestBulkApproval_DuplicateIDs_AppliedOnce checks that an operator
// submitting the same id twice (slow UI, double-click) does not double-
// audit or double-count.
func TestBulkApproval_DuplicateIDs_AppliedOnce(t *testing.T) {
	srv, dir := srvWithStateDir(t)
	a := enqueuedApproval(t, dir, "merge_pr", &state.SupervisorTarget{PR: 1})

	body := fmt.Sprintf(`{"ids":[%q,%q],"verb":"reject"}`, a.ID, a.ID)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/approvals/bulk", bytes.NewBufferString(body))
	w := httptest.NewRecorder()
	srv.handleApproval(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	var resp approvalBulkResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Applied != 1 || len(resp.Items) != 1 {
		t.Fatalf("expected exactly one apply on duplicate id; got resp = %+v", resp)
	}
}

// --- fleet variant ----------------------------------------------------------

// TestFleetBulkApproval_RoutesByProject verifies the fleet bulk endpoint
// scopes the transition to the named project's state dir.
func TestFleetBulkApproval_RoutesByProject(t *testing.T) {
	srv, projectName, dir := newFleetForApprovalTest(t)
	a := enqueuedApproval(t, dir, "merge_pr", &state.SupervisorTarget{PR: 1})
	b := enqueuedApproval(t, dir, "close_issue", &state.SupervisorTarget{Issue: 2})

	url := fmt.Sprintf("/api/v1/fleet/approvals/bulk?project=%s", projectName)
	body := fmt.Sprintf(`{"ids":[%q,%q],"verb":"reject"}`, a.ID, b.ID)
	req := httptest.NewRequest(http.MethodPost, url, bytes.NewBufferString(body))
	w := httptest.NewRecorder()
	srv.handleFleetApproval(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	st, _ := state.Load(dir)
	for _, ap := range st.Approvals {
		if ap.Status != state.ApprovalStatusRejected {
			t.Fatalf("approval %s status = %q", ap.ID, ap.Status)
		}
	}
}

func TestFleetBulkApproval_MissingProject_400(t *testing.T) {
	srv, _, _ := newFleetForApprovalTest(t)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/fleet/approvals/bulk",
		bytes.NewBufferString(`{"ids":["x"],"verb":"reject"}`))
	w := httptest.NewRecorder()
	srv.handleFleetApproval(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

func TestFleetBulkApproval_UnknownProject_404(t *testing.T) {
	srv, _, _ := newFleetForApprovalTest(t)
	url := "/api/v1/fleet/approvals/bulk?project=ghost"
	req := httptest.NewRequest(http.MethodPost, url,
		bytes.NewBufferString(`{"ids":["x"],"verb":"reject"}`))
	w := httptest.NewRecorder()
	srv.handleFleetApproval(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
}
