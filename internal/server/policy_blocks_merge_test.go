package server

import (
	"strings"
	"testing"

	"github.com/befeast/maestro/internal/state"
)

// #425 (sup-98): policy_blocks_merge from the supervisor must flow into
// the session-level "needs attention" pill so the dashboard surfaces the
// operator-required merge approval instead of a passive "monitoring PR"
// indication.
func TestApplySupervisorAttention_PolicyBlocksMerge_NeedsAttention(t *testing.T) {
	infos := []sessionInfo{{
		Slot:        "scribe-1",
		IssueNumber: 200,
		PRNumber:    115,
		Status:      string(state.StatusPROpen),
	}}
	decision := &state.SupervisorDecision{
		RecommendedAction: "merge_pr",
		StuckStates: []state.SupervisorStuckState{{
			Code:              state.StuckPolicyBlocksMerge,
			Severity:          "warning",
			Summary:           "PR #115 is ready to merge but project policy requires operator approval for merge_pr.",
			RecommendedAction: "merge_pr",
			Target:            &state.SupervisorTarget{Issue: 200, PR: 115, Session: "scribe-1"},
		}},
	}

	applySupervisorAttention(infos, decision)

	if !infos[0].NeedsAttention {
		t.Fatalf("policy_blocks_merge must flag NeedsAttention: %#v", infos[0])
	}
	if !strings.Contains(infos[0].StatusReason, "operator approval") {
		t.Fatalf("status reason = %q, want it to name operator approval", infos[0].StatusReason)
	}
}

// supervisorStuckNeedsAttention must classify policy_blocks_merge as an
// attention state so it cannot regress to "informational".
func TestSupervisorStuckNeedsAttention_PolicyBlocksMerge(t *testing.T) {
	stuck := state.SupervisorStuckState{Code: state.StuckPolicyBlocksMerge, Severity: "warning"}
	if !supervisorStuckNeedsAttention(stuck) {
		t.Fatal("policy_blocks_merge must require attention")
	}
}
