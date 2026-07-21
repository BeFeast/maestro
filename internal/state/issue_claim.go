package state

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// IssueClaim describes why an issue is not eligible for a fresh worker. Claims
// are derived from durable approvals and sessions so selection and Fleet
// diagnostics explain the same reservation.
type IssueClaim struct {
	IssueNumber int    `json:"issue_number"`
	Kind        string `json:"kind"`
	Session     string `json:"session,omitempty"`
	PRNumber    int    `json:"pr_number,omitempty"`
	ApprovalID  string `json:"approval_id,omitempty"`
	Status      string `json:"status,omitempty"`
	Reason      string `json:"reason"`
}

const (
	IssueClaimImplementation       = "implementation"
	IssueClaimOpenPRMaintenance    = "open_pr_maintenance"
	IssueClaimScheduledRetry       = "scheduled_retry"
	IssueClaimRetainedWorktree     = "retained_worktree"
	IssueClaimTerminalReconcile    = "terminal_reconciliation"
	IssueClaimOperatorGate         = "operator_gate"
	IssueClaimRepairDispatch       = "repair_dispatch"
	IssueClaimReviewRepairDispatch = "review_repair_dispatch"
)

// ActiveIssueClaims returns every durable issue claim in deterministic order.
// An approved repair approval takes precedence over the session it reserves;
// any different same-issue session is still returned so diagnostics expose a
// competing worker instead of hiding it.
func (s *State) ActiveIssueClaims() []IssueClaim {
	if s == nil {
		return nil
	}

	claims := make([]IssueClaim, 0)
	reservedSessions := make(map[string]struct{})
	approvalIndexes := make([]int, 0, len(s.Approvals))
	for i := range s.Approvals {
		if activeRepairDispatchApproval(&s.Approvals[i]) {
			approvalIndexes = append(approvalIndexes, i)
		}
	}
	sort.Slice(approvalIndexes, func(i, j int) bool {
		return s.Approvals[approvalIndexes[i]].ID < s.Approvals[approvalIndexes[j]].ID
	})
	for _, index := range approvalIndexes {
		approval := &s.Approvals[index]
		target := approval.Target
		kind := IssueClaimRepairDispatch
		if approval.Action == approvalActionSpawnReviewRepair {
			kind = IssueClaimReviewRepairDispatch
		}
		session := strings.TrimSpace(target.Session)
		if session != "" {
			reservedSessions[session] = struct{}{}
		}
		claims = append(claims, IssueClaim{
			IssueNumber: target.Issue,
			Kind:        kind,
			Session:     session,
			PRNumber:    target.PR,
			ApprovalID:  approval.ID,
			Status:      string(approval.Status),
			Reason:      repairDispatchClaimReason(approval),
		})
	}

	slots := make([]string, 0, len(s.Sessions))
	for slot := range s.Sessions {
		slots = append(slots, slot)
	}
	sort.Strings(slots)
	for _, slot := range slots {
		sess := s.Sessions[slot]
		if sess == nil || sess.IssueNumber <= 0 {
			continue
		}
		if _, reserved := reservedSessions[slot]; reserved {
			continue
		}
		if claim, ok := sessionIssueClaim(slot, sess); ok {
			claims = append(claims, claim)
		}
	}

	sort.SliceStable(claims, func(i, j int) bool {
		if claims[i].IssueNumber != claims[j].IssueNumber {
			return claims[i].IssueNumber < claims[j].IssueNumber
		}
		if issueClaimPriority(claims[i]) != issueClaimPriority(claims[j]) {
			return issueClaimPriority(claims[i]) < issueClaimPriority(claims[j])
		}
		if claims[i].ApprovalID != claims[j].ApprovalID {
			return claims[i].ApprovalID < claims[j].ApprovalID
		}
		return claims[i].Session < claims[j].Session
	})
	return claims
}

func issueClaimPriority(claim IssueClaim) int {
	switch claim.Kind {
	case IssueClaimRepairDispatch, IssueClaimReviewRepairDispatch:
		return 0
	case IssueClaimImplementation, IssueClaimScheduledRetry, IssueClaimOperatorGate:
		return 1
	case IssueClaimOpenPRMaintenance:
		return 2
	default:
		return 3
	}
}

// IssueClaimFor returns the first durable claim for issueNumber.
func (s *State) IssueClaimFor(issueNumber int) (IssueClaim, bool) {
	if issueNumber <= 0 {
		return IssueClaim{}, false
	}
	for _, claim := range s.ActiveIssueClaims() {
		if claim.IssueNumber == issueNumber {
			return claim, true
		}
	}
	return IssueClaim{}, false
}

// ActiveRepairDispatchApproval returns the effective approval reserving the
// issue for action. Approved is included with awaiting_dispatch so the brief
// transition between operator approval and executor finalization is also safe.
func (s *State) ActiveRepairDispatchApproval(issueNumber int, action string) (*Approval, bool) {
	if s == nil || issueNumber <= 0 {
		return nil, false
	}
	for i := range s.Approvals {
		approval := &s.Approvals[i]
		if approval.Action != action || !activeRepairDispatchApproval(approval) || approval.Target.Issue != issueNumber {
			continue
		}
		return approval, true
	}
	return nil, false
}

