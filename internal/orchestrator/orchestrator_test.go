package orchestrator

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/github"
	"github.com/befeast/maestro/internal/notify"
	"github.com/befeast/maestro/internal/router"
	"github.com/befeast/maestro/internal/state"
)

func makeIssue(number int, title string, labels ...string) github.Issue {
	issue := github.Issue{Number: number, Title: title}
	for _, l := range labels {
		issue.Labels = append(issue.Labels, struct {
			Name string `json:"name"`
		}{Name: l})
	}
	return issue
}

func TestHashOutput_UsesLast50LinesOnly(t *testing.T) {
	lines := make([]string, 0, 60)
	for i := 1; i <= 60; i++ {
		lines = append(lines, fmt.Sprintf("line-%d", i))
	}
	all := strings.Join(lines, "\n")
	last50 := strings.Join(lines[10:], "\n")

	got := hashOutput(all)
	want := hashOutput(last50)
	if got != want {
		t.Fatalf("hashOutput() should only depend on last 50 lines; got %q want %q", got, want)
	}
}

func TestCountSilentTimeoutKillsForIssue(t *testing.T) {
	s := state.NewState()
	s.Sessions["pan-1"] = &state.Session{IssueNumber: 78, LastNotifiedStatus: "silent_timeout"}
	s.Sessions["pan-2"] = &state.Session{IssueNumber: 78, LastNotifiedStatus: "silent_timeout"}
	s.Sessions["pan-3"] = &state.Session{IssueNumber: 78, LastNotifiedStatus: "ci_failure"}
	s.Sessions["pan-4"] = &state.Session{IssueNumber: 79, LastNotifiedStatus: "silent_timeout"}

	if got := countSilentTimeoutKillsForIssue(s, 78); got != 2 {
		t.Fatalf("countSilentTimeoutKillsForIssue(78)=%d, want 2", got)
	}
}

func TestSelectPrompt_BugLabel(t *testing.T) {
	o := &Orchestrator{
		cfg:                   &config.Config{Repo: "owner/repo"},
		promptBase:            "default prompt",
		bugPromptBase:         "bug prompt",
		enhancementPromptBase: "enhancement prompt",
	}
	got := o.selectPrompt(makeIssue(1, "Fix crash", "bug"))
	if got != "bug prompt" {
		t.Errorf("selectPrompt() = %q, want %q", got, "bug prompt")
	}
}

func TestSelectPrompt_EnhancementLabel(t *testing.T) {
	o := &Orchestrator{
		cfg:                   &config.Config{Repo: "owner/repo"},
		promptBase:            "default prompt",
		bugPromptBase:         "bug prompt",
		enhancementPromptBase: "enhancement prompt",
	}
	got := o.selectPrompt(makeIssue(2, "Add feature", "enhancement"))
	if got != "enhancement prompt" {
		t.Errorf("selectPrompt() = %q, want %q", got, "enhancement prompt")
	}
}

func TestSelectPrompt_FallbackToDefault(t *testing.T) {
	o := &Orchestrator{
		cfg:                   &config.Config{Repo: "owner/repo"},
		promptBase:            "default prompt",
		bugPromptBase:         "bug prompt",
		enhancementPromptBase: "enhancement prompt",
	}
	got := o.selectPrompt(makeIssue(3, "Update docs", "documentation"))
	if got != "default prompt" {
		t.Errorf("selectPrompt() = %q, want %q", got, "default prompt")
	}
}

func TestSelectPrompt_BugTakesPrecedenceOverEnhancement(t *testing.T) {
	o := &Orchestrator{
		cfg:                   &config.Config{Repo: "owner/repo"},
		promptBase:            "default prompt",
		bugPromptBase:         "bug prompt",
		enhancementPromptBase: "enhancement prompt",
	}
	got := o.selectPrompt(makeIssue(4, "Bug and enhancement", "bug", "enhancement"))
	if got != "bug prompt" {
		t.Errorf("selectPrompt() = %q, want %q (bug should take precedence)", got, "bug prompt")
	}
}

func TestSelectPrompt_NoBugPromptConfigured(t *testing.T) {
	o := &Orchestrator{
		cfg:                   &config.Config{Repo: "owner/repo"},
		promptBase:            "default prompt",
		bugPromptBase:         "",
		enhancementPromptBase: "enhancement prompt",
	}
	got := o.selectPrompt(makeIssue(5, "Fix crash", "bug"))
	if got != "default prompt" {
		t.Errorf("selectPrompt() = %q, want %q (should fall back when bug_prompt not set)", got, "default prompt")
	}
}

func TestSelectPrompt_NoEnhancementPromptConfigured(t *testing.T) {
	o := &Orchestrator{
		cfg:                   &config.Config{Repo: "owner/repo"},
		promptBase:            "default prompt",
		bugPromptBase:         "bug prompt",
		enhancementPromptBase: "",
	}
	got := o.selectPrompt(makeIssue(6, "Add feature", "enhancement"))
	if got != "default prompt" {
		t.Errorf("selectPrompt() = %q, want %q (should fall back when enhancement_prompt not set)", got, "default prompt")
	}
}

func TestSelectPrompt_NoLabels(t *testing.T) {
	o := &Orchestrator{
		cfg:                   &config.Config{Repo: "owner/repo"},
		promptBase:            "default prompt",
		bugPromptBase:         "bug prompt",
		enhancementPromptBase: "enhancement prompt",
	}
	got := o.selectPrompt(makeIssue(7, "Something"))
	if got != "default prompt" {
		t.Errorf("selectPrompt() = %q, want %q", got, "default prompt")
	}
}

func TestSelectPrompt_CaseInsensitiveLabel(t *testing.T) {
	o := &Orchestrator{
		cfg:                   &config.Config{Repo: "owner/repo"},
		promptBase:            "default prompt",
		bugPromptBase:         "bug prompt",
		enhancementPromptBase: "enhancement prompt",
	}
	got := o.selectPrompt(makeIssue(8, "Fix crash", "Bug"))
	if got != "bug prompt" {
		t.Errorf("selectPrompt() = %q, want %q (label matching should be case-insensitive)", got, "bug prompt")
	}
}

// TestReconcileRunningSessions_DeadWorkerWithOpenPR_TransitionsToPROpen verifies
// the fix for the infinite-spawn bug (issue #152): when a worker exits after
// creating a PR, reconcile must NOT mark the session dead — it must transition
// to pr_open so that IssueInProgress returns true and no duplicate worker is spawned.
func TestReconcileRunningSessions_DeadWorkerWithOpenPR_TransitionsToPROpen(t *testing.T) {
	s := state.NewState()
	s.Sessions["mae-5"] = &state.Session{
		IssueNumber: 105,
		IssueTitle:  "fix crash",
		Status:      state.StatusRunning,
		PID:         9999,
		TmuxSession: "maestro-mae-5",
		Branch:      "feat/mae-5-105-fix-crash",
	}

	openPRs := []github.PR{
		{Number: 137, HeadRefName: "feat/mae-5-105-fix-crash", Title: "fix crash"},
	}

	o := &Orchestrator{
		pidAliveFn:          func(pid int) bool { return false },
		tmuxSessionExistsFn: func(name string) bool { return false },
		listOpenPRsFn:       func() ([]github.PR, error) { return openPRs, nil },
	}

	changed := o.reconcileRunningSessions(s)
	if !changed {
		t.Fatal("expected reconciliation to report changes")
	}

	sess := s.Sessions["mae-5"]
	if sess.Status != state.StatusPROpen {
		t.Fatalf("status = %q, want %q (worker created PR before exiting — should not be dead)", sess.Status, state.StatusPROpen)
	}
	if sess.PRNumber != 137 {
		t.Fatalf("pr_number = %d, want 137", sess.PRNumber)
	}
	if sess.PID != 0 {
		t.Fatalf("pid = %d, want 0", sess.PID)
	}
	if sess.TmuxSession != "" {
		t.Fatalf("tmux_session = %q, want empty", sess.TmuxSession)
	}
	if sess.FinishedAt == nil {
		t.Fatal("finished_at should be set")
	}
	// Crucially: IssueInProgress must return true so no duplicate worker is spawned
	if !s.IssueInProgress(105) {
		t.Fatal("IssueInProgress(105) must return true after transition to pr_open")
	}
}

// TestReconcileRunningSessions_DeadWorkerNoPR_TransitionsToDead verifies that
// the existing behaviour is preserved when no PR exists for the dead worker.
func TestReconcileRunningSessions_DeadWorkerNoPR_TransitionsToDead(t *testing.T) {
	s := state.NewState()
	s.Sessions["mae-6"] = &state.Session{
		IssueNumber: 106,
		IssueTitle:  "add feature",
		Status:      state.StatusRunning,
		PID:         8888,
		TmuxSession: "maestro-mae-6",
		Branch:      "feat/mae-6-106-add-feature",
	}

	// No open PRs for this branch
	o := &Orchestrator{
		pidAliveFn:          func(pid int) bool { return false },
		tmuxSessionExistsFn: func(name string) bool { return false },
		listOpenPRsFn:       func() ([]github.PR, error) { return []github.PR{}, nil },
	}

	changed := o.reconcileRunningSessions(s)
	if !changed {
		t.Fatal("expected reconciliation to report changes")
	}

	sess := s.Sessions["mae-6"]
	if sess.Status != state.StatusDead {
		t.Fatalf("status = %q, want %q", sess.Status, state.StatusDead)
	}
	if sess.PRNumber != 0 {
		t.Fatalf("pr_number = %d, want 0", sess.PRNumber)
	}
}

// TestReconcileRunningSessions_PRListError_FallsBackToDead ensures that when
// the GitHub PR listing fails, reconcile still marks the session dead (degraded
// mode) rather than panicking or blocking indefinitely.
func TestReconcileRunningSessions_PRListError_FallsBackToDead(t *testing.T) {
	s := state.NewState()
	s.Sessions["mae-7"] = &state.Session{
		IssueNumber: 107,
		Status:      state.StatusRunning,
		PID:         7777,
		TmuxSession: "maestro-mae-7",
		Branch:      "feat/mae-7-107-something",
	}

	o := &Orchestrator{
		pidAliveFn:          func(pid int) bool { return false },
		tmuxSessionExistsFn: func(name string) bool { return false },
		listOpenPRsFn:       func() ([]github.PR, error) { return nil, fmt.Errorf("network error") },
	}

	changed := o.reconcileRunningSessions(s)
	if !changed {
		t.Fatal("expected reconciliation to report changes")
	}
	sess := s.Sessions["mae-7"]
	// Falls back to dead when PR list unavailable — better to mark dead than to loop forever
	if sess.Status != state.StatusDead {
		t.Fatalf("status = %q, want %q (should fall back to dead when PR list fails)", sess.Status, state.StatusDead)
	}
}

func TestReconcileRunningSessions_DeadPIDGetsMarkedDead(t *testing.T) {
	s := state.NewState()
	s.Sessions["pan-1"] = &state.Session{
		IssueNumber:        71,
		Status:             state.StatusRunning,
		PID:                4242,
		TmuxSession:        "maestro-pan-1",
		RetryCount:         2,
		IssueTitle:         "stale worker",
		LastNotifiedStatus: "",
		Branch:             "feat/pan-1-71-stale-worker",
	}

	o := &Orchestrator{
		pidAliveFn:          func(pid int) bool { return false },
		tmuxSessionExistsFn: func(name string) bool { return true },
		listOpenPRsFn:       func() ([]github.PR, error) { return []github.PR{}, nil },
	}

	changed := o.reconcileRunningSessions(s)
	if !changed {
		t.Fatal("expected reconciliation to report changes")
	}

	sess := s.Sessions["pan-1"]
	if sess.Status != state.StatusDead {
		t.Fatalf("status = %q, want %q", sess.Status, state.StatusDead)
	}
	if sess.PID != 0 {
		t.Fatalf("pid = %d, want 0", sess.PID)
	}
	if sess.TmuxSession != "" {
		t.Fatalf("tmux_session = %q, want empty", sess.TmuxSession)
	}
	if sess.RetryCount != 2 {
		t.Fatalf("retry_count = %d, want 2", sess.RetryCount)
	}
	if sess.FinishedAt == nil {
		t.Fatal("finished_at should be set when session is marked dead")
	}
}

func TestReconcileRunningSessions_MissingTmuxGetsMarkedDead(t *testing.T) {
	s := state.NewState()
	s.Sessions["pan-2"] = &state.Session{
		IssueNumber: 71,
		Status:      state.StatusRunning,
		PID:         5151,
		TmuxSession: "maestro-pan-2",
		Branch:      "feat/pan-2-71-stale",
	}

	o := &Orchestrator{
		pidAliveFn:          func(pid int) bool { return true },
		tmuxSessionExistsFn: func(name string) bool { return false },
		listOpenPRsFn:       func() ([]github.PR, error) { return []github.PR{}, nil },
	}

	changed := o.reconcileRunningSessions(s)
	if !changed {
		t.Fatal("expected reconciliation to report changes")
	}

	sess := s.Sessions["pan-2"]
	if sess.Status != state.StatusDead {
		t.Fatalf("status = %q, want %q", sess.Status, state.StatusDead)
	}
	if sess.PID != 0 {
		t.Fatalf("pid = %d, want 0", sess.PID)
	}
	if sess.TmuxSession != "" {
		t.Fatalf("tmux_session = %q, want empty", sess.TmuxSession)
	}
	if sess.RetryCount != 0 {
		t.Fatalf("retry_count = %d, want 0", sess.RetryCount)
	}
	if sess.FinishedAt == nil {
		t.Fatal("finished_at should be set when session is marked dead")
	}
}

func TestReconcileRunningSessions_UsesDefaultTmuxNameWhenMissingInState(t *testing.T) {
	s := state.NewState()
	s.Sessions["pan-3"] = &state.Session{
		IssueNumber: 73,
		Status:      state.StatusRunning,
		PID:         6262,
		Branch:      "feat/pan-3-73-something",
		// TmuxSession intentionally empty; should fall back to worker.TmuxSessionName(slot)
	}

	calledWith := ""
	o := &Orchestrator{
		pidAliveFn: func(pid int) bool { return true },
		tmuxSessionExistsFn: func(name string) bool {
			calledWith = name
			return true
		},
		listOpenPRsFn: func() ([]github.PR, error) { return []github.PR{}, nil },
	}

	changed := o.reconcileRunningSessions(s)
	if changed {
		t.Fatal("expected no reconciliation changes when pid and tmux are healthy")
	}
	if calledWith != "maestro-pan-3" {
		t.Fatalf("tmux session checked = %q, want %q", calledWith, "maestro-pan-3")
	}
}

func TestRunDeployCmd_Success(t *testing.T) {
	o := &Orchestrator{
		cfg: &config.Config{
			Repo:                 "owner/repo",
			LocalPath:            "/tmp",
			DeployCmd:            "echo deploy-ok",
			DeployTimeoutMinutes: 15,
		},
		notifier: &notify.Notifier{},
	}
	if err := o.runDeployCmd(42); err != nil {
		t.Errorf("runDeployCmd() unexpected error: %v", err)
	}
}

func TestRunDeployCmd_Failure(t *testing.T) {
	o := &Orchestrator{
		cfg: &config.Config{
			Repo:                 "owner/repo",
			LocalPath:            "/tmp",
			DeployCmd:            "exit 1",
			DeployTimeoutMinutes: 15,
		},
		notifier: &notify.Notifier{},
	}
	err := o.runDeployCmd(42)
	if err == nil {
		t.Fatal("runDeployCmd() expected error, got nil")
	}
	if !strings.Contains(err.Error(), "deploy command failed") {
		t.Errorf("error = %q, want it to contain 'deploy command failed'", err.Error())
	}
}

func TestRunDeployCmd_CapturesOutput(t *testing.T) {
	o := &Orchestrator{
		cfg: &config.Config{
			Repo:                 "owner/repo",
			LocalPath:            "/tmp",
			DeployCmd:            "echo hello-deploy && exit 1",
			DeployTimeoutMinutes: 15,
		},
		notifier: &notify.Notifier{},
	}
	err := o.runDeployCmd(42)
	if err == nil {
		t.Fatal("runDeployCmd() expected error, got nil")
	}
	if !strings.Contains(err.Error(), "hello-deploy") {
		t.Errorf("error = %q, want it to contain command output 'hello-deploy'", err.Error())
	}
}

func TestRunDeployCmd_UsesConfiguredTimeout(t *testing.T) {
	o := &Orchestrator{
		cfg: &config.Config{
			Repo:                 "owner/repo",
			LocalPath:            "/tmp",
			DeployCmd:            "sleep 5",
			DeployTimeoutMinutes: 1, // 1 minute — command should succeed well within this
		},
		notifier: &notify.Notifier{},
	}
	if err := o.runDeployCmd(42); err != nil {
		t.Errorf("runDeployCmd() unexpected error: %v", err)
	}
}

func TestMergeStrategy_DefaultSequential(t *testing.T) {
	o := &Orchestrator{cfg: &config.Config{Repo: "owner/repo"}}
	if got := o.mergeStrategy(); got != "sequential" {
		t.Fatalf("mergeStrategy() = %q, want %q", got, "sequential")
	}
}

func TestMergeStrategy_Parallel(t *testing.T) {
	o := &Orchestrator{cfg: &config.Config{Repo: "owner/repo", MergeStrategy: "parallel"}}
	if got := o.mergeStrategy(); got != "parallel" {
		t.Fatalf("mergeStrategy() = %q, want %q", got, "parallel")
	}
}

func TestMergeInterval_Default30s(t *testing.T) {
	o := &Orchestrator{cfg: &config.Config{Repo: "owner/repo"}}
	if got := o.mergeInterval(); got != 30*time.Second {
		t.Fatalf("mergeInterval() = %s, want %s", got, 30*time.Second)
	}
}

func TestMergeInterval_Explicit(t *testing.T) {
	o := &Orchestrator{cfg: &config.Config{Repo: "owner/repo", MergeIntervalSeconds: 45}}
	if got := o.mergeInterval(); got != 45*time.Second {
		t.Fatalf("mergeInterval() = %s, want %s", got, 45*time.Second)
	}
}

// --- resolveBackend tests ---

func cfgWithBackends(defaultBackend string, backends ...string) *config.Config {
	m := make(map[string]config.BackendDef, len(backends))
	for _, b := range backends {
		m[b] = config.BackendDef{Cmd: b}
	}
	return &config.Config{
		Repo: "owner/repo",
		Model: config.ModelConfig{
			Default:  defaultBackend,
			Backends: m,
		},
	}
}

func TestResolveBackend_ModelLabelOverride(t *testing.T) {
	cfg := cfgWithBackends("claude", "claude", "codex", "gemini")
	o := &Orchestrator{cfg: cfg, router: router.New(cfg)}
	got := o.resolveBackend(makeIssue(1, "Fix bug", "model:codex"))
	if got != "codex" {
		t.Errorf("resolveBackend() = %q, want %q", got, "codex")
	}
}

func TestResolveBackend_ModelLabelGemini(t *testing.T) {
	cfg := cfgWithBackends("claude", "claude", "codex", "gemini")
	o := &Orchestrator{cfg: cfg, router: router.New(cfg)}
	got := o.resolveBackend(makeIssue(2, "Add feature", "enhancement", "model:gemini"))
	if got != "gemini" {
		t.Errorf("resolveBackend() = %q, want %q", got, "gemini")
	}
}

