package orchestrator

import (
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/github"
	"github.com/befeast/maestro/internal/pipeline"
	"github.com/befeast/maestro/internal/state"
	"github.com/befeast/maestro/internal/worker"
)

// advancePipeline handles phase transitions for pipeline-enabled sessions.
// Called when a running pipeline worker's process dies.
// Returns true if the session was handled by pipeline logic (caller should skip normal dead-worker handling).
func (o *Orchestrator) advancePipeline(st *state.State, slotName string, sess *state.Session) bool {
	if sess.Phase == state.PhaseNone {
		return false // not a pipeline session
	}

	switch sess.Phase {
	case state.PhasePlan:
		return o.handlePlanComplete(st, slotName, sess)
	case state.PhaseAdvisor:
		return o.handleAdvisorComplete(slotName, sess)
	case state.PhaseImplement:
		return o.handleImplementComplete(slotName, sess)
	case state.PhaseValidate:
		return o.handleValidateComplete(slotName, sess)
	default:
		return false
	}
}

// handlePlanComplete checks if the planner produced artifacts and advances to
// Advisor when configured, otherwise preserving the historical direct handoff
// to implementation.
func (o *Orchestrator) handlePlanComplete(st *state.State, slotName string, sess *state.Session) bool {
	cfg := o.pipelineConfigForSession(sess)
	o.runAfterRunHook(sess)

	if !pipeline.PlanArtifactsExist(sess.Worktree) {
		log.Printf("[pipeline] planner %s did not produce plan artifacts — marking as dead", slotName)
		sess.Status = state.StatusDead
		now := time.Now().UTC()
		sess.FinishedAt = &now
		state.MarkWorkerEnded(sess, now)
		o.notifier.Sendf("⚠️ maestro: planner %s (issue #%d) failed — no plan artifacts produced",
			slotName, sess.IssueNumber)
		return true
	}

	sess.PlanVersion++
	if pipeline.NextPhase(cfg, state.PhasePlan) == state.PhaseAdvisor {
		log.Printf("[pipeline] planner %s completed plan v%d — advancing to Advisor", slotName, sess.PlanVersion)
		return o.startAdvisorPhase(st, cfg, slotName, sess)
	}

	log.Printf("[pipeline] planner %s completed — advancing to implement phase", slotName)
	return o.startImplementPhase(cfg, slotName, sess, "📋→🔨 maestro: %s (issue #%d) plan complete, starting implementer")
}

