package worker

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// #783: a routing tier's effort/model override must be threaded into the worker
// argv for claude/codex/gemini (until #783 only Pi read cfg.Model).

func buildArgs(t *testing.T, backend string, cfg BackendConfig) string {
	t.Helper()
	dir := t.TempDir()
	promptFile := filepath.Join(dir, "prompt.md")
	if err := os.WriteFile(promptFile, []byte("do the thing"), 0644); err != nil {
		t.Fatal(err)
	}
	cmd, _, err := BuildWorkerCmd(backend, cfg, promptFile, dir)
	if err != nil {
		t.Fatalf("BuildWorkerCmd(%s): %v", backend, err)
	}
	return strings.Join(cmd.Args, " ")
}

func TestTierArgv_ClaudeModelEffort(t *testing.T) {
	args := buildArgs(t, "claude", BackendConfig{Cmd: "claude", Model: "opus-4.8", Effort: "high"})
	if !strings.Contains(args, "--model opus-4.8") {
		t.Errorf("claude args missing tier model: %s", args)
	}
	if !strings.Contains(args, "--effort high") {
		t.Errorf("claude args missing tier effort: %s", args)
	}
}

func TestTierArgv_CodexModelEffort(t *testing.T) {
	args := buildArgs(t, "codex", BackendConfig{Cmd: "codex", Model: "gpt-5.5", Effort: "low"})
	if !strings.Contains(args, "--model gpt-5.5") {
		t.Errorf("codex args missing tier model: %s", args)
	}
	// codex takes reasoning effort as a -c override, not --effort.
	if !strings.Contains(args, "model_reasoning_effort=low") {
		t.Errorf("codex args missing model_reasoning_effort: %s", args)
	}
	if strings.Contains(args, "--effort") {
		t.Errorf("codex must not use --effort: %s", args)
	}
}

func TestTierArgv_GeminiModelEffort(t *testing.T) {
	args := buildArgs(t, "gemini", BackendConfig{Cmd: "gemini", Model: "gemini-3-pro", Effort: "medium"})
	if !strings.Contains(args, "--model gemini-3-pro") {
		t.Errorf("gemini args missing tier model: %s", args)
	}
	if !strings.Contains(args, "--effort medium") {
		t.Errorf("gemini args missing tier effort: %s", args)
	}
}

func TestTierArgv_NoOverrideWhenEmpty(t *testing.T) {
	// Without a tier override the argv is unchanged — no spurious --model/--effort.
	args := buildArgs(t, "claude", BackendConfig{Cmd: "claude"})
	if strings.Contains(args, "--model") || strings.Contains(args, "--effort") {
		t.Errorf("claude args gained model/effort flags with no override: %s", args)
	}
}

func TestTierArgv_OperatorPinnedModelWins(t *testing.T) {
	// An operator-pinned --model in cmd suppresses the tier override (no duplicate).
	args := buildArgs(t, "claude", BackendConfig{Cmd: "claude --model pinned", Model: "opus-4.8"})
	if strings.Count(args, "--model") != 1 {
		t.Errorf("expected exactly one --model (operator pin wins): %s", args)
	}
	if strings.Contains(args, "opus-4.8") {
		t.Errorf("tier model must not override an operator-pinned model: %s", args)
	}
}

func TestTierArgv_OperatorPinnedCodexEffortWins(t *testing.T) {
	args := buildArgs(t, "codex", BackendConfig{
		Cmd:       "codex",
		ExtraArgs: []string{"-c", "model_reasoning_effort=xhigh"},
		Effort:    "low",
	})
	if strings.Contains(args, "model_reasoning_effort=low") {
		t.Errorf("tier effort must not override operator-pinned codex effort: %s", args)
	}
	if !strings.Contains(args, "model_reasoning_effort=xhigh") {
		t.Errorf("operator-pinned codex effort missing: %s", args)
	}
}
