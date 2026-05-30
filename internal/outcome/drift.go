package outcome

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// Drift recommended actions exposed by DetectDrift. They map to supervisor
// queue mutations and Fleet operator-state controls.
const (
	DriftActionNone            = "none"
	DriftActionCommentPR       = "comment_pr"
	DriftActionCommentIssue    = "comment_issue"
	DriftActionReopenIssue     = "reopen_issue"
	DriftActionLabelIssue      = "label_issue"
	DriftActionRequestApproval = "request_approval"
	DriftActionCheckHealth     = "check_outcome_health"
)

// DriftPolicy controls how Maestro responds when outcome drift is detected
// after a PR merge or issue close. Mutating actions (reopen, label, comment)
// require explicit approval unless AutoApprove is set.
type DriftPolicy struct {
	CommentPR    bool     `yaml:"comment_pr" json:"comment_pr,omitempty"`
	CommentIssue bool     `yaml:"comment_issue" json:"comment_issue,omitempty"`
	ReopenIssue  bool     `yaml:"reopen_issue" json:"reopen_issue,omitempty"`
	Labels       []string `yaml:"labels" json:"labels,omitempty"`
	AutoApprove  bool     `yaml:"auto_approve" json:"auto_approve,omitempty"`
}

// DriftPR identifies a merged pull request that may not have actually
// delivered the configured runtime outcome.
type DriftPR struct {
	Number   int       `json:"number"`
	Title    string    `json:"title,omitempty"`
	URL      string    `json:"url,omitempty"`
	MergedAt time.Time `json:"merged_at,omitempty"`
}

// DriftIssue identifies a closed issue that may not have actually delivered
// the configured runtime outcome.
type DriftIssue struct {
	Number   int       `json:"number"`
	Title    string    `json:"title,omitempty"`
	URL      string    `json:"url,omitempty"`
	ClosedAt time.Time `json:"closed_at,omitempty"`
}

// DriftInput collects everything DetectDrift needs to decide whether
// outcome drift is active for the project.
type DriftInput struct {
	Brief        Brief
	LastMergeAt  time.Time
	MergedPRs    []DriftPR
	ClosedIssues []DriftIssue
	Health       *HealthCheckResult
	Policy       DriftPolicy
}

// DriftItem is the active outcome-drift item exposed to Fleet/CLI surfaces.
// It contains the merged PR, the closed issue, the failing or missing
// runtime signal, and the recommended next action.
type DriftItem struct {
	HealthState       string       `json:"health_state"`
	Signal            string       `json:"signal,omitempty"`
	SignalSummary     string       `json:"signal_summary,omitempty"`
	SignalDetail      string       `json:"signal_detail,omitempty"`
	HealthCheckedAt   string       `json:"health_checked_at,omitempty"`
	Goal              string       `json:"goal,omitempty"`
	Summary           string       `json:"summary"`
	NextAction        string       `json:"next_action"`
	RecommendedAction string       `json:"recommended_action,omitempty"`
	RequiresApproval  bool         `json:"requires_approval,omitempty"`
	Labels            []string     `json:"labels,omitempty"`
	MergedPRs         []DriftPR    `json:"merged_prs,omitempty"`
	ClosedIssues      []DriftIssue `json:"closed_issues,omitempty"`
}

// DetectDrift returns the active drift item when GitHub reports work as done
// (a merged PR or closed issue) but the configured outcome/runtime signal
// says otherwise. It returns nil when there is no drift to report.
func DetectDrift(input DriftInput) *DriftItem {
	brief := input.Brief.Normalized()
	if !brief.Configured() {
		return nil
	}

	mergedPRs := sortedMergedPRs(input.MergedPRs)
	closedIssues := sortedClosedIssues(input.ClosedIssues)
	if len(mergedPRs) == 0 && len(closedIssues) == 0 {
		return nil
	}

	health := classifyDriftHealth(brief, input.Health, input.LastMergeAt)
	if health.state == HealthHealthy {
		return nil
	}

	goal := brief.Goal()
	if goal == "" {
		goal = "the configured runtime outcome"
	}

	item := &DriftItem{
		HealthState:     health.state,
		Signal:          health.signal,
		SignalSummary:   health.summary,
		SignalDetail:    health.detail,
		HealthCheckedAt: health.checkedAt,
		Goal:            goal,
		MergedPRs:       mergedPRs,
		ClosedIssues:    closedIssues,
	}
	item.Summary = driftSummary(health.state, goal, mergedPRs, closedIssues)
	item.NextAction = driftNextAction(brief, health.state)
	item.RecommendedAction, item.RequiresApproval, item.Labels = applyDriftPolicy(input.Policy, mergedPRs, closedIssues)
	return item
}

type driftHealth struct {
	state     string
	signal    string
	summary   string
	detail    string
	checkedAt string
}

func classifyDriftHealth(brief Brief, health *HealthCheckResult, lastMergeAt time.Time) driftHealth {
	if !brief.HasHealthSignal() {
		return driftHealth{state: HealthUnmonitored}
	}
	if health == nil || health.CheckedAt.IsZero() {
		return driftHealth{state: HealthUnknown}
	}
	if !lastMergeAt.IsZero() && health.CheckedAt.Before(lastMergeAt) {
		return driftHealth{state: HealthUnknown}
	}
	return driftHealth{
		state:     normalizedHealthState(health.State),
		signal:    health.Signal,
		summary:   health.Summary,
		detail:    health.Detail,
		checkedAt: health.CheckedAt.UTC().Format(time.RFC3339),
	}
}

