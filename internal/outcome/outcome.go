package outcome

import (
	"fmt"
	"strings"
	"time"
)

const (
	HealthNotConfigured = "not_configured"
	HealthUnmonitored   = "unmonitored"
	HealthUnknown       = "unknown"
	HealthHealthy       = "healthy"
	HealthFailing       = "failing"

	RecoveryModeDisabled  = "disabled"
	RecoveryModeAutomatic = "automatic"

	RecoveryStatusExecuting           = "executing"
	RecoveryStatusVerificationPending = "verification_pending"
	RecoveryStatusVerified            = "verified"
	RecoveryStatusFailed              = "failed"
	RecoveryStatusUncertain           = "uncertain"
)

const (
	defaultRecoveryInterval = time.Minute
	defaultRecoveryCooldown = 20 * time.Minute
	defaultRecoveryTimeout  = 2 * time.Minute
)

// Brief is the project operating brief Maestro uses to judge progress by the
// runtime outcome instead of by raw issue throughput.
type Brief struct {
	DesiredOutcome          string   `yaml:"desired_outcome" json:"desired_outcome,omitempty"`
	RuntimeTarget           string   `yaml:"runtime_target" json:"runtime_target,omitempty"`
	RuntimeURL              string   `yaml:"runtime_url" json:"runtime_url,omitempty"`
	DeploymentStatusCommand string   `yaml:"deployment_status_command" json:"deployment_status_command,omitempty"`
	DeployStatusCommand     string   `yaml:"deploy_status_command" json:"-"`
	HealthcheckCommand      string   `yaml:"healthcheck_command" json:"healthcheck_command,omitempty"`
	VerifierCommand         string   `yaml:"verifier_command" json:"verifier_command,omitempty"`
	HealthcheckURL          string   `yaml:"healthcheck_url" json:"healthcheck_url,omitempty"`
	SourceRepoPath          string   `yaml:"source_repo_path" json:"source_repo_path,omitempty"`
	RuntimeHost             string   `yaml:"runtime_host" json:"runtime_host,omitempty"`
	RequiredRoutes          []string `yaml:"required_routes" json:"required_routes,omitempty"`
	RequiresDeploy          bool     `yaml:"requires_deploy" json:"requires_deploy,omitempty"`
	PassRequiredForDone     *bool    `yaml:"pass_required_for_done" json:"-"`
	FailRequiresVisibleWork *bool    `yaml:"fail_requires_visible_work" json:"-"`
	NonGoals                []string `yaml:"non_goals" json:"non_goals,omitempty"`
	// RecoveryCommand is omitted from API JSON: Fleet exposes only its bounded
	// execution receipt, never command text or output.
	RecoveryCommand         string `yaml:"recovery_command" json:"-"`
	RecoveryMode            string `yaml:"recovery_mode" json:"recovery_mode,omitempty"`
	RecoveryIntervalSeconds int    `yaml:"recovery_interval_seconds" json:"recovery_interval_seconds,omitempty"`
	RecoveryCooldownMinutes int    `yaml:"recovery_cooldown_minutes" json:"recovery_cooldown_minutes,omitempty"`
	RecoveryTimeoutSeconds  int    `yaml:"recovery_timeout_seconds" json:"recovery_timeout_seconds,omitempty"`
}

// Status is the concise outcome state exposed by CLI/API/dashboard surfaces.
type Status struct {
	Configured              bool              `json:"configured"`
	Goal                    string            `json:"goal,omitempty"`
	DesiredOutcome          string            `json:"desired_outcome,omitempty"`
	RuntimeTarget           string            `json:"runtime_target,omitempty"`
	RuntimeURL              string            `json:"runtime_url,omitempty"`
	RuntimeHost             string            `json:"runtime_host,omitempty"`
	HealthState             string            `json:"health_state"`
	HealthCheckedAt         string            `json:"health_checked_at,omitempty"`
	HealthSignal            string            `json:"health_signal,omitempty"`
	HealthSummary           string            `json:"health_summary,omitempty"`
	HealthDetail            string            `json:"health_detail,omitempty"`
	NextAction              string            `json:"next_action,omitempty"`
	SourceRepoPath          string            `json:"source_repo_path,omitempty"`
	DeploymentStatusCommand string            `json:"deployment_status_command,omitempty"`
	DeployStatusCommand     string            `json:"deploy_status_command,omitempty"`
	HealthcheckCommand      string            `json:"healthcheck_command,omitempty"`
	VerifierCommand         string            `json:"verifier_command,omitempty"`
	HealthcheckURL          string            `json:"healthcheck_url,omitempty"`
	RequiredRoutes          []string          `json:"required_routes,omitempty"`
	RequiresDeploy          bool              `json:"requires_deploy,omitempty"`
	NonGoals                []string          `json:"non_goals,omitempty"`
	PassRequiredForDone     bool              `json:"pass_required_for_done,omitempty"`
	FailRequiresVisibleWork bool              `json:"fail_requires_visible_work,omitempty"`
	MergedPRs               int               `json:"merged_prs,omitempty"`
	LastMergeAt             string            `json:"last_merge_at,omitempty"`
	Checks                  []HealthCheckItem `json:"checks,omitempty"`
	RecoveryConfigured      bool              `json:"recovery_configured,omitempty"`
	RecoveryMode            string            `json:"recovery_mode,omitempty"`
	Recovery                *RecoveryState    `json:"recovery,omitempty"`
}

