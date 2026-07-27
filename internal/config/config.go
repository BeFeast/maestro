package config

import (
	"bytes"
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	pathpkg "path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/befeast/maestro/internal/outcome"
	"github.com/befeast/maestro/internal/progress"
	"gopkg.in/yaml.v3"
)

// TelegramConfig configures the Telegram notification channel.
//
// The bot token is a credential and MUST NOT be stored in config (#1143) --
// the same rule ServerAuthConfig states for the dashboard token. Set
// BotTokenEnv to the name of an environment variable the operator populates
// from a secret manager (Infisical, 1Password, ...); the process reads the
// value at send time and the config store only ever holds the variable name,
// so `maestro config-store export` and every DB backup stay credential-free.
//
// The legacy plaintext BotToken still works so existing rows keep notifying,
// but Config.Warnings() names the field at load time so an operator migrates.
// When BotTokenEnv resolves to a non-empty value it wins over BotToken.
type TelegramConfig struct {
	Target string `yaml:"target"`
	// BotToken is the DEPRECATED plaintext form of the bot token (#1143).
	// Prefer BotTokenEnv; a non-empty value here raises a config warning.
	BotToken string `yaml:"bot_token"`
	// BotTokenEnv names an environment variable holding the bot token; the
	// token itself is never stored here.
	BotTokenEnv string `yaml:"bot_token_env"`
	Mode        string `yaml:"mode"`         // "direct" (Telegram Bot API) or "openclaw" (OpenClaw relay); default: "direct"
	OpenclawURL string `yaml:"openclaw_url"` // only needed when mode=openclaw
	DigestMode  bool   `yaml:"digest_mode"`  // batch notifications per cycle instead of sending immediately
}

// Token resolves the Telegram bot token. BotTokenEnv is consulted first: when
// it names an environment variable that holds a non-empty value, that value is
// used and the plaintext BotToken is ignored. Otherwise the legacy plaintext
// BotToken is returned so pre-#1143 rows keep working. Returns "" when neither
// source yields a value, which callers treat as "Telegram direct mode not
// configured" (the openclaw relay path is unaffected).
func (c TelegramConfig) Token() string {
	if env := strings.TrimSpace(c.BotTokenEnv); env != "" {
		if v := strings.TrimSpace(os.Getenv(env)); v != "" {
			return v
		}
	}
	return strings.TrimSpace(c.BotToken)
}

// PlaintextTokenActive reports whether the plaintext BotToken is the value
// Token() would return -- i.e. the credential is really being read out of the
// config store rather than the environment.
func (c TelegramConfig) PlaintextTokenActive() bool {
	if strings.TrimSpace(c.BotToken) == "" {
		return false
	}
	if env := strings.TrimSpace(c.BotTokenEnv); env != "" {
		if strings.TrimSpace(os.Getenv(env)) != "" {
			return false
		}
	}
	return true
}

// NotifyConfig (#1018) carries lightweight push-notification transports that
// live alongside the Telegram digest channel. The first transport is ntfy —
// a plain HTTP POST push channel the supervisor/orchestrator/watchdog use to
// emit alert-class notifications (gate-fail streaks, idle stalls, delivery
// advances). Absent config = no ntfy fan-out; the Telegram path is unaffected.
type NotifyConfig struct {
	Ntfy NtfyConfig `yaml:"ntfy"`
}

// NtfyConfig configures the ntfy push transport. Topic is a per-project value
// (fleet config supplies the default, a project overrides it). The auth token
// is NEVER stored in config or repo: TokenEnv names an environment variable /
// secret-store key the process reads at send time.
type NtfyConfig struct {
	BaseURL  string `yaml:"base_url"`  // e.g. https://ntfy.sh or a self-hosted base URL
	Topic    string `yaml:"topic"`     // per-project topic (fleet default, project override)
	TokenEnv string `yaml:"token_env"` // env var name holding the bearer token; the token itself is never stored here
}

// Enabled reports whether the ntfy transport has enough config to POST.
func (c NtfyConfig) Enabled() bool {
	return strings.TrimSpace(c.BaseURL) != "" && strings.TrimSpace(c.Topic) != ""
}

// Token resolves the ntfy bearer token from the configured env var. Returns ""
// when TokenEnv is unset or the env var is unset/empty. Callers treat "" as
// "unauthenticated ntfy" (public topics need no token).
func (c NtfyConfig) Token() string {
	env := strings.TrimSpace(c.TokenEnv)
	if env == "" {
		return ""
	}
	return strings.TrimSpace(os.Getenv(env))
}

// BackendDef defines a model backend CLI.
type BackendDef struct {
	Cmd        string    `yaml:"cmd"`
	ExtraArgs  []string  `yaml:"extra_args"`
	PromptMode string    `yaml:"prompt_mode"` // how to deliver prompt: "arg" (last argument), "stdin" (via stdin), "file" (file path as argument)
	Enabled    *bool     `yaml:"enabled"`     // nil means enabled for backward compatibility
	MCP        MCPConfig `yaml:"mcp,omitempty"`

	// #513/#1000: optional per-backend attribution metadata. Maestro stores it
	// in durable session state and surfaces it in Fleet Mission Control; it must
	// never be injected into product commits. All four fields are optional, and
	// absent values render as "—" in the UI (legacy behavior preserved).
	Provider string `yaml:"provider,omitempty"` // e.g. "anthropic", "openai", "groq"
	Model    string `yaml:"model,omitempty"`    // e.g. "opus-4.8", "gpt-5.5", "llama-3.3-70b-versatile"
	Variant  string `yaml:"variant,omitempty"`  // e.g. "opus[1m]", "fast", "sonnet"
	Effort   string `yaml:"effort,omitempty"`   // e.g. "xhigh", "medium", "low"

	// #783/#792 (P1-A): a routing tier's per-tier model/effort override, carried
	// DISTINCTLY from the #513 attribution Model/Effort above. These are never
	// parsed from YAML — the orchestrator injects them onto a config clone via
	// applyTierOverride only for a real policy-resolved tier override. Only these
	// reach the worker argv (see worker.appendTierModelEffort); the #513
	// attribution Model/Effort must NOT leak into argv for a non-policy config.
	TierModel  string `yaml:"-" json:"-"`
	TierEffort string `yaml:"-" json:"-"`

	// #737/#947: opt-in structured usage capture for backends whose JSON mode
	// is optional (claude/codex/opencode). The worker runner pipes NDJSON through
	// `maestro stream-split`, which writes raw frames to slot.jsonl for usage
	// accounting while keeping slot.log human-readable. Kimi is structured by
	// default and does not require this flag.
	UsageStream bool `yaml:"usage_stream,omitempty"`

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

	// PricingClass declares how the backend bills so always-on internal
	// loops (supervisor cycles, the auto-router) can refuse to run their
	// LLM path on a per-token model without an explicit opt-in (#838). One
	// of PricingClassFlat / PricingClassSubscription / PricingClassMetered;
	// empty is treated as flat for backward compatibility. Only an explicit
	// `metered` gates the loops — a backend that leaves this unset keeps
	// running unchanged, even when it has a `pricing:` table set for cost
	// observability (a subscription backend legitimately configures pricing
	// for #619). parse() rejects any value outside the three classes.
	PricingClass string `yaml:"pricing_class,omitempty"`

	// Quota describes the backend's subscription window capacity (#704).
	// When configured, maestro tracks estimated token usage against the
	// provider's 5-hour session window and weekly cap, surfaces the
	// percentages on the fleet API / Mission Control, and steers fresh
	// dispatch to fallback backends once usage crosses the threshold.
	Quota BackendQuota `yaml:"quota,omitempty"`

	// UsageLimitPatterns (#805) holds extra regexes that classify this
	// backend's dead-worker log tail as an account-quota exhaustion, in
	// addition to the built-in signatures (codex "You've hit your usage
	// limit", the codex settings/usage URL, claude "usage limit reached").
	// A quota death gates the backend in backend_health and fails the
	// attempt over to the next fallback backend without consuming the
	// per-issue retry budget. Keep entries high-precision: a pattern that
	// matches ordinary work output (a prompt echo, test output) causes
	// false backend gating. Validated to compile at config parse.
	UsageLimitPatterns []string `yaml:"usage_limit_patterns,omitempty"`

	// SupervisorAttemptTimeoutSeconds overrides supervisor.attempt_timeout_seconds
	// for this backend when it serves a supervisor consult. Set it on slow
	// print-mode carriers (claude CLI at high effort ≈ 180) so the walk gives
	// them a real chance instead of billing a generation and killing it at the
	// 45s default. Workers are unaffected — this is a supervisor-only knob.
	SupervisorAttemptTimeoutSeconds int `yaml:"supervisor_attempt_timeout_seconds,omitempty"`

	// SubagentHint steers an orchestrating backend (e.g. Claude Code) to
	// delegate grunt subtasks to cheaper sub-agent models instead of
	// reusing the expensive orchestrator model for everything (#706). When
	// set, the worker prompt gains a "Sub-agent Model Policy" section
	// carrying this text. When empty, the prompt is unchanged — configs
	// without the field behave exactly as before. DefaultSubagentHint is a
	// ready-made value operators can paste under their claude backend.
	SubagentHint string `yaml:"subagent_hint,omitempty"`
}

// DefaultSubagentHint is the recommended sub-agent model policy shipped for
// the claude backend (#706). Orchestrating CLIs spawn their own sub-agents
// and, by default, run them on the same expensive model — bulk grunt subtasks
// (file sweeps, searches, mechanical edits) then burn the subscription window
// at orchestrator-model prices, multiplied across parallel sub-agents. Set
// model.backends.<name>.subagent_hint to this (or a tuned variant) to opt in;
// leaving it unset keeps the prompt unchanged.
const DefaultSubagentHint = "Use cheaper sub-agent models (e.g. opus/sonnet) for delegated grunt subtasks such as file sweeps, searches, and mechanical edits; reserve the main orchestrator model for planning and final review."

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
// observability rollup (#619). Rates are USD per million tokens. All
// fields default to 0; a backend whose pricing is unset is reported
// with tokens only ("price not configured") so it degrades gracefully.
type BackendPricing struct {
	InputUSDPerMtok  float64 `yaml:"input_usd_per_mtok,omitempty"`
	OutputUSDPerMtok float64 `yaml:"output_usd_per_mtok,omitempty"`
	// CacheReadUSDPerMtok prices reused (cache_read) input tokens — the bulk
	// of an agentic run's tokens, since the full context is re-sent every
	// turn and served from the prompt cache (#739). When unset (0) it
	// defaults to DefaultCacheReadMultiplier × InputUSDPerMtok (Anthropic's
	// cache-read rate). Set it to override for a non-Anthropic provider.
	CacheReadUSDPerMtok float64 `yaml:"cache_read_usd_per_mtok,omitempty"`
	// CacheWriteUSDPerMtok prices freshly written (cache_creation) input
	// tokens. When unset (0) it defaults to DefaultCacheWriteMultiplier ×
	// InputUSDPerMtok (Anthropic's 5-minute cache-write rate).
	CacheWriteUSDPerMtok float64 `yaml:"cache_write_usd_per_mtok,omitempty"`
}

// Anthropic's published cache rates relative to the base input rate: a
// cache read is ~10% of input, a 5-minute cache write ~125% of input.
// These are the defaults applied when a backend leaves the cache rates
// unset; operators on a different provider can override per-backend.
const (
	DefaultCacheReadMultiplier  = 0.1
	DefaultCacheWriteMultiplier = 1.25
)

// Configured reports whether at least one rate is non-zero. Callers use
// this to switch a backend's cost cell between a $ value and a
// "price not configured" hint.
func (p BackendPricing) Configured() bool {
	return p.InputUSDPerMtok > 0 || p.OutputUSDPerMtok > 0
}

// CacheReadRate returns the effective cache-read rate (USD/Mtok): the
// configured rate when set, otherwise DefaultCacheReadMultiplier × input.
func (p BackendPricing) CacheReadRate() float64 {
	if p.CacheReadUSDPerMtok > 0 {
		return p.CacheReadUSDPerMtok
	}
	return DefaultCacheReadMultiplier * p.InputUSDPerMtok
}

// CacheWriteRate returns the effective cache-write rate (USD/Mtok): the
// configured rate when set, otherwise DefaultCacheWriteMultiplier × input.
func (p BackendPricing) CacheWriteRate() float64 {
	if p.CacheWriteUSDPerMtok > 0 {
		return p.CacheWriteUSDPerMtok
	}
	return DefaultCacheWriteMultiplier * p.InputUSDPerMtok
}

// EstimateCostUSD returns the USD estimate for the given total token
// count using a 50/50 input/output blend. Workers record a single
// combined token counter so a finer split is not available; operators
// who only price one side can leave the other at zero and the average
// halves the cost as expected. Returns 0 when no rates are set or
// tokens is non-positive.
//
// Prefer EstimateCostSplit when the backend stamps per-dimension tokens
// (#739): the blend over-states an agentic run's cost by a large factor
// because it prices cache_read tokens — most of an agentic run — at the
// full input rate instead of the ~10% cache-read rate.
func (p BackendPricing) EstimateCostUSD(tokens int) float64 {
	if tokens <= 0 || !p.Configured() {
		return 0
	}
	rate := (p.InputUSDPerMtok + p.OutputUSDPerMtok) / 2
	return float64(tokens) * rate / 1_000_000.0
}

// EstimateCostSplit returns the USD estimate for a cache-aware token
// breakdown (#739). Each dimension is priced with its own rate: input and
// output at the configured rates, cache_read and cache_write at the
// (defaulted) cache rates. Negative counts are treated as zero. Returns 0
// when the backend has no pricing configured.
//
// This is the cache-aware replacement for the blended EstimateCostUSD: an
// agentic run re-sends its full context every turn, so cache_read tokens
// dominate and the ~10% cache-read rate makes the realistic cost a small
// fraction of the naive blend.
func (p BackendPricing) EstimateCostSplit(input, output, cacheRead, cacheWrite int) float64 {
	if !p.Configured() {
		return 0
	}
	cost := nonNegTokens(input)*p.InputUSDPerMtok +
		nonNegTokens(output)*p.OutputUSDPerMtok +
		nonNegTokens(cacheRead)*p.CacheReadRate() +
		nonNegTokens(cacheWrite)*p.CacheWriteRate()
	return cost / 1_000_000.0
}

// nonNegTokens clamps a token count to a non-negative float so a stray
// negative counter cannot subtract from a cost estimate.
func nonNegTokens(n int) float64 {
	if n <= 0 {
		return 0
	}
	return float64(n)
}

