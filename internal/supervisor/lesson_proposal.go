package supervisor

import (
	"fmt"
	"strings"
	"time"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/state"
)

func recordLessonProposalForDecision(cfg *config.Config, st *state.State, decision state.SupervisorDecision) (*state.LessonProposal, bool) {
	proposal, ok := lessonProposalFromDecision(cfg, st, decision)
	if !ok {
		return nil, false
	}
	stored, _, created := st.RecordLessonProposal(proposal, decision.CreatedAt, decision.Repo, decision.Project)
	return stored, created
}

func lessonProposalFromDecision(cfg *config.Config, st *state.State, decision state.SupervisorDecision) (state.LessonProposal, bool) {
	if cfg == nil || st == nil {
		return state.LessonProposal{}, false
	}
	if !cfg.Supervisor.LessonProposalsOn() {
		return state.LessonProposal{}, false
	}
	for _, stuck := range decision.StuckStates {
		if !lessonProposalEligibleForStuckState(st, stuck) {
			continue
		}
		if proposal, ok := lessonProposalFromStuckState(cfg, decision, stuck); ok {
			return proposal, true
		}
	}
	for slot, sess := range st.Sessions {
		if sess == nil {
			continue
		}
		if sess.Status == state.StatusConflictFailed {
			target := &state.SupervisorTarget{Issue: sess.IssueNumber, PR: sess.PRNumber, Session: slot}
			stuck := state.SupervisorStuckState{
				Code:              "conflict_unresolvable",
				Severity:          SeverityBlocked,
				Summary:           fmt.Sprintf("Issue #%d reached conflict_failed in session %s.", sess.IssueNumber, slot),
				RecommendedAction: "Resolve the rebase or merge conflict manually before retrying.",
				Target:            target,
				Evidence:          []string{fmt.Sprintf("Session %s status=conflict_failed", slot)},
			}
			if !lessonProposalEligibleForStuckState(st, stuck) {
				continue
			}
			return lessonProposalFromStuckState(cfg, decision, stuck)
		}
	}
	return state.LessonProposal{}, false
}

func lessonProposalFromStuckState(cfg *config.Config, decision state.SupervisorDecision, stuck state.SupervisorStuckState) (state.LessonProposal, bool) {
	failureClass := lessonFailureClass(stuck.Code)
	if failureClass == "" {
		return state.LessonProposal{}, false
	}
	area := lessonArea(stuck.Target, decision.Target)
	minimalRepro := lessonMinimalRepro(stuck)
	target := state.LessonProposalTargetAgentsMD
	if strings.TrimSpace(cfg.WorkerPrompt) != "" {
		target = state.LessonProposalTargetWorkerPrompt
	}
	proposal := state.LessonProposal{
		CreatedAt:      decision.CreatedAt,
		FailureClass:   failureClass,
		Area:           area,
		MinimalRepro:   minimalRepro,
		SuggestedRule:  lessonSuggestedRule(failureClass, minimalRepro),
		Target:         target,
		Status:         state.LessonProposalStatusPending,
		SourceDecision: decision.ID,
		SourceTarget:   cloneLessonTarget(firstTarget(stuck.Target, decision.Target)),
	}
	if proposal.CreatedAt.IsZero() {
		proposal.CreatedAt = time.Now().UTC()
	}
	return proposal, true
}

func lessonProposalEligibleForStuckState(st *state.State, stuck state.SupervisorStuckState) bool {
	if lessonFailureClass(stuck.Code) != "retry_exhausted" {
		return true
	}
	issue := 0
	if stuck.Target != nil {
		issue = stuck.Target.Issue
	}
	if issue <= 0 {
		return true
	}
	return retryExhaustedIssueHasTaskRunEvidence(st, issue)
}

