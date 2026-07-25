package orchestrator

import (
	"crypto/sha256"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"

	"github.com/befeast/maestro/internal/github"
	"github.com/befeast/maestro/internal/state"
)

// prGateTransitionForCI binds an authoritative GitHub check-rollup read to the
// exact project/issue/PR/head. A legacy test hook can return a verdict without a
// head; that remains valid for merge behavior but intentionally cannot create a
// durable snapshot.
func (o *Orchestrator) prGateTransitionForCI(sess *state.Session, pr github.PR, rollup github.PRCheckRollup, effective string) (state.PRGateTransition, bool) {
	if o == nil || o.cfg == nil || sess == nil || strings.TrimSpace(rollup.HeadSHA) == "" {
		return state.PRGateTransition{}, false
	}
	transition := state.PRGateTransition{
		Project:                o.cfg.Repo,
		IssueNumber:            sess.IssueNumber,
		PRNumber:               pr.Number,
		HeadSHA:                rollup.HeadSHA,
		CIObserved:             rollup.Complete,
		CIRollupVerdict:        normalizePRGateCIVerdict(rollup.Verdict),
		CIEffectiveVerdict:     normalizePRGateCIVerdict(effective),
		CheckRollupObserved:    rollup.Complete && strings.TrimSpace(rollup.Fingerprint) != "",
		CheckRollupFingerprint: strings.TrimSpace(rollup.Fingerprint),
	}
	return transition, true
}

func normalizePRGateCIVerdict(value string) state.PRGateCIVerdict {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "pending":
		return state.PRGateCIPending
	case "success":
		return state.PRGateCISuccess
	case "failure":
		return state.PRGateCIFailure
	default:
		return state.PRGateCIUnknown
	}
}

// observePRGateCI reads the exact current PR head/check rollup and applies the
// narrow mergeable-state compatibility rule used by the merge flow. It does
// not mutate state: callers may enrich the returned transition with review
// evidence before persisting it, or persist the CI-only observation when a
// separate merge precondition (such as attribution) is safely deferred.
func (o *Orchestrator) observePRGateCI(sess *state.Session, pr github.PR) (github.PRCheckRollup, string, state.PRGateTransition, bool, time.Time, error) {
	ciRollup, err := o.prCheckRollup(pr.Number)
	if err != nil {
		return github.PRCheckRollup{}, "", state.PRGateTransition{}, false, time.Time{}, err
	}
	ciStatus := ciRollup.Verdict

	// #424: an aggregate can remain pending after every required check is green.
	// Only GitHub's clean mergeable state may override it; unstable can coexist
	// with explicitly failed checks and therefore remains pending/failing.
	if ciStatus == "pending" {
		if mergeable, mergeState, mergeErr := o.prMergeStatus(pr.Number); mergeErr == nil && mergeable == "MERGEABLE" && mergeState == "clean" {
			log.Printf("[orch] PR #%d (%s) aggregate CI=pending but mergeable_state=%s — treating as success (#424)", pr.Number, sess.Branch, mergeState)
			ciStatus = "success"
		}
	}

	log.Printf("[orch] PR #%d (%s) CI=%s", pr.Number, sess.Branch, ciStatus)
	transition, observable := o.prGateTransitionForCI(sess, pr, ciRollup, ciStatus)
	return ciRollup, ciStatus, transition, observable, time.Now().UTC(), nil
}

func (o *Orchestrator) persistPRGateTransition(s *state.State, transition state.PRGateTransition, now time.Time) {
	if s == nil || strings.TrimSpace(transition.HeadSHA) == "" {
		return
	}
	if _, _, err := s.RecordPRGateTransition(transition, now.UTC()); err != nil {
		// Keep raw identity and upstream content out of the log. The error is a
		// closed validation class only; merge/review behavior remains unchanged.
		log.Printf("[orch] PR-gate progress snapshot rejected for PR #%d: %v", transition.PRNumber, err)
	}
}

func (o *Orchestrator) prGateHeadMatches(prNumber int, expected string) bool {
	expected = strings.TrimSpace(expected)
	if expected == "" {
		return false
	}
	current, err := o.prHeadSHA(prNumber)
	if err != nil {
		log.Printf("[orch] PR-gate head recheck for PR #%d unavailable: %v", prNumber, err)
		return false
	}
	if !strings.EqualFold(strings.TrimSpace(current), expected) {
		log.Printf("[orch] PR #%d head changed during CI/review observation — deferring gate decision to the next cycle", prNumber)
		return false
	}
	return true
}