func driftSummary(healthState, goal string, prs []DriftPR, issues []DriftIssue) string {
	descriptor := healthDescriptor(healthState)
	mergedPart := joinPRNumbers(prs)
	closedPart := joinIssueNumbers(issues)
	switch {
	case mergedPart != "" && closedPart != "":
		return fmt.Sprintf("GitHub reports PR %s merged and issue %s closed, but runtime outcome health is %s for %s.", mergedPart, closedPart, descriptor, goal)
	case mergedPart != "":
		return fmt.Sprintf("GitHub reports PR %s merged, but runtime outcome health is %s for %s.", mergedPart, descriptor, goal)
	case closedPart != "":
		return fmt.Sprintf("GitHub reports issue %s closed, but runtime outcome health is %s for %s.", closedPart, descriptor, goal)
	default:
		return fmt.Sprintf("Runtime outcome health is %s for %s.", descriptor, goal)
	}
}

func driftNextAction(brief Brief, healthState string) string {
	switch healthState {
	case HealthFailing:
		if cmd := strings.TrimSpace(brief.HealthcheckCommand); cmd != "" {
			return fmt.Sprintf("Runtime outcome is failing; re-run %q and fix the runtime/deploy regression before treating the merged work as done.", cmd)
		}
		if url := strings.TrimSpace(brief.HealthcheckURL); url != "" {
			return fmt.Sprintf("Runtime outcome is failing; re-check %s and fix the runtime/deploy regression before treating the merged work as done.", url)
		}
		return "Runtime outcome is failing; fix the runtime/deploy regression before treating the merged work as done."
	case HealthUnknown:
		return "Run the configured outcome healthcheck before treating the merged PR or closed issue as fully delivered."
	case HealthUnmonitored:
		return "Add a read-only outcome health signal (deployment status / healthcheck command or URL), then verify the runtime target."
	default:
		return "Verify the configured runtime outcome before treating the merged PR or closed issue as fully delivered."
	}
}

func applyDriftPolicy(policy DriftPolicy, prs []DriftPR, issues []DriftIssue) (action string, requiresApproval bool, labels []string) {
	labels = compactStrings(policy.Labels)
	switch {
	case policy.ReopenIssue && len(issues) > 0:
		action = DriftActionReopenIssue
	case len(labels) > 0 && len(issues) > 0:
		action = DriftActionLabelIssue
	case policy.CommentIssue && len(issues) > 0:
		action = DriftActionCommentIssue
	case policy.CommentPR && len(prs) > 0:
		action = DriftActionCommentPR
	default:
		return DriftActionCheckHealth, false, labels
	}
	if policy.AutoApprove {
		return action, false, labels
	}
	return DriftActionRequestApproval, true, labels
}

func healthDescriptor(healthState string) string {
	switch healthState {
	case HealthFailing:
		return "failing"
	case HealthUnknown:
		return "unknown"
	case HealthUnmonitored:
		return "not monitored"
	case HealthHealthy:
		return "healthy"
	case HealthNotConfigured:
		return "not configured"
	default:
		return strings.ReplaceAll(healthState, "_", " ")
	}
}

func joinPRNumbers(prs []DriftPR) string {
	parts := make([]string, 0, len(prs))
	for _, pr := range prs {
		if pr.Number <= 0 {
			continue
		}
		parts = append(parts, fmt.Sprintf("#%d", pr.Number))
	}
	return strings.Join(parts, ", ")
}

func joinIssueNumbers(issues []DriftIssue) string {
	parts := make([]string, 0, len(issues))
	for _, issue := range issues {
		if issue.Number <= 0 {
			continue
		}
		parts = append(parts, fmt.Sprintf("#%d", issue.Number))
	}
	return strings.Join(parts, ", ")
}

func sortedMergedPRs(prs []DriftPR) []DriftPR {
	out := make([]DriftPR, 0, len(prs))
	seen := make(map[int]struct{}, len(prs))
	for _, pr := range prs {
		if pr.Number <= 0 {
			continue
		}
		if _, ok := seen[pr.Number]; ok {
			continue
		}
		seen[pr.Number] = struct{}{}
		out = append(out, pr)
	}
	sort.SliceStable(out, func(i, j int) bool {
		ai, aj := out[i].MergedAt, out[j].MergedAt
		if ai.IsZero() && aj.IsZero() {
			return out[i].Number < out[j].Number
		}
		if ai.IsZero() {
			return false
		}
		if aj.IsZero() {
			return true
		}
		return ai.After(aj)
	})
	return out
}

func sortedClosedIssues(issues []DriftIssue) []DriftIssue {
	out := make([]DriftIssue, 0, len(issues))
	seen := make(map[int]struct{}, len(issues))
	for _, issue := range issues {
		if issue.Number <= 0 {
			continue
		}
		if _, ok := seen[issue.Number]; ok {
			continue
		}
		seen[issue.Number] = struct{}{}
		out = append(out, issue)
	}
	sort.SliceStable(out, func(i, j int) bool {
		ai, aj := out[i].ClosedAt, out[j].ClosedAt
		if ai.IsZero() && aj.IsZero() {
			return out[i].Number < out[j].Number
		}
		if ai.IsZero() {
			return false
		}
		if aj.IsZero() {
			return true
		}
		return ai.After(aj)
	})
	return out
}
