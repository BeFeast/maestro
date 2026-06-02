package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/state"
)

// approvalDecisionRequest is the JSON body accepted by approve/reject
// endpoints. actor/reason are recorded in the approval audit; missing
// fields default to "dashboard".
type approvalDecisionRequest struct {
	Actor  string `json:"actor,omitempty"`
	Reason string `json:"reason,omitempty"`
}

// approvalDecisionResponse mirrors what handlers return on success.
type approvalDecisionResponse struct {
	OK       bool            `json:"ok"`
	Approval *state.Approval `json:"approval"`
}

// approvalBulkRequest is the JSON body accepted by bulk reject/supersede
// endpoints (#533 spec gap 7). IDs is the list of approval identifiers
// the operator multi-selected on the dashboard; Verb is "reject" or
// "supersede" — the server applies the same audit-recorded transition
// to each id, atomically (single state.Save) so a partial failure does
// not leave the queue in a half-applied state.
type approvalBulkRequest struct {
	IDs    []string `json:"ids"`
	Verb   string   `json:"verb"`
	Actor  string   `json:"actor,omitempty"`
	Reason string   `json:"reason,omitempty"`
}

// approvalBulkItemResult describes the per-id outcome inside an
// approvalBulkResponse. Status is "ok" for an applied transition,
// "skipped" when the approval was already in a terminal state, or
// "error" with Error populated for unrecoverable per-id failures.
type approvalBulkItemResult struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
}

// approvalBulkResponse is the JSON body returned by bulk endpoints. OK
// is true when no id produced an error. Counts let the dashboard render
// a "rejected 3, skipped 1" toast without re-scanning Items.
type approvalBulkResponse struct {
	OK      bool                     `json:"ok"`
	Applied int                      `json:"applied"`
	Skipped int                      `json:"skipped"`
	Errors  int                      `json:"errors"`
	Items   []approvalBulkItemResult `json:"items"`
}

// approvalRoute is "approve" or "reject" parsed from the URL.
type approvalRoute struct {
	id   string
	verb string // "approve" | "reject"
}

// parseApprovalPath splits /<prefix>/{id}/{verb} into (id, verb). Returns
// ok=false on any shape mismatch (extra segments, empty id, unknown verb).
func parseApprovalPath(prefix, urlPath string) (approvalRoute, bool) {
	rest := strings.TrimPrefix(urlPath, prefix)
	if rest == urlPath {
		return approvalRoute{}, false
	}
	parts := strings.Split(strings.Trim(rest, "/"), "/")
	if len(parts) != 2 {
		return approvalRoute{}, false
	}
	id := strings.TrimSpace(parts[0])
	verb := strings.TrimSpace(parts[1])
	if id == "" {
		return approvalRoute{}, false
	}
	if verb != "approve" && verb != "reject" {
		return approvalRoute{}, false
	}
	return approvalRoute{id: id, verb: verb}, true
}