// HealthCheckItem is the allow-listed, bounded part of structured checker
// output safe to persist and render. Unknown fields and raw details are
// intentionally discarded.
type HealthCheckItem struct {
	Name     string `json:"name"`
	Blocking bool   `json:"blocking,omitempty"`
	Status   string `json:"status"`
}

// HealthCheckResult is the durable result of a read-only runtime/deploy health
// check. It is intentionally compact because it is stored in Maestro state.
type HealthCheckResult struct {
	CheckedAt      time.Time         `json:"checked_at,omitempty"`
	Signal         string            `json:"signal,omitempty"`
	State          string            `json:"state"`
	Summary        string            `json:"summary,omitempty"`
	Detail         string            `json:"detail,omitempty"`
	ExitCode       int               `json:"exit_code,omitempty"`
	DurationMillis int64             `json:"duration_ms,omitempty"`
	Checks         []HealthCheckItem `json:"checks,omitempty"`
}

// RecoveryState is the durable, command-text-free execution lease and receipt
// for automatic outcome recovery. Executing is persisted before launch so a
// service restart or overlapping loop never replays an uncertain command.
type RecoveryState struct {
	AttemptID        string    `json:"attempt_id,omitempty"`
	Status           string    `json:"status,omitempty"`
	Attempts         int       `json:"attempts,omitempty"`
	TriggerCheckedAt time.Time `json:"trigger_checked_at,omitempty"`
	StartedAt        time.Time `json:"started_at,omitempty"`
	FinishedAt       time.Time `json:"finished_at,omitempty"`
	UpdatedAt        time.Time `json:"updated_at,omitempty"`
	NextEligibleAt   time.Time `json:"next_eligible_at,omitempty"`
	VerifiedAt       time.Time `json:"verified_at,omitempty"`
	ExitCode         *int      `json:"exit_code,omitempty"`
	Summary          string    `json:"summary,omitempty"`
}

func (b Brief) Normalized() Brief {
	b.DesiredOutcome = strings.TrimSpace(b.DesiredOutcome)
	b.RuntimeTarget = strings.TrimSpace(b.RuntimeTarget)
	b.RuntimeURL = strings.TrimSpace(b.RuntimeURL)
	if b.RuntimeTarget == "" {
		b.RuntimeTarget = b.RuntimeURL
	}
	if b.RuntimeURL == "" {
		b.RuntimeURL = b.RuntimeTarget
	}
	b.DeploymentStatusCommand = strings.TrimSpace(b.DeploymentStatusCommand)
	b.DeployStatusCommand = strings.TrimSpace(b.DeployStatusCommand)
	if b.DeploymentStatusCommand == "" {
		b.DeploymentStatusCommand = b.DeployStatusCommand
	}
	b.HealthcheckCommand = strings.TrimSpace(b.HealthcheckCommand)
	b.VerifierCommand = strings.TrimSpace(b.VerifierCommand)
	if b.HealthcheckCommand == "" {
		b.HealthcheckCommand = b.VerifierCommand
	}
	if b.VerifierCommand == "" {
		b.VerifierCommand = b.HealthcheckCommand
	}
	b.HealthcheckURL = strings.TrimSpace(b.HealthcheckURL)
	b.SourceRepoPath = strings.TrimSpace(b.SourceRepoPath)
	b.RuntimeHost = strings.TrimSpace(b.RuntimeHost)
	b.RequiredRoutes = compactStrings(b.RequiredRoutes)
	b.NonGoals = compactStrings(b.NonGoals)
	b.RecoveryCommand = strings.TrimSpace(b.RecoveryCommand)
	b.RecoveryMode = strings.ToLower(strings.TrimSpace(b.RecoveryMode))
	if b.RecoveryMode == "" {
		b.RecoveryMode = RecoveryModeDisabled
	}
	return b
}

