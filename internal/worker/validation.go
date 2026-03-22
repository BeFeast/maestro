package worker

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/befeast/maestro/internal/github"
)

// GenerateValidationContract produces a structured validation contract for the
// given issue and writes it as VALIDATION.md in the worktree directory.
// Returns the contract text and any error.
//
// The contract encodes:
//   - Test-first workflow instructions
//   - Required assertions derived from the issue description
//   - Quality gates (build, test, lint, smoke test)
//   - Done vs partial criteria
func GenerateValidationContract(issue github.Issue, worktreePath string) (string, error) {
	var b strings.Builder

	b.WriteString(fmt.Sprintf("# Validation Contract — Issue #%d: %s\n\n", issue.Number, issue.Title))

	// Test-first workflow
	b.WriteString("## Test-First Workflow\n\n")
	b.WriteString("Before writing implementation code, follow this sequence:\n\n")
	b.WriteString("1. **Identify assertions** — extract testable requirements from the issue description\n")
	b.WriteString("2. **Write failing tests** — encode each assertion as a test that fails before implementation\n")
	b.WriteString("3. **Implement until tests pass** — write the minimum code to make each test green\n")
	b.WriteString("4. **Refactor** — clean up only after all tests pass\n\n")
	b.WriteString("This prevents implementation-led tests that merely confirm what was written.\n\n")

	// Issue requirements
	b.WriteString("## Issue Requirements\n\n")
	if strings.TrimSpace(issue.Body) != "" {
		b.WriteString(issue.Body)
		b.WriteString("\n\n")
	} else {
		b.WriteString("(No description provided — derive assertions from the issue title.)\n\n")
	}

	// Quality gates
	b.WriteString("## Quality Gates\n\n")
	b.WriteString("All of the following must pass before the PR is opened:\n\n")
	b.WriteString("- [ ] **Build passes** — project compiles without errors\n")
	b.WriteString("- [ ] **Tests pass** — all existing and new tests are green\n")
	b.WriteString("- [ ] **Lint clean** — no formatter or linter warnings\n")
	b.WriteString("- [ ] **Smoke test** — manual verification that the feature works as described\n\n")

	// Done criteria
	b.WriteString("## Done Criteria\n\n")
	b.WriteString("**Done** means:\n")
	b.WriteString("- All quality gates pass\n")
	b.WriteString("- Tests cover the assertions derived from the issue\n")
	b.WriteString("- PR description includes validation evidence (which assertions passed)\n\n")
	b.WriteString("**Partial** means:\n")
	b.WriteString("- Some quality gates pass but not all\n")
	b.WriteString("- Implementation works but tests are missing or incomplete\n")
	b.WriteString("- PR opened without validation evidence\n")

	contract := b.String()

	// Write to worktree
	outPath := filepath.Join(worktreePath, "VALIDATION.md")
	if err := os.WriteFile(outPath, []byte(contract), 0644); err != nil {
		return "", fmt.Errorf("write VALIDATION.md: %w", err)
	}

	return contract, nil
}
