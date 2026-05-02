package pipeline

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/github"
	"github.com/befeast/maestro/internal/state"
)

// ---- Phase-based pipeline tests ----

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
		},
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

func TestDefaultPipelinePromptsForbidAutoClosingReferences(t *testing.T) {
	cfg := &config.Config{Repo: "owner/repo"}
	issue := github.Issue{Number: 351, Title: "runtime verification", Body: "Keep the issue open until runtime is verified."}

	for _, phase := range []state.Phase{state.PhasePlan, state.PhaseValidate} {
		prompt := PromptForPhase(cfg, phase, issue, "/tmp/wt", "feat/runtime")
		if !strings.Contains(prompt, "Refs #351") || !strings.Contains(prompt, "auto-closing keywords") {
			t.Fatalf("phase %s prompt missing non-closing PR reference guidance:\n%s", phase, prompt)
		}
		for _, forbidden := range []string{"Closes #351", "Fixes #351", "Resolves #351"} {
			if strings.Contains(prompt, forbidden) {
				t.Fatalf("phase %s prompt contains closing reference %q:\n%s", phase, forbidden, prompt)
			}
		}
	}

	preamble := ImplementerPreamble(&state.Session{})
	if !strings.Contains(preamble, "non-closing issue references") || !strings.Contains(preamble, "Refs #N") {
		t.Fatalf("implementer preamble missing non-closing PR guidance:\n%s", preamble)
	}
}

// ---- GSD pre-worker pipeline tests ----

func TestExtractKeywords(t *testing.T) {
	title := "Add authentication middleware"
	body := "We need to add JWT-based authentication middleware to the API server."

	kw := extractKeywords(title, body)

	if len(kw) == 0 {
		t.Fatal("expected keywords, got none")
	}

	found := map[string]bool{}
	for _, k := range kw {
		found[k] = true
	}
	for _, want := range []string{"authentication", "middleware", "jwt-based"} {
		if !found[want] {
			t.Errorf("missing keyword %q in %v", want, kw)
		}
	}
}

func TestExtractKeywords_StopWordsFiltered(t *testing.T) {
	kw := extractKeywords("the and for", "this that with from")
	if len(kw) != 0 {
		t.Errorf("expected no keywords from stop words, got %v", kw)
	}
}

func TestExtractKeywords_LimitTo15(t *testing.T) {
	body := "a1word b2word c3word d4word e5word f6word g7word h8word i9word j10word k11word l12word m13word n14word o15word p16word q17word"
	kw := extractKeywords("extra title words here", body)
	if len(kw) > 15 {
		t.Errorf("expected max 15 keywords, got %d", len(kw))
	}
}

func TestFindRelevantFiles(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "internal", "auth"), 0755)
	os.WriteFile(filepath.Join(dir, "internal", "auth", "middleware.go"), []byte("package auth"), 0644)
	os.WriteFile(filepath.Join(dir, "internal", "auth", "jwt.go"), []byte("package auth"), 0644)
	os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main"), 0644)

	files := findRelevantFiles(dir, []string{"auth", "middleware"})
	if len(files) == 0 {
		t.Fatal("expected to find relevant files")
	}

	found := false
	for _, f := range files {
		if strings.Contains(f.Path, "middleware.go") {
			found = true
			if len(f.MatchedKeywords) < 2 {
				t.Errorf("middleware.go should match both keywords, got %v", f.MatchedKeywords)
			}
		}
	}
	if !found {
		t.Error("middleware.go not found in results")
	}
}

func TestFindRelevantFiles_SkipsDotGit(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, ".git", "objects"), 0755)
	os.WriteFile(filepath.Join(dir, ".git", "config"), []byte("config"), 0644)
	os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main"), 0644)

	files := findRelevantFiles(dir, []string{"config"})
	for _, f := range files {
		if strings.Contains(f.Path, ".git") {
			t.Errorf("should not include .git files, got %s", f.Path)
		}
	}
}

