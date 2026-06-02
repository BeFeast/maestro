package approver

import (
	"strings"
	"testing"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/state"
)

// TestApprovalTitle_DropsSummaryDuplication pins issue #533 spec gap 12:
// the card title MUST be the short `<verb> · #<target>` form, NOT the
// full supervisor summary. A regression here means the dashboard shows
// the summary text twice (once as title, once as body).
func TestApprovalTitle_DropsSummaryDuplication(t *testing.T) {
	cases := []struct {
		name   string
		action string
		target *state.SupervisorTarget
		want   string
	}{
		{
			name:   "spawn_worker_uses_short_form_not_summary",
			action: "spawn_worker",
			target: &state.SupervisorTarget{Issue: 487},
			want:   "Start worker · #487",
		},
		{
			name:   "merge_pr_uses_pr_number",
			action: config.SupervisorActionMergePR,
			target: &state.SupervisorTarget{PR: 123, Issue: 100},
			want:   "Merge PR · #123",
		},
		{
			name:   "close_issue_uses_issue_number",
			action: config.SupervisorActionCloseIssue,
			target: &state.SupervisorTarget{Issue: 42},
			want:   "Close issue · #42",
		},
		{
			name:   "delete_worktree_uses_slot",
			action: config.SupervisorActionDeleteWorktree,
			target: &state.SupervisorTarget{Session: "sup-7"},
			want:   "Delete worktree · slot sup-7",
		},
		{
			name:   "spawn_review_repair_uses_pr_number",
			action: config.SupervisorActionSpawnReviewRepair,
			target: &state.SupervisorTarget{PR: 555, Issue: 540},
			want:   "Spawn review-repair · #555",
		},
		{
			name:   "open_child_issue_uses_parent_issue_number",
			action: "open_child_issue",
			target: &state.SupervisorTarget{Issue: 146},
			want:   "Open child issue · #146",
		},
		{
			name:   "change_global_config_no_target_collapses_suffix",
			action: config.SupervisorActionChangeGlobalConfig,
			target: nil,
			want:   "Apply config change",
		},
		{
			name:   "unknown_verb_prettified",
			action: "do_something",
			target: &state.SupervisorTarget{Issue: 1},
			want:   "Do something · #1",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			longSummary := "Start a worker for issue #487: P0: add HTTP auth on mutating dashboard endpoints (write-path premortem #4)"
			a := &state.Approval{Action: tc.action, Target: tc.target, Summary: longSummary}
			got := ApprovalTitle(a)
			if got != tc.want {
				t.Fatalf("ApprovalTitle = %q, want %q", got, tc.want)
			}
			// The whole point of the fix: the title must NOT contain the
			// supervisor's long summary text.
			if strings.Contains(got, longSummary) {
				t.Fatalf("title %q contains supervisor summary %q — gap 12 regression", got, longSummary)
			}
		})
	}
}

func TestApprovalTitle_NilApproval(t *testing.T) {
	if got := ApprovalTitle(nil); got != "" {
		t.Fatalf("ApprovalTitle(nil) = %q, want empty", got)
	}
}

// TestApprovalGroupKey_GroupsByTarget pins issue #533 spec gap 1: two
// approvals that share the same target — whether the verb is the same or
// not — collapse onto a single group key so the SPA can render them as
// «regenerated N times» on one card instead of N flat cards.
func TestApprovalGroupKey_GroupsByTarget(t *testing.T) {
	cases := []struct {
		name string
		a    *state.Approval
		want string
	}{
		{
			name: "pr_target_keyed_by_pr_number",
			a:    &state.Approval{Action: config.SupervisorActionMergePR, Target: &state.SupervisorTarget{PR: 99, Issue: 50}},
			want: "pr:99",
		},
		{
			name: "issue_target_keyed_by_issue_number",
			a:    &state.Approval{Action: "spawn_worker", Target: &state.SupervisorTarget{Issue: 487}},
			want: "issue:487",
		},
		{
			name: "session_target_keyed_by_slot",
			a:    &state.Approval{Action: config.SupervisorActionDeleteWorktree, Target: &state.SupervisorTarget{Session: "sup-7"}},
			want: "session:sup-7",
		},
		{
			name: "pr_dominates_when_both_set",
			a:    &state.Approval{Target: &state.SupervisorTarget{Issue: 500, PR: 600}},
			want: "pr:600",
		},
		{
			name: "no_target_falls_back_to_id_singleton",
			a:    &state.Approval{ID: "approval-XYZ", Target: nil},
			want: "id:approval-XYZ",
		},
		{
			name: "no_target_no_id_collapses_to_ungrouped",
			a:    &state.Approval{},
			want: "ungrouped",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ApprovalGroupKey(tc.a); got != tc.want {
				t.Fatalf("ApprovalGroupKey = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestApprovalGroupKey_TwoApprovalsSameTargetShareKey is the directly
// observable behaviour the dashboard relies on: minting a fresh approval
// over an already-resolved one on the same issue produces the same group
// key, so the SPA's «regenerated 2 times» badge can count them.
func TestApprovalGroupKey_TwoApprovalsSameTargetShareKey(t *testing.T) {
	a := &state.Approval{ID: "first", Action: "spawn_worker", Target: &state.SupervisorTarget{Issue: 487}}
	b := &state.Approval{ID: "second", Action: "spawn_worker", Target: &state.SupervisorTarget{Issue: 487}}
	if ApprovalGroupKey(a) != ApprovalGroupKey(b) {
		t.Fatalf("same-target approvals must share a group key; got %q and %q",
			ApprovalGroupKey(a), ApprovalGroupKey(b))
	}
}

func TestApprovalGroupKey_NilApproval(t *testing.T) {
	if got := ApprovalGroupKey(nil); got != "" {
		t.Fatalf("ApprovalGroupKey(nil) = %q, want empty", got)
	}
}
