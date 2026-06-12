package config

import (
	"crypto/md5"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/befeast/maestro/internal/outcome"
	"gopkg.in/yaml.v3"
)

type TelegramConfig struct {
	Target      string `yaml:"target"`
	BotToken    string `yaml:"bot_token"`
	Mode        string `yaml:"mode"`         // "direct" (Telegram Bot API) or "openclaw" (OpenClaw relay); default: "direct"
	OpenclawURL string `yaml:"openclaw_url"` // only needed when mode=openclaw
	DigestMode  bool   `yaml:"digest_mode"`  // batch notifications per cycle instead of sending immediately
}

// BackendDef defines a model backend CLI.
type BackendDef struct {
	Cmd        string    `yaml:"cmd"`
	ExtraArgs  []string  `yaml:"extra_args"`
	PromptMode string    `yaml:"prompt_mode"` // how to deliver prompt: "arg" (last argument), "stdin" (via stdin), "file" (file path as argument)
	Enabled    *bool     `yaml:"enabled"`     // nil means enabled for backward compatibility
	MCP        MCPConfig `yaml:"mcp,omitempty"`

	// #513: optional per-backend attribution metadata. Lets the
	// dashboard / commit trailer record which provider+model actually
	// produced the work, beyond the shim name. All four are optional;
	// absent fields render as "—" in UI and are omitted from commit
	// trailers. Backends that don't set them keep working unchanged
	// (legacy behaviour preserved).
	Provider string `yaml:"provider,omitempty"` // e.g. "anthropic", "openai", "groq"
	Model    string `yaml:"model,omitempty"`    // e.g. "opus-4.8", "gpt-5.5", "llama-3.3-70b-versatile"
	Variant  string `yaml:"variant,omitempty"`  // e.g. "opus[1m]", "fast", "sonnet"
	Effort   string `yaml:"effort,omitempty"`   // e.g. "xhigh", "medium", "low"

	// #507: NonAgentic marks a backend that is a TEXT-COMPLETION helper,
	// not an agentic CLI capable of producing a real PR. Such a backend
	// MUST NOT be used as model.default or in model.fallback_backends —
	// the worker prompt assumes file reads/writes, bash exec, git/gh
	// operations. A non-agentic backend running as a worker emits the
	// steps as TEXT into its log, never executes them, and the
	// supervisor reconciles to dead with no PR. Found live 2026-05-30
	// (sup-84 wrote a plausible Go test scaffold + git/gh commands as
	// strings, no commits or PR materialised). config.parse rejects
	// any config that wires a NonAgentic backend into the worker chain.
	NonAgentic bool `yaml:"non_agentic,omitempty"`

	// Pricing carries the per-backend $/Mtok rates used by the fleet
	// cost-observability aggregation (#619). Both fields are optional —
	// when neither is set, the cost panel renders tokens only for the
	// backend ("price not configured") instead of a $ estimate. Workers
	// only stamp a single total-token count per session, so the estimator
	// blends input/output 50/50; operators who care about precision can
	// set the same value for both.
	Pricing BackendPricing `yaml:"pricing,omitempty"`
}

// MCPConfig is an opt-in per-backend worker MCP attachment. When empty,
// Maestro passes no MCP configuration to spawned workers.
type MCPConfig struct {
	// Configs are backend-native MCP config JSON strings or file paths. Claude
	// receives them via --mcp-config. Codex cannot consume these directly; use
	// Servers for Codex workers.
	Configs []string `yaml:"configs,omitempty"`
	// Strict is supported by Claude workers and makes --mcp-config the only
	// MCP source for the session.
	Strict  bool                    `yaml:"strict,omitempty"`
	Servers map[string]MCPServerDef `yaml:"servers,omitempty"`
}

// MCPServerDef describes one MCP server exposed to a worker backend.
// Configure either Command for stdio or URL for streamable HTTP.
type MCPServerDef struct {
	Command           string            `yaml:"command,omitempty"`
	Args              []string          `yaml:"args,omitempty"`
	Env               map[string]string `yaml:"env,omitempty"`
	URL               string            `yaml:"url,omitempty"`
	BearerTokenEnvVar string            `yaml:"bearer_token_env_var,omitempty"`
	Headers           map[string]string `yaml:"headers,omitempty"`
	AllowedTools      []string          `yaml:"allowed_tools,omitempty"`
	StartupTimeoutMs  int               `yaml:"startup_timeout_ms,omitempty"`
	ToolTimeoutMs     int               `yaml:"tool_timeout_ms,omitempty"`
	Trust             string            `yaml:"trust,omitempty"`
}

// BackendPricing is the per-backend cost table used by the fleet cost
// observability rollup (#619). Rates are USD per million tokens. Both
// fields default to 0; a backend whose pricing is unset is reported
// with tokens only ("price not configured") so it degrades gracefully.
type BackendPricing struct {
	InputUSDPerMtok  float64 `yaml:"input_usd_per_mtok,omitempty"`
	OutputUSDPerMtok float64 `yaml:"output_usd_per_mtok,omitempty"`
}

// Configured reports whether at least one rate is non-zero. Callers use
// this to switch a backend's cost cell between a $ value and a
// "price not configured" hint.
func (p BackendPricing) Configured() bool {
	return p.InputUSDPerMtok > 0 || p.OutputUSDPerMtok > 0
}

// EstimateCostUSD returns the USD estimate for the given total token
// count using a 50/50 input/output blend. Workers record a single
// combined token counter so a finer split is not available; operators
// who only price one side can leave the other at zero and the average
// halves the cost as expected. Returns 0 when no rates are set or
// tokens is non-positive.
func (p BackendPricing) EstimateCostUSD(tokens int) float64 {
	if tokens <= 0 || !p.Configured() {
		return 0
	}
	rate := (p.InputUSDPerMtok + p.OutputUSDPerMtok) / 2
	return float64(tokens) * rate / 1_000_000.0
}

func (b BackendDef) IsEnabled() bool {
	return b.Enabled == nil || *b.Enabled
}

// ModelConfig holds multi-backend configuration.
type ModelConfig struct {
	Default          string                `yaml:"default"` // "claude", "codex", etc.
	Backends         map[string]BackendDef `yaml:"backends"`
	FallbackBackends []string              `yaml:"fallback_backends"` // ordered list of backends to try when rate-limited
}

// VersioningConfig controls automatic version bumping on PR merge.
type VersioningConfig struct {
	Enabled       bool     `yaml:"enabled"`
	Files         []string `yaml:"files"`          // Files containing version strings to update
	DefaultBump   string   `yaml:"default_bump"`   // "patch", "minor", or "major"
	TagPrefix     string   `yaml:"tag_prefix"`     // e.g. "v"
	CreateRelease bool     `yaml:"create_release"` // Create GitHub release on bump
}

// GitHubProjectsConfig controls syncing issue status to GitHub Projects boards.
type GitHubProjectsConfig struct {
	Enabled       bool `yaml:"enabled"`
	ProjectNumber int  `yaml:"project_number"` // GitHub Project number (auto-detect from repo)
}

// SelfDeployConfig gates the opt-in post-merge self-deploy of the maestro
// binary itself (#698). Default OFF: projects without a `self_deploy:` block
// (or with enabled: false) see no behavior change.
//
// When enabled, the orchestrator launches scripts/self-deploy.sh through a
// detached transient systemd unit (systemd-run --user) after every PR merge.
// Running detached is what lets the script restart maestro's own units and
// survive that restart; restarting via `systemctl --user restart` keeps the
// unit's ExecStop drain semantics intact.
type SelfDeployConfig struct {
	Enabled        bool     `yaml:"enabled"`          // default false (opt-in)
	Script         string   `yaml:"script"`           // deploy script path (default: <local_path>/scripts/self-deploy.sh)
	BinPath        string   `yaml:"bin_path"`         // install target (default: path of the running binary)
	Units          []string `yaml:"units"`            // systemd user units to restart (default: ["maestro.service"])
	HealthURL      string   `yaml:"health_url"`       // running-process version probe (default: http://127.0.0.1:<server.port>/api/v1/state when server.port > 0)
	HealthTokenEnv string   `yaml:"health_token_env"` // env var holding the bearer token for health_url (default: server.auth.token_env)
	TimeoutMinutes int      `yaml:"timeout_minutes"`  // build+install+restart+verify budget; must cover unit drain (default: 30)
}

// EffectiveScript returns the deploy script path, defaulting to
// scripts/self-deploy.sh inside the project checkout.
func (c SelfDeployConfig) EffectiveScript(localPath string) string {
	if s := strings.TrimSpace(c.Script); s != "" {
		return expandHome(s)
	}
	if strings.TrimSpace(localPath) == "" {
		return ""
	}
	return filepath.Join(localPath, "scripts", "self-deploy.sh")
}

// EffectiveUnits returns the systemd user units to restart, defaulting to
// the unit name `maestro init` installs.
func (c SelfDeployConfig) EffectiveUnits() []string {
	units := make([]string, 0, len(c.Units))
	for _, u := range c.Units {
		if u = strings.TrimSpace(u); u != "" {
			units = append(units, u)
		}
	}
	if len(units) == 0 {
		return []string{"maestro.service"}
	}
	return units
}

// EffectiveHealthURL returns the URL polled to confirm the restarted process
// reports the freshly stamped version. Empty means CLI + unit checks only.
func (c SelfDeployConfig) EffectiveHealthURL(server ServerConfig) string {
	if u := strings.TrimSpace(c.HealthURL); u != "" {
		return u
	}
	if server.Port <= 0 {
		return ""
	}
	host := strings.TrimSpace(server.Host)
	if host == "" {
		host = "127.0.0.1"
	}
	return fmt.Sprintf("http://%s/api/v1/state", net.JoinHostPort(host, strconv.Itoa(server.Port)))
}

