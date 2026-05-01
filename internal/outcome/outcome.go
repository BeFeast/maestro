package outcome

import (
	"strings"
	"time"
)

const (
	HealthNotConfigured = "not_configured"
	HealthUnmonitored   = "unmonitored"
	HealthUnknown       = "unknown"
	HealthHealthy       = "healthy"
	HealthFailing       = "failing"
)

// Brief is the project operating brief Maestro uses to judge progress by the
// runtime outcome instead of by raw issue throughput.
type Brief struct {
	DesiredOutcome          string   `yaml:"desired_outcome" json:"desired_outcome,omitempty"`
	RuntimeTarget           string   `yaml:"runtime_target" json:"runtime_target,omitempty"`
	DeploymentStatusCommand string   `yaml:"deployment_status_command" json:"deployment_status_command,omitempty"`
	DeployStatusCommand     string   `yaml:"deploy_status_command" json:"-"`
	HealthcheckCommand      string   `yaml:"healthcheck_command" json:"healthcheck_command,omitempty"`
	HealthcheckURL          string   `yaml:"healthcheck_url" json:"healthcheck_url,omitempty"`
	SourceRepoPath          string   `yaml:"source_repo_path" json:"source_repo_path,omitempty"`
	RuntimeHost             string   `yaml:"runtime_host" json:"runtime_host,omitempty"`
	NonGoals                []string `yaml:"non_goals" json:"non_goals,omitempty"`
}

// Status is the concise outcome state exposed by CLI/API/dashboard surfaces.
type Status struct {
	Configured              bool     `json:"configured"`
	Goal                    string   `json:"goal,omitempty"`
	DesiredOutcome          string   `json:"desired_outcome,omitempty"`
	RuntimeTarget           string   `json:"runtime_target,omitempty"`
	RuntimeHost             string   `json:"runtime_host,omitempty"`
	HealthState             string   `json:"health_state"`
	HealthCheckedAt         string   `json:"health_checked_at,omitempty"`
	HealthSignal            string   `json:"health_signal,omitempty"`
	HealthSummary           string   `json:"health_summary,omitempty"`
	HealthDetail            string   `json:"health_detail,omitempty"`
	NextAction              string   `json:"next_action,omitempty"`
	SourceRepoPath          string   `json:"source_repo_path,omitempty"`
	DeploymentStatusCommand string   `json:"deployment_status_command,omitempty"`
	DeployStatusCommand     string   `json:"deploy_status_command,omitempty"`
	HealthcheckCommand      string   `json:"healthcheck_command,omitempty"`
	HealthcheckURL          string   `json:"healthcheck_url,omitempty"`
	NonGoals                []string `json:"non_goals,omitempty"`
	MergedPRs               int      `json:"merged_prs,omitempty"`
	LastMergeAt             string   `json:"last_merge_at,omitempty"`
}

// HealthCheckResult is the durable result of a read-only runtime/deploy health
// check. It is intentionally compact because it is stored in Maestro state.
type HealthCheckResult struct {
	CheckedAt      time.Time `json:"checked_at,omitempty"`
	Signal         string    `json:"signal,omitempty"`
	State          string    `json:"state"`
	Summary        string    `json:"summary,omitempty"`
	Detail         string    `json:"detail,omitempty"`
	ExitCode       int       `json:"exit_code,omitempty"`
	DurationMillis int64     `json:"duration_ms,omitempty"`
}

func (b Brief) Normalized() Brief {
	b.DesiredOutcome = strings.TrimSpace(b.DesiredOutcome)
	b.RuntimeTarget = strings.TrimSpace(b.RuntimeTarget)
	b.DeploymentStatusCommand = strings.TrimSpace(b.DeploymentStatusCommand)
	b.DeployStatusCommand = strings.TrimSpace(b.DeployStatusCommand)
	if b.DeploymentStatusCommand == "" {
		b.DeploymentStatusCommand = b.DeployStatusCommand
	}
	b.HealthcheckCommand = strings.TrimSpace(b.HealthcheckCommand)
	b.HealthcheckURL = strings.TrimSpace(b.HealthcheckURL)
	b.SourceRepoPath = strings.TrimSpace(b.SourceRepoPath)
	b.RuntimeHost = strings.TrimSpace(b.RuntimeHost)
	b.NonGoals = compactStrings(b.NonGoals)
	return b
}

