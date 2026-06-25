package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/befeast/maestro/internal/approvalstore"
	"github.com/befeast/maestro/internal/state"
)

// sqliteSrv returns a single-project Server wired to the SQLite approvals
// store at a fresh db path, sharing the JSON state dir for write-through.
func sqliteSrv(t *testing.T) (*Server, string) {
	t.Helper()
	srv, dir := srvWithStateDir(t)
	if err := srv.SetApprovalStore(approvalstore.ModeSQLite, filepath.Join(t.TempDir(), "maestro.db")); err != nil {
		t.Fatalf("set approval store: %v", err)
	}
	return srv, dir
}

func postApprove(t *testing.T, srv *Server, id, verb string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/approvals/"+id+"/"+verb,
		bytes.NewBufferString(`{"actor":"oleg","reason":"green"}`))
	w := httptest.NewRecorder()
	srv.handleApproval(w, req)
	return w
}

// TestHandleApproval_SQLite_HappyPath is the sqlite-store parity for
// TestHandleApproval_Approve_HappyPath: the endpoint behaves identically and
// the write-through lands the approved status in JSON state.
func TestHandleApproval_SQLite_HappyPath(t *testing.T) {
	srv, dir := sqliteSrv(t)
	a := enqueuedApproval(t, dir, "merge_pr", &state.SupervisorTarget{PR: 12})

	w := postApprove(t, srv, a.ID, "approve")
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
	// Write-through: JSON state (executor loop's source) reflects approved.
	st, _ := state.Load(dir)
	if st.Approvals[0].Status != state.ApprovalStatusApproved {
		t.Fatalf("json disk status = %q, want approved", st.Approvals[0].Status)
	}
}

func TestHandleApproval_SQLite_Reject(t *testing.T) {
	srv, dir := sqliteSrv(t)
	a := enqueuedApproval(t, dir, "close_issue", &state.SupervisorTarget{Issue: 7})

	w := postApprove(t, srv, a.ID, "reject")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	st, _ := state.Load(dir)
	if st.Approvals[0].Status != state.ApprovalStatusRejected {
		t.Fatalf("json disk status = %q, want rejected", st.Approvals[0].Status)
	}
}

// TestHandleApproval_SQLite_ByDecisionID is the sqlite parity for the
// CLI/JSON promise that `approve <approval-or-decision-id>` accepts either id.
// The SQLite claim must target the RESOLVED Approval.ID (the row key), not the
// user-supplied decision-id alias — claiming the raw alias would miss the row
// and return 404 even though FindApproval resolves it.
func TestHandleApproval_SQLite_ByDecisionID(t *testing.T) {
	srv, dir := sqliteSrv(t)
	a := enqueuedApproval(t, dir, "merge_pr", &state.SupervisorTarget{PR: 21})
	if a.DecisionID == "" || a.DecisionID == a.ID {
		t.Fatalf("test needs a distinct decision-id alias; got id=%q decision_id=%q", a.ID, a.DecisionID)
	}

	w := postApprove(t, srv, a.DecisionID, "approve")
	if w.Code != http.StatusOK {
		t.Fatalf("approve by decision-id status = %d, want 200; body=%s", w.Code, w.Body.String())
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
		t.Fatalf("json disk status = %q, want approved", st.Approvals[0].Status)
	}
}

func TestHandleApproval_SQLite_NotFound_404(t *testing.T) {
	srv, _ := sqliteSrv(t)
	w := postApprove(t, srv, "does-not-exist", "approve")
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", w.Code, w.Body.String())
	}
}

func TestHandleApproval_SQLite_SecondApprove_409(t *testing.T) {
	srv, dir := sqliteSrv(t)
	a := enqueuedApproval(t, dir, "merge_pr", &state.SupervisorTarget{PR: 1})

	if w := postApprove(t, srv, a.ID, "approve"); w.Code != http.StatusOK {
		t.Fatalf("first approve status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if w := postApprove(t, srv, a.ID, "approve"); w.Code != http.StatusConflict {
		t.Fatalf("second approve status = %d, want 409 (already processed); body=%s", w.Code, w.Body.String())
	}
}

// TestHandleApproval_SQLite_ConcurrentClaimOnce reproduces the write-path
// premortem double-execute race at the HTTP boundary: two parallel approve
// POSTs for the SAME id (each opening its own store connection, as the CLI and
// daemon do across processes) resolve to exactly ONE 200 winner; every other
// request is told the approval was already processed (409). Exactly one caller
// would proceed to fire the side effect.
func TestHandleApproval_SQLite_ConcurrentClaimOnce(t *testing.T) {
	srv, dir := sqliteSrv(t)
	a := enqueuedApproval(t, dir, "merge_pr", &state.SupervisorTarget{PR: 42})

	const racers = 6
	var (
		ok        int32
		conflict  int32
		start     = make(chan struct{})
		wg        sync.WaitGroup
		codeOther = make(chan string, racers)
	)
	wg.Add(racers)
	for i := 0; i < racers; i++ {
		go func() {
			defer wg.Done()
			<-start
			w := postApprove(t, srv, a.ID, "approve")
			switch w.Code {
			case http.StatusOK:
				atomic.AddInt32(&ok, 1)
			case http.StatusConflict:
				atomic.AddInt32(&conflict, 1)
			default:
				codeOther <- w.Body.String()
			}
		}()
	}
	close(start)
	wg.Wait()
	close(codeOther)
	for c := range codeOther {
		t.Errorf("unexpected response (want 200 or 409): %s", c)
	}

	if ok != 1 {
		t.Fatalf("200 OK winners = %d, want exactly 1 (claim-once violated)", ok)
	}
	if conflict != racers-1 {
		t.Fatalf("409 already-processed = %d, want %d", conflict, racers-1)
	}
	// Final JSON state is approved exactly once.
	st, _ := state.Load(dir)
	if st.Approvals[0].Status != state.ApprovalStatusApproved {
		t.Fatalf("final json status = %q, want approved", st.Approvals[0].Status)
	}
}
