package worker

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/github"
)

func TestAssemblePromptIncludesSecretSafetyGuardrails(t *testing.T) {
	cfg := &config.Config{Repo: "BeFeast/ok-gobot"}
	issue := github.Issue{
		Number: 157,
		Title:  "security hardening",
		Body:   "Fix secret handling.",
	}

	prompt := assemblePrompt("base prompt", issue, "/tmp/worktree", "codex/security", cfg)

	required := []string{
		"Do NOT commit or mention API keys",
		"Do NOT commit temp/debug artifacts such as tmp/, _tmp/, *.log, *.logs, *.test, or *.test.json",
		"Do NOT paste logs, doctor output, env dumps, or secret-bearing snippets into the PR body or comments.",
		`gh pr create --repo BeFeast/ok-gobot --title "security hardening" --body "Refs #157"`,
		"Do NOT use GitHub auto-closing keywords",
		"Code landing is not outcome completion",
	}
	for _, want := range required {
		if !strings.Contains(prompt, want) {
			t.Fatalf("assemblePrompt() missing %q\nprompt:\n%s", want, prompt)
		}
	}
	if containsAutoClosingIssueReference(prompt) {
		t.Fatalf("assemblePrompt() contains auto-closing issue reference:\n%s", prompt)
	}
}

func TestContainsAutoClosingIssueReference(t *testing.T) {
	tests := []struct {
		text string
		want bool
	}{
		{`Closes #157`, true},
		{`fixes #157`, true},
		{`Resolved #157`, true},
		{`Refs #157`, false},
		{`Implements #157`, false},
		{`Do NOT use Closes, Fixes, or Resolves.`, false},
	}

	for _, tt := range tests {
		if got := containsAutoClosingIssueReference(tt.text); got != tt.want {
			t.Fatalf("containsAutoClosingIssueReference(%q) = %v, want %v", tt.text, got, tt.want)
		}
	}
}

func TestAssemblePromptIncludesSearchSafetyGuardrails(t *testing.T) {
	cfg := &config.Config{Repo: "BeFeast/maestro"}
	issue := github.Issue{Number: 319, Title: "worker safety", Body: "Constrain broad searches."}

	prompt := assemblePrompt("base prompt", issue, "/tmp/worktree", "feat/safety", cfg)

	for _, want := range []string{
		"## Worker Search Safety",
		"The assigned worktree is `/tmp/worktree`",
		"Do NOT run `rg`, `find`, or `grep` from broad filesystem roots such as `/`, `/mnt`, or `/home`.",
		"MAESTRO_ALLOW_BROAD_SEARCH=1",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("assemblePrompt() missing %q\nprompt:\n%s", want, prompt)
		}
	}
}

// --- Tests from main: validation contract placeholder ---

func TestAssemblePrompt_ValidationContractFromFile(t *testing.T) {
	worktree := t.TempDir()
	validationContent := `## Validation Contract
- [ ] Build passes
- [ ] Tests pass
- [ ] Feature X works end-to-end`
	if err := os.WriteFile(filepath.Join(worktree, "VALIDATION.md"), []byte(validationContent), 0644); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{Repo: "BeFeast/maestro"}
	issue := github.Issue{Number: 42, Title: "test feature", Body: "Add feature X."}
	base := "Template with {{ISSUE_NUMBER}} and {{VALIDATION_CONTRACT}} here."

	prompt := assemblePrompt(base, issue, worktree, "feat/test-42", cfg)

	if !strings.Contains(prompt, validationContent) {
		t.Fatalf("expected VALIDATION.md content in prompt, got:\n%s", prompt)
	}
	if strings.Contains(prompt, "{{VALIDATION_CONTRACT}}") {
		t.Fatal("placeholder {{VALIDATION_CONTRACT}} was not replaced")
	}
}

func TestAssemblePrompt_ValidationContractMissingFile(t *testing.T) {
	worktree := t.TempDir() // no VALIDATION.md

	cfg := &config.Config{Repo: "BeFeast/maestro"}
	issue := github.Issue{Number: 42, Title: "test feature", Body: "Do stuff."}
	base := "Template with {{ISSUE_NUMBER}} and {{VALIDATION_CONTRACT}} end."

	prompt := assemblePrompt(base, issue, worktree, "feat/test-42", cfg)

	if strings.Contains(prompt, "{{VALIDATION_CONTRACT}}") {
		t.Fatal("placeholder {{VALIDATION_CONTRACT}} was not replaced when file is missing")
	}
	// Should contain a fallback message about no contract
	if !strings.Contains(prompt, "No VALIDATION.md found") {
		t.Fatalf("expected fallback message when VALIDATION.md missing, got:\n%s", prompt)
	}
}

