package main

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/outcome"
	"github.com/befeast/maestro/internal/state"
)

func TestPromptInit(t *testing.T) {
	tests := []struct {
		name       string
		input      string
		question   string
		defaultVal string
		want       string
	}{
		{"user input", "my-answer\n", "Question", "default", "my-answer"},
		{"empty uses default", "\n", "Question", "default", "default"},
		{"no default empty", "\n", "Question", "", ""},
		{"trims whitespace", "  spaced  \n", "Question", "", "spaced"},
		{"eof uses default", "", "Question", "fallback", "fallback"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			scanner := bufio.NewScanner(strings.NewReader(tt.input))
			var buf bytes.Buffer
			got := promptInit(scanner, &buf, tt.question, tt.defaultVal)
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestPromptInitOutput(t *testing.T) {
	scanner := bufio.NewScanner(strings.NewReader("answer\n"))
	var buf bytes.Buffer
	promptInit(scanner, &buf, "Name", "default")
	if !strings.Contains(buf.String(), "? Name (default): ") {
		t.Errorf("prompt output = %q, want to contain '? Name (default): '", buf.String())
	}

	scanner = bufio.NewScanner(strings.NewReader("answer\n"))
	buf.Reset()
	promptInit(scanner, &buf, "Name", "")
	if !strings.Contains(buf.String(), "? Name: ") {
		t.Errorf("prompt output = %q, want to contain '? Name: '", buf.String())
	}
}

func TestBuildProjectStatusJSONIncludesOutcome(t *testing.T) {
	cfg := &config.Config{
		Repo:          "org/repo",
		SessionPrefix: "rep",
		Outcome: outcome.Brief{
			DesiredOutcome: "Repo is live",
			RuntimeTarget:  "https://repo.example.com",
			HealthcheckURL: "https://repo.example.com/healthz",
		},
	}
	st := state.NewState()
	st.Sessions["done"] = &state.Session{IssueNumber: 1, Status: state.StatusDone, PRNumber: 10}

	got := buildProjectStatusJSON(cfg, st)
	if got.Repo != "org/repo" || got.Prefix != "rep" {
		t.Fatalf("project metadata = %q/%q, want org/repo rep", got.Repo, got.Prefix)
	}
	if !got.Outcome.Configured || got.Outcome.Goal != "Repo is live" || got.Outcome.HealthState != outcome.HealthUnknown {
		t.Fatalf("outcome = %+v, want configured unknown health", got.Outcome)
	}
}

func TestSystemdUnit(t *testing.T) {
	content := systemdUnit("/usr/bin/maestro", "/etc/maestro.yaml", "/usr/local/bin:/usr/bin:/bin")
	if !strings.Contains(content, "ExecStart=/usr/bin/maestro run --config /etc/maestro.yaml") {
		t.Error("should contain correct ExecStart line")
	}
	if !strings.Contains(content, "Environment=PATH=/usr/local/bin:/usr/bin:/bin") {
		t.Error("should contain Environment=PATH line")
	}
	for _, section := range []string{"[Unit]", "[Service]", "[Install]"} {
		if !strings.Contains(content, section) {
			t.Errorf("should contain %s section", section)
		}
	}
}

func TestLaunchdPlist(t *testing.T) {
	content := launchdPlist("/usr/bin/maestro", "/etc/maestro.yaml", "/usr/local/bin:/usr/bin:/bin")
	if !strings.Contains(content, "<string>/usr/bin/maestro</string>") {
		t.Error("should contain binary path")
	}
	if !strings.Contains(content, "<string>/etc/maestro.yaml</string>") {
		t.Error("should contain config path")
	}
	if !strings.Contains(content, "com.maestro.agent") {
		t.Error("should contain label")
	}
	if !strings.Contains(content, "<key>EnvironmentVariables</key>") {
		t.Error("should contain EnvironmentVariables key")
	}
	if !strings.Contains(content, "<string>/usr/local/bin:/usr/bin:/bin</string>") {
		t.Error("should contain PATH value")
	}
}

func TestRunInitWizard(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	// repo, local_path(default), worktree(default), max_parallel(default),
	// model(default), label(default), telegram(no)
	input := "BeFeast/myrepo\n\n\n\n\n\n\n"
	var output bytes.Buffer

	err := runInitWizard(strings.NewReader(input), &output, tmpDir)
	if err != nil {
		t.Fatalf("runInitWizard error: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(tmpDir, "maestro.yaml"))
	if err != nil {
		t.Fatal("maestro.yaml not created")
	}

	yamlStr := string(data)
	checks := map[string]string{
		"repo":          "repo: BeFeast/myrepo",
		"local_path":    "local_path: ~/src/myrepo",
		"worktree_base": "worktree_base: ~/.worktrees/myrepo",
		"max_parallel":  "max_parallel: 3",
		"issue_labels":  "enhancement",
		"model":         "default: claude",
	}
	for field, want := range checks {
		if !strings.Contains(yamlStr, want) {
			t.Errorf("yaml missing %s (%q), got:\n%s", field, want, yamlStr)
		}
	}

	// No telegram section when declined
	if strings.Contains(yamlStr, "telegram") {
		t.Errorf("yaml should not contain telegram when declined, got:\n%s", yamlStr)
	}

	out := output.String()
	if !strings.Contains(out, "Welcome to Maestro") {
		t.Error("should show welcome message")
	}
	if !strings.Contains(out, "\u2705 Created maestro.yaml") {
		t.Error("should show config created message")
	}

	// Verify ~/.maestro/ was created
	if _, err := os.Stat(filepath.Join(tmpDir, ".maestro")); os.IsNotExist(err) {
		t.Error("~/.maestro/ directory not created")
	}
}

func TestRunInitWizardWithTelegram(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	input := "BeFeast/myrepo\n\n\n\n\n\ny\n79510949\n"
	var output bytes.Buffer

	err := runInitWizard(strings.NewReader(input), &output, tmpDir)
	if err != nil {
		t.Fatalf("runInitWizard error: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(tmpDir, "maestro.yaml"))
	if err != nil {
		t.Fatal("maestro.yaml not created")
	}

	yamlStr := string(data)
	if !strings.Contains(yamlStr, "telegram") {
		t.Errorf("yaml should contain telegram section, got:\n%s", yamlStr)
	}
	if !strings.Contains(yamlStr, "79510949") {
		t.Errorf("yaml should contain telegram target, got:\n%s", yamlStr)
	}
}

func TestRunInitWizardCustomValues(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	input := "myorg/myapp\n~/code/myapp\n~/.wt/myapp\n5\ncodex\nbug\nN\n"
	var output bytes.Buffer

	err := runInitWizard(strings.NewReader(input), &output, tmpDir)
	if err != nil {
		t.Fatalf("runInitWizard error: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(tmpDir, "maestro.yaml"))
	if err != nil {
		t.Fatal("maestro.yaml not created")
	}

	yamlStr := string(data)
	checks := map[string]string{
		"repo":          "repo: myorg/myapp",
		"local_path":    "local_path: ~/code/myapp",
		"worktree_base": "worktree_base: ~/.wt/myapp",
		"max_parallel":  "max_parallel: 5",
		"issue_labels":  "bug",
		"model":         "default: codex",
	}
	for field, want := range checks {
		if !strings.Contains(yamlStr, want) {
			t.Errorf("yaml missing %s (%q), got:\n%s", field, want, yamlStr)
		}
	}
}

func TestRunInitWizardExistingConfig(t *testing.T) {
	tmpDir := t.TempDir()
	os.WriteFile(filepath.Join(tmpDir, "maestro.yaml"), []byte("existing"), 0644)

	var output bytes.Buffer
	err := runInitWizard(strings.NewReader("BeFeast/test\n"), &output, tmpDir)
	if err == nil {
		t.Fatal("expected error when maestro.yaml exists")
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Errorf("error should mention 'already exists', got: %v", err)
	}
}

func TestRunInitWizardEmptyRepo(t *testing.T) {
	tmpDir := t.TempDir()

	var output bytes.Buffer
	err := runInitWizard(strings.NewReader("\n"), &output, tmpDir)
	if err == nil {
		t.Fatal("expected error for empty repo")
	}
	if !strings.Contains(err.Error(), "repo is required") {
		t.Errorf("error should mention 'repo is required', got: %v", err)
	}
}

func TestRunInitWizardInvalidRepoFormat(t *testing.T) {
	tests := []struct {
		name  string
		input string
	}{
		{"no slash", "justrepo\n"},
		{"empty owner", "/repo\n"},
		{"empty repo name", "owner/\n"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tmpDir := t.TempDir()
			var output bytes.Buffer
			err := runInitWizard(strings.NewReader(tt.input), &output, tmpDir)
			if err == nil {
				t.Fatal("expected error for invalid repo format")
			}
			if !strings.Contains(err.Error(), "owner/repo") {
				t.Errorf("error should mention 'owner/repo', got: %v", err)
			}
		})
	}
}

func TestRunInitWizardStateDirConfirmation(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	input := "org/repo\n\n\n\n\n\n\n"
	var output bytes.Buffer

	err := runInitWizard(strings.NewReader(input), &output, tmpDir)
	if err != nil {
		t.Fatalf("runInitWizard error: %v", err)
	}

	out := output.String()
	maestroDir := filepath.Join(tmpDir, ".maestro")
	if !strings.Contains(out, maestroDir) {
		t.Errorf("output should mention state directory %s, got:\n%s", maestroDir, out)
	}
}

func TestRunInitWizardInvalidMaxParallel(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	input := "org/repo\n\n\nabc\n\n\n\n"
	var output bytes.Buffer

	err := runInitWizard(strings.NewReader(input), &output, tmpDir)
	if err != nil {
		t.Fatalf("runInitWizard error: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(tmpDir, "maestro.yaml"))
	if err != nil {
		t.Fatal("maestro.yaml not created")
	}

	if !strings.Contains(string(data), "max_parallel: 3") {
		t.Errorf("invalid max_parallel should default to 3, got:\n%s", string(data))
	}
}

func TestRunInitWizardInvalidBackend(t *testing.T) {
	tmpDir := t.TempDir()

	// repo, local_path(default), worktree(default), max_parallel(default),
	// model(invalid), ...
	input := "org/repo\n\n\n\ngpt4\n"
	var output bytes.Buffer

	err := runInitWizard(strings.NewReader(input), &output, tmpDir)
	if err == nil {
		t.Fatal("expected error for invalid backend")
	}
	if !strings.Contains(err.Error(), "invalid model backend") {
		t.Errorf("error should mention 'invalid model backend', got: %v", err)
	}
	if !strings.Contains(err.Error(), "claude") {
		t.Errorf("error should list valid backends, got: %v", err)
	}
}

func TestRunInitWizardAllValidBackends(t *testing.T) {
	for _, backend := range validBackends {
		t.Run(backend, func(t *testing.T) {
			tmpDir := t.TempDir()
			t.Setenv("HOME", tmpDir)

			input := fmt.Sprintf("org/repo\n\n\n\n%s\n\n\n", backend)
			var output bytes.Buffer

			err := runInitWizard(strings.NewReader(input), &output, tmpDir)
			if err != nil {
				t.Fatalf("valid backend %q should not error: %v", backend, err)
			}

			data, err := os.ReadFile(filepath.Join(tmpDir, "maestro.yaml"))
			if err != nil {
				t.Fatal("maestro.yaml not created")
			}
			if !strings.Contains(string(data), "default: "+backend) {
				t.Errorf("yaml should contain 'default: %s', got:\n%s", backend, string(data))
			}
		})
	}
}

func TestCheckInitPrerequisites(t *testing.T) {
	var buf bytes.Buffer
	checkInitPrerequisites(&buf)
	// git should always be available in test environments
	if strings.Contains(buf.String(), "git not found") {
		t.Error("git should be found in test environment")
	}
}

func TestRunInitWizardRicherConfig(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	input := "org/repo\n\n\n\n\n\n\n"
	var output bytes.Buffer

	err := runInitWizard(strings.NewReader(input), &output, tmpDir)
	if err != nil {
		t.Fatalf("runInitWizard error: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(tmpDir, "maestro.yaml"))
	if err != nil {
		t.Fatal("maestro.yaml not created")
	}

	yamlStr := string(data)
	commentedKeys := []string{
		"# max_runtime_minutes: 120",
		"# auto_rebase: true",
		"# merge_strategy: sequential",
		"# worker_prompt:",
		"# outcome:",
		"#   desired_outcome:",
		"# exclude_labels:",
	}
	for _, key := range commentedKeys {
		if !strings.Contains(yamlStr, key) {
			t.Errorf("yaml should contain commented-out %q, got:\n%s", key, yamlStr)
		}
	}
}

func TestBuildInitYAML(t *testing.T) {
	cfg := initYAMLConfig{
		Repo:         "org/repo",
		LocalPath:    "~/src/repo",
		WorktreeBase: "~/.worktrees/repo",
		MaxParallel:  3,
		IssueLabels:  []string{"enhancement"},
		Model:        initYAMLModel{Default: "claude"},
	}

	yaml := buildInitYAML(cfg)

	// Active config present
	if !strings.Contains(yaml, "repo: org/repo") {
		t.Error("should contain repo")
	}
	if !strings.Contains(yaml, "max_parallel: 3") {
		t.Error("should contain max_parallel")
	}

	// Commented examples present
	if !strings.Contains(yaml, "# max_runtime_minutes: 120") {
		t.Error("should contain commented max_runtime_minutes")
	}

	// No telegram when nil
	if strings.Contains(yaml, "telegram") {
		t.Error("should not contain telegram when nil")
	}

	// With telegram
	cfg.Telegram = &initYAMLTelegram{Target: "12345"}
	yaml = buildInitYAML(cfg)
	if !strings.Contains(yaml, "telegram:") {
		t.Error("should contain telegram section")
	}
	if !strings.Contains(yaml, `target: "12345"`) {
		t.Error("should contain telegram target")
	}
}

func TestRunInitWizardShowsBackendOptions(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	input := "org/repo\n\n\n\n\n\n\n"
	var output bytes.Buffer

	err := runInitWizard(strings.NewReader(input), &output, tmpDir)
	if err != nil {
		t.Fatalf("runInitWizard error: %v", err)
	}

	out := output.String()
	if !strings.Contains(out, "cline") {
		t.Errorf("model backend prompt should mention cline, got:\n%s", out)
	}
}