// EffectiveHealthTokenEnv returns the env var name holding the bearer token
// for the health probe, falling back to the dashboard auth token env.
func (c SelfDeployConfig) EffectiveHealthTokenEnv(server ServerConfig) string {
	if e := strings.TrimSpace(c.HealthTokenEnv); e != "" {
		return e
	}
	return strings.TrimSpace(server.Auth.TokenEnv)
}

// EffectiveTimeoutMinutes returns the total deploy budget. The default is
// deliberately larger than deploy_timeout_minutes because a unit restart
// honors drain (in-flight workers finish before the stop completes).
func (c SelfDeployConfig) EffectiveTimeoutMinutes() int {
	if c.TimeoutMinutes > 0 {
		return c.TimeoutMinutes
	}
	return 30
}

const (
	SupervisorActionAddReadyLabel       = "add_ready_label"
	SupervisorActionRemoveReadyLabel    = "remove_ready_label"
	SupervisorActionAddBlockedLabel     = "add_blocked_label"
	SupervisorActionRemoveBlockedLabel  = "remove_blocked_label"
	SupervisorActionAddIssueComment     = "add_issue_comment"
	SupervisorActionMergePR             = "merge_pr"
	SupervisorActionCloseIssue          = "close_issue"
	SupervisorActionCloseIssueBatch     = "close_issue_batch"
	SupervisorActionDeleteWorktree      = "delete_worktree"
	SupervisorActionChangeGlobalConfig  = "change_global_config"
	SupervisorActionApplyLessonProposal = "apply_lesson_proposal"
	// SupervisorActionRestartWorker / SupervisorActionStopWorker are the
	// per-session worker-control verbs surfaced by the fleet snapshot
	// (#567). Both are approval-gated: the operator clicks Restart/Stop on
	// a worker row, the server enqueues a pending Approval, and the
	// approver executor calls the WorkerController to kill the tmux
	// session + (for restart) mark the slot for the next dispatcher
	// respawn.
	SupervisorActionRestartWorker = "restart_worker"
	SupervisorActionStopWorker    = "stop_worker"
	// SupervisorActionSpawnReviewRepair is the auto review-repair respawn verb
	// minted by the supervisor when a green+mergeable PR is settled
	// retry_exhausted on review feedback and at least one Greptile P0/P1
	// inline comment is still on the head SHA (#565). The orchestrator
	// dispatcher spawns a scoped opus repair worker keyed on
	// (pr_number, head_sha) so the same head is never re-attempted.
	SupervisorActionSpawnReviewRepair = "spawn_review_repair"
)

// SupervisorConfig defines local policy for supervisor decisions.
type SupervisorConfig struct {
	Enabled                 bool                            `yaml:"enabled" json:"enabled"`
	Backend                 string                          `yaml:"backend" json:"backend,omitempty"`
	Model                   string                          `yaml:"model" json:"model,omitempty"`
	Effort                  string                          `yaml:"effort" json:"effort,omitempty"`
	Prompt                  string                          `yaml:"prompt" json:"prompt,omitempty"`
	DryRun                  bool                            `yaml:"dry_run" json:"dry_run,omitempty"`
	Mode                    string                          `yaml:"mode" json:"mode"`
	ReadyLabel              string                          `yaml:"ready_label" json:"ready_label,omitempty"`
	BlockedLabel            string                          `yaml:"blocked_label" json:"blocked_label,omitempty"`
	QueueComments           bool                            `yaml:"queue_comments" json:"queue_comments,omitempty"`
	OneAtATime              bool                            `yaml:"one_at_a_time" json:"one_at_a_time,omitempty"`
	DispatchSLASeconds      int                             `yaml:"dispatch_sla_seconds" json:"dispatch_sla_seconds,omitempty"`
	ExcludedLabels          []string                        `yaml:"excluded_labels" json:"excluded_labels,omitempty"`
	AllowIssueTypes         []string                        `yaml:"allow_issue_types" json:"allow_issue_types,omitempty"`
	OrderedQueue            SupervisorOrderedQueueConfig    `yaml:"ordered_queue" json:"ordered_queue,omitempty"`
	DynamicWave             SupervisorDynamicWaveConfig     `yaml:"dynamic_wave" json:"dynamic_wave,omitempty"`
	HandoffPlanner          SupervisorHandoffPlannerConfig  `yaml:"handoff_planner" json:"handoff_planner,omitempty"`
	ReviewRepair            SupervisorReviewRepairConfig    `yaml:"review_repair" json:"review_repair,omitempty"`
	PreflightCommand        string                          `yaml:"preflight_command" json:"preflight_command,omitempty"`
	CompletionGates         SupervisorCompletionGatesConfig `yaml:"completion_gates" json:"completion_gates,omitempty"`
	SafeActions             []string                        `yaml:"safe_actions" json:"safe_actions,omitempty"`
	ApprovalRequired        []string                        `yaml:"approval_required" json:"approval_required,omitempty"`
	AllowedActions          []string                        `yaml:"allowed_actions" json:"allowed_actions,omitempty"`
	ApprovalRequiredActions []string                        `yaml:"approval_required_actions" json:"approval_required_actions,omitempty"`
	PolicyPath              string                          `yaml:"-" json:"policy_path,omitempty"`
	LessonProposalsEnabled  *bool                           `yaml:"lesson_proposals_enabled" json:"lesson_proposals_enabled,omitempty"`

	excludedLabelsSet bool
}

// LessonProposalsOn reports whether the supervisor should generate lesson
// proposals. Defaults to true; set supervisor.lesson_proposals_enabled: false
// to suppress the low-value, never-approved lesson-proposal approvals until the
// dedup / auto-resolve work (#668) lands.
func (c SupervisorConfig) LessonProposalsOn() bool {
	return c.LessonProposalsEnabled == nil || *c.LessonProposalsEnabled
}

// SupervisorHandoffPlannerConfig describes the supervisor-owned continuation
// from an open handoff/epic issue to the next runnable child issue. When the
// dynamic-wave queue has no eligible runnable issues but an open handoff epic
// remains, the supervisor uses this config to recommend (and, in a future
// PR, execute) the creation of the next concrete child issue instead of
// silently idling on "none".
//
// v1 is intentionally narrow: it owns the deterministic detection +
// recommendation path. The actual child-issue creation is approval-gated
// until the safe-action `open_child_issue` lands.
type SupervisorHandoffPlannerConfig struct {
	Enabled                      *bool    `yaml:"enabled" json:"enabled,omitempty"`
	SourceIssueLabels            []string `yaml:"source_issue_labels" json:"source_issue_labels,omitempty"`
	ChildReadyLabel              string   `yaml:"child_ready_label" json:"child_ready_label,omitempty"`
	ChildBlockedLabel            string   `yaml:"child_blocked_label" json:"child_blocked_label,omitempty"`
	MaxChildrenPerCycle          int      `yaml:"max_children_per_cycle" json:"max_children_per_cycle,omitempty"`
	MaxOpenChildren              int      `yaml:"max_open_children" json:"max_open_children,omitempty"`
	IssueTemplate                string   `yaml:"issue_template" json:"issue_template,omitempty"`
	ParseSections                []string `yaml:"parse_sections" json:"parse_sections,omitempty"`
	PreflightCommand             string   `yaml:"preflight_command" json:"preflight_command,omitempty"`
	RequirePreflightBeforeCreate bool     `yaml:"require_preflight_before_create" json:"require_preflight_before_create,omitempty"`
	RequirePreflightBeforeSpawn  bool     `yaml:"require_preflight_before_spawn" json:"require_preflight_before_spawn,omitempty"`
}

// Active reports whether the handoff planner path should be considered.
// Default is disabled (planner only runs when explicitly enabled per project).
func (h SupervisorHandoffPlannerConfig) Active() bool {
	return h.Enabled != nil && *h.Enabled
}

// EffectiveSourceLabels returns the configured source labels with sensible
// defaults ("epic", "design-handoff") when none are set explicitly.
func (h SupervisorHandoffPlannerConfig) EffectiveSourceLabels() []string {
	if len(h.SourceIssueLabels) > 0 {
		return h.SourceIssueLabels
	}
	return []string{"epic", "design-handoff"}
}

// SupervisorReviewRepairConfig governs the auto review-repair respawn
// (#565). When a green+mergeable PR is settled retry_exhausted on review
// feedback AND ≥1 Greptile P0/P1 inline comment is still on the current
// head SHA, the supervisor mints a spawn_review_repair action so a fresh
// worker scoped to those comments — running on the configured strong
// backend — can address them. A separate retry budget keeps the main
// retries-per-issue budget independent.
//
// Default is enabled when the SupervisorConfig is otherwise live (review
// feedback retry exhaustion is the dogfood failure mode this config
// targets; an explicit nil means "default on").
type SupervisorReviewRepairConfig struct {
	// Enabled gates the entire feature. nil = default on so the dogfood
	// failure mode (#564, #555, #535) is fixed without explicit opt-in.
	Enabled *bool `yaml:"enabled" json:"enabled,omitempty"`
	// Backend names the worker backend used for review repair. Empty =
	// "claude" so the strongest backend addresses the residual P0/P1.
	Backend string `yaml:"backend" json:"backend,omitempty"`
	// Model and Effort are forwarded to the backend selection when set.
	Model  string `yaml:"model" json:"model,omitempty"`
	Effort string `yaml:"effort" json:"effort,omitempty"`
	// MaxRetries caps how many review-repair workers can be spawned for a
	// single (pr_number, head_sha) before falling through to operator
	// review. Default 1: the head SHA changes after every push, so a
	// single retry is enough to either land the fix or surface to the
	// operator. Set 0 to disable spawning entirely.
	MaxRetries int `yaml:"max_retries" json:"max_retries,omitempty"`
	// FallThroughToMergeApproval, when true, makes the supervisor emit a
	// merge_pr decision after the review-repair budget exhausts so the
	// cautious gate mints an approval and the operator can sign off on
	// the residual P0/P1. When false (default) the supervisor stays on
	// monitor_open_pr with an attention stuck state. Either way the PR
	// never dead-ends silently.
	FallThroughToMergeApproval *bool `yaml:"fall_through_to_merge_approval" json:"fall_through_to_merge_approval,omitempty"`
}