func TestResolveBackend_UnknownBackendFallsToDefault(t *testing.T) {
	cfg := cfgWithBackends("claude", "claude", "codex")
	o := &Orchestrator{cfg: cfg, router: router.New(cfg)}
	got := o.resolveBackend(makeIssue(3, "Fix bug", "model:nonexistent"))
	if got != "claude" {
		t.Errorf("resolveBackend() = %q, want %q (unknown backend should fall back to default)", got, "claude")
	}
}

func TestResolveBackend_NoLabelReturnsDefault(t *testing.T) {
	cfg := cfgWithBackends("claude", "claude", "codex")
	o := &Orchestrator{cfg: cfg, router: router.New(cfg)}
	got := o.resolveBackend(makeIssue(4, "Fix bug"))
	if got != "claude" {
		t.Errorf("resolveBackend() = %q, want %q", got, "claude")
	}
}

func TestResolveBackend_NoLabelWithAutoRouting(t *testing.T) {
	cfg := cfgWithBackends("claude", "claude", "codex")
	cfg.Routing.Mode = "auto"
	r := router.New(cfg)
	r.RouteFn = func(issue github.Issue) (string, string, error) {
		return "codex", "simple fix", nil
	}
	o := &Orchestrator{cfg: cfg, router: r}
	got := o.resolveBackend(makeIssue(5, "Simple fix"))
	if got != "codex" {
		t.Errorf("resolveBackend() = %q, want %q", got, "codex")
	}
}

func TestResolveBackend_LabelOverridesAutoRouting(t *testing.T) {
	cfg := cfgWithBackends("claude", "claude", "codex", "gemini")
	cfg.Routing.Mode = "auto"
	routerCalled := false
	r := router.New(cfg)
	r.RouteFn = func(issue github.Issue) (string, string, error) {
		routerCalled = true
		return "codex", "router pick", nil
	}
	o := &Orchestrator{cfg: cfg, router: r}
	got := o.resolveBackend(makeIssue(6, "Fix bug", "model:gemini"))
	if got != "gemini" {
		t.Errorf("resolveBackend() = %q, want %q (label should override auto-routing)", got, "gemini")
	}
	if routerCalled {
		t.Error("router should not be called when model: label is present")
	}
}

func TestResolveBackend_AutoRoutingErrorFallsToDefault(t *testing.T) {
	cfg := cfgWithBackends("claude", "claude", "codex")
	cfg.Routing.Mode = "auto"
	r := router.New(cfg)
	r.RouteFn = func(issue github.Issue) (string, string, error) {
		return "", "", fmt.Errorf("network error")
	}
	o := &Orchestrator{cfg: cfg, router: r}
	got := o.resolveBackend(makeIssue(7, "Fix bug"))
	if got != "claude" {
		t.Errorf("resolveBackend() = %q, want %q (should fall back on router error)", got, "claude")
	}
}

func TestResolveBackend_AutoRoutingDisabled(t *testing.T) {
	cfg := cfgWithBackends("claude", "claude", "codex")
	cfg.Routing.Mode = "manual"
	routerCalled := false
	r := router.New(cfg)
	r.RouteFn = func(issue github.Issue) (string, string, error) {
		routerCalled = true
		return "codex", "router pick", nil
	}
	o := &Orchestrator{cfg: cfg, router: r}
	got := o.resolveBackend(makeIssue(8, "Fix bug"))
	if got != "claude" {
		t.Errorf("resolveBackend() = %q, want %q", got, "claude")
	}
	if routerCalled {
		t.Error("router should not be called when routing mode is not auto")
	}
}

func TestResolveBackend_EmptyModelLabelIgnored(t *testing.T) {
	cfg := cfgWithBackends("claude", "claude", "codex")
	o := &Orchestrator{cfg: cfg, router: router.New(cfg)}
	// "model:" with no value after the colon should be ignored
	got := o.resolveBackend(makeIssue(9, "Fix bug", "model:"))
	if got != "claude" {
		t.Errorf("resolveBackend() = %q, want %q (empty model: label should be ignored)", got, "claude")
	}
}

func TestResolveBackend_MultipleLabelsFirstModelWins(t *testing.T) {
	cfg := cfgWithBackends("claude", "claude", "codex", "gemini")
	o := &Orchestrator{cfg: cfg, router: router.New(cfg)}
	got := o.resolveBackend(makeIssue(10, "Fix bug", "bug", "model:codex", "model:gemini"))
	if got != "codex" {
		t.Errorf("resolveBackend() = %q, want %q (first model: label should win)", got, "codex")
	}
}

// newMergeTestOrchestrator creates an Orchestrator wired with test fakes for
// autoMergePRs / mergeReadyPR. It records which PR numbers were merged and
// stubs CI + Greptile to always return "success" / approved.
func newMergeTestOrchestrator(cfg *config.Config, prs []github.PR) (*Orchestrator, *[]int) {
	merged := make([]int, 0)
	return &Orchestrator{
		cfg:      cfg,
		notifier: &notify.Notifier{},
		listOpenPRsFn: func() ([]github.PR, error) {
			return prs, nil
		},
		ghPRCIStatusFn: func(prNumber int) (string, error) {
			return "success", nil
		},
		ghPRGreptileApprovedFn: func(prNumber int) (bool, bool, error) {
			return true, false, nil // approved, not pending
		},
		ghMergePRFn: func(prNumber int) error {
			merged = append(merged, prNumber)
			return nil
		},
		ghCloseIssueFn: func(number int, comment string) error {
			return nil
		},
		workerStopFn: func(cfg *config.Config, slotName string, sess *state.Session) error {
			return nil
		},
	}, &merged
}

// makeTestState creates a State with N sessions in pr_open status, each mapped
// to the corresponding PR in prs (by index). Slot names are "slot-0", "slot-1", etc.
func makeTestState(prs []github.PR) *state.State {
	s := state.NewState()
	for i, pr := range prs {
		slotName := fmt.Sprintf("slot-%d", i)
		s.Sessions[slotName] = &state.Session{
			IssueNumber: 100 + i,
			IssueTitle:  fmt.Sprintf("issue %d", 100+i),
			Branch:      pr.HeadRefName,
			Status:      state.StatusPROpen,
			PRNumber:    pr.Number,
		}
	}
	return s
}

func TestAutoMergePRs_ParallelMergesAllReady(t *testing.T) {
	prs := []github.PR{
		{Number: 10, HeadRefName: "feat/a"},
		{Number: 20, HeadRefName: "feat/b"},
		{Number: 30, HeadRefName: "feat/c"},
	}

	cfg := &config.Config{Repo: "owner/repo", MergeStrategy: "parallel"}
	o, merged := newMergeTestOrchestrator(cfg, prs)
	s := makeTestState(prs)

	o.autoMergePRs(s)

	if len(*merged) != 3 {
		t.Fatalf("parallel mode merged %d PRs, want 3", len(*merged))
	}
	// Verify all three PR numbers are present (sorted by PR number)
	for i, want := range []int{10, 20, 30} {
		if (*merged)[i] != want {
			t.Errorf("merged[%d] = %d, want %d", i, (*merged)[i], want)
		}
	}
}

func TestAutoMergePRs_ParallelUpdatesState(t *testing.T) {
	prs := []github.PR{
		{Number: 10, HeadRefName: "feat/a"},
		{Number: 20, HeadRefName: "feat/b"},
	}

	cfg := &config.Config{Repo: "owner/repo", MergeStrategy: "parallel"}
	o, _ := newMergeTestOrchestrator(cfg, prs)
	s := makeTestState(prs)

	before := time.Now()
	o.autoMergePRs(s)

	// All sessions should be marked done
	for slotName, sess := range s.Sessions {
		if sess.Status != state.StatusDone {
			t.Errorf("session %s status = %q, want %q", slotName, sess.Status, state.StatusDone)
		}
		if sess.FinishedAt == nil {
			t.Errorf("session %s has nil FinishedAt", slotName)
		}
	}

	// LastMergeAt should be updated
	if s.LastMergeAt.Before(before) {
		t.Errorf("LastMergeAt = %v, expected after %v", s.LastMergeAt, before)
	}
}

func TestAutoMergePRs_ParallelIgnoresInterval(t *testing.T) {
	prs := []github.PR{
		{Number: 10, HeadRefName: "feat/a"},
		{Number: 20, HeadRefName: "feat/b"},
	}

	cfg := &config.Config{
		Repo:                 "owner/repo",
		MergeStrategy:        "parallel",
		MergeIntervalSeconds: 300, // 5 minutes — should be ignored in parallel mode
	}
	o, merged := newMergeTestOrchestrator(cfg, prs)
	s := makeTestState(prs)
	// Set LastMergeAt to 1 second ago — sequential would skip, parallel should not
	s.LastMergeAt = time.Now().Add(-1 * time.Second)

	o.autoMergePRs(s)

	if len(*merged) != 2 {
		t.Fatalf("parallel mode should ignore interval; merged %d PRs, want 2", len(*merged))
	}
}

func TestAutoMergePRs_ParallelMergeOrder(t *testing.T) {
	// PRs given in reverse order — should still merge in ascending PR number order
	prs := []github.PR{
		{Number: 30, HeadRefName: "feat/c"},
		{Number: 10, HeadRefName: "feat/a"},
		{Number: 20, HeadRefName: "feat/b"},
	}

	cfg := &config.Config{Repo: "owner/repo", MergeStrategy: "parallel"}
	o, merged := newMergeTestOrchestrator(cfg, prs)
	s := makeTestState(prs)

	o.autoMergePRs(s)

	want := []int{10, 20, 30}
	for i, w := range want {
		if (*merged)[i] != w {
			t.Errorf("merged[%d] = %d, want %d (should be sorted by PR number)", i, (*merged)[i], w)
		}
	}
}

func TestAutoMergePRs_ParallelPartialFailure(t *testing.T) {
	prs := []github.PR{
		{Number: 10, HeadRefName: "feat/a"},
		{Number: 20, HeadRefName: "feat/b"},
		{Number: 30, HeadRefName: "feat/c"},
	}

	cfg := &config.Config{Repo: "owner/repo", MergeStrategy: "parallel"}
	merged := make([]int, 0)
	o := &Orchestrator{
		cfg:      cfg,
		notifier: &notify.Notifier{},
		listOpenPRsFn: func() ([]github.PR, error) {
			return prs, nil
		},
		ghPRCIStatusFn: func(prNumber int) (string, error) {
			return "success", nil
		},
		ghPRGreptileApprovedFn: func(prNumber int) (bool, bool, error) {
			return true, false, nil
		},
		ghMergePRFn: func(prNumber int) error {
			if prNumber == 20 {
				return fmt.Errorf("merge conflict")
			}
			merged = append(merged, prNumber)
			return nil
		},
		ghCloseIssueFn: func(number int, comment string) error {
			return nil
		},
		workerStopFn: func(cfg *config.Config, slotName string, sess *state.Session) error {
			return nil
		},
	}
	s := makeTestState(prs)

	o.autoMergePRs(s)

	// PRs 10 and 30 should merge; PR 20 should fail
	if len(merged) != 2 {
		t.Fatalf("expected 2 successful merges, got %d", len(merged))
	}
	if merged[0] != 10 || merged[1] != 30 {
		t.Errorf("merged = %v, want [10, 30]", merged)
	}

	// Verify state: sessions for PR 10 and 30 should be done, PR 20 should still be pr_open
	doneCount := 0
	openCount := 0
	for _, sess := range s.Sessions {
		if sess.Status == state.StatusDone {
			doneCount++
		}
		if sess.Status == state.StatusPROpen {
			openCount++
		}
	}
	if doneCount != 2 {
		t.Errorf("expected 2 done sessions, got %d", doneCount)
	}
	if openCount != 1 {
		t.Errorf("expected 1 still-open session, got %d", openCount)
	}
}

func TestAutoMergePRs_ParallelStateConsistency(t *testing.T) {
	// Verify that after parallel merges, the state is consistent:
	// - All merged sessions are StatusDone with FinishedAt set
	// - LastMergeAt is recent
	// - No session is in an inconsistent intermediate state
	prs := []github.PR{
		{Number: 1, HeadRefName: "feat/one"},
		{Number: 2, HeadRefName: "feat/two"},
		{Number: 3, HeadRefName: "feat/three"},
		{Number: 4, HeadRefName: "feat/four"},
		{Number: 5, HeadRefName: "feat/five"},
	}

	cfg := &config.Config{Repo: "owner/repo", MergeStrategy: "parallel"}
	o, merged := newMergeTestOrchestrator(cfg, prs)
	s := makeTestState(prs)

	o.autoMergePRs(s)

	if len(*merged) != 5 {
		t.Fatalf("expected 5 merges, got %d", len(*merged))
	}

	for slotName, sess := range s.Sessions {
		if sess.Status != state.StatusDone {
			t.Errorf("session %s: status = %q, want %q", slotName, sess.Status, state.StatusDone)
		}
		if sess.FinishedAt == nil {
			t.Errorf("session %s: FinishedAt is nil", slotName)
		}
	}

	if s.LastMergeAt.IsZero() {
		t.Error("LastMergeAt should not be zero after parallel merges")
	}
}

func TestAutoMergePRs_SequentialMergesOnlyFirst(t *testing.T) {
	prs := []github.PR{
		{Number: 10, HeadRefName: "feat/a"},
		{Number: 20, HeadRefName: "feat/b"},
		{Number: 30, HeadRefName: "feat/c"},
	}

	cfg := &config.Config{Repo: "owner/repo", MergeStrategy: "sequential"}
	o, merged := newMergeTestOrchestrator(cfg, prs)
	s := makeTestState(prs)

	o.autoMergePRs(s)

	if len(*merged) != 1 {
		t.Fatalf("sequential mode merged %d PRs, want 1", len(*merged))
	}
	if (*merged)[0] != 10 {
		t.Errorf("sequential should merge lowest PR number first; merged PR #%d, want #10", (*merged)[0])
	}
}

func TestAutoMergePRs_SequentialRespectsInterval(t *testing.T) {
	prs := []github.PR{
		{Number: 10, HeadRefName: "feat/a"},
	}

	cfg := &config.Config{
		Repo:                 "owner/repo",
		MergeStrategy:        "sequential",
		MergeIntervalSeconds: 60,
	}
	o, merged := newMergeTestOrchestrator(cfg, prs)
	s := makeTestState(prs)
	// Last merge was 5 seconds ago, interval is 60s — should skip
	s.LastMergeAt = time.Now().Add(-5 * time.Second)

	o.autoMergePRs(s)

	if len(*merged) != 0 {
		t.Fatalf("sequential mode should respect interval; merged %d PRs, want 0", len(*merged))
	}
}

func TestAutoMergePRs_SequentialMergesAfterInterval(t *testing.T) {
	prs := []github.PR{
		{Number: 10, HeadRefName: "feat/a"},
		{Number: 20, HeadRefName: "feat/b"},
	}

	cfg := &config.Config{
		Repo:                 "owner/repo",
		MergeStrategy:        "sequential",
		MergeIntervalSeconds: 1,
	}
	o, merged := newMergeTestOrchestrator(cfg, prs)
	s := makeTestState(prs)
	// Last merge was 2 seconds ago, interval is 1s — should merge
	s.LastMergeAt = time.Now().Add(-2 * time.Second)

	o.autoMergePRs(s)

	if len(*merged) != 1 {
		t.Fatalf("sequential mode should merge after interval elapsed; merged %d PRs, want 1", len(*merged))
	}
	if (*merged)[0] != 10 {
		t.Errorf("merged PR #%d, want #10", (*merged)[0])
	}
}

func TestAutoMergePRs_SequentialFirstMergeNoWait(t *testing.T) {
	// When LastMergeAt is zero (no prior merges), sequential mode should merge immediately
	prs := []github.PR{
		{Number: 10, HeadRefName: "feat/a"},
	}

	cfg := &config.Config{
		Repo:                 "owner/repo",
		MergeStrategy:        "sequential",
		MergeIntervalSeconds: 300, // large interval
	}
	o, merged := newMergeTestOrchestrator(cfg, prs)
	s := makeTestState(prs)
	// LastMergeAt is zero — first ever merge

	o.autoMergePRs(s)

	if len(*merged) != 1 {
		t.Fatalf("sequential first merge should not wait; merged %d PRs, want 1", len(*merged))
	}
}

func TestAutoMergePRs_SkipsNonReadySessions(t *testing.T) {
	prs := []github.PR{
		{Number: 10, HeadRefName: "feat/a"},
		{Number: 20, HeadRefName: "feat/b"},
	}

	cfg := &config.Config{Repo: "owner/repo", MergeStrategy: "parallel"}
	o, merged := newMergeTestOrchestrator(cfg, prs)
	s := makeTestState(prs)

	// Mark one session as already done — should not be picked for merge
	for _, sess := range s.Sessions {
		if sess.PRNumber == 20 {
			sess.Status = state.StatusDone
		}
	}

	o.autoMergePRs(s)

	if len(*merged) != 1 {
		t.Fatalf("expected 1 merge (other session is done), got %d", len(*merged))
	}
	if (*merged)[0] != 10 {
		t.Errorf("merged PR #%d, want #10", (*merged)[0])
	}
}

func TestAutoMergePRs_QueuedSessionsAreEligible(t *testing.T) {
	prs := []github.PR{
		{Number: 10, HeadRefName: "feat/a"},
	}

	cfg := &config.Config{Repo: "owner/repo", MergeStrategy: "parallel"}
	o, merged := newMergeTestOrchestrator(cfg, prs)
	s := state.NewState()
	s.Sessions["slot-0"] = &state.Session{
		IssueNumber: 100,
		Branch:      "feat/a",
		Status:      state.StatusQueued,
		PRNumber:    10,
	}

	o.autoMergePRs(s)

	if len(*merged) != 1 {
		t.Fatalf("queued session should be eligible for merge; merged %d PRs, want 1", len(*merged))
	}
}

func TestAutoMergePRs_CIFailureBlocksMerge(t *testing.T) {
	prs := []github.PR{
		{Number: 10, HeadRefName: "feat/a"},
		{Number: 20, HeadRefName: "feat/b"},
	}

	cfg := &config.Config{Repo: "owner/repo", MergeStrategy: "parallel"}
	merged := make([]int, 0)
	o := &Orchestrator{
		cfg:      cfg,
		notifier: &notify.Notifier{},
		listOpenPRsFn: func() ([]github.PR, error) {
			return prs, nil
		},
		ghPRCIStatusFn: func(prNumber int) (string, error) {
			if prNumber == 10 {
				return "failure", nil
			}
			return "success", nil
		},
		ghPRGreptileApprovedFn: func(prNumber int) (bool, bool, error) {
			return true, false, nil
		},
		ghMergePRFn: func(prNumber int) error {
			merged = append(merged, prNumber)
			return nil
		},
		ghCloseIssueFn: func(number int, comment string) error {
			return nil
		},
		workerStopFn: func(cfg *config.Config, slotName string, sess *state.Session) error {
			return nil
		},
	}
	s := makeTestState(prs)

	o.autoMergePRs(s)

	if len(merged) != 1 {
		t.Fatalf("expected 1 merge (CI failing on PR #10), got %d", len(merged))
	}
	if merged[0] != 20 {
		t.Errorf("merged PR #%d, want #20", merged[0])
	}
}