func (b Brief) Configured() bool {
	b = b.Normalized()
	return b.DesiredOutcome != ""
}

func (b Brief) Goal() string {
	return strings.TrimSpace(b.DesiredOutcome)
}

func (b Brief) HasHealthSignal() bool {
	b = b.Normalized()
	return b.DeploymentStatusCommand != "" || b.HealthcheckCommand != "" || b.HealthcheckURL != ""
}

// StatusFor returns the current known outcome status. Callers may pass the
// latest persisted health check result; StatusFor never executes checks itself.
func StatusFor(brief Brief, mergedPRs int, lastMergeAt time.Time, checks ...HealthCheckResult) Status {
	brief = brief.Normalized()
	if !brief.Configured() {
		return Status{
			Configured:  false,
			HealthState: HealthNotConfigured,
			NextAction:  "Define an outcome brief so Maestro can judge progress by runtime health instead of issue throughput.",
		}
	}

	status := Status{
		Configured:              true,
		Goal:                    brief.Goal(),
		DesiredOutcome:          brief.Goal(),
		RuntimeTarget:           brief.RuntimeTarget,
		RuntimeHost:             brief.RuntimeHost,
		SourceRepoPath:          brief.SourceRepoPath,
		DeploymentStatusCommand: brief.DeploymentStatusCommand,
		DeployStatusCommand:     brief.DeploymentStatusCommand,
		HealthcheckCommand:      brief.HealthcheckCommand,
		HealthcheckURL:          brief.HealthcheckURL,
		NonGoals:                append([]string(nil), brief.NonGoals...),
		MergedPRs:               mergedPRs,
	}
	if !lastMergeAt.IsZero() {
		status.LastMergeAt = lastMergeAt.UTC().Format(time.RFC3339)
	}

	check, hasCheck := latestCheck(checks)
	if hasCheck {
		status.HealthCheckedAt = check.CheckedAt.UTC().Format(time.RFC3339)
		status.HealthSignal = check.Signal
		status.HealthSummary = check.Summary
		status.HealthDetail = check.Detail
		if lastMergeAt.IsZero() || !check.CheckedAt.Before(lastMergeAt) {
			status.HealthState = normalizedHealthState(check.State)
			switch status.HealthState {
			case HealthHealthy:
				status.NextAction = "Runtime outcome health is passing; continue normal supervisor policy."
			case HealthFailing:
				status.NextAction = "Fix runtime/deploy health before dispatching more issue work."
			default:
				status.NextAction = "Re-run the configured runtime healthcheck before dispatching more issue throughput."
			}
			return status
		}
	}

	if brief.HasHealthSignal() {
		status.HealthState = HealthUnknown
		status.NextAction = "Run the configured deployment status or healthcheck and prioritize runtime/deploy fixes until it passes."
	} else {
		status.HealthState = HealthUnmonitored
		status.NextAction = "Add a read-only deployment status or healthcheck command/URL, then verify the runtime target."
	}
	if mergedPRs > 0 && (status.HealthState == HealthUnknown || status.HealthState == HealthUnmonitored) {
		status.NextAction = "Verify the configured runtime outcome before dispatching more issue throughput."
	}
	return status
}

func latestCheck(checks []HealthCheckResult) (HealthCheckResult, bool) {
	var latest HealthCheckResult
	for _, check := range checks {
		if check.CheckedAt.IsZero() {
			continue
		}
		if latest.CheckedAt.IsZero() || check.CheckedAt.After(latest.CheckedAt) {
			latest = check
		}
	}
	if latest.CheckedAt.IsZero() {
		return HealthCheckResult{}, false
	}
	return latest, true
}

func normalizedHealthState(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case HealthHealthy:
		return HealthHealthy
	case HealthFailing:
		return HealthFailing
	case HealthUnknown:
		return HealthUnknown
	case HealthUnmonitored:
		return HealthUnmonitored
	default:
		return HealthUnknown
	}
}

func compactStrings(values []string) []string {
	compact := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		key := strings.ToLower(value)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		compact = append(compact, value)
	}
	return compact
}
