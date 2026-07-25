package worker

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
)

// JSONLPathForLog returns the side-channel NDJSON path paired with a worker
// log path: slot.log -> slot.jsonl. The stream-splitter appends raw claude
// stream-json frames here while slot.log keeps the rendered human-readable
// text. A path without a .log suffix simply gets .jsonl appended.
func JSONLPathForLog(logFile string) string {
	if logFile == "" {
		return ""
	}
	if strings.HasSuffix(logFile, ".log") {
		return strings.TrimSuffix(logFile, ".log") + ".jsonl"
	}
	return logFile + ".jsonl"
}

// RunStreamSplit reads a backend's structured NDJSON stream from r, appends
// every raw line verbatim to jsonlPath (the machine-readable side-channel the
// usage parser consumes), and writes human-readable rendered text to w (the
// worker log via tee). It is deliberately resilient: a line that cannot be
// opened/decoded still passes through to w, and if jsonlPath cannot be opened
// it degrades to pure pass-through so the worker log is never lost. Output is
// flushed per line so the orchestrator's live log polling (rate-limit /
// silent-timeout detection) sees output in real time.
func RunStreamSplit(backend, jsonlPath string, r io.Reader, w io.Writer) error {
	return RunStreamSplitWithBudget(backend, jsonlPath, 0, "", 0, r, w, nil)
}

// RunStreamSplitWithBudget adds live token enforcement to the structured
// stream. Usage is evaluated once per provider response/event, so measurement
// lag is bounded to one backend response plus line-flush time.
func RunStreamSplitWithBudget(backend, jsonlPath string, maxTokens int, markerPath string, workerGeneration uint64, r io.Reader, w io.Writer, stop func()) error {
	var jf *os.File
	if strings.TrimSpace(jsonlPath) != "" {
		f, err := os.OpenFile(jsonlPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[maestro] stream-split: cannot open %s: %v (passing stream through)\n", jsonlPath, err)
		} else {
			jf = f
			defer jf.Close()
		}
	}

	br := bufio.NewReader(r)
	bw := bufio.NewWriter(w)
	defer bw.Flush()
	monitor := newLiveTokenMonitor(backend, maxTokens)

	for {
		// ReadString tolerates arbitrarily long frames (tool results / large
		// diffs) that would overflow a bufio.Scanner token.
		chunk, readErr := br.ReadString('\n')
		if len(chunk) > 0 {
			if jf != nil {
				// Append the raw frame verbatim (newline included) so the
				// side-channel is a faithful NDJSON capture for the parser.
				if _, err := jf.WriteString(chunk); err != nil {
					fmt.Fprintf(os.Stderr, "[maestro] stream-split: write %s: %v\n", jsonlPath, err)
				}
			}
			line := strings.TrimRight(chunk, "\r\n")
			if observed, exceeded := monitor.observe(line); exceeded {
				if err := writeTokenBudgetMarker(markerPath, backend, monitor.measure, observed, maxTokens, workerGeneration); err != nil {
					if stop != nil {
						stop()
					}
					return fmt.Errorf("write token-budget marker: %w", err)
				}
				bw.Flush()
				if jf != nil {
					_ = jf.Sync()
				}
				if stop != nil {
					stop()
				}
				return ErrTokenBudgetExceeded
			}
			if rendered := renderStreamLine(backend, line); rendered != "" {
				bw.WriteString(rendered)
				if !strings.HasSuffix(rendered, "\n") {
					bw.WriteByte('\n')
				}
				bw.Flush()
			}
		}
		if readErr != nil {
			if readErr == io.EOF {
				return nil
			}
			return readErr
		}
	}
}

// renderStreamLine converts one structured stream line into human-readable
// text for the worker log. claude (#737) and codex (#738) are rendered; any
// other backend (and any non-JSON or unrecognized line) passes through
// verbatim, which preserves stderr error text so the rate-limit /
// auth-failure text parsers keep matching (acceptance criterion).
func renderStreamLine(backend, line string) string {
	switch strings.TrimSpace(backend) {
	case "claude":
		return renderClaudeStreamLine(line)
	case "codex":
		return renderCodexStreamLine(line)
	case "kimi":
		return renderKimiStreamLine(line)
	case "opencode":
		return renderOpenCodeStreamLine(line)
	default:
		return line
	}
}