// ApprovedRepairOwnsSession reports whether an approved/awaiting-dispatch
// repair authority reserves the exact canonical issue/session/PR identity.
// Cleanup uses this immediately before filesystem mutation: an operator's
// approved in-place recovery is a durable ownership claim even during the
// short window before the replacement PID is persisted.
func (s *State) ApprovedRepairOwnsSession(slot string, issueNumber, prNumber int) (*Approval, bool) {
	if s == nil || strings.TrimSpace(slot) == "" || issueNumber <= 0 {
		return nil, false
	}
	for i := range s.Approvals {
		approval := &s.Approvals[i]
		if !activeRepairDispatchApproval(approval) || approval.Target == nil {
			continue
		}
		target := approval.Target
		if strings.TrimSpace(target.Session) != strings.TrimSpace(slot) || target.Issue != issueNumber {
			continue
		}
		if target.PR > 0 && prNumber > 0 && target.PR != prNumber {
			continue
		}
		return approval, true
	}
	return nil, false
}

// ResolveDispatchedSpawnRepairApproval retires a classic repair approval only
// after its reserved session has successfully reached a worker. Re-running
// after a save/reload is idempotent because terminal approvals are ignored.
func (s *State) ResolveDispatchedSpawnRepairApproval(id string, now time.Time, reason string) bool {
	if s == nil || strings.TrimSpace(id) == "" {
		return false
	}
	for i := range s.Approvals {
		approval := &s.Approvals[i]
		if approval.ID != id || approval.Action != approvalActionSpawnRepairWorker {
			continue
		}
		return s.supersedeApprovalFrom(approval, now, reason,
			ApprovalStatusApproved, ApprovalStatusAwaitingDispatch)
	}
	return false
}

// StaleActiveRepairDispatchApproval retires one exact repair authority after
// dispatch proves that its durable issue/session/PR reservation is no longer
// valid. This is intentionally ID-scoped: another independently reviewed
// repair for the same issue must not be invalidated as a side effect. Repeated
// calls are idempotent because terminal approvals are ignored.
func (s *State) StaleActiveRepairDispatchApproval(id string, now time.Time, reason string) bool {
	if s == nil || strings.TrimSpace(id) == "" {
		return false
	}
	for i := range s.Approvals {
		approval := &s.Approvals[i]
		if approval.ID != id || !repairDispatchApprovalNeedsGuardReconcile(approval) {
			continue
		}
		s.markApprovalStale(approval, now, reason)
		return true
	}
	return false
}

// StaleActiveRepairDispatchApprovals retires delayed repair authority when a
// current issue guard (for example a newly-added blocked label) says dispatch
// is no longer allowed. Approval is not timeless authority: dispatch must
// revalidate the issue at execution time and make the obsolete intent terminal
// so it cannot keep bypassing the guard on every later cycle.
func (s *State) StaleActiveRepairDispatchApprovals(issueNumber int, now time.Time, reason string) []Approval {
	if s == nil || issueNumber <= 0 {
		return nil
	}
	var staled []Approval
	for i := range s.Approvals {
		approval := &s.Approvals[i]
		if !repairDispatchApprovalNeedsGuardReconcile(approval) || approval.Target.Issue != issueNumber {
			continue
		}
		s.markApprovalStale(approval, now, reason)
		staled = append(staled, *approval)
	}
	return staled
}

// ActiveRepairDispatchApprovalIssues returns every issue with a non-terminal
// classic or review-repair approval. Pending is included: it cannot dispatch
// yet, but it is still an operator-visible gate that must become stale when the
// issue is blocked before approval.
func (s *State) ActiveRepairDispatchApprovalIssues() []int {
	if s == nil {
		return nil
	}
	seen := make(map[int]struct{})
	for i := range s.Approvals {
		approval := &s.Approvals[i]
		if repairDispatchApprovalNeedsGuardReconcile(approval) {
			seen[approval.Target.Issue] = struct{}{}
		}
	}
	issues := make([]int, 0, len(seen))
	for issue := range seen {
		issues = append(issues, issue)
	}
	sort.Ints(issues)
	return issues
}

func repairDispatchApprovalNeedsGuardReconcile(approval *Approval) bool {
	if approval == nil || approval.Target == nil || approval.Target.Issue <= 0 {
		return false
	}
	if approval.Action != approvalActionSpawnRepairWorker && approval.Action != approvalActionSpawnReviewRepair {
		return false
	}
	return approval.Status == ApprovalStatusPending || activeRepairDispatchApproval(approval)
}

func activeRepairDispatchApproval(approval *Approval) bool {
	if approval == nil || approval.Target == nil || approval.Target.Issue <= 0 {
		return false
	}
	if approval.Action != approvalActionSpawnRepairWorker && approval.Action != approvalActionSpawnReviewRepair {
		return false
	}
	return approval.Status == ApprovalStatusApproved || approval.Status == ApprovalStatusAwaitingDispatch
}