func TestRunResearch(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "internal", "pipeline"), 0755)
	os.WriteFile(filepath.Join(dir, "internal", "pipeline", "research.go"), []byte("package pipeline"), 0644)

	ctx, err := runResearch(dir, 42, "Add pipeline research", "Research phase for worker pipeline")
	if err != nil {
		t.Fatalf("runResearch: %v", err)
	}
	if ctx == "" {
		t.Fatal("expected non-empty research context")
	}
	if !strings.Contains(ctx, "Pre-coding Research") {
		t.Error("context should contain header")
	}

	researchFile := filepath.Join(dir, ".maestro", "research", "42.md")
	if _, err := os.Stat(researchFile); os.IsNotExist(err) {
		t.Errorf("expected research file at %s", researchFile)
	}
}

func TestDiscoverPatterns_Go(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module test"), 0644)
	os.MkdirAll(filepath.Join(dir, "internal"), 0755)
	os.MkdirAll(filepath.Join(dir, "cmd"), 0755)

	patterns := discoverPatterns(dir)
	found := map[string]bool{}
	for _, p := range patterns {
		found[p] = true
	}
	if !found["Go project (go.mod found)"] {
		t.Error("should detect Go project")
	}
	if !found["Internal packages in internal/"] {
		t.Error("should detect internal/ directory")
	}
}

func TestExtractRequirements(t *testing.T) {
	body := `## Features
- Add authentication middleware to the API
- Implement JWT token validation
1. Create database migration for users table
2. Add unit tests for auth module
- [ ] Write integration tests
- [x] Update documentation`

	reqs := extractRequirements(body)
	if len(reqs) < 4 {
		t.Fatalf("expected at least 4 requirements, got %d: %v", len(reqs), reqs)
	}
}

func TestExtractRequirements_Empty(t *testing.T) {
	reqs := extractRequirements("Just a paragraph with no structured items.")
	if len(reqs) != 0 {
		t.Errorf("expected 0 requirements from plain text, got %d", len(reqs))
	}
}

func TestExtractRequirements_ShortItemsSkipped(t *testing.T) {
	body := "- OK\n- too short\n- This is a longer requirement that should be kept"
	reqs := extractRequirements(body)
	if len(reqs) != 1 {
		t.Errorf("expected 1 requirement (short items filtered), got %d: %v", len(reqs), reqs)
	}
}

func TestValidatePlanContent(t *testing.T) {
	requirements := []string{
		"Add authentication middleware",
		"Implement JWT validation",
	}
	plan := "# Plan\n\nWe will add authentication middleware using JWT validation.\nFiles: `internal/auth/middleware.go`"

	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "internal", "auth"), 0755)
	os.WriteFile(filepath.Join(dir, "internal", "auth", "middleware.go"), []byte("package auth"), 0644)

	issues := validatePlanContent(plan, requirements, dir)
	for _, issue := range issues {
		if issue.Severity == "error" {
			t.Errorf("unexpected error: %s", issue.Message)
		}
	}
}

func TestValidatePlanContent_MissingRequirement(t *testing.T) {
	requirements := []string{
		"Add authentication middleware",
		"Implement database connection pooling",
	}
	plan := "# Plan\n\nWe will add authentication middleware."

	issues := validatePlanContent(plan, requirements, t.TempDir())
	foundWarning := false
	for _, issue := range issues {
		if strings.Contains(issue.Message, "requirement 2") {
			foundWarning = true
		}
	}
	if !foundWarning {
		t.Error("expected warning about missing requirement 2")
	}
}

func TestTruncate(t *testing.T) {
	if got := truncate("hello world", 5); got != "hello..." {
		t.Errorf("truncate(11, 5) = %q, want %q", got, "hello...")
	}
	if got := truncate("hi", 10); got != "hi" {
		t.Errorf("truncate(2, 10) = %q, want %q", got, "hi")
	}
}

func TestDetectTestInfrastructure_Go(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module test"), 0644)

	cmds := detectTestInfrastructure(dir)
	if len(cmds) == 0 {
		t.Fatal("expected test commands for Go project")
	}
	found := false
	for _, c := range cmds {
		if c == "go test ./..." {
			found = true
		}
	}
	if !found {
		t.Errorf("expected 'go test ./...' in %v", cmds)
	}
}

func TestDetectTestInfrastructure_Multiple(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module test"), 0644)
	os.WriteFile(filepath.Join(dir, "Makefile"), []byte("test:\n\tgo test"), 0644)

	cmds := detectTestInfrastructure(dir)
	if len(cmds) < 2 {
		t.Errorf("expected at least 2 test commands, got %d: %v", len(cmds), cmds)
	}
}

