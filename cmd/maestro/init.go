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
	Repo         string            `yaml:"repo"`
	LocalPath    string            `yaml:"local_path"`
	WorktreeBase string            `yaml:"worktree_base"`
	MaxParallel  int               `yaml:"max_parallel"`
	IssueLabels  []string          `yaml:"issue_labels"`
	Model        initYAMLModel     `yaml:"model"`
	Telegram     *initYAMLTelegram `yaml:"telegram,omitempty"`
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

	// Check prerequisites and warn about missing tools
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
		telegramTarget := promptInit(scanner, w, "Telegram target ID", "")
		if telegramTarget != "" {
			telegram = &initYAMLTelegram{Target: telegramTarget}
		}
	}

	fmt.Fprintln(w)

	// Build config
	cfg := initYAMLConfig{
		Repo:         repo,
		LocalPath:    localPath,
		WorktreeBase: worktreeBase,
		MaxParallel:  maxParallel,
		IssueLabels:  []string{issueLabel},
		Model:        initYAMLModel{Default: modelBackend},
		Telegram:     telegram,
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

// checkPrerequisites checks for required and optional tools and prints
// warnings for anything missing. It does not block the wizard — just warns.
// The lookPath parameter allows tests to inject a stub.
func checkPrerequisites(w io.Writer) {
	checkPrerequisitesWithLookPath(w, exec.LookPath)
}

func checkPrerequisitesWithLookPath(w io.Writer, lookPath func(string) (string, error)) {
	type tool struct {
		name     string
		required bool
		hint     string
	}

	required := []tool{
		{"git", true, "pre-installed on most systems"},
		{"gh", true, "install from https://cli.github.com"},
		{"tmux", true, "install: apt install tmux / brew install tmux"},
	}

	backends := []tool{
		{"claude", false, ""},
		{"codex", false, ""},
		{"gemini", false, ""},
		{"cline", false, ""},
	}

	fmt.Fprintln(w, "Checking prerequisites...")

	allOk := true
	for _, t := range required {
		if _, err := lookPath(t.name); err != nil {
			fmt.Fprintf(w, "  [!] %s not found — %s\n", t.name, t.hint)
			allOk = false
		}
	}

	hasBackend := false
	for _, t := range backends {
		if _, err := lookPath(t.name); err == nil {
			hasBackend = true
			break
		}
	}
	if !hasBackend {
		fmt.Fprintln(w, "  [!] No AI backend CLI found (claude, codex, gemini, or cline)")
		fmt.Fprintln(w, "      Install at least one before running maestro")
		allOk = false
	}

	if allOk {
		fmt.Fprintln(w, "  All prerequisites found.")
	}
	fmt.Fprintln(w)
}
