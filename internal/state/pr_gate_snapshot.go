package state

import (
	"crypto/sha256"
	"fmt"
	"sort"
	"strings"
	"time"
)

// PRGateCIVerdict is a closed, non-secret CI/check-rollup state. The
// orchestrator records both GitHub's aggregate rollup and its effective verdict
// (which may use GitHub's mergeable-state override for a stale legacy status).
type PRGateCIVerdict string

const (
	PRGateCIUnknown PRGateCIVerdict = "unknown"
	PRGateCIPending PRGateCIVerdict = "pending"
	PRGateCISuccess PRGateCIVerdict = "success"
	PRGateCIFailure PRGateCIVerdict = "failure"
)

// PRGateReviewDecision is the normalized aggregate review-gate decision.
type PRGateReviewDecision string

const (
	PRGateReviewUnknown  PRGateReviewDecision = "unknown"
	PRGateReviewDisabled PRGateReviewDecision = "disabled"
	PRGateReviewPending  PRGateReviewDecision = "pending"
	PRGateReviewPassed   PRGateReviewDecision = "passed"
	PRGateReviewBlocked  PRGateReviewDecision = "blocked"
)

// PRGateSnapshot is one durable, semantic PR-gate generation. It contains only
// public forge identity, closed verdicts, timestamps, counts, and opaque
// non-reversible fingerprints. Raw check output, review bodies, private paths,
// credentials, notification-dedup state, and Greptile re-trigger timers are
// forbidden here (#887).
type PRGateSnapshot struct {
	Project                       string               `json:"project"`
	IssueNumber                   int                  `json:"issue_number"`
	PRNumber                      int                  `json:"pr_number"`
	HeadSHA                       string               `json:"head_sha"`
	Generation                    uint64               `json:"generation"`
	CIRollupVerdict               PRGateCIVerdict      `json:"ci_rollup_verdict"`
	CIEffectiveVerdict            PRGateCIVerdict      `json:"ci_effective_verdict"`
	CheckRollupFingerprint        string               `json:"check_rollup_fingerprint,omitempty"`
	ReviewDecision                PRGateReviewDecision `json:"review_decision"`
	ReviewVerdictFingerprint      string               `json:"review_verdict_fingerprint,omitempty"`
	ActionableFindingsFingerprint string               `json:"actionable_findings_fingerprint,omitempty"`
	ActionableFindingsCount       int                  `json:"actionable_findings_count,omitempty"`
	FeedbackGeneration            uint64               `json:"feedback_generation,omitempty"`
	MergeCommitSHA                string               `json:"merge_commit_sha,omitempty"`
	MergedAt                      time.Time            `json:"merged_at,omitempty"`
	UpdatedAt                     time.Time            `json:"updated_at"`
}

// PRGateTransition is one authoritative orchestrator observation. Availability
// is explicit: an unavailable upstream read preserves the last durable value,
// so a polling failure or signal disappearance can never fabricate progress.
type PRGateTransition struct {
	Project     string
	IssueNumber int
	PRNumber    int
	HeadSHA     string

	CIObserved             bool
	CIRollupVerdict        PRGateCIVerdict
	CIEffectiveVerdict     PRGateCIVerdict
	CheckRollupObserved    bool
	CheckRollupFingerprint string

	ReviewObserved                bool
	ReviewDecision                PRGateReviewDecision
	ReviewVerdictFingerprint      string
	ActionableFindingsFingerprint string
	ActionableFindingsCount       int

	MergeObserved  bool
	MergeCommitSHA string
	MergedAt       time.Time
}

// Key returns the JSON-stable opaque key for the exact
// project/issue/PR/head/generation identity. The full exact identity remains in
// the value for audit and actuation binding; the map key itself exposes no raw
// material.
func (s PRGateSnapshot) Key() string {
	project, ok := normalizePRGateProject(s.Project)
	if !ok || s.IssueNumber <= 0 || s.PRNumber <= 0 || !validFullSHA(s.HeadSHA) || s.Generation == 0 {
		return ""
	}
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%d\x00%d\x00%s\x00%d", project, s.IssueNumber, s.PRNumber, strings.ToLower(s.HeadSHA), s.Generation)))
	return fmt.Sprintf("pr-gate:%x", sum[:12])
}

// LatestPRGateSnapshot returns the newest durable generation for one logical
// PR. project may be empty only for a project-scoped state consumer (the
// supervisor collector); non-empty callers get an exact project match.
func (s *State) LatestPRGateSnapshot(project string, issueNumber, prNumber int) (PRGateSnapshot, bool) {
	if s == nil || issueNumber <= 0 || prNumber <= 0 {
		return PRGateSnapshot{}, false
	}
	project = strings.TrimSpace(project)
	var latest PRGateSnapshot
	found := false
	for key, snapshot := range s.PRGateSnapshots {
		if key != snapshot.Key() || snapshot.IssueNumber != issueNumber || snapshot.PRNumber != prNumber {
			continue
		}
		if project != "" && snapshot.Project != project {
			continue
		}
		if !found || newerPRGateSnapshot(snapshot, latest) {
			latest, found = snapshot, true
		}
	}
	return latest, found
}

