package state

import (
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/befeast/maestro/internal/outcome"
)

// Worker outcomes that mark a code_landed session as a delivery which did NOT
// settle its issue (#1020). Both keep the session terminal for history while
// ReleasedForRedispatch drops its issue claim so a fresh worker is dispatched.
const (
	// WorkerOutcomeRecordOnlyDelivery: the merged PR changed only non-functional
	// (documentation/record) paths, so it delivered evidence, not a fix.
	WorkerOutcomeRecordOnlyDelivery = "record_only_delivery"
	// WorkerOutcomeCodeLandedIneffective: the merged PR was a real code change,
	// but the blocking outcome check the issue targeted stayed failing with the
	// same fingerprint past the post-merge verification deadline.
	WorkerOutcomeCodeLandedIneffective = "code_landed_ineffective"
	// WorkerOutcomeMergedPRIssueStillOpen: the PR merged successfully, but the
	// linked issue remained open past terminal reconciliation's close grace.
	WorkerOutcomeMergedPRIssueStillOpen = "merged_pr_issue_still_open"
)

// OutcomeFailureFingerprint returns a stable identity for a FAILING outcome
// health result, or "" when the result is not a definite failure. The
// fingerprint intentionally derives only from durable, low-cardinality fields —
// the failing blocking-check names+statuses, else the signal+exit code — so a
// transient re-run of the same failure produces the same value while free-form
// summary/detail text (timestamps, hostnames) never perturbs it. This is what
// lets the ineffective-code_landed guard tell "the same red check is still red"
// apart from "a new, different failure appeared".
func OutcomeFailureFingerprint(result outcome.HealthCheckResult) string {
	if !strings.EqualFold(strings.TrimSpace(result.State), outcome.HealthFailing) {
		return ""
	}
	var blocking []string
	for _, c := range result.Checks {
		if !c.Blocking {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(c.Status), outcome.HealthHealthy) {
			continue
		}
		name := strings.ToLower(strings.TrimSpace(c.Name))
		status := strings.ToLower(strings.TrimSpace(c.Status))
		blocking = append(blocking, name+"="+status)
	}
	if len(blocking) > 0 {
		sort.Strings(blocking)
		return "checks:" + strings.Join(blocking, ",")
	}
	signal := strings.ToLower(strings.TrimSpace(result.Signal))
	if signal == "" {
		signal = "outcome"
	}
	return "signal:" + signal + ":exit:" + strconv.Itoa(result.ExitCode)
}

// CodeLandedIneffective reports whether a code_landed session's post-merge
// verification has definitively failed: the session is still holding its issue
// (not already released), its verification deadline has elapsed, and the
// blocking outcome check is STILL failing with the exact fingerprint recorded
// when the deadline was armed. Deterministic and side-effect free so the
// orchestrator wiring and unit tests share one definition.
func CodeLandedIneffective(sess *Session, current outcome.HealthCheckResult, now time.Time) bool {
	if sess == nil || sess.Status != StatusCodeLanded || sess.ReleasedForRedispatch {
		return false
	}
	if sess.CodeLandedVerifyDeadline == nil || now.Before(*sess.CodeLandedVerifyDeadline) {
		return false
	}
	fp := OutcomeFailureFingerprint(current)
	return fp != "" && fp == sess.OutcomeFailureFingerprint
}
