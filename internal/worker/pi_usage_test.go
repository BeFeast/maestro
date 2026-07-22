package worker

import (
	"math"
	"strings"
	"testing"
)

// #730: ParsePiUsage aggregates provider usage from a Pi --mode json event
// stream and reports model/tokens/cost for session attribution.
func TestParsePiUsage_SingleTurn(t *testing.T) {
	// Real stream shape captured from `pi --mode json -p` against the
	// ollama/glm-5.2:cloud provider (reformatted one line per event).
	const stream = `{"type":"session","version":3,"id":"abc","timestamp":"2026-06-19T21:10:12.069Z","cwd":"/tmp"}
{"type":"agent_start"}
{"type":"turn_start"}
{"type":"message_start","message":{"role":"assistant","content":[],"provider":"ollama","model":"glm-5.2:cloud","usage":{"input":0,"output":0,"cacheRead":0,"cacheWrite":0,"totalTokens":0,"cost":{"input":0,"output":0,"total":0}}}}
{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"OK"}],"provider":"ollama","model":"glm-5.2:cloud","usage":{"input":770,"output":3,"cacheRead":0,"cacheWrite":0,"totalTokens":773,"cost":{"input":0.001078,"output":0.0000132,"total":0.0010912}}}}
{"type":"turn_end","message":{"role":"assistant","content":[{"type":"text","text":"OK"}],"provider":"ollama","model":"glm-5.2:cloud","usage":{"input":770,"output":3,"cacheRead":0,"cacheWrite":0,"totalTokens":773,"cost":{"input":0.001078,"output":0.0000132,"total":0.0010912}}},"toolResults":[]}
{"type":"agent_end","messages":[{"role":"user","content":[{"type":"text","text":"Reply"}]},{"role":"assistant","content":[{"type":"text","text":"OK"}],"provider":"ollama","model":"glm-5.2:cloud","usage":{"input":770,"output":3,"cacheRead":0,"cacheWrite":0,"totalTokens":773,"cost":{"input":0.001078,"output":0.0000132,"total":0.0010912}}}]}
`
	usage, ok := ParsePiUsage(stream)
	if !ok {
		t.Fatal("expected ok=true, got false")
	}
	if usage.Provider != "ollama" {
		t.Errorf("Provider = %q, want ollama", usage.Provider)
	}
	if usage.Model != "glm-5.2:cloud" {
		t.Errorf("Model = %q, want glm-5.2:cloud", usage.Model)
	}
	if usage.Input != 770 || usage.Output != 3 {
		t.Errorf("Input/Output = %d/%d, want 770/3", usage.Input, usage.Output)
	}
	if usage.TotalTokens != 773 {
		t.Errorf("TotalTokens = %d, want 773", usage.TotalTokens)
	}
	if usage.BudgetTokens != 773 {
		t.Errorf("BudgetTokens = %d, want 773", usage.BudgetTokens)
	}
	if math.Abs(usage.CostUSD-0.0010912) > 1e-9 {
		t.Errorf("CostUSD = %v, want 0.0010912", usage.CostUSD)
	}
}

// #730: multiple turns must sum usage + cost across turn_end events without
// double-counting the duplicate message_end usage block.
func TestParsePiUsage_MultiTurnSums(t *testing.T) {
	const stream = `{"type":"turn_end","message":{"role":"assistant","provider":"ollama","model":"glm-5.2:cloud","usage":{"input":1000,"output":10,"cacheRead":200,"cacheWrite":50,"totalTokens":1260,"cost":{"input":0.0014,"output":0.000044,"total":0.001444}}}}
{"type":"turn_end","message":{"role":"assistant","provider":"ollama","model":"glm-5.2:cloud","usage":{"input":2200,"output":20,"cacheRead":0,"cacheWrite":0,"totalTokens":2220,"cost":{"input":0.00308,"output":0.000088,"total":0.003168}}}}
`
	usage, ok := ParsePiUsage(stream)
	if !ok {
		t.Fatal("expected ok=true")
	}
	if usage.Input != 3200 || usage.Output != 30 || usage.CacheRead != 200 || usage.CacheWrite != 50 {
		t.Errorf("usage fields = in=%d out=%d cr=%d cw=%d, want 3200/30/200/50", usage.Input, usage.Output, usage.CacheRead, usage.CacheWrite)
	}
	if usage.TotalTokens != 3480 {
		t.Errorf("TotalTokens = %d, want 3480", usage.TotalTokens)
	}
	if usage.BudgetTokens != 3280 {
		t.Errorf("BudgetTokens = %d, want 3280 (cache reads excluded)", usage.BudgetTokens)
	}
	if math.Abs(usage.CostUSD-(0.001444+0.003168)) > 1e-9 {
		t.Errorf("CostUSD = %v, want %v", usage.CostUSD, 0.001444+0.003168)
	}
}

