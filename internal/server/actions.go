package server

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/state"
)

// actionGitHubClient is the narrow surface the safe-action dispatcher needs
// from a GitHub client. The real *github.Client satisfies this; tests inject
// a fake. Keeping the interface here (next to the dispatcher) avoids dragging
// internal/server into a hard dep on internal/github.
type actionGitHubClient interface {
	AddIssueLabel(issueNumber int, label string) error
	RemoveIssueLabel(issueNumber int, label string) error
	CommentIssue(issueNumber int, body string) error
}

// actionAuditRecorder appends an audit-log entry for a successfully executed
// action. The fleet server passes a closure that writes to the per-project
// audit file; the single-project server can pass the same shape (or nil if
// the audit log is not configured).
type actionAuditRecorder func(actor, action, target, reason string) error

// safeActionResult is the dispatcher's verdict.
//
//   - handled=false → the action was NOT one of the safe verbs we own here;
//     the caller must fall through to its existing 501/approval handling.
//   - handled=true with err==nil → action executed; status is the HTTP code
//     to return (typically 200) and body is the JSON payload.
//   - handled=true with err!=nil → action was claimed by us but failed;
//     status is the HTTP code to return and body carries the error message.
type safeActionResult struct {
	handled bool
	status  int
	body    any
	err     error
}

// safeActionResponse is the JSON body returned on a successful safe action.
type safeActionResponse struct {
	OK       bool   `json:"ok"`
	ActionID string `json:"action_id"`
	Target   string `json:"target,omitempty"`
	AuditID  string `json:"audit_id,omitempty"`
}

// dispatchSafeAction routes a controlActionRequest to the matching GitHub
// helper for the three safe verbs (add_ready_label, remove_ready_label,
// add_issue_comment). Approval-required action_ids return handled=false so
// the caller can keep its 501 response. Unknown action_ids return a 400.
//
// readOnly is checked by the caller before this is invoked; we assume here
// that the request has already cleared the read-only gate.
func dispatchSafeAction(req controlActionRequest, cfg *config.Config, gh actionGitHubClient, audit actionAuditRecorder) safeActionResult {
	id := strings.TrimSpace(req.ActionID)
	if id == "" {
		return safeActionResult{handled: true, status: http.StatusBadRequest, body: map[string]string{"error": "action_id is required"}, err: errors.New("missing action_id")}
	}
	if isApprovalRequiredAction(id) || isUIOnlyAffordance(id) {
		// Not our responsibility — caller dispatches to dispatchApprovalAction
		// for the cautious-gate verbs, or falls back to 501 for UI-only.
		return safeActionResult{handled: false}
	}
	if !isSafeAction(id) {
		return safeActionResult{handled: true, status: http.StatusBadRequest, body: map[string]string{"error": fmt.Sprintf("unknown action_id %q", id)}, err: fmt.Errorf("unknown action_id %q", id)}
	}
	if gh == nil {
		return safeActionResult{handled: true, status: http.StatusInternalServerError, body: map[string]string{"error": "no GitHub client configured for this server"}, err: errors.New("nil github client")}
	}
	if req.IssueNumber <= 0 {
		return safeActionResult{handled: true, status: http.StatusBadRequest, body: map[string]string{"error": "issue_number is required for safe actions"}, err: errors.New("missing issue_number")}
	}

	readyLabel := safeActionReadyLabel(cfg)
	blockedLabel := safeActionBlockedLabel(cfg)
	target := fmt.Sprintf("issue #%d", req.IssueNumber)

	switch id {
	case config.SupervisorActionAddReadyLabel:
		if readyLabel == "" {
			return safeActionResult{handled: true, status: http.StatusInternalServerError, body: map[string]string{"error": "no ready label configured (cfg.Supervisor.ReadyLabel / cfg.IssueLabels)"}, err: errors.New("no ready label")}
		}
		if err := gh.AddIssueLabel(req.IssueNumber, readyLabel); err != nil {
			return safeActionResult{handled: true, status: http.StatusBadGateway, body: map[string]string{"error": fmt.Sprintf("add ready label: %v", err)}, err: err}
		}
		return safeActionResult{handled: true, status: http.StatusOK, body: makeSafeOK(id, target, audit, req, fmt.Sprintf("+label %s", readyLabel))}
	case config.SupervisorActionRemoveReadyLabel:
		if readyLabel == "" {
			return safeActionResult{handled: true, status: http.StatusInternalServerError, body: map[string]string{"error": "no ready label configured"}, err: errors.New("no ready label")}
		}
		if err := gh.RemoveIssueLabel(req.IssueNumber, readyLabel); err != nil {
			return safeActionResult{handled: true, status: http.StatusBadGateway, body: map[string]string{"error": fmt.Sprintf("remove ready label: %v", err)}, err: err}
		}
		return safeActionResult{handled: true, status: http.StatusOK, body: makeSafeOK(id, target, audit, req, fmt.Sprintf("-label %s", readyLabel))}
	case config.SupervisorActionRemoveBlockedLabel:
		if blockedLabel == "" {
			return safeActionResult{handled: true, status: http.StatusInternalServerError, body: map[string]string{"error": "no blocked label configured (cfg.Supervisor.BlockedLabel)"}, err: errors.New("no blocked label")}
		}
		if err := gh.RemoveIssueLabel(req.IssueNumber, blockedLabel); err != nil {
			return safeActionResult{handled: true, status: http.StatusBadGateway, body: map[string]string{"error": fmt.Sprintf("remove blocked label: %v", err)}, err: err}
		}
		return safeActionResult{handled: true, status: http.StatusOK, body: makeSafeOK(id, target, audit, req, fmt.Sprintf("-label %s", blockedLabel))}
	case config.SupervisorActionAddIssueComment:
		body := strings.TrimSpace(req.Body)
		if body == "" {
			return safeActionResult{handled: true, status: http.StatusBadRequest, body: map[string]string{"error": "body is required for add_issue_comment"}, err: errors.New("missing body")}
		}
		if err := gh.CommentIssue(req.IssueNumber, body); err != nil {
			return safeActionResult{handled: true, status: http.StatusBadGateway, body: map[string]string{"error": fmt.Sprintf("comment issue: %v", err)}, err: err}
		}
		return safeActionResult{handled: true, status: http.StatusOK, body: makeSafeOK(id, target, audit, req, "+comment")}
	}

	// Defensive: should be unreachable given isSafeAction filter above.
	return safeActionResult{handled: true, status: http.StatusBadRequest, body: map[string]string{"error": fmt.Sprintf("unhandled safe action %q", id)}, err: fmt.Errorf("unhandled safe action %q", id)}
}

