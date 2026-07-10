package state

import (
	"sort"
	"strings"
	"time"
)

// actionEditIssueBody is the approval verb that applies a groomed issue-body
// rewrite (#851). It mirrors config.SupervisorActionEditIssueBody; the state
// package deliberately does not import internal/config (that would invert the
// layering — see MigrateApprovalsBindRepo), so the literal lives here too.
const actionEditIssueBody = "edit_issue_body"

// riskApprovalGated mirrors supervisor.RiskApprovalGated; the state package
// cannot import the supervisor package (and treats Risk as an opaque string on
// the wire), so the literal lives here for the edit_issue_body mint.
const riskApprovalGated = "approval_gated"

// SpecLintTrack is the durable per-issue spec-lint / grooming record (#851).
// It answers two idempotency questions the supervisor asks every cycle:
//   - "have I already linted this exact body?" (BodyHash) — so a passing or
//     unchanged issue never gets a second comment;
//   - "have I already handled this `@maestro groom` mention?"
//     (LastGroomCommentID) — so one mention fires grooming exactly once.
//
// UpdatedAt is the tie-break for the 3-way merge (mergeSpecLintTracks), so a
// concurrent orchestrator Save cannot clobber a fresher lint mark.
type SpecLintTrack struct {
	Issue    int    `json:"issue"`
	BodyHash string `json:"body_hash,omitempty"`
	// Pass is the last lint verdict for BodyHash: true when the issue met the
	// good-spec rules, false when the checklist comment was posted.
	Pass               bool      `json:"pass"`
	LintedAt           time.Time `json:"linted_at,omitempty"`
	LastGroomCommentID int64     `json:"last_groom_comment_id,omitempty"`
	LastGroomedAt      time.Time `json:"last_groomed_at,omitempty"`
	UpdatedAt          time.Time `json:"updated_at,omitempty"`
}

// SpecLintTrackFor returns the lint record for an issue, if any.
func (s *State) SpecLintTrackFor(issue int) (SpecLintTrack, bool) {
	if s == nil || issue <= 0 {
		return SpecLintTrack{}, false
	}
	track, ok := s.SpecLintTracks[issue]
	return track, ok
}

// IssueLintedForBody reports whether the issue has already been linted at the
// given body hash. The supervisor gates the (cost-bearing) lint LLM pass on
// this so it lints at most once per body change.
func (s *State) IssueLintedForBody(issue int, bodyHash string) bool {
	if s == nil || issue <= 0 || strings.TrimSpace(bodyHash) == "" {
		return false
	}
	track, ok := s.SpecLintTracks[issue]
	return ok && track.BodyHash == bodyHash
}

// SpecLintPassedForBody reports whether the issue's current body has passed
// spec-lint. Used by the require_lint_pass gate to decide whether the ready
// label may be applied. A missing or stale-hash record returns false, so the
// gate is default-closed until lint proves the current body passes.
func (s *State) SpecLintPassedForBody(issue int, bodyHash string) bool {
	if s == nil || issue <= 0 || strings.TrimSpace(bodyHash) == "" {
		return false
	}
	track, ok := s.SpecLintTracks[issue]
	return ok && track.Pass && track.BodyHash == bodyHash
}

// RecordSpecLint records the lint verdict for an issue's current body,
// preserving any groom-mention bookkeeping. Idempotent: re-recording the same
// (bodyHash, pass) only refreshes timestamps.
func (s *State) RecordSpecLint(issue int, bodyHash string, pass bool, now time.Time) {
	if s == nil || issue <= 0 {
		return
	}
	if s.SpecLintTracks == nil {
		s.SpecLintTracks = make(map[int]SpecLintTrack)
	}
	track := s.SpecLintTracks[issue]
	track.Issue = issue
	track.BodyHash = strings.TrimSpace(bodyHash)
	track.Pass = pass
	track.LintedAt = normalizedTime(now)
	track.UpdatedAt = normalizedTime(now)
	s.SpecLintTracks[issue] = track
}

// GroomMentionHandled reports whether a `@maestro groom` comment with the given
// id (or an earlier one) has already been handled for this issue. GitHub
// comment ids increase monotonically, so a stored id >= the observed id means
// the mention is not new.
func (s *State) GroomMentionHandled(issue int, commentID int64) bool {
	if s == nil || issue <= 0 || commentID <= 0 {
		return false
	}
	track, ok := s.SpecLintTracks[issue]
	return ok && track.LastGroomCommentID >= commentID
}

// MarkGroomMentionHandled records that a `@maestro groom` mention up to
// commentID has been handled for this issue, preserving the lint verdict.
func (s *State) MarkGroomMentionHandled(issue int, commentID int64, now time.Time) {
	if s == nil || issue <= 0 || commentID <= 0 {
		return
	}
	if s.SpecLintTracks == nil {
		s.SpecLintTracks = make(map[int]SpecLintTrack)
	}
	track := s.SpecLintTracks[issue]
	track.Issue = issue
	if commentID > track.LastGroomCommentID {
		track.LastGroomCommentID = commentID
	}
	track.LastGroomedAt = normalizedTime(now)
	track.UpdatedAt = normalizedTime(now)
	s.SpecLintTracks[issue] = track
}