func TestAutoMergePRs_ParallelAllFailures(t *testing.T) {
	// When every merge fails in parallel mode, no sessions should transition
	// to done, and LastMergeAt should remain unchanged.
	prs := []github.PR{
		{Number: 10, HeadRefName: "feat/a"},
		{Number: 20, HeadRefName: "feat/b"},
		{Number: 30, HeadRefName: "feat/c"},
	}

	cfg := &config.Config{Repo: "owner/repo", MergeStrategy: "parallel"}
	merged := make([]int, 0)
	o := &Orchestrator{
		cfg:      cfg,
		notifier: &notify.Notifier{},
		listOpenPRsFn: func() ([]github.PR, error) {
			return prs, nil
		},
		ghPRCIStatusFn: func(prNumber int) (string, error) {
			return "success", nil
		},
		ghPRGreptileApprovedFn: func(prNumber int) (bool, bool, error) {
			return true, false, nil
		},
		ghMergePRFn: func(prNumber int) error {
			return fmt.Errorf("merge conflict on PR #%d", prNumber)
		},
		ghCloseIssueFn: func(number int, comment string) error {
			return nil
		},
		workerStopFn: func(cfg *config.Config, slotName string, sess *state.Session) error {
			return nil
		},
	}
	s := makeTestState(prs)

	o.autoMergePRs(s)

	if len(merged) != 0 {
		t.Fatalf("expected 0 successful merges, got %d", len(merged))
	}

	// All sessions should remain in pr_open status
	for slotName, sess := range s.Sessions {
		if sess.Status != state.StatusPROpen {
			t.Errorf("session %s: status = %q, want %q", slotName, sess.Status, state.StatusPROpen)
		}
		if sess.FinishedAt != nil {
			t.Errorf("session %s: FinishedAt should be nil when merge failed", slotName)
		}
	}

	// LastMergeAt should remain zero (no successful merge)
	if !s.LastMergeAt.IsZero() {
		t.Errorf("LastMergeAt should be zero when all merges fail, got %v", s.LastMergeAt)
	}
}

// --- checkSessions: worker_max_tokens enforcement tests ---

// newCheckSessionsOrchestrator creates an Orchestrator wired with test fakes for
// checkSessions. The captureTmuxOutput is returned by the captureTmuxFn hook.
// The stopped slice records slot names of stopped workers.
func newCheckSessionsOrchestrator(cfg *config.Config, tmuxOutput string) (*Orchestrator, *[]string) {
	stopped := make([]string, 0)
	return &Orchestrator{
		cfg:      cfg,
		notifier: &notify.Notifier{},
		listOpenPRsFn: func() ([]github.PR, error) {
			return []github.PR{}, nil
		},
		isIssueClosedFn: func(issueNumber int) (bool, error) {
			return false, nil
		},
		pidAliveFn: func(pid int) bool {
			return true // worker is alive
		},
		captureTmuxFn: func(session string) (string, error) {
			return tmuxOutput, nil
		},
		workerStopFn: func(cfg *config.Config, slotName string, sess *state.Session) error {
			stopped = append(stopped, slotName)
			return nil
		},
	}, &stopped
}

func TestCheckSessions_TokenLimitExceeded_KillsWorker(t *testing.T) {
	cfg := &config.Config{
		Repo:              "owner/repo",
		WorkerMaxTokens:   50000,
		MaxRuntimeMinutes: 999,
	}
	// Worker output reports 75,000 tokens — exceeds 50,000 limit
	o, stopped := newCheckSessionsOrchestrator(cfg, "tokens 75000 (in 25000 / out 50000)")

	s := state.NewState()
	s.Sessions["mae-1"] = &state.Session{
		IssueNumber: 101,
		IssueTitle:  "test issue",
		Status:      state.StatusRunning,
		PID:         1234,
		TmuxSession: "maestro-mae-1",
		Branch:      "feat/mae-1-101-test",
		StartedAt:   time.Now().Add(-10 * time.Minute),
	}

	o.checkSessions(s)

	sess := s.Sessions["mae-1"]
	if sess.Status != state.StatusDead {
		t.Fatalf("status = %q, want %q", sess.Status, state.StatusDead)
	}
	if sess.LastNotifiedStatus != "token_limit" {
		t.Fatalf("last_notified_status = %q, want %q", sess.LastNotifiedStatus, "token_limit")
	}
	if sess.TokensUsed != 75000 {
		t.Fatalf("tokens_used = %d, want 75000", sess.TokensUsed)
	}
	if sess.FinishedAt == nil {
		t.Fatal("finished_at should be set")
	}
	if len(*stopped) != 1 || (*stopped)[0] != "mae-1" {
		t.Fatalf("stopped = %v, want [mae-1]", *stopped)
	}
}

func TestCheckSessions_TokensBelowLimit_WorkerSurvives(t *testing.T) {
	cfg := &config.Config{
		Repo:              "owner/repo",
		WorkerMaxTokens:   100000,
		MaxRuntimeMinutes: 999,
	}
	// Worker output reports 50,000 tokens — below 100,000 limit
	o, stopped := newCheckSessionsOrchestrator(cfg, "tokens 50000 (in 10000 / out 40000)")

	s := state.NewState()
	s.Sessions["mae-2"] = &state.Session{
		IssueNumber: 102,
		Status:      state.StatusRunning,
		PID:         2345,
		TmuxSession: "maestro-mae-2",
		Branch:      "feat/mae-2-102-test",
		StartedAt:   time.Now().Add(-5 * time.Minute),
	}

	o.checkSessions(s)

	sess := s.Sessions["mae-2"]
	if sess.Status != state.StatusRunning {
		t.Fatalf("status = %q, want %q", sess.Status, state.StatusRunning)
	}
	if sess.TokensUsed != 50000 {
		t.Fatalf("tokens_used = %d, want 50000", sess.TokensUsed)
	}
	if len(*stopped) != 0 {
		t.Fatalf("stopped = %v, want empty", *stopped)
	}
}

func TestCheckSessions_TokenLimitZero_NoEnforcement(t *testing.T) {
	cfg := &config.Config{
		Repo:              "owner/repo",
		WorkerMaxTokens:   0, // disabled
		MaxRuntimeMinutes: 999,
	}
	// Worker reports 999,999 tokens — but limit is disabled
	o, stopped := newCheckSessionsOrchestrator(cfg, "tokens 999999")

	s := state.NewState()
	s.Sessions["mae-3"] = &state.Session{
		IssueNumber: 103,
		Status:      state.StatusRunning,
		PID:         3456,
		TmuxSession: "maestro-mae-3",
		Branch:      "feat/mae-3-103-test",
		StartedAt:   time.Now().Add(-5 * time.Minute),
	}

	o.checkSessions(s)

	sess := s.Sessions["mae-3"]
	if sess.Status != state.StatusRunning {
		t.Fatalf("status = %q, want %q (limit disabled)", sess.Status, state.StatusRunning)
	}
	// Tokens should still be tracked even when limit is disabled
	if sess.TokensUsed != 999999 {
		t.Fatalf("tokens_used = %d, want 999999 (should track even when limit=0)", sess.TokensUsed)
	}
	if len(*stopped) != 0 {
		t.Fatalf("stopped = %v, want empty", *stopped)
	}
}

func TestCheckSessions_TokenLimitAlreadyNotified_NoDuplicateKill(t *testing.T) {
	cfg := &config.Config{
		Repo:              "owner/repo",
		WorkerMaxTokens:   50000,
		MaxRuntimeMinutes: 999,
	}
	o, stopped := newCheckSessionsOrchestrator(cfg, "tokens 75000")

	s := state.NewState()
	s.Sessions["mae-4"] = &state.Session{
		IssueNumber:        104,
		Status:             state.StatusRunning,
		PID:                4567,
		TmuxSession:        "maestro-mae-4",
		Branch:             "feat/mae-4-104-test",
		StartedAt:          time.Now().Add(-10 * time.Minute),
		TokensUsed:         75000,
		LastNotifiedStatus: "token_limit", // already notified
	}

	o.checkSessions(s)

	sess := s.Sessions["mae-4"]
	// Should remain running — the token_limit kill was already applied in a prior cycle
	if sess.Status != state.StatusRunning {
		t.Fatalf("status = %q, want %q (already notified, should not re-kill)", sess.Status, state.StatusRunning)
	}
	if len(*stopped) != 0 {
		t.Fatalf("stopped = %v, want empty (should not duplicate kill)", *stopped)
	}
}

func TestCheckSessions_TokensAtExactLimit_WorkerSurvives(t *testing.T) {
	cfg := &config.Config{
		Repo:              "owner/repo",
		WorkerMaxTokens:   50000,
		MaxRuntimeMinutes: 999,
	}
	// Worker output reports exactly 50,000 tokens — at limit, not over (strict >)
	o, stopped := newCheckSessionsOrchestrator(cfg, "tokens 50000")

	s := state.NewState()
	s.Sessions["mae-5"] = &state.Session{
		IssueNumber: 105,
		Status:      state.StatusRunning,
		PID:         5678,
		TmuxSession: "maestro-mae-5",
		Branch:      "feat/mae-5-105-test",
		StartedAt:   time.Now().Add(-5 * time.Minute),
	}

	o.checkSessions(s)

	sess := s.Sessions["mae-5"]
	if sess.Status != state.StatusRunning {
		t.Fatalf("status = %q, want %q (at exact limit, uses strict >)", sess.Status, state.StatusRunning)
	}
	if sess.TokensUsed != 50000 {
		t.Fatalf("tokens_used = %d, want 50000", sess.TokensUsed)
	}
	if len(*stopped) != 0 {
		t.Fatalf("stopped = %v, want empty", *stopped)
	}
}

func TestCheckSessions_TokenLimitOnlyExceedingSessionKilled(t *testing.T) {
	cfg := &config.Config{
		Repo:              "owner/repo",
		WorkerMaxTokens:   50000,
		MaxRuntimeMinutes: 999,
	}

	// Per-session tmux output: mae-6 is over limit, mae-7 is under
	tmuxOutputs := map[string]string{
		"maestro-mae-6": "tokens 75000 (in 25000 / out 50000)",
		"maestro-mae-7": "tokens 30000 (in 10000 / out 20000)",
	}
	stopped := make([]string, 0)
	o := &Orchestrator{
		cfg:      cfg,
		notifier: &notify.Notifier{},
		listOpenPRsFn: func() ([]github.PR, error) {
			return []github.PR{}, nil
		},
		isIssueClosedFn: func(issueNumber int) (bool, error) {
			return false, nil
		},
		pidAliveFn: func(pid int) bool {
			return true
		},
		captureTmuxFn: func(session string) (string, error) {
			if out, ok := tmuxOutputs[session]; ok {
				return out, nil
			}
			return "", nil
		},
		workerStopFn: func(cfg *config.Config, slotName string, sess *state.Session) error {
			stopped = append(stopped, slotName)
			return nil
		},
	}

	s := state.NewState()
	s.Sessions["mae-6"] = &state.Session{
		IssueNumber: 106,
		Status:      state.StatusRunning,
		PID:         6789,
		TmuxSession: "maestro-mae-6",
		Branch:      "feat/mae-6-106-over",
		StartedAt:   time.Now().Add(-10 * time.Minute),
	}
	s.Sessions["mae-7"] = &state.Session{
		IssueNumber: 107,
		Status:      state.StatusRunning,
		PID:         7890,
		TmuxSession: "maestro-mae-7",
		Branch:      "feat/mae-7-107-under",
		StartedAt:   time.Now().Add(-10 * time.Minute),
	}

	o.checkSessions(s)

	sess6 := s.Sessions["mae-6"]
	if sess6.Status != state.StatusDead {
		t.Fatalf("mae-6 status = %q, want %q", sess6.Status, state.StatusDead)
	}
	if sess6.TokensUsed != 75000 {
		t.Fatalf("mae-6 tokens_used = %d, want 75000", sess6.TokensUsed)
	}

	sess7 := s.Sessions["mae-7"]
	if sess7.Status != state.StatusRunning {
		t.Fatalf("mae-7 status = %q, want %q", sess7.Status, state.StatusRunning)
	}
	if sess7.TokensUsed != 30000 {
		t.Fatalf("mae-7 tokens_used = %d, want 30000", sess7.TokensUsed)
	}

	if len(stopped) != 1 || stopped[0] != "mae-6" {
		t.Fatalf("stopped = %v, want [mae-6]", stopped)
	}
}

func TestAutoMergePRs_ParallelStatePersistence(t *testing.T) {
	// Verify that state survives a save/load cycle after parallel merges.
	// This addresses the "race conditions on the state file" concern from issue #159.
	prs := []github.PR{
		{Number: 10, HeadRefName: "feat/a"},
		{Number: 20, HeadRefName: "feat/b"},
		{Number: 30, HeadRefName: "feat/c"},
	}

	cfg := &config.Config{Repo: "owner/repo", MergeStrategy: "parallel"}
	o, merged := newMergeTestOrchestrator(cfg, prs)
	s := makeTestState(prs)

	o.autoMergePRs(s)

	if len(*merged) != 3 {
		t.Fatalf("expected 3 merges, got %d", len(*merged))
	}

	// Save state to a temp directory and reload it
	stateDir := t.TempDir()
	if err := state.Save(stateDir, s); err != nil {
		t.Fatalf("save state: %v", err)
	}

	loaded, err := state.Load(stateDir)
	if err != nil {
		t.Fatalf("load state: %v", err)
	}

	// Verify loaded state matches in-memory state
	if len(loaded.Sessions) != len(s.Sessions) {
		t.Fatalf("loaded %d sessions, want %d", len(loaded.Sessions), len(s.Sessions))
	}

	for slotName, origSess := range s.Sessions {
		loadedSess, ok := loaded.Sessions[slotName]
		if !ok {
			t.Errorf("session %s missing after load", slotName)
			continue
		}
		if loadedSess.Status != origSess.Status {
			t.Errorf("session %s: loaded status = %q, want %q", slotName, loadedSess.Status, origSess.Status)
		}
		if loadedSess.FinishedAt == nil {
			t.Errorf("session %s: loaded FinishedAt is nil", slotName)
		}
		if loadedSess.PRNumber != origSess.PRNumber {
			t.Errorf("session %s: loaded PRNumber = %d, want %d", slotName, loadedSess.PRNumber, origSess.PRNumber)
		}
	}

	if loaded.LastMergeAt.IsZero() {
		t.Error("loaded LastMergeAt should not be zero")
	}
	// Time precision: JSON round-trip truncates to seconds on some platforms,
	// so check that the times are within 1 second of each other.
	diff := s.LastMergeAt.Sub(loaded.LastMergeAt)
	if diff < 0 {
		diff = -diff
	}
	if diff > time.Second {
		t.Errorf("LastMergeAt drift after round-trip: original=%v loaded=%v", s.LastMergeAt, loaded.LastMergeAt)
	}
}

func TestMergeReadyPR_BehindMainTriggersRebase(t *testing.T) {
	rebased := false
	o := &Orchestrator{
		cfg:      &config.Config{Repo: "owner/repo", AutoRebase: true},
		notifier: &notify.Notifier{},
		ghMergePRFn: func(prNumber int) error {
			return fmt.Errorf("gh pr merge 10: the head branch is not up to date with the base branch")
		},
		rebaseWorktreeFn: func(worktreePath, branch string, autoResolveFiles []string) error {
			rebased = true
			return nil
		},
	}

	sess := &state.Session{
		IssueNumber: 100,
		IssueTitle:  "test issue",
		Branch:      "feat/a",
		Worktree:    "/tmp/wt",
		Status:      state.StatusPROpen,
		PRNumber:    10,
	}
	pr := github.PR{Number: 10, HeadRefName: "feat/a"}

	result := o.mergeReadyPR("slot-0", sess, pr)

	if result {
		t.Fatal("mergeReadyPR should return false when merge fails")
	}
	if !rebased {
		t.Fatal("expected rebase to be triggered for 'not up to date' error")
	}
	if sess.Status != state.StatusQueued {
		t.Errorf("session status = %q, want %q", sess.Status, state.StatusQueued)
	}
	if !sess.RebaseAttempted {
		t.Error("RebaseAttempted should be true after successful rebase")
	}
}

func TestMergeReadyPR_BehindMainRebaseFailsMarksConflict(t *testing.T) {
	o := &Orchestrator{
		cfg:      &config.Config{Repo: "owner/repo", AutoRebase: true},
		notifier: &notify.Notifier{},
		gh:       github.New("owner/repo"),
		ghMergePRFn: func(prNumber int) error {
			return fmt.Errorf("gh pr merge 10: the head branch is not up to date with the base branch")
		},
		rebaseWorktreeFn: func(worktreePath, branch string, autoResolveFiles []string) error {
			return fmt.Errorf("rebase failed: conflict in main.go")
		},
	}

	sess := &state.Session{
		IssueNumber: 100,
		IssueTitle:  "test issue",
		Branch:      "feat/a",
		Worktree:    "/tmp/wt",
		Status:      state.StatusPROpen,
		PRNumber:    10,
	}
	pr := github.PR{Number: 10, HeadRefName: "feat/a"}

	result := o.mergeReadyPR("slot-0", sess, pr)

	if result {
		t.Fatal("mergeReadyPR should return false when rebase fails")
	}
	if sess.Status != state.StatusConflictFailed {
		t.Errorf("session status = %q, want %q", sess.Status, state.StatusConflictFailed)
	}
	if !sess.RebaseAttempted {
		t.Error("RebaseAttempted should be true after failed rebase")
	}
	if sess.FinishedAt == nil {
		t.Error("FinishedAt should be set for conflict_failed session")
	}
}

func TestMergeReadyPR_BehindMainNoAutoRebase(t *testing.T) {
	rebased := false
	o := &Orchestrator{
		cfg:      &config.Config{Repo: "owner/repo", AutoRebase: false},
		notifier: &notify.Notifier{},
		ghMergePRFn: func(prNumber int) error {
			return fmt.Errorf("gh pr merge 10: the head branch is not up to date with the base branch")
		},
		rebaseWorktreeFn: func(worktreePath, branch string, autoResolveFiles []string) error {
			rebased = true
			return nil
		},
	}

	sess := &state.Session{
		IssueNumber: 100,
		IssueTitle:  "test issue",
		Branch:      "feat/a",
		Worktree:    "/tmp/wt",
		Status:      state.StatusPROpen,
		PRNumber:    10,
	}
	pr := github.PR{Number: 10, HeadRefName: "feat/a"}

	result := o.mergeReadyPR("slot-0", sess, pr)

	if result {
		t.Fatal("mergeReadyPR should return false")
	}
	if rebased {
		t.Fatal("rebase should not be triggered when AutoRebase is disabled")
	}
	if sess.Status != state.StatusPROpen {
		t.Errorf("session status = %q, want %q (should stay pr_open)", sess.Status, state.StatusPROpen)
	}
}

