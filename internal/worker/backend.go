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
func NewClaudeCmd(cfg BackendConfig, promptFile, worktree string) *exec.Cmd {
	promptData, _ := os.ReadFile(promptFile)
	claudeCmd := cfg.Cmd
	if claudeCmd == "" {
		claudeCmd = "claude"
	}
	args := []string{"--dangerously-skip-permissions", "-p", string(promptData)}
	args = append(args, cfg.ExtraArgs...)
	cmd := exec.Command(claudeCmd, args...)
	cmd.Dir = worktree
	return cmd
}

// NewCodexCmd builds: codex exec --dangerously-bypass-approvals-and-sandbox -C <worktree> - < promptFile
func NewCodexCmd(cfg BackendConfig, promptFile, worktree string) *exec.Cmd {
	codexCmd := cfg.Cmd
	if codexCmd == "" {
		codexCmd = "codex"
	}
	args := []string{"exec", "--dangerously-bypass-approvals-and-sandbox", "-C", worktree, "-"}
	args = append(args, cfg.ExtraArgs...)
	cmd := exec.Command(codexCmd, args...)
	cmd.Dir = worktree
	// Prompt via stdin
	f, err := os.Open(promptFile)
	if err == nil {
		cmd.Stdin = f
	}
	return cmd
}

// BuildWorkerCmd creates the right exec.Cmd based on backend name.
func BuildWorkerCmd(backendName string, cfg BackendConfig, promptFile, worktree string) (*exec.Cmd, error) {
	switch backendName {
	case "claude", "":
		return NewClaudeCmd(cfg, promptFile, worktree), nil
	case "codex":
		return NewCodexCmd(cfg, promptFile, worktree), nil
	default:
		return nil, fmt.Errorf("unknown backend: %s", backendName)
	}
}
