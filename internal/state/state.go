package state

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/befeast/maestro/internal/outcome"
	"github.com/gofrs/flock"
)

type SessionStatus string

const (
	StatusQueued         SessionStatus = "queued"
	StatusRunning        SessionStatus = "running"
	StatusPROpen         SessionStatus = "pr_open"
	StatusCodeLanded     SessionStatus = "code_landed"
	StatusDone           SessionStatus = "done"
	StatusFailed         SessionStatus = "failed"
	StatusConflictFailed SessionStatus = "conflict_failed"
	StatusDead           SessionStatus = "dead"
	StatusRetryExhausted SessionStatus = "retry_exhausted" // max retries reached, needs manual review
)

type SessionDisplayStatus string

const (
	DisplayReviewRetryBackoff SessionDisplayStatus = "review_retry_backoff"
	DisplayReviewRetryPending SessionDisplayStatus = "review_retry_pending"
	DisplayReviewRetryRunning SessionDisplayStatus = "review_retry_running"
	DisplayReviewRetryRecheck SessionDisplayStatus = "review_retry_recheck"
	// DisplayBackendRateLimited marks a session whose worker exited because its
	// backend hit a provider usage limit with no fallback available. It is a
	// distinct, non-failure display token so operators do not confuse a
	// rate-limited backend with a genuine retry_exhausted code failure.
	DisplayBackendRateLimited SessionDisplayStatus = "backend_rate_limited"
	LiveSessionRecentWindow                        = 24 * time.Hour
)

const RetryReasonReviewFeedback = "review_feedback"

const (
	BackendHealthAvailable = "available"
	BackendHealthCooldown  = "cooldown"

	BackendBlockProviderLimit = "provider_limit"
	BackendBlockDisabled      = "disabled"
	BackendBlockAlreadyTried  = "already_tried"
	BackendBlockCurrent       = "current_backend"
	BackendBlockUnknown       = "unknown_backend"
)

// BackendHealth records cross-session availability for a configured backend.
type BackendHealth struct {
	State       string     `json:"state"`
	Reason      string     `json:"reason,omitempty"`
	Pattern     string     `json:"pattern,omitempty"`
	Since       time.Time  `json:"since,omitempty"`
	RetryAfter  *time.Time `json:"retry_after,omitempty"`
	LastSession string     `json:"last_session,omitempty"`
}

// BackendCandidate explains why one backend was or was not selectable.
type BackendCandidate struct {
	Backend    string  `json:"backend"`
	Available  bool    `json:"available"`
	BlockedBy  string  `json:"blocked_by,omitempty"`
	RetryAfter string  `json:"retry_after,omitempty"`
	Fit        float64 `json:"fit,omitempty"`
	Policy     float64 `json:"policy,omitempty"`
	Final      float64 `json:"final,omitempty"`
}

// BackendSelection is an audit record for worker backend choice.
type BackendSelection struct {
	SelectedBackend string             `json:"selected_backend,omitempty"`
	SelectionReason string             `json:"selection_reason"`
	CandidateScores []BackendCandidate `json:"candidate_scores,omitempty"`
	HardPin         bool               `json:"hard_pin,omitempty"`
	PreviousBackend string             `json:"previous_backend,omitempty"`
}

// Phase represents which pipeline phase a session is currently in.
type Phase string

const (
	PhaseNone      Phase = ""          // legacy single-phase mode (no pipeline)
	PhasePlan      Phase = "plan"      // planner: creates MAESTRO_PLAN.md + VALIDATION.md
	PhaseImplement Phase = "implement" // implementer: writes code based on plan
	PhaseValidate  Phase = "validate"  // validator: checks assertions, gates PR creation
)

type Session struct {
	IssueNumber                 int               `json:"issue_number"`
	IssueTitle                  string            `json:"issue_title"`
	Worktree                    string            `json:"worktree"`
	Branch                      string            `json:"branch"`
	PID                         int               `json:"pid"`
	TmuxSession                 string            `json:"tmux_session,omitempty"`
	LogFile                     string            `json:"log_file"`
	StartedAt                   time.Time         `json:"started_at"`
	FinishedAt                  *time.Time        `json:"finished_at,omitempty"`
	Status                      SessionStatus     `json:"status"`
	PRNumber                    int               `json:"pr_number,omitempty"`
	Backend                     string            `json:"backend,omitempty"` // "claude", "codex", etc.
	LongRunning                 bool              `json:"long_running,omitempty"`
	RebaseAttempted             bool              `json:"rebase_attempted,omitempty"`
	NotifiedCIFail              bool              `json:"notified_ci_fail,omitempty"`           // deprecated: use LastNotifiedStatus
	LastNotifiedStatus          string            `json:"last_notified_status,omitempty"`       // dedup: last notification type sent
	LiveVerificationNotified    bool              `json:"live_verification_notified,omitempty"` // #570 one-shot: hold-for-live-verification board sync + operator notification already fired
	RetryCount                  int               `json:"retry_count,omitempty"`                // per-session retry counter; the global per-issue limit (max_retries_per_issue) combines this with FailedAttemptsForIssue
	NextRetryAt                 *time.Time        `json:"next_retry_at,omitempty"`
	LastOutputHash              string            `json:"last_output_hash,omitempty"`
	LastOutputChangedAt         time.Time         `json:"last_output_changed_at,omitempty"`
	TokensUsedAttempt           int               `json:"tokens_used_attempt,omitempty"`            // tokens consumed in current attempt (reset on respawn)
	TokensUsedTotal             int               `json:"tokens_used_total,omitempty"`              // cumulative tokens across the issue lifecycle
	RateLimitHit                bool              `json:"rate_limit_hit,omitempty"`                 // true if worker was rate-limited (tmux detection, running worker)
	TriedBackends               []string          `json:"tried_backends,omitempty"`                 // backends already attempted (for rate-limit fallback)
	ProviderLimitBackend        string            `json:"provider_limit_backend,omitempty"`         // backend that hit a provider capacity limit
	ProviderLimitReason         string            `json:"provider_limit_reason,omitempty"`          // provider limit signature or class
	ProviderLimitResetAt        *time.Time        `json:"provider_limit_reset_at,omitempty"`        // provider-stated reset time parsed from the limit message ("try again at ..."), UTC
	BackendSelection            *BackendSelection `json:"backend_selection,omitempty"`              // latest backend selection audit record
	Phase                       Phase             `json:"phase,omitempty"`                          // current pipeline phase (empty = legacy single-phase)
	ValidationFails             int               `json:"validation_fails,omitempty"`               // number of failed validation attempts
	ValidationFeedback          string            `json:"validation_feedback,omitempty"`            // feedback from last failed validation
	CIFailureOutput             string            `json:"ci_failure_output,omitempty"`              // CI failure output captured before retry (passed to next worker as context)
	PreviousAttemptFeedback     string            `json:"previous_attempt_feedback,omitempty"`      // feedback from previous failed PR attempt
	PreviousAttemptFeedbackKind string            `json:"previous_attempt_feedback_kind,omitempty"` // review_feedback, rebase_conflict
	RetryReason                 string            `json:"retry_reason,omitempty"`                   // current retry lifecycle reason, e.g. review_feedback
	CheckpointFile              string            `json:"checkpoint_file,omitempty"`                // path to CHECKPOINT.md saved at soft token threshold
	DeploymentFinishedAt        *time.Time        `json:"deployment_finished_at,omitempty"`         // set when the post-merge deploy hook succeeds

	// #513: per-segment attribution timeline. Every spawn / respawn /
	// fallover appends a new entry; the previous entry's EndedAt is
	// closed at the same moment. Records who actually produced the
	// commits (provider/model/variant/effort) so the dashboard +
	// commit trailer can show "first 12m on claude opus-4.8 xhigh,
	// then 4m on codex gpt-5.5 medium after rate-limit fallover".
	Attribution []BackendAttribution `json:"attribution,omitempty"`
}

// BackendAttribution is one segment of a session's backend timeline.
// A session has at least one (the initial spawn) and gains more if the
// rate-limit fallover or pipeline-phase machinery respawns it on a
// different backend.
type BackendAttribution struct {
	Backend   string     `json:"backend"`              // shim name (claude, codex, freellm, opencode, …)
	Provider  string     `json:"provider,omitempty"`   // anthropic, openai, groq, …
	Model     string     `json:"model,omitempty"`      // opus-4.8, gpt-5.5, llama-3.3-70b-versatile, …
	Variant   string     `json:"variant,omitempty"`    // opus[1m], fast, sonnet, …
	Effort    string     `json:"effort,omitempty"`     // xhigh, medium, low, …
	StartedAt time.Time  `json:"started_at"`           // when this segment became active
	EndedAt   *time.Time `json:"ended_at,omitempty"`   // when the segment was closed; nil if still active
	EndReason string     `json:"end_reason,omitempty"` // why the segment closed: "completed", "provider_limit", "fallover", "in_place_respawn", "killed"
	Reason    string     `json:"reason,omitempty"`     // why this segment was started: "initial_spawn", "fallover", "in_place_respawn", "phase_transition"
}

// SessionAttention explains why a session needs operator attention and the
// safest next action Maestro can infer from persisted state.
type SessionAttention struct {
	Reason         string
	NextAction     string
	NeedsAttention bool
}

// SessionAttentionFor returns a concise, state-backed explanation for a session.
// The alive pointer should be provided only when the caller has checked the
// recorded running process.
func SessionAttentionFor(sess *Session, alive *bool) SessionAttention {
	return SessionAttentionForAt(sess, alive, time.Now().UTC())
}

