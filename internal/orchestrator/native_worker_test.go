package orchestrator

import (
	"os"
	"path/filepath"
	"testing"
	"time"

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
