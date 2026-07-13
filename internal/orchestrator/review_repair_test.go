package orchestrator

import (
	"context"
	"testing"
	"time"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/state"
	"github.com/befeast/maestro/internal/supervisor"
)

// reviewRepairCfg returns a minimal config with review-repair on and
// MaxRetries set to a deterministic value.
func reviewRepairCfg(maxRetries int) *config.Config {
	cfg := &config.Config{Repo: "owner/repo", MaxParallel: 5}
	enabled := true
	cfg.Supervisor.ReviewRepair.Enabled = &enabled
	cfg.Supervisor.ReviewRepair.MaxRetries = maxRetries
	cfg.Supervisor.ReviewRepair.Backend = "claude"
	return cfg
}

// #565 acceptance criterion: exactly one review-repair worker per
// (pr_number, head_sha). The orchestrator's tryClaimReviewRepairSlot
// honours the configured budget and refuses subsequent claims for the
// same head SHA.
func TestTryClaimReviewRepairSlot_HonoursBudget(t *testing.T) {
	o := &Orchestrator{cfg: reviewRepairCfg(1)}
	s := state.NewState()
	target := &state.SupervisorTarget{Issue: 442, PR: 564, HeadSHA: "deadbeef0000"}
	payload := &state.SupervisorReviewRepairPayload{HeadSHA: "deadbeef0000", MaxRetries: 1, Backend: "claude"}

	if !o.tryClaimReviewRepairSlot(s, target, payload) {
		t.Fatal("first claim must succeed")
	}
	if o.tryClaimReviewRepairSlot(s, target, payload) {
		t.Fatal("second claim against same (pr,head) must be refused once budget=1 is exhausted")
	}
	track, _ := s.LookupReviewRepairTrack(564, "deadbeef0000")
	if !track.Exhausted {
		t.Fatalf("track.Exhausted=false after refusal: %+v", track)
	}
}

// #874: a worker start that fails after the slot was claimed must release the
// attempt, so the bounded budget is not spent by a start that never produced a
// worker and the still-active approval remains dispatchable next cycle.
func TestReleaseReviewRepairSlot_RestoresBudgetAfterFailedStart(t *testing.T) {
	o := &Orchestrator{cfg: reviewRepairCfg(1)}
	s := state.NewState()
	target := &state.SupervisorTarget{Issue: 442, PR: 564, HeadSHA: "deadbeef0000"}
	payload := &state.SupervisorReviewRepairPayload{HeadSHA: "deadbeef0000", MaxRetries: 1, Backend: "claude"}

	if !o.tryClaimReviewRepairSlot(s, target, payload) {
		t.Fatal("first claim must succeed")
	}
	// Simulate the failed start: the dispatcher releases the claimed slot.
	o.releaseReviewRepairSlot(s, target, payload)

	track, _ := s.LookupReviewRepairTrack(564, "deadbeef0000")
	if track.Attempts != 0 || track.Exhausted {
		t.Fatalf("after release: track=%+v, want attempts=0 not exhausted", track)
	}
	// Budget restored: the next cycle can claim the same (pr,head) again.
	if !o.tryClaimReviewRepairSlot(s, target, payload) {
		t.Fatal("claim after release must succeed — a failed start must not burn the budget")
	}
}

// A new head SHA on the same PR (the repair worker pushed a fix) opens
// a fresh budget. The orchestrator must allow the next claim.
func TestTryClaimReviewRepairSlot_NewHeadSHAFreshBudget(t *testing.T) {
	o := &Orchestrator{cfg: reviewRepairCfg(1)}
	s := state.NewState()
	target := &state.SupervisorTarget{Issue: 442, PR: 564, HeadSHA: "old"}
	payload := &state.SupervisorReviewRepairPayload{HeadSHA: "old", MaxRetries: 1, Backend: "claude"}
	if !o.tryClaimReviewRepairSlot(s, target, payload) {
		t.Fatal("first claim on old head must succeed")
	}

	// Same (pr) but new head SHA: budget for the new head starts at 0.
	newTarget := &state.SupervisorTarget{Issue: 442, PR: 564, HeadSHA: "new"}
	newPayload := &state.SupervisorReviewRepairPayload{HeadSHA: "new", MaxRetries: 1, Backend: "claude"}
	if !o.tryClaimReviewRepairSlot(s, newTarget, newPayload) {
		t.Fatal("claim on new head SHA must start a fresh budget")
	}
}

