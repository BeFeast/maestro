package worker

import (
	"encoding/json"
	"strings"
)

// KimiUsage is the cumulative usage parsed from Kimi Code CLI stream-json.
// Kimi's native token names distinguish uncached input from cache reads and
// cache creation, matching Maestro's split-cost accounting dimensions.
type KimiUsage struct {
	Model       string
	Input       int
	Output      int
	CacheRead   int
	CacheWrite  int
	TotalTokens int
	CostUSD     float64
}

type kimiUsageBlock struct {
	Input      int
	Output     int
	CacheRead  int
	CacheWrite int
	CostUSD    float64
}

// ParseKimiUsage scans an appended Kimi JSONL side channel and sums usage
// across steps and attempts. It accepts the native TokenUsage field names
// (input_other/output/input_cache_read/input_cache_creation) in either a
// top-level stream message or Kimi's StatusUpdate envelope. Camel-case aliases
// are accepted for the newer TypeScript event contract.
func ParseKimiUsage(text string) (KimiUsage, bool) {
	var out KimiUsage
	seen := false
	byMessage := make(map[string]kimiUsageBlock)

	for _, raw := range strings.Split(text, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || line[0] != '{' {
			continue
		}
		root, ok := decodeJSONObject([]byte(line))
		if !ok {
			continue
		}
		usageMap, container, ok := findKimiUsageMap(root)
		if !ok {
			continue
		}
		usage := decodeKimiUsageBlock(usageMap)
		if usage.CostUSD == 0 {
			usage.CostUSD = firstJSONNumber(container, "cost_usd", "total_cost_usd")
		}
		if usage.CostUSD == 0 {
			usage.CostUSD = firstJSONNumber(root, "cost_usd", "total_cost_usd")
		}
		if model := firstJSONString(container, "model", "model_name"); model != "" {
			out.Model = model
		} else if model := firstJSONString(root, "model", "model_name"); model != "" {
			out.Model = model
		}

		messageID := firstJSONString(container, "message_id", "messageId", "id")
		if messageID == "" {
			messageID = firstJSONString(root, "message_id", "messageId")
		}
		if messageID == "" {
			addKimiUsage(&out, usage)
		} else {
			previous := byMessage[messageID]
			addKimiUsage(&out, kimiUsageBlock{
				Input:      positiveDelta(usage.Input, previous.Input),
				Output:     positiveDelta(usage.Output, previous.Output),
				CacheRead:  positiveDelta(usage.CacheRead, previous.CacheRead),
				CacheWrite: positiveDelta(usage.CacheWrite, previous.CacheWrite),
				CostUSD:    positiveFloatDelta(usage.CostUSD, previous.CostUSD),
			})
			byMessage[messageID] = usage
		}
		if kimiUsageTotal(usage) > 0 || usage.CostUSD > 0 {
			seen = true
		}
	}

	out.TotalTokens = out.Input + out.Output + out.CacheRead + out.CacheWrite
	return out, seen
}

func addKimiUsage(out *KimiUsage, usage kimiUsageBlock) {
	if out == nil {
		return
	}
	out.Input += usage.Input
	out.Output += usage.Output
	out.CacheRead += usage.CacheRead
	out.CacheWrite += usage.CacheWrite
	out.CostUSD += usage.CostUSD
}

func positiveFloatDelta(current, previous float64) float64 {
	if current > previous {
		return current - previous
	}
	return 0
}

func kimiUsageTotal(usage kimiUsageBlock) int {
	return usage.Input + usage.Output + usage.CacheRead + usage.CacheWrite
}

func decodeKimiUsageBlock(m map[string]json.RawMessage) kimiUsageBlock {
	return kimiUsageBlock{
		Input:      firstKimiJSONInt(m, "input_other", "inputOther"),
		Output:     firstKimiJSONInt(m, "output", "output_tokens", "outputTokens"),
		CacheRead:  firstKimiJSONInt(m, "input_cache_read", "inputCacheRead", "cache_read_input_tokens"),
		CacheWrite: firstKimiJSONInt(m, "input_cache_creation", "inputCacheCreation", "cache_creation_input_tokens"),
		CostUSD:    firstJSONNumber(m, "cost_usd", "total_cost_usd"),
	}
}

// findKimiUsageMap finds one usage block and returns the closest containing
// object as well, so callers can read sibling message_id/model/cost fields.
func findKimiUsageMap(root map[string]json.RawMessage) (map[string]json.RawMessage, map[string]json.RawMessage, bool) {
	if usage, ok := nestedJSONObject(root, "usage", "token_usage", "tokenUsage"); ok {
		return usage, root, true
	}
	for _, key := range []string{"payload", "message", "data"} {
		child, ok := objectField(root, key)
		if !ok {
			continue
		}
		if usage, container, ok := findKimiUsageMap(child); ok {
			return usage, container, true
		}
	}
	return nil, nil, false
}

func decodeJSONObject(data []byte) (map[string]json.RawMessage, bool) {
	var out map[string]json.RawMessage
	if json.Unmarshal(data, &out) != nil || out == nil {
		return nil, false
	}
	return out, true
}

func objectField(m map[string]json.RawMessage, key string) (map[string]json.RawMessage, bool) {
	raw, ok := m[key]
	if !ok {
		return nil, false
	}
	return decodeJSONObject(raw)
}

func nestedJSONObject(m map[string]json.RawMessage, keys ...string) (map[string]json.RawMessage, bool) {
	for _, key := range keys {
		if child, ok := objectField(m, key); ok {
			return child, true
		}
	}
	return nil, false
}

func firstKimiJSONInt(m map[string]json.RawMessage, keys ...string) int {
	for _, key := range keys {
		raw, ok := m[key]
		if !ok {
			continue
		}
		var value int
		if json.Unmarshal(raw, &value) == nil {
			return value
		}
	}
	return 0
}

func firstJSONNumber(m map[string]json.RawMessage, keys ...string) float64 {
	for _, key := range keys {
		raw, ok := m[key]
		if !ok {
			continue
		}
		var value float64
		if json.Unmarshal(raw, &value) == nil {
			return value
		}
	}
	return 0
}

func firstJSONString(m map[string]json.RawMessage, keys ...string) string {
	for _, key := range keys {
		raw, ok := m[key]
		if !ok {
			continue
		}
		var value string
		if json.Unmarshal(raw, &value) == nil && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
