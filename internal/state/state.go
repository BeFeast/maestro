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
	"sync"
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
	// DisplayWaitingForIssueGuard marks a canonical retry that is deliberately
	// held because the issue currently carries a configured exclude label. The
	// retry still owns its issue/worktree lease, but it is not a failed worker
	// requiring operator attention; removing the guard lets the same session
	// resume on the next orchestrator cycle.
	DisplayWaitingForIssueGuard SessionDisplayStatus = "waiting_for_issue_guard"
	// DisplayBackendRateLimited marks a session whose worker exited because its
	// backend hit a provider usage limit with no fallback available. It is a
	// distinct, non-failure display token so operators do not confuse a
	// rate-limited backend with a genuine retry_exhausted code failure.
	DisplayBackendRateLimited SessionDisplayStatus = "backend_rate_limited"
	// DisplayBackendAuthFailure marks a session whose worker exited because its
	// backend could not authenticate (e.g. claude CLI 401, #693) with no
	// fallback available. Like DisplayBackendRateLimited it is a backend
	// outage, not a work failure, and did not burn the per-issue retry budget.
	DisplayBackendAuthFailure SessionDisplayStatus = "backend_auth_failure"
	// DisplayBackendModelUnavailable marks a session whose worker exited
	// because its backend's configured model was unavailable — pulled,
	// renamed, or not accessible to the account (#713) — with no fallback
	// available. Distinct from DisplayBackendAuthFailure so operators see
	// "the model is gone" (swap the model id) rather than "fix credentials".
	// Like the other backend-block tokens it did not burn the retry budget.
	DisplayBackendModelUnavailable SessionDisplayStatus = "backend_model_unavailable"
	// DisplayBackendModelCooldown marks a session whose provider exhausted the
	// compatible credential pool for one requested model. Other models on the
	// provider remain eligible.
	DisplayBackendModelCooldown SessionDisplayStatus = "backend_model_cooldown"
	// DisplayBackendUsageLimit marks a session whose worker exited because
	// its backend's account usage quota is exhausted (#805; live: codex
	// "You've hit your usage limit") with no fallback available. Distinct
	// from DisplayBackendRateLimited so operators see "quota exhausted,
	// wait for the window reset" rather than a generic capacity blip. Like
	// the other backend-block tokens it did not burn the retry budget.
	DisplayBackendUsageLimit SessionDisplayStatus = "backend_usage_limit"
	// DisplayTokenBudgetExceeded is a deterministic worker stop, not a retryable
	// process death or provider outage.
	DisplayTokenBudgetExceeded SessionDisplayStatus = "token_budget_exceeded"
	LiveSessionRecentWindow                         = 24 * time.Hour
)

const (
	RetryReasonReviewFeedback  = "review_feedback"
	RetryReasonStalledProgress = "stalled_progress"
)

const (
	BackendHealthAvailable = "available"
	BackendHealthCooldown  = "cooldown"

	BackendBlockProviderLimit = "provider_limit"
	// BackendBlockAuthFailure gates a backend whose CLI failed to
	// authenticate (e.g. 401 invalid/expired credentials, #693). Worker
	// deaths attributed to it are backend failures, not work failures: they
	// must not consume the per-issue retry budget.
	BackendBlockAuthFailure = "auth_failure"
	// BackendBlockModelUnavailable gates a backend whose CLI failed because
	// its configured model is unavailable — pulled from the plan, renamed, or
	// not accessible to the account (#713). Like auth_failure it is a hard
	// backend failure, not a work failure: worker deaths attributed to it must
	// not consume the per-issue retry budget. It is kept distinct from
	// auth_failure so the operator remediation differs (swap the model id vs
	// fix credentials).
	BackendBlockModelUnavailable = "model_unavailable"
	// BackendBlockModelCooldown gates one provider/model route after the proxy
	// has rotated through every compatible credential and found none usable.
	// It must never become a provider-wide BackendHealth gate.
	BackendBlockModelCooldown = "model_cooldown"
	// BackendBlockUsageLimit gates a backend whose CLI died because the
	// account's usage quota is exhausted (#805; live: codex "You've hit
	// your usage limit ... try again at 12:30 PM" killed every worker on
	// the then-default backend and the retry policy burned the per-issue
	// budget respawning onto it). Like auth_failure it is a hard backend
	// failure, not a work failure, and must not consume the retry budget.
	// Distinct from provider_limit (which carries a provider-stated reset)
	// and quota_pressure (predictive, token-estimate based): usage_limit is
	// the reactive classification of a quota death whose reset time could
	// not be parsed, so the cooldown is a fixed re-probe window.
	BackendBlockUsageLimit   = "usage_limit"
	BackendBlockDisabled     = "disabled"
	BackendBlockAlreadyTried = "already_tried"
	BackendBlockCurrent      = "current_backend"
	BackendBlockUnknown      = "unknown_backend"
	// BackendBlockQuotaPressure gates a backend whose estimated
	// subscription-window usage crossed the quota dispatch threshold
	// (#704). Unlike auth_failure it is a soft, predictive gate: the
	// backend still works, but fresh dispatch prefers fallbacks until
	// the window resets so the remaining capacity survives for work
	// already in flight.
	BackendBlockQuotaPressure = "quota_pressure"
)

// BackendHealth records cross-session availability for a configured backend.
type BackendHealth struct {
	State                     string     `json:"state"`
	Reason                    string     `json:"reason,omitempty"`
	Pattern                   string     `json:"pattern,omitempty"`
	Provider                  string     `json:"provider,omitempty"`
	Model                     string     `json:"model,omitempty"`
	CredentialCandidates      int        `json:"credential_candidates,omitempty"`
	CredentialCandidatesKnown bool       `json:"credential_candidates_known,omitempty"`
	CredentialUsable          int        `json:"credential_usable,omitempty"`
	CredentialUsableKnown     bool       `json:"credential_usable_known,omitempty"`
	AggregateReason           string     `json:"aggregate_reason,omitempty"`
	Since                     time.Time  `json:"since,omitempty"`
	RetryAfter                *time.Time `json:"retry_after,omitempty"`
	LastSession               string     `json:"last_session,omitempty"`
}

// BackendCandidate explains why one backend was or was not selectable.
type BackendCandidate struct {
	Backend    string  `json:"backend"`
	Provider   string  `json:"provider,omitempty"`
	Model      string  `json:"model,omitempty"`
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
	TaskType        string             `json:"task_type,omitempty"`
	CandidateScores []BackendCandidate `json:"candidate_scores,omitempty"`
	HardPin         bool               `json:"hard_pin,omitempty"`
	PreviousBackend string             `json:"previous_backend,omitempty"`

	// Task-aware policy routing observability (#783, RFC §2.7). Tier is the
	// strength tier the policy resolved to; Effort/Model are the per-tier
	// overrides threaded into the worker argv. ShadowTier is the tier the
	// policy *would* have picked while routing.policy.shadow is on (dispatch
	// unchanged) so a wave can be validated before enabling.
	Tier       string `json:"tier,omitempty"`
	Effort     string `json:"effort,omitempty"`
	Model      string `json:"model,omitempty"`
	ShadowTier string `json:"shadow_tier,omitempty"`
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
	IssueNumber int           `json:"issue_number"`
	IssueTitle  string        `json:"issue_title"`
	Worktree    string        `json:"worktree"`
	Branch      string        `json:"branch"`
	PID         int           `json:"pid"`
	TmuxSession string        `json:"tmux_session,omitempty"`
	LogFile     string        `json:"log_file"`
	StartedAt   time.Time     `json:"started_at"`
	FinishedAt  *time.Time    `json:"finished_at,omitempty"`
	Status      SessionStatus `json:"status"`
	PRNumber    int           `json:"pr_number,omitempty"`
	Backend     string        `json:"backend,omitempty"` // "claude", "codex", etc.
	// #730: model + self-reported cost captured from the backend's own
	// usage stream (Pi --mode json event stream). Empty/zero for backends
	// that do not self-report; the fleet cost panel then falls back to the
	// configured per-backend pricing estimate.
	Model                    string     `json:"model,omitempty"`                  // model the backend reported for this run (e.g. glm-5.2:cloud, claude-opus-4-8)
	CostUSDBackend           float64    `json:"cost_usd_backend,omitempty"`       // USD cost the backend self-reported (Pi cost.total / claude total_cost_usd)
	UsageTokensWatermark     int        `json:"usage_tokens_watermark,omitempty"` // #730/#737: high-water mark of the current attempt's backend usage stream; reset when an attempt log is rotated so a replacement process starts from its own zero while TokensUsedTotal remains cumulative
	LongRunning              bool       `json:"long_running,omitempty"`
	RebaseAttempted          bool       `json:"rebase_attempted,omitempty"`
	NotifiedCIFail           bool       `json:"notified_ci_fail,omitempty"`           // deprecated: use LastNotifiedStatus
	LastNotifiedStatus       string     `json:"last_notified_status,omitempty"`       // dedup: last notification type sent
	LiveVerificationNotified bool       `json:"live_verification_notified,omitempty"` // #570 one-shot: hold-for-live-verification board sync + operator notification already fired
	RetryCount               int        `json:"retry_count,omitempty"`                // per-session retry counter; the global per-issue limit (max_retries_per_issue) combines this with FailedAttemptsForIssue
	MaintenanceRetryCount    int        `json:"maintenance_retry_count,omitempty"`    // bounded post-PR maintenance attempts (review feedback / rebase conflict repair), separate from implementation retries
	NextRetryAt              *time.Time `json:"next_retry_at,omitempty"`
	RetryHoldReason          string     `json:"retry_hold_reason,omitempty"` // current dispatch guard holding a scheduled canonical retry; does not release its issue/worktree lease
	LastOutputHash           string     `json:"last_output_hash,omitempty"`
	LastOutputChangedAt      time.Time  `json:"last_output_changed_at,omitempty"`
	TokensUsedAttempt        int        `json:"tokens_used_attempt,omitempty"` // tokens consumed in current attempt (reset on respawn)
	TokensUsedTotal          int        `json:"tokens_used_total,omitempty"`   // cumulative tokens across the issue lifecycle (sum of the split dimensions below; kept for back-compat)
	WorkerOutcome            string     `json:"worker_outcome,omitempty"`      // deterministic terminal worker outcome, e.g. token_budget_exceeded
	// #739: cache-aware split token counters stamped from a backend usage
	// stream (claude stream-json / Pi --mode json). Cumulative run totals so
	// the cost panel can price each dimension separately — cache_read tokens
	// dominate an agentic run and cost ~10% of input, so blending them into
	// TokensUsedTotal over-states cost. Zero for backends that do not stamp a
	// split; the cost rollup then falls back to the blended estimate.
	TokensInput                 int               `json:"tokens_input,omitempty"`                   // cumulative non-cached input tokens
	TokensOutput                int               `json:"tokens_output,omitempty"`                  // cumulative output (generated) tokens
	TokensCacheRead             int               `json:"tokens_cache_read,omitempty"`              // cumulative cache-read (reused context) tokens, discounted
	TokensCacheWrite            int               `json:"tokens_cache_write,omitempty"`             // cumulative cache-write (cache creation) tokens
	QuotaTokensAccounted        int               `json:"quota_tokens_accounted,omitempty"`         // portion of TokensUsedTotal already accrued into BackendQuotaUsage windows (#704)
	RateLimitHit                bool              `json:"rate_limit_hit,omitempty"`                 // true when the worker died on a transient backend block (provider limit or auth failure, #693); excludes the session from the per-issue retry budget
	TriedBackends               []string          `json:"tried_backends,omitempty"`                 // backends already attempted (for backend-failure fallback)
	ProviderLimitBackend        string            `json:"provider_limit_backend,omitempty"`         // backend that hit a provider capacity limit or auth failure
	ProviderLimitReason         string            `json:"provider_limit_reason,omitempty"`          // backend block signature or class (e.g. BackendBlockAuthFailure)
	ProviderLimitResetAt        *time.Time        `json:"provider_limit_reset_at,omitempty"`        // provider-stated reset time parsed from the limit message ("try again at ..."), UTC
	ProviderLimitProvider       string            `json:"provider_limit_provider,omitempty"`        // secret-free provider route for model-scoped failures
	ProviderLimitModel          string            `json:"provider_limit_model,omitempty"`           // requested model for model-scoped failures
	CredentialCandidates        int               `json:"credential_candidates,omitempty"`          // aggregate candidate count reported by the proxy
	CredentialCandidatesKnown   bool              `json:"credential_candidates_known,omitempty"`    // distinguishes an omitted count from a real zero
	CredentialUsable            int               `json:"credential_usable,omitempty"`              // candidates usable for ProviderLimitModel
	CredentialUsableKnown       bool              `json:"credential_usable_known,omitempty"`        // distinguishes an omitted count from a real zero
	CredentialAggregateReason   string            `json:"credential_aggregate_reason,omitempty"`    // aggregate proxy reason; never a credential identifier
	BackendSelection            *BackendSelection `json:"backend_selection,omitempty"`              // latest backend selection audit record
	Phase                       Phase             `json:"phase,omitempty"`                          // current pipeline phase (empty = legacy single-phase)
	PipelineFull                bool              `json:"pipeline_full,omitempty"`                  // true when issue label opted this session into plan/implement/validate
	ValidationFails             int               `json:"validation_fails,omitempty"`               // number of failed validation attempts
	ValidationFeedback          string            `json:"validation_feedback,omitempty"`            // feedback from last failed validation
	CIFailureOutput             string            `json:"ci_failure_output,omitempty"`              // CI failure output captured before retry (passed to next worker as context)
	FailingCheckContext         string            `json:"failing_check_context,omitempty"`          // #857: bounded excerpt of a check-run still failing on the PR head, carried into a retry so the worker sees the red check its previous push introduced (consumed on respawn)
	PreviousAttemptFeedback     string            `json:"previous_attempt_feedback,omitempty"`      // feedback from previous failed PR attempt
	PreviousAttemptFeedbackKind string            `json:"previous_attempt_feedback_kind,omitempty"` // review_feedback, rebase_conflict
	RetryReason                 string            `json:"retry_reason,omitempty"`                   // current retry lifecycle reason, e.g. review_feedback
	OperatorGateName            string            `json:"operator_gate_name,omitempty"`             // explicit human/operator gate holding this PR; cleared when the gate opens
	OperatorGateRequiredAction  string            `json:"operator_gate_required_action,omitempty"`  // concise operator action needed to clear OperatorGateName
	LastClosedPRNumber          int               `json:"last_closed_pr_number,omitempty"`          // PR the retry path closed before scheduling this retry (#800); if an operator reopens and merges it while the backoff runs, the pre-respawn staleness check sees the merge and cancels the retry
	ReleasedForRedispatch       bool              `json:"released_for_redispatch,omitempty"`        // #818: a retry_exhausted session whose closed-unmerged PR was reconciled and the issue released for fresh dispatch. Marked failed so the attempt counts toward max_retries_per_issue, but the board must mirror it as runnable Todo (not Blocked) so the dynamic wave re-dispatches instead of re-stranding it
	LastTerminalReconcileAt     *time.Time        `json:"last_terminal_reconcile_at,omitempty"`     // #940: last successful authoritative issue/PR reconciliation for a terminal session. Bounds historical forge polling while preserving the 10-minute hands-off SLA across daemon restarts
	CheckpointFile              string            `json:"checkpoint_file,omitempty"`                // path to CHECKPOINT.md saved at soft token threshold
	RestartCheckpointAt         *time.Time        `json:"restart_checkpoint_at,omitempty"`          // #877/#966: set when the daemon deliberately checkpoints this still-running worker on shutdown. A non-nil value tells the next daemon to adopt the same surviving isolated PID/worktree, or resume the same logical session in place if the worker exited, exactly once. Cleared on successful adoption/resume so it cannot duplicate.
	DeploymentFinishedAt        *time.Time        `json:"deployment_finished_at,omitempty"`         // set when the post-merge deploy hook succeeds
	CodeLandedVerifyDeadline    *time.Time        `json:"code_landed_verify_deadline,omitempty"`    // #1020: when a code_landed session was first observed failing its blocking outcome check; past this the fix is judged ineffective if the SAME fingerprint persists
	OutcomeFailureFingerprint   string            `json:"outcome_failure_fingerprint,omitempty"`    // #1020: stable identity of the blocking outcome failure captured when the deadline was armed; a changed fingerprint re-arms rather than convicting

	// #705: opt-in verify.visual outcome for this session's PR. Set once by
	// the orchestrator's merge flow: "not_required" (no UI paths touched),
	// "attached" (evidence found on the PR), or "missing" (UI-affecting PR
	// without attached evidence — warning comment posted, supervisor records
	// a finding). Empty until checked. Advisory only; never blocks merge.
	VisualEvidence       string `json:"visual_evidence,omitempty"`
	VisualEvidenceDetail string `json:"visual_evidence_detail,omitempty"` // operator-facing detail for the "missing" case

	// #691: greptile webhook-miss self-heal. The orchestrator tracks how
	// long the review gate has been greptile=pending on one head SHA; past
	// the configured threshold it posts "@greptile review" on the PR
	// (cooldown-bounded) to re-trigger the missed review. A new head SHA
	// (push or server-side update-branch) restarts the pending clock.
	ReviewPendingHeadSHA string     `json:"review_pending_head_sha,omitempty"` // head SHA the pending clock applies to
	ReviewPendingSince   *time.Time `json:"review_pending_since,omitempty"`    // first observation of greptile=pending on that head
	ReviewRetriggerAt    *time.Time `json:"review_retrigger_at,omitempty"`     // last "@greptile review" re-trigger (cooldown anchor)

	// #426: distinguish agent execution time from workflow elapsed time.
	// WorkerEndedAt is stamped the FIRST time the worker process exits
	// (running -> pr_open / dead / failed / etc.). It is never overwritten
	// by later status transitions, so worker_runtime (StartedAt -> WorkerEndedAt)
	// reflects only the coding agent's wall-clock time, not subsequent
	// PR-open / CI / Greptile / merge waiting. PROpenedAt is stamped the
	// first time the session enters pr_open and is preserved similarly,
	// so pr_open_runtime (PROpenedAt -> FinishedAt) is attributable to
	// orchestration latency instead of agent runtime.
	WorkerEndedAt *time.Time `json:"worker_ended_at,omitempty"`
	PROpenedAt    *time.Time `json:"pr_opened_at,omitempty"`

	// #513: per-segment attribution timeline. Every spawn / respawn /
	// fallover appends a new entry; the previous entry's EndedAt is
	// closed at the same moment. Records who actually produced the
	// commits (provider/model/variant/effort) so the dashboard +
	// commit trailer can show "first 12m on claude opus-4.8 xhigh,
	// then 4m on codex gpt-5.5 medium after rate-limit fallover".
	Attribution []BackendAttribution `json:"attribution,omitempty"`
}

