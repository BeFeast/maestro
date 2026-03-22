package worker

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/befeast/maestro/internal/github"
)

func TestGenerateValidationContract_WritesFile(t *testing.T) {
	dir := t.TempDir()
	issue := github.Issue{
		Number: 42,
		Title:  "Add user authentication",
		Body:   "Implement OAuth2 login flow with Google provider.\n\n- Users can sign in with Google\n- Session persists across page reloads\n- Logout clears session",
	}

	contract, err := GenerateValidationContract(issue, dir)
	if err != nil {
		t.Fatalf("GenerateValidationContract() error: %v", err)
	}

	// Contract text should be returned
	if contract == "" {
		t.Fatal("expected non-empty contract string")
	}

	// VALIDATION.md should be written to the worktree
	data, err := os.ReadFile(filepath.Join(dir, "VALIDATION.md"))
	if err != nil {
		t.Fatalf("expected VALIDATION.md to exist: %v", err)
	}
	if string(data) != contract {
		t.Error("file content should match returned contract")
	}
}

func TestGenerateValidationContract_ContainsIssueInfo(t *testing.T) {
	dir := t.TempDir()
	issue := github.Issue{
		Number: 99,
		Title:  "Fix broken pagination",
		Body:   "The pagination component breaks when there are more than 100 items.",
	}

	contract, err := GenerateValidationContract(issue, dir)
	if err != nil {
		t.Fatalf("GenerateValidationContract() error: %v", err)
	}

	required := []string{
		"#99",
		"Fix broken pagination",
		"Quality Gates",
		"Build passes",
		"Tests pass",
	}
	for _, want := range required {
		if !strings.Contains(contract, want) {
			t.Errorf("contract missing %q", want)
		}
	}
}

func TestGenerateValidationContract_ContainsTestFirstSection(t *testing.T) {
	dir := t.TempDir()
	issue := github.Issue{
		Number: 10,
		Title:  "Add search feature",
		Body:   "Add full-text search to the API.",
	}

	contract, err := GenerateValidationContract(issue, dir)
	if err != nil {
		t.Fatalf("GenerateValidationContract() error: %v", err)
	}

	required := []string{
		"Test-First",
		"Write failing tests",
		"Implement until tests pass",
	}
	for _, want := range required {
		if !strings.Contains(contract, want) {
			t.Errorf("contract missing %q", want)
		}
	}
}

func TestGenerateValidationContract_ContainsDoneCriteria(t *testing.T) {
	dir := t.TempDir()
	issue := github.Issue{
		Number: 5,
		Title:  "Refactor config loader",
		Body:   "Simplify the config loading logic.",
	}

	contract, err := GenerateValidationContract(issue, dir)
	if err != nil {
		t.Fatalf("GenerateValidationContract() error: %v", err)
	}

	required := []string{
		"Done Criteria",
		"Partial",
	}
	for _, want := range required {
		if !strings.Contains(contract, want) {
			t.Errorf("contract missing %q", want)
		}
	}
}

func TestGenerateValidationContract_EmptyBody(t *testing.T) {
	dir := t.TempDir()
	issue := github.Issue{
		Number: 1,
		Title:  "Minimal issue",
		Body:   "",
	}

	contract, err := GenerateValidationContract(issue, dir)
	if err != nil {
		t.Fatalf("GenerateValidationContract() error: %v", err)
	}

	// Should still produce a valid contract even with empty body
	if !strings.Contains(contract, "#1") {
		t.Error("contract should reference issue number even with empty body")
	}
	if !strings.Contains(contract, "Quality Gates") {
		t.Error("contract should include quality gates even with empty body")
	}
}
