package server

import (
	"testing"
	"time"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/state"
)

func TestFleetEpicProgressFromState_NoDecisionYieldsEmpty(t *testing.T) {
	st := state.NewState()
	epics, summary := fleetEpicProgressFromState(st)
	if epics != nil {
		t.Errorf("epics = %#v, want nil for empty state", epics)
	}
	if summary != (fleetEpicSummary{}) {
		t.Errorf("summary = %#v, want zero-valued", summary)
	}
}

func TestFleetEpicProgressFromState_DerivesSummaryFromLatestDecision(t *testing.T) {
	st := state.NewState()
	now := time.Date(2026, 6, 3, 12, 0, 0, 0, time.UTC)
	st.SupervisorDecisions = []state.SupervisorDecision{
		{
			ID:        "sup-1",
			CreatedAt: now,
			Epics: []state.EpicProgress{
				{Number: 100, TotalChildren: 3, MergedCount: 3, OpenCount: 0, AllChildrenDone: true, OutcomeHealthy: true, Complete: true},
				{Number: 101, TotalChildren: 4, MergedCount: 1, OpenCount: 3, Complete: false},
			},
		},
	}

	epics, summary := fleetEpicProgressFromState(st)
	if len(epics) != 2 {
		t.Fatalf("epics len = %d, want 2", len(epics))
	}
	if summary.Tracked != 2 {
		t.Errorf("Tracked = %d, want 2", summary.Tracked)
	}
	if summary.Complete != 1 || summary.InProgress != 1 {
		t.Errorf("Complete/InProgress = %d/%d, want 1/1", summary.Complete, summary.InProgress)
	}
	if summary.ChildrenTotal != 7 {
		t.Errorf("ChildrenTotal = %d, want 7", summary.ChildrenTotal)
	}
	if summary.ChildrenMerged != 4 {
		t.Errorf("ChildrenMerged = %d, want 4", summary.ChildrenMerged)
	}
	if summary.ChildrenOpen != 3 {
		t.Errorf("ChildrenOpen = %d, want 3", summary.ChildrenOpen)
	}
}

func TestFleetEpicProgressFromState_AwaitingApprovalCountsCloseIssueOnEpic(t *testing.T) {
	st := state.NewState()
	now := time.Date(2026, 6, 3, 12, 0, 0, 0, time.UTC)
	st.SupervisorDecisions = []state.SupervisorDecision{
		{
			ID:        "sup-1",
			CreatedAt: now,
			Epics: []state.EpicProgress{
				{Number: 200, TotalChildren: 2, MergedCount: 2, Complete: true},
				{Number: 300, TotalChildren: 1, MergedCount: 0, Complete: false},
			},
		},
	}
	// Pending close_issue against the complete epic counts as awaiting
	// approval; the same verb against a non-epic does not.
	st.Approvals = []state.Approval{
		{
			ID:     "close-epic",
			Action: config.SupervisorActionCloseIssue,
			Target: &state.SupervisorTarget{Issue: 200},
			Status: state.ApprovalStatusPending,
		},
		{
			ID:     "close-child",
			Action: config.SupervisorActionCloseIssue,
			Target: &state.SupervisorTarget{Issue: 999},
			Status: state.ApprovalStatusPending,
		},
		{
			ID:     "close-epic-but-executed",
			Action: config.SupervisorActionCloseIssue,
			Target: &state.SupervisorTarget{Issue: 200},
			Status: state.ApprovalStatusExecuted,
		},
	}

	_, summary := fleetEpicProgressFromState(st)
	if summary.AwaitingApproval != 1 {
		t.Fatalf("AwaitingApproval = %d, want 1", summary.AwaitingApproval)
	}
}
