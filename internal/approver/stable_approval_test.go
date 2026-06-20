package approver

import (
	"fmt"
	"testing"
	"time"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/state"
)

// driveCycles re-emits an unchanged approval-gated decision over n supervise
// cycles, churning the bound session's runtime snapshot between cycles the way
// a retry-exhausted worker's NextRetryAt / RetryCount move in production. It
// returns the (stable) approval id and fails the test if it ever churns.
func driveCycles(t *testing.T, st *state.State, slot string, decision state.SupervisorDecision, now time.Time, n int) string {
	t.Helper()
	var id string
	for cycle := 0; cycle < n; cycle++ {
		at := now.Add(time.Duration(cycle) * time.Minute)
		st.MarkStaleApprovals(at) // cycle top, before re-emit
		d := decision
		d.ID = fmt.Sprintf("sup-cycle-%d", cycle)
		d.CreatedAt = at
		a := st.RecordPendingApprovalForDecision(d, at)
		if a == nil {
			t.Fatalf("cycle %d: approval was nil", cycle)
		}
		if cycle == 0 {
			id = a.ID
		} else if a.ID != id {
			t.Fatalf("cycle %d: approval id churned %q -> %q", cycle, id, a.ID)
		}
		if sess := st.Sessions[slot]; sess != nil {
			next := at.Add(time.Hour)
			sess.NextRetryAt = &next
			sess.RetryCount = cycle + 1
		}
	}
	if len(st.Approvals) != 1 {
		t.Fatalf("len(approvals) = %d, want 1 (no churn siblings)", len(st.Approvals))
	}
	return id
}

// TestStableApproval_MergePR_AcrossCyclesExecutesOnce covers #750 criteria
// 3/4/5 for the merge_pr verb: an unchanged decision keeps a stable, approvable
// id across supervise cycles, and approving once executes the merge exactly
// once.
func TestStableApproval_MergePR_AcrossCyclesExecutesOnce(t *testing.T) {
	now := time.Date(2026, 6, 20, 9, 0, 0, 0, time.UTC)
	st := state.NewState()
	st.Sessions["slot-1"] = &state.Session{IssueNumber: 700, PRNumber: 748, Status: state.StatusRetryExhausted}

	decision := state.SupervisorDecision{
		Project:           "owner/repo",
		Repo:              "owner/repo",
		Summary:           "Merge PR #748.",
		RecommendedAction: config.SupervisorActionMergePR,
		Target:            &state.SupervisorTarget{PR: 748, Issue: 700},
		Risk:              "mutating",
	}

	id := driveCycles(t, st, "slot-1", decision, now, 3)

	approved, err := st.ApproveApproval(id, now.Add(10*time.Minute), "cli", "land it")
	if err != nil {
		t.Fatalf("ApproveApproval(stable id) after cycles: %v", err)
	}

	gh := &fakeGH{}
	ex := &Executor{GH: gh, Cfg: newCfg()}
	res := ex.Execute(approved)
	if res.Status != state.ApprovalStatusExecuted {
		t.Fatalf("execute status = %q (err=%v), want executed", res.Status, res.Err)
	}
	if got := len(gh.mergeCalls); got != 1 {
		t.Fatalf("MergePR called %d times, want exactly 1 (idempotent)", got)
	}
	if gh.mergeCalls[0] != 748 {
		t.Fatalf("MergePR(%d), want 748", gh.mergeCalls[0])
	}
}

// TestStableApproval_SpawnRepairWorker_AcrossCyclesApprovable covers #750
// criterion 5 for the spawn_repair_worker verb (the dogfood case that had to be
// hand-merged): an unchanged decision keeps a stable, approvable id across
// cycles, and approving routes to awaiting_dispatch so the dispatcher loop owns
// the respawn.
func TestStableApproval_SpawnRepairWorker_AcrossCyclesApprovable(t *testing.T) {
	now := time.Date(2026, 6, 20, 9, 0, 0, 0, time.UTC)
	st := state.NewState()
	st.Sessions["slot-2"] = &state.Session{IssueNumber: 442, PRNumber: 564, Status: state.StatusRetryExhausted}

	decision := state.SupervisorDecision{
		Project:           "owner/repo",
		Repo:              "owner/repo",
		Summary:           "Spawn a repair worker for issue #442 (PR #564).",
		RecommendedAction: "spawn_repair_worker",
		Target:            &state.SupervisorTarget{Issue: 442, PR: 564},
		Risk:              "mutating",
	}

	id := driveCycles(t, st, "slot-2", decision, now, 4)

	approved, err := st.ApproveApproval(id, now.Add(10*time.Minute), "cli", "unblock hand-off")
	if err != nil {
		t.Fatalf("ApproveApproval(stable id) after cycles: %v", err)
	}

	ex := &Executor{Cfg: newCfg()}
	res := ex.Execute(approved)
	if res.Status != state.ApprovalStatusAwaitingDispatch {
		t.Fatalf("execute status = %q (err=%v), want awaiting_dispatch", res.Status, res.Err)
	}
}

// TestStableApproval_DistinctDecisionsGetDistinctIDs pins criterion 4: a
// genuinely different decision (different target) is content-addressed to a
// different id and coexists with the first — superseding is reserved for real
// changes, not the same-decision churn.
func TestStableApproval_DistinctDecisionsGetDistinctIDs(t *testing.T) {
	now := time.Date(2026, 6, 20, 9, 0, 0, 0, time.UTC)
	st := state.NewState()

	mk := func(pr int) state.SupervisorDecision {
		return state.SupervisorDecision{
			ID:                fmt.Sprintf("sup-%d", pr),
			CreatedAt:         now,
			Project:           "owner/repo",
			Repo:              "owner/repo",
			Summary:           fmt.Sprintf("Merge PR #%d.", pr),
			RecommendedAction: config.SupervisorActionMergePR,
			Target:            &state.SupervisorTarget{PR: pr},
			Risk:              "mutating",
		}
	}

	a := st.RecordPendingApprovalForDecision(mk(748), now)
	b := st.RecordPendingApprovalForDecision(mk(749), now)
	if a.ID == b.ID {
		t.Fatalf("distinct PR targets shared an id %q", a.ID)
	}
	if len(st.Approvals) != 2 {
		t.Fatalf("len(approvals) = %d, want 2 (distinct decisions coexist)", len(st.Approvals))
	}
	for _, ap := range st.Approvals {
		if ap.Status != state.ApprovalStatusPending {
			t.Fatalf("approval %q status = %q, want pending (no supersede on distinct decisions)", ap.ID, ap.Status)
		}
	}
}