// Active reports whether the auto review-repair respawn should fire.
// Default true — the feature exists to close a hands-off ceiling, and
// every project that does not want it can flip Enabled to false.
func (r SupervisorReviewRepairConfig) Active() bool {
	if r.Enabled == nil {
		return true
	}
	return *r.Enabled
}

// EffectiveBackend returns the configured backend or "claude" (the
// default strong backend per the issue's acceptance criteria).
func (r SupervisorReviewRepairConfig) EffectiveBackend() string {
	if b := strings.TrimSpace(r.Backend); b != "" {
		return b
	}
	return "claude"
}

// EffectiveMaxRetries returns the configured retry cap or 1 when unset.
// MaxRetries==0 is treated literally (disable spawning) — the default is
// applied at parse time so callers see the operator's intent verbatim.
func (r SupervisorReviewRepairConfig) EffectiveMaxRetries() int {
	if r.MaxRetries < 0 {
		return 0
	}
	return r.MaxRetries
}

// FallThroughMergeEnabled reports whether the supervisor should mint a
// merge_pr decision (cautious-gate approval) once the review-repair
// budget exhausts. Default false: the supervisor surfaces the residual
// findings via stuck states instead.
func (r SupervisorReviewRepairConfig) FallThroughMergeEnabled() bool {
	if r.FallThroughToMergeApproval == nil {
		return false
	}
	return *r.FallThroughToMergeApproval
}

// SupervisorCompletionGatesConfig configures the issue-specific completion
// gates that apply after a PR merges. Runtime health alone is not enough to
// close issues that require live visual or deployment verification — when
// any gate label or body marker is present, the supervisor must not collapse
// the Done check to healthz.
type SupervisorCompletionGatesConfig struct {
	RequiredLabels      []string `yaml:"required_labels" json:"required_labels,omitempty"`
	BodyMarkers         []string `yaml:"body_markers" json:"body_markers,omitempty"`
	LiveVisualCommand   string   `yaml:"live_visual_command" json:"live_visual_command,omitempty"`
	DeploymentStatusCmd string   `yaml:"deployment_status_command" json:"deployment_status_command,omitempty"`
	VerificationLabel   string   `yaml:"verification_label" json:"verification_label,omitempty"`
}

// Active reports whether any issue-specific completion gate is configured.
// A nil/zero CompletionGates means callers should keep the legacy
// healthz-only behaviour, so existing projects are unaffected.
func (g SupervisorCompletionGatesConfig) Active() bool {
	return len(g.RequiredLabels) > 0 ||
		len(g.BodyMarkers) > 0 ||
		strings.TrimSpace(g.LiveVisualCommand) != "" ||
		strings.TrimSpace(g.DeploymentStatusCmd) != "" ||
		strings.TrimSpace(g.VerificationLabel) != ""
}

// IssueRequiresLiveVerification reports whether the issue (identified by its
// labels and body) requires live/visual verification before it can be
// treated as Done. Callers use this in two places:
//
//   - the post-merge close pipeline: refuse to close the issue on healthz
//     alone when this returns true;
//   - the supervisor handoff/dispatch: surface a "needs verification"
//     stuck-state so an operator can see what is still open.
//
// The match is case-insensitive on labels and a plain substring scan on
// body markers — small, predictable, no regex.
func (g SupervisorCompletionGatesConfig) IssueRequiresLiveVerification(labels []string, body string) bool {
	if !g.Active() {
		return false
	}
	lowerBody := strings.ToLower(body)
	for _, label := range labels {
		l := strings.ToLower(strings.TrimSpace(label))
		if l == "" {
			continue
		}
		for _, required := range g.RequiredLabels {
			if strings.EqualFold(strings.TrimSpace(required), l) {
				return true
			}
		}
		if v := strings.TrimSpace(g.VerificationLabel); v != "" && strings.EqualFold(v, l) {
			return true
		}
	}
	for _, marker := range g.BodyMarkers {
		m := strings.ToLower(strings.TrimSpace(marker))
		if m == "" {
			continue
		}
		if strings.Contains(lowerBody, m) {
			return true
		}
	}
	return false
}

// SupervisorOrderedQueueConfig pins supervisor selection to a fixed issue order.
type SupervisorOrderedQueueConfig struct {
	Enabled    bool  `yaml:"enabled" json:"enabled"`
	Issues     []int `yaml:"issues" json:"issues,omitempty"`
	DoneIssues []int `yaml:"done_issues" json:"done_issues,omitempty"`
}

// SupervisorDynamicWaveConfig enables policy-driven issue selection without a
// fixed issue-number list.
type SupervisorDynamicWaveConfig struct {
	Enabled                 *bool                             `yaml:"enabled" json:"enabled,omitempty"`
	OwnsReadyLabel          bool                              `yaml:"owns_ready_label" json:"owns_ready_label,omitempty"`
	RunnableProjectStatuses []string                          `yaml:"runnable_project_statuses" json:"runnable_project_statuses,omitempty"`
	DependencyUnblock       SupervisorDependencyUnblockConfig `yaml:"dependency_unblock" json:"dependency_unblock,omitempty"`
}

func (w SupervisorDynamicWaveConfig) Active() bool {
	return w.Enabled != nil && *w.Enabled
}

// SupervisorDependencyUnblockConfig configures the dependency-based dynamic
// wave controller. When enabled, the supervisor evaluates issues carrying the
// configured blocked label, parses dependency references from their body, and
// unblocks them (remove blocked label, add ready label, leave evidence
// comment) once every dependency is closed/merged. It also enrolls those
// blocked issues into the configured GitHub Project so wave members are
// visible before they go runnable.
//
// Defaults on for dynamic waves that own the ready/blocked labels. Set
// enabled: false when blocker chains are intentionally operator-managed.
// The Scribe redesign handoff used an external cron
// (`scribe-redesign-handoff-unblocker`) as a workaround; this config lets
// supervisor own that handoff dev role directly. See issue #442.
type SupervisorDependencyUnblockConfig struct {
	Enabled             *bool `yaml:"enabled" json:"enabled,omitempty"`
	MaxRunnable         int   `yaml:"max_runnable" json:"max_runnable,omitempty"`
	EnrollInProject     *bool `yaml:"enroll_in_project" json:"enroll_in_project,omitempty"`
	AnnounceWithComment *bool `yaml:"announce_with_comment" json:"announce_with_comment,omitempty"`
}

// Active reports whether the dependency-unblock controller should run. Parsed
// configs default this on for owned dynamic waves with both ready and blocked
// labels, unless the operator explicitly sets enabled: false.
func (d SupervisorDependencyUnblockConfig) Active() bool {
	return d.Enabled != nil && *d.Enabled
}

// EnrollInProjectEnabled reports whether blocked wave members should be
// enrolled into the configured GitHub Project. Default true when the
// dependency-unblock controller is active.
func (d SupervisorDependencyUnblockConfig) EnrollInProjectEnabled() bool {
	if d.EnrollInProject == nil {
		return true
	}
	return *d.EnrollInProject
}

// AnnounceWithCommentEnabled reports whether every automatic unblock should
// leave an issue comment with dependency evidence. Default true so operators
// always have an audit trail of why a wave opened. See acceptance criterion
// "Every automatic unblock leaves a GitHub comment describing dependency
// evidence."
func (d SupervisorDependencyUnblockConfig) AnnounceWithCommentEnabled() bool {
	if d.AnnounceWithComment == nil {
		return true
	}
	return *d.AnnounceWithComment
}

func (q SupervisorOrderedQueueConfig) Active() bool {
	return q.Enabled || len(q.Issues) > 0
}

func (q SupervisorOrderedQueueConfig) IsDone(number int) bool {
	for _, done := range q.DoneIssues {
		if done == number {
			return true
		}
	}
	return false
}

func (s *SupervisorConfig) UnmarshalYAML(value *yaml.Node) error {
	type rawSupervisorConfig SupervisorConfig
	var raw rawSupervisorConfig
	if err := value.Decode(&raw); err != nil {
		return err
	}
	*s = SupervisorConfig(raw)
	if value.Kind == yaml.MappingNode {
		for i := 0; i+1 < len(value.Content); i += 2 {
			if value.Content[i].Value == "excluded_labels" {
				s.excludedLabelsSet = true
				break
			}
		}
	}
	return nil
}

func (s SupervisorConfig) OrderedQueueActive() bool {
	return s.OrderedQueue.Active()
}

func (s SupervisorConfig) AllowsSafeAction(action string) bool {
	action = normalizePolicyToken(action)
	for _, configured := range s.SafeActions {
		if configured == action {
			return true
		}
	}
	return false
}

// RoutingConfig controls automatic backend selection via LLM router.
type RoutingConfig struct {
	Mode             string            `yaml:"mode"`               // "auto", "manual" (labels only)
	RouterModel      string            `yaml:"router_model"`       // backend name from model.backends (default: "claude")
	RouterModelName  string            `yaml:"router_model_name"`  // specific model to use (default: "claude-sonnet-4-6")
	RouterPrompt     string            `yaml:"router_prompt"`      // prompt template with {{BACKENDS}}, {{NUMBER}}, {{TITLE}}, {{BODY}}
	TaskTypeBackends map[string]string `yaml:"task_type_backends"` // task_type -> backend override used only when routing.mode=auto

	// Role-specific backend overrides for the planner → implementer → validator pipeline.
	// Each maps to a backend name from model.backends. If empty, falls back to issue-level routing.
	PlannerBackend        string `yaml:"planner_backend"`        // backend for planning phase (e.g. "gemini-flash")
	ImplementationBackend string `yaml:"implementation_backend"` // backend for implementation phase (e.g. "claude")
	ValidatorBackend      string `yaml:"validator_backend"`      // backend for validation phase (e.g. "claude")
}

