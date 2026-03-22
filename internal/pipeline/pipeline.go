// Package pipeline implements the planner → implementer → validator phase pipeline
// and the GSD-inspired pre-worker context preparation phases.
//
// Phase pipeline (when pipeline.enabled is true):
//  1. Plan   — creates MAESTRO_PLAN.md + VALIDATION.md in the worktree
//  2. Implement — writes code based on the plan (current worker behavior)
//  3. Validate — checks assertions from VALIDATION.md, gates PR creation
//
// GSD pre-worker phases (configurable independently):
//   - Research: scans codebase for relevant patterns, writes context file
//   - Plan Validation: extracts requirements, builds and validates a plan
//   - Test Mapping: maps requirements to verification commands, generates verify.sh
//
// Each phase-pipeline phase runs as a separate worker session in the same worktree.
// Failed validation retries the implementer with feedback, not a full re-plan.
// GSD phases run before the worker starts and inject context into the prompt.
package pipeline

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/state"
)

// ---- Phase-based pipeline (planner → implementer → validator) ----

const (
	// PlanFile is the plan artifact written by the planner phase.
	PlanFile = "MAESTRO_PLAN.md"
	// ValidationFile is the validation contract written by the planner phase.
	ValidationFile = "VALIDATION.md"
	// ValidationResultFile is written by the validator with pass/fail + feedback.
	ValidationResultFile = "VALIDATION_RESULT.md"
)

// IsEnabled returns true if the pipeline is enabled in config.
func IsEnabled(cfg *config.Config) bool {
	return cfg.Pipeline.Enabled
}

// InitialPhase returns the first phase for a new session based on config.
// If the planner is disabled, starts directly with implement.
func InitialPhase(cfg *config.Config) state.Phase {
	if !cfg.Pipeline.Enabled {
		return state.PhaseNone
	}
	if cfg.Pipeline.Planner.Enabled {
		return state.PhasePlan
	}
	return state.PhaseImplement
}

// NextPhase returns the phase that should follow the given completed phase.
// Returns PhaseNone when the pipeline is complete (after validate or implement
// if validator is disabled).
func NextPhase(cfg *config.Config, completed state.Phase) state.Phase {
	switch completed {
	case state.PhasePlan:
		return state.PhaseImplement
	case state.PhaseImplement:
		if cfg.Pipeline.Validator.Enabled {
			return state.PhaseValidate
		}
		return state.PhaseNone // no validator → done, proceed to PR flow
	case state.PhaseValidate:
		return state.PhaseNone // pipeline complete
	default:
		return state.PhaseNone
	}
}

// PlanArtifactsExist checks whether the planner wrote its required artifacts.
func PlanArtifactsExist(worktreePath string) bool {
	planPath := filepath.Join(worktreePath, PlanFile)
	valPath := filepath.Join(worktreePath, ValidationFile)
	_, errPlan := os.Stat(planPath)
	_, errVal := os.Stat(valPath)
	return errPlan == nil && errVal == nil
}

// ValidationPassed checks whether the validator wrote a passing result.
// Returns (passed, feedback, error).
func ValidationPassed(worktreePath string) (bool, string, error) {
	resultPath := filepath.Join(worktreePath, ValidationResultFile)
	data, err := os.ReadFile(resultPath)
	if err != nil {
		return false, "", fmt.Errorf("read validation result: %w", err)
	}
	content := string(data)
	// Look for PASS/FAIL marker in the first line
	firstLine := strings.SplitN(content, "\n", 2)[0]
	firstLine = strings.TrimSpace(strings.ToUpper(firstLine))
	if strings.Contains(firstLine, "PASS") {
		return true, "", nil
	}
	// Extract feedback (everything after the first line)
	feedback := ""
	if parts := strings.SplitN(content, "\n", 2); len(parts) > 1 {
		feedback = strings.TrimSpace(parts[1])
	}
	return false, feedback, nil
}

// BackendForPhase returns the backend name to use for a given phase.
// Falls back to the default backend if no role-specific backend is configured.
func BackendForPhase(cfg *config.Config, phase state.Phase) string {
	switch phase {
	case state.PhasePlan:
		if cfg.Pipeline.Planner.Backend != "" {
			return cfg.Pipeline.Planner.Backend
		}
	case state.PhaseValidate:
		if cfg.Pipeline.Validator.Backend != "" {
			return cfg.Pipeline.Validator.Backend
		}
	}
	return cfg.Model.Default
}

