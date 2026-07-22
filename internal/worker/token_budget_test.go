package worker

import (
	"errors"
	"os"
	"strconv"
	"strings"
	"testing"
)

func TestRunStreamSplitWithBudget_StopsBeforeRunawayCompletion(t *testing.T) {
	dir := t.TempDir()
	jsonl := dir + "/slot.jsonl"
	markerPath := dir + "/slot.token-budget.json"
	stream := claudeAssistantUsageFrame(40_000) + "\n" +
		claudeAssistantUsageFrame(45_000) + "\n" +
		claudeAssistantUsageFrame(92_038) + "\n"
	stopped := 0
	err := RunStreamSplitWithBudget("claude", jsonl, 80_000, markerPath, strings.NewReader(stream), &strings.Builder{}, func() {
		stopped++
	})
	if !errors.Is(err, ErrTokenBudgetExceeded) {
		t.Fatalf("RunStreamSplitWithBudget error = %v, want %v", err, ErrTokenBudgetExceeded)
	}
	if stopped != 1 {
		t.Fatalf("stop calls = %d, want 1", stopped)
	}
	marker, ok := ReadTokenBudgetMarker(dir + "/slot.log")
	if !ok {
		t.Fatal("budget marker not readable")
	}
	if marker.TokensObserved != 85_000 || marker.MaxTokens != 80_000 {
		t.Fatalf("marker = %+v, want observed=85000 max=80000", marker)
	}
	if marker.Measure != TokenBudgetMeasureUncached {
		t.Fatalf("marker measure = %q, want %q", marker.Measure, TokenBudgetMeasureUncached)
	}
	data, err := os.ReadFile(jsonl)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "92038") {
		t.Fatal("177k runaway tail was consumed after the 80k budget stop")
	}
}

func TestLiveTokenMonitor_ClaudeIgnoresCacheReadAndDeduplicatesMessageFrames(t *testing.T) {
	monitor := newLiveTokenMonitor("claude", 160_000)
	// The same assistant message is emitted once for thinking and again for its
	// text block. The live finance-tracker failure carried 123,476 cached tokens
	// but only 77,030 uncached tokens; neither the cache replay nor the duplicate
	// frame may exhaust the worker budget.
	frame := `{"type":"assistant","message":{"id":"msg-fin-26","role":"assistant","usage":{"input_tokens":10,"output_tokens":30,"cache_creation_input_tokens":76990,"cache_read_input_tokens":123476}}}`
	observed, exceeded := monitor.observe(frame)
	if exceeded || observed != 77_030 {
		t.Fatalf("first frame = %d,%t, want 77030,false", observed, exceeded)
	}
	observed, exceeded = monitor.observe(frame)
	if exceeded || observed != 77_030 {
		t.Fatalf("duplicate frame = %d,%t, want 77030,false", observed, exceeded)
	}
}

func TestLiveTokenMonitor_ClaudeMissingUsageDoesNotKillHealthyWorker(t *testing.T) {
	monitor := newLiveTokenMonitor("claude", 1)
	for _, frame := range []string{
		`{"type":"assistant","message":{"id":"msg-1","role":"assistant","usage":{"input_tokens":0,"output_tokens":0}}}`,
		`{"type":"result","subtype":"success","result":"done"}`,
	} {
		observed, exceeded := monitor.observe(frame)
		if observed != 0 || exceeded {
			t.Fatalf("missing usage produced budget progress/kill: observed=%d exceeded=%t", observed, exceeded)
		}
	}
}

func TestLiveTokenMonitor_PiIgnoresCacheRead(t *testing.T) {
	monitor := newLiveTokenMonitor("pi", 100_000)
	line := `{"type":"turn_end","message":{"role":"assistant","usage":{"input":10,"output":20,"cacheRead":150000,"cacheWrite":30000}}}`
	observed, exceeded := monitor.observe(line)
	if exceeded || observed != 30_030 {
		t.Fatalf("observe = %d,%t, want 30030,false", observed, exceeded)
	}
}

func TestLiveTokenMonitor_BackendEvents(t *testing.T) {
	tests := []struct {
		name    string
		backend string
		line    string
		measure string
	}{
		{"claude assistant", "claude", claudeAssistantUsageFrame(100), TokenBudgetMeasureUncached},
		{"pi turn", "pi", `{"type":"turn_end","message":{"role":"assistant","usage":{"input":60,"output":40}}}`, TokenBudgetMeasureUncached},
		{"opencode step", "opencode", `{"type":"step_finish","part":{"type":"step-finish","tokens":{"input":60,"output":30,"reasoning":10}}}`, TokenBudgetMeasureInputOutputReasoning},
		{"codex native budget", "codex", `{"type":"turn.failed","error":{"message":"shared rollout token budget exhausted"}}`, TokenBudgetMeasureCodexRollout},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			monitor := newLiveTokenMonitor(tt.backend, 100)
			observed, exceeded := monitor.observe(tt.line)
			if !exceeded || observed != 100 {
				t.Fatalf("observe = %d,%t, want 100,true", observed, exceeded)
			}
			if monitor.measure != tt.measure {
				t.Fatalf("measure = %q, want %q", monitor.measure, tt.measure)
			}
		})
	}
}

