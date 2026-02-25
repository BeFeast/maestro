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

func TestParse_IssueLabelsDefault_Empty(t *testing.T) {
	yaml := `
repo: owner/repo
`
	cfg, err := parse([]byte(yaml))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(cfg.IssueLabels) != 0 {
		t.Errorf("IssueLabels = %v, want empty (no label filter)", cfg.IssueLabels)
	}
}

func TestParse_IssueLabelsExplicitEmpty(t *testing.T) {
	yaml := `
repo: owner/repo
issue_labels: []
`
	cfg, err := parse([]byte(yaml))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(cfg.IssueLabels) != 0 {
		t.Errorf("IssueLabels = %v, want empty (no label filter)", cfg.IssueLabels)
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

func TestParse_WorkerSilentTimeoutMinutesDefault(t *testing.T) {
	yaml := `repo: owner/repo`
	cfg, err := parse([]byte(yaml))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if cfg.WorkerSilentTimeoutMinutes != 0 {
		t.Errorf("WorkerSilentTimeoutMinutes = %d, want 0", cfg.WorkerSilentTimeoutMinutes)
	}
}

func TestParse_WorkerSilentTimeoutMinutesExplicit(t *testing.T) {
	yaml := `
repo: owner/repo
worker_silent_timeout_minutes: 25
`
	cfg, err := parse([]byte(yaml))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if cfg.WorkerSilentTimeoutMinutes != 25 {
		t.Errorf("WorkerSilentTimeoutMinutes = %d, want 25", cfg.WorkerSilentTimeoutMinutes)
	}
}

func TestParse_AutoRebaseDefault(t *testing.T) {
	yaml := `repo: owner/repo`
	cfg, err := parse([]byte(yaml))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !cfg.AutoRebase {
		t.Error("AutoRebase should default to true")
	}
}

func TestParse_AutoRebaseExplicitFalse(t *testing.T) {
	yaml := `
repo: owner/repo
auto_rebase: false
`
	cfg, err := parse([]byte(yaml))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if cfg.AutoRebase {
		t.Error("AutoRebase should be false when explicitly configured")
	}
}

func TestParse_ModelConfigDefaults(t *testing.T) {
	yaml := `repo: owner/repo`
	cfg, err := parse([]byte(yaml))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if cfg.Model.Default != "claude" {
		t.Errorf("expected default backend=claude, got %q", cfg.Model.Default)
	}
	if _, ok := cfg.Model.Backends["claude"]; !ok {
		t.Error("expected claude backend to be present in map")
	}
}

func TestParse_ModelConfigExplicit(t *testing.T) {
	yaml := `
repo: owner/repo
model:
  default: codex
  backends:
    claude:
      cmd: claude
    codex:
      cmd: /usr/local/bin/codex
      extra_args: ["--approval-mode", "full-auto"]
    gemini:
      cmd: gemini-cli
      extra_args: ["--yolo"]
`
	cfg, err := parse([]byte(yaml))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if cfg.Model.Default != "codex" {
		t.Errorf("expected default=codex, got %q", cfg.Model.Default)
	}
	if len(cfg.Model.Backends) < 3 {
		t.Errorf("expected at least 3 backends, got %d", len(cfg.Model.Backends))
	}
	codex := cfg.Model.Backends["codex"]
	if codex.Cmd != "/usr/local/bin/codex" {
		t.Errorf("expected codex cmd=/usr/local/bin/codex, got %q", codex.Cmd)
	}
	if len(codex.ExtraArgs) != 2 || codex.ExtraArgs[0] != "--approval-mode" {
		t.Errorf("expected codex extra_args=[--approval-mode full-auto], got %v", codex.ExtraArgs)
	}
	gemini := cfg.Model.Backends["gemini"]
	if gemini.Cmd != "gemini-cli" {
		t.Errorf("expected gemini cmd=gemini-cli, got %q", gemini.Cmd)
	}
}

func TestParse_ModelConfigPromptMode(t *testing.T) {
	yaml := `
repo: owner/repo
model:
  default: custom
  backends:
    custom:
      cmd: my-cli
      prompt_mode: stdin
      extra_args: ["--auto"]
`
	cfg, err := parse([]byte(yaml))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	custom := cfg.Model.Backends["custom"]
	if custom.PromptMode != "stdin" {
		t.Errorf("expected prompt_mode=stdin, got %q", custom.PromptMode)
	}
	if custom.Cmd != "my-cli" {
		t.Errorf("expected cmd=my-cli, got %q", custom.Cmd)
	}
}

func TestParse_ClaudeCmdBackwardCompat(t *testing.T) {
	yaml := `
repo: owner/repo
claude_cmd: /custom/path/claude
`
	cfg, err := parse([]byte(yaml))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	claude := cfg.Model.Backends["claude"]
	if claude.Cmd != "/custom/path/claude" {
		t.Errorf("expected claude_cmd to populate backends[claude].cmd, got %q", claude.Cmd)
	}
}

func TestParse_ClaudeCmdDoesNotOverrideExplicit(t *testing.T) {
	yaml := `
repo: owner/repo
claude_cmd: /old/claude
model:
  backends:
    claude:
      cmd: /new/claude
`
	cfg, err := parse([]byte(yaml))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	claude := cfg.Model.Backends["claude"]
	if claude.Cmd != "/new/claude" {
		t.Errorf("explicit model.backends.claude.cmd should take precedence, got %q", claude.Cmd)
	}
}

func TestParse_DigestModeDefault(t *testing.T) {
	yaml := `repo: owner/repo`
	cfg, err := parse([]byte(yaml))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if cfg.Telegram.DigestMode {
		t.Error("DigestMode should default to false")
	}
}

func TestParse_DigestModeExplicit(t *testing.T) {
	yaml := `
repo: owner/repo
telegram:
  target: "12345"
  digest_mode: true
`
	cfg, err := parse([]byte(yaml))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !cfg.Telegram.DigestMode {
		t.Error("DigestMode should be true when explicitly set")
	}
}

func TestLoadDir(t *testing.T) {
	dir := t.TempDir()

	// Write two config files
	cfg1 := `repo: owner/alpha`
	cfg2 := `repo: owner/beta`
	if err := os.WriteFile(filepath.Join(dir, "alpha.yaml"), []byte(cfg1), 0644); err != nil {
		t.Fatalf("write alpha.yaml: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "beta.yml"), []byte(cfg2), 0644); err != nil {
		t.Fatalf("write beta.yml: %v", err)
	}
	// Write a non-yaml file that should be ignored
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("ignore me"), 0644); err != nil {
		t.Fatalf("write README.md: %v", err)
	}

	cfgs, err := LoadDir(dir)
	if err != nil {
		t.Fatalf("LoadDir: %v", err)
	}
	if len(cfgs) != 2 {
		t.Fatalf("expected 2 configs, got %d", len(cfgs))
	}
	// os.ReadDir returns entries sorted by name, so alpha first
	if cfgs[0].Repo != "owner/alpha" {
		t.Errorf("cfgs[0].Repo = %q, want owner/alpha", cfgs[0].Repo)
	}
	if cfgs[1].Repo != "owner/beta" {
		t.Errorf("cfgs[1].Repo = %q, want owner/beta", cfgs[1].Repo)
	}
}

func TestLoadDir_Empty(t *testing.T) {
	dir := t.TempDir()
	_, err := LoadDir(dir)
	if err == nil {
		t.Fatal("expected error for empty directory, got nil")
	}
}

func TestLoadDir_SkipsSubdirectories(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "subdir"), 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "project.yaml"), []byte("repo: owner/proj"), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
	cfgs, err := LoadDir(dir)
	if err != nil {
		t.Fatalf("LoadDir: %v", err)
	}
	if len(cfgs) != 1 {
		t.Fatalf("expected 1 config, got %d", len(cfgs))
	}
}

