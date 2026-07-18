package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/befeast/maestro/internal/approvalstore"
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
	Warning  string          `json:"warning,omitempty"`
}

// saveApprovalDecisionState is injectable for commit-point tests. SQLite is
// authoritative for deploy_project; state.json is a reconciled read mirror.
var saveApprovalDecisionState = state.Save

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
func applyApprovalDecision(w http.ResponseWriter, r *http.Request, readOnly bool, scope string, stateDir string, route approvalRoute, auth authChecker, store approvalstore.Binding, execute ApprovalExecutor) {
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

	// ApplyDecision performs the approve/reject transition against the
	// configured store. In sqlite mode the pending→approved/rejected claim is
	// atomic (claim-once across processes), so two parallel approves of the
	// same id resolve to exactly one winner; the loser gets ErrApprovalNotPending
	// (409). In json mode it is the legacy st.ApproveApproval/RejectApproval.
	store.StateDir = stateDir
	now := time.Now().UTC()
	st, approval, err := approvalstore.ApplyDecision(store, route.verb, route.id, now, actor, reason)
	if err != nil {
		if st == nil {
			// state.Load (or store Open) failed before any transition.
			writeError(w, http.StatusInternalServerError, fmt.Sprintf("load state: %v", err))
			return
		}
		var deliveryMirror *state.Approval
		if approval != nil && approval.Action == state.ApprovalActionDeployProject && approval.Delivery != nil {
			deliveryMirror = approval
		} else if current, ok := st.FindApproval(route.id); ok && current.Action == state.ApprovalActionDeployProject && current.Delivery != nil {
			deliveryMirror = current
		}
		// Persist any partial state changes (stale/superseded transitions
		// can mutate even when the verb fails) before returning.
		if persistErr := saveApprovalDecisionState(stateDir, st); persistErr != nil {
			if deliveryMirror != nil {
				// SQLite remains authoritative. Preserve the original decision
				// status below and keep the private PathError out of API/log output.
				log.Printf("[server] delivery %s: %s", deliveryMirror.ID, state.DeliveryMirrorReconciliationPending)
			} else {
				writeError(w, http.StatusInternalServerError, fmt.Sprintf("save state after %s failed", route.verb))
				return
			}
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
		case errors.Is(err, state.ErrApprovalNotApproved):
			writeError(w, http.StatusBadRequest, err.Error())
		default:
			// Infrastructure errors (sqlite open/exec, write-through) — the
			// approval state machine only emits the sentinels above.
			writeError(w, http.StatusInternalServerError, err.Error())
		}
		return
	}
	if err := saveApprovalDecisionState(stateDir, st); err != nil {
		if approval != nil && approval.Action == state.ApprovalActionDeployProject {
			// ApplyDecision already committed the authorization in the
			// authoritative SQLite ledger. Returning 500 would falsely tell the
			// operator it was rejected while the supervisor may legitimately import
			// and execute it. Acknowledge the commit and surface mirror degradation;
			// normal reconciliation repairs state.json.
			// Never return or log the raw Save error here. os.PathError embeds the
			// private absolute StateDir/temp path, while the operator only needs the
			// durable commit + reconciliation-pending outcome.
			warning := "approval committed; " + state.DeliveryMirrorReconciliationPending
			log.Printf("[server] delivery %s: %s", approval.ID, warning)
			writeJSON(w, http.StatusOK, approvalDecisionResponse{OK: true, Approval: approval, Warning: warning})
			return
		}
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("save state: %v", err))
		return
	}

	// A safety stop is different from every cadence-owned approval: once the
	// authenticated operator has durably authorized it, execute that exact
	// immutable target before returning. Leaving it in approved for the next
	// supervisor poll is the containment gap this path exists to close.
	if route.verb == "approve" && approval != nil && approval.Action == config.SupervisorActionStopWorker {
		terminal, execErr := executeApprovedStopWorker(st, approval, actor, stateDir, store, execute)
		if execErr != nil {
			writeError(w, http.StatusInternalServerError, execErr.Error())
			return
		}
		writeJSON(w, http.StatusOK, approvalDecisionResponse{OK: true, Approval: terminal})
		return
	}
	writeJSON(w, http.StatusOK, approvalDecisionResponse{OK: true, Approval: approval})
}