// SessionAttentionForAt is SessionAttentionFor with an explicit clock for tests.
func SessionAttentionForAt(sess *Session, alive *bool, now time.Time) SessionAttention {
	if sess == nil {
		return SessionAttention{}
	}
	if attention, ok := reviewFeedbackRetryAttention(sess, alive, now); ok {
		return attention
	}

	switch sess.Status {
	case StatusRunning:
		if alive != nil && !*alive {
			return SessionAttention{
				Reason:         "State says running, but the worker PID is not alive.",
				NextAction:     "Run a Maestro reconciliation cycle so the session can be marked dead and retried if eligible.",
				NeedsAttention: true,
			}
		}
		if sess.PID == 0 {
			return SessionAttention{
				Reason:         "Worker is marked running, but no PID is recorded.",
				NextAction:     "Run a Maestro reconciliation cycle or inspect the worker before dispatching more work.",
				NeedsAttention: true,
			}
		}
		return SessionAttention{Reason: "Worker process is alive and writing to its session log."}
	case StatusPROpen:
		if sess.PRNumber > 0 {
			return SessionAttention{
				Reason:     fmt.Sprintf("PR #%d is open; Maestro is waiting for CI, Greptile review, or the merge gate.", sess.PRNumber),
				NextAction: "Wait for checks and review gates to pass; Maestro will merge when the merge gate allows it.",
			}
		}
		return SessionAttention{
			Reason:         "Session is waiting on an open PR, but no PR number is recorded yet.",
			NextAction:     "Reconcile the session with the GitHub PR before dispatching duplicate work.",
			NeedsAttention: true,
		}
	case StatusCodeLanded:
		if sess.PRNumber > 0 {
			return SessionAttention{
				Reason:         fmt.Sprintf("PR #%d has merged; code landed, but runtime/deployment verification is still required before the issue can be closed.", sess.PRNumber),
				NextAction:     "Run the required verification or deployment checks, then close the issue only after the outcome is confirmed.",
				NeedsAttention: true,
			}
		}
		return SessionAttention{
			Reason:         "Code landed, but runtime/deployment verification is still required before the issue can be closed.",
			NextAction:     "Run the required verification or deployment checks, then close the issue only after the outcome is confirmed.",
			NeedsAttention: true,
		}
	case StatusQueued:
		return SessionAttention{
			Reason:     "Worker follow-up is queued; Maestro is waiting for CI, Greptile, or the merge gate before merging.",
			NextAction: "Wait for the queued PR checks and merge gate to clear.",
		}
	case StatusDead:
		if sess.NextRetryAt != nil {
			return SessionAttention{
				Reason:         "Worker exited; a retry is scheduled after the current backoff.",
				NextAction:     "Wait for the scheduled retry or inspect the failed attempt if it should not retry.",
				NeedsAttention: true,
			}
		}
		if sess.RateLimitHit {
			backend := sess.ProviderLimitBackend
			if backend == "" {
				backend = sess.Backend
			}
			return SessionAttention{
				Reason:         fmt.Sprintf("Backend %s hit a provider capacity limit; no fallback backend is currently available or allowed.", backend),
				NextAction:     "Wait for provider capacity to recover, enable another backend, or change routing policy before retrying.",
				NeedsAttention: true,
			}
		}
		return SessionAttention{
			Reason:         "Worker exited and is waiting for retry or reconciliation.",
			NextAction:     "Run a Maestro reconciliation cycle or review the failed attempt.",
			NeedsAttention: true,
		}
	case StatusRetryExhausted:
		if sess.PRNumber > 0 {
			if sessionHasFailedCheckEvidence(sess) {
				return SessionAttention{
					Reason:         fmt.Sprintf("Retry limit exhausted after checks failed; PR #%d remains open.", sess.PRNumber),
					NextAction:     "Fix failing checks or retry intentionally before this PR can merge.",
					NeedsAttention: true,
				}
			}
			return SessionAttention{
				Reason:         fmt.Sprintf("Retry limit exhausted with PR #%d still open.", sess.PRNumber),
				NextAction:     "Keep the PR in normal merge flow if checks and review gates pass; otherwise retry intentionally.",
				NeedsAttention: true,
			}
		}
		return SessionAttention{
			Reason:         "Retry limit exhausted before a usable PR was produced.",
			NextAction:     "Review the failed attempts, adjust the issue or retry budget, then restart intentionally.",
			NeedsAttention: true,
		}
	case StatusFailed:
		return SessionAttention{
			Reason:         "Worker failed after the configured retry policy.",
			NextAction:     "Review the failure and restart intentionally when the issue is ready.",
			NeedsAttention: true,
		}
	case StatusConflictFailed:
		return SessionAttention{
			Reason:         "Automatic conflict resolution failed; the branch needs manual rebase/conflict handling.",
			NextAction:     "Rebase or resolve conflicts before retrying or merging.",
			NeedsAttention: true,
		}
	case StatusDone:
		return SessionAttention{Reason: "Issue is complete; PR merged or issue was closed and the session is terminal."}
	default:
		return SessionAttention{Reason: "Session is waiting for the next Maestro reconciliation cycle."}
	}
}

// SessionDisplayStatusFor returns the status token dashboards should display.
func SessionDisplayStatusFor(sess *Session, alive *bool) string {
	return SessionDisplayStatusForAt(sess, alive, time.Now().UTC())
}

// SessionDisplayStatusForAt is SessionDisplayStatusFor with an explicit clock for tests.
func SessionDisplayStatusForAt(sess *Session, alive *bool, now time.Time) string {
	if sess == nil {
		return ""
	}
	if sess.Status == StatusRunning && alive != nil && !*alive {
		return string(sess.Status)
	}
	if display := reviewFeedbackRetryDisplayStatus(sess, now); display != "" {
		return string(display)
	}
	if backendRateLimitedDisplayStatus(sess) {
		return string(DisplayBackendRateLimited)
	}
	return string(sess.Status)
}

// backendRateLimitedDisplayStatus reports whether a session represents a worker
// that exited because its backend hit a provider usage limit with no fallback
// available. Such a session is marked Dead but, unlike a real failure, did not
// burn the per-issue retry budget; it must be shown distinctly so operators do
// not mistake it for retry_exhausted.
func backendRateLimitedDisplayStatus(sess *Session) bool {
	if sess == nil {
		return false
	}
	if sess.Status != StatusDead && sess.Status != StatusRetryExhausted {
		return false
	}
	// A scheduled retry takes precedence: the worker is going to respawn, so it
	// is not blocked on a provider limit even if an earlier attempt was.
	if sess.NextRetryAt != nil {
		return false
	}
	return sess.RateLimitHit && strings.TrimSpace(sess.ProviderLimitBackend) != ""
}

func reviewFeedbackRetryAttention(sess *Session, alive *bool, now time.Time) (SessionAttention, bool) {
	if sess == nil || (sess.Status == StatusRunning && alive != nil && !*alive) {
		return SessionAttention{}, false
	}
	switch reviewFeedbackRetryDisplayStatus(sess, now) {
	case DisplayReviewRetryBackoff:
		return SessionAttention{
			Reason:     "Review feedback retry is scheduled; Maestro is waiting for the retry backoff before starting the in-place retry worker.",
			NextAction: "Wait for the scheduled retry worker to start, or inspect the review feedback if it should not retry.",
		}, true
	case DisplayReviewRetryPending:
		return SessionAttention{
			Reason:     "Review feedback retry is ready; Maestro is waiting for an available retry worker slot.",
			NextAction: "Wait for the retry worker to start in the next orchestration cycle.",
		}, true
	case DisplayReviewRetryRunning:
		return SessionAttention{
			Reason:     "Review feedback retry worker is running; Maestro is updating the existing PR in place.",
			NextAction: "Wait for the retry worker to finish and push updates to the PR.",
		}, true
	case DisplayReviewRetryRecheck:
		return SessionAttention{
			Reason:     "Review feedback retry updated the PR; Maestro is waiting for CI, Greptile, or the merge gate to recheck it.",
			NextAction: "Wait for checks and review gates to pass; Maestro will merge when the merge gate allows it.",
		}, true
	default:
		return SessionAttention{}, false
	}
}

func reviewFeedbackRetryDisplayStatus(sess *Session, now time.Time) SessionDisplayStatus {
	if !hasReviewFeedbackRetry(sess) {
		return ""
	}
	switch sess.Status {
	case StatusDead:
		if sess.NextRetryAt == nil {
			return ""
		}
		if now.IsZero() {
			now = time.Now().UTC()
		}
		if now.Before(*sess.NextRetryAt) {
			return DisplayReviewRetryBackoff
		}
		return DisplayReviewRetryPending
	case StatusRunning:
		return DisplayReviewRetryRunning
	case StatusPROpen, StatusQueued:
		return DisplayReviewRetryRecheck
	default:
		return ""
	}
}

func hasReviewFeedbackRetry(sess *Session) bool {
	if sess == nil {
		return false
	}
	return strings.TrimSpace(sess.RetryReason) == RetryReasonReviewFeedback
}

func sessionHasFailedCheckEvidence(sess *Session) bool {
	if strings.TrimSpace(sess.CIFailureOutput) != "" {
		return true
	}
	return strings.EqualFold(strings.TrimSpace(sess.LastNotifiedStatus), "ci_failure")
}

// UnmarshalJSON implements custom unmarshalling to preserve the legacy
// "tokens_used" field from older state files. Before the split into
// per-attempt and total counters, a single "tokens_used" field tracked
// cumulative token usage. When loading old state, map it to both new fields.
func (s *Session) UnmarshalJSON(data []byte) error {
	// Use an alias to avoid infinite recursion.
	type SessionAlias Session
	aux := &struct {
		*SessionAlias
		LegacyTokensUsed int `json:"tokens_used,omitempty"`
	}{
		SessionAlias: (*SessionAlias)(s),
	}
	if err := json.Unmarshal(data, aux); err != nil {
		return err
	}
	// If legacy field is set and both new fields are zero, migrate.
	if aux.LegacyTokensUsed > 0 && s.TokensUsedAttempt == 0 && s.TokensUsedTotal == 0 {
		s.TokensUsedAttempt = aux.LegacyTokensUsed
		s.TokensUsedTotal = aux.LegacyTokensUsed
	}
	return nil
}

// Mission tracks a decomposed epic and its child issues.
type Mission struct {
	ParentIssue int    `json:"parent_issue"`
	ChildIssues []int  `json:"child_issues"`
	Status      string `json:"status"` // "active", "done"
}

const DefaultSupervisorDecisionLimit = 20

var (
	ErrApprovalNotFound        = errors.New("approval not found")
	ErrApprovalNotPending      = errors.New("approval is not pending")
	ErrApprovalStale           = errors.New("approval is stale")
	ErrApprovalSuperseded      = errors.New("approval is superseded")
	ErrApprovalPayloadMismatch = errors.New("approval payload changed")
	ErrStateConflict           = errors.New("state write conflict")
)

// SupervisorTarget identifies the primary object a supervisor decision refers to.
type SupervisorTarget struct {
	Issue   int    `json:"issue,omitempty"`
	PR      int    `json:"pr,omitempty"`
	Session string `json:"session,omitempty"`
}

// SupervisorProjectState captures the read-only state snapshot behind a decision.
type SupervisorProjectState struct {
	Sessions       int `json:"sessions"`
	Running        int `json:"running"`
	PROpen         int `json:"pr_open"`
	Queued         int `json:"queued"`
	RetryExhausted int `json:"retry_exhausted"`
	OpenIssues     int `json:"open_issues"`
	OpenPRs        int `json:"open_prs"`
	AvailableSlots int `json:"available_slots"`
}

const (
	StuckMissingOutcomeBrief    = "missing_outcome_brief"
	StuckNoOutcomeProgress      = "no_outcome_progress"
	StuckHandoffEpicNeedsChild  = "handoff_epic_needs_child"
	StuckPreflightFailed        = "preflight_failed"
	StuckIssueNeedsVerification = "issue_needs_verification"
	// StuckPolicyBlocksMerge is emitted when an open PR is fully ready to
	// merge (not draft, mergeable, CI green, review gate passed) but the
	// project policy lists merge_pr (or close_issue) in approval_required.
	// The supervisor still emits the merge_pr recommendation (which mints
	// the approval), but the dashboard/CLI needs to see this code so it can
	// show "operator approval required" instead of a passive "monitoring".
	StuckPolicyBlocksMerge = "policy_blocks_merge"
	// StuckPendingChecks is emitted when an open PR is being monitored
	// because at least one gate (draft / mergeable / CI / review) has not
	// yet cleared. Lets the dashboard render a specific "waiting on checks"
	// state instead of conflating it with policy or repair gating.
	StuckPendingChecks = "pending_checks"
)

// SupervisorIssueCandidate describes the issue selected by queue policy without
// exposing issue body content in persisted supervisor state.
type SupervisorIssueCandidate struct {
	Number        int      `json:"number"`
	Title         string   `json:"title,omitempty"`
	Labels        []string `json:"labels,omitempty"`
	PriorityLabel string   `json:"priority_label,omitempty"`
	ProjectStatus string   `json:"project_status,omitempty"`
}

// SupervisorQueueAnalysis captures explainable issue-selection counts for
// Mission Control and --json output.
type SupervisorQueueAnalysis struct {
	PolicyRule                    string                    `json:"policy_rule,omitempty"`
	OpenIssues                    int                       `json:"open_issues"`
	EligibleCandidates            int                       `json:"eligible_candidates"`
	ExcludedIssues                int                       `json:"excluded_issues"`
	HeldIssues                    int                       `json:"held_issues"`
	BlockedByDependencyIssues     int                       `json:"blocked_by_dependency_issues"`
	NonRunnableProjectStatusCount int                       `json:"non_runnable_project_status_count"`
	SelectedCandidate             *SupervisorIssueCandidate `json:"selected_candidate,omitempty"`
	SkippedReasons                []string                  `json:"skipped_reasons,omitempty"`
}

