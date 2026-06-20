package config

import (
	"strings"
	"testing"
)

// #706: model.backends.<name>.subagent_hint carries the per-backend sub-agent
// model policy injected into the worker prompt. Backends without the field
// leave it empty so the prompt is unchanged.
func TestParse_BackendSubagentHint(t *testing.T) {
	yaml := `
repo: owner/repo
model:
  default: claude
  backends:
    claude:
      cmd: claude
      subagent_hint: "Use cheaper sub-agent models for grunt work."
    codex:
      cmd: codex
`
	cfg, err := parse([]byte(yaml))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got := cfg.Model.Backends["claude"].SubagentHint; got != "Use cheaper sub-agent models for grunt work." {
		t.Fatalf("claude subagent_hint = %q, want the configured value", got)
	}
	if got := cfg.Model.Backends["codex"].SubagentHint; got != "" {
		t.Fatalf("codex has no subagent_hint; want empty, got %q", got)
	}
}

func TestDefaultSubagentHintIsActionable(t *testing.T) {
	if strings.TrimSpace(DefaultSubagentHint) == "" {
		t.Fatal("DefaultSubagentHint must be non-empty")
	}
	for _, want := range []string{"sub-agent", "cheaper"} {
		if !strings.Contains(strings.ToLower(DefaultSubagentHint), want) {
			t.Fatalf("DefaultSubagentHint should mention %q: %q", want, DefaultSubagentHint)
		}
	}
}
