package worker

import (
	"encoding/json"
	"strings"
)

// PiUsage is the aggregated provider usage parsed from a Pi `--mode json`
// event stream (#730). Pi emits a newline-delimited stream of events; the
// turn_end / message_end / agent_end events carry the per-turn provider
// usage block (input/output/cacheRead/cacheWrite tokens + a self-computed
// cost) and the provider/model that produced them.
type PiUsage struct {
	Provider     string  // last provider seen on a usage-bearing event
	Model        string  // last model seen on a usage-bearing event
	Input        int     // sum of input tokens across turns
	Output       int     // sum of output tokens across turns
	CacheRead    int     // sum of cache-read tokens across turns
	CacheWrite   int     // sum of cache-write tokens across turns
	TotalTokens  int     // Input + Output + CacheRead + CacheWrite
	BudgetTokens int     // Input + Output + CacheWrite (cache reads excluded)
	CostUSD      float64 // sum of Pi self-reported cost.total across turns
}

// piUsageEvent is the subset of a Pi AgentSessionEvent line that carries
// usage. Only the `message` field is decoded for turn_end / message_end;
// agent_end carries a `messages` array instead.
type piUsageEvent struct {
	Type     string           `json:"type"`
	Message  *piUsageMessage  `json:"message"`
	Messages []piUsageMessage `json:"messages"`
}

// piUsageMessage is the subset of a Pi message carrying provider usage.
type piUsageMessage struct {
	Role     string        `json:"role"`
	Provider string        `json:"provider"`
	Model    string        `json:"model"`
	Usage    *piUsageBlock `json:"usage"`
}

// piUsageBlock is Pi's per-turn usage object. `cost.total` is the USD Pi
// computed from the model's `cost` rates in ~/.pi/agent/models.json.
type piUsageBlock struct {
	Input       int    `json:"input"`
	Output      int    `json:"output"`
	CacheRead   int    `json:"cacheRead"`
	CacheWrite  int    `json:"cacheWrite"`
	TotalTokens int    `json:"totalTokens"`
	Cost        piCost `json:"cost"`
}

type piCost struct {
	Input  float64 `json:"input"`
	Output float64 `json:"output"`
	Total  float64 `json:"total"`
}

// ParsePiUsage scans a Pi `--mode json` event stream (the buffered worker
// log) and aggregates provider usage across turns. It returns ok=false
// when no usage-bearing event is found, so callers can fall back to the
// generic token parser for non-Pi logs.
//
// Aggregation strategy (#730):
//
//   - turn_end fires once per turn with that turn's assistant message usage,
//     so summing input/output/cacheRead/cacheWrite + cost.total across all
//     turn_end events gives the run total without double-counting;
//   - message_end fires for both the user message (usage 0) and the
//     assistant message (usage = the turn's usage), so it is only used as a
//     fallback when no turn_end event is present (e.g. a truncated log that
//     captured the assistant message_end but not the trailing turn_end);
//   - agent_end's messages array carries the final assistant message usage
//     and is the last-resort fallback for a log that ended mid-turn;
//   - model/provider are taken from the last event that carried them.
func ParsePiUsage(text string) (PiUsage, bool) {
	var out PiUsage
	seen := false
	var fallback *piUsageBlock
	var fallbackCost float64

	lines := strings.Split(text, "\n")
	for _, raw := range lines {
		line := strings.TrimSpace(raw)
		if line == "" || line[0] != '{' {
			continue
		}
		var ev piUsageEvent
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			continue
		}
		switch ev.Type {
		case "turn_end":
			if ev.Message == nil || ev.Message.Usage == nil {
				continue
			}
			out.Provider = nonEmpty(out.Provider, ev.Message.Provider)
			out.Model = nonEmpty(out.Model, ev.Message.Model)
			out.Input += ev.Message.Usage.Input
			out.Output += ev.Message.Usage.Output
			out.CacheRead += ev.Message.Usage.CacheRead
			out.CacheWrite += ev.Message.Usage.CacheWrite
			out.CostUSD += ev.Message.Usage.Cost.Total
			seen = true
		case "message_end":
			// Only assistant messages carry usage; user messages report 0.
			if ev.Message == nil || ev.Message.Usage == nil || ev.Message.Role != "assistant" {
				continue
			}
			out.Provider = nonEmpty(out.Provider, ev.Message.Provider)
			out.Model = nonEmpty(out.Model, ev.Message.Model)
			fallback = ev.Message.Usage
			fallbackCost = ev.Message.Usage.Cost.Total
		case "agent_end":
			// Take the last assistant message in the array as the fallback.
			for i := len(ev.Messages) - 1; i >= 0; i-- {
				m := ev.Messages[i]
				if m.Role != "assistant" || m.Usage == nil {
					continue
				}
				out.Provider = nonEmpty(out.Provider, m.Provider)
				out.Model = nonEmpty(out.Model, m.Model)
				if fallback == nil {
					fallback = &piUsageBlock{
						Input:       m.Usage.Input,
						Output:      m.Usage.Output,
						CacheRead:   m.Usage.CacheRead,
						CacheWrite:  m.Usage.CacheWrite,
						TotalTokens: m.Usage.TotalTokens,
						Cost:        m.Usage.Cost,
					}
					fallbackCost = m.Usage.Cost.Total
				}
				break
			}
		}
	}

	if !seen && fallback != nil {
		out.Input = fallback.Input
		out.Output = fallback.Output
		out.CacheRead = fallback.CacheRead
		out.CacheWrite = fallback.CacheWrite
		out.CostUSD = fallbackCost
		seen = true
	}
	out.TotalTokens = out.Input + out.Output + out.CacheRead + out.CacheWrite
	out.BudgetTokens = out.Input + out.Output + out.CacheWrite
	return out, seen
}

func nonEmpty(fallback, candidate string) string {
	if strings.TrimSpace(candidate) == "" {
		return fallback
	}
	return candidate
}
