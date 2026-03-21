package pipeline

import (
	"strings"
	"testing"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/github"
	"github.com/befeast/maestro/internal/state"
)

func TestPromptForPhase_Research(t *testing.T) {
	cfg := &config.Config{
		Repo: "test/repo",
	}
	issue := github.Issue{
		Number: 42,
		Title:  "Add feature",
		Body:   "Implement a new feature",
	}

	prompt := PromptForPhase(cfg, state.PhaseResearch, issue, "/tmp/worktree", "feat/branch")
	if !strings.Contains(prompt, "researcher") {
		t.Error("research prompt should mention researcher role")
	}
	if !strings.Contains(prompt, "#42") {
		t.Error("research prompt should contain issue number")
	}
	if !strings.Contains(prompt, "RESEARCH_CONTEXT.md") {
		t.Error("research prompt should reference research context file")
	}
}

func TestPromptTemplateForPhase_Research(t *testing.T) {
	cfg := &config.Config{}
	tmpl := PromptTemplateForPhase(cfg, state.PhaseResearch)
	if !strings.Contains(tmpl, "{{ISSUE_NUMBER}}") {
		t.Error("research template should contain {{ISSUE_NUMBER}} placeholder")
	}
}

func TestImplementerPreamble_WithResearch(t *testing.T) {
	cfg := &config.Config{
		Pipeline: config.PipelineConfig{
			Research: config.RoleConfig{Enabled: true},
		},
	}
	sess := &state.Session{}

	preamble := ImplementerPreamble(cfg, sess)
	if !strings.Contains(preamble, "RESEARCH_CONTEXT.md") {
		t.Error("preamble should reference research context when research is enabled")
	}
}

func TestImplementerPreamble_WithTestMapping(t *testing.T) {
	cfg := &config.Config{
		Pipeline: config.PipelineConfig{
			TestMapping: true,
		},
	}
	sess := &state.Session{}

	preamble := ImplementerPreamble(cfg, sess)
	if !strings.Contains(preamble, "verify.sh") {
		t.Error("preamble should reference verify.sh when test mapping is enabled")
	}
	if !strings.Contains(preamble, "Test Mapping") {
		t.Error("preamble should contain test mapping section")
	}
}

func TestImplementerPreamble_NoExtras(t *testing.T) {
	cfg := &config.Config{}
	sess := &state.Session{}

	preamble := ImplementerPreamble(cfg, sess)
	if strings.Contains(preamble, "RESEARCH_CONTEXT.md") {
		t.Error("preamble should not reference research when disabled")
	}
	if strings.Contains(preamble, "verify.sh") {
		t.Error("preamble should not reference verify.sh when test mapping is disabled")
	}
}

func TestImplementerPreamble_WithValidationFeedback(t *testing.T) {
	cfg := &config.Config{}
	sess := &state.Session{
		ValidationFeedback: "Tests are failing because of missing import",
	}

	preamble := ImplementerPreamble(cfg, sess)
	if !strings.Contains(preamble, "missing import") {
		t.Error("preamble should include validation feedback")
	}
}
