package worker

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

const TokenBudgetExceededOutcome = "token_budget_exceeded"
const TokenBudgetMeasureUncached = "uncached_tokens"
const TokenBudgetMeasureCodexRollout = "codex_rollout_tokens"
const TokenBudgetMeasureInputOutputReasoning = "input_output_reasoning_tokens"
const TokenBudgetMeasureProviderTotal = "provider_total_tokens"
const TokenBudgetMeasureProviderTotalLegacy = "provider_total_tokens_legacy"

var ErrTokenBudgetExceeded = errors.New(TokenBudgetExceededOutcome)

// TokenBudgetMarker is the durable, non-sensitive handoff from the worker
// stream process to orchestrator and Fleet reconciliation.
type TokenBudgetMarker struct {
	Outcome          string    `json:"outcome"`
	Backend          string    `json:"backend"`
	TokensObserved   int       `json:"tokens_observed"`
	MaxTokens        int       `json:"max_tokens"`
	Measure          string    `json:"measure"`
	WorkerGeneration uint64    `json:"worker_generation,omitempty"`
	MeasuredAt       time.Time `json:"measured_at"`
}

func TokenBudgetMarkerPathForLog(logFile string) string {
	if logFile == "" {
		return ""
	}
	if strings.HasSuffix(logFile, ".log") {
		return strings.TrimSuffix(logFile, ".log") + ".token-budget.json"
	}
	return logFile + ".token-budget.json"
}

func ReadTokenBudgetMarker(logFile string) (TokenBudgetMarker, bool) {
	path := TokenBudgetMarkerPathForLog(logFile)
	if path == "" {
		return TokenBudgetMarker{}, false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return TokenBudgetMarker{}, false
	}
	var marker TokenBudgetMarker
	if json.Unmarshal(data, &marker) != nil || marker.Outcome != TokenBudgetExceededOutcome || marker.MaxTokens <= 0 {
		return TokenBudgetMarker{}, false
	}
	if strings.TrimSpace(marker.Measure) == "" {
		marker.Measure = TokenBudgetMeasureProviderTotalLegacy
	}
	return marker, true
}

// ReadTokenBudgetMarkerForAttempt rejects a marker written by a predecessor
// process that reused the same stable slot log path. New markers carry the
// exact worker generation. Timestamp matching is retained only for markers
// written by an older Maestro version during a rolling upgrade.
func ReadTokenBudgetMarkerForAttempt(logFile string, workerGeneration uint64, startedAt time.Time) (TokenBudgetMarker, bool) {
	marker, ok := ReadTokenBudgetMarker(logFile)
	if !ok {
		return TokenBudgetMarker{}, false
	}
	if marker.WorkerGeneration > 0 {
		if workerGeneration == 0 || marker.WorkerGeneration != workerGeneration {
			return TokenBudgetMarker{}, false
		}
		return marker, true
	}
	if !startedAt.IsZero() && (marker.MeasuredAt.IsZero() || marker.MeasuredAt.Before(startedAt)) {
		return TokenBudgetMarker{}, false
	}
	return marker, true
}