// Session.VisualEvidence outcomes for the opt-in verify.visual step (#705).
const (
	// VisualEvidenceNotRequired: the PR touches no configured UI paths.
	VisualEvidenceNotRequired = "not_required"
	// VisualEvidenceAttached: image evidence was found on the PR.
	VisualEvidenceAttached = "attached"
	// VisualEvidenceMissing: UI-affecting PR without attached evidence —
	// the orchestrator posted a warning comment and the supervisor records
	// a visual_evidence_missing finding. Advisory; never blocks merge.
	VisualEvidenceMissing = "missing"
)

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
	TaskType  string     `json:"task_type,omitempty"`  // router classification: refactor, bugfix, test, vision, design, docs, infra
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
	if sess.Status == StatusDead && sess.NextRetryAt != nil && strings.TrimSpace(sess.RetryHoldReason) != "" {
		return SessionAttention{
			Reason:     "Automatic retry is waiting for the issue's current dispatch guard to clear.",
			NextAction: "No worker restart is allowed while the guard remains; Maestro will resume the same canonical session after it clears.",
		}
	}
	if attention, ok := reviewFeedbackRetryAttention(sess, alive, now); ok {
		return attention
	}
	if sess.WorkerOutcome == string(DisplayTokenBudgetExceeded) {
		return SessionAttention{
			Reason:         fmt.Sprintf("Worker stopped after reaching its configured token budget (%s tokens observed).", formatSessionTokens(sess.TokensUsedAttempt)),
			NextAction:     "Review the partial work and raise or disable worker_max_tokens only if a larger run is intentional.",
			NeedsAttention: true,
		}
	}
	if attention, ok := OperatorGateAttention(sess); ok {
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
			if sess.ProviderLimitReason == BackendBlockAuthFailure {
				return SessionAttention{
					Reason:         fmt.Sprintf("Backend %s failed authentication (invalid or expired credentials); no fallback backend is currently available or allowed.", backend),
					NextAction:     "Re-authenticate the backend CLI (or wait for credential sync), then retry; the per-issue retry budget was not consumed.",
					NeedsAttention: true,
				}
			}
			if sess.ProviderLimitReason == BackendBlockModelUnavailable {
				return SessionAttention{
					Reason:         fmt.Sprintf("Backend %s could not load its configured model (unavailable, renamed, or no access); no fallback backend is currently available or allowed.", backend),
					NextAction:     "Point the backend at an available model id (or restore model access), then retry; the per-issue retry budget was not consumed.",
					NeedsAttention: true,
				}
			}
			if sess.ProviderLimitReason == BackendBlockModelCooldown {
				route := backend
				if sess.ProviderLimitProvider != "" {
					route = sess.ProviderLimitProvider
				}
				if sess.ProviderLimitModel != "" {
					route += "/" + sess.ProviderLimitModel
				}
				return SessionAttention{
					Reason:         fmt.Sprintf("Provider/model route %s has no usable compatible credential; other models on the provider remain eligible.", route),
					NextAction:     "Wait for the route retry time or restore model access on a compatible credential; the per-issue retry budget was not consumed.",
					NeedsAttention: true,
				}
			}
			if sess.ProviderLimitReason == BackendBlockUsageLimit {
				return SessionAttention{
					Reason:         fmt.Sprintf("Backend %s has exhausted its account usage quota; no fallback backend is currently available or allowed.", backend),
					NextAction:     "Wait for the provider quota window to reset (or raise the plan limit / enable another backend); the per-issue retry budget was not consumed.",
					NeedsAttention: true,
				}
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

// OperatorGateAttention returns a stable, state-backed waiting verdict for an
// explicit human/operator gate. It is independent of the runtime status because
// an operator can apply a hold while a retry is already scheduled.
func OperatorGateAttention(sess *Session) (SessionAttention, bool) {
	if sess == nil || strings.TrimSpace(sess.OperatorGateName) == "" {
		return SessionAttention{}, false
	}
	action := strings.TrimSpace(sess.OperatorGateRequiredAction)
	if action == "" {
		action = "Complete the operator decision, then remove the hold label or let the gated check pass."
	}
	pr := "the session"
	if sess.PRNumber > 0 {
		pr = fmt.Sprintf("PR #%d", sess.PRNumber)
	}
	return SessionAttention{
		Reason:         fmt.Sprintf("%s is held by operator gate %q.", pr, sess.OperatorGateName),
		NextAction:     action + " The retry budget was not consumed.",
		NeedsAttention: true,
	}, true
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
	if sess.WorkerOutcome == string(DisplayTokenBudgetExceeded) {
		return string(DisplayTokenBudgetExceeded)
	}
	if sess.Status == StatusRunning && alive != nil && !*alive {
		return string(sess.Status)
	}
	if sess.Status == StatusDead && sess.NextRetryAt != nil && strings.TrimSpace(sess.RetryHoldReason) != "" {
		return string(DisplayWaitingForIssueGuard)
	}
	if display := reviewFeedbackRetryDisplayStatus(sess, now); display != "" {
		return string(display)
	}
	if backendRateLimitedDisplayStatus(sess) {
		switch sess.ProviderLimitReason {
		case BackendBlockAuthFailure:
			return string(DisplayBackendAuthFailure)
		case BackendBlockModelUnavailable:
			return string(DisplayBackendModelUnavailable)
		case BackendBlockModelCooldown:
			return string(DisplayBackendModelCooldown)
		case BackendBlockUsageLimit:
			return string(DisplayBackendUsageLimit)
		}
		return string(DisplayBackendRateLimited)
	}
	return string(sess.Status)
}

func formatSessionTokens(tokens int) string {
	if tokens <= 0 {
		return "unknown"
	}
	return fmt.Sprintf("%d", tokens)
}

// backendRateLimitedDisplayStatus reports whether a session represents a worker
// that exited because of a backend block (provider usage limit or auth
// failure, #693) with no fallback available. Such a session is marked Dead
// but, unlike a real failure, did not burn the per-issue retry budget; it
// must be shown distinctly so operators do not mistake it for retry_exhausted.
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

// HasSplitTokens reports whether the session carries the cache-aware split
// token breakdown stamped from a backend usage stream (#739). When true the
// cost rollup prices each dimension separately (EstimateCostSplit); when
// false it falls back to the blended estimate over TokensUsedTotal.
func (s *Session) HasSplitTokens() bool {
	if s == nil {
		return false
	}
	return s.TokensInput > 0 || s.TokensOutput > 0 || s.TokensCacheRead > 0 || s.TokensCacheWrite > 0
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
	ErrNoStateChange           = errors.New("state update made no change")
)

// SupervisorTarget identifies the primary object a supervisor decision refers to.
type SupervisorTarget struct {
	Issue int `json:"issue,omitempty"`
	PR    int `json:"pr,omitempty"`
	// Issues carries a batch of verified merged issue candidates for one
	// cautious-gate close approval. Single-issue verbs keep using Issue/PR.
	Issues []SupervisorIssueTarget `json:"issues,omitempty"`
	// HeadSHA stamps the PR head SHA at decision time. Used by the
	// review-repair pipeline (#565) so a decision is keyed on the exact
	// head it observed; a new push that moves head invalidates a pending
	// spawn_review_repair recommendation automatically (dedup hashes
	// include the target).
	HeadSHA string `json:"head_sha,omitempty"`
	Session string `json:"session,omitempty"`
	// Body carries the proposed replacement issue body for an edit_issue_body
	// approval (#851). It is set only on that verb's target; the approver
	// executor reads it on approve and calls gh.EditIssueBody. It is part of
	// the approval payload hash (ComputePayloadHash hashes the whole Target)
	// but intentionally NOT part of approvalTargetsEqual, so a re-groom with a
	// fresh rewrite refreshes the pending approval in place rather than piling
	// up a duplicate.
	Body string `json:"body,omitempty"`
	// BaseBodyHash is the digest of the issue body the edit_issue_body rewrite
	// was groomed against, stamped at mint time (#851 review). On approve the
	// executor re-fetches the live body and refuses the edit if its hash no
	// longer matches — so a manual edit made after the proposal was minted is
	// never silently clobbered. Like Body, it is intentionally NOT part of
	// approvalTargetsEqual so a fresh re-groom refreshes the pending approval
	// in place.
	BaseBodyHash string `json:"base_body_hash,omitempty"`
}

type SupervisorIssueTarget struct {
	Issue int `json:"issue"`
	PR    int `json:"pr,omitempty"`
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
	// StuckReviewRepairSpawned is emitted when the supervisor mints a
	// spawn_review_repair decision: a green+mergeable+retry_exhausted PR
	// carries ≥1 Greptile P0/P1 inline comment on its current head SHA
	// and a scoped repair worker should run. Surfaces as an attention
	// item so operators see the auto-respawn instead of a silent
	// monitor_open_pr loop (#565).
	StuckReviewRepairSpawned = "review_repair_spawned"
	// StuckReviewRepairExhausted is emitted when the (pr,head_sha) repair
	// budget is exhausted but Greptile P0/P1 findings remain on head. The
	// PR is held for operator review with the unresolved comments
	// surfaced as evidence. NEVER a silent dead-end (#565).
	StuckReviewRepairExhausted = "review_repair_exhausted"
	StuckSearchGuardrailTrip   = "search_guardrail_trip"
	// StuckSupervisorMeteredBackend is emitted when the supervisor LLM path is
	// refused because supervisor.backend (or its model.default fallback) resolves
	// to a metered (per-token) backend and supervisor.allow_metered_backend is not
	// set (#838). The cycle runs deterministic-only so an always-on loop cannot
	// silently burn per-token cost; the code surfaces the disabled LLM path as a
	// red attention badge on Mission Control until the operator re-points the
	// backend or opts in.
	StuckSupervisorMeteredBackend = "supervisor_metered_backend"
	// StuckGuardrailConflict is emitted when the supervisor LLM's
	// recommendation disagrees with the deterministic guardrail (#689).
	// The conflict is resolved to the safe side for the cycle (the
	// deterministic decision when it is risk=safe, otherwise an explicit
	// no-op) instead of exiting rc=1 — a stable disagreement used to put
	// systemd in a restart loop. This code surfaces the standoff to the
	// operator while the loop stays alive.
	StuckGuardrailConflict = "guardrail_conflict"
)

const (
	LessonProposalStatusPending  = "pending"
	LessonProposalStatusApplied  = "applied"
	LessonProposalStatusDeclined = "declined"

	LessonProposalTargetWorkerPrompt = "worker_prompt"
	LessonProposalTargetAgentsMD     = "agents_md"
)

// LessonProposal is a durable, approval-gated candidate rule derived from a
// recurring terminal failure. Maestro records it for operator review, but never
// applies it until the linked approval is explicitly approved.
type LessonProposal struct {
	ID              string            `json:"id"`
	CreatedAt       time.Time         `json:"created_at"`
	UpdatedAt       time.Time         `json:"updated_at,omitempty"`
	FailureClass    string            `json:"failure_class"`
	Area            string            `json:"area"`
	MinimalRepro    string            `json:"minimal_repro"`
	SuggestedRule   string            `json:"suggested_rule"`
	Target          string            `json:"target"`
	Fingerprint     string            `json:"fingerprint"`
	Status          string            `json:"status"`
	ApprovalID      string            `json:"approval_id,omitempty"`
	SourceDecision  string            `json:"source_decision,omitempty"`
	SourceTarget    *SupervisorTarget `json:"source_target,omitempty"`
	AppliedAt       *time.Time        `json:"applied_at,omitempty"`
	DeclinedAt      *time.Time        `json:"declined_at,omitempty"`
	ResolutionActor string            `json:"resolution_actor,omitempty"`
	ResolutionNote  string            `json:"resolution_note,omitempty"`
}

// SupervisorIssueCandidate describes the issue selected by queue policy without
// exposing issue body content in persisted supervisor state.
type SupervisorIssueCandidate struct {
	Number        int      `json:"number"`
	Title         string   `json:"title,omitempty"`
	Labels        []string `json:"labels,omitempty"`
	PriorityLabel string   `json:"priority_label,omitempty"`
	ProjectStatus string   `json:"project_status,omitempty"`
}

// SupervisorSkippedCandidate is one open issue the queue policy excluded this
// cycle, paired with the human-readable reason and skip category so the
// Mission Control Queue / Next view can render "why #707 was skipped" without
// re-deriving it from the free-text SkippedReasons strings (#720). Number is
// best-effort: it is always set on the dynamic-wave path (where the issue
// object is in hand) and recovered from the leading "Issue #N" token on the
// ordered-queue / default paths.
type SupervisorSkippedCandidate struct {
	Number        int    `json:"number,omitempty"`
	Title         string `json:"title,omitempty"`
	PriorityLabel string `json:"priority_label,omitempty"`
	Category      string `json:"category,omitempty"`
	Reason        string `json:"reason"`
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

	// EligibleRanked is the eligible candidate set in the real selection
	// order (priority P0<P1<P2<P3, then ascending issue number — see the
	// supervisor's sortDynamicWaveCandidates). The first entry mirrors
	// SelectedCandidate. Bounded to keep persisted decisions small; added
	// for the Mission Control Queue / Next decision plane (#720).
	EligibleRanked []SupervisorIssueCandidate `json:"eligible_ranked,omitempty"`
	// SkippedCandidates pairs each skipped open issue with its reason and
	// skip category so the decision plane can render every skipped candidate
	// next to the next/eligible set (#720). Bounded to match EligibleRanked.
	SkippedCandidates []SupervisorSkippedCandidate `json:"skipped_candidates,omitempty"`
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
	// ReviewRepair carries the scoped findings and backend choice for a
	// spawn_review_repair decision (#565). The orchestrator dispatcher
	// uses this payload to build the worker prompt and pick the strong
	// backend without re-fetching review comments from GitHub. nil for
	// every non-review-repair decision.
	ReviewRepair *SupervisorReviewRepairPayload `json:"review_repair,omitempty"`
	// Epics is the epic-completion aggregate snapshot for the cycle that
	// produced this decision (#650). Each entry summarises one open epic's
	// children-merged ratio and the outcome health gate, so the fleet /
	// status surfaces can render "epic in progress" / "epic complete"
	// without re-listing issues. Empty when no candidate epic is open or
	// no candidate epic has parseable children.
	Epics []EpicProgress `json:"epics,omitempty"`
}

// EpicProgress is the per-epic aggregate the supervisor records alongside
// a decision (#650). Counts and child slices are read by the fleet /
// status surface; Complete=true means every child is merged or closed AND
// the configured outcome health gate is healthy — the precondition for
// the approval-gated epic close recommendation. Never used to auto-close
// without a human approval.
type EpicProgress struct {
	Number          int      `json:"number"`
	Title           string   `json:"title,omitempty"`
	Children        []int    `json:"children,omitempty"`
	MergedChildren  []int    `json:"merged_children,omitempty"`
	OpenChildren    []int    `json:"open_children,omitempty"`
	TotalChildren   int      `json:"total_children"`
	MergedCount     int      `json:"merged_count"`
	OpenCount       int      `json:"open_count"`
	OutcomeHealth   string   `json:"outcome_health,omitempty"`
	OutcomeHealthy  bool     `json:"outcome_healthy"`
	AllChildrenDone bool     `json:"all_children_done"`
	Complete        bool     `json:"complete"`
	Summary         string   `json:"summary,omitempty"`
	Evidence        []string `json:"evidence,omitempty"`
}

// SupervisorReviewRepairPayload carries the auto-review-repair payload
// on a SupervisorDecision (#565). The orchestrator dispatcher reads it
// to build the scoped worker prompt and pick the configured strong
// backend.
type SupervisorReviewRepairPayload struct {
	HeadSHA    string                    `json:"head_sha"`
	Backend    string                    `json:"backend,omitempty"`
	Model      string                    `json:"model,omitempty"`
	Effort     string                    `json:"effort,omitempty"`
	MaxRetries int                       `json:"max_retries,omitempty"`
	Attempts   int                       `json:"attempts,omitempty"`
	Findings   []SupervisorReviewFinding `json:"findings,omitempty"`
}

// SupervisorReviewFinding is one Greptile inline P0/P1 comment scoped
// onto a spawn_review_repair decision. Carries enough context for the
// worker prompt to address the comment without re-fetching from GitHub.
type SupervisorReviewFinding struct {
	Path     string `json:"path,omitempty"`
	Line     int    `json:"line,omitempty"`
	Body     string `json:"body,omitempty"`
	Severity string `json:"severity,omitempty"`
	User     string `json:"user,omitempty"`
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
	// ApprovalStatusExecuting is the durable claim an executor takes on an
	// approved delivery (deploy_project) BEFORE any external side effect
	// (#872 safety addendum). Exactly one executor wins the approved→executing
	// transition; a daemon restart that observes an executing row must NOT
	// replay it — the executor only claims from approved, so an executing row
	// is skipped and surfaced for operator reconciliation instead.
	ApprovalStatusExecuting ApprovalStatus = "executing"
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
	// ApprovalAuditExecuting records the durable approved→executing claim on a
	// delivery approval (#872).
	ApprovalAuditExecuting = "executing"
	// ApprovalAuditExecutionReleased records a pre-side-effect release of an
	// executing delivery claim after transient freshness verification failed.
	ApprovalAuditExecutionReleased = "execution_released"
	// ApprovalAuditDeliveryReconciled records an explicit operator decision for
	// an execution whose process ended without a durable terminal result. The
	// structured outcome lives in DeliveryPayload; audit text stays fixed.
	ApprovalAuditDeliveryReconciled = "delivery_reconciled"
)

const (
	approvalActionCloseIssue        = "close_issue"
	approvalActionCloseIssueBatch   = "close_issue_batch"
	approvalActionSpawnWorker       = "spawn_worker"
	approvalActionSpawnRepairWorker = "spawn_repair_worker"
	approvalActionSpawnReviewRepair = "spawn_review_repair"
	// ApprovalActionDeployProject is the #872 approval-gated post-merge
	// delivery verb (kept in lockstep with config.SupervisorActionDeployProject).
	ApprovalActionDeployProject = "deploy_project"
)

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

	// LessonProposalID links apply_lesson_proposal approvals to the durable
	// proposed rule they gate. Rejection/apply transitions update that record.
	LessonProposalID string `json:"lesson_proposal_id,omitempty"`

	// ReviewRepair carries the scoped review-repair payload (head SHA,
	// blocking findings, backend) for a spawn_review_repair approval (#874).
	// Before #874 this payload lived only on the SupervisorDecision, so the
	// orchestrator dispatcher resolved it from LatestSupervisorDecision(); a
	// manually enqueued approval (or one whose decision aged out of the
	// bounded decision ring) had no payload and could enter awaiting_dispatch
	// with no dispatcher path. Persisting the payload on the durable approval
	// lets the dispatcher converge on exactly one worker without a coincident
	// latest supervisor decision. nil for every non-review-repair approval.
	ReviewRepair *SupervisorReviewRepairPayload `json:"review_repair,omitempty"`

	// Delivery carries the #872 approval-gated post-merge delivery payload for
	// a deploy_project approval: the exact merged revision plus an explicit
	// operator-declared-safe label/plan, and (once executed) allow-listed result
	// metadata. Nil on every other verb. It is part of
	// ComputePayloadHash so a superseding merge (a different MergedSHA) can
	// never be approved into deploying the stale revision.
	Delivery *DeliveryPayload `json:"delivery,omitempty"`
}

// DeliveryPayload is the strict allow-list persisted for a deploy_project
// approval (#872). It deliberately has no command, verifier, local path,
// destination, rollback command, stdout/stderr, or error-text fields. Those
// values are execution inputs and can contain credentials even after a
// best-effort redaction pass. The executor reads them from live config and
// persists only immutable coordinates, explicit operator-declared-safe labels,
// and structured result metadata.
type DeliveryPayload struct {
	Project   string    `json:"project,omitempty"`
	Repo      string    `json:"repo,omitempty"`
	PR        int       `json:"pr,omitempty"`
	Issue     int       `json:"issue,omitempty"`
	MergedSHA string    `json:"merged_sha"`          // exact merge commit, pinned at mint
	MergedAt  time.Time `json:"merged_at,omitempty"` // authoritative GitHub merge order
	// ApprovalGeneration increments only when the latest approval for this
	// exact SHA+config digest became stale because its immutable expiry passed.
	// It gives an intentional renewal a distinct content-addressed ID without
	// resurrecting rejected, failed, executing, or otherwise-stale decisions.
	ApprovalGeneration int `json:"approval_generation,omitempty"`

	// These three fields are copied only from config keys whose names explicitly
	// declare them safe for durable operator UI/history. They default blank; no
	// command/target/rollback value is ever inferred into them.
	TargetLabel       string `json:"target_label,omitempty"`
	VerificationLabel string `json:"verification_label,omitempty"`
	RollbackLabel     string `json:"rollback_label,omitempty"`
	TimeoutMinutes    int    `json:"timeout_minutes,omitempty"`
	// ConfigDigest binds the exact delivery mode/command/verifier/timeouts and
	// operator context that were present when the approval was minted. The raw
	// command remains out of durable state; execution fails closed if the live
	// config digest differs.
	ConfigDigest string    `json:"config_digest"`
	ExpiresAt    time.Time `json:"expires_at,omitempty"`

	// Execution result — structured allow-list only. Exit codes are pointers so
	// an actual exit 0 differs from a stage that never ran. FailureStage is one
	// of precondition|checkout|deploy|verify|cleanup; no error text is stored.
	StartedAt      time.Time `json:"started_at,omitempty"`
	FinishedAt     time.Time `json:"finished_at,omitempty"`
	FailureStage   string    `json:"failure_stage,omitempty"`
	DeployExitCode *int      `json:"deploy_exit_code,omitempty"`
	VerifyExitCode *int      `json:"verify_exit_code,omitempty"`
	TimedOut       bool      `json:"timed_out,omitempty"`
	CleanupFailed  bool      `json:"cleanup_failed,omitempty"`
	Verified       bool      `json:"verified,omitempty"`
	// CompletionSource/ReconcileOutcome are closed codes used only by explicit
	// operator reconciliation of an interrupted executing row. Normal executor
	// completion leaves both empty.
	CompletionSource string `json:"completion_source,omitempty"`
	ReconcileOutcome string `json:"reconcile_outcome,omitempty"`
	// StaleCause is a closed status code, never free text. It distinguishes a
	// renewable config-drift decision from expiry and integrity/payload failures.
	StaleCause string `json:"stale_cause,omitempty"`
	// ExecutedRevision is the revision that was actually checked out at
	// execute time. It equals MergedSHA on a clean run; a mismatch fails the
	// delivery before any command runs (never deploy whatever is at LocalPath).
	ExecutedRevision string `json:"executed_revision,omitempty"`
}

// Clone returns a deep copy of the payload (all fields are value types).
func (p *DeliveryPayload) Clone() *DeliveryPayload {
	if p == nil {
		return nil
	}
	cp := *p
	if p.DeployExitCode != nil {
		code := *p.DeployExitCode
		cp.DeployExitCode = &code
	}
	if p.VerifyExitCode != nil {
		code := *p.VerifyExitCode
		cp.VerifyExitCode = &code
	}
	return &cp
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
	LessonProposals     []LessonProposal           `json:"lesson_proposals,omitempty"`
	OutcomeHealth       *outcome.HealthCheckResult `json:"outcome_health,omitempty"`
	OutcomeRecovery     *outcome.RecoveryState     `json:"outcome_recovery,omitempty"`
	// OutcomeGateStreaks tracks per-gate consecutive scheduled-failure runs with
	// a failure fingerprint (gate name + reason class). OutcomeGateStreakCheckedAt
	// is the CheckedAt of the last health result already folded into the streaks,
	// so each scheduled run increments a streak exactly once.
	OutcomeGateStreaks         []outcome.GateStreak                `json:"outcome_gate_streaks,omitempty"`
	OutcomeGateStreakCheckedAt time.Time                           `json:"outcome_gate_streak_checked_at,omitempty"`
	BackendHealth              map[string]BackendHealth            `json:"backend_health,omitempty"`
	ProviderModelHealth        map[string]map[string]BackendHealth `json:"provider_model_health,omitempty"`
	BackendQuotaUsage          map[string]*BackendQuotaUsage       `json:"backend_quota_usage,omitempty"`
	ProjectStatusSync          map[int]ProjectStatusSync           `json:"project_status_sync,omitempty"`
	NextSlot                   int                                 `json:"next_slot"`
	LastMergeAt                time.Time                           `json:"last_merge_at,omitempty"`

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

	// Paused is the first-class operator pause (#683): while it is set, the
	// orchestrator skips issue selection entirely and spawns no new workers,
	// but in-flight workers run to completion and land their PRs normally.
	// Unlike SpawnDrain it deliberately survives restarts of both project
	// units — only `maestro resume` clears it. PausedAt records when the
	// flag last toggled (UTC) so concurrent state writers resolve pause
	// on/off latest-write-wins, mirroring the drain merge semantics.
	Paused   bool      `json:"paused,omitempty"`
	PausedAt time.Time `json:"paused_at,omitempty"`

	// ReviewRepairTracks records per-(pr_number,head_sha) idempotency for
	// the #565 auto review-repair respawn. Each spawn bumps the Attempts
	// counter; the supervisor refuses to mint a fresh recommendation once
	// the counter reaches the configured budget. Keyed on
	// "PR#<n>@<head_sha>" to keep the map JSON-stable; HeadSHA is the
	// short SHA observed when the decision was recorded.
	ReviewRepairTracks map[string]ReviewRepairTrack `json:"review_repair_tracks,omitempty"`

	// PRGateSnapshots is the authoritative, durable PR/CI/review/merge progress
	// observed by the orchestrator (#887). Each value is keyed by the exact
	// project+issue+PR+head+generation identity; notification-dedup and review
	// re-trigger timer fields deliberately never enter this map.
	PRGateSnapshots map[string]PRGateSnapshot `json:"pr_gate_snapshots,omitempty"`

	// SpecLintTracks records the last spec-lint result per issue (#851),
	// keyed by issue number (project scope is the state.json file itself).
	// The supervisor lints an issue at most once per body change by comparing
	// the current body hash to the stored one, and records the last handled
	// `@maestro groom` comment so a mention fires grooming only once. Wired
	// through the 3-way merge (mergeSpecLintTracks) latest-write-wins so a
	// concurrent orchestrator Save cannot clobber a fresh lint mark.
	SpecLintTracks map[int]SpecLintTrack `json:"spec_lint_tracks,omitempty"`

	// SpecGroomCursor is the issue number where the last spec-groom cycle
	// stopped examining (#851 review). The per-cycle cap only lets a bounded
	// window of open issues be examined (comment fetch + LLM pass); the cursor
	// lets the next cycle resume just past that window and wrap around, so a
	// repo with more open issues than the cap eventually drains every issue
	// instead of forever re-examining the same first N and starving the tail.
	// Best-effort: it only steers which window runs, never lint/groom
	// idempotency (guaranteed by SpecLintTracks), so a merge that keeps the
	// on-disk value simply re-examines a window and self-heals next cycle.
	SpecGroomCursor int `json:"spec_groom_cursor,omitempty"`

	// MaterialProgress is the durable per-project stalled-progress watermark
	// and last recovery decision (#887). It records the last material progress
	// identity/time across the full lifecycle (issue → worker → PR/CI/review →
	// merge/release → approval-gated delivery) and survives daemon restart
	// without resetting or duplicating the deadline, because the deadline is
	// always derived from Watermark.At + the silence budget. nil until the
	// watchdog has evaluated at least once.
	MaterialProgress *MaterialProgress `json:"material_progress,omitempty"`

	loadedHash  string
	loadedState *State
}

// ReviewRepairTrack is one (pr_number, head_sha) idempotency record for
// the auto review-repair respawn (#565). It carries the count of repair
// workers spawned for this exact head and the terminal state once the
// budget is exhausted, so the supervisor neither double-spawns nor loops
// after the head SHA has been retired.
type ReviewRepairTrack struct {
	PRNumber          int       `json:"pr_number"`
	HeadSHA           string    `json:"head_sha"`
	IssueNumber       int       `json:"issue_number,omitempty"`
	Attempts          int       `json:"attempts"`
	LastDecisionAt    time.Time `json:"last_decision_at,omitempty"`
	LastApprovalID    string    `json:"last_approval_id,omitempty"`
	Exhausted         bool      `json:"exhausted,omitempty"`
	ExhaustedAt       time.Time `json:"exhausted_at,omitempty"`
	UnresolvedSummary string    `json:"unresolved_summary,omitempty"`
}

// reviewRepairTrackKey is the JSON-stable key used in ReviewRepairTracks.
// Empty headSHA falls back to a sentinel so a missing SHA never collapses
// two distinct heads under the same key.
func reviewRepairTrackKey(pr int, headSHA string) string {
	head := strings.TrimSpace(headSHA)
	if head == "" {
		head = "unknown"
	}
	return fmt.Sprintf("PR#%d@%s", pr, head)
}

// LookupReviewRepairTrack returns the existing (pr,head_sha) record if
// any. Callers use this to gate a fresh recommendation (already at
// budget → fall through to operator) or to bump the attempt counter on
// dispatch.
func (s *State) LookupReviewRepairTrack(prNumber int, headSHA string) (ReviewRepairTrack, bool) {
	if s == nil || prNumber <= 0 {
		return ReviewRepairTrack{}, false
	}
	track, ok := s.ReviewRepairTracks[reviewRepairTrackKey(prNumber, headSHA)]
	return track, ok
}

// RecordReviewRepairAttempt increments the attempt counter for the given
// (pr,head_sha) pair, capping at maxAttempts. Returns (track, spawned)
// where spawned reports whether this call actually took a slot (false
// when the budget was already exhausted). When spawned=true, the caller
// is free to dispatch the worker; when false the caller should leave the
// PR for operator review.
func (s *State) RecordReviewRepairAttempt(prNumber int, headSHA string, issueNumber int, approvalID string, maxAttempts int, now time.Time) (ReviewRepairTrack, bool) {
	if s == nil || prNumber <= 0 {
		return ReviewRepairTrack{}, false
	}
	if s.ReviewRepairTracks == nil {
		s.ReviewRepairTracks = make(map[string]ReviewRepairTrack)
	}
	key := reviewRepairTrackKey(prNumber, headSHA)
	track := s.ReviewRepairTracks[key]
	track.PRNumber = prNumber
	track.HeadSHA = strings.TrimSpace(headSHA)
	if issueNumber > 0 {
		track.IssueNumber = issueNumber
	}
	if track.Exhausted || (maxAttempts > 0 && track.Attempts >= maxAttempts) {
		track.Exhausted = true
		if track.ExhaustedAt.IsZero() {
			track.ExhaustedAt = normalizedTime(now)
		}
		s.ReviewRepairTracks[key] = track
		return track, false
	}
	track.Attempts++
	track.LastDecisionAt = normalizedTime(now)
	if strings.TrimSpace(approvalID) != "" {
		track.LastApprovalID = approvalID
	}
	if maxAttempts > 0 && track.Attempts >= maxAttempts {
		// Budget reached this attempt — leave Exhausted=false so the
		// dispatched worker can land its fix; the next supervisor cycle
		// that still sees a P0/P1 on this head will flip Exhausted.
	}
	s.ReviewRepairTracks[key] = track
	return track, true
}

// ReleaseReviewRepairAttempt rolls back a single attempt previously taken by
// RecordReviewRepairAttempt for (pr,head_sha) — used when the dispatch that
// claimed the slot failed to actually start a worker (#874). A failed start
// must not burn an attempt from the bounded repair budget, or a run of start
// failures would exhaust the budget and permanently reject an approved repair
// that never reached a worker. Decrements the counter (never below zero) and
// clears an Exhausted flag whose timestamp coincides with this rolled-back
// attempt. No-op when there is no track or no attempt to release.
func (s *State) ReleaseReviewRepairAttempt(prNumber int, headSHA string, now time.Time) {
	if s == nil || prNumber <= 0 || s.ReviewRepairTracks == nil {
		return
	}
	key := reviewRepairTrackKey(prNumber, headSHA)
	track, ok := s.ReviewRepairTracks[key]
	if !ok || track.Attempts <= 0 {
		return
	}
	track.Attempts--
	// A just-claimed attempt never sets Exhausted (RecordReviewRepairAttempt
	// leaves it false on the budget-reaching attempt), but stay defensive: if
	// the released attempt was the one that tipped the pair into Exhausted,
	// undo it so the freed slot is dispatchable again.
	if track.Attempts == 0 {
		track.Exhausted = false
		track.ExhaustedAt = time.Time{}
	}
	track.LastDecisionAt = normalizedTime(now)
	s.ReviewRepairTracks[key] = track
}

// MarkReviewRepairExhausted records that a (pr,head_sha) pair has run
// out of repair attempts and the operator must take over. Idempotent —
// calling twice does not multiply the timestamp.
func (s *State) MarkReviewRepairExhausted(prNumber int, headSHA string, summary string, now time.Time) ReviewRepairTrack {
	if s == nil || prNumber <= 0 {
		return ReviewRepairTrack{}
	}
	if s.ReviewRepairTracks == nil {
		s.ReviewRepairTracks = make(map[string]ReviewRepairTrack)
	}
	key := reviewRepairTrackKey(prNumber, headSHA)
	track := s.ReviewRepairTracks[key]
	track.PRNumber = prNumber
	track.HeadSHA = strings.TrimSpace(headSHA)
	if !track.Exhausted {
		track.Exhausted = true
		track.ExhaustedAt = normalizedTime(now)
	}
	if strings.TrimSpace(summary) != "" {
		track.UnresolvedSummary = summary
	}
	s.ReviewRepairTracks[key] = track
	return track
}

type ProjectStatusSync struct {
	Status   string    `json:"status"`
	SyncedAt time.Time `json:"synced_at,omitempty"`
}

func NewState() *State {
	return &State{
		Sessions:            make(map[string]*Session),
		Missions:            make(map[int]*Mission),
		ProjectStatusSync:   make(map[int]ProjectStatusSync),
		BackendHealth:       make(map[string]BackendHealth),
		ProviderModelHealth: make(map[string]map[string]BackendHealth),
		SpecLintTracks:      make(map[int]SpecLintTrack),
		PRGateSnapshots:     make(map[string]PRGateSnapshot),
		NextSlot:            1,
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

// Store abstracts persistence of orchestration State so call-sites need not
// depend on the package-level Load/Save free functions directly. The default
// jsonStore implementation (NewJSONStore) wraps the existing JSON-file path —
// flock + 3-way merge on Save — so its behavior is identical to calling
// Load/Save directly. A future database-backed implementation can satisfy the
// same contract without touching call-sites. The typed accessors/mutators
// (Sessions, Approvals, RecordSupervisorDecision, …) live on the *State value
// that Load returns and Save persists.
type Store interface {
	// Load returns the current persisted State, or a fresh empty State when
	// none has been written yet.
	Load() (*State, error)
	// Save persists s, merging independent concurrent writes exactly as the
	// package-level Save does (flock + 3-way merge).
	Save(s *State) error
}

// jsonStore is the default Store backed by <dir>/state.json. It is a thin
// wrapper over the package-level Load/Save free functions, so its on-disk
// format and concurrency semantics are unchanged.
type jsonStore struct {
	dir string
}

// NewJSONStore returns the default file-backed Store rooted at stateDir.
func NewJSONStore(stateDir string) Store {
	return &jsonStore{dir: stateDir}
}

// StateDir returns the directory this store persists to.
func (j *jsonStore) StateDir() string { return j.dir }

// Load implements Store by delegating to the package-level Load.
func (j *jsonStore) Load() (*State, error) { return Load(j.dir) }

// Save implements Store by delegating to the package-level Save.
func (j *jsonStore) Save(s *State) error { return Save(j.dir, s) }

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
	s.normalizeDeliveryApprovals()
	s.rememberLoaded(data)
	return s, nil
}

// SaveHook is invoked after every successful Save, with the state dir that was
// written and the just-persisted snapshot. It is the single write-through
// chokepoint that lets a long-lived process (the daemon) mirror EVERY state.Save
// — orchestrator, supervisor, watchdog, server action — into a secondary store
// (the shared maestro.db, #760) without threading a store handle through every
// call site. The hook runs synchronously while the per-state flock is STILL held,
// so the mirror write is serialised with the JSON write: two concurrent Saves for
// the same dir mirror in the order they wrote JSON, and the mirror can never
// settle on a snapshot older than the JSON. A hook MUST NOT re-enter Save/Load
// for stateDir (it already holds the flock) and should be fast. A nil hook (the
// default) leaves the JSON-only path unchanged.
type SaveHook func(stateDir string, s *State)

var (
	saveHookMu sync.RWMutex
	saveHook   SaveHook
)

// SetSaveHook installs (or clears, with nil) the process-global write-through
// hook invoked after every successful Save. It is process-global because Save is
// a free function reached from many packages; the daemon sets it once when the
// SQLite mirror is enabled and clears it otherwise.
func SetSaveHook(h SaveHook) {
	saveHookMu.Lock()
	saveHook = h
	saveHookMu.Unlock()
}

func currentSaveHook() SaveHook {
	saveHookMu.RLock()
	defer saveHookMu.RUnlock()
	return saveHook
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

	if err := saveLocked(stateDir, s); err != nil {
		return err
	}
	// Write-through mirror (#760): replicate the just-persisted snapshot into the
	// secondary store while STILL holding the flock, so the mirror is serialised
	// with the JSON write and cannot regress to an older snapshot under concurrent
	// Saves. Nil unless the daemon registered a mirror, so the JSON-only path is
	// unchanged.
	if hook := currentSaveHook(); hook != nil {
		hook(stateDir, s)
	}
	return nil
}

// Update applies fn to the latest state snapshot while holding the per-project
// flock, then persists that exact mutation before releasing the lock. It is the
// compare-and-swap boundary for operations that must validate current identity
// immediately before changing durable ownership, such as watchdog recovery
// lease claims. fn must not perform external side effects or re-enter Load/Save.
func Update(stateDir string, fn func(*State) error) error {
	if fn == nil {
		return fmt.Errorf("update state: nil callback")
	}
	if err := os.MkdirAll(stateDir, 0755); err != nil {
		return fmt.Errorf("create state dir: %w", err)
	}
	unlock, err := lockState(stateDir)
	if err != nil {
		return err
	}
	defer unlock()

	current, data, err := readStateFile(StatePath(stateDir))
	if err != nil {
		return err
	}
	current.rememberLoaded(data)
	if err := fn(current); err != nil {
		if errors.Is(err, ErrNoStateChange) {
			return nil
		}
		return err
	}
	if err := saveLocked(stateDir, current); err != nil {
		return err
	}
	if hook := currentSaveHook(); hook != nil {
		hook(stateDir, current)
	}
	return nil
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
	// Final persistence boundary: a concurrent current snapshot may have been
	// written by an older binary carrying delivery free text. Project again
	// after the 3-way merge so neither state.json nor SaveHook can reintroduce
	// command/target/output/summary secrets.
	desired.normalizeDeliveryApprovals()
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
	s.normalizeDeliveryApprovals()
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
	if s.ProviderModelHealth == nil {
		s.ProviderModelHealth = make(map[string]map[string]BackendHealth)
	}
	if s.SpecLintTracks == nil {
		s.SpecLintTracks = make(map[int]SpecLintTrack)
	}
	if s.PRGateSnapshots == nil {
		s.PRGateSnapshots = make(map[string]PRGateSnapshot)
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
	s.LessonProposals = src.LessonProposals
	s.OutcomeHealth = src.OutcomeHealth
	s.OutcomeGateStreaks = src.OutcomeGateStreaks
	s.OutcomeGateStreakCheckedAt = src.OutcomeGateStreakCheckedAt
	s.ProjectStatusSync = src.ProjectStatusSync
	s.BackendHealth = src.BackendHealth
	s.ProviderModelHealth = src.ProviderModelHealth
	s.BackendQuotaUsage = src.BackendQuotaUsage
	s.SpecLintTracks = src.SpecLintTracks
	s.PRGateSnapshots = src.PRGateSnapshots
	s.SpecGroomCursor = src.SpecGroomCursor
	s.NextSlot = src.NextSlot
	s.LastMergeAt = src.LastMergeAt
	s.SpawnDrain = src.SpawnDrain
	s.SpawnDrainAt = src.SpawnDrainAt
	s.Paused = src.Paused
	s.PausedAt = src.PausedAt
	s.MaterialProgress = src.MaterialProgress
	// Keep the caller's in-memory snapshot aligned with the merged file. Save
	// calls copyFrom after a three-way merge, then rememberLoaded records the
	// merged file hash. Omitting the heartbeat tuple here leaves a long-lived
	// writer (notably the material-progress watchdog) holding an older pulse
	// while believing it loaded the current file. Its next non-conflicting save
	// can then regress LastRunOnceAt and resurrect an obsolete stuck verdict.
	s.LastRunOnceAt = src.LastRunOnceAt
	s.SupervisorStuck = src.SupervisorStuck
	s.SupervisorStuckReason = src.SupervisorStuckReason
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
	if err := mergeLessonProposals(merged, base, current, ours); err != nil {
		return nil, err
	}
	merged.OutcomeHealth = mergeOutcomeHealth(base.OutcomeHealth, current.OutcomeHealth, ours.OutcomeHealth)
	merged.OutcomeRecovery = mergeOutcomeRecovery(base.OutcomeRecovery, current.OutcomeRecovery, ours.OutcomeRecovery)
	merged.OutcomeGateStreaks, merged.OutcomeGateStreakCheckedAt = mergeOutcomeGateStreaks(current, ours)
	merged.ProjectStatusSync = mergeProjectStatusSync(current.ProjectStatusSync, ours.ProjectStatusSync)
	merged.SpecLintTracks = mergeSpecLintTracks(current.SpecLintTracks, ours.SpecLintTracks)
	merged.PRGateSnapshots = mergePRGateSnapshots(current.PRGateSnapshots, ours.PRGateSnapshots)
	merged.BackendHealth = mergeBackendHealth(current.BackendHealth, ours.BackendHealth)
	merged.ProviderModelHealth = mergeProviderModelHealth(current.ProviderModelHealth, ours.ProviderModelHealth)
	merged.BackendQuotaUsage = mergeBackendQuotaUsage(current.BackendQuotaUsage, ours.BackendQuotaUsage)
	merged.NextSlot = mergeMonotonicInt(base.NextSlot, current.NextSlot, ours.NextSlot)
	merged.LastMergeAt = mergeLatestTime(base.LastMergeAt, current.LastMergeAt, ours.LastMergeAt)
	mergeSpawnDrain(merged, current, ours)
	mergePaused(merged, current, ours)
	mergeSupervisorHeartbeat(merged, current, ours)
	mergeMaterialProgress(merged, current, ours)
	return merged, nil
}

// mergeSupervisorHeartbeat preserves the newest completed supervisor pulse
// across ordinary three-way merges. The orchestrator and material-progress
// evaluator write the same state concurrently; before this field-specific
// merge, their otherwise-compatible writes silently kept current's old pulse
// while accepting the new supervisor decision. Stuck state follows the pulse
// that produced it. At an equal pulse, a watchdog's stuck=true wins so an
// unrelated save cannot erase a real overdue verdict; a later RunOnce clears it
// by advancing LastRunOnceAt.
func mergeSupervisorHeartbeat(merged, current, ours *State) {
	switch {
	case ours.LastRunOnceAt.After(current.LastRunOnceAt):
		merged.LastRunOnceAt = ours.LastRunOnceAt
		merged.SupervisorStuck = ours.SupervisorStuck
		merged.SupervisorStuckReason = ours.SupervisorStuckReason
	case current.LastRunOnceAt.After(ours.LastRunOnceAt):
		merged.LastRunOnceAt = current.LastRunOnceAt
		merged.SupervisorStuck = current.SupervisorStuck
		merged.SupervisorStuckReason = current.SupervisorStuckReason
	default:
		merged.LastRunOnceAt = current.LastRunOnceAt
		if current.SupervisorStuck || ours.SupervisorStuck {
			merged.SupervisorStuck = true
			merged.SupervisorStuckReason = current.SupervisorStuckReason
			if merged.SupervisorStuckReason == "" {
				merged.SupervisorStuckReason = ours.SupervisorStuckReason
			}
			return
		}
		merged.SupervisorStuck = false
		merged.SupervisorStuckReason = ""
	}
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

// mergePaused resolves the pause flag (#683) latest-write-wins by PausedAt,
// mirroring mergeSpawnDrain: `maestro pause` sets Paused=true and `maestro
// resume` clears it — both stamp PausedAt — so picking the snapshot with the
// newer timestamp keeps a concurrent orchestrator save from clobbering a
// fresh pause request, and a fresh resume from being undone by a stale set.
func mergePaused(merged, current, ours *State) {
	if ours.PausedAt.After(current.PausedAt) {
		merged.Paused = ours.Paused
		merged.PausedAt = ours.PausedAt
		return
	}
	merged.Paused = current.Paused
	merged.PausedAt = current.PausedAt
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

func mergeProviderModelHealth(current, ours map[string]map[string]BackendHealth) map[string]map[string]BackendHealth {
	if len(current) == 0 && len(ours) == 0 {
		return nil
	}
	merged := make(map[string]map[string]BackendHealth)
	for _, provider := range unionBackendHealthKeysForNested(current, ours) {
		models := mergeBackendHealth(current[provider], ours[provider])
		if len(models) > 0 {
			merged[provider] = models
		}
	}
	return merged
}

func unionBackendHealthKeysForNested(maps ...map[string]map[string]BackendHealth) []string {
	seen := make(map[string]struct{})
	for _, values := range maps {
		for key := range values {
			seen[key] = struct{}{}
		}
	}
	keys := make([]string, 0, len(seen))
	for key := range seen {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
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

func mergeLessonProposals(merged, base, current, ours *State) error {
	merged.LessonProposals = cloneState(current).LessonProposals
	keys := unionStringSets(lessonProposalKeys(base.LessonProposals), lessonProposalKeys(current.LessonProposals), lessonProposalKeys(ours.LessonProposals))
	for _, key := range keys {
		baseValue, baseOK := lessonProposalByKey(base.LessonProposals, key)
		currentValue, currentOK := lessonProposalByKey(current.LessonProposals, key)
		oursValue, oursOK := lessonProposalByKey(ours.LessonProposals, key)
		resolved, keep, err := resolveListValue("lesson proposal "+key, baseValue, baseOK, currentValue, currentOK, oursValue, oursOK)
		if err != nil {
			return err
		}
		if keep {
			merged.LessonProposals = upsertLessonProposal(merged.LessonProposals, resolved.(LessonProposal))
		} else {
			merged.LessonProposals = deleteLessonProposal(merged.LessonProposals, key)
		}
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
	clone.Checks = append([]outcome.HealthCheckItem(nil), value.Checks...)
	return &clone
}

func mergeOutcomeRecovery(base, current, ours *outcome.RecoveryState) *outcome.RecoveryState {
	latest := current
	if latest == nil || (ours != nil && ours.UpdatedAt.After(latest.UpdatedAt)) {
		latest = ours
	}
	if latest == nil {
		latest = base
	}
	return cloneOutcomeRecovery(latest)
}

func cloneOutcomeRecovery(value *outcome.RecoveryState) *outcome.RecoveryState {
	if value == nil {
		return nil
	}
	clone := *value
	if value.ExitCode != nil {
		code := *value.ExitCode
		clone.ExitCode = &code
	}
	return &clone
}

// mergeOutcomeGateStreaks keeps the streak table whose folded-run watermark is
// newest. The table and its watermark advance together on every scheduled fold,
// so the newer watermark carries the authoritative counters and emission marks.
func mergeOutcomeGateStreaks(current, ours *State) ([]outcome.GateStreak, time.Time) {
	winner := current
	if winner == nil || (ours != nil && ours.OutcomeGateStreakCheckedAt.After(winner.OutcomeGateStreakCheckedAt)) {
		winner = ours
	}
	if winner == nil {
		return nil, time.Time{}
	}
	return append([]outcome.GateStreak(nil), winner.OutcomeGateStreaks...), winner.OutcomeGateStreakCheckedAt
}

// RecordOutcomeGateStreaks folds one scheduled health check result into the
// per-gate streak table and returns the streaks that reached the escalation
// threshold with a pending notification or intake. It records each distinct
// scheduled run at most once: a result whose CheckedAt is not newer than the
// last folded run is ignored, so overlapping evaluators never double-count.
func (s *State) RecordOutcomeGateStreaks(result outcome.HealthCheckResult, threshold int, runLink string, now time.Time) []outcome.GateStreakEvent {
	if s == nil {
		return nil
	}
	if !result.CheckedAt.IsZero() && !result.CheckedAt.After(s.OutcomeGateStreakCheckedAt) {
		return nil
	}
	next, events := outcome.RecordGateStreaks(s.OutcomeGateStreaks, result, threshold, runLink, now)
	s.OutcomeGateStreaks = next
	if !result.CheckedAt.IsZero() {
		s.OutcomeGateStreakCheckedAt = result.CheckedAt.UTC()
	}
	return events
}

// MarkGateStreakNotified records that the gate_fail_streak notification for a
// fingerprint has been sent, deduping repeat same-fingerprint escalations.
func (s *State) MarkGateStreakNotified(gate, fingerprint string, now time.Time) {
	if s == nil {
		return
	}
	for i := range s.OutcomeGateStreaks {
		if s.OutcomeGateStreaks[i].Gate == gate && s.OutcomeGateStreaks[i].Fingerprint == fingerprint {
			s.OutcomeGateStreaks[i].NotifiedFingerprint = fingerprint
			return
		}
	}
}

// MarkGateStreakIntaken records that the deduped repair issue for a fingerprint
// has been filed, so a repeat same-fingerprint failure never re-files.
func (s *State) MarkGateStreakIntaken(gate, fingerprint string, issue int, now time.Time) {
	if s == nil {
		return
	}
	for i := range s.OutcomeGateStreaks {
		if s.OutcomeGateStreaks[i].Gate == gate && s.OutcomeGateStreaks[i].Fingerprint == fingerprint {
			s.OutcomeGateStreaks[i].IntakeFingerprint = fingerprint
			if issue > 0 {
				s.OutcomeGateStreaks[i].IntakeIssue = issue
			}
			return
		}
	}
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

func lessonProposalKeys(proposals []LessonProposal) map[string]bool {
	keys := make(map[string]bool)
	for _, proposal := range proposals {
		keys[lessonProposalKey(proposal)] = true
	}
	return keys
}

func lessonProposalByKey(proposals []LessonProposal, key string) (LessonProposal, bool) {
	for _, proposal := range proposals {
		if lessonProposalKey(proposal) == key {
			return proposal, true
		}
	}
	return LessonProposal{}, false
}

func lessonProposalKey(proposal LessonProposal) string {
	if proposal.ID != "" {
		return proposal.ID
	}
	if proposal.Fingerprint != "" {
		return "fingerprint:" + proposal.Fingerprint
	}
	return stableHash(proposal)
}

func upsertLessonProposal(proposals []LessonProposal, proposal LessonProposal) []LessonProposal {
	key := lessonProposalKey(proposal)
	for i := range proposals {
		if lessonProposalKey(proposals[i]) == key {
			proposals[i] = proposal
			return proposals
		}
	}
	return append(proposals, proposal)
}

func deleteLessonProposal(proposals []LessonProposal, key string) []LessonProposal {
	filtered := proposals[:0]
	for _, proposal := range proposals {
		if lessonProposalKey(proposal) != key {
			filtered = append(filtered, proposal)
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
		// #750: same decision identity (action, target) → update the live
		// approval IN PLACE and return it under its existing, stable id.
		// Re-evaluation must NOT supersede + re-mint a sibling just because
		// the volatile runtime snapshot (session status, retry count, PR
		// head) moved between cycles — that churn staled the freshest id the
		// operator just read, so `supervise approve <id>` and the SPA button
		// raced a moving target every cycle (dogfood 2026-06-20: 170 stale +
		// 81 superseded in history; merge_pr / spawn_repair_worker were
		// un-actionable). The previous behaviour returned the existing record
		// untouched only when the target-state snapshot was identical and
		// otherwise superseded+re-minted; both branches now collapse to a
		// single in-place refresh. Supersession is reserved for a genuinely
		// different decision (different action/target), which hashes to a
		// different id and falls through to the create path below, and for
		// the dedicated reconcilers (worker started, issue auto-closed). The
		// executor re-validates runtime preconditions at execute time
		// (executeMergePR re-checks PRMergeStatus; the worktree/worker verbs
		// re-check slot reuse), so keeping the approval live across churn does
		// not widen the blast radius.
		s.refreshPendingApproval(existingMatch, decision, freshTargetStateHash, now)
		return existingMatch
	}

	id := approvalID(decision, createdAt)
	if s.approvalIDInUse(id) {
		// A terminal approval for the same decision identity already holds
		// this content-addressed id (the previous instance completed and the
		// same decision recurred). Disambiguate so ids stay unique; the LIVE
		// pending record is refreshed in place above and never reaches here,
		// so this never re-introduces per-cycle churn on an effective gate.
		id = id + "-" + createdAt.UTC().Format("20060102T150405.000000000Z")
	}

	approval := Approval{
		ID:              id,
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
		// #874: persist the review-repair payload on the durable approval so
		// the orchestrator dispatcher can converge from the approval alone,
		// without needing the coincident LatestSupervisorDecision to still be
		// a spawn_review_repair carrying the payload.
		ReviewRepair: cloneReviewRepairPayload(decision.ReviewRepair),
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

// refreshPendingApproval updates a still-effective (pending or
// awaiting_dispatch) approval in place from a fresh re-evaluation of the same
// decision identity, preserving its id, creation time, status, and audit
// trail so the record stays stable and approvable across supervise cycles
// (#750). Only the re-derived decision content (summary, risk, evidence,
// repo/project) and the informational target-state snapshot are refreshed;
// the recomputed PayloadHash keeps ensureApprovalCurrent's internal-
// consistency check satisfied. No audit entry is appended — a re-evaluation
// that changed nothing material must not churn the audit log (that noise was
// part of the #750 symptom).
func (s *State) refreshPendingApproval(approval *Approval, decision SupervisorDecision, targetStateHash string, now time.Time) {
	if approval == nil {
		return
	}
	approval.Summary = decision.Summary
	approval.Risk = decision.Risk
	approval.Evidence = append([]string(nil), decision.Reasons...)
	if strings.TrimSpace(decision.Repo) != "" {
		approval.Repo = decision.Repo
	}
	if strings.TrimSpace(decision.Project) != "" {
		approval.Project = decision.Project
	}
	// #874: refresh the durable review-repair payload from a re-minted
	// decision that still carries one (a fresh supervisor cycle observed new
	// findings on the same head). Never clobber a good payload with nil — a
	// non-review-repair re-evaluation of the same target identity must not
	// strip the proof the dispatcher needs.
	if decision.ReviewRepair != nil {
		approval.ReviewRepair = cloneReviewRepairPayload(decision.ReviewRepair)
	}
	approval.TargetStateHash = targetStateHash
	approval.UpdatedAt = normalizedTime(now)
	approval.PayloadHash = approval.ComputePayloadHash()
}

// EffectiveReviewRepairPayloadForPR returns a proven review-repair payload
// (head SHA, blocking findings, backend) for the given PR, searched first on
// still-effective approvals and then on the recent supervisor decisions
// (#874). It is the "prove it or reject it" source for a manual
// spawn_review_repair enqueue: the manual control cannot compute review
// findings itself, so it reuses a payload the supervisor already observed on
// this PR. Returns (nil, nil, false) when no such proof exists — the caller
// must then reject the enqueue rather than mint an approval no dispatcher can
// complete. The returned payload/target are deep copies safe to stamp onto a
// fresh decision.
func (s *State) EffectiveReviewRepairPayloadForPR(prNumber int) (*SupervisorReviewRepairPayload, *SupervisorTarget, bool) {
	if s == nil || prNumber <= 0 {
		return nil, nil, false
	}
	// Effective approvals win: they are durable across the bounded decision
	// ring and already carry the payload since #874. Newest UpdatedAt first.
	var bestApproval *Approval
	for i := range s.Approvals {
		a := &s.Approvals[i]
		if a.Action != approvalActionSpawnReviewRepair || a.ReviewRepair == nil {
			continue
		}
		switch a.Status {
		case ApprovalStatusPending, ApprovalStatusApproved, ApprovalStatusAwaitingDispatch:
		default:
			continue
		}
		if a.Target == nil || a.Target.PR != prNumber {
			continue
		}
		if bestApproval == nil || a.UpdatedAt.After(bestApproval.UpdatedAt) {
			bestApproval = a
		}
	}
	if bestApproval != nil {
		return cloneReviewRepairPayload(bestApproval.ReviewRepair), cloneSupervisorTarget(bestApproval.Target), true
	}
	// Fall back to the most recent supervisor decision that observed blocking
	// findings on this PR.
	var best *SupervisorDecision
	for i := range s.SupervisorDecisions {
		d := &s.SupervisorDecisions[i]
		if d.RecommendedAction != approvalActionSpawnReviewRepair || d.ReviewRepair == nil {
			continue
		}
		if d.Target == nil || d.Target.PR != prNumber {
			continue
		}
		if best == nil || d.CreatedAt.After(best.CreatedAt) {
			best = d
		}
	}
	if best == nil {
		return nil, nil, false
	}
	return cloneReviewRepairPayload(best.ReviewRepair), cloneSupervisorTarget(best.Target), true
}

// approvalIDInUse reports whether any approval already carries id. Used by the
// content-addressed mint path to disambiguate a fresh approval whose decision
// identity collides with a terminal (executed/superseded/stale) record from a
// previous instance of the same decision (#750).
func (s *State) approvalIDInUse(id string) bool {
	for i := range s.Approvals {
		if s.Approvals[i].ID == id {
			return true
		}
	}
	return false
}

// RecordLessonProposal records a recurring-failure lesson candidate and mints a
// linked pending approval. If the same failure class, source session, and
// suggested rule already exists, it updates the durable proposal metadata
// without re-opening resolved proposals or appending another same-id approval.
func (s *State) RecordLessonProposal(proposal LessonProposal, now time.Time, repo, project string) (*LessonProposal, *Approval, bool) {
	if s == nil {
		return nil, nil, false
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	proposal.CreatedAt = normalizedTime(firstNonZeroTime(proposal.CreatedAt, now))
	proposal.UpdatedAt = normalizedTime(now)
	proposal.FailureClass = strings.TrimSpace(proposal.FailureClass)
	proposal.Area = strings.TrimSpace(proposal.Area)
	proposal.MinimalRepro = strings.TrimSpace(proposal.MinimalRepro)
	proposal.SuggestedRule = strings.TrimSpace(proposal.SuggestedRule)
	proposal.Target = strings.TrimSpace(proposal.Target)
	if proposal.Target == "" {
		proposal.Target = LessonProposalTargetAgentsMD
	}
	if proposal.Status == "" {
		proposal.Status = LessonProposalStatusPending
	}
	if proposal.Fingerprint == "" {
		proposal.Fingerprint = LessonProposalFingerprint(proposal.FailureClass, lessonProposalSourceSession(&proposal), proposal.SuggestedRule)
	}
	if proposal.ID == "" {
		proposal.ID = lessonProposalID(proposal.Fingerprint)
	}
	if proposal.FailureClass == "" || proposal.Area == "" || proposal.SuggestedRule == "" {
		return nil, nil, false
	}
	for i := range s.LessonProposals {
		existing := &s.LessonProposals[i]
		if existing.Fingerprint != proposal.Fingerprint && !lessonProposalEquivalent(*existing, proposal) {
			continue
		}
		existing.UpdatedAt = proposal.UpdatedAt
		existing.MinimalRepro = proposal.MinimalRepro
		existing.SuggestedRule = proposal.SuggestedRule
		existing.Target = proposal.Target
		existing.SourceDecision = proposal.SourceDecision
		existing.SourceTarget = cloneSupervisorTarget(proposal.SourceTarget)
		if existing.ID == "" {
			existing.ID = proposal.ID
		}
		if existing.FailureClass == "" {
			existing.FailureClass = proposal.FailureClass
		}
		if existing.Area == "" {
			existing.Area = proposal.Area
		}
		approval := s.findApprovalByLessonProposalID(existing.ID)
		if approval != nil {
			existing.ApprovalID = approval.ID
			if approval.DecisionID == "" {
				approval.DecisionID = existing.SourceDecision
				approval.PayloadHash = approval.ComputePayloadHash()
			}
			if existing.Status == LessonProposalStatusPending && approval.Status == ApprovalStatusPending {
				approval.UpdatedAt = proposal.UpdatedAt
				approval.Evidence = []string{existing.MinimalRepro, existing.SuggestedRule}
				approval.Repo = repo
				approval.Project = project
				approval.PayloadHash = approval.ComputePayloadHash()
			}
		}
		return existing, approval, false
	}

	s.LessonProposals = append(s.LessonProposals, proposal)
	stored := &s.LessonProposals[len(s.LessonProposals)-1]
	approval := Approval{
		ID:               "approval-" + stored.ID,
		CreatedAt:        stored.CreatedAt,
		UpdatedAt:        stored.UpdatedAt,
		Action:           "apply_lesson_proposal",
		DecisionID:       stored.SourceDecision,
		Summary:          fmt.Sprintf("Apply lesson proposal for %s in %s.", stored.FailureClass, stored.Area),
		Risk:             "approval_gated",
		Evidence:         []string{stored.MinimalRepro, stored.SuggestedRule},
		Status:           ApprovalStatusPending,
		Repo:             repo,
		Project:          project,
		LessonProposalID: stored.ID,
	}
	approval.PayloadHash = approval.ComputePayloadHash()
	approval.Audit = append(approval.Audit, ApprovalAudit{
		At:          stored.UpdatedAt,
		Event:       ApprovalAuditCreated,
		PayloadHash: approval.PayloadHash,
	})
	s.Approvals = append(s.Approvals, approval)
	stored.ApprovalID = approval.ID
	return stored, &s.Approvals[len(s.Approvals)-1], true
}

func firstNonZeroTime(values ...time.Time) time.Time {
	for _, value := range values {
		if !value.IsZero() {
			return value
		}
	}
	return time.Time{}
}

func LessonProposalFingerprint(failureClass, sourceSession, suggestedRule string) string {
	return stableHash(struct {
		FailureClass  string `json:"failure_class"`
		SourceSession string `json:"source_session"`
		SuggestedRule string `json:"suggested_rule"`
	}{
		FailureClass:  strings.ToLower(strings.TrimSpace(failureClass)),
		SourceSession: strings.ToLower(strings.TrimSpace(sourceSession)),
		SuggestedRule: strings.TrimSpace(suggestedRule),
	})
}

func lessonProposalEquivalent(a, b LessonProposal) bool {
	return strings.EqualFold(strings.TrimSpace(a.FailureClass), strings.TrimSpace(b.FailureClass)) &&
		strings.EqualFold(lessonProposalSourceSession(&a), lessonProposalSourceSession(&b)) &&
		strings.TrimSpace(a.SuggestedRule) == strings.TrimSpace(b.SuggestedRule)
}

func lessonProposalSourceSession(proposal *LessonProposal) string {
	if proposal == nil || proposal.SourceTarget == nil {
		return ""
	}
	return strings.TrimSpace(proposal.SourceTarget.Session)
}

func lessonProposalID(fingerprint string) string {
	fp := strings.TrimSpace(fingerprint)
	if len(fp) > 12 {
		fp = fp[:12]
	}
	if fp == "" {
		fp = time.Now().UTC().Format("20060102T150405")
	}
	return "lesson-" + fp
}

func (s *State) findApprovalByLessonProposalID(id string) *Approval {
	var fallback *Approval
	for i := range s.Approvals {
		if s.Approvals[i].LessonProposalID == id {
			approval := &s.Approvals[i]
			switch approval.Status {
			case ApprovalStatusPending, ApprovalStatusApproved, ApprovalStatusAwaitingDispatch:
				return approval
			}
			if fallback == nil {
				fallback = approval
			}
		}
	}
	return fallback
}

func (s *State) FindLessonProposal(id string) (*LessonProposal, bool) {
	for i := range s.LessonProposals {
		proposal := &s.LessonProposals[i]
		if proposal.ID == id || proposal.Fingerprint == id {
			return proposal, true
		}
	}
	return nil, false
}

func (s *State) MarkLessonProposalApplied(id string, now time.Time, actor, note string) bool {
	proposal, ok := s.FindLessonProposal(id)
	if !ok {
		return false
	}
	at := normalizedTime(now)
	proposal.Status = LessonProposalStatusApplied
	proposal.UpdatedAt = at
	proposal.AppliedAt = &at
	proposal.ResolutionActor = actor
	proposal.ResolutionNote = note
	return true
}

func (s *State) MarkLessonProposalDeclined(id string, now time.Time, actor, note string) bool {
	proposal, ok := s.FindLessonProposal(id)
	if !ok {
		return false
	}
	at := normalizedTime(now)
	proposal.Status = LessonProposalStatusDeclined
	proposal.UpdatedAt = at
	proposal.DeclinedAt = &at
	proposal.ResolutionActor = actor
	proposal.ResolutionNote = note
	return true
}

// approvalTargetsEqual returns true if two SupervisorTarget pointers refer
// to the same target identity (issue/pr/head_sha/session). Used by the
// at-mint dedup check in RecordPendingApprovalForDecision. HeadSHA is
// part of the identity (#565) so a new head SHA — i.e. the repair worker
// pushed an update — produces a distinct approval instead of being
// coalesced as a duplicate of the stale one.
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
	if !supervisorIssueTargetsEqual(a.Issues, b.Issues) {
		return false
	}
	if strings.TrimSpace(a.HeadSHA) != strings.TrimSpace(b.HeadSHA) {
		return false
	}
	if strings.TrimSpace(a.Session) != strings.TrimSpace(b.Session) {
		return false
	}
	return true
}

func supervisorIssueTargetsEqual(a, b []SupervisorIssueTarget) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Issue != b[i].Issue || a[i].PR != b[i].PR {
			return false
		}
	}
	return true
}

func (s *State) FindApproval(id string) (*Approval, bool) {
	if approval, ok := s.findApprovalByIDAndStatus(id, ApprovalStatusPending); ok {
		return approval, true
	}
	if approval, ok := s.findApprovalByIDAndStatus(id, ApprovalStatusApproved); ok {
		return approval, true
	}
	if approval, ok := s.findApprovalByIDAndStatus(id, ApprovalStatusAwaitingDispatch); ok {
		return approval, true
	}
	for i := range s.Approvals {
		approval := &s.Approvals[i]
		if approvalMatchesID(approval, id) {
			return approval, true
		}
	}
	return nil, false
}

func (s *State) findApprovalByIDAndStatus(id string, status ApprovalStatus) (*Approval, bool) {
	for i := range s.Approvals {
		approval := &s.Approvals[i]
		if approval.Status == status && approvalMatchesID(approval, id) {
			return approval, true
		}
	}
	return nil, false
}

func approvalMatchesID(approval *Approval, id string) bool {
	if approval == nil {
		return false
	}
	if approval.ID == id || approval.DecisionID == id {
		return true
	}
	return false
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
	if approval.Action == ApprovalActionDeployProject {
		reason = deliveryAuditReason(ApprovalAuditApproved)
	}
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
	if approval.Action == ApprovalActionDeployProject {
		reason = deliveryAuditReason(ApprovalAuditRejected)
	}
	approval.Audit = append(approval.Audit, ApprovalAudit{
		At:              approval.UpdatedAt,
		Event:           ApprovalAuditRejected,
		Actor:           actor,
		Reason:          reason,
		PayloadHash:     approval.PayloadHash,
		TargetStateHash: approval.TargetStateHash,
	})
	if approval.LessonProposalID != "" {
		s.MarkLessonProposalDeclined(approval.LessonProposalID, approval.UpdatedAt, actor, reason)
	}
	return approval, nil
}

// MarkStaleApprovals marks a pending approval stale only when its payload
// drifts out of sync with its stored PayloadHash (an internal-consistency
// guard). As of #750 it no longer stales on target-state-snapshot drift:
// re-evaluating the same (action, target) decision refreshes the approval in
// place, and the volatile runtime snapshot moving between cycles must not make
// the freshest id un-approvable. Genuinely-moot approvals are retired by the
// dedicated reconcilers (ReconcileSpawnWorkerApprovalsForStartedWorkers,
// MarkCloseIssueApprovalsStaleForVerifiedIssue, ...) or by the executor's
// execute-time re-validation.
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

// MarkCloseIssueApprovalsStaleForVerifiedIssue expires in-flight close_issue
// approvals that became moot because Maestro already closed the verified,
// merged issue on the orchestrator trust path.
func (s *State) MarkCloseIssueApprovalsStaleForVerifiedIssue(issueNumber int, now time.Time) int {
	if s == nil || issueNumber <= 0 {
		return 0
	}
	count := 0
	reason := fmt.Sprintf("issue #%d auto-closed after verified merge", issueNumber)
	for i := range s.Approvals {
		approval := &s.Approvals[i]
		if !closeIssueApprovalTargetsIssue(approval, issueNumber) {
			continue
		}
		switch approval.Status {
		case ApprovalStatusPending, ApprovalStatusApproved, ApprovalStatusAwaitingDispatch:
			s.markApprovalStale(approval, now, reason)
			count++
		}
	}
	return count
}

func closeIssueApprovalTargetsIssue(approval *Approval, issueNumber int) bool {
	if approval == nil || approval.Target == nil || issueNumber <= 0 {
		return false
	}
	switch approval.Action {
	case approvalActionCloseIssue:
		return approval.Target.Issue == issueNumber
	case approvalActionCloseIssueBatch:
		for _, target := range approval.Target.Issues {
			if target.Issue == issueNumber {
				return true
			}
		}
	}
	return false
}

// MarkSpawnRepairWorkerApprovalsStaleForResolvedIssue expires in-flight
// spawn_repair_worker approvals that became moot because the issue they were
// going to repair is resolved — the orchestrator auto-closed it after a verified
// merge, so there is nothing left to repair. Without this the pending approval
// lingers indefinitely and surfaces as a Past-SLA red flag on the Approvals
// dashboard (dogfood #773/#774/#775: repair approvals outlived their merged PRs;
// the operator had to reject them by hand every wave). Mirrors
// MarkCloseIssueApprovalsStaleForVerifiedIssue, co-located at the same
// auto-close trust path.
func (s *State) MarkSpawnRepairWorkerApprovalsStaleForResolvedIssue(issueNumber int, now time.Time) int {
	reason := fmt.Sprintf("issue #%d resolved (verified merge) — repair worker moot", issueNumber)
	return len(s.StaleSpawnRepairWorkerApprovalsForResolvedIssue(issueNumber, now, reason))
}

// StaleSpawnRepairWorkerApprovalsForResolvedIssue is the reason-carrying core of
// MarkSpawnRepairWorkerApprovalsStaleForResolvedIssue. It expires every active
// (pending/approved/awaiting_dispatch) spawn_repair_worker approval targeting
// issueNumber and returns the staled approvals (post-transition copies) so the
// caller can emit a per-approval operator journal record — approval id +
// issue/PR target — and mirror the terminal transition into the SQLite approval
// store (#866). The reason distinguishes the terminal outcome (verified merge vs
// externally closed) in the audit trail. Idempotent: an approval already stale
// is skipped by markApprovalStale, so re-running returns nothing new.
func (s *State) StaleSpawnRepairWorkerApprovalsForResolvedIssue(issueNumber int, now time.Time, reason string) []Approval {
	if s == nil || issueNumber <= 0 {
		return nil
	}
	if strings.TrimSpace(reason) == "" {
		reason = fmt.Sprintf("issue #%d resolved — repair worker moot", issueNumber)
	}
	var staled []Approval
	for i := range s.Approvals {
		approval := &s.Approvals[i]
		if approval.Action != approvalActionSpawnRepairWorker || approval.Target == nil || approval.Target.Issue != issueNumber {
			continue
		}
		switch approval.Status {
		case ApprovalStatusPending, ApprovalStatusApproved, ApprovalStatusAwaitingDispatch:
			s.markApprovalStale(approval, now, reason)
			staled = append(staled, *approval)
		}
	}
	return staled
}

// SupersedeReviewRepairApprovalsForStaleHead supersedes every still-active
// spawn_review_repair approval for prNumber whose durable payload targets a
// head SHA other than currentHead (#874 changed-head). Dispatching such an
// approval would repair a revision that no longer exists on the PR, so the
// approval is superseded instead — the next supervisor cycle re-evaluates the
// current head and mints a fresh proof if findings remain. Returns the ids
// superseded.
func (s *State) SupersedeReviewRepairApprovalsForStaleHead(prNumber int, currentHead string, now time.Time, reason string) []string {
	if s == nil || prNumber <= 0 {
		return nil
	}
	current := strings.TrimSpace(currentHead)
	var ids []string
	for i := range s.Approvals {
		approval := &s.Approvals[i]
		if approval.Action != approvalActionSpawnReviewRepair || approval.ReviewRepair == nil {
			continue
		}
		if approval.Target == nil || approval.Target.PR != prNumber {
			continue
		}
		if strings.TrimSpace(approval.ReviewRepair.HeadSHA) == current && current != "" {
			continue
		}
		// Supersede from any active state, Approved included — a changed head
		// makes an already-approved (but not yet dispatched) repair target a
		// stale revision that must not reach a worker (#874).
		if s.supersedeApprovalFrom(approval, now, reason,
			ApprovalStatusPending, ApprovalStatusApproved, ApprovalStatusAwaitingDispatch) {
			ids = append(ids, approval.ID)
		}
	}
	return ids
}

// ResolveDispatchedReviewRepairApproval supersedes the effective
// spawn_review_repair approval with the given id once the orchestrator has
// actually dispatched its worker (#874). This keeps the durable-approval
// dispatch path from re-resolving (and re-reading the PR head) every cycle,
// and leaves no active approval behind after a repair reaches a worker.
// Returns true when an active approval was superseded.
func (s *State) ResolveDispatchedReviewRepairApproval(id string, now time.Time, reason string) bool {
	if s == nil || strings.TrimSpace(id) == "" {
		return false
	}
	for i := range s.Approvals {
		approval := &s.Approvals[i]
		if approval.ID != id || approval.Action != approvalActionSpawnReviewRepair {
			continue
		}
		// The durable-approval dispatch path spawns from an Approved (or
		// AwaitingDispatch) record, so the terminal transition must cover
		// Approved too — otherwise the approval stays active and re-selectable
		// while SQLite is told it is terminal (#874).
		return s.supersedeApprovalFrom(approval, now, reason,
			ApprovalStatusPending, ApprovalStatusApproved, ApprovalStatusAwaitingDispatch)
	}
	return false
}

// ActiveSpawnRepairWorkerApprovalIssues returns the sorted, distinct set of
// issue numbers that still carry an active (pending/approved/awaiting_dispatch)
// spawn_repair_worker approval. The orchestrator's standing reconciler (#866)
// uses it to check only issues with a repair approval actually outstanding,
// rather than scanning every session or every open issue each cycle.
func (s *State) ActiveSpawnRepairWorkerApprovalIssues() []int {
	if s == nil {
		return nil
	}
	seen := make(map[int]struct{})
	for i := range s.Approvals {
		approval := &s.Approvals[i]
		if approval.Action != approvalActionSpawnRepairWorker || approval.Target == nil || approval.Target.Issue <= 0 {
			continue
		}
		switch approval.Status {
		case ApprovalStatusPending, ApprovalStatusApproved, ApprovalStatusAwaitingDispatch:
			seen[approval.Target.Issue] = struct{}{}
		}
	}
	if len(seen) == 0 {
		return nil
	}
	issues := make([]int, 0, len(seen))
	for issue := range seen {
		issues = append(issues, issue)
	}
	sort.Ints(issues)
	return issues
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
	if approval.Action == ApprovalActionDeployProject && approval.Delivery.DeliveryExpired(now) {
		s.markApprovalStale(approval, now, "delivery approval expired before execution")
		return ErrApprovalStale
	}
	if approval.PayloadHash != "" && approval.ComputePayloadHash() != approval.PayloadHash {
		s.markApprovalStale(approval, now, "approval payload changed")
		return ErrApprovalPayloadMismatch
	}
	// #750: a pending approval is NOT staled merely because the volatile
	// runtime snapshot (session status, retry count, PR head) drifted since it
	// was minted. The approval authorizes a decision identified by (action,
	// target); that identity is unchanged here (the payload hash matched), and
	// RecordPendingApprovalForDecision refreshes the record in place each
	// cycle instead of re-minting. Staling on target-state churn is exactly
	// what made the freshest id un-approvable a cycle later — the CLI/SPA
	// approve path raced a moving target every supervise cycle (dogfood
	// 2026-06-20: two `supervise approve <freshest-id>` attempts both returned
	// "approval is stale"). Runtime preconditions are re-validated by the
	// executor at execute time (e.g. executeMergePR re-checks PRMergeStatus;
	// the delete/stop/restart verbs re-check slot reuse), so dropping the
	// approve-time target-state gate does not weaken the safety envelope.
	return nil
}

func (s *State) markApprovalStale(approval *Approval, now time.Time, reason string) {
	if approval.Status == ApprovalStatusStale {
		return
	}
	approval.Status = ApprovalStatusStale
	approval.UpdatedAt = normalizedTime(now)
	if approval.Action == ApprovalActionDeployProject {
		if approval.Delivery != nil {
			switch {
			case strings.Contains(reason, "config changed"):
				approval.Delivery.StaleCause = DeliveryStaleCauseConfigDrift
			case strings.Contains(reason, "expired"):
				approval.Delivery.StaleCause = DeliveryStaleCauseExpired
			case strings.Contains(reason, "payload changed"), strings.Contains(reason, "integrity"):
				approval.Delivery.StaleCause = DeliveryStaleCauseIntegrity
			default:
				approval.Delivery.StaleCause = DeliveryStaleCauseOther
			}
		}
		reason = deliveryAuditReason(ApprovalAuditStale)
	}
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
	// Approved is intentionally excluded here: an approved record is queued
	// in the supervisor executor (ApprovalsAwaitingExecution), so a general
	// reconciler must not yank it out from under the executor.
	s.supersedeApprovalFrom(approval, now, reason, ApprovalStatusPending, ApprovalStatusAwaitingDispatch)
}

// supersedeApprovalFrom transitions approval → superseded when its current
// status is one of allowedFrom, appending an audit entry, and reports whether
// the transition happened. It is the shared core of markApprovalSuperseded; the
// review-repair dispatch path (#874) also supersedes from Approved because the
// orchestrator dispatches a durable review-repair approval directly — racing
// the supervisor executor that would otherwise move it Approved →
// AwaitingDispatch — so the just-dispatched (or stale-head) approval must become
// terminal from whichever active state it is observed in. Returning true only on
// a real transition keeps callers from reporting success (and mirroring a
// terminal status to SQLite) while the record silently stays active.
func (s *State) supersedeApprovalFrom(approval *Approval, now time.Time, reason string, allowedFrom ...ApprovalStatus) bool {
	transition := false
	for _, from := range allowedFrom {
		if approval.Status == from {
			transition = true
			break
		}
	}
	if !transition {
		return false
	}
	approval.Status = ApprovalStatusSuperseded
	approval.UpdatedAt = normalizedTime(now)
	if approval.Action == ApprovalActionDeployProject {
		reason = deliveryAuditReason(ApprovalAuditSuperseded)
	}
	approval.Audit = append(approval.Audit, ApprovalAudit{
		At:              approval.UpdatedAt,
		Event:           ApprovalAuditSuperseded,
		Reason:          reason,
		PayloadHash:     approval.PayloadHash,
		TargetStateHash: s.ApprovalTargetStateHash(approval.Target),
	})
	return true
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
		DecisionID:       a.DecisionID,
		Action:           a.Action,
		Target:           a.Target,
		Summary:          a.Summary,
		Risk:             a.Risk,
		Evidence:         a.Evidence,
		LessonProposalID: a.LessonProposalID,
		Delivery:         a.Delivery.identity(),
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
	DecisionID       string                   `json:"decision_id,omitempty"`
	Action           string                   `json:"action"`
	Target           *SupervisorTarget        `json:"target,omitempty"`
	Summary          string                   `json:"summary"`
	Risk             string                   `json:"risk"`
	Evidence         []string                 `json:"evidence,omitempty"`
	LessonProposalID string                   `json:"lesson_proposal_id,omitempty"`
	Delivery         *deliveryPayloadIdentity `json:"delivery,omitempty"`
}

// deliveryPayloadIdentity is the immutable subset of a DeliveryPayload folded
// into ComputePayloadHash. It deliberately excludes the mutable execution
// result (output, timings, verified) so recording a run does not drift the
// payload hash and stale the approval mid-flight, while the pinned MergedSHA
// (and target/rollback/preview) IS hashed so a superseding merge for a
// different revision is a genuinely different payload.
type deliveryPayloadIdentity struct {
	Project            string    `json:"project,omitempty"`
	Repo               string    `json:"repo,omitempty"`
	PR                 int       `json:"pr,omitempty"`
	Issue              int       `json:"issue,omitempty"`
	MergedSHA          string    `json:"merged_sha"`
	MergedAt           time.Time `json:"merged_at,omitempty"`
	ApprovalGeneration int       `json:"approval_generation,omitempty"`
	TargetLabel        string    `json:"target_label,omitempty"`
	VerificationLabel  string    `json:"verification_label,omitempty"`
	RollbackLabel      string    `json:"rollback_label,omitempty"`
	TimeoutMinutes     int       `json:"timeout_minutes,omitempty"`
	ConfigDigest       string    `json:"config_digest"`
	ExpiresAt          time.Time `json:"expires_at,omitempty"`
}

func (p *DeliveryPayload) identity() *deliveryPayloadIdentity {
	if p == nil {
		return nil
	}
	return &deliveryPayloadIdentity{
		Project:            p.Project,
		Repo:               p.Repo,
		PR:                 p.PR,
		Issue:              p.Issue,
		MergedSHA:          p.MergedSHA,
		MergedAt:           p.MergedAt,
		ApprovalGeneration: p.ApprovalGeneration,
		TargetLabel:        p.TargetLabel,
		VerificationLabel:  p.VerificationLabel,
		RollbackLabel:      p.RollbackLabel,
		TimeoutMinutes:     p.TimeoutMinutes,
		ConfigDigest:       p.ConfigDigest,
		ExpiresAt:          p.ExpiresAt,
	}
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
	// #750: content-address the approval id on the decision IDENTITY —
	// (action, target) — NOT the per-cycle decision.ID or mint timestamp.
	// Re-evaluating the same logical decision across supervise cycles then
	// yields the SAME id, so the approval minted in cycle N stays the live,
	// approvable record in cycle N+k. The old timestamp/decision-id keying
	// minted a fresh sibling id every cycle (and staled the prior one), so
	// `maestro supervise approve <id>` and the SPA button always raced a
	// moving target — the gate was un-actionable for the very verbs that
	// matter (merge_pr / spawn_repair_worker). Two decisions that share an
	// identity intentionally collapse to one approval (the idempotency the
	// gate needs); a genuinely different decision (different action/target)
	// hashes to a different id. RecordPendingApprovalForDecision updates the
	// matching approval in place rather than minting a sibling under a new id.
	if key := approvalDecisionIdentityHash(decision.RecommendedAction, decision.Target); key != "" {
		return "approval-" + key
	}
	// Defensive fallback: a decision with neither an action nor a target
	// identity (never produced by a real gated mint) keeps the legacy keying
	// so two such records cannot collide on an empty hash.
	if decision.ID != "" {
		return "approval-" + decision.ID
	}
	return "approval-" + createdAt.UTC().Format("20060102T150405.000000000Z")
}

// approvalDecisionIdentityHash returns a stable 16-hex digest of a decision's
// identity: the (action, target) tuple that determines WHICH side effect an
// approval authorizes. The volatile per-cycle runtime snapshot folded into
// ApprovalTargetStateHash (session status, retry count, next-retry time) is
// deliberately excluded so the digest is stable across supervise cycles
// (#750). Target.HeadSHA IS part of the identity — a decision stamped against
// a specific PR head (e.g. spawn_review_repair, #565) genuinely changes when
// the head moves. Returns "" when there is nothing to address (no action and
// no target). Whitespace in Session/HeadSHA is trimmed so the digest matches
// approvalTargetsEqual's trimmed comparison.
func approvalDecisionIdentityHash(action string, target *SupervisorTarget) string {
	action = strings.TrimSpace(action)
	if action == "" && target == nil {
		return ""
	}
	full := stableHash(approvalDecisionIdentity{
		Action: action,
		Target: normalizedTargetIdentity(target),
	})
	if len(full) > 16 {
		return full[:16]
	}
	return full
}

type approvalDecisionIdentity struct {
	Action string            `json:"action"`
	Target *SupervisorTarget `json:"target,omitempty"`
}

// normalizedTargetIdentity returns a copy of target with Session/HeadSHA
// trimmed, so the content-addressed approval id is robust to incidental
// whitespace and consistent with approvalTargetsEqual. Body is cleared: for
// edit_issue_body (#851) the identity is (action, issue), so a re-groom with a
// fresh rewrite refreshes the pending approval in place rather than minting a
// duplicate under a new content-addressed id — matching approvalTargetsEqual,
// which also ignores Body.
func normalizedTargetIdentity(target *SupervisorTarget) *SupervisorTarget {
	if target == nil {
		return nil
	}
	clone := cloneSupervisorTarget(target)
	clone.HeadSHA = strings.TrimSpace(clone.HeadSHA)
	clone.Session = strings.TrimSpace(clone.Session)
	clone.Body = ""
	return clone
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
	for _, issue := range target.Issues {
		if issue.Issue > 0 && issue.Issue == sess.IssueNumber {
			return true
		}
		if issue.PR > 0 && issue.PR == sess.PRNumber {
			return true
		}
	}
	return false
}

func cloneSupervisorTarget(target *SupervisorTarget) *SupervisorTarget {
	if target == nil {
		return nil
	}
	clone := *target
	clone.Issues = append([]SupervisorIssueTarget(nil), target.Issues...)
	return &clone
}

// cloneReviewRepairPayload deep-copies a review-repair payload so an
// approval carries its own copy independent of the source decision (#874).
func cloneReviewRepairPayload(payload *SupervisorReviewRepairPayload) *SupervisorReviewRepairPayload {
	if payload == nil {
		return nil
	}
	clone := *payload
	clone.Findings = append([]SupervisorReviewFinding(nil), payload.Findings...)
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

// CapacityInput carries the concurrency knobs that govern how many live
// implementation workers may be spawned. The caller populates it from config so
// the state package stays free of a config import.
//
// #814: pr_open (PR-gate) sessions have no live worker process — their PID is
// cleared when the PR is opened and Maestro only waits on CI / review / the
// merge gate. Counting them against MaxParallel makes the pipeline look "full"
// while it is merely gate-bound, so a long PRD-backed queue with a few open PRs
// stops dispatching new implementation work and Mission Control shows 0 live
// workers. When MaxLiveWorkers > 0, spawn capacity is computed from live
// workers alone and pr_open sessions no longer consume a slot.
type CapacityInput struct {
	MaxParallel          int
	MaxLiveWorkers       int
	MaxConcurrentByState map[string]int
}

// Capacity is an operator-facing snapshot that separates live implementation
// workers (StatusRunning) from PR-gate sessions (StatusPROpen) so Mission
// Control can explain why a project is or is not spawning new work instead of
// only reporting a single conflated "workers" number.
type Capacity struct {
	LiveWorkers    int  // StatusRunning — worker processes actively implementing
	PRGates        int  // StatusPROpen — PR open, no live process, waiting on gates
	Limit          int  // effective limit that governs live-worker spawning
	CapacityUsed   int  // slots counted as in-use for spawn accounting
	AvailableSlots int  // free slots available for new live workers
	BlockedByGates int  // live-worker slots withheld purely because pr_open sessions count against capacity (0 once gates are separated out)
	Separated      bool // true when MaxLiveWorkers>0, i.e. pr_open sessions do not consume live-worker capacity
}

// GateBound reports whether PR-gate sessions are the reason no live worker can
// spawn: there are open PRs, no live worker is implementing, and no slot is
// free. This is the misleading "0 workers but not idle" state from #814.
func (c Capacity) GateBound() bool {
	return c.LiveWorkers == 0 && c.PRGates > 0 && c.AvailableSlots == 0
}

// Capacity computes the live-worker spawn budget, separating live workers from
// PR-gate sessions per the CapacityInput knobs. It is the single source of
// truth for "how many new workers can start" shared by the orchestrator, the
// supervisor, and Mission Control.
func (s *State) Capacity(in CapacityInput) Capacity {
	var counts map[SessionStatus]int
	if s != nil {
		counts = s.CountByStatus()
	}
	live := counts[StatusRunning]
	gates := counts[StatusPROpen]

	c := Capacity{LiveWorkers: live, PRGates: gates}

	if in.MaxLiveWorkers > 0 {
		// Separated accounting: pr_open sessions do not consume live-worker
		// capacity. Spawning is governed by live workers alone.
		c.Separated = true
		c.Limit = in.MaxLiveWorkers
		c.CapacityUsed = live
		c.AvailableSlots = in.MaxLiveWorkers - live
		c.BlockedByGates = 0
	} else {
		// Legacy accounting: running + pr_open both count against max_parallel.
		c.Limit = in.MaxParallel
		active := live + gates
		c.CapacityUsed = active
		c.AvailableSlots = in.MaxParallel - active
		// How many additional live workers could run if gates were separated
		// out of the budget — the slots pr_open sessions are withholding.
		if headroom := in.MaxParallel - live; headroom > 0 && gates > 0 {
			c.BlockedByGates = min(gates, headroom)
		}
	}
	if c.AvailableSlots < 0 {
		c.AvailableSlots = 0
	}

	// The per-state "running" limit caps live-worker spawning in either mode —
	// new workers always enter StatusRunning. Non-"running" per-state limits
	// (e.g. "pr_open") intentionally do not gate dispatch.
	if limit, ok := in.MaxConcurrentByState["running"]; ok && limit > 0 {
		runningSlots := limit - live
		if runningSlots < 0 {
			runningSlots = 0
		}
		if runningSlots < c.AvailableSlots {
			c.AvailableSlots = runningSlots
		}
	}
	return c
}

// ProjectActivity is a single operator-facing token that explains why a project
// is or is not turning eligible issues into implementation work. It lets Mission
// Control distinguish "idle because the queue is empty" from "idle because every
// slot is a PR gate" at a glance (#814).
type ProjectActivity string

const (
	ActivityImplementing         ProjectActivity = "implementing"            // at least one live worker is running
	ActivityBlockedByGates       ProjectActivity = "blocked_by_pr_gates"     // no live worker; capacity is consumed by pr_open sessions while eligible work waits
	ActivityWaitingOnGates       ProjectActivity = "waiting_on_pr_gates"     // no live worker, PRs open, and no eligible work left — just waiting for gates
	ActivityBlockedByApprovals   ProjectActivity = "blocked_by_approvals"    // dispatch is held pending an operator approval decision
	ActivityBlockedByModelLimits ProjectActivity = "blocked_by_model_limits" // every eligible backend is blocked by a model/usage limit
	ActivityNeedsAttention       ProjectActivity = "needs_attention"         // no live worker; canonical actionable work requires repair/reconciliation
	ActivityPaused               ProjectActivity = "paused"                  // operator paused the project
	ActivityQueueEmpty           ProjectActivity = "queue_empty"             // no eligible ready issues remain
	ActivityIdle                 ProjectActivity = "idle"                    // idle for none of the more specific reasons above
)

// ActivityInput carries the non-session signals ClassifyActivity needs beyond
// the session-derived Capacity snapshot.
type ActivityInput struct {
	Capacity            Capacity
	EligibleIssues      int  // ready issues that could be dispatched now
	PendingApprovals    int  // spawn/merge approvals awaiting an operator decision
	ActionableAttention int  // canonical, non-self-resolving worker/session blockers
	BackendsBlocked     bool // every eligible backend is blocked by a model/usage limit
	Paused              bool // operator paused the project
}

// ClassifyActivity returns the ProjectActivity token and a concise
// operator-facing explanation for why a project is (not) implementing. The
// ordering is deliberate: live work wins, then the specific block reasons, so an
// operator always sees the most actionable cause rather than a generic "idle".
func ClassifyActivity(in ActivityInput) (ProjectActivity, string) {
	c := in.Capacity
	if c.LiveWorkers > 0 {
		return ActivityImplementing, fmt.Sprintf("Implementing: %d live worker(s) running%s.", c.LiveWorkers, prGateSuffix(c.PRGates))
	}
	if in.Paused {
		return ActivityPaused, "Project is paused; dispatch is intentionally suspended."
	}
	if in.BackendsBlocked {
		return ActivityBlockedByModelLimits, "Blocked by model limits: every eligible backend is rate/usage limited."
	}
	if in.PendingApprovals > 0 {
		return ActivityBlockedByApprovals, fmt.Sprintf("Blocked by approvals: %d decision(s) awaiting an operator.", in.PendingApprovals)
	}
	if in.ActionableAttention > 0 {
		return ActivityNeedsAttention, fmt.Sprintf("Needs attention: %d actionable worker/session blocker(s) require repair or reconciliation.", in.ActionableAttention)
	}
	// Gate-bound: no live worker and no free slot while PRs are open. Only a
	// problem when eligible work is waiting — that is the recurring #814
	// intervention loop. With eligible work drained it is simply "waiting",
	// including separated-concurrency projects where PR gates intentionally do
	// not consume otherwise-free implementation slots. Calling that state
	// queue_empty hides the real gate/outcome work still in flight.
	if c.PRGates > 0 {
		if in.EligibleIssues > 0 && c.AvailableSlots == 0 {
			return ActivityBlockedByGates, fmt.Sprintf("Blocked by PR gates: %d PR gate(s) hold all capacity while %d ready issue(s) wait; raise max_live_workers or max_parallel to keep implementing.", c.PRGates, in.EligibleIssues)
		}
		if in.EligibleIssues <= 0 {
			return ActivityWaitingOnGates, fmt.Sprintf("Waiting on PR gates: %d PR(s) open, no eligible work left to dispatch.", c.PRGates)
		}
	}
	if in.EligibleIssues <= 0 {
		return ActivityQueueEmpty, "Queue empty: no eligible ready issues to dispatch."
	}
	return ActivityIdle, "Idle: eligible work exists and capacity is free; awaiting the next dispatch cycle."
}

func prGateSuffix(gates int) string {
	if gates <= 0 {
		return ""
	}
	return fmt.Sprintf(" (%d PR gate(s) waiting on CI/review)", gates)
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

// SetPaused marks the project paused (#683): the orchestrator skips issue
// selection and spawns no new workers, while in-flight workers finish
// normally. The timestamp is stamped so concurrent state writers resolve
// pause on/off latest-write-wins.
func (s *State) SetPaused(at time.Time) {
	if s == nil {
		return
	}
	s.Paused = true
	s.PausedAt = normalizedTime(at)
}

// ClearPaused lifts an operator pause. Called by `maestro resume`; the
// orchestrator picks the change up on its next cycle without a restart.
func (s *State) ClearPaused(at time.Time) {
	if s == nil {
		return
	}
	s.Paused = false
	s.PausedAt = normalizedTime(at)
}

// PauseActive reports whether the project is currently paused by an operator.
func (s *State) PauseActive() bool {
	return s != nil && s.Paused
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

// SessionAt returns the session currently bound to slot, if any —
// regardless of session status (running, done, failed, dead). No
// liveness filter is applied; callers that need one must compose with
// SessionLive / SessionLiveAt explicitly.
//
// Used by the approver executor (#488 slot-reuse fence) to verify a
// delete_worktree approval still targets the issue that was running
// when the approval was queued. The fence intentionally treats any
// session at the slot — including a freshly-terminated one — as the
// authoritative slot owner, since a terminated session for issue #N
// still indicates the slot was most recently bound to #N.
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
	case DisplayReviewRetryBackoff, DisplayReviewRetryPending, DisplayReviewRetryRunning, DisplayReviewRetryRecheck, DisplayWaitingForIssueGuard:
		return true
	}

	if SessionAttentionForAt(sess, nil, now).NeedsAttention && SessionAttentionActionableAt(sess, now) {
		return true
	}

	changedAt := SessionChangedAt(sess)
	return !changedAt.IsZero() && now.Sub(changedAt.UTC()) <= LiveSessionRecentWindow
}

// FleetAttentionTTL is the freshness window during which a non-progressing
// session (dead, failed, conflict_failed, or retry_exhausted without an open
// PR) still counts as actionable operator attention. Older sessions are
// reconcilable/archivable, not actionable, and must not be surfaced as
// `live, needs_attention` on the fleet snapshot (#566).
const FleetAttentionTTL = 24 * time.Hour

// SessionAttentionActionableAt reports whether a session that
// SessionAttentionForAt classifies as needs_attention is still inside the
// actionable window for fleet surfaces.
//
// A retry_exhausted session that still has an open PR (PRNumber > 0) is
// always actionable: a green-but-gate-blocked PR with no respawn is exactly
// what a human needs to look at (#564). A session with a scheduled retry
// (NextRetryAt set) is also always actionable. Beyond those, dead / failed /
// conflict_failed / retry_exhausted-without-PR sessions age out after
// FleetAttentionTTL so a deploy 2 days ago cannot dominate the verdict.
//
// Statuses that intrinsically represent live work (running, pr_open, queued)
// are always actionable for as long as the session exists; they are not the
// "stale dead worker" failure mode this gate guards against.
func SessionAttentionActionableAt(sess *Session, now time.Time) bool {
	return sessionAttentionActionableAt(sess, now, FleetAttentionTTL)
}

func sessionAttentionActionableAt(sess *Session, now time.Time, ttl time.Duration) bool {
	if sess == nil {
		return false
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	switch sess.Status {
	case StatusRunning, StatusPROpen, StatusQueued, StatusCodeLanded:
		return true
	}
	if sess.NextRetryAt != nil {
		return true
	}
	if sess.Status == StatusRetryExhausted && sess.PRNumber > 0 {
		return true
	}
	if ttl <= 0 {
		return true
	}
	changedAt := SessionChangedAt(sess)
	if changedAt.IsZero() {
		return true
	}
	return now.Sub(changedAt) <= ttl
}

// MarkWorkerEnded stamps WorkerEndedAt with `now` the FIRST time the worker
// process is observed to have stopped. Subsequent calls are no-ops, so a
// later status transition (pr_open -> code_landed -> done) cannot move the
// recorded agent-exit time forward. Callers should invoke this before/at
// any transition out of StatusRunning (running -> pr_open / dead / failed /
// retry_exhausted / conflict_failed / code_landed / done).
//
// See #426: dashboard "Runtime" used to conflate agent execution with
// PR-open / CI / Greptile / merge waiting because FinishedAt was rewritten
// at every status change.
func MarkWorkerEnded(sess *Session, now time.Time) {
	if sess == nil || sess.WorkerEndedAt != nil {
		return
	}
	t := normalizedTime(now)
	sess.WorkerEndedAt = &t
}

// MarkPROpened stamps PROpenedAt with `now` the FIRST time the session
// enters StatusPROpen. Subsequent calls are no-ops, so a session that
// flips pr_open -> running -> pr_open during a review-retry loop keeps
// the original PR-open timestamp. Used by the dashboard to attribute
// pr_open_runtime (PROpenedAt -> FinishedAt) to orchestration latency
// rather than agent execution.
func MarkPROpened(sess *Session, now time.Time) {
	if sess == nil || sess.PROpenedAt != nil {
		return
	}
	t := normalizedTime(now)
	sess.PROpenedAt = &t
}

// MaxBackendCooldownTTL caps how long a BackendHealth cooldown entry stays
// surfaced when no RetryAfter was parsed from the provider message. Without
// this cap, a transient provider rate-limit would render as
// "auto-recovery pending" indefinitely even though the backend is back in
// active use (#600). 24h is the longest a Claude/Codex provider limit
// realistically takes to reset.
const MaxBackendCooldownTTL = 24 * time.Hour

// ReconcileBackendHealth walks the BackendHealth map and clears stale
// cooldown entries so the MC BACKENDS panel reflects reality (#600):
//
//   - RetryAfter is non-nil and in the past → cooldown has elapsed; the
//     selector already treats this backend as available, so the panel
//     should too.
//   - RetryAfter is nil but Since is older than MaxBackendCooldownTTL →
//     a stale provider-limit cooldown that was never cleared because no
//     RetryAfter was parsed. Caps the "auto-recovery pending" indefinite
//     state.
//   - A session bound to this backend started after Since AND produced
//     PR evidence (PROpen / CodeLanded with PRNumber > 0) → the backend
//     is demonstrably back in service.
//
// StatusDone is intentionally excluded as recovery evidence: a session
// can land in StatusDone via external issue closure (orchestrator
// checkSessions) without the backend ever producing useful output, so
// trusting it would silently clear a still-rate-limited backend.
//
// Returns true when at least one entry was cleared so the caller can
// decide whether to persist the change.
func ReconcileBackendHealth(s *State, now time.Time) bool {
	if s == nil || (len(s.BackendHealth) == 0 && len(s.ProviderModelHealth) == 0) {
		return false
	}
	changed := false
	for name, health := range s.BackendHealth {
		if health.State != BackendHealthCooldown {
			continue
		}
		if health.RetryAfter != nil && !now.Before(*health.RetryAfter) {
			delete(s.BackendHealth, name)
			changed = true
			continue
		}
		if health.RetryAfter == nil && !health.Since.IsZero() && now.Sub(health.Since) >= MaxBackendCooldownTTL {
			delete(s.BackendHealth, name)
			changed = true
			continue
		}
		// PR evidence proves the backend works, which clears auth and
		// provider-limit cooldowns — but a quota_pressure gate (#704) is
		// predictive, not a malfunction: the backend keeps producing PRs
		// while pressured. It clears only via RetryAfter (window reset)
		// or the orchestrator's quota reconcile dropping below threshold.
		if health.Reason != BackendBlockQuotaPressure && backendHasPRSuccessAfter(s, name, health.Since) {
			delete(s.BackendHealth, name)
			changed = true
			continue
		}
	}
	for provider, models := range s.ProviderModelHealth {
		for model, health := range models {
			if health.State != BackendHealthCooldown {
				continue
			}
			if health.RetryAfter != nil && !now.Before(*health.RetryAfter) {
				delete(models, model)
				changed = true
				continue
			}
			if health.RetryAfter == nil && !health.Since.IsZero() && now.Sub(health.Since) >= MaxBackendCooldownTTL {
				delete(models, model)
				changed = true
			}
		}
		if len(models) == 0 {
			delete(s.ProviderModelHealth, provider)
		}
	}
	return changed
}

// backendHasPRSuccessAfter reports whether any session bound to backend
// started after `since` and produced PR evidence — i.e. reached PROpen
// or CodeLanded with a non-zero PRNumber. StatusDone alone does not
// count because it can be set when an issue is closed externally, which
// is not proof the backend produced useful output (#600 review).
func backendHasPRSuccessAfter(s *State, backend string, since time.Time) bool {
	if s == nil || backend == "" {
		return false
	}
	for _, sess := range s.Sessions {
		if sess == nil || sess.Backend != backend {
			continue
		}
		if sess.PRNumber <= 0 {
			continue
		}
		switch sess.Status {
		case StatusPROpen, StatusCodeLanded:
		default:
			continue
		}
		if sess.StartedAt.IsZero() {
			continue
		}
		if since.IsZero() || sess.StartedAt.After(since) {
			return true
		}
	}
	return false
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

// IssueInProgress returns true if the given issue already has a durable claim.
// Claims include active sessions, scheduled retries, retained open-PR
// maintenance work, and approved repair dispatch reservations.
func (s *State) IssueInProgress(issueNum int) bool {
	_, ok := s.IssueClaimFor(issueNum)
	return ok
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
// Sessions marked as backend-blocked (RateLimitHit — provider capacity limit
// or backend auth failure) are NOT counted as failed attempts: a transient
// backend block must not consume the per-issue retry budget.
// See #432 / #458 / #466 / #693.
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

// IsRetentionEligible reports whether a session in this status is eligible
// for CompactSessions to consider it. Active states (running / pr_open /
// queued and any non-listed state) are never touched. Matches the issue
// #497 spec, which includes code_landed in the retention-eligible set even
// though IsTerminal does not (code_landed is treated as terminal once the
// session falls outside both retention floors).
func IsRetentionEligible(status SessionStatus) bool {
	switch status {
	case StatusDone, StatusFailed, StatusConflictFailed, StatusDead, StatusRetryExhausted, StatusCodeLanded:
		return true
	}
	return false
}

// SessionRetentionPolicy controls CompactSessions.
//
// A retention-eligible session is removed only when it is BOTH outside the
// newest KeepLast window AND older than MinAge. This implements the #497
// rule "keep last N or last D days, whichever is longer". Setting either
// floor to 0 or negative disables that floor.
type SessionRetentionPolicy struct {
	KeepLast    int           // minimum newest-N eligible sessions to retain regardless of age
	MinAge      time.Duration // retain any eligible session younger than this duration
	ArchiveFile string        // if non-empty, append pruned sessions to this JSONL file before delete
}

// SessionCompactionResult is the count summary returned by CompactSessions.
type SessionCompactionResult struct {
	Removed  int // number of sessions deleted from state.Sessions
	Archived int // number of sessions appended to the archive file
}

// archivedSessionRecord is the JSONL line format written to the archive
// file. Each pruned session is serialized as one record per line so the
// archive remains append-only and grep-friendly.
type archivedSessionRecord struct {
	Slot       string    `json:"slot"`
	ArchivedAt time.Time `json:"archived_at"`
	Session    *Session  `json:"session"`
}

// CompactSessions removes retention-eligible sessions from s.Sessions that
// fall outside BOTH the policy's count window (KeepLast newest) AND the
// policy's age window (MinAge). When policy.ArchiveFile is non-empty,
// pruned sessions are first appended to that file as JSONL (one record per
// line) for forensics, mirroring the audit-log pattern. The function is
// idempotent — invoking it repeatedly with the same state and clock is a
// no-op after the first call.
//
// Active sessions (running, pr_open, queued, and any non-listed state) are
// never touched. now is the wall clock used for the age window so callers
// can inject a deterministic clock in tests.
func (s *State) CompactSessions(policy SessionRetentionPolicy, now time.Time) (SessionCompactionResult, error) {
	var result SessionCompactionResult
	if s == nil || len(s.Sessions) == 0 {
		return result, nil
	}

	type candidate struct {
		slot     string
		sess     *Session
		finished time.Time
	}
	eligible := make([]candidate, 0, len(s.Sessions))
	for slot, sess := range s.Sessions {
		if sess == nil || !IsRetentionEligible(sess.Status) {
			continue
		}
		finished := sess.StartedAt
		if sess.FinishedAt != nil {
			finished = *sess.FinishedAt
		}
		eligible = append(eligible, candidate{slot: slot, sess: sess, finished: finished})
	}
	if len(eligible) == 0 {
		return result, nil
	}
	sort.Slice(eligible, func(i, j int) bool {
		if !eligible[i].finished.Equal(eligible[j].finished) {
			return eligible[i].finished.After(eligible[j].finished)
		}
		// Stable tiebreak on slot name so the kept set is deterministic
		// across runs even when several sessions share a FinishedAt.
		return eligible[i].slot < eligible[j].slot
	})

	keepLast := policy.KeepLast
	if keepLast < 0 {
		keepLast = 0
	}

	pruneSlots := make([]candidate, 0)
	for i, c := range eligible {
		if i < keepLast {
			continue
		}
		if policy.MinAge > 0 && now.Sub(c.finished) < policy.MinAge {
			continue
		}
		pruneSlots = append(pruneSlots, c)
	}
	if len(pruneSlots) == 0 {
		return result, nil
	}

	if policy.ArchiveFile != "" {
		archivedAt := now.UTC()
		if err := os.MkdirAll(filepath.Dir(policy.ArchiveFile), 0755); err != nil {
			return result, fmt.Errorf("create archive dir: %w", err)
		}
		f, err := os.OpenFile(policy.ArchiveFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
		if err != nil {
			return result, fmt.Errorf("open archive: %w", err)
		}
		enc := json.NewEncoder(f)
		for _, c := range pruneSlots {
			rec := archivedSessionRecord{Slot: c.slot, ArchivedAt: archivedAt, Session: c.sess}
			if err := enc.Encode(rec); err != nil {
				_ = f.Close()
				return result, fmt.Errorf("write archive record: %w", err)
			}
			result.Archived++
		}
		if err := f.Close(); err != nil {
			return result, fmt.Errorf("close archive: %w", err)
		}
	}

	for _, c := range pruneSlots {
		delete(s.Sessions, c.slot)
		result.Removed++
	}
	return result, nil
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
	if approval.Action == ApprovalActionDeployProject {
		summary = deliveryAuditReason(ApprovalAuditExecuted)
	}
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
	if approval.Action == ApprovalActionDeployProject {
		errMsg = deliveryAuditReason(ApprovalAuditExecutionFailed)
	}
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

// MigrateApprovalsBindRepo back-fills the Repo stamp on any in-flight
// approval that predates #489 (premortem failure mode #3).
//
// Approvals created before #489 were not bound to a project at write-time
// — the executor trusted ambient cfg.Repo. After #489 every fresh approval
// carries Repo + Project (see RecordPendingApprovalForDecision +
// dispatchApprovalAction), and the executor refuses any approval whose
// Repo does not match its cfg.Repo. To roll #489 out without aborting
// already-pending approvals on upgrade, this migration walks Approvals
// that are still in a non-terminal status (pending, awaiting_dispatch,
// approved) and stamps the missing Repo from the caller-supplied repo
// slug. Project stays blank when we cannot infer it — the executor only
// fences on Repo.
//
// Returns the number of approvals stamped. The intended call site is
// once per supervisor cycle: the operation is idempotent (already-stamped
// approvals are skipped), cheap (a single in-memory pass), and converges
// fresh after a daemon restart so a long-running daemon and a freshly
// loaded daemon agree.
//
// Terminal-status approvals (executed / execution_failed / rejected /
// stale / superseded / execution_skipped) are deliberately NOT migrated
// — they are historical records that have already had their side effect
// adjudicated, and rewriting them retroactively would muddy the audit
// trail.
//
// Takes a plain repo string (not *config.Config) on purpose: the state
// package must not depend on internal/config, which would invert the
// existing layering.
func (s *State) MigrateApprovalsBindRepo(repo string) int {
	if s == nil {
		return 0
	}
	repo = strings.TrimSpace(repo)
	if repo == "" {
		return 0
	}
	migrated := 0
	for i := range s.Approvals {
		a := &s.Approvals[i]
		switch a.Status {
		case ApprovalStatusPending,
			ApprovalStatusAwaitingDispatch,
			ApprovalStatusApproved:
		default:
			continue
		}
		if strings.TrimSpace(a.Repo) != "" {
			continue
		}
		a.Repo = repo
		migrated++
	}
	return migrated
}

// MigrateDuplicateApprovalIDs rewrites legacy duplicate approval IDs so the
// actionable approval keeps the canonical operator-facing ID and historical
// twins become individually addressable. It also back-fills lesson approval
// DecisionID links from the durable lesson proposal when older state omitted
// them.
func (s *State) MigrateDuplicateApprovalIDs() int {
	if s == nil || len(s.Approvals) == 0 {
		return 0
	}
	migrated := 0
	proposalsByID := make(map[string]*LessonProposal, len(s.LessonProposals))
	for i := range s.LessonProposals {
		proposal := &s.LessonProposals[i]
		if proposal.ID != "" {
			proposalsByID[proposal.ID] = proposal
		}
	}
	used := make(map[string]int, len(s.Approvals))
	byID := make(map[string][]int)
	for i := range s.Approvals {
		approval := &s.Approvals[i]
		if approval.LessonProposalID != "" && approval.DecisionID == "" {
			if proposal := proposalsByID[approval.LessonProposalID]; proposal != nil && proposal.SourceDecision != "" {
				approval.DecisionID = proposal.SourceDecision
				approval.PayloadHash = approval.ComputePayloadHash()
				migrated++
			}
		}
		id := strings.TrimSpace(approval.ID)
		if id == "" {
			continue
		}
		used[id]++
		byID[id] = append(byID[id], i)
	}
	for id, indexes := range byID {
		if len(indexes) < 2 {
			continue
		}
		canonical := canonicalApprovalIndex(s.Approvals, indexes)
		for _, idx := range indexes {
			if idx == canonical {
				continue
			}
			approval := &s.Approvals[idx]
			oldID := approval.ID
			approval.ID = nextDuplicateApprovalID(id, used)
			if proposal := proposalsByID[approval.LessonProposalID]; proposal != nil && proposal.ApprovalID == oldID {
				proposal.ApprovalID = approval.ID
			}
			migrated++
		}
		for _, idx := range indexes {
			approval := &s.Approvals[idx]
			if proposal := proposalsByID[approval.LessonProposalID]; proposal != nil {
				if proposal.ApprovalID == "" || idx == canonical || approval.Status == ApprovalStatusPending {
					proposal.ApprovalID = approval.ID
				}
			}
		}
	}
	return migrated
}

func canonicalApprovalIndex(approvals []Approval, indexes []int) int {
	if len(indexes) == 0 {
		return -1
	}
	for _, status := range []ApprovalStatus{
		ApprovalStatusPending,
		ApprovalStatusApproved,
		ApprovalStatusAwaitingDispatch,
	} {
		best := -1
		for _, idx := range indexes {
			if approvals[idx].Status != status {
				continue
			}
			if best == -1 || approvals[idx].CreatedAt.After(approvals[best].CreatedAt) {
				best = idx
			}
		}
		if best != -1 {
			return best
		}
	}
	return indexes[0]
}

func nextDuplicateApprovalID(base string, used map[string]int) string {
	n := used[base]
	for {
		n++
		candidate := fmt.Sprintf("%s-legacy-%d", base, n)
		if used[candidate] == 0 {
			used[base] = n
			used[candidate] = 1
			return candidate
		}
	}
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