// RenderClaudeStreamLine is the exported entry point for rendering a single
// claude stream-json line (used by tests verifying the renderer keeps
// rate-limit / auth-failure text intact).
func RenderClaudeStreamLine(line string) string {
	return renderClaudeStreamLine(line)
}

// RenderCodexStreamLine is the exported entry point for rendering a single
// codex `exec --json` line (used by tests verifying the renderer keeps
// rate-limit / auth-failure text intact).
func RenderCodexStreamLine(line string) string {
	return renderCodexStreamLine(line)
}

// RenderKimiStreamLine is the exported entry point for rendering one Kimi
// Code CLI stream-json line.
func RenderKimiStreamLine(line string) string {
	return renderKimiStreamLine(line)
}

type kimiRenderToolCall struct {
	Function struct {
		Name string `json:"name"`
	} `json:"function"`
}

// renderKimiStreamLine renders Kimi's Message-shaped JSONL into readable log
// text while preserving unknown/error frames verbatim. Usage-only frames get a
// compact summary; raw usage JSON remains in the side channel.
func renderKimiStreamLine(line string) string {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" || trimmed[0] != '{' {
		return line
	}
	root, ok := decodeJSONObject([]byte(trimmed))
	if !ok {
		return line
	}
	role := firstJSONString(root, "role")
	switch role {
	case "assistant":
		parts := make([]string, 0, 2)
		if raw, ok := root["content"]; ok {
			if content := renderClaudeContent(raw); strings.TrimSpace(content) != "" {
				parts = append(parts, content)
			}
		}
		var calls []kimiRenderToolCall
		if raw, ok := root["tool_calls"]; ok && json.Unmarshal(raw, &calls) == nil {
			for _, call := range calls {
				name := strings.TrimSpace(call.Function.Name)
				if name == "" {
					name = "unknown"
				}
				parts = append(parts, "[tool_use: "+name+"]")
			}
		}
		if len(parts) > 0 {
			return strings.Join(parts, "\n")
		}
	case "tool":
		content := ""
		if raw, ok := root["content"]; ok {
			content = renderClaudeContent(raw)
		}
		if strings.TrimSpace(content) == "" {
			return "[tool_result]"
		}
		return "[tool_result] " + content
	case "meta":
		if content := firstJSONString(root, "content", "error_message"); content != "" {
			return "[kimi] " + content
		}
	}
	if usageMap, _, ok := findKimiUsageMap(root); ok {
		usage := decodeKimiUsageBlock(usageMap)
		if kimiUsageTotal(usage) > 0 {
			return fmt.Sprintf("[kimi] usage: input=%d cached=%d cache_write=%d output=%d",
				usage.Input, usage.CacheRead, usage.CacheWrite, usage.Output)
		}
	}
	return line
}

type claudeRenderFrame struct {
	Type          string               `json:"type"`
	Subtype       string               `json:"subtype"`
	Model         string               `json:"model"`
	Message       *claudeRenderMessage `json:"message"`
	Result        string               `json:"result"`
	IsError       bool                 `json:"is_error"`
	NumTurns      int                  `json:"num_turns"`
	TotalCostUSD  float64              `json:"total_cost_usd"`
	RateLimitInfo json.RawMessage      `json:"rate_limit_info"`
}

type claudeRenderMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type claudeContentBlock struct {
	Type    string          `json:"type"`
	Text    string          `json:"text"`
	Name    string          `json:"name"`
	Content json.RawMessage `json:"content"`
	IsError bool            `json:"is_error"`
}