func TestLiveTokenMonitor_CodexReadsCumulativeRolloutUsage(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	threadID := "019abcde-1234-7000-8000-000000000906"
	dir := home + "/sessions/2026/07/15"
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	rollout := `{"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":83000,"cached_input_tokens":70000,"output_tokens":2000,"reasoning_output_tokens":1000,"total_tokens":85000}}}}` + "\n"
	if err := os.WriteFile(dir+"/rollout-test-"+threadID+".jsonl", []byte(rollout), 0o644); err != nil {
		t.Fatal(err)
	}

	monitor := newLiveTokenMonitor("codex", 80_000)
	monitor.observe(`{"type":"thread.started","thread_id":"` + threadID + `"}`)
	observed, exceeded := monitor.observe(`{"type":"item.started","item":{"type":"command_execution"}}`)
	if !exceeded || observed != 85_000 {
		t.Fatalf("observe = %d,%t, want 85000,true", observed, exceeded)
	}
}

func TestBuildWorkerCmd_TokenBudgetFailClosedAndCodexProxy(t *testing.T) {
	t.Run("claude requires structured usage", func(t *testing.T) {
		_, _, err := BuildWorkerCmd("claude", BackendConfig{Cmd: "claude", TokenBudget: 80_000}, "/tmp/prompt", "/tmp")
		if err == nil || !strings.Contains(err.Error(), "requires claude usage_stream") {
			t.Fatalf("error = %v, want fail-closed usage_stream error", err)
		}
	})

	t.Run("unsupported backend fails closed", func(t *testing.T) {
		_, _, err := BuildWorkerCmd("custom", BackendConfig{Cmd: "custom", TokenBudget: 80_000}, "/tmp/prompt", "/tmp")
		if err == nil || !strings.Contains(err.Error(), "cannot be enforced live") {
			t.Fatalf("error = %v, want unsupported live-budget error", err)
		}
	})

	t.Run("codex receives native rollout budget", func(t *testing.T) {
		cmd, _, err := BuildWorkerCmd("codex", BackendConfig{Cmd: "codex", UsageStream: true, TokenBudget: 80_000}, "/tmp/prompt", "/tmp")
		if err != nil {
			t.Fatal(err)
		}
		joined := strings.Join(cmd.Args, " ")
		for _, want := range []string{"--json", "features.rollout_budget.enabled=true", "features.rollout_budget.limit_tokens=80000", "features.rollout_budget.reminder_at_remaining_tokens=[16000,8000]", "sampling_token_weight=1.0", "prefill_token_weight=1.0"} {
			if !strings.Contains(joined, want) {
				t.Errorf("codex args missing %q: %s", want, joined)
			}
		}
		if strings.Contains(joined, "--enable rollout_budget") {
			t.Errorf("codex args use boolean rollout_budget form that discards configured limits: %s", joined)
		}
	})

	t.Run("codex tiny budget emits valid required reminder list", func(t *testing.T) {
		cmd, _, err := BuildWorkerCmd("codex", BackendConfig{Cmd: "codex", UsageStream: true, TokenBudget: 1}, "/tmp/prompt", "/tmp")
		if err != nil {
			t.Fatal(err)
		}
		joined := strings.Join(cmd.Args, " ")
		if !strings.Contains(joined, "features.rollout_budget.reminder_at_remaining_tokens=[]") {
			t.Fatalf("codex args missing valid empty reminder list: %s", joined)
		}
	})

	t.Run("codex ephemeral mode fails closed", func(t *testing.T) {
		_, _, err := BuildWorkerCmd("codex", BackendConfig{Cmd: "codex", UsageStream: true, TokenBudget: 80_000, ExtraArgs: []string{"--ephemeral"}}, "/tmp/prompt", "/tmp")
		if err == nil || !strings.Contains(err.Error(), "persisted Codex live token telemetry") {
			t.Fatalf("error = %v, want ephemeral fail-closed error", err)
		}
	})
}

func claudeAssistantUsageFrame(total int) string {
	return `{"type":"assistant","message":{"role":"assistant","usage":{"input_tokens":` +
		strconv.Itoa(total) + `,"output_tokens":0,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}}`
}
