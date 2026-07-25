package pipeline

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/github"
	"github.com/befeast/maestro/internal/state"
)

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

## Search Safety

- Use the worktree as the current directory before code search commands: ` + "`cd {{WORKTREE}}`" + `.
- Do NOT run ` + "`rg`" + `, ` + "`find`" + `, or ` + "`grep`" + ` from broad filesystem roots such as ` + "`/`" + `, ` + "`/mnt`" + `, or ` + "`/home`" + `.
- If a broad host search is intentional, set ` + "`MAESTRO_ALLOW_BROAD_SEARCH=1`" + ` for that single command.

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

const defaultAdvisorPrompt = `You are an independent **Advisor** in a coding-agent pipeline. Review the issue, the canonical implementation plan, and the validation contract before any implementation begins.

You are review-only. You must not edit source files, mutate MAESTRO_PLAN.md or VALIDATION.md, commit, push, create a PR, or otherwise change git HEAD. The only file you may create is MAESTRO_PLAN_REVIEW.md.

Write MAESTRO_PLAN_REVIEW.md with an exact first line:

- PLAN_APPROVED when the plan and validation contract are ready for implementation; or
- PLAN_REVISE when the planner must address findings before implementation.

For PLAN_REVISE, put specific unresolved findings after the first line. Do not place any preamble, Markdown fence, whitespace, or commentary before the verdict token. Missing or malformed output fails closed.
`

const defaultValidatorPrompt = `You are a **validator** for a coding agent pipeline. Your job is to verify that an implementation meets the validation contract.

## Issue

**#{{ISSUE_NUMBER}}: {{ISSUE_TITLE}}**

{{ISSUE_BODY}}

## Repository

- Repo: {{REPO}}
- Worktree: {{WORKTREE}}
- Branch: {{BRANCH}}

## Search Safety

- Use the worktree as the current directory before code search commands: ` + "`cd {{WORKTREE}}`" + `.
- Do NOT run ` + "`rg`" + `, ` + "`find`" + `, or ` + "`grep`" + ` from broad filesystem roots such as ` + "`/`" + `, ` + "`/mnt`" + `, or ` + "`/home`" + `.
- If a broad host search is intentional, set ` + "`MAESTRO_ALLOW_BROAD_SEARCH=1`" + ` for that single command.

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
	case state.PhasePlan:
		base = loadPromptOrDefault(cfg.Pipeline.Planner.Prompt, defaultPlannerPrompt)
	case state.PhaseAdvisor:
		base = loadPromptOrDefault(cfg.Pipeline.Advisor.Prompt, defaultAdvisorPrompt)
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
	case state.PhasePlan:
		return loadPromptOrDefault(cfg.Pipeline.Planner.Prompt, defaultPlannerPrompt)
	case state.PhaseAdvisor:
		return loadPromptOrDefault(cfg.Pipeline.Advisor.Prompt, defaultAdvisorPrompt)
	case state.PhaseValidate:
		return loadPromptOrDefault(cfg.Pipeline.Validator.Prompt, defaultValidatorPrompt)
	default:
		return ""
	}
}

// AdvisorPrompt builds the mandatory Advisor packet. The packet is appended
// even for a custom prompt so the issue, artifacts, version/round, ledger, and
// strict verdict contract can never be accidentally omitted by a template.
func AdvisorPrompt(cfg *config.Config, issue github.Issue, worktreePath, branchName string, sess *state.Session) (string, error) {
	plan, err := os.ReadFile(filepath.Join(worktreePath, PlanFile))
	if err != nil {
		return "", fmt.Errorf("read %s: %w", PlanFile, err)
	}
	validation, err := os.ReadFile(filepath.Join(worktreePath, ValidationFile))
	if err != nil {
		return "", fmt.Errorf("read %s: %w", ValidationFile, err)
	}
	base := PromptForPhase(cfg, state.PhaseAdvisor, issue, worktreePath, branchName)
	ledger := "None."
	planVersion := 0
	reviewRound := 0
	if sess != nil {
		planVersion = sess.PlanVersion
		reviewRound = sess.AdvisorReviewRound
		if strings.TrimSpace(sess.AdvisorFindingsLedger) != "" {
			ledger = sess.AdvisorFindingsLedger
		}
	}
	return fmt.Sprintf(`%s

---

## Mandatory Advisor Review Packet

### Issue

**#%d: %s**

%s

### Review identity

- Plan version: %d
- Review round: %d

### Compact unresolved-findings ledger

%s

### Current MAESTRO_PLAN.md

%s

### Current VALIDATION.md

%s

Re-check the strict verdict and review-only rules above. Write only %s; do not mutate either canonical artifact.
`, base, issue.Number, issue.Title, issue.Body, planVersion, reviewRound, ledger, string(plan), string(validation), AdvisorReviewFile), nil
}

// PlannerRevisionPrompt gives the planner sole ownership of canonical plan
// changes while carrying every accumulated Advisor finding into the revision.
func PlannerRevisionPrompt(cfg *config.Config, issue github.Issue, worktreePath, branchName string, sess *state.Session) string {
	base := PromptForPhase(cfg, state.PhasePlan, issue, worktreePath, branchName)
	ledger := "No Advisor findings were recorded."
	version := 0
	if sess != nil {
		version = sess.PlanVersion
		if strings.TrimSpace(sess.AdvisorFindingsLedger) != "" {
			ledger = sess.AdvisorFindingsLedger
		}
	}
	return fmt.Sprintf(`%s

---

## Advisor-requested plan revision

You remain the sole owner of MAESTRO_PLAN.md and VALIDATION.md. Revise both canonical artifacts as needed to address every finding below. Do not implement the issue and do not create a PR.

Current plan version: %d

### Accumulated Advisor findings

%s
`, base, version, ledger)
}

// ImplementerPreamble returns extra context to prepend to the implementer prompt
// when running in pipeline mode. This includes instructions to read the plan
// and any validation feedback from previous attempts.
func ImplementerPreamble(sess *state.Session) string {
	var sb strings.Builder
	sb.WriteString("## Pipeline Mode\n\n")
	sb.WriteString("This issue is being worked on in pipeline mode. ")
	sb.WriteString("Read `MAESTRO_PLAN.md` in the worktree root for the implementation plan.\n")
	sb.WriteString("Follow the plan steps in order.\n\n")

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
