package orchestrator

import (
	"fmt"
	"log"
	"time"

	"github.com/befeast/maestro/internal/github"
	"github.com/befeast/maestro/internal/pipeline"
	"github.com/befeast/maestro/internal/state"
	"github.com/befeast/maestro/internal/worker"
)

// advancePipeline handles phase transitions for pipeline-enabled sessions.
// Called when a running pipeline worker's process dies.
// Returns true if the session was handled by pipeline logic (caller should skip normal dead-worker handling).
func (o *Orchestrator) advancePipeline(slotName string, sess *state.Session) bool {
	if sess.Phase == state.PhaseNone {
		return false // not a pipeline session
	}

	switch sess.Phase {
	case state.PhaseResearch:
		return o.handleResearchComplete(slotName, sess)
	case state.PhasePlan:
		return o.handlePlanComplete(slotName, sess)
	case state.PhaseImplement:
		return o.handleImplementComplete(slotName, sess)
	case state.PhaseValidate:
		return o.handleValidateComplete(slotName, sess)
	default:
		return false
	}
}

// handleResearchComplete checks if the researcher produced a context file and advances to the next phase.
func (o *Orchestrator) handleResearchComplete(slotName string, sess *state.Session) bool {
	o.runAfterRunHook(sess)

	if !pipeline.ResearchArtifactsExist(sess.Worktree) {
		log.Printf("[pipeline] researcher %s did not produce research artifacts — advancing anyway", slotName)
	} else {
		log.Printf("[pipeline] researcher %s completed — research context available", slotName)
	}

	nextPhase := pipeline.NextPhase(o.cfg, state.PhaseResearch)
	sess.Phase = nextPhase

	issue, err := o.getIssue(sess.IssueNumber)
	if err != nil {
		log.Printf("[pipeline] fetch issue #%d for %s phase: %v — marking dead", sess.IssueNumber, nextPhase, err)
		sess.Status = state.StatusDead
		now := time.Now().UTC()
		sess.FinishedAt = &now
		return true
	}

	var promptContent string
	if nextPhase == state.PhaseImplement {
		promptContent = o.buildImplementerPrompt(sess, issue)
	} else {
		promptContent = pipeline.PromptForPhase(o.cfg, nextPhase, issue, sess.Worktree, sess.Branch)
	}
	backendName := pipeline.BackendForPhase(o.cfg, nextPhase)

	if err := o.startPhase(slotName, sess, promptContent, backendName); err != nil {
		log.Printf("[pipeline] start %s phase for %s: %v — marking dead", nextPhase, slotName, err)
		sess.Status = state.StatusDead
		now := time.Now().UTC()
		sess.FinishedAt = &now
		return true
	}

	arrow := "📋"
	if nextPhase == state.PhaseImplement {
		arrow = "🔨"
	}
	o.notifier.Sendf("🔍→%s maestro: %s (issue #%d) research complete, starting %s phase",
		arrow, slotName, sess.IssueNumber, nextPhase)
	return true
}

// handlePlanComplete checks if the planner produced artifacts and advances to implement phase.
func (o *Orchestrator) handlePlanComplete(slotName string, sess *state.Session) bool {
	o.runAfterRunHook(sess)

	if !pipeline.PlanArtifactsExist(sess.Worktree) {
		log.Printf("[pipeline] planner %s did not produce plan artifacts — marking as dead", slotName)
		sess.Status = state.StatusDead
		now := time.Now().UTC()
		sess.FinishedAt = &now
		o.notifier.Sendf("⚠️ maestro: planner %s (issue #%d) failed — no plan artifacts produced",
			slotName, sess.IssueNumber)
		return true
	}

	log.Printf("[pipeline] planner %s completed — advancing to implement phase", slotName)
	sess.Phase = state.PhaseImplement

	issue, err := o.getIssue(sess.IssueNumber)
	if err != nil {
		log.Printf("[pipeline] fetch issue #%d for implement phase: %v — marking dead", sess.IssueNumber, err)
		sess.Status = state.StatusDead
		now := time.Now().UTC()
		sess.FinishedAt = &now
		return true
	}

	promptContent := o.buildImplementerPrompt(sess, issue)
	backendName := pipeline.BackendForPhase(o.cfg, state.PhaseImplement)

	if err := o.startPhase(slotName, sess, promptContent, backendName); err != nil {
		log.Printf("[pipeline] start implement phase for %s: %v — marking dead", slotName, err)
		sess.Status = state.StatusDead
		now := time.Now().UTC()
		sess.FinishedAt = &now
		return true
	}

	o.notifier.Sendf("📋→🔨 maestro: %s (issue #%d) plan complete, starting implementer",
		slotName, sess.IssueNumber)
	return true
}

