package worker

import (
	"encoding/json"
	"strings"
)

// KimiUsage is the cumulative provider usage parsed from Kimi Code CLI's
// stream-json output. Kimi reports one TokenUsage block per model step using
// input_other/output/input_cache_read/input_cache_creation fields. The
// stream-splitter appends every attempt to one slot.jsonl, so summing distinct
// step usage produces the monotonic run total used by Maestro's watermark.
// Kimi does not report USD cost; configured backend pricing supplies the
// virtual cost from these split counters.
type KimiUsage struct {
	Model       string
	Input       int
	Output      int
	CacheRead   int
	CacheWrite  int
	TotalTokens int
}

type kimiStreamFrame struct {
	Type       string          `json:"type"`
	Role       string          `json:"role"`
	Model      string          `json:"model"`
	MessageID  string          `json:"message_id"`
	SessionID  string          `json:"session_id"`
	Usage      *kimiUsageBlock `json:"usage"`
	TokenUsage *kimiUsageBlock `json:"token_usage"`
	Payload    *kimiPayload    `json:"payload"`
	Message    json.RawMessage `json:"message"`
}

type kimiPayload struct {
	Model      string          `json:"model"`
	MessageID  string          `json:"message_id"`
	SessionID  string          `json:"session_id"`
	Usage      *kimiUsageBlock `json:"usage"`
	TokenUsage *kimiUsageBlock `json:"token_usage"`
}

// kimiUsageBlock accepts the public Kimi TokenUsage names plus common
// stream-provider aliases. The aliases keep the parser compatible with Kimi
// custom providers that preserve upstream usage naming in stream-json.
type kimiUsageBlock struct {
	InputOther              int `json:"input_other"`
	InputOtherCamel         int `json:"inputOther"`
	Input                   int `json:"input"`
	InputTokens             int `json:"input_tokens"`
	Output                  int `json:"output"`
	OutputTokens            int `json:"output_tokens"`
	InputCacheRead          int `json:"input_cache_read"`
	InputCacheReadCamel     int `json:"inputCacheRead"`
	CacheRead               int `json:"cache_read"`
	CacheReadCamel          int `json:"cacheRead"`
	InputCacheCreation      int `json:"input_cache_creation"`
	InputCacheCreationCamel int `json:"inputCacheCreation"`
	CacheCreationInput      int `json:"cache_creation_input_tokens"`
	CacheWrite              int `json:"cache_write"`
	CacheWriteCamel         int `json:"cacheWrite"`
	TotalTokens             int `json:"total_tokens"`
}

type kimiUsageCounts struct {
	Input      int
	Output     int
	CacheRead  int
	CacheWrite int
}

func (u *kimiUsageBlock) counts() kimiUsageCounts {
	if u == nil {
		return kimiUsageCounts{}
	}
	counts := kimiUsageCounts{
		Input:  firstPositive(u.InputOther, u.InputOtherCamel, u.Input, u.InputTokens),
		Output: firstPositive(u.Output, u.OutputTokens),
		CacheRead: firstPositive(
			u.InputCacheRead,
			u.InputCacheReadCamel,
			u.CacheRead,
			u.CacheReadCamel,
		),
		CacheWrite: firstPositive(
			u.InputCacheCreation,
			u.InputCacheCreationCamel,
			u.CacheCreationInput,
			u.CacheWrite,
			u.CacheWriteCamel,
		),
	}
	if counts.total() == 0 && u.TotalTokens > 0 {
		counts.Input = u.TotalTokens
	}
	return counts
}

func (u kimiUsageCounts) total() int {
	return u.Input + u.Output + u.CacheRead + u.CacheWrite
}

func (u *kimiUsageCounts) add(delta kimiUsageCounts) {
	u.Input += delta.Input
	u.Output += delta.Output
	u.CacheRead += delta.CacheRead
	u.CacheWrite += delta.CacheWrite
}

