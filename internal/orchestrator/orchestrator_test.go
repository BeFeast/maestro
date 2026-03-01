package orchestrator

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/github"
	"github.com/befeast/maestro/internal/notify"
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
			Repo:      "owner/repo",
			LocalPath: "/tmp",
			DeployCmd: "echo deploy-ok",
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
			Repo:      "owner/repo",
			LocalPath: "/tmp",
			DeployCmd: "exit 1",
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
			Repo:      "owner/repo",
			LocalPath: "/tmp",
			DeployCmd: "echo hello-deploy && exit 1",
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
	o := &Orchestrator{cfg: cfgWithBackends("claude", "claude", "codex", "gemini")}
	got := o.resolveBackend(makeIssue(1, "Fix bug", "model:codex"))
	if got != "codex" {
		t.Errorf("resolveBackend() = %q, want %q", got, "codex")
	}
}

func TestResolveBackend_ModelLabelGemini(t *testing.T) {
	o := &Orchestrator{cfg: cfgWithBackends("claude", "claude", "codex", "gemini")}
	got := o.resolveBackend(makeIssue(2, "Add feature", "enhancement", "model:gemini"))
	if got != "gemini" {
		t.Errorf("resolveBackend() = %q, want %q", got, "gemini")
	}
}

func TestResolveBackend_UnknownBackendFallsToDefault(t *testing.T) {
	o := &Orchestrator{cfg: cfgWithBackends("claude", "claude", "codex")}
	got := o.resolveBackend(makeIssue(3, "Fix bug", "model:nonexistent"))
	if got != "claude" {
		t.Errorf("resolveBackend() = %q, want %q (unknown backend should fall back to default)", got, "claude")
	}
}

func TestResolveBackend_NoLabelReturnsDefault(t *testing.T) {
	o := &Orchestrator{cfg: cfgWithBackends("claude", "claude", "codex")}
	got := o.resolveBackend(makeIssue(4, "Fix bug"))
	if got != "claude" {
		t.Errorf("resolveBackend() = %q, want %q", got, "claude")
	}
}

func TestResolveBackend_NoLabelWithAutoRouting(t *testing.T) {
	cfg := cfgWithBackends("claude", "claude", "codex")
	cfg.Routing.Mode = "auto"
	o := &Orchestrator{
		cfg: cfg,
		routeFn: func(issue github.Issue) (string, string, error) {
			return "codex", "simple fix", nil
		},
	}
	got := o.resolveBackend(makeIssue(5, "Simple fix"))
	if got != "codex" {
		t.Errorf("resolveBackend() = %q, want %q", got, "codex")
	}
}

func TestResolveBackend_LabelOverridesAutoRouting(t *testing.T) {
	cfg := cfgWithBackends("claude", "claude", "codex", "gemini")
	cfg.Routing.Mode = "auto"
	routerCalled := false
	o := &Orchestrator{
		cfg: cfg,
		routeFn: func(issue github.Issue) (string, string, error) {
			routerCalled = true
			return "codex", "router pick", nil
		},
	}
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
	o := &Orchestrator{
		cfg: cfg,
		routeFn: func(issue github.Issue) (string, string, error) {
			return "", "", fmt.Errorf("network error")
		},
	}
	got := o.resolveBackend(makeIssue(7, "Fix bug"))
	if got != "claude" {
		t.Errorf("resolveBackend() = %q, want %q (should fall back on router error)", got, "claude")
	}
}

func TestResolveBackend_AutoRoutingDisabled(t *testing.T) {
	cfg := cfgWithBackends("claude", "claude", "codex")
	cfg.Routing.Mode = "manual"
	routerCalled := false
	o := &Orchestrator{
		cfg: cfg,
		routeFn: func(issue github.Issue) (string, string, error) {
			routerCalled = true
			return "codex", "router pick", nil
		},
	}
	got := o.resolveBackend(makeIssue(8, "Fix bug"))
	if got != "claude" {
		t.Errorf("resolveBackend() = %q, want %q", got, "claude")
	}
	if routerCalled {
		t.Error("router should not be called when routing mode is not auto")
	}
}

func TestResolveBackend_EmptyModelLabelIgnored(t *testing.T) {
	o := &Orchestrator{cfg: cfgWithBackends("claude", "claude", "codex")}
	// "model:" with no value after the colon should be ignored
	got := o.resolveBackend(makeIssue(9, "Fix bug", "model:"))
	if got != "claude" {
		t.Errorf("resolveBackend() = %q, want %q (empty model: label should be ignored)", got, "claude")
	}
}

func TestResolveBackend_MultipleLabelsFirstModelWins(t *testing.T) {
	o := &Orchestrator{cfg: cfgWithBackends("claude", "claude", "codex", "gemini")}
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
