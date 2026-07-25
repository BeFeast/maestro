package worker

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/befeast/maestro/internal/config"
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

func buildArgsFromDef(t *testing.T, backend string, def config.BackendDef) string {
	t.Helper()
	return buildArgs(t, backend, workerBackendConfig(def))
}

func TestTierArgv_ClaudeModelEffort(t *testing.T) {
	args := buildArgs(t, "claude", BackendConfig{Cmd: "claude", TierModel: "opus-4.8", TierEffort: "high"})
	if !strings.Contains(args, "--model opus-4.8") {
		t.Errorf("claude args missing tier model: %s", args)
	}
	if !strings.Contains(args, "--effort high") {
		t.Errorf("claude args missing tier effort: %s", args)
	}
}

func TestTierArgv_CodexModelEffort(t *testing.T) {
	args := buildArgs(t, "codex", BackendConfig{Cmd: "codex", TierModel: "gpt-5.5", TierEffort: "low"})
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
	// #792 P2-D: gemini takes --model but has NO --effort flag, so a tier effort
	// must be dropped rather than emitting an unsupported flag that crashes it.
	args := buildArgs(t, "gemini", BackendConfig{Cmd: "gemini", TierModel: "gemini-3-pro", TierEffort: "medium"})
	if !strings.Contains(args, "--model gemini-3-pro") {
		t.Errorf("gemini args missing tier model: %s", args)
	}
	if strings.Contains(args, "--effort") {
		t.Errorf("gemini must not emit --effort (unsupported by the gemini CLI): %s", args)
	}
}

func TestTierArgv_KimiModelAndDropsEffort(t *testing.T) {
	args := buildArgs(t, "kimi", BackendConfig{Cmd: "kimi", TierModel: "kimi-k2.5", TierEffort: "high"})
	if !strings.Contains(args, "--model kimi-k2.5") {
		t.Errorf("kimi args missing tier model: %s", args)
	}
	if strings.Contains(args, "--effort") || strings.Contains(args, "model_reasoning_effort") {
		t.Errorf("kimi must not emit an unsupported effort flag: %s", args)
	}
}

func TestTierArgv_NoOverrideWhenEmpty(t *testing.T) {
	// Without a tier override the argv is unchanged — no spurious --model/--effort.
	args := buildArgs(t, "claude", BackendConfig{Cmd: "claude"})
	if strings.Contains(args, "--model") || strings.Contains(args, "--effort") {
		t.Errorf("claude args gained model/effort flags with no override: %s", args)
	}
}

// TestTierArgv_AttributionMetadataDoesNotLeak is the #792 P1-A regression guard:
// the #513 attribution fields (Provider/Model/Effort) must NEVER reach the worker
// argv. Pre-#783 the claude/codex/gemini builders ignored them; #783 accidentally
// threaded them on every dispatch (live fleets pin opus[1m] in cmd while the
// attribution model is opus-4.8 → wrong/duplicate model). Only the distinct
// TierModel/TierEffort carriers (set solely by a real policy tier override) may
// thread, so a non-policy #513-metadata config dispatches byte-for-byte as before.
func TestTierArgv_AttributionMetadataDoesNotLeak(t *testing.T) {
	for _, backend := range []string{"claude", "codex", "gemini", "kimi"} {
		cfg := BackendConfig{Cmd: backend, Provider: "anthropic", Model: "opus-4.8", Effort: "xhigh"}
		args := buildArgs(t, backend, cfg)
		if strings.Contains(args, "--model") || strings.Contains(args, "opus-4.8") {
			t.Errorf("%s: #513 attribution model leaked into argv: %s", backend, args)
		}
		if strings.Contains(args, "--effort") || strings.Contains(args, "model_reasoning_effort") {
			t.Errorf("%s: #513 attribution effort leaked into argv: %s", backend, args)
		}
	}
}

func TestBackendEffortDefault_ClaudeEmitsEffort(t *testing.T) {
	args := buildArgsFromDef(t, "claude", config.BackendDef{Cmd: "claude", Effort: "high"})
	if !strings.Contains(args, "--effort high") {
		t.Errorf("claude backend effort default not emitted: %s", args)
	}
}

func TestBackendEffortDefault_ClaudeReplacesStalePinnedEffort(t *testing.T) {
	args := buildArgsFromDef(t, "claude", config.BackendDef{
		Cmd:       "claude --model opus --effort xhigh",
		ExtraArgs: []string{"--effort", "medium"},
		Effort:    "high",
	})
	if strings.Contains(args, "--effort xhigh") || strings.Contains(args, "--effort medium") {
		t.Errorf("stale claude effort pin survived backend effort update: %s", args)
	}
	if strings.Count(args, "--effort") != 1 || !strings.Contains(args, "--effort high") {
		t.Errorf("claude args should contain exactly backend effort high: %s", args)
	}
}

