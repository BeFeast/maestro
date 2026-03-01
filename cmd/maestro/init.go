package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// validBackends is the set of supported model backends for init validation.
var validBackends = []string{"claude", "codex", "gemini", "cline"}

// initConfig holds the values collected by the init wizard.
type initConfig struct {
	Repo         string
	LocalPath    string
	WorktreeBase string
	MaxParallel  int
	IssueLabel   string
	ModelBackend string
	TelegramID   string
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
	backendHint := strings.Join(validBackends, "/")
	modelBackend := promptInit(scanner, w, fmt.Sprintf("Default model backend (%s)", backendHint), "claude")

	if !isValidBackend(modelBackend) {
		return fmt.Errorf("invalid model backend %q — valid options: %s", modelBackend, strings.Join(validBackends, ", "))
	}

	issueLabel := promptInit(scanner, w, "Issue label filter", "enhancement")

	maxParallel, err := strconv.Atoi(maxParallelStr)
	if err != nil || maxParallel < 1 {
		maxParallel = 3
	}

	telegramAnswer := promptInit(scanner, w, "Telegram notifications? (y/N)", "")
	wantsTelegram := strings.EqualFold(telegramAnswer, "y") || strings.EqualFold(telegramAnswer, "yes")

	var telegramID string
	if wantsTelegram {
		fmt.Fprintf(w, "  \u2192 Telegram target ID: ")
		if scanner.Scan() {
			telegramID = strings.TrimSpace(scanner.Text())
		}
	}

	fmt.Fprintln(w)

	// Build config
	cfg := initConfig{
		Repo:         repo,
		LocalPath:    localPath,
		WorktreeBase: worktreeBase,
		MaxParallel:  maxParallel,
		IssueLabel:   issueLabel,
		ModelBackend: modelBackend,
		TelegramID:   telegramID,
	}

	yamlData := renderInitConfig(cfg)
	if err := os.WriteFile(yamlPath, []byte(yamlData), 0644); err != nil {
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

func isValidBackend(name string) bool {
	for _, b := range validBackends {
		if b == name {
			return true
		}
	}
	return false
}

// renderInitConfig produces the YAML config file content with commented-out
// examples for commonly used settings so new users can discover features.
func renderInitConfig(cfg initConfig) string {
	var b strings.Builder
	fmt.Fprintf(&b, "repo: %s\n", cfg.Repo)
	fmt.Fprintf(&b, "local_path: %s\n", cfg.LocalPath)
	fmt.Fprintf(&b, "worktree_base: %s\n", cfg.WorktreeBase)
	fmt.Fprintf(&b, "max_parallel: %d\n", cfg.MaxParallel)
	fmt.Fprintf(&b, "issue_labels:\n  - %s\n", cfg.IssueLabel)
	fmt.Fprintf(&b, "model:\n  default: %s\n", cfg.ModelBackend)

	if cfg.TelegramID != "" {
		fmt.Fprintf(&b, "telegram:\n  target: \"%s\"\n", cfg.TelegramID)
	}

	// Commented-out examples for discoverability
	b.WriteString("\n# --- Optional settings (uncomment to enable) ---\n")
	b.WriteString("# max_runtime_minutes: 120\n")
	b.WriteString("# auto_rebase: true\n")
	b.WriteString("# merge_strategy: sequential\n")
	b.WriteString("# worker_prompt: ./worker-prompt.md\n")
	b.WriteString("# exclude_labels:\n")
	b.WriteString("#   - wontfix\n")
	b.WriteString("#   - blocked\n")

	return b.String()
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
