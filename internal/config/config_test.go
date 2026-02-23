package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParse_IssueLabelsNew(t *testing.T) {
	yaml := `
repo: owner/repo
issue_labels:
  - bug
  - enhancement
  - documentation
`
	cfg, err := parse([]byte(yaml))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	want := []string{"bug", "enhancement", "documentation"}
	if len(cfg.IssueLabels) != len(want) {
		t.Fatalf("IssueLabels = %v, want %v", cfg.IssueLabels, want)
	}
	for i, l := range cfg.IssueLabels {
		if l != want[i] {
			t.Errorf("IssueLabels[%d] = %q, want %q", i, l, want[i])
		}
	}
}

func TestParse_IssueLabelsBackwardCompat(t *testing.T) {
	yaml := `
repo: owner/repo
issue_label: bug
`
	cfg, err := parse([]byte(yaml))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(cfg.IssueLabels) != 1 || cfg.IssueLabels[0] != "bug" {
		t.Errorf("IssueLabels = %v, want [bug]", cfg.IssueLabels)
	}
}

func TestParse_IssueLabelsDefault(t *testing.T) {
	yaml := `
repo: owner/repo
`
	cfg, err := parse([]byte(yaml))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(cfg.IssueLabels) != 1 || cfg.IssueLabels[0] != "enhancement" {
		t.Errorf("IssueLabels = %v, want [enhancement]", cfg.IssueLabels)
	}
}

func TestParse_IssueLabelsLegacyMerged(t *testing.T) {
	yaml := `
repo: owner/repo
issue_label: bug
issue_labels:
  - enhancement
  - documentation
`
	cfg, err := parse([]byte(yaml))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	// bug from issue_label should be appended to issue_labels
	want := []string{"enhancement", "documentation", "bug"}
	if len(cfg.IssueLabels) != len(want) {
		t.Fatalf("IssueLabels = %v, want %v", cfg.IssueLabels, want)
	}
	for i, l := range cfg.IssueLabels {
		if l != want[i] {
			t.Errorf("IssueLabels[%d] = %q, want %q", i, l, want[i])
		}
	}
}

func TestParse_IssueLabelsLegacyNoDuplicate(t *testing.T) {
	yaml := `
repo: owner/repo
issue_label: enhancement
issue_labels:
  - enhancement
  - bug
`
	cfg, err := parse([]byte(yaml))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	// enhancement already in issue_labels, should not duplicate
	want := []string{"enhancement", "bug"}
	if len(cfg.IssueLabels) != len(want) {
		t.Fatalf("IssueLabels = %v, want %v", cfg.IssueLabels, want)
	}
	for i, l := range cfg.IssueLabels {
		if l != want[i] {
			t.Errorf("IssueLabels[%d] = %q, want %q", i, l, want[i])
		}
	}
}

func TestParse_SessionPrefixDefault(t *testing.T) {
	yaml := `repo: BeFeast/panoptikon`
	cfg, err := parse([]byte(yaml))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.SessionPrefix != "pan" {
		t.Errorf("expected session_prefix=pan, got %q", cfg.SessionPrefix)
	}
}

func TestParse_SessionPrefixExplicit(t *testing.T) {
	yaml := `
repo: BeFeast/panoptikon
session_prefix: myapp
`
	cfg, err := parse([]byte(yaml))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.SessionPrefix != "myapp" {
		t.Errorf("expected session_prefix=myapp, got %q", cfg.SessionPrefix)
	}
}

func TestParse_SessionPrefixShortRepoName(t *testing.T) {
	yaml := `repo: user/ab`
	cfg, err := parse([]byte(yaml))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.SessionPrefix != "ab" {
		t.Errorf("expected session_prefix=ab, got %q", cfg.SessionPrefix)
	}
}

func TestParse_StateDirDefault(t *testing.T) {
	yaml := `repo: BeFeast/panoptikon`
	cfg, err := parse([]byte(yaml))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	home := os.Getenv("HOME")
	// Default should be ~/.maestro/<md5-hash>
	if !filepath.HasPrefix(cfg.StateDir, filepath.Join(home, ".maestro")) {
		t.Errorf("expected state_dir under ~/.maestro, got %q", cfg.StateDir)
	}
	if cfg.StateDir == "" {
		t.Error("state_dir should not be empty")
	}
}

func TestParse_StateDirExplicit(t *testing.T) {
	yaml := `
repo: BeFeast/panoptikon
state_dir: /tmp/maestro-test
`
	cfg, err := parse([]byte(yaml))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.StateDir != "/tmp/maestro-test" {
		t.Errorf("expected state_dir=/tmp/maestro-test, got %q", cfg.StateDir)
	}
}

func TestParse_StateDirExpandsHome(t *testing.T) {
	yaml := `
repo: BeFeast/panoptikon
state_dir: ~/.maestro/panoptikon
`
	cfg, err := parse([]byte(yaml))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	home := os.Getenv("HOME")
	expected := filepath.Join(home, ".maestro/panoptikon")
	if cfg.StateDir != expected {
		t.Errorf("expected state_dir=%s, got %q", expected, cfg.StateDir)
	}
}

