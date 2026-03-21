package worker

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/github"
)

func TestAssemblePrompt_TestFirstGuidance_Enabled(t *testing.T) {
	cfg := &config.Config{Repo: "owner/repo", TestFirst: true}
	issue := github.Issue{Number: 10, Title: "add feature", Body: "Implement X."}
	base := "You are a worker.\n{{ISSUE_NUMBER}} {{ISSUE_TITLE}}\n{{ISSUE_BODY}}\n{{BRANCH}} {{WORKTREE}} {{REPO}}"

	prompt := assemblePrompt(base, issue, "/tmp/wt", "feat/branch", cfg)

	// Must contain test-first guidance
	if !strings.Contains(prompt, "Test-First") {
		t.Fatal("expected test-first guidance section in prompt")
	}
	if !strings.Contains(prompt, "Write failing tests") {
		t.Fatal("expected 'Write failing tests' instruction in prompt")
	}
}

func TestAssemblePrompt_TestFirstGuidance_Disabled(t *testing.T) {
	cfg := &config.Config{Repo: "owner/repo", TestFirst: false}
	issue := github.Issue{Number: 10, Title: "add feature", Body: "Implement X."}
	base := "You are a worker.\n{{ISSUE_NUMBER}} {{ISSUE_TITLE}}\n{{ISSUE_BODY}}\n{{BRANCH}} {{WORKTREE}} {{REPO}}"

	prompt := assemblePrompt(base, issue, "/tmp/wt", "feat/branch", cfg)

	if strings.Contains(prompt, "Test-First") {
		t.Fatal("test-first guidance should NOT appear when TestFirst is false")
	}
}

func TestAssemblePrompt_ValidationContract_FileExists(t *testing.T) {
	dir := t.TempDir()
	validationContent := `## Validation Contract
- [ ] API returns 200 on valid input
- [ ] Error returned for missing field
`
	if err := os.WriteFile(filepath.Join(dir, "VALIDATION.md"), []byte(validationContent), 0644); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{Repo: "owner/repo", TestFirst: true}
	issue := github.Issue{Number: 5, Title: "test", Body: "body"}
	base := "Worker.\n{{ISSUE_NUMBER}} {{ISSUE_TITLE}}\n{{ISSUE_BODY}}\n{{BRANCH}} {{WORKTREE}} {{REPO}}"

	prompt := assemblePrompt(base, issue, dir, "feat/b", cfg)

	if !strings.Contains(prompt, "API returns 200 on valid input") {
		t.Fatal("expected VALIDATION.md content injected into prompt")
	}
	if !strings.Contains(prompt, "Validation Contract") {
		t.Fatal("expected validation contract header in prompt")
	}
}

func TestAssemblePrompt_ValidationContract_NoFile(t *testing.T) {
	dir := t.TempDir() // no VALIDATION.md

	cfg := &config.Config{Repo: "owner/repo", TestFirst: true}
	issue := github.Issue{Number: 5, Title: "test", Body: "body"}
	base := "Worker.\n{{ISSUE_NUMBER}} {{ISSUE_TITLE}}\n{{ISSUE_BODY}}\n{{BRANCH}} {{WORKTREE}} {{REPO}}"

	prompt := assemblePrompt(base, issue, dir, "feat/b", cfg)

	// Should still have test-first guidance but no validation contract section
	if !strings.Contains(prompt, "Test-First") {
		t.Fatal("expected test-first guidance even without VALIDATION.md")
	}
	// Should not contain the injected header for validation contract file
	if strings.Contains(prompt, "Validation Contract (from VALIDATION.md)") {
		t.Fatal("should not inject validation contract header when file doesn't exist")
	}
}

func TestAssemblePrompt_LegacyMode_TestFirst(t *testing.T) {
	// Legacy mode: base does NOT contain {{ISSUE_NUMBER}}
	cfg := &config.Config{Repo: "owner/repo", TestFirst: true}
	issue := github.Issue{Number: 7, Title: "fix bug", Body: "Fix it."}

	prompt := assemblePrompt("base prompt without templates", issue, "/tmp/wt", "feat/b", cfg)

	// Legacy mode should also get test-first guidance
	if !strings.Contains(prompt, "Test-First") {
		t.Fatal("expected test-first guidance in legacy mode prompt")
	}
}

func TestReadValidationContract_Exists(t *testing.T) {
	dir := t.TempDir()
	content := "## Done criteria\n- build passes\n"
	if err := os.WriteFile(filepath.Join(dir, "VALIDATION.md"), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	got := readValidationContract(dir)
	if got != content {
		t.Fatalf("readValidationContract = %q, want %q", got, content)
	}
}

func TestReadValidationContract_Missing(t *testing.T) {
	dir := t.TempDir()
	got := readValidationContract(dir)
	if got != "" {
		t.Fatalf("readValidationContract should return empty for missing file, got %q", got)
	}
}

func TestTestFirstGuidanceContent(t *testing.T) {
	guidance := testFirstGuidance()

	required := []string{
		"Write failing tests",
		"Implement until tests pass",
		"assertions",
		"validation contract",
	}
	for _, want := range required {
		if !strings.Contains(strings.ToLower(guidance), strings.ToLower(want)) {
			t.Errorf("test-first guidance missing %q", want)
		}
	}
}