// supervisorSelectedReviewRepair must resolve the latest decision when
// the cautious gate is permissive (no approval required).
func TestSupervisorSelectedReviewRepair_PermissivePicksLatestDecision(t *testing.T) {
	o := &Orchestrator{cfg: reviewRepairCfg(1)}
	s := state.NewState()
	now := time.Now().UTC()
	dec := state.SupervisorDecision{
		ID:                "test-1",
		CreatedAt:         now,
		RecommendedAction: supervisor.ActionSpawnReviewRepair,
		Risk:              supervisor.RiskMutating,
		RequiresApproval:  false,
		Target:            &state.SupervisorTarget{Issue: 442, PR: 564, HeadSHA: "deadbeef"},
		ReviewRepair: &state.SupervisorReviewRepairPayload{
			HeadSHA: "deadbeef",
			Backend: "claude",
			Findings: []state.SupervisorReviewFinding{
				{Path: "internal/foo.go", Line: 1, Body: "P1: bad", Severity: "P1"},
			},
		},
	}
	s.RecordSupervisorDecision(dec, state.DefaultSupervisorDecisionLimit)

	payload, target := o.supervisorSelectedReviewRepair(s, 442)
	if payload == nil || target == nil {
		t.Fatal("expected payload + target for issue 442")
	}
	if target.PR != 564 || target.HeadSHA != "deadbeef" {
		t.Fatalf("target = %+v", target)
	}
	if len(payload.Findings) != 1 || payload.Findings[0].Path != "internal/foo.go" {
		t.Fatalf("payload findings = %+v", payload.Findings)
	}
}

// Cautious gate: with RequiresApproval=true and no matching approved
// approval, the orchestrator must NOT dispatch yet.
func TestSupervisorSelectedReviewRepair_RequiresEffectiveApproval(t *testing.T) {
	o := &Orchestrator{cfg: reviewRepairCfg(1)}
	s := state.NewState()
	now := time.Now().UTC()
	dec := state.SupervisorDecision{
		ID:                "test-1",
		CreatedAt:         now,
		RecommendedAction: supervisor.ActionSpawnReviewRepair,
		Risk:              supervisor.RiskApprovalGated,
		RequiresApproval:  true,
		Target:            &state.SupervisorTarget{Issue: 442, PR: 564, HeadSHA: "deadbeef"},
		ReviewRepair:      &state.SupervisorReviewRepairPayload{HeadSHA: "deadbeef", Backend: "claude"},
	}
	s.RecordSupervisorDecision(dec, state.DefaultSupervisorDecisionLimit)

	if payload, _ := o.supervisorSelectedReviewRepair(s, 442); payload != nil {
		t.Fatalf("approval-gated decision without effective approval must NOT dispatch; got payload=%+v", payload)
	}

	// Adding an awaiting_dispatch approval for the same (action, target)
	// makes the dispatch eligible.
	s.Approvals = append(s.Approvals, state.Approval{
		ID:     "ap-1",
		Action: supervisor.ActionSpawnReviewRepair,
		Status: state.ApprovalStatusAwaitingDispatch,
		Target: &state.SupervisorTarget{Issue: 442, PR: 564, HeadSHA: "deadbeef"},
	})
	if payload, _ := o.supervisorSelectedReviewRepair(s, 442); payload == nil {
		t.Fatal("expected dispatch after awaiting_dispatch approval was recorded")
	}
}

// reviewRepairApproval builds an effective spawn_review_repair approval whose
// durable payload targets head.
func reviewRepairApproval(issue, pr int, head string) state.Approval {
	return state.Approval{
		ID:     "ap-durable",
		Action: supervisor.ActionSpawnReviewRepair,
		Status: state.ApprovalStatusAwaitingDispatch,
		Target: &state.SupervisorTarget{Issue: issue, PR: pr, HeadSHA: head},
		ReviewRepair: &state.SupervisorReviewRepairPayload{
			HeadSHA: head,
			Backend: "claude",
			Findings: []state.SupervisorReviewFinding{
				{Path: "internal/foo.go", Line: 1, Body: "P1: bad", Severity: "P1"},
			},
		},
	}
}

