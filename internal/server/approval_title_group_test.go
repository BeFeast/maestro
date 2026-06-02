package server

import (
	"strings"
	"testing"
	"time"

	"github.com/befeast/maestro/internal/state"
)

// TestFleetApproval_TitleDropsSummaryDuplication pins issue #533 spec gap
// 12 at the API layer: the JSON Title field is the short
// "<verb> · #<target>" form, NOT the full supervisor summary that lives
// in Summary. The SPA renders Title as the heading and Summary as the
// body — when these collide (the pre-#533 bug) the card prints the same
// long sentence twice.
func TestFleetApproval_TitleDropsSummaryDuplication(t *testing.T) {
	longSummary := "Start a worker for issue #487: P0: add HTTP auth on mutating dashboard endpoints (write-path premortem #4)"
	st := &state.State{
		Approvals: []state.Approval{{
			ID:      "ap-1",
			Action:  "spawn_worker",
			Target:  &state.SupervisorTarget{Issue: 487},
			Status:  state.ApprovalStatusPending,
			Summary: longSummary,
		}},
	}
	project := fleetProjectState{Name: "scribe", Repo: "owner/repo"}

	items := makeFleetApprovalStates(project, st, time.Now().UTC())
	if len(items) != 1 {
		t.Fatalf("items = %d, want 1", len(items))
	}
	got := items[0]

	if got.Title != "Start worker · #487" {
		t.Fatalf("Title = %q, want %q", got.Title, "Start worker · #487")
	}
	if got.Summary != longSummary {
		t.Fatalf("Summary = %q, want supervisor reasoning preserved verbatim", got.Summary)
	}
	if strings.Contains(got.Title, longSummary) {
		t.Fatalf("Title %q absorbs the supervisor summary — gap 12 regression", got.Title)
	}
	if got.Title == got.Summary {
		t.Fatalf("Title and Summary collide (%q) — gap 12 regression", got.Title)
	}
}

// TestFleetApproval_GroupKeyAndSize_RegeneratedCard pins gap 1: two LIVE
// approvals on the same target collapse onto one group key with
// GroupSize=2, so the SPA can render «regenerated 2 times» on a single
// card instead of two flat rows.
func TestFleetApproval_GroupKeyAndSize_RegeneratedCard(t *testing.T) {
	st := &state.State{
		Approvals: []state.Approval{
			{
				ID:     "first",
				Action: "spawn_worker",
				Target: &state.SupervisorTarget{Issue: 487},
				Status: state.ApprovalStatusPending,
			},
			{
				ID:     "second",
				Action: "spawn_worker",
				Target: &state.SupervisorTarget{Issue: 487},
				Status: state.ApprovalStatusAwaitingDispatch,
			},
		},
	}
	items := makeFleetApprovalStates(fleetProjectState{Name: "scribe"}, st, time.Now().UTC())
	if len(items) != 2 {
		t.Fatalf("items = %d, want 2", len(items))
	}
	for _, item := range items {
		if item.GroupKey != "issue:487" {
			t.Fatalf("approval %s GroupKey = %q, want issue:487", item.ID, item.GroupKey)
		}
		if item.GroupSize != 2 {
			t.Fatalf("approval %s GroupSize = %d, want 2 (one card, regenerated once)", item.ID, item.GroupSize)
		}
	}
}

// TestFleetApproval_GroupSize_OnlyCountsLive verifies a terminal approval
// (rejected / superseded / stale) is excluded from the regenerated count
// — once the operator dismisses one of two duplicates, the remaining one
// renders as a singleton, not as a 2-card stack.
func TestFleetApproval_GroupSize_OnlyCountsLive(t *testing.T) {
	st := &state.State{
		Approvals: []state.Approval{
			{
				ID:     "live",
				Action: "spawn_worker",
				Target: &state.SupervisorTarget{Issue: 487},
				Status: state.ApprovalStatusPending,
			},
			{
				ID:     "dead-rejected",
				Action: "spawn_worker",
				Target: &state.SupervisorTarget{Issue: 487},
				Status: state.ApprovalStatusRejected,
			},
			{
				ID:     "dead-superseded",
				Action: "spawn_worker",
				Target: &state.SupervisorTarget{Issue: 487},
				Status: state.ApprovalStatusSuperseded,
			},
		},
	}
	items := makeFleetApprovalStates(fleetProjectState{Name: "scribe"}, st, time.Now().UTC())
	if len(items) != 3 {
		t.Fatalf("items = %d, want 3", len(items))
	}
	for _, item := range items {
		if item.GroupKey != "issue:487" {
			t.Fatalf("approval %s GroupKey = %q, want issue:487", item.ID, item.GroupKey)
		}
		if item.GroupSize != 1 {
			t.Fatalf("approval %s GroupSize = %d, want 1 (one live, two terminal)", item.ID, item.GroupSize)
		}
	}
}

// TestFleetApproval_GroupKey_PerTargetSeparation verifies that approvals on
// different targets do NOT share a group — issue #487 and issue #488 each
// render as singletons.
func TestFleetApproval_GroupKey_PerTargetSeparation(t *testing.T) {
	st := &state.State{
		Approvals: []state.Approval{
			{ID: "a", Action: "spawn_worker", Target: &state.SupervisorTarget{Issue: 487}, Status: state.ApprovalStatusPending},
			{ID: "b", Action: "spawn_worker", Target: &state.SupervisorTarget{Issue: 488}, Status: state.ApprovalStatusPending},
		},
	}
	items := makeFleetApprovalStates(fleetProjectState{Name: "scribe"}, st, time.Now().UTC())
	for _, item := range items {
		if item.GroupSize != 1 {
			t.Fatalf("approval %s GroupSize = %d, want 1 (per-target separation)", item.ID, item.GroupSize)
		}
	}
}
