package supervisor

import (
	"testing"
	"time"

	"github.com/befeast/maestro/internal/approver"
	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/github"
	"github.com/befeast/maestro/internal/state"
)

// #736: every action the supervisor's decision layer can EMIT and drive to a
// real side effect must be resolvable by RunOnce — EITHER applied directly as
// an operator-whitelisted safe queue mutation OR mintable as an approval the
// executor can run (approver.KnownApprovalActions). An action that is neither
// rots silently: the at-mint guard refuses it every cycle and nothing applies
// it. label_issue_ready regressed exactly here (cautious+LLM dynamic-wave
// handoff: the safe mutation was dropped after the LLM raised the risk, and
// the verb was missing from the registry so the mint was refused). This test
// catches that drift class at CI time.
//
// Informational / operator-attention recommendations (none, the wait_*
// family, monitor_open_pr, check_outcome_health, notify_red,
// review_retry_exhausted, preflight_failed) are intentionally excluded: they
// request human attention and are never executed by RunOnce.
func TestSupervisorActions_ResolveToSafeMutationOrRegistry(t *testing.T) {
	// Actions that drive a real side effect when emitted.
	sideEffectActions := []string{
		ActionSpawnWorker,
		ActionSpawnRepairWorker,
		ActionSpawnReviewRepair,
		ActionMergePR,
		ActionOpenChildIssue,
		ActionLabelIssueReady,
		ActionUnblockIssue,
		config.SupervisorActionCloseIssue,
		config.SupervisorActionCloseIssueBatch,
	}
	// Actions whose side effect is a safe queue mutation: RunOnce applies
	// their planned label mutations directly (applyOrMintDecision) when the
	// operator whitelists the corresponding safe action, so the "OR" arm is
	// satisfied even when the verb is not in the approval registry.
	appliedAsSafeMutation := map[string]bool{
		ActionLabelIssueReady: true, // add_ready_label / remove_ready_label
		ActionUnblockIssue:    true, // remove_blocked_label + add_ready_label
	}
	for _, action := range sideEffectActions {
		action := action
		t.Run(action, func(t *testing.T) {
			if appliedAsSafeMutation[action] || approver.IsKnownApprovalAction(action) {
				return
			}
			t.Fatalf("supervisor emits side-effecting %q but it is neither applied as a safe mutation nor in approver.KnownApprovalActions = %v — RunOnce can never resolve it; the at-mint guard would refuse it every cycle and the project would stall silently (#736)", action, approver.KnownApprovalActionList())
		})
	}
}

// #662: every mutating action constant the supervisor's decision layer can
// emit MUST be in the executor registry (approver.KnownApprovalActions).
// Otherwise the at-mint guard refuses the verb every cycle on a cautious
// project and the issue stalls silently — the bug this PR fixes for
// spawn_repair_worker.
//
// The list below is the explicit set of mutating/approval-gated action
// constants exported from this package. Add to this list any new
// ActionXxx constant that the decision layer routes to RiskMutating or
// RiskApprovalGated; the test then fails until the executor learns the
// verb (executor.go switch + registry.go KnownApprovalActions).
func TestSupervisorMutatingActions_AreInExecutorRegistry(t *testing.T) {
	mutatingActions := []string{
		ActionSpawnWorker,
		ActionSpawnRepairWorker,
		ActionSpawnReviewRepair,
		ActionMergePR,
		ActionOpenChildIssue,
		// #736: label_issue_ready is normally applied directly as the
		// operator-whitelisted add_ready_label safe mutation, but when the
		// operator gates it behind approval_required the supervisor mints an
		// approval instead — which the executor must be able to run. Missing
		// here is exactly how the cautious+LLM dynamic-wave handoff stalled
		// silently ("refusing to mint approval: action label_issue_ready").
		ActionLabelIssueReady,
	}
	for _, action := range mutatingActions {
		action := action
		t.Run(action, func(t *testing.T) {
			if !approver.IsKnownApprovalAction(action) {
				t.Fatalf("supervisor emits %q but it is NOT in approver.KnownApprovalActions = %v — at-mint guard would refuse every cycle (#662)", action, approver.KnownApprovalActionList())
			}
		})
	}
}

// #662 regression: a dead session with a draft + failing PR triggers the
// deterministic spawn_repair_worker decision. The resulting action MUST
// be registry-supported so the at-mint guard does not refuse the
// approval on cautious projects. Without this pin we re-introduce the
// silent stall (live evidence in #662: "refusing to mint approval:
// action \"spawn_repair_worker\" is not in the executor registry").
func TestDecide_SpawnRepairWorkerDecision_IsRegistrySupported(t *testing.T) {
	cfg := testConfig(t)
	reader := &fakeReader{
		prs: []github.PR{{
			Number:      7,
			HeadRefName: "feat/checks",
			State:       "OPEN",
			Mergeable:   "MERGEABLE",
			IsDraft:     true,
		}},
		ciStatuses: map[int]string{7: "failure"},
	}
	st := state.NewState()
	st.Sessions["slot-1"] = &state.Session{
		IssueNumber: 1,
		IssueTitle:  "spawn repair regression",
		Status:      state.StatusDead,
		PRNumber:    7,
		Branch:      "feat/checks",
		StartedAt:   time.Now().UTC().Add(-time.Hour),
	}

	decision, err := testEngine(cfg, reader).Decide(st)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if decision.RecommendedAction != ActionSpawnRepairWorker {
		t.Fatalf("action = %q, want %q (deterministic open-PR-not-progressing path)", decision.RecommendedAction, ActionSpawnRepairWorker)
	}
	if !approver.IsKnownApprovalAction(decision.RecommendedAction) {
		t.Fatalf("decision.RecommendedAction = %q is not in approver.KnownApprovalActions = %v — at-mint guard would refuse and the project would stall silently (#662)", decision.RecommendedAction, approver.KnownApprovalActionList())
	}
}

// #662 regression: a retry-exhausted ready-labeled issue with no usable
// PR triggers the other deterministic spawn_repair_worker emission
// site. Same registry contract applies.
func TestDecide_RetryExhaustedRepairCandidate_IsRegistrySupported(t *testing.T) {
	cfg := testConfig(t)
	cfg.IssueLabels = []string{"maestro-ready"}
	enableDynamicWave(cfg)
	reader := &fakeReader{issues: []github.Issue{
		withProjectStatus(testIssue(808, "registry drift regression", "maestro-ready"), "Blocked"),
	}}
	st := state.NewState()
	st.Sessions["pan-72"] = &state.Session{
		IssueNumber: 808,
		IssueTitle:  "registry drift regression",
		Status:      state.StatusRetryExhausted,
		StartedAt:   time.Now().UTC().Add(-time.Hour),
	}

	decision, err := testEngine(cfg, reader).Decide(st)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if decision.RecommendedAction != ActionSpawnRepairWorker {
		t.Fatalf("action = %q, want %q", decision.RecommendedAction, ActionSpawnRepairWorker)
	}
	if !approver.IsKnownApprovalAction(decision.RecommendedAction) {
		t.Fatalf("decision.RecommendedAction = %q is not in approver.KnownApprovalActions = %v — at-mint guard would refuse and the project would stall silently (#662)", decision.RecommendedAction, approver.KnownApprovalActionList())
	}
}
