package worker

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// splitCmd splits a command string into binary and extra arguments.
// e.g. "claude --model opus" → ("claude", ["--model", "opus"])
func splitCmd(cmd string) (binary string, prefixArgs []string) {
	parts := strings.Fields(cmd)
	if len(parts) == 0 {
		return cmd, nil
	}
	return parts[0], parts[1:]
}

// BackendConfig holds the CLI command and any extra args from config.
type BackendConfig struct {
	Cmd        string   // binary name (e.g. "claude", "codex", "gemini")
	ExtraArgs  []string // additional args from config
	PromptMode string   // how to deliver prompt: "arg", "stdin", "file"
}

// Backend builds the exec.Cmd for a specific model CLI.
type Backend interface {
	// BuildCmd creates the command to run the model CLI.
	// Returns the command, an optional stdinFile path (for backends that read
	// the prompt via stdin), and any error.
	BuildCmd(cfg BackendConfig, promptFile, worktree string) (cmd *exec.Cmd, stdinFile string, err error)
}

// knownBackends maps backend names to their implementations.
var knownBackends = map[string]Backend{
	"claude": claudeBackend{},
	"cline":  clineBackend{},
	"codex":  codexBackend{},
	"gemini": geminiBackend{},
}

// --- Claude Backend ---

type claudeBackend struct{}

func (claudeBackend) BuildCmd(cfg BackendConfig, promptFile, worktree string) (*exec.Cmd, string, error) {
	promptData, err := os.ReadFile(promptFile)
	if err != nil {
		return nil, "", fmt.Errorf("read prompt file: %w", err)
	}
	claudeCmd := cfg.Cmd
	if claudeCmd == "" {
		claudeCmd = "claude"
	}
	binary, cmdArgs := splitCmd(claudeCmd)
	args := append(cmdArgs, "--dangerously-skip-permissions", "-p", string(promptData))
	args = append(args, cfg.ExtraArgs...)
	cmd := exec.Command(binary, args...)
	cmd.Dir = worktree
	return cmd, "", nil
}

// --- Codex Backend ---

type codexBackend struct{}

func (codexBackend) BuildCmd(cfg BackendConfig, promptFile, worktree string) (*exec.Cmd, string, error) {
	codexCmd := cfg.Cmd
	if codexCmd == "" {
		codexCmd = "codex"
	}
	binary, cmdArgs := splitCmd(codexCmd)
	args := append(cmdArgs, "exec", "--dangerously-bypass-approvals-and-sandbox", "-C", worktree, "-")
	args = append(args, cfg.ExtraArgs...)
	cmd := exec.Command(binary, args...)
	cmd.Dir = worktree
	// Stdin redirection is handled by the runner script — no file opened here
	return cmd, promptFile, nil
}

// --- Gemini Backend ---

type geminiBackend struct{}

func (geminiBackend) BuildCmd(cfg BackendConfig, promptFile, worktree string) (*exec.Cmd, string, error) {
	geminiCmd := cfg.Cmd
	if geminiCmd == "" {
		geminiCmd = "gemini"
	}
	promptData, err := os.ReadFile(promptFile)
	if err != nil {
		return nil, "", fmt.Errorf("read prompt file: %w", err)
	}
	binary, cmdArgs := splitCmd(geminiCmd)
	args := append(cmdArgs, "-p", string(promptData))
	args = append(args, cfg.ExtraArgs...)
	cmd := exec.Command(binary, args...)
	cmd.Dir = worktree
	return cmd, "", nil
}

// --- Cline Backend ---

type clineBackend struct{}

func (clineBackend) BuildCmd(cfg BackendConfig, promptFile, worktree string) (*exec.Cmd, string, error) {
	promptData, err := os.ReadFile(promptFile)
	if err != nil {
		return nil, "", fmt.Errorf("read prompt file: %w", err)
	}
	clineCmd := cfg.Cmd
	if clineCmd == "" {
		clineCmd = "cline"
	}
	binary, cmdArgs := splitCmd(clineCmd)
	args := append(cmdArgs, "-y", string(promptData))
	args = append(args, cfg.ExtraArgs...)
	cmd := exec.Command(binary, args...)
	cmd.Dir = worktree
	return cmd, "", nil
}

// --- Generic Backend ---

// genericBackend handles arbitrary CLIs using the prompt_mode config field.
// Supported prompt modes:
//   - "arg" (default): pass prompt content as the last CLI argument
//   - "stdin": redirect prompt file to stdin via the runner script
//   - "file": pass prompt file path as the last CLI argument
type genericBackend struct{}

func (genericBackend) BuildCmd(cfg BackendConfig, promptFile, worktree string) (*exec.Cmd, string, error) {
	if cfg.Cmd == "" {
		return nil, "", fmt.Errorf("generic backend requires cmd to be set")
	}
	binary, cmdArgs := splitCmd(cfg.Cmd)

	mode := cfg.PromptMode
	if mode == "" {
		mode = "arg"
	}

	var args []string
	var stdinFile string

	args = append(args, cmdArgs...)
	args = append(args, cfg.ExtraArgs...)

	switch mode {
	case "arg":
		promptData, err := os.ReadFile(promptFile)
		if err != nil {
			return nil, "", fmt.Errorf("read prompt file: %w", err)
		}
		args = append(args, string(promptData))
	case "stdin":
		stdinFile = promptFile
	case "file":
		args = append(args, promptFile)
	default:
		return nil, "", fmt.Errorf("unknown prompt_mode %q (supported: arg, stdin, file)", mode)
	}

	cmd := exec.Command(binary, args...)
	cmd.Dir = worktree
	return cmd, stdinFile, nil
}

// BuildWorkerCmd creates the right exec.Cmd based on backend name.
// Known backends (claude, codex, gemini) use their specific command builders.
// Unknown backends use the generic builder with prompt_mode from config.
// Returns the command, an optional stdinFile path (for backends that read
// the prompt via stdin, e.g. codex), and any error.
func BuildWorkerCmd(backendName string, cfg BackendConfig, promptFile, worktree string) (cmd *exec.Cmd, stdinFile string, err error) {
	if backendName == "" {
		backendName = "claude"
	}

	if b, ok := knownBackends[backendName]; ok {
		return b.BuildCmd(cfg, promptFile, worktree)
	}

	// Fallback: use generic backend for unknown backends
	return (genericBackend{}).BuildCmd(cfg, promptFile, worktree)
}

// KnownBackends returns a list of built-in backend names.
func KnownBackends() []string {
	names := make([]string, 0, len(knownBackends))
	for name := range knownBackends {
		names = append(names, name)
	}
	return names
}
