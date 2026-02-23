package worker

import (
	"fmt"
	"os"
	"os/exec"
)

// BackendConfig holds the CLI command and any extra args from config.
type BackendConfig struct {
	Cmd       string   // binary name (e.g. "claude", "codex")
	ExtraArgs []string // additional args from config
}

// NewClaudeCmd builds a claude --dangerously-skip-permissions -p "$(cat promptFile)" command.
// Prompt is read from file to avoid shell quoting issues.
func NewClaudeCmd(cfg BackendConfig, promptFile, worktree string) (*exec.Cmd, error) {
	promptData, err := os.ReadFile(promptFile)
	if err != nil {
		return nil, fmt.Errorf("read prompt file: %w", err)
	}
	claudeCmd := cfg.Cmd
	if claudeCmd == "" {
		claudeCmd = "claude"
	}
	args := []string{"--dangerously-skip-permissions", "-p", string(promptData)}
	args = append(args, cfg.ExtraArgs...)
	cmd := exec.Command(claudeCmd, args...)
	cmd.Dir = worktree
	return cmd, nil
}

// NewCodexCmd builds: codex exec --dangerously-bypass-approvals-and-sandbox -C <worktree> - < promptFile
// Note: the prompt file is NOT opened here. The runner script in worker.go handles
// stdin redirection via shell `< promptFile`, so no file descriptor is held open.
func NewCodexCmd(cfg BackendConfig, promptFile, worktree string) (*exec.Cmd, error) {
	codexCmd := cfg.Cmd
	if codexCmd == "" {
		codexCmd = "codex"
	}
	args := []string{"exec", "--dangerously-bypass-approvals-and-sandbox", "-C", worktree, "-"}
	args = append(args, cfg.ExtraArgs...)
	cmd := exec.Command(codexCmd, args...)
	cmd.Dir = worktree
	// Stdin redirection is handled by the runner script — no file opened here
	return cmd, nil
}

// BuildWorkerCmd creates the right exec.Cmd based on backend name.
// Returns the command, an optional stdinFile path (for backends that read
// the prompt via stdin, e.g. codex), and any error.
func BuildWorkerCmd(backendName string, cfg BackendConfig, promptFile, worktree string) (cmd *exec.Cmd, stdinFile string, err error) {
	switch backendName {
	case "claude", "":
		cmd, err = NewClaudeCmd(cfg, promptFile, worktree)
		return cmd, "", err
	case "codex":
		cmd, err = NewCodexCmd(cfg, promptFile, worktree)
		return cmd, promptFile, err
	default:
		return nil, "", fmt.Errorf("unknown backend: %s", backendName)
	}
}
