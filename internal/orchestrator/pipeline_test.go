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
	"github.com/befeast/maestro/internal/router"
	"github.com/befeast/maestro/internal/state"
)

func pipelineOrchestrator(cfg *config.Config) *Orchestrator {
	return &Orchestrator{
		cfg:      cfg,
		notifier: notify.NewWithToken("", "", "", "http://localhost:0"),
		gh:       github.New(cfg.Repo),
		router:   router.New(cfg),
		repo:     cfg.Repo,
	}
}

func pipelineConfig() *config.Config {
	return &config.Config{
		Repo:      "owner/repo",
		StateDir:  os.TempDir(),
		LocalPath: os.TempDir(),
		Pipeline: config.PipelineConfig{
			Enabled:   true,
			Planner:   config.RoleConfig{Enabled: true},
			Validator: config.RoleConfig{Enabled: true},
		},
		MaxRuntimeMinutes: 120,
		Model: config.ModelConfig{
			Default:  "claude",
			Backends: map[string]config.BackendDef{"claude": {Cmd: "claude"}},
		},
	}
}

func TestAdvancePipeline_NonPipelineSession(t *testing.T) {
	cfg := pipelineConfig()
	o := pipelineOrchestrator(cfg)
	sess := &state.Session{Phase: state.PhaseNone}
	if o.advancePipeline("slot-1", sess) {
		t.Error("expected false for non-pipeline session")
	}
}

func TestAdvancePipeline_PlanComplete_NoArtifacts(t *testing.T) {
	cfg := pipelineConfig()
	o := pipelineOrchestrator(cfg)

	worktreeDir := t.TempDir()
	sess := &state.Session{
		IssueNumber: 1,
		Phase:       state.PhasePlan,
		Worktree:    worktreeDir,
		Status:      state.StatusRunning,
	}

	handled := o.advancePipeline("slot-1", sess)
	if !handled {
		t.Fatal("expected handled")
	}
	if sess.Status != state.StatusDead {
		t.Errorf("expected dead, got %s", sess.Status)
	}
}

func TestAdvancePipeline_PlanComplete_WithArtifacts(t *testing.T) {
	cfg := pipelineConfig()
	o := pipelineOrchestrator(cfg)

	worktreeDir := t.TempDir()
	os.WriteFile(filepath.Join(worktreeDir, pipeline.PlanFile), []byte("plan"), 0644)
	os.WriteFile(filepath.Join(worktreeDir, pipeline.ValidationFile), []byte("val"), 0644)

	sess := &state.Session{
		IssueNumber: 42,
		IssueTitle:  "Test issue",
		Phase:       state.PhasePlan,
		Worktree:    worktreeDir,
		Branch:      "feat/test",
		Status:      state.StatusRunning,
	}

	// Mock getIssue and startPhase
	o.getIssueFn = func(number int) (github.Issue, error) {
		return github.Issue{Number: number, Title: "Test issue"}, nil
	}
	startPhaseCalled := false
	o.workerStartPhaseFn = func(cfg *config.Config, s *state.Session, slotName, prompt, backendName string) error {
		startPhaseCalled = true
		s.Status = state.StatusRunning
		s.PID = 999
		return nil
	}

	handled := o.advancePipeline("slot-1", sess)
	if !handled {
		t.Fatal("expected handled")
	}
	if sess.Phase != state.PhaseImplement {
		t.Errorf("expected implement phase, got %s", sess.Phase)
	}
	if !startPhaseCalled {
		t.Error("expected startPhase to be called")
	}
}

func TestAdvancePipeline_ImplementComplete_ValidatorEnabled(t *testing.T) {
	cfg := pipelineConfig()
	o := pipelineOrchestrator(cfg)

	worktreeDir := t.TempDir()
	sess := &state.Session{
		IssueNumber: 42,
		IssueTitle:  "Test issue",
		Phase:       state.PhaseImplement,
		Worktree:    worktreeDir,
		Branch:      "feat/test",
		Status:      state.StatusRunning,
	}

	o.getIssueFn = func(number int) (github.Issue, error) {
		return github.Issue{Number: number, Title: "Test issue"}, nil
	}
	o.workerStartPhaseFn = func(cfg *config.Config, s *state.Session, slotName, prompt, backendName string) error {
		s.Status = state.StatusRunning
		s.PID = 999
		return nil
	}

	handled := o.advancePipeline("slot-1", sess)
	if !handled {
		t.Fatal("expected handled")
	}
	if sess.Phase != state.PhaseValidate {
		t.Errorf("expected validate phase, got %s", sess.Phase)
	}
}

func TestAdvancePipeline_ImplementComplete_ValidatorDisabled(t *testing.T) {
	cfg := pipelineConfig()
	cfg.Pipeline.Validator.Enabled = false
	o := pipelineOrchestrator(cfg)

	sess := &state.Session{
		Phase:  state.PhaseImplement,
		Status: state.StatusRunning,
	}

	handled := o.advancePipeline("slot-1", sess)
	if handled {
		t.Error("expected not handled (should fall through to normal dead-worker flow)")
	}
	if sess.Phase != state.PhaseNone {
		t.Errorf("expected PhaseNone, got %s", sess.Phase)
	}
}