// startAdvisorPhase snapshots the worktree invariants and launches one bounded
// independent review. No Planner/Advisor overlap is possible: this runs only
// after the dead Planner process was observed and under the existing session
// lease used by every phase transition.
func (o *Orchestrator) startAdvisorPhase(st *state.State, cfg *config.Config, slotName string, sess *state.Session) bool {
	if sess.AdvisorMaxReviewRounds == 0 {
		sess.AdvisorMaxReviewRounds = cfg.Pipeline.EffectiveAdvisorReviewRounds()
	}
	sess.AdvisorBestEffort = cfg.Pipeline.AdvisorBestEffort
	if sess.AdvisorReviewRound >= sess.AdvisorMaxReviewRounds {
		return o.finishAdvisorGate(cfg, slotName, sess, pipeline.AdvisorVerdictRevise, "review_rounds_exhausted", sess.AdvisorUnresolvedFindings)
	}
	sess.Phase = state.PhaseAdvisor
	sess.AdvisorReviewRound++
	backendName := pipeline.BackendForPhase(cfg, state.PhaseAdvisor)
	sess.AdvisorBackend = backendName
	sess.AdvisorModel = strings.TrimSpace(cfg.Model.Backends[backendName].Model)
	sess.AdvisorVerdict = ""
	sess.AdvisorTerminalReason = ""
	sess.AdvisorBypassed = false
	if err := pipeline.RemoveAdvisorReview(sess.Worktree); err != nil {
		return o.finishAdvisorGate(cfg, slotName, sess, pipeline.AdvisorVerdictInvalid, "stale_review_cleanup_failed", err.Error())
	}

	if blockedBy, retryAt := o.dispatchBackendBlock(st, backendName, "", time.Now().UTC()); blockedBy != "" {
		finding := fmt.Sprintf("Required Advisor backend %s is unavailable (%s).", backendName, blockedBy)
		if retryAt != nil {
			finding = fmt.Sprintf("%s Retry after %s.", finding, retryAt.UTC().Format(time.RFC3339))
		}
		return o.finishAdvisorGate(cfg, slotName, sess, pipeline.AdvisorVerdictInvalid, "backend_unavailable", finding)
	}
	if def, ok := cfg.Model.Backends[backendName]; ok && def.NonAgentic {
		return o.finishAdvisorGate(cfg, slotName, sess, pipeline.AdvisorVerdictInvalid, "backend_unavailable", fmt.Sprintf("Required Advisor backend %s is non-agentic and cannot review a worktree.", backendName))
	}

	issue, err := o.getIssue(sess.IssueNumber)
	if err != nil {
		return o.finishAdvisorGate(cfg, slotName, sess, pipeline.AdvisorVerdictInvalid, "issue_fetch_failed", err.Error())
	}
	baseline, err := pipeline.CaptureAdvisorWorkspace(sess.Worktree)
	if err != nil {
		return o.finishAdvisorGate(cfg, slotName, sess, pipeline.AdvisorVerdictInvalid, "workspace_snapshot_failed", err.Error())
	}

	sess.AdvisorBaselineHead = baseline.Head
	sess.AdvisorBaselineWorktree = baseline.Worktree
	sess.AdvisorBaselineRemoteRefs = baseline.RemoteRefs
	sess.AdvisorBaselinePlanDigest = baseline.PlanDigest
	sess.AdvisorBaselineValidationDigest = baseline.ValidationDigest

	promptContent, err := pipeline.AdvisorPrompt(cfg, issue, sess.Worktree, sess.Branch, sess)
	if err != nil {
		return o.finishAdvisorGate(cfg, slotName, sess, pipeline.AdvisorVerdictInvalid, "prompt_build_failed", err.Error())
	}
	if err := o.startPhase(cfg, slotName, sess, promptContent, backendName); err != nil {
		return o.finishAdvisorGate(cfg, slotName, sess, pipeline.AdvisorVerdictInvalid, "advisor_start_failed", err.Error())
	}

	log.Printf("[pipeline] Advisor %s started plan v%d review round %d/%d (%s)", slotName, sess.PlanVersion, sess.AdvisorReviewRound, sess.AdvisorMaxReviewRounds, advisorRoute(sess))
	o.notifier.Sendf("📋→🧭 maestro: %s (issue #%d) starting Advisor review %d/%d for plan v%d (%s)",
		slotName, sess.IssueNumber, sess.AdvisorReviewRound, sess.AdvisorMaxReviewRounds, sess.PlanVersion, advisorRoute(sess))
	return true
}

