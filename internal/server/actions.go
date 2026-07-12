package server

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"sort"
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
//
// authenticatedActor, when non-empty, is the identity resolved from the
// Authorization header (#487). It overrides the request body's actor field
// when the audit entry is written — closing the forensic hole where a body
// could claim actor="oleg" without any credential.
func dispatchSafeAction(req controlActionRequest, cfg *config.Config, gh actionGitHubClient, audit actionAuditRecorder, authenticatedActor string) safeActionResult {
	id := strings.TrimSpace(req.ActionID)
	if id == "" {
		return safeActionResult{handled: true, status: http.StatusBadRequest, body: map[string]string{"error": "action_id is required"}, err: errors.New("missing action_id")}
	}
	// #567: the fleet snapshot surfaces operator-friendly verbs
	// (mark_issue_ready / mark_issue_blocked / approve_merge); translate
	// them to the underlying safe/approval verb BEFORE the dispatcher
	// switches so the wiring stays in one place.
	id = translateUIActionID(id)
	req.ActionID = id
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
		summary := fmt.Sprintf("+label %s", readyLabel)
		if err := auditOrAbort(audit, req, authenticatedActor, id, target, summary); err != nil {
			return auditFailureResult(id, target, err)
		}
		if err := gh.AddIssueLabel(req.IssueNumber, readyLabel); err != nil {
			return safeActionResult{handled: true, status: http.StatusBadGateway, body: map[string]string{"error": fmt.Sprintf("add ready label: %v", err)}, err: err}
		}
		return safeActionResult{handled: true, status: http.StatusOK, body: makeSafeOK(id, target)}
	case config.SupervisorActionRemoveReadyLabel:
		if readyLabel == "" {
			return safeActionResult{handled: true, status: http.StatusInternalServerError, body: map[string]string{"error": "no ready label configured"}, err: errors.New("no ready label")}
		}
		summary := fmt.Sprintf("-label %s", readyLabel)
		if err := auditOrAbort(audit, req, authenticatedActor, id, target, summary); err != nil {
			return auditFailureResult(id, target, err)
		}
		if err := gh.RemoveIssueLabel(req.IssueNumber, readyLabel); err != nil {
			return safeActionResult{handled: true, status: http.StatusBadGateway, body: map[string]string{"error": fmt.Sprintf("remove ready label: %v", err)}, err: err}
		}
		return safeActionResult{handled: true, status: http.StatusOK, body: makeSafeOK(id, target)}
	case config.SupervisorActionAddBlockedLabel:
		if blockedLabel == "" {
			return safeActionResult{handled: true, status: http.StatusInternalServerError, body: map[string]string{"error": "no blocked label configured (cfg.Supervisor.BlockedLabel)"}, err: errors.New("no blocked label")}
		}
		summary := fmt.Sprintf("+label %s", blockedLabel)
		if err := auditOrAbort(audit, req, authenticatedActor, id, target, summary); err != nil {
			return auditFailureResult(id, target, err)
		}
		if err := gh.AddIssueLabel(req.IssueNumber, blockedLabel); err != nil {
			return safeActionResult{handled: true, status: http.StatusBadGateway, body: map[string]string{"error": fmt.Sprintf("add blocked label: %v", err)}, err: err}
		}
		return safeActionResult{handled: true, status: http.StatusOK, body: makeSafeOK(id, target)}
	case config.SupervisorActionRemoveBlockedLabel:
		if blockedLabel == "" {
			return safeActionResult{handled: true, status: http.StatusInternalServerError, body: map[string]string{"error": "no blocked label configured (cfg.Supervisor.BlockedLabel)"}, err: errors.New("no blocked label")}
		}
		summary := fmt.Sprintf("-label %s", blockedLabel)
		if err := auditOrAbort(audit, req, authenticatedActor, id, target, summary); err != nil {
			return auditFailureResult(id, target, err)
		}
		if err := gh.RemoveIssueLabel(req.IssueNumber, blockedLabel); err != nil {
			return safeActionResult{handled: true, status: http.StatusBadGateway, body: map[string]string{"error": fmt.Sprintf("remove blocked label: %v", err)}, err: err}
		}
		return safeActionResult{handled: true, status: http.StatusOK, body: makeSafeOK(id, target)}
	case config.SupervisorActionAddIssueComment:
		body := strings.TrimSpace(req.Body)
		if body == "" {
			return safeActionResult{handled: true, status: http.StatusBadRequest, body: map[string]string{"error": "body is required for add_issue_comment"}, err: errors.New("missing body")}
		}
		if err := auditOrAbort(audit, req, authenticatedActor, id, target, "+comment"); err != nil {
			return auditFailureResult(id, target, err)
		}
		if err := gh.CommentIssue(req.IssueNumber, body); err != nil {
			return safeActionResult{handled: true, status: http.StatusBadGateway, body: map[string]string{"error": fmt.Sprintf("comment issue: %v", err)}, err: err}
		}
		return safeActionResult{handled: true, status: http.StatusOK, body: makeSafeOK(id, target)}
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
		config.SupervisorActionAddBlockedLabel,
		config.SupervisorActionRemoveBlockedLabel,
		config.SupervisorActionAddIssueComment:
		return true
	}
	return false
}

