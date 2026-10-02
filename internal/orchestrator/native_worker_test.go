package orchestrator

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/befeast/maestro/internal/aiexecution"
	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/github"
	"github.com/befeast/maestro/internal/notify"
	"github.com/befeast/maestro/internal/pipeline"
	"github.com/befeast/maestro/internal/state"
	"github.com/befeast/maestro/internal/worker"
)

func TestNativeWorkerLaunchContextUsesPerCallConfig(t *testing.T) {
	cfg := &config.Config{WorkerNativeSessionRegistration: &config.NativeSessionRegistrationConfig{}}
	planner := nativeWorkerConfig(cfg, "planner", "")
	repair := nativeWorkerConfig(cfg, "repair", "parent")
	if cfg.WorkerLaunchContext != nil || planner == repair || planner == cfg || planner.WorkerLaunchContext.Role != "planner" || repair.WorkerLaunchContext.Role != "repair" || repair.WorkerLaunchContext.ParentRoleRunID != "parent" {
		t.Fatal("shared launch context was mutated")
	}
}

func TestNativeWorkerRetryHoldPreservesGenerationBudgetAndFeedback(t *testing.T) {
	cfg := &config.Config{Repo: "fixture/repo", StateDir: t.TempDir(), MaxRetryBackoffMs: 300000, MaxRuntimeMinutes: 999, WorkerNativeSessionRegistration: &config.NativeSessionRegistrationConfig{}}
	calls := 0
	o := &Orchestrator{cfg: cfg, notifier: &notify.Notifier{}, promptBase: "fixture task", getIssueFn: func(n int) (github.Issue, error) { return makeIssue(n, "fixture issue"), nil },
		respawnWorkerFn: func(_ *config.Config, _ string, _ *state.Session, _ string, _ github.Issue, _ string, _ string) error {
			calls++
			return &worker.NativeRegistrationHold{Code: "authority_unavailable"}
		}}
	past := time.Now().UTC().Add(-time.Second)
	s := state.NewState()
	sess := &state.Session{IssueNumber: 1207, Status: state.StatusDead, RetryCount: 2, NextRetryAt: &past, WorkerGeneration: 4, Branch: "fixture", CIFailureOutput: "failure detail", PreviousAttemptFeedback: "review detail", PreviousAttemptFeedbackKind: "review"}
	s.Sessions["fixture-1"] = sess
	o.respawnDueRetries(s, 10)
	if calls != 1 || sess.NativeRegistrationHold != "authority_unavailable" || sess.RetryCount != 2 || sess.WorkerGeneration != 4 || sess.NextRetryAt == nil || !sess.NextRetryAt.Equal(past) || sess.CIFailureOutput != "failure detail" || sess.PreviousAttemptFeedback != "review detail" || sess.Status != state.StatusDead {
		t.Fatalf("hold consumed retry: %+v", sess)
	}
	o.respawnDueRetries(s, 10)
	if calls != 1 || !s.IssueInProgress(1207) {
		t.Fatal("held generation retried or lost issue claim")
	}
}

