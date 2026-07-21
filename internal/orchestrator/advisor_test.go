package orchestrator

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/github"
	"github.com/befeast/maestro/internal/pipeline"
	"github.com/befeast/maestro/internal/state"
)

func TestAdvisorPipelineApprovalStartsImplementer(t *testing.T) {
	cfg, o := advisorTestOrchestrator(t, false)
	dir := advisorOrchestratorWorktree(t)
	sess := advisorPlanSession(dir)
	var phases []state.Phase
	var advisorPrompt string
	var advisorBackend string
	o.workerStartPhaseFn = func(_ *config.Config, s *state.Session, _ string, prompt, backend string) error {
		phases = append(phases, s.Phase)
		if s.Phase == state.PhaseAdvisor {
			advisorPrompt = prompt
			advisorBackend = backend
		}
		s.Status = state.StatusRunning
		s.PID++
		return nil
	}

	if !o.advancePipeline(state.NewState(), "slot-advisor", sess) {
		t.Fatal("planner completion was not handled")
	}
	if sess.Phase != state.PhaseAdvisor || sess.PlanVersion != 1 || sess.AdvisorReviewRound != 1 || sess.AdvisorMaxReviewRounds != 2 {
		t.Fatalf("after planner = phase %q plan v%d round %d/%d", sess.Phase, sess.PlanVersion, sess.AdvisorReviewRound, sess.AdvisorMaxReviewRounds)
	}
	if advisorBackend != "advisor" {
		t.Fatalf("Advisor backend = %q", advisorBackend)
	}
	for _, want := range []string{"issue body", "plan v1", "validation v1", "Plan version: 1", "Review round: 1"} {
		if !strings.Contains(advisorPrompt, want) {
			t.Fatalf("Advisor prompt missing %q:\n%s", want, advisorPrompt)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, pipeline.AdvisorReviewFile), []byte("PLAN_APPROVED\nIndependent review complete."), 0o644); err != nil {
		t.Fatal(err)
	}
	if !o.advancePipeline(state.NewState(), "slot-advisor", sess) {
		t.Fatal("Advisor completion was not handled")
	}
	if sess.Phase != state.PhaseImplement || sess.AdvisorVerdict != pipeline.AdvisorVerdictApproved || sess.AdvisorUnresolvedFindings != "" {
		t.Fatalf("after approval = phase %q verdict %q findings %q", sess.Phase, sess.AdvisorVerdict, sess.AdvisorUnresolvedFindings)
	}
	if len(sess.AdvisorReviews) != 1 || sess.AdvisorReviews[0].Backend != "advisor" || sess.AdvisorReviews[0].Model != "review-model" {
		t.Fatalf("Advisor history = %+v", sess.AdvisorReviews)
	}
	if got := phases; len(got) != 2 || got[0] != state.PhaseAdvisor || got[1] != state.PhaseImplement {
		t.Fatalf("started phases = %v", got)
	}
	_ = cfg
}

