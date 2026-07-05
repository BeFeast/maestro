package worker

import (
	"encoding/json"
	"strings"
)

// OpenCodeUsage is the aggregated provider usage parsed from an opencode
// --format json NDJSON stream. Each `opencode run` invocation emits one
// terminal `step_finish` event carrying that invocation's cumulative token
// usage and cost. The stream-splitter appends every attempt's frames to the
// same slot.jsonl, so summing across step_finish events yields the cumulative
// run total across attempts — monotonic as the log grows, which is what the
// orchestrator's respawn-safe token watermark expects.
//
// opencode total = input + output + reasoning (reasoning is a separate
// thinking-tokens bucket, not a subset of output). Cache read/write are also
// separate counts. Cost is self-reported USD.
type OpenCodeUsage struct {
	Input       int
	Output      int
	Reasoning   int
	CacheRead   int
	CacheWrite  int
	TotalTokens int    // Input + Output + Reasoning
	CostUSD     float64
}

// opencodeStreamFrame is the subset of an opencode --format json event line
// this package decodes. The terminal step_finish event carries
// tokens + cost; all other event types (step_start, text) are ignored here.
type opencodeStreamFrame struct {
	Type string            `json:"type"`
	Part *opencodeUsagePart `json:"part"`
}

type opencodeUsagePart struct {
	Type   string              `json:"type"`
	Tokens *opencodeUsageBlock `json:"tokens"`
	Cost   float64             `json:"cost"`
}

// opencodeUsageBlock mirrors the usage object opencode emits on step_finish.
// total = input + output + reasoning. cache.read / cache.write are separate
// cumulative counts.
type opencodeUsageBlock struct {
	Total     int                `json:"total"`
	Input     int                `json:"input"`
	Output    int                `json:"output"`
	Reasoning int                `json:"reasoning"`
	Cache     *opencodeCacheInfo `json:"cache"`
}

type opencodeCacheInfo struct {
	Write int `json:"write"`
	Read  int `json:"read"`
}

// ParseOpenCodeUsage scans an opencode --format json NDJSON stream (the
// appended slot.jsonl) and aggregates usage across every terminal step_finish
// event. It returns ok=false when no usage-bearing event is found, so callers
// can leave tokens at 0 (the documented degradation when the stream-splitter
// was unavailable or no event has landed yet) and fall back to the legacy text
// parser.
func ParseOpenCodeUsage(text string) (OpenCodeUsage, bool) {
	var out OpenCodeUsage
	seen := false

	for _, raw := range strings.Split(text, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || line[0] != '{' {
			continue
		}
		var fr opencodeStreamFrame
		if err := json.Unmarshal([]byte(line), &fr); err != nil {
			continue
		}
		if fr.Type != "step_finish" || fr.Part == nil || fr.Part.Tokens == nil {
			continue
		}
		tk := fr.Part.Tokens
		out.Input += tk.Input
		out.Output += tk.Output
		out.Reasoning += tk.Reasoning
		out.CostUSD += fr.Part.Cost
		if tk.Cache != nil {
			out.CacheRead += tk.Cache.Read
			out.CacheWrite += tk.Cache.Write
		}
		seen = true
	}

	out.TotalTokens = out.Input + out.Output + out.Reasoning
	return out, seen
}
