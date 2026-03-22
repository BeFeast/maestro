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
		`gh pr create --repo BeFeast/ok-gobot --title "security hardening" --body "Closes #157"`,
	}
	for _, want := range required {
		if !strings.Contains(prompt, want) {
			t.Fatalf("assemblePrompt() missing %q\nprompt:\n%s", want, prompt)
		}
	}
}

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
	// Templates without {{VALIDATION_CONTRACT}} should work unchanged
	cfg := &config.Config{Repo: "BeFeast/maestro"}
	issue := github.Issue{Number: 10, Title: "basic", Body: "Simple issue."}
	base := "Template with {{ISSUE_NUMBER}} only."

	prompt := assemblePrompt(base, issue, t.TempDir(), "feat/basic-10", cfg)

	if !strings.Contains(prompt, "10") {
		t.Fatal("expected issue number in prompt")
	}
	if strings.Contains(prompt, "VALIDATION") {
		t.Fatal("should not inject validation content when placeholder is absent")
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