// handleAdvisorComplete accepts only an explicit strict verdict, proves the
// Advisor respected its review-only boundary, and either starts implementation
// or returns accumulated findings to the Planner.
func (o *Orchestrator) handleAdvisorComplete(slotName string, sess *state.Session) bool {
	cfg := o.pipelineConfigForSession(sess)
	if sess.AdvisorMaxReviewRounds == 0 {
		sess.AdvisorMaxReviewRounds = cfg.Pipeline.EffectiveAdvisorReviewRounds()
	}
	o.runAfterRunHook(sess)
	o.captureAdvisorModel(sess)

	baseline := pipeline.AdvisorWorkspaceSnapshot{
		Head:             sess.AdvisorBaselineHead,
		Worktree:         sess.AdvisorBaselineWorktree,
		RemoteRefs:       sess.AdvisorBaselineRemoteRefs,
		PlanDigest:       sess.AdvisorBaselinePlanDigest,
		ValidationDigest: sess.AdvisorBaselineValidationDigest,
	}
	if reason, err := pipeline.ValidateAdvisorWorkspace(sess.Worktree, baseline); err != nil {
		return o.finishAdvisorGate(cfg, slotName, sess, pipeline.AdvisorVerdictInvalid, reason, err.Error())
	}

	result, err := pipeline.ReadAdvisorResult(sess.Worktree)
	if err != nil {
		findings := result.Findings
		if findings == "" {
			findings = err.Error()
		}
		reason := "invalid_verdict"
		if strings.Contains(err.Error(), "read advisor review") {
			reason = "missing_review_artifact"
		}
		return o.finishAdvisorGate(cfg, slotName, sess, pipeline.AdvisorVerdictInvalid, reason, findings)
	}

	switch result.Verdict {
	case pipeline.AdvisorVerdictApproved:
		sess.AdvisorVerdict = result.Verdict
		sess.AdvisorUnresolvedFindings = ""
		sess.AdvisorTerminalReason = ""
		sess.AdvisorBypassed = false
		o.recordAdvisorReview(sess, result.Verdict, result.Findings, "", false)
		log.Printf("[pipeline] Advisor %s verdict=%s for plan v%d review round %d (%s)", slotName, result.Verdict, sess.PlanVersion, sess.AdvisorReviewRound, advisorRoute(sess))
		o.notifier.Sendf("🧭→🔨 maestro: %s (issue #%d) Advisor verdict=%s for plan v%d round %d (%s); starting implementer",
			slotName, sess.IssueNumber, result.Verdict, sess.PlanVersion, sess.AdvisorReviewRound, advisorRoute(sess))
		return o.startImplementPhase(cfg, slotName, sess, "")

	case pipeline.AdvisorVerdictRevise:
		sess.AdvisorVerdict = result.Verdict
		sess.AdvisorUnresolvedFindings = result.Findings
		sess.AdvisorFindingsLedger = pipeline.AppendAdvisorFindings(sess.AdvisorFindingsLedger, sess.PlanVersion, sess.AdvisorReviewRound, result.Findings)
		if sess.AdvisorReviewRound >= sess.AdvisorMaxReviewRounds {
			return o.finishAdvisorGate(cfg, slotName, sess, result.Verdict, "review_rounds_exhausted", result.Findings)
		}
		o.recordAdvisorReview(sess, result.Verdict, result.Findings, "", false)
		if err := pipeline.RemoveAdvisorReview(sess.Worktree); err != nil {
			return o.finishAdvisorGate(cfg, slotName, sess, pipeline.AdvisorVerdictInvalid, "stale_review_cleanup_failed", err.Error())
		}
		issue, fetchErr := o.getIssue(sess.IssueNumber)
		if fetchErr != nil {
			return o.finishAdvisorGate(cfg, slotName, sess, pipeline.AdvisorVerdictInvalid, "issue_fetch_failed", fetchErr.Error())
		}
		sess.Phase = state.PhasePlan
		promptContent := pipeline.PlannerRevisionPrompt(cfg, issue, sess.Worktree, sess.Branch, sess)
		backendName := pipeline.BackendForPhase(cfg, state.PhasePlan)
		if startErr := o.startPhase(cfg, slotName, sess, promptContent, backendName); startErr != nil {
			return o.finishAdvisorGate(cfg, slotName, sess, pipeline.AdvisorVerdictInvalid, "planner_revision_start_failed", startErr.Error())
		}
		log.Printf("[pipeline] Advisor %s verdict=%s for plan v%d round %d/%d (%s); returning exact findings to Planner: %s", slotName, result.Verdict, sess.PlanVersion, sess.AdvisorReviewRound, sess.AdvisorMaxReviewRounds, advisorRoute(sess), result.Findings)
		o.notifier.Sendf("🧭→📋 maestro: %s (issue #%d) Advisor verdict=%s for plan v%d round %d/%d (%s)\n%s",
			slotName, sess.IssueNumber, result.Verdict, sess.PlanVersion, sess.AdvisorReviewRound, sess.AdvisorMaxReviewRounds, advisorRoute(sess), result.Findings)
		return true
	}

	return o.finishAdvisorGate(cfg, slotName, sess, pipeline.AdvisorVerdictInvalid, "invalid_verdict", result.Raw)
}