func writeTokenBudgetMarker(path, backend, measure string, observed, maxTokens int, workerGeneration uint64) error {
	if path == "" {
		return fmt.Errorf("empty token-budget marker path")
	}
	if observed < maxTokens {
		observed = maxTokens
	}
	marker := TokenBudgetMarker{
		Outcome:          TokenBudgetExceededOutcome,
		Backend:          backend,
		TokensObserved:   observed,
		MaxTokens:        maxTokens,
		Measure:          measure,
		WorkerGeneration: workerGeneration,
		MeasuredAt:       time.Now().UTC(),
	}
	data, err := json.Marshal(marker)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// StopCurrentProcessGroup terminates the tmux pane's worker pipeline. The
// stream-split process runs in the same process group as the backend and tee;
// signaling the group prevents an agent that ignores a broken stdout pipe from
// continuing to consume tokens after the marker is durable.
func StopCurrentProcessGroup() {
	_ = syscall.Kill(-syscall.Getpgrp(), syscall.SIGTERM)
}

type liveTokenMonitor struct {
	backend          string
	measure          string
	maxTokens        int
	totalTokens      int
	claudeAssistants int
	claudeMessages   map[string]int
	codexThreadID    string
}

func newLiveTokenMonitor(backend string, maxTokens int) *liveTokenMonitor {
	return &liveTokenMonitor{
		backend:        strings.TrimSpace(backend),
		measure:        TokenBudgetMeasureForBackend(backend),
		maxTokens:      maxTokens,
		claudeMessages: make(map[string]int),
	}
}

// TokenBudgetMeasureForBackend names the exact measure enforced by the live
// stream monitor. Claude and Pi exclude cache reads; the other structured
// backends retain their native token definitions.
func TokenBudgetMeasureForBackend(backend string) string {
	switch strings.TrimSpace(backend) {
	case "claude", "pi":
		return TokenBudgetMeasureUncached
	case "codex":
		return TokenBudgetMeasureCodexRollout
	case "opencode":
		return TokenBudgetMeasureInputOutputReasoning
	default:
		return TokenBudgetMeasureProviderTotal
	}
}

func (m *liveTokenMonitor) observe(line string) (int, bool) {
	if m == nil || m.maxTokens <= 0 {
		return 0, false
	}
	trimmed := strings.TrimSpace(line)
	if trimmed == "" || trimmed[0] != '{' {
		return m.totalTokens, false
	}

	switch m.backend {
	case "claude":
		var fr claudeStreamFrame
		if json.Unmarshal([]byte(trimmed), &fr) != nil {
			break
		}
		switch fr.Type {
		case "assistant":
			if fr.Message != nil && fr.Message.Usage != nil {
				measured := claudeBudgetUsage(fr.Message.Usage)
				messageID := strings.TrimSpace(fr.Message.ID)
				if messageID == "" {
					m.claudeAssistants += measured
				} else if previous := m.claudeMessages[messageID]; measured > previous {
					m.claudeAssistants += measured - previous
					m.claudeMessages[messageID] = measured
				}
				if m.claudeAssistants > m.totalTokens {
					m.totalTokens = m.claudeAssistants
				}
			}
		case "result":
			if fr.Usage != nil {
				if total := claudeBudgetUsage(fr.Usage); total > m.totalTokens {
					m.totalTokens = total
				}
			}
		}
	case "codex":
		var fr struct {
			Type     string           `json:"type"`
			ThreadID string           `json:"thread_id"`
			Usage    *codexUsageBlock `json:"usage"`
			Error    *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal([]byte(trimmed), &fr) != nil {
			break
		}
		if fr.Type == "thread.started" && strings.TrimSpace(fr.ThreadID) != "" {
			m.codexThreadID = strings.TrimSpace(fr.ThreadID)
		}
		if m.codexThreadID != "" && (strings.HasPrefix(fr.Type, "item.") || strings.HasPrefix(fr.Type, "turn.")) {
			if total, ok := readCodexLiveUsage(m.codexThreadID); ok && total > m.totalTokens {
				m.totalTokens = total
			}
		}
		if fr.Type == "turn.completed" && fr.Usage != nil {
			if total := fr.Usage.InputTokens + fr.Usage.OutputTokens; total > m.totalTokens {
				m.totalTokens = total
			}
		}
		if fr.Type == "turn.failed" && fr.Error != nil && strings.Contains(strings.ToLower(fr.Error.Message), "rollout token budget exhausted") {
			m.totalTokens = m.maxTokens
		}
	case "pi":
		var ev piUsageEvent
		if json.Unmarshal([]byte(trimmed), &ev) == nil && ev.Type == "turn_end" && ev.Message != nil && ev.Message.Usage != nil {
			m.totalTokens += piBudgetUsage(ev.Message.Usage)
		}
	case "opencode":
		var fr opencodeStreamFrame
		if json.Unmarshal([]byte(trimmed), &fr) == nil && fr.Type == "step_finish" && fr.Part != nil && fr.Part.Tokens != nil {
			m.totalTokens += fr.Part.Tokens.Input + fr.Part.Tokens.Output + fr.Part.Tokens.Reasoning
		}
	}

	return m.totalTokens, m.totalTokens >= m.maxTokens
}

func readCodexLiveUsage(threadID string) (int, bool) {
	threadID = strings.TrimSpace(threadID)
	if threadID == "" {
		return 0, false
	}
	home := strings.TrimSpace(os.Getenv("CODEX_HOME"))
	if home == "" {
		userHome, err := os.UserHomeDir()
		if err != nil {
			return 0, false
		}
		home = filepath.Join(userHome, ".codex")
	}
	paths, err := filepath.Glob(filepath.Join(home, "sessions", "*", "*", "*", "rollout-*"+threadID+".jsonl"))
	if err != nil || len(paths) == 0 {
		return 0, false
	}
	f, err := os.Open(paths[len(paths)-1])
	if err != nil {
		return 0, false
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return 0, false
	}
	const tailBytes int64 = 256 * 1024
	start := info.Size() - tailBytes
	if start < 0 {
		start = 0
	}
	data := make([]byte, info.Size()-start)
	if _, err := f.ReadAt(data, start); err != nil && len(data) == 0 {
		return 0, false
	}
	if start > 0 {
		if newline := strings.IndexByte(string(data), '\n'); newline >= 0 {
			data = data[newline+1:]
		}
	}
	maxTokens := 0
	for _, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || line[0] != '{' {
			continue
		}
		var event struct {
			Type    string `json:"type"`
			Payload *struct {
				Type string `json:"type"`
				Info *struct {
					Total *struct {
						InputTokens  int `json:"input_tokens"`
						OutputTokens int `json:"output_tokens"`
						TotalTokens  int `json:"total_tokens"`
					} `json:"total_token_usage"`
				} `json:"info"`
			} `json:"payload"`
		}
		if json.Unmarshal([]byte(line), &event) != nil || event.Type != "event_msg" || event.Payload == nil || event.Payload.Type != "token_count" || event.Payload.Info == nil || event.Payload.Info.Total == nil {
			continue
		}
		total := event.Payload.Info.Total.TotalTokens
		if total <= 0 {
			total = event.Payload.Info.Total.InputTokens + event.Payload.Info.Total.OutputTokens
		}
		if total > maxTokens {
			maxTokens = total
		}
	}
	return maxTokens, maxTokens > 0
}

func claudeUsageTotal(usage *claudeUsageBlock) int {
	if usage == nil {
		return 0
	}
	return usage.InputTokens + usage.OutputTokens + usage.CacheCreationInputTokens + usage.CacheReadInputTokens
}

func piUsageTotal(usage *piUsageBlock) int {
	if usage == nil {
		return 0
	}
	return usage.Input + usage.Output + usage.CacheRead + usage.CacheWrite
}

// claudeBudgetUsage is the live ceiling measure, intentionally distinct from
// claudeUsageTotal (provider/cost telemetry). Cache reads are already-produced
// context replayed into a later turn; charging them again makes the ceiling
// grow by the whole context on every assistant response. The ceiling therefore
// counts only uncached input, output, and freshly-created cache tokens.
func claudeBudgetUsage(usage *claudeUsageBlock) int {
	if usage == nil {
		return 0
	}
	return usage.InputTokens + usage.OutputTokens + usage.CacheCreationInputTokens
}

func piBudgetUsage(usage *piUsageBlock) int {
	if usage == nil {
		return 0
	}
	return usage.Input + usage.Output + usage.CacheWrite
}