func TestParse_PromptTemplateFields(t *testing.T) {
	yaml := `
repo: owner/repo
worker_prompt: /path/to/default.md
bug_prompt: /path/to/bug.md
enhancement_prompt: /path/to/enhancement.md
`
	cfg, err := parse([]byte(yaml))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if cfg.WorkerPrompt != "/path/to/default.md" {
		t.Errorf("WorkerPrompt = %q, want /path/to/default.md", cfg.WorkerPrompt)
	}
	if cfg.BugPrompt != "/path/to/bug.md" {
		t.Errorf("BugPrompt = %q, want /path/to/bug.md", cfg.BugPrompt)
	}
	if cfg.EnhancementPrompt != "/path/to/enhancement.md" {
		t.Errorf("EnhancementPrompt = %q, want /path/to/enhancement.md", cfg.EnhancementPrompt)
	}
}

func TestParse_PromptTemplateExpandsHome(t *testing.T) {
	yaml := `
repo: owner/repo
bug_prompt: ~/prompts/bug.md
enhancement_prompt: ~/prompts/enhancement.md
`
	cfg, err := parse([]byte(yaml))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	home := os.Getenv("HOME")
	expectedBug := filepath.Join(home, "prompts/bug.md")
	expectedEnh := filepath.Join(home, "prompts/enhancement.md")
	if cfg.BugPrompt != expectedBug {
		t.Errorf("BugPrompt = %q, want %q", cfg.BugPrompt, expectedBug)
	}
	if cfg.EnhancementPrompt != expectedEnh {
		t.Errorf("EnhancementPrompt = %q, want %q", cfg.EnhancementPrompt, expectedEnh)
	}
}

func TestParse_PromptTemplateFieldsOptional(t *testing.T) {
	yaml := `
repo: owner/repo
worker_prompt: /path/to/default.md
`
	cfg, err := parse([]byte(yaml))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if cfg.BugPrompt != "" {
		t.Errorf("BugPrompt = %q, want empty", cfg.BugPrompt)
	}
	if cfg.EnhancementPrompt != "" {
		t.Errorf("EnhancementPrompt = %q, want empty", cfg.EnhancementPrompt)
	}
}

func TestParse_DifferentReposDifferentStateDirs(t *testing.T) {
	yaml1 := `repo: BeFeast/panoptikon`
	yaml2 := `repo: BeFeast/myapp`

	cfg1, err := parse([]byte(yaml1))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	cfg2, err := parse([]byte(yaml2))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg1.StateDir == cfg2.StateDir {
		t.Errorf("different repos should have different default state_dirs, both got %q", cfg1.StateDir)
	}
}

func TestParse_RoutingDefaults(t *testing.T) {
	yaml := `repo: owner/repo`
	cfg, err := parse([]byte(yaml))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if cfg.Routing.Mode != "manual" {
		t.Errorf("Routing.Mode = %q, want %q", cfg.Routing.Mode, "manual")
	}
	if cfg.Routing.RouterModel != "claude" {
		t.Errorf("Routing.RouterModel = %q, want %q", cfg.Routing.RouterModel, "claude")
	}
	if cfg.Routing.RouterModelName != "claude-sonnet-4-6" {
		t.Errorf("Routing.RouterModelName = %q, want %q", cfg.Routing.RouterModelName, "claude-sonnet-4-6")
	}
}

func TestParse_RoutingExplicit(t *testing.T) {
	yaml := `
repo: owner/repo
routing:
  mode: auto
  router_model: claude
  router_model_name: claude-haiku-4-5-20251001
  router_prompt: "Pick: {{BACKENDS}} for #{{NUMBER}}"
`
	cfg, err := parse([]byte(yaml))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if cfg.Routing.Mode != "auto" {
		t.Errorf("Routing.Mode = %q, want %q", cfg.Routing.Mode, "auto")
	}
	if cfg.Routing.RouterModelName != "claude-haiku-4-5-20251001" {
		t.Errorf("Routing.RouterModelName = %q, want %q", cfg.Routing.RouterModelName, "claude-haiku-4-5-20251001")
	}
	if cfg.Routing.RouterPrompt != "Pick: {{BACKENDS}} for #{{NUMBER}}" {
		t.Errorf("Routing.RouterPrompt = %q", cfg.Routing.RouterPrompt)
	}
}

func TestParse_MaxRuntimeMinutesDefault(t *testing.T) {
	yaml := `repo: owner/repo`
	cfg, err := parse([]byte(yaml))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if cfg.MaxRuntimeMinutes != 120 {
		t.Errorf("MaxRuntimeMinutes = %d, want 120", cfg.MaxRuntimeMinutes)
	}
}

func TestParse_MaxRuntimeMinutesExplicit(t *testing.T) {
	yaml := `
repo: owner/repo
max_runtime_minutes: 60
`
	cfg, err := parse([]byte(yaml))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if cfg.MaxRuntimeMinutes != 60 {
		t.Errorf("MaxRuntimeMinutes = %d, want 60", cfg.MaxRuntimeMinutes)
	}
}
