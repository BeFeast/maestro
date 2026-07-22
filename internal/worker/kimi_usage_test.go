package worker

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseKimiUsage_Fixture(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "kimi_stream_usage.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	usage, ok := ParseKimiUsage(string(data))
	if !ok {
		t.Fatal("ParseKimiUsage returned ok=false")
	}
	if usage.Input != 1800 || usage.Output != 100 {
		t.Fatalf("input/output = %d/%d, want 1800/100", usage.Input, usage.Output)
	}
	if usage.CacheRead != 600 || usage.CacheWrite != 100 {
		t.Fatalf("cache read/write = %d/%d, want 600/100", usage.CacheRead, usage.CacheWrite)
	}
	if usage.TotalTokens != 2600 {
		t.Fatalf("TotalTokens = %d, want 2600", usage.TotalTokens)
	}
}

func TestParseKimiUsage_DeduplicatesMessageUpdates(t *testing.T) {
	const stream = `{"type":"StatusUpdate","payload":{"token_usage":{"input_other":100,"output":10,"input_cache_read":20,"input_cache_creation":5},"message_id":"same"}}
{"type":"StatusUpdate","payload":{"token_usage":{"input_other":125,"output":15,"input_cache_read":20,"input_cache_creation":5},"message_id":"same"}}
`
	usage, ok := ParseKimiUsage(stream)
	if !ok {
		t.Fatal("ParseKimiUsage returned ok=false")
	}
	if usage.Input != 125 || usage.Output != 15 || usage.CacheRead != 20 || usage.CacheWrite != 5 || usage.TotalTokens != 165 {
		t.Fatalf("usage = %+v, want deduplicated total 165", usage)
	}
}

func TestParseKimiUsage_TerminalFallbackAndRecordedEnvelope(t *testing.T) {
	const terminal = `{"type":"result","model":"kimi-k2","usage":{"input_tokens":700,"output_tokens":30,"cache_read":50,"cache_write":20}}
`
	usage, ok := ParseKimiUsage(terminal)
	if !ok || usage.TotalTokens != 800 || usage.Model != "kimi-k2" {
		t.Fatalf("terminal usage = %+v, ok=%t, want model kimi-k2 total 800", usage, ok)
	}

	const recorded = `{"timestamp":1,"message":{"type":"StatusUpdate","payload":{"token_usage":{"input_other":40,"output":2,"input_cache_read":8,"input_cache_creation":0},"message_id":"recorded"}}}
`
	usage, ok = ParseKimiUsage(recorded)
	if !ok || usage.TotalTokens != 50 {
		t.Fatalf("recorded envelope usage = %+v, ok=%t, want total 50", usage, ok)
	}
}
