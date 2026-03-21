package pipeline

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/state"
)

func TestIsEnabled(t *testing.T) {
	cfg := &config.Config{}
	if IsEnabled(cfg) {
		t.Error("expected disabled by default")
	}
	cfg.Pipeline.Enabled = true
	if !IsEnabled(cfg) {
		t.Error("expected enabled")
	}
}

func TestInitialPhase(t *testing.T) {
	tests := []struct {
		name     string
		cfg      config.Config
		expected state.Phase
	}{
		{
			name:     "pipeline disabled",
			cfg:      config.Config{},
			expected: state.PhaseNone,
		},
		{
			name: "pipeline enabled, planner enabled",
			cfg: config.Config{
				Pipeline: config.PipelineConfig{
					Enabled: true,
					Planner: config.RoleConfig{Enabled: true},
				},
			},
			expected: state.PhasePlan,
		},
		{
			name: "pipeline enabled, planner disabled",
			cfg: config.Config{
				Pipeline: config.PipelineConfig{
					Enabled: true,
					Planner: config.RoleConfig{Enabled: false},
				},
			},
			expected: state.PhaseImplement,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := InitialPhase(&tt.cfg)
			if got != tt.expected {
				t.Errorf("got %q, want %q", got, tt.expected)
			}
		})
	}
}

func TestNextPhase(t *testing.T) {
	tests := []struct {
		name      string
		completed state.Phase
		validator bool
		expected  state.Phase
	}{
		{"plan → implement", state.PhasePlan, false, state.PhaseImplement},
		{"implement → validate (validator enabled)", state.PhaseImplement, true, state.PhaseValidate},
		{"implement → none (validator disabled)", state.PhaseImplement, false, state.PhaseNone},
		{"validate → none", state.PhaseValidate, true, state.PhaseNone},
		{"none → none", state.PhaseNone, false, state.PhaseNone},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &config.Config{
				Pipeline: config.PipelineConfig{
					Validator: config.RoleConfig{Enabled: tt.validator},
				},
			}
			got := NextPhase(cfg, tt.completed)
			if got != tt.expected {
				t.Errorf("got %q, want %q", got, tt.expected)
			}
		})
	}
}

func TestPlanArtifactsExist(t *testing.T) {
	dir := t.TempDir()

	if PlanArtifactsExist(dir) {
		t.Error("expected false with no artifacts")
	}

	// Create only plan file
	os.WriteFile(filepath.Join(dir, PlanFile), []byte("plan"), 0644)
	if PlanArtifactsExist(dir) {
		t.Error("expected false with only plan file")
	}

	// Create validation file too
	os.WriteFile(filepath.Join(dir, ValidationFile), []byte("validation"), 0644)
	if !PlanArtifactsExist(dir) {
		t.Error("expected true with both artifacts")
	}
}

