package config

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

type TelegramConfig struct {
	Target      string `yaml:"target"`
	OpenclawURL string `yaml:"openclaw_url"`
}

// BackendDef defines a model backend CLI.
type BackendDef struct {
	Cmd       string   `yaml:"cmd"`
	ExtraArgs []string `yaml:"extra_args"`
}

// ModelConfig holds multi-backend configuration.
type ModelConfig struct {
	Default  string                `yaml:"default"`  // "claude", "codex", etc.
	Backends map[string]BackendDef `yaml:"backends"`
}

type Config struct {
	Repo          string         `yaml:"repo"`
	LocalPath     string         `yaml:"local_path"`
	WorktreeBase  string         `yaml:"worktree_base"`
	MaxParallel   int            `yaml:"max_parallel"`
	ClaudeCmd     string         `yaml:"claude_cmd"` // deprecated: use model.backends.claude.cmd
	IssueLabel    string         `yaml:"issue_label"`
	ExcludeLabels []string       `yaml:"exclude_labels"`
	WorkerPrompt  string         `yaml:"worker_prompt"`
	Model         ModelConfig    `yaml:"model"`
	Telegram      TelegramConfig `yaml:"telegram"`
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
		MaxParallel: 5,
		ClaudeCmd:   "claude",
		IssueLabel:  "enhancement",
	}
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	if cfg.Repo == "" {
		return nil, fmt.Errorf("config: repo is required")
	}

	// Expand ~ in paths
	cfg.LocalPath = expandHome(cfg.LocalPath)
	cfg.WorktreeBase = expandHome(cfg.WorktreeBase)
	cfg.WorkerPrompt = expandHome(cfg.WorkerPrompt)

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

	return cfg, nil
}

func expandHome(path string) string {
	if len(path) > 1 && path[:2] == "~/" {
		return filepath.Join(os.Getenv("HOME"), path[2:])
	}
	return path
}
