package orchestrator

import (
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