// TopSkippedReason returns the first concise queue skip reason available for UI cards.
func (q *SupervisorQueueAnalysis) TopSkippedReason() string {
	if q == nil {
		return ""
	}
	for _, reason := range q.SkippedReasons {
		if reason = strings.TrimSpace(reason); reason != "" {
			return reason
		}
	}
	return ""
}

// IdleReason summarizes why a supervisor-controlled project has no eligible work.
func (q *SupervisorQueueAnalysis) IdleReason() string {
	if q == nil || q.EligibleCandidates > 0 {
		return ""
	}
	if q.OpenIssues == 0 {
		return "No open issues are available."
	}
	if q.ExcludedIssues >= q.OpenIssues {
		return fmt.Sprintf("Policy excluded all %s.", openIssuePhrase(q.OpenIssues))
	}
	if q.HeldIssues >= q.OpenIssues {
		return fmt.Sprintf("Held/meta policy held all %s.", openIssuePhrase(q.OpenIssues))
	}
	if q.BlockedByDependencyIssues >= q.OpenIssues {
		return fmt.Sprintf("Open dependencies blocked all %s.", openIssuePhrase(q.OpenIssues))
	}
	if q.NonRunnableProjectStatusCount >= q.OpenIssues {
		return fmt.Sprintf("All %s are in a non-runnable project status.", openIssuePhrase(q.OpenIssues))
	}
	if q.classifiedSkipCount() >= q.OpenIssues {
		return fmt.Sprintf("Queue policy classified all %s: %s.", openIssuePhrase(q.OpenIssues), strings.Join(q.skipCategorySummaries(), ", "))
	}
	if reason := q.TopSkippedReason(); reason != "" {
		return "No issue is eligible: " + reason
	}
	return "No issue is eligible under the current supervisor policy."
}

func (q *SupervisorQueueAnalysis) classifiedSkipCount() int {
	if q == nil {
		return 0
	}
	return q.ExcludedIssues + q.HeldIssues + q.BlockedByDependencyIssues + q.NonRunnableProjectStatusCount
}

func (q *SupervisorQueueAnalysis) skipCategorySummaries() []string {
	if q == nil {
		return nil
	}
	categories := []struct {
		label string
		count int
	}{
		{label: "excluded", count: q.ExcludedIssues},
		{label: "held/meta", count: q.HeldIssues},
		{label: "blocked-by-dependency", count: q.BlockedByDependencyIssues},
		{label: "non-runnable project status", count: q.NonRunnableProjectStatusCount},
	}
	summaries := make([]string, 0, len(categories))
	for _, category := range categories {
		if category.count > 0 {
			summaries = append(summaries, fmt.Sprintf("%s=%d", category.label, category.count))
		}
	}
	return summaries
}

func openIssuePhrase(count int) string {
	if count == 1 {
		return "1 open issue"
	}
	return fmt.Sprintf("%d open issues", count)
}

// SupervisorMutation records one durable GitHub mutation planned or attempted by
// the supervisor queue action loop.
type SupervisorMutation struct {
	Type       string `json:"type"`
	Issue      int    `json:"issue,omitempty"`
	Label      string `json:"label,omitempty"`
	Body       string `json:"body,omitempty"`
	Status     string `json:"status"`
	ErrorClass string `json:"error_class,omitempty"`
}

// SupervisorStuckState explains a specific reason Maestro is not progressing.
type SupervisorStuckState struct {
	Code              string            `json:"code"`
	Severity          string            `json:"severity"`
	Summary           string            `json:"summary"`
	Evidence          []string          `json:"evidence,omitempty"`
	RecommendedAction string            `json:"recommended_action"`
	SupervisorCanAct  bool              `json:"supervisor_can_act"`
	Target            *SupervisorTarget `json:"target,omitempty"`
}

// SupervisorDecision is a stable, machine-readable supervisor orchestration record.
type SupervisorDecision struct {
	ID                string                   `json:"id"`
	CreatedAt         time.Time                `json:"created_at"`
	Project           string                   `json:"project"`
	Repo              string                   `json:"repo,omitempty"`
	Mode              string                   `json:"mode"`
	PolicyRule        string                   `json:"policy_rule,omitempty"`
	Status            string                   `json:"status,omitempty"`
	Summary           string                   `json:"summary"`
	RecommendedAction string                   `json:"recommended_action"`
	Target            *SupervisorTarget        `json:"target,omitempty"`
	Risk              string                   `json:"risk"`
	Confidence        float64                  `json:"confidence"`
	ErrorClass        string                   `json:"error_class,omitempty"`
	Reasons           []string                 `json:"reasons,omitempty"`
	RequiresApproval  bool                     `json:"requires_approval"`
	Mutations         []SupervisorMutation     `json:"mutations,omitempty"`
	StuckStates       []SupervisorStuckState   `json:"stuck_states,omitempty"`
	Outcome           *outcome.Status          `json:"outcome,omitempty"`
	ProjectState      SupervisorProjectState   `json:"project_state"`
	QueueAnalysis     *SupervisorQueueAnalysis `json:"queue_analysis,omitempty"`
	ApprovalID        string                   `json:"approval_id,omitempty"`
}

type ApprovalStatus string

const (
	ApprovalStatusPending          ApprovalStatus = "pending"
	ApprovalStatusApproved         ApprovalStatus = "approved"
	ApprovalStatusRejected         ApprovalStatus = "rejected"
	ApprovalStatusStale            ApprovalStatus = "stale"
	ApprovalStatusSuperseded       ApprovalStatus = "superseded"
	ApprovalStatusExecuted         ApprovalStatus = "executed"
	ApprovalStatusExecutionFailed  ApprovalStatus = "execution_failed"
	ApprovalStatusExecutionSkipped ApprovalStatus = "execution_skipped"
	// ApprovalStatusAwaitingDispatch marks an approval that the operator
	// has approved but whose side effect lives on a separate loop
	// (e.g. spawn_worker — the dispatcher tick allocates the slot).
	// Distinct from execution_skipped (which means "no action will
	// happen") so dedup can keep it as a still-effective record.
	ApprovalStatusAwaitingDispatch ApprovalStatus = "awaiting_dispatch"
)

const (
	ApprovalAuditCreated          = "created"
	ApprovalAuditApproved         = "approved"
	ApprovalAuditRejected         = "rejected"
	ApprovalAuditStale            = "stale"
	ApprovalAuditSuperseded       = "superseded"
	ApprovalAuditExecuted         = "executed"
	ApprovalAuditExecutionFailed  = "execution_failed"
	ApprovalAuditExecutionSkipped = "execution_skipped"
	ApprovalAuditAwaitingDispatch = "awaiting_dispatch"
)

const approvalActionSpawnWorker = "spawn_worker"

// Approval records a risky supervisor decision that needs explicit resolution.
type Approval struct {
	ID              string            `json:"id"`
	DecisionID      string            `json:"decision_id,omitempty"`
	CreatedAt       time.Time         `json:"created_at"`
	UpdatedAt       time.Time         `json:"updated_at,omitempty"`
	Action          string            `json:"action"`
	Target          *SupervisorTarget `json:"target,omitempty"`
	Summary         string            `json:"summary"`
	Risk            string            `json:"risk"`
	Evidence        []string          `json:"evidence,omitempty"`
	Status          ApprovalStatus    `json:"status"`
	PayloadHash     string            `json:"payload_hash"`
	TargetStateHash string            `json:"target_state_hash,omitempty"`
	Audit           []ApprovalAudit   `json:"audit,omitempty"`
	// Repo + Project bind the approval to a specific project context at
	// write-time (#489). The executor refuses any approval whose Repo
	// does not match the executor's cfg.Repo, defending against
	// cross-project mutation if Executor wiring drifts in the future.
	// Both omitempty for back-compat with approvals created before #489.
	Repo    string `json:"repo,omitempty"`
	Project string `json:"project,omitempty"`
}

type ApprovalAudit struct {
	At              time.Time `json:"at"`
	Event           string    `json:"event"`
	Actor           string    `json:"actor,omitempty"`
	Reason          string    `json:"reason,omitempty"`
	PayloadHash     string    `json:"payload_hash,omitempty"`
	TargetStateHash string    `json:"target_state_hash,omitempty"`
}

type State struct {
	Sessions            map[string]*Session        `json:"sessions"`
	Missions            map[int]*Mission           `json:"missions,omitempty"` // parent issue number → mission
	SupervisorDecisions []SupervisorDecision       `json:"supervisor_decisions,omitempty"`
	Approvals           []Approval                 `json:"approvals,omitempty"`
	OutcomeHealth       *outcome.HealthCheckResult `json:"outcome_health,omitempty"`
	BackendHealth       map[string]BackendHealth   `json:"backend_health,omitempty"`
	ProjectStatusSync   map[int]ProjectStatusSync  `json:"project_status_sync,omitempty"`
	NextSlot            int                        `json:"next_slot"`
	LastMergeAt         time.Time                  `json:"last_merge_at,omitempty"`

	// RestartRequired is set by the running orchestrator when a config field that
	// cannot be hot-applied (model.default, routing.*) changes during a reload. It is
	// surfaced by `maestro status` and the Fleet API so an operator sees the pending
	// restart without grepping the daemon journal. RestartRequiredReason explains why.
	RestartRequired       bool   `json:"restart_required,omitempty"`
	RestartRequiredReason string `json:"restart_required_reason,omitempty"`

	// LastRunOnceAt is stamped by supervisor.RunOnce on every successful
	// cycle (just before state.Save). The supervise daemon's watchdog
	// reads this from state on its own ticker and emits a loud warning
	// + sets SupervisorStuck=true if the gap is older than 3*interval.
	// Used by Fleet API and by the systemd journal watcher to detect
	// silent supervise-loop wedges (#499). Zero value means "no cycle
	// has run since the daemon started" — handled by the watchdog's
	// startup grace.
	LastRunOnceAt time.Time `json:"last_run_once_at,omitempty"`

	// SupervisorStuck is set by the watchdog when LastRunOnceAt is
	// older than 3*interval. Cleared on any successful RunOnce that
	// stamps LastRunOnceAt fresh.
	SupervisorStuck       bool   `json:"supervisor_stuck,omitempty"`
	SupervisorStuckReason string `json:"supervisor_stuck_reason,omitempty"`

	// SpawnDrain requests a graceful drain of the run loop (#541): while it
	// is set, the orchestrator refuses to claim new issues or spawn new
	// workers but lets in-flight workers finish. It is set by `maestro
	// drain` and cleared automatically when the orchestrator starts, so a
	// drain never persists across a legitimate restart. SpawnDrainAt records
	// when the drain was requested (UTC).
	SpawnDrain   bool      `json:"spawn_drain,omitempty"`
	SpawnDrainAt time.Time `json:"spawn_drain_at,omitempty"`

	loadedHash  string
	loadedState *State
}

type ProjectStatusSync struct {
	Status   string    `json:"status"`
	SyncedAt time.Time `json:"synced_at,omitempty"`
}

func NewState() *State {
	return &State{
		Sessions:          make(map[string]*Session),
		Missions:          make(map[int]*Mission),
		ProjectStatusSync: make(map[int]ProjectStatusSync),
		BackendHealth:     make(map[string]BackendHealth),
		NextSlot:          1,
	}
}

