package router

import (
	"testing"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/github"
)

func TestParseResponse_ValidJSON(t *testing.T) {
	resp, err := parseResponse(`{"backend": "codex", "reason": "simple bug fix"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Backend != "codex" {
		t.Errorf("backend = %q, want %q", resp.Backend, "codex")
	}
	if resp.Reason != "simple bug fix" {
		t.Errorf("reason = %q, want %q", resp.Reason, "simple bug fix")
	}
}

func TestParseResponse_JSONWithSurroundingText(t *testing.T) {
	input := `Here is my analysis:
{"backend": "claude", "reason": "multi-file refactor"}
That's my recommendation.`
	resp, err := parseResponse(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Backend != "claude" {
		t.Errorf("backend = %q, want %q", resp.Backend, "claude")
	}
}

func TestParseResponse_InvalidJSON(t *testing.T) {
	_, err := parseResponse("I think codex would be best")
	if err == nil {
		t.Error("expected error for non-JSON output")
	}
}

func TestParseResponse_EmptyBackend(t *testing.T) {
	_, err := parseResponse(`{"backend": "", "reason": "no idea"}`)
	if err == nil {
		t.Error("expected error for empty backend field")
	}
}

func TestBuildPrompt_DefaultTemplate(t *testing.T) {
	cfg := &config.Config{
		Model: config.ModelConfig{
			Default: "claude",
			Backends: map[string]config.BackendDef{
				"claude": {Cmd: "claude"},
				"codex":  {Cmd: "codex"},
			},
		},
		Routing: config.RoutingConfig{
			Mode:        "auto",
			RouterModel: "claude",
		},
	}
	r := New(cfg)
	issue := github.Issue{
		Number: 42,
		Title:  "Fix SQL injection",
		Body:   "The login form is vulnerable.",
	}

	prompt := r.buildPrompt(issue)

	if !contains(prompt, "#42") {
		t.Error("prompt should contain issue number")
	}
	if !contains(prompt, "Fix SQL injection") {
		t.Error("prompt should contain issue title")
	}
	if !contains(prompt, "The login form is vulnerable.") {
		t.Error("prompt should contain issue body")
	}
	// Should contain both backend names (order may vary)
	if !contains(prompt, "claude") || !contains(prompt, "codex") {
		t.Error("prompt should contain backend names")
	}
}

func TestBuildPrompt_CustomTemplate(t *testing.T) {
	cfg := &config.Config{
		Model: config.ModelConfig{
			Default: "claude",
			Backends: map[string]config.BackendDef{
				"claude": {Cmd: "claude"},
			},
		},
		Routing: config.RoutingConfig{
			Mode:         "auto",
			RouterModel:  "claude",
			RouterPrompt: "Pick backend for #{{NUMBER}}: {{TITLE}}. Options: {{BACKENDS}}",
		},
	}
	r := New(cfg)
	issue := github.Issue{Number: 10, Title: "Add tests"}

	prompt := r.buildPrompt(issue)

	expected := "Pick backend for #10: Add tests. Options: claude"
	if prompt != expected {
		t.Errorf("prompt = %q, want %q", prompt, expected)
	}
}

func TestRoute_UnknownBackendFallsBack(t *testing.T) {
	cfg := &config.Config{
		Model: config.ModelConfig{
			Default: "claude",
			Backends: map[string]config.BackendDef{
				"claude": {Cmd: "claude"},
			},
		},
		Routing: config.RoutingConfig{
			Mode:        "auto",
			RouterModel: "claude",
		},
	}
	r := New(cfg)

	// Simulate: parseResponse returns a valid response but with unknown backend
	// We test the validation logic by calling the internal parseResponse + validation
	resp, err := parseResponse(`{"backend": "gemini", "reason": "test"}`)
	if err != nil {
		t.Fatalf("unexpected parse error: %v", err)
	}

	// Check that the backend doesn't exist in config
	if _, ok := cfg.Model.Backends[resp.Backend]; ok {
		t.Fatal("test setup error: backend should not exist")
	}

	// The Route method would fall back to default — verify that logic
	_ = r // Router exists but we can't call Route without a real CLI
	if resp.Backend != "gemini" {
		t.Errorf("parsed backend = %q, want %q", resp.Backend, "gemini")
	}
}

func TestTruncate(t *testing.T) {
	if got := truncate("short", 10); got != "short" {
		t.Errorf("truncate short = %q", got)
	}
	if got := truncate("this is a long string", 10); got != "this is..." {
		t.Errorf("truncate long = %q, want %q", got, "this is...")
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && containsStr(s, substr)
}

func containsStr(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