func TestFindTestFiles(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "internal", "auth"), 0755)
	os.WriteFile(filepath.Join(dir, "internal", "auth", "auth.go"), []byte("package auth"), 0644)
	os.WriteFile(filepath.Join(dir, "internal", "auth", "auth_test.go"), []byte("package auth"), 0644)
	os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main"), 0644)

	files := findTestFiles(dir)
	if len(files) != 1 {
		t.Fatalf("expected 1 test file, got %d: %v", len(files), files)
	}
	if !strings.Contains(files[0], "auth_test.go") {
		t.Errorf("expected auth_test.go, got %s", files[0])
	}
}

func TestFindRelatedTestFile(t *testing.T) {
	testFiles := []string{
		"internal/auth/auth_test.go",
		"internal/server/server_test.go",
		"internal/config/config_test.go",
	}

	result := findRelatedTestFile(testFiles, []string{"auth", "middleware"})
	if !strings.Contains(result, "auth_test.go") {
		t.Errorf("expected auth_test.go, got %q", result)
	}
}

func TestGenerateVerifyScript(t *testing.T) {
	mappings := []testMapping{
		{Requirement: "Add authentication", VerifyCmd: "go test ./internal/auth/..."},
		{Requirement: "Update docs", VerifyCmd: "# No automated test found — manual verification required"},
	}
	testCmds := []string{"go test ./..."}

	script := generateVerifyScript(mappings, testCmds)
	if !strings.Contains(script, "#!/bin/bash") {
		t.Error("script should start with shebang")
	}
	if !strings.Contains(script, "go test ./...") {
		t.Error("script should contain project test command")
	}
	if !strings.Contains(script, "go test ./internal/auth/...") {
		t.Error("script should contain specific test command")
	}
	if !strings.Contains(script, "WARN") {
		t.Error("script should warn about manual verification")
	}
}

func TestMapTests(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module test"), 0644)
	os.MkdirAll(filepath.Join(dir, "internal", "auth"), 0755)
	os.WriteFile(filepath.Join(dir, "internal", "auth", "auth_test.go"), []byte("package auth"), 0644)

	verifyPath, summary, err := mapTests(42, "Add auth", "- Add authentication middleware", dir, "")
	if err != nil {
		t.Fatalf("mapTests: %v", err)
	}
	if verifyPath == "" {
		t.Error("expected verify script path")
	}
	if _, err := os.Stat(verifyPath); os.IsNotExist(err) {
		t.Errorf("verify script not found at %s", verifyPath)
	}
	if summary == "" {
		t.Error("expected test mapping summary")
	}
}

func TestShellEscape(t *testing.T) {
	if got := shellEscape("hello 'world'"); got != "hello '\\''world'\\''" {
		t.Errorf("shellEscape = %q", got)
	}
}

func TestRunGSD_AllDisabled(t *testing.T) {
	f := false
	cfg := &config.Config{
		Repo: "test/repo",
		Pipeline: config.PipelineConfig{
			Research:       false,
			PlanValidation: &f,
			TestMapping:    &f,
		},
	}

	result := RunGSD(cfg, t.TempDir(), 1, "Test", "Test body")
	if result.ResearchContext != "" || result.Plan != "" || result.VerifyScript != "" {
		t.Error("expected empty result when all phases disabled")
	}
}

func TestRunGSD_ResearchOnly(t *testing.T) {
	f := false
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "internal", "pipeline"), 0755)
	os.WriteFile(filepath.Join(dir, "internal", "pipeline", "research.go"), []byte("package pipeline"), 0644)

	cfg := &config.Config{
		Repo: "test/repo",
		Pipeline: config.PipelineConfig{
			Research:       true,
			PlanValidation: &f,
			TestMapping:    &f,
		},
	}

	result := RunGSD(cfg, dir, 42, "Add pipeline research", "Research worker pipeline")
	if result.ResearchContext == "" {
		t.Error("expected research context")
	}
	if result.Plan != "" {
		t.Error("plan should be empty when disabled")
	}
}