// handleImplementComplete advances to validate phase or proceeds to PR flow.
func (o *Orchestrator) handleImplementComplete(slotName string, sess *state.Session) bool {
	o.runAfterRunHook(sess)

	// When test_mapping is enabled, verify the script exists and is executable before proceeding
	if o.cfg.Pipeline.TestMapping && !pipeline.VerifyScriptReady(sess.Worktree) {
		log.Printf("[pipeline] implementer %s did not produce executable verify.sh — marking as failed", slotName)
		sess.Status = state.StatusFailed
		now := time.Now().UTC()
		sess.FinishedAt = &now
		o.notifier.Sendf("❌ maestro: %s (issue #%d) failed — missing or non-executable .maestro/verify.sh",
			slotName, sess.IssueNumber)
		return true
	}

	nextPhase := pipeline.NextPhase(o.cfg, state.PhaseImplement)
	if nextPhase == state.PhaseNone {
		// No validator — fall through to normal dead-worker handling (PR detection, retry, etc.)
		log.Printf("[pipeline] implementer %s done, no validator configured — returning to normal flow", slotName)
		sess.Phase = state.PhaseNone
		return false
	}

	log.Printf("[pipeline] implementer %s completed — advancing to validate phase", slotName)
	sess.Phase = state.PhaseValidate

	issue, err := o.getIssue(sess.IssueNumber)
	if err != nil {
		log.Printf("[pipeline] fetch issue #%d for validate phase: %v — marking dead", sess.IssueNumber, err)
		sess.Status = state.StatusDead
		now := time.Now().UTC()
		sess.FinishedAt = &now
		return true
	}

	promptContent := pipeline.PromptForPhase(o.cfg, state.PhaseValidate, issue, sess.Worktree, sess.Branch)
	backendName := pipeline.BackendForPhase(o.cfg, state.PhaseValidate)

	if err := o.startPhase(slotName, sess, promptContent, backendName); err != nil {
		log.Printf("[pipeline] start validate phase for %s: %v — marking dead", slotName, err)
		sess.Status = state.StatusDead
		now := time.Now().UTC()
		sess.FinishedAt = &now
		return true
	}

	o.notifier.Sendf("🔨→✅ maestro: %s (issue #%d) implementation complete, starting validator",
		slotName, sess.IssueNumber)
	return true
}

// handleValidateComplete checks validation result and either proceeds to PR flow or retries implementer.
func (o *Orchestrator) handleValidateComplete(slotName string, sess *state.Session) bool {
	o.runAfterRunHook(sess)

	passed, feedback, err := pipeline.ValidationPassed(sess.Worktree)
	if err != nil {
		log.Printf("[pipeline] read validation result for %s: %v — treating as failed", slotName, err)
		passed = false
		feedback = fmt.Sprintf("Could not read validation result: %v", err)
	}

	if passed {
		log.Printf("[pipeline] validator %s PASSED — returning to normal flow for PR detection", slotName)
		// Clear phase so the normal dead-worker handler can detect PRs
		sess.Phase = state.PhaseNone
		return false
	}

	// Validation failed — retry implementer with feedback
	sess.ValidationFails++
	sess.ValidationFeedback = feedback
	log.Printf("[pipeline] validator %s FAILED (attempt %d): %s", slotName, sess.ValidationFails, truncateFeedback(feedback))

	// After configured validation failures, give up
	maxRetries := pipeline.MaxValidationRetries(o.cfg)
	if sess.ValidationFails >= maxRetries {
		log.Printf("[pipeline] validator %s exhausted validation retries — marking as failed", slotName)
		sess.Status = state.StatusFailed
		now := time.Now().UTC()
		sess.FinishedAt = &now
		o.notifier.Sendf("❌ maestro: %s (issue #%d) failed validation %d times — giving up",
			slotName, sess.IssueNumber, sess.ValidationFails)
		return true
	}

	// Retry implementer with feedback
	sess.Phase = state.PhaseImplement
	issue, err := o.getIssue(sess.IssueNumber)
	if err != nil {
		log.Printf("[pipeline] fetch issue #%d for implement retry: %v — marking dead", sess.IssueNumber, err)
		sess.Status = state.StatusDead
		now := time.Now().UTC()
		sess.FinishedAt = &now
		return true
	}

	promptContent := o.buildImplementerPrompt(sess, issue)
	backendName := pipeline.BackendForPhase(o.cfg, state.PhaseImplement)

	if err := o.startPhase(slotName, sess, promptContent, backendName); err != nil {
		log.Printf("[pipeline] start implement retry for %s: %v — marking dead", slotName, err)
		sess.Status = state.StatusDead
		now := time.Now().UTC()
		sess.FinishedAt = &now
		return true
	}

	o.notifier.Sendf("✅→🔨 maestro: %s (issue #%d) validation failed, retrying implementer (attempt %d)",
		slotName, sess.IssueNumber, sess.ValidationFails+1)
	return true
}

// buildImplementerPrompt builds the implementer prompt with pipeline preamble.
func (o *Orchestrator) buildImplementerPrompt(sess *state.Session, issue github.Issue) string {
	preamble := pipeline.ImplementerPreamble(o.cfg, sess)
	base := o.selectPrompt(issue)
	return preamble + "\n" + base + fmt.Sprintf(`

---

## Your Current Task

**Issue #%d: %s**

**Repository:** %s
**Worktree path:** %s

### Issue Description
%s
`,
		issue.Number, issue.Title,
		o.cfg.Repo,
		sess.Worktree,
		issue.Body,
	)
}

// startPhase is a wrapper around worker.StartPhase with test hook support.
func (o *Orchestrator) startPhase(slotName string, sess *state.Session, prompt, backendName string) error {
	if o.workerStartPhaseFn != nil {
		return o.workerStartPhaseFn(o.cfg, sess, slotName, prompt, backendName)
	}
	return worker.StartPhase(o.cfg, sess, slotName, prompt, backendName)
}

func truncateFeedback(s string) string {
	if len(s) > 200 {
		return s[:197] + "..."
	}
	return s
}