// isSafeAction reports whether the action_id is one this dispatcher executes
// directly (no approval). Mirrors cfg.Supervisor.SafeActions defaults.
func isSafeAction(id string) bool {
	switch id {
	case config.SupervisorActionAddReadyLabel,
		config.SupervisorActionRemoveReadyLabel,
		config.SupervisorActionRemoveBlockedLabel,
		config.SupervisorActionAddIssueComment:
		return true
	}
	return false
}

// isApprovalRequiredAction reports whether the action_id is one of the four
// cautious-gate verbs that must be enqueued as a pending Approval rather
// than executed synchronously. dispatchApprovalAction handles these.
func isApprovalRequiredAction(id string) bool {
	switch id {
	case config.SupervisorActionMergePR,
		config.SupervisorActionCloseIssue,
		config.SupervisorActionDeleteWorktree,
		config.SupervisorActionChangeGlobalConfig:
		return true
	}
	return false
}

// isUIOnlyAffordance reports whether the action_id is a legacy UI-affordance
// verb that is mutating but not yet wired into either dispatcher. These keep
// returning 501 until they are migrated to safe or approval-required
// semantics.
func isUIOnlyAffordance(id string) bool {
	switch id {
	case "approve_merge", "restart_worker", "stop_worker",
		"mark_issue_ready", "mark_issue_blocked":
		return true
	}
	return false
}

func safeActionReadyLabel(cfg *config.Config) string {
	if cfg == nil {
		return ""
	}
	// Only honour cfg.Supervisor.ReadyLabel — NEVER fall back to
	// cfg.IssueLabels, which is the FILTER set ("bug", "feature", etc.)
	// used to decide which issues maestro tracks. Falling back to the
	// first IssueLabels entry would let `remove_ready_label` strip a
	// tracking label off an issue, silently de-listing it. Greptile P1
	// on #478.
	return strings.TrimSpace(cfg.Supervisor.ReadyLabel)
}

