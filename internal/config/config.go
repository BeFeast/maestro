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

type Config struct {
	Repo          string         `yaml:"repo"`
	LocalPath     string         `yaml:"local_path"`
	WorktreeBase  string         `yaml:"worktree_base"`
	MaxParallel   int            `yaml:"max_parallel"`
	ClaudeCmd     string         `yaml:"claude_cmd"`
	IssueLabel    string         `yaml:"issue_label"`
	ExcludeLabels []string       `yaml:"exclude_labels"`
	WorkerPrompt  string         `yaml:"worker_prompt"`
	Telegram      TelegramConfig `yaml:"telegram"`
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

	return cfg, nil
}

func expandHome(path string) string {
	if len(path) > 1 && path[:2] == "~/" {
		return filepath.Join(os.Getenv("HOME"), path[2:])
	}
	return path
}
