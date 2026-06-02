package supervisor

import (
	"fmt"
	"strings"
	"time"

	"github.com/befeast/maestro/internal/github"
	"github.com/befeast/maestro/internal/state"
)

// PolicyRuleReviewRepair identifies the supervisor policy rule that
// generated an auto-review-repair recommendation in audit/state.
const PolicyRuleReviewRepair = "supervisor.review_repair"

// reviewRepairCandidate is the result of inspecting one (session, PR)
// pair for the #565 auto review-repair pipeline.
type reviewRepairCandidate struct {
	HeadSHA  string
	Findings []github.ReviewComment
	// Exhausted reports whether the (pr,head_sha) repair budget already
	// ran out. When true the supervisor falls through to a visible
	// operator decision (merge_pr approval or attention stuck state) —
	// never a silent dead-end.
	Exhausted bool
	// Track is the current ReviewRepairTrack record (zero-valued when
	// no track exists yet). Useful for the stuck-state evidence.
	Track state.ReviewRepairTrack
}

// evaluateAutoReviewRepair inspects an open PR + its bound session and
// returns a candidate when the auto-review-repair branch should fire.
// Returns (nil, "") when the branch does not apply.
//
// The branch fires only when ALL the following hold (matches the #565
// acceptance criteria):
//
//   - SupervisorReviewRepairConfig.Active() (default on).
//   - The session is settled retry_exhausted on review feedback.
//   - The PR is not draft, mergeable, CI green, AND the Greptile gate
//     is "not approved" only because of inline P0/P1 findings on head.
//   - The Greptile reader exposes PRHighSeverityReviewOnHead and reports
//     at least one P0/P1 inline finding on the current head SHA.
//
// When the budget for the (pr,head_sha) pair is exhausted the candidate
// is returned with Exhausted=true so the caller can choose a fall-
// through (merge_pr approval, attention stuck state).
func (e *Engine) evaluateAutoReviewRepair(st *state.State, sess *state.Session, pr github.PR) *reviewRepairCandidate {
	if e == nil || e.cfg == nil || st == nil || sess == nil {
		return nil
	}
	if !e.cfg.Supervisor.ReviewRepair.Active() {
		return nil
	}
	if pr.Number <= 0 {
		return nil
	}
	if pr.IsDraft {
		return nil
	}
	if !reviewRepairSessionSettled(sess, pr.Number) {
		return nil
	}

	mergeable := strings.ToUpper(strings.TrimSpace(pr.Mergeable))
	if mr, ok := e.reader.(prMergeableReader); ok {
		if fresh, err := mr.PRMergeable(pr.Number); err == nil {
			mergeable = strings.ToUpper(strings.TrimSpace(fresh))
		}
	}
	if mergeable != "MERGEABLE" {
		return nil
	}

	if ciReader, ok := e.reader.(prCIStatusReader); ok {
		if ciStatus, err := ciReader.PRCIStatus(pr.Number); err == nil {
			ciLower := strings.ToLower(strings.TrimSpace(ciStatus))
			if ciLower != "success" {
				if !mergeStateAllowsMerge(e.reader, pr.Number) {
					return nil
				}
			}
		} else {
			return nil
		}
	} else {
		return nil
	}

	reader, ok := e.reader.(prHighSeverityReviewReader)
	if !ok {
		return nil
	}
	sha, findings, hasFindings, err := reader.PRHighSeverityReviewOnHead(pr.Number)
	if err != nil {
		return nil
	}
	if !hasFindings {
		return nil
	}

	track, _ := st.LookupReviewRepairTrack(pr.Number, sha)
	maxRetries := e.cfg.Supervisor.ReviewRepair.EffectiveMaxRetries()
	candidate := &reviewRepairCandidate{
		HeadSHA:  sha,
		Findings: findings,
		Track:    track,
	}
	if maxRetries <= 0 {
		candidate.Exhausted = true
		return candidate
	}
	if track.Exhausted || track.Attempts >= maxRetries {
		candidate.Exhausted = true
		return candidate
	}
	return candidate
}