func TestMergeReadyPR_OtherMergeErrorNoRebase(t *testing.T) {
	rebased := false
	o := &Orchestrator{
		cfg:      &config.Config{Repo: "owner/repo", AutoRebase: true},
		notifier: &notify.Notifier{},
		ghMergePRFn: func(prNumber int) error {
			return fmt.Errorf("gh pr merge 10: some other error")
		},
		rebaseWorktreeFn: func(worktreePath, branch string, autoResolveFiles []string) error {
			rebased = true
			return nil
		},
	}

	sess := &state.Session{
		IssueNumber: 100,
		IssueTitle:  "test issue",
		Branch:      "feat/a",
		Worktree:    "/tmp/wt",
		Status:      state.StatusPROpen,
		PRNumber:    10,
	}
	pr := github.PR{Number: 10, HeadRefName: "feat/a"}

	result := o.mergeReadyPR("slot-0", sess, pr)

	if result {
		t.Fatal("mergeReadyPR should return false")
	}
	if rebased {
		t.Fatal("rebase should not be triggered for non-'not up to date' errors")
	}
	if sess.Status != state.StatusPROpen {
		t.Errorf("session status = %q, want %q", sess.Status, state.StatusPROpen)
	}
	if sess.LastNotifiedStatus != "merge_failed" {
		t.Errorf("LastNotifiedStatus = %q, want %q", sess.LastNotifiedStatus, "merge_failed")
	}
}

// --- silent timeout tests ---

// newSilentTimeoutOrchestrator creates an Orchestrator wired for checkSessions
// testing. The tmux capture function returns the provided output string.
// It records whether stopWorker was called and which labels were added.
func newSilentTimeoutOrchestrator(timeoutMinutes int, tmuxOutput string) (*Orchestrator, *bool, *[]string) {
	stopped := false
	labels := make([]string, 0)
	return &Orchestrator{
		cfg: &config.Config{
			Repo:                       "owner/repo",
			WorkerSilentTimeoutMinutes: timeoutMinutes,
			MaxRuntimeMinutes:          120,
		},
		notifier:        &notify.Notifier{},
		pidAliveFn:      func(pid int) bool { return true },
		listOpenPRsFn:   func() ([]github.PR, error) { return nil, nil },
		isIssueClosedFn: func(number int) (bool, error) { return false, nil },
		tmuxCaptureFn:   func(session string) (string, error) { return tmuxOutput, nil },
		workerStopFn: func(cfg *config.Config, slotName string, sess *state.Session) error {
			stopped = true
			return nil
		},
		addIssueLabelFn: func(number int, label string) error {
			labels = append(labels, label)
			return nil
		},
	}, &stopped, &labels
}

func TestCheckSessions_SilentTimeoutKillsStuckWorker(t *testing.T) {
	output := "some static output\nline 2\nline 3"
	o, stopped, _ := newSilentTimeoutOrchestrator(10, output)

	s := state.NewState()
	s.Sessions["slot-1"] = &state.Session{
		IssueNumber:         42,
		IssueTitle:          "stuck worker",
		Status:              state.StatusRunning,
		PID:                 1234,
		TmuxSession:         "maestro-slot-1",
		Branch:              "feat/slot-1-42-stuck",
		StartedAt:           time.Now().Add(-30 * time.Minute),
		LastOutputHash:      hashOutput(output),                // same hash as current output
		LastOutputChangedAt: time.Now().Add(-15 * time.Minute), // 15 min ago > 10 min timeout
	}

	o.checkSessions(s)

	sess := s.Sessions["slot-1"]
	if !*stopped {
		t.Fatal("expected worker to be stopped")
	}
	if sess.Status != state.StatusDead {
		t.Errorf("status = %q, want %q", sess.Status, state.StatusDead)
	}
	if sess.LastNotifiedStatus != "silent_timeout" {
		t.Errorf("LastNotifiedStatus = %q, want %q", sess.LastNotifiedStatus, "silent_timeout")
	}
	if sess.FinishedAt == nil {
		t.Error("FinishedAt should be set")
	}
}

func TestCheckSessions_SilentTimeoutWithinTimeout_NoKill(t *testing.T) {
	output := "some static output\nline 2\nline 3"
	o, stopped, _ := newSilentTimeoutOrchestrator(10, output)

	s := state.NewState()
	s.Sessions["slot-1"] = &state.Session{
		IssueNumber:         42,
		IssueTitle:          "not yet stuck",
		Status:              state.StatusRunning,
		PID:                 1234,
		TmuxSession:         "maestro-slot-1",
		Branch:              "feat/slot-1-42-not-stuck",
		StartedAt:           time.Now().Add(-30 * time.Minute),
		LastOutputHash:      hashOutput(output),
		LastOutputChangedAt: time.Now().Add(-5 * time.Minute), // 5 min ago < 10 min timeout
	}

	o.checkSessions(s)

	sess := s.Sessions["slot-1"]
	if *stopped {
		t.Fatal("worker should NOT be stopped within timeout")
	}
	if sess.Status != state.StatusRunning {
		t.Errorf("status = %q, want %q", sess.Status, state.StatusRunning)
	}
}

func TestCheckSessions_SilentTimeoutOutputChanges_NoKill(t *testing.T) {
	// Tmux returns different output than last recorded hash
	o, stopped, _ := newSilentTimeoutOrchestrator(10, "new output line\nline 2")

	s := state.NewState()
	s.Sessions["slot-1"] = &state.Session{
		IssueNumber:         42,
		IssueTitle:          "active worker",
		Status:              state.StatusRunning,
		PID:                 1234,
		TmuxSession:         "maestro-slot-1",
		Branch:              "feat/slot-1-42-active",
		StartedAt:           time.Now().Add(-30 * time.Minute),
		LastOutputHash:      hashOutput("old output"), // different from current
		LastOutputChangedAt: time.Now().Add(-15 * time.Minute),
	}

	o.checkSessions(s)

	sess := s.Sessions["slot-1"]
	if *stopped {
		t.Fatal("worker should NOT be stopped when output changes")
	}
	if sess.Status != state.StatusRunning {
		t.Errorf("status = %q, want %q", sess.Status, state.StatusRunning)
	}
	// Hash should be updated to new output
	if sess.LastOutputHash != hashOutput("new output line\nline 2") {
		t.Error("LastOutputHash should be updated to new output hash")
	}
}

func TestCheckSessions_SilentTimeoutDisabled_NoKill(t *testing.T) {
	output := "static output"
	o, stopped, _ := newSilentTimeoutOrchestrator(0, output) // timeout=0 means disabled

	s := state.NewState()
	s.Sessions["slot-1"] = &state.Session{
		IssueNumber:         42,
		IssueTitle:          "no timeout",
		Status:              state.StatusRunning,
		PID:                 1234,
		TmuxSession:         "maestro-slot-1",
		Branch:              "feat/slot-1-42-no-timeout",
		StartedAt:           time.Now().Add(-30 * time.Minute),
		LastOutputHash:      hashOutput(output),
		LastOutputChangedAt: time.Now().Add(-60 * time.Minute), // way past any timeout
	}

	o.checkSessions(s)

	sess := s.Sessions["slot-1"]
	if *stopped {
		t.Fatal("worker should NOT be stopped when timeout is disabled (0)")
	}
	if sess.Status != state.StatusRunning {
		t.Errorf("status = %q, want %q", sess.Status, state.StatusRunning)
	}
}

func TestCheckSessions_SilentTimeoutFirstKill_NoBlockedLabel(t *testing.T) {
	output := "static output"
	o, _, labels := newSilentTimeoutOrchestrator(10, output)

	s := state.NewState()
	// Only one session for this issue — first silent timeout
	s.Sessions["slot-1"] = &state.Session{
		IssueNumber:         42,
		IssueTitle:          "first timeout",
		Status:              state.StatusRunning,
		PID:                 1234,
		TmuxSession:         "maestro-slot-1",
		Branch:              "feat/slot-1-42-first",
		StartedAt:           time.Now().Add(-30 * time.Minute),
		LastOutputHash:      hashOutput(output),
		LastOutputChangedAt: time.Now().Add(-15 * time.Minute),
	}

	o.checkSessions(s)

	sess := s.Sessions["slot-1"]
	if sess.Status != state.StatusDead {
		t.Errorf("status = %q, want %q", sess.Status, state.StatusDead)
	}
	// First silent timeout should NOT add "blocked" label
	for _, label := range *labels {
		if label == "blocked" {
			t.Error("first silent timeout should NOT add 'blocked' label")
		}
	}
}

func TestCheckSessions_SilentTimeoutSecondKill_LabelsBlocked(t *testing.T) {
	output := "static output"
	o, _, labels := newSilentTimeoutOrchestrator(10, output)

	s := state.NewState()
	// Previous silent timeout for same issue
	s.Sessions["slot-old"] = &state.Session{
		IssueNumber:        42,
		LastNotifiedStatus: "silent_timeout",
		Status:             state.StatusDead,
	}
	// Current running session — will be killed
	s.Sessions["slot-1"] = &state.Session{
		IssueNumber:         42,
		IssueTitle:          "second timeout",
		Status:              state.StatusRunning,
		PID:                 1234,
		TmuxSession:         "maestro-slot-1",
		Branch:              "feat/slot-1-42-second",
		StartedAt:           time.Now().Add(-30 * time.Minute),
		LastOutputHash:      hashOutput(output),
		LastOutputChangedAt: time.Now().Add(-15 * time.Minute),
	}

	o.checkSessions(s)

	sess := s.Sessions["slot-1"]
	if sess.Status != state.StatusDead {
		t.Errorf("status = %q, want %q", sess.Status, state.StatusDead)
	}
	// Second silent timeout SHOULD add "blocked" label
	found := false
	for _, label := range *labels {
		if label == "blocked" {
			found = true
		}
	}
	if !found {
		t.Error("second silent timeout should add 'blocked' label")
	}
}

// --- cleanup_worktrees_on_merge tests ---

func TestMergeReadyPR_CleansUpWorktreeOnMerge(t *testing.T) {
	cleanupTrue := true
	stopped := false
	o := &Orchestrator{
		cfg: &config.Config{
			Repo:                    "owner/repo",
			CleanupWorktreesOnMerge: &cleanupTrue,
		},
		notifier: &notify.Notifier{},
		ghMergePRFn: func(prNumber int) error {
			return nil
		},
		ghCloseIssueFn: func(number int, comment string) error {
			return nil
		},
		workerStopFn: func(cfg *config.Config, slotName string, sess *state.Session) error {
			stopped = true
			return nil
		},
	}

	sess := &state.Session{
		IssueNumber: 100,
		IssueTitle:  "test issue",
		Branch:      "feat/a",
		Worktree:    "/tmp/wt",
		Status:      state.StatusPROpen,
		PRNumber:    10,
	}
	pr := github.PR{Number: 10, HeadRefName: "feat/a"}

	result := o.mergeReadyPR("slot-0", sess, pr)

	if !result {
		t.Fatal("mergeReadyPR should return true on successful merge")
	}
	if !stopped {
		t.Fatal("worker should be stopped when cleanup_worktrees_on_merge is true")
	}
	if sess.Worktree != "" {
		t.Errorf("Worktree = %q, want empty (should be cleared after cleanup)", sess.Worktree)
	}
	if sess.Status != state.StatusDone {
		t.Errorf("Status = %q, want %q", sess.Status, state.StatusDone)
	}
}

func TestMergeReadyPR_SkipsCleanupWhenDisabled(t *testing.T) {
	cleanupFalse := false
	stopped := false
	o := &Orchestrator{
		cfg: &config.Config{
			Repo:                    "owner/repo",
			CleanupWorktreesOnMerge: &cleanupFalse,
		},
		notifier: &notify.Notifier{},
		ghMergePRFn: func(prNumber int) error {
			return nil
		},
		ghCloseIssueFn: func(number int, comment string) error {
			return nil
		},
		workerStopFn: func(cfg *config.Config, slotName string, sess *state.Session) error {
			stopped = true
			return nil
		},
	}

	sess := &state.Session{
		IssueNumber: 100,
		IssueTitle:  "test issue",
		Branch:      "feat/a",
		Worktree:    "/tmp/wt",
		Status:      state.StatusPROpen,
		PRNumber:    10,
	}
	pr := github.PR{Number: 10, HeadRefName: "feat/a"}

	result := o.mergeReadyPR("slot-0", sess, pr)

	if !result {
		t.Fatal("mergeReadyPR should return true on successful merge")
	}
	if stopped {
		t.Fatal("worker should NOT be stopped when cleanup_worktrees_on_merge is false")
	}
	if sess.Worktree != "/tmp/wt" {
		t.Errorf("Worktree = %q, want %q (should be preserved)", sess.Worktree, "/tmp/wt")
	}
	if sess.Status != state.StatusDone {
		t.Errorf("Status = %q, want %q", sess.Status, state.StatusDone)
	}
}

func TestMergeReadyPR_DefaultConfigCleansUp(t *testing.T) {
	// Config with nil CleanupWorktreesOnMerge should default to true
	stopped := false
	cfg := &config.Config{Repo: "owner/repo"}
	// Simulate default: ShouldCleanupWorktrees returns true for nil pointer
	o := &Orchestrator{
		cfg:      cfg,
		notifier: &notify.Notifier{},
		ghMergePRFn: func(prNumber int) error {
			return nil
		},
		ghCloseIssueFn: func(number int, comment string) error {
			return nil
		},
		workerStopFn: func(cfg *config.Config, slotName string, sess *state.Session) error {
			stopped = true
			return nil
		},
	}

	sess := &state.Session{
		IssueNumber: 100,
		IssueTitle:  "test issue",
		Branch:      "feat/a",
		Worktree:    "/tmp/wt",
		Status:      state.StatusPROpen,
		PRNumber:    10,
	}
	pr := github.PR{Number: 10, HeadRefName: "feat/a"}

	result := o.mergeReadyPR("slot-0", sess, pr)

	if !result {
		t.Fatal("mergeReadyPR should return true on successful merge")
	}
	if !stopped {
		t.Fatal("worker should be stopped with default config (nil = true)")
	}
	if sess.Worktree != "" {
		t.Errorf("Worktree = %q, want empty", sess.Worktree)
	}
}

func TestCheckSessions_SilentTimeoutFirstObservation_SetsHash(t *testing.T) {
	output := "initial output\nline 2"
	o, stopped, _ := newSilentTimeoutOrchestrator(10, output)

	s := state.NewState()
	s.Sessions["slot-1"] = &state.Session{
		IssueNumber: 42,
		IssueTitle:  "new worker",
		Status:      state.StatusRunning,
		PID:         1234,
		TmuxSession: "maestro-slot-1",
		Branch:      "feat/slot-1-42-new",
		StartedAt:   time.Now().Add(-5 * time.Minute),
		// LastOutputHash and LastOutputChangedAt are zero values (first observation)
	}

	o.checkSessions(s)

	sess := s.Sessions["slot-1"]
	if *stopped {
		t.Fatal("worker should NOT be stopped on first observation")
	}
	if sess.Status != state.StatusRunning {
		t.Errorf("status = %q, want %q", sess.Status, state.StatusRunning)
	}
	if sess.LastOutputHash == "" {
		t.Error("LastOutputHash should be set on first observation")
	}
	if sess.LastOutputHash != hashOutput(output) {
		t.Errorf("LastOutputHash = %q, want hash of output", sess.LastOutputHash)
	}
	if sess.LastOutputChangedAt.IsZero() {
		t.Error("LastOutputChangedAt should be set on first observation")
	}
}

func TestCheckSessions_SilentTimeoutTmuxCaptureFails_NoKill(t *testing.T) {
	o := &Orchestrator{
		cfg: &config.Config{
			Repo:                       "owner/repo",
			WorkerSilentTimeoutMinutes: 10,
			MaxRuntimeMinutes:          120,
		},
		notifier:        &notify.Notifier{},
		pidAliveFn:      func(pid int) bool { return true },
		listOpenPRsFn:   func() ([]github.PR, error) { return nil, nil },
		isIssueClosedFn: func(number int) (bool, error) { return false, nil },
		tmuxCaptureFn: func(session string) (string, error) {
			return "", fmt.Errorf("tmux session not found")
		},
		workerStopFn: func(cfg *config.Config, slotName string, sess *state.Session) error {
			t.Fatal("stopWorker should not be called when tmux capture fails")
			return nil
		},
	}

	output := "static output"
	s := state.NewState()
	s.Sessions["slot-1"] = &state.Session{
		IssueNumber:         42,
		IssueTitle:          "tmux broken",
		Status:              state.StatusRunning,
		PID:                 1234,
		TmuxSession:         "maestro-slot-1",
		Branch:              "feat/slot-1-42-tmux-broken",
		StartedAt:           time.Now().Add(-30 * time.Minute),
		LastOutputHash:      hashOutput(output),
		LastOutputChangedAt: time.Now().Add(-15 * time.Minute), // past timeout
	}

	o.checkSessions(s)

	sess := s.Sessions["slot-1"]
	if sess.Status != state.StatusRunning {
		t.Errorf("status = %q, want %q — worker must survive tmux capture failure", sess.Status, state.StatusRunning)
	}
}

func TestCheckSessions_SilentTimeoutStopFails_StillMarksDead(t *testing.T) {
	output := "static output"
	o := &Orchestrator{
		cfg: &config.Config{
			Repo:                       "owner/repo",
			WorkerSilentTimeoutMinutes: 10,
			MaxRuntimeMinutes:          120,
		},
		notifier:        &notify.Notifier{},
		pidAliveFn:      func(pid int) bool { return true },
		listOpenPRsFn:   func() ([]github.PR, error) { return nil, nil },
		isIssueClosedFn: func(number int) (bool, error) { return false, nil },
		tmuxCaptureFn:   func(session string) (string, error) { return output, nil },
		workerStopFn: func(cfg *config.Config, slotName string, sess *state.Session) error {
			return fmt.Errorf("permission denied")
		},
		addIssueLabelFn: func(number int, label string) error { return nil },
	}

	s := state.NewState()
	s.Sessions["slot-1"] = &state.Session{
		IssueNumber:         42,
		IssueTitle:          "stop will fail",
		Status:              state.StatusRunning,
		PID:                 1234,
		TmuxSession:         "maestro-slot-1",
		Branch:              "feat/slot-1-42-stop-fail",
		StartedAt:           time.Now().Add(-30 * time.Minute),
		LastOutputHash:      hashOutput(output),
		LastOutputChangedAt: time.Now().Add(-15 * time.Minute),
	}

	o.checkSessions(s)

	sess := s.Sessions["slot-1"]
	if sess.Status != state.StatusDead {
		t.Errorf("status = %q, want %q — session must be marked dead even if stop fails", sess.Status, state.StatusDead)
	}
	if sess.LastNotifiedStatus != "silent_timeout" {
		t.Errorf("LastNotifiedStatus = %q, want %q", sess.LastNotifiedStatus, "silent_timeout")
	}
	if sess.FinishedAt == nil {
		t.Error("FinishedAt should be set even when stop fails")
	}
}

