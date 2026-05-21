package outcome

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// DriftActionPolicy describes how Maestro should respond to a drift item.
const (
	// DriftActionPolicyApprovalRequired marks an action that mutates GitHub
	// (comment, reopen, label) and must therefore be operator-approved.
	DriftActionPolicyApprovalRequired = "approval_required"
	// DriftActionPolicyReadOnly marks an action that requires no mutation —
	// the operator simply runs the configured healthcheck.
	DriftActionPolicyReadOnly = "read_only"
)

// DriftSession is the minimal session view consumed by the drift detector.
// State packages can build these without leaking the full session type into
// the outcome package.
type DriftSession struct {
	Slot        string
	IssueNumber int
	IssueTitle  string
	PRNumber    int
	PRMerged    bool
	IssueClosed bool
	LandedAt    time.Time
}

// DriftItem describes a unit of work GitHub considers complete (PR merged or
// issue closed) whose runtime/deploy outcome is failing, unknown, or missing.
// Drift items belong on the active Fleet surface, not in history.
type DriftItem struct {
	Slot              string    `json:"slot,omitempty"`
	IssueNumber       int       `json:"issue_number,omitempty"`
	IssueTitle        string    `json:"issue_title,omitempty"`
	PRNumber          int       `json:"pr_number,omitempty"`
	PRMerged          bool      `json:"pr_merged,omitempty"`
	IssueClosed       bool      `json:"issue_closed,omitempty"`
	LandedAt          time.Time `json:"landed_at,omitempty"`
	HealthState       string    `json:"health_state"`
	HealthSignal      string    `json:"health_signal,omitempty"`
	HealthCheckedAt   time.Time `json:"health_checked_at,omitempty"`
	HealthSummary     string    `json:"health_summary,omitempty"`
	Reason            string    `json:"reason"`
	RecommendedAction string    `json:"recommended_action"`
	ActionPolicy      string    `json:"action_policy,omitempty"`
}