func TestValidationPassed(t *testing.T) {
	dir := t.TempDir()

	// No result file
	_, _, err := ValidationPassed(dir)
	if err == nil {
		t.Error("expected error with no result file")
	}

	// PASS result
	os.WriteFile(filepath.Join(dir, ValidationResultFile), []byte("PASS\nAll good"), 0644)
	passed, feedback, err := ValidationPassed(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !passed {
		t.Error("expected passed")
	}
	if feedback != "" {
		t.Errorf("unexpected feedback: %q", feedback)
	}

	// FAIL result
	os.WriteFile(filepath.Join(dir, ValidationResultFile), []byte("FAIL\nTests don't pass\nFix the build"), 0644)
	passed, feedback, err = ValidationPassed(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if passed {
		t.Error("expected failed")
	}
	if feedback != "Tests don't pass\nFix the build" {
		t.Errorf("unexpected feedback: %q", feedback)
	}
}

func TestBackendForPhase(t *testing.T) {
	cfg := &config.Config{
		Model: config.ModelConfig{Default: "claude"},
		Pipeline: config.PipelineConfig{
			Planner:   config.RoleConfig{Backend: "haiku"},
			Validator: config.RoleConfig{Backend: "sonnet"},
		},
	}

	if got := BackendForPhase(cfg, state.PhasePlan); got != "haiku" {
		t.Errorf("plan backend: got %q, want haiku", got)
	}
	if got := BackendForPhase(cfg, state.PhaseImplement); got != "claude" {
		t.Errorf("implement backend: got %q, want claude", got)
	}
	if got := BackendForPhase(cfg, state.PhaseValidate); got != "sonnet" {
		t.Errorf("validate backend: got %q, want sonnet", got)
	}

	// Empty role backends → default
	cfg2 := &config.Config{
		Model: config.ModelConfig{Default: "claude"},
	}
	if got := BackendForPhase(cfg2, state.PhasePlan); got != "claude" {
		t.Errorf("empty plan backend: got %q, want claude", got)
	}
}

func TestMaxRuntimeForPhase(t *testing.T) {
	cfg := &config.Config{
		MaxRuntimeMinutes: 120,
		Pipeline: config.PipelineConfig{
			Planner:   config.RoleConfig{MaxRuntimeMinutes: 30},
			Validator: config.RoleConfig{MaxRuntimeMinutes: 45},
			Research:  config.RoleConfig{MaxRuntimeMinutes: 10},
		},
	}

	if got := MaxRuntimeForPhase(cfg, state.PhaseResearch); got != 10 {
		t.Errorf("research runtime: got %d, want 10", got)
	}
	if got := MaxRuntimeForPhase(cfg, state.PhasePlan); got != 30 {
		t.Errorf("plan runtime: got %d, want 30", got)
	}
	if got := MaxRuntimeForPhase(cfg, state.PhaseImplement); got != 120 {
		t.Errorf("implement runtime: got %d, want 120", got)
	}
	if got := MaxRuntimeForPhase(cfg, state.PhaseValidate); got != 45 {
		t.Errorf("validate runtime: got %d, want 45", got)
	}
}

func TestInitialPhase_Research(t *testing.T) {
	tests := []struct {
		name     string
		cfg      config.Config
		expected state.Phase
	}{
		{
			name: "research enabled takes priority",
			cfg: config.Config{
				Pipeline: config.PipelineConfig{
					Enabled:  true,
					Research: config.RoleConfig{Enabled: true},
					Planner:  config.RoleConfig{Enabled: true},
				},
			},
			expected: state.PhaseResearch,
		},
		{
			name: "research only, no planner",
			cfg: config.Config{
				Pipeline: config.PipelineConfig{
					Enabled:  true,
					Research: config.RoleConfig{Enabled: true},
				},
			},
			expected: state.PhaseResearch,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := InitialPhase(&tt.cfg)
			if got != tt.expected {
				t.Errorf("got %q, want %q", got, tt.expected)
			}
		})
	}
}

func TestNextPhase_Research(t *testing.T) {
	tests := []struct {
		name     string
		planner  bool
		expected state.Phase
	}{
		{"research → plan (planner enabled)", true, state.PhasePlan},
		{"research → implement (planner disabled)", false, state.PhaseImplement},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &config.Config{
				Pipeline: config.PipelineConfig{
					Planner: config.RoleConfig{Enabled: tt.planner},
				},
			}
			got := NextPhase(cfg, state.PhaseResearch)
			if got != tt.expected {
				t.Errorf("got %q, want %q", got, tt.expected)
			}
		})
	}
}

func TestResearchArtifactsExist(t *testing.T) {
	dir := t.TempDir()

	if ResearchArtifactsExist(dir) {
		t.Error("expected false with no artifacts")
	}

	// Create the research file
	researchDir := filepath.Join(dir, ResearchDir)
	os.MkdirAll(researchDir, 0755)
	os.WriteFile(filepath.Join(researchDir, ResearchFile), []byte("research"), 0644)
	if !ResearchArtifactsExist(dir) {
		t.Error("expected true with research file")
	}
}

func TestBackendForPhase_Research(t *testing.T) {
	cfg := &config.Config{
		Model: config.ModelConfig{Default: "claude"},
		Pipeline: config.PipelineConfig{
			Research: config.RoleConfig{Backend: "haiku"},
		},
	}

	if got := BackendForPhase(cfg, state.PhaseResearch); got != "haiku" {
		t.Errorf("research backend: got %q, want haiku", got)
	}

	// Empty research backend → default
	cfg2 := &config.Config{
		Model: config.ModelConfig{Default: "claude"},
	}
	if got := BackendForPhase(cfg2, state.PhaseResearch); got != "claude" {
		t.Errorf("empty research backend: got %q, want claude", got)
	}
}

func TestMaxValidationRetries(t *testing.T) {
	// Default
	cfg := &config.Config{}
	if got := MaxValidationRetries(cfg); got != 3 {
		t.Errorf("default: got %d, want 3", got)
	}

	// Custom
	cfg2 := &config.Config{
		Pipeline: config.PipelineConfig{MaxValidationRetries: 5},
	}
	if got := MaxValidationRetries(cfg2); got != 5 {
		t.Errorf("custom: got %d, want 5", got)
	}
}
