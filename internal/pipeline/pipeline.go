// Package pipeline implements the planner → implementer → validator phase pipeline.
//
// When pipeline mode is enabled in config, each issue goes through three phases:
//  1. Plan   — creates MAESTRO_PLAN.md + VALIDATION.md in the worktree
//  2. Implement — writes code based on the plan (current worker behavior)
//  3. Validate — checks assertions from VALIDATION.md, gates PR creation
//
// Each phase runs as a separate worker session in the same worktree.
// Failed validation retries the implementer with feedback, not a full re-plan.
package pipeline

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/state"
)

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