// applyApprovalDecision is the shared body of every approve/reject
// handler. It loads state from stateDir, calls the matching state
// transition, persists, and writes the JSON response. Returns nil on a
// path the handler must continue (none today; the body always responds).
//
// #487: auth check fires BEFORE the read-only gate so an unauthenticated
// request always sees 401 (never 403/405). The authenticated identity
// overrides any actor field in the request body — closing the bypass where
// "approve IS the human-authorization step, and approve has no auth"
// (write-path premortem failure mode #4).
func applyApprovalDecision(w http.ResponseWriter, r *http.Request, readOnly bool, scope string, stateDir string, route approvalRoute, auth authChecker) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	authenticatedActor, ok := requireAuth(w, r, auth)
	if !ok {
		return
	}
	if readOnly {
		writeError(w, http.StatusForbidden, scope+" is read-only; approval write actions require approval-backed controls to be enabled in configuration")
		return
	}
	if strings.TrimSpace(stateDir) == "" {
		writeError(w, http.StatusInternalServerError, "no state dir is available to update the approval")
		return
	}

	var req approvalDecisionRequest
	if r.Body != nil {
		defer r.Body.Close()
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil && err != io.EOF {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("decode approval request: %v", err))
			return
		}
	}
	actor := resolveActor(authenticatedActor, req.Actor, "dashboard")
	reason := strings.TrimSpace(req.Reason)

	st, err := state.Load(stateDir)
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("load state: %v", err))
		return
	}

	now := time.Now().UTC()
	var approval *state.Approval
	switch route.verb {
	case "approve":
		approval, err = st.ApproveApproval(route.id, now, actor, reason)
	case "reject":
		approval, err = st.RejectApproval(route.id, now, actor, reason)
	default:
		// parseApprovalPath already enforces this; defensive fall-back.
		writeError(w, http.StatusBadRequest, fmt.Sprintf("unknown approval verb %q", route.verb))
		return
	}
	if err != nil {
		// Persist any partial state changes (stale/superseded transitions
		// can mutate even when the verb fails) before returning.
		if persistErr := state.Save(stateDir, st); persistErr != nil {
			writeError(w, http.StatusInternalServerError, fmt.Sprintf("save state after %s error %v: %v", route.verb, err, persistErr))
			return
		}
		switch {
		case errors.Is(err, state.ErrApprovalNotFound):
			writeError(w, http.StatusNotFound, err.Error())
		case errors.Is(err, state.ErrApprovalStale),
			errors.Is(err, state.ErrApprovalSuperseded),
			errors.Is(err, state.ErrApprovalNotPending),
			errors.Is(err, state.ErrApprovalPayloadMismatch):
			// Greptile P1 on #481: PayloadMismatch is also a stale-
			// conflict condition (the approval payload changed under
			// the client). Map to 409 so dashboards can detect it
			// uniformly with the other conflict cases instead of
			// receiving a misleading 400 Bad Request.
			writeError(w, http.StatusConflict, err.Error())
		default:
			writeError(w, http.StatusBadRequest, err.Error())
		}
		return
	}
	if err := state.Save(stateDir, st); err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("save state: %v", err))
		return
	}
	writeJSON(w, http.StatusOK, approvalDecisionResponse{OK: true, Approval: approval})
}

