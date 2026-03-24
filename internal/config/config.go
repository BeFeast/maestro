package config

import (
	"crypto/md5"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

type TelegramConfig struct {
	Target      string `yaml:"target"`
	BotToken    string `yaml:"bot_token"`
	OpenclawURL string `yaml:"openclaw_url"`
	DigestMode  bool   `yaml:"digest_mode"` // batch notifications per cycle instead of sending immediately
}

// BackendDef defines a model backend CLI.
type BackendDef struct {
	Cmd        string   `yaml:"cmd"`
	ExtraArgs  []string `yaml:"extra_args"`
	PromptMode string   `yaml:"prompt_mode"` // how to deliver prompt: "arg" (last argument), "stdin" (via stdin), "file" (file path as argument)
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

// RoutingConfig controls automatic backend selection via LLM router.
type RoutingConfig struct {
	Mode            string `yaml:"mode"`              // "auto", "manual" (labels only)
	RouterModel     string `yaml:"router_model"`      // backend name from model.backends (default: "claude")
	RouterModelName string `yaml:"router_model_name"` // specific model to use (default: "claude-sonnet-4-6")
	RouterPrompt    string `yaml:"router_prompt"`     // prompt template with {{BACKENDS}}, {{NUMBER}}, {{TITLE}}, {{BODY}}

	// Role-specific backend overrides for the planner → implementer → validator pipeline.
	// Each maps to a backend name from model.backends. If empty, falls back to issue-level routing.
	PlannerBackend        string `yaml:"planner_backend"`        // backend for planning phase (e.g. "gemini-flash")
	ImplementationBackend string `yaml:"implementation_backend"` // backend for implementation phase (e.g. "claude")
	ValidatorBackend      string `yaml:"validator_backend"`      // backend for validation phase (e.g. "claude")
}

// ServerConfig controls the optional HTTP API server.
type ServerConfig struct {
	Port int `yaml:"port"` // 0 = disabled (default)
}

// RoleConfig defines settings for a single pipeline role (planner, validator).
type RoleConfig struct {
	Enabled           bool   `yaml:"enabled"`
	Backend           string `yaml:"backend"`             // backend name from model.backends (empty = use default)
	Prompt            string `yaml:"prompt"`              // path to prompt template (empty = built-in default)
	MaxRuntimeMinutes int    `yaml:"max_runtime_minutes"` // override per-role max runtime (0 = use global)
}

// PipelineConfig controls the planner → implementer → validator pipeline
// and the GSD-inspired pre-worker context preparation phases.
type PipelineConfig struct {
	// Phase-based pipeline (planner → implementer → validator)
	Enabled   bool       `yaml:"enabled"`   // enable 3-phase pipeline (default: false = legacy single-phase)
	Planner   RoleConfig `yaml:"planner"`   // planner role settings
	Validator RoleConfig `yaml:"validator"` // validator role settings
	// Implementer uses the existing worker_prompt / bug_prompt / enhancement_prompt settings.

	// GSD-inspired pre-worker context preparation phases
	Research       bool  `yaml:"research"`        // spawn a research subagent before worker starts (default: false)
	PlanValidation *bool `yaml:"plan_validation"` // validate a plan before coding starts (default: true)
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

// HooksConfig holds lifecycle hook scripts that run at key points.
type HooksConfig struct {
	AfterCreate  string `yaml:"after_create"`  // runs once when a new issue workspace is first created
	BeforeRun    string `yaml:"before_run"`    // runs before each agent attempt (fatal on failure)
	AfterRun     string `yaml:"after_run"`     // runs after each agent attempt (logged, not fatal)
	BeforeRemove string `yaml:"before_remove"` // runs before workspace cleanup (logged, not fatal)
	TimeoutMs    int    `yaml:"timeout_ms"`    // timeout for hook execution in milliseconds (default: 60000)
}

type Config struct {
	Server                     ServerConfig         `yaml:"server"`
	Repo                       string               `yaml:"repo"`
	LocalPath                  string               `yaml:"local_path"`
	WorktreeBase               string               `yaml:"worktree_base"`
	MaxParallel                int                  `yaml:"max_parallel"`
	MaxConcurrentByState       map[string]int       `yaml:"max_concurrent_by_state"`       // per-state concurrency limits (e.g. "running": 5, "pr_open": 2)
	MaxRuntimeMinutes          int                  `yaml:"max_runtime_minutes"`           // max worker runtime in minutes (default: 120)
	WorkerSilentTimeoutMinutes int                  `yaml:"worker_silent_timeout_minutes"` // kill running worker if tmux output hash doesn't change for N minutes (0 = disabled)
	WorkerMaxTokens            int                  `yaml:"worker_max_tokens"`             // kill worker when token usage exceeds this threshold (0 = unlimited)
	WorkerSoftTokenThreshold   *float64             `yaml:"worker_soft_token_threshold"`   // fraction of worker_max_tokens to trigger checkpoint+respawn (default: 0.8, 0 = disabled)
	MaxRetriesPerIssue         int                  `yaml:"max_retries_per_issue"`         // max failed worker sessions per issue before giving up (default: 3, 0 = unlimited)
	AutoRebase                 bool                 `yaml:"auto_rebase"`                   // auto-attempt rebase for conflicting sessions (default: true)
	ClaudeCmd                  string               `yaml:"claude_cmd"`                    // deprecated: use model.backends.claude.cmd
	IssueLabel                 string               `yaml:"issue_label"`                   // deprecated: use issue_labels
	IssueLabels                []string             `yaml:"issue_labels"`
	ExcludeLabels              []string             `yaml:"exclude_labels"`
	WorkerPrompt               string               `yaml:"worker_prompt"`
	BugPrompt                  string               `yaml:"bug_prompt"`          // prompt template for issues with "bug" label
	EnhancementPrompt          string               `yaml:"enhancement_prompt"`  // prompt template for issues with "enhancement" label
	PromptSections             []string             `yaml:"prompt_sections"`     // additional prompt section files appended to the base prompt
	ValidationContract         bool                 `yaml:"validation_contract"` // generate VALIDATION.md in worktree with test-first guidance
	SessionPrefix              string               `yaml:"session_prefix"`      // worker session name prefix (default: first 3 chars of repo name)
	StateDir                   string               `yaml:"state_dir"`           // state/log directory (default: ~/.maestro/<repo-hash>)
	Model                      ModelConfig          `yaml:"model"`
	Routing                    RoutingConfig        `yaml:"routing"`
	DeployCmd                  string               `yaml:"deploy_cmd"`             // shell command to run after successful PR merge
	DeployTimeoutMinutes       int                  `yaml:"deploy_timeout_minutes"` // timeout for deploy command in minutes (default: 15)
	MergeStrategy              string               `yaml:"merge_strategy"`         // "sequential" | "parallel"
	MergeIntervalSeconds       int                  `yaml:"merge_interval_seconds"` // minimum seconds between merges in sequential mode
	Telegram                   TelegramConfig       `yaml:"telegram"`
	Versioning                 VersioningConfig     `yaml:"versioning"`
	GitHubProjects             GitHubProjectsConfig `yaml:"github_projects"`
	MaxRetryBackoffMs          int                  `yaml:"max_retry_backoff_ms"`       // cap for exponential retry backoff in milliseconds (default: 300000 = 5 min)
	AutoResolveFiles           []string             `yaml:"auto_resolve_files"`         // files to auto-resolve conflicts by keeping both sides
	CleanupWorktreesOnMerge    *bool                `yaml:"cleanup_worktrees_on_merge"` // remove worktrees immediately after PR merge (default: true)
	Pipeline                   PipelineConfig       `yaml:"pipeline"`
	Hooks                      HooksConfig          `yaml:"hooks"`
	Missions                   MissionsConfig       `yaml:"missions"`
	BlockerPatterns            []string             `yaml:"blocker_patterns"`      // regex patterns to detect blocker references in issue body (e.g. "blocked by #(\\d+)")
	PollIntervalSeconds        int                  `yaml:"poll_interval_seconds"` // override poll interval from config (0 = use CLI flag)
	SourcePath                 string               `yaml:"-"`                     // path the config was loaded from (not serialized)
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
	for _, path := range candidates {
		data, err = os.ReadFile(path)
		if err == nil {
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
	// Set SourcePath to the candidate that succeeded
	for _, p := range candidates {
		if _, e := os.Stat(p); e == nil {
			cfg.SourcePath = p
			break
		}
	}
	return cfg, nil
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
	cfg.WorkerPrompt = expandHome(cfg.WorkerPrompt)
	cfg.BugPrompt = expandHome(cfg.BugPrompt)
	cfg.EnhancementPrompt = expandHome(cfg.EnhancementPrompt)
	cfg.Pipeline.Planner.Prompt = expandHome(cfg.Pipeline.Planner.Prompt)
	cfg.Pipeline.Validator.Prompt = expandHome(cfg.Pipeline.Validator.Prompt)
	for i, s := range cfg.PromptSections {
		cfg.PromptSections[i] = expandHome(s)
	}
	cfg.StateDir = expandHome(cfg.StateDir)

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

	if cfg.Telegram.OpenclawURL == "" {
		cfg.Telegram.OpenclawURL = "http://localhost:18789"
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
			`depends on.*?#(\d+)`,
		}
	}

	return cfg, nil
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
