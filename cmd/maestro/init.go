package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// initYAMLConfig is the config structure written to maestro.yaml by init.
type initYAMLConfig struct {
	Repo          string            `yaml:"repo"`
	LocalPath     string            `yaml:"local_path"`
	WorktreeBase  string            `yaml:"worktree_base"`
	MaxParallel   int               `yaml:"max_parallel"`
	SessionPrefix string            `yaml:"session_prefix"`
	IssueLabels   []string          `yaml:"issue_labels"`
	Model         initYAMLModel     `yaml:"model"`
	Telegram      *initYAMLTelegram `yaml:"telegram,omitempty"`
}

type initYAMLModel struct {
	Default string `yaml:"default"`
}

type initYAMLTelegram struct {
	Target string `yaml:"target"`
}

func initCmd(args []string) {
	if err := runInitWizard(os.Stdin, os.Stdout, "."); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func runInitWizard(r io.Reader, w io.Writer, outDir string) error {
	yamlPath := filepath.Join(outDir, "maestro.yaml")
	if _, err := os.Stat(yamlPath); err == nil {
		return fmt.Errorf("maestro.yaml already exists in %s (remove it first to re-initialize)", outDir)
	}

	scanner := bufio.NewScanner(r)

	fmt.Fprintln(w)
	fmt.Fprintln(w, "Welcome to Maestro! Let's set up your first project.")
	fmt.Fprintln(w)

	// Check prerequisites and warn about missing ones
	checkPrerequisites(w)

	repo := promptInit(scanner, w, "GitHub repo (owner/repo)", "")
	if repo == "" {
		return fmt.Errorf("repo is required")
	}

	parts := strings.SplitN(repo, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return fmt.Errorf("repo must be in owner/repo format (e.g. BeFeast/panoptikon)")
	}
	repoName := parts[1]

	localPath := promptInit(scanner, w, "Local clone path", "~/src/"+repoName)
	worktreeBase := promptInit(scanner, w, "Worktree base dir", "~/.worktrees/"+repoName)
	maxParallelStr := promptInit(scanner, w, "Max parallel workers", "3")
	modelBackend := promptInit(scanner, w, "Default model backend (claude/codex/gemini/cline)", "claude")
	issueLabel := promptInit(scanner, w, "Issue label filter", "enhancement")

	maxParallel, err := strconv.Atoi(maxParallelStr)
	if err != nil || maxParallel < 1 {
		maxParallel = 3
	}

	telegramAnswer := promptInit(scanner, w, "Telegram notifications? (y/N)", "")
	wantsTelegram := strings.EqualFold(telegramAnswer, "y") || strings.EqualFold(telegramAnswer, "yes")

	var telegram *initYAMLTelegram
	if wantsTelegram {
		fmt.Fprintf(w, "  \u2192 Telegram target ID: ")
		if scanner.Scan() {
			if id := strings.TrimSpace(scanner.Text()); id != "" {
				telegram = &initYAMLTelegram{Target: id}
			}
		}
	}

	fmt.Fprintln(w)

	// Derive session_prefix from repo name (first 3 chars)
	sessionPrefix := repoName
	if len(sessionPrefix) > 3 {
		sessionPrefix = sessionPrefix[:3]
	}

	// Build config
	cfg := initYAMLConfig{
		Repo:          repo,
		LocalPath:     localPath,
		WorktreeBase:  worktreeBase,
		MaxParallel:   maxParallel,
		SessionPrefix: sessionPrefix,
		IssueLabels:   []string{issueLabel},
		Model:         initYAMLModel{Default: modelBackend},
		Telegram:      telegram,
	}

	yamlData, err := yaml.Marshal(&cfg)
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}

	if err := os.WriteFile(yamlPath, yamlData, 0644); err != nil {
		return fmt.Errorf("write maestro.yaml: %w", err)
	}
	fmt.Fprintln(w, "\u2705 Created maestro.yaml")

	// Create ~/.maestro/ state directory
	maestroDir := filepath.Join(os.Getenv("HOME"), ".maestro")
	if err := os.MkdirAll(maestroDir, 0755); err != nil {
		fmt.Fprintf(w, "Note: could not create state directory: %v\n", err)
	} else {
		fmt.Fprintf(w, "\u2705 Created %s\n", maestroDir)
	}

	// Create service file (non-fatal)
	binPath, _ := os.Executable()
	if binPath == "" {
		binPath = "maestro"
	}
	absConfigPath, _ := filepath.Abs(yamlPath)
	if absConfigPath == "" {
		absConfigPath = yamlPath
	}

	if err := writeInitServiceFile(w, binPath, absConfigPath); err != nil {
		fmt.Fprintf(w, "Note: could not create service file: %v\n", err)
	}

	// Next steps
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Run: maestro run --once    (test run)")
	switch runtime.GOOS {
	case "linux":
		fmt.Fprintln(w, "     systemctl --user enable --now maestro.service")
	case "darwin":
		fmt.Fprintln(w, "     launchctl load ~/Library/LaunchAgents/com.maestro.agent.plist")
	}

	return nil
}