// isApprovalRequiredAction reports whether the action_id is one of the
// cautious-gate verbs that must be enqueued as a pending Approval rather
// than executed synchronously. dispatchApprovalAction handles these.
func isApprovalRequiredAction(id string) bool {
	switch id {
	case config.SupervisorActionMergePR,
		config.SupervisorActionCloseIssue,
		config.SupervisorActionCloseIssueBatch,
		config.SupervisorActionDeleteWorktree,
		config.SupervisorActionChangeGlobalConfig,
		// #565: review-repair respawn is approval-gated (RiskMutating);
		// the orchestrator dispatcher loop spawns the actual worker once
		// the cautious gate approves.
		config.SupervisorActionSpawnReviewRepair,
		// #567: per-session worker-control verbs surfaced by the fleet
		// snapshot — both go through the cautious gate before the
		// executor calls into the WorkerController.
		config.SupervisorActionRestartWorker,
		config.SupervisorActionStopWorker:
		return true
	}
	return false
}

// isUIOnlyAffordance reports whether the action_id is a legacy UI-affordance
// verb that is mutating but not yet wired into either dispatcher. These keep
// returning 501 until they are migrated to safe or approval-required
// semantics. #567 moved the original five fleet-snapshot verbs out of this
// set — mark_issue_ready / mark_issue_blocked / approve_merge / restart_worker
// / stop_worker — so the set is now empty in the default build. Kept for
// future verbs that surface in the UI before the cautious gate catches up.
func isUIOnlyAffordance(id string) bool {
	_ = id
	return false
}

