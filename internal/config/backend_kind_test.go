package config

import (
	"strings"
	"testing"
)

// #684: exec-path resolution order — known name, then provider field, then
// cmd binary basename, then generic.
func TestResolveBackendKind(t *testing.T) {
	tests := []struct {
		name     string
		provider string
		cmd      string
		want     string
	}{
		// 1. Known backend names keep their behaviour regardless of other fields.
		{"claude", "", "", BackendKindClaude},
		{"codex", "", "", BackendKindCodex},
		{"gemini", "", "gemini-cli", BackendKindGemini},
		{"cline", "", "", BackendKindCline},
		{"kimi", "", "", BackendKindKimi},
		// 2. Provider field resolves custom names (sup-175 `fable:` shape).
		{"fable", "anthropic", "claude --model claude-fable-5 --effort xhigh", BackendKindClaude},
		{"fast", "openai", "codex --profile fast", BackendKindCodex},
		{"flash", "google", "gemini", BackendKindGemini},
		{"wrapped", "cline", "cline", BackendKindCline},
		{"k2", "moonshot", "kimi", BackendKindKimi},
		{"k3", "kimi", "my-kimi-wrapper", BackendKindKimi},
		// #730: provider pi / ollama resolves to the first-class pi backend.
		{"pi-ollama", "ollama", "pi", BackendKindPi},
		{"picustom", "pi", "my-pi-shim", BackendKindPi},
		// Provider matching is case/whitespace-insensitive.
		{"fable", " Anthropic ", "", BackendKindClaude},
		// Provider wins over a conflicting cmd binary.
		{"odd", "openai", "claude", BackendKindCodex},
		// 3. cmd binary basename is the second heuristic.
		{"mymodel", "", "/usr/local/bin/claude --model opus", BackendKindClaude},
		{"mymodel", "", "codex --flag", BackendKindCodex},
		{"mymodel", "groq", "gemini", BackendKindGemini},
		{"moonshot-model", "", "/usr/local/bin/kimi --verbose", BackendKindKimi},
		{"picli", "", "/usr/local/bin/pi", BackendKindPi},
		// 4. Everything else is generic.
		{"helper", "groq", "groq-cli", BackendKindGeneric},
		{"custom", "", "my-cli --verbose", BackendKindGeneric},
		{"empty", "", "", BackendKindGeneric},
		// Basename matching is exact — gemini-cli under a custom name is not gemini.
		{"custom-gemini", "", "gemini-cli", BackendKindGeneric},
	}
	for _, tt := range tests {
		if got := ResolveBackendKind(tt.name, tt.provider, tt.cmd); got != tt.want {
			t.Errorf("ResolveBackendKind(%q, %q, %q) = %q, want %q", tt.name, tt.provider, tt.cmd, got, tt.want)
		}
	}
}

// #684: a backend that resolves to the generic exec path must warn loudly at
// startup, naming the backend and what it loses.
func TestConfig_Warnings_GenericPathBackendWarns(t *testing.T) {
	cfg := &Config{
		Model: ModelConfig{
			Default: "mystery",
			Backends: map[string]BackendDef{
				"mystery": {Cmd: "my-cli --verbose"},
			},
		},
	}
	warnings := cfg.Warnings()
	found := false
	for _, msg := range warnings {
		if strings.Contains(msg, `"mystery"`) && strings.Contains(msg, "generic exec path") && strings.Contains(msg, "permission-bypass") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("Warnings() = %v, want a generic exec path warning naming the backend and the lost permission bypass", warnings)
	}
}

// #684: a custom-named backend that resolves via provider (or cmd basename)
// keeps the CLI-specific path — no generic-path warning.
func TestConfig_Warnings_CustomBackendProviderResolvedNoWarning(t *testing.T) {
	cfg := &Config{
		Model: ModelConfig{
			Default: "fable",
			Backends: map[string]BackendDef{
				"fable":   {Provider: "anthropic", Cmd: "claude --model claude-fable-5 --effort xhigh"},
				"mymodel": {Cmd: "/usr/local/bin/codex --profile fast"},
			},
		},
	}
	for _, msg := range cfg.Warnings() {
		if strings.Contains(msg, "generic exec path") {
			t.Fatalf("Warnings() = %v, provider/basename-resolved backends should not trigger the generic path warning", cfg.Warnings())
		}
	}
}