func retryExhaustedIssueHasTaskRunEvidence(st *state.State, issue int) bool {
	if st == nil || issue <= 0 {
		return true
	}
	seenFailedAttempt := false
	for _, sess := range st.Sessions {
		if sess == nil || sess.IssueNumber != issue || sess.RateLimitHit {
			continue
		}
		switch sess.Status {
		case state.StatusDead, state.StatusFailed, state.StatusRetryExhausted, state.StatusConflictFailed:
			seenFailedAttempt = true
		default:
			continue
		}
		if sessionHasTaskRunEvidence(sess) {
			return true
		}
	}
	return !seenFailedAttempt
}

func sessionHasTaskRunEvidence(sess *state.Session) bool {
	if sess == nil {
		return false
	}
	if sess.PRNumber > 0 || sess.TokensUsedAttempt > 0 || sess.TokensUsedTotal > 0 {
		return true
	}
	if !sess.LastOutputChangedAt.IsZero() {
		return true
	}
	if strings.TrimSpace(sess.CIFailureOutput) != "" || strings.TrimSpace(sess.PreviousAttemptFeedback) != "" {
		return true
	}
	return false
}

func lessonFailureClass(code string) string {
	switch strings.TrimSpace(code) {
	case "retry_exhausted", "retry_exhausted_open_pr":
		return "retry_exhausted"
	case state.StuckReviewRepairExhausted:
		return "review_repair_exhausted"
	case "conflict_unresolvable", "unmergeable_pr":
		return "conflict_unresolvable"
	case state.StuckSearchGuardrailTrip:
		return "search_guardrail_trip"
	default:
		return ""
	}
}

func lessonArea(targets ...*state.SupervisorTarget) string {
	target := firstTarget(targets...)
	if target == nil {
		return "project"
	}
	switch {
	case target.Issue > 0:
		return fmt.Sprintf("issue:%d", target.Issue)
	case target.PR > 0:
		return fmt.Sprintf("pr:%d", target.PR)
	case strings.TrimSpace(target.Session) != "":
		return "session:" + strings.TrimSpace(target.Session)
	default:
		return "project"
	}
}

func lessonMinimalRepro(stuck state.SupervisorStuckState) string {
	parts := []string{strings.TrimSpace(stuck.Summary)}
	for _, evidence := range stuck.Evidence {
		if evidence = strings.TrimSpace(evidence); evidence != "" {
			parts = append(parts, evidence)
		}
	}
	if action := strings.TrimSpace(stuck.RecommendedAction); action != "" {
		parts = append(parts, "recommended_action="+action)
	}
	return strings.Join(parts, " | ")
}

func lessonSuggestedRule(failureClass, minimalRepro string) string {
	switch failureClass {
	case "retry_exhausted":
		return "When a previous Maestro attempt is marked retry_exhausted, inspect the saved failure context before changing code, identify the smallest failing step that exhausted retries, and change the implementation or validation plan to avoid repeating that step before opening or updating a PR."
	case "review_repair_exhausted":
		return "When review repair is exhausted, stop making broad edits and address only the unresolved high-severity review findings on the current PR head; summarize each finding and the exact file-level fix before pushing another update."
	case "conflict_unresolvable":
		return "When a branch has unresolved rebase or merge conflicts, resolve the conflict first with the smallest file-scoped change, run the affected tests, and do not continue feature work until the branch is clean against the base branch."
	case "search_guardrail_trip":
		return "When searching the repository, run rg, grep, or find from the assigned worktree and scope path arguments to that worktree; do not search broad filesystem roots unless the operator explicitly grants a one-command broad-search override."
	default:
		return "Before retrying a recurring Maestro failure, inspect the recorded stuck state, identify the repeated mistake, and add a narrow file- or workflow-level guard so the next attempt avoids the same failure mode."
	}
}

func firstTarget(targets ...*state.SupervisorTarget) *state.SupervisorTarget {
	for _, target := range targets {
		if target != nil {
			return target
		}
	}
	return nil
}

func cloneLessonTarget(target *state.SupervisorTarget) *state.SupervisorTarget {
	if target == nil {
		return nil
	}
	clone := *target
	clone.Issues = append([]state.SupervisorIssueTarget(nil), target.Issues...)
	return &clone
}