// RecordPRGateTransition applies one semantic forge observation. Generation
// advances only when head, CI/check rollup, review decision/finding identity, or
// merge identity actually changes. Repeated polling, notifications, and review
// re-trigger bookkeeping therefore leave UpdatedAt and Generation untouched.
func (s *State) RecordPRGateTransition(transition PRGateTransition, now time.Time) (PRGateSnapshot, bool, error) {
	if s == nil {
		return PRGateSnapshot{}, false, fmt.Errorf("record PR-gate transition on nil state")
	}
	project, ok := normalizePRGateProject(transition.Project)
	if !ok {
		return PRGateSnapshot{}, false, fmt.Errorf("invalid PR-gate project identity")
	}
	head := strings.ToLower(strings.TrimSpace(transition.HeadSHA))
	if transition.IssueNumber <= 0 || transition.PRNumber <= 0 || !validFullSHA(head) {
		return PRGateSnapshot{}, false, fmt.Errorf("PR-gate transition requires positive issue/PR and a full head SHA")
	}

	previous, hadPrevious := s.LatestPRGateSnapshot(project, transition.IssueNumber, transition.PRNumber)
	next := previous
	if !hadPrevious || previous.HeadSHA != head {
		next = PRGateSnapshot{
			Project:            project,
			IssueNumber:        transition.IssueNumber,
			PRNumber:           transition.PRNumber,
			HeadSHA:            head,
			CIRollupVerdict:    PRGateCIUnknown,
			CIEffectiveVerdict: PRGateCIUnknown,
			ReviewDecision:     PRGateReviewUnknown,
		}
		if hadPrevious {
			next.FeedbackGeneration = previous.FeedbackGeneration
		}
	}

	if transition.CIObserved {
		rollup, validRollup := normalizePRGateCIVerdict(transition.CIRollupVerdict)
		effective, validEffective := normalizePRGateCIVerdict(transition.CIEffectiveVerdict)
		if !validRollup || !validEffective {
			return PRGateSnapshot{}, false, fmt.Errorf("invalid PR-gate CI verdict")
		}
		next.CIRollupVerdict = rollup
		next.CIEffectiveVerdict = effective
	}
	if transition.CheckRollupObserved {
		fingerprint, valid := normalizeOpaqueFingerprint(transition.CheckRollupFingerprint)
		if !valid {
			return PRGateSnapshot{}, false, fmt.Errorf("invalid PR-gate check-rollup fingerprint")
		}
		next.CheckRollupFingerprint = fingerprint
	}

	if transition.ReviewObserved {
		decision, valid := normalizePRGateReviewDecision(transition.ReviewDecision)
		if !valid || transition.ActionableFindingsCount < 0 {
			return PRGateSnapshot{}, false, fmt.Errorf("invalid PR-gate review observation")
		}
		verdictFingerprint, valid := normalizeOptionalOpaqueFingerprint(transition.ReviewVerdictFingerprint)
		if !valid {
			return PRGateSnapshot{}, false, fmt.Errorf("invalid PR-gate review-verdict fingerprint")
		}
		findingFingerprint, valid := normalizeOptionalOpaqueFingerprint(transition.ActionableFindingsFingerprint)
		if !valid || (transition.ActionableFindingsCount > 0 && findingFingerprint == "") || (transition.ActionableFindingsCount == 0 && findingFingerprint != "") {
			return PRGateSnapshot{}, false, fmt.Errorf("invalid PR-gate actionable-finding identity")
		}
		if next.ActionableFindingsFingerprint != findingFingerprint || next.ActionableFindingsCount != transition.ActionableFindingsCount {
			next.FeedbackGeneration++
		}
		next.ReviewDecision = decision
		next.ReviewVerdictFingerprint = verdictFingerprint
		next.ActionableFindingsFingerprint = findingFingerprint
		next.ActionableFindingsCount = transition.ActionableFindingsCount
	}

	if transition.MergeObserved {
		mergeSHA := strings.ToLower(strings.TrimSpace(transition.MergeCommitSHA))
		if !validFullSHA(mergeSHA) || transition.MergedAt.IsZero() {
			return PRGateSnapshot{}, false, fmt.Errorf("PR-gate merge transition requires full merge SHA and timestamp")
		}
		next.MergeCommitSHA = mergeSHA
		next.MergedAt = transition.MergedAt.UTC()
	}

	changed := !hadPrevious || !prGateSnapshotSemanticEqual(previous, next)
	if !changed {
		return previous, false, nil
	}
	next.Generation = 1
	if hadPrevious {
		next.Generation = previous.Generation + 1
	}
	next.UpdatedAt = normalizedTime(now)
	if s.PRGateSnapshots == nil {
		s.PRGateSnapshots = make(map[string]PRGateSnapshot)
	}
	for key, snapshot := range s.PRGateSnapshots {
		if snapshot.Project == project && snapshot.IssueNumber == transition.IssueNumber && snapshot.PRNumber == transition.PRNumber {
			delete(s.PRGateSnapshots, key)
		}
	}
	key := next.Key()
	if key == "" {
		return PRGateSnapshot{}, false, fmt.Errorf("invalid exact PR-gate snapshot identity")
	}
	s.PRGateSnapshots[key] = next
	return next, true, nil
}

