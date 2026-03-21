package pipeline

import (
	"fmt"
	"os"
	"strings"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/github"
	"github.com/befeast/maestro/internal/state"
)

const defaultResearchPrompt = `You are a **researcher** for a coding agent pipeline. Your job is to analyze a GitHub issue and the surrounding codebase to produce a research context file that will help the implementer work more effectively.

## Issue

**#{{ISSUE_NUMBER}}: {{ISSUE_TITLE}}**

{{ISSUE_BODY}}

## Repository

- Repo: {{REPO}}
- Worktree: {{WORKTREE}}
- Branch: {{BRANCH}}

## Instructions

1. Read and understand the issue requirements.
2. Scan the codebase for patterns relevant to this task:
   - Find existing code that does similar things
   - Identify files that will likely need modification
   - Note architectural patterns and conventions used in the project
   - Look for related tests that cover similar functionality
3. Create the directory ` + "`.maestro/research/`" + ` if it doesn't exist.
4. Write ` + "`.maestro/research/RESEARCH_CONTEXT.md`" + ` with:
   - **Relevant Files**: list of files relevant to this issue with brief descriptions
   - **Code Patterns**: conventions and patterns found in the codebase
   - **Similar Implementations**: existing code that does similar things
   - **Test Patterns**: how tests are structured in this project
   - **Potential Risks**: edge cases or architectural conflicts to watch for
5. Commit the research file with message: "research: add context for #{{ISSUE_NUMBER}}"
6. Do NOT implement the issue. Only research and document findings.
7. Do NOT create a PR.
`

const testMappingPreamble = `## Test Mapping Requirements

**Every change must have automated verification.**

You MUST create a verification script at ` + "`.maestro/verify.sh`" + ` that:
1. Maps each requirement from the issue to a specific test or check command
2. Runs all verification commands (build, test, lint, etc.)
3. Exits with code 0 only if ALL verifications pass

Example format:
` + "```bash" + `
#!/bin/bash
set -e
# Verification for Issue #{{ISSUE_NUMBER}}: {{ISSUE_TITLE}}

# Build check
go build ./...

# Test check
go test ./...

# Lint check
go vet ./...

echo "All verifications passed"
` + "```" + `

Run ` + "`.maestro/verify.sh`" + ` after implementation and include results in the PR description.
If any verification fails, fix the issue before creating the PR.

`

const defaultPlannerPrompt = `You are a **planner** for a coding agent pipeline. Your job is to analyze a GitHub issue and produce two artifacts in the repository root:

1. **MAESTRO_PLAN.md** — a step-by-step implementation plan
2. **VALIDATION.md** — a validation contract with concrete assertions

## Issue

**#{{ISSUE_NUMBER}}: {{ISSUE_TITLE}}**

{{ISSUE_BODY}}

## Repository

- Repo: {{REPO}}
- Worktree: {{WORKTREE}}
- Branch: {{BRANCH}}

## Instructions

1. Read and understand the codebase relevant to this issue.
2. Create ` + "`MAESTRO_PLAN.md`" + ` in the worktree root with:
   - A summary of what needs to change
   - Ordered implementation steps (files to create/modify, functions to add/change)
   - Dependencies between steps
   - Any risks or edge cases
3. Create ` + "`VALIDATION.md`" + ` in the worktree root with:
   - Concrete assertions that can be checked after implementation
   - Expected test outcomes (which tests should pass)
   - Build verification steps
   - Smoke tests or behavioral checks
   - Each assertion should be a clear pass/fail criterion
4. Commit both files with message: "plan: add implementation plan for #{{ISSUE_NUMBER}}"
5. Do NOT implement the issue. Only plan and define validation criteria.
6. Do NOT create a PR.
`

const defaultValidatorPrompt = `You are a **validator** for a coding agent pipeline. Your job is to verify that an implementation meets the validation contract.

## Issue

**#{{ISSUE_NUMBER}}: {{ISSUE_TITLE}}**

{{ISSUE_BODY}}

## Repository

- Repo: {{REPO}}
- Worktree: {{WORKTREE}}
- Branch: {{BRANCH}}

## Instructions

1. Read ` + "`VALIDATION.md`" + ` in the worktree root — this is your validation contract.
2. Read ` + "`MAESTRO_PLAN.md`" + ` for context on what was planned.
3. For each assertion in VALIDATION.md:
   - Check whether the implementation satisfies it
   - Run any specified tests or build commands
   - Verify behavioral expectations
4. Create ` + "`VALIDATION_RESULT.md`" + ` in the worktree root with:
   - First line: either "PASS" or "FAIL"
   - For each assertion: whether it passed or failed, with details
   - If FAIL: specific feedback on what needs to change
5. Commit the result file.
6. Do NOT modify implementation code.
7. Do NOT create a PR.
`

