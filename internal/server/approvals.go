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

// --- single-project server hookup -------------------------------------------

func (s *Server) handleApproval(w http.ResponseWriter, r *http.Request) {
	route, ok := parseApprovalPath("/api/v1/approvals/", r.URL.Path)
	if !ok {
		writeError(w, http.StatusNotFound, "expected /api/v1/approvals/{id}/{approve|reject}")
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
	route, ok := parseApprovalPath("/api/v1/fleet/approvals/", r.URL.Path)
	if !ok {
		writeError(w, http.StatusNotFound, "expected /api/v1/fleet/approvals/{id}/{approve|reject}?project=...")
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