func prGateSnapshotSemanticEqual(a, b PRGateSnapshot) bool {
	return a.Project == b.Project && a.IssueNumber == b.IssueNumber && a.PRNumber == b.PRNumber && a.HeadSHA == b.HeadSHA &&
		a.CIRollupVerdict == b.CIRollupVerdict && a.CIEffectiveVerdict == b.CIEffectiveVerdict &&
		a.CheckRollupFingerprint == b.CheckRollupFingerprint && a.ReviewDecision == b.ReviewDecision &&
		a.ReviewVerdictFingerprint == b.ReviewVerdictFingerprint &&
		a.ActionableFindingsFingerprint == b.ActionableFindingsFingerprint && a.ActionableFindingsCount == b.ActionableFindingsCount &&
		a.FeedbackGeneration == b.FeedbackGeneration && a.MergeCommitSHA == b.MergeCommitSHA && a.MergedAt.Equal(b.MergedAt)
}

func normalizePRGateCIVerdict(value PRGateCIVerdict) (PRGateCIVerdict, bool) {
	value = PRGateCIVerdict(strings.ToLower(strings.TrimSpace(string(value))))
	switch value {
	case PRGateCIUnknown, PRGateCIPending, PRGateCISuccess, PRGateCIFailure:
		return value, true
	default:
		return "", false
	}
}

func normalizePRGateReviewDecision(value PRGateReviewDecision) (PRGateReviewDecision, bool) {
	value = PRGateReviewDecision(strings.ToLower(strings.TrimSpace(string(value))))
	switch value {
	case PRGateReviewUnknown, PRGateReviewDisabled, PRGateReviewPending, PRGateReviewPassed, PRGateReviewBlocked:
		return value, true
	default:
		return "", false
	}
}

func normalizePRGateProject(value string) (string, bool) {
	value = strings.TrimSpace(value)
	parts := strings.Split(value, "/")
	if len(parts) != 2 || !validForgeName(parts[0]) || !validForgeName(parts[1]) {
		return "", false
	}
	return value, true
}

func validForgeName(value string) bool {
	if value == "" || value == "." || value == ".." {
		return false
	}
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.' {
			continue
		}
		return false
	}
	return true
}

func validFullSHA(value string) bool {
	value = strings.TrimSpace(value)
	if len(value) != 40 {
		return false
	}
	for _, r := range value {
		if (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F') {
			continue
		}
		return false
	}
	return true
}

func normalizeOpaqueFingerprint(value string) (string, bool) {
	value = strings.ToLower(strings.TrimSpace(value))
	if len(value) < 16 || len(value) > 64 {
		return "", false
	}
	for _, r := range value {
		if (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') {
			continue
		}
		return "", false
	}
	return value, true
}

func normalizeOptionalOpaqueFingerprint(value string) (string, bool) {
	if strings.TrimSpace(value) == "" {
		return "", true
	}
	return normalizeOpaqueFingerprint(value)
}

func newerPRGateSnapshot(candidate, current PRGateSnapshot) bool {
	if candidate.Generation != current.Generation {
		return candidate.Generation > current.Generation
	}
	if !candidate.UpdatedAt.Equal(current.UpdatedAt) {
		return candidate.UpdatedAt.After(current.UpdatedAt)
	}
	return candidate.Key() > current.Key()
}

// mergePRGateSnapshots keeps the newest semantic generation for each logical
// project+issue+PR. This prevents a stale supervisor save from clobbering a
// fresh orchestrator transition and also collapses an old exact-generation key
// that a concurrent writer still carried.
func mergePRGateSnapshots(current, ours map[string]PRGateSnapshot) map[string]PRGateSnapshot {
	latest := make(map[string]PRGateSnapshot)
	for _, snapshots := range []map[string]PRGateSnapshot{current, ours} {
		for key, snapshot := range snapshots {
			if key != snapshot.Key() {
				continue
			}
			logical := fmt.Sprintf("%s\x00%d\x00%d", snapshot.Project, snapshot.IssueNumber, snapshot.PRNumber)
			prior, ok := latest[logical]
			if !ok || newerPRGateSnapshot(snapshot, prior) {
				latest[logical] = snapshot
			}
		}
	}
	keys := make([]string, 0, len(latest))
	for logical := range latest {
		keys = append(keys, logical)
	}
	sort.Strings(keys)
	merged := make(map[string]PRGateSnapshot, len(keys))
	for _, logical := range keys {
		snapshot := latest[logical]
		merged[snapshot.Key()] = snapshot
	}
	return merged
}
