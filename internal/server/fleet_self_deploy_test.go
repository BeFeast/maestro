package server

import (
	"testing"
	"time"
)

// #711: a failed/rolled-back self-deploy finding must surface loudly via
// /api/v1/fleet — an error-tone, top-priority operator state with
// deploy-specific copy — so an undeployed-but-merged host is not silent. The
// finding is recognized by its `self-deploy-` decision ID and reuses the
// dispatch_failure error tier rather than being mislabeled as a queue failure.
func TestFleetSelfDeployFailureOperatorState(t *testing.T) {
	now := time.Now().UTC()

	op := buildFleetProjectOperatorState(fleetProjectState{
		Name: "Dogfood",
		Repo: "befeast/maestro",
		Supervisor: supervisorInfo{Latest: &supervisorDecisionInfo{
			ID:                "self-deploy-20260612T100000.000000000Z",
			CreatedAt:         now,
			Status:            "failed",
			Summary:           "self-deploy: failed after PR #710 — staging /usr/local/bin/maestro.next failed",
			RecommendedAction: "verify the maestro units and binary by hand, then redeploy manually",
		}},
	})

	if op.Kind != "dispatch_failure" {
		t.Errorf("Kind = %q, want dispatch_failure (error tier)", op.Kind)
	}
	if op.Tone != "error" {
		t.Errorf("Tone = %q, want error", op.Tone)
	}
	if op.Label != "Self-deploy failed" {
		t.Errorf("Label = %q, want \"Self-deploy failed\"", op.Label)
	}
	if op.Summary == "" || op.NextAction == "" {
		t.Errorf("Summary/NextAction must be populated: %+v", op)
	}
	if !fleetOperatorKindNeedsAction(op.Kind) {
		t.Error("self-deploy failure must be an operator-action kind")
	}
	if got := fleetNextActionCTAForProject(op.Kind, op); got != "Redeploy maestro binary" {
		t.Errorf("CTA = %q, want \"Redeploy maestro binary\"", got)
	}
}

// A rolled-back self-deploy (Status "failed", carrying the rollback reason)
// is surfaced the same way — the binary is safe but the merge did not deploy.
func TestFleetSelfDeployRolledBackOperatorState(t *testing.T) {
	now := time.Now().UTC()

	op := buildFleetProjectOperatorState(fleetProjectState{
		Name: "Dogfood",
		Repo: "befeast/maestro",
		Supervisor: supervisorInfo{Latest: &supervisorDecisionInfo{
			ID:                "self-deploy-20260612T110000.000000000Z",
			CreatedAt:         now,
			Status:            "failed",
			Summary:           "self-deploy: rolled back to previous binary after PR #710 — health check timed out",
			RecommendedAction: "inspect the failed deploy, fix the regression, re-merge",
		}},
	})

	if op.Tone != "error" || op.Label != "Self-deploy failed" {
		t.Fatalf("rolled-back self-deploy operator state = %+v, want error/\"Self-deploy failed\"", op)
	}
}

// A normal (non self-deploy) failed supervisor decision keeps the generic
// dispatch-failure copy — the #711 special-case is scoped to self-deploy IDs.
func TestFleetDispatchFailureUnchangedForNonSelfDeploy(t *testing.T) {
	now := time.Now().UTC()

	op := buildFleetProjectOperatorState(fleetProjectState{
		Name: "Other",
		Repo: "owner/other",
		Supervisor: supervisorInfo{Latest: &supervisorDecisionInfo{
			ID:        "supervisor-20260612T120000.000000000Z",
			CreatedAt: now,
			Status:    "failed",
			Summary:   "Supervisor queue action failed for issue #9.",
		}},
	})

	if op.Label != "Dispatch failure" {
		t.Errorf("Label = %q, want \"Dispatch failure\" for a non self-deploy finding", op.Label)
	}
}
