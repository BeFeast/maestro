package worker

import "testing"

// claudeSmokeStream is a real 1-turn claude `--output-format stream-json
// --verbose` capture (claude 2.1.183, prompt "Reply with exactly the word:
// pong"), with session_id/uuid values redacted. The token counts and
// total_cost_usd are verbatim from the live run so the parser is exercised
// against the actual frame shape, not a hand-written guess (#737).
const claudeSmokeStream = `{"type":"system","subtype":"init","cwd":"/tmp","session_id":"REDACTED","model":"claude-opus-4-8[1m]","permissionMode":"bypassPermissions","tools":[],"uuid":"REDACTED"}
{"type":"rate_limit_event","rate_limit_info":{"status":"allowed"},"session_id":"REDACTED","uuid":"REDACTED"}
{"type":"assistant","message":{"id":"msg_REDACTED","type":"message","role":"assistant","model":"claude-opus-4-8","content":[{"type":"text","text":"pong"}],"stop_reason":"end_turn","usage":{"input_tokens":2487,"cache_creation_input_tokens":1924,"cache_read_input_tokens":15661,"output_tokens":1}},"session_id":"REDACTED","uuid":"REDACTED"}
{"type":"result","subtype":"success","is_error":false,"duration_ms":5000,"num_turns":1,"result":"pong","session_id":"REDACTED","total_cost_usd":0.0401785,"usage":{"input_tokens":2487,"cache_creation_input_tokens":1924,"cache_read_input_tokens":15661,"output_tokens":4,"server_tool_use":{"web_search_requests":0}},"uuid":"REDACTED"}
`

func TestParseClaudeUsage_RealSmokeCapture(t *testing.T) {
	usage, ok := ParseClaudeUsage(claudeSmokeStream)
	if !ok {
		t.Fatal("ParseClaudeUsage returned ok=false on a real result frame")
	}
	if usage.Input != 2487 {
		t.Errorf("Input = %d, want 2487", usage.Input)
	}
	if usage.Output != 4 {
		t.Errorf("Output = %d, want 4", usage.Output)
	}
	if usage.CacheRead != 15661 {
		t.Errorf("CacheRead = %d, want 15661", usage.CacheRead)
	}
	if usage.CacheWrite != 1924 {
		t.Errorf("CacheWrite = %d, want 1924", usage.CacheWrite)
	}
	// 2487 + 4 + 15661 + 1924 = 20076
	if usage.TotalTokens != 20076 {
		t.Errorf("TotalTokens = %d, want 20076", usage.TotalTokens)
	}
	if usage.CostUSD != 0.0401785 {
		t.Errorf("CostUSD = %v, want 0.0401785", usage.CostUSD)
	}
	// system/init model (the configured session model) is preferred.
	if usage.Model != "claude-opus-4-8[1m]" {
		t.Errorf("Model = %q, want claude-opus-4-8[1m]", usage.Model)
	}
}

// A log with no result frame (truncated / non-claude) must report ok=false so
// the orchestrator leaves tokens at 0 instead of stamping garbage.
func TestParseClaudeUsage_NoResultFrame(t *testing.T) {
	const noResult = `{"type":"system","subtype":"init","model":"claude-opus-4-8"}
{"type":"assistant","message":{"role":"assistant","model":"claude-opus-4-8","content":[{"type":"text","text":"working"}]}}
not json at all
`
	if usage, ok := ParseClaudeUsage(noResult); ok {
		t.Fatalf("expected ok=false without a result frame, got %+v", usage)
	}
}

func TestParseClaudeUsage_MissingTerminalUsageIsExplicitlyUnreliable(t *testing.T) {
	const stream = `{"type":"system","subtype":"init","model":"proxy-model"}
{"type":"assistant","message":{"id":"msg-1","model":"proxy-model","usage":{"input_tokens":100,"output_tokens":5}}}
{"type":"result","subtype":"success","result":"done","total_cost_usd":0.12}
`
	usage, ok := ParseClaudeUsage(stream)
	if !ok {
		t.Fatal("assistant-frame lower bound should remain observable")
	}
	if usage.TotalTokens != 105 {
		t.Fatalf("TotalTokens = %d, want lower-bound 105", usage.TotalTokens)
	}
	if !usage.UsageUnreliable || usage.UsageUnreliableReason != claudeUsageMissingResult || usage.UsageUnreliableScope != claudeUsageScopeAccounting {
		t.Fatalf("usage reliability = %+v, want missing-result degradation", usage)
	}
	if usage.CostUSD != 0.12 {
		t.Fatalf("CostUSD = %v, want trustworthy backend-reported 0.12", usage.CostUSD)
	}
}

