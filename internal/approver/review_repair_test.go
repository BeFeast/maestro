package approver

import (
	"strings"
	"testing"

	"github.com/befeast/maestro/internal/state"
)

// #565: spawn_review_repair must NOT fall into the unknown-action
// default. It is awaiting_dispatch (the orchestrator dispatcher owns
// the actual side effect) with an actionable summary so the operator
// sees who is going to do what next.
func TestExecute_SpawnReviewRepair_ReturnsAwaitingDispatch(t *testing.T) {
	ex := &Executor{Cfg: newCfg()}
	a := mkApproval("spawn_review_repair", &state.SupervisorTarget{Issue: 442, PR: 564, HeadSHA: "deadbeef"}, "fix P1 review feedback", "")

	res := ex.Execute(a)
	if res.Status != state.ApprovalStatusAwaitingDispatch {
		t.Fatalf("status = %q, want %q (#565)", res.Status, state.ApprovalStatusAwaitingDispatch)
	}
	if !strings.Contains(res.Summary, "PR #564") {
		t.Fatalf("summary = %q, want PR #564 reference", res.Summary)
	}
	if !strings.Contains(res.Summary, "#442") {
		t.Fatalf("summary = %q, want issue #442 reference", res.Summary)
	}
	if !strings.Contains(strings.ToLower(res.Summary), "dispatcher") {
		t.Fatalf("summary = %q, want dispatcher mention so operator knows next step", res.Summary)
	}
	if res.Err != nil {
		t.Fatalf("err = %v, want nil for awaiting-dispatch", res.Err)
	}
}

// #662: spawn_repair_worker is now a registry verb so cautious projects
// can mint the approval and the orchestrator's dispatcher loop can
// actually start the repair worker. Before #662 the supervisor emitted
// this verb from the deterministic path (open PR not progressing,
// retry-exhausted with no usable PR) but the executor had no case, so
// the at-mint guard refused the approval every cycle and the issue
// stalled silently. Same dispatcher shape as spawn_worker.
func TestExecute_SpawnRepairWorker_ReturnsAwaitingDispatch(t *testing.T) {
	ex := &Executor{Cfg: newCfg()}
	a := mkApproval("spawn_repair_worker", &state.SupervisorTarget{Issue: 442, PR: 564}, "repair worker", "")

	res := ex.Execute(a)
	if res.Status != state.ApprovalStatusAwaitingDispatch {
		t.Fatalf("status = %q, want %q (#662)", res.Status, state.ApprovalStatusAwaitingDispatch)
	}
	if !strings.Contains(res.Summary, "#442") {
		t.Fatalf("summary = %q, want issue #442 reference", res.Summary)
	}
	if !strings.Contains(res.Summary, "#564") {
		t.Fatalf("summary = %q, want PR #564 reference", res.Summary)
	}
	if !strings.Contains(strings.ToLower(res.Summary), "dispatcher") {
		t.Fatalf("summary = %q, want dispatcher mention so operator knows next step", res.Summary)
	}
	if res.Err != nil {
		t.Fatalf("err = %v, want nil for awaiting-dispatch", res.Err)
	}
}