func safeActionBlockedLabel(cfg *config.Config) string {
	if cfg == nil {
		return ""
	}
	return strings.TrimSpace(cfg.Supervisor.BlockedLabel)
}

func makeSafeOK(id, target string, audit actionAuditRecorder, req controlActionRequest, summary string) safeActionResponse {
	resp := safeActionResponse{OK: true, ActionID: id, Target: target}
	if audit != nil {
		actor := strings.TrimSpace(req.Actor)
		if actor == "" {
			actor = "dashboard"
		}
		reason := strings.TrimSpace(req.Reason)
		if reason == "" {
			reason = summary
		}
		// Best-effort: audit failures must not poison a successful action.
		_ = audit(actor, id, target, reason)
	}
	return resp
}

// approvalEnqueueResponse is the JSON body returned on a successful enqueue.
type approvalEnqueueResponse struct {
	OK         bool   `json:"ok"`
	ActionID   string `json:"action_id"`
	ApprovalID string `json:"approval_id"`
	Status     string `json:"status"`
	Target     string `json:"target,omitempty"`
}

// dispatchApprovalAction enqueues a pending Approval for one of the four
// cautious-gate verbs (merge_pr, close_issue, delete_worktree,
// change_global_config). It does NOT execute the action — that is the
// approver/executor pipeline (#475 part 2 PR B).
//
// The HTTP caller must have cleared the read-only gate before this is
// invoked.
//
// Returns 202 Accepted on success with the approval id, so callers can
// distinguish enqueued-pending (202) from executed (200, safe actions).
//
// stateDir is the per-project state directory. nil cfg or empty stateDir
// produce a 500 — the dashboard cannot enqueue without persistence.
func dispatchApprovalAction(req controlActionRequest, cfg *config.Config, stateDir string, audit actionAuditRecorder) safeActionResult {
	id := strings.TrimSpace(req.ActionID)
	if !isApprovalRequiredAction(id) {
		return safeActionResult{handled: false}
	}
	if cfg == nil {
		return safeActionResult{handled: true, status: http.StatusInternalServerError, body: map[string]string{"error": "no project config bound to this server"}, err: errors.New("nil cfg")}
	}
	if strings.TrimSpace(stateDir) == "" {
		return safeActionResult{handled: true, status: http.StatusInternalServerError, body: map[string]string{"error": "no state dir is available to record the approval"}, err: errors.New("empty state dir")}
	}

	// Per-verb argument validation — fail fast if the request is malformed.
	if err := validateApprovalRequest(id, req); err != nil {
		return safeActionResult{handled: true, status: http.StatusBadRequest, body: map[string]string{"error": err.Error()}, err: err}
	}

	st, err := state.Load(stateDir)
	if err != nil {
		return safeActionResult{handled: true, status: http.StatusInternalServerError, body: map[string]string{"error": fmt.Sprintf("load state: %v", err)}, err: err}
	}

	now := time.Now().UTC()
	decision := buildApprovalDecision(id, req, cfg, now)
	approval := st.RecordPendingApprovalForDecision(decision, now)
	if approval == nil {
		return safeActionResult{handled: true, status: http.StatusInternalServerError, body: map[string]string{"error": "failed to record approval"}, err: errors.New("RecordPendingApprovalForDecision returned nil")}
	}

	if err := state.Save(stateDir, st); err != nil {
		return safeActionResult{handled: true, status: http.StatusInternalServerError, body: map[string]string{"error": fmt.Sprintf("save state: %v", err)}, err: err}
	}

	target := approvalTargetSummary(decision.Target)
	if audit != nil {
		actor := strings.TrimSpace(req.Actor)
		if actor == "" {
			actor = "dashboard"
		}
		reason := strings.TrimSpace(req.Reason)
		if reason == "" {
			reason = "enqueued by dashboard"
		}
		// Best-effort.
		_ = audit(actor, "enqueue:"+id, target, reason)
	}

	return safeActionResult{
		handled: true,
		status:  http.StatusAccepted,
		body: approvalEnqueueResponse{
			OK:         true,
			ActionID:   id,
			ApprovalID: approval.ID,
			Status:     string(approval.Status),
			Target:     target,
		},
	}
}