func TestAdvisorPipelineRevisionLoopAndExhaustion(t *testing.T) {
	_, o := advisorTestOrchestrator(t, false)
	dir := advisorOrchestratorWorktree(t)
	sess := advisorPlanSession(dir)
	var plannerRevisionPrompt string
	o.workerStartPhaseFn = func(_ *config.Config, s *state.Session, _ string, prompt, _ string) error {
		if s.Phase == state.PhasePlan {
			plannerRevisionPrompt = prompt
		}
		s.Status = state.StatusRunning
		s.PID++
		return nil
	}

	o.advancePipeline(state.NewState(), "slot-revise", sess)
	firstFindings := "Add a timeout test.\nDefine the fail-closed API state."
	os.WriteFile(filepath.Join(dir, pipeline.AdvisorReviewFile), []byte("PLAN_REVISE\n"+firstFindings), 0o644)
	o.advancePipeline(state.NewState(), "slot-revise", sess)
	if sess.Phase != state.PhasePlan || sess.AdvisorUnresolvedFindings != firstFindings || !strings.Contains(plannerRevisionPrompt, firstFindings) {
		t.Fatalf("revision handoff phase=%q findings=%q prompt=%q", sess.Phase, sess.AdvisorUnresolvedFindings, plannerRevisionPrompt)
	}
	if _, err := os.Stat(filepath.Join(dir, pipeline.AdvisorReviewFile)); !os.IsNotExist(err) {
		t.Fatalf("stale review artifact was not removed: %v", err)
	}

	os.WriteFile(filepath.Join(dir, pipeline.PlanFile), []byte("plan v2 with timeout\n"), 0o644)
	os.WriteFile(filepath.Join(dir, pipeline.ValidationFile), []byte("validation v2 with fail-closed assertions\n"), 0o644)
	advisorGit(t, dir, "add", pipeline.PlanFile, pipeline.ValidationFile)
	advisorGit(t, dir, "commit", "-m", "revise plan")
	o.advancePipeline(state.NewState(), "slot-revise", sess)
	if sess.Phase != state.PhaseAdvisor || sess.PlanVersion != 2 || sess.AdvisorReviewRound != 2 {
		t.Fatalf("second review phase=%q plan=%d round=%d", sess.Phase, sess.PlanVersion, sess.AdvisorReviewRound)
	}
	secondFindings := "The validation command is still ambiguous."
	os.WriteFile(filepath.Join(dir, pipeline.AdvisorReviewFile), []byte("PLAN_REVISE\n"+secondFindings), 0o644)
	o.advancePipeline(state.NewState(), "slot-revise", sess)
	if sess.Status != state.StatusFailed || sess.Phase != state.PhaseAdvisor || sess.AdvisorTerminalReason != "review_rounds_exhausted" || sess.AdvisorUnresolvedFindings != secondFindings {
		t.Fatalf("exhaustion status=%q phase=%q reason=%q findings=%q", sess.Status, sess.Phase, sess.AdvisorTerminalReason, sess.AdvisorUnresolvedFindings)
	}
	if len(sess.AdvisorReviews) != 2 || sess.AdvisorReviews[1].TerminalReason != "review_rounds_exhausted" {
		t.Fatalf("review history = %+v", sess.AdvisorReviews)
	}
	attention := state.SessionAttentionFor(sess, nil)
	if !attention.NeedsAttention || !strings.Contains(attention.Reason, secondFindings) {
		t.Fatalf("attention = %+v", attention)
	}
}

func TestAdvisorPipelineMalformedAndMissingVerdictsFailClosed(t *testing.T) {
	for _, tt := range []struct {
		name    string
		content *string
		reason  string
	}{
		{name: "missing", reason: "missing_review_artifact"},
		{name: "malformed", content: stringPtr("Review complete: PLAN_APPROVED\n"), reason: "invalid_verdict"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, o := advisorTestOrchestrator(t, false)
			dir := advisorOrchestratorWorktree(t)
			sess := advisorPlanSession(dir)
			o.workerStartPhaseFn = phaseStartStub
			o.advancePipeline(state.NewState(), "slot-invalid", sess)
			if tt.content != nil {
				os.WriteFile(filepath.Join(dir, pipeline.AdvisorReviewFile), []byte(*tt.content), 0o644)
			}
			o.advancePipeline(state.NewState(), "slot-invalid", sess)
			if sess.Status != state.StatusFailed || sess.AdvisorTerminalReason != tt.reason || sess.Phase != state.PhaseAdvisor {
				t.Fatalf("status=%q phase=%q reason=%q", sess.Status, sess.Phase, sess.AdvisorTerminalReason)
			}
		})
	}
}

func TestAdvisorPipelineBestEffortBypassIsExplicitAndAudited(t *testing.T) {
	_, o := advisorTestOrchestrator(t, true)
	dir := advisorOrchestratorWorktree(t)
	sess := advisorPlanSession(dir)
	var implementStarted bool
	o.workerStartPhaseFn = func(_ *config.Config, s *state.Session, _ string, _ string, _ string) error {
		if s.Phase == state.PhaseImplement {
			implementStarted = true
		}
		s.Status = state.StatusRunning
		s.PID++
		return nil
	}
	o.advancePipeline(state.NewState(), "slot-bypass", sess)
	os.WriteFile(filepath.Join(dir, pipeline.AdvisorReviewFile), []byte("probably approved"), 0o644)
	o.advancePipeline(state.NewState(), "slot-bypass", sess)
	if !implementStarted || sess.Phase != state.PhaseImplement || !sess.AdvisorBypassed || sess.AdvisorVerdict != pipeline.AdvisorVerdictBypassed || sess.AdvisorTerminalReason != "invalid_verdict" {
		t.Fatalf("bypass state = %+v", sess)
	}
	if len(sess.AdvisorReviews) != 1 || !sess.AdvisorReviews[0].Bypassed || sess.AdvisorReviews[0].Verdict != pipeline.AdvisorVerdictInvalid {
		t.Fatalf("bypass history = %+v", sess.AdvisorReviews)
	}
}

