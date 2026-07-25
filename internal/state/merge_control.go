package state

import (
	"fmt"
	"strings"
	"time"
)

const mergeClaimTTL = 5 * time.Minute

// MergeControl is the durable compare/claim record for one PR's merge
// boundary. Mission Control holds and merge claims share this record so a late
// hold invalidates an earlier claim instead of racing an external merge call.
type MergeControl struct {
	IssueNumber int `json:"issue_number,omitempty"`
	PRNumber    int `json:"pr_number"`

	Held       bool      `json:"held,omitempty"`
	HoldActor  string    `json:"hold_actor,omitempty"`
	HoldReason string    `json:"hold_reason,omitempty"`
	HeldAt     time.Time `json:"held_at,omitempty"`

	ClaimID      string    `json:"claim_id,omitempty"`
	ClaimHeadSHA string    `json:"claim_head_sha,omitempty"`
	ClaimOwner   string    `json:"claim_owner,omitempty"`
	ClaimedAt    time.Time `json:"claimed_at,omitempty"`

	LastRefusalReason string    `json:"last_refusal_reason,omitempty"`
	LastRefusedAt     time.Time `json:"last_refused_at,omitempty"`
	LastResult        string    `json:"last_result,omitempty"`
	UpdatedAt         time.Time `json:"updated_at"`
}

func (s *State) MergeControlForPR(prNumber int) (MergeControl, bool) {
	if s == nil || prNumber <= 0 {
		return MergeControl{}, false
	}
	control, ok := s.MergeControls[prNumber]
	return control, ok
}

// HoldMerge records an explicit operator hold and invalidates any in-flight
// claim. The external blocked-label write happens separately; keeping this
// durable hold even if that write fails is the fail-safe outcome.
func (s *State) HoldMerge(issueNumber, prNumber int, actor, reason string, now time.Time) MergeControl {
	if s.MergeControls == nil {
		s.MergeControls = make(map[int]MergeControl)
	}
	control := s.MergeControls[prNumber]
	control.IssueNumber = issueNumber
	control.PRNumber = prNumber
	control.Held = true
	control.HoldActor = strings.TrimSpace(actor)
	control.HoldReason = strings.TrimSpace(reason)
	control.HeldAt = normalizedTime(now)
	control.ClaimID = ""
	control.ClaimHeadSHA = ""
	control.ClaimOwner = ""
	control.ClaimedAt = time.Time{}
	control.LastResult = "held"
	control.UpdatedAt = normalizedTime(now)
	s.MergeControls[prNumber] = control
	return control
}

func (s *State) ReleaseMergeHold(issueNumber, prNumber int, actor, reason string, now time.Time) MergeControl {
	if s.MergeControls == nil {
		s.MergeControls = make(map[int]MergeControl)
	}
	control := s.MergeControls[prNumber]
	control.IssueNumber = issueNumber
	control.PRNumber = prNumber
	control.Held = false
	control.HoldActor = strings.TrimSpace(actor)
	control.HoldReason = strings.TrimSpace(reason)
	control.HeldAt = time.Time{}
	control.LastResult = "released"
	control.UpdatedAt = normalizedTime(now)
	s.MergeControls[prNumber] = control
	return control
}