// translateUIActionID maps the operator-friendly verbs the fleet snapshot
// surfaces on per-worker / per-project rows to the underlying safe or
// approval verb the dispatcher knows about (#567). Returns id unchanged
// when no translation is needed.
//
//   - mark_issue_ready   → add_ready_label   (safe)
//   - mark_issue_blocked → add_blocked_label (safe)
//   - approve_merge      → merge_pr          (cautious-gate enqueue)
//
// restart_worker / stop_worker keep their UI names because the executor
// dispatches on those exact verbs.
func translateUIActionID(id string) string {
	switch id {
	case "mark_issue_ready":
		return config.SupervisorActionAddReadyLabel
	case "mark_issue_blocked":
		return config.SupervisorActionAddBlockedLabel
	case "approve_merge":
		return config.SupervisorActionMergePR
	}
	return id
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

// auditOrAbort writes the "attempted" audit entry for a write-path
// action. Audit BEFORE the side effect: if audit fails, the caller
// MUST NOT proceed with the gh / state mutation. Returns nil on
// success or when the recorder is nil (no auditing configured).
//
// #491: closes premortem failure mode #9. Previously the audit call was
// fire-and-forget after the side effect; a filesystem hiccup or wrong
// permissions silently dropped the audit record while the gh mutation
// went through, leaving forensics with no way to distinguish a
// filesystem error from an attacker covering tracks.
//
// #487: authenticatedActor (when non-empty) overrides any actor in the
// request body — the body's actor field is attacker-controlled and MUST
// NOT be trusted to identify who did what (write-path premortem #4 +
// failure mode #9 audit drift).
func auditOrAbort(audit actionAuditRecorder, req controlActionRequest, authenticatedActor, action, target, summary string) error {
	if audit == nil {
		return nil
	}
	actor := resolveActor(authenticatedActor, req.Actor, "dashboard")
	reason := strings.TrimSpace(req.Reason)
	if reason == "" {
		reason = summary
	}
	return audit(actor, action, target, reason)
}

// auditFailureResult turns an audit-write error into a 500 result the
// HTTP handler can return as-is. Used by both dispatchSafeAction and
// dispatchApprovalAction so the audit-fail-closed contract is uniform.
func auditFailureResult(action, target string, err error) safeActionResult {
	return safeActionResult{
		handled: true,
		status:  http.StatusInternalServerError,
		body: map[string]string{
			"error": fmt.Sprintf("audit write failed for %s on %s: %v — refusing to proceed without an audit record", action, target, err),
		},
		err: fmt.Errorf("audit write failed for %s: %w", action, err),
	}
}

func makeSafeOK(id, target string) safeActionResponse {
	return safeActionResponse{OK: true, ActionID: id, Target: target}
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
//
// authenticatedActor (#487), when non-empty, overrides any actor in the
// request body — this is the cautious-gate defense-in-depth contract: even
// for an authenticated caller the verb is enqueued (not executed) AND the
// audit + decision identity comes from the credential, not the body.
func dispatchApprovalAction(req controlActionRequest, cfg *config.Config, stateDir string, audit actionAuditRecorder, authenticatedActor string) safeActionResult {
	// #567: translate UI verbs to underlying verbs first so the per-verb
	// switches below operate on canonical action_ids.
	id := translateUIActionID(strings.TrimSpace(req.ActionID))
	req.ActionID = id
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
	decision := buildApprovalDecision(id, req, cfg, authenticatedActor, now)

	// #874: a manual spawn_review_repair enqueue must persist a durable
	// payload (repo/issue/PR/head SHA/blocking findings/backend) or be
	// rejected before any mutation. The dispatcher consumes that payload
	// directly; without it the approval would enter awaiting_dispatch with
	// no dispatcher path (the exact live wedge on sup-307 / #866). The server
	// cannot compute review findings itself, so it reuses a payload the
	// supervisor already proved on this PR's current head.
	if id == config.SupervisorActionSpawnReviewRepair {
		res, ok := enrichSpawnReviewRepairDecision(&decision, st, req.PRNumber)
		if !ok {
			return res
		}
	}

	approval := st.RecordPendingApprovalForDecision(decision, now)
	if approval == nil {
		return safeActionResult{handled: true, status: http.StatusInternalServerError, body: map[string]string{"error": "failed to record approval"}, err: errors.New("RecordPendingApprovalForDecision returned nil")}
	}

	target := approvalTargetSummary(decision.Target)
	// #491 audit-fail-closed: record the audit BEFORE persisting to disk.
	// If the audit append fails, on-disk state stays unchanged (no orphan
	// pending approval that has no audit record). The in-memory state was
	// already mutated by RecordPendingApprovalForDecision but it is not
	// observable until state.Save below; the next Load discards it.
	if err := auditOrAbort(audit, req, authenticatedActor, "enqueue:"+id, target, "enqueued by dashboard"); err != nil {
		return auditFailureResult("enqueue:"+id, target, err)
	}
	if err := state.Save(stateDir, st); err != nil {
		return safeActionResult{handled: true, status: http.StatusInternalServerError, body: map[string]string{"error": fmt.Sprintf("save state: %v", err)}, err: err}
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
	case config.SupervisorActionCloseIssueBatch:
		if len(normalizeCloseIssueBatch(req.Issues)) == 0 {
			return errors.New("issues is required for close_issue_batch")
		}
	case config.SupervisorActionDeleteWorktree:
		if err := state.ValidateSlotID(req.Slot); err != nil {
			return fmt.Errorf("delete_worktree: %w", err)
		}
	case config.SupervisorActionChangeGlobalConfig:
		if strings.TrimSpace(req.Reason) == "" {
			return errors.New("reason is required for change_global_config")
		}
	case config.SupervisorActionSpawnReviewRepair:
		// #565: the dispatcher needs a PR number to spawn the scoped
		// repair worker; the head SHA is observed at decision time and
		// can be empty for the HTTP enqueue (operators may invoke this
		// manually against the latest head).
		if req.PRNumber <= 0 {
			return errors.New("pr_number is required for spawn_review_repair")
		}
	case config.SupervisorActionRestartWorker, config.SupervisorActionStopWorker:
		// #567: the executor / WorkerController needs the slot to kill
		// the tmux session. We also require an issue number so the
		// slot-reuse fence (premortem #7) can refuse to operate on a
		// recycled slot that now belongs to a different issue.
		if err := state.ValidateSlotID(req.Slot); err != nil {
			return fmt.Errorf("%s: %w", id, err)
		}
		if req.IssueNumber <= 0 {
			return fmt.Errorf("issue_number is required for %s (slot-reuse fence)", id)
		}
	}
	return nil
}

// enrichSpawnReviewRepairDecision stamps a proven review-repair payload onto a
// manual spawn_review_repair decision, or returns a rejection result when no
// proof exists for the target PR (#874). The proof — head SHA, blocking
// findings, backend — is reused from a payload the supervisor already observed
// on this PR (an effective approval or a recent decision); the durable payload
// then rides the minted approval so the dispatcher can converge on exactly one
// worker without a coincident latest supervisor decision. Returns (result,
// ok): ok=false means the caller must return result (an HTTP rejection) instead
// of recording the approval.
func enrichSpawnReviewRepairDecision(decision *state.SupervisorDecision, st *state.State, prNumber int) (safeActionResult, bool) {
	payload, provenTarget, ok := st.EffectiveReviewRepairPayloadForPR(prNumber)
	if !ok {
		msg := fmt.Sprintf(
			"cannot enqueue spawn_review_repair for PR #%d: no proven blocking review findings on its current head. "+
				"The supervisor mints a scoped review-repair (with head SHA + findings) automatically once a green, "+
				"mergeable, retry-exhausted PR is blocked by review — approve that when it appears. A manual enqueue "+
				"without that proof would stall in awaiting_dispatch with no dispatcher path, so it is refused here.",
			prNumber)
		return safeActionResult{
			handled: true,
			status:  http.StatusConflict,
			body:    map[string]string{"error": msg},
			err:     fmt.Errorf("spawn_review_repair PR #%d: no proven review-repair payload to enqueue", prNumber),
		}, false
	}
	decision.ReviewRepair = payload
	if decision.Target == nil {
		decision.Target = &state.SupervisorTarget{PR: prNumber}
	}
	// Key the approval on the proven head SHA so a later head push mints a
	// distinct approval (approvalTargetsEqual includes HeadSHA) and repeat
	// clicks on the same head coalesce to the one record.
	decision.Target.HeadSHA = payload.HeadSHA
	if decision.Target.Issue == 0 && provenTarget != nil {
		decision.Target.Issue = provenTarget.Issue
	}
	return safeActionResult{}, true
}

// buildApprovalDecision constructs a synthetic SupervisorDecision so the
// HTTP enqueue path uses the same approval pipeline as the LLM supervisor.
// The decision is recorded in state as the approval source.
//
// #487: authenticatedActor (when non-empty) replaces req.Actor in the
// decision's Reasons field — the audit-of-record for who enqueued the
// approval cannot be set from an attacker-controlled body field.
func buildApprovalDecision(id string, req controlActionRequest, cfg *config.Config, authenticatedActor string, now time.Time) state.SupervisorDecision {
	actor := resolveActor(authenticatedActor, req.Actor, "dashboard")
	summary := strings.TrimSpace(req.Reason)
	if summary == "" {
		summary = fmt.Sprintf("HTTP enqueue of %s", id)
	}

	target := &state.SupervisorTarget{
		Issue:   req.IssueNumber,
		PR:      req.PRNumber,
		Issues:  normalizeCloseIssueBatch(req.Issues),
		Session: strings.TrimSpace(req.Slot),
	}
	if target.Issue == 0 && target.PR == 0 && len(target.Issues) == 0 && target.Session == "" {
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
	if len(target.Issues) > 0 {
		parts = append(parts, fmt.Sprintf("%d issues", len(target.Issues)))
	}
	if target.Session != "" {
		parts = append(parts, "session "+target.Session)
	}
	return strings.Join(parts, " ")
}

func normalizeCloseIssueBatch(in []state.SupervisorIssueTarget) []state.SupervisorIssueTarget {
	if len(in) == 0 {
		return nil
	}
	byIssue := make(map[int]state.SupervisorIssueTarget, len(in))
	for _, item := range in {
		if item.Issue <= 0 {
			continue
		}
		existing := byIssue[item.Issue]
		if existing.Issue == 0 || existing.PR <= 0 {
			byIssue[item.Issue] = item
		}
	}
	if len(byIssue) == 0 {
		return nil
	}
	issues := make([]int, 0, len(byIssue))
	for issue := range byIssue {
		issues = append(issues, issue)
	}
	sort.Ints(issues)
	out := make([]state.SupervisorIssueTarget, 0, len(issues))
	for _, issue := range issues {
		out = append(out, byIssue[issue])
	}
	return out
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