// DetectDrifts returns active drift items for the given sessions. A drift
// exists when a session has merged code or a closed issue but the latest
// outcome check is missing, stale, unknown, or failing. A healthy post-merge
// check suppresses the drift.
//
// Callers without a configured brief get no drift items: drift detection is
// only meaningful once the project has declared a runtime outcome.
func DetectDrifts(brief Brief, sessions []DriftSession, latestCheck *HealthCheckResult, now time.Time) []DriftItem {
	brief = brief.Normalized()
	if !brief.Configured() {
		return nil
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	hasSignal := brief.HasHealthSignal()
	drifts := make([]DriftItem, 0, len(sessions))
	for _, sess := range sessions {
		if !sess.PRMerged && !sess.IssueClosed {
			continue
		}
		item, ok := buildDriftItem(brief, sess, latestCheck, hasSignal)
		if !ok {
			continue
		}
		drifts = append(drifts, item)
	}
	sortDrifts(drifts)
	return drifts
}

func buildDriftItem(brief Brief, sess DriftSession, latestCheck *HealthCheckResult, hasSignal bool) (DriftItem, bool) {
	item := DriftItem{
		Slot:        strings.TrimSpace(sess.Slot),
		IssueNumber: sess.IssueNumber,
		IssueTitle:  strings.TrimSpace(sess.IssueTitle),
		PRNumber:    sess.PRNumber,
		PRMerged:    sess.PRMerged && sess.PRNumber > 0,
		IssueClosed: sess.IssueClosed,
		LandedAt:    sess.LandedAt.UTC(),
	}

	if !hasSignal {
		item.HealthState = HealthUnmonitored
		item.Reason = driftReason(item, brief, HealthUnmonitored, "no runtime/deploy health signal is configured")
		item.RecommendedAction = "Add a read-only deployment status or healthcheck command/URL, then verify the runtime outcome."
		item.ActionPolicy = DriftActionPolicyReadOnly
		return item, true
	}

	if latestCheck != nil && !latestCheck.CheckedAt.IsZero() {
		item.HealthSignal = strings.TrimSpace(latestCheck.Signal)
		item.HealthCheckedAt = latestCheck.CheckedAt.UTC()
		item.HealthSummary = strings.TrimSpace(latestCheck.Summary)
		state := normalizedHealthState(latestCheck.State)
		// A check that landed before this session's merge/close cannot prove
		// the runtime outcome for it.
		if !item.LandedAt.IsZero() && latestCheck.CheckedAt.Before(item.LandedAt) {
			item.HealthState = HealthUnknown
			item.Reason = driftReason(item, brief, HealthUnknown, fmt.Sprintf("the most recent runtime check (%s) ran before this work landed", latestCheck.CheckedAt.UTC().Format(time.RFC3339)))
			item.RecommendedAction = driftMutatingRecommendedAction(item)
			item.ActionPolicy = DriftActionPolicyApprovalRequired
			return item, true
		}
		switch state {
		case HealthHealthy:
			return DriftItem{}, false
		case HealthFailing:
			item.HealthState = HealthFailing
			item.Reason = driftReason(item, brief, HealthFailing, firstNonEmpty(item.HealthSummary, "the runtime healthcheck is failing"))
			item.RecommendedAction = driftMutatingRecommendedAction(item)
			item.ActionPolicy = DriftActionPolicyApprovalRequired
			return item, true
		default:
			item.HealthState = HealthUnknown
			item.Reason = driftReason(item, brief, HealthUnknown, "the runtime healthcheck has not confirmed the outcome")
			item.RecommendedAction = driftMutatingRecommendedAction(item)
			item.ActionPolicy = DriftActionPolicyApprovalRequired
			return item, true
		}
	}

	item.HealthState = HealthUnknown
	item.Reason = driftReason(item, brief, HealthUnknown, "no runtime healthcheck has been recorded for this work")
	item.RecommendedAction = driftMutatingRecommendedAction(item)
	item.ActionPolicy = DriftActionPolicyApprovalRequired
	return item, true
}

func driftReason(item DriftItem, brief Brief, state, detail string) string {
	subject := driftSubject(item)
	goal := strings.TrimSpace(brief.Goal())
	if goal == "" {
		goal = "the configured runtime outcome"
	}
	stateLabel := strings.ReplaceAll(state, "_", " ")
	if detail = strings.TrimSpace(detail); detail != "" {
		return fmt.Sprintf("%s but runtime outcome health is %s for %s: %s.", subject, stateLabel, goal, detail)
	}
	return fmt.Sprintf("%s but runtime outcome health is %s for %s.", subject, stateLabel, goal)
}

func driftSubject(item DriftItem) string {
	switch {
	case item.PRMerged && item.PRNumber > 0 && item.IssueClosed && item.IssueNumber > 0:
		return fmt.Sprintf("PR #%d merged and issue #%d closed", item.PRNumber, item.IssueNumber)
	case item.PRMerged && item.PRNumber > 0:
		if item.IssueNumber > 0 {
			return fmt.Sprintf("PR #%d for issue #%d merged", item.PRNumber, item.IssueNumber)
		}
		return fmt.Sprintf("PR #%d merged", item.PRNumber)
	case item.IssueClosed && item.IssueNumber > 0:
		return fmt.Sprintf("Issue #%d closed", item.IssueNumber)
	default:
		return "Work landed"
	}
}

func driftMutatingRecommendedAction(item DriftItem) string {
	// All follow-ups (comment, reopen, label) mutate GitHub state, so the
	// supervisor should request approval before applying them. The text gives
	// the operator the concrete safe and mutating options.
	switch {
	case item.PRMerged && item.PRNumber > 0 && item.IssueNumber > 0:
		return fmt.Sprintf("Run the configured runtime healthcheck; if it stays unhealthy, request approval to comment on PR #%d, reopen issue #%d, or relabel it for follow-up.", item.PRNumber, item.IssueNumber)
	case item.PRMerged && item.PRNumber > 0:
		return fmt.Sprintf("Run the configured runtime healthcheck; if it stays unhealthy, request approval to comment on PR #%d for follow-up.", item.PRNumber)
	case item.IssueClosed && item.IssueNumber > 0:
		return fmt.Sprintf("Run the configured runtime healthcheck; if it stays unhealthy, request approval to reopen or relabel issue #%d.", item.IssueNumber)
	default:
		return "Run the configured runtime healthcheck; if it stays unhealthy, request approval before mutating GitHub state."
	}
}

func sortDrifts(drifts []DriftItem) {
	sort.SliceStable(drifts, func(i, j int) bool {
		si, sj := driftSeverity(drifts[i].HealthState), driftSeverity(drifts[j].HealthState)
		if si != sj {
			return si < sj
		}
		ti, tj := drifts[i].LandedAt, drifts[j].LandedAt
		if !ti.Equal(tj) {
			return ti.Before(tj)
		}
		if drifts[i].PRNumber != drifts[j].PRNumber {
			return drifts[i].PRNumber < drifts[j].PRNumber
		}
		return drifts[i].IssueNumber < drifts[j].IssueNumber
	})
}

func driftSeverity(state string) int {
	switch state {
	case HealthFailing:
		return 0
	case HealthUnknown:
		return 1
	case HealthUnmonitored:
		return 2
	default:
		return 3
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if v := strings.TrimSpace(value); v != "" {
			return v
		}
	}
	return ""
}