// reviewRepairSessionSettled reports whether the session has reached the
// "settled retry_exhausted on review feedback" state — i.e. the worker
// has spent its retry budget on review-feedback fixes and the orchestrator
// has stopped scheduling fresh retries. Mirrors the orchestrator's
// isSettledRetryExhausted contract so the two surfaces agree on the same
// terminal condition.
func reviewRepairSessionSettled(sess *state.Session, prNumber int) bool {
	if sess == nil {
		return false
	}
	if sess.Status != state.StatusRetryExhausted {
		return false
	}
	if prNumber <= 0 || sess.PRNumber != prNumber {
		return false
	}
	// The orchestrator stamps LastNotifiedStatus = "review_retry_exhausted"
	// when the review-feedback retry budget ran out (#556). That's the
	// signal we use to scope the auto-review-repair branch — CI
	// retry-exhausted or rebase-conflict retry-exhausted are different
	// failure modes and are not in scope here.
	if sess.LastNotifiedStatus == "review_retry_exhausted" {
		return true
	}
	// Back-compat: an older session may carry the review-feedback
	// PreviousAttemptFeedbackKind without the LastNotifiedStatus stamp.
	return sess.PreviousAttemptFeedbackKind == state.RetryReasonReviewFeedback
}

// buildReviewRepairDecision constructs the spawn_review_repair decision
// for a candidate. The decision is RiskMutating (approval-gated under
// cautious mode) and carries the per-finding evidence so the dispatcher
// can scope the worker prompt to exactly the inline comments returned by
// the Greptile reader (#565: "scoped to the specific Greptile comments,
// not the whole issue restatement").
func (e *Engine) buildReviewRepairDecision(st *state.State, now time.Time, projectState state.SupervisorProjectState, baseReasons []string, slot string, sess *state.Session, pr github.PR, candidate *reviewRepairCandidate) state.SupervisorDecision {
	target := &state.SupervisorTarget{
		Issue:   sess.IssueNumber,
		PR:      pr.Number,
		HeadSHA: candidate.HeadSHA,
		Session: slot,
	}
	reasons := appendReasons(baseReasons,
		fmt.Sprintf("PR #%d is green and mergeable but Greptile flags %d P0/P1 inline comment(s) on head %s", pr.Number, len(candidate.Findings), shortSHA(candidate.HeadSHA)),
		fmt.Sprintf("Session %s is settled retry_exhausted on review feedback (#565: would otherwise dead-end)", slot),
		fmt.Sprintf("Review-repair budget: attempt %d of %d for (PR #%d, head %s)", candidate.Track.Attempts+1, e.cfg.Supervisor.ReviewRepair.EffectiveMaxRetries(), pr.Number, shortSHA(candidate.HeadSHA)),
	)
	reasons = appendReasons(reasons, formatReviewRepairFindings(candidate.Findings)...)
	summary := fmt.Sprintf("Spawn a scoped review-repair worker for PR #%d to address %d Greptile P0/P1 inline comment(s) on head %s", pr.Number, len(candidate.Findings), shortSHA(candidate.HeadSHA))
	decision := e.decision(st, now, projectState, ActionSpawnReviewRepair, summary,
		RiskMutating, 0.9, target, PolicyRuleReviewRepair, reasons)
	decision.StuckStates = append(decision.StuckStates, reviewRepairSpawnedStuckState(target, candidate))
	decision.ReviewRepair = e.buildReviewRepairPayload(candidate)
	return decision
}

// buildReviewRepairPayload converts the in-memory candidate into the
// serializable payload carried on the supervisor decision. The
// dispatcher reads back the same payload to build the worker prompt.
func (e *Engine) buildReviewRepairPayload(candidate *reviewRepairCandidate) *state.SupervisorReviewRepairPayload {
	cfg := e.cfg.Supervisor.ReviewRepair
	payload := &state.SupervisorReviewRepairPayload{
		HeadSHA:    candidate.HeadSHA,
		Backend:    cfg.EffectiveBackend(),
		Model:      strings.TrimSpace(cfg.Model),
		Effort:     strings.TrimSpace(cfg.Effort),
		MaxRetries: cfg.EffectiveMaxRetries(),
		Attempts:   candidate.Track.Attempts,
	}
	for _, f := range candidate.Findings {
		payload.Findings = append(payload.Findings, state.SupervisorReviewFinding{
			Path:     f.Path,
			Line:     f.Line,
			Body:     f.Body,
			Severity: severityLabel(f.Body),
			User:     f.User,
		})
	}
	return payload
}

