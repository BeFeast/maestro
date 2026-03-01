package router

import (
	"fmt"
	"testing"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/github"
)

func makeIssue(number int, title string, labels ...string) github.Issue {
	issue := github.Issue{Number: number, Title: title}
	for _, l := range labels {
		issue.Labels = append(issue.Labels, struct {
			Name string `json:"name"`
		}{Name: l})
	}
	return issue
}

func TestBackendFromLabels_ModelLabel(t *testing.T) {
	issue := makeIssue(1, "Fix bug", "enhancement", "model:codex")
	got := BackendFromLabels(issue)
	if got != "codex" {
		t.Errorf("BackendFromLabels() = %q, want %q", got, "codex")
	}
}

func TestBackendFromLabels_NoModelLabel(t *testing.T) {
	issue := makeIssue(2, "Add feature", "enhancement", "bug")
	got := BackendFromLabels(issue)
	if got != "" {
		t.Errorf("BackendFromLabels() = %q, want empty", got)
	}
}

func TestBackendFromLabels_NoLabels(t *testing.T) {
	issue := makeIssue(3, "Update docs")
	got := BackendFromLabels(issue)
	if got != "" {
		t.Errorf("BackendFromLabels() = %q, want empty", got)
	}
}

func TestBackendFromLabels_MultipleModelLabels_FirstWins(t *testing.T) {
	issue := makeIssue(4, "Complex", "model:gemini", "model:codex")
	got := BackendFromLabels(issue)
	if got != "gemini" {
		t.Errorf("BackendFromLabels() = %q, want %q (first model: label wins)", got, "gemini")
	}
}

func TestBackendFromLabels_EmptyModelValue(t *testing.T) {
	issue := makeIssue(5, "Edge case", "model:", "model:cline")
	got := BackendFromLabels(issue)
	if got != "cline" {
		t.Errorf("BackendFromLabels() = %q, want %q (empty model: should be skipped)", got, "cline")
	}
}

func TestBackendFromLabels_AllKnownBackends(t *testing.T) {
	backends := []string{"claude", "codex", "gemini", "cline"}
	for _, b := range backends {
		issue := makeIssue(10, "Test", "model:"+b)
		got := BackendFromLabels(issue)
		if got != b {
			t.Errorf("BackendFromLabels(model:%s) = %q, want %q", b, got, b)
		}
	}
}

func TestValidateBackend_Known(t *testing.T) {
	cfg := &config.Config{
		Model: config.ModelConfig{
			Default: "claude",
			Backends: map[string]config.BackendDef{
				"claude": {Cmd: "claude"},
				"codex":  {Cmd: "codex"},
			},
		},
	}
	name, ok := ValidateBackend("codex", cfg)
	if !ok || name != "codex" {
		t.Errorf("ValidateBackend(codex) = (%q, %v), want (%q, true)", name, ok, "codex")
	}
}

func TestValidateBackend_Unknown(t *testing.T) {
	cfg := &config.Config{
		Model: config.ModelConfig{
			Default: "claude",
			Backends: map[string]config.BackendDef{
				"claude": {Cmd: "claude"},
			},
		},
	}
	name, ok := ValidateBackend("nonexistent", cfg)
	if ok || name != "claude" {
		t.Errorf("ValidateBackend(nonexistent) = (%q, %v), want (%q, false)", name, ok, "claude")
	}
}

func TestResolveBackend_LabelOverride(t *testing.T) {
	cfg := &config.Config{
		Model: config.ModelConfig{
			Default: "claude",
			Backends: map[string]config.BackendDef{
				"claude": {Cmd: "claude"},
				"codex":  {Cmd: "codex"},
			},
		},
		Routing: config.RoutingConfig{Mode: "manual"},
	}
	r := New(cfg)

	issue := makeIssue(42, "Fix SQL injection", "enhancement", "model:codex")
	name, reason := r.ResolveBackend(issue)
	if name != "codex" {
		t.Errorf("ResolveBackend() name = %q, want %q", name, "codex")
	}
	if reason != "label" {
		t.Errorf("ResolveBackend() reason = %q, want %q", reason, "label")
	}
}

func TestResolveBackend_LabelOverride_UnknownBackend(t *testing.T) {
	cfg := &config.Config{
		Model: config.ModelConfig{
			Default: "claude",
			Backends: map[string]config.BackendDef{
				"claude": {Cmd: "claude"},
			},
		},
		Routing: config.RoutingConfig{Mode: "manual"},
	}
	r := New(cfg)

	issue := makeIssue(43, "Test unknown", "model:nonexistent")
	name, reason := r.ResolveBackend(issue)
	if name != "claude" {
		t.Errorf("ResolveBackend() name = %q, want %q (should fall back to default)", name, "claude")
	}
	if reason != "unknown label backend" {
		t.Errorf("ResolveBackend() reason = %q, want %q", reason, "unknown label backend")
	}
}