func TestAdvisorPipelineRejectsSourceMutation(t *testing.T) {
	_, o := advisorTestOrchestrator(t, true)
	dir := advisorOrchestratorWorktree(t)
	sess := advisorPlanSession(dir)
	o.workerStartPhaseFn = phaseStartStub
	o.advancePipeline(state.NewState(), "slot-mutate", sess)
	os.WriteFile(filepath.Join(dir, "source.go"), []byte("package source\n"), 0o644)
	os.WriteFile(filepath.Join(dir, pipeline.AdvisorReviewFile), []byte("PLAN_APPROVED\n"), 0o644)
	o.advancePipeline(state.NewState(), "slot-mutate", sess)
	if sess.Status != state.StatusFailed || sess.AdvisorTerminalReason != "advisor_worktree_mutation" || sess.AdvisorBypassed {
		t.Fatalf("mutation state = status %q reason %q", sess.Status, sess.AdvisorTerminalReason)
	}
}

func TestAdvisorPipelineUnavailableRequiredBackendFailsClosed(t *testing.T) {
	cfg, o := advisorTestOrchestrator(t, false)
	disabled := false
	def := cfg.Model.Backends["advisor"]
	def.Enabled = &disabled
	cfg.Model.Backends["advisor"] = def
	dir := advisorOrchestratorWorktree(t)
	sess := advisorPlanSession(dir)
	started := false
	o.workerStartPhaseFn = func(_ *config.Config, _ *state.Session, _, _, _ string) error { started = true; return nil }
	o.advancePipeline(state.NewState(), "slot-unavailable", sess)
	if started || sess.Status != state.StatusFailed || sess.AdvisorTerminalReason != "backend_unavailable" || sess.AdvisorReviewRound != 1 {
		t.Fatalf("backend gate started=%v status=%q reason=%q round=%d", started, sess.Status, sess.AdvisorTerminalReason, sess.AdvisorReviewRound)
	}
}

func TestAdvisorPipelineRestartReconcilesPersistedReview(t *testing.T) {
	_, o := advisorTestOrchestrator(t, false)
	dir := advisorOrchestratorWorktree(t)
	sess := advisorPlanSession(dir)
	o.workerStartPhaseFn = phaseStartStub
	o.advancePipeline(state.NewState(), "slot-restart", sess)
	data, err := json.Marshal(sess)
	if err != nil {
		t.Fatal(err)
	}
	var restored state.Session
	if err := json.Unmarshal(data, &restored); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, pipeline.AdvisorReviewFile), []byte("PLAN_APPROVED\n"), 0o644)
	implementStarted := false
	o.workerStartPhaseFn = func(_ *config.Config, s *state.Session, _ string, _ string, _ string) error {
		implementStarted = s.Phase == state.PhaseImplement
		s.Status = state.StatusRunning
		return nil
	}
	o.advancePipeline(state.NewState(), "slot-restart", &restored)
	if !implementStarted || restored.Phase != state.PhaseImplement || restored.AdvisorVerdict != pipeline.AdvisorVerdictApproved {
		t.Fatalf("restored Advisor state = %+v", restored)
	}
}

func TestAdvisorBackendDeathAndTimeoutFailClosed(t *testing.T) {
	t.Run("backend model unavailable", func(t *testing.T) {
		cfg, o := advisorTestOrchestrator(t, false)
		cfg.MaxRuntimeMinutes = 999
		o.listOpenPRsFn = func() ([]github.PR, error) { return nil, nil }
		o.isIssueClosedFn = func(int) (bool, error) { return false, nil }
		o.pidAliveFn = func(int) bool { return false }
		o.isRateLimitedFn = func(string) bool { return false }
		o.authFailureFromLogFn = func(string) (bool, string) { return false, "" }
		o.modelUnavailableFromLogFn = func(string) (bool, string) { return true, "model_access_denied" }
		o.usageLimitFromLogFn = func(string, []string) (bool, string) { return false, "" }
		st := state.NewState()
		st.Sessions["slot-backend"] = &state.Session{
			IssueNumber: 928, IssueTitle: "Advisor", Status: state.StatusRunning, Phase: state.PhaseAdvisor,
			PID: 999, StartedAt: time.Now().UTC(), Backend: "advisor", AdvisorBackend: "advisor", AdvisorReviewRound: 1, AdvisorMaxReviewRounds: 2,
		}
		o.checkSessions(st)
		sess := st.Sessions["slot-backend"]
		if sess.Status != state.StatusFailed || sess.AdvisorTerminalReason != "backend_unavailable" || sess.ProviderLimitReason != state.BackendBlockModelUnavailable {
			t.Fatalf("backend death state = %+v", sess)
		}
	})

	t.Run("phase runtime", func(t *testing.T) {
		cfg, o := advisorTestOrchestrator(t, false)
		cfg.MaxRuntimeMinutes = 120
		cfg.Pipeline.Advisor.MaxRuntimeMinutes = 1
		o.listOpenPRsFn = func() ([]github.PR, error) { return nil, nil }
		o.isIssueClosedFn = func(int) (bool, error) { return false, nil }
		o.pidAliveFn = func(int) bool { return true }
		o.captureTmuxFn = func(string) (string, error) { return "working", nil }
		o.workerStopFn = func(*config.Config, string, *state.Session) error { return nil }
		logFile := filepath.Join(t.TempDir(), "advisor.log")
		os.WriteFile(logFile, []byte("still working\n"), 0o644)
		st := state.NewState()
		st.Sessions["slot-timeout"] = &state.Session{
			IssueNumber: 928, IssueTitle: "Advisor", Status: state.StatusRunning, Phase: state.PhaseAdvisor,
			PID: 999, StartedAt: time.Now().UTC().Add(-2 * time.Minute), Backend: "advisor", AdvisorBackend: "advisor", AdvisorReviewRound: 1, AdvisorMaxReviewRounds: 2, LogFile: logFile,
		}
		o.checkSessions(st)
		sess := st.Sessions["slot-timeout"]
		if sess.Status != state.StatusFailed || sess.AdvisorTerminalReason != "timeout" {
			t.Fatalf("timeout state = %+v", sess)
		}
	})
}