// #874: the dispatcher must converge from a durable approval's payload even
// when the LATEST supervisor decision is monitor_open_pr (the live sup-307
// wedge). The head guard reads the PR head; when it matches the approved
// payload, the payload + approval id are returned for dispatch.
func TestResolveReviewRepairDispatch_ConsumesDurableApprovalPayload(t *testing.T) {
	o := &Orchestrator{cfg: reviewRepairCfg(1), ghPRHeadSHAFn: func(pr int) (string, error) {
		return "deadbeef", nil
	}}
	s := state.NewState()
	now := time.Now().UTC()
	// Latest decision is monitor_open_pr — carries no review-repair payload.
	s.RecordSupervisorDecision(state.SupervisorDecision{
		ID:                "mon-1",
		CreatedAt:         now,
		RecommendedAction: supervisor.ActionMonitorOpenPR,
		Target:            &state.SupervisorTarget{Issue: 442, PR: 564, HeadSHA: "deadbeef"},
	}, state.DefaultSupervisorDecisionLimit)
	s.Approvals = append(s.Approvals, reviewRepairApproval(442, 564, "deadbeef"))

	payload, target, approvalID := o.resolveReviewRepairDispatch(s, 442)
	if payload == nil || target == nil {
		t.Fatal("expected dispatch from the durable approval payload despite monitor_open_pr being latest")
	}
	if approvalID != "ap-durable" {
		t.Fatalf("approvalID = %q, want ap-durable (approval-sourced dispatch)", approvalID)
	}
	if target.PR != 564 || len(payload.Findings) != 1 {
		t.Fatalf("payload/target = %+v / %+v", payload, target)
	}
}

// #874 changed-head: when the PR head has moved past the approved payload, the
// dispatcher must NOT repair the stale revision — it supersedes the approval
// and declines to dispatch.
func TestResolveReviewRepairDispatch_ChangedHeadSupersedes(t *testing.T) {
	o := &Orchestrator{cfg: reviewRepairCfg(1), ghPRHeadSHAFn: func(pr int) (string, error) {
		return "newhead", nil
	}}
	s := state.NewState()
	s.Approvals = append(s.Approvals, reviewRepairApproval(442, 564, "oldhead"))

	payload, _, _ := o.resolveReviewRepairDispatch(s, 442)
	if payload != nil {
		t.Fatal("changed head must not dispatch a stale-revision repair")
	}
	if s.Approvals[0].Status != state.ApprovalStatusSuperseded {
		t.Fatalf("approval status = %q, want superseded after head moved", s.Approvals[0].Status)
	}
}

// #874: a head-read error is handled conservatively — leave the approval
// pending rather than repair a possibly-wrong revision.
func TestResolveReviewRepairDispatch_HeadReadErrorLeavesPending(t *testing.T) {
	o := &Orchestrator{cfg: reviewRepairCfg(1), ghPRHeadSHAFn: func(pr int) (string, error) {
		return "", context.DeadlineExceeded
	}}
	s := state.NewState()
	s.Approvals = append(s.Approvals, reviewRepairApproval(442, 564, "oldhead"))

	if payload, _, _ := o.resolveReviewRepairDispatch(s, 442); payload != nil {
		t.Fatal("head-read error must not dispatch")
	}
	if s.Approvals[0].Status != state.ApprovalStatusAwaitingDispatch {
		t.Fatalf("approval status = %q, want still awaiting_dispatch after a head-read error", s.Approvals[0].Status)
	}
}

// #874: a pending (unapproved) durable approval must NOT dispatch — the
// cautious gate has to clear first.
func TestResolveReviewRepairDispatch_PendingApprovalDoesNotDispatch(t *testing.T) {
	o := &Orchestrator{cfg: reviewRepairCfg(1), ghPRHeadSHAFn: func(pr int) (string, error) {
		return "deadbeef", nil
	}}
	s := state.NewState()
	pending := reviewRepairApproval(442, 564, "deadbeef")
	pending.Status = state.ApprovalStatusPending
	s.Approvals = append(s.Approvals, pending)

	if payload, _, _ := o.resolveReviewRepairDispatch(s, 442); payload != nil {
		t.Fatal("a pending (unapproved) approval must not be dispatched")
	}
}
