package worker

import (
	"encoding/json"
	"strings"
)

// CodexUsage is the aggregated provider usage parsed from a codex
// `exec --json` JSONL stream (#738). Each `codex exec` invocation emits one
// terminal `turn.completed` event carrying that invocation's cumulative token
// usage. The stream-splitter appends every attempt's frames to the same
// slot.jsonl, so summing across turn.completed events yields the cumulative
// run total across attempts — monotonic as the log grows, which is what the
// orchestrator's respawn-safe token watermark expects.
//
// codex follows OpenAI usage semantics: cached_input_tokens is a SUBSET of
// input_tokens (the portion of the prompt served from cache), not a separate
// bucket; reasoning_output_tokens is likewise a subset of output_tokens.
// TotalTokens is therefore Input + Output only — adding CacheRead would
// double-count the cached prompt tokens. CacheRead is retained for the
// cache-aware split-cost model (#739). codex reports no USD, so cost stays
// virtual (computed from the configured pricing block, no self-reported USD).
type CodexUsage struct {
	Input       int // sum of input_tokens across turn.completed events (includes the cached subset)
	Output      int // sum of output_tokens across turn.completed events
	CacheRead   int // sum of cached_input_tokens (a subset of Input; not added to TotalTokens)
	TotalTokens int // Input + Output
}

// codexStreamFrame is the subset of a codex `exec --json` line this package
// decodes. The terminal turn.completed event carries the usage block; all
// other event types (thread.started, turn.started, item.*) are ignored here.
type codexStreamFrame struct {
	Type  string           `json:"type"`
	Usage *codexUsageBlock `json:"usage"`
}

// codexUsageBlock mirrors the usage object codex emits on turn.completed.
// cached_input_tokens and reasoning_output_tokens are subsets of
// input_tokens / output_tokens respectively (OpenAI semantics); only
// input/output feed the token total.
type codexUsageBlock struct {
	InputTokens           int `json:"input_tokens"`
	CachedInputTokens     int `json:"cached_input_tokens"`
	OutputTokens          int `json:"output_tokens"`
	ReasoningOutputTokens int `json:"reasoning_output_tokens"`
}

// ParseCodexUsage scans a codex `exec --json` JSONL stream (the appended
// slot.jsonl) and aggregates usage across every terminal turn.completed event.
// It returns ok=false when no usage-bearing event is found, so callers can
// leave tokens at 0 (the documented degradation when the stream-splitter was
// unavailable or no event has landed yet) and fall back to the legacy text
// parser.
func ParseCodexUsage(text string) (CodexUsage, bool) {
	var out CodexUsage
	seen := false

	for _, raw := range strings.Split(text, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || line[0] != '{' {
			continue
		}
		var fr codexStreamFrame
		if err := json.Unmarshal([]byte(line), &fr); err != nil {
			continue
		}
		if fr.Type != "turn.completed" || fr.Usage == nil {
			continue
		}
		out.Input += fr.Usage.InputTokens
		out.Output += fr.Usage.OutputTokens
		out.CacheRead += fr.Usage.CachedInputTokens
		seen = true
	}

	out.TotalTokens = out.Input + out.Output
	return out, seen
}