// ServerConfig controls the optional HTTP API server.
type ServerConfig struct {
	Host     string           `yaml:"host"`      // host/interface to bind; default: 127.0.0.1
	Port     int              `yaml:"port"`      // 0 = disabled (default)
	ReadOnly bool             `yaml:"read_only"` // disable mutating HTTP endpoints when true
	Auth     ServerAuthConfig `yaml:"auth"`      // HTTP auth on mutating endpoints (#487)
}

// ServerAuthConfig configures app-level auth on every mutating dashboard
// endpoint (#487 / write-path premortem #4). The token itself MUST NOT be
// stored in YAML — set TokenEnv to the name of an environment variable the
// operator populates from a secret manager (Infisical, 1Password, etc.).
//
// When the resolved token is non-empty, every POST to /api/v1/.../actions,
// /api/v1/.../approvals/{id}/{approve|reject}, /api/v1/audit/log, and
// /api/v1/refresh requires Authorization: Bearer <token> (or Basic auth
// where the password equals the token). Read-only GETs stay open.
//
// When TokenEnv is unset OR the env var is empty, mutating endpoints behave
// as before — useful for unit tests and the initial rollout. Operators
// running maestro on a shared network MUST set TokenEnv.
type ServerAuthConfig struct {
	TokenEnv  string `yaml:"token_env"`  // env var name to read the bearer token from
	ActorName string `yaml:"actor_name"` // audit actor recorded for authenticated requests
}

// Token resolves the bearer token from the configured env var. Returns ""
// when TokenEnv is unset, when the env var is unset, or when the env var
// is empty/whitespace. Callers treat "" as "auth not configured".
func (a ServerAuthConfig) Token() string {
	env := strings.TrimSpace(a.TokenEnv)
	if env == "" {
		return ""
	}
	return strings.TrimSpace(os.Getenv(env))
}

// ResolvedActorName returns the actor recorded in audit for authenticated
// requests. Defaults to "dashboard-authenticated" when ActorName is empty.
func (a ServerAuthConfig) ResolvedActorName() string {
	name := strings.TrimSpace(a.ActorName)
	if name == "" {
		return "dashboard-authenticated"
	}
	return name
}

// RoleConfig defines settings for a single pipeline role (planner, validator).
type RoleConfig struct {
	Enabled           bool   `yaml:"enabled"`
	Backend           string `yaml:"backend"`             // backend name from model.backends (empty = use default)
	Prompt            string `yaml:"prompt"`              // path to prompt template (empty = built-in default)
	MaxRuntimeMinutes int    `yaml:"max_runtime_minutes"` // override per-role max runtime (0 = use global)
}

// PipelineConfig controls the planner → implementer → validator phase pipeline
// and deterministic pre-worker context preparation phases.
type PipelineConfig struct {
	// Phase-based pipeline (planner → implementer → validator)
	Enabled   bool       `yaml:"enabled"`   // enable 3-phase pipeline globally (default: false; issue label pipeline:full opts in per worker)
	Planner   RoleConfig `yaml:"planner"`   // planner role settings
	Validator RoleConfig `yaml:"validator"` // validator role settings
	// Implementer uses the existing worker_prompt / bug_prompt / enhancement_prompt settings.

	// Deterministic pre-worker context preparation phases. These are heuristic
	// local scans/checks, not separate agent sessions.
	Research       bool  `yaml:"research"`        // scan repo context before worker starts (default: false)
	PlanValidation *bool `yaml:"plan_validation"` // heuristic plan coverage check before coding starts (default: true)
	TestMapping    *bool `yaml:"test_mapping"`    // map requirements to verify commands (default: true)
}

// PlanValidationEnabled returns whether plan validation is enabled (default: true).
func (p PipelineConfig) PlanValidationEnabled() bool {
	if p.PlanValidation == nil {
		return true
	}
	return *p.PlanValidation
}

// TestMappingEnabled returns whether test mapping is enabled (default: true).
func (p PipelineConfig) TestMappingEnabled() bool {
	if p.TestMapping == nil {
		return true
	}
	return *p.TestMapping
}

// MissionsConfig controls mission mode for decomposing epics into child issues.
type MissionsConfig struct {
	Enabled     bool     `yaml:"enabled"`
	MaxChildren int      `yaml:"max_children"` // max child issues per mission (default: 10)
	Labels      []string `yaml:"labels"`       // labels that identify mission issues (default: ["mission", "epic"])
}

// ToolHookConfig describes an agent tool hook that runs inside the worker
// session. Command is executed from the worker worktree.
type ToolHookConfig struct {
	Command        string `yaml:"command" json:"command"`                   // shell command to run for the hook
	Matcher        string `yaml:"matcher" json:"matcher"`                   // backend-specific tool matcher; defaults by event
	BlockOnFailure bool   `yaml:"block_on_failure" json:"block_on_failure"` // when true, non-zero command results block the tool/event
}

// HooksConfig holds lifecycle hook scripts that run at key points.
type HooksConfig struct {
	AfterCreate  string         `yaml:"after_create"`  // runs once when a new issue workspace is first created
	BeforeRun    string         `yaml:"before_run"`    // runs before each agent attempt (fatal on failure)
	AfterRun     string         `yaml:"after_run"`     // runs after each agent attempt (logged, not fatal)
	BeforeRemove string         `yaml:"before_remove"` // runs before workspace cleanup (logged, not fatal)
	PreTool      ToolHookConfig `yaml:"pre_tool"`      // runs inside the worker session before matching tool calls
	PostEdit     ToolHookConfig `yaml:"post_edit"`     // runs inside the worker session after file edit tools
	TimeoutMs    int            `yaml:"timeout_ms"`    // timeout for hook execution in milliseconds (default: 60000)
}

// StaleSessionReconcilerConfig configures filtering of stale supervisor
// sessions out of the operator-facing attention list.
//
// Defaults are conservative so a worker that simply has not written state
// for a few minutes is never reclassified as stale. A session is filtered
// only when it has been idle for at least IdleAfterMinutes AND its
// recorded worktree is missing on disk (when require_worktree_missing is
// true, the default).
type StaleSessionReconcilerConfig struct {
	Enabled                *bool `yaml:"enabled"`                  // default: true
	IdleAfterMinutes       int   `yaml:"idle_after_minutes"`       // default: 1440 (24h)
	RequireWorktreeMissing *bool `yaml:"require_worktree_missing"` // default: true
	MergedPRDismisses      *bool `yaml:"merged_pr_dismisses"`      // default: true
}

// IsEnabled returns whether stale-session reconciliation runs.
// Default is enabled when the field is unset.
func (c StaleSessionReconcilerConfig) IsEnabled() bool {
	if c.Enabled == nil {
		return true
	}
	return *c.Enabled
}

// WorktreeMissingRequired returns whether a session must also have a missing
// worktree to be classified as stale. Default true.
func (c StaleSessionReconcilerConfig) WorktreeMissingRequired() bool {
	if c.RequireWorktreeMissing == nil {
		return true
	}
	return *c.RequireWorktreeMissing
}

// IdleAfter returns the idle window after which a session is considered for
// reconciliation. Default 24h when IdleAfterMinutes is unset or non-positive.
func (c StaleSessionReconcilerConfig) IdleAfter() int {
	if c.IdleAfterMinutes <= 0 {
		return 24 * 60
	}
	return c.IdleAfterMinutes
}

// MergedPRDismissesEnabled reports whether a dead session whose linked PR is
// merged should be dismissed independently of the idle/worktree window.
// Default true.
func (c StaleSessionReconcilerConfig) MergedPRDismissesEnabled() bool {
	if c.MergedPRDismisses == nil {
		return true
	}
	return *c.MergedPRDismisses
}

// ReviewRetriggerConfig governs the orchestrator's self-healing re-trigger
// for the greptile review gate (#691). Greptile occasionally misses its
// review webhook: the PR sits CI=green with zero review signal on the
// current head and the orchestrator loops "waiting for review gate
// (greptile=pending)" forever. Server-side update-branch makes this worse —
// the gate resets on the new head and the re-review webhook is missed again.
// When the greptile stream has been pending on the same head SHA for more
// than PendingMinutes, the orchestrator posts "@greptile review" on the PR
// (bounded by CooldownMinutes to avoid comment churn) so the review re-runs
// without operator intervention.
type ReviewRetriggerConfig struct {
	Enabled         *bool `yaml:"enabled"`          // default: true
	PendingMinutes  int   `yaml:"pending_minutes"`  // re-trigger after this many minutes of greptile=pending on one head (default: 10)
	CooldownMinutes int   `yaml:"cooldown_minutes"` // minimum minutes between re-trigger comments per session (default: 30)
}

// Active reports whether the stale-review re-trigger runs. Default true —
// the orchestrator owns the review gate and should own its liveness.
func (c ReviewRetriggerConfig) Active() bool {
	if c.Enabled == nil {
		return true
	}
	return *c.Enabled
}

// EffectivePendingFor returns how long the greptile stream must stay pending
// on the same head before a re-trigger fires. Default 10 minutes when unset
// or non-positive.
func (c ReviewRetriggerConfig) EffectivePendingFor() time.Duration {
	if c.PendingMinutes <= 0 {
		return 10 * time.Minute
	}
	return time.Duration(c.PendingMinutes) * time.Minute
}

// EffectiveCooldown returns the minimum gap between re-trigger comments for
// one session. Default 30 minutes when unset or non-positive.
func (c ReviewRetriggerConfig) EffectiveCooldown() time.Duration {
	if c.CooldownMinutes <= 0 {
		return 30 * time.Minute
	}
	return time.Duration(c.CooldownMinutes) * time.Minute
}