func (s *State) ProjectStatusSynced(issueNumber int, status string) bool {
	if s == nil || issueNumber <= 0 || strings.TrimSpace(status) == "" {
		return false
	}
	record, ok := s.ProjectStatusSync[issueNumber]
	return ok && record.Status == status
}

func (s *State) MarkProjectStatusSynced(issueNumber int, status string, syncedAt time.Time) {
	if s == nil || issueNumber <= 0 || strings.TrimSpace(status) == "" {
		return
	}
	if s.ProjectStatusSync == nil {
		s.ProjectStatusSync = make(map[int]ProjectStatusSync)
	}
	s.ProjectStatusSync[issueNumber] = ProjectStatusSync{
		Status:   status,
		SyncedAt: syncedAt.UTC(),
	}
}

func StatePath(stateDir string) string {
	return filepath.Join(stateDir, "state.json")
}

func LogDir(stateDir string) string {
	return filepath.Join(stateDir, "logs")
}

func Load(stateDir string) (*State, error) {
	path := StatePath(stateDir)
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		s := NewState()
		s.rememberLoaded(nil)
		return s, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read state: %w", err)
	}

	s := NewState()
	if err := json.Unmarshal(data, s); err != nil {
		return nil, fmt.Errorf("parse state: %w", err)
	}
	s.normalize()
	s.rememberLoaded(data)
	return s, nil
}

func Save(stateDir string, s *State) error {
	if err := os.MkdirAll(stateDir, 0755); err != nil {
		return fmt.Errorf("create state dir: %w", err)
	}
	unlock, err := lockState(stateDir)
	if err != nil {
		return err
	}
	defer unlock()

	return saveLocked(stateDir, s)
}

func saveLocked(stateDir string, s *State) error {
	if s == nil {
		s = NewState()
	}
	s.normalize()

	path := StatePath(stateDir)
	current, currentData, err := readStateFile(path)
	if err != nil {
		return err
	}
	currentHash := hashBytes(currentData)
	desired := s
	if currentHash != s.loadedHash {
		base := s.loadedState
		if base == nil {
			base = NewState()
		}
		merged, err := mergeStateSnapshots(base, current, s)
		if err != nil {
			return err
		}
		desired = merged
	}
	desired.ReconcileSpawnWorkerApprovalsForStartedWorkers(time.Now().UTC())

	data, err := json.MarshalIndent(desired, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal state: %w", err)
	}

	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return fmt.Errorf("write temp state: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("atomic rename state: %w", err)
	}
	if desired != s {
		s.copyFrom(desired)
	}
	s.rememberLoaded(data)
	return nil
}

func lockState(stateDir string) (func(), error) {
	lockPath := filepath.Join(stateDir, ".state.lock")
	stateLock := flock.New(lockPath)
	if err := stateLock.Lock(); err != nil {
		return nil, fmt.Errorf("lock state: %w", err)
	}
	return func() {
		_ = stateLock.Unlock()
	}, nil
}

func readStateFile(path string) (*State, []byte, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return NewState(), nil, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("read state: %w", err)
	}
	s := NewState()
	if err := json.Unmarshal(data, s); err != nil {
		return nil, nil, fmt.Errorf("parse state: %w", err)
	}
	s.normalize()
	return s, data, nil
}

func (s *State) rememberLoaded(data []byte) {
	s.loadedHash = hashBytes(data)
	s.loadedState = cloneState(s)
}

