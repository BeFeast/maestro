package server

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/befeast/maestro/internal/config"
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
	if isApprovalRequiredAction(id) {
		// Not our responsibility — caller falls back to its 501 / approval path.
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

// isApprovalRequiredAction reports whether the action_id must go through the
// cautious approval gate. These return handled=false from this dispatcher so
// the existing 501 response is preserved (until the approval enqueue path
// lands as part 2 of the dashboard write-path).
func isApprovalRequiredAction(id string) bool {
	switch id {
	case "merge_pr", "close_issue", "delete_worktree", "change_global_config",
		// Existing UI affordances that are mutating but not in our safe set:
		"approve_merge", "restart_worker", "stop_worker",
		"mark_issue_ready", "mark_issue_blocked":
		return true
	}
	return false
}

func safeActionReadyLabel(cfg *config.Config) string {
	if cfg == nil {
		return ""
	}
	if l := strings.TrimSpace(cfg.Supervisor.ReadyLabel); l != "" {
		return l
	}
	for _, l := range cfg.IssueLabels {
		if t := strings.TrimSpace(l); t != "" {
			return t
		}
	}
	return ""
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