// buildReviewRepairExhaustedDecision builds a fall-through decision when
// the (pr,head_sha) repair budget is exhausted but Greptile P0/P1 findings
// remain on head. Per #565 acceptance criteria the supervisor MUST NOT
// silently dead-end — it either routes to a merge_pr approval (when
// supervisor.review_repair.fall_through_to_merge_approval=true) or
// surfaces a review_repair_exhausted stuck state so the operator sees the
// unresolved comments listed in the MC attention queue.
func (e *Engine) buildReviewRepairExhaustedDecision(st *state.State, now time.Time, projectState state.SupervisorProjectState, baseReasons []string, slot string, sess *state.Session, pr github.PR, candidate *reviewRepairCandidate) state.SupervisorDecision {
	target := &state.SupervisorTarget{
		Issue:   sess.IssueNumber,
		PR:      pr.Number,
		HeadSHA: candidate.HeadSHA,
		Session: slot,
	}
	reasons := appendReasons(baseReasons,
		fmt.Sprintf("PR #%d retry-exhausted on review feedback; review-repair budget for (PR #%d, head %s) is also exhausted", pr.Number, pr.Number, shortSHA(candidate.HeadSHA)),
		fmt.Sprintf("%d Greptile P0/P1 inline comment(s) remain unaddressed on head %s — operator decision required", len(candidate.Findings), shortSHA(candidate.HeadSHA)),
	)
	reasons = appendReasons(reasons, formatReviewRepairFindings(candidate.Findings)...)

	if e.cfg.Supervisor.ReviewRepair.FallThroughMergeEnabled() {
		summary := fmt.Sprintf("Review-repair budget exhausted for PR #%d on head %s — fall through to operator merge approval (residual P0/P1 advisory)", pr.Number, shortSHA(candidate.HeadSHA))
		decision := e.decision(st, now, projectState, ActionMergePR, summary,
			RiskMutating, 0.85, target, PolicyRuleReviewRepair, reasons)
		decision.StuckStates = append(decision.StuckStates, reviewRepairExhaustedStuckState(target, candidate))
		return decision
	}

	summary := fmt.Sprintf("Hold PR #%d for operator review: review-repair budget exhausted on head %s with %d residual P0/P1 finding(s)", pr.Number, shortSHA(candidate.HeadSHA), len(candidate.Findings))
	decision := e.decision(st, now, projectState, ActionMonitorOpenPR, summary,
		RiskSafe, 0.9, target, PolicyRuleReviewRepair, reasons)
	decision.StuckStates = append(decision.StuckStates, reviewRepairExhaustedStuckState(target, candidate))
	return decision
}

func reviewRepairSpawnedStuckState(target *state.SupervisorTarget, candidate *reviewRepairCandidate) state.SupervisorStuckState {
	summary := fmt.Sprintf("Auto review-repair worker queued for PR #%d (head %s): %d Greptile P0/P1 finding(s) on head", target.PR, shortSHA(candidate.HeadSHA), len(candidate.Findings))
	return state.SupervisorStuckState{
		Code:              state.StuckReviewRepairSpawned,
		Severity:          SeverityWarning,
		Summary:           summary,
		Evidence:          reviewRepairEvidence(candidate.Findings),
		RecommendedAction: ActionSpawnReviewRepair,
		Target:            target,
	}
}

func reviewRepairExhaustedStuckState(target *state.SupervisorTarget, candidate *reviewRepairCandidate) state.SupervisorStuckState {
	summary := fmt.Sprintf("Review-repair budget exhausted for PR #%d on head %s — %d Greptile P0/P1 finding(s) unresolved; operator decision required", target.PR, shortSHA(candidate.HeadSHA), len(candidate.Findings))
	return state.SupervisorStuckState{
		Code:              state.StuckReviewRepairExhausted,
		Severity:          SeverityBlocked,
		Summary:           summary,
		Evidence:          reviewRepairEvidence(candidate.Findings),
		RecommendedAction: ActionMonitorOpenPR,
		Target:            target,
	}
}

// formatReviewRepairFindings returns a short, decision-Reasons-friendly
// summary of each finding ("internal/foo.go:42 P1: <body>"). Truncates
// bodies so the reason list does not balloon when Greptile pastes large
// suggestion blocks.
func formatReviewRepairFindings(findings []github.ReviewComment) []string {
	out := make([]string, 0, len(findings))
	for _, f := range findings {
		out = append(out, "Greptile finding: "+formatReviewCommentLine(f))
	}
	return out
}

// reviewRepairEvidence returns a denser per-finding evidence list used in
// the stuck-state, with the same truncation so dashboards stay readable.
func reviewRepairEvidence(findings []github.ReviewComment) []string {
	out := make([]string, 0, len(findings))
	for _, f := range findings {
		out = append(out, formatReviewCommentLine(f))
	}
	return out
}

func formatReviewCommentLine(f github.ReviewComment) string {
	path := strings.TrimSpace(f.Path)
	body := compactReviewBody(f.Body)
	sev := severityLabel(f.Body)
	switch {
	case path != "" && f.Line > 0:
		return fmt.Sprintf("%s:%d %s: %s", path, f.Line, sev, body)
	case path != "":
		return fmt.Sprintf("%s %s: %s", path, sev, body)
	default:
		return fmt.Sprintf("%s: %s", sev, body)
	}
}

