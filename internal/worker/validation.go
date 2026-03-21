package worker

import (
	"os"
	"path/filepath"
)

// testFirstGuidance returns the test-first workflow section to inject into worker prompts.
func testFirstGuidance() string {
	return `
---

## Test-First Workflow

Before writing implementation code, follow this test-first approach:

### Step 1: Identify assertions from the issue requirements
- Read the issue description carefully
- Extract concrete, testable behaviors (e.g. "function returns X when given Y")
- List edge cases and error conditions

### Step 2: Write failing tests that encode those assertions
- Write tests BEFORE implementation — tests should fail initially
- Each test should assert one specific behavior from the requirements
- Name tests descriptively: TestFeature_Scenario_ExpectedBehavior
- Include both happy-path and error-path tests

### Step 3: Implement until tests pass
- Write the minimum code to make each test pass
- Run tests after each change to verify progress
- Do not write tests that merely confirm what you just implemented — tests must be derived from requirements, not from your code

### Step 4: Verify the validation contract
- All required assertions pass
- Build passes cleanly
- No lint errors
- Include which assertions passed in the PR description under "## Validation"
`
}

// readValidationContract reads VALIDATION.md from the worktree if it exists.
// Returns empty string if the file does not exist.
func readValidationContract(worktreePath string) string {
	data, err := os.ReadFile(filepath.Join(worktreePath, "VALIDATION.md"))
	if err != nil {
		return ""
	}
	return string(data)
}