const validatorRetryPreamble = `## Previous Validation Feedback

The previous implementation attempt failed validation. Here is the feedback from the validator:

---
%s
---

Please address the issues described above in your implementation.

`

// PromptForPhase returns the prompt content for a given pipeline phase.
// It loads custom prompts from config paths, falling back to built-in defaults.
// When worktreePath and branchName are provided, template variables are substituted.
// When empty, variables like {{WORKTREE}} are left for later substitution (e.g. by assemblePrompt).
func PromptForPhase(cfg *config.Config, phase state.Phase, issue github.Issue, worktreePath, branchName string) string {
	var base string
	switch phase {
	case state.PhaseResearch:
		base = loadPromptOrDefault(cfg.Pipeline.Research.Prompt, defaultResearchPrompt)
	case state.PhasePlan:
		base = loadPromptOrDefault(cfg.Pipeline.Planner.Prompt, defaultPlannerPrompt)
	case state.PhaseValidate:
		base = loadPromptOrDefault(cfg.Pipeline.Validator.Prompt, defaultValidatorPrompt)
	default:
		return "" // implementer uses existing prompt system
	}

	return substituteVars(base, issue, worktreePath, branchName, cfg.Repo)
}

// PromptTemplateForPhase returns the raw prompt template for a phase without variable substitution.
// This is useful when the caller will handle substitution later (e.g. worker.Start → assemblePrompt).
func PromptTemplateForPhase(cfg *config.Config, phase state.Phase) string {
	switch phase {
	case state.PhaseResearch:
		return loadPromptOrDefault(cfg.Pipeline.Research.Prompt, defaultResearchPrompt)
	case state.PhasePlan:
		return loadPromptOrDefault(cfg.Pipeline.Planner.Prompt, defaultPlannerPrompt)
	case state.PhaseValidate:
		return loadPromptOrDefault(cfg.Pipeline.Validator.Prompt, defaultValidatorPrompt)
	default:
		return ""
	}
}

// ImplementerPreamble returns extra context to prepend to the implementer prompt
// when running in pipeline mode. This includes instructions to read the plan,
// research context, test mapping requirements, and any validation feedback.
func ImplementerPreamble(cfg *config.Config, sess *state.Session) string {
	var sb strings.Builder
	sb.WriteString("## Pipeline Mode\n\n")
	sb.WriteString("This issue is being worked on in pipeline mode. ")
	sb.WriteString("Read `MAESTRO_PLAN.md` in the worktree root for the implementation plan.\n")
	sb.WriteString("Follow the plan steps in order.\n\n")

	// Include research context reference if research phase was run
	if cfg.Pipeline.Research.Enabled {
		sb.WriteString("A research context file is available at `.maestro/research/RESEARCH_CONTEXT.md`.\n")
		sb.WriteString("Read it for relevant codebase patterns and findings before implementing.\n\n")
	}

	// Include test mapping requirements if enabled
	if cfg.Pipeline.TestMapping {
		sb.WriteString(testMappingPreamble)
	}

	if sess.ValidationFeedback != "" {
		sb.WriteString(fmt.Sprintf(validatorRetryPreamble, sess.ValidationFeedback))
	}

	return sb.String()
}

func loadPromptOrDefault(path, defaultPrompt string) string {
	if path == "" {
		return defaultPrompt
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return defaultPrompt
	}
	return string(data)
}

func substituteVars(tmpl string, issue github.Issue, worktreePath, branchName, repo string) string {
	r := strings.NewReplacer(
		"{{ISSUE_NUMBER}}", fmt.Sprintf("%d", issue.Number),
		"{{ISSUE_TITLE}}", issue.Title,
		"{{ISSUE_BODY}}", issue.Body,
		"{{BRANCH}}", branchName,
		"{{WORKTREE}}", worktreePath,
		"{{REPO}}", repo,
	)
	return r.Replace(tmpl)
}