func TestResolveBackend_DefaultFallback(t *testing.T) {
	cfg := &config.Config{
		Model: config.ModelConfig{
			Default: "claude",
			Backends: map[string]config.BackendDef{
				"claude": {Cmd: "claude"},
				"codex":  {Cmd: "codex"},
			},
		},
		Routing: config.RoutingConfig{Mode: "manual"},
	}
	r := New(cfg)

	issue := makeIssue(44, "Add feature", "enhancement")
	name, reason := r.ResolveBackend(issue)
	if name != "claude" {
		t.Errorf("ResolveBackend() name = %q, want %q", name, "claude")
	}
	if reason != "default" {
		t.Errorf("ResolveBackend() reason = %q, want %q", reason, "default")
	}
}

func TestResolveBackend_LabelTakesPrecedenceOverAutoRouting(t *testing.T) {
	cfg := &config.Config{
		Model: config.ModelConfig{
			Default: "claude",
			Backends: map[string]config.BackendDef{
				"claude": {Cmd: "claude"},
				"codex":  {Cmd: "codex"},
				"gemini": {Cmd: "gemini"},
			},
		},
		Routing: config.RoutingConfig{
			Mode:        "auto",
			RouterModel: "claude",
		},
	}
	r := New(cfg)

	// Even with auto-routing enabled, the label should win
	issue := makeIssue(45, "Refactor auth", "model:gemini")
	name, reason := r.ResolveBackend(issue)
	if name != "gemini" {
		t.Errorf("ResolveBackend() name = %q, want %q (label should override auto-routing)", name, "gemini")
	}
	if reason != "label" {
		t.Errorf("ResolveBackend() reason = %q, want %q", reason, "label")
	}
}

func TestResolveBackend_NoLabelsManualMode(t *testing.T) {
	cfg := &config.Config{
		Model: config.ModelConfig{
			Default: "codex",
			Backends: map[string]config.BackendDef{
				"codex": {Cmd: "codex"},
			},
		},
		Routing: config.RoutingConfig{Mode: "manual"},
	}
	r := New(cfg)

	issue := makeIssue(46, "Something")
	name, reason := r.ResolveBackend(issue)
	if name != "codex" {
		t.Errorf("ResolveBackend() name = %q, want %q (default)", name, "codex")
	}
	if reason != "default" {
		t.Errorf("ResolveBackend() reason = %q, want %q", reason, "default")
	}
}

func TestResolveBackend_AutoRoutingViaRouteFn(t *testing.T) {
	cfg := &config.Config{
		Model: config.ModelConfig{
			Default: "claude",
			Backends: map[string]config.BackendDef{
				"claude": {Cmd: "claude"},
				"codex":  {Cmd: "codex"},
			},
		},
		Routing: config.RoutingConfig{Mode: "auto"},
	}
	r := New(cfg)
	r.RouteFn = func(issue github.Issue) (string, string, error) {
		return "codex", "simple fix", nil
	}

	issue := makeIssue(47, "Simple fix")
	name, reason := r.ResolveBackend(issue)
	if name != "codex" {
		t.Errorf("ResolveBackend() name = %q, want %q", name, "codex")
	}
	if reason != "simple fix" {
		t.Errorf("ResolveBackend() reason = %q, want %q", reason, "simple fix")
	}
}

func TestResolveBackend_AutoRoutingErrorFallsToDefault(t *testing.T) {
	cfg := &config.Config{
		Model: config.ModelConfig{
			Default: "claude",
			Backends: map[string]config.BackendDef{
				"claude": {Cmd: "claude"},
				"codex":  {Cmd: "codex"},
			},
		},
		Routing: config.RoutingConfig{Mode: "auto"},
	}
	r := New(cfg)
	r.RouteFn = func(issue github.Issue) (string, string, error) {
		return "", "", fmt.Errorf("network error")
	}

	issue := makeIssue(48, "Fix bug")
	name, reason := r.ResolveBackend(issue)
	if name != "claude" {
		t.Errorf("ResolveBackend() name = %q, want %q (should fall back to default)", name, "claude")
	}
	if reason != "default" {
		t.Errorf("ResolveBackend() reason = %q, want %q", reason, "default")
	}
}