// renderClaudeStreamLine renders one claude stream-json frame. A line that is
// not a JSON object, or fails to decode, is returned verbatim so nothing is
// lost from the worker log (and any plain stderr error text still reaches the
// text parsers).
func renderClaudeStreamLine(line string) string {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" || trimmed[0] != '{' {
		return line
	}
	var fr claudeRenderFrame
	if err := json.Unmarshal([]byte(trimmed), &fr); err != nil {
		return line
	}
	switch fr.Type {
	case "system":
		if m := strings.TrimSpace(fr.Model); m != "" {
			return "[claude] model: " + m
		}
		return ""
	case "assistant", "user":
		if fr.Message == nil {
			return ""
		}
		return renderClaudeContent(fr.Message.Content)
	case "result":
		var b strings.Builder
		if txt := strings.TrimSpace(fr.Result); txt != "" {
			b.WriteString(fr.Result)
			b.WriteByte('\n')
		}
		fmt.Fprintf(&b, "[claude] result: subtype=%s is_error=%t turns=%d cost=$%.4f",
			fr.Subtype, fr.IsError, fr.NumTurns, fr.TotalCostUSD)
		return b.String()
	case "rate_limit_event":
		if len(fr.RateLimitInfo) > 0 {
			return "[claude] rate_limit: " + string(fr.RateLimitInfo)
		}
		return line
	default:
		// Unknown frame type — preserve it verbatim rather than drop it.
		return line
	}
}

// renderClaudeContent renders a message content field, which is either a JSON
// string or an array of content blocks (text / tool_use / tool_result).
func renderClaudeContent(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	// content may be a plain string (some user messages).
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []claudeContentBlock
	if json.Unmarshal(raw, &blocks) != nil {
		// Unexpected shape — surface the raw JSON rather than drop content.
		return string(raw)
	}
	parts := make([]string, 0, len(blocks))
	for _, blk := range blocks {
		switch blk.Type {
		case "text":
			if t := blk.Text; t != "" {
				parts = append(parts, t)
			}
		case "tool_use":
			parts = append(parts, "[tool_use: "+strings.TrimSpace(blk.Name)+"]")
		case "tool_result":
			label := "[tool_result]"
			if blk.IsError {
				label = "[tool_result error]"
			}
			if inner := renderClaudeContent(blk.Content); inner != "" {
				parts = append(parts, label+" "+inner)
			} else {
				parts = append(parts, label)
			}
		default:
			if t := blk.Text; t != "" {
				parts = append(parts, t)
			}
		}
	}
	return strings.Join(parts, "\n")
}

// codexRenderFrame is the subset of a codex `exec --json` line the renderer
// decodes. turn.completed carries the usage block (reused from codex_usage.go);
// item.started / item.completed carry the work item being rendered.
type codexRenderFrame struct {
	Type  string           `json:"type"`
	Item  *codexRenderItem `json:"item"`
	Usage *codexUsageBlock `json:"usage"`
}

// codexRenderItem is the subset of a codex item.* envelope the renderer needs:
// agent_message text, the executed command + its output/exit code, and file
// changes.
type codexRenderItem struct {
	Type             string            `json:"type"`
	Text             string            `json:"text"`
	Command          string            `json:"command"`
	AggregatedOutput string            `json:"aggregated_output"`
	ExitCode         *int              `json:"exit_code"`
	Changes          []codexFileChange `json:"changes"`
}

type codexFileChange struct {
	Path string `json:"path"`
	Kind string `json:"kind"`
}

// renderCodexStreamLine renders one codex `exec --json` event into
// human-readable text for slot.log (#738). A line that is not a JSON object,
// or fails to decode, is returned verbatim so nothing is lost from the worker
// log (and any plain stderr error text — rate-limit / auth-failure — still
// reaches the text parsers). Unknown top-level event types (e.g. turn.failed)
// are likewise preserved verbatim so error signatures survive.
func renderCodexStreamLine(line string) string {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" || trimmed[0] != '{' {
		return line
	}
	var fr codexRenderFrame
	if err := json.Unmarshal([]byte(trimmed), &fr); err != nil {
		return line
	}
	switch fr.Type {
	case "thread.started", "turn.started":
		// Structural envelopes with no human-facing content — drop.
		return ""
	case "item.started":
		return renderCodexItem(fr.Item, false)
	case "item.completed":
		return renderCodexItem(fr.Item, true)
	case "turn.completed":
		if fr.Usage == nil {
			return ""
		}
		return fmt.Sprintf("[codex] usage: input=%d cached=%d output=%d",
			fr.Usage.InputTokens, fr.Usage.CachedInputTokens, fr.Usage.OutputTokens)
	default:
		// Unknown event type — preserve it verbatim rather than drop it, so
		// error/rate-limit text carried in an unrecognized frame still matches.
		return line
	}
}