// TryClaimMerge atomically compares the current hold/claim state and reserves
// the PR for one final validation+merge attempt. The caller persists this via
// state.Update before performing any GitHub side effect.
func (s *State) TryClaimMerge(issueNumber, prNumber int, claimID, headSHA, owner string, now time.Time) (MergeControl, string) {
	if s.MergeControls == nil {
		s.MergeControls = make(map[int]MergeControl)
	}
	control := s.MergeControls[prNumber]
	control.IssueNumber = issueNumber
	control.PRNumber = prNumber
	if control.LastResult == "merged" {
		reason := "merge already completed at the final claim boundary"
		control.LastRefusalReason = reason
		control.LastRefusedAt = normalizedTime(now)
		control.UpdatedAt = normalizedTime(now)
		s.MergeControls[prNumber] = control
		return control, reason
	}
	if control.Held {
		reason := control.HoldReason
		if reason == "" {
			reason = "an explicit merge hold is active"
		}
		control.LastRefusalReason = reason
		control.LastRefusedAt = normalizedTime(now)
		control.LastResult = "refused"
		control.UpdatedAt = normalizedTime(now)
		s.MergeControls[prNumber] = control
		return control, reason
	}
	if strings.TrimSpace(control.ClaimID) != "" && control.ClaimID != claimID &&
		!control.ClaimedAt.IsZero() && now.Before(control.ClaimedAt.Add(mergeClaimTTL)) {
		reason := fmt.Sprintf("merge claim %s is already active", control.ClaimID)
		control.LastRefusalReason = reason
		control.LastRefusedAt = normalizedTime(now)
		control.LastResult = "refused"
		control.UpdatedAt = normalizedTime(now)
		s.MergeControls[prNumber] = control
		return control, reason
	}
	control.ClaimID = strings.TrimSpace(claimID)
	control.ClaimHeadSHA = strings.TrimSpace(headSHA)
	control.ClaimOwner = strings.TrimSpace(owner)
	control.ClaimedAt = normalizedTime(now)
	control.LastResult = "claimed"
	control.UpdatedAt = normalizedTime(now)
	s.MergeControls[prNumber] = control
	return control, ""
}

func (s *State) ValidateMergeClaim(prNumber int, claimID string) (MergeControl, string) {
	control, ok := s.MergeControlForPR(prNumber)
	if !ok {
		return MergeControl{}, "merge claim disappeared before execution"
	}
	if control.Held {
		reason := control.HoldReason
		if reason == "" {
			reason = "an explicit merge hold became active"
		}
		return control, reason
	}
	if strings.TrimSpace(control.ClaimID) == "" || control.ClaimID != strings.TrimSpace(claimID) {
		return control, "merge claim was superseded before execution"
	}
	return control, ""
}

func (s *State) RecordMergeRefusal(prNumber int, claimID, reason string, now time.Time) {
	if s.MergeControls == nil {
		s.MergeControls = make(map[int]MergeControl)
	}
	control := s.MergeControls[prNumber]
	control.PRNumber = prNumber
	if claimID == "" || control.ClaimID == claimID {
		control.ClaimID = ""
		control.ClaimHeadSHA = ""
		control.ClaimOwner = ""
		control.ClaimedAt = time.Time{}
	}
	control.LastRefusalReason = strings.TrimSpace(reason)
	control.LastRefusedAt = normalizedTime(now)
	control.LastResult = "refused"
	control.UpdatedAt = normalizedTime(now)
	s.MergeControls[prNumber] = control
}

func (s *State) FinishMergeClaim(prNumber int, claimID, result string, now time.Time) {
	if s.MergeControls == nil {
		s.MergeControls = make(map[int]MergeControl)
	}
	control := s.MergeControls[prNumber]
	control.PRNumber = prNumber
	if control.ClaimID == strings.TrimSpace(claimID) {
		control.ClaimID = ""
		control.ClaimHeadSHA = ""
		control.ClaimOwner = ""
		control.ClaimedAt = time.Time{}
	}
	control.LastResult = strings.TrimSpace(result)
	control.UpdatedAt = normalizedTime(now)
	s.MergeControls[prNumber] = control
}

func mergeMergeControls(current, ours map[int]MergeControl) map[int]MergeControl {
	merged := make(map[int]MergeControl, len(current)+len(ours))
	for pr, control := range current {
		merged[pr] = control
	}
	for pr, control := range ours {
		existing, ok := merged[pr]
		if !ok || control.UpdatedAt.After(existing.UpdatedAt) ||
			(control.UpdatedAt.Equal(existing.UpdatedAt) && control.Held && !existing.Held) {
			merged[pr] = control
		}
	}
	return merged
}
