package worker

import "testing"

// codexSmokeStream is a real 1-turn codex `exec --json` capture
// (codex-cli 0.139.0, prompt "Reply with exactly the word: pong"), with the
// thread_id redacted. The token counts are verbatim from the live run so the
// parser is exercised against the actual frame shape, not a hand-written
// guess (#738). codex reports no USD — cost is virtual (configured pricing).
const codexSmokeStream = `{"type":"thread.started","thread_id":"REDACTED"}
{"type":"turn.started"}
{"type":"item.completed","item":{"id":"item_0","type":"agent_message","text":"pong"}}
{"type":"turn.completed","usage":{"input_tokens":12087,"cached_input_tokens":4992,"output_tokens":5,"reasoning_output_tokens":0}}
`

func TestParseCodexUsage_RealSmokeCapture(t *testing.T) {
	usage, ok := ParseCodexUsage(codexSmokeStream)
	if !ok {
		t.Fatal("ParseCodexUsage returned ok=false on a real turn.completed event")
	}
	if usage.Input != 12087 {
		t.Errorf("Input = %d, want 12087", usage.Input)
	}
	if usage.Output != 5 {
		t.Errorf("Output = %d, want 5", usage.Output)
	}
	if usage.CacheRead != 4992 {
		t.Errorf("CacheRead = %d, want 4992", usage.CacheRead)
	}
	// cached_input_tokens is a subset of input_tokens (OpenAI semantics), so
	// the total is input + output only — NOT input + output + cache.
	if usage.TotalTokens != 12092 {
		t.Errorf("TotalTokens = %d, want 12092 (input+output, cache is a subset of input)", usage.TotalTokens)
	}
}

// A stream with no turn.completed event (truncated / non-codex) must report
// ok=false so the orchestrator leaves tokens at 0 and falls back to the
// legacy text parser instead of stamping garbage.
func TestParseCodexUsage_NoUsageEvent(t *testing.T) {
	const noUsage = `{"type":"thread.started","thread_id":"REDACTED"}
{"type":"turn.started"}
{"type":"item.completed","item":{"id":"item_0","type":"agent_message","text":"working"}}
not json at all
`
	if usage, ok := ParseCodexUsage(noUsage); ok {
		t.Fatalf("expected ok=false without a turn.completed event, got %+v", usage)
	}
}

// Multiple turn.completed events (the slot.jsonl after a respawn appends a
// second `codex exec` invocation's frames) must SUM to the cumulative run
// total — the property the respawn-safe token watermark relies on.
func TestParseCodexUsage_SumsAcrossTurns(t *testing.T) {
	const twoRuns = `{"type":"turn.completed","usage":{"input_tokens":700,"cached_input_tokens":100,"output_tokens":70,"reasoning_output_tokens":0}}
{"type":"turn.completed","usage":{"input_tokens":900,"cached_input_tokens":0,"output_tokens":100,"reasoning_output_tokens":10}}
`
	usage, ok := ParseCodexUsage(twoRuns)
	if !ok {
		t.Fatal("expected ok=true")
	}
	// Input 700+900=1600, Output 70+100=170, CacheRead 100+0=100.
	if usage.Input != 1600 {
		t.Errorf("Input = %d, want 1600", usage.Input)
	}
	if usage.Output != 170 {
		t.Errorf("Output = %d, want 170", usage.Output)
	}
	if usage.CacheRead != 100 {
		t.Errorf("CacheRead = %d, want 100", usage.CacheRead)
	}
	// 1600 + 170 = 1770 (cache subset excluded from the total).
	if usage.TotalTokens != 1770 {
		t.Errorf("TotalTokens = %d, want 1770 (sum of input+output across turns)", usage.TotalTokens)
	}
}