// BackendQuota calibrates a subscription-style backend against its
// provider's usage windows (#704). Subscription plans (e.g. Claude
// Max) meter a 5-hour rolling session window plus a weekly cap, and
// the provider exposes no programmatic usage API — so maestro
// estimates usage from its own per-session token counters against
// these operator-calibrated capacities. Both capacities are optional;
// a backend with neither set is not quota-tracked.
type BackendQuota struct {
	// WindowTokens is the estimated token capacity of one 5-hour
	// session window. Calibrate by reading `claude /status` (or the
	// provider dashboard) at a known usage percent and dividing the
	// fleet's token burn for that window by that fraction.
	WindowTokens int `yaml:"window_tokens,omitempty"`
	// WeeklyTokens is the estimated token capacity of the weekly cap,
	// calibrated the same way against the weekly usage readout.
	WeeklyTokens int `yaml:"weekly_tokens,omitempty"`
	// DispatchThreshold is the used fraction (0..1) above which fresh
	// dispatches prefer the next fallback backend until the window
	// resets. Defaults to 0.85 when unset.
	DispatchThreshold float64 `yaml:"dispatch_threshold,omitempty"`
}

// Configured reports whether at least one window capacity is set, i.e.
// whether the backend participates in quota tracking at all.
func (q BackendQuota) Configured() bool {
	return q.WindowTokens > 0 || q.WeeklyTokens > 0
}

// DefaultQuotaDispatchThreshold is the used fraction above which fresh
// dispatch prefers fallback backends when quota.dispatch_threshold is
// not set (#704: "default 85%").
const DefaultQuotaDispatchThreshold = 0.85

// EffectiveDispatchThreshold returns the configured threshold or the
// 0.85 default. parse() rejects values outside (0, 1].
func (q BackendQuota) EffectiveDispatchThreshold() float64 {
	if q.DispatchThreshold > 0 {
		return q.DispatchThreshold
	}
	return DefaultQuotaDispatchThreshold
}

func (b BackendDef) IsEnabled() bool {
	return b.Enabled == nil || *b.Enabled
}

// Backend pricing classes (#838). Only PricingClassMetered gates the always-on
// supervisor/router LLM loops; flat, subscription, and unset run unchanged.
const (
	PricingClassFlat         = "flat"
	PricingClassSubscription = "subscription"
	PricingClassMetered      = "metered"
)

// IsMetered reports whether the backend bills per token in a way that must not
// back an unbounded always-on loop without an explicit opt-in (#838). Only an
// explicit `pricing_class: metered` qualifies; an unset class — even with a
// `pricing:` table configured for cost observability (#619) — is treated as
// flat so existing subscription backends keep running unchanged.
func (b BackendDef) IsMetered() bool {
	return strings.EqualFold(strings.TrimSpace(b.PricingClass), PricingClassMetered)
}

// validPricingClass reports whether a pricing_class string is one of the known
// classes (or empty, which defaults to flat). parse() uses it to reject typos
// that would otherwise silently disable the metered guard.
func validPricingClass(class string) bool {
	switch strings.ToLower(strings.TrimSpace(class)) {
	case "", PricingClassFlat, PricingClassSubscription, PricingClassMetered:
		return true
	default:
		return false
	}
}

// ModelConfig holds multi-backend configuration.
type ModelConfig struct {
	Default          string                `yaml:"default"` // "claude", "codex", etc.
	Backends         map[string]BackendDef `yaml:"backends"`
	FallbackBackends []string              `yaml:"fallback_backends"` // ordered list of backends to try when rate-limited
	// ProviderLanes declares provider-local defaults and fallback order. When
	// fallback_backends is set, that legacy explicit backend chain remains the
	// project override. Otherwise lanes compose in declaration order, with each
	// provider's default followed by its local fallbacks before the next provider.
	ProviderLanes []ProviderLane `yaml:"provider_lanes,omitempty"`
	// HoldOnCooldown makes quota-class backend cooldowns a wait, not a cascade:
	// when the routed backend is cooling down with a known reset inside the
	// configured window, dispatch/retry hold for that reset instead of walking
	// the fallback chain. See HoldOnCooldownConfig.
	HoldOnCooldown HoldOnCooldownConfig `yaml:"hold_on_cooldown,omitempty"`
}

// HoldOnCooldownConfig controls hold-vs-cascade semantics for quota-class
// backend cooldowns (provider_limit, usage_limit, model_cooldown,
// model_overloaded, quota_pressure). Off, a cooling backend cascades work onto
// the next fallback rung immediately — under a fleet-wide primary cooldown
// that dumps every project onto one fallback seat (2026-07 live: an Anthropic
// cooldown pushed ~80% of the week's tokens through the single ChatGPT seat).
// On, a cooldown whose RetryAfter lands within max_wait_minutes holds the work
// for the reset; only unknown or beyond-window resets (weekly caps), and
// non-quota failures (auth_failure, model_unavailable, disabled), still
// cascade.
type HoldOnCooldownConfig struct {
	Enabled bool `yaml:"enabled"`
	// MaxWaitMinutes bounds how long a hold may wait for a stated reset.
	// 0 means the default (360 = 6h, covering a 5-hour subscription window);
	// resets further out than this cascade to the next rung as before.
	MaxWaitMinutes int `yaml:"max_wait_minutes,omitempty"`
}

// defaultHoldOnCooldownMaxWait covers a full 5-hour subscription window with
// slack; a stated reset beyond it (e.g. a weekly cap) is not worth idling for.
const defaultHoldOnCooldownMaxWait = 360 * time.Minute