// SessionRetentionConfig bounds the growth of state.Sessions by compacting
// terminal sessions once both the count and age floors are exceeded (#497).
// Defaults keep the 20 newest terminal sessions per project and any terminal
// session younger than 7 days (whichever is larger), and append pruned
// sessions to <state_dir>/sessions-archive.jsonl for forensics.
type SessionRetentionConfig struct {
	Enabled     *bool  `yaml:"enabled,omitempty"`      // default: true
	KeepLast    int    `yaml:"keep_last,omitempty"`    // default: 20 (0 = default; negative = no count floor)
	MinAgeDays  int    `yaml:"min_age_days,omitempty"` // default: 7  (0 = default; negative = no age floor)
	Archive     *bool  `yaml:"archive,omitempty"`      // default: true
	ArchiveFile string `yaml:"archive_file,omitempty"` // default: <state_dir>/sessions-archive.jsonl
}

// IsEnabled reports whether session retention runs. Default true.
func (c SessionRetentionConfig) IsEnabled() bool {
	if c.Enabled == nil {
		return true
	}
	return *c.Enabled
}

// EffectiveKeepLast returns the count floor. 0 means "use default (20)";
// a negative value disables the count floor entirely.
func (c SessionRetentionConfig) EffectiveKeepLast() int {
	if c.KeepLast < 0 {
		return 0
	}
	if c.KeepLast == 0 {
		return 20
	}
	return c.KeepLast
}

// EffectiveMinAge returns the age floor. 0 means "use default (7d)";
// a negative value disables the age floor entirely.
func (c SessionRetentionConfig) EffectiveMinAge() time.Duration {
	if c.MinAgeDays < 0 {
		return 0
	}
	if c.MinAgeDays == 0 {
		return 7 * 24 * time.Hour
	}
	return time.Duration(c.MinAgeDays) * 24 * time.Hour
}

// ArchiveEnabled reports whether pruned sessions are appended to a JSONL
// archive before deletion. Default true.
func (c SessionRetentionConfig) ArchiveEnabled() bool {
	if c.Archive == nil {
		return true
	}
	return *c.Archive
}

// EffectiveArchiveFile returns the absolute path to the archive file, or
// empty when archiving is disabled. stateDir is the per-project state
// directory and is used as the default location.
func (c SessionRetentionConfig) EffectiveArchiveFile(stateDir string) string {
	if !c.ArchiveEnabled() {
		return ""
	}
	if strings.TrimSpace(c.ArchiveFile) != "" {
		return expandHome(c.ArchiveFile)
	}
	return filepath.Join(stateDir, "sessions-archive.jsonl")
}

type Config struct {
	Server                          ServerConfig                 `yaml:"server"`
	Supervisor                      SupervisorConfig             `yaml:"supervisor"`
	Repo                            string                       `yaml:"repo"`
	Outcome                         outcome.Brief                `yaml:"outcome"`
	LocalPath                       string                       `yaml:"local_path"`
	WorktreeBase                    string                       `yaml:"worktree_base"`
	MaxParallel                     int                          `yaml:"max_parallel"`
	MaxConcurrentByState            map[string]int               `yaml:"max_concurrent_by_state"`       // per-state concurrency limits (e.g. "running": 5, "pr_open": 2)
	MaxRuntimeMinutes               int                          `yaml:"max_runtime_minutes"`           // max worker runtime in minutes (default: 120)
	WorkerSilentTimeoutMinutes      int                          `yaml:"worker_silent_timeout_minutes"` // kill running worker if tmux output hash doesn't change for N minutes (0 = disabled)
	WorkerMaxTokens                 int                          `yaml:"worker_max_tokens"`             // kill worker when token usage exceeds this threshold (0 = unlimited)
	WorkerSoftTokenThreshold        *float64                     `yaml:"worker_soft_token_threshold"`   // fraction of worker_max_tokens to trigger checkpoint+respawn (default: 0.8, 0 = disabled)
	MaxRetriesPerIssue              int                          `yaml:"max_retries_per_issue"`         // max failed worker sessions per issue before giving up (default: 3, 0 = unlimited)
	AutoRebase                      bool                         `yaml:"auto_rebase"`                   // auto-attempt rebase for conflicting sessions (default: true)
	ClaudeCmd                       string                       `yaml:"claude_cmd"`                    // deprecated: use model.backends.claude.cmd
	IssueLabel                      string                       `yaml:"issue_label"`                   // deprecated: use issue_labels
	IssueLabels                     []string                     `yaml:"issue_labels"`
	ExcludeLabels                   []string                     `yaml:"exclude_labels"`
	WorkerPrompt                    string                       `yaml:"worker_prompt"`
	BugPrompt                       string                       `yaml:"bug_prompt"`          // prompt template for issues with "bug" label
	EnhancementPrompt               string                       `yaml:"enhancement_prompt"`  // prompt template for issues with "enhancement" label
	PromptSections                  []string                     `yaml:"prompt_sections"`     // additional prompt section files appended to the base prompt
	ValidationContract              bool                         `yaml:"validation_contract"` // generate VALIDATION.md in worktree with test-first guidance
	SessionPrefix                   string                       `yaml:"session_prefix"`      // worker session name prefix (default: first 3 chars of repo name)
	StateDir                        string                       `yaml:"state_dir"`           // state/log directory (default: ~/.maestro/<repo-hash>)
	Model                           ModelConfig                  `yaml:"model"`
	Routing                         RoutingConfig                `yaml:"routing"`
	DeployCmd                       string                       `yaml:"deploy_cmd"`                         // shell command to run after successful PR merge
	DeployTimeoutMinutes            int                          `yaml:"deploy_timeout_minutes"`             // timeout for deploy command in minutes (default: 15)
	MergeStrategy                   string                       `yaml:"merge_strategy"`                     // "sequential" | "parallel"
	MergeIntervalSeconds            int                          `yaml:"merge_interval_seconds"`             // minimum seconds between merges in sequential mode
	ReviewGate                      string                       `yaml:"review_gate"`                        // "greptile" (default) | "none"
	ReviewGateStreams               []string                     `yaml:"review_gate_streams"`                // optional review dimensions; default ["greptile"], opt-in ["greptile","simplicity"]
	ReviewRetrigger                 ReviewRetriggerConfig        `yaml:"review_retrigger"`                   // #691: re-post "@greptile review" when the gate wedges at pending with no review on head
	AutoRetryReviewFeedback         bool                         `yaml:"auto_retry_review_feedback"`         // close PRs with review comments and respawn a fixer
	MergeExhaustedNonCriticalReview *bool                        `yaml:"merge_exhausted_noncritical_review"` // #565: merge a green PR after review-feedback retries exhaust when only non-critical (P1/P2/P3) findings remain (no P0 on head). nil = default-on.
	AutoRetryRebaseConflicts        bool                         `yaml:"auto_retry_rebase_conflicts"`        // retry PRs whose auto-rebase fails with conflicts
	Telegram                        TelegramConfig               `yaml:"telegram"`
	Versioning                      VersioningConfig             `yaml:"versioning"`
	SelfDeploy                      SelfDeployConfig             `yaml:"self_deploy"` // #698: opt-in post-merge self-deploy of the maestro binary (default OFF)
	GitHubProjects                  GitHubProjectsConfig         `yaml:"github_projects"`
	MaxRetryBackoffMs               int                          `yaml:"max_retry_backoff_ms"`       // cap for exponential retry backoff in milliseconds (default: 300000 = 5 min)
	AutoResolveFiles                []string                     `yaml:"auto_resolve_files"`         // files to auto-resolve conflicts by keeping both sides
	AutoRestoreFiles                []string                     `yaml:"auto_restore_files"`         // dirty files that may be restored before auto-rebase
	CleanupWorktreesOnMerge         *bool                        `yaml:"cleanup_worktrees_on_merge"` // remove worktrees immediately after PR merge (default: true)
	Pipeline                        PipelineConfig               `yaml:"pipeline"`
	Hooks                           HooksConfig                  `yaml:"hooks"`
	Missions                        MissionsConfig               `yaml:"missions"`
	BlockerPatterns                 []string                     `yaml:"blocker_patterns"`         // regex patterns to detect blocker references in issue body for queue skips and dependency_unblock (e.g. "blocked by #(\\d+)"; first capture group must be issue number)
	PollIntervalSeconds             int                          `yaml:"poll_interval_seconds"`    // override poll interval from config (0 = use CLI flag)
	StaleSessionReconciler          StaleSessionReconcilerConfig `yaml:"stale_session_reconciler"` // filter stale supervisor sessions from operator attention
	SessionRetention                SessionRetentionConfig       `yaml:"session_retention"`        // #497: bound state.Sessions growth via terminal-session compaction
	SourcePath                      string                       `yaml:"-"`                        // path the config was loaded from (not serialized)
}