func TestHashOutput_FewerThan50Lines(t *testing.T) {
	short := "line1\nline2\nline3"
	h1 := hashOutput(short)
	h2 := hashOutput(short)
	if h1 != h2 {
		t.Fatal("hashOutput should be deterministic")
	}
	if h1 == "" {
		t.Fatal("hashOutput should not return empty string")
	}
}

func TestHashOutput_EmptyString(t *testing.T) {
	h := hashOutput("")
	if h == "" {
		t.Fatal("hashOutput should not return empty string for empty input")
	}
}

func TestCountSilentTimeoutKillsForIssue_NoMatches(t *testing.T) {
	s := state.NewState()
	s.Sessions["slot-1"] = &state.Session{IssueNumber: 10, LastNotifiedStatus: "ci_failure"}
	s.Sessions["slot-2"] = &state.Session{IssueNumber: 20, LastNotifiedStatus: "silent_timeout"}

	if got := countSilentTimeoutKillsForIssue(s, 10); got != 0 {
		t.Fatalf("countSilentTimeoutKillsForIssue(10) = %d, want 0 (ci_failure != silent_timeout)", got)
	}
	if got := countSilentTimeoutKillsForIssue(s, 99); got != 0 {
		t.Fatalf("countSilentTimeoutKillsForIssue(99) = %d, want 0 (no sessions for issue)", got)
	}
}

// --- retry limit tests ---

// newStartWorkersOrchestrator creates an Orchestrator wired with test fakes for
// startNewWorkers. It returns the orchestrator, a slice of started issue numbers,
// and a slice of labels added.
func newStartWorkersOrchestrator(cfg *config.Config, issues []github.Issue) (*Orchestrator, *[]int, *[]string) {
	started := make([]int, 0)
	labels := make([]string, 0)
	slotCounter := 0
	return &Orchestrator{
		cfg:      cfg,
		notifier: &notify.Notifier{},
		router:   router.New(cfg),
		listOpenIssuesFn: func(labelFilter []string) ([]github.Issue, error) {
			return issues, nil
		},
		hasOpenPRForIssueFn: func(issueNumber int) (bool, error) {
			return false, nil
		},
		addIssueLabelFn: func(number int, label string) error {
			labels = append(labels, fmt.Sprintf("#%d:%s", number, label))
			return nil
		},
		workerStartFn: func(cfg *config.Config, s *state.State, repo string, issue github.Issue, promptBase, backend string) (string, error) {
			slotCounter++
			slotName := fmt.Sprintf("slot-%d", slotCounter)
			s.Sessions[slotName] = &state.Session{
				IssueNumber: issue.Number,
				IssueTitle:  issue.Title,
				Status:      state.StatusRunning,
				PID:         1000 + slotCounter,
				Branch:      fmt.Sprintf("feat/%s", slotName),
				StartedAt:   time.Now().UTC(),
			}
			started = append(started, issue.Number)
			return slotName, nil
		},
	}, &started, &labels
}

func TestStartNewWorkers_SkipsRetryExhaustedIssue(t *testing.T) {
	cfg := cfgWithBackends("claude", "claude")
	cfg.MaxRetriesPerIssue = 3

	issues := []github.Issue{
		makeIssue(42, "failing issue"),
		makeIssue(43, "fresh issue"),
	}

	o, started, labels := newStartWorkersOrchestrator(cfg, issues)
	s := state.NewState()

	// Simulate 3 prior failed attempts for issue #42 (dead without PR)
	now := time.Now().UTC()
	for i := 0; i < 3; i++ {
		slotName := fmt.Sprintf("old-%d", i)
		finished := now.Add(-time.Duration(3-i) * time.Hour)
		s.Sessions[slotName] = &state.Session{
			IssueNumber: 42,
			Status:      state.StatusDead,
			PRNumber:    0,
			StartedAt:   finished.Add(-30 * time.Minute),
			FinishedAt:  &finished,
		}
	}

	o.startNewWorkers(s, 5)

	// Only issue #43 should be started
	if len(*started) != 1 {
		t.Fatalf("started %d workers, want 1", len(*started))
	}
	if (*started)[0] != 43 {
		t.Errorf("started issue #%d, want #43", (*started)[0])
	}

	// Issue #42 should be labeled blocked
	foundBlocked := false
	for _, label := range *labels {
		if label == "#42:blocked" {
			foundBlocked = true
		}
	}
	if !foundBlocked {
		t.Errorf("expected issue #42 to be labeled blocked, labels = %v", *labels)
	}

	// The most recent dead session for issue #42 should be marked retry_exhausted
	if !s.IssueRetryExhausted(42) {
		t.Error("issue #42 should have a retry_exhausted session")
	}
}

func TestStartNewWorkers_RetryLimitDisabledWhenZero(t *testing.T) {
	cfg := cfgWithBackends("claude", "claude")
	cfg.MaxRetriesPerIssue = 0 // unlimited

	issues := []github.Issue{
		makeIssue(42, "failing issue"),
	}

	o, started, _ := newStartWorkersOrchestrator(cfg, issues)
	s := state.NewState()

	// 10 prior failures — should still spawn because limit is disabled
	now := time.Now().UTC()
	for i := 0; i < 10; i++ {
		slotName := fmt.Sprintf("old-%d", i)
		finished := now.Add(-time.Duration(10-i) * time.Hour)
		s.Sessions[slotName] = &state.Session{
			IssueNumber: 42,
			Status:      state.StatusDead,
			PRNumber:    0,
			StartedAt:   finished.Add(-30 * time.Minute),
			FinishedAt:  &finished,
		}
	}

	o.startNewWorkers(s, 5)

	if len(*started) != 1 {
		t.Fatalf("started %d workers, want 1 (limit disabled)", len(*started))
	}
}

func TestStartNewWorkers_RetryExhaustedNotifiesOnce(t *testing.T) {
	cfg := cfgWithBackends("claude", "claude")
	cfg.MaxRetriesPerIssue = 2

	issues := []github.Issue{
		makeIssue(42, "failing issue"),
	}

	o, _, labels := newStartWorkersOrchestrator(cfg, issues)
	s := state.NewState()

	// 2 prior failures
	now := time.Now().UTC()
	for i := 0; i < 2; i++ {
		slotName := fmt.Sprintf("old-%d", i)
		finished := now.Add(-time.Duration(2-i) * time.Hour)
		s.Sessions[slotName] = &state.Session{
			IssueNumber: 42,
			Status:      state.StatusDead,
			PRNumber:    0,
			StartedAt:   finished.Add(-30 * time.Minute),
			FinishedAt:  &finished,
		}
	}

	// First cycle: should mark retry_exhausted and label blocked
	o.startNewWorkers(s, 5)
	if !s.IssueRetryExhausted(42) {
		t.Fatal("issue #42 should be retry_exhausted after first detection")
	}
	firstLabelCount := len(*labels)
	if firstLabelCount == 0 {
		t.Fatal("expected blocked label on first detection")
	}

	// Second cycle: should skip but NOT re-label or re-notify
	o.startNewWorkers(s, 5)
	if len(*labels) != firstLabelCount {
		t.Errorf("labels added on second cycle: %v (should not duplicate)", *labels)
	}
}

func TestStartNewWorkers_BelowLimitStillSpawns(t *testing.T) {
	cfg := cfgWithBackends("claude", "claude")
	cfg.MaxRetriesPerIssue = 3

	issues := []github.Issue{
		makeIssue(42, "failing issue"),
	}

	o, started, _ := newStartWorkersOrchestrator(cfg, issues)
	s := state.NewState()

	// Only 2 prior failures — below limit of 3
	now := time.Now().UTC()
	for i := 0; i < 2; i++ {
		slotName := fmt.Sprintf("old-%d", i)
		finished := now.Add(-time.Duration(2-i) * time.Hour)
		s.Sessions[slotName] = &state.Session{
			IssueNumber: 42,
			Status:      state.StatusDead,
			PRNumber:    0,
			StartedAt:   finished.Add(-30 * time.Minute),
			FinishedAt:  &finished,
		}
	}

	o.startNewWorkers(s, 5)

	if len(*started) != 1 {
		t.Fatalf("started %d workers, want 1 (below retry limit)", len(*started))
	}
	if (*started)[0] != 42 {
		t.Errorf("started issue #%d, want #42", (*started)[0])
	}
}

func TestStartNewWorkers_FailedWithPRNotCounted(t *testing.T) {
	cfg := cfgWithBackends("claude", "claude")
	cfg.MaxRetriesPerIssue = 2

	issues := []github.Issue{
		makeIssue(42, "issue with PR failures"),
	}

	o, started, _ := newStartWorkersOrchestrator(cfg, issues)
	s := state.NewState()

	// 3 "failed" sessions, but all have PRs — should NOT count toward retry limit
	now := time.Now().UTC()
	for i := 0; i < 3; i++ {
		slotName := fmt.Sprintf("old-%d", i)
		finished := now.Add(-time.Duration(3-i) * time.Hour)
		s.Sessions[slotName] = &state.Session{
			IssueNumber: 42,
			Status:      state.StatusFailed,
			PRNumber:    100 + i, // has PR
			StartedAt:   finished.Add(-30 * time.Minute),
			FinishedAt:  &finished,
		}
	}

	o.startNewWorkers(s, 5)

	// Should still spawn because failed-with-PR doesn't count
	if len(*started) != 1 {
		t.Fatalf("started %d workers, want 1 (PR failures don't count)", len(*started))
	}
}

// --- zombie session cleanup tests (#187) ---

// TestCheckSessions_ConflictFailedClosedIssue_TransitionsToDone verifies that
// a conflict_failed session whose issue is closed gets transitioned to done,
// freeing the slot and preventing zombie sessions.
func TestCheckSessions_ConflictFailedClosedIssue_TransitionsToDone(t *testing.T) {
	now := time.Now().UTC()
	o := &Orchestrator{
		cfg:      &config.Config{Repo: "owner/repo", MaxRuntimeMinutes: 120},
		notifier: &notify.Notifier{},
		listOpenPRsFn: func() ([]github.PR, error) {
			return []github.PR{}, nil
		},
		isIssueClosedFn: func(issueNumber int) (bool, error) {
			return true, nil // issue is closed
		},
		workerStopFn: func(cfg *config.Config, slotName string, sess *state.Session) error {
			return nil
		},
	}

	s := state.NewState()
	s.Sessions["pan-59"] = &state.Session{
		IssueNumber:     263,
		IssueTitle:      "stuck conflict",
		Status:          state.StatusConflictFailed,
		Branch:          "feat/pan-59-263-stuck",
		RebaseAttempted: true,
		FinishedAt:      &now,
	}

	o.checkSessions(s)

	sess := s.Sessions["pan-59"]
	if sess.Status != state.StatusDone {
		t.Fatalf("status = %q, want %q", sess.Status, state.StatusDone)
	}
}

// TestCheckSessions_FailedClosedIssue_TransitionsToDone verifies that
// a failed session whose issue is closed gets transitioned to done.
func TestCheckSessions_FailedClosedIssue_TransitionsToDone(t *testing.T) {
	now := time.Now().UTC()
	o := &Orchestrator{
		cfg:      &config.Config{Repo: "owner/repo", MaxRuntimeMinutes: 120},
		notifier: &notify.Notifier{},
		listOpenPRsFn: func() ([]github.PR, error) {
			return []github.PR{}, nil
		},
		isIssueClosedFn: func(issueNumber int) (bool, error) {
			return true, nil
		},
		workerStopFn: func(cfg *config.Config, slotName string, sess *state.Session) error {
			return nil
		},
	}

	s := state.NewState()
	s.Sessions["pan-10"] = &state.Session{
		IssueNumber: 100,
		IssueTitle:  "failed worker",
		Status:      state.StatusFailed,
		Branch:      "feat/pan-10-100-failed",
		FinishedAt:  &now,
	}

	o.checkSessions(s)

	sess := s.Sessions["pan-10"]
	if sess.Status != state.StatusDone {
		t.Fatalf("status = %q, want %q", sess.Status, state.StatusDone)
	}
}

// TestCheckSessions_DeadClosedIssue_TransitionsToDone verifies that
// a dead session whose issue is closed gets transitioned to done.
func TestCheckSessions_DeadClosedIssue_TransitionsToDone(t *testing.T) {
	now := time.Now().UTC()
	o := &Orchestrator{
		cfg:      &config.Config{Repo: "owner/repo", MaxRuntimeMinutes: 120},
		notifier: &notify.Notifier{},
		listOpenPRsFn: func() ([]github.PR, error) {
			return []github.PR{}, nil
		},
		isIssueClosedFn: func(issueNumber int) (bool, error) {
			return true, nil
		},
		workerStopFn: func(cfg *config.Config, slotName string, sess *state.Session) error {
			return nil
		},
	}

	s := state.NewState()
	s.Sessions["pan-11"] = &state.Session{
		IssueNumber: 101,
		IssueTitle:  "dead worker",
		Status:      state.StatusDead,
		Branch:      "feat/pan-11-101-dead",
		FinishedAt:  &now,
	}

	o.checkSessions(s)

	sess := s.Sessions["pan-11"]
	if sess.Status != state.StatusDone {
		t.Fatalf("status = %q, want %q", sess.Status, state.StatusDone)
	}
}

// TestCheckSessions_ConflictFailedOpenIssue_StaysConflictFailed verifies that
// a conflict_failed session whose issue is still open remains conflict_failed.
func TestCheckSessions_ConflictFailedOpenIssue_StaysConflictFailed(t *testing.T) {
	now := time.Now().UTC()
	o := &Orchestrator{
		cfg:      &config.Config{Repo: "owner/repo", MaxRuntimeMinutes: 120},
		notifier: &notify.Notifier{},
		listOpenPRsFn: func() ([]github.PR, error) {
			return []github.PR{}, nil
		},
		isIssueClosedFn: func(issueNumber int) (bool, error) {
			return false, nil // issue is open
		},
		workerStopFn: func(cfg *config.Config, slotName string, sess *state.Session) error {
			return nil
		},
	}

	s := state.NewState()
	s.Sessions["pan-60"] = &state.Session{
		IssueNumber:     264,
		IssueTitle:      "conflict but open",
		Status:          state.StatusConflictFailed,
		Branch:          "feat/pan-60-264-conflict",
		RebaseAttempted: true,
		FinishedAt:      &now,
	}

	o.checkSessions(s)

	sess := s.Sessions["pan-60"]
	if sess.Status != state.StatusConflictFailed {
		t.Fatalf("status = %q, want %q", sess.Status, state.StatusConflictFailed)
	}
}

// TestCheckSessions_PROpenClosedIssue_TransitionsToDone verifies that
// a pr_open session whose issue is closed gets transitioned to done,
// freeing the worker slot.
func TestCheckSessions_PROpenClosedIssue_TransitionsToDone(t *testing.T) {
	stopped := make([]string, 0)
	o := &Orchestrator{
		cfg:      &config.Config{Repo: "owner/repo", MaxRuntimeMinutes: 120},
		notifier: &notify.Notifier{},
		listOpenPRsFn: func() ([]github.PR, error) {
			return []github.PR{{Number: 50, HeadRefName: "feat/pan-20-200-pr"}}, nil
		},
		isIssueClosedFn: func(issueNumber int) (bool, error) {
			return true, nil
		},
		workerStopFn: func(cfg *config.Config, slotName string, sess *state.Session) error {
			stopped = append(stopped, slotName)
			return nil
		},
	}

	s := state.NewState()
	s.Sessions["pan-20"] = &state.Session{
		IssueNumber: 200,
		IssueTitle:  "pr open but issue closed",
		Status:      state.StatusPROpen,
		Branch:      "feat/pan-20-200-pr",
		PRNumber:    50,
	}

	o.checkSessions(s)

	sess := s.Sessions["pan-20"]
	if sess.Status != state.StatusDone {
		t.Fatalf("status = %q, want %q", sess.Status, state.StatusDone)
	}
	if sess.FinishedAt == nil {
		t.Fatal("FinishedAt should be set")
	}
	if len(stopped) != 1 || stopped[0] != "pan-20" {
		t.Fatalf("stopped = %v, want [pan-20]", stopped)
	}
}

// TestCheckSessions_QueuedClosedIssue_TransitionsToDone verifies that
// a queued session (post-rebase) whose issue is closed gets transitioned to done.
func TestCheckSessions_QueuedClosedIssue_TransitionsToDone(t *testing.T) {
	stopped := make([]string, 0)
	o := &Orchestrator{
		cfg:      &config.Config{Repo: "owner/repo", MaxRuntimeMinutes: 120},
		notifier: &notify.Notifier{},
		listOpenPRsFn: func() ([]github.PR, error) {
			return []github.PR{}, nil
		},
		isIssueClosedFn: func(issueNumber int) (bool, error) {
			return true, nil
		},
		workerStopFn: func(cfg *config.Config, slotName string, sess *state.Session) error {
			stopped = append(stopped, slotName)
			return nil
		},
	}

	s := state.NewState()
	s.Sessions["pan-30"] = &state.Session{
		IssueNumber:     300,
		IssueTitle:      "queued but issue closed",
		Status:          state.StatusQueued,
		Branch:          "feat/pan-30-300-queued",
		RebaseAttempted: true,
	}

	o.checkSessions(s)

	sess := s.Sessions["pan-30"]
	if sess.Status != state.StatusDone {
		t.Fatalf("status = %q, want %q", sess.Status, state.StatusDone)
	}
	if sess.FinishedAt == nil {
		t.Fatal("FinishedAt should be set")
	}
}

// TestCheckSessions_DeadClosedIssue_SetsFinishedAtIfNil verifies that
// FinishedAt is set when transitioning a dead session with nil FinishedAt.
func TestCheckSessions_DeadClosedIssue_SetsFinishedAtIfNil(t *testing.T) {
	o := &Orchestrator{
		cfg:      &config.Config{Repo: "owner/repo", MaxRuntimeMinutes: 120},
		notifier: &notify.Notifier{},
		listOpenPRsFn: func() ([]github.PR, error) {
			return []github.PR{}, nil
		},
		isIssueClosedFn: func(issueNumber int) (bool, error) {
			return true, nil
		},
		workerStopFn: func(cfg *config.Config, slotName string, sess *state.Session) error {
			return nil
		},
	}

	s := state.NewState()
	s.Sessions["pan-12"] = &state.Session{
		IssueNumber: 102,
		IssueTitle:  "dead no finished_at",
		Status:      state.StatusDead,
		Branch:      "feat/pan-12-102-dead",
		// FinishedAt intentionally nil
	}

	o.checkSessions(s)

	sess := s.Sessions["pan-12"]
	if sess.Status != state.StatusDone {
		t.Fatalf("status = %q, want %q", sess.Status, state.StatusDone)
	}
	if sess.FinishedAt == nil {
		t.Fatal("FinishedAt should be set when transitioning from dead with nil FinishedAt")
	}
}

// --- rate-limit detection tests (running worker, tmux output) ---

