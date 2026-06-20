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
// text for the worker log. Only the claude backend is rendered today; any
// other backend (and any non-JSON or unrecognized line) passes through
// verbatim, which preserves stderr error text so the rate-limit /
// auth-failure text parsers keep matching (#737 acceptance).
func renderStreamLine(backend, line string) string {
	if strings.TrimSpace(backend) != "claude" {
		return line
	}
	return renderClaudeStreamLine(line)
}

// RenderClaudeStreamLine is the exported entry point for rendering a single
// claude stream-json line (used by tests verifying the renderer keeps
// rate-limit / auth-failure text intact).
func RenderClaudeStreamLine(line string) string {
	return renderClaudeStreamLine(line)
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