func (b Brief) Validate() error {
	b = b.Normalized()
	if b.RecoveryIntervalSeconds < 0 || b.RecoveryCooldownMinutes < 0 || b.RecoveryTimeoutSeconds < 0 {
		return fmt.Errorf("outcome recovery interval, cooldown, and timeout must be >= 0")
	}
	switch b.RecoveryMode {
	case RecoveryModeDisabled:
		return nil
	case RecoveryModeAutomatic:
		if !b.Configured() {
			return fmt.Errorf("outcome.recovery_mode automatic requires outcome.desired_outcome")
		}
		if !b.HasHealthSignal() {
			return fmt.Errorf("outcome.recovery_mode automatic requires a health signal")
		}
		if b.RecoveryCommand == "" {
			return fmt.Errorf("outcome.recovery_mode automatic requires outcome.recovery_command")
		}
		return nil
	default:
		return fmt.Errorf("outcome.recovery_mode %q is invalid; use disabled or automatic", b.RecoveryMode)
	}
}

func (b Brief) AutomaticRecoveryEnabled() bool {
	b = b.Normalized()
	return b.Configured() && b.RecoveryMode == RecoveryModeAutomatic && b.RecoveryCommand != "" && b.HasHealthSignal()
}

func (b Brief) EffectiveRecoveryInterval() time.Duration {
	if b.RecoveryIntervalSeconds > 0 {
		return time.Duration(b.RecoveryIntervalSeconds) * time.Second
	}
	return defaultRecoveryInterval
}

func (b Brief) EffectiveRecoveryCooldown() time.Duration {
	if b.RecoveryCooldownMinutes > 0 {
		return time.Duration(b.RecoveryCooldownMinutes) * time.Minute
	}
	return defaultRecoveryCooldown
}

func (b Brief) EffectiveRecoveryTimeout() time.Duration {
	if b.RecoveryTimeoutSeconds > 0 {
		return time.Duration(b.RecoveryTimeoutSeconds) * time.Second
	}
	return defaultRecoveryTimeout
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

func (b Brief) PassRequiredForDoneEnabled() bool {
	b = b.Normalized()
	if b.PassRequiredForDone != nil {
		return *b.PassRequiredForDone
	}
	return b.Configured()
}

func (b Brief) FailRequiresVisibleWorkEnabled() bool {
	b = b.Normalized()
	if b.FailRequiresVisibleWork != nil {
		return *b.FailRequiresVisibleWork
	}
	return b.Configured()
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
		RuntimeURL:              brief.RuntimeURL,
		RuntimeHost:             brief.RuntimeHost,
		SourceRepoPath:          brief.SourceRepoPath,
		DeploymentStatusCommand: brief.DeploymentStatusCommand,
		DeployStatusCommand:     brief.DeploymentStatusCommand,
		HealthcheckCommand:      brief.HealthcheckCommand,
		VerifierCommand:         brief.VerifierCommand,
		HealthcheckURL:          brief.HealthcheckURL,
		RequiredRoutes:          append([]string(nil), brief.RequiredRoutes...),
		RequiresDeploy:          brief.RequiresDeploy,
		NonGoals:                append([]string(nil), brief.NonGoals...),
		PassRequiredForDone:     brief.PassRequiredForDoneEnabled(),
		FailRequiresVisibleWork: brief.FailRequiresVisibleWorkEnabled(),
		RecoveryConfigured:      brief.AutomaticRecoveryEnabled(),
		RecoveryMode:            brief.RecoveryMode,
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
		status.Checks = append([]HealthCheckItem(nil), check.Checks...)
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

// AttachRecovery adds the persisted receipt to a status without exposing the
// configured command.
func AttachRecovery(status Status, recovery *RecoveryState) Status {
	if recovery == nil {
		return status
	}
	copy := *recovery
	if recovery.ExitCode != nil {
		code := *recovery.ExitCode
		copy.ExitCode = &code
	}
	status.Recovery = &copy
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
