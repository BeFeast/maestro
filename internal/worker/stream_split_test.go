package worker

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestJSONLPathForLog(t *testing.T) {
	cases := map[string]string{
		"/logs/sup-1.log":           "/logs/sup-1.jsonl",
		"/logs/sup-1-implement.log": "/logs/sup-1-implement.jsonl",
		"/logs/sup-1":               "/logs/sup-1.jsonl",
		"":                          "",
	}
	for in, want := range cases {
		if got := JSONLPathForLog(in); got != want {
			t.Errorf("JSONLPathForLog(%q) = %q, want %q", in, got, want)
		}
	}
}

// RunStreamSplit must append the raw NDJSON frames verbatim to the jsonl side
// channel (so ParseClaudeUsage can read them) while rendering human-readable
// text to stdout (slot.log).
func TestRunStreamSplit_SplitsRawAndRendered(t *testing.T) {
	dir := t.TempDir()
	jsonlPath := filepath.Join(dir, "sup-1.jsonl")

	var out strings.Builder
	if err := RunStreamSplit("claude", jsonlPath, strings.NewReader(claudeSmokeStream), &out); err != nil {
		t.Fatalf("RunStreamSplit: %v", err)
	}

	// (a) the jsonl side channel is a faithful raw capture the parser accepts.
	raw, err := os.ReadFile(jsonlPath)
	if err != nil {
		t.Fatalf("read jsonl: %v", err)
	}
	if usage, ok := ParseClaudeUsage(string(raw)); !ok || usage.TotalTokens != 20076 {
		t.Fatalf("jsonl side channel did not round-trip usage: ok=%v usage=%+v", ok, usage)
	}

	// (b) stdout (slot.log) is human-readable: the final answer is present and
	// the raw frame envelope is not echoed verbatim.
	rendered := out.String()
	if !strings.Contains(rendered, "pong") {
		t.Errorf("rendered output missing assistant text:\n%s", rendered)
	}
	if strings.Contains(rendered, `"cache_read_input_tokens"`) {
		t.Errorf("rendered slot.log should not contain raw usage JSON:\n%s", rendered)
	}
	if !strings.Contains(rendered, "[claude] result:") {
		t.Errorf("rendered output missing result summary:\n%s", rendered)
	}
}

// If the jsonl path cannot be opened, RunStreamSplit must degrade to
// pass-through so the worker log is never lost.
func TestRunStreamSplit_DegradesWhenJSONLUnwritable(t *testing.T) {
	// A path whose parent does not exist cannot be opened.
	badPath := filepath.Join(t.TempDir(), "missing-dir", "sup-1.jsonl")
	var out strings.Builder
	if err := RunStreamSplit("claude", badPath, strings.NewReader(claudeSmokeStream), &out); err != nil {
		t.Fatalf("RunStreamSplit should not fail the pipeline on jsonl open error: %v", err)
	}
	if !strings.Contains(out.String(), "pong") {
		t.Errorf("stream should still pass through to stdout when jsonl is unwritable:\n%s", out.String())
	}
}

// A non-claude backend passes every line through verbatim.
func TestRunStreamSplit_NonClaudePassthrough(t *testing.T) {
	var out strings.Builder
	in := "plain line one\nplain line two\n"
	if err := RunStreamSplit("codex", "", strings.NewReader(in), &out); err != nil {
		t.Fatalf("RunStreamSplit: %v", err)
	}
	if out.String() != in {
		t.Errorf("non-claude passthrough = %q, want %q", out.String(), in)
	}
}

// #737 acceptance: routing the rate-limit / auth-failure fixtures through the
// claude renderer must not break the text parsers. Plain stderr lines pass
// through verbatim; the same signatures embedded in a stream-json result
// frame surface via the rendered result text. Either way the detectors fire.
func TestRenderClaudeStreamLine_PreservesDetectionSignatures(t *testing.T) {
	signatures := []struct {
		name string
		text string
		// detect reports whether the renderer output still trips the parser.
		detect func(string) bool
	}{
		{"claude rate limit (stderr passthrough)", "Error: You've hit your limit for Claude. Please wait before trying again.", OutputContainsRateLimit},
		{"http 429 (stderr passthrough)", "Request failed with status 429", OutputContainsRateLimit},
		{"auth failure (stderr passthrough)", "Failed to authenticate. API Error: 401 Invalid authentication credentials", func(s string) bool { hit, _ := DetectAuthFailure(s); return hit }},
	}

	for _, sig := range signatures {
		t.Run(sig.name+"/plain", func(t *testing.T) {
			// Plain (non-JSON) stderr lines must pass through unchanged.
			if got := RenderClaudeStreamLine(sig.text); got != sig.text {
				t.Fatalf("plain line not passed through verbatim:\ngot  %q\nwant %q", got, sig.text)
			}
			if !sig.detect(RenderClaudeStreamLine(sig.text)) {
				t.Errorf("detector regressed on rendered plain line: %q", sig.text)
			}
		})

		t.Run(sig.name+"/in-result-frame", func(t *testing.T) {
			frame := `{"type":"result","subtype":"error_during_execution","is_error":true,"num_turns":1,"result":` +
				jsonQuote(sig.text) + `,"total_cost_usd":0,"usage":{"input_tokens":1,"output_tokens":1,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}`
			rendered := RenderClaudeStreamLine(frame)
			if !sig.detect(rendered) {
				t.Errorf("detector regressed on rendered result frame:\nframe:    %s\nrendered: %s", frame, rendered)
			}
		})
	}
}

// jsonQuote returns a JSON string literal for s (small local helper to embed
// detection signatures inside a result frame without pulling in encoding/json).
func jsonQuote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}