// severityLabel extracts P0/P1 from a Greptile comment body. Defaults to
// "P?" when no marker is found — the comment must already be a known
// high-severity finding to reach this code path.
func severityLabel(body string) string {
	lower := strings.ToLower(body)
	switch {
	case strings.Contains(lower, "alt=\"p0\""), strings.Contains(lower, "/p0"), strings.Contains(lower, "badge/p0"):
		return "P0"
	case strings.Contains(lower, "alt=\"p1\""), strings.Contains(lower, "/p1"), strings.Contains(lower, "badge/p1"):
		return "P1"
	}
	return "P?"
}

const reviewRepairBodyTruncate = 240

func compactReviewBody(body string) string {
	body = strings.TrimSpace(body)
	if body == "" {
		return ""
	}
	body = strings.ReplaceAll(body, "\r\n", " ")
	body = strings.ReplaceAll(body, "\n", " ")
	body = strings.Join(strings.Fields(body), " ")
	if len(body) > reviewRepairBodyTruncate {
		return body[:reviewRepairBodyTruncate] + "…"
	}
	return body
}

func shortSHA(sha string) string {
	sha = strings.TrimSpace(sha)
	if len(sha) <= 12 {
		return sha
	}
	return sha[:12]
}

// FormatReviewRepairPromptFromPayload builds the worker prompt from the
// review-repair payload carried on a SupervisorDecision. Used by the
// orchestrator dispatcher when starting the scoped repair worker; it
// avoids a fresh GitHub round-trip.
func FormatReviewRepairPromptFromPayload(issueNumber, prNumber int, payload *state.SupervisorReviewRepairPayload) string {
	if payload == nil {
		return ""
	}
	findings := make([]github.ReviewComment, 0, len(payload.Findings))
	for _, f := range payload.Findings {
		findings = append(findings, github.ReviewComment{
			Path: f.Path,
			Line: f.Line,
			Body: f.Body,
			User: f.User,
		})
	}
	return FormatReviewRepairPrompt(issueNumber, prNumber, payload.HeadSHA, findings)
}

// FormatReviewRepairPrompt builds the worker prompt for a spawn_review_repair
// dispatch. The prompt is scoped to the specific Greptile findings (path /
// line / body) so the worker addresses only the comments — not a restatement
// of the original issue. Used by the orchestrator when starting the scoped
// repair worker.
func FormatReviewRepairPrompt(issueNumber int, prNumber int, headSHA string, findings []github.ReviewComment) string {
	var b strings.Builder
	b.WriteString("# Auto review-repair worker\n\n")
	b.WriteString(fmt.Sprintf("You are addressing Greptile review feedback on PR #%d (issue #%d) at head %s.\n\n", prNumber, issueNumber, shortSHA(headSHA)))
	b.WriteString("The previous worker for this issue exhausted its review-feedback retry budget on this PR.\n")
	b.WriteString("Your scope is NARROW: resolve EACH of the inline Greptile P0/P1 comments listed below.\n")
	b.WriteString("Do NOT re-implement the original issue. Do NOT refactor outside the touched files.\n\n")
	b.WriteString("Per finding:\n")
	b.WriteString("  1. Read the comment context (path, line, body).\n")
	b.WriteString("  2. Make the minimal code change that resolves the comment.\n")
	b.WriteString("  3. Push to the existing PR branch (do not open a new PR).\n\n")
	b.WriteString("## Unresolved Greptile P0/P1 inline comments on head ")
	b.WriteString(shortSHA(headSHA))
	b.WriteString("\n\n")
	for i, f := range findings {
		b.WriteString(fmt.Sprintf("### Finding %d (%s)\n", i+1, severityLabel(f.Body)))
		path := strings.TrimSpace(f.Path)
		if path != "" {
			if f.Line > 0 {
				b.WriteString(fmt.Sprintf("- Location: `%s:%d`\n", path, f.Line))
			} else {
				b.WriteString(fmt.Sprintf("- Location: `%s`\n", path))
			}
		}
		body := strings.TrimSpace(f.Body)
		if body != "" {
			b.WriteString("- Comment:\n\n")
			for _, line := range strings.Split(body, "\n") {
				b.WriteString("    ")
				b.WriteString(line)
				b.WriteString("\n")
			}
		}
		b.WriteString("\n")
	}
	b.WriteString("After every fix, run the project's test suite locally to confirm the change does not regress green CI.\n")
	return b.String()
}