// MaxRuntimeForPhase returns the max runtime in minutes for a given phase.
// Falls back to the global max_runtime_minutes if no role-specific value is set.
func MaxRuntimeForPhase(cfg *config.Config, phase state.Phase) int {
	switch phase {
	case state.PhasePlan:
		if cfg.Pipeline.Planner.MaxRuntimeMinutes > 0 {
			return cfg.Pipeline.Planner.MaxRuntimeMinutes
		}
	case state.PhaseValidate:
		if cfg.Pipeline.Validator.MaxRuntimeMinutes > 0 {
			return cfg.Pipeline.Validator.MaxRuntimeMinutes
		}
	}
	return cfg.MaxRuntimeMinutes
}

// ---- GSD-inspired pre-worker context preparation ----

// GSDResult holds the output of the GSD pre-worker pipeline phases.
// The context fields are appended to the worker prompt.
type GSDResult struct {
	// ResearchContext is the markdown context from the research phase.
	ResearchContext string

	// Plan is the validated plan text.
	Plan string

	// VerifyScript is the path to the generated verify.sh script.
	VerifyScript string

	// TestMappingSummary is a markdown summary of test mappings.
	TestMappingSummary string
}

// PromptSection returns a formatted string to append to the worker prompt.
// It includes all pipeline outputs that are non-empty.
func (r *GSDResult) PromptSection() string {
	if r == nil {
		return ""
	}

	var sections []string

	if r.ResearchContext != "" {
		sections = append(sections, "## Pre-coding Research\n\n"+r.ResearchContext)
	}
	if r.Plan != "" {
		sections = append(sections, "## Implementation Plan\n\nThe following plan has been validated. Use it as your guide:\n\n"+r.Plan)
	}
	if r.TestMappingSummary != "" {
		sections = append(sections, "## Test Mapping\n\n"+r.TestMappingSummary)
	}

	if len(sections) == 0 {
		return ""
	}

	return "\n\n---\n\n# Pipeline Context\n\n" + strings.Join(sections, "\n\n---\n\n")
}

// RunGSD executes the GSD-inspired pre-worker pipeline phases before a worker starts.
// All phases are best-effort — a failure in one phase logs a warning
// but does not prevent subsequent phases or worker startup.
func RunGSD(cfg *config.Config, worktreePath string, issueNumber int, issueTitle, issueBody string) *GSDResult {
	result := &GSDResult{}
	pipeline := cfg.Pipeline

	anyEnabled := pipeline.Research || pipeline.PlanValidationEnabled() || pipeline.TestMappingEnabled()
	if !anyEnabled {
		return result
	}

	log.Printf("[pipeline] running GSD pre-worker pipeline for issue #%d (research=%v, plan_validation=%v, test_mapping=%v)",
		issueNumber, pipeline.Research, pipeline.PlanValidationEnabled(), pipeline.TestMappingEnabled())

	// Phase 1: Research
	if pipeline.Research {
		log.Printf("[pipeline] GSD phase 1/3: research")
		ctx, err := runResearch(worktreePath, issueNumber, issueTitle, issueBody)
		if err != nil {
			log.Printf("[pipeline] research phase error: %v (continuing)", err)
		} else {
			result.ResearchContext = ctx
		}
	}

	// Phase 2: Plan Validation
	if pipeline.PlanValidationEnabled() {
		log.Printf("[pipeline] GSD phase 2/3: plan validation")
		plan, err := validatePlan(issueNumber, issueTitle, issueBody, worktreePath, result.ResearchContext)
		if err != nil {
			log.Printf("[pipeline] plan validation error: %v (continuing)", err)
		} else {
			result.Plan = plan
		}
	}

	// Phase 3: Test Mapping
	if pipeline.TestMappingEnabled() {
		log.Printf("[pipeline] GSD phase 3/3: test mapping")
		verifyPath, summary, err := mapTests(issueNumber, issueTitle, issueBody, worktreePath, result.Plan)
		if err != nil {
			log.Printf("[pipeline] test mapping error: %v (continuing)", err)
		} else {
			result.VerifyScript = verifyPath
			result.TestMappingSummary = summary
		}
	}

	log.Printf("[pipeline] GSD pre-worker pipeline complete for issue #%d", issueNumber)
	return result
}