func (o *Orchestrator) startImplementPhase(cfg *config.Config, slotName string, sess *state.Session, notificationFormat string) bool {
	sess.Phase = state.PhaseImplement
	// A best-effort bypass after an Advisor backend outage starts a fresh role;
	// keep the outage in Advisor history while clearing current-attempt backend
	// failure flags that would otherwise misclassify the Implementer.
	sess.RateLimitHit = false
	sess.ProviderLimitBackend = ""
	sess.ProviderLimitReason = ""
	sess.ProviderLimitResetAt = nil
	sess.ProviderLimitProvider = ""
	sess.ProviderLimitModel = ""
	issue, err := o.getIssue(sess.IssueNumber)
	if err != nil {
		log.Printf("[pipeline] fetch issue #%d for implement phase: %v — marking dead", sess.IssueNumber, err)
		o.markPipelineDead(sess)
		return true
	}
	promptContent := o.buildImplementerPrompt(sess, issue)
	backendName := pipeline.BackendForPhase(cfg, state.PhaseImplement)
	if err := o.startPhase(cfg, slotName, sess, promptContent, backendName); err != nil {
		log.Printf("[pipeline] start implement phase for %s: %v — marking dead", slotName, err)
		o.markPipelineDead(sess)
		return true
	}
	if notificationFormat != "" {
		o.notifier.Sendf(notificationFormat, slotName, sess.IssueNumber)
	}
	return true
}

// finishAdvisorGate is the single terminal path. Fail-closed is the default;
// the separately configured best-effort mode records a visible bypass before
// implementation starts.
func (o *Orchestrator) finishAdvisorGate(cfg *config.Config, slotName string, sess *state.Session, verdict, reason, findings string) bool {
	o.captureAdvisorModel(sess)
	findings = strings.TrimSpace(findings)
	if findings == "" {
		findings = fmt.Sprintf("Advisor gate ended without an explicit approval (%s).", reason)
	}
	sess.AdvisorVerdict = verdict
	sess.AdvisorTerminalReason = reason
	sess.AdvisorUnresolvedFindings = findings

	if sess.AdvisorBestEffort && advisorFailureBypassable(reason) {
		sess.AdvisorBypassed = true
		o.recordAdvisorReview(sess, verdict, findings, reason, true)
		sess.AdvisorVerdict = pipeline.AdvisorVerdictBypassed
		log.Printf("[pipeline] Advisor gate %s verdict=%s bypassed by explicit best-effort config for plan v%d round %d (%s): %s — %s", slotName, verdict, sess.PlanVersion, sess.AdvisorReviewRound, advisorRoute(sess), reason, findings)
		o.notifier.Sendf("⚠️ maestro: %s (issue #%d) explicitly bypassed Advisor verdict=%s for plan v%d round %d (%s); terminal reason=%s\n%s",
			slotName, sess.IssueNumber, verdict, sess.PlanVersion, sess.AdvisorReviewRound, advisorRoute(sess), reason, findings)
		return o.startImplementPhase(cfg, slotName, sess, "")
	}

	sess.AdvisorBypassed = false
	o.recordAdvisorReview(sess, verdict, findings, reason, false)
	sess.Phase = state.PhaseAdvisor
	sess.Status = state.StatusFailed
	now := time.Now().UTC()
	sess.FinishedAt = &now
	state.MarkWorkerEnded(sess, now)
	o.syncProject(sess.IssueNumber, github.ProjectStatusBlocked)
	log.Printf("[pipeline] Advisor gate %s verdict=%s failed closed before implementation for plan v%d round %d (%s): %s — %s", slotName, verdict, sess.PlanVersion, sess.AdvisorReviewRound, advisorRoute(sess), reason, findings)
	o.notifier.Sendf("❌ maestro: %s (issue #%d) Advisor verdict=%s failed closed before implementation for plan v%d round %d (%s); terminal reason=%s\n%s",
		slotName, sess.IssueNumber, verdict, sess.PlanVersion, sess.AdvisorReviewRound, advisorRoute(sess), reason, findings)
	return true
}

