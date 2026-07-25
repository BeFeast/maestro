package config

import (
	"strings"
	"testing"
)

func TestPipelineAdvisorReviewRoundsDefaultsAndCaps(t *testing.T) {
	if got := (PipelineConfig{}).EffectiveAdvisorReviewRounds(); got != DefaultAdvisorReviewRounds {
		t.Fatalf("default rounds = %d, want %d", got, DefaultAdvisorReviewRounds)
	}
	if got := (PipelineConfig{AdvisorReviewRounds: 4}).EffectiveAdvisorReviewRounds(); got != 4 {
		t.Fatalf("configured rounds = %d, want 4", got)
	}
	if got := (PipelineConfig{AdvisorReviewRounds: 99}).EffectiveAdvisorReviewRounds(); got != MaxAdvisorReviewRounds {
		t.Fatalf("defensive cap = %d, want %d", got, MaxAdvisorReviewRounds)
	}
}

func TestParsePipelineAdvisorRoleAndBudget(t *testing.T) {
	cfg, err := Parse([]byte(`
repo: owner/repo
model:
  default: codex
  backends:
    codex: {cmd: codex}
    advisor: {cmd: claude}
pipeline:
  enabled: true
  planner: {enabled: true}
  advisor:
    enabled: true
    backend: advisor
    prompt: ./prompts/advisor.md
    effort: high
    max_runtime_minutes: 17
  advisor_review_rounds: 4
  advisor_best_effort: true
`))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Pipeline.Advisor.Enabled || cfg.Pipeline.Advisor.Backend != "advisor" || cfg.Pipeline.Advisor.Effort != "high" || cfg.Pipeline.Advisor.MaxRuntimeMinutes != 17 {
		t.Fatalf("advisor role = %#v", cfg.Pipeline.Advisor)
	}
	if cfg.Pipeline.EffectiveAdvisorReviewRounds() != 4 || !cfg.Pipeline.AdvisorBestEffort {
		t.Fatalf("advisor budget/bypass = %d/%v", cfg.Pipeline.EffectiveAdvisorReviewRounds(), cfg.Pipeline.AdvisorBestEffort)
	}
}

func TestParsePipelineAdvisorRejectsRoundsAboveHardCap(t *testing.T) {
	_, err := Parse([]byte("repo: owner/repo\npipeline:\n  advisor_review_rounds: 6\n"))
	if err == nil || !strings.Contains(err.Error(), "advisor_review_rounds") {
		t.Fatalf("Parse error = %v, want hard-cap error", err)
	}
}
