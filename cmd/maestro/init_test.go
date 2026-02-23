package main

import (
	"bufio"
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
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

func TestSystemdUnit(t *testing.T) {
	content := systemdUnit("/usr/bin/maestro", "/etc/maestro.yaml")
	if !strings.Contains(content, "ExecStart=/usr/bin/maestro run --config /etc/maestro.yaml") {
		t.Error("should contain correct ExecStart line")
	}
	for _, section := range []string{"[Unit]", "[Service]", "[Install]"} {
		if !strings.Contains(content, section) {
			t.Errorf("should contain %s section", section)
		}
	}
}

func TestLaunchdPlist(t *testing.T) {
	content := launchdPlist("/usr/bin/maestro", "/etc/maestro.yaml")
	if !strings.Contains(content, "<string>/usr/bin/maestro</string>") {
		t.Error("should contain binary path")
	}
	if !strings.Contains(content, "<string>/etc/maestro.yaml</string>") {
		t.Error("should contain config path")
	}
	if !strings.Contains(content, "com.maestro.agent") {
		t.Error("should contain label")
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