func writeInitServiceFile(w io.Writer, binPath, configPath string) error {
	switch runtime.GOOS {
	case "linux":
		dir := filepath.Join(os.Getenv("HOME"), ".config", "systemd", "user")
		if err := os.MkdirAll(dir, 0755); err != nil {
			return err
		}
		path := filepath.Join(dir, "maestro.service")
		if err := os.WriteFile(path, []byte(systemdUnit(binPath, configPath)), 0644); err != nil {
			return err
		}
		fmt.Fprintf(w, "\u2705 Created %s\n", path)
	case "darwin":
		dir := filepath.Join(os.Getenv("HOME"), "Library", "LaunchAgents")
		if err := os.MkdirAll(dir, 0755); err != nil {
			return err
		}
		path := filepath.Join(dir, "com.maestro.agent.plist")
		if err := os.WriteFile(path, []byte(launchdPlist(binPath, configPath)), 0644); err != nil {
			return err
		}
		fmt.Fprintf(w, "\u2705 Created %s\n", path)
	}
	return nil
}

func systemdUnit(binPath, configPath string) string {
	return fmt.Sprintf(`[Unit]
Description=Maestro - AI coding agent orchestrator
After=network.target

[Service]
Type=simple
ExecStart=%s run --config %s
Restart=on-failure
RestartSec=30

[Install]
WantedBy=default.target
`, binPath, configPath)
}

func launchdPlist(binPath, configPath string) string {
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>com.maestro.agent</string>
    <key>ProgramArguments</key>
    <array>
        <string>%s</string>
        <string>run</string>
        <string>--config</string>
        <string>%s</string>
    </array>
    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    <true/>
    <key>StandardOutPath</key>
    <string>/tmp/maestro.stdout.log</string>
    <key>StandardErrorPath</key>
    <string>/tmp/maestro.stderr.log</string>
</dict>
</plist>
`, binPath, configPath)
}

func checkPrerequisites(w io.Writer) {
	type prereq struct {
		name     string
		cmd      string
		required bool
		hint     string
	}
	prereqs := []prereq{
		{"git", "git", true, "install git: https://git-scm.com"},
		{"gh", "gh", true, "install gh: https://cli.github.com"},
		{"tmux", "tmux", true, "install tmux: sudo apt install tmux (Linux) / brew install tmux (macOS)"},
	}
	aiCLIs := []prereq{
		{"claude", "claude", false, ""},
		{"codex", "codex", false, ""},
		{"gemini", "gemini", false, ""},
		{"cline", "cline", false, ""},
	}

	missing := false
	for _, p := range prereqs {
		if _, err := exec.LookPath(p.cmd); err != nil {
			fmt.Fprintf(w, "  Warning: %s not found — %s\n", p.name, p.hint)
			missing = true
		}
	}

	hasAI := false
	for _, p := range aiCLIs {
		if _, err := exec.LookPath(p.cmd); err == nil {
			hasAI = true
			break
		}
	}
	if !hasAI {
		fmt.Fprintf(w, "  Warning: no AI CLI found (claude, codex, gemini, cline) — install at least one\n")
		missing = true
	}

	// Check gh auth
	if _, err := exec.LookPath("gh"); err == nil {
		if err := exec.Command("gh", "auth", "status").Run(); err != nil {
			fmt.Fprintf(w, "  Warning: gh not authenticated — run: gh auth login\n")
			missing = true
		}
	}

	if missing {
		fmt.Fprintln(w)
	}
}

func promptInit(scanner *bufio.Scanner, w io.Writer, question, defaultVal string) string {
	if defaultVal != "" {
		fmt.Fprintf(w, "? %s (%s): ", question, defaultVal)
	} else {
		fmt.Fprintf(w, "? %s: ", question)
	}
	if !scanner.Scan() {
		return defaultVal
	}
	answer := strings.TrimSpace(scanner.Text())
	if answer == "" {
		return defaultVal
	}
	return answer
}