func advisorFailureBypassable(reason string) bool {
	switch reason {
	case "canonical_artifact_mutated", "advisor_commit_detected", "advisor_push_detected", "advisor_worktree_mutation", "advisor_pr_created", "workspace_check_failed":
		return false
	default:
		return true
	}
}

func advisorRoute(sess *state.Session) string {
	if sess == nil {
		return "backend=unknown model=unknown"
	}
	backend := strings.TrimSpace(sess.AdvisorBackend)
	if backend == "" {
		backend = "unknown"
	}
	model := strings.TrimSpace(sess.AdvisorModel)
	if model == "" {
		model = "unknown"
	}
	return fmt.Sprintf("backend=%s model=%s", backend, model)
}

func (o *Orchestrator) recordAdvisorReview(sess *state.Session, verdict, findings, terminalReason string, bypassed bool) {
	if sess == nil {
		return
	}
	sess.AdvisorReviews = append(sess.AdvisorReviews, state.AdvisorReview{
		PlanVersion:    sess.PlanVersion,
		ReviewRound:    sess.AdvisorReviewRound,
		Backend:        sess.AdvisorBackend,
		Model:          sess.AdvisorModel,
		Verdict:        verdict,
		Findings:       strings.TrimSpace(findings),
		TerminalReason: terminalReason,
		Bypassed:       bypassed,
		ReviewedAt:     time.Now().UTC(),
	})
}

func (o *Orchestrator) captureAdvisorModel(sess *state.Session) {
	if sess == nil || sess.Phase != state.PhaseAdvisor {
		return
	}
	if model := strings.TrimSpace(sess.Model); model != "" {
		sess.AdvisorModel = model
		return
	}
	if len(sess.Attribution) > 0 {
		if model := strings.TrimSpace(sess.Attribution[len(sess.Attribution)-1].Model); model != "" {
			sess.AdvisorModel = model
		}
	}
}

// handleAdvisorBackendDeath classifies required-backend failures before the
// generic pipeline completion path can mislabel a missing review artifact as a
// normal verdict error. Advisor backend failures do not fall over silently: the
// configured independent reviewer is a required gate unless best-effort was
// explicitly enabled.
func (o *Orchestrator) handleAdvisorBackendDeath(st *state.State, slotName string, sess *state.Session) bool {
	now := time.Now().UTC()
	if hit, resetAt := o.providerRateLimitFromLog(sess.LogFile); hit {
		o.runAfterRunHook(sess)
		o.updateTokensUsedFromWorkerLog(slotName, sess)
		o.recordProviderLimit(st, slotName, sess, "advisor_provider_limit", resetAt, now)
		finding := fmt.Sprintf("Required Advisor backend %s hit a provider limit.", sess.Backend)
		if resetAt != nil {
			finding = fmt.Sprintf("%s Retry after %s.", finding, resetAt.UTC().Format(time.RFC3339))
		}
		return o.finishAdvisorGate(o.pipelineConfigForSession(sess), slotName, sess, pipeline.AdvisorVerdictInvalid, "backend_unavailable", finding)
	}
	if failure, hit := o.classifyBackendFailure(sess, now); hit {
		o.runAfterRunHook(sess)
		o.updateTokensUsedFromWorkerLog(slotName, sess)
		o.recordBackendFailure(st, slotName, sess, failure, now)
		copy := backendFailureCopyFor(failure.reason)
		finding := fmt.Sprintf("Required Advisor backend %s %s (%s).", sess.Backend, copy.desc, failure.pattern)
		return o.finishAdvisorGate(o.pipelineConfigForSession(sess), slotName, sess, pipeline.AdvisorVerdictInvalid, "backend_unavailable", finding)
	}
	return false
}

func (o *Orchestrator) markPipelineDead(sess *state.Session) {
	sess.Status = state.StatusDead
	now := time.Now().UTC()
	sess.FinishedAt = &now
	state.MarkWorkerEnded(sess, now)
}