// #730: a custom-named pi backend (provider: ollama) keeps the first-class
// pi exec path — no generic-path warning.
func TestConfig_Warnings_PiBackendNoGenericWarning(t *testing.T) {
	cfg := &Config{
		Model: ModelConfig{
			Default: "pi-ollama",
			Backends: map[string]BackendDef{
				"pi-ollama": {Provider: "ollama", Cmd: "pi", Model: "glm-5.2:cloud"},
			},
		},
	}
	for _, msg := range cfg.Warnings() {
		if strings.Contains(msg, "generic exec path") {
			t.Fatalf("Warnings() = %v, pi backend should not trigger the generic path warning", cfg.Warnings())
		}
	}
}

func TestConfig_Warnings_KimiBackendNoGenericWarning(t *testing.T) {
	cfg := &Config{
		Model: ModelConfig{
			Default: "kimi-k2",
			Backends: map[string]BackendDef{
				"kimi-k2": {Provider: "moonshot", Cmd: "kimi"},
			},
		},
	}
	for _, msg := range cfg.Warnings() {
		if strings.Contains(msg, "generic exec path") {
			t.Fatalf("Warnings() = %v, Kimi backend should not trigger the generic path warning", cfg.Warnings())
		}
	}
}

// Known backend names never trigger the generic-path warning.
func TestConfig_Warnings_KnownBackendNamesNoGenericWarning(t *testing.T) {
	cfg := &Config{
		Model: ModelConfig{
			Default: "claude",
			Backends: map[string]BackendDef{
				"claude": {Cmd: "claude"},
			},
		},
	}
	for _, msg := range cfg.Warnings() {
		if strings.Contains(msg, "generic exec path") {
			t.Fatalf("Warnings() = %v, known backend names should not trigger the generic path warning", cfg.Warnings())
		}
	}
}

// Non-agentic text-completion helpers never run as workers — the generic
// path is their expected shape, so they must stay silent.
func TestConfig_Warnings_NonAgenticGenericNoWarning(t *testing.T) {
	cfg := &Config{
		Model: ModelConfig{
			Default: "claude",
			Backends: map[string]BackendDef{
				"claude": {Cmd: "claude"},
				"helper": {Cmd: "groq-cli", Provider: "groq", NonAgentic: true},
			},
		},
	}
	for _, msg := range cfg.Warnings() {
		if strings.Contains(msg, "generic exec path") {
			t.Fatalf("Warnings() = %v, non-agentic backends should not trigger the generic path warning", cfg.Warnings())
		}
	}
}

// Disabled backends are not dispatched — no warning.
func TestConfig_Warnings_DisabledGenericBackendNoWarning(t *testing.T) {
	disabled := false
	cfg := &Config{
		Model: ModelConfig{
			Default: "claude",
			Backends: map[string]BackendDef{
				"claude": {Cmd: "claude"},
				"old":    {Cmd: "my-cli", Enabled: &disabled},
			},
		},
	}
	for _, msg := range cfg.Warnings() {
		if strings.Contains(msg, "generic exec path") {
			t.Fatalf("Warnings() = %v, disabled backends should not trigger the generic path warning", cfg.Warnings())
		}
	}
}

// #684: a resolved kind that disagrees with the cmd binary (provider: openai
// driving a claude binary) gets one CLI's flags passed to another — warn.
func TestConfig_Warnings_ProviderCmdMismatchWarns(t *testing.T) {
	cfg := &Config{
		Model: ModelConfig{
			Default: "odd",
			Backends: map[string]BackendDef{
				"odd": {Provider: "openai", Cmd: "claude --model opus"},
			},
		},
	}
	warnings := cfg.Warnings()
	found := false
	for _, msg := range warnings {
		if strings.Contains(msg, `"odd"`) && strings.Contains(msg, "codex exec path") && strings.Contains(msg, "claude binary") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("Warnings() = %v, want a provider/cmd mismatch warning", warnings)
	}
}