func TestAdvancePipeline_ValidatePass(t *testing.T) {
	cfg := pipelineConfig()
	o := pipelineOrchestrator(cfg)

	worktreeDir := t.TempDir()
	os.WriteFile(filepath.Join(worktreeDir, pipeline.ValidationResultFile), []byte("PASS\nAll good"), 0644)

	sess := &state.Session{
		IssueNumber: 42,
		Phase:       state.PhaseValidate,
		Worktree:    worktreeDir,
		Status:      state.StatusRunning,
	}

	handled := o.advancePipeline("slot-1", sess)
	if handled {
		t.Error("expected not handled (should fall through for PR detection)")
	}
	if sess.Phase != state.PhaseNone {
		t.Errorf("expected PhaseNone, got %s", sess.Phase)
	}
}

func TestAdvancePipeline_ValidateFail_RetryImplementer(t *testing.T) {
	cfg := pipelineConfig()
	o := pipelineOrchestrator(cfg)

	worktreeDir := t.TempDir()
	os.WriteFile(filepath.Join(worktreeDir, pipeline.ValidationResultFile),
		[]byte("FAIL\nBuild doesn't compile"), 0644)

	sess := &state.Session{
		IssueNumber: 42,
		IssueTitle:  "Test issue",
		Phase:       state.PhaseValidate,
		Worktree:    worktreeDir,
		Branch:      "feat/test",
		Status:      state.StatusRunning,
	}

	o.getIssueFn = func(number int) (github.Issue, error) {
		return github.Issue{Number: number, Title: "Test issue"}, nil
	}
	o.workerStartPhaseFn = func(cfg *config.Config, s *state.Session, slotName, prompt, backendName string) error {
		s.Status = state.StatusRunning
		s.PID = 999
		return nil
	}

	handled := o.advancePipeline("slot-1", sess)
	if !handled {
		t.Fatal("expected handled")
	}
	if sess.Phase != state.PhaseImplement {
		t.Errorf("expected implement phase (retry), got %s", sess.Phase)
	}
	if sess.ValidationFails != 1 {
		t.Errorf("expected 1 validation fail, got %d", sess.ValidationFails)
	}
	if sess.ValidationFeedback != "Build doesn't compile" {
		t.Errorf("unexpected feedback: %q", sess.ValidationFeedback)
	}
}

func TestAdvancePipeline_ValidateFail_ExhaustedRetries(t *testing.T) {
	cfg := pipelineConfig()
	o := pipelineOrchestrator(cfg)

	worktreeDir := t.TempDir()
	os.WriteFile(filepath.Join(worktreeDir, pipeline.ValidationResultFile),
		[]byte("FAIL\nStill broken"), 0644)

	sess := &state.Session{
		IssueNumber:     42,
		Phase:           state.PhaseValidate,
		Worktree:        worktreeDir,
		Status:          state.StatusRunning,
		ValidationFails: 2, // Already failed twice, this will be the 3rd
	}

	handled := o.advancePipeline("slot-1", sess)
	if !handled {
		t.Fatal("expected handled")
	}
	if sess.Status != state.StatusFailed {
		t.Errorf("expected failed, got %s", sess.Status)
	}
	if sess.ValidationFails != 3 {
		t.Errorf("expected 3 validation fails, got %d", sess.ValidationFails)
	}
}

func TestCheckSessions_PipelinePhaseAdvance(t *testing.T) {
	cfg := pipelineConfig()
	stateDir := t.TempDir()
	cfg.StateDir = stateDir

	o := pipelineOrchestrator(cfg)

	worktreeDir := t.TempDir()
	os.WriteFile(filepath.Join(worktreeDir, pipeline.PlanFile), []byte("plan"), 0644)
	os.WriteFile(filepath.Join(worktreeDir, pipeline.ValidationFile), []byte("val"), 0644)

	s := state.NewState()
	now := time.Now().UTC()
	s.Sessions["slot-1"] = &state.Session{
		IssueNumber: 42,
		IssueTitle:  "Test",
		Phase:       state.PhasePlan,
		Worktree:    worktreeDir,
		Branch:      "feat/test",
		Status:      state.StatusRunning,
		PID:         99999, // dead PID
		StartedAt:   now,
	}

	// Mock PID as dead
	o.pidAliveFn = func(pid int) bool { return false }
	o.listOpenPRsFn = func() ([]github.PR, error) { return nil, nil }
	o.isIssueClosedFn = func(n int) (bool, error) { return false, nil }
	o.getIssueFn = func(number int) (github.Issue, error) {
		return github.Issue{Number: number, Title: "Test"}, nil
	}
	o.workerStartPhaseFn = func(cfg *config.Config, sess *state.Session, slotName, prompt, backendName string) error {
		sess.Status = state.StatusRunning
		sess.PID = 12345
		return nil
	}

	o.checkSessions(s)

	sess := s.Sessions["slot-1"]
	if sess.Phase != state.PhaseImplement {
		t.Errorf("expected implement phase after plan complete, got %s", sess.Phase)
	}
	if sess.Status != state.StatusRunning {
		t.Errorf("expected running, got %s", sess.Status)
	}
}
