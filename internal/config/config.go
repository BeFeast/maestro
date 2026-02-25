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
	Default  string                `yaml:"default"` // "claude", "codex", etc.
	Backends map[string]BackendDef `yaml:"backends"`
}

// VersioningConfig controls automatic version bumping on PR merge.
type VersioningConfig struct {
	Enabled       bool     `yaml:"enabled"`
	Files         []string `yaml:"files"`          // Files containing version strings to update
	DefaultBump   string   `yaml:"default_bump"`   // "patch", "minor", or "major"
	TagPrefix     string   `yaml:"tag_prefix"`     // e.g. "v"
	CreateRelease bool     `yaml:"create_release"` // Create GitHub release on bump
}

// RoutingConfig controls automatic backend selection via LLM router.
type RoutingConfig struct {
	Mode            string `yaml:"mode"`              // "auto", "manual" (labels only)
	RouterModel     string `yaml:"router_model"`      // backend name from model.backends (default: "claude")
	RouterModelName string `yaml:"router_model_name"` // specific model to use (default: "claude-sonnet-4-6")
	RouterPrompt    string `yaml:"router_prompt"`     // prompt template with {{BACKENDS}}, {{NUMBER}}, {{TITLE}}, {{BODY}}
}

type Config struct {
	Repo                       string           `yaml:"repo"`
	LocalPath                  string           `yaml:"local_path"`
	WorktreeBase               string           `yaml:"worktree_base"`
	MaxParallel                int              `yaml:"max_parallel"`
	MaxRuntimeMinutes          int              `yaml:"max_runtime_minutes"`           // max worker runtime in minutes (default: 120)
	WorkerSilentTimeoutMinutes int              `yaml:"worker_silent_timeout_minutes"` // kill running worker if tmux output hash doesn't change for N minutes (0 = disabled)
	AutoRebase                 bool             `yaml:"auto_rebase"`                   // auto-attempt rebase for conflicting sessions (default: true)
	ClaudeCmd                  string           `yaml:"claude_cmd"`                    // deprecated: use model.backends.claude.cmd
	IssueLabel                 string           `yaml:"issue_label"`                   // deprecated: use issue_labels
	IssueLabels                []string         `yaml:"issue_labels"`
	ExcludeLabels              []string         `yaml:"exclude_labels"`
	WorkerPrompt               string           `yaml:"worker_prompt"`
	BugPrompt                  string           `yaml:"bug_prompt"`                    // prompt template for issues with "bug" label
	EnhancementPrompt          string           `yaml:"enhancement_prompt"`            // prompt template for issues with "enhancement" label
	SessionPrefix              string           `yaml:"session_prefix"`                // worker session name prefix (default: first 3 chars of repo name)
	StateDir                   string           `yaml:"state_dir"`                     // state/log directory (default: ~/.maestro/<repo-hash>)
	Model                      ModelConfig      `yaml:"model"`
	Routing                    RoutingConfig    `yaml:"routing"`
	DeployCmd                  string           `yaml:"deploy_cmd"`                    // shell command to run after successful PR merge
	MergeStrategy              string           `yaml:"merge_strategy"`                // "sequential" | "parallel"
	MergeIntervalSeconds       int              `yaml:"merge_interval_seconds"`        // minimum seconds between merges in sequential mode
	Telegram                   TelegramConfig   `yaml:"telegram"`
	Versioning                 VersioningConfig `yaml:"versioning"`
	AutoResolveFiles           []string         `yaml:"auto_resolve_files"`            // files to auto-resolve conflicts by keeping both sides
}

// LoadFrom loads config from a specific path.
func LoadFrom(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}
	return parse(data)
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
	return parse(data)
}

func parse(data []byte) (*Config, error) {

	cfg := &Config{
		MaxParallel:          5,
		MaxRuntimeMinutes:    120,
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

func expandHome(path string) string {
	if len(path) > 1 && path[:2] == "~/" {
		return filepath.Join(os.Getenv("HOME"), path[2:])
	}
	return path
}