// validateApprovalRequest enforces the per-verb required fields BEFORE we
// touch state, so a 400 cannot leave a half-written approval.
func validateApprovalRequest(id string, req controlActionRequest) error {
	switch id {
	case config.SupervisorActionMergePR:
		if req.PRNumber <= 0 {
			return errors.New("pr_number is required for merge_pr")
		}
	case config.SupervisorActionCloseIssue:
		if req.IssueNumber <= 0 {
			return errors.New("issue_number is required for close_issue")
		}
	case config.SupervisorActionDeleteWorktree:
		if err := state.ValidateSlotID(req.Slot); err != nil {
			return fmt.Errorf("delete_worktree: %w", err)
		}
	case config.SupervisorActionChangeGlobalConfig:
		if strings.TrimSpace(req.Reason) == "" {
			return errors.New("reason is required for change_global_config")
		}
	}
	return nil
}

// buildApprovalDecision constructs a synthetic SupervisorDecision so the
// HTTP enqueue path uses the same approval pipeline as the LLM supervisor.
// The decision is recorded in state as the approval source.
func buildApprovalDecision(id string, req controlActionRequest, cfg *config.Config, now time.Time) state.SupervisorDecision {
	actor := strings.TrimSpace(req.Actor)
	if actor == "" {
		actor = "dashboard"
	}
	summary := strings.TrimSpace(req.Reason)
	if summary == "" {
		summary = fmt.Sprintf("HTTP enqueue of %s", id)
	}

	target := &state.SupervisorTarget{
		Issue:   req.IssueNumber,
		PR:      req.PRNumber,
		Session: strings.TrimSpace(req.Slot),
	}
	if target.Issue == 0 && target.PR == 0 && target.Session == "" {
		target = nil
	}

	projectName := strings.TrimSpace(req.Project)
	if projectName == "" && cfg != nil {
		projectName = cfg.Repo
	}

	decision := state.SupervisorDecision{
		ID:                fmt.Sprintf("http-%s-%s-%s", id, now.UTC().Format("20060102T150405.000000000Z"), randomDecisionSuffix()),
		CreatedAt:         now,
		Project:           projectName,
		Repo:              repoFromConfig(cfg),
		Mode:              "http_enqueue",
		Status:            "pending_approval",
		Summary:           summary,
		RecommendedAction: id,
		Target:            target,
		Risk:              "high",
		Confidence:        1.0,
		Reasons:           []string{fmt.Sprintf("enqueued by %s via /api/v1/.../actions", actor)},
		RequiresApproval:  true,
	}
	return decision
}

func approvalTargetSummary(target *state.SupervisorTarget) string {
	if target == nil {
		return ""
	}
	parts := []string{}
	if target.Issue > 0 {
		parts = append(parts, fmt.Sprintf("issue #%d", target.Issue))
	}
	if target.PR > 0 {
		parts = append(parts, fmt.Sprintf("PR #%d", target.PR))
	}
	if target.Session != "" {
		parts = append(parts, "session "+target.Session)
	}
	return strings.Join(parts, " ")
}

// randomDecisionSuffix returns a short random hex token used to
// disambiguate decision IDs minted in the same nanosecond. Greptile P1
// on #479: two concurrent enqueues at the same OS clock tick would
// otherwise produce identical Approval IDs. Falling back to a
// timestamp-only string on read failure keeps the path deterministic
// in the (effectively impossible) case crypto/rand returns an error.
func randomDecisionSuffix() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "x"
	}
	return hex.EncodeToString(b[:])
}

// repoFromConfig returns the trimmed cfg.Repo or "" if cfg is nil.
// Used by buildApprovalDecision to stamp the project repo onto every
// HTTP-enqueued approval (#489).
func repoFromConfig(cfg *config.Config) string {
	if cfg == nil {
		return ""
	}
	return strings.TrimSpace(cfg.Repo)
}