// MaxWait returns the bounded hold window as a duration.
func (h HoldOnCooldownConfig) MaxWait() time.Duration {
	if h.MaxWaitMinutes > 0 {
		return time.Duration(h.MaxWaitMinutes) * time.Minute
	}
	return defaultHoldOnCooldownMaxWait
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

// GitHubAppConfig holds credentials for authenticating as a GitHub App
// installation instead of a personal access token (#823). When populated the
// daemon signs a JWT with the App's private key, exchanges it for a
// short-lived installation access token, and auto-refreshes before expiry.
// The PAT/`gh` path stays as fallback when this block is absent or incomplete.
//
// The private key MUST live on disk — `private_key_path` points to the PEM
// file. The key is never read into config, never logged, and never stored in
// the config store; only the path is persisted.
type GitHubAppConfig struct {
	AppID          int64  `yaml:"app_id"`           // GitHub App ID (numeric)
	PrivateKeyPath string `yaml:"private_key_path"` // path to the PEM-encoded RSA private key
	InstallationID int64  `yaml:"installation_id"`  // installation ID for the target org/account
}

// Configured reports whether all three required fields are set.
func (c GitHubAppConfig) Configured() bool {
	return c.AppID > 0 && c.InstallationID > 0 && strings.TrimSpace(c.PrivateKeyPath) != ""
}

// Mirror source modes for supervisor/orchestrator reads (#826, epic #811 phase
// D). The GitHub read-model mirror lives in maestro.db (phase C); this selects
// whether the read paths consult it.
const (
	// GitHubSourceAPI reads every supervisor/orchestrator GitHub state directly
	// from the API — today's behavior, and the fleet-wide escape hatch.
	GitHubSourceAPI = "api"
	// GitHubSourceMirrorFirst serves the high-volume reads (open-issue/open-PR
	// lists, issue/PR state) from a warm local mirror, falling back to the API on
	// a miss or a stale row.
	GitHubSourceMirrorFirst = "mirror-first"
)

// GitHubMirrorConfig selects how the supervisor/orchestrator read GitHub state
// (#826). Empty defaults to "api" (today's behavior) until the mirror-first path
// has soaked in production, after which the default flips to "mirror-first".
//
// Setting `source: api` is the fleet-wide escape hatch: the source is consulted
// on every read, so a live config-store edit restores API-direct reads without a
// redeploy (#826 AC 3/8). GitHub stays authoritative for all writes and
// merge-gating reads regardless of this setting.
type GitHubMirrorConfig struct {
	Source       string `yaml:"source"`        // "" (=api) | "api" | "mirror-first"
	StaleSeconds int    `yaml:"stale_seconds"` // freshness horizon override in seconds; 0 = default (24h)
	// ReconcileSeconds is the cadence of the low-frequency reconciliation loop
	// (#827, phase E) that snapshots GitHub and repairs mirror drift a missed
	// webhook left behind. 0 = default (DefaultMirrorReconcileInterval). The loop
	// only runs when the mirror is open (webhook ingestion configured); a caught-up
	// mirror costs near-zero quota per pass because the snapshot reads answer 304.
	ReconcileSeconds int `yaml:"reconcile_seconds"`
}

// DefaultMirrorReconcileInterval is the reconciliation cadence used when
// github_mirror.reconcile_seconds is unset. Low by design: reconciliation is a
// safety net over webhook ingestion, not the primary refresh path, and each pass
// is cheap on an unchanged repo (conditional 304 reads), so a 15-minute sweep
// keeps drift bounded without adding meaningful API load.
const DefaultMirrorReconcileInterval = 15 * time.Minute

// MirrorFirst reports whether reads should be served mirror-first. Any value
// other than "mirror-first" (including the empty default and typos) is API — the
// safe direction, since a mis-set flag can only fall back to the authoritative
// API, never fabricate a stale mirror read.
func (c GitHubMirrorConfig) MirrorFirst() bool {
	return strings.EqualFold(strings.TrimSpace(c.Source), GitHubSourceMirrorFirst)
}

// StaleHorizon is the freshness window a mirror row must fall within to be
// served locally. A non-positive stale_seconds falls back to 24h — the same
// conservative horizon mirrorstore.DefaultStaleHorizon uses.
func (c GitHubMirrorConfig) StaleHorizon() time.Duration {
	if c.StaleSeconds <= 0 {
		return 24 * time.Hour
	}
	return time.Duration(c.StaleSeconds) * time.Second
}

// ReconcileInterval is the cadence of the mirror reconciliation loop (#827). A
// non-positive reconcile_seconds falls back to DefaultMirrorReconcileInterval.
func (c GitHubMirrorConfig) ReconcileInterval() time.Duration {
	if c.ReconcileSeconds <= 0 {
		return DefaultMirrorReconcileInterval
	}
	return time.Duration(c.ReconcileSeconds) * time.Second
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
	Script         string   `yaml:"script"`           // deploy script path override; empty = stage origin/main:scripts/self-deploy.sh into state_dir (#1077), with checkout fallback
	BinPath        string   `yaml:"bin_path"`         // install target (default: path of the running binary)
	InstallViaSudo bool     `yaml:"install_via_sudo"` // #711: stage/rename/rollback bin_path via `sudo -n` so a root-owned target (e.g. /usr/local/bin/maestro) can be updated by the unprivileged deploy user; requires passwordless sudo (default false)
	Scope          string   `yaml:"scope"`            // #716: systemd unit scope — "user" (systemctl --user, default for back-compat) or "system" (sudo -n systemctl restart, for system units like the Loki fleet)
	Units          []string `yaml:"units"`            // systemd units to restart (default: ["maestro.service"])
	HealthURL      string   `yaml:"health_url"`       // running-process version probe (default: http://127.0.0.1:<server.port>/api/v1/state when server.port > 0)
	HealthTokenEnv string   `yaml:"health_token_env"` // env var holding the bearer token for health_url (default: server.auth.token_env)
	TimeoutMinutes int      `yaml:"timeout_minutes"`  // build+install+restart+verify budget; must cover unit drain (default: 30)
	// RestartTimeoutSeconds bounds only the blocking systemctl restart step. It is
	// deliberately much smaller than TimeoutMinutes so Fleet unavailability is
	// reported shortly after the daemon's bounded drain, not hidden under the
	// overall build/deploy budget (#966).
	RestartTimeoutSeconds int `yaml:"restart_timeout_seconds"`

	// #722: minimum interval between self-deploy triggers. The deploy restarts
	// the run-loop's own unit, so a burst of merges — or a run-loop restarted by
	// its own deploy — can re-fire a new deploy while a previous one is still in
	// flight (build + drain + verify + rollback), producing a self-triggering
	// cascade that bounces the fleet web process mid-verify so verify never
	// converges. The orchestrator debounces re-triggers within this window.
	// Default: timeout_minutes (at most one deploy per budget).
	MinIntervalMinutes int `yaml:"min_interval_minutes"`
}

// SelfDeployScope* are the valid values for SelfDeployConfig.Scope.
const (
	SelfDeployScopeUser   = "user"   // per-user systemd manager (systemctl --user)
	SelfDeployScopeSystem = "system" // system systemd manager (sudo -n systemctl)
)

// EffectiveScope returns the systemd unit scope, defaulting to "user" for
// back-compat (#716). Any value other than "system" maps to "user".
func (c SelfDeployConfig) EffectiveScope() string {
	if strings.EqualFold(strings.TrimSpace(c.Scope), SelfDeployScopeSystem) {
		return SelfDeployScopeSystem
	}
	return SelfDeployScopeUser
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

// DefaultSelfDeployRestartTimeoutSeconds covers the daemon's default four-minute
// whole-shutdown deadline plus a fixed 30-second process-start grace. Keep this
// separate from the 30-minute build/deploy timeout: a blocked restart means Fleet
// is unavailable and must surface promptly (#966).
const DefaultSelfDeployRestartTimeoutSeconds = 270

func (c SelfDeployConfig) EffectiveRestartTimeoutSeconds() int {
	if c.RestartTimeoutSeconds > 0 {
		return c.RestartTimeoutSeconds
	}
	return DefaultSelfDeployRestartTimeoutSeconds
}

// EffectiveMinIntervalMinutes returns the debounce window between self-deploy
// triggers (#722). Because the deploy restarts the run-loop's own unit, a burst
// of merges (or a run-loop restarted mid-deploy) must not re-fire a fresh
// deploy while a previous one may still be in flight — that is the
// self-triggering cascade. Defaults to the deploy timeout so at most one deploy
// runs per budget; set explicitly to allow faster back-to-back deploys.
func (c SelfDeployConfig) EffectiveMinIntervalMinutes() int {
	if c.MinIntervalMinutes > 0 {
		return c.MinIntervalMinutes
	}
	return c.EffectiveTimeoutMinutes()
}

const (
	SupervisorActionAddReadyLabel       = "add_ready_label"
	SupervisorActionRemoveReadyLabel    = "remove_ready_label"
	SupervisorActionAddBlockedLabel     = "add_blocked_label"
	SupervisorActionRemoveBlockedLabel  = "remove_blocked_label"
	SupervisorActionHoldMerge           = "hold_merge"
	SupervisorActionReleaseMerge        = "release_merge"
	SupervisorActionAddIssueComment     = "add_issue_comment"
	SupervisorActionMergePR             = "merge_pr"
	SupervisorActionCloseIssue          = "close_issue"
	SupervisorActionCloseIssueBatch     = "close_issue_batch"
	SupervisorActionDeleteWorktree      = "delete_worktree"
	SupervisorActionChangeGlobalConfig  = "change_global_config"
	SupervisorActionApplyLessonProposal = "apply_lesson_proposal"
	// SupervisorActionEditIssueBody applies a groomed issue-body rewrite
	// (#851). It is approval-gated: the spec-groom step posts the proposed
	// rewrite as a comment and mints this approval carrying the new body on
	// Target.Body; the approver executor calls gh.EditIssueBody on approve,
	// and a reject leaves the issue untouched.
	SupervisorActionEditIssueBody = "edit_issue_body"
	// SupervisorActionRestartWorker / SupervisorActionStopWorker are the
	// per-session worker-control verbs surfaced by the fleet snapshot
	// (#567). Both are approval-gated: the operator clicks Restart/Stop on
	// a worker row, the server enqueues a pending Approval, and the
	// approver executor calls the WorkerController to kill the tmux
	// session + (for restart) mark the slot for the next dispatcher
	// respawn.
	SupervisorActionRestartWorker = "restart_worker"
	SupervisorActionStopWorker    = "stop_worker"
	// SupervisorActionSpawnRepairWorker resumes one explicitly reserved
	// issue/session in place. Unlike restart_worker it preserves a retained
	// worktree and never allocates a replacement slot.
	SupervisorActionSpawnRepairWorker = "spawn_repair_worker"
	// SupervisorActionRestartStaleBackendWorkers is a fleet helper surfaced
	// when running workers' immutable attribution differs from the current
	// effective backend settings (#900). It only enqueues per-worker
	// restart_worker approvals for PR-less workers; open-PR workers are skipped
	// and must use in-place repair/handoff.
	SupervisorActionRestartStaleBackendWorkers = "restart_stale_backend_workers"
	// SupervisorActionSpawnReviewRepair is the auto review-repair respawn verb
	// minted by the supervisor when a green+mergeable PR is settled
	// retry_exhausted on review feedback and at least one Greptile P0/P1
	// inline comment is still on the head SHA (#565). The orchestrator
	// dispatcher spawns a scoped opus repair worker keyed on
	// (pr_number, head_sha) so the same head is never re-attempted.
	SupervisorActionSpawnReviewRepair = "spawn_review_repair"
	// SupervisorActionDeployProject is the #872 approval-gated post-merge
	// delivery verb. In delivery.mode=approval_required a qualifying merge
	// enqueues this approval carrying the exact merged revision plus
	// operator-safe target/rollback/verification context; only an operator
	// approve runs the project's deploy/install command and live verifier.
	// The approver runs it exactly once behind a durable approved→executing
	// claim so a daemon restart cannot replay an in-flight delivery.
	SupervisorActionDeployProject = "deploy_project"
)

// DeliveryMode selects the post-merge delivery behavior for a project (#872).
type DeliveryMode string

const (
	// DeliveryModeDisabled runs no post-merge delivery.
	DeliveryModeDisabled DeliveryMode = "disabled"
	// DeliveryModeApprovalRequired enqueues a pending deploy_project approval
	// after a qualifying merge and executes the delivery command only when an
	// operator approves it. This is the default for a project that configures
	// a delivery command via the new block without naming a mode.
	DeliveryModeApprovalRequired DeliveryMode = "approval_required"
	// DeliveryModeAutomatic runs the delivery command immediately after a
	// qualifying merge — the legacy deploy_cmd behavior, retained for
	// back-compat and only reachable by an explicit per-project opt-in.
	DeliveryModeAutomatic DeliveryMode = "automatic"
)

// DeliveryConfig is the #872 approval-gated post-merge delivery block. It
// supersedes the legacy top-level deploy_cmd/deploy_timeout_minutes fields
// (which fold into DeliveryModeAutomatic for back-compat via
// Config.EffectiveDelivery). A fresh project should set delivery.mode:
// approval_required so a merged revision creates an auditable approval instead
// of an immediate, unattended deploy.
type DeliveryConfig struct {
	// Mode is disabled | approval_required | automatic. Empty defaults to
	// approval_required when a Command is set (the safe default), else disabled.
	Mode DeliveryMode `yaml:"mode" json:"mode,omitempty"`
	// Command is the deploy/install entrypoint. In approval_required it must be
	// one argument-free ./repo/relative executable and runs from the isolated
	// exact-SHA checkout; automatic/legacy mode retains shell-command semantics.
	Command string `yaml:"command" json:"command,omitempty"`
	// TimeoutMinutes bounds Command (and VerifyCommand). Default 15.
	TimeoutMinutes int `yaml:"timeout_minutes" json:"timeout_minutes,omitempty"`
	// ApprovalTimeoutMinutes bounds how long a pending delivery approval stays
	// actionable. The default is 24 hours; an expired approval is retained in
	// audit history but can never claim execution.
	ApprovalTimeoutMinutes int `yaml:"approval_timeout_minutes" json:"approval_timeout_minutes,omitempty"`
	// Target is legacy free-form context. It participates in the approval
	// digest but is never copied into approval state/history/API because older
	// configs may contain hostnames, credentials, or command fragments.
	Target string `yaml:"target" json:"target,omitempty"`
	// Rollback is legacy free-form context with the same persistence rule as
	// Target: runtime/config only, never approval state/history/API.
	Rollback string `yaml:"rollback" json:"rollback,omitempty"`
	// TargetLabel is an explicit operator assertion that this short label is
	// safe for durable approval/history/API display. It is mandatory for
	// approval_required and is never inferred from Target.
	TargetLabel string `yaml:"target_label" json:"target_label,omitempty"`
	// VerificationLabel is mandatory operator-declared-safe context describing
	// the verification outcome (the raw VerifyCommand is never persisted).
	VerificationLabel string `yaml:"verification_label" json:"verification_label,omitempty"`
	// RollbackLabel is mandatory operator-declared-safe rollback context. When
	// rollback is deliberately unavailable, use "none: <reason>" explicitly.
	// It is never inferred from Rollback.
	RollbackLabel string `yaml:"rollback_label" json:"rollback_label,omitempty"`
	// VerifyCommand is the live/deployment verifier run after Command succeeds.
	// The same strict repo-relative entrypoint contract applies in
	// approval_required. A non-zero exit marks execution_failed.
	VerifyCommand string `yaml:"verify_command" json:"verify_command,omitempty"`
	// LocalPath is runtime-only context copied from Config.LocalPath by
	// EffectiveDelivery. It is deliberately not a delivery-block YAML field,
	// but it participates in ApprovalDigest: moving the checkout changes what
	// source and scripts would execute and therefore requires a fresh approval.
	LocalPath string `yaml:"-" json:"-"`
}

// deliveryTimeoutDefaultMinutes is the fallback delivery timeout.
const deliveryTimeoutDefaultMinutes = 15

const deliveryApprovalTimeoutDefaultMinutes = 24 * 60

// EffectiveDelivery resolves the delivery configuration for a project, folding
// the legacy deploy_cmd/deploy_timeout_minutes fields into an automatic-mode
// DeliveryConfig for back-compat (#872). Resolution order:
//
//   - an explicit delivery block wins; an empty delivery.mode defaults to
//     approval_required when a command is present (safe default) and disabled
//     when it is not;
//   - otherwise a legacy deploy_cmd maps to DeliveryModeAutomatic so existing
//     fleet projects keep firing their deploy immediately after merge with no
//     silent behavior change (Config.Warnings surfaces the deprecation);
//   - otherwise delivery is disabled.
//
// The returned config always carries a normalized TimeoutMinutes.
func (c *Config) EffectiveDelivery() DeliveryConfig {
	if c == nil {
		return DeliveryConfig{Mode: DeliveryModeDisabled, TimeoutMinutes: deliveryTimeoutDefaultMinutes}
	}
	d := c.Delivery
	if d.configured() {
		if strings.TrimSpace(string(d.Mode)) == "" {
			if strings.TrimSpace(d.Command) != "" {
				d.Mode = DeliveryModeApprovalRequired
			} else {
				d.Mode = DeliveryModeDisabled
			}
		}
		d.TimeoutMinutes = normalizeDeliveryTimeout(d.TimeoutMinutes)
		d.LocalPath = c.LocalPath
		return d
	}
	if strings.TrimSpace(c.DeployCmd) != "" {
		return DeliveryConfig{
			Mode:           DeliveryModeAutomatic,
			Command:        c.DeployCmd,
			TimeoutMinutes: normalizeDeliveryTimeout(c.DeployTimeoutMinutes),
			LocalPath:      c.LocalPath,
		}
	}
	return DeliveryConfig{Mode: DeliveryModeDisabled, TimeoutMinutes: normalizeDeliveryTimeout(c.DeployTimeoutMinutes), LocalPath: c.LocalPath}
}

// configured reports whether the operator set any field of the delivery block.
func (d DeliveryConfig) configured() bool {
	return strings.TrimSpace(string(d.Mode)) != "" ||
		strings.TrimSpace(d.Command) != "" ||
		d.TimeoutMinutes != 0 ||
		d.ApprovalTimeoutMinutes != 0 ||
		strings.TrimSpace(d.Target) != "" ||
		strings.TrimSpace(d.Rollback) != "" ||
		strings.TrimSpace(d.TargetLabel) != "" ||
		strings.TrimSpace(d.VerificationLabel) != "" ||
		strings.TrimSpace(d.RollbackLabel) != "" ||
		strings.TrimSpace(d.VerifyCommand) != ""
}

// EffectiveTimeout returns the delivery command timeout as a duration.
func (d DeliveryConfig) EffectiveTimeout() time.Duration {
	return time.Duration(normalizeDeliveryTimeout(d.TimeoutMinutes)) * time.Minute
}

// EffectiveApprovalTimeout returns the maximum age of a pending delivery
// approval. It is deliberately separate from the command execution timeout.
func (d DeliveryConfig) EffectiveApprovalTimeout() time.Duration {
	minutes := d.ApprovalTimeoutMinutes
	if minutes <= 0 {
		minutes = deliveryApprovalTimeoutDefaultMinutes
	}
	return time.Duration(minutes) * time.Minute
}

// ApprovalDigest binds the exact execution-relevant delivery configuration to
// the approval without persisting the raw command as an executable payload.
// Any command, verifier, timeout, mode, target, or rollback drift requires a
// fresh approval before execution.
func (d DeliveryConfig) ApprovalDigest() string {
	parts := []string{
		string(d.Mode),
		d.Command,
		d.VerifyCommand,
		strconv.Itoa(normalizeDeliveryTimeout(d.TimeoutMinutes)),
		strconv.Itoa(int(d.EffectiveApprovalTimeout() / time.Minute)),
		d.Target,
		d.Rollback,
		d.TargetLabel,
		d.VerificationLabel,
		d.RollbackLabel,
		d.LocalPath,
	}
	var canonical strings.Builder
	for _, part := range parts {
		canonical.WriteString(strconv.Itoa(len(part)))
		canonical.WriteByte(':')
		canonical.WriteString(part)
		canonical.WriteByte(';')
	}
	sum := sha256.Sum256([]byte(canonical.String()))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// ValidMode reports whether Mode is one of the three modeled delivery modes.
// An empty mode is valid (it resolves via EffectiveDelivery).
func (d DeliveryConfig) ValidMode() bool {
	switch d.Mode {
	case "", DeliveryModeDisabled, DeliveryModeApprovalRequired, DeliveryModeAutomatic:
		return true
	default:
		return false
	}
}

func normalizeDeliveryTimeout(min int) int {
	if min <= 0 {
		return deliveryTimeoutDefaultMinutes
	}
	return min
}

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
	OperatorGate            SupervisorOperatorGateConfig    `yaml:"operator_gate" json:"operator_gate,omitempty"`
	SafeActions             []string                        `yaml:"safe_actions" json:"safe_actions,omitempty"`
	ApprovalRequired        []string                        `yaml:"approval_required" json:"approval_required,omitempty"`
	AllowedActions          []string                        `yaml:"allowed_actions" json:"allowed_actions,omitempty"`
	ApprovalRequiredActions []string                        `yaml:"approval_required_actions" json:"approval_required_actions,omitempty"`
	PolicyPath              string                          `yaml:"-" json:"policy_path,omitempty"`
	LessonProposalsEnabled  *bool                           `yaml:"lesson_proposals_enabled" json:"lesson_proposals_enabled,omitempty"`
	// AttemptTimeoutSeconds bounds ONE supervisor LLM backend attempt. Default
	// 45s — calibrated for fast print-mode backends (codex/sol). A slow carrier
	// (claude CLI at high effort through a proxy) needs more; give it a
	// per-backend override (model.backends.<name>.supervisor_attempt_timeout_seconds)
	// instead of raising this global. Burn RCA 2026-07-24: a chain of three
	// claude-binary candidates under the 45s default produced ~365
	// paid-and-discarded generations in 4.5h — every attempt billed
	// server-side, then killed client-side before the answer arrived.
	AttemptTimeoutSeconds int `yaml:"attempt_timeout_seconds" json:"attempt_timeout_seconds,omitempty"`
	// TotalTimeoutSeconds bounds the whole candidate walk for one consult so a
	// slow chain can never overrun the supervise interval. Default 180s.
	TotalTimeoutSeconds int `yaml:"total_timeout_seconds" json:"total_timeout_seconds,omitempty"`
	// AlwaysConsultLLM restores the pre-#837 behavior of calling the supervisor
	// LLM on every enabled cycle, even when the deterministic guardrail already
	// decided a safe, mutation-free action (action=none / wait_* / monitor_open_pr).
	// Default false: those cycles short-circuit and skip the backend call, since
	// the LLM can only agree with a risk=safe guardrail decision and therefore
	// cannot change it (see internal/supervisor/llm.go decideWithLLM). Set true to
	// force a full-context second opinion on every cycle regardless of token cost.
	AlwaysConsultLLM bool `yaml:"always_consult_llm" json:"always_consult_llm,omitempty"`

	// TempDir repoints TMPDIR/TMP/TEMP for the supervisor's backend children
	// (#1127). These probes are started directly by the daemon and never enter a
	// worker lease, so worker_runtime isolation cannot reach them; without an
	// explicit environment they inherit the daemon's, which on the fleet host
	// means the RAM-backed /tmp. Empty selects DefaultSupervisorTempDir().
	TempDir string `yaml:"temp_dir" json:"temp_dir,omitempty"`

	// AllowMeteredBackend opts this project's supervisor LLM loop into running on
	// a metered (per-token) backend (#838). Default false: when supervisor.backend
	// (or the model.default fallback the supervisor uses) resolves to a backend
	// with pricing_class: metered, the supervisor refuses the LLM path and runs
	// deterministic-only, emitting a red stuck state — so a config-store edit that
	// re-points the backend at a per-token model cannot silently burn cost on
	// always-on "do nothing" cycles (incident 2026-07-09). Set true to accept the
	// per-token cost explicitly.
	AllowMeteredBackend bool `yaml:"allow_metered_backend" json:"allow_metered_backend,omitempty"`

	// SpecGroom configures the issue-grooming agent + spec-lint quality gate
	// (#851). Off by default: the supervisor only lints ready-candidate issues
	// and answers `@maestro groom` mentions when SpecGroom.Enabled is set. All
	// side effects (a single lint checklist comment, a groom proposal comment)
	// go through the existing safe/cautious surfaces, and applying a rewrite is
	// the approval-gated edit_issue_body verb.
	SpecGroom SupervisorSpecGroomConfig `yaml:"spec_groom" json:"spec_groom,omitempty"`

	// NonFunctionalPaths lists extra doublestar globs whose EXCLUSIVE change in a
	// merged PR must not settle a bug issue (#1020): a docs-only QA record is a
	// record delivery, not a fix, so the session is released for fresh dispatch
	// instead of silencing the issue. docs/** is always included; this field only
	// extends the set (for example a project-specific records/ or qa/ tree).
	NonFunctionalPaths []string `yaml:"non_functional_paths" json:"non_functional_paths,omitempty"`

	// Recommendation lifecycle policy. Zero selects the built-in defaults;
	// negative values are rejected during normalization.
	UnchangedDecisionWindowSeconds int `yaml:"unchanged_decision_window_seconds" json:"unchanged_decision_window_seconds,omitempty"`
	RecommendationTTLSeconds       int `yaml:"recommendation_ttl_seconds" json:"recommendation_ttl_seconds,omitempty"`

	excludedLabelsSet bool
}

const (
	defaultUnchangedDecisionWindow = time.Hour
	defaultRecommendationTTL       = 24 * time.Hour
)

// EffectiveUnchangedDecisionWindow returns the durable journal suppression
// window for identical recommendations. Zero keeps the default at one hour.
func (c SupervisorConfig) EffectiveUnchangedDecisionWindow() time.Duration {
	if c.UnchangedDecisionWindowSeconds <= 0 {
		return defaultUnchangedDecisionWindow
	}
	return time.Duration(c.UnchangedDecisionWindowSeconds) * time.Second
}

// EffectiveRecommendationTTL returns the maximum age of an unconsumed
// recommendation before it is durably dropped with a disposition reason.
func (c SupervisorConfig) EffectiveRecommendationTTL() time.Duration {
	if c.RecommendationTTLSeconds <= 0 {
		return defaultRecommendationTTL
	}
	return time.Duration(c.RecommendationTTLSeconds) * time.Second
}

// defaultNonFunctionalPaths mirrors pipeline.DefaultNonFunctionalPaths. It is
// duplicated here to keep the config package free of a dependency on pipeline
// (pipeline imports config, so the reverse edge would be an import cycle).
var defaultNonFunctionalPaths = []string{"docs/**"}

// EffectiveNonFunctionalPaths returns the non-functional path globs used to
// classify a merged PR as a documentation/record delivery (#1020): the docs/**
// default unioned with any project-configured extensions, de-duplicated with
// input order preserved (defaults first).
func (c SupervisorConfig) EffectiveNonFunctionalPaths() []string {
	seen := make(map[string]struct{}, len(defaultNonFunctionalPaths)+len(c.NonFunctionalPaths))
	out := make([]string, 0, len(defaultNonFunctionalPaths)+len(c.NonFunctionalPaths))
	for _, group := range [][]string{defaultNonFunctionalPaths, c.NonFunctionalPaths} {
		for _, p := range group {
			p = strings.TrimSpace(p)
			if p == "" {
				continue
			}
			if _, ok := seen[p]; ok {
				continue
			}
			seen[p] = struct{}{}
			out = append(out, p)
		}
	}
	return out
}

// SupervisorSpecGroomConfig gates the spec-lint + grooming capability (#851).
// Both knobs default to false (plain bools), so an untouched config never lints,
// grooms, or withholds a ready label — enabling is a config-store row change.
type SupervisorSpecGroomConfig struct {
	// Enabled turns the whole capability on for this project. While false the
	// supervisor performs no lint pass, posts no comments, and mints no
	// edit_issue_body approvals.
	Enabled bool `yaml:"enabled" json:"enabled,omitempty"`

	// RequireLintPass, when true, withholds the ready label from an issue whose
	// current body has not passed spec-lint (default warn-only: a failing issue
	// still gets its lint comment but keeps its normal labeling flow). Has no
	// effect unless Enabled is also true.
	RequireLintPass bool `yaml:"require_lint_pass" json:"require_lint_pass,omitempty"`
}

// SupervisorOperatorGateConfig describes deliberate human/operator holds that
// must be treated as waiting states, not retryable implementation failures.
// Check names are machine-readable check-run names or commit-status contexts;
// shell-style globs are allowed for repository-specific naming variants.
type SupervisorOperatorGateConfig struct {
	CheckNames     []string `yaml:"check_names" json:"check_names,omitempty"`
	Labels         []string `yaml:"labels" json:"labels,omitempty"`
	RequiredAction string   `yaml:"required_action" json:"required_action,omitempty"`
}

// SpecGroomOn reports whether the spec-lint + grooming capability is enabled
// for this project. Default false (#851).
func (c SupervisorConfig) SpecGroomOn() bool {
	return c.SpecGroom.Enabled
}

// SpecGroomRequireLintPass reports whether the ready label must be withheld from
// issues that have not passed spec-lint. Only meaningful when SpecGroomOn().
func (c SupervisorConfig) SpecGroomRequireLintPass() bool {
	return c.SpecGroom.Enabled && c.SpecGroom.RequireLintPass
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
	Mode             string            `yaml:"mode"`               // "manual" (labels only, default), "auto" (LLM router), "policy" (task-aware tiers, #783)
	RouterModel      string            `yaml:"router_model"`       // backend name from model.backends (default: "claude")
	RouterModelName  string            `yaml:"router_model_name"`  // specific model to use (default: "claude-sonnet-4-6")
	RouterPrompt     string            `yaml:"router_prompt"`      // prompt template with {{BACKENDS}}, {{NUMBER}}, {{TITLE}}, {{BODY}}
	TaskTypeBackends map[string]string `yaml:"task_type_backends"` // task_type -> backend override used only when routing.mode=auto

	// AllowMeteredBackend opts the auto-router into running its per-issue LLM
	// classification on a metered (per-token) backend (#838). Default false: when
	// routing.router_model resolves to a pricing_class: metered backend the router
	// refuses the LLM call and falls back to model.default — the same guard the
	// supervisor applies to its own always-on loop.
	AllowMeteredBackend bool `yaml:"allow_metered_backend,omitempty"`

	// Task-aware model routing (#783, RFC docs/model-routing-rfc.md §2.5). Tiers
	// are named strength bands (backend + optional effort/model override) and
	// Policy is the deterministic signal→tier rule list evaluated only when
	// Mode == "policy". When Mode != "policy" both are inert and selection is
	// byte-for-byte today's behavior — the label override and model.default
	// paths are untouched (RFC §2.9).
	Tiers  map[string]RoutingTier `yaml:"tiers,omitempty"`
	Policy *RoutingPolicy         `yaml:"policy,omitempty"`
}

// RoutingTier is one named strength tier (RFC §2.4). It points at a backend
// declared in model.backends and may override that backend's reasoning effort
// and/or model id per-tier; the overrides are threaded into the worker argv
// (codex/claude/gemini) so one backend can serve multiple tiers. Rank orders
// tiers for the escalation climb — lower is cheaper/weaker — and ties break by
// tier name; set explicit ranks (e.g. cheap=0, standard=1, strong=2) when
// escalation is enabled.
type RoutingTier struct {
	Backend string `yaml:"backend"`
	Effort  string `yaml:"effort,omitempty"`
	Model   string `yaml:"model,omitempty"`
	Rank    int    `yaml:"rank,omitempty"`
}

// RoutingPolicy is the deterministic first-match signal→tier policy evaluated
// when routing.mode == "policy" (RFC §2.5). The label override (model:<name>)
// is evaluated before the policy and always wins; an unmatched issue falls to
// DefaultTier.
type RoutingPolicy struct {
	DefaultTier string              `yaml:"default_tier"`
	Rules       []RoutingPolicyRule `yaml:"rules,omitempty"`
	Escalation  RoutingEscalation   `yaml:"escalation,omitempty"`
	Budget      RoutingBudget       `yaml:"budget,omitempty"`
	// Shadow logs the tier the policy would pick without changing the
	// dispatched backend, so an operator can validate the rules against a real
	// wave before enabling (RFC §2.8 shadow-mode rollout).
	Shadow bool `yaml:"shadow,omitempty"`
}

// RoutingPolicyRule maps a signal predicate to a tier. Rules are evaluated in
// order and the first whose When predicate matches wins (RFC §2.4 step 2).
type RoutingPolicyRule struct {
	When RoutingSignalMatch `yaml:"when"`
	Tier string             `yaml:"tier"`
}

// RoutingSignalMatch is a rule predicate over issue signals. A rule matches
// when every field it sets matches the issue (logical AND); unset fields are
// ignored. The signals are all deterministically derivable from issue data the
// router already holds (RFC §2.2), so no extra LLM call is needed.
type RoutingSignalMatch struct {
	Labels       []string `yaml:"labels,omitempty"`        // issue-label globs (filepath.Match), e.g. "model:*", "migration"
	RiskKeywords []string `yaml:"risk_keywords,omitempty"` // case-insensitive substrings matched against title+body
	Size         string   `yaml:"size,omitempty"`          // "small" | "large" (from a size:<v> label)
	Dependency   string   `yaml:"dependency,omitempty"`    // "leaf" | "foundation" (from a dependency:<v> label)
}

// RoutingEscalation is the cheap-first, escalate-on-failure ladder (RFC §2.6):
// on an enabled trigger the next attempt climbs one tier (effort-first within a
// backend, then backend), capped at MaxTier and the per-issue retry budget.
type RoutingEscalation struct {
	Enabled bool     `yaml:"enabled,omitempty"`
	On      []string `yaml:"on,omitempty"`       // any of: ci_failure, review_rejection, retry
	MaxTier string   `yaml:"max_tier,omitempty"` // climb never exceeds this tier (default: highest-rank tier)
}

// RoutingBudget caps how many issues a wave routes to the strong band so a
// burst of large tasks cannot blow the cost envelope (RFC §2.5 budget).
type RoutingBudget struct {
	MaxStrongPerWave int `yaml:"max_strong_per_wave,omitempty"` // 0 = unlimited
}

// Escalation trigger tokens accepted in routing.policy.escalation.on (RFC §2.6).
const (
	EscalationOnCIFailure       = "ci_failure"
	EscalationOnReviewRejection = "review_rejection"
	EscalationOnRetry           = "retry"
)

// PolicyPassthroughTier is the reserved rule tier that documents "this signal
// bypasses the policy" (e.g. the model:* label rule in the RFC example). A rule
// resolving to it is treated as no policy match, so resolution falls through to
// DefaultTier / model.default exactly as if the rule were absent.
const PolicyPassthroughTier = "passthrough"

// PolicyMode is the routing.mode value that turns on task-aware tier routing.
const PolicyMode = "policy"

// IsPolicyMode reports whether task-aware tier routing is enabled.
func (r RoutingConfig) IsPolicyMode() bool {
	return strings.EqualFold(strings.TrimSpace(r.Mode), PolicyMode)
}

// OrderedTierNames returns tier names sorted by ascending rank, ties broken by
// name. This is the deterministic order the escalation ladder climbs.
func (r RoutingConfig) OrderedTierNames() []string {
	names := make([]string, 0, len(r.Tiers))
	for name := range r.Tiers {
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool {
		ri, rj := r.Tiers[names[i]].Rank, r.Tiers[names[j]].Rank
		if ri != rj {
			return ri < rj
		}
		return names[i] < names[j]
	})
	return names
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

// RoleConfig defines settings for a single pipeline role (planner, advisor,
// implementer, validator).
type RoleConfig struct {
	Enabled           bool   `yaml:"enabled"`
	Backend           string `yaml:"backend"`             // backend name from model.backends (empty = use default)
	Prompt            string `yaml:"prompt"`              // path to prompt template (empty = built-in default)
	MaxRuntimeMinutes int    `yaml:"max_runtime_minutes"` // override per-role max runtime (0 = use global)
	// Effort is the per-phase reasoning-effort override (#841). When set it is
	// threaded into the worker argv for the phase via
	// worker.appendTierModelEffort — claude `--effort <e>`, codex `-c
	// model_reasoning_effort=<e>`; gemini has no effort flag and drops it. Empty
	// leaves the backend's effort unchanged (today's behavior). This enables the
	// "plan-big / execute-small" economics: strong effort on plan/validate, a low
	// effort on the token-heavy implement phase.
	Effort string `yaml:"effort"`
}

const (
	DefaultAdvisorReviewRounds = 2
	MaxAdvisorReviewRounds     = 5
)

// PipelineConfig controls the planner → optional advisor → implementer →
// validator phase pipeline and deterministic pre-worker context preparation
// phases.
type PipelineConfig struct {
	// Phase-based pipeline (planner → optional advisor → implementer → validator)
	Enabled bool       `yaml:"enabled"` // enable the phase pipeline globally (default: false; issue labels can opt in per worker)
	Planner RoleConfig `yaml:"planner"` // planner role settings
	Advisor RoleConfig `yaml:"advisor"` // independent, review-only plan advisor (default: disabled)
	// AdvisorReviewRounds bounds Advisor passes across planner revisions. Zero
	// means the default of two; parse rejects values above the hard cap of five.
	AdvisorReviewRounds int        `yaml:"advisor_review_rounds"`
	AdvisorBestEffort   bool       `yaml:"advisor_best_effort"` // explicit auditable bypass instead of fail-closed (default: false)
	Validator           RoleConfig `yaml:"validator"`           // validator role settings
	// Implementer carries the implement phase's own backend/effort override (#841)
	// so the token-heavy implement phase can run on a cheap backend + low effort
	// while plan/validate keep the strong model. Empty backend falls back to
	// model.default (unchanged default); the prompt still comes from the existing
	// worker_prompt / bug_prompt / enhancement_prompt settings.
	Implementer RoleConfig `yaml:"implementer"`

	// Deterministic pre-worker context preparation phases. These are heuristic
	// local scans/checks, not separate agent sessions.
	Research       bool  `yaml:"research"`        // scan repo context before worker starts (default: false)
	PlanValidation *bool `yaml:"plan_validation"` // heuristic plan coverage check before coding starts (default: true)
	TestMapping    *bool `yaml:"test_mapping"`    // map requirements to verify commands (default: true)
}

// EffectiveAdvisorReviewRounds returns the bounded number of Advisor passes.
// Directly-constructed configs are clamped defensively even though Parse rejects
// values above the hard cap.
func (p PipelineConfig) EffectiveAdvisorReviewRounds() int {
	rounds := p.AdvisorReviewRounds
	if rounds <= 0 {
		rounds = DefaultAdvisorReviewRounds
	}
	if rounds > MaxAdvisorReviewRounds {
		return MaxAdvisorReviewRounds
	}
	return rounds
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

// VerifyConfig holds opt-in post-implementation verification steps.
type VerifyConfig struct {
	Visual VerifyVisualConfig `yaml:"visual"` // #705: visual evidence for UI-affecting PRs
}

// VerifyVisualConfig configures the opt-in visual-evidence step for
// UI-affecting PRs (#705). When enabled, workers are instructed to run the
// project's screenshot harness and attach the resulting images to the PR
// before declaring done; the orchestrator checks UI-affecting PRs (changed
// files matching Paths) for attached evidence and posts a warning comment
// when it is missing. Advisory in v1 — never blocks merge.
type VerifyVisualConfig struct {
	Enabled        bool     `yaml:"enabled"`         // opt in per project (default: false)
	Command        string   `yaml:"command"`         // project script that launches the app and writes screenshots to OutputDir; run from the worktree root
	Paths          []string `yaml:"paths"`           // globs that classify a PR as UI-affecting (e.g. "**/*.jsx", "web/**"); `**` crosses directories
	OutputDir      string   `yaml:"output_dir"`      // worktree-relative screenshot directory (default: .maestro/screenshots)
	TimeoutMinutes int      `yaml:"timeout_minutes"` // capture command budget in minutes (default: 10)
}

// Active reports whether the visual-evidence step is fully configured:
// enabled with a capture command and at least one UI path glob. The
// misconfigured shapes (enabled without command/paths) surface via
// Config.Warnings instead of half-running.
func (v VerifyVisualConfig) Active() bool {
	return v.Enabled && strings.TrimSpace(v.Command) != "" && len(v.Paths) > 0
}

// ResolvedOutputDir returns the worktree-relative directory the capture
// command writes screenshots to. Default: .maestro/screenshots.
func (v VerifyVisualConfig) ResolvedOutputDir() string {
	dir := strings.TrimSpace(v.OutputDir)
	if dir == "" {
		return filepath.Join(".maestro", "screenshots")
	}
	return dir
}

// Timeout returns the capture command budget. Default: 10 minutes.
func (v VerifyVisualConfig) Timeout() time.Duration {
	if v.TimeoutMinutes <= 0 {
		return 10 * time.Minute
	}
	return time.Duration(v.TimeoutMinutes) * time.Minute
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
	// MaxAttempts caps how many re-trigger comments one session may post for
	// a single head. Without a cap a permanently silent reviewer collects a
	// "@greptile review" comment every cooldown window forever (live 2026-07:
	// Greptile paused after exhausting its free credits and every wedged PR
	// kept accruing comments). Default 3; <= 0 means the default, and a
	// deliberate "never stop" is spelled as a large number.
	MaxAttempts int `yaml:"max_attempts,omitempty"`
	// MissingAfterMinutes makes an UNOBSERVED review gate non-blocking once the
	// gate has been silent this long on one head — no check run, no comment,
	// nothing. 0 (default) preserves today's behavior: a silent gate holds the
	// PR forever. A reviewer that DOES answer still hard-blocks regardless of
	// this value, so enabling it never weakens a working gate; it only bounds
	// the wait on a reviewer that never shows up.
	MissingAfterMinutes int `yaml:"missing_after_minutes,omitempty"`
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

// EffectiveMaxAttempts caps re-trigger comments per head; 0 means unlimited,
// which is the default.
//
// The cap is deliberately opt-in. Capping by default would REMOVE the only
// automatic recovery an untouched install has: before this knob existed, a
// wedged gate kept being nudged every cooldown window, so a review service that
// came back hours later was woken by the next nudge and the PR merged
// hands-off. A default cap silences those nudges after N tries while the
// escape hatch that would release the PR (missing_after_minutes) is itself
// off by default — turning "eventually self-heals" into "wedged forever" for an
// operator who changed nothing. An operator who sets a cap is accepting that
// trade, and should set missing_after_minutes with it.
func (c ReviewRetriggerConfig) EffectiveMaxAttempts() int {
	if c.MaxAttempts <= 0 {
		return 0
	}
	return c.MaxAttempts
}

// MissingReviewGraceOrZero returns how long a fully silent review gate may hold
// a PR before it is treated as non-blocking, or 0 when the operator has not
// opted in (today's block-forever behavior).
func (c ReviewRetriggerConfig) MissingReviewGraceOrZero() time.Duration {
	if c.MissingAfterMinutes <= 0 {
		return 0
	}
	return time.Duration(c.MissingAfterMinutes) * time.Minute
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

// StalledProgressWatchdogConfig configures the durable per-project
// stalled-progress watchdog (#887).
//
// It supersedes the legacy `worker_silent_timeout_minutes`, which watched
// terminal output only — so it could kill a worker that was actively editing
// files without emitting output, and when disabled (0/absent) it could not
// recover a genuinely hung-but-alive worker. The watchdog instead derives one
// material-progress watermark from whichever phase-appropriate signals are
// present (issue/session + lease, process/tmux identity, terminal/checkpoint,
// bounded git evidence, PR/CI/review, delivery lease) and only recommends an
// action when *no* signal has advanced for the whole silence budget.
//
// The default is a 20-minute maximum silence for new hands-off projects. A
// stall in a safe pre-delivery phase asks the orchestrator to stop the single
// stale worker and retry once under the existing retry budget; a stall on an
// executing/uncertain delivery lease is surfaced for operator reconciliation
// and never replayed (#872).
type StalledProgressWatchdogConfig struct {
	Enabled           *bool `yaml:"enabled,omitempty"`               // explicit opt-in; external lifecycle tooling may set true only after accepted runtime evidence
	MaxSilenceMinutes int   `yaml:"max_silence_minutes,omitempty"`   // default: 20 (0 = default; negative = disabled)
	EvalIntervalSecs  int   `yaml:"eval_interval_seconds,omitempty"` // watchdog evaluation cadence; default: 60
}

// IsEnabled reports whether the stalled-progress watchdog is explicitly
// enabled. Missing config is inactive: upgrading Maestro must not silently arm
// recovery across every legacy project. Maestro's own genesis does not
// auto-enable it; evidence-gated external lifecycle tooling may opt in.
func (c StalledProgressWatchdogConfig) IsEnabled() bool {
	if c.Enabled == nil {
		return false
	}
	return *c.Enabled
}

// EffectiveMaxSilence returns the maximum silence budget before the watchdog
// recommends an action. A zero MaxSilenceMinutes means "use the 20-minute default"; a negative
// value (or a disabled watchdog) returns 0, which the evaluator treats as
// "disabled" so no quiet worker is ever killed by a misconfiguration.
func (c StalledProgressWatchdogConfig) EffectiveMaxSilence() time.Duration {
	if !c.IsEnabled() || c.MaxSilenceMinutes < 0 {
		return 0
	}
	if c.MaxSilenceMinutes == 0 {
		return progress.DefaultMaxSilence
	}
	return time.Duration(c.MaxSilenceMinutes) * time.Minute
}

// EffectiveEvalInterval returns the watchdog's own evaluation cadence, which is
// distinct from the orchestrator poll interval and the supervisor loop cadence
// (#887 requirement that Fleet report them separately). Default 60s.
func (c StalledProgressWatchdogConfig) EffectiveEvalInterval() time.Duration {
	if c.EvalIntervalSecs <= 0 {
		return 60 * time.Second
	}
	return time.Duration(c.EvalIntervalSecs) * time.Second
}

// IsActive reports whether the v1 stalled-progress watchdog evaluator is armed.
// It is deliberately stronger than IsEnabled:
// an explicit negative silence budget is an operator disable, even when the
// enabled flag was omitted. Fleet and runtime scheduling use this method so
// they never advertise or run a watchdog whose effective budget is zero.
func (c StalledProgressWatchdogConfig) IsActive() bool {
	return c.EffectiveMaxSilence() > 0
}

// EffectiveWorkerSilentTimeout returns the deprecated terminal-output-only
// timeout only when the v1 stalled-progress watchdog is inactive. The two
// detectors must never run in parallel: doing so lets the legacy detector kill
// a worker that the multi-signal watchdog correctly sees editing its worktree.
//
// Parsed legacy-only configs are migrated to the v1 silence budget below, but
// this runtime guard also protects programmatically-constructed configs and an
// older config object during a hot reload.
func (c *Config) EffectiveWorkerSilentTimeout() time.Duration {
	if c == nil || c.StalledProgressWatchdog.suppressesLegacyTimeout() || c.WorkerSilentTimeoutMinutes <= 0 {
		return 0
	}
	return time.Duration(c.WorkerSilentTimeoutMinutes) * time.Minute
}

// suppressesLegacyTimeout is intentionally based on explicit v1 configuration,
// not IsActive. An enabled:true stanza with an invalid/negative budget must fail
// closed; otherwise the new watchdog is disabled while the deprecated terminal-
// only killer unexpectedly remains armed. enabled:false is the sole deliberate
// compatibility escape hatch.
func (c StalledProgressWatchdogConfig) suppressesLegacyTimeout() bool {
	if c.Enabled != nil {
		return *c.Enabled
	}
	return c.MaxSilenceMinutes != 0 || c.EvalIntervalSecs != 0
}

// ManagementHomeKind enumerates the supported management-home backends (#869).
// Only "obsidian" is recognised in this slice; parse() rejects any other value
// so a typo cannot masquerade as a working control-room link.
const ManagementHomeKindObsidian = "obsidian"

// ManagementHomeBoundary is the fixed PM-vs-executable boundary statement (#870)
// injected verbatim into worker prompts and the supervisor project packet when a
// project configures a Management Home. It exists in one place so the worker
// prompt and the supervisor packet always agree on the same rule: the home is
// private planning context, the issue and approved in-repo docs are the only
// executable contract, workers do not read/edit the home unless the issue
// explicitly assigns doc/admin work there, and the absolute path is never copied
// into any GitHub-facing or generated repo output.
const ManagementHomeBoundary = "The Management Home is private product-management and planning context, not an executable requirement source. The assigned GitHub issue and the approved in-repo documentation are the only executable contract; do not treat the Management Home as a task list, and do not read or summarize it. Do not create or edit anything in the Management Home unless the assigned issue explicitly assigns documentation or admin work there. Never copy the absolute Management Home path into issue comments, PR bodies, commit messages, or any generated repository file."

// ManagementHomeConfig is the optional, descriptive link from a Maestro project
// row back to its PM / control-room home (#869). It carries identity metadata
// only — Maestro never reads, traverses, or writes the referenced home in this
// slice; the block round-trips through config-store and is surfaced on the Fleet
// API for external bootstrap adapters to correlate Area/repo/Maestro row.
//
//   - Kind:      management-home backend ("obsidian" is the only supported value).
//   - Path:      execution-host absolute path of the home (validated non-empty).
//   - Vault:     Obsidian vault display name (required for kind obsidian).
//   - VaultPath: vault-RELATIVE path to the project's Area, e.g. Dev/Areas/<slug>.
//     Must be relative (not absolute) and contain no ".." traversal (validated).
type ManagementHomeConfig struct {
	Kind      string `yaml:"kind,omitempty" json:"kind,omitempty"`
	Path      string `yaml:"path,omitempty" json:"path,omitempty"`
	Vault     string `yaml:"vault,omitempty" json:"vault,omitempty"`
	VaultPath string `yaml:"vault_path,omitempty" json:"vault_path,omitempty"`
}

// Configured reports whether any management-home field is set, i.e. whether the
// block is present at all. A config with no management_home leaves every field
// empty and is skipped by validation, so legacy configs parse unchanged.
func (m ManagementHomeConfig) Configured() bool {
	return strings.TrimSpace(m.Kind) != "" ||
		strings.TrimSpace(m.Path) != "" ||
		strings.TrimSpace(m.Vault) != "" ||
		strings.TrimSpace(m.VaultPath) != ""
}

// projectIDPattern matches a canonical 8-4-4-4-12 UUID (any case). A stable
// project_id must be a real UUID so an external bootstrap adapter can prove that
// Area, repo, and Maestro row refer to the same durable project (#869).
var projectIDPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// validateProjectID rejects a present-but-malformed project_id. An empty id is
// allowed (the field is optional and legacy rows have none).
func validateProjectID(id string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil
	}
	if !projectIDPattern.MatchString(id) {
		return fmt.Errorf("config: project_id %q is not a valid UUID (want canonical 8-4-4-4-12, e.g. 3f2504e0-4f89-41d3-9a0c-0305e82c3301)", id)
	}
	return nil
}

// validateManagementHome enforces the management_home contract (#869): a
// supported kind, a non-empty path, the obsidian-required vault/vault_path, and
// a vault-relative vault_path free of absolute prefixes and ".." traversal. An
// unconfigured block validates trivially so legacy configs are unaffected.
func validateManagementHome(m ManagementHomeConfig) error {
	if !m.Configured() {
		return nil
	}
	switch strings.ToLower(strings.TrimSpace(m.Kind)) {
	case ManagementHomeKindObsidian:
		// supported
	case "":
		return fmt.Errorf("config: management_home.kind is required (supported: %s)", ManagementHomeKindObsidian)
	default:
		return fmt.Errorf("config: management_home.kind = %q is not supported (supported: %s)", m.Kind, ManagementHomeKindObsidian)
	}
	if strings.TrimSpace(m.Path) == "" {
		return fmt.Errorf("config: management_home.path is required and must be non-empty")
	}
	// Obsidian consistency: both the vault name and the vault-relative Area path
	// are required — a home missing either cannot be resolved back to a project.
	if strings.TrimSpace(m.Vault) == "" {
		return fmt.Errorf("config: management_home.vault is required for kind %s", ManagementHomeKindObsidian)
	}
	if strings.TrimSpace(m.VaultPath) == "" {
		return fmt.Errorf("config: management_home.vault_path is required for kind %s", ManagementHomeKindObsidian)
	}
	return validateVaultRelPath(strings.TrimSpace(m.VaultPath))
}

// validateVaultRelPath rejects a vault_path that is absolute or contains a ".."
// traversal segment. Vault paths are logical, forward-slash, vault-relative
// references (e.g. Dev/Areas/maestro), so the check is on '/'-split segments and
// a leading-slash test rather than any host filesystem semantics — Maestro never
// resolves the path against a real directory.
func validateVaultRelPath(p string) error {
	if strings.Contains(p, `\`) {
		return fmt.Errorf("config: management_home.vault_path %q must use normalized forward-slash form (backslashes are not allowed)", p)
	}
	if strings.HasPrefix(p, "/") || filepath.IsAbs(p) || pathpkg.IsAbs(p) || windowsDriveAbsPath(p) {
		return fmt.Errorf("config: management_home.vault_path %q must be vault-relative, not absolute", p)
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." {
			return fmt.Errorf("config: management_home.vault_path %q must not contain a '..' path traversal segment", p)
		}
	}
	if clean := pathpkg.Clean(p); clean == "." || clean != p {
		return fmt.Errorf("config: management_home.vault_path %q must be normalized (no empty, '.' or trailing path segments)", p)
	}
	return nil
}

func windowsDriveAbsPath(p string) bool {
	return len(p) >= 3 && ((p[0] >= 'a' && p[0] <= 'z') || (p[0] >= 'A' && p[0] <= 'Z')) && p[1] == ':' && p[2] == '/'
}

// RemoteRunnerConfig is the deliberately small, project-scoped SSH adapter
// used by the remote-worker spike (#1058). The control plane still owns issue
// selection, durable state, the local tmux/process lease, and a lightweight
// shadow worktree. Only the agent CLI and its canonical working tree move to
// the configured host.
//
// Enabled is an explicit opt-in and defaults false. CredentialsFile names a
// private file that exists on the runner; Maestro never reads it on the
// control-plane host or copies credential values over SSH.
type RemoteRunnerConfig struct {
	Enabled         bool     `yaml:"enabled"`
	Target          string   `yaml:"target"`           // ssh target, e.g. runner@example.internal
	RepoPath        string   `yaml:"repo_path"`        // existing clone on the runner
	WorktreeBase    string   `yaml:"worktree_base"`    // runner-side parent for per-slot worktrees
	BaseBranch      string   `yaml:"base_branch"`      // default: main
	SSHCommand      string   `yaml:"ssh_command"`      // default: ssh
	SSHArgs         []string `yaml:"ssh_args"`         // argv entries before target; no shell parsing
	MaestroCommand  string   `yaml:"maestro_command"`  // runner-side binary; default: maestro
	CredentialsFile string   `yaml:"credentials_file"` // optional runner-side private credential file
}

func validateRemoteRunner(cfg *Config) error {
	if cfg == nil || !cfg.RemoteRunner.Enabled {
		return nil
	}
	r := &cfg.RemoteRunner
	if strings.TrimSpace(r.SSHCommand) == "" {
		r.SSHCommand = "ssh"
	}
	r.SSHCommand = strings.TrimSpace(r.SSHCommand)
	if strings.TrimSpace(r.MaestroCommand) == "" {
		r.MaestroCommand = "maestro"
	}
	r.MaestroCommand = strings.TrimSpace(r.MaestroCommand)
	if strings.TrimSpace(r.BaseBranch) == "" {
		r.BaseBranch = "main"
	}
	r.BaseBranch = strings.TrimSpace(r.BaseBranch)
	if err := validateRemoteCommandToken("ssh_command", r.SSHCommand); err != nil {
		return err
	}
	if err := validateRemoteCommandToken("maestro_command", r.MaestroCommand); err != nil {
		return err
	}
	target := strings.TrimSpace(r.Target)
	if target == "" || strings.HasPrefix(target, "-") || containsControlOrSpace(target) {
		return fmt.Errorf("config: remote_runner.target must be a non-empty ssh target without whitespace or a leading '-'")
	}
	r.Target = target
	r.RepoPath = strings.TrimSpace(r.RepoPath)
	r.WorktreeBase = strings.TrimSpace(r.WorktreeBase)
	r.CredentialsFile = strings.TrimSpace(r.CredentialsFile)
	if err := validateRemoteAbsolutePath("repo_path", r.RepoPath, false); err != nil {
		return err
	}
	if err := validateRemoteAbsolutePath("worktree_base", r.WorktreeBase, false); err != nil {
		return err
	}
	if r.CredentialsFile != "" {
		if err := validateRemoteAbsolutePath("credentials_file", r.CredentialsFile, true); err != nil {
			return err
		}
	}
	if err := validateRemoteGitRef("base_branch", r.BaseBranch); err != nil {
		return err
	}
	if err := validateRemoteSSHArgs(r.SSHArgs); err != nil {
		return err
	}
	if cfg.AutoRebase {
		return fmt.Errorf("config: remote_runner requires auto_rebase: false in the v1 spike; rebase automation still operates on the control-plane shadow worktree")
	}
	if cfg.ValidationContract {
		return fmt.Errorf("config: remote_runner does not support validation_contract in the v1 spike")
	}
	if cfg.Pipeline.Enabled {
		return fmt.Errorf("config: remote_runner does not support pipeline.enabled in the v1 spike")
	}
	if cfg.Hooks.AfterCreate != "" || cfg.Hooks.BeforeRun != "" || cfg.Hooks.AfterRun != "" || cfg.Hooks.BeforeRemove != "" ||
		cfg.Hooks.PreTool.Command != "" || cfg.Hooks.PostEdit.Command != "" {
		return fmt.Errorf("config: remote_runner does not support lifecycle or tool hooks in the v1 spike")
	}
	return nil
}

func validateRemoteCommandToken(name, value string) error {
	value = strings.TrimSpace(value)
	if value == "" || strings.HasPrefix(value, "-") || containsControlOrSpace(value) {
		return fmt.Errorf("config: remote_runner.%s must be one executable path or name without whitespace", name)
	}
	return nil
}

func validateRemoteSSHArgs(args []string) error {
	needsValue := map[string]bool{
		"-B": true, "-b": true, "-c": true, "-E": true, "-e": true,
		"-F": true, "-I": true, "-i": true, "-J": true, "-L": true,
		"-l": true, "-m": true, "-O": true, "-o": true, "-p": true,
		"-Q": true, "-R": true, "-S": true, "-W": true, "-w": true,
	}
	expectValue := false
	for i, arg := range args {
		if strings.ContainsAny(arg, "\x00\r\n") {
			return fmt.Errorf("config: remote_runner.ssh_args[%d] must not contain NUL or newlines", i)
		}
		if expectValue {
			if arg == "" {
				return fmt.Errorf("config: remote_runner.ssh_args[%d] must not be empty", i)
			}
			expectValue = false
			continue
		}
		if arg == "" || arg == "--" || !strings.HasPrefix(arg, "-") {
			return fmt.Errorf("config: remote_runner.ssh_args[%d] must be an ssh option, not a target or remote command", i)
		}
		expectValue = needsValue[arg]
	}
	if expectValue {
		return fmt.Errorf("config: remote_runner.ssh_args ends with an option that requires a value")
	}
	return nil
}

func validateRemoteAbsolutePath(name, value string, allowFile bool) error {
	value = strings.TrimSpace(value)
	if value == "" || strings.ContainsAny(value, "\x00\r\n") || !pathpkg.IsAbs(value) {
		return fmt.Errorf("config: remote_runner.%s must be an absolute runner-side POSIX path", name)
	}
	clean := pathpkg.Clean(value)
	if clean != value || clean == "/" {
		return fmt.Errorf("config: remote_runner.%s must be normalized and must not be '/'", name)
	}
	if !allowFile && strings.HasSuffix(value, "/") {
		return fmt.Errorf("config: remote_runner.%s must not have a trailing slash", name)
	}
	return nil
}

func validateRemoteGitRef(name, value string) error {
	value = strings.TrimSpace(value)
	if value == "" || value == "@" || strings.ContainsAny(value, "\x00\r\n ~^:?*[\\") || strings.HasPrefix(value, "-") ||
		strings.HasPrefix(value, ".") || strings.Contains(value, "/.") || strings.HasSuffix(value, ".lock") ||
		strings.HasPrefix(value, "/") || strings.HasSuffix(value, "/") || strings.HasSuffix(value, ".") ||
		strings.Contains(value, "..") || strings.Contains(value, "@{") || strings.Contains(value, "//") {
		return fmt.Errorf("config: remote_runner.%s is not a safe git branch name", name)
	}
	return nil
}

func containsControlOrSpace(value string) bool {
	for _, r := range value {
		if unicode.IsControl(r) || unicode.IsSpace(r) {
			return true
		}
	}
	return false
}

type Config struct {
	Server     ServerConfig     `yaml:"server"`
	Supervisor SupervisorConfig `yaml:"supervisor"`
	Repo       string           `yaml:"repo"`
	// ProjectID is the optional stable UUID identifying this project durably,
	// independent of the mutable repo/state/store names (#869). Empty for legacy
	// rows; validated as a canonical UUID when present and kept immutable across
	// an in-place config-store upsert.
	ProjectID string `yaml:"project_id,omitempty"`
	// ManagementHome is the optional, descriptive link back to the project's PM /
	// control-room home (#869). Metadata only — never read or traversed here.
	ManagementHome                  ManagementHomeConfig          `yaml:"management_home,omitempty"`
	Outcome                         outcome.Brief                 `yaml:"outcome"`
	LocalPath                       string                        `yaml:"local_path"`
	WorktreeBase                    string                        `yaml:"worktree_base"`
	WorkerRuntime                   WorkerRuntimeConfig           `yaml:"worker_runtime,omitempty"`
	RemoteRunner                    RemoteRunnerConfig            `yaml:"remote_runner"`
	MaxParallel                     int                           `yaml:"max_parallel"`
	MaxLiveWorkers                  int                           `yaml:"max_live_workers"`              // #814: cap on live implementation workers (StatusRunning). When >0, pr_open PR-gate sessions no longer consume spawn capacity, so a gate-bound queue keeps dispatching live workers up to this limit. 0 = legacy (pr_open counts against max_parallel).
	MaxConcurrentByState            map[string]int                `yaml:"max_concurrent_by_state"`       // per-state concurrency limits (e.g. "running": 5, "pr_open": 2)
	MaxRuntimeMinutes               int                           `yaml:"max_runtime_minutes"`           // max worker runtime in minutes (default: 120)
	WorkerSilentTimeoutMinutes      int                           `yaml:"worker_silent_timeout_minutes"` // deprecated (#887): terminal-output-only kill; superseded by stalled_progress_watchdog. Kills a running worker if tmux output hash doesn't change for N minutes (0 = disabled)
	StalledProgressWatchdog         StalledProgressWatchdogConfig `yaml:"stalled_progress_watchdog"`     // #887: explicit-opt-in durable multi-signal watchdog (20-minute max silence when enabled with no override)
	WorkerMaxTokens                 int                           `yaml:"worker_max_tokens"`             // per-attempt live token budget; Claude/Pi exclude cache reads (0 = unlimited)
	WorkerSoftTokenThreshold        *float64                      `yaml:"worker_soft_token_threshold"`   // fraction of worker_max_tokens to trigger checkpoint+respawn (default: 0.8, 0 = disabled)
	MaxRetriesPerIssue              int                           `yaml:"max_retries_per_issue"`         // max failed worker sessions per issue before giving up (default: 3, 0 = unlimited)
	AutoRebase                      bool                          `yaml:"auto_rebase"`                   // auto-attempt rebase for conflicting sessions (default: true)
	ClaudeCmd                       string                        `yaml:"claude_cmd"`                    // deprecated: use model.backends.claude.cmd
	IssueLabel                      string                        `yaml:"issue_label"`                   // deprecated: use issue_labels
	IssueLabels                     []string                      `yaml:"issue_labels"`
	ExcludeLabels                   []string                      `yaml:"exclude_labels"`
	WorkerPrompt                    string                        `yaml:"worker_prompt"`
	BugPrompt                       string                        `yaml:"bug_prompt"`          // prompt template for issues with "bug" label
	EnhancementPrompt               string                        `yaml:"enhancement_prompt"`  // prompt template for issues with "enhancement" label
	PromptSections                  []string                      `yaml:"prompt_sections"`     // additional prompt section files appended to the base prompt
	ValidationContract              bool                          `yaml:"validation_contract"` // generate VALIDATION.md in worktree with test-first guidance
	SessionPrefix                   string                        `yaml:"session_prefix"`      // worker session name prefix (default: first 3 chars of repo name)
	StateDir                        string                        `yaml:"state_dir"`           // state/log directory (default: ~/.maestro/<repo-hash>)
	Model                           ModelConfig                   `yaml:"model"`
	Routing                         RoutingConfig                 `yaml:"routing"`
	DeployCmd                       string                        `yaml:"deploy_cmd"`                         // deprecated (#872): legacy automatic post-merge deploy; folds into delivery.mode=automatic via EffectiveDelivery
	DeployTimeoutMinutes            int                           `yaml:"deploy_timeout_minutes"`             // timeout for deploy command in minutes (default: 15)
	Delivery                        DeliveryConfig                `yaml:"delivery"`                           // #872: approval-gated post-merge delivery (disabled|approval_required|automatic)
	MergeStrategy                   string                        `yaml:"merge_strategy"`                     // "sequential" | "parallel"
	MergeIntervalSeconds            int                           `yaml:"merge_interval_seconds"`             // minimum seconds between merges in sequential mode
	ReviewGate                      string                        `yaml:"review_gate"`                        // "greptile" (default) | "none"
	ReviewGateStreams               []string                      `yaml:"review_gate_streams"`                // optional review dimensions; default ["greptile"], opt-in ["greptile","simplicity"]
	ReviewRetrigger                 ReviewRetriggerConfig         `yaml:"review_retrigger"`                   // #691: re-post "@greptile review" when the gate wedges at pending with no review on head
	AutoRetryReviewFeedback         bool                          `yaml:"auto_retry_review_feedback"`         // close PRs with review comments and respawn a fixer
	MergeExhaustedNonCriticalReview *bool                         `yaml:"merge_exhausted_noncritical_review"` // #565: merge a green PR after review-feedback retries exhaust when only non-critical (P1/P2/P3) findings remain (no P0 on head). nil = default-on.
	AutoRetryRebaseConflicts        bool                          `yaml:"auto_retry_rebase_conflicts"`        // retry PRs whose auto-rebase fails with conflicts
	Telegram                        TelegramConfig                `yaml:"telegram"`
	Notify                          NotifyConfig                  `yaml:"notify"` // #1018: ntfy push transport + alert-class routing
	Versioning                      VersioningConfig              `yaml:"versioning"`
	SelfDeploy                      SelfDeployConfig              `yaml:"self_deploy"` // #698: opt-in post-merge self-deploy of the maestro binary (default OFF)
	GitHubProjects                  GitHubProjectsConfig          `yaml:"github_projects"`
	GitHubApp                       GitHubAppConfig               `yaml:"github_app"`                 // #823: GitHub App auth (app_id + private_key_path + installation_id)
	GitHubMirror                    GitHubMirrorConfig            `yaml:"github_mirror"`              // #826: mirror-first vs api-direct supervisor/orchestrator reads
	MaxRetryBackoffMs               int                           `yaml:"max_retry_backoff_ms"`       // cap for exponential retry backoff in milliseconds (default: 300000 = 5 min)
	AutoResolveFiles                []string                      `yaml:"auto_resolve_files"`         // files to auto-resolve conflicts by keeping both sides
	AutoRestoreFiles                []string                      `yaml:"auto_restore_files"`         // dirty files that may be restored before auto-rebase
	CleanupWorktreesOnMerge         *bool                         `yaml:"cleanup_worktrees_on_merge"` // remove worktrees immediately after PR merge (default: true)
	Pipeline                        PipelineConfig                `yaml:"pipeline"`
	Verify                          VerifyConfig                  `yaml:"verify"` // #705: opt-in post-implementation verify steps (visual evidence)
	Hooks                           HooksConfig                   `yaml:"hooks"`
	Missions                        MissionsConfig                `yaml:"missions"`
	BlockerPatterns                 []string                      `yaml:"blocker_patterns"`         // regex patterns to detect blocker references in issue body for queue skips and dependency_unblock (e.g. "blocked by #(\\d+)"; first capture group must be issue number)
	PollIntervalSeconds             int                           `yaml:"poll_interval_seconds"`    // override poll interval from config (0 = use CLI flag)
	StaleSessionReconciler          StaleSessionReconcilerConfig  `yaml:"stale_session_reconciler"` // filter stale supervisor sessions from operator attention
	SessionRetention                SessionRetentionConfig        `yaml:"session_retention"`        // #497: bound state.Sessions growth via terminal-session compaction
	SourcePath                      string                        `yaml:"-"`                        // path the config was loaded from (not serialized)
	// RuntimeSuperviseIntervalSeconds is injected by the daemon from the actual
	// Options.SuperviseInterval that owns the running loop. It is runtime-only:
	// project YAML cannot claim a cadence the daemon did not schedule.
	RuntimeSuperviseIntervalSeconds int `yaml:"-" json:"-"`
	// SettingsSources records, per fleet-controllable settings key (#839), which
	// layer supplied the effective value: "project" (the project's own YAML),
	// "fleet" (a config-store settings default), or "builtin". Populated by
	// configstore.Load; nil for file-loaded configs. Not serialized and ignored
	// by config equality — it is display provenance for effective_config only.
	SettingsSources map[string]string `yaml:"-" json:"-"`
	// FleetOnlySettings carries the resolved values of daemon/control-plane
	// settings that have no project-YAML field. Config-store loads populate it
	// for Mission Control display; file-loaded configs leave it nil and use the
	// registered built-in defaults.
	FleetOnlySettings map[string]string `yaml:"-" json:"-"`
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

// ParseStrict is Parse plus a strict unknown-key check for project create/edit
// WRITE paths (#869): a misspelled field (e.g. a mistyped `management_home` key)
// is rejected by exact name instead of being silently discarded. Read paths keep
// using the tolerant Parse so legacy rows carrying a field this build does not
// know still load and run unchanged.
//
// The strictness is applied by a separate KnownFields decode probe over the raw
// document; the returned *Config is produced by the same parse() so defaults and
// semantic validation are identical to the tolerant path.
func ParseStrict(data []byte) (*Config, error) {
	if err := strictUnknownKeyCheck(data); err != nil {
		return nil, err
	}
	return parse(data)
}

// strictUnknownKeyCheck decodes the document with yaml KnownFields(true) purely
// to surface an unknown/misspelled key by name. It ignores every other decode
// error (missing repo, type mismatches, etc.) — those are reported with better
// context by parse(); this probe's only job is the unknown-key report. Types
// with a custom UnmarshalYAML decode with their own decoder, so the supervisor
// subtree is checked separately below with a methodless alias.
func strictUnknownKeyCheck(data []byte) error {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var probe Config
	err := dec.Decode(&probe)
	if err != nil && !errors.Is(err, io.EOF) && strings.Contains(err.Error(), "not found in type") {
		return fmt.Errorf("config: %w", err)
	}
	// Any other decode error is left for parse() to report with full context.
	return strictSupervisorUnknownKeyCheck(data)
}

// strictSupervisorConfig is methodless so KnownFields can inspect the
// supervisor subtree instead of SupervisorConfig.UnmarshalYAML handling it with
// a tolerant Node.Decode. The normal Parse path remains tolerant for legacy
// reads; only ParseStrict invokes this probe (#869).
type strictSupervisorConfig SupervisorConfig

func strictSupervisorUnknownKeyCheck(data []byte) error {
	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		return nil // parse() reports syntax/type errors with normal context
	}
	supervisor := yamlMappingValue(&root, "supervisor")
	if supervisor == nil {
		return nil
	}
	encoded, err := yaml.Marshal(supervisor)
	if err != nil {
		return nil
	}
	dec := yaml.NewDecoder(bytes.NewReader(encoded))
	dec.KnownFields(true)
	var probe strictSupervisorConfig
	if err := dec.Decode(&probe); err != nil && strings.Contains(err.Error(), "not found in type") {
		return fmt.Errorf("config: %w", err)
	}
	return nil
}

func yamlMappingValue(root *yaml.Node, key string) *yaml.Node {
	if root == nil {
		return nil
	}
	node := root
	if node.Kind == yaml.DocumentNode && len(node.Content) > 0 {
		node = node.Content[0]
	}
	if node.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == key {
			return node.Content[i+1]
		}
	}
	return nil
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
	var rawRoot yaml.Node
	_ = yaml.Unmarshal(data, &rawRoot) // the successful decode above already validated YAML
	v1StanzaPresent := yamlMappingValue(&rawRoot, "stalled_progress_watchdog") != nil

	// #887 legacy migration / mutual exclusion. A legacy-only timeout is an
	// existing operator opt-in, so migrate its chosen budget to v1 and mark v1
	// explicitly enabled. Any explicit v1 stanza wins and suppresses legacy even
	// when its budget disables v1; otherwise enabled:true + max_silence:-1 would
	// unexpectedly leave the unsafe terminal-only killer armed. Explicit
	// enabled:false is the sole compatibility escape hatch. No parse result can
	// run both paths.
	if cfg.WorkerSilentTimeoutMinutes > 0 {
		explicitV1Disable := cfg.StalledProgressWatchdog.Enabled != nil && !*cfg.StalledProgressWatchdog.Enabled
		if !v1StanzaPresent {
			enabled := true
			cfg.StalledProgressWatchdog.Enabled = &enabled
			cfg.StalledProgressWatchdog.MaxSilenceMinutes = cfg.WorkerSilentTimeoutMinutes
			cfg.WorkerSilentTimeoutMinutes = 0
		} else if !explicitV1Disable {
			cfg.WorkerSilentTimeoutMinutes = 0
		}
	}
	if cfg.Repo == "" {
		return nil, fmt.Errorf("config: repo is required")
	}
	if cfg.Pipeline.AdvisorReviewRounds < 0 || cfg.Pipeline.AdvisorReviewRounds > MaxAdvisorReviewRounds {
		return nil, fmt.Errorf("config: pipeline.advisor_review_rounds must be between 1 and %d when set (0 uses the default of %d)", MaxAdvisorReviewRounds, DefaultAdvisorReviewRounds)
	}

	// #869: optional project identity metadata. A present project_id must be a
	// UUID; a present management_home must satisfy its field contract. Both are
	// absent in legacy configs, so these checks are no-ops there.
	if err := validateProjectID(cfg.ProjectID); err != nil {
		return nil, err
	}
	if err := validateManagementHome(cfg.ManagementHome); err != nil {
		return nil, err
	}
	if err := validateWorkerRuntime(cfg.WorkerRuntime); err != nil {
		return nil, err
	}
	if err := validateSupervisorTempDir(cfg.Supervisor); err != nil {
		return nil, err
	}
	if err := validateRemoteRunner(cfg); err != nil {
		return nil, err
	}
	if !cfg.Delivery.ValidMode() {
		return nil, fmt.Errorf("config: delivery.mode %q is invalid (want disabled, approval_required, or automatic)", cfg.Delivery.Mode)
	}
	effectiveDelivery := cfg.EffectiveDelivery()
	if effectiveDelivery.Mode == DeliveryModeApprovalRequired {
		if strings.TrimSpace(effectiveDelivery.Command) == "" {
			return nil, fmt.Errorf("config: delivery.command is required when delivery.mode is approval_required")
		}
		if !validApprovalDeliveryEntrypoint(effectiveDelivery.Command) {
			return nil, fmt.Errorf("config: delivery.command must be one repo-relative executable path like ./scripts/deploy.sh when delivery.mode is approval_required (no arguments, whitespace, shell syntax, absolute path, or ..)")
		}
		if strings.TrimSpace(effectiveDelivery.VerifyCommand) == "" {
			return nil, fmt.Errorf("config: delivery.verify_command is required when delivery.mode is approval_required")
		}
		if !validApprovalDeliveryEntrypoint(effectiveDelivery.VerifyCommand) {
			return nil, fmt.Errorf("config: delivery.verify_command must be one repo-relative executable path like ./scripts/verify-delivery.sh when delivery.mode is approval_required (no arguments, whitespace, shell syntax, absolute path, or ..)")
		}
		if strings.TrimSpace(effectiveDelivery.TargetLabel) == "" {
			return nil, fmt.Errorf("config: delivery.target_label is required when delivery.mode is approval_required")
		}
		if err := validateDeliverySafeLabel("target_label", effectiveDelivery.TargetLabel, 256); err != nil {
			return nil, err
		}
		if strings.TrimSpace(effectiveDelivery.VerificationLabel) == "" {
			return nil, fmt.Errorf("config: delivery.verification_label is required when delivery.mode is approval_required")
		}
		if err := validateDeliverySafeLabel("verification_label", effectiveDelivery.VerificationLabel, 512); err != nil {
			return nil, err
		}
		rollbackLabel := strings.TrimSpace(effectiveDelivery.RollbackLabel)
		if rollbackLabel == "" {
			return nil, fmt.Errorf("config: delivery.rollback_label is required when delivery.mode is approval_required (use \"none: <reason>\" when rollback is unavailable)")
		}
		if err := validateDeliverySafeLabel("rollback_label", effectiveDelivery.RollbackLabel, 512); err != nil {
			return nil, err
		}
		if strings.EqualFold(rollbackLabel, "none") || (strings.HasPrefix(strings.ToLower(rollbackLabel), "none:") && strings.TrimSpace(rollbackLabel[len("none:"):]) == "") {
			return nil, fmt.Errorf("config: delivery.rollback_label must include a reason after \"none:\" when rollback is unavailable")
		}
	}

	// A negative max_live_workers is meaningless; clamp to 0 (legacy: pr_open
	// counts against max_parallel) so a typo can never silently disable
	// dispatch entirely (#814).
	if cfg.MaxLiveWorkers < 0 {
		cfg.MaxLiveWorkers = 0
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
	cfg.WorkerRuntime.ScratchRoot = expandHome(cfg.WorkerRuntime.ScratchRoot)
	cfg.Supervisor.TempDir = expandHome(cfg.Supervisor.TempDir)
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
	cfg.Pipeline.Advisor.Prompt = expandHome(cfg.Pipeline.Advisor.Prompt)
	cfg.Pipeline.Implementer.Prompt = expandHome(cfg.Pipeline.Implementer.Prompt)
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
	canonicalStateDir, err := canonicalPath(cfg.StateDir)
	if err != nil {
		return nil, fmt.Errorf("config: canonicalize state_dir %q: %w", cfg.StateDir, err)
	}
	cfg.StateDir = canonicalStateDir

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
		if len(cfg.Model.FallbackBackends) == 0 && len(cfg.Model.ProviderLanes) > 0 && strings.TrimSpace(cfg.Model.ProviderLanes[0].Default) != "" {
			cfg.Model.Default = strings.TrimSpace(cfg.Model.ProviderLanes[0].Default)
		} else {
			cfg.Model.Default = "claude"
		}
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
	// Provider lanes must reference explicit backend definitions. Validate before
	// the legacy model.default compatibility block below can synthesize a backend
	// for the effective default.
	if err := validateProviderLanes(cfg); err != nil {
		return nil, err
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
			return nil, fmt.Errorf("config: model.fallback_backends references %q which is not defined in model.backends; define it under model.backends or remove it from fallback_backends (the default backend is auto-defined, but fallback names must be declared explicitly)", fb)
		}
		if def.NonAgentic {
			return nil, fmt.Errorf("config: model.fallback_backends includes %q which is marked non_agentic; the fallback chain is the worker chain — a non-agentic entry would produce fake-PR sessions when paid backends are exhausted. Remove %q from fallback_backends and use it only for supervisor sub-tasks", fb, fb)
		}
	}
	if cfg.Model.HoldOnCooldown.MaxWaitMinutes < 0 {
		return nil, fmt.Errorf("config: model.hold_on_cooldown.max_wait_minutes = %d; want >= 0 (0 uses the default window)", cfg.Model.HoldOnCooldown.MaxWaitMinutes)
	}
	if cfg.Supervisor.AttemptTimeoutSeconds < 0 {
		return nil, fmt.Errorf("config: supervisor.attempt_timeout_seconds = %d; want >= 0 (0 uses the 45s default)", cfg.Supervisor.AttemptTimeoutSeconds)
	}
	if cfg.Supervisor.TotalTimeoutSeconds < 0 {
		return nil, fmt.Errorf("config: supervisor.total_timeout_seconds = %d; want >= 0 (0 uses the 180s default)", cfg.Supervisor.TotalTimeoutSeconds)
	}
	for name, def := range cfg.Model.Backends {
		if def.SupervisorAttemptTimeoutSeconds < 0 {
			return nil, fmt.Errorf("config: model.backends.%s.supervisor_attempt_timeout_seconds = %d; want >= 0", name, def.SupervisorAttemptTimeoutSeconds)
		}
	}
	// #704: quota calibration sanity. Capacities must be non-negative and
	// the dispatch threshold a fraction in (0, 1]; a percent-style value
	// (e.g. 85) almost certainly means the operator meant 0.85, so fail
	// fast instead of silently never steering dispatch.
	for name, def := range cfg.Model.Backends {
		if def.Quota.WindowTokens < 0 || def.Quota.WeeklyTokens < 0 {
			return nil, fmt.Errorf("config: model.backends.%s.quota window_tokens/weekly_tokens must be >= 0", name)
		}
		if t := def.Quota.DispatchThreshold; t < 0 || t > 1 {
			return nil, fmt.Errorf("config: model.backends.%s.quota.dispatch_threshold = %v; want a fraction in (0, 1], e.g. 0.85 for 85%%", name, t)
		}
		// #805: usage-limit classifier extras must compile — an invalid
		// regex would otherwise be skipped silently at classification
		// time and the operator would believe the signature is covered.
		for _, p := range def.UsageLimitPatterns {
			if _, err := regexp.Compile(p); err != nil {
				return nil, fmt.Errorf("config: model.backends.%s.usage_limit_patterns entry %q does not compile: %v", name, p, err)
			}
		}
		// #838: a mistyped pricing_class (e.g. "meterd", "per_token") would
		// silently leave the metered guard disabled, re-opening the exact
		// always-on burn the field exists to prevent. Fail fast instead.
		if !validPricingClass(def.PricingClass) {
			return nil, fmt.Errorf("config: model.backends.%s.pricing_class = %q; want one of flat, subscription, metered (or empty)", name, def.PricingClass)
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

	// Task-aware routing tiers/policy (#783). Validation fires only when the
	// new fields are present or mode: policy is requested, so existing configs
	// (no tiers/policy, mode manual|auto) parse and validate identically.
	if err := validateRoutingPolicy(cfg); err != nil {
		return nil, err
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

	// GitHub read source (#826). Normalize to a known value; anything unrecognised
	// — including the empty default and typos — resolves to API-direct, which is
	// always the safe direction (it can only fall back to the authoritative API).
	switch strings.ToLower(strings.TrimSpace(cfg.GitHubMirror.Source)) {
	case GitHubSourceMirrorFirst:
		cfg.GitHubMirror.Source = GitHubSourceMirrorFirst
	default:
		cfg.GitHubMirror.Source = GitHubSourceAPI
	}

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
	if err := cfg.Outcome.Validate(); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}

	return cfg, nil
}

func validApprovalDeliveryEntrypoint(command string) bool {
	if command == "" || command != strings.TrimSpace(command) || !strings.HasPrefix(command, "./") {
		return false
	}
	rel := strings.TrimPrefix(command, "./")
	if rel == "" || pathpkg.IsAbs(rel) || pathpkg.Clean(rel) != rel || strings.HasPrefix(rel, "../") {
		return false
	}
	for _, part := range strings.Split(rel, "/") {
		if part == "" || part == "." || part == ".." {
			return false
		}
	}
	for _, ch := range rel {
		if (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') ||
			(ch >= '0' && ch <= '9') || ch == '/' || ch == '_' || ch == '-' || ch == '.' {
			continue
		}
		return false
	}
	return true
}

func validateDeliverySafeLabel(name, value string, maxRunes int) error {
	if !utf8.ValidString(value) || utf8.RuneCountInString(value) > maxRunes {
		return fmt.Errorf("config: delivery.%s must be valid UTF-8 and at most %d characters", name, maxRunes)
	}
	for _, ch := range value {
		if unicode.IsControl(ch) {
			return fmt.Errorf("config: delivery.%s must not contain control characters", name)
		}
		if unicode.Is(unicode.Cf, ch) {
			return fmt.Errorf("config: delivery.%s must not contain Unicode format characters", name)
		}
	}
	return nil
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
	if policy.UnchangedDecisionWindowSeconds < 0 {
		return fmt.Errorf("supervisor.unchanged_decision_window_seconds must be >= 0")
	}
	if policy.RecommendationTTLSeconds < 0 {
		return fmt.Errorf("supervisor.recommendation_ttl_seconds must be >= 0")
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
	policy.OperatorGate.CheckNames = normalizeStringList(policy.OperatorGate.CheckNames)
	policy.OperatorGate.Labels = normalizeStringList(policy.OperatorGate.Labels)
	policy.OperatorGate.RequiredAction = strings.TrimSpace(policy.OperatorGate.RequiredAction)
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
	if msg := c.verifyVisualWarning(); msg != "" {
		warnings = append(warnings, msg)
	}
	warnings = append(warnings, c.deliveryWarnings()...)
	warnings = append(warnings, c.credentialWarnings()...)
	return warnings
}

// credentialWarnings surfaces config fields that still carry credential
// material in plaintext (#1143). Plaintext keeps working -- refusing to load
// would strand every pre-migration row -- but the value is visible in
// `maestro config-store export`, in any YAML dumped from the store, and in
// every DB backup, so the operator is told which field to move.
//
// Audit of every credential-bearing field in internal/config, for the record:
//
//   - telegram.bot_token          -- plaintext; migrate to telegram.bot_token_env (below)
//   - server.auth.token_env       -- env-only already (#487)
//   - notify.ntfy.token_env       -- env-only already (#1018)
//   - self_deploy.health_token_env -- env-only already
//   - github_app.private_key_path -- a path; the PEM never enters config (#823)
//   - model.backends.*.mcp.servers.*.bearer_token_env_var -- env-only already
//   - model.backends.*.mcp.servers.*.{headers,env} -- free-form maps handed to
//     the worker harness verbatim, so a literal secret pasted there is stored
//     just like telegram.bot_token was. Warned about below.
func (c *Config) credentialWarnings() []string {
	if c == nil {
		return nil
	}
	var out []string
	if strings.TrimSpace(c.Telegram.BotToken) != "" {
		if c.Telegram.PlaintextTokenActive() {
			out = append(out, "config: telegram.bot_token stores the bot token in plaintext — it is visible in `maestro config-store export` and in every DB backup. Move the token into a secret manager, expose it to the daemon as an environment variable, and set telegram.bot_token_env to that variable name instead.")
		} else {
			out = append(out, fmt.Sprintf("config: telegram.bot_token still holds a plaintext credential but is unused because telegram.bot_token_env (%s) resolves — delete telegram.bot_token from the config store.", strings.TrimSpace(c.Telegram.BotTokenEnv)))
		}
	}
	out = append(out, c.mcpCredentialWarnings()...)
	return out
}

// credentialKeyHints are substrings that mark a header/env key as carrying a
// credential. Matched case-insensitively against the key only; values are
// never logged.
var credentialKeyHints = []string{"authorization", "token", "secret", "password", "passwd", "apikey", "api_key", "access_key", "private_key", "credential"}

// looksLikeCredentialKey reports whether a header or env key names a secret.
func looksLikeCredentialKey(key string) bool {
	k := strings.ToLower(strings.TrimSpace(key))
	if k == "" {
		return false
	}
	for _, hint := range credentialKeyHints {
		if strings.Contains(k, hint) {
			return true
		}
	}
	// "key" alone is too generic to match as a substring (it hits "keyword",
	// "monkey"), so only an exact/suffixed form counts.
	return k == "key" || strings.HasSuffix(k, "-key") || strings.HasSuffix(k, "_key")
}

// isEnvReference reports whether a value is an indirection ($VAR or ${VAR})
// rather than the credential itself.
func isEnvReference(value string) bool {
	v := strings.TrimSpace(value)
	return strings.HasPrefix(v, "$")
}

// mcpCredentialWarnings names MCP server headers/env entries whose key marks a
// credential and whose value is a literal rather than an environment
// reference. The value is never echoed.
func (c *Config) mcpCredentialWarnings() []string {
	var out []string
	for _, backend := range sortedKeys(c.Model.Backends) {
		servers := c.Model.Backends[backend].MCP.Servers
		for _, server := range sortedKeys(servers) {
			def := servers[server]
			for _, block := range []struct {
				name   string
				values map[string]string
			}{{"headers", def.Headers}, {"env", def.Env}} {
				for _, key := range sortedKeys(block.values) {
					value := strings.TrimSpace(block.values[key])
					if value == "" || !looksLikeCredentialKey(key) || isEnvReference(value) {
						continue
					}
					out = append(out, fmt.Sprintf("config: model.backends.%s.mcp.servers.%s.%s.%s stores a credential in plaintext — it is visible in `maestro config-store export` and in every DB backup. Keep the secret in the environment and reference it (for an HTTP server use bearer_token_env_var).", backend, server, block.name, key))
				}
			}
		}
	}
	return out
}

// sortedKeys returns the map keys in deterministic order so warning output is
// stable across loads.
func sortedKeys[V any](m map[string]V) []string {
	if len(m) == 0 {
		return nil
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// deliveryWarnings surfaces the #872 delivery-block misconfigurations at load
// time:
//
//   - a legacy deploy_cmd (with no explicit delivery block) still runs
//     automatically after every merge — retained for back-compat but loud, so
//     an operator migrates to delivery.mode: approval_required deliberately;
//   - delivery.mode: automatic runs an unattended deploy on every merge — the
//     opt-out from the approval gate, worth naming so it is never a silent
//     default;
//   - an invalid delivery.mode is rejected loudly rather than silently
//     resolving to a surprising behavior;
//   - an automatic delivery block that names no command is inert. The same
//     omission in approval_required is a hard parse error.
func (c *Config) deliveryWarnings() []string {
	if c == nil {
		return nil
	}
	var out []string
	legacy := strings.TrimSpace(c.DeployCmd) != "" && !c.Delivery.configured()
	if legacy {
		out = append(out, "config: deploy_cmd is deprecated (#872) — it runs an unattended deploy automatically after every merge (delivery.mode=automatic). Migrate to a delivery: block with mode: approval_required so a merged revision creates an auditable approval before the deploy runs.")
	}
	if !c.Delivery.ValidMode() {
		out = append(out, fmt.Sprintf("config: delivery.mode %q is invalid — must be one of disabled, approval_required, automatic.", c.Delivery.Mode))
		return out
	}
	eff := c.EffectiveDelivery()
	if eff.Mode == DeliveryModeAutomatic && c.Delivery.configured() {
		out = append(out, "config: delivery.mode: automatic runs the delivery command with no approval on every qualifying merge — the opt-out from the #872 approval gate. Use delivery.mode: approval_required unless unattended delivery is intended.")
	}
	if c.Delivery.configured() && strings.TrimSpace(c.Delivery.Command) == "" && eff.Mode == DeliveryModeAutomatic {
		out = append(out, "config: a delivery: block is set but delivery.command is empty — no delivery will run. Set delivery.command or remove the block.")
	}
	return out
}

// verifyVisualWarning surfaces a verify.visual block that is enabled but
// cannot run (#705): without a capture command or UI path globs the step is
// silently inert, which reads as "visual evidence is covered" when it is not.
func (c *Config) verifyVisualWarning() string {
	v := c.Verify.Visual
	if !v.Enabled || v.Active() {
		return ""
	}
	var missing []string
	if strings.TrimSpace(v.Command) == "" {
		missing = append(missing, "verify.visual.command")
	}
	if len(v.Paths) == 0 {
		missing = append(missing, "verify.visual.paths")
	}
	return fmt.Sprintf(
		"config: verify.visual.enabled is true but %s is not set — the visual-evidence step is inert. Set a capture command and UI path globs, or disable verify.visual.",
		strings.Join(missing, " and "),
	)
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
//   - routing.mode is empty or "manual" (auto/policy would be the task lever)
//   - no per-role pipeline backends are set (pipeline.{planner,validator}.backend
//     is a legitimate role-based shape even without auto/policy)
//
// This keeps the warning silent for the common single-backend project, for
// projects that opted into auto/policy routing, and for projects that
// purposefully pin per-role pipeline backends. It fires loudly for the failure
// mode in #427.
func (c *Config) manualRoutingLabelPinWarning() string {
	if c == nil {
		return ""
	}
	if len(c.Model.Backends) < 2 {
		return ""
	}
	mode := strings.ToLower(strings.TrimSpace(c.Routing.Mode))
	if mode == "auto" || mode == PolicyMode {
		return ""
	}
	if strings.TrimSpace(c.Pipeline.Planner.Backend) != "" ||
		strings.TrimSpace(c.Pipeline.Advisor.Backend) != "" ||
		strings.TrimSpace(c.Pipeline.Validator.Backend) != "" {
		return ""
	}
	return fmt.Sprintf(
		"config: %d backends are configured but routing.mode is %q and no pipeline.{planner,advisor,validator}.backend is set — backend selection will be by model:<name> label or model.default only, not by task content. Set routing.mode: policy for task-aware routing, routing.mode: auto for LLM routing, or per-role pipeline backends for role-based routing.",
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
		SupervisorActionAddReadyLabel:              true,
		SupervisorActionRemoveReadyLabel:           true,
		SupervisorActionAddBlockedLabel:            true,
		SupervisorActionRemoveBlockedLabel:         true,
		SupervisorActionHoldMerge:                  true,
		SupervisorActionReleaseMerge:               true,
		SupervisorActionAddIssueComment:            true,
		SupervisorActionMergePR:                    true,
		SupervisorActionCloseIssue:                 true,
		SupervisorActionCloseIssueBatch:            true,
		SupervisorActionDeleteWorktree:             true,
		SupervisorActionChangeGlobalConfig:         true,
		SupervisorActionApplyLessonProposal:        true,
		SupervisorActionSpawnRepairWorker:          true,
		SupervisorActionSpawnReviewRepair:          true,
		SupervisorActionRestartWorker:              true,
		SupervisorActionRestartStaleBackendWorkers: true,
		SupervisorActionStopWorker:                 true,
		SupervisorActionEditIssueBody:              true,
	}
}

func knownSupervisorActionNames() []string {
	return []string{
		SupervisorActionAddReadyLabel,
		SupervisorActionRemoveReadyLabel,
		SupervisorActionAddBlockedLabel,
		SupervisorActionRemoveBlockedLabel,
		SupervisorActionHoldMerge,
		SupervisorActionReleaseMerge,
		SupervisorActionAddIssueComment,
		SupervisorActionMergePR,
		SupervisorActionCloseIssue,
		SupervisorActionCloseIssueBatch,
		SupervisorActionDeleteWorktree,
		SupervisorActionChangeGlobalConfig,
		SupervisorActionApplyLessonProposal,
		SupervisorActionSpawnRepairWorker,
		SupervisorActionSpawnReviewRepair,
		SupervisorActionRestartWorker,
		SupervisorActionRestartStaleBackendWorkers,
		SupervisorActionStopWorker,
		SupervisorActionEditIssueBody,
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
	stateOwners := make(map[string]string)
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
		if previous, exists := stateOwners[cfg.StateDir]; exists {
			return nil, fmt.Errorf("load %s: canonical state_dir %q is already used by %s", name, cfg.StateDir, previous)
		}
		stateOwners[cfg.StateDir] = name
		cfgs = append(cfgs, cfg)
	}
	if len(cfgs) == 0 {
		return nil, fmt.Errorf("no config files found in %s", dir)
	}
	return cfgs, nil
}

// canonicalPath returns one stable absolute identity for a path. EvalSymlinks
// is applied to the deepest existing ancestor so aliases remain canonical even
// when the final state directory has not been created yet. Delivery claim keys
// use this value; lexical or symlink aliases must never produce two SQLite rows
// for the same state.json directory.
func canonicalPath(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", errors.New("path is required")
	}
	abs, err := filepath.Abs(filepath.Clean(path))
	if err != nil {
		return "", err
	}
	probe := abs
	var suffix []string
	for {
		resolved, evalErr := filepath.EvalSymlinks(probe)
		if evalErr == nil {
			for i := len(suffix) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, suffix[i])
			}
			return filepath.Clean(resolved), nil
		}
		if !os.IsNotExist(evalErr) {
			return "", evalErr
		}
		parent := filepath.Dir(probe)
		if parent == probe {
			// filepath.Abs already produced a stable clean lexical identity;
			// reaching the root only means no ancestor could be stat'ed.
			return abs, nil
		}
		suffix = append(suffix, filepath.Base(probe))
		probe = parent
	}
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