// newRateLimitOrchestrator creates an Orchestrator wired for checkSessions
// rate-limit testing. tmuxOutput is returned by the capture hook.
// Returns the orchestrator, a slice of stopped slot names, and a slice of
// (slotName, backendName) pairs for respawned workers.
func newRateLimitOrchestrator(cfg *config.Config, tmuxOutput string) (*Orchestrator, *[]string, *[][2]string) {
	stopped := make([]string, 0)
	respawned := make([][2]string, 0) // [slotName, backendName]
	return &Orchestrator{
		cfg:      cfg,
		notifier: &notify.Notifier{},
		listOpenPRsFn: func() ([]github.PR, error) {
			return []github.PR{}, nil
		},
		isIssueClosedFn: func(issueNumber int) (bool, error) {
			return false, nil
		},
		pidAliveFn: func(pid int) bool {
			return true
		},
		captureTmuxFn: func(session string) (string, error) {
			return tmuxOutput, nil
		},
		workerStopFn: func(cfg *config.Config, slotName string, sess *state.Session) error {
			stopped = append(stopped, slotName)
			return nil
		},
		getIssueFn: func(number int) (github.Issue, error) {
			return github.Issue{Number: number, Title: "test issue"}, nil
		},
		workerRespawnFn: func(cfg *config.Config, slotName string, sess *state.Session, repo string, issue github.Issue, promptBase string, backendName string) error {
			respawned = append(respawned, [2]string{slotName, backendName})
			sess.Backend = backendName
			sess.Status = state.StatusRunning
			return nil
		},
	}, &stopped, &respawned
}

// --- model fallback tests (dead worker, log file) ---

// newFallbackTestOrchestrator creates an Orchestrator wired for testing
// rate-limit fallback in checkSessions. It records respawned backends.
func newFallbackTestOrchestrator(cfg *config.Config, rateLimited bool) (*Orchestrator, *[]string) {
	respawnedBackends := make([]string, 0)
	return &Orchestrator{
		cfg:        cfg,
		notifier:   &notify.Notifier{},
		router:     router.New(cfg),
		promptBase: "test prompt",
		listOpenPRsFn: func() ([]github.PR, error) {
			return []github.PR{}, nil
		},
		isIssueClosedFn: func(issueNumber int) (bool, error) {
			return false, nil
		},
		pidAliveFn: func(pid int) bool {
			return false // worker is dead
		},
		isRateLimitedFn: func(logFile string) bool {
			return rateLimited
		},
		getIssueFn: func(number int) (github.Issue, error) {
			return github.Issue{Number: number, Title: "test issue"}, nil
		},
		respawnWorkerFn: func(cfg *config.Config, slotName string, sess *state.Session, repo string, issue github.Issue, promptBase string, backendName string) error {
			respawnedBackends = append(respawnedBackends, backendName)
			sess.Status = state.StatusRunning
			sess.PID = 9999
			sess.Backend = backendName
			sess.StartedAt = time.Now().UTC()
			sess.FinishedAt = nil
			return nil
		},
	}, &respawnedBackends
}

// ---- Running-worker rate-limit tests (tmux detection, worker alive) ----

func TestCheckSessions_RateLimitDetected_NoFallback_MarksDead(t *testing.T) {
	cfg := &config.Config{
		Repo:              "owner/repo",
		MaxRuntimeMinutes: 999,
		Model: config.ModelConfig{
			Default:  "claude",
			Backends: map[string]config.BackendDef{"claude": {Cmd: "claude"}},
		},
	}
	o, stopped, _ := newRateLimitOrchestrator(cfg, "Error: You've hit your limit for today.")

	s := state.NewState()
	s.Sessions["mae-1"] = &state.Session{
		IssueNumber: 101,
		IssueTitle:  "test issue",
		Status:      state.StatusRunning,
		PID:         1234,
		TmuxSession: "maestro-mae-1",
		Branch:      "feat/mae-1-101-test",
		Backend:     "claude",
		StartedAt:   time.Now().Add(-10 * time.Minute),
	}

	o.checkSessions(s)

	sess := s.Sessions["mae-1"]
	if sess.Status != state.StatusDead {
		t.Fatalf("status = %q, want %q", sess.Status, state.StatusDead)
	}
	if sess.LastNotifiedStatus != "rate_limit" {
		t.Fatalf("last_notified_status = %q, want %q", sess.LastNotifiedStatus, "rate_limit")
	}
	if !sess.RateLimitHit {
		t.Fatal("rate_limit_hit should be true")
	}
	if sess.FinishedAt == nil {
		t.Fatal("finished_at should be set")
	}
	if len(*stopped) != 1 || (*stopped)[0] != "mae-1" {
		t.Fatalf("stopped = %v, want [mae-1]", *stopped)
	}
}

func TestCheckSessions_RateLimitDetected_WithFallback_Respawns(t *testing.T) {
	cfg := &config.Config{
		Repo:              "owner/repo",
		MaxRuntimeMinutes: 999,
		Model: config.ModelConfig{
			Default:          "claude",
			FallbackBackends: []string{"gemini"},
			Backends: map[string]config.BackendDef{
				"claude": {Cmd: "claude"},
				"gemini": {Cmd: "gemini"},
			},
		},
	}
	o, stopped, respawned := newRateLimitOrchestrator(cfg, "rate limit exceeded â try again later")

	s := state.NewState()
	s.Sessions["mae-2"] = &state.Session{
		IssueNumber: 102,
		IssueTitle:  "test issue",
		Status:      state.StatusRunning,
		PID:         2345,
		TmuxSession: "maestro-mae-2",
		Branch:      "feat/mae-2-102-test",
		Backend:     "claude",
		StartedAt:   time.Now().Add(-10 * time.Minute),
	}

	o.checkSessions(s)

	sess := s.Sessions["mae-2"]
	// Worker should be stopped (old one killed) then respawned with fallback
	if len(*stopped) != 1 || (*stopped)[0] != "mae-2" {
		t.Fatalf("stopped = %v, want [mae-2]", *stopped)
	}
	if len(*respawned) != 1 {
		t.Fatalf("respawned = %v, want 1 entry", *respawned)
	}
	if (*respawned)[0][0] != "mae-2" || (*respawned)[0][1] != "gemini" {
		t.Fatalf("respawned = %v, want [mae-2, gemini]", (*respawned)[0])
	}
	// Session should be running with new backend
	if sess.Status != state.StatusRunning {
		t.Fatalf("status = %q, want %q (should be running after fallback respawn)", sess.Status, state.StatusRunning)
	}
	if sess.Backend != "gemini" {
		t.Fatalf("backend = %q, want %q", sess.Backend, "gemini")
	}
	if !sess.RateLimitHit {
		t.Fatal("rate_limit_hit should be true")
	}
}

func TestCheckSessions_RateLimitDetected_AlreadyOnFallback_MarksDead(t *testing.T) {
	cfg := &config.Config{
		Repo:              "owner/repo",
		MaxRuntimeMinutes: 999,
		Model: config.ModelConfig{
			Default:          "claude",
			FallbackBackends: []string{"gemini"},
			Backends: map[string]config.BackendDef{
				"claude": {Cmd: "claude"},
				"gemini": {Cmd: "gemini"},
			},
		},
	}
	o, stopped, respawned := newRateLimitOrchestrator(cfg, "Error 429: too many requests")

	s := state.NewState()
	s.Sessions["mae-3"] = &state.Session{
		IssueNumber: 103,
		IssueTitle:  "test issue",
		Status:      state.StatusRunning,
		PID:         3456,
		TmuxSession: "maestro-mae-3",
		Branch:      "feat/mae-3-103-test",
		Backend:     "gemini", // already on fallback
		StartedAt:   time.Now().Add(-10 * time.Minute),
	}

	o.checkSessions(s)

	sess := s.Sessions["mae-3"]
	// Should NOT respawn â already on fallback backend
	if sess.Status != state.StatusDead {
		t.Fatalf("status = %q, want %q (already on fallback, should be dead)", sess.Status, state.StatusDead)
	}
	if len(*stopped) != 1 {
		t.Fatalf("stopped = %v, want 1 entry", *stopped)
	}
	if len(*respawned) != 0 {
		t.Fatalf("respawned = %v, want empty (should not respawn when already on fallback)", *respawned)
	}
	if sess.LastNotifiedStatus != "rate_limit" {
		t.Fatalf("last_notified_status = %q, want %q", sess.LastNotifiedStatus, "rate_limit")
	}
}

func TestCheckSessions_RateLimitAlreadyNotified_NoDuplicateKill(t *testing.T) {
	cfg := &config.Config{
		Repo:              "owner/repo",
		MaxRuntimeMinutes: 999,
		Model: config.ModelConfig{
			Default:  "claude",
			Backends: map[string]config.BackendDef{"claude": {Cmd: "claude"}},
		},
	}
	// Output still contains rate limit text from previous cycle
	o, stopped, _ := newRateLimitOrchestrator(cfg, "Error: rate limit exceeded")

	s := state.NewState()
	s.Sessions["mae-4"] = &state.Session{
		IssueNumber:        104,
		IssueTitle:         "test issue",
		Status:             state.StatusRunning,
		PID:                4567,
		TmuxSession:        "maestro-mae-4",
		Branch:             "feat/mae-4-104-test",
		Backend:            "claude",
		StartedAt:          time.Now().Add(-10 * time.Minute),
		RateLimitHit:       true,         // already detected
		LastNotifiedStatus: "rate_limit", // already notified
	}

	o.checkSessions(s)

	sess := s.Sessions["mae-4"]
	// Should remain running â rate limit was already handled
	if sess.Status != state.StatusRunning {
		t.Fatalf("status = %q, want %q (already notified, should not re-kill)", sess.Status, state.StatusRunning)
	}
	if len(*stopped) != 0 {
		t.Fatalf("stopped = %v, want empty (should not duplicate kill)", *stopped)
	}
}

func TestCheckSessions_NoRateLimit_WorkerSurvives(t *testing.T) {
	cfg := &config.Config{
		Repo:              "owner/repo",
		MaxRuntimeMinutes: 999,
		Model: config.ModelConfig{
			Default:          "claude",
			FallbackBackends: []string{"gemini"},
			Backends: map[string]config.BackendDef{
				"claude": {Cmd: "claude"},
				"gemini": {Cmd: "gemini"},
			},
		},
	}
	// Normal output, no rate limit patterns
	o, stopped, respawned := newRateLimitOrchestrator(cfg, "tokens 50000 (in 10000 / out 40000)\nTask completed successfully.")

	s := state.NewState()
	s.Sessions["mae-5"] = &state.Session{
		IssueNumber: 105,
		IssueTitle:  "test issue",
		Status:      state.StatusRunning,
		PID:         5678,
		TmuxSession: "maestro-mae-5",
		Branch:      "feat/mae-5-105-test",
		Backend:     "claude",
		StartedAt:   time.Now().Add(-5 * time.Minute),
	}

	o.checkSessions(s)

	sess := s.Sessions["mae-5"]
	if sess.Status != state.StatusRunning {
		t.Fatalf("status = %q, want %q", sess.Status, state.StatusRunning)
	}
	if sess.RateLimitHit {
		t.Fatal("rate_limit_hit should be false")
	}
	if len(*stopped) != 0 {
		t.Fatalf("stopped = %v, want empty", *stopped)
	}
	if len(*respawned) != 0 {
		t.Fatalf("respawned = %v, want empty", *respawned)
	}
}

func TestCheckSessions_RateLimit429Pattern_Detected(t *testing.T) {
	cfg := &config.Config{
		Repo:              "owner/repo",
		MaxRuntimeMinutes: 999,
		Model: config.ModelConfig{
			Default:  "claude",
			Backends: map[string]config.BackendDef{"claude": {Cmd: "claude"}},
		},
	}
	o, stopped, _ := newRateLimitOrchestrator(cfg, "API returned status 429")

	s := state.NewState()
	s.Sessions["mae-6"] = &state.Session{
		IssueNumber: 106,
		IssueTitle:  "test issue",
		Status:      state.StatusRunning,
		PID:         6789,
		TmuxSession: "maestro-mae-6",
		Branch:      "feat/mae-6-106-test",
		Backend:     "claude",
		StartedAt:   time.Now().Add(-10 * time.Minute),
	}

	o.checkSessions(s)

	sess := s.Sessions["mae-6"]
	if sess.Status != state.StatusDead {
		t.Fatalf("status = %q, want %q", sess.Status, state.StatusDead)
	}
	if !sess.RateLimitHit {
		t.Fatal("rate_limit_hit should be true for 429 pattern")
	}
	if len(*stopped) != 1 {
		t.Fatalf("stopped = %v, want 1 entry", *stopped)
	}
}

// ---- Dead-worker fallback tests (log file detection, worker dead) ----

func TestCheckSessions_RateLimitFallbackToCodex(t *testing.T) {
	cfg := cfgWithBackends("claude", "claude", "codex", "gemini")
	cfg.Model.FallbackBackends = []string{"codex", "gemini"}
	cfg.MaxRuntimeMinutes = 999

	o, respawned := newFallbackTestOrchestrator(cfg, true)

	s := state.NewState()
	s.Sessions["mae-1"] = &state.Session{
		IssueNumber: 101,
		IssueTitle:  "test issue",
		Status:      state.StatusRunning,
		PID:         1234,
		TmuxSession: "maestro-mae-1",
		Branch:      "feat/mae-1-101-test",
		LogFile:     "/tmp/test.log",
		Backend:     "claude",
		StartedAt:   time.Now().Add(-10 * time.Minute),
	}

	o.checkSessions(s)

	sess2 := s.Sessions["mae-1"]
	if sess2.Status != state.StatusRunning {
		t.Fatalf("status = %q, want %q (should be respawned with fallback)", sess2.Status, state.StatusRunning)
	}
	if len(*respawned) != 1 || (*respawned)[0] != "codex" {
		t.Fatalf("respawned = %v, want [codex]", *respawned)
	}
	if len(sess2.TriedBackends) != 1 || sess2.TriedBackends[0] != "claude" {
		t.Fatalf("tried_backends = %v, want [claude]", sess2.TriedBackends)
	}
}

func TestCheckSessions_RateLimitFallbackSkipsAlreadyTried(t *testing.T) {
	cfg := cfgWithBackends("claude", "claude", "codex", "gemini")
	cfg.Model.FallbackBackends = []string{"codex", "gemini"}
	cfg.MaxRuntimeMinutes = 999

	o, respawned := newFallbackTestOrchestrator(cfg, true)

	s := state.NewState()
	s.Sessions["mae-1"] = &state.Session{
		IssueNumber:   101,
		IssueTitle:    "test issue",
		Status:        state.StatusRunning,
		PID:           1234,
		TmuxSession:   "maestro-mae-1",
		Branch:        "feat/mae-1-101-test",
		LogFile:       "/tmp/test.log",
		Backend:       "codex",
		StartedAt:     time.Now().Add(-10 * time.Minute),
		TriedBackends: []string{"claude"}, // claude already tried
	}

	o.checkSessions(s)

	sess2 := s.Sessions["mae-1"]
	if sess2.Status != state.StatusRunning {
		t.Fatalf("status = %q, want %q", sess2.Status, state.StatusRunning)
	}
	if len(*respawned) != 1 || (*respawned)[0] != "gemini" {
		t.Fatalf("respawned = %v, want [gemini] (should skip codex since claudeâcodex already tried)", *respawned)
	}
	if len(sess2.TriedBackends) != 2 {
		t.Fatalf("tried_backends = %v, want [claude, codex]", sess2.TriedBackends)
	}
}

func TestCheckSessions_RateLimitNoFallbackAvailable_NormalRetry(t *testing.T) {
	cfg := cfgWithBackends("claude", "claude", "codex", "gemini")
	cfg.Model.FallbackBackends = []string{"codex", "gemini"}
	cfg.MaxRuntimeMinutes = 999

	o, respawned := newFallbackTestOrchestrator(cfg, true)

	s := state.NewState()
	s.Sessions["mae-1"] = &state.Session{
		IssueNumber:   101,
		IssueTitle:    "test issue",
		Status:        state.StatusRunning,
		PID:           1234,
		TmuxSession:   "maestro-mae-1",
		Branch:        "feat/mae-1-101-test",
		LogFile:       "/tmp/test.log",
		Backend:       "gemini",
		StartedAt:     time.Now().Add(-10 * time.Minute),
		TriedBackends: []string{"claude", "codex"}, // all fallbacks exhausted
	}

	o.checkSessions(s)

	sess2 := s.Sessions["mae-1"]
	// Should schedule retry with backoff (not immediate respawn)
	if sess2.Status != state.StatusDead {
		t.Fatalf("status = %q, want %q (should schedule backoff)", sess2.Status, state.StatusDead)
	}
	if sess2.NextRetryAt == nil {
		t.Fatal("NextRetryAt should be set for scheduled retry")
	}
	if len(*respawned) != 0 {
		t.Fatalf("respawned = %v, want [] (retry is scheduled, not immediate)", *respawned)
	}
	if sess2.RetryCount != 1 {
		t.Fatalf("retry_count = %d, want 1", sess2.RetryCount)
	}
}

func TestCheckSessions_NoRateLimit_NormalRetry(t *testing.T) {
	cfg := cfgWithBackends("claude", "claude", "codex", "gemini")
	cfg.Model.FallbackBackends = []string{"codex", "gemini"}
	cfg.MaxRuntimeMinutes = 999

	o, respawned := newFallbackTestOrchestrator(cfg, false) // NOT rate limited

	s := state.NewState()
	s.Sessions["mae-1"] = &state.Session{
		IssueNumber: 101,
		IssueTitle:  "test issue",
		Status:      state.StatusRunning,
		PID:         1234,
		TmuxSession: "maestro-mae-1",
		Branch:      "feat/mae-1-101-test",
		LogFile:     "/tmp/test.log",
		Backend:     "claude",
		StartedAt:   time.Now().Add(-10 * time.Minute),
	}

	o.checkSessions(s)

	sess2 := s.Sessions["mae-1"]
	// Should schedule retry with backoff (not immediate respawn)
	if sess2.Status != state.StatusDead {
		t.Fatalf("status = %q, want %q (should schedule backoff)", sess2.Status, state.StatusDead)
	}
	if sess2.NextRetryAt == nil {
		t.Fatal("NextRetryAt should be set for scheduled retry")
	}
	if len(*respawned) != 0 {
		t.Fatalf("respawned = %v, want [] (retry is scheduled, not immediate)", *respawned)
	}
	if sess2.RetryCount != 1 {
		t.Fatalf("retry_count = %d, want 1", sess2.RetryCount)
	}
}

func TestCheckSessions_RateLimitNoFallbackConfigured_NormalRetry(t *testing.T) {
	cfg := cfgWithBackends("claude", "claude", "codex")
	// No fallback_backends configured
	cfg.MaxRuntimeMinutes = 999

	o, respawned := newFallbackTestOrchestrator(cfg, true)

	s := state.NewState()
	s.Sessions["mae-1"] = &state.Session{
		IssueNumber: 101,
		IssueTitle:  "test issue",
		Status:      state.StatusRunning,
		PID:         1234,
		TmuxSession: "maestro-mae-1",
		Branch:      "feat/mae-1-101-test",
		LogFile:     "/tmp/test.log",
		Backend:     "claude",
		StartedAt:   time.Now().Add(-10 * time.Minute),
	}

	o.checkSessions(s)

	sess2 := s.Sessions["mae-1"]
	// No fallback backends configured, so should schedule retry with backoff
	if sess2.Status != state.StatusDead {
		t.Fatalf("status = %q, want %q (should schedule backoff)", sess2.Status, state.StatusDead)
	}
	if sess2.NextRetryAt == nil {
		t.Fatal("NextRetryAt should be set for scheduled retry")
	}
	if len(*respawned) != 0 {
		t.Fatalf("respawned = %v, want [] (retry is scheduled, not immediate)", *respawned)
	}
	if sess2.RetryCount != 1 {
		t.Fatalf("retry_count = %d, want 1", sess2.RetryCount)
	}
}

