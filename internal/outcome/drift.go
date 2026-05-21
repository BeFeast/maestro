package outcome

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// CompletedWork is one "GitHub says done" record — a merged PR and/or closed
// issue — that the drift detector evaluates against the configured runtime
// outcome signals.
//
// The shape is intentionally decoupled from internal/state so this package
// stays a leaf any caller can adapt.
type CompletedWork struct {
	Session     string
	MergedPR    int
	ClosedIssue int
	FinishedAt  time.Time
}

// Drift records the state "GitHub says done, but project outcome/runtime says
// not done" for one completed work record. It is what Fleet renders as an
// active drift item and what supervisor policy turns into a stuck state.
type Drift struct {
	Session          string    `json:"session,omitempty"`
	MergedPR         int       `json:"merged_pr,omitempty"`
	ClosedIssue      int       `json:"closed_issue,omitempty"`
	Signal           string    `json:"signal,omitempty"`
	HealthState      string    `json:"health_state"`
	Summary          string    `json:"summary"`
	NextAction       string    `json:"next_action"`
	RequiresApproval bool      `json:"requires_approval,omitempty"`
	FinishedAt       time.Time `json:"finished_at,omitempty"`
}

// DetectDrift returns drift items for completed work whose runtime outcome
// cannot be confirmed by the latest health check. It is read-only and never
// runs commands; callers must execute outcome.Checker separately.
//
// A drift item is emitted when any of the following holds for a completed
// record:
//   - The outcome brief is configured but no health signal is configured
//     (HealthUnmonitored).
//   - No health check has been recorded yet (HealthUnknown).
//   - The latest health check is in HealthFailing or HealthUnknown state.
//   - The latest check is healthy but its CheckedAt predates the record's
//     FinishedAt, so it cannot prove the runtime caught up with the merge.
//
// A healthy check whose CheckedAt is at-or-after the record's FinishedAt
// clears that record. Records without a recorded FinishedAt fall back to the
// latest known health state without a per-record staleness check.
//
// If the brief is not configured at all, DetectDrift returns nil — that case
// is surfaced as a separate "missing outcome brief" stuck state by the
// supervisor.
func DetectDrift(brief Brief, completed []CompletedWork, latest *HealthCheckResult) []Drift {
	if len(completed) == 0 {
		return nil
	}
	brief = brief.Normalized()
	if !brief.Configured() {
		return nil
	}
	signal := configuredSignalName(brief)
	health, healthCheckedAt := evaluateLatestHealth(brief, latest)

	out := make([]Drift, 0, len(completed))
	for _, work := range completed {
		if work.MergedPR == 0 && work.ClosedIssue == 0 {
			continue
		}
		recordHealth := health
		if recordHealth == HealthHealthy && !work.FinishedAt.IsZero() && healthCheckedAt.Before(work.FinishedAt) {
			recordHealth = HealthUnknown
		}
		if recordHealth == HealthHealthy {
			continue
		}
		drift := Drift{
			Session:     strings.TrimSpace(work.Session),
			MergedPR:    work.MergedPR,
			ClosedIssue: work.ClosedIssue,
			Signal:      signal,
			HealthState: recordHealth,
			FinishedAt:  work.FinishedAt,
		}
		drift.Summary = driftSummary(brief, work, recordHealth)
		drift.NextAction = driftNextAction(brief, recordHealth)
		out = append(out, drift)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].MergedPR != out[j].MergedPR {
			return out[i].MergedPR < out[j].MergedPR
		}
		if out[i].ClosedIssue != out[j].ClosedIssue {
			return out[i].ClosedIssue < out[j].ClosedIssue
		}
		return out[i].Session < out[j].Session
	})
	return out
}

func configuredSignalName(brief Brief) string {
	switch {
	case brief.HealthcheckURL != "":
		return "healthcheck_url"
	case brief.HealthcheckCommand != "":
		return "healthcheck_command"
	case brief.DeploymentStatusCommand != "":
		return "deployment_status_command"
	default:
		return ""
	}
}

func evaluateLatestHealth(brief Brief, latest *HealthCheckResult) (string, time.Time) {
	if !brief.HasHealthSignal() {
		return HealthUnmonitored, time.Time{}
	}
	if latest == nil || latest.CheckedAt.IsZero() {
		return HealthUnknown, time.Time{}
	}
	return normalizedHealthState(latest.State), latest.CheckedAt
}

func driftSummary(brief Brief, work CompletedWork, health string) string {
	goal := strings.TrimSpace(brief.Goal())
	if goal == "" {
		goal = "the configured runtime outcome"
	}
	subject := workSubject(work)
	switch health {
	case HealthFailing:
		return fmt.Sprintf("%s landed, but the configured runtime check is failing for %s.", subject, goal)
	case HealthUnmonitored:
		return fmt.Sprintf("%s landed, but no runtime health signal is configured to verify %s.", subject, goal)
	default:
		return fmt.Sprintf("%s landed, but runtime outcome health is %s for %s.", subject, strings.ReplaceAll(health, "_", " "), goal)
	}
}

func driftNextAction(brief Brief, health string) string {
	switch health {
	case HealthUnmonitored:
		return "Configure a read-only deployment status or healthcheck command/URL so Maestro can verify the runtime outcome."
	case HealthFailing:
		return "Fix the failing runtime/deploy health signal before treating this merge as truly done."
	default:
		if brief.HasHealthSignal() {
			return "Re-run the configured runtime healthcheck and prioritize runtime/deploy fixes until it passes."
		}
		return "Configure a runtime health signal and verify the runtime target before treating this work as complete."
	}
}

func workSubject(work CompletedWork) string {
	switch {
	case work.MergedPR > 0 && work.ClosedIssue > 0:
		return fmt.Sprintf("PR #%d (issue #%d)", work.MergedPR, work.ClosedIssue)
	case work.MergedPR > 0:
		return fmt.Sprintf("PR #%d", work.MergedPR)
	case work.ClosedIssue > 0:
		return fmt.Sprintf("Issue #%d", work.ClosedIssue)
	default:
		return "Completed work"
	}
}