func TestNativeWorkerPhaseHoldPreservesPriorPhaseAndCounters(t *testing.T) {
	cfg := pipelineConfig()
	cfg.StateDir = t.TempDir()
	cfg.WorkerNativeSessionRegistration = &config.NativeSessionRegistrationConfig{}
	o := pipelineOrchestrator(cfg)
	o.getIssueFn = func(n int) (github.Issue, error) { return makeIssue(n, "fixture issue"), nil }
	dir := t.TempDir()
	for _, name := range []string{pipeline.PlanFile, pipeline.ValidationFile} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("fixture"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	calls := 0
	o.workerStartPhaseFn = func(_ *config.Config, s *state.Session, _ string, _ string, _ string) error {
		calls++
		if s.Phase != state.PhaseImplement {
			t.Fatal("wrong trusted role")
		}
		return &worker.NativeRegistrationHold{Code: "registration_conflict"}
	}
	sess := &state.Session{IssueNumber: 1207, Status: state.StatusRunning, Phase: state.PhasePlan, PlanVersion: 1, WorkerGeneration: 3, Worktree: dir}
	if !o.advancePipeline(state.NewState(), "fixture-1", sess) {
		t.Fatal("not handled")
	}
	if sess.Phase != state.PhasePlan || sess.PlanVersion != 1 || sess.WorkerGeneration != 3 || sess.NativeRegistrationHold != "registration_conflict" {
		t.Fatalf("phase hold consumed generation: %+v", sess)
	}
	o.advancePipeline(state.NewState(), "fixture-1", sess)
	if calls != 1 {
		t.Fatal("hold repeated phase")
	}
}

func TestNativeWorkerRepairUsesTrustedCopiedRoleAndRetainsApprovalOnHold(t *testing.T) {
	const head = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	cfg := cfgWithBackends("claude", "claude")
	cfg.WorkerNativeSessionRegistration = &config.NativeSessionRegistrationConfig{}
	o, fresh, _ := newStartWorkersOrchestrator(cfg, []github.Issue{makeIssue(7, "repair exact PR", "maestro-ready")})
	o.hasOpenPRForIssueFn = func(int) (bool, error) { return true, nil }
	o.ghPRHeadSHAFn = func(int) (string, error) { return head, nil }
	o.ghPRCheckRollupFn = func(int) (github.PRCheckRollup, error) {
		return github.PRCheckRollup{HeadSHA: head, Verdict: "failure", Complete: true}, nil
	}
	o.ghPRMergeStatusFn = func(int) (string, string, error) { return "MERGEABLE", "clean", nil }
	o.ghPRReviewGateVerdictFn = func(int, []string) (github.ReviewGateVerdict, error) {
		return github.ReviewGateVerdict{Passed: true}, nil
	}
	calls := 0
	o.respawnInPlaceFn = func(c *config.Config, _ string, s *state.Session, _ string, _ github.Issue, _, _ string) error {
		calls++
		if c == cfg || c.WorkerLaunchContext == nil || c.WorkerLaunchContext.Role != "repair" || c.WorkerLaunchContext.ParentRoleRunID != s.NativeRoleRunID {
			t.Fatal("repair lacked isolated trusted role")
		}
		return &worker.NativeRegistrationHold{Code: "unresolved_launch", LaunchUncertain: true}
	}
	st := repairGateTestState(time.Now().UTC(), head)
	st.Sessions["slot-7"].NativeRoleRunID = "00000000-0000-4000-8000-000000000001"
	o.startNewWorkers(st, 1)
	if calls != 1 || len(*fresh) != 0 || st.Sessions["slot-7"].NativeRegistrationHold != "unresolved_launch" || cfg.WorkerLaunchContext != nil {
		t.Fatal("repair hold lost identity")
	}
	if got := approvalStatus(t, st, "repair-7"); got != state.ApprovalStatusAwaitingDispatch {
		t.Fatalf("held repair consumed approval=%s", got)
	}
	o.startNewWorkers(st, 1)
	if calls != 1 {
		t.Fatal("held repair automatically retried")
	}
}

func TestNativeRuntimeReconcileHoldRoutesPrelaunchWedge(t *testing.T) {
	for code, want := range map[string]bool{
		"unresolved_launch":               true,
		"native_process_identity_missing": true,
		"previous_outcome_unknown":        true,
		"native_generation_sealed":        false,
		"setup_failed":                    false,
		"":                                false,
	} {
		if got := nativeRuntimeReconcileHold(code); got != want {
			t.Fatalf("nativeRuntimeReconcileHold(%q)=%v want %v", code, got, want)
		}
	}
}

// nativeHoldRoutingState holds one slot per routing shape at cycle start.
func nativeHoldRoutingState() *state.State {
	s := state.NewState()
	s.Sessions["fixture-1"] = &state.Session{IssueNumber: 21, Status: state.StatusFailed, NativeRegistrationHold: "binding_inventory_incomplete", Worktree: "/worktrees/fixture-1", Branch: "feat/fixture-1-21"}
	s.Sessions["fixture-2"] = &state.Session{IssueNumber: 22, Status: state.StatusDead, NativeRegistrationHold: "native_process_identity_missing", WorkerGeneration: 3, NativeRoleRunID: "00000000-0000-4000-8000-000000000003"}
	s.Sessions["fixture-3"] = &state.Session{IssueNumber: 23, Status: state.StatusFailed, NativeRegistrationHold: "unresolved_launch"}
	s.Sessions["fixture-4"] = &state.Session{IssueNumber: 24, Status: state.StatusRunning}
	s.Sessions["fixture-5"] = &state.Session{IssueNumber: 25, Status: state.StatusFailed, NativeRegistrationHold: "setup_failed", ReleasedForRedispatch: true}
	s.Sessions["fixture-6"] = &state.Session{IssueNumber: 26, Status: state.StatusPROpen, NativeRegistrationHold: "setup_failed", WorkerGeneration: 2, NativeRoleRunID: "00000000-0000-4000-8000-000000000006"}
	return s
}

func nativeHoldRoutingOrchestrator(t *testing.T, runtime, expiry map[string]int) *Orchestrator {
	t.Helper()
	cfg := &config.Config{StateDir: t.TempDir(), AIExecution: aiexecution.Policy{RequireVerifiedRoute: true}}
	return &Orchestrator{cfg: cfg, notifier: &notify.Notifier{},
		nativeRuntimeReconcileFn: func(_ *config.Config, _ *state.State, slot string) error {
			runtime[slot]++
			return &worker.NativeRegistrationHold{Code: "native_launch_abandonment_unproven"}
		},
		nativePrelaunchExpiryFn: func(_ *config.Config, s *state.State, slot string) (bool, error) {
			expiry[slot]++
			sess := s.Sessions[slot]
			sess.NativeRegistrationHold = ""
			sess.ReleasedForRedispatch = true
			sess.WorkerOutcome = state.WorkerOutcomeNativePrelaunchAbandoned
			return true, nil
		}}
}

// Each held slot reaches exactly one reconciler per cycle: wedge/launch-gap
// holds the exact runtime reconciliation, a failed first-generation hold the
// expired pre-launch reconciliation; everything else is left alone. A released
// slot is not routed again on the next cycle (idempotent).
func TestNativeHoldsAtCycleStartRouteEachSlotOnce(t *testing.T) {
	runtime, expiry := map[string]int{}, map[string]int{}
	o := nativeHoldRoutingOrchestrator(t, runtime, expiry)
	s := nativeHoldRoutingState()
	if !s.IssueHasNonFreshClaim(21) {
		t.Fatal("fixture: the held failed slot must claim its issue")
	}
	o.reconcileNativeHoldsAtCycleStart(s)
	wantRuntime := map[string]int{"fixture-2": 1, "fixture-3": 1}
	wantExpiry := map[string]int{"fixture-1": 1}
	if len(runtime) != len(wantRuntime) || runtime["fixture-2"] != 1 || runtime["fixture-3"] != 1 {
		t.Fatalf("runtime routing=%v want %v", runtime, wantRuntime)
	}
	if len(expiry) != len(wantExpiry) || expiry["fixture-1"] != 1 {
		t.Fatalf("expiry routing=%v want %v", expiry, wantExpiry)
	}
	if s.IssueHasNonFreshClaim(21) {
		t.Fatal("released slot still claims its issue")
	}
	o.reconcileNativeHoldsAtCycleStart(s)
	if expiry["fixture-1"] != 1 || len(expiry) != 1 || runtime["fixture-2"] != 2 || runtime["fixture-3"] != 2 {
		t.Fatalf("second cycle: runtime=%v expiry=%v", runtime, expiry)
	}
}

// Releasing a held issue re-enables dispatch, so it waits while the project
// is paused, drained or emergency-stopped; runtime reconciliation of wedge
// holds (which never releases an issue) still runs.
func TestNativeHoldsAtCycleStartDeferExpiredPrelaunchWhileStopped(t *testing.T) {
	for _, mode := range []string{"paused", "drained", "emergency"} {
		t.Run(mode, func(t *testing.T) {
			runtime, expiry := map[string]int{}, map[string]int{}
			o := nativeHoldRoutingOrchestrator(t, runtime, expiry)
			s := nativeHoldRoutingState()
			switch mode {
			case "paused":
				s.Paused = true
			case "drained":
				s.SpawnDrain = true
			case "emergency":
				o.SetEmergencyHalt(func() bool { return true })
			}
			o.reconcileNativeHoldsAtCycleStart(s)
			if len(expiry) != 0 {
				t.Fatalf("expired prelaunch reconciled while %s: %v", mode, expiry)
			}
			if sess := s.Sessions["fixture-1"]; sess.NativeRegistrationHold != "binding_inventory_incomplete" || sess.ReleasedForRedispatch || !s.IssueHasNonFreshClaim(21) {
				t.Fatalf("held slot changed while %s: %+v", mode, sess)
			}
			if runtime["fixture-2"] != 1 || runtime["fixture-3"] != 1 {
				t.Fatalf("runtime reconciliation skipped while %s: %v", mode, runtime)
			}
		})
	}
}

func TestNativeHoldsAtCycleStartRequireVerifiedRoute(t *testing.T) {
	runtime, expiry := map[string]int{}, map[string]int{}
	o := nativeHoldRoutingOrchestrator(t, runtime, expiry)
	o.cfg.AIExecution = aiexecution.Policy{}
	o.reconcileNativeHoldsAtCycleStart(nativeHoldRoutingState())
	if len(runtime) != 0 || len(expiry) != 0 {
		t.Fatalf("reconciled without the verified route: runtime=%v expiry=%v", runtime, expiry)
	}
}

// A typed hold from the expired pre-launch reconciliation keeps the slot held
// and claimed; the next cycle tries again.
func TestNativeHoldsAtCycleStartKeepHoldOnExpiredPrelaunchFailure(t *testing.T) {
	runtime, expiry := map[string]int{}, map[string]int{}
	o := nativeHoldRoutingOrchestrator(t, runtime, expiry)
	o.nativePrelaunchExpiryFn = func(_ *config.Config, _ *state.State, slot string) (bool, error) {
		expiry[slot]++
		return false, &worker.NativeRegistrationHold{Code: "outcome_authority_unavailable", LaunchUncertain: true}
	}
	s := nativeHoldRoutingState()
	o.reconcileNativeHoldsAtCycleStart(s)
	o.reconcileNativeHoldsAtCycleStart(s)
	if expiry["fixture-1"] != 2 || s.Sessions["fixture-1"].NativeRegistrationHold != "binding_inventory_incomplete" || !s.IssueHasNonFreshClaim(21) {
		t.Fatalf("expiry=%v session=%+v", expiry, s.Sessions["fixture-1"])
	}
}