func TestBackendEffortDefault_CodexEmitsReasoningEffort(t *testing.T) {
	args := buildArgsFromDef(t, "codex", config.BackendDef{Cmd: "codex", Effort: "high"})
	if !strings.Contains(args, "model_reasoning_effort=high") {
		t.Errorf("codex backend effort default not emitted: %s", args)
	}
	if strings.Contains(args, "--effort") {
		t.Errorf("codex must not use --effort for backend effort: %s", args)
	}
}

func TestBackendEffortDefault_CodexReplacesStalePinnedEffort(t *testing.T) {
	args := buildArgsFromDef(t, "codex", config.BackendDef{
		Cmd:       "codex -c model_reasoning_effort=xhigh",
		ExtraArgs: []string{"-c", "model_reasoning_effort=medium"},
		Effort:    "high",
	})
	if strings.Contains(args, "model_reasoning_effort=xhigh") || strings.Contains(args, "model_reasoning_effort=medium") {
		t.Errorf("stale codex effort pin survived backend effort update: %s", args)
	}
	if strings.Count(args, "model_reasoning_effort=") != 1 || !strings.Contains(args, "model_reasoning_effort=high") {
		t.Errorf("codex args should contain exactly backend reasoning effort high: %s", args)
	}
}

func TestBackendEffortDefault_UnsupportedBackendDropsEffort(t *testing.T) {
	args := buildArgsFromDef(t, "gemini", config.BackendDef{Cmd: "gemini", Effort: "high"})
	if strings.Contains(args, "--effort") || strings.Contains(args, "model_reasoning_effort") {
		t.Errorf("gemini must not emit unsupported effort flag: %s", args)
	}
}

func TestTierArgv_OperatorPinnedModelWins(t *testing.T) {
	// An operator-pinned --model in cmd suppresses the tier override (no duplicate).
	args := buildArgs(t, "claude", BackendConfig{Cmd: "claude --model pinned", TierModel: "opus-4.8"})
	if strings.Count(args, "--model") != 1 {
		t.Errorf("expected exactly one --model (operator pin wins): %s", args)
	}
	if strings.Contains(args, "opus-4.8") {
		t.Errorf("tier model must not override an operator-pinned model: %s", args)
	}
}

func TestTierArgv_CodexEffortOverrideReplacesPinnedEffort(t *testing.T) {
	args := buildArgs(t, "codex", BackendConfig{
		Cmd:        "codex",
		ExtraArgs:  []string{"-c", "model_reasoning_effort=xhigh"},
		TierEffort: "low",
	})
	if strings.Contains(args, "model_reasoning_effort=xhigh") {
		t.Errorf("stale operator-pinned codex effort must not override configured effort: %s", args)
	}
	if strings.Count(args, "model_reasoning_effort=") != 1 || !strings.Contains(args, "model_reasoning_effort=low") {
		t.Errorf("configured codex effort missing or duplicated: %s", args)
	}
}

// #841: a pipeline phase's per-role effort rides the SAME TierEffort carrier as a
// routing tier's effort, so it emits per-backend exactly like the tier path —
// claude --effort, codex -c model_reasoning_effort, gemini drops it. These guard
// the per-phase effort argv emission end-to-end (pipeline.ApplyPhaseEffort /
// StartPhase set TierEffort; the builders emit it).
func TestPhaseEffort_ClaudeEmitsEffort(t *testing.T) {
	args := buildArgs(t, "claude", BackendConfig{Cmd: "claude", TierEffort: "high"})
	if !strings.Contains(args, "--effort high") {
		t.Errorf("claude phase effort not emitted: %s", args)
	}
}

func TestPhaseEffort_CodexEmitsReasoningEffort(t *testing.T) {
	args := buildArgs(t, "codex", BackendConfig{Cmd: "codex", TierEffort: "low"})
	if !strings.Contains(args, "model_reasoning_effort=low") {
		t.Errorf("codex phase effort not emitted: %s", args)
	}
	if strings.Contains(args, "--effort") {
		t.Errorf("codex must not use --effort: %s", args)
	}
}

func TestPhaseEffort_GeminiDropsEffort(t *testing.T) {
	// gemini has no reasoning-effort flag — a phase effort must be dropped, not
	// emitted as an unsupported --effort that would crash the worker.
	args := buildArgs(t, "gemini", BackendConfig{Cmd: "gemini", TierEffort: "medium"})
	if strings.Contains(args, "--effort") {
		t.Errorf("gemini must not emit --effort for a phase effort: %s", args)
	}
}
