package approver

import (
	"fmt"
	"strings"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/state"
)

// ApprovalTitle returns the short, action-first card title for an approval
// (issue #533, spec gap 12). The supervisor stores the full reasoning in
// Approval.Summary — historically the dashboard used that string for BOTH
// the card title and the body, producing a 1:1 duplication that read like
// "Start a worker for issue #487: P0: ..." twice on the same card. The
// title returned here is intentionally terse — "Start worker · #487",
// "Merge PR · #123", "Close issue · #42" — so the long-form summary is
// rendered exactly once, as the body.
//
// The function is total over an Approval; an unrecognised verb falls back
// to a verb-prettified form ("foo bar · #N"), and a missing target collapses
// the suffix.
func ApprovalTitle(approval *state.Approval) string {
	if approval == nil {
		return ""
	}
	verb := actionVerbLabel(approval.Action)
	suffix := approvalTargetSuffix(approval.Target)
	if suffix == "" {
		return verb
	}
	return verb + " · " + suffix
}

// ApprovalGroupKey returns the per-target dedup key the dashboard uses to
// fold N re-emitted approvals that share the same (issue, pr) target into a
// single card with a «regenerated N times» badge (issue #533, spec gap 1).
//
// The key is stable across the lifetime of a target so an approval on issue
// #487 minted at t0 and the same approval minted at t1 (after the dedup
// window expired or a fresh decision rolled in) share a key — the SPA can
// then count and group without needing to interpret the verb.
//
// When the approval lacks a target, the approval ID is used as a singleton
// key so the card still renders as its own group instead of all
// target-less approvals collapsing onto one row. When the approval also
// lacks an ID, the empty string is returned so the SPA / fleet aggregator
// skip the card entirely instead of folding unrelated approvals onto a
// shared literal key.
func ApprovalGroupKey(approval *state.Approval) string {
	if approval == nil {
		return ""
	}
	if approval.Target != nil {
		t := approval.Target
		switch {
		case t.PR > 0:
			return fmt.Sprintf("pr:%d", t.PR)
		case t.Issue > 0:
			return fmt.Sprintf("issue:%d", t.Issue)
		case strings.TrimSpace(t.Session) != "":
			return "session:" + strings.TrimSpace(t.Session)
		}
	}
	if id := strings.TrimSpace(approval.ID); id != "" {
		return "id:" + id
	}
	return ""
}

// actionVerbLabel maps an Approval.Action to the short verb form the card
// title uses. Kept in sync with the SPA's actionLabel() so server-rendered
// and client-rendered titles agree.
func actionVerbLabel(action string) string {
	switch strings.TrimSpace(action) {
	case config.SupervisorActionMergePR:
		return "Merge PR"
	case config.SupervisorActionCloseIssue:
		return "Close issue"
	case config.SupervisorActionDeleteWorktree:
		return "Delete worktree"
	case config.SupervisorActionChangeGlobalConfig:
		return "Apply config change"
	case "spawn_worker":
		return "Start worker"
	case "open_child_issue":
		return "Open child issue"
	case config.SupervisorActionSpawnReviewRepair:
		return "Spawn review-repair"
	case "":
		return "Approval"
	default:
		return prettifyVerb(action)
	}
}

// approvalTargetSuffix returns the "#N" / "slot foo" tail of an approval
// title — empty when no target is set.
func approvalTargetSuffix(target *state.SupervisorTarget) string {
	if target == nil {
		return ""
	}
	switch {
	case target.PR > 0:
		return fmt.Sprintf("#%d", target.PR)
	case target.Issue > 0:
		return fmt.Sprintf("#%d", target.Issue)
	case strings.TrimSpace(target.Session) != "":
		return "slot " + strings.TrimSpace(target.Session)
	}
	return ""
}

// prettifyVerb replaces underscores with spaces and uppercases the first
// rune, so a future verb the registry knows but this file does not still
// renders as a reasonable title ("do_something" -> "Do something").
func prettifyVerb(verb string) string {
	v := strings.TrimSpace(verb)
	if v == "" {
		return ""
	}
	v = strings.ReplaceAll(v, "_", " ")
	return strings.ToUpper(v[:1]) + v[1:]
}