// renderCodexItem renders one codex work item. command_execution shows the
// command on start and its output + exit code on completion; agent_message and
// file_change render on completion. An unrecognized item type renders its text
// (if any) or a compact marker so slot.log never carries raw item JSON.
func renderCodexItem(item *codexRenderItem, completed bool) string {
	if item == nil {
		return ""
	}
	switch item.Type {
	case "agent_message":
		if completed {
			return item.Text
		}
		return ""
	case "command_execution":
		if !completed {
			return "[codex] $ " + strings.TrimSpace(item.Command)
		}
		var b strings.Builder
		if out := strings.TrimRight(item.AggregatedOutput, "\n"); out != "" {
			b.WriteString(out)
			b.WriteByte('\n')
		}
		exit := 0
		if item.ExitCode != nil {
			exit = *item.ExitCode
		}
		fmt.Fprintf(&b, "[codex] exit=%d", exit)
		return b.String()
	case "file_change":
		if !completed {
			return ""
		}
		parts := make([]string, 0, len(item.Changes))
		for _, c := range item.Changes {
			parts = append(parts, fmt.Sprintf("[codex] %s %s", strings.TrimSpace(c.Kind), strings.TrimSpace(c.Path)))
		}
		return strings.Join(parts, "\n")
	default:
		if completed {
			if t := strings.TrimSpace(item.Text); t != "" {
				return item.Text
			}
			if it := strings.TrimSpace(item.Type); it != "" {
				return "[codex] " + it
			}
		}
		return ""
	}
}

// --- OpenCode stream renderer ---

// opencodeRenderFrame is the subset of an opencode --format json event line the
// renderer decodes. step_finish carries the usage block with tokens/cost.
type opencodeRenderFrame struct {
	Type      string              `json:"type"`
	Timestamp int64               `json:"timestamp"`
	Part      *opencodeRenderPart `json:"part"`
}

type opencodeRenderPart struct {
	Type   string              `json:"type"`
	Text   string              `json:"text"`
	Reason string              `json:"reason"`
	Tokens *opencodeUsageBlock `json:"tokens"`
	Cost   float64             `json:"cost"`
}

// renderOpenCodeStreamLine renders one opencode --format json event into
// human-readable text for slot.log. A line that is not a JSON object, or
// fails to decode, is returned verbatim so nothing is lost from the worker
// log (and any plain stderr error text — rate-limit / auth-failure — still
// reaches the text parsers). Unknown top-level event types are likewise
// preserved verbatim so error signatures survive.
func renderOpenCodeStreamLine(line string) string {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" || trimmed[0] != '{' {
		return line
	}
	var fr opencodeRenderFrame
	if err := json.Unmarshal([]byte(trimmed), &fr); err != nil {
		return line
	}
	switch fr.Type {
	case "step_start":
		return ""
	case "text":
		if fr.Part != nil && fr.Part.Text != "" {
			return fr.Part.Text
		}
		return ""
	case "step_finish":
		if fr.Part == nil || fr.Part.Tokens == nil {
			return ""
		}
		var b strings.Builder
		tk := fr.Part.Tokens
		fmt.Fprintf(&b, "[opencode] usage: input=%d output=%d total=%d", tk.Input, tk.Output, tk.Total)
		if tk.Reasoning > 0 {
			fmt.Fprintf(&b, " reasoning=%d", tk.Reasoning)
		}
		if tk.Cache != nil && (tk.Cache.Read > 0 || tk.Cache.Write > 0) {
			fmt.Fprintf(&b, " cache_read=%d cache_write=%d", tk.Cache.Read, tk.Cache.Write)
		}
		fmt.Fprintf(&b, " cost=$%.4f", fr.Part.Cost)
		return b.String()
	default:
		return line
	}
}
