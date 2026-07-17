package orchestrator

import (
	"testing"
	"time"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/github"
	"github.com/befeast/maestro/internal/state"
)

func repairGateTestState(now time.Time, head string) *state.State {
	s := state.NewState()
	s.Sessions["slot-7"] = &state.Session{
		IssueNumber: 7,
		IssueTitle:  "repair exact PR",
		Status:      state.StatusDead,
		PRNumber:    70,
		Worktree:    "/work/slot-7",
		Branch:      "feat/slot-7-7-repair",
		Backend:     "codex",
	}
	approval := repairApproval("repair-7", 7, 70, state.ApprovalStatusAwaitingDispatch, now)
	approval.Target.Session = "slot-7"
	approval.Target.HeadSHA = head
	s.Approvals = []state.Approval{approval}
	return s
}

func TestSpawnRepairDispatch_CurrentPendingHeadStalesApprovalWithoutRespawn(t *testing.T) {
	const head = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	cfg := cfgWithBackends("codex", "codex")
	o, freshStarts, _ := newStartWorkersOrchestrator(cfg, []github.Issue{makeIssue(7, "repair exact PR", "maestro-ready")})
	o.hasOpenPRForIssueFn = func(int) (bool, error) { return true, nil }
	o.ghPRHeadSHAFn = func(int) (string, error) { return head, nil }
	o.ghPRCheckRollupFn = func(int) (github.PRCheckRollup, error) {
		return github.PRCheckRollup{HeadSHA: head, Verdict: "pending", Complete: true}, nil
	}
	o.ghPRMergeStatusFn = func(int) (string, string, error) { return "MERGEABLE", "unstable", nil }
	o.ghPRReviewGateVerdictFn = func(int, []string) (github.ReviewGateVerdict, error) {
		return github.ReviewGateVerdict{Pending: true, Streams: []github.ReviewStreamVerdict{{Name: "greptile", Pending: true}}}, nil
	}
	respawns := 0
	o.respawnInPlaceFn = func(*config.Config, string, *state.Session, string, github.Issue, string, string) error {
		respawns++
		return nil
	}

	s := repairGateTestState(time.Now().UTC(), head)
	o.startNewWorkers(s, 1)

	if respawns != 0 || len(*freshStarts) != 0 {
		t.Fatalf("repair dispatched for pending current head: respawns=%d fresh=%v", respawns, *freshStarts)
	}
	if got := approvalStatus(t, s, "repair-7"); got != state.ApprovalStatusStale {
		t.Fatalf("repair approval = %q, want stale", got)
	}
}

func TestSpawnRepairDispatch_HeadMoveStalesBoundApprovalBeforeReadingOldGates(t *testing.T) {
	const oldHead = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const newHead = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	cfg := cfgWithBackends("codex", "codex")
	o, freshStarts, _ := newStartWorkersOrchestrator(cfg, []github.Issue{makeIssue(7, "repair exact PR", "maestro-ready")})
	o.hasOpenPRForIssueFn = func(int) (bool, error) { return true, nil }
	o.ghPRHeadSHAFn = func(int) (string, error) { return newHead, nil }
	checkReads := 0
	o.ghPRCheckRollupFn = func(int) (github.PRCheckRollup, error) {
		checkReads++
		return github.PRCheckRollup{HeadSHA: oldHead, Verdict: "failure", Complete: true}, nil
	}

	s := repairGateTestState(time.Now().UTC(), oldHead)
	o.startNewWorkers(s, 1)

	if checkReads != 0 || len(*freshStarts) != 0 {
		t.Fatalf("stale-head repair read old gates or dispatched: checkReads=%d fresh=%v", checkReads, *freshStarts)
	}
	if got := approvalStatus(t, s, "repair-7"); got != state.ApprovalStatusStale {
		t.Fatalf("repair approval = %q, want stale", got)
	}
}

func TestSpawnRepairDispatch_CurrentReviewFindingAllowsExactInPlaceRepair(t *testing.T) {
	const head = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	cfg := cfgWithBackends("codex", "codex")
	o, freshStarts, _ := newStartWorkersOrchestrator(cfg, []github.Issue{makeIssue(7, "repair exact PR", "maestro-ready")})
	o.hasOpenPRForIssueFn = func(int) (bool, error) { return true, nil }
	o.ghPRHeadSHAFn = func(int) (string, error) { return head, nil }
	o.ghPRCheckRollupFn = func(int) (github.PRCheckRollup, error) {
		return github.PRCheckRollup{HeadSHA: head, Verdict: "success", Complete: true}, nil
	}
	o.ghPRMergeStatusFn = func(int) (string, string, error) { return "MERGEABLE", "clean", nil }
	o.ghPRReviewGateVerdictFn = func(int, []string) (github.ReviewGateVerdict, error) {
		return github.ReviewGateVerdict{
			Passed: false,
			Streams: []github.ReviewStreamVerdict{{
				Name: "greptile", Passed: false,
				Findings: []github.ReviewComment{{Path: "main.go", Line: 7, Body: "correctness", User: "greptile"}},
			}},
		}, nil
	}
	respawns := 0
	o.respawnInPlaceFn = func(_ *config.Config, slot string, sess *state.Session, _ string, _ github.Issue, _, _ string) error {
		if slot != "slot-7" {
			t.Fatalf("respawn slot = %q, want slot-7", slot)
		}
		respawns++
		sess.Status = state.StatusRunning
		return nil
	}

	s := repairGateTestState(time.Now().UTC(), head)
	o.startNewWorkers(s, 1)

	if respawns != 1 || len(*freshStarts) != 0 || len(s.Sessions) != 1 {
		t.Fatalf("exact review repair mismatch: respawns=%d fresh=%v sessions=%d", respawns, *freshStarts, len(s.Sessions))
	}
	if got := approvalStatus(t, s, "repair-7"); got != state.ApprovalStatusSuperseded {
		t.Fatalf("repair approval = %q, want consumed/superseded", got)
	}
}