func TestCheckSessions_RateLimitFallbackDoesNotIncrementRetryCount(t *testing.T) {
	cfg := cfgWithBackends("claude", "claude", "codex", "gemini")
	cfg.Model.FallbackBackends = []string{"codex", "gemini"}
	cfg.MaxRuntimeMinutes = 999

	o, _ := newFallbackTestOrchestrator(cfg, true)

	s := state.NewState()
	s.Sessions["mae-1"] = &state.Session{
		IssueNumber: 101,
		IssueTitle:  "test issue",
		Status:      state.StatusRunning,
		PID:         1234,
		TmuxSession: "maestro-mae-1",
		Branch:      "feat/mae-1-101-test",
		LogFile:     "/tmp/test.log",
		Backend:     "claude",
		StartedAt:   time.Now().Add(-10 * time.Minute),
	}

	o.checkSessions(s)

	sess2 := s.Sessions["mae-1"]
	// Fallback should NOT increment retry count â fallback is separate from normal retry
	if sess2.RetryCount != 0 {
		t.Fatalf("retry_count = %d, want 0 (fallback should not increment retry count)", sess2.RetryCount)
	}
}

func TestNextFallbackBackend_Basic(t *testing.T) {
	cfg := cfgWithBackends("claude", "claude", "codex", "gemini")
	cfg.Model.FallbackBackends = []string{"codex", "gemini"}

	o := &Orchestrator{cfg: cfg}
	sess2 := &state.Session{Backend: "claude"}

	got := o.nextFallbackBackend(sess2)
	if got != "codex" {
		t.Errorf("nextFallbackBackend() = %q, want %q", got, "codex")
	}
}

func TestNextFallbackBackend_SkipsTried(t *testing.T) {
	cfg := cfgWithBackends("claude", "claude", "codex", "gemini")
	cfg.Model.FallbackBackends = []string{"codex", "gemini"}

	o := &Orchestrator{cfg: cfg}
	sess2 := &state.Session{Backend: "codex", TriedBackends: []string{"claude"}}

	got := o.nextFallbackBackend(sess2)
	if got != "gemini" {
		t.Errorf("nextFallbackBackend() = %q, want %q", got, "gemini")
	}
}

func TestNextFallbackBackend_AllExhausted(t *testing.T) {
	cfg := cfgWithBackends("claude", "claude", "codex", "gemini")
	cfg.Model.FallbackBackends = []string{"codex", "gemini"}

	o := &Orchestrator{cfg: cfg}
	sess2 := &state.Session{Backend: "gemini", TriedBackends: []string{"claude", "codex"}}

	got := o.nextFallbackBackend(sess2)
	if got != "" {
		t.Errorf("nextFallbackBackend() = %q, want empty (all exhausted)", got)
	}
}

func TestNextFallbackBackend_NoFallbacksConfigured(t *testing.T) {
	cfg := cfgWithBackends("claude", "claude", "codex")

	o := &Orchestrator{cfg: cfg}
	sess2 := &state.Session{Backend: "claude"}

	got := o.nextFallbackBackend(sess2)
	if got != "" {
		t.Errorf("nextFallbackBackend() = %q, want empty (no fallbacks configured)", got)
	}
}

func TestNextFallbackBackend_SkipsUnknownBackend(t *testing.T) {
	cfg := cfgWithBackends("claude", "claude", "codex")
	cfg.Model.FallbackBackends = []string{"unknown_backend", "codex"}

	o := &Orchestrator{cfg: cfg}
	sess2 := &state.Session{Backend: "claude"}

	got := o.nextFallbackBackend(sess2)
	if got != "codex" {
		t.Errorf("nextFallbackBackend() = %q, want %q (should skip unknown backend)", got, "codex")
	}
}

// --- per-state concurrency limit tests ---

func TestAvailableSlots_NoPerStateLimit(t *testing.T) {
	cfg := &config.Config{MaxParallel: 10}
	s := state.NewState()
	s.Sessions["slot-1"] = &state.Session{Status: state.StatusRunning}
	s.Sessions["slot-2"] = &state.Session{Status: state.StatusPROpen}

	got := availableSlots(cfg, s, 2)
	if got != 8 {
		t.Errorf("availableSlots() = %d, want 8", got)
	}
}

func TestAvailableSlots_RunningLimitCapsSlots(t *testing.T) {
	cfg := &config.Config{
		MaxParallel:          10,
		MaxConcurrentByState: map[string]int{"running": 3},
	}
	s := state.NewState()
	s.Sessions["slot-1"] = &state.Session{Status: state.StatusRunning}
	s.Sessions["slot-2"] = &state.Session{Status: state.StatusRunning}
	s.Sessions["slot-3"] = &state.Session{Status: state.StatusPROpen}
	s.Sessions["slot-4"] = &state.Session{Status: state.StatusPROpen}

	got := availableSlots(cfg, s, 4)
	if got != 1 {
		t.Errorf("availableSlots() = %d, want 1 (running limit should cap)", got)
	}
}

func TestAvailableSlots_RunningLimitExceeded(t *testing.T) {
	cfg := &config.Config{
		MaxParallel:          10,
		MaxConcurrentByState: map[string]int{"running": 2},
	}
	s := state.NewState()
	s.Sessions["slot-1"] = &state.Session{Status: state.StatusRunning}
	s.Sessions["slot-2"] = &state.Session{Status: state.StatusRunning}
	s.Sessions["slot-3"] = &state.Session{Status: state.StatusRunning}

	got := availableSlots(cfg, s, 3)
	if got != 0 {
		t.Errorf("availableSlots() = %d, want 0 (running limit exceeded)", got)
	}
}

func TestAvailableSlots_GlobalLimitMoreRestrictive(t *testing.T) {
	cfg := &config.Config{
		MaxParallel:          5,
		MaxConcurrentByState: map[string]int{"running": 10},
	}
	s := state.NewState()
	s.Sessions["slot-1"] = &state.Session{Status: state.StatusRunning}
	s.Sessions["slot-2"] = &state.Session{Status: state.StatusRunning}
	s.Sessions["slot-3"] = &state.Session{Status: state.StatusRunning}
	s.Sessions["slot-4"] = &state.Session{Status: state.StatusPROpen}

	got := availableSlots(cfg, s, 4)
	if got != 1 {
		t.Errorf("availableSlots() = %d, want 1 (global limit should cap)", got)
	}
}

func TestAvailableSlots_ZeroWhenAtGlobalMax(t *testing.T) {
	cfg := &config.Config{
		MaxParallel:          3,
		MaxConcurrentByState: map[string]int{"running": 5},
	}
	s := state.NewState()
	s.Sessions["slot-1"] = &state.Session{Status: state.StatusRunning}
	s.Sessions["slot-2"] = &state.Session{Status: state.StatusRunning}
	s.Sessions["slot-3"] = &state.Session{Status: state.StatusPROpen}

	got := availableSlots(cfg, s, 3)
	if got != 0 {
		t.Errorf("availableSlots() = %d, want 0 (at global max)", got)
	}
}

func TestAvailableSlots_TerminalSessionsIgnored(t *testing.T) {
	cfg := &config.Config{
		MaxParallel:          10,
		MaxConcurrentByState: map[string]int{"running": 3},
	}
	s := state.NewState()
	s.Sessions["slot-1"] = &state.Session{Status: state.StatusRunning}
	s.Sessions["slot-2"] = &state.Session{Status: state.StatusDone}
	s.Sessions["slot-3"] = &state.Session{Status: state.StatusFailed}

	got := availableSlots(cfg, s, 1)
	if got != 2 {
		t.Errorf("availableSlots() = %d, want 2", got)
	}
}

func TestAvailableSlots_NonRunningLimitIgnoredForDispatch(t *testing.T) {
	cfg := &config.Config{
		MaxParallel:          10,
		MaxConcurrentByState: map[string]int{"pr_open": 1},
	}
	s := state.NewState()
	s.Sessions["slot-1"] = &state.Session{Status: state.StatusRunning}
	s.Sessions["slot-2"] = &state.Session{Status: state.StatusPROpen}
	s.Sessions["slot-3"] = &state.Session{Status: state.StatusPROpen}

	got := availableSlots(cfg, s, 3)
	if got != 7 {
		t.Errorf("availableSlots() = %d, want 7 (pr_open limit shouldn't affect dispatch)", got)
	}
}

// --- blocker-aware dispatch tests ---

func TestFindOpenBlockers_AllClosed(t *testing.T) {
	o := &Orchestrator{
		isIssueClosedFn: func(number int) (bool, error) {
			return true, nil // all blockers closed
		},
	}
	got := o.findOpenBlockers([]int{10, 20, 30})
	if len(got) != 0 {
		t.Errorf("findOpenBlockers() = %v, want empty (all closed)", got)
	}
}

func TestFindOpenBlockers_SomeOpen(t *testing.T) {
	closedIssues := map[int]bool{10: true, 20: false, 30: true}
	o := &Orchestrator{
		isIssueClosedFn: func(number int) (bool, error) {
			return closedIssues[number], nil
		},
	}
	got := o.findOpenBlockers([]int{10, 20, 30})
	if len(got) != 1 || got[0] != 20 {
		t.Errorf("findOpenBlockers() = %v, want [20]", got)
	}
}

func TestFindOpenBlockers_ErrorAssumesOpen(t *testing.T) {
	o := &Orchestrator{
		isIssueClosedFn: func(number int) (bool, error) {
			return false, fmt.Errorf("network error")
		},
	}
	got := o.findOpenBlockers([]int{42})
	if len(got) != 1 || got[0] != 42 {
		t.Errorf("findOpenBlockers() = %v, want [42] (error should assume open)", got)
	}
}

func TestFindOpenBlockers_Empty(t *testing.T) {
	o := &Orchestrator{}
	got := o.findOpenBlockers(nil)
	if len(got) != 0 {
		t.Errorf("findOpenBlockers() = %v, want empty", got)
	}
}

func TestStartNewWorkers_SkipsBlockedIssue(t *testing.T) {
	cfg := cfgWithBackends("claude", "claude")
	cfg.BlockerPatterns = []string{`blocked by #(\d+)`}

	issues := []github.Issue{
		{Number: 42, Title: "blocked issue", Body: "This is blocked by #10"},
		{Number: 43, Title: "free issue", Body: "No blockers here"},
	}

	o, started, _ := newStartWorkersOrchestrator(cfg, issues)
	// Issue #10 is still open (not closed)
	o.isIssueClosedFn = func(number int) (bool, error) {
		return false, nil
	}

	s := state.NewState()
	o.startNewWorkers(s, 5)

	if len(*started) != 1 {
		t.Fatalf("started %d workers, want 1", len(*started))
	}
	if (*started)[0] != 43 {
		t.Errorf("started issue #%d, want #43", (*started)[0])
	}
}

func TestStartNewWorkers_DispatchesWhenBlockersClosed(t *testing.T) {
	cfg := cfgWithBackends("claude", "claude")
	cfg.BlockerPatterns = []string{`blocked by #(\d+)`}

	issues := []github.Issue{
		{Number: 42, Title: "was blocked", Body: "This is blocked by #10"},
	}

	o, started, _ := newStartWorkersOrchestrator(cfg, issues)
	// Blocker #10 is closed
	o.isIssueClosedFn = func(number int) (bool, error) {
		return true, nil
	}

	s := state.NewState()
	o.startNewWorkers(s, 5)

	if len(*started) != 1 {
		t.Fatalf("started %d workers, want 1 (blocker closed)", len(*started))
	}
	if (*started)[0] != 42 {
		t.Errorf("started issue #%d, want #42", (*started)[0])
	}
}

func TestStartNewWorkers_NoPatternsNoBlockerCheck(t *testing.T) {
	cfg := cfgWithBackends("claude", "claude")
	// No blocker_patterns configured

	issues := []github.Issue{
		{Number: 42, Title: "has blocker text", Body: "blocked by #10"},
	}

	o, started, _ := newStartWorkersOrchestrator(cfg, issues)

	s := state.NewState()
	o.startNewWorkers(s, 5)

	// Should dispatch because blocker_patterns is empty (feature disabled)
	if len(*started) != 1 {
		t.Fatalf("started %d workers, want 1 (no patterns = no check)", len(*started))
	}
}

func TestReloadConfig_AppliesReloadableFields(t *testing.T) {
	cfg := &config.Config{
		Repo:               "owner/repo",
		MaxParallel:        5,
		MaxRuntimeMinutes:  120,
		MaxRetriesPerIssue: 3,
		Model: config.ModelConfig{
			Default:  "claude",
			Backends: map[string]config.BackendDef{"claude": {Cmd: "claude"}},
		},
	}
	o := &Orchestrator{
		cfg:      cfg,
		repo:     cfg.Repo,
		notifier: notify.NewWithToken("", "", ""),
		router:   router.New(cfg),
	}

	newCfg := &config.Config{
		Repo:               "owner/repo",
		MaxParallel:        10,
		MaxRuntimeMinutes:  60,
		MaxRetriesPerIssue: 5,
		Model: config.ModelConfig{
			Default:  "claude",
			Backends: map[string]config.BackendDef{"claude": {Cmd: "claude"}},
		},
	}

	ticker := time.NewTicker(10 * time.Minute)
	defer ticker.Stop()
	o.reloadConfig(newCfg, &ticker)

	if o.cfg.MaxParallel != 10 {
		t.Errorf("MaxParallel = %d, want 10", o.cfg.MaxParallel)
	}
	if o.cfg.MaxRuntimeMinutes != 60 {
		t.Errorf("MaxRuntimeMinutes = %d, want 60", o.cfg.MaxRuntimeMinutes)
	}
	if o.cfg.MaxRetriesPerIssue != 5 {
		t.Errorf("MaxRetriesPerIssue = %d, want 5", o.cfg.MaxRetriesPerIssue)
	}
}

func TestReloadConfig_PollIntervalChange(t *testing.T) {
	cfg := &config.Config{
		Repo:                "owner/repo",
		PollIntervalSeconds: 600,
		Model: config.ModelConfig{
			Default:  "claude",
			Backends: map[string]config.BackendDef{"claude": {Cmd: "claude"}},
		},
	}
	o := &Orchestrator{
		cfg:      cfg,
		repo:     cfg.Repo,
		notifier: notify.NewWithToken("", "", ""),
		router:   router.New(cfg),
	}

	newCfg := &config.Config{
		Repo:                "owner/repo",
		PollIntervalSeconds: 300,
		Model: config.ModelConfig{
			Default:  "claude",
			Backends: map[string]config.BackendDef{"claude": {Cmd: "claude"}},
		},
	}

	ticker := time.NewTicker(10 * time.Minute)
	defer ticker.Stop()
	o.reloadConfig(newCfg, &ticker)

	if o.cfg.PollIntervalSeconds != 300 {
		t.Errorf("PollIntervalSeconds = %d, want 300", o.cfg.PollIntervalSeconds)
	}
}

func TestReloadConfig_NoChanges(t *testing.T) {
	cfg := &config.Config{
		Repo:        "owner/repo",
		MaxParallel: 5,
		Model: config.ModelConfig{
			Default:  "claude",
			Backends: map[string]config.BackendDef{"claude": {Cmd: "claude"}},
		},
	}
	o := &Orchestrator{
		cfg:      cfg,
		repo:     cfg.Repo,
		notifier: notify.NewWithToken("", "", ""),
		router:   router.New(cfg),
	}

	newCfg := &config.Config{
		Repo:        "owner/repo",
		MaxParallel: 5,
		Model: config.ModelConfig{
			Default:  "claude",
			Backends: map[string]config.BackendDef{"claude": {Cmd: "claude"}},
		},
	}

	ticker := time.NewTicker(10 * time.Minute)
	defer ticker.Stop()
	// Should not panic or change anything
	o.reloadConfig(newCfg, &ticker)

	if o.cfg.MaxParallel != 5 {
		t.Errorf("MaxParallel = %d, want 5 (unchanged)", o.cfg.MaxParallel)
	}
}

func TestReloadConfig_IssueLabelsUpdated(t *testing.T) {
	cfg := &config.Config{
		Repo:        "owner/repo",
		IssueLabels: []string{"bug"},
		Model: config.ModelConfig{
			Default:  "claude",
			Backends: map[string]config.BackendDef{"claude": {Cmd: "claude"}},
		},
	}
	o := &Orchestrator{
		cfg:      cfg,
		repo:     cfg.Repo,
		notifier: notify.NewWithToken("", "", ""),
		router:   router.New(cfg),
	}

	newCfg := &config.Config{
		Repo:        "owner/repo",
		IssueLabels: []string{"bug", "enhancement"},
		Model: config.ModelConfig{
			Default:  "claude",
			Backends: map[string]config.BackendDef{"claude": {Cmd: "claude"}},
		},
	}

	ticker := time.NewTicker(10 * time.Minute)
	defer ticker.Stop()
	o.reloadConfig(newCfg, &ticker)

	if len(o.cfg.IssueLabels) != 2 || o.cfg.IssueLabels[1] != "enhancement" {
		t.Errorf("IssueLabels = %v, want [bug enhancement]", o.cfg.IssueLabels)
	}
}

func TestReloadConfig_PromptPathReload(t *testing.T) {
	dir := t.TempDir()
	promptFile := dir + "/prompt.md"
	os.WriteFile(promptFile, []byte("new prompt content"), 0644)

	cfg := &config.Config{
		Repo:         "owner/repo",
		WorkerPrompt: "/old/path.md",
		Model: config.ModelConfig{
			Default:  "claude",
			Backends: map[string]config.BackendDef{"claude": {Cmd: "claude"}},
		},
	}
	o := &Orchestrator{
		cfg:        cfg,
		repo:       cfg.Repo,
		promptBase: "old prompt",
		notifier:   notify.NewWithToken("", "", ""),
		router:     router.New(cfg),
	}

	newCfg := &config.Config{
		Repo:         "owner/repo",
		WorkerPrompt: promptFile,
		Model: config.ModelConfig{
			Default:  "claude",
			Backends: map[string]config.BackendDef{"claude": {Cmd: "claude"}},
		},
	}

	ticker := time.NewTicker(10 * time.Minute)
	defer ticker.Stop()
	o.reloadConfig(newCfg, &ticker)

	if o.cfg.WorkerPrompt != promptFile {
		t.Errorf("WorkerPrompt = %q, want %q", o.cfg.WorkerPrompt, promptFile)
	}
	if o.promptBase != "new prompt content" {
		t.Errorf("promptBase = %q, want %q", o.promptBase, "new prompt content")
	}
}

