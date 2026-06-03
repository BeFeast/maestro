package worker

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"

	"github.com/befeast/maestro/internal/config"
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
	Model      string   // optional model name for role-specific backend calls
	Effort     string   // optional reasoning effort for role-specific backend calls
	MCP        config.MCPConfig
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
	claudeCmd := cfg.Cmd
	if claudeCmd == "" {
		claudeCmd = "claude"
	}
	binary, cmdArgs := splitCmd(claudeCmd)
	// Deliver the prompt via stdin, not as a CLI argument. Worker prompts are
	// usually bounded but grow on retries (CI-failure context + Greptile review
	// comments are appended), so a large PR can push a single argv past the
	// Linux MAX_ARG_STRLEN limit (128 KiB) and fail fork/exec with "argument
	// list too long". `claude -p` reads the prompt from stdin when no prompt
	// argument is given; the runner script wires promptFile to stdin (mirrors
	// the codex `-` path and the supervisor claude branch from #453).
	args := append(cmdArgs, "--dangerously-skip-permissions", "-p")
	var err error
	args, err = appendClaudeMCPOptions(args, cfg.MCP)
	if err != nil {
		return nil, "", err
	}
	args = append(args, cfg.ExtraArgs...)
	cmd := exec.Command(binary, args...)
	cmd.Dir = worktree
	// Stdin redirection is handled by the runner script — no file opened here.
	return cmd, promptFile, nil
}

// --- Codex Backend ---

type codexBackend struct{}