// LoadFrom loads config from a specific path.
func LoadFrom(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}
	cfg, err := parse(data)
	if err != nil {
		return nil, err
	}
	cfg.SourcePath = path
	if err := loadSupervisorPolicyFile(path, cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

func Load() (*Config, error) {
	candidates := []string{
		"maestro.yaml",
		"maestro.yml",
		filepath.Join(os.Getenv("HOME"), ".maestro", "config.yaml"),
		filepath.Join(os.Getenv("HOME"), ".maestro", "config.yml"),
	}

	var data []byte
	var err error
	var loadedPath string
	for _, path := range candidates {
		data, err = os.ReadFile(path)
		if err == nil {
			loadedPath = path
			break
		}
	}
	if err != nil {
		return nil, fmt.Errorf("no config file found (tried maestro.yaml, ~/.maestro/config.yaml): %w", err)
	}
	cfg, parseErr := parse(data)
	if parseErr != nil {
		return nil, parseErr
	}
	cfg.SourcePath = loadedPath
	if err := loadSupervisorPolicyFile(loadedPath, cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

// Parse decodes one project config document and applies the same defaults and
// validation used by LoadFrom. It does not load sidecar supervisor policy files.
func Parse(data []byte) (*Config, error) {
	return parse(data)
}

func parse(data []byte) (*Config, error) {

	cfg := &Config{
		MaxParallel:          5,
		MaxRuntimeMinutes:    120,
		MaxRetriesPerIssue:   3,
		DeployTimeoutMinutes: 15,
		AutoRebase:           true,
		ClaudeCmd:            "claude",
		MergeStrategy:        "sequential",
		MergeIntervalSeconds: 30,
		ReviewGate:           "greptile",
		AutoResolveFiles: []string{
			"server/src/api/mod.rs",
			"web/src/lib/api.ts",
			"web/src/lib/types.ts",
		},
	}
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	if cfg.Repo == "" {
		return nil, fmt.Errorf("config: repo is required")
	}

	// Normalize max_concurrent_by_state keys: trim + lowercase
	if len(cfg.MaxConcurrentByState) > 0 {
		normalized := make(map[string]int, len(cfg.MaxConcurrentByState))
		for k, v := range cfg.MaxConcurrentByState {
			normalized[strings.ToLower(strings.TrimSpace(k))] = v
		}
		cfg.MaxConcurrentByState = normalized
	}

	// Merge deprecated issue_label into issue_labels (OR filter)
	if cfg.IssueLabel != "" {
		found := false
		for _, l := range cfg.IssueLabels {
			if l == cfg.IssueLabel {
				found = true
				break
			}
		}
		if !found {
			cfg.IssueLabels = append(cfg.IssueLabels, cfg.IssueLabel)
		}
	}
	// If no labels configured, IssueLabels stays nil — meaning no label filter
	// (all open issues will be fetched).

	// Expand ~ in paths
	cfg.LocalPath = expandHome(cfg.LocalPath)
	cfg.WorktreeBase = expandHome(cfg.WorktreeBase)
	cfg.Outcome = cfg.Outcome.Normalized()
	if cfg.Outcome.Configured() {
		cfg.Outcome.SourceRepoPath = expandHome(cfg.Outcome.SourceRepoPath)
		if cfg.Outcome.SourceRepoPath == "" {
			cfg.Outcome.SourceRepoPath = cfg.LocalPath
		}
	}
	cfg.WorkerPrompt = expandHome(cfg.WorkerPrompt)
	cfg.BugPrompt = expandHome(cfg.BugPrompt)
	cfg.EnhancementPrompt = expandHome(cfg.EnhancementPrompt)
	cfg.Pipeline.Planner.Prompt = expandHome(cfg.Pipeline.Planner.Prompt)
	cfg.Pipeline.Validator.Prompt = expandHome(cfg.Pipeline.Validator.Prompt)
	cfg.Supervisor.Prompt = expandHome(cfg.Supervisor.Prompt)
	for i, s := range cfg.PromptSections {
		cfg.PromptSections[i] = expandHome(s)
	}
	cfg.StateDir = expandHome(cfg.StateDir)
	cfg.SelfDeploy.Script = expandHome(cfg.SelfDeploy.Script)
	cfg.SelfDeploy.BinPath = expandHome(cfg.SelfDeploy.BinPath)

	// Default session_prefix: first 3 chars of repo name
	if cfg.SessionPrefix == "" {
		parts := strings.Split(cfg.Repo, "/")
		name := parts[len(parts)-1]
		if len(name) >= 3 {
			cfg.SessionPrefix = name[:3]
		} else {
			cfg.SessionPrefix = name
		}
	}

	// Default state_dir: ~/.maestro/<md5-hash-of-repo>
	if cfg.StateDir == "" {
		hash := fmt.Sprintf("%x", md5.Sum([]byte(cfg.Repo)))[:12]
		cfg.StateDir = filepath.Join(os.Getenv("HOME"), ".maestro", hash)
	}

	// Default max_retry_backoff_ms: 300000 (5 minutes)
	if cfg.MaxRetryBackoffMs <= 0 {
		cfg.MaxRetryBackoffMs = 300000
	}

	if cfg.Telegram.Mode == "" {
		cfg.Telegram.Mode = "direct"
	}
	if strings.TrimSpace(cfg.Server.Host) == "" {
		cfg.Server.Host = "127.0.0.1"
	}

	// Model backend defaults
	if cfg.Model.Default == "" {
		cfg.Model.Default = "claude"
	}
	if cfg.Model.Backends == nil {
		cfg.Model.Backends = make(map[string]BackendDef)
	}
	// Backward compat: claude_cmd populates the claude backend if not explicitly set
	if cfg.ClaudeCmd != "" {
		if _, ok := cfg.Model.Backends["claude"]; !ok {
			cfg.Model.Backends["claude"] = BackendDef{Cmd: cfg.ClaudeCmd}
		}
	}

	// Ensure the default backend is always present in the map
	if _, ok := cfg.Model.Backends[cfg.Model.Default]; !ok {
		cfg.Model.Backends[cfg.Model.Default] = BackendDef{Cmd: cfg.Model.Default}
	}

	// #507: refuse to start when a non-agentic backend is wired into the
	// worker chain. A non-agentic backend (e.g. the maestro-freellm
	// text-completion shim) must be available for supervisor sub-tasks,
	// but spawning a worker against it produces fake-PR sessions —
	// instructions emitted as text, never executed. Fail fast at config
	// parse so the daemon never even tries.
	if def, ok := cfg.Model.Backends[cfg.Model.Default]; ok && def.NonAgentic {
		return nil, fmt.Errorf("config: model.default = %q which is marked non_agentic; non-agentic backends cannot drive workers (they emit instructions as text, never execute git/gh). Pick an agentic backend (claude, codex, opencode, ...) and keep the non-agentic helper available for supervisor sub-tasks only", cfg.Model.Default)
	}
	for _, fb := range cfg.Model.FallbackBackends {
		def, ok := cfg.Model.Backends[fb]
		if !ok {
			continue // unknown backend names are caught elsewhere; we only gate non-agentic here.
		}
		if def.NonAgentic {
			return nil, fmt.Errorf("config: model.fallback_backends includes %q which is marked non_agentic; the fallback chain is the worker chain — a non-agentic entry would produce fake-PR sessions when paid backends are exhausted. Remove %q from fallback_backends and use it only for supervisor sub-tasks", fb, fb)
		}
	}

	// Supervisor defaults
	if cfg.Supervisor.Backend == "" {
		cfg.Supervisor.Backend = cfg.Model.Default
	}
	if cfg.Supervisor.AllowedActions == nil {
		cfg.Supervisor.AllowedActions = []string{
			"none",
			"wait_for_running_worker",
			"wait_for_capacity",
			"wait_for_ordered_queue",
			"monitor_open_pr",
			"review_retry_exhausted",
			"check_outcome_health",
			"notify_red",
			"spawn_worker",
			"spawn_repair_worker",
			"spawn_review_repair",
			"label_issue_ready",
			"add_ready_label",
			"open_child_issue",
			"preflight_failed",
		}
	}
	if cfg.Supervisor.ApprovalRequiredActions == nil {
		cfg.Supervisor.ApprovalRequiredActions = []string{
			"review_retry_exhausted",
			"spawn_worker",
			"spawn_repair_worker",
			"spawn_review_repair",
			"label_issue_ready",
			"add_ready_label",
			"open_child_issue",
		}
	}
	// #565 default budget: 1 review-repair attempt per (pr_number, head_sha).
	// The head SHA changes after every push, so a single repair worker is
	// enough to either land the fix or surface to operator review. Operators
	// can override per-project via supervisor.review_repair.max_retries.
	if cfg.Supervisor.ReviewRepair.MaxRetries == 0 {
		cfg.Supervisor.ReviewRepair.MaxRetries = 1
	}

	// Routing defaults
	if cfg.Routing.Mode == "" {
		cfg.Routing.Mode = "manual"
	}
	if cfg.Routing.RouterModel == "" {
		cfg.Routing.RouterModel = "claude"
	}
	if cfg.Routing.RouterModelName == "" {
		cfg.Routing.RouterModelName = "claude-sonnet-4-6"
	}
	if len(cfg.Routing.TaskTypeBackends) > 0 {
		normalized := make(map[string]string, len(cfg.Routing.TaskTypeBackends))
		for taskType, backend := range cfg.Routing.TaskTypeBackends {
			taskType = strings.ToLower(strings.TrimSpace(taskType))
			backend = strings.TrimSpace(backend)
			if !validRoutingTaskType(taskType) {
				return nil, fmt.Errorf("config: routing.task_type_backends has unknown task type %q (valid: refactor, bugfix, test, vision, design, docs, infra)", taskType)
			}
			if backend == "" {
				return nil, fmt.Errorf("config: routing.task_type_backends.%s is empty", taskType)
			}
			if _, ok := cfg.Model.Backends[backend]; !ok {
				return nil, fmt.Errorf("config: routing.task_type_backends.%s = %q is not declared in model.backends", taskType, backend)
			}
			normalized[taskType] = backend
		}
		cfg.Routing.TaskTypeBackends = normalized
	}

	// Versioning defaults
	if cfg.Versioning.DefaultBump == "" {
		cfg.Versioning.DefaultBump = "patch"
	}
	if cfg.Versioning.TagPrefix == "" {
		cfg.Versioning.TagPrefix = "v"
	}

	// Default cleanup_worktrees_on_merge to true if not set
	if cfg.CleanupWorktreesOnMerge == nil {
		t := true
		cfg.CleanupWorktreesOnMerge = &t
	}

	// Merge defaults
	switch strings.ToLower(strings.TrimSpace(cfg.MergeStrategy)) {
	case "", "sequential":
		cfg.MergeStrategy = "sequential"
	case "parallel":
		cfg.MergeStrategy = "parallel"
	default:
		cfg.MergeStrategy = "sequential"
	}
	if cfg.MergeIntervalSeconds <= 0 {
		cfg.MergeIntervalSeconds = 30
	}

	// Review gate defaults
	switch strings.ToLower(strings.TrimSpace(cfg.ReviewGate)) {
	case "", "greptile":
		cfg.ReviewGate = "greptile"
	case "none", "off", "disabled":
		cfg.ReviewGate = "none"
	default:
		cfg.ReviewGate = "greptile"
	}
	cfg.ReviewGateStreams = normalizeReviewGateStreams(cfg.ReviewGate, cfg.ReviewGateStreams)

	// Default hooks timeout
	if cfg.Hooks.TimeoutMs <= 0 {
		cfg.Hooks.TimeoutMs = 60000
	}

	// Default soft token threshold: 0.8 (80% of worker_max_tokens)
	if cfg.WorkerSoftTokenThreshold == nil {
		d := 0.8
		cfg.WorkerSoftTokenThreshold = &d
	}

	// Missions defaults
	if cfg.Missions.MaxChildren <= 0 {
		cfg.Missions.MaxChildren = 10
	}
	if len(cfg.Missions.Labels) == 0 {
		cfg.Missions.Labels = []string{"mission", "epic"}
	}

	// Default blocker patterns: nil means "not set" → use defaults.
	// An explicit empty slice (blocker_patterns: []) means "disabled".
	if cfg.BlockerPatterns == nil {
		cfg.BlockerPatterns = []string{
			`blocked by.*?#(\d+)`,
			`blocked until.*?#(\d+).*merged`,
			`depends on.*?#(\d+)`,
		}
	}

	if err := normalizeSupervisorPolicy(&cfg.Supervisor); err != nil {
		return nil, err
	}

	return cfg, nil
}

func normalizeReviewGateStreams(reviewGate string, streams []string) []string {
	if strings.EqualFold(strings.TrimSpace(reviewGate), "none") {
		return nil
	}
	if len(streams) == 0 {
		return []string{"greptile"}
	}
	out := make([]string, 0, len(streams))
	seen := make(map[string]struct{}, len(streams))
	for _, raw := range streams {
		name := strings.ToLower(strings.TrimSpace(raw))
		switch name {
		case "", "off", "disabled", "none":
			continue
		case "greptile", "simplicity":
			// supported as configured
		default:
			continue
		}
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		out = append(out, name)
	}
	if len(out) == 0 {
		return []string{"greptile"}
	}
	return out
}

func (c *Config) EffectiveReviewGateStreams() []string {
	if c == nil {
		return nil
	}
	return normalizeReviewGateStreams(c.ReviewGate, c.ReviewGateStreams)
}

func validRoutingTaskType(taskType string) bool {
	switch taskType {
	case "refactor", "bugfix", "test", "vision", "design", "docs", "infra":
		return true
	default:
		return false
	}
}

// SupervisorPolicyCandidatePaths returns structured policy files that may live
// beside the config or inside the configured repository checkout.
func SupervisorPolicyCandidatePaths(configPath string, cfg *Config) []string {
	var candidates []string
	if configPath != "" {
		dir := filepath.Dir(configPath)
		candidates = appendSupervisorPolicyCandidates(candidates, dir)
		candidates = appendSupervisorPolicyCandidates(candidates, filepath.Join(dir, ".maestro"))
	}
	if cfg != nil && strings.TrimSpace(cfg.LocalPath) != "" {
		candidates = appendSupervisorPolicyCandidates(candidates, filepath.Join(cfg.LocalPath, ".maestro"))
	}
	return uniqueStrings(candidates)
}

func loadSupervisorPolicyFile(configPath string, cfg *Config) error {
	for _, path := range SupervisorPolicyCandidatePaths(configPath, cfg) {
		data, err := os.ReadFile(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return fmt.Errorf("read supervisor policy %s: %w", path, err)
		}
		policy, ok, err := parseSupervisorPolicyFile(path, data)
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		policy.PolicyPath = path
		if err := normalizeSupervisorPolicy(&policy); err != nil {
			return fmt.Errorf("load supervisor policy %s: %w", path, err)
		}
		cfg.Supervisor = policy
		return nil
	}
	return nil
}

func parseSupervisorPolicyFile(path string, data []byte) (SupervisorConfig, bool, error) {
	data, ok := supervisorPolicyYAML(path, data)
	if !ok {
		return SupervisorConfig{}, false, nil
	}
	var wrapped struct {
		Supervisor *SupervisorConfig `yaml:"supervisor"`
	}
	if err := yaml.Unmarshal(data, &wrapped); err != nil {
		return SupervisorConfig{}, false, fmt.Errorf("parse supervisor policy %s: %w", path, err)
	}
	if wrapped.Supervisor != nil {
		return *wrapped.Supervisor, true, nil
	}
	var policy SupervisorConfig
	if err := yaml.Unmarshal(data, &policy); err != nil {
		return SupervisorConfig{}, false, fmt.Errorf("parse supervisor policy %s: %w", path, err)
	}
	return policy, true, nil
}

func supervisorPolicyYAML(path string, data []byte) ([]byte, bool) {
	if strings.ToLower(filepath.Ext(path)) != ".md" {
		return data, true
	}
	text := strings.TrimLeft(string(data), "\ufeff\r\n\t ")
	if !strings.HasPrefix(text, "---") {
		return nil, false
	}
	text = strings.TrimPrefix(text, "---")
	text = strings.TrimPrefix(text, "\r\n")
	text = strings.TrimPrefix(text, "\n")
	end := strings.Index(text, "\n---")
	if end < 0 {
		return nil, false
	}
	return []byte(text[:end]), true
}

func appendSupervisorPolicyCandidates(candidates []string, dir string) []string {
	if strings.TrimSpace(dir) == "" {
		return candidates
	}
	return append(candidates,
		filepath.Join(dir, "supervisor.yaml"),
		filepath.Join(dir, "supervisor.yml"),
		filepath.Join(dir, "supervisor.md"),
	)
}

func normalizeSupervisorPolicy(policy *SupervisorConfig) error {
	policy.Mode = normalizePolicyToken(policy.Mode)
	if policy.Mode == "" {
		policy.Mode = "cautious"
	}
	policy.ReadyLabel = strings.TrimSpace(policy.ReadyLabel)
	policy.BlockedLabel = strings.TrimSpace(policy.BlockedLabel)
	policy.PreflightCommand = strings.TrimSpace(policy.PreflightCommand)
	if policy.DispatchSLASeconds < 0 {
		return fmt.Errorf("supervisor.dispatch_sla_seconds must be >= 0")
	}
	policy.ExcludedLabels = normalizeStringList(policy.ExcludedLabels)
	policy.AllowIssueTypes = normalizeStringList(policy.AllowIssueTypes)
	policy.SafeActions = normalizeActionList(policy.SafeActions)
	policy.ApprovalRequired = normalizeActionList(policy.ApprovalRequired)
	policy.HandoffPlanner.SourceIssueLabels = normalizeStringList(policy.HandoffPlanner.SourceIssueLabels)
	policy.HandoffPlanner.ChildReadyLabel = strings.TrimSpace(policy.HandoffPlanner.ChildReadyLabel)
	policy.HandoffPlanner.ChildBlockedLabel = strings.TrimSpace(policy.HandoffPlanner.ChildBlockedLabel)
	policy.HandoffPlanner.IssueTemplate = strings.TrimSpace(policy.HandoffPlanner.IssueTemplate)
	policy.HandoffPlanner.PreflightCommand = strings.TrimSpace(policy.HandoffPlanner.PreflightCommand)
	policy.HandoffPlanner.ParseSections = normalizeStringList(policy.HandoffPlanner.ParseSections)
	policy.CompletionGates.RequiredLabels = normalizeStringList(policy.CompletionGates.RequiredLabels)
	policy.CompletionGates.BodyMarkers = normalizeStringList(policy.CompletionGates.BodyMarkers)
	policy.CompletionGates.LiveVisualCommand = strings.TrimSpace(policy.CompletionGates.LiveVisualCommand)
	policy.CompletionGates.DeploymentStatusCmd = strings.TrimSpace(policy.CompletionGates.DeploymentStatusCmd)
	policy.CompletionGates.VerificationLabel = strings.TrimSpace(policy.CompletionGates.VerificationLabel)
	if policy.DynamicWave.DependencyUnblock.Enabled == nil &&
		policy.DynamicWave.Active() &&
		policy.DynamicWave.OwnsReadyLabel &&
		policy.ReadyLabel != "" &&
		policy.BlockedLabel != "" {
		enabled := true
		policy.DynamicWave.DependencyUnblock.Enabled = &enabled
	}

	if !policy.excludedLabelsSet && len(policy.ExcludedLabels) == 0 {
		policy.ExcludedLabels = []string{"epic", "meta"}
	}
	if len(policy.AllowIssueTypes) > 0 {
		policy.ExcludedLabels = removeAllowedIssueTypes(policy.ExcludedLabels, policy.AllowIssueTypes)
	}
	if policy.HandoffPlanner.MaxChildrenPerCycle < 0 {
		policy.HandoffPlanner.MaxChildrenPerCycle = 0
	}
	if policy.HandoffPlanner.MaxOpenChildren < 0 {
		policy.HandoffPlanner.MaxOpenChildren = 0
	}
	return validateSupervisorPolicy(*policy)
}

func validateSupervisorPolicy(policy SupervisorConfig) error {
	seenIssues := make(map[int]struct{}, len(policy.OrderedQueue.Issues))
	for i, issue := range policy.OrderedQueue.Issues {
		if issue <= 0 {
			return fmt.Errorf("config: supervisor.ordered_queue.issues[%d] must be a positive issue number", i)
		}
		if _, ok := seenIssues[issue]; ok {
			return fmt.Errorf("config: supervisor.ordered_queue.issues[%d] duplicates issue #%d", i, issue)
		}
		seenIssues[issue] = struct{}{}
	}
	for i, issue := range policy.OrderedQueue.DoneIssues {
		if issue <= 0 {
			return fmt.Errorf("config: supervisor.ordered_queue.done_issues[%d] must be a positive issue number", i)
		}
	}
	if err := validateSupervisorActions("safe_actions", policy.SafeActions); err != nil {
		return err
	}
	return validateSupervisorActions("approval_required", policy.ApprovalRequired)
}

func validateSupervisorActions(field string, actions []string) error {
	for i, action := range actions {
		if !knownSupervisorActions()[action] {
			return fmt.Errorf("config: supervisor.%s[%d] has unknown action %q (allowed: %s)", field, i, action, strings.Join(knownSupervisorActionNames(), ", "))
		}
	}
	return nil
}

// Warnings returns non-fatal configuration issues an operator should
// address. Each entry is a single-line human-readable sentence; callers
// log them on startup / hot-reload. Returns nil when nothing is wrong so
// callers can early-out without checking length.
//
// #425 (sup-98): hands-off projects (`review_gate` configured for
// auto-merge AND no human in the loop) silently broke when
// `merge_pr` was in `approval_required` — the supervisor recommended
// `merge_pr`, the cautious gate minted an approval, and nothing ever
// merged because nobody was around to click. The warning makes the
// misconfiguration loud at config load time.
func (c *Config) Warnings() []string {
	if c == nil {
		return nil
	}
	var warnings []string
	if msg := c.handsOffMergeApprovalWarning(); msg != "" {
		warnings = append(warnings, msg)
	}
	if msg := c.manualRoutingLabelPinWarning(); msg != "" {
		warnings = append(warnings, msg)
	}
	warnings = append(warnings, c.backendResolutionWarnings()...)
	return warnings
}

// manualRoutingLabelPinWarning surfaces the #427 misconfiguration where the
// project advertises multiple backends but uses no routing mechanism that
// reacts to issue content. In that shape, every dispatched issue picks its
// backend from a model:* label (or falls through to model.default) — which is
// label-pinning, not task-based routing. Operators reading the docs may
// believe Maestro is matching tasks to backends when it is not.
//
// The warning fires only when ALL three are true:
//   - 2+ backends are configured (single-backend setups have nothing to route)
//   - routing.mode is empty or "manual" (auto-routing would be the task lever)
//   - no role-specific backends are set (planner/implementer/validator overrides
//     would be a legitimate role-based shape even without auto)
//
// This keeps the warning silent for the common single-backend project, for
// projects that opted into auto-routing, and for projects that purposefully
// pin per-role backends. It fires loudly for the failure mode in #427.
func (c *Config) manualRoutingLabelPinWarning() string {
	if c == nil {
		return ""
	}
	if len(c.Model.Backends) < 2 {
		return ""
	}
	mode := strings.ToLower(strings.TrimSpace(c.Routing.Mode))
	if mode == "auto" {
		return ""
	}
	if strings.TrimSpace(c.Routing.PlannerBackend) != "" ||
		strings.TrimSpace(c.Routing.ImplementationBackend) != "" ||
		strings.TrimSpace(c.Routing.ValidatorBackend) != "" {
		return ""
	}
	return fmt.Sprintf(
		"config: %d backends are configured but routing.mode is %q and no planner/implementation/validator_backend is set — backend selection will be by model:<name> label or model.default only, not by task content. Set routing.mode: auto for task-based routing or per-role backends for role-based routing.",
		len(c.Model.Backends),
		coalesceRoutingMode(c.Routing.Mode),
	)
}

func coalesceRoutingMode(mode string) string {
	trimmed := strings.ToLower(strings.TrimSpace(mode))
	if trimmed == "" {
		return "manual"
	}
	return trimmed
}

func (c *Config) handsOffMergeApprovalWarning() string {
	if c == nil {
		return ""
	}
	gate := strings.ToLower(strings.TrimSpace(c.ReviewGate))
	// Hands-off here means the project relies on Maestro to drive PRs
	// through the merge gate without a human in the loop. The greptile /
	// none gates are both compatible with hands-off operation; what
	// matters is whether an operator is expected to click the merge
	// button. Today that signal lives in approval_required.
	if gate == "" {
		return ""
	}
	gated := mergeApprovalGatedVerbs(c.Supervisor.ApprovalRequired)
	if len(gated) == 0 {
		return ""
	}
	return fmt.Sprintf(
		"config: hands-off project (review_gate=%s) has %s in supervisor.approval_required — the supervisor will mint an approval for every green PR and merge will block until a human acts. Remove from approval_required or accept the manual step.",
		gate,
		strings.Join(gated, "/"),
	)
}

func mergeApprovalGatedVerbs(approvalRequired []string) []string {
	wanted := map[string]struct{}{
		SupervisorActionMergePR:    {},
		SupervisorActionCloseIssue: {},
	}
	seen := map[string]struct{}{}
	var out []string
	for _, raw := range approvalRequired {
		v := strings.ToLower(strings.TrimSpace(raw))
		if _, ok := wanted[v]; !ok {
			continue
		}
		if _, dup := seen[v]; dup {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	return out
}

func knownSupervisorActions() map[string]bool {
	return map[string]bool{
		SupervisorActionAddReadyLabel:       true,
		SupervisorActionRemoveReadyLabel:    true,
		SupervisorActionAddBlockedLabel:     true,
		SupervisorActionRemoveBlockedLabel:  true,
		SupervisorActionAddIssueComment:     true,
		SupervisorActionMergePR:             true,
		SupervisorActionCloseIssue:          true,
		SupervisorActionCloseIssueBatch:     true,
		SupervisorActionDeleteWorktree:      true,
		SupervisorActionChangeGlobalConfig:  true,
		SupervisorActionApplyLessonProposal: true,
		SupervisorActionSpawnReviewRepair:   true,
		SupervisorActionRestartWorker:       true,
		SupervisorActionStopWorker:          true,
	}
}

func knownSupervisorActionNames() []string {
	return []string{
		SupervisorActionAddReadyLabel,
		SupervisorActionRemoveReadyLabel,
		SupervisorActionAddBlockedLabel,
		SupervisorActionRemoveBlockedLabel,
		SupervisorActionAddIssueComment,
		SupervisorActionMergePR,
		SupervisorActionCloseIssue,
		SupervisorActionCloseIssueBatch,
		SupervisorActionDeleteWorktree,
		SupervisorActionChangeGlobalConfig,
		SupervisorActionApplyLessonProposal,
		SupervisorActionSpawnReviewRepair,
		SupervisorActionRestartWorker,
		SupervisorActionStopWorker,
	}
}

func normalizeActionList(values []string) []string {
	normalized := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		value = normalizePolicyToken(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		normalized = append(normalized, value)
	}
	return normalized
}

func normalizeStringList(values []string) []string {
	normalized := make([]string, 0, len(values))
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
		normalized = append(normalized, value)
	}
	return normalized
}

func normalizePolicyToken(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

func removeAllowedIssueTypes(excluded, allowed []string) []string {
	allowedSet := make(map[string]struct{}, len(allowed))
	for _, label := range allowed {
		allowedSet[strings.ToLower(label)] = struct{}{}
	}
	filtered := excluded[:0]
	for _, label := range excluded {
		if _, ok := allowedSet[strings.ToLower(label)]; ok {
			continue
		}
		filtered = append(filtered, label)
	}
	return filtered
}

func uniqueStrings(values []string) []string {
	unique := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		unique = append(unique, value)
	}
	return unique
}

// LoadDir loads all YAML config files from a directory, sorted by filename.
func LoadDir(dir string) ([]*Config, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read config dir %s: %w", dir, err)
	}
	var cfgs []*Config
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".yaml") && !strings.HasSuffix(name, ".yml") {
			continue
		}
		cfg, err := LoadFrom(filepath.Join(dir, name))
		if err != nil {
			return nil, fmt.Errorf("load %s: %w", name, err)
		}
		cfgs = append(cfgs, cfg)
	}
	if len(cfgs) == 0 {
		return nil, fmt.Errorf("no config files found in %s", dir)
	}
	return cfgs, nil
}

// ShouldCleanupWorktrees returns whether worktrees should be removed after PR merge.
// SoftTokenThreshold returns the soft token threshold fraction (0–1).
// Returns 0 if disabled (pointer is nil or value is 0).
func (c *Config) SoftTokenThreshold() float64 {
	if c.WorkerSoftTokenThreshold == nil {
		return 0
	}
	return *c.WorkerSoftTokenThreshold
}

func (c *Config) ShouldCleanupWorktrees() bool {
	if c.CleanupWorktreesOnMerge == nil {
		return true
	}
	return *c.CleanupWorktreesOnMerge
}

// ResolvePath returns the config file path, using SourcePath if set, otherwise the default candidate.
func (c *Config) ResolvePath() string {
	if c.SourcePath != "" {
		return c.SourcePath
	}
	candidates := []string{
		"maestro.yaml",
		"maestro.yml",
		filepath.Join(os.Getenv("HOME"), ".maestro", "config.yaml"),
		filepath.Join(os.Getenv("HOME"), ".maestro", "config.yml"),
	}
	for _, p := range candidates {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return "maestro.yaml"
}

func expandHome(path string) string {
	if len(path) > 1 && path[:2] == "~/" {
		return filepath.Join(os.Getenv("HOME"), path[2:])
	}
	return path
}

// MergeExhaustedNonCriticalReviewEnabled reports whether the orchestrator may
// merge a green PR that exhausted its review-feedback retries when only
// non-critical (P1/P2/P3) Greptile findings remain on head. Defaults to true
// (nil) so hands-off delivery converges instead of dead-ending forever; set
// `merge_exhausted_noncritical_review: false` to require manual handling.
func (c *Config) MergeExhaustedNonCriticalReviewEnabled() bool {
	if c == nil || c.MergeExhaustedNonCriticalReview == nil {
		return true
	}
	return *c.MergeExhaustedNonCriticalReview
}
