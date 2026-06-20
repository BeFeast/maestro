package worker

import (
	"encoding/json"
	"strings"
)

// ClaudeUsage is the aggregated provider usage parsed from a claude
// `--output-format stream-json --verbose` NDJSON stream (#737). The terminal
// `result` frame of each `claude -p` invocation carries that invocation's
// cumulative `usage` (input/output/cache tokens) and `total_cost_usd`.
// Because the stream-splitter appends every run's frames to the same
// slot.jsonl, summing across all `result` frames yields the cumulative run
// total across attempts — monotonic as the log grows, which is what the
// orchestrator's respawn-safe token watermark expects.
type ClaudeUsage struct {
	Model       string  // session model (system/init), falling back to the assistant message model
	Input       int     // sum of input_tokens across result frames
	Output      int     // sum of output_tokens across result frames
	CacheRead   int     // sum of cache_read_input_tokens across result frames
	CacheWrite  int     // sum of cache_creation_input_tokens across result frames
	TotalTokens int     // Input + Output + CacheRead + CacheWrite
	CostUSD     float64 // sum of total_cost_usd across result frames
}

// claudeStreamFrame is the subset of a claude stream-json line this package
// decodes. `model` is present on the system/init frame; `message` on
// assistant/user frames; `usage` + `total_cost_usd` on the result frame.
type claudeStreamFrame struct {
	Type         string               `json:"type"`
	Subtype      string               `json:"subtype"`
	Model        string               `json:"model"`
	Message      *claudeStreamMessage `json:"message"`
	Usage        *claudeUsageBlock    `json:"usage"`
	TotalCostUSD float64              `json:"total_cost_usd"`
}

// claudeStreamMessage is the subset of a claude assistant/user message that
// carries the model name (and a per-turn usage block we do not aggregate —
// the result frame's usage is the authoritative run total).
type claudeStreamMessage struct {
	Model string            `json:"model"`
	Usage *claudeUsageBlock `json:"usage"`
}

// claudeUsageBlock mirrors the Anthropic usage object emitted on the result
// frame. cache_creation_input_tokens are freshly written cache tokens
// (CacheWrite); cache_read_input_tokens are reused cache tokens (CacheRead).
type claudeUsageBlock struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
}

// ParseClaudeUsage scans a claude stream-json NDJSON stream (the appended
// slot.jsonl) and aggregates usage across every terminal `result` frame.
// It returns ok=false when no usage-bearing result frame is found, so callers
// can leave tokens/cost at 0 (the documented degradation when the
// stream-splitter was unavailable or no frame has landed yet).
func ParseClaudeUsage(text string) (ClaudeUsage, bool) {
	var out ClaudeUsage
	seen := false
	var systemModel, assistantModel string

	for _, raw := range strings.Split(text, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || line[0] != '{' {
			continue
		}
		var fr claudeStreamFrame
		if err := json.Unmarshal([]byte(line), &fr); err != nil {
			continue
		}
		switch fr.Type {
		case "system":
			if strings.TrimSpace(fr.Model) != "" {
				systemModel = fr.Model
			}
		case "assistant":
			if fr.Message != nil && strings.TrimSpace(fr.Message.Model) != "" {
				assistantModel = fr.Message.Model
			}
		case "result":
			if fr.Usage == nil {
				continue
			}
			out.Input += fr.Usage.InputTokens
			out.Output += fr.Usage.OutputTokens
			out.CacheRead += fr.Usage.CacheReadInputTokens
			out.CacheWrite += fr.Usage.CacheCreationInputTokens
			out.CostUSD += fr.TotalCostUSD
			seen = true
		}
	}

	// Prefer the session model from the system/init frame (the configured
	// model, e.g. claude-opus-4-8[1m]); fall back to the assistant message
	// model when init was not captured (truncated stream).
	out.Model = nonEmpty(assistantModel, systemModel)
	out.TotalTokens = out.Input + out.Output + out.CacheRead + out.CacheWrite
	return out, seen
}