func TestStrSliceEqual(t *testing.T) {
	tests := []struct {
		a, b []string
		want bool
	}{
		{nil, nil, true},
		{[]string{}, []string{}, true},
		{[]string{"a"}, []string{"a"}, true},
		{[]string{"a", "b"}, []string{"a", "b"}, true},
		{[]string{"a"}, []string{"b"}, false},
		{[]string{"a"}, []string{"a", "b"}, false},
		{nil, []string{"a"}, false},
	}
	for _, tt := range tests {
		got := strSliceEqual(tt.a, tt.b)
		if got != tt.want {
			t.Errorf("strSliceEqual(%v, %v) = %v, want %v", tt.a, tt.b, got, tt.want)
		}
	}
}

// --- exponential retry backoff tests ---

func TestRetryBackoffMs_FirstAttempt(t *testing.T) {
	// attempt=1: 10000 * 2^0 = 10000ms
	got := retryBackoffMs(1, 300000)
	if got != 10000 {
		t.Errorf("retryBackoffMs(1, 300000) = %d, want 10000", got)
	}
}

func TestRetryBackoffMs_SecondAttempt(t *testing.T) {
	// attempt=2: 10000 * 2^1 = 20000ms
	got := retryBackoffMs(2, 300000)
	if got != 20000 {
		t.Errorf("retryBackoffMs(2, 300000) = %d, want 20000", got)
	}
}

func TestRetryBackoffMs_ThirdAttempt(t *testing.T) {
	// attempt=3: 10000 * 2^2 = 40000ms
	got := retryBackoffMs(3, 300000)
	if got != 40000 {
		t.Errorf("retryBackoffMs(3, 300000) = %d, want 40000", got)
	}
}

func TestRetryBackoffMs_CappedAtMax(t *testing.T) {
	// attempt=10: 10000 * 2^9 = 5120000 > 300000, should be capped
	got := retryBackoffMs(10, 300000)
	if got != 300000 {
		t.Errorf("retryBackoffMs(10, 300000) = %d, want 300000 (capped)", got)
	}
}

func TestRetryBackoffMs_ZeroAttempt(t *testing.T) {
	// attempt=0 should be treated as 1
	got := retryBackoffMs(0, 300000)
	if got != 10000 {
		t.Errorf("retryBackoffMs(0, 300000) = %d, want 10000", got)
	}
}

func TestRetryBackoffMs_SmallCap(t *testing.T) {
	// cap of 5000ms should limit even the first attempt
	got := retryBackoffMs(1, 5000)
	if got != 5000 {
		t.Errorf("retryBackoffMs(1, 5000) = %d, want 5000 (capped)", got)
	}
}

func TestCheckSessions_DeadWorkerSchedulesRetryWithBackoff(t *testing.T) {
	cfg := &config.Config{
		Repo:              "owner/repo",
		MaxRetryBackoffMs: 300000,
		MaxRuntimeMinutes: 999,
	}
	o := &Orchestrator{
		cfg:      cfg,
		notifier: &notify.Notifier{},
		listOpenPRsFn: func() ([]github.PR, error) {
			return []github.PR{}, nil
		},
		isIssueClosedFn: func(issueNumber int) (bool, error) {
			return false, nil
		},
		pidAliveFn: func(pid int) bool {
			return false // worker is dead
		},
	}

	s := state.NewState()
	s.Sessions["mae-10"] = &state.Session{
		IssueNumber: 110,
		IssueTitle:  "test backoff",
		Status:      state.StatusRunning,
		PID:         9876,
		TmuxSession: "maestro-mae-10",
		Branch:      "feat/mae-10-110-test",
		StartedAt:   time.Now().Add(-10 * time.Minute),
		RetryCount:  0,
	}

	o.checkSessions(s)

	sess := s.Sessions["mae-10"]
	if sess.Status != state.StatusDead {
		t.Fatalf("status = %q, want %q", sess.Status, state.StatusDead)
	}
	if sess.RetryCount != 1 {
		t.Fatalf("retry_count = %d, want 1", sess.RetryCount)
	}
	if sess.NextRetryAt == nil {
		t.Fatal("NextRetryAt should be set for scheduled retry")
	}
	// NextRetryAt should be ~10s in the future (10000ms for attempt 1)
	until := time.Until(*sess.NextRetryAt)
	if until < 5*time.Second || until > 15*time.Second {
		t.Errorf("NextRetryAt should be ~10s from now, got %s", until)
	}
	if sess.FinishedAt == nil {
		t.Fatal("FinishedAt should be set")
	}
}

func TestCheckSessions_AlreadyRetriedWorkerFails(t *testing.T) {
	cfg := &config.Config{
		Repo:              "owner/repo",
		MaxRetryBackoffMs: 300000,
		MaxRuntimeMinutes: 999,
	}
	labeled := make([]string, 0)
	o := &Orchestrator{
		cfg:      cfg,
		notifier: &notify.Notifier{},
		listOpenPRsFn: func() ([]github.PR, error) {
			return []github.PR{}, nil
		},
		isIssueClosedFn: func(issueNumber int) (bool, error) {
			return false, nil
		},
		pidAliveFn: func(pid int) bool {
			return false // worker is dead
		},
		addIssueLabelFn: func(number int, label string) error {
			labeled = append(labeled, label)
			return nil
		},
	}

	s := state.NewState()
	s.Sessions["mae-11"] = &state.Session{
		IssueNumber: 111,
		IssueTitle:  "already retried",
		Status:      state.StatusRunning,
		PID:         8765,
		TmuxSession: "maestro-mae-11",
		Branch:      "feat/mae-11-111-test",
		StartedAt:   time.Now().Add(-10 * time.Minute),
		RetryCount:  1, // already retried once
	}

	o.checkSessions(s)

	sess := s.Sessions["mae-11"]
	if sess.Status != state.StatusFailed {
		t.Fatalf("status = %q, want %q", sess.Status, state.StatusFailed)
	}
	if sess.NextRetryAt != nil {
		t.Fatal("NextRetryAt should be nil for permanently failed session")
	}
	found := false
	for _, label := range labeled {
		if label == "blocked" {
			found = true
		}
	}
	if !found {
		t.Error("expected 'blocked' label to be added on permanent failure")
	}
}

func TestRespawnDueRetries_BackoffElapsed_Respawns(t *testing.T) {
	cfg := &config.Config{
		Repo:              "owner/repo",
		MaxRetryBackoffMs: 300000,
		MaxRuntimeMinutes: 999,
	}
	respawned := false
	o := &Orchestrator{
		cfg:        cfg,
		notifier:   &notify.Notifier{},
		promptBase: "test prompt",
		getIssueFn: func(number int) (github.Issue, error) {
			return makeIssue(number, "test issue"), nil
		},
		respawnWorkerFn: func(cfg *config.Config, slotName string, sess *state.Session, repo string, issue github.Issue, promptBase string, backend string) error {
			respawned = true
			sess.Status = state.StatusRunning
			sess.PID = 5555
			return nil
		},
	}

	pastTime := time.Now().UTC().Add(-1 * time.Second) // backoff already elapsed
	s := state.NewState()
	s.Sessions["mae-12"] = &state.Session{
		IssueNumber: 112,
		IssueTitle:  "test issue",
		Status:      state.StatusDead,
		RetryCount:  1,
		NextRetryAt: &pastTime,
		Branch:      "feat/mae-12-112-test",
	}

	o.respawnDueRetries(s)

	if !respawned {
		t.Fatal("expected worker to be respawned after backoff elapsed")
	}
	sess := s.Sessions["mae-12"]
	if sess.NextRetryAt != nil {
		t.Fatal("NextRetryAt should be nil after respawn")
	}
	if sess.Status != state.StatusRunning {
		t.Errorf("status = %q, want %q", sess.Status, state.StatusRunning)
	}
}

func TestRespawnDueRetries_BackoffNotElapsed_Waits(t *testing.T) {
	cfg := &config.Config{
		Repo:              "owner/repo",
		MaxRetryBackoffMs: 300000,
	}
	respawned := false
	o := &Orchestrator{
		cfg:      cfg,
		notifier: &notify.Notifier{},
		respawnWorkerFn: func(cfg *config.Config, slotName string, sess *state.Session, repo string, issue github.Issue, promptBase string, backend string) error {
			respawned = true
			return nil
		},
	}

	futureTime := time.Now().UTC().Add(1 * time.Hour) // backoff not yet elapsed
	s := state.NewState()
	s.Sessions["mae-13"] = &state.Session{
		IssueNumber: 113,
		IssueTitle:  "waiting",
		Status:      state.StatusDead,
		RetryCount:  1,
		NextRetryAt: &futureTime,
		Branch:      "feat/mae-13-113-test",
	}

	o.respawnDueRetries(s)

	if respawned {
		t.Fatal("worker should NOT be respawned while backoff is still pending")
	}
	sess := s.Sessions["mae-13"]
	if sess.NextRetryAt == nil {
		t.Fatal("NextRetryAt should still be set while waiting")
	}
	if sess.Status != state.StatusDead {
		t.Errorf("status = %q, want %q", sess.Status, state.StatusDead)
	}
}

func TestRespawnDueRetries_RespawnFails_MarksAsFailed(t *testing.T) {
	cfg := &config.Config{
		Repo:              "owner/repo",
		MaxRetryBackoffMs: 300000,
	}
	o := &Orchestrator{
		cfg:      cfg,
		notifier: &notify.Notifier{},
		getIssueFn: func(number int) (github.Issue, error) {
			return makeIssue(number, "test"), nil
		},
		respawnWorkerFn: func(cfg *config.Config, slotName string, sess *state.Session, repo string, issue github.Issue, promptBase string, backend string) error {
			return fmt.Errorf("respawn error")
		},
	}

	pastTime := time.Now().UTC().Add(-1 * time.Second)
	s := state.NewState()
	s.Sessions["mae-14"] = &state.Session{
		IssueNumber: 114,
		IssueTitle:  "will fail",
		Status:      state.StatusDead,
		RetryCount:  1,
		NextRetryAt: &pastTime,
		Branch:      "feat/mae-14-114-test",
	}

	o.respawnDueRetries(s)

	sess := s.Sessions["mae-14"]
	if sess.Status != state.StatusFailed {
		t.Fatalf("status = %q, want %q", sess.Status, state.StatusFailed)
	}
	if sess.FinishedAt == nil {
		t.Fatal("FinishedAt should be set on failure")
	}
}

func TestIssueInProgress_DeadWithPendingRetry(t *testing.T) {
	s := state.NewState()
	futureTime := time.Now().UTC().Add(1 * time.Hour)
	s.Sessions["mae-15"] = &state.Session{
		IssueNumber: 115,
		Status:      state.StatusDead,
		RetryCount:  1,
		NextRetryAt: &futureTime,
	}

	if !s.IssueInProgress(115) {
		t.Fatal("IssueInProgress should return true for dead session with pending retry")
	}
}

func TestIssueInProgress_DeadWithoutRetry(t *testing.T) {
	s := state.NewState()
	s.Sessions["mae-16"] = &state.Session{
		IssueNumber: 116,
		Status:      state.StatusDead,
		RetryCount:  1,
		NextRetryAt: nil, // no pending retry
	}

	if s.IssueInProgress(116) {
		t.Fatal("IssueInProgress should return false for dead session without pending retry")
	}
}

// --- syncProject tests ---

func TestSyncProject_DisabledByDefault(t *testing.T) {
	// When github_projects is not enabled, syncProject should be a no-op (not panic)
	o := &Orchestrator{
		cfg: &config.Config{Repo: "owner/repo"},
		gh:  github.New("owner/repo"),
	}
	// Should not panic or make any API calls
	o.syncProject(42, github.ProjectStatusInProgress)
}

func TestSyncProject_DisabledWhenNoProjectNumber(t *testing.T) {
	o := &Orchestrator{
		cfg: &config.Config{
			Repo: "owner/repo",
			GitHubProjects: config.GitHubProjectsConfig{
				Enabled:       true,
				ProjectNumber: 0, // not set
			},
		},
		gh: github.New("owner/repo"),
	}
	// Should not panic or make any API calls
	o.syncProject(42, github.ProjectStatusInProgress)
}

func TestCheckSessions_SoftTokenThreshold_CheckpointsAndRespawns(t *testing.T) {
	cfg := &config.Config{
		Repo:                     "owner/repo",
		WorkerMaxTokens:          100000,
		WorkerSoftTokenThreshold: 0.8, // soft limit at 80,000
		MaxRuntimeMinutes:        999,
	}
	// Worker reports 85,000 tokens — above soft (80k) but below hard (100k)
	stopped := make([]string, 0)
	checkpointSaved := false
	respawned := false
	o := &Orchestrator{
		cfg:      cfg,
		notifier: &notify.Notifier{},
		listOpenPRsFn: func() ([]github.PR, error) {
			return []github.PR{}, nil
		},
		isIssueClosedFn: func(issueNumber int) (bool, error) {
			return false, nil
		},
		pidAliveFn: func(pid int) bool {
			return true
		},
		captureTmuxFn: func(session string) (string, error) {
			return "tokens 85000 (in 30000 / out 55000)", nil
		},
		workerStopFn: func(cfg *config.Config, slotName string, sess *state.Session) error {
			stopped = append(stopped, slotName)
			return nil
		},
		saveCheckpointFn: func(worktreePath string, issueNumber int, tokensUsed int) error {
			checkpointSaved = true
			return nil
		},
		getIssueFn: func(number int) (github.Issue, error) {
			return github.Issue{Number: number, Title: "test issue"}, nil
		},
		respawnWorkerFn: func(cfg *config.Config, slotName string, sess *state.Session, repo string, issue github.Issue, promptBase string, backendName string) error {
			respawned = true
			sess.Status = state.StatusRunning
			sess.StartedAt = time.Now().UTC()
			sess.FinishedAt = nil
			sess.LastNotifiedStatus = ""
			return nil
		},
	}

	s := state.NewState()
	s.Sessions["mae-10"] = &state.Session{
		IssueNumber: 201,
		IssueTitle:  "test soft threshold",
		Status:      state.StatusRunning,
		PID:         5001,
		TmuxSession: "maestro-mae-10",
		Branch:      "feat/mae-10-201-test",
		StartedAt:   time.Now().Add(-30 * time.Minute),
	}

	o.checkSessions(s)

	if !checkpointSaved {
		t.Fatal("checkpoint should have been saved")
	}
	if !respawned {
		t.Fatal("worker should have been respawned")
	}
	if len(stopped) != 1 || stopped[0] != "mae-10" {
		t.Fatalf("stopped = %v, want [mae-10]", stopped)
	}

	sess := s.Sessions["mae-10"]
	if sess.Status != state.StatusRunning {
		t.Fatalf("status = %q, want %q (should be running after respawn)", sess.Status, state.StatusRunning)
	}
	if !sess.CheckpointSaved {
		t.Fatal("CheckpointSaved should be true")
	}
}

func TestCheckSessions_BelowSoftThreshold_NoAction(t *testing.T) {
	cfg := &config.Config{
		Repo:                     "owner/repo",
		WorkerMaxTokens:          100000,
		WorkerSoftTokenThreshold: 0.8, // soft limit at 80,000
		MaxRuntimeMinutes:        999,
	}
	// Worker reports 50,000 tokens — well below soft threshold (80k)
	o, stopped := newCheckSessionsOrchestrator(cfg, "tokens 50000")

	s := state.NewState()
	s.Sessions["mae-11"] = &state.Session{
		IssueNumber: 202,
		Status:      state.StatusRunning,
		PID:         5002,
		TmuxSession: "maestro-mae-11",
		Branch:      "feat/mae-11-202-test",
		StartedAt:   time.Now().Add(-10 * time.Minute),
	}

	o.checkSessions(s)

	sess := s.Sessions["mae-11"]
	if sess.Status != state.StatusRunning {
		t.Fatalf("status = %q, want %q", sess.Status, state.StatusRunning)
	}
	if sess.CheckpointSaved {
		t.Fatal("CheckpointSaved should be false — below soft threshold")
	}
	if len(*stopped) != 0 {
		t.Fatalf("stopped = %v, want empty", *stopped)
	}
}

func TestCheckSessions_SoftTokenThreshold_RespawnFails_MarksDead(t *testing.T) {
	cfg := &config.Config{
		Repo:                     "owner/repo",
		WorkerMaxTokens:          100000,
		WorkerSoftTokenThreshold: 0.8,
		MaxRuntimeMinutes:        999,
	}
	stopped := make([]string, 0)
	o := &Orchestrator{
		cfg:      cfg,
		notifier: &notify.Notifier{},
		listOpenPRsFn: func() ([]github.PR, error) {
			return []github.PR{}, nil
		},
		isIssueClosedFn: func(issueNumber int) (bool, error) {
			return false, nil
		},
		pidAliveFn: func(pid int) bool {
			return true
		},
		captureTmuxFn: func(session string) (string, error) {
			return "tokens 85000", nil
		},
		workerStopFn: func(cfg *config.Config, slotName string, sess *state.Session) error {
			stopped = append(stopped, slotName)
			return nil
		},
		saveCheckpointFn: func(worktreePath string, issueNumber int, tokensUsed int) error {
			return nil
		},
		getIssueFn: func(number int) (github.Issue, error) {
			return github.Issue{Number: number, Title: "test issue"}, nil
		},
		respawnWorkerFn: func(cfg *config.Config, slotName string, sess *state.Session, repo string, issue github.Issue, promptBase string, backendName string) error {
			return fmt.Errorf("respawn failed")
		},
	}

	s := state.NewState()
	s.Sessions["mae-12"] = &state.Session{
		IssueNumber: 203,
		IssueTitle:  "test soft threshold fail",
		Status:      state.StatusRunning,
		PID:         5003,
		TmuxSession: "maestro-mae-12",
		Branch:      "feat/mae-12-203-test",
		StartedAt:   time.Now().Add(-30 * time.Minute),
	}

	o.checkSessions(s)

	sess := s.Sessions["mae-12"]
	if sess.Status != state.StatusDead {
		t.Fatalf("status = %q, want %q", sess.Status, state.StatusDead)
	}
	if sess.FinishedAt == nil {
		t.Fatal("finished_at should be set")
	}
}

func TestCheckSessions_SoftTokenAlreadyNotified_SkipsSoftCheck(t *testing.T) {
	cfg := &config.Config{
		Repo:                     "owner/repo",
		WorkerMaxTokens:          100000,
		WorkerSoftTokenThreshold: 0.8,
		MaxRuntimeMinutes:        999,
	}
	// Worker reports 85,000 tokens — above soft threshold but already notified
	o, stopped := newCheckSessionsOrchestrator(cfg, "tokens 85000")

	s := state.NewState()
	s.Sessions["mae-13"] = &state.Session{
		IssueNumber:        204,
		Status:             state.StatusRunning,
		PID:                5004,
		TmuxSession:        "maestro-mae-13",
		Branch:             "feat/mae-13-204-test",
		StartedAt:          time.Now().Add(-10 * time.Minute),
		LastNotifiedStatus: "soft_token", // already handled
	}

	o.checkSessions(s)

	sess := s.Sessions["mae-13"]
	if sess.Status != state.StatusRunning {
		t.Fatalf("status = %q, want %q (soft_token already notified, should skip)", sess.Status, state.StatusRunning)
	}
	if len(*stopped) != 0 {
		t.Fatalf("stopped = %v, want empty", *stopped)
	}
}