func prGateReviewProgress(verdict github.ReviewGateVerdict) (state.PRGateReviewDecision, string, string, int, []state.PRGateReviewStream) {
	decision := state.PRGateReviewBlocked
	switch {
	case verdict.Pending:
		decision = state.PRGateReviewPending
	case verdict.Passed:
		decision = state.PRGateReviewPassed
	}

	streamParts := make([]string, 0, len(verdict.Streams))
	streamFacts := make([]state.PRGateReviewStream, 0, len(verdict.Streams))
	findingDigests := make([]string, 0)
	for _, stream := range verdict.Streams {
		perStreamFindings := make([]string, 0, len(stream.Findings))
		for _, finding := range stream.Findings {
			digest := prGateOpaqueFingerprint(
				"user="+finding.User,
				"path="+finding.Path,
				fmt.Sprintf("line=%d", finding.Line),
				"body="+finding.Body,
			)
			perStreamFindings = append(perStreamFindings, digest)
			findingDigests = append(findingDigests, digest)
		}
		sort.Strings(perStreamFindings)
		streamFacts = append(streamFacts, state.PRGateReviewStream{
			Name:          strings.TrimSpace(stream.Name),
			Passed:        stream.Passed,
			Pending:       stream.Pending,
			Score:         stream.Score,
			ScoreMax:      stream.ScoreMax,
			Verdict:       state.PRGateReviewVerdict(stream.Verdict),
			FindingsCount: len(stream.Findings),
		})
		streamParts = append(streamParts, prGateOpaqueFingerprint(
			"stream="+strings.TrimSpace(stream.Name),
			fmt.Sprintf("passed=%t", stream.Passed),
			fmt.Sprintf("pending=%t", stream.Pending),
			fmt.Sprintf("score=%d/%d", stream.Score, stream.ScoreMax),
			"verdict="+strings.TrimSpace(stream.Verdict),
			"findings="+strings.Join(perStreamFindings, ","),
		))
	}
	sort.Slice(streamFacts, func(i, j int) bool { return streamFacts[i].Name < streamFacts[j].Name })
	sort.Strings(streamParts)
	sort.Strings(findingDigests)
	verdictFingerprint := prGateOpaqueFingerprint(
		"decision="+string(decision),
		"streams="+strings.Join(streamParts, ","),
	)
	findingFingerprint := ""
	if len(findingDigests) > 0 {
		findingFingerprint = prGateOpaqueFingerprint("findings=" + strings.Join(findingDigests, ","))
	}
	return decision, verdictFingerprint, findingFingerprint, len(findingDigests), streamFacts
}

func addPRGateReviewVerdict(transition *state.PRGateTransition, verdict github.ReviewGateVerdict) {
	if transition == nil {
		return
	}
	decision, verdictFingerprint, findingsFingerprint, findingsCount, streams := prGateReviewProgress(verdict)
	transition.ReviewObserved = true
	transition.ReviewDecision = decision
	transition.ReviewStreams = streams
	transition.ReviewVerdictFingerprint = verdictFingerprint
	transition.ActionableFindingsFingerprint = findingsFingerprint
	transition.ActionableFindingsCount = findingsCount
}

func addPRGateReviewDisabled(transition *state.PRGateTransition) {
	if transition == nil {
		return
	}
	transition.ReviewObserved = true
	transition.ReviewDecision = state.PRGateReviewDisabled
	transition.ReviewVerdictFingerprint = prGateOpaqueFingerprint("review-gate=disabled")
	transition.ActionableFindingsFingerprint = ""
	transition.ActionableFindingsCount = 0
}

// addPRGateLateFeedback records the semantic identity of legacy actionable
// feedback without persisting the feedback text or any path embedded in it.
func addPRGateLateFeedback(transition *state.PRGateTransition, feedback string) {
	if transition == nil || strings.TrimSpace(feedback) == "" {
		return
	}
	findingFingerprint := prGateOpaqueFingerprint("feedback=" + feedback)
	transition.ReviewObserved = true
	transition.ReviewDecision = state.PRGateReviewBlocked
	transition.ReviewVerdictFingerprint = prGateOpaqueFingerprint("legacy-actionable-feedback=" + findingFingerprint)
	transition.ActionableFindingsFingerprint = findingFingerprint
	transition.ActionableFindingsCount = 1
}

func prGateOpaqueFingerprint(parts ...string) string {
	h := sha256.New()
	for _, part := range parts {
		h.Write([]byte(part))
		h.Write([]byte{0})
	}
	return fmt.Sprintf("%x", h.Sum(nil))[:16]
}

func (o *Orchestrator) persistMergedPRGateTransition(s *state.State, sess *state.Session, prNumber int, mergeInfo github.PRMergeInfo, now time.Time) {
	if o == nil || o.cfg == nil || s == nil || sess == nil {
		return
	}
	head := strings.TrimSpace(mergeInfo.HeadSHA)
	if head == "" {
		if previous, ok := s.LatestPRGateSnapshot(o.cfg.Repo, sess.IssueNumber, prNumber); ok {
			head = previous.HeadSHA
		}
	}
	transition := state.PRGateTransition{
		Project:        o.cfg.Repo,
		IssueNumber:    sess.IssueNumber,
		PRNumber:       prNumber,
		HeadSHA:        head,
		MergeObserved:  true,
		MergeCommitSHA: mergeInfo.SHA,
		MergedAt:       mergeInfo.MergedAt,
	}
	o.persistPRGateTransition(s, transition, now)
}

// ensureMergedPRGateSnapshot closes the merge-command/eventual-consistency
// window and imports manual/reconciled merges. It is idempotent on the durable
// merge identity, so a code_landed cycle performs no upstream read once the
// exact transition has been captured.
func (o *Orchestrator) ensureMergedPRGateSnapshot(s *state.State, sess *state.Session, now time.Time) {
	if o == nil || o.cfg == nil || s == nil || sess == nil || sess.PRNumber <= 0 {
		return
	}
	if existing, ok := s.LatestPRGateSnapshot(o.cfg.Repo, sess.IssueNumber, sess.PRNumber); ok && existing.MergeCommitSHA != "" && !existing.MergedAt.IsZero() {
		return
	}
	mergeInfo, err := o.prMergeInfo(sess.PRNumber)
	if err != nil {
		log.Printf("[orch] PR-gate merge identity for PR #%d unavailable during reconciliation: %v", sess.PRNumber, err)
		return
	}
	o.persistMergedPRGateTransition(s, sess, sess.PRNumber, mergeInfo, now)
}
