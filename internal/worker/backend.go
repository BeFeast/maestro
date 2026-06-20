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

// argsHaveOutputFormat reports whether the given args already pin an
// --output-format flag (either `--output-format json` or `--output-format=json`).
// Used so an operator-specified output format in cmd/extra_args overrides the
// #737 stream-json default instead of producing a duplicate flag.
func argsHaveOutputFormat(args []string) bool {
	for _, a := range args {
		if a == "--output-format" || strings.HasPrefix(a, "--output-format=") {
			return true
		}
	}
	return false
}

// BackendConfig holds the CLI command and any extra args from config.
type BackendConfig struct {
	Cmd        string   // binary name (e.g. "claude", "codex", "gemini")
	ExtraArgs  []string // additional args from config
	PromptMode string   // how to deliver prompt: "arg", "stdin", "file"
	Provider   string   // per-backend provider field; resolves the exec path for custom-named backends (#684)
	Model      string   // optional model name for role-specific backend calls
	Effort     string   // optional reasoning effort for role-specific backend calls
	// UsageStream opts a claude-kind worker into `--output-format stream-json
	// --verbose` so its NDJSON usage stream can be captured on a side-channel
	// slot.jsonl (#737). Off by default; ignored by non-claude backends.
	UsageStream bool
	MCP         config.MCPConfig
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
	"pi":     piBackend{},
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
	// #737: opt-in structured streaming so usage (tokens/cost) can be parsed
	// from the NDJSON side-channel. stream-json with -p requires --verbose.
	// Skipped when the operator already pins an --output-format via cmd or
	// extra_args, so their choice (and the matching runner wiring) wins.
	if cfg.UsageStream && !argsHaveOutputFormat(cmdArgs) && !argsHaveOutputFormat(cfg.ExtraArgs) {
		args = append(args, "--output-format", "stream-json", "--verbose")
	}
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

// --- Pi Backend ---

// piWorkerTools is the default built-in tool allowlist for an autonomous
// Pi worker run. read/bash/edit/write are on by default in Pi; grep/find/ls
// are off by default — enable them so the worker can search the worktree.
// Operators can override the allowlist per-backend via extra_args (a
// trailing --tools wins) or narrow it there for locked-down runs. #730.
const piWorkerTools = "read,bash,edit,write,grep,find,ls"

// piBackend runs the Pi coding agent (badlogic/pi-mono) headless in JSON
// event-stream mode so Maestro can capture provider usage (model/tokens/
// cost) the way it does for the first-class claude/codex backends. #730.
//
// Pi is invoked as: pi --mode json --no-session --provider <provider>
// --model <model> --tools <allowlist> [extra_args...] -p
//
// Print mode (-p) reads the prompt from stdin when no prompt argument is
// given, mirroring claude -p / codex - and keeping large worker prompts
// (retries append CI-failure + review context) under the Linux
// MAX_ARG_STRLEN single-argument limit (128 KiB). The runner script wires
// promptFile to stdin. The newline-delimited JSON event stream is written
// to the worker log, where the orchestrator's Pi usage parser aggregates
// model/tokens/cost from turn_end / agent_end events.
type piBackend struct{}

func (piBackend) BuildCmd(cfg BackendConfig, promptFile, worktree string) (*exec.Cmd, string, error) {
	piCmd := cfg.Cmd
	if piCmd == "" {
		piCmd = "pi"
	}
	binary, cmdArgs := splitCmd(piCmd)
	args := append([]string(nil), cmdArgs...)
	args = append(args, "--mode", "json", "--no-session")
	if p := strings.TrimSpace(cfg.Provider); p != "" {
		args = append(args, "--provider", p)
	}
	if m := strings.TrimSpace(cfg.Model); m != "" {
		args = append(args, "--model", m)
	}
	args = append(args, "--tools", piWorkerTools)
	args = append(args, cfg.ExtraArgs...)
	// -p with no prompt argument reads the prompt from stdin; keep it last
	// so extra_args flags stay flags and never shadow the stdin prompt.
	args = append(args, "-p")
	cmd := exec.Command(binary, args...)
	cmd.Dir = worktree
	// Stdin redirection is handled by the runner script — no file opened here.
	return cmd, promptFile, nil
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

// resolveBackendKind maps a backend name + config to the CLI-specific exec
// path (#684). Custom-named backends resolve by the per-backend provider
// field, then by the binary basename of cmd, so a known CLI registered under
// a custom key (e.g. `fable: {provider: anthropic, cmd: "claude --model …"}`)
// keeps its CLI-specific behaviour — permission-bypass flags and stdin prompt
// delivery — instead of silently degrading to the generic path.
func resolveBackendKind(backendName string, cfg BackendConfig) string {
	if backendName == "" {
		backendName = "claude"
	}
	return config.ResolveBackendKind(backendName, cfg.Provider, cfg.Cmd)
}

// maestroExecutablePath resolves the running maestro binary, which provides
// the `stream-split` subcommand the worker runner pipes a claude stream-json
// stream through (#737). Returns ok=false when the path cannot be resolved,
// signalling the caller to degrade to a plain `tee` pipeline (log preserved,
// usage capture disabled).
func maestroExecutablePath() (string, bool) {
	exe, err := os.Executable()
	if err != nil || strings.TrimSpace(exe) == "" {
		return "", false
	}
	return exe, true
}

// streamSplitForBackend returns the worker runner's stream-split configuration,
// or nil when the plain `tee` pipeline should be used. Only a claude-kind
// backend with usage_stream opted in streams NDJSON; when the maestro binary
// cannot be resolved it degrades to nil (plain tee) per #737.
func streamSplitForBackend(backendName string, cfg BackendConfig, logFile string) *streamSplit {
	if !cfg.UsageStream {
		return nil
	}
	if resolveBackendKind(backendName, cfg) != config.BackendKindClaude {
		return nil
	}
	bin, ok := maestroExecutablePath()
	if !ok {
		return nil
	}
	return &streamSplit{
		MaestroBin: bin,
		Backend:    config.BackendKindClaude,
		JSONLPath:  JSONLPathForLog(logFile),
	}
}

// BuildWorkerCmd creates the right exec.Cmd for a backend. The backend name,
// provider field, and cmd binary resolve to a CLI-specific builder (claude,
// codex, gemini, cline — see resolveBackendKind); anything else uses the
// generic builder with prompt_mode from config.
// Returns the command, an optional stdinFile path (for backends that read
// the prompt via stdin, e.g. claude/codex), and any error.
func BuildWorkerCmd(backendName string, cfg BackendConfig, promptFile, worktree string) (cmd *exec.Cmd, stdinFile string, err error) {
	if b, ok := knownBackends[resolveBackendKind(backendName, cfg)]; ok {
		return b.BuildCmd(cfg, promptFile, worktree)
	}

	// Fallback: use generic backend for unknown backends
	return (genericBackend{}).BuildCmd(cfg, promptFile, worktree)
}

// BuildSupervisorCmd creates a read-only model command for supervisor decisions.
// It reuses backend prompt delivery semantics but intentionally avoids worker-only
// permission bypass flags. Like BuildWorkerCmd, the exec path is resolved from
// the backend name, provider field, and cmd binary (#684) so custom-named
// backends keep stdin prompt delivery.
func BuildSupervisorCmd(backendName string, cfg BackendConfig, promptFile, worktree string) (cmd *exec.Cmd, stdinFile string, err error) {
	switch resolveBackendKind(backendName, cfg) {
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
	case "pi":
		piCmd := cfg.Cmd
		if piCmd == "" {
			piCmd = "pi"
		}
		binary, cmdArgs := splitCmd(piCmd)
		args := append([]string(nil), cmdArgs...)
		args = append(args, "--mode", "json", "--no-session")
		if p := strings.TrimSpace(cfg.Provider); p != "" {
			args = append(args, "--provider", p)
		}
		// Supervisor prompts are read-only; run Pi with no mutating tools so a
		// decision prompt cannot accidentally touch the worktree. -p reads
		// the prompt from stdin (no prompt argument), keeping supervisor
		// prompts that embed live PR/issue/review context under the Linux
		// MAX_ARG_STRLEN single-argument limit. Pi uses --thinking (not
		// --effort) for reasoning level, so cfg.Effort is intentionally not
		// mapped here; operators can pass --thinking via extra_args.
		args = append(args, "--no-tools")
		args = append(args, cfg.ExtraArgs...)
		if m := strings.TrimSpace(cfg.Model); m != "" {
			args = append(args, "--model", m)
		}
		args = append(args, "-p")
		cmd := exec.Command(binary, args...)
		cmd.Dir = worktree
		return cmd, promptFile, nil
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