// applyApprovalBulk runs reject-or-supersede across a set of approval ids
// in a single state Load/Save (#533 spec gap 7). The handler is strict
// about all-or-nothing semantics: the entire batch is pre-validated
// (every id must exist) BEFORE any mutation happens, so a single unknown
// id rejects the whole request with status 400 and leaves the on-disk
// queue untouched. Already-terminal approvals inside the batch are
// reported as status="skipped" (no mutation occurred) — the operator
// likely re-submitted after a stale snapshot, and treating that as a
// half-failure would surprise them.
//
// Identity rules mirror applyApprovalDecision: the authenticated actor
// (when auth is configured) overrides any body field; the read-only
// gate fires AFTER auth so an unauthenticated probe always sees 401.
func applyApprovalBulk(w http.ResponseWriter, r *http.Request, readOnly bool, scope string, stateDir string, auth authChecker) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	authenticatedActor, ok := requireAuth(w, r, auth)
	if !ok {
		return
	}
	if readOnly {
		writeError(w, http.StatusForbidden, scope+" is read-only; approval write actions require approval-backed controls to be enabled in configuration")
		return
	}
	if strings.TrimSpace(stateDir) == "" {
		writeError(w, http.StatusInternalServerError, "no state dir is available to update approvals")
		return
	}

	var req approvalBulkRequest
	if r.Body != nil {
		defer r.Body.Close()
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil && err != io.EOF {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("decode bulk approval request: %v", err))
			return
		}
	}
	verb := strings.TrimSpace(req.Verb)
	if verb != "reject" && verb != "supersede" {
		writeError(w, http.StatusBadRequest, "verb must be \"reject\" or \"supersede\"")
		return
	}
	if len(req.IDs) == 0 {
		writeError(w, http.StatusBadRequest, "ids is required and must contain at least one approval id")
		return
	}

	actor := resolveActor(authenticatedActor, req.Actor, "dashboard")
	reason := strings.TrimSpace(req.Reason)

	st, err := state.Load(stateDir)
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("load state: %v", err))
		return
	}

	// Deduplicate ids preserving operator order so a slow UI / double-click
	// that submits the same id twice does not double-audit.
	uniqIDs := make([]string, 0, len(req.IDs))
	seen := make(map[string]struct{}, len(req.IDs))
	for _, raw := range req.IDs {
		id := strings.TrimSpace(raw)
		if id == "" {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		uniqIDs = append(uniqIDs, id)
	}
	if len(uniqIDs) == 0 {
		writeError(w, http.StatusBadRequest, "ids is required and must contain at least one non-empty approval id")
		return
	}

	// Pre-validate every id before mutating any approval. The bulk
	// endpoint's atomicity contract is: a single unknown id rejects the
	// whole batch with no on-disk change. Without this pre-pass the
	// loop would mutate the valid ids in memory, the missing-id error
	// would force the save off, and an inconsistent half-batch could
	// still leak to disk if any future caller adds new persist paths.
	resp := approvalBulkResponse{Items: make([]approvalBulkItemResult, 0, len(uniqIDs))}
	missing := make([]string, 0)
	for _, id := range uniqIDs {
		if _, ok := st.FindApproval(id); !ok {
			missing = append(missing, id)
		}
	}
	if len(missing) > 0 {
		for _, id := range uniqIDs {
			item := approvalBulkItemResult{ID: id}
			if _, ok := st.FindApproval(id); !ok {
				item.Status = "error"
				item.Error = state.ErrApprovalNotFound.Error()
				resp.Errors++
			} else {
				// Valid id, but the whole batch was rejected because
				// other ids are unknown — surface as "skipped" with a
				// note so the SPA can re-submit a cleaned-up batch.
				item.Status = "skipped"
				item.Error = "batch rejected: other ids in this request are unknown"
				resp.Skipped++
			}
			resp.Items = append(resp.Items, item)
		}
		resp.OK = false
		writeJSON(w, http.StatusBadRequest, resp)
		return
	}

	now := time.Now().UTC()
	for _, id := range uniqIDs {
		var txErr error
		switch verb {
		case "reject":
			_, txErr = st.RejectApproval(id, now, actor, reason)
		case "supersede":
			_, txErr = st.SupersedeApproval(id, now, actor, reason)
		}
		item := approvalBulkItemResult{ID: id}
		switch {
		case txErr == nil:
			item.Status = "ok"
			resp.Applied++
		case errors.Is(txErr, state.ErrApprovalNotFound):
			// Should not happen — we pre-validated — but report under
			// errors for completeness if a future caller bypasses the
			// pre-validation pass.
			item.Status = "error"
			item.Error = txErr.Error()
			resp.Errors++
		case errors.Is(txErr, state.ErrApprovalStale),
			errors.Is(txErr, state.ErrApprovalSuperseded),
			errors.Is(txErr, state.ErrApprovalNotPending),
			errors.Is(txErr, state.ErrApprovalPayloadMismatch):
			// Already-resolved approvals are not failures for bulk —
			// the operator may have re-submitted after a partial apply
			// or a stale snapshot. Report them as skipped so the SPA
			// can render «3 rejected · 1 skipped» without raising an
			// error banner.
			item.Status = "skipped"
			item.Error = txErr.Error()
			resp.Skipped++
		default:
			item.Status = "error"
			item.Error = txErr.Error()
			resp.Errors++
		}
		resp.Items = append(resp.Items, item)
	}

	// All-or-nothing persist: if any id surfaced an unrecoverable error
	// (other than already-terminal "skipped"), drop the in-memory
	// mutations on the floor by NOT saving. The operator sees the
	// per-id error report and can re-submit a cleaned batch.
	if resp.Errors > 0 {
		resp.OK = false
		writeJSON(w, http.StatusBadRequest, resp)
		return
	}
	if resp.Applied > 0 {
		if err := state.Save(stateDir, st); err != nil {
			writeError(w, http.StatusInternalServerError, fmt.Sprintf("save state: %v", err))
			return
		}
	}
	resp.OK = true
	writeJSON(w, http.StatusOK, resp)
}

