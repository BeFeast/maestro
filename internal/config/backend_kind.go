package config

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
)

// Backend kinds name the CLI-specific exec path a backend resolves to.
// Workers use the kind (not the raw backend name) to pick the command
// builder, so a known CLI registered under a custom backend key keeps its
// CLI-specific behaviour: permission-bypass flags and stdin prompt
// delivery. #684.
const (
	BackendKindClaude   = "claude"
	BackendKindCodex    = "codex"
	BackendKindGemini   = "gemini"
	BackendKindCline    = "cline"
	BackendKindKimi     = "kimi"
	BackendKindPi       = "pi"
	BackendKindOpencode = "opencode"
	BackendKindGeneric  = "generic"
)

// providerBackendKinds maps the per-backend provider attribution field
// (#513) to the CLI family it implies. Providers without a first-class
// CLI integration (e.g. "groq") are intentionally absent and resolve via
// the cmd-basename heuristic or the generic path.
var providerBackendKinds = map[string]string{
	"anthropic": BackendKindClaude,
	"claude":    BackendKindClaude,
	"openai":    BackendKindCodex,
	"codex":     BackendKindCodex,
	"google":    BackendKindGemini,
	"gemini":    BackendKindGemini,
	"cline":     BackendKindCline,
	"kimi":      BackendKindKimi,
	"moonshot":  BackendKindKimi,
	"pi":        BackendKindPi,
	"ollama":    BackendKindPi,
	"opencode":  BackendKindOpencode,
}

// ResolveBackendKind decides which CLI-specific exec path a backend uses.
// Resolution order (#684):
//  1. the backend name itself — claude/codex/gemini/cline/kimi keys keep their
//     existing behaviour,
//  2. the per-backend provider field (anthropic → claude, openai → codex, …),
//  3. the binary basename of cmd's first token (claude/codex/gemini/cline/kimi),
//  4. the generic fallback (prompt_mode applies, no permission-bypass flag).
//
// Before #684, dispatch was keyed on the name alone: the Anthropic CLI
// registered as `fable:` silently fell through to the generic path, lost
// --dangerously-skip-permissions and stdin prompt delivery, and every
// mutating tool call was denied by the CLI's permission layer (sup-175).
func ResolveBackendKind(name, provider, cmd string) string {
	switch name {
	case BackendKindClaude, BackendKindCodex, BackendKindGemini, BackendKindCline, BackendKindKimi, BackendKindPi, BackendKindOpencode:
		return name
	}
	if kind, ok := providerBackendKinds[strings.ToLower(strings.TrimSpace(provider))]; ok {
		return kind
	}
	if kind := backendKindFromCmd(cmd); kind != "" {
		return kind
	}
	return BackendKindGeneric
}

// backendKindFromCmd matches the binary basename of cmd's first token
// against the known CLIs. Returns "" when there is no match.
func backendKindFromCmd(cmd string) string {
	fields := strings.Fields(cmd)
	if len(fields) == 0 {
		return ""
	}
	switch base := filepath.Base(fields[0]); base {
	case BackendKindClaude, BackendKindCodex, BackendKindGemini, BackendKindCline, BackendKindKimi, BackendKindPi, BackendKindOpencode:
		return base
	}
	return ""
}

// backendResolutionWarnings surfaces backends whose exec path is unlikely
// to be what the operator intended (#684):
//
//   - a backend that resolves to the generic path loses CLI permission
//     bypass and (unless prompt_mode=stdin) argv prompt delivery risks
//     E2BIG on large prompts — loud warning naming the backend;
//   - a backend whose resolved kind disagrees with the cmd binary
//     (e.g. provider: openai with a claude binary) will pass one CLI's
//     flags to another CLI.
//
// Non-agentic backends are skipped: they are text-completion helpers that
// never run as workers (config.parse rejects them from the worker chain),
// so the generic path is their expected shape.
func (c *Config) backendResolutionWarnings() []string {
	var warnings []string
	for _, name := range sortedBackendNames(c.Model.Backends) {
		def := c.Model.Backends[name]
		if def.Enabled != nil && !*def.Enabled {
			continue
		}
		if def.NonAgentic {
			continue
		}
		kind := ResolveBackendKind(name, def.Provider, def.Cmd)
		if kind == BackendKindGeneric {
			warnings = append(warnings, fmt.Sprintf(
				"config: backend %q resolves to the generic exec path (provider %q and cmd %q match no known CLI) — workers run without a permission-bypass flag (mutating tool calls may be denied) and the prompt is delivered per prompt_mode=%q (argv delivery risks E2BIG on large prompts). If this wraps a known CLI, set provider: anthropic|openai|google|cline|moonshot|kimi|pi|ollama or use a claude/codex/gemini/cline/kimi/pi binary in cmd.",
				name, def.Provider, def.Cmd, coalescePromptMode(def.PromptMode)))
			continue
		}
		if cmdKind := backendKindFromCmd(def.Cmd); cmdKind != "" && cmdKind != kind {
			warnings = append(warnings, fmt.Sprintf(
				"config: backend %q resolves to the %s exec path but cmd %q runs the %s binary — %s-specific flags will be passed to %s. Align the provider field with the cmd binary or rename the backend.",
				name, kind, def.Cmd, cmdKind, kind, cmdKind))
		}
	}
	return warnings
}

func sortedBackendNames(backends map[string]BackendDef) []string {
	names := make([]string, 0, len(backends))
	for name := range backends {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func coalescePromptMode(mode string) string {
	trimmed := strings.ToLower(strings.TrimSpace(mode))
	if trimmed == "" {
		return "arg"
	}
	return trimmed
}