// RecordEditIssueBodyApproval mints (or refreshes in place) a pending
// edit_issue_body approval carrying the proposed rewrite on Target.Body (#851).
// Dedup is keyed on (edit_issue_body, issue): a re-groom with a fresh rewrite
// updates the live pending approval's body and payload hash under its stable
// id rather than piling up a sibling. Approving executes the edit; rejecting
// leaves the issue untouched. Returns the stored approval.
func (s *State) RecordEditIssueBodyApproval(issue int, body, summary, repo, project string, evidence []string, now time.Time) *Approval {
	if s == nil || issue <= 0 {
		return nil
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	now = normalizedTime(now)
	target := &SupervisorTarget{Issue: issue, Body: body}

	// Dedup against a still-effective approval for the same issue.
	for i := range s.Approvals {
		candidate := &s.Approvals[i]
		if candidate.Status != ApprovalStatusPending && candidate.Status != ApprovalStatusAwaitingDispatch {
			continue
		}
		if candidate.Action != actionEditIssueBody {
			continue
		}
		if candidate.Target == nil || candidate.Target.Issue != issue {
			continue
		}
		candidate.Target = cloneSupervisorTarget(target)
		candidate.Summary = summary
		candidate.Risk = riskApprovalGated
		candidate.Evidence = append([]string(nil), evidence...)
		if strings.TrimSpace(repo) != "" {
			candidate.Repo = repo
		}
		if strings.TrimSpace(project) != "" {
			candidate.Project = project
		}
		candidate.UpdatedAt = now
		candidate.PayloadHash = candidate.ComputePayloadHash()
		return candidate
	}

	decision := SupervisorDecision{
		RecommendedAction: actionEditIssueBody,
		Target:            cloneSupervisorTarget(target),
	}
	id := approvalID(decision, now)
	if s.approvalIDInUse(id) {
		id = id + "-" + now.UTC().Format("20060102T150405.000000000Z")
	}
	approval := Approval{
		ID:              id,
		CreatedAt:       now,
		UpdatedAt:       now,
		Action:          actionEditIssueBody,
		Target:          cloneSupervisorTarget(target),
		Summary:         summary,
		Risk:            riskApprovalGated,
		Evidence:        append([]string(nil), evidence...),
		Status:          ApprovalStatusPending,
		TargetStateHash: s.ApprovalTargetStateHash(target),
		Repo:            repo,
		Project:         project,
	}
	approval.PayloadHash = approval.ComputePayloadHash()
	approval.Audit = append(approval.Audit, ApprovalAudit{
		At:              now,
		Event:           ApprovalAuditCreated,
		PayloadHash:     approval.PayloadHash,
		TargetStateHash: approval.TargetStateHash,
	})
	s.Approvals = append(s.Approvals, approval)
	return &s.Approvals[len(s.Approvals)-1]
}

// PendingEditIssueBodyApproval returns the live pending/awaiting-dispatch
// edit_issue_body approval for an issue, if one exists. Used to avoid churning
// a fresh proposal comment when an unresolved proposal is already queued.
func (s *State) PendingEditIssueBodyApproval(issue int) (*Approval, bool) {
	if s == nil || issue <= 0 {
		return nil, false
	}
	for i := range s.Approvals {
		candidate := &s.Approvals[i]
		if candidate.Status != ApprovalStatusPending && candidate.Status != ApprovalStatusAwaitingDispatch {
			continue
		}
		if candidate.Action != actionEditIssueBody {
			continue
		}
		if candidate.Target != nil && candidate.Target.Issue == issue {
			return candidate, true
		}
	}
	return nil, false
}

func mergeSpecLintTracks(current, ours map[int]SpecLintTrack) map[int]SpecLintTrack {
	merged := make(map[int]SpecLintTrack)
	for _, key := range unionSpecLintTrackKeys(current, ours) {
		currentValue, currentOK := current[key]
		oursValue, oursOK := ours[key]
		switch {
		case currentOK && oursOK:
			if oursValue.UpdatedAt.After(currentValue.UpdatedAt) {
				merged[key] = oursValue
			} else {
				merged[key] = currentValue
			}
		case currentOK:
			merged[key] = currentValue
		case oursOK:
			merged[key] = oursValue
		}
	}
	return merged
}

func unionSpecLintTrackKeys(maps ...map[int]SpecLintTrack) []int {
	seen := make(map[int]struct{})
	for _, m := range maps {
		for k := range m {
			seen[k] = struct{}{}
		}
	}
	keys := make([]int, 0, len(seen))
	for k := range seen {
		keys = append(keys, k)
	}
	sort.Ints(keys)
	return keys
}
