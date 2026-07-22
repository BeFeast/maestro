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

// This fixture is a sanitized 2026-07-21 live capture from Claude Code 2.1.216
// running `--model grok-4.5` through CLIProxyAPI. Identifiers/timing were
// removed, while the configured/init model, translated assistant model, frame
// shapes, and usage/cost values are retained. The assistant frame reports
// zeros; the terminal result carries the authoritative non-zero totals.
func TestRunStreamSplit_ParsesCapturedNonAnthropicProxyStream(t *testing.T) {
	fixture, err := os.ReadFile(filepath.Join("testdata", "claude_proxy_grok_4_5_stream.jsonl"))
	if err != nil {
		t.Fatalf("read proxy fixture: %v", err)
	}
	dir := t.TempDir()
	jsonlPath := filepath.Join(dir, "sup-946.jsonl")
	var rendered strings.Builder
	if err := RunStreamSplit("claude", jsonlPath, strings.NewReader(string(fixture)), &rendered); err != nil {
		t.Fatalf("RunStreamSplit: %v", err)
	}

	raw, err := os.ReadFile(jsonlPath)
	if err != nil {
		t.Fatalf("read split side channel: %v", err)
	}
	if string(raw) != string(fixture) {
		t.Fatal("stream-split did not preserve the captured proxy frames verbatim")
	}
	usage, ok := ParseClaudeUsage(string(raw))
	if !ok {
		t.Fatal("captured non-Anthropic result usage was not parsed")
	}
	if !usage.UsageUnreliable || usage.UsageUnreliableScope != claudeUsageScopeLiveBudget || usage.UsageUnreliableReason != claudeUsageZeroLiveAssistant {
		t.Fatalf("captured zero assistant usage did not mark live-budget degradation: %+v", usage)
	}
	if usage.Model != "grok-4.5" || usage.Input != 24762 || usage.Output != 22 || usage.CacheRead != 1280 || usage.TotalTokens != 26064 {
		t.Fatalf("parsed proxy usage = %+v, want grok-4.5 24762/22/1280 total=26064", usage)
	}
	if usage.CostUSD != 0.125 {
		t.Fatalf("CostUSD = %v, want 0.125", usage.CostUSD)
	}
	if !strings.Contains(rendered.String(), "PONG") || !strings.Contains(rendered.String(), "[claude] result:") {
		t.Fatalf("rendered proxy stream lost worker output:\n%s", rendered.String())
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

// RunStreamSplit with the codex backend must append the raw NDJSON frames
// verbatim to the jsonl side channel (so ParseCodexUsage can read them) while
// rendering human-readable text to stdout (slot.log) — no raw usage JSON.
func TestRunStreamSplit_Codex(t *testing.T) {
	dir := t.TempDir()
	jsonlPath := filepath.Join(dir, "sup-1.jsonl")

	var out strings.Builder
	if err := RunStreamSplit("codex", jsonlPath, strings.NewReader(codexSmokeStream), &out); err != nil {
		t.Fatalf("RunStreamSplit: %v", err)
	}

	// (a) the jsonl side channel round-trips usage through the parser.
	raw, err := os.ReadFile(jsonlPath)
	if err != nil {
		t.Fatalf("read jsonl: %v", err)
	}
	if usage, ok := ParseCodexUsage(string(raw)); !ok || usage.TotalTokens != 12092 {
		t.Fatalf("jsonl side channel did not round-trip usage: ok=%v usage=%+v", ok, usage)
	}

	// (b) stdout (slot.log) is human-readable: the assistant text and a usage
	// summary are present; the raw usage JSON envelope is not echoed verbatim.
	rendered := out.String()
	if !strings.Contains(rendered, "pong") {
		t.Errorf("rendered output missing assistant text:\n%s", rendered)
	}
	if strings.Contains(rendered, `"cached_input_tokens"`) {
		t.Errorf("rendered slot.log should not contain raw usage JSON:\n%s", rendered)
	}
	if !strings.Contains(rendered, "[codex] usage:") {
		t.Errorf("rendered output missing usage summary:\n%s", rendered)
	}
}

func TestRunStreamSplit_KimiFixture(t *testing.T) {
	fixture, err := os.ReadFile(filepath.Join("testdata", "kimi_stream_usage.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	jsonlPath := filepath.Join(dir, "kimi.jsonl")
	var out strings.Builder
	if err := RunStreamSplit("kimi", jsonlPath, strings.NewReader(string(fixture)), &out); err != nil {
		t.Fatalf("RunStreamSplit: %v", err)
	}

	raw, err := os.ReadFile(jsonlPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != string(fixture) {
		t.Fatal("stream-split did not preserve Kimi JSONL verbatim")
	}
	usage, ok := ParseKimiUsage(string(raw))
	if !ok || usage.TotalTokens != 2600 {
		t.Fatalf("Kimi split usage = %+v, ok=%t, want total 2600", usage, ok)
	}
	rendered := out.String()
	for _, want := range []string{"Inspecting the repository.", "[tool_use: Shell]", "Implementation complete.", "[kimi] usage:"} {
		if !strings.Contains(rendered, want) {
			t.Errorf("rendered Kimi output missing %q:\n%s", want, rendered)
		}
	}
	if strings.Contains(rendered, `"input_cache_read"`) {
		t.Fatalf("rendered Kimi log leaked raw usage JSON:\n%s", rendered)
	}
}

// The codex renderer must turn item events into readable text: a command and
// its output/exit, a file change, and an agent message — never raw item JSON.
func TestRenderCodexStreamLine_ItemsAreReadable(t *testing.T) {
	cases := []struct {
		name string
		line string
		want []string
	}{
		{
			"command start",
			`{"type":"item.started","item":{"id":"i1","type":"command_execution","command":"/bin/bash -lc ls","status":"in_progress"}}`,
			[]string{"[codex] $ /bin/bash -lc ls"},
		},
		{
			"command completed",
			`{"type":"item.completed","item":{"id":"i1","type":"command_execution","command":"/bin/bash -lc ls","aggregated_output":"a.txt\nb.txt\n","exit_code":0,"status":"completed"}}`,
			[]string{"a.txt", "b.txt", "[codex] exit=0"},
		},
		{
			"file change",
			`{"type":"item.completed","item":{"id":"i3","type":"file_change","changes":[{"path":"/wt/hello.txt","kind":"add"}],"status":"completed"}}`,
			[]string{"[codex] add /wt/hello.txt"},
		},
		{
			"agent message",
			`{"type":"item.completed","item":{"id":"i0","type":"agent_message","text":"all done"}}`,
			[]string{"all done"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := RenderCodexStreamLine(tc.line)
			if strings.Contains(got, "\"type\":") {
				t.Errorf("rendered line leaked raw JSON: %q", got)
			}
			for _, want := range tc.want {
				if !strings.Contains(got, want) {
					t.Errorf("rendered = %q, want substring %q", got, want)
				}
			}
		})
	}
}

// #738 acceptance: routing rate-limit / auth-failure signatures through the
// codex renderer must not break the text parsers. Plain stderr lines pass
// through verbatim; an unrecognized JSON event is preserved verbatim too, so
// either way the detectors fire.
func TestRenderCodexStreamLine_PreservesDetectionSignatures(t *testing.T) {
	signatures := []struct {
		name   string
		text   string
		detect func(string) bool
	}{
		{"codex rate limit (stderr passthrough)", "Error: rate limit exceeded, please try again later", OutputContainsRateLimit},
		{"http 429 (stderr passthrough)", "Request failed with status 429", OutputContainsRateLimit},
		{"auth failure (stderr passthrough)", "API Error: 401 Invalid authentication credentials", func(s string) bool { hit, _ := DetectAuthFailure(s); return hit }},
	}
	for _, sig := range signatures {
		t.Run(sig.name, func(t *testing.T) {
			if got := RenderCodexStreamLine(sig.text); got != sig.text {
				t.Fatalf("plain line not passed through verbatim:\ngot  %q\nwant %q", got, sig.text)
			}
			if !sig.detect(RenderCodexStreamLine(sig.text)) {
				t.Errorf("detector regressed on rendered plain line: %q", sig.text)
			}
			// An unknown JSON event carrying the signature must survive too.
			frame := `{"type":"error","message":` + jsonQuote(sig.text) + `}`
			if !sig.detect(RenderCodexStreamLine(frame)) {
				t.Errorf("detector regressed on rendered unknown event:\nframe: %s", frame)
			}
		})
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