// handleImplementComplete advances to validate phase or proceeds to PR flow.
func (o *Orchestrator) handleImplementComplete(slotName string, sess *state.Session) bool {
	cfg := o.pipelineConfigForSession(sess)
	o.runAfterRunHook(sess)

	nextPhase := pipeline.NextPhase(cfg, state.PhaseImplement)
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
		state.MarkWorkerEnded(sess, now)
		return true
	}

	promptContent := pipeline.PromptForPhase(cfg, state.PhaseValidate, issue, sess.Worktree, sess.Branch)
	backendName := pipeline.BackendForPhase(cfg, state.PhaseValidate)

	if err := o.startPhase(cfg, slotName, sess, promptContent, backendName); err != nil {
		log.Printf("[pipeline] start validate phase for %s: %v — marking dead", slotName, err)
		sess.Status = state.StatusDead
		now := time.Now().UTC()
		sess.FinishedAt = &now
		state.MarkWorkerEnded(sess, now)
		return true
	}

	o.notifier.Sendf("🔨→✅ maestro: %s (issue #%d) implementation complete, starting validator",
		slotName, sess.IssueNumber)
	return true
}

// handleValidateComplete checks validation result and either proceeds to PR flow or retries implementer.
func (o *Orchestrator) handleValidateComplete(slotName string, sess *state.Session) bool {
	cfg := o.pipelineConfigForSession(sess)
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

	// After 3 validation failures, give up
	if sess.ValidationFails >= 3 {
		log.Printf("[pipeline] validator %s exhausted validation retries — marking as failed", slotName)
		sess.Status = state.StatusFailed
		now := time.Now().UTC()
		sess.FinishedAt = &now
		state.MarkWorkerEnded(sess, now)
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
		state.MarkWorkerEnded(sess, now)
		return true
	}

	promptContent := o.buildImplementerPrompt(sess, issue)
	backendName := pipeline.BackendForPhase(cfg, state.PhaseImplement)

	if err := o.startPhase(cfg, slotName, sess, promptContent, backendName); err != nil {
		log.Printf("[pipeline] start implement retry for %s: %v — marking dead", slotName, err)
		sess.Status = state.StatusDead
		now := time.Now().UTC()
		sess.FinishedAt = &now
		state.MarkWorkerEnded(sess, now)
		return true
	}

	o.notifier.Sendf("✅→🔨 maestro: %s (issue #%d) validation failed, retrying implementer (attempt %d)",
		slotName, sess.IssueNumber, sess.ValidationFails+1)
	return true
}

func (o *Orchestrator) pipelineConfigForSession(sess *state.Session) *config.Config {
	if o.cfg == nil || sess == nil || (!sess.PipelineFull && !sess.PipelineAdvised) {
		return o.cfg
	}
	cfg := *o.cfg
	cfg.Pipeline.Enabled = true
	if sess.PipelineFull || sess.PipelineAdvised {
		cfg.Pipeline.Planner.Enabled = true
		cfg.Pipeline.Validator.Enabled = true
	}
	if sess.PipelineAdvised {
		cfg.Pipeline.Advisor.Enabled = true
	}
	return &cfg
}

// buildImplementerPrompt builds the implementer prompt with pipeline preamble.
func (o *Orchestrator) buildImplementerPrompt(sess *state.Session, issue github.Issue) string {
	preamble := pipeline.ImplementerPreamble(sess)
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
func (o *Orchestrator) startPhase(cfg *config.Config, slotName string, sess *state.Session, prompt, backendName string) error {
	if cfg == nil {
		cfg = o.cfg
	}
	return state.WithSessionLease(cfg.StateDir, slotName, func() error {
		if o.workerStartPhaseFn != nil {
			return o.workerStartPhaseFn(cfg, sess, slotName, prompt, backendName)
		}
		return worker.StartPhase(cfg, sess, slotName, prompt, backendName)
	})
}

func truncateFeedback(s string) string {
	if len(s) > 200 {
		return s[:197] + "..."
	}
	return s
}