// #730: a truncated log that captured the assistant message_end but not the
// trailing turn_end still falls back to the message_end usage so the run is
// attributed instead of dropping to tokens=0.
func TestParsePiUsage_FallsBackToMessageEnd(t *testing.T) {
	const stream = `{"type":"message_end","message":{"role":"user","content":[],"usage":{"input":0,"output":0,"cacheRead":0,"cacheWrite":0,"totalTokens":0,"cost":{"total":0}}}}
{"type":"message_end","message":{"role":"assistant","provider":"ollama","model":"glm-5.2:cloud","usage":{"input":500,"output":5,"cacheRead":0,"cacheWrite":0,"totalTokens":505,"cost":{"total":0.0007}}}}
`
	usage, ok := ParsePiUsage(stream)
	if !ok {
		t.Fatal("expected ok=true from message_end fallback")
	}
	if usage.TotalTokens != 505 {
		t.Errorf("TotalTokens = %d, want 505", usage.TotalTokens)
	}
	if usage.Model != "glm-5.2:cloud" {
		t.Errorf("Model = %q, want glm-5.2:cloud", usage.Model)
	}
	if math.Abs(usage.CostUSD-0.0007) > 1e-9 {
		t.Errorf("CostUSD = %v, want 0.0007", usage.CostUSD)
	}
}

// #730: agent_end is the last-resort fallback for a log that ended mid-turn
// with no turn_end / message_end.
func TestParsePiUsage_FallsBackToAgentEnd(t *testing.T) {
	const stream = `{"type":"agent_end","messages":[{"role":"user","content":[{"type":"text","text":"hi"}]},{"role":"assistant","provider":"ollama","model":"glm-5.2:cloud","usage":{"input":42,"output":2,"cacheRead":0,"cacheWrite":0,"totalTokens":44,"cost":{"total":0.0001}}}]}
`
	usage, ok := ParsePiUsage(stream)
	if !ok {
		t.Fatal("expected ok=true from agent_end fallback")
	}
	if usage.TotalTokens != 44 {
		t.Errorf("TotalTokens = %d, want 44", usage.TotalTokens)
	}
	if usage.Model != "glm-5.2:cloud" {
		t.Errorf("Model = %q, want glm-5.2:cloud", usage.Model)
	}
}

// #730: a buffer with no usage-bearing events (e.g. a claude log or a Pi
// startup banner) must report ok=false so the caller keeps the generic path.
func TestParsePiUsage_NoUsageReturnsFalse(t *testing.T) {
	const stream = `{"type":"session","version":3,"id":"abc"}
{"type":"agent_start"}
some non-JSON log line
Tokens: 1234 input, 5678 output
`
	_, ok := ParsePiUsage(stream)
	if ok {
		t.Fatal("expected ok=false for a stream with no Pi usage events")
	}
}

// #730: malformed JSON lines are skipped without aborting the parse.
func TestParsePiUsage_SkipsMalformedLines(t *testing.T) {
	const stream = `garbage line
{not json
{"type":"turn_end","message":{"role":"assistant","provider":"ollama","model":"glm-5.2:cloud","usage":{"input":10,"output":1,"cacheRead":0,"cacheWrite":0,"totalTokens":11,"cost":{"total":0.0001}}}}
`
	usage, ok := ParsePiUsage(stream)
	if !ok {
		t.Fatal("expected ok=true")
	}
	if usage.TotalTokens != 11 {
		t.Errorf("TotalTokens = %d, want 11", usage.TotalTokens)
	}
	if !strings.Contains(stream, "glm-5.2:cloud") && usage.Model != "glm-5.2:cloud" {
		t.Errorf("Model = %q", usage.Model)
	}
}