func hashBytes(data []byte) string {
	if data == nil {
		return ""
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func (s *State) normalize() {
	if s.Sessions == nil {
		s.Sessions = make(map[string]*Session)
	}
	if s.Missions == nil {
		s.Missions = make(map[int]*Mission)
	}
	if s.ProjectStatusSync == nil {
		s.ProjectStatusSync = make(map[int]ProjectStatusSync)
	}
	if s.BackendHealth == nil {
		s.BackendHealth = make(map[string]BackendHealth)
	}
	if s.NextSlot == 0 {
		s.NextSlot = 1
	}
}

func (s *State) copyFrom(src *State) {
	s.Sessions = src.Sessions
	s.Missions = src.Missions
	s.SupervisorDecisions = src.SupervisorDecisions
	s.Approvals = src.Approvals
	s.OutcomeHealth = src.OutcomeHealth
	s.ProjectStatusSync = src.ProjectStatusSync
	s.BackendHealth = src.BackendHealth
	s.NextSlot = src.NextSlot
	s.LastMergeAt = src.LastMergeAt
	s.SpawnDrain = src.SpawnDrain
	s.SpawnDrainAt = src.SpawnDrainAt
}

func cloneState(s *State) *State {
	if s == nil {
		return NewState()
	}
	data, err := json.Marshal(s)
	if err != nil {
		return NewState()
	}
	clone := NewState()
	if err := json.Unmarshal(data, clone); err != nil {
		return NewState()
	}
	clone.normalize()
	return clone
}

func mergeStateSnapshots(base, current, ours *State) (*State, error) {
	base = cloneState(base)
	current = cloneState(current)
	ours = cloneState(ours)
	merged := cloneState(current)

	if err := mergeSessions(merged, base, current, ours); err != nil {
		return nil, err
	}
	if err := mergeMissions(merged, base, current, ours); err != nil {
		return nil, err
	}
	if err := mergeApprovals(merged, base, current, ours); err != nil {
		return nil, err
	}
	if err := mergeSupervisorDecisions(merged, base, current, ours); err != nil {
		return nil, err
	}
	merged.OutcomeHealth = mergeOutcomeHealth(base.OutcomeHealth, current.OutcomeHealth, ours.OutcomeHealth)
	merged.ProjectStatusSync = mergeProjectStatusSync(current.ProjectStatusSync, ours.ProjectStatusSync)
	merged.BackendHealth = mergeBackendHealth(current.BackendHealth, ours.BackendHealth)
	merged.NextSlot = mergeMonotonicInt(base.NextSlot, current.NextSlot, ours.NextSlot)
	merged.LastMergeAt = mergeLatestTime(base.LastMergeAt, current.LastMergeAt, ours.LastMergeAt)
	mergeSpawnDrain(merged, current, ours)
	return merged, nil
}

// mergeSpawnDrain resolves the drain flag (#541) latest-write-wins by
// SpawnDrainAt. The drain CLI sets SpawnDrain=true and the orchestrator clears
// it (SpawnDrain=false) — both stamp SpawnDrainAt — so picking the snapshot
// with the newer timestamp keeps a concurrent orchestrator save from clobbering
// a fresh drain request, and a fresh clear from being undone by a stale set.
func mergeSpawnDrain(merged, current, ours *State) {
	if ours.SpawnDrainAt.After(current.SpawnDrainAt) {
		merged.SpawnDrain = ours.SpawnDrain
		merged.SpawnDrainAt = ours.SpawnDrainAt
		return
	}
	merged.SpawnDrain = current.SpawnDrain
	merged.SpawnDrainAt = current.SpawnDrainAt
}

func mergeProjectStatusSync(current, ours map[int]ProjectStatusSync) map[int]ProjectStatusSync {
	merged := make(map[int]ProjectStatusSync)
	for _, key := range unionProjectStatusSyncKeys(current, ours) {
		currentValue, currentOK := current[key]
		oursValue, oursOK := ours[key]
		switch {
		case currentOK && oursOK:
			if oursValue.SyncedAt.After(currentValue.SyncedAt) {
				merged[key] = oursValue
			} else {
				merged[key] = currentValue
			}
		case currentOK:
			merged[key] = currentValue
		case oursOK:
			merged[key] = oursValue
		}
	}
	return merged
}

func mergeBackendHealth(current, ours map[string]BackendHealth) map[string]BackendHealth {
	merged := make(map[string]BackendHealth)
	for _, key := range unionBackendHealthKeys(current, ours) {
		currentValue, currentOK := current[key]
		oursValue, oursOK := ours[key]
		switch {
		case currentOK && oursOK:
			if oursValue.Since.After(currentValue.Since) {
				merged[key] = oursValue
			} else {
				merged[key] = currentValue
			}
		case currentOK:
			merged[key] = currentValue
		case oursOK:
			merged[key] = oursValue
		}
	}
	return merged
}

func unionBackendHealthKeys(maps ...map[string]BackendHealth) []string {
	seen := make(map[string]struct{})
	for _, m := range maps {
		for k := range m {
			seen[k] = struct{}{}
		}
	}
	keys := make([]string, 0, len(seen))
	for k := range seen {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func unionProjectStatusSyncKeys(maps ...map[int]ProjectStatusSync) []int {
	seen := make(map[int]struct{})
	for _, m := range maps {
		for k := range m {
			seen[k] = struct{}{}
		}
	}
	keys := make([]int, 0, len(seen))
	for k := range seen {
		keys = append(keys, k)
	}
	sort.Ints(keys)
	return keys
}

func mergeSessions(merged, base, current, ours *State) error {
	for _, key := range unionKeys(base.Sessions, current.Sessions, ours.Sessions) {
		baseValue := base.Sessions[key]
		currentValue := current.Sessions[key]
		oursValue := ours.Sessions[key]
		resolved, keep, err := resolveSnapshotValue("session "+key, baseValue, currentValue, oursValue)
		if err != nil {
			return err
		}
		if keep {
			merged.Sessions[key] = resolved.(*Session)
		} else {
			delete(merged.Sessions, key)
		}
	}
	return nil
}

func mergeMissions(merged, base, current, ours *State) error {
	for _, key := range unionIntKeys(base.Missions, current.Missions, ours.Missions) {
		baseValue := base.Missions[key]
		currentValue := current.Missions[key]
		oursValue := ours.Missions[key]
		resolved, keep, err := resolveSnapshotValue(fmt.Sprintf("mission %d", key), baseValue, currentValue, oursValue)
		if err != nil {
			return err
		}
		if keep {
			merged.Missions[key] = resolved.(*Mission)
		} else {
			delete(merged.Missions, key)
		}
	}
	return nil
}

func mergeApprovals(merged, base, current, ours *State) error {
	merged.Approvals = cloneState(current).Approvals
	keys := unionStringSets(approvalKeys(base.Approvals), approvalKeys(current.Approvals), approvalKeys(ours.Approvals))
	for _, key := range keys {
		baseValue, baseOK := approvalByKey(base.Approvals, key)
		currentValue, currentOK := approvalByKey(current.Approvals, key)
		oursValue, oursOK := approvalByKey(ours.Approvals, key)
		resolved, keep, err := resolveListValue("approval "+key, baseValue, baseOK, currentValue, currentOK, oursValue, oursOK)
		if err != nil {
			return err
		}
		if keep {
			merged.Approvals = upsertApproval(merged.Approvals, resolved.(Approval))
		} else {
			merged.Approvals = deleteApproval(merged.Approvals, key)
		}
	}
	return nil
}

func mergeSupervisorDecisions(merged, base, current, ours *State) error {
	merged.SupervisorDecisions = cloneState(current).SupervisorDecisions
	keys := unionStringSets(decisionKeys(base.SupervisorDecisions), decisionKeys(current.SupervisorDecisions), decisionKeys(ours.SupervisorDecisions))
	for _, key := range keys {
		baseValue, baseOK := decisionByKey(base.SupervisorDecisions, key)
		currentValue, currentOK := decisionByKey(current.SupervisorDecisions, key)
		oursValue, oursOK := decisionByKey(ours.SupervisorDecisions, key)
		resolved, keep, err := resolveListValue("supervisor decision "+key, baseValue, baseOK, currentValue, currentOK, oursValue, oursOK)
		if err != nil {
			return err
		}
		if keep {
			merged.SupervisorDecisions = upsertDecision(merged.SupervisorDecisions, resolved.(SupervisorDecision))
		} else {
			merged.SupervisorDecisions = deleteDecision(merged.SupervisorDecisions, key)
		}
	}
	if len(merged.SupervisorDecisions) > DefaultSupervisorDecisionLimit {
		merged.SupervisorDecisions = append([]SupervisorDecision(nil), merged.SupervisorDecisions[len(merged.SupervisorDecisions)-DefaultSupervisorDecisionLimit:]...)
	}
	return nil
}

func resolveSnapshotValue(name string, baseValue, currentValue, oursValue interface{}) (interface{}, bool, error) {
	baseOK := !jsonEqual(baseValue, nil)
	currentOK := !jsonEqual(currentValue, nil)
	oursOK := !jsonEqual(oursValue, nil)
	return resolveListValue(name, baseValue, baseOK, currentValue, currentOK, oursValue, oursOK)
}

func resolveListValue(name string, baseValue interface{}, baseOK bool, currentValue interface{}, currentOK bool, oursValue interface{}, oursOK bool) (interface{}, bool, error) {
	oursChanged := baseOK != oursOK || !jsonEqual(baseValue, oursValue)
	currentChanged := baseOK != currentOK || !jsonEqual(baseValue, currentValue)
	switch {
	case !oursChanged:
		return currentValue, currentOK, nil
	case !currentChanged:
		return oursValue, oursOK, nil
	case currentOK == oursOK && jsonEqual(currentValue, oursValue):
		return currentValue, currentOK, nil
	default:
		return nil, false, fmt.Errorf("%w: %s changed concurrently", ErrStateConflict, name)
	}
}

func jsonEqual(a, b interface{}) bool {
	return stableHash(a) == stableHash(b)
}

func mergeMonotonicInt(base, current, ours int) int {
	if ours == base {
		return current
	}
	if current == base {
		return ours
	}
	if ours > current {
		return ours
	}
	return current
}

func mergeLatestTime(base, current, ours time.Time) time.Time {
	if ours.Equal(base) {
		return current
	}
	if current.Equal(base) || ours.After(current) {
		return ours
	}
	return current
}

func mergeOutcomeHealth(base, current, ours *outcome.HealthCheckResult) *outcome.HealthCheckResult {
	candidate := latestOutcomeHealth(current, ours)
	if candidate != nil {
		return candidate
	}
	return cloneOutcomeHealth(base)
}

func latestOutcomeHealth(values ...*outcome.HealthCheckResult) *outcome.HealthCheckResult {
	var latest *outcome.HealthCheckResult
	for _, value := range values {
		if value == nil || value.CheckedAt.IsZero() {
			continue
		}
		if latest == nil || value.CheckedAt.After(latest.CheckedAt) {
			latest = value
		}
	}
	return cloneOutcomeHealth(latest)
}

func cloneOutcomeHealth(value *outcome.HealthCheckResult) *outcome.HealthCheckResult {
	if value == nil {
		return nil
	}
	clone := *value
	return &clone
}

func unionKeys(maps ...map[string]*Session) []string {
	seen := make(map[string]bool)
	for _, m := range maps {
		for key := range m {
			seen[key] = true
		}
	}
	return sortedStringKeys(seen)
}

func unionIntKeys(maps ...map[int]*Mission) []int {
	seen := make(map[int]bool)
	for _, m := range maps {
		for key := range m {
			seen[key] = true
		}
	}
	keys := make([]int, 0, len(seen))
	for key := range seen {
		keys = append(keys, key)
	}
	sort.Ints(keys)
	return keys
}

func unionStringSets(sets ...map[string]bool) []string {
	seen := make(map[string]bool)
	for _, set := range sets {
		for key := range set {
			seen[key] = true
		}
	}
	return sortedStringKeys(seen)
}

func sortedStringKeys(seen map[string]bool) []string {
	keys := make([]string, 0, len(seen))
	for key := range seen {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func approvalKeys(approvals []Approval) map[string]bool {
	keys := make(map[string]bool)
	for _, approval := range approvals {
		keys[approvalKey(approval)] = true
	}
	return keys
}

func approvalByKey(approvals []Approval, key string) (Approval, bool) {
	for _, approval := range approvals {
		if approvalKey(approval) == key {
			return approval, true
		}
	}
	return Approval{}, false
}

func approvalKey(approval Approval) string {
	if approval.ID != "" {
		return approval.ID
	}
	if approval.DecisionID != "" {
		return "decision:" + approval.DecisionID
	}
	return stableHash(approval)
}

func upsertApproval(approvals []Approval, approval Approval) []Approval {
	key := approvalKey(approval)
	for i := range approvals {
		if approvalKey(approvals[i]) == key {
			approvals[i] = approval
			return approvals
		}
	}
	return append(approvals, approval)
}

func deleteApproval(approvals []Approval, key string) []Approval {
	filtered := approvals[:0]
	for _, approval := range approvals {
		if approvalKey(approval) != key {
			filtered = append(filtered, approval)
		}
	}
	return filtered
}

func decisionKeys(decisions []SupervisorDecision) map[string]bool {
	keys := make(map[string]bool)
	for _, decision := range decisions {
		keys[decisionKey(decision)] = true
	}
	return keys
}

func decisionByKey(decisions []SupervisorDecision, key string) (SupervisorDecision, bool) {
	for _, decision := range decisions {
		if decisionKey(decision) == key {
			return decision, true
		}
	}
	return SupervisorDecision{}, false
}

func decisionKey(decision SupervisorDecision) string {
	if decision.ID != "" {
		return decision.ID
	}
	return stableHash(decision)
}

func upsertDecision(decisions []SupervisorDecision, decision SupervisorDecision) []SupervisorDecision {
	key := decisionKey(decision)
	for i := range decisions {
		if decisionKey(decisions[i]) == key {
			decisions[i] = decision
			return decisions
		}
	}
	return append(decisions, decision)
}

func deleteDecision(decisions []SupervisorDecision, key string) []SupervisorDecision {
	filtered := decisions[:0]
	for _, decision := range decisions {
		if decisionKey(decision) != key {
			filtered = append(filtered, decision)
		}
	}
	return filtered
}

// NextSlotName returns "{prefix}-N" for the next available slot
func (s *State) NextSlotName(prefix string) string {
	name := fmt.Sprintf("%s-%d", prefix, s.NextSlot)
	s.NextSlot++
	return name
}

// RecordSupervisorDecision appends a supervisor decision and keeps only recent records.
func (s *State) RecordSupervisorDecision(decision SupervisorDecision, limit int) {
	if limit <= 0 {
		limit = DefaultSupervisorDecisionLimit
	}
	s.SupervisorDecisions = append(s.SupervisorDecisions, decision)
	if len(s.SupervisorDecisions) > limit {
		s.SupervisorDecisions = append([]SupervisorDecision(nil), s.SupervisorDecisions[len(s.SupervisorDecisions)-limit:]...)
	}
}

// LatestSupervisorDecision returns the newest supervisor decision by creation time.
func (s *State) LatestSupervisorDecision() *SupervisorDecision {
	if len(s.SupervisorDecisions) == 0 {
		return nil
	}
	latest := 0
	for i := 1; i < len(s.SupervisorDecisions); i++ {
		if s.SupervisorDecisions[i].CreatedAt.After(s.SupervisorDecisions[latest].CreatedAt) {
			latest = i
		}
	}
	return &s.SupervisorDecisions[latest]
}

// RecordPendingApprovalForDecision creates a pending approval tied to a decision payload.
//
// Dedup contract (added 2026-05-31, fixes the #471 approvals storm where the
// same (action, target) was minted 56 times in 12 hours):
//
//   - If an existing PENDING approval matches the same (Action, Target) AND
//     the target snapshot (TargetStateHash) is unchanged, return the
//     existing approval and do NOT append a new one. This is the storm
//     case: supervisor recommended the same action again, but nothing
//     downstream changed, so a duplicate would only add noise.
//
//   - If an existing PENDING approval matches the same (Action, Target) but
//     the TargetStateHash has changed (e.g. session moved from running to
//     dead, PR opened, retry count bumped), the existing approval is marked
//     superseded and a new one is created. This is a legitimate re-emit
//     against fresh state.
//
//   - If no existing PENDING approval matches, create new (legacy
//     behaviour preserved).
func (s *State) RecordPendingApprovalForDecision(decision SupervisorDecision, now time.Time) *Approval {
	if s == nil {
		return nil
	}
	// #490: refuse malformed Target.Session at the state-write boundary.
	// Otherwise a malformed slot from the supervisor LLM lands in state,
	// is approved by an unsuspecting operator, and only the executor's
	// WorktreePathForSlot catches it — too late for forensics. Validators
	// live at every ingress.
	if decision.Target != nil {
		if sess := strings.TrimSpace(decision.Target.Session); sess != "" {
			if err := ValidateSlotID(sess); err != nil {
				return nil
			}
		}
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	createdAt := decision.CreatedAt
	if createdAt.IsZero() {
		createdAt = now
	}

	freshTargetStateHash := s.ApprovalTargetStateHash(decision.Target)

	// Dedup: scan existing pending approvals for the same (Action, Target).
	// Only PENDING records are considered — terminal-state approvals are
	// out of scope and never block a re-emit.
	var existingMatch *Approval
	for i := range s.Approvals {
		candidate := &s.Approvals[i]
		// #515: also coalesce against awaiting_dispatch — operator
		// approved an earlier mint, the side-effect (spawn) is still
		// in flight on the dispatcher loop, supervisor must not mint
		// a fresh pending for the same target in this race window.
		if candidate.Status != ApprovalStatusPending && candidate.Status != ApprovalStatusAwaitingDispatch {
			continue
		}
		if candidate.Action != decision.RecommendedAction {
			continue
		}
		if !approvalTargetsEqual(candidate.Target, decision.Target) {
			continue
		}
		existingMatch = candidate
		break
	}

	if existingMatch != nil {
		if existingMatch.TargetStateHash == freshTargetStateHash {
			// Same (action, target, target-state) — the storm case.
			// Return the existing approval untouched.
			return existingMatch
		}
		// Same (action, target) but state has moved on — supersede the
		// stale record and fall through to create a fresh one.
		s.markApprovalSuperseded(existingMatch, now,
			"superseded by fresh re-emit: target state changed before approval")
	}

	approval := Approval{
		ID:              approvalID(decision, createdAt),
		DecisionID:      decision.ID,
		CreatedAt:       createdAt,
		UpdatedAt:       now,
		Action:          decision.RecommendedAction,
		Target:          cloneSupervisorTarget(decision.Target),
		Summary:         decision.Summary,
		Risk:            decision.Risk,
		Evidence:        append([]string(nil), decision.Reasons...),
		Status:          ApprovalStatusPending,
		TargetStateHash: freshTargetStateHash,
		Repo:            decision.Repo,
		Project:         decision.Project,
	}
	approval.PayloadHash = approval.ComputePayloadHash()
	approval.Audit = append(approval.Audit, ApprovalAudit{
		At:              now,
		Event:           ApprovalAuditCreated,
		PayloadHash:     approval.PayloadHash,
		TargetStateHash: approval.TargetStateHash,
	})
	s.Approvals = append(s.Approvals, approval)
	return &s.Approvals[len(s.Approvals)-1]
}

// approvalTargetsEqual returns true if two SupervisorTarget pointers refer
// to the same target identity (issue/pr/session). Used by the at-mint
// dedup check in RecordPendingApprovalForDecision.
func approvalTargetsEqual(a, b *SupervisorTarget) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	if a.Issue != b.Issue {
		return false
	}
	if a.PR != b.PR {
		return false
	}
	if strings.TrimSpace(a.Session) != strings.TrimSpace(b.Session) {
		return false
	}
	return true
}

func (s *State) FindApproval(id string) (*Approval, bool) {
	for i := range s.Approvals {
		approval := &s.Approvals[i]
		if approval.ID == id || approval.DecisionID == id {
			return approval, true
		}
	}
	return nil, false
}

func (s *State) ApproveApproval(id string, now time.Time, actor, reason string) (*Approval, error) {
	approval, err := s.pendingApproval(id)
	if err != nil {
		return approval, err
	}
	if err := s.ensureApprovalCurrent(approval, now); err != nil {
		return approval, err
	}
	approval.Status = ApprovalStatusApproved
	approval.UpdatedAt = normalizedTime(now)
	approval.Audit = append(approval.Audit, ApprovalAudit{
		At:              approval.UpdatedAt,
		Event:           ApprovalAuditApproved,
		Actor:           actor,
		Reason:          reason,
		PayloadHash:     approval.PayloadHash,
		TargetStateHash: approval.TargetStateHash,
	})
	return approval, nil
}

func (s *State) RejectApproval(id string, now time.Time, actor, reason string) (*Approval, error) {
	approval, err := s.pendingApproval(id)
	if err != nil {
		return approval, err
	}
	approval.Status = ApprovalStatusRejected
	approval.UpdatedAt = normalizedTime(now)
	approval.Audit = append(approval.Audit, ApprovalAudit{
		At:              approval.UpdatedAt,
		Event:           ApprovalAuditRejected,
		Actor:           actor,
		Reason:          reason,
		PayloadHash:     approval.PayloadHash,
		TargetStateHash: approval.TargetStateHash,
	})
	return approval, nil
}

// MarkStaleApprovals marks pending approvals stale when their payload or target snapshot changes.
func (s *State) MarkStaleApprovals(now time.Time) int {
	count := 0
	for i := range s.Approvals {
		approval := &s.Approvals[i]
		if approval.Status != ApprovalStatusPending {
			continue
		}
		if err := s.ensureApprovalCurrent(approval, now); err != nil {
			count++
		}
	}
	return count
}

// ReconcileSpawnWorkerApprovalsForStartedWorkers marks pending spawn_worker approvals
// as historical once a matching worker session has started for the same target.
func (s *State) ReconcileSpawnWorkerApprovalsForStartedWorkers(now time.Time) int {
	if s == nil || len(s.Approvals) == 0 || len(s.Sessions) == 0 {
		return 0
	}
	count := 0
	names := make([]string, 0, len(s.Sessions))
	for name := range s.Sessions {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		count += s.ReconcileSpawnWorkerApprovalsForStartedSession(name, s.Sessions[name], now)
	}
	return count
}

// ReconcileSpawnWorkerApprovalsForStartedSession supersedes pending spawn_worker
// approvals that requested the worker represented by the started session.
func (s *State) ReconcileSpawnWorkerApprovalsForStartedSession(slot string, sess *Session, now time.Time) int {
	if s == nil || sess == nil || sess.IssueNumber <= 0 {
		return 0
	}
	count := 0
	for i := range s.Approvals {
		approval := &s.Approvals[i]
		if !spawnWorkerApprovalMatchesSession(approval, slot, sess) {
			continue
		}
		s.markApprovalSuperseded(approval, now, fmt.Sprintf("worker %s started for issue #%d", slot, sess.IssueNumber))
		count++
	}
	return count
}

func (s *State) pendingApproval(id string) (*Approval, error) {
	approval, ok := s.FindApproval(id)
	if !ok {
		return nil, ErrApprovalNotFound
	}
	if approval.Status == ApprovalStatusStale {
		return approval, ErrApprovalStale
	}
	if approval.Status == ApprovalStatusSuperseded {
		return approval, ErrApprovalSuperseded
	}
	if approval.Status != ApprovalStatusPending {
		return approval, ErrApprovalNotPending
	}
	return approval, nil
}

func (s *State) ensureApprovalCurrent(approval *Approval, now time.Time) error {
	if approval.PayloadHash != "" && approval.ComputePayloadHash() != approval.PayloadHash {
		s.markApprovalStale(approval, now, "approval payload changed")
		return ErrApprovalPayloadMismatch
	}
	currentTargetStateHash := s.ApprovalTargetStateHash(approval.Target)
	if approval.TargetStateHash != "" && currentTargetStateHash != approval.TargetStateHash {
		s.markApprovalStale(approval, now, "approval target state changed")
		return ErrApprovalStale
	}
	return nil
}

func (s *State) markApprovalStale(approval *Approval, now time.Time, reason string) {
	if approval.Status == ApprovalStatusStale {
		return
	}
	approval.Status = ApprovalStatusStale
	approval.UpdatedAt = normalizedTime(now)
	approval.Audit = append(approval.Audit, ApprovalAudit{
		At:              approval.UpdatedAt,
		Event:           ApprovalAuditStale,
		Reason:          reason,
		PayloadHash:     approval.PayloadHash,
		TargetStateHash: s.ApprovalTargetStateHash(approval.Target),
	})
}

func (s *State) markApprovalSuperseded(approval *Approval, now time.Time, reason string) {
	// #515: AwaitingDispatch is "still in flight on a separate loop";
	// reconcile must be allowed to supersede it once the side effect
	// it's awaiting actually lands (e.g. spawn_worker -> worker started).
	if approval.Status != ApprovalStatusPending && approval.Status != ApprovalStatusAwaitingDispatch {
		return
	}
	approval.Status = ApprovalStatusSuperseded
	approval.UpdatedAt = normalizedTime(now)
	approval.Audit = append(approval.Audit, ApprovalAudit{
		At:              approval.UpdatedAt,
		Event:           ApprovalAuditSuperseded,
		Reason:          reason,
		PayloadHash:     approval.PayloadHash,
		TargetStateHash: s.ApprovalTargetStateHash(approval.Target),
	})
}

func spawnWorkerApprovalMatchesSession(approval *Approval, slot string, sess *Session) bool {
	if approval == nil || sess == nil || approval.Action != approvalActionSpawnWorker {
		return false
	}
	// #515: both pending (not yet approved) and awaiting_dispatch (approved
	// but dispatcher hasn't ticked yet) records are still effective; both
	// must be superseded once the worker really starts.
	if approval.Status != ApprovalStatusPending && approval.Status != ApprovalStatusAwaitingDispatch {
		return false
	}
	if approval.Target == nil {
		return false
	}
	target := approval.Target
	matched := false
	if target.Session != "" {
		if target.Session != slot {
			return false
		}
		matched = true
	}
	if target.Issue > 0 {
		if target.Issue != sess.IssueNumber {
			return false
		}
		matched = true
	}
	if target.PR > 0 {
		if target.PR != sess.PRNumber {
			return false
		}
		matched = true
	}
	if !matched {
		return false
	}
	if sess.StartedAt.IsZero() {
		return false
	}
	if approval.CreatedAt.IsZero() {
		return true
	}
	return !sess.StartedAt.Before(approval.CreatedAt.UTC())
}

func (a Approval) ComputePayloadHash() string {
	return stableHash(approvalPayload{
		DecisionID: a.DecisionID,
		Action:     a.Action,
		Target:     a.Target,
		Summary:    a.Summary,
		Risk:       a.Risk,
		Evidence:   a.Evidence,
	})
}

// ApprovalTargetStateHash returns a stable digest of state relevant to a target.
func (s *State) ApprovalTargetStateHash(target *SupervisorTarget) string {
	snapshot := approvalTargetStateSnapshot{Target: cloneSupervisorTarget(target)}
	if s == nil || target == nil {
		return stableHash(snapshot)
	}
	names := make([]string, 0, len(s.Sessions))
	for name := range s.Sessions {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		sess := s.Sessions[name]
		if sess == nil || !approvalTargetMatches(target, name, sess) {
			continue
		}
		snapshot.Sessions = append(snapshot.Sessions, approvalSessionSnapshot{
			Slot:        name,
			IssueNumber: sess.IssueNumber,
			Status:      sess.Status,
			Branch:      sess.Branch,
			PRNumber:    sess.PRNumber,
			FinishedAt:  sess.FinishedAt,
			RetryCount:  sess.RetryCount,
			NextRetryAt: sess.NextRetryAt,
		})
	}
	return stableHash(snapshot)
}

type approvalPayload struct {
	DecisionID string            `json:"decision_id,omitempty"`
	Action     string            `json:"action"`
	Target     *SupervisorTarget `json:"target,omitempty"`
	Summary    string            `json:"summary"`
	Risk       string            `json:"risk"`
	Evidence   []string          `json:"evidence,omitempty"`
}

type approvalTargetStateSnapshot struct {
	Target   *SupervisorTarget         `json:"target,omitempty"`
	Sessions []approvalSessionSnapshot `json:"sessions,omitempty"`
}

type approvalSessionSnapshot struct {
	Slot        string        `json:"slot"`
	IssueNumber int           `json:"issue_number"`
	Status      SessionStatus `json:"status"`
	Branch      string        `json:"branch,omitempty"`
	PRNumber    int           `json:"pr_number,omitempty"`
	FinishedAt  *time.Time    `json:"finished_at,omitempty"`
	RetryCount  int           `json:"retry_count,omitempty"`
	NextRetryAt *time.Time    `json:"next_retry_at,omitempty"`
}

func approvalID(decision SupervisorDecision, createdAt time.Time) string {
	if decision.ID != "" {
		return "approval-" + decision.ID
	}
	return "approval-" + createdAt.UTC().Format("20060102T150405.000000000Z")
}

func approvalTargetMatches(target *SupervisorTarget, slot string, sess *Session) bool {
	if target.Session != "" && target.Session == slot {
		return true
	}
	if target.Issue > 0 && target.Issue == sess.IssueNumber {
		return true
	}
	if target.PR > 0 && target.PR == sess.PRNumber {
		return true
	}
	return false
}

func cloneSupervisorTarget(target *SupervisorTarget) *SupervisorTarget {
	if target == nil {
		return nil
	}
	clone := *target
	return &clone
}

func stableHash(value interface{}) string {
	data, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func normalizedTime(t time.Time) time.Time {
	if t.IsZero() {
		return time.Now().UTC()
	}
	return t.UTC()
}

// ActiveSessions returns sessions that are currently running
func (s *State) ActiveSessions() []*Session {
	var active []*Session
	for _, sess := range s.Sessions {
		if sess.Status == StatusRunning || sess.Status == StatusPROpen {
			active = append(active, sess)
		}
	}
	return active
}

// RunningSessions returns the in-flight worker sessions — those whose tmux
// worker process is actively executing (StatusRunning). Drain (#541) waits for
// this set to empty; pr_open sessions are intentionally excluded because they
// have no live worker and are re-attached by the next supervisor's reconcile
// path after a restart.
func (s *State) RunningSessions() []*Session {
	if s == nil {
		return nil
	}
	var running []*Session
	for _, sess := range s.Sessions {
		if sess != nil && sess.Status == StatusRunning {
			running = append(running, sess)
		}
	}
	return running
}

// RunningSessionCount returns the number of in-flight worker sessions.
func (s *State) RunningSessionCount() int {
	return len(s.RunningSessions())
}

// SetSpawnDrain requests a graceful drain (#541): the run loop stops claiming
// new issues and spawning new workers but lets in-flight workers finish. The
// timestamp is stamped so concurrent state writers resolve drain on/off by
// latest-write-wins.
func (s *State) SetSpawnDrain(at time.Time) {
	if s == nil {
		return
	}
	s.SpawnDrain = true
	s.SpawnDrainAt = normalizedTime(at)
}

// ClearSpawnDrain lifts a graceful drain. The orchestrator calls this on
// startup so a drain never persists across a legitimate restart.
func (s *State) ClearSpawnDrain(at time.Time) {
	if s == nil {
		return
	}
	s.SpawnDrain = false
	s.SpawnDrainAt = normalizedTime(at)
}

// DrainActive reports whether a graceful drain is currently requested.
func (s *State) DrainActive() bool {
	return s != nil && s.SpawnDrain
}

// LiveSessions returns sessions that belong in the default operator view.
func (s *State) LiveSessions() []*Session {
	return s.LiveSessionsAt(time.Now().UTC())
}

// LiveSessionsAt returns sessions that are running, actionable, still in PR or
// retry review flow, or changed within the recent live window.
func (s *State) LiveSessionsAt(now time.Time) []*Session {
	if s == nil {
		return nil
	}
	live := make([]*Session, 0)
	for _, sess := range s.Sessions {
		if SessionLiveAt(sess, now) {
			live = append(live, sess)
		}
	}
	return live
}

// SessionAt returns the live session bound to slot, if any. Used by the
// approver executor (#488 slot-reuse fence) to verify a delete_worktree
// approval still targets the worker that was running when the approval
// was queued.
func (s *State) SessionAt(slot string) (*Session, bool) {
	if s == nil || s.Sessions == nil {
		return nil, false
	}
	sess, ok := s.Sessions[slot]
	if !ok || sess == nil {
		return nil, false
	}
	return sess, true
}

// SessionLive reports whether a session belongs in the default operator view.
func SessionLive(sess *Session) bool {
	return SessionLiveAt(sess, time.Now().UTC())
}

// SessionLiveAt is SessionLive with an explicit clock for tests.
func SessionLiveAt(sess *Session, now time.Time) bool {
	if sess == nil {
		return false
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}

	switch sess.Status {
	case StatusRunning, StatusPROpen, StatusQueued:
		return true
	}

	switch SessionDisplayStatus(SessionDisplayStatusForAt(sess, nil, now)) {
	case DisplayReviewRetryBackoff, DisplayReviewRetryPending, DisplayReviewRetryRunning, DisplayReviewRetryRecheck:
		return true
	}

	if SessionAttentionForAt(sess, nil, now).NeedsAttention {
		return true
	}

	changedAt := SessionChangedAt(sess)
	return !changedAt.IsZero() && now.Sub(changedAt.UTC()) <= LiveSessionRecentWindow
}

// SessionChangedAt returns the newest persisted activity timestamp for a session.
func SessionChangedAt(sess *Session) time.Time {
	if sess == nil {
		return time.Time{}
	}
	changedAt := sess.StartedAt
	if sess.FinishedAt != nil && sess.FinishedAt.After(changedAt) {
		changedAt = *sess.FinishedAt
	}
	if !sess.LastOutputChangedAt.IsZero() && sess.LastOutputChangedAt.After(changedAt) {
		changedAt = sess.LastOutputChangedAt
	}
	return changedAt.UTC()
}

// CountByStatus returns a map of session status → count for all non-terminal sessions.
func (s *State) CountByStatus() map[SessionStatus]int {
	counts := make(map[SessionStatus]int)
	for _, sess := range s.Sessions {
		if !IsTerminal(sess.Status) {
			counts[sess.Status]++
		}
	}
	return counts
}

// DonePRCount counts completed sessions that produced a PR. It is a conservative
// proxy for issue throughput that may still fail to advance the runtime outcome.
func (s *State) DonePRCount() int {
	if s == nil {
		return 0
	}
	count := 0
	for _, sess := range s.Sessions {
		if sess != nil && (sess.Status == StatusDone || sess.Status == StatusCodeLanded) && sess.PRNumber > 0 {
			count++
		}
	}
	return count
}

// IssueInProgress returns true if the given issue is already being handled.
// This includes dead sessions with a pending retry (NextRetryAt set) to prevent
// duplicate worker spawns during backoff periods.
func (s *State) IssueInProgress(issueNum int) bool {
	for _, sess := range s.Sessions {
		if sess.IssueNumber != issueNum {
			continue
		}
		if sess.Status == StatusRunning || sess.Status == StatusPROpen || sess.Status == StatusQueued || sess.Status == StatusCodeLanded {
			return true
		}
		// Dead session with pending retry — still in progress
		if sess.Status == StatusDead && sess.NextRetryAt != nil {
			return true
		}
	}
	return false
}

// IssueDone returns true if the given issue already has a completed session.
func (s *State) IssueDone(issueNum int) bool {
	for _, sess := range s.Sessions {
		if sess.IssueNumber == issueNum && sess.Status == StatusDone {
			return true
		}
	}
	return false
}

// FailedAttemptsForIssue counts sessions for the given issue that ended
// without producing a PR (dead, failed, or retry_exhausted).
//
// Sessions marked as rate-limited (RateLimitHit) are NOT counted as failed
// attempts: a transient backend block must not consume the per-issue retry
// budget. See #432 / #458 / #466.
func (s *State) FailedAttemptsForIssue(issueNum int) int {
	count := 0
	for _, sess := range s.Sessions {
		if sess.IssueNumber == issueNum && sess.PRNumber == 0 &&
			(sess.Status == StatusDead || sess.Status == StatusFailed || sess.Status == StatusRetryExhausted) &&
			!sess.RateLimitHit {
			count++
		}
	}
	return count
}

// IssueRetryExhausted returns true if any session for the given issue
// has been marked as retry_exhausted.
func (s *State) IssueRetryExhausted(issueNum int) bool {
	for _, sess := range s.Sessions {
		if sess.IssueNumber == issueNum && sess.Status == StatusRetryExhausted {
			return true
		}
	}
	return false
}

// MarkIssueRetryExhausted transitions the most recent dead/failed session
// for the given issue to StatusRetryExhausted.
func (s *State) MarkIssueRetryExhausted(issueNum int) {
	var latest *Session
	var latestTime time.Time
	for _, sess := range s.Sessions {
		if sess.IssueNumber == issueNum &&
			(sess.Status == StatusDead || sess.Status == StatusFailed) {
			var t time.Time
			if sess.FinishedAt != nil {
				t = *sess.FinishedAt
			} else {
				t = sess.StartedAt
			}
			if latest == nil || t.After(latestTime) {
				latest = sess
				latestTime = t
			}
		}
	}
	if latest != nil {
		latest.Status = StatusRetryExhausted
	}
}

// IsTerminal returns true if the status represents a completed/dead session.
// StatusPriority returns a sort key for session statuses.
// Lower values sort first. Running sessions appear at the top,
// followed by actionable states, then terminal states.
func StatusPriority(status SessionStatus) int {
	switch status {
	case StatusRunning:
		return 0
	case StatusPROpen:
		return 1
	case StatusQueued:
		return 2
	case StatusCodeLanded:
		return 3
	case StatusDead:
		return 4
	case StatusFailed:
		return 5
	case StatusConflictFailed:
		return 6
	case StatusRetryExhausted:
		return 7
	case StatusDone:
		return 8
	default:
		return 9
	}
}

func IsTerminal(status SessionStatus) bool {
	switch status {
	case StatusDone, StatusFailed, StatusConflictFailed, StatusDead, StatusRetryExhausted:
		return true
	}
	return false
}

// CompletedSession is a Session paired with its slot name.
type CompletedSession struct {
	SlotName string
	*Session
}

// CompletedSessions returns sessions in a terminal state, sorted by FinishedAt descending.
func (s *State) CompletedSessions() []CompletedSession {
	var result []CompletedSession
	for name, sess := range s.Sessions {
		if IsTerminal(sess.Status) {
			result = append(result, CompletedSession{SlotName: name, Session: sess})
		}
	}
	sort.Slice(result, func(i, j int) bool {
		fi, fj := result[i].FinishedAt, result[j].FinishedAt
		if fi == nil && fj == nil {
			return result[i].StartedAt.After(result[j].StartedAt)
		}
		if fi == nil {
			return false
		}
		if fj == nil {
			return true
		}
		return fi.After(*fj)
	})
	return result
}

// IsMissionParent returns true if the given issue number is a mission parent.
func (s *State) IsMissionParent(issueNum int) bool {
	if s.Missions == nil {
		return false
	}
	_, ok := s.Missions[issueNum]
	return ok
}

// IsMissionChild returns true if the given issue number is a child of any mission.
func (s *State) IsMissionChild(issueNum int) bool {
	if s.Missions == nil {
		return false
	}
	for _, m := range s.Missions {
		for _, child := range m.ChildIssues {
			if child == issueNum {
				return true
			}
		}
	}
	return false
}

// PruneOldSessions removes completed sessions older than maxAge.
// Returns the number of pruned sessions.
func (s *State) PruneOldSessions(maxAge time.Duration) int {
	pruned := 0
	for name, sess := range s.Sessions {
		if !IsTerminal(sess.Status) {
			continue
		}
		finished := sess.FinishedAt
		if finished == nil {
			// Fallback: use StartedAt if FinishedAt is not set
			finished = &sess.StartedAt
		}
		if time.Since(*finished) > maxAge {
			delete(s.Sessions, name)
			pruned++
		}
	}
	return pruned
}

// StaleSessionPolicy controls reconciliation of sessions that have been idle
// past their useful lifetime. The policy is intentionally conservative so a
// live worker is never reclassified as stale.
type StaleSessionPolicy struct {
	// Enabled turns on stale-session reconciliation. When false, the helpers
	// in this file always report sessions as live.
	Enabled bool
	// IdleAfter is the minimum time a session must be idle (no state writes,
	// finished/started in the past) before it can be considered stale.
	IdleAfter time.Duration
	// RequireWorktreeMissing demands that the recorded worktree path does not
	// exist on disk before a session is considered stale. Together with the
	// idle window this prevents a live worker that simply has not written
	// state recently from being filtered out.
	RequireWorktreeMissing bool
	// MergedPRDismisses, when true, treats a dead session whose linked PR is
	// known to be MERGED as completed regardless of idle time or worktree
	// presence. PRStateForBranchPR supplies the lookup; when nil, this path
	// has no effect.
	MergedPRDismisses bool
	// PRStateForBranchPR returns the known PR state ("MERGED", "OPEN", ...)
	// for the given session branch name and the candidate session's own
	// PRNumber. The PRNumber match guards against false dismissals when an
	// issue is re-opened: a stale CodeLanded record on the same branch but
	// with a different PRNumber must not poison a live retry. An empty
	// return value means unknown.
	PRStateForBranchPR func(branch string, prNumber int) string
}

// MergedPRReason is the audit reason emitted when a session is dismissed
// because its linked PR is known to be merged.
const MergedPRReason = "linked PR merged"

// StaleSessionAudit records a single stale-session reconciliation event.
// Callers persist this through a project's audit log; it is not stored in
// state.json so historical records remain even if state is rewritten.
type StaleSessionAudit struct {
	Slot        string    `json:"slot"`
	IssueNumber int       `json:"issue_number,omitempty"`
	PRNumber    int       `json:"pr_number,omitempty"`
	Status      string    `json:"status,omitempty"`
	Reason      string    `json:"reason"`
	IdleSeconds int64     `json:"idle_seconds"`
	At          time.Time `json:"at"`
}

// SessionStale reports whether a session should be filtered from operator
// attention because the recorded worker has clearly stopped progressing.
// The optional worktreeExists callback lets callers inject filesystem checks
// without leaking os.Stat into the state package boundary.
func SessionStale(sess *Session, now time.Time, policy StaleSessionPolicy, worktreeExists func(string) bool) (StaleSessionAudit, bool) {
	if sess == nil || !policy.Enabled {
		return StaleSessionAudit{}, false
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}

	// Live workers and waiting-for-CI sessions are never stale: they are
	// actively progressing or expected to progress without operator help.
	switch sess.Status {
	case StatusRunning, StatusPROpen, StatusQueued, StatusCodeLanded, StatusDone:
		return StaleSessionAudit{}, false
	}
	// Sessions with a scheduled retry are still "in flight" by policy.
	if sess.NextRetryAt != nil && now.Before(sess.NextRetryAt.UTC()) {
		return StaleSessionAudit{}, false
	}

	changedAt := SessionChangedAt(sess)
	idle := time.Duration(0)
	if !changedAt.IsZero() {
		idle = now.Sub(changedAt)
	}

	// Linked-PR-merged path: when enabled, a dead session whose own PR is
	// known to be MERGED is dismissed regardless of idle time or worktree
	// presence. The lookup must match on (branch, PRNumber) so a stale
	// CodeLanded record on the same branch but for a different PR (e.g.
	// after the issue was re-opened) cannot poison a live retry. The audit
	// carries an explicit "linked PR merged" reason so operators can see
	// which condition fired.
	if policy.MergedPRDismisses && policy.PRStateForBranchPR != nil && sess.PRNumber > 0 {
		branch := strings.TrimSpace(sess.Branch)
		if branch != "" {
			if state := strings.ToUpper(strings.TrimSpace(policy.PRStateForBranchPR(branch, sess.PRNumber))); state == "MERGED" {
				idleSeconds := int64(0)
				if idle > 0 {
					idleSeconds = int64(idle.Round(time.Second) / time.Second)
				}
				return StaleSessionAudit{
					IssueNumber: sess.IssueNumber,
					PRNumber:    sess.PRNumber,
					Status:      string(sess.Status),
					Reason:      MergedPRReason,
					IdleSeconds: idleSeconds,
					At:          now,
				}, true
			}
		}
	}

	if policy.IdleAfter <= 0 {
		return StaleSessionAudit{}, false
	}
	if changedAt.IsZero() {
		return StaleSessionAudit{}, false
	}
	if idle < policy.IdleAfter {
		return StaleSessionAudit{}, false
	}

	if policy.RequireWorktreeMissing {
		// We must have positive evidence (a recorded worktree path that no
		// longer exists on disk) before reclassifying a session as stale.
		// Without that evidence, the session is left alone so a live worker
		// is never reclaimed by the reconciler.
		worktreePath := strings.TrimSpace(sess.Worktree)
		if worktreePath == "" || worktreeExists == nil || worktreeExists(worktreePath) {
			return StaleSessionAudit{}, false
		}
	}

	reason := stalenessReason(sess, idle, policy)
	return StaleSessionAudit{
		Slot:        "",
		IssueNumber: sess.IssueNumber,
		PRNumber:    sess.PRNumber,
		Status:      string(sess.Status),
		Reason:      reason,
		IdleSeconds: int64(idle.Round(time.Second) / time.Second),
		At:          now,
	}, true
}

func stalenessReason(sess *Session, idle time.Duration, policy StaleSessionPolicy) string {
	idleHours := int(idle.Round(time.Hour) / time.Hour)
	if policy.RequireWorktreeMissing {
		return fmt.Sprintf("idle %dh and worktree no longer present", idleHours)
	}
	return fmt.Sprintf("idle %dh past reconciliation window", idleHours)
}

// ReconcileStaleSessions returns audit records for every session in the state
// that the policy classifies as stale. The state is not mutated: callers use
// the audit list to decide what to filter from operator-facing surfaces and to
// emit audit-log entries.
func (s *State) ReconcileStaleSessions(now time.Time, policy StaleSessionPolicy, worktreeExists func(string) bool) []StaleSessionAudit {
	if s == nil || !policy.Enabled {
		return nil
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	slots := make([]string, 0, len(s.Sessions))
	for slot := range s.Sessions {
		slots = append(slots, slot)
	}
	sort.Strings(slots)
	audits := make([]StaleSessionAudit, 0)
	for _, slot := range slots {
		sess := s.Sessions[slot]
		audit, stale := SessionStale(sess, now, policy, worktreeExists)
		if !stale {
			continue
		}
		audit.Slot = slot
		audits = append(audits, audit)
	}
	return audits
}

// HasApprovedSpawnForIssue reports whether a spawn_worker approval for the
// given issue number is currently in status=approved or status=execution_skipped.
// The dispatcher uses this to recognize that an operator has already given
// the go-ahead to spawn a worker for this issue, so a second approval
// prompt is not required when the next dispatch cycle reaches the issue.
func (s *State) HasApprovedSpawnForIssue(issueNumber int) bool {
	if s == nil || issueNumber <= 0 {
		return false
	}
	for i := range s.Approvals {
		a := &s.Approvals[i]
		if a.Action != approvalActionSpawnWorker {
			continue
		}
		if a.Target == nil || a.Target.Issue != issueNumber {
			continue
		}
		switch a.Status {
		case ApprovalStatusApproved, ApprovalStatusExecutionSkipped:
			return true
		}
	}
	return false
}

// ListApprovedApprovals returns approvals in status=approved (i.e. ready
// to be executed by the approver pipeline). Order: oldest first by
// CreatedAt, so the executor picks them up FIFO.
func (s *State) ListApprovedApprovals() []*Approval {
	if s == nil {
		return nil
	}
	out := make([]*Approval, 0)
	for i := range s.Approvals {
		if s.Approvals[i].Status == ApprovalStatusApproved {
			out = append(out, &s.Approvals[i])
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out
}

// ErrApprovalNotApproved is returned when MarkApprovalExecuted /
// MarkApprovalExecutionFailed / MarkApprovalExecutionSkipped is called on
// an approval that is not in status=approved (e.g. already executed).
var ErrApprovalNotApproved = errors.New("approval is not in status=approved")

// MarkApprovalExecuted transitions an approval from approved → executed
// and appends an audit entry. Idempotent at the caller boundary: if the
// approval has already been executed (or moved to any non-approved
// state), returns ErrApprovalNotApproved without mutating state — so a
// concurrent executor cannot double-execute.
func (s *State) MarkApprovalExecuted(id string, now time.Time, actor, summary string) (*Approval, error) {
	approval, ok := s.FindApproval(id)
	if !ok {
		return nil, ErrApprovalNotFound
	}
	if approval.Status != ApprovalStatusApproved {
		return approval, ErrApprovalNotApproved
	}
	approval.Status = ApprovalStatusExecuted
	approval.UpdatedAt = normalizedTime(now)
	approval.Audit = append(approval.Audit, ApprovalAudit{
		At:              approval.UpdatedAt,
		Event:           ApprovalAuditExecuted,
		Actor:           actor,
		Reason:          summary,
		PayloadHash:     approval.PayloadHash,
		TargetStateHash: approval.TargetStateHash,
	})
	return approval, nil
}

// MarkApprovalExecutionFailed transitions approved → execution_failed.
// Same idempotency guarantee as MarkApprovalExecuted.
func (s *State) MarkApprovalExecutionFailed(id string, now time.Time, actor, errMsg string) (*Approval, error) {
	approval, ok := s.FindApproval(id)
	if !ok {
		return nil, ErrApprovalNotFound
	}
	if approval.Status != ApprovalStatusApproved {
		return approval, ErrApprovalNotApproved
	}
	approval.Status = ApprovalStatusExecutionFailed
	approval.UpdatedAt = normalizedTime(now)
	approval.Audit = append(approval.Audit, ApprovalAudit{
		At:              approval.UpdatedAt,
		Event:           ApprovalAuditExecutionFailed,
		Actor:           actor,
		Reason:          errMsg,
		PayloadHash:     approval.PayloadHash,
		TargetStateHash: approval.TargetStateHash,
	})
	return approval, nil
}

// MarkApprovalExecutionSkipped transitions approved → execution_skipped
// with an audit reason. Used for verbs the executor intentionally does
// not run yet (e.g. change_global_config until the YAML pipeline lands).
func (s *State) MarkApprovalExecutionSkipped(id string, now time.Time, actor, reason string) (*Approval, error) {
	approval, ok := s.FindApproval(id)
	if !ok {
		return nil, ErrApprovalNotFound
	}
	if approval.Status != ApprovalStatusApproved {
		return approval, ErrApprovalNotApproved
	}
	approval.Status = ApprovalStatusExecutionSkipped
	approval.UpdatedAt = normalizedTime(now)
	approval.Audit = append(approval.Audit, ApprovalAudit{
		At:              approval.UpdatedAt,
		Event:           ApprovalAuditExecutionSkipped,
		Actor:           actor,
		Reason:          reason,
		PayloadHash:     approval.PayloadHash,
		TargetStateHash: approval.TargetStateHash,
	})
	return approval, nil
}

// MarkApprovalAwaitingDispatch transitions approved → awaiting_dispatch
// with an audit reason. Used for verbs whose side effect lives on a
// separate loop (spawn_worker — the dispatcher tick allocates the slot;
// open_child_issue — the operator creates the child until the
// safe-action executor lands). Distinct from ExecutionSkipped: a
// dispatcher loop will resolve this approval, dedup must keep treating
// it as effective until then.
func (s *State) MarkApprovalAwaitingDispatch(id string, now time.Time, actor, reason string) (*Approval, error) {
	approval, ok := s.FindApproval(id)
	if !ok {
		return nil, ErrApprovalNotFound
	}
	if approval.Status != ApprovalStatusApproved {
		return approval, ErrApprovalNotApproved
	}
	approval.Status = ApprovalStatusAwaitingDispatch
	approval.UpdatedAt = normalizedTime(now)
	approval.Audit = append(approval.Audit, ApprovalAudit{
		At:              approval.UpdatedAt,
		Event:           ApprovalAuditAwaitingDispatch,
		Actor:           actor,
		Reason:          reason,
		PayloadHash:     approval.PayloadHash,
		TargetStateHash: approval.TargetStateHash,
	})
	return approval, nil
}

// ValidateSlotID is the canonical slot-id validator used at EVERY
// state-write ingress (#490 / premortem #5). A slot id names a session
// in `state.Sessions` and is later concatenated into a worktree path
// under `cfg.WorktreeBase`; a malformed slot ("../etc/passwd", a NUL
// byte, a backslash) would let an attacker (or a hallucinating
// supervisor LLM) escape the worktree base and steer subsequent
// `worker.RemoveWorktree` calls at unrelated paths.
//
// Validators live HERE, not at the executor-only boundary, so any
// future refactor that adds a new write-path (HTTP, CLI, supervisor
// LLM, replay tool) gets the check for free as long as it routes
// through state.RecordPendingApprovalForDecision or another helper
// that calls ValidateSlotID.
//
// Empty input is rejected. The shape is intentionally narrow: ASCII
// letters / digits / `-` / `_`, length 1..96. We do NOT accept `/`,
// `\`, `.`, `..`, NUL, or anything outside that ASCII set. Tests
// in state_test.go enforce the negative cases.
func ValidateSlotID(slot string) error {
	s := strings.TrimSpace(slot)
	if s == "" {
		return errors.New("slot is empty")
	}
	if s == "." || s == ".." {
		return errors.New("slot is a traversal segment")
	}
	if len(s) > 96 {
		return errors.New("slot exceeds 96 bytes")
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z':
		case c >= 'A' && c <= 'Z':
		case c >= '0' && c <= '9':
		case c == '-' || c == '_':
		default:
			return fmt.Errorf("slot %q contains disallowed byte %q at index %d", slot, c, i)
		}
	}
	return nil
}
