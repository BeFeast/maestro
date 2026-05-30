package state

import (
	"testing"
	"time"
)

// TestHasApprovedSpawnForIssue covers the two states that count as
// "operator already gave the go-ahead to spawn" (approved + the post-fix
// execution_skipped status the approver returns for spawn_worker).
func TestHasApprovedSpawnForIssue(t *testing.T) {
	now := time.Now().UTC()
	s := NewState()
	s.Approvals = []Approval{
		// Pending: not counted; operator has not approved yet.
		{
			ID:        "p-1",
			Action:    "spawn_worker",
			Target:    &SupervisorTarget{Issue: 1},
			Status:    ApprovalStatusPending,
			CreatedAt: now,
		},
		// Approved spawn for issue 2: counted.
		{
			ID:        "p-2",
			Action:    "spawn_worker",
			Target:    &SupervisorTarget{Issue: 2},
			Status:    ApprovalStatusApproved,
			CreatedAt: now,
		},
		// Skipped spawn for issue 3 (post-fix path): counted.
		{
			ID:        "p-3",
			Action:    "spawn_worker",
			Target:    &SupervisorTarget{Issue: 3},
			Status:    ApprovalStatusExecutionSkipped,
			CreatedAt: now,
		},
		// Approved but for a different action: not counted.
		{
			ID:        "p-4",
			Action:    "merge_pr",
			Target:    &SupervisorTarget{Issue: 4, PR: 99},
			Status:    ApprovalStatusApproved,
			CreatedAt: now,
		},
	}

	tests := []struct {
		issue int
		want  bool
	}{
		{issue: 0, want: false},
		{issue: 1, want: false},
		{issue: 2, want: true},
		{issue: 3, want: true},
		{issue: 4, want: false},
		{issue: 99, want: false},
	}
	for _, tc := range tests {
		if got := s.HasApprovedSpawnForIssue(tc.issue); got != tc.want {
			t.Errorf("HasApprovedSpawnForIssue(%d) = %v, want %v", tc.issue, got, tc.want)
		}
	}
}

func TestHasApprovedSpawnForIssue_NilSafe(t *testing.T) {
	var s *State
	if s.HasApprovedSpawnForIssue(1) {
		t.Errorf("nil state must return false")
	}
}