func TestParse_DeployCmdEmpty(t *testing.T) {
	yaml := `repo: owner/repo`
	cfg, err := parse([]byte(yaml))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if cfg.DeployCmd != "" {
		t.Errorf("DeployCmd = %q, want empty", cfg.DeployCmd)
	}
}

func TestParse_DeployCmdExplicit(t *testing.T) {
	yaml := `
repo: owner/repo
deploy_cmd: "go build ./cmd/app/ && systemctl --user restart app"
`
	cfg, err := parse([]byte(yaml))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	want := "go build ./cmd/app/ && systemctl --user restart app"
	if cfg.DeployCmd != want {
		t.Errorf("DeployCmd = %q, want %q", cfg.DeployCmd, want)
	}
}

func TestParse_MergeDefaults(t *testing.T) {
	yaml := `repo: owner/repo`
	cfg, err := parse([]byte(yaml))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if cfg.MergeStrategy != "sequential" {
		t.Errorf("MergeStrategy = %q, want %q", cfg.MergeStrategy, "sequential")
	}
	if cfg.MergeIntervalSeconds != 30 {
		t.Errorf("MergeIntervalSeconds = %d, want 30", cfg.MergeIntervalSeconds)
	}
}

func TestParse_MergeConfigExplicit(t *testing.T) {
	yaml := `
repo: owner/repo
merge_strategy: parallel
merge_interval_seconds: 45
`
	cfg, err := parse([]byte(yaml))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if cfg.MergeStrategy != "parallel" {
		t.Errorf("MergeStrategy = %q, want %q", cfg.MergeStrategy, "parallel")
	}
	if cfg.MergeIntervalSeconds != 45 {
		t.Errorf("MergeIntervalSeconds = %d, want 45", cfg.MergeIntervalSeconds)
	}
}

func TestParse_MergeConfigInvalidFallsBack(t *testing.T) {
	yaml := `
repo: owner/repo
merge_strategy: weird
merge_interval_seconds: 0
`
	cfg, err := parse([]byte(yaml))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if cfg.MergeStrategy != "sequential" {
		t.Errorf("MergeStrategy = %q, want %q", cfg.MergeStrategy, "sequential")
	}
	if cfg.MergeIntervalSeconds != 30 {
		t.Errorf("MergeIntervalSeconds = %d, want 30", cfg.MergeIntervalSeconds)
	}
}
