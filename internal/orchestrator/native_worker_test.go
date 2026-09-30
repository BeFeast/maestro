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