func TestPipelineAdvisedLabelForcesFullAdvisedPipeline(t *testing.T) {
	base := pipelineConfig()
	base.Pipeline.Enabled = false
	base.Pipeline.Advisor.Enabled = false
	cfg, full, advised := pipelineConfigForIssue(base, makeIssue(928, "Advised", pipelineAdvisedLabel))
	if full || !advised || !cfg.Pipeline.Enabled || !cfg.Pipeline.Planner.Enabled || !cfg.Pipeline.Advisor.Enabled || !cfg.Pipeline.Validator.Enabled {
		t.Fatalf("selection full=%v advised=%v pipeline=%+v", full, advised, cfg.Pipeline)
	}
	fullCfg, full, advised := pipelineConfigForIssue(base, makeIssue(928, "Full", pipelineFullLabel))
	if !full || advised || fullCfg.Pipeline.Advisor.Enabled {
		t.Fatalf("pipeline:full compatibility full=%v advised=%v advisor=%v", full, advised, fullCfg.Pipeline.Advisor.Enabled)
	}
}

func advisorTestOrchestrator(t *testing.T, bestEffort bool) (*config.Config, *Orchestrator) {
	t.Helper()
	cfg := pipelineConfig()
	cfg.StateDir = t.TempDir()
	cfg.LocalPath = t.TempDir()
	cfg.MaxRuntimeMinutes = 999
	cfg.Pipeline.Advisor = config.RoleConfig{Enabled: true, Backend: "advisor", Effort: "high", MaxRuntimeMinutes: 10}
	cfg.Pipeline.AdvisorBestEffort = bestEffort
	cfg.Model.Backends["advisor"] = config.BackendDef{Cmd: "claude", Model: "review-model"}
	o := pipelineOrchestrator(cfg)
	o.getIssueFn = func(number int) (github.Issue, error) {
		return github.Issue{Number: number, Title: "Advisor issue", Body: "issue body"}, nil
	}
	return cfg, o
}

func advisorPlanSession(dir string) *state.Session {
	return &state.Session{
		IssueNumber: 928,
		IssueTitle:  "Advisor issue",
		Phase:       state.PhasePlan,
		Worktree:    dir,
		Branch:      "feat/advisor",
		Status:      state.StatusRunning,
		PID:         100,
		StartedAt:   time.Now().UTC(),
	}
}

func phaseStartStub(_ *config.Config, s *state.Session, _ string, _ string, _ string) error {
	s.Status = state.StatusRunning
	s.PID++
	return nil
}

func advisorOrchestratorWorktree(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	advisorGit(t, dir, "init", "-q")
	advisorGit(t, dir, "config", "user.email", "advisor@example.invalid")
	advisorGit(t, dir, "config", "user.name", "Advisor Test")
	os.WriteFile(filepath.Join(dir, pipeline.PlanFile), []byte("plan v1\n"), 0o644)
	os.WriteFile(filepath.Join(dir, pipeline.ValidationFile), []byte("validation v1\n"), 0o644)
	advisorGit(t, dir, "add", pipeline.PlanFile, pipeline.ValidationFile)
	advisorGit(t, dir, "commit", "-m", "plan")
	return dir
}

func advisorGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmdArgs := append([]string{"-C", dir}, args...)
	if out, err := exec.Command("git", cmdArgs...).CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

func stringPtr(value string) *string { return &value }