// --- single-project server hookup -------------------------------------------

func (s *Server) handleApproval(w http.ResponseWriter, r *http.Request) {
	// #533 spec gap 7: /api/v1/approvals/bulk handles multi-id
	// reject/supersede. Route it here so the single-project server does
	// not need a separate mux entry (the catch-all /api/v1/approvals/
	// prefix already owns this segment).
	if strings.TrimSpace(strings.TrimPrefix(r.URL.Path, "/api/v1/approvals/")) == "bulk" {
		stateDir := ""
		cfg := s.cfg
		if cfg != nil {
			stateDir = cfg.StateDir
		}
		applyApprovalBulk(w, r, cfgServerReadOnly(cfg), "server", stateDir, s.auth)
		return
	}
	route, ok := parseApprovalPath("/api/v1/approvals/", r.URL.Path)
	if !ok {
		writeError(w, http.StatusNotFound, "expected /api/v1/approvals/{id}/{approve|reject} or /api/v1/approvals/bulk")
		return
	}
	stateDir := ""
	cfg := s.cfg
	if cfg != nil {
		stateDir = cfg.StateDir
	}
	applyApprovalDecision(w, r, cfgServerReadOnly(cfg), "server", stateDir, route, s.auth)
}

func cfgServerReadOnly(cfg *config.Config) bool {
	if cfg == nil {
		return true
	}
	return cfg.Server.ReadOnly
}

// --- fleet server hookup ----------------------------------------------------

func (s *FleetServer) handleFleetApproval(w http.ResponseWriter, r *http.Request) {
	// Greptile P2 on #481: enforce 405 BEFORE the project lookup so a
	// GET against an unknown project returns 405, not 404.
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	// #487: auth fires BEFORE path / project parsing so an unauthenticated
	// probe cannot enumerate fleet topology via 404/400 differences. Spec:
	// "every mutating POST without a valid credential returns 401".
	if _, ok := requireAuth(w, r, s.auth); !ok {
		return
	}
	// #533 spec gap 7: /api/v1/fleet/approvals/bulk handles multi-id
	// reject/supersede across one project (selected via ?project=...).
	if strings.TrimSpace(strings.TrimPrefix(r.URL.Path, "/api/v1/fleet/approvals/")) == "bulk" {
		projectName := strings.TrimSpace(r.URL.Query().Get("project"))
		if projectName == "" {
			writeError(w, http.StatusBadRequest, "project query parameter is required")
			return
		}
		project, ok := s.findProject(projectName)
		if !ok {
			writeError(w, http.StatusNotFound, fmt.Sprintf("unknown project %q", projectName))
			return
		}
		readOnly := s.readOnly
		if project.cfg != nil && project.cfg.Server.ReadOnly {
			readOnly = true
		}
		stateDir := ""
		if project.cfg != nil {
			stateDir = project.cfg.StateDir
		}
		applyApprovalBulk(w, r, readOnly, fmt.Sprintf("fleet project %q", projectName), stateDir, s.auth)
		return
	}
	route, ok := parseApprovalPath("/api/v1/fleet/approvals/", r.URL.Path)
	if !ok {
		writeError(w, http.StatusNotFound, "expected /api/v1/fleet/approvals/{id}/{approve|reject}?project=... or /api/v1/fleet/approvals/bulk?project=...")
		return
	}
	projectName := strings.TrimSpace(r.URL.Query().Get("project"))
	if projectName == "" {
		writeError(w, http.StatusBadRequest, "project query parameter is required")
		return
	}
	project, ok := s.findProject(projectName)
	if !ok {
		writeError(w, http.StatusNotFound, fmt.Sprintf("unknown project %q", projectName))
		return
	}

	// Project-scoped read-only: either the global flag or the project's own.
	readOnly := s.readOnly
	if project.cfg != nil && project.cfg.Server.ReadOnly {
		readOnly = true
	}
	stateDir := ""
	if project.cfg != nil {
		stateDir = project.cfg.StateDir
	}
	applyApprovalDecision(w, r, readOnly, fmt.Sprintf("fleet project %q", projectName), stateDir, route, s.auth)
}
