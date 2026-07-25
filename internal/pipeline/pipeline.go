// Package pipeline implements the planner → optional advisor → implementer → validator phase pipeline
// and the deterministic pre-worker context preparation phases.
//
// Phase pipeline (when pipeline.enabled is true):
//  1. Plan      — creates MAESTRO_PLAN.md + VALIDATION.md in the worktree
//  2. Advisor   — independently reviews the plan and either approves or requests revision
//  3. Implement — writes code based on the approved plan (current worker behavior)
//  4. Validate  — checks assertions from VALIDATION.md, gates PR creation
//
// Deterministic pre-worker context-prep phases (configurable independently):
//   - Research: scans codebase for relevant patterns, writes context file
//   - Plan Validation: heuristically extracts requirements and checks plan coverage
//   - Test Mapping: maps requirements to verification commands, generates verify.sh
//
// Each phase-pipeline phase runs as a separate worker session in the same worktree.
// Failed validation retries the implementer with feedback, not a full re-plan.
// Context-prep phases run before the worker starts and inject context into the prompt.
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

// ---- Phase-based pipeline (planner → optional advisor → implementer → validator) ----

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
	if cfg.Pipeline.Planner.Enabled || cfg.Pipeline.Advisor.Enabled {
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
		if cfg.Pipeline.Advisor.Enabled {
			return state.PhaseAdvisor
		}
		return state.PhaseImplement
	case state.PhaseAdvisor:
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

// roleForPhase returns the RoleConfig governing a pipeline phase, and whether a
// role is defined for it. The implement phase carries its own role (#841) so it
// can run on a cheap backend + low effort while plan/validate keep the strong
// model; PhaseNone / unknown phases have no role.
func roleForPhase(cfg *config.Config, phase state.Phase) (config.RoleConfig, bool) {
	switch phase {
	case state.PhasePlan:
		return cfg.Pipeline.Planner, true
	case state.PhaseAdvisor:
		return cfg.Pipeline.Advisor, true
	case state.PhaseImplement:
		return cfg.Pipeline.Implementer, true
	case state.PhaseValidate:
		return cfg.Pipeline.Validator, true
	default:
		return config.RoleConfig{}, false
	}
}

// BackendForPhase returns the backend name to use for a given phase.
// Falls back to the effective route default if no role-specific backend is configured.
// The implement phase honors pipeline.implementer.backend when set (#841); unset
// keeps the historical default behavior unless provider lanes override it.
func BackendForPhase(cfg *config.Config, phase state.Phase) string {
	if role, ok := roleForPhase(cfg, phase); ok {
		if b := strings.TrimSpace(role.Backend); b != "" {
			return b
		}
	}
	return cfg.Model.EffectiveDefault()
}

// EffortForPhase returns the reasoning-effort override configured for a pipeline
// phase's role (planner/implementer/validator), or "" when the role carries no
// effort (#841). The empty default preserves today's behavior: no per-phase
// --effort / -c model_reasoning_effort is emitted.
func EffortForPhase(cfg *config.Config, phase state.Phase) string {
	if role, ok := roleForPhase(cfg, phase); ok {
		return strings.TrimSpace(role.Effort)
	}
	return ""
}

// ApplyPhaseEffort returns a config whose backendName def carries the phase
// role's effort as a TierEffort override (#841), threaded into the worker argv by
// worker.appendTierModelEffort (claude `--effort`, codex `-c
// model_reasoning_effort`; gemini drops it as unsupported). When the phase has no
// effort configured — or the backend is unknown — cfg is returned unchanged, so
// today's dispatch is byte-for-byte preserved. The override is applied to a clone
// (deep-copying the Backends map) so the shared base config is never mutated,
// mirroring the routing-tier override path.
func ApplyPhaseEffort(cfg *config.Config, backendName string, phase state.Phase) *config.Config {
	effort := EffortForPhase(cfg, phase)
	if effort == "" {
		return cfg
	}
	def, ok := cfg.Model.Backends[backendName]
	if !ok {
		return cfg
	}
	clone := *cfg
	backends := make(map[string]config.BackendDef, len(cfg.Model.Backends))
	for k, v := range cfg.Model.Backends {
		backends[k] = v
	}
	def.Effort = effort
	def.TierEffort = effort
	backends[backendName] = def
	clone.Model.Backends = backends
	return &clone
}

// MaxRuntimeForPhase returns the max runtime in minutes for a given phase.
// Falls back to the global max_runtime_minutes if no role-specific value is set.
func MaxRuntimeForPhase(cfg *config.Config, phase state.Phase) int {
	if role, ok := roleForPhase(cfg, phase); ok {
		if role.MaxRuntimeMinutes > 0 {
			return role.MaxRuntimeMinutes
		}
	}
	return cfg.MaxRuntimeMinutes
}

// ---- Deterministic pre-worker context preparation ----

// GSDResult holds the output of the deterministic pre-worker context-prep phases.
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

// RunGSD executes deterministic context-prep phases before a worker starts.
// All phases are best-effort — a failure in one phase logs a warning
// but does not prevent subsequent phases or worker startup.
func RunGSD(cfg *config.Config, worktreePath string, issueNumber int, issueTitle, issueBody string) *GSDResult {
	result := &GSDResult{}
	pipeline := cfg.Pipeline

	anyEnabled := pipeline.Research || pipeline.PlanValidationEnabled() || pipeline.TestMappingEnabled()
	if !anyEnabled {
		return result
	}

	log.Printf("[pipeline] running deterministic context-prep for issue #%d (research=%v, heuristic_plan_validation=%v, test_mapping=%v)",
		issueNumber, pipeline.Research, pipeline.PlanValidationEnabled(), pipeline.TestMappingEnabled())

	// Phase 1: Research
	if pipeline.Research {
		log.Printf("[pipeline] context-prep phase 1/3: research")
		ctx, err := runResearch(worktreePath, issueNumber, issueTitle, issueBody)
		if err != nil {
			log.Printf("[pipeline] research phase error: %v (continuing)", err)
		} else {
			result.ResearchContext = ctx
		}
	}

	// Phase 2: heuristic plan validation
	if pipeline.PlanValidationEnabled() {
		log.Printf("[pipeline] context-prep phase 2/3: heuristic plan validation")
		plan, err := validatePlan(issueNumber, issueTitle, issueBody, worktreePath, result.ResearchContext)
		if err != nil {
			log.Printf("[pipeline] plan validation error: %v (continuing)", err)
		} else {
			result.Plan = plan
		}
	}

	// Phase 3: Test Mapping
	if pipeline.TestMappingEnabled() {
		log.Printf("[pipeline] context-prep phase 3/3: test mapping")
		verifyPath, summary, err := mapTests(issueNumber, issueTitle, issueBody, worktreePath, result.Plan)
		if err != nil {
			log.Printf("[pipeline] test mapping error: %v (continuing)", err)
		} else {
			result.VerifyScript = verifyPath
			result.TestMappingSummary = summary
		}
	}

	log.Printf("[pipeline] deterministic context-prep complete for issue #%d", issueNumber)
	return result
}