func TestAssemblePrompt_NoValidationPlaceholder(t *testing.T) {
	// Templates without {{VALIDATION_CONTRACT}} and no VALIDATION.md should work unchanged
	cfg := &config.Config{Repo: "BeFeast/maestro"}
	issue := github.Issue{Number: 10, Title: "basic", Body: "Simple issue."}
	base := "Template with {{ISSUE_NUMBER}} only."

	prompt := assemblePrompt(base, issue, t.TempDir(), "feat/basic-10", cfg)

	if !strings.Contains(prompt, "10") {
		t.Fatal("expected issue number in prompt")
	}
	if strings.Contains(prompt, "VALIDATION") {
		t.Fatal("should not inject validation content when placeholder is absent and no file exists")
	}
}

func TestReadValidationContract(t *testing.T) {
	t.Run("file exists", func(t *testing.T) {
		dir := t.TempDir()
		content := "# Validation\n- build passes\n"
		if err := os.WriteFile(filepath.Join(dir, "VALIDATION.md"), []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
		got := readValidationContract(dir)
		if got != content {
			t.Fatalf("expected %q, got %q", content, got)
		}
	})

	t.Run("file missing", func(t *testing.T) {
		got := readValidationContract(t.TempDir())
		if got != "" {
			t.Fatalf("expected empty string for missing file, got %q", got)
		}
	})
}

// --- Tests for prompt sections ---

func TestAssemblePrompt_IncludesPromptSections(t *testing.T) {
	dir := t.TempDir()

	// Write two section files
	section1 := filepath.Join(dir, "tdd.md")
	section2 := filepath.Join(dir, "style.md")
	os.WriteFile(section1, []byte("## TDD Section\nWrite tests first."), 0644)
	os.WriteFile(section2, []byte("## Style Section\nFollow coding standards."), 0644)

	cfg := &config.Config{
		Repo:           "owner/repo",
		PromptSections: []string{section1, section2},
	}
	issue := github.Issue{Number: 1, Title: "test", Body: "body"}

	base := "Base prompt with {{ISSUE_NUMBER}}"
	prompt := assemblePrompt(base, issue, "/tmp/wt", "feat/branch", cfg)

	if !strings.Contains(prompt, "## TDD Section") {
		t.Error("prompt should include TDD section content")
	}
	if !strings.Contains(prompt, "## Style Section") {
		t.Error("prompt should include style section content")
	}
}

func TestAssemblePrompt_SkipsMissingSections(t *testing.T) {
	cfg := &config.Config{
		Repo:           "owner/repo",
		PromptSections: []string{"/nonexistent/path.md"},
	}
	issue := github.Issue{Number: 1, Title: "test", Body: "body"}

	base := "Base prompt with {{ISSUE_NUMBER}}"
	prompt := assemblePrompt(base, issue, "/tmp/wt", "feat/branch", cfg)

	// Should still produce a valid prompt without crashing
	if !strings.Contains(prompt, "Base prompt with 1") {
		t.Error("prompt should still contain base content when sections are missing")
	}
}

func TestAssemblePrompt_IncludesValidationContractAutoAppend(t *testing.T) {
	dir := t.TempDir()

	// Write a VALIDATION.md in the worktree
	validationContent := "## Validation Contract\n- [ ] Build passes\n- [ ] Tests pass"
	os.WriteFile(filepath.Join(dir, "VALIDATION.md"), []byte(validationContent), 0644)

	cfg := &config.Config{Repo: "owner/repo"}
	issue := github.Issue{Number: 5, Title: "test", Body: "body"}

	// Template WITHOUT {{VALIDATION_CONTRACT}} — contract should be auto-appended
	base := "Base prompt with {{ISSUE_NUMBER}}"
	prompt := assemblePrompt(base, issue, dir, "feat/branch", cfg)

	if !strings.Contains(prompt, "Validation Contract") {
		t.Error("prompt should auto-append VALIDATION.md content when placeholder is absent")
	}
	if !strings.Contains(prompt, "Build passes") {
		t.Error("prompt should include quality gates from VALIDATION.md")
	}
}

func TestAssemblePrompt_NoValidationFileIsOK(t *testing.T) {
	dir := t.TempDir() // empty dir, no VALIDATION.md

	cfg := &config.Config{Repo: "owner/repo"}
	issue := github.Issue{Number: 1, Title: "test", Body: "body"}

	base := "Base prompt with {{ISSUE_NUMBER}}"
	prompt := assemblePrompt(base, issue, dir, "feat/branch", cfg)

	// Should still produce a valid prompt
	if !strings.Contains(prompt, "Base prompt with 1") {
		t.Error("prompt should work without VALIDATION.md")
	}
}