func TestRunGSD_PlanValidationDefault(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module test"), 0644)

	f := false
	cfg := &config.Config{
		Repo: "test/repo",
		Pipeline: config.PipelineConfig{
			Research:    false,
			TestMapping: &f,
		},
	}

	result := RunGSD(cfg, dir, 42, "Add feature", "- Add new feature\n- Update tests")
	if result.Plan == "" {
		t.Error("expected plan when plan_validation defaults to true")
	}
}

func TestRunGSD_TestMappingDefault(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module test"), 0644)

	f := false
	cfg := &config.Config{
		Repo: "test/repo",
		Pipeline: config.PipelineConfig{
			Research:       false,
			PlanValidation: &f,
		},
	}

	result := RunGSD(cfg, dir, 42, "Add feature", "- Add new feature")
	if result.VerifyScript == "" {
		t.Error("expected verify script when test_mapping defaults to true")
	}
}

func TestRunGSD_AllEnabled(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module test"), 0644)
	os.MkdirAll(filepath.Join(dir, "internal", "pipeline"), 0755)
	os.WriteFile(filepath.Join(dir, "internal", "pipeline", "pipeline.go"), []byte("package pipeline"), 0644)

	cfg := &config.Config{
		Repo: "test/repo",
		Pipeline: config.PipelineConfig{
			Research: true,
		},
	}

	result := RunGSD(cfg, dir, 42, "Add pipeline feature", "- Add research phase\n- Add plan validation")
	if result.ResearchContext == "" {
		t.Error("expected research context")
	}
	if result.Plan == "" {
		t.Error("expected plan")
	}
	if result.VerifyScript == "" {
		t.Error("expected verify script")
	}
}

func TestGSDResult_PromptSection_Empty(t *testing.T) {
	r := &GSDResult{}
	if s := r.PromptSection(); s != "" {
		t.Errorf("expected empty prompt section, got %q", s)
	}
}

func TestGSDResult_PromptSection_Nil(t *testing.T) {
	var r *GSDResult
	if s := r.PromptSection(); s != "" {
		t.Errorf("expected empty prompt section for nil, got %q", s)
	}
}

func TestGSDResult_PromptSection_WithContent(t *testing.T) {
	r := &GSDResult{
		ResearchContext:    "some research",
		Plan:               "a plan",
		TestMappingSummary: "test map",
	}
	s := r.PromptSection()
	if !strings.Contains(s, "Pipeline Context") {
		t.Error("should contain Pipeline Context header")
	}
	if !strings.Contains(s, "some research") {
		t.Error("should contain research context")
	}
	if !strings.Contains(s, "a plan") {
		t.Error("should contain plan")
	}
	if !strings.Contains(s, "test map") {
		t.Error("should contain test mapping")
	}
}

func TestPipelineConfig_GSDDefaults(t *testing.T) {
	p := config.PipelineConfig{}
	if p.Research {
		t.Error("research should default to false")
	}
	if !p.PlanValidationEnabled() {
		t.Error("plan validation should default to true (nil pointer)")
	}
	if !p.TestMappingEnabled() {
		t.Error("test mapping should default to true (nil pointer)")
	}
}

func TestPipelineConfig_GSDExplicitFalse(t *testing.T) {
	f := false
	p := config.PipelineConfig{
		PlanValidation: &f,
		TestMapping:    &f,
	}
	if p.PlanValidationEnabled() {
		t.Error("plan validation should be false when explicitly set")
	}
	if p.TestMappingEnabled() {
		t.Error("test mapping should be false when explicitly set")
	}
}

func TestPipelineConfig_GSDExplicitTrue(t *testing.T) {
	tr := true
	p := config.PipelineConfig{
		PlanValidation: &tr,
		TestMapping:    &tr,
	}
	if !p.PlanValidationEnabled() {
		t.Error("plan validation should be true when explicitly set")
	}
	if !p.TestMappingEnabled() {
		t.Error("test mapping should be true when explicitly set")
	}
}

func TestBuildTestMappingSummary(t *testing.T) {
	mappings := []testMapping{
		{Requirement: "Add feature", VerifyCmd: "go test ./..."},
	}
	summary := buildTestMappingSummary(mappings, "/tmp/verify.sh")
	if !strings.Contains(summary, "Test Mapping") {
		t.Error("summary should contain header")
	}
	if !strings.Contains(summary, "go test") {
		t.Error("summary should contain verify command")
	}
}