func repairDispatchClaimReason(approval *Approval) string {
	target := approval.Target
	label := "repair dispatch"
	if approval.Action == approvalActionSpawnReviewRepair {
		label = "review-repair dispatch"
	}
	reason := fmt.Sprintf("issue #%d reserved for %s by approval %s", target.Issue, label, approval.ID)
	if session := strings.TrimSpace(target.Session); session != "" {
		reason += fmt.Sprintf(" on session %s", session)
	}
	if target.PR > 0 {
		reason += fmt.Sprintf(" for PR #%d", target.PR)
	}
	return reason
}

func sessionIssueClaim(slot string, sess *Session) (IssueClaim, bool) {
	claim := IssueClaim{
		IssueNumber: sess.IssueNumber,
		Session:     slot,
		PRNumber:    sess.PRNumber,
		Status:      string(sess.Status),
	}
	if strings.TrimSpace(sess.OperatorGateName) != "" {
		claim.Kind = IssueClaimOperatorGate
		claim.Reason = fmt.Sprintf("issue #%d is held by operator gate %q on session %s", sess.IssueNumber, sess.OperatorGateName, slot)
		return claim, true
	}
	switch sess.Status {
	case StatusRunning, StatusQueued:
		claim.Kind = IssueClaimImplementation
		claim.Reason = fmt.Sprintf("issue #%d already has active session %s (%s)", sess.IssueNumber, slot, sess.Status)
		return claim, true
	case StatusCodeLanded:
		// #1020: a code_landed session normally holds an implementation claim
		// until post-merge verification settles the issue. But when that session
		// is released for redispatch — because its merged PR delivered only
		// non-functional (docs/record) changes, or its blocking outcome check
		// stayed red past the verification deadline — the merge did NOT fix the
		// issue. The claim must drop so the dynamic wave re-dispatches a fresh
		// worker instead of silencing the issue behind an ineffective merge.
		if !sess.ReleasedForRedispatch {
			claim.Kind = IssueClaimImplementation
			claim.Reason = fmt.Sprintf("issue #%d already has active session %s (%s)", sess.IssueNumber, slot, sess.Status)
			return claim, true
		}
	case StatusPROpen:
		claim.Kind = IssueClaimOpenPRMaintenance
		claim.Reason = fmt.Sprintf("issue #%d is maintained by session %s for PR #%d", sess.IssueNumber, slot, sess.PRNumber)
		return claim, true
	case StatusDone:
		// A merged/completed PR is not the end of issue identity until forge
		// reconciliation closes the linked issue (or explicitly releases the
		// session for redispatch). Without this lease there is a real race:
		// pr_open -> done removes the open-PR claim, then the still-open issue
		// retains its ready label and the next selector cycle starts a duplicate
		// worker before close-issue reconciliation completes.
		if sess.PRNumber > 0 && sess.FinishedAt != nil && !sess.ReleasedForRedispatch {
			claim.Kind = IssueClaimTerminalReconcile
			claim.Reason = fmt.Sprintf("issue #%d awaits terminal reconciliation on completed session %s / PR #%d", sess.IssueNumber, slot, sess.PRNumber)
			return claim, true
		}
	case StatusDead:
		if sess.NextRetryAt != nil {
			claim.Kind = IssueClaimScheduledRetry
			claim.Reason = fmt.Sprintf("issue #%d is reserved for retry on session %s", sess.IssueNumber, slot)
			return claim, true
		}
	}

	// A dead/retry-exhausted/conflict session with a retained PR/worktree remains
	// the maintenance owner until reconciliation proves the PR is closed or
	// explicitly releases it for redispatch.
	if sess.PRNumber > 0 && !sess.ReleasedForRedispatch &&
		(sess.Status == StatusDead || sess.Status == StatusRetryExhausted || sess.Status == StatusConflictFailed) {
		claim.Kind = IssueClaimOpenPRMaintenance
		claim.Reason = fmt.Sprintf("issue #%d retains PR #%d maintenance claim on session %s (%s)", sess.IssueNumber, sess.PRNumber, slot, sess.Status)
		return claim, true
	}

	// A retained worktree is an issue-level lease even before PR registration.
	// A worker may finish with completed staged changes while gates are still
	// running; treating that dead session as free creates a second worktree and
	// risks deleting the first one during fallback recovery.
	if strings.TrimSpace(sess.Worktree) != "" && !sess.ReleasedForRedispatch &&
		(sess.Status == StatusDead || sess.Status == StatusFailed || sess.Status == StatusRetryExhausted || sess.Status == StatusConflictFailed) {
		claim.Kind = IssueClaimRetainedWorktree
		claim.Reason = fmt.Sprintf("issue #%d retains resumable worktree on session %s (%s)", sess.IssueNumber, slot, sess.Status)
		return claim, true
	}
	return IssueClaim{}, false
}