// executeApprovedStopWorker runs the project-scoped safety callback only after
// the approved transition has been saved, then records and persists a terminal
// result before the HTTP request returns. The callback receives a detached
// approval value so it cannot retarget the durable authorization in memory.
func executeApprovedStopWorker(st *state.State, approval *state.Approval, actor, stateDir string, store approvalstore.Binding, execute ApprovalExecutor) (*state.Approval, error) {
	result := ApprovalExecutionResult{
		Status:  state.ApprovalStatusExecutionFailed,
		Summary: "stop_worker immediate executor is unavailable for this project",
	}
	if execute != nil {
		result = execute(st, immutableApprovalSnapshot(approval))
	}

	if result.Summary == "" {
		result.Summary = fmt.Sprintf("stop_worker completed with status %s", result.Status)
	}
	now := time.Now().UTC()
	var (
		terminal *state.Approval
		err      error
	)
	switch result.Status {
	case state.ApprovalStatusExecuted:
		terminal, err = st.MarkApprovalExecuted(approval.ID, now, actor, result.Summary)
	case state.ApprovalStatusExecutionSkipped:
		terminal, err = st.MarkApprovalExecutionSkipped(approval.ID, now, actor, result.Summary)
	case state.ApprovalStatusExecutionFailed:
		terminal, err = st.MarkApprovalExecutionFailed(approval.ID, now, actor, result.Summary)
	default:
		result.Status = state.ApprovalStatusExecutionFailed
		result.Summary = fmt.Sprintf("stop_worker immediate executor returned non-terminal status %q", result.Status)
		terminal, err = st.MarkApprovalExecutionFailed(approval.ID, now, actor, result.Summary)
	}
	if err != nil {
		return nil, fmt.Errorf("record stop_worker terminal result: %w", err)
	}
	if err := saveApprovalDecisionState(stateDir, st); err != nil {
		return nil, fmt.Errorf("save stop_worker terminal result: %w", err)
	}
	store.StateDir = stateDir
	if err := approvalstore.FinalizeExecution(store, approval.ID, terminal.Status, now, actor, result.Summary); err != nil {
		return nil, fmt.Errorf("finalize stop_worker terminal result: %w", err)
	}
	return terminal, nil
}

func immutableApprovalSnapshot(approval *state.Approval) state.Approval {
	if approval == nil {
		return state.Approval{}
	}
	snapshot := *approval
	if approval.Target != nil {
		target := *approval.Target
		target.Issues = append([]state.SupervisorIssueTarget(nil), approval.Target.Issues...)
		snapshot.Target = &target
	}
	snapshot.Evidence = append([]string(nil), approval.Evidence...)
	snapshot.Audit = append([]state.ApprovalAudit(nil), approval.Audit...)
	return snapshot
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
	applyApprovalDecision(w, r, cfgServerReadOnly(cfg), "server", stateDir, route, s.auth, s.approvalBinding(cfg), nil)
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
	// "every mutating POST without a valid credential returns 401". Read the
	// checker once (#768) so both the gate and applyApprovalDecision below see
	// the same live token even if a hot-add re-derives it concurrently.
	auth := s.liveAuth()
	if _, ok := requireAuth(w, r, auth); !ok {
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
	if project.approvalMu != nil {
		project.approvalMu.Lock()
		defer project.approvalMu.Unlock()
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
	applyApprovalDecision(w, r, readOnly, fmt.Sprintf("fleet project %q", projectName), stateDir, route, auth, s.approvalBinding(project.cfg), project.approvalExecutor)
}
