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
	return argsHaveFlag(args, "--output-format")
}

// argsHaveFlag reports whether args already contain the given flag, either as
// a bare token (`--json`) or in the `--json=value` form. Used so an
// operator-pinned flag in cmd/extra_args overrides a backend default instead
// of producing a duplicate (e.g. the codex #738 `--json` opt-in).
func argsHaveFlag(args []string, flag string) bool {
	for _, a := range args {
		if a == flag || strings.HasPrefix(a, flag+"=") {
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
	Model      string   // optional model name for role-specific backend calls (#513 attribution metadata)
	Effort     string   // optional reasoning effort for role-specific backend calls (#513 attribution metadata)
	// BackendEffort is the backend row's default reasoning-effort policy (#900).
	// TierModel/TierEffort carry a routing tier or pipeline phase override
	// (#783/#841), DISTINCT from the #513 attribution Model/Effort above.
	BackendEffort string
	TierModel     string
	TierEffort    string
	// UsageStream opts a structured-stream worker into emitting NDJSON usage
	// frames on a side-channel slot.jsonl: claude `--output-format stream-json
	// --verbose` (#737) and codex `exec --json` (#738). Off by default;
	// ignored by backends without a structured-stream mode.
	UsageStream bool
	// TokenBudget is the active per-attempt hard ceiling. A positive value
	// requires an enforceable live-usage mode; BuildWorkerCmd fails closed for
	// backend/output combinations that only reveal usage after process exit.
	TokenBudget int
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
	"claude":   claudeBackend{},
	"cline":    clineBackend{},
	"codex":    codexBackend{},
	"gemini":   geminiBackend{},
	"kimi":     kimiBackend{},
	"opencode": opencodeBackend{},
	"pi":       piBackend{},
}

// --- Claude Backend ---

type claudeBackend struct{}

func (claudeBackend) BuildCmd(cfg BackendConfig, promptFile, worktree string) (*exec.Cmd, string, error) {
	claudeCmd := cfg.Cmd
	if claudeCmd == "" {
		claudeCmd = "claude"
	}
	binary, cmdArgs := splitCmd(claudeCmd)
	cmdArgs, cfg.ExtraArgs = stripConfiguredEffortPins(cmdArgs, cfg.ExtraArgs, config.BackendKindClaude, cfg)
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
	// #783/#900: thread the effective model/effort policy into argv.
	args = appendTierModelEffort(args, pinnedArgs(cmdArgs, cfg), config.BackendKindClaude, cfg)
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
	cmdArgs, cfg.ExtraArgs = stripConfiguredEffortPins(cmdArgs, cfg.ExtraArgs, config.BackendKindCodex, cfg)
	args := append(cmdArgs, "exec", "--dangerously-bypass-approvals-and-sandbox", "-C", worktree)
	// #738: opt-in structured streaming so usage (tokens) can be parsed from
	// the NDJSON side-channel. `codex exec --json` emits JSONL events incl. a
	// terminal turn.completed usage block {input, cached_input, output}.
	// Skipped when the operator already pins --json via cmd or extra_args, so
	// their choice (and the matching runner wiring) wins — no duplicate flag.
	if cfg.UsageStream && !argsHaveFlag(cmdArgs, "--json") && !argsHaveFlag(cfg.ExtraArgs, "--json") {
		args = append(args, "--json")
	}
	var err error
	args, err = appendCodexMCPOptions(args, cfg.MCP)
	if err != nil {
		return nil, "", err
	}
	// #783/#900: thread the effective model/effort policy into argv. codex takes
	// the reasoning effort as `-c model_reasoning_effort=<e>`.
	args = appendTierModelEffort(args, pinnedArgs(cmdArgs, cfg), config.BackendKindCodex, cfg)
	args = append(args, cfg.ExtraArgs...)
	if cfg.TokenBudget > 0 {
		// codex exec's public JSONL reports usage only at turn completion. The
		// native rollout budget is enforced inside the agent loop after every
		// provider response, including sub-agent work, so it is the enforceable
		// live proxy for the configured Maestro ceiling. Codex 0.144 changed
		// rollout_budget from a boolean feature to a configured feature object:
		// `--enable rollout_budget` now replaces that object and discards its
		// limit. Set the object's enabled field directly and provide the required
		// reminder thresholds instead.
		reminders := make([]string, 0, 2)
		for _, divisor := range []int{5, 10} {
			threshold := cfg.TokenBudget / divisor
			if threshold > 0 && threshold < cfg.TokenBudget {
				reminders = append(reminders, strconv.Itoa(threshold))
			}
		}
		args = append(args,
			"-c", "features.rollout_budget.enabled=true",
			"-c", fmt.Sprintf("features.rollout_budget.limit_tokens=%d", cfg.TokenBudget),
			"-c", fmt.Sprintf("features.rollout_budget.reminder_at_remaining_tokens=[%s]", strings.Join(reminders, ",")),
			"-c", "features.rollout_budget.sampling_token_weight=1.0",
			"-c", "features.rollout_budget.prefill_token_weight=1.0",
		)
	}
	args = append(args, "-")
	cmd := exec.Command(binary, args...)
	cmd.Dir = worktree
	// Stdin redirection is handled by the runner script — no file opened here
	return cmd, promptFile, nil
}

// --- Kimi Backend ---

// kimiBackend runs Kimi Code CLI in non-interactive print mode. Print mode
// implicitly enables Kimi's AFK/auto-approval behavior; with no -p/--prompt
// value, a non-TTY stdin is read as the prompt. Keeping the prompt out of argv
// avoids the Linux MAX_ARG_STRLEN ceiling for retry-expanded worker prompts.
//
// Kimi's stream-json contract is the first-class output mode here rather than
// an opt-in. The stream splitter preserves the raw JSONL for usage accounting
// and renders assistant/tool messages into the human-readable worker log.
type kimiBackend struct{}

func (kimiBackend) BuildCmd(cfg BackendConfig, promptFile, worktree string) (*exec.Cmd, string, error) {
	kimiCmd := cfg.Cmd
	if kimiCmd == "" {
		kimiCmd = "kimi"
	}
	binary, cmdArgs := splitCmd(kimiCmd)
	pinned := pinnedArgs(cmdArgs, cfg)
	args := append([]string(nil), cmdArgs...)
	if !argsHaveFlag(pinned, "--print") {
		args = append(args, "--print")
	}
	if !argsHaveOutputFormat(pinned) {
		args = append(args, "--output-format=stream-json")
	}
	args = appendTierModelEffort(args, pinned, config.BackendKindKimi, cfg)
	args = append(args, cfg.ExtraArgs...)
	cmd := exec.Command(binary, args...)
	cmd.Dir = worktree
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
	// #783: thread a routing tier's model/effort override into argv (skipped
	// when the operator pinned them in cmd/extra_args).
	args = appendTierModelEffort(args, pinnedArgs(cmdArgs, cfg), config.BackendKindGemini, cfg)
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

// --- OpenCode Backend ---

// opencodeBackend runs the OpenCode coding agent headless in JSON event-stream
// mode so Maestro can capture provider usage (model/tokens/cost) the way it
// does for the first-class claude/codex backends.
//
// OpenCode is invoked as: opencode run --auto --format json
// [--model <model>] [extra_args...]
//
// --auto auto-approves permissions that are not explicitly denied, rounding up to
// the equivalent of claude --dangerously-skip-permissions. run reads the prompt
// from stdin when no positional message is given; the runner script wires
// promptFile to stdin, keeping large worker prompts under the Linux
// MAX_ARG_STRLEN single-argument limit.
//
// --format json emits an NDJSON event stream (step_start / text / step_finish)
// to the worker log. The stream-splitter forwards raw JSON frames to the
// slot.jsonl side-channel and renders human-readable text to slot.log, and the
// orchestrator's opencode usage parser aggregates model/tokens/cost from the
// final step_finish event in the JSONL.
type opencodeBackend struct{}

func (opencodeBackend) BuildCmd(cfg BackendConfig, promptFile, worktree string) (*exec.Cmd, string, error) {
	opencodeCmd := cfg.Cmd
	if opencodeCmd == "" {
		opencodeCmd = "opencode"
	}
	binary, cmdArgs := splitCmd(opencodeCmd)
	args := append(cmdArgs, "run", "--auto", "--format", "json")
	if m := strings.TrimSpace(cfg.Model); m != "" {
		args = append(args, "--model", m)
	}
	args = appendTierModelEffort(args, pinnedArgs(cmdArgs, cfg), config.BackendKindOpencode, cfg)
	args = append(args, cfg.ExtraArgs...)
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

// argsHaveCodexEffort reports whether codex's reasoning-effort knob is already
// present via `-c model_reasoning_effort=...` in cmd/extra_args.
func argsHaveCodexEffort(args []string) bool {
	for _, a := range args {
		if strings.Contains(a, "model_reasoning_effort") {
			return true
		}
	}
	return false
}

func configuredReasoningEffort(cfg BackendConfig) string {
	if effort := strings.TrimSpace(cfg.TierEffort); effort != "" {
		return effort
	}
	return strings.TrimSpace(cfg.BackendEffort)
}

func stripConfiguredEffortPins(cmdArgs, extraArgs []string, kind string, cfg BackendConfig) ([]string, []string) {
	if configuredReasoningEffort(cfg) == "" {
		return cmdArgs, extraArgs
	}
	switch kind {
	case config.BackendKindClaude:
		return stripFlagWithOptionalValue(cmdArgs, "--effort"), stripFlagWithOptionalValue(extraArgs, "--effort")
	case config.BackendKindCodex:
		return stripCodexEffortArgs(cmdArgs), stripCodexEffortArgs(extraArgs)
	default:
		return cmdArgs, extraArgs
	}
}

func stripFlagWithOptionalValue(args []string, flag string) []string {
	out := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == flag {
			if i+1 < len(args) {
				i++
			}
			continue
		}
		if strings.HasPrefix(a, flag+"=") {
			continue
		}
		out = append(out, a)
	}
	return out
}

func stripCodexEffortArgs(args []string) []string {
	out := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "-c" && i+1 < len(args) && strings.Contains(args[i+1], "model_reasoning_effort") {
			i++
			continue
		}
		if strings.Contains(a, "model_reasoning_effort") {
			continue
		}
		out = append(out, a)
	}
	return out
}

// appendTierModelEffort threads a routing tier's optional per-tier model/effort
// override (#783, RFC §2.4 step 5) into a first-class agentic backend's worker
// argv. It reads cfg.TierModel/cfg.TierEffort — the DISTINCT tier-override
// carriers — NOT the #513 attribution cfg.Model/cfg.Effort, so a non-policy
// config (which only sets the attribution fields) dispatches byte-for-byte as
// before (#792 P1-A). The tier-override fields are only populated by the
// orchestrator's applyTierOverride for a real policy-resolved tier.
//
// The flag spelling is backend-specific: codex takes the reasoning effort as
// `-c model_reasoning_effort=<e>` and claude takes `--effort <e>`; all three
// take `--model <m>`. gemini has no reasoning-effort flag (RFC §2.4 step 5 only
// names codex/claude), so an effort is dropped for it rather than emitting
// an unsupported `--effort` that would crash the worker (#792 P2-D). An operator
// who already pinned the model in cmd or extra_args wins. Configured effort is
// canonicalized before this helper for Claude/Codex so stale cmd/extra_args
// effort pins do not silently override a backend-row update (#900). pinned is
// the union of the backend's cmd-prefix args and extra_args.
func appendTierModelEffort(args, pinned []string, kind string, cfg BackendConfig) []string {
	if model := strings.TrimSpace(cfg.TierModel); model != "" && !argsHaveFlag(pinned, "--model") {
		args = append(args, "--model", model)
	}
	effort := configuredReasoningEffort(cfg)
	if effort == "" {
		return args
	}
	switch kind {
	case config.BackendKindCodex:
		if !argsHaveCodexEffort(pinned) {
			args = append(args, "-c", "model_reasoning_effort="+effort)
		}
	case config.BackendKindClaude:
		if !argsHaveFlag(pinned, "--effort") {
			args = append(args, "--effort", effort)
		}
	case config.BackendKindOpencode:
		if !argsHaveFlag(pinned, "--variant") {
			args = append(args, "--variant", effort)
		}
	default:
		// gemini and any other agentic backend routed here have no reasoning-effort
		// CLI flag — silently drop the tier effort (the tier model still applies).
	}
	return args
}

// workerBackendConfig builds the worker BackendConfig from a resolved backend
// def. TierModel/TierEffort carry a routing tier's override DISTINCTLY from the
// #513 attribution Model/Effort, but backend Effort now also acts as the stored
// backend's default reasoning-effort policy (#900). A tier/phase override still
// rides TierEffort and wins for that single dispatch; otherwise the backend's
// configured Effort is emitted through the backend-correct flag spelling.
func workerBackendConfig(def config.BackendDef) BackendConfig {
	return BackendConfig{
		Cmd:           def.Cmd,
		ExtraArgs:     def.ExtraArgs,
		PromptMode:    def.PromptMode,
		Provider:      def.Provider,
		Model:         def.Model,
		Effort:        def.Effort,
		BackendEffort: def.Effort,
		TierModel:     def.TierModel,
		TierEffort:    def.TierEffort,
		UsageStream:   def.UsageStream,
		MCP:           def.MCP,
	}
}

// pinnedArgs returns the union of a backend's cmd-prefix args and extra_args as
// a fresh slice, used to detect operator-pinned flags so a tier override does
// not duplicate them.
func pinnedArgs(cmdArgs []string, cfg BackendConfig) []string {
	pinned := make([]string, 0, len(cmdArgs)+len(cfg.ExtraArgs))
	pinned = append(pinned, cmdArgs...)
	pinned = append(pinned, cfg.ExtraArgs...)
	return pinned
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
// or nil when the plain `tee` pipeline should be used. Claude/codex/opencode
// require usage_stream; Kimi is structured by default, while Pi is structured
// when its live budget needs the side channel. When the maestro binary cannot
// be resolved this degrades to nil (plain tee). The resolved kind is passed to
// the splitter so it renders the right human-readable form into slot.log.
func streamSplitForBackend(backendName string, cfg BackendConfig, logFile string, workerGeneration uint64) *streamSplit {
	kind := resolveBackendKind(backendName, cfg)
	if kind == config.BackendKindKimi {
		if !kimiUsesStreamJSON(cfg) {
			return nil
		}
	} else if !cfg.UsageStream && !(cfg.TokenBudget > 0 && kind == config.BackendKindPi) {
		return nil
	}
	if kind != config.BackendKindClaude && kind != config.BackendKindCodex && kind != config.BackendKindKimi && kind != config.BackendKindOpencode && kind != config.BackendKindPi {
		return nil
	}
	bin, ok := maestroExecutablePath()
	if !ok {
		return nil
	}
	return &streamSplit{
		MaestroBin: bin,
		Backend:    kind,
		JSONLPath:  JSONLPathForLog(logFile),
		MaxTokens:  cfg.TokenBudget,
		MarkerPath: TokenBudgetMarkerPathForLog(logFile),
		Generation: workerGeneration,
	}
}

// kimiUsesStreamJSON reports whether the effective Kimi output format is the
// structured default. An operator can explicitly pin a different format in cmd
// or extra_args; in that case the command still runs, but no JSON side channel
// is installed and usage remains unavailable.
func kimiUsesStreamJSON(cfg BackendConfig) bool {
	_, cmdArgs := splitCmd(cfg.Cmd)
	format, pinned := argsFlagValue(pinnedArgs(cmdArgs, cfg), "--output-format")
	return !pinned || strings.EqualFold(strings.TrimSpace(format), "stream-json")
}

func argsFlagValue(args []string, flag string) (string, bool) {
	var value string
	found := false
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == flag {
			found = true
			if i+1 < len(args) {
				value = args[i+1]
				i++
			}
			continue
		}
		if strings.HasPrefix(arg, flag+"=") {
			found = true
			value = strings.TrimPrefix(arg, flag+"=")
		}
	}
	return value, found
}

// BuildWorkerCmd creates the right exec.Cmd for a backend. The backend name,
// provider field, and cmd binary resolve to a CLI-specific builder (claude,
// codex, gemini, kimi, cline — see resolveBackendKind); anything else uses the
// generic builder with prompt_mode from config.
// Returns the command, an optional stdinFile path (for backends that read
// the prompt via stdin, e.g. claude/codex), and any error.
func BuildWorkerCmd(backendName string, cfg BackendConfig, promptFile, worktree string) (cmd *exec.Cmd, stdinFile string, err error) {
	if err := validateLiveTokenBudget(backendName, cfg); err != nil {
		return nil, "", err
	}
	if b, ok := knownBackends[resolveBackendKind(backendName, cfg)]; ok {
		return b.BuildCmd(cfg, promptFile, worktree)
	}

	// Fallback: use generic backend for unknown backends
	return (genericBackend{}).BuildCmd(cfg, promptFile, worktree)
}

func validateLiveTokenBudget(backendName string, cfg BackendConfig) error {
	if cfg.TokenBudget <= 0 {
		return nil
	}
	kind := resolveBackendKind(backendName, cfg)
	_, cmdArgs := splitCmd(cfg.Cmd)
	switch kind {
	case config.BackendKindClaude:
		if !cfg.UsageStream || argsHaveOutputFormat(cmdArgs) || argsHaveOutputFormat(cfg.ExtraArgs) {
			return fmt.Errorf("worker_max_tokens=%d requires claude usage_stream with Maestro-managed stream-json output", cfg.TokenBudget)
		}
	case config.BackendKindCodex:
		if !cfg.UsageStream {
			return fmt.Errorf("worker_max_tokens=%d requires codex usage_stream so the native rollout budget outcome is observable", cfg.TokenBudget)
		}
		if argsHaveFlag(cmdArgs, "--ephemeral") || argsHaveFlag(cfg.ExtraArgs, "--ephemeral") {
			return fmt.Errorf("worker_max_tokens=%d requires persisted Codex live token telemetry; --ephemeral is not enforceable", cfg.TokenBudget)
		}
	case config.BackendKindPi:
		if argsHaveFlag(cfg.ExtraArgs, "--mode") {
			return fmt.Errorf("worker_max_tokens=%d requires Pi's Maestro-managed JSON mode", cfg.TokenBudget)
		}
	case config.BackendKindOpencode:
		if !cfg.UsageStream || argsHaveFlag(cfg.ExtraArgs, "--format") {
			return fmt.Errorf("worker_max_tokens=%d requires OpenCode usage_stream with Maestro-managed JSON output", cfg.TokenBudget)
		}
	case config.BackendKindKimi:
		return fmt.Errorf("worker_max_tokens=%d cannot be enforced live for Kimi: stream-json does not guarantee response-by-response usage; disable the budget or select a backend with live token telemetry", cfg.TokenBudget)
	default:
		return fmt.Errorf("worker_max_tokens=%d cannot be enforced live for backend %q; disable the budget or use claude, codex, pi, or opencode structured usage", cfg.TokenBudget, backendName)
	}
	return nil
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
	case "kimi":
		kimiCmd := cfg.Cmd
		if kimiCmd == "" {
			kimiCmd = "kimi"
		}
		binary, cmdArgs := splitCmd(kimiCmd)
		pinned := pinnedArgs(cmdArgs, cfg)
		args := append([]string(nil), cmdArgs...)
		if !argsHaveFlag(pinned, "--print") {
			args = append(args, "--print")
		}
		if !argsHaveFlag(pinned, "--plan") {
			args = append(args, "--plan")
		}
		if !argsHaveFlag(pinned, "--final-message-only") {
			args = append(args, "--final-message-only")
		}
		if model := strings.TrimSpace(cfg.Model); model != "" && !argsHaveFlag(pinned, "--model") {
			args = append(args, "--model", model)
		}
		args = append(args, cfg.ExtraArgs...)
		cmd := exec.Command(binary, args...)
		cmd.Dir = worktree
		return cmd, promptFile, nil
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
	case "opencode":
		opencodeCmd := cfg.Cmd
		if opencodeCmd == "" {
			opencodeCmd = "opencode"
		}
		binary, cmdArgs := splitCmd(opencodeCmd)
		// Supervisor prompts are read-only decisions; no --auto bypass needed.
		// run reads the prompt from stdin when no positional message is given,
		// mirroring claude -p / codex - and keeping large supervisor prompts
		// under the Linux MAX_ARG_STRLEN single-argument limit.
		args := append(cmdArgs, "run", "--format", "json")
		if m := strings.TrimSpace(cfg.Model); m != "" {
			args = append(args, "--model", m)
		}
		args = append(args, cfg.ExtraArgs...)
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