func TestParseClaudeUsage_ZeroTerminalUsageIsNotProgress(t *testing.T) {
	const stream = `{"type":"result","subtype":"success","result":"done","usage":{"input_tokens":0,"output_tokens":0}}
`
	usage, ok := ParseClaudeUsage(stream)
	if ok {
		t.Fatalf("zero usage returned ok=true: %+v", usage)
	}
	if usage.TotalTokens != 0 {
		t.Fatalf("TotalTokens = %d, want 0", usage.TotalTokens)
	}
	if !usage.UsageUnreliable || usage.UsageUnreliableReason != claudeUsageZeroResult || usage.UsageUnreliableScope != claudeUsageScopeAccounting {
		t.Fatalf("usage reliability = %+v, want zero-result degradation", usage)
	}
}

func TestParseClaudeUsage_LiveAssistantFramesBeforeResult(t *testing.T) {
	const live = `{"type":"system","subtype":"init","model":"claude-opus"}
{"type":"assistant","message":{"role":"assistant","model":"claude-opus","usage":{"input_tokens":30000,"output_tokens":1000,"cache_creation_input_tokens":2000,"cache_read_input_tokens":7000}}}
{"type":"assistant","message":{"role":"assistant","model":"claude-opus","usage":{"input_tokens":25000,"output_tokens":1000,"cache_creation_input_tokens":0,"cache_read_input_tokens":14000}}}
`
	usage, ok := ParseClaudeUsage(live)
	if !ok {
		t.Fatal("live assistant usage must be available before the terminal result frame")
	}
	if usage.TotalTokens != 80_000 {
		t.Fatalf("TotalTokens = %d, want 80000", usage.TotalTokens)
	}
}

func TestParseClaudeUsage_DeduplicatesRepeatedAssistantMessageFrames(t *testing.T) {
	const live = `{"type":"assistant","message":{"id":"msg-same","role":"assistant","model":"claude-fable-5","content":[{"type":"thinking"}],"usage":{"input_tokens":2,"output_tokens":5,"cache_creation_input_tokens":12258,"cache_read_input_tokens":30869}}}
{"type":"assistant","message":{"id":"msg-same","role":"assistant","model":"claude-fable-5","content":[{"type":"text","text":"done"}],"usage":{"input_tokens":2,"output_tokens":5,"cache_creation_input_tokens":12258,"cache_read_input_tokens":30869}}}
`
	usage, ok := ParseClaudeUsage(live)
	if !ok {
		t.Fatal("live assistant usage must be available")
	}
	if usage.TotalTokens != 43_134 {
		t.Fatalf("TotalTokens = %d, want one message's 43134 rather than duplicate 86268", usage.TotalTokens)
	}
}

// Multiple result frames (the slot.jsonl after a respawn appends a second
// run's frames) must SUM to the cumulative run total — the property the
// respawn-safe token watermark relies on.
func TestParseClaudeUsage_SumsAcrossResultFrames(t *testing.T) {
	const twoRuns = `{"type":"result","subtype":"success","is_error":false,"num_turns":1,"result":"a","total_cost_usd":0.01,"usage":{"input_tokens":700,"output_tokens":70,"cache_creation_input_tokens":0,"cache_read_input_tokens":3}}
{"type":"result","subtype":"success","is_error":false,"num_turns":1,"result":"b","total_cost_usd":0.02,"usage":{"input_tokens":900,"output_tokens":100,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}
`
	usage, ok := ParseClaudeUsage(twoRuns)
	if !ok {
		t.Fatal("expected ok=true")
	}
	// (700+70+0+3) + (900+100+0+0) = 773 + 1000 = 1773
	if usage.TotalTokens != 1773 {
		t.Errorf("TotalTokens = %d, want 1773 (sum across result frames)", usage.TotalTokens)
	}
	if usage.CostUSD != 0.03 {
		t.Errorf("CostUSD = %v, want 0.03 (sum across result frames)", usage.CostUSD)
	}
}