func (codexBackend) BuildCmd(cfg BackendConfig, promptFile, worktree string) (*exec.Cmd, string, error) {
	codexCmd := cfg.Cmd
	if codexCmd == "" {
		codexCmd = "codex"
	}
	binary, cmdArgs := splitCmd(codexCmd)
	args := append(cmdArgs, "exec", "--dangerously-bypass-approvals-and-sandbox", "-C", worktree)
	var err error
	args, err = appendCodexMCPOptions(args, cfg.MCP)
	if err != nil {
		return nil, "", err
	}
	args = append(args, cfg.ExtraArgs...)
	args = append(args, "-")
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

func appendModelOptions(args []string, cfg BackendConfig) []string {
	args = append(args, cfg.ExtraArgs...)
	if strings.TrimSpace(cfg.Model) != "" {
		args = append(args, "--model", strings.TrimSpace(cfg.Model))
	}
	if strings.TrimSpace(cfg.Effort) != "" {
		args = append(args, "--effort", strings.TrimSpace(cfg.Effort))
	}
	return args
}

func appendClaudeMCPOptions(args []string, mcp config.MCPConfig) ([]string, error) {
	configs := append([]string{}, mcp.Configs...)
	if len(mcp.Servers) > 0 {
		inline, err := claudeMCPConfigJSON(mcp.Servers)
		if err != nil {
			return nil, err
		}
		configs = append(configs, inline)
	}
	if len(configs) > 0 {
		args = append(args, "--mcp-config")
		args = append(args, configs...)
		if mcp.Strict {
			args = append(args, "--strict-mcp-config")
		}
	}
	return args, nil
}

func claudeMCPConfigJSON(servers map[string]config.MCPServerDef) (string, error) {
	out := map[string]map[string]map[string]interface{}{"mcpServers": {}}
	for _, name := range sortedServerNames(servers) {
		server := servers[name]
		if strings.TrimSpace(server.Command) != "" && strings.TrimSpace(server.URL) != "" {
			return "", fmt.Errorf("mcp server %q: configure command or url, not both", name)
		}
		entry := make(map[string]interface{})
		if url := strings.TrimSpace(server.URL); url != "" {
			entry["type"] = "http"
			entry["url"] = url
			if len(server.Headers) > 0 {
				entry["headers"] = server.Headers
			}
		} else if command := strings.TrimSpace(server.Command); command != "" {
			entry["command"] = command
			if len(server.Args) > 0 {
				entry["args"] = server.Args
			}
			if len(server.Env) > 0 {
				entry["env"] = server.Env
			}
		} else {
			return "", fmt.Errorf("mcp server %q: command or url is required", name)
		}
		out["mcpServers"][name] = entry
	}
	data, err := json.Marshal(out)
	if err != nil {
		return "", fmt.Errorf("marshal claude mcp config: %w", err)
	}
	return string(data), nil
}

func appendCodexMCPOptions(args []string, mcp config.MCPConfig) ([]string, error) {
	if len(mcp.Configs) > 0 {
		return nil, fmt.Errorf("codex MCP config paths/JSON are not supported; configure model.backends.<name>.mcp.servers instead")
	}
	for _, name := range sortedServerNames(mcp.Servers) {
		server := mcp.Servers[name]
		if strings.TrimSpace(server.Command) != "" && strings.TrimSpace(server.URL) != "" {
			return nil, fmt.Errorf("mcp server %q: configure command or url, not both", name)
		}
		prefix := "mcp_servers." + tomlKey(name) + "."
		if command := strings.TrimSpace(server.Command); command != "" {
			args = append(args, "-c", prefix+"command="+tomlString(command))
			if len(server.Args) > 0 {
				args = append(args, "-c", prefix+"args="+tomlStringArray(server.Args))
			}
			if len(server.Env) > 0 {
				args = append(args, "-c", prefix+"env="+tomlStringMap(server.Env))
			}
		} else if url := strings.TrimSpace(server.URL); url != "" {
			args = append(args, "-c", prefix+"url="+tomlString(url))
			if bearer := strings.TrimSpace(server.BearerTokenEnvVar); bearer != "" {
				args = append(args, "-c", prefix+"bearer_token_env_var="+tomlString(bearer))
			}
		} else {
			return nil, fmt.Errorf("mcp server %q: command or url is required", name)
		}
		if len(server.AllowedTools) > 0 {
			args = append(args, "-c", prefix+"allowed_tools="+tomlStringArray(server.AllowedTools))
		}
		if server.StartupTimeoutMs > 0 {
			args = append(args, "-c", prefix+"startup_timeout_ms="+strconv.Itoa(server.StartupTimeoutMs))
		}
		if server.ToolTimeoutMs > 0 {
			args = append(args, "-c", prefix+"tool_timeout_ms="+strconv.Itoa(server.ToolTimeoutMs))
		}
		if trust := strings.TrimSpace(server.Trust); trust != "" {
			args = append(args, "-c", prefix+"trust="+tomlString(trust))
		}
	}
	return args, nil
}

func sortedServerNames(servers map[string]config.MCPServerDef) []string {
	names := make([]string, 0, len(servers))
	for name := range servers {
		name = strings.TrimSpace(name)
		if name != "" {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

func tomlKey(key string) string {
	return strconv.Quote(strings.TrimSpace(key))
}

func tomlString(value string) string {
	return strconv.Quote(value)
}

func tomlStringArray(values []string) string {
	parts := make([]string, 0, len(values))
	for _, value := range values {
		parts = append(parts, tomlString(value))
	}
	return "[" + strings.Join(parts, ",") + "]"
}

func tomlStringMap(values map[string]string) string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, tomlKey(key)+"="+tomlString(values[key]))
	}
	return "{" + strings.Join(parts, ",") + "}"
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

// BuildSupervisorCmd creates a read-only model command for supervisor decisions.
// It reuses backend prompt delivery semantics but intentionally avoids worker-only
// permission bypass flags.
func BuildSupervisorCmd(backendName string, cfg BackendConfig, promptFile, worktree string) (cmd *exec.Cmd, stdinFile string, err error) {
	if backendName == "" {
		backendName = "claude"
	}

	switch backendName {
	case "claude":
		claudeCmd := cfg.Cmd
		if claudeCmd == "" {
			claudeCmd = "claude"
		}
		binary, cmdArgs := splitCmd(claudeCmd)
		// Deliver the prompt via stdin, not as a CLI argument. Supervisor
		// prompts embed live PR/issue/review context and routinely exceed the
		// Linux MAX_ARG_STRLEN single-argument limit (128 KiB), which fails
		// fork/exec with "argument list too long". `claude -p` reads the prompt
		// from stdin when no prompt argument is given (mirrors the codex `-` path).
		args := append(cmdArgs, "-p")
		args = appendModelOptions(args, cfg)
		cmd := exec.Command(binary, args...)
		cmd.Dir = worktree
		return cmd, promptFile, nil
	case "codex":
		codexCmd := cfg.Cmd
		if codexCmd == "" {
			codexCmd = "codex"
		}
		binary, cmdArgs := splitCmd(codexCmd)
		args := append(cmdArgs, "exec", "-C", worktree, "-")
		args = appendModelOptions(args, cfg)
		cmd := exec.Command(binary, args...)
		cmd.Dir = worktree
		return cmd, promptFile, nil
	case "gemini":
		promptData, err := os.ReadFile(promptFile)
		if err != nil {
			return nil, "", fmt.Errorf("read prompt file: %w", err)
		}
		geminiCmd := cfg.Cmd
		if geminiCmd == "" {
			geminiCmd = "gemini"
		}
		binary, cmdArgs := splitCmd(geminiCmd)
		// NOTE: latent E2BIG ceiling. The gemini CLI takes its prompt via the
		// `-p` argument, so a prompt approaching the Linux MAX_ARG_STRLEN limit
		// (128 KiB) would fail fork/exec. Supervisor prompts are read-only and
		// bounded, and the gemini CLI has no stable stdin-prompt contract to
		// mirror the codex/claude `-`/`-p`-with-stdin paths, so this is left on
		// argv delivery. Revisit if gemini gains stdin prompt support.
		args := append(cmdArgs, "-p", string(promptData))
		args = appendModelOptions(args, cfg)
		cmd := exec.Command(binary, args...)
		cmd.Dir = worktree
		return cmd, "", nil
	case "cline":
		promptData, err := os.ReadFile(promptFile)
		if err != nil {
			return nil, "", fmt.Errorf("read prompt file: %w", err)
		}
		clineCmd := cfg.Cmd
		if clineCmd == "" {
			clineCmd = "cline"
		}
		binary, cmdArgs := splitCmd(clineCmd)
		// NOTE: latent E2BIG ceiling, same as the gemini case above. cline takes
		// the task as a positional argument after `-y` with no stable
		// stdin-prompt contract, so it is left on argv delivery. Supervisor
		// prompts are read-only and bounded; revisit if cline gains stdin support.
		args := append(cmdArgs, "-y", string(promptData))
		args = appendModelOptions(args, cfg)
		cmd := exec.Command(binary, args...)
		cmd.Dir = worktree
		return cmd, "", nil
	default:
		return buildGenericSupervisorCmd(cfg, promptFile, worktree)
	}
}

func buildGenericSupervisorCmd(cfg BackendConfig, promptFile, worktree string) (*exec.Cmd, string, error) {
	if cfg.Cmd == "" {
		return nil, "", fmt.Errorf("generic backend requires cmd to be set")
	}
	binary, cmdArgs := splitCmd(cfg.Cmd)
	mode := cfg.PromptMode
	if mode == "" {
		mode = "arg"
	}
	args := appendModelOptions(append([]string(nil), cmdArgs...), cfg)
	stdinFile := ""
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

// KnownBackends returns a list of built-in backend names.
func KnownBackends() []string {
	names := make([]string, 0, len(knownBackends))
	for name := range knownBackends {
		names = append(names, name)
	}
	return names
}