func (u kimiUsageCounts) positiveDelta(previous kimiUsageCounts) kimiUsageCounts {
	return kimiUsageCounts{
		Input:      positiveDelta(u.Input, previous.Input),
		Output:     positiveDelta(u.Output, previous.Output),
		CacheRead:  positiveDelta(u.CacheRead, previous.CacheRead),
		CacheWrite: positiveDelta(u.CacheWrite, previous.CacheWrite),
	}
}

// ParseKimiUsage scans an appended Kimi JSONL side channel and sums distinct
// per-step usage. StatusUpdate.token_usage is the primary public shape. Direct
// assistant usage blocks are also accepted for print-stream variants, and a
// terminal result/turn.completed usage block is used only as a fallback when
// no per-step usage was observed. Repeated updates for one message_id are
// deduplicated by positive deltas.
func ParseKimiUsage(text string) (KimiUsage, bool) {
	var out KimiUsage
	var stepTotal, terminalTotal kimiUsageCounts
	stepSeen := false
	terminalSeen := false
	stepMessages := make(map[string]kimiUsageCounts)

	for _, raw := range strings.Split(text, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || line[0] != '{' {
			continue
		}
		var frame kimiStreamFrame
		if json.Unmarshal([]byte(line), &frame) != nil {
			continue
		}
		frame = unwrapKimiFrame(frame)
		usage, messageID, model := kimiFrameUsage(frame)
		if strings.TrimSpace(model) != "" {
			out.Model = model
		}
		counts := usage.counts()
		if counts.total() == 0 {
			continue
		}

		frameType := strings.ToLower(strings.TrimSpace(frame.Type))
		switch frameType {
		case "result", "turn.completed", "turn_completed":
			terminalTotal.add(counts)
			terminalSeen = true
		default:
			if frameType != "statusupdate" && frameType != "status_update" && frameType != "usage" && strings.ToLower(strings.TrimSpace(frame.Role)) != "assistant" {
				continue
			}
			key := strings.TrimSpace(messageID)
			if sessionID := strings.TrimSpace(kimiFrameSessionID(frame)); key != "" && sessionID != "" {
				key = sessionID + ":" + key
			}
			if key == "" {
				stepTotal.add(counts)
			} else {
				previous := stepMessages[key]
				stepTotal.add(counts.positiveDelta(previous))
				stepMessages[key] = counts
			}
			stepSeen = true
		}
	}

	selected := stepTotal
	seen := stepSeen
	if !seen && terminalSeen {
		selected = terminalTotal
		seen = true
	}
	out.Input = selected.Input
	out.Output = selected.Output
	out.CacheRead = selected.CacheRead
	out.CacheWrite = selected.CacheWrite
	out.TotalTokens = selected.total()
	return out, seen
}

func unwrapKimiFrame(frame kimiStreamFrame) kimiStreamFrame {
	if strings.TrimSpace(frame.Type) != "" || strings.TrimSpace(frame.Role) != "" || len(frame.Message) == 0 {
		return frame
	}
	var nested kimiStreamFrame
	if json.Unmarshal(frame.Message, &nested) == nil {
		return nested
	}
	return frame
}

func kimiFrameUsage(frame kimiStreamFrame) (*kimiUsageBlock, string, string) {
	usage := frame.TokenUsage
	if usage == nil {
		usage = frame.Usage
	}
	messageID := frame.MessageID
	model := frame.Model
	if frame.Payload != nil {
		if frame.Payload.TokenUsage != nil {
			usage = frame.Payload.TokenUsage
		} else if frame.Payload.Usage != nil {
			usage = frame.Payload.Usage
		}
		if strings.TrimSpace(frame.Payload.MessageID) != "" {
			messageID = frame.Payload.MessageID
		}
		if strings.TrimSpace(frame.Payload.Model) != "" {
			model = frame.Payload.Model
		}
	}
	return usage, messageID, model
}

func kimiFrameSessionID(frame kimiStreamFrame) string {
	if frame.Payload != nil && strings.TrimSpace(frame.Payload.SessionID) != "" {
		return frame.Payload.SessionID
	}
	return frame.SessionID
}

func firstPositive(values ...int) int {
	for _, value := range values {
		if value > 0 {
			return value
		}
	}
	return 0
}
