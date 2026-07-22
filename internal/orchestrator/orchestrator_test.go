package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/github"
	"github.com/befeast/maestro/internal/notify"
	"github.com/befeast/maestro/internal/outcome"
	"github.com/befeast/maestro/internal/pipeline"
	"github.com/befeast/maestro/internal/router"
	"github.com/befeast/maestro/internal/state"
	"github.com/befeast/maestro/internal/supervisor"
	"github.com/befeast/maestro/internal/worker"
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

func boolPtr(v bool) *bool {
	return &v
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

func TestRespawnPreservingWorktreeRepairsExistingDirectoryWithInvalidGitMetadata(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	base := filepath.Join(root, "worktrees")
	worktree := filepath.Join(base, "ok-player-277")
	branch := "feat/ok-player-277-346"
	for _, args := range [][]string{
		{"init", "-b", "main", repo},
		{"-C", repo, "config", "user.email", "test@example.com"},
		{"-C", repo, "config", "user.name", "Test"},
	} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(repo, "base.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"-C", repo, "add", "base.txt"},
		{"-C", repo, "commit", "-m", "base"},
		{"-C", repo, "branch", branch},
	} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	if err := os.MkdirAll(worktree, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worktree, ".git"), []byte("gitdir: /missing/admin/metadata\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worktree, "preserved.txt"), []byte("do not lose\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	branchesBefore, err := exec.Command("git", "-C", repo, "for-each-ref", "--format=%(refname)", "refs/heads").CombinedOutput()
	if err != nil {
		t.Fatalf("list branches before recovery: %v: %s", err, branchesBefore)
	}

	cfg := &config.Config{LocalPath: repo, WorktreeBase: base}
	sess := &state.Session{IssueNumber: 346, PRNumber: 397, Worktree: worktree, Branch: branch}
	respawned := false
	o := &Orchestrator{
		cfg:               cfg,
		restoreWorktreeFn: worker.RestoreMissingWorktree,
		respawnInPlaceFn: func(_ *config.Config, slotName string, got *state.Session, _ string, _ github.Issue, _, _ string) error {
			respawned = true
			if slotName != "ok-player-277" {
				t.Fatalf("slot = %q, want canonical ok-player-277", slotName)
			}
			if got.Worktree != worktree || got.Branch != branch || got.PRNumber != 397 {
				t.Fatalf("canonical identity changed: worktree=%q branch=%q PR=%d", got.Worktree, got.Branch, got.PRNumber)
			}
			out, err := exec.Command("git", "-C", worktree, "rev-parse", "--show-toplevel").CombinedOutput()
			if err != nil || filepath.Clean(strings.TrimSpace(string(out))) != filepath.Clean(worktree) {
				t.Fatalf("restored worktree root = %q, %v; want %q", strings.TrimSpace(string(out)), err, worktree)
			}
			return nil
		},
	}
	if err := o.respawnPreservingWorktreeWithConfig(cfg, "ok-player-277", sess, github.Issue{Number: 346}, "prompt", "sol"); err != nil {
		t.Fatalf("respawn preserving worktree: %v", err)
	}
	if !respawned {
		t.Fatal("canonical in-place respawn was not invoked")
	}
	branchesAfter, err := exec.Command("git", "-C", repo, "for-each-ref", "--format=%(refname)", "refs/heads").CombinedOutput()
	if err != nil || string(branchesAfter) != string(branchesBefore) {
		t.Fatalf("branches changed during recovery: before=%q after=%q err=%v", branchesBefore, branchesAfter, err)
	}
	backups, err := filepath.Glob(worktree + ".orphaned-*")
	if err != nil || len(backups) != 1 {
		t.Fatalf("orphan backups = %v, %v; want one", backups, err)
	}
	if got, err := os.ReadFile(filepath.Join(backups[0], "preserved.txt")); err != nil || string(got) != "do not lose\n" {
		t.Fatalf("preserved orphan content = %q, %v", got, err)
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
	retryAt := time.Now().UTC().Add(-time.Minute)
	s.Sessions["mae-5"] = &state.Session{
		IssueNumber: 105,
		IssueTitle:  "fix crash",
		Status:      state.StatusRunning,
		PID:         9999,
		TmuxSession: "maestro-mae-5",
		Branch:      "feat/mae-5-105-fix-crash",
		NextRetryAt: &retryAt,
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
	if sess.NextRetryAt != nil {
		t.Fatalf("next_retry_at = %v, want nil after authoritative PR-open reconciliation", sess.NextRetryAt)
	}
	if sess.FinishedAt == nil {
		t.Fatal("finished_at should be set")
	}
	// Crucially: IssueInProgress must return true so no duplicate worker is spawned
	if !s.IssueInProgress(105) {
		t.Fatal("IssueInProgress(105) must return true after transition to pr_open")
	}
}

// A Dead session QUEUED for an in-place respawn (NextRetryAt set — the
// review-feedback / CI-failure / rebase retry path) must NOT be flipped to
// pr_open by the Step-1 reconcile. Flipping clears the Dead status before the
// Step-2b respawnDueRetries — which only relaunches StatusDead sessions — so the
// worker never respawns and the maintenance-retry budget burns to a held PR
// without a single real fix attempt (#758 in-place-respawn race).
func TestReconcileRunningSessions_DeadWithPendingRetry_NotFlippedToPROpen(t *testing.T) {
	s := state.NewState()
	retryAt := time.Now().UTC().Add(10 * time.Second)
	s.Sessions["sup-9"] = &state.Session{
		IssueNumber: 200,
		IssueTitle:  "review-repair retry",
		Status:      state.StatusDead,
		NextRetryAt: &retryAt,
		RetryCount:  1,
		Branch:      "feat/sup-9-200-fix",
		PRNumber:    300,
	}
	openPRs := []github.PR{{Number: 300, HeadRefName: "feat/sup-9-200-fix", Title: "fix"}}
	o := &Orchestrator{
		cfg:                 &config.Config{StateDir: t.TempDir()},
		pidAliveFn:          func(pid int) bool { return false },
		tmuxSessionExistsFn: func(name string) bool { return false },
		listOpenPRsFn:       func() ([]github.PR, error) { return openPRs, nil },
	}

	// checkSessions (Step 2) is the reconcile that flips terminal sessions with
	// an open PR to pr_open — and where the in-place-respawn race lives.
	o.checkSessions(s)

	sess := s.Sessions["sup-9"]
	if sess.Status != state.StatusDead {
		t.Fatalf("status = %q, want %q — a retry-queued Dead session must stay Dead so respawnDueRetries relaunches it", sess.Status, state.StatusDead)
	}
	if sess.NextRetryAt == nil {
		t.Fatal("NextRetryAt must be preserved — the in-place respawn depends on it")
	}
}

func TestCheckSessions_PROpenClearsStaleRetryMarker(t *testing.T) {
	retryAt := time.Now().UTC().Add(-time.Minute)
	s := state.NewState()
	s.Sessions["ok-player-302"] = &state.Session{
		IssueNumber: 406,
		IssueTitle:  "canonical Flatpak repair",
		Status:      state.StatusPROpen,
		NextRetryAt: &retryAt,
		RetryCount:  2,
		Branch:      "feat/ok-player-345-flatpak-beta-retry",
		PRNumber:    388,
	}
	o := &Orchestrator{
		cfg:                 &config.Config{StateDir: t.TempDir()},
		listOpenPRsFn:       func() ([]github.PR, error) { return nil, nil },
		isIssueClosedFn:     func(int) (bool, error) { return false, nil },
		pidAliveFn:          func(int) bool { return false },
		tmuxSessionExistsFn: func(string) bool { return false },
	}

	o.checkSessions(s)

	sess := s.Sessions["ok-player-302"]
	if sess.Status != state.StatusPROpen {
		t.Fatalf("status = %q, want %q", sess.Status, state.StatusPROpen)
	}
	if sess.NextRetryAt != nil {
		t.Fatalf("next_retry_at = %v, want nil for pr_open canonical session", sess.NextRetryAt)
	}
	if sess.RetryCount != 2 {
		t.Fatalf("retry_count = %d, want preserved history 2", sess.RetryCount)
	}
}

func TestCheckSessions_DonePRReleasesTerminalClaimAfterIssueCloses(t *testing.T) {
	finishedAt := time.Now().UTC().Add(-time.Minute)
	s := state.NewState()
	s.Sessions["ok-player-274"] = &state.Session{
		IssueNumber: 365, Status: state.StatusDone, PRNumber: 370, FinishedAt: &finishedAt,
	}
	if _, ok := s.IssueClaimFor(365); !ok {
		t.Fatal("done PR must hold a terminal reconciliation claim before issue closure")
	}
	o := &Orchestrator{
		cfg:                 &config.Config{StateDir: t.TempDir()},
		listOpenPRsFn:       func() ([]github.PR, error) { return nil, nil },
		isIssueClosedFn:     func(issueNumber int) (bool, error) { return issueNumber == 365, nil },
		pidAliveFn:          func(int) bool { return false },
		tmuxSessionExistsFn: func(string) bool { return false },
	}

	o.checkSessions(s)

	if !s.Sessions["ok-player-274"].ReleasedForRedispatch {
		t.Fatal("closed issue must release its completed PR terminal claim")
	}
	if _, ok := s.IssueClaimFor(365); ok {
		t.Fatal("closed issue retained terminal reconciliation claim")
	}
}

func TestReconcileRunningSessions_DeadWorkerWithOpenPR_CapturesTokensFromPersistedLog(t *testing.T) {
	stateDir := t.TempDir()
	logDir := state.LogDir(stateDir)
	if err := os.MkdirAll(logDir, 0755); err != nil {
		t.Fatalf("mkdir log dir: %v", err)
	}
	logPath := logDir + "/sup-150.log"
	if err := os.WriteFile(logPath, []byte("worker output\n\ntokens used\n100,965\nImplemented and opened PR\n"), 0644); err != nil {
		t.Fatalf("write worker log: %v", err)
	}

	s := state.NewState()
	s.Sessions["sup-150"] = &state.Session{
		IssueNumber:       633,
		IssueTitle:        "capture token total",
		Status:            state.StatusRunning,
		PID:               9999,
		TmuxSession:       "maestro-sup-150",
		Branch:            "feat/sup-150-633-cost-obs",
		TokensUsedAttempt: 2000,
		TokensUsedTotal:   5000,
	}

	o := &Orchestrator{
		cfg:                 &config.Config{StateDir: stateDir},
		pidAliveFn:          func(pid int) bool { return false },
		tmuxSessionExistsFn: func(name string) bool { return false },
		listOpenPRsFn: func() ([]github.PR, error) {
			return []github.PR{{Number: 632, HeadRefName: "feat/sup-150-633-cost-obs", Title: "cost obs"}}, nil
		},
	}

	changed := o.reconcileRunningSessions(s)
	if !changed {
		t.Fatal("expected reconciliation to report changes")
	}

	sess := s.Sessions["sup-150"]
	if sess.Status != state.StatusPROpen {
		t.Fatalf("status = %q, want %q", sess.Status, state.StatusPROpen)
	}
	if sess.PRNumber != 632 {
		t.Fatalf("pr_number = %d, want 632", sess.PRNumber)
	}
	if sess.TokensUsedAttempt != 100965 {
		t.Fatalf("tokens_used_attempt = %d, want 100965", sess.TokensUsedAttempt)
	}
	if sess.TokensUsedTotal != 103965 {
		t.Fatalf("tokens_used_total = %d, want 103965", sess.TokensUsedTotal)
	}
	if sess.PID != 0 {
		t.Fatalf("pid = %d, want 0", sess.PID)
	}
	if sess.TmuxSession != "" {
		t.Fatalf("tmux_session = %q, want empty", sess.TmuxSession)
	}
}

func TestReconcileRunningSessions_PushedBranchWithoutPR_AutoCreatesPR(t *testing.T) {
	s := state.NewState()
	t0 := time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC)
	t1 := t0.Add(12 * time.Minute)
	s.Sessions["mae-8"] = &state.Session{
		IssueNumber: 108,
		IssueTitle:  "add branch rescue",
		Status:      state.StatusRunning,
		PID:         8080,
		TmuxSession: "maestro-mae-8",
		Branch:      "feat/mae-8-108-add-branch-rescue",
		Worktree:    "/tmp/mae-8",
		Attribution: []state.BackendAttribution{
			{
				Backend:   "codex",
				Provider:  "openai",
				Model:     "gpt-5.5",
				Effort:    "medium",
				StartedAt: t0,
				EndedAt:   &t1,
				EndReason: "fallover",
			},
			{
				Backend:   "claude",
				Provider:  "anthropic",
				Model:     "opus-4.8",
				Effort:    "xhigh",
				StartedAt: t1,
			},
		},
	}

	var gotTitle, gotBody, gotBase, gotHead string
	o := &Orchestrator{
		pidAliveFn:          func(pid int) bool { return false },
		tmuxSessionExistsFn: func(name string) bool { return false },
		listOpenPRsFn:       func() ([]github.PR, error) { return []github.PR{}, nil },
		remoteBranchExistsFn: func(branch string) (bool, error) {
			return branch == "feat/mae-8-108-add-branch-rescue", nil
		},
		createPRFn: func(title, body, base, head string) (int, error) {
			gotTitle, gotBody, gotBase, gotHead = title, body, base, head
			return 144, nil
		},
	}

	changed := o.reconcileRunningSessions(s)
	if !changed {
		t.Fatal("expected reconciliation to report changes")
	}

	sess := s.Sessions["mae-8"]
	if sess.Status != state.StatusPROpen {
		t.Fatalf("status = %q, want %q", sess.Status, state.StatusPROpen)
	}
	if sess.PRNumber != 144 {
		t.Fatalf("pr_number = %d, want 144", sess.PRNumber)
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
	if gotBase != "main" {
		t.Fatalf("base = %q, want main", gotBase)
	}
	if gotHead != "feat/mae-8-108-add-branch-rescue" {
		t.Fatalf("head = %q", gotHead)
	}
	if !strings.Contains(gotTitle, "add branch rescue") || !strings.Contains(gotTitle, "(#108)") {
		t.Fatalf("unexpected title %q", gotTitle)
	}
	if !strings.Contains(gotBody, "Refs #108") || strings.Contains(gotBody, "Closes #108") || !strings.Contains(gotBody, "auto-created") {
		t.Fatalf("unexpected body %q", gotBody)
	}
	if !strings.Contains(gotBody, "feat/mae-8-108-add-branch-rescue") {
		t.Fatalf("body missing branch name: %q", gotBody)
	}
	// The PR body lands on the target repo, which may be public: no backend
	// attribution, pids, tmux session names, or host-side paths (#799).
	for _, leak := range []string{
		"Maestro-Backend", "pid", "tmux", "state_dir",
		"codex", "openai", "gpt-5.5", "claude", "anthropic", "opus-4.8",
		"8080", "/tmp/mae-8",
	} {
		if strings.Contains(gotBody, leak) {
			t.Fatalf("PR body leaks orchestration internals (%q): %q", leak, gotBody)
		}
	}
	if !s.IssueInProgress(108) {
		t.Fatal("IssueInProgress(108) must remain true after auto-created PR")
	}
}

// Backend attribution is internal control-plane state. Reconciliation must
// adopt the existing PR without changing its public commit or PR body (#1000).
func TestReconcileRunningSessions_OpenPR_DoesNotAmendProductCommit(t *testing.T) {
	s := state.NewState()
	t0 := time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC)
	s.Sessions["mae-9"] = &state.Session{
		IssueNumber: 109,
		IssueTitle:  "existing pr",
		Status:      state.StatusRunning,
		PID:         9090,
		TmuxSession: "maestro-mae-9",
		Branch:      "feat/mae-9-109-existing-pr",
		Worktree:    "/tmp/mae-9",
		Attribution: []state.BackendAttribution{{
			Backend:   "codex",
			Provider:  "openai",
			Model:     "gpt-5.5",
			Effort:    "medium",
			StartedAt: t0,
		}},
	}

	o := &Orchestrator{
		pidAliveFn:          func(pid int) bool { return false },
		tmuxSessionExistsFn: func(name string) bool { return false },
		listOpenPRsFn: func() ([]github.PR, error) {
			return []github.PR{{
				Number:      145,
				HeadRefName: "feat/mae-9-109-existing-pr",
				Body:        "Refs #109\n",
			}}, nil
		},
	}

	changed := o.reconcileRunningSessions(s)
	if !changed {
		t.Fatal("expected reconciliation to report changes")
	}
	sess := s.Sessions["mae-9"]
	if sess.Status != state.StatusPROpen {
		t.Fatalf("status = %q, want %q", sess.Status, state.StatusPROpen)
	}
	if sess.PRNumber != 145 {
		t.Fatalf("pr_number = %d, want 145", sess.PRNumber)
	}
}

func TestAutoCreatedPRBody_NoOrchestrationInternals(t *testing.T) {
	sess := &state.Session{
		IssueNumber: 799,
		IssueTitle:  "sanitize auto-created PR bodies",
		PID:         4242,
		TmuxSession: "maestro-mae-1",
		Worktree:    "/srv/example-worktrees/mae-1",
	}
	body := autoCreatedPRBody(sess, "feat/mae-1-799-sanitize")
	if !strings.Contains(body, "Refs #799") {
		t.Fatalf("body missing issue ref: %q", body)
	}
	if !strings.Contains(body, "feat/mae-1-799-sanitize") {
		t.Fatalf("body missing branch name: %q", body)
	}
	for _, leak := range []string{"Maestro-Backend", "pid", "tmux", "state_dir", "4242", "maestro-mae-1", "/srv/example-worktrees"} {
		if strings.Contains(body, leak) {
			t.Fatalf("PR body leaks orchestration internals (%q): %q", leak, body)
		}
	}
}

func TestCreatePR_NoAttributionPolicyRejectsForbiddenPublicText(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "AGENTS.md"), []byte("No AI attribution anywhere in git/GitHub artifacts.\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	called := false
	o := &Orchestrator{
		cfg: &config.Config{LocalPath: root},
		createPRFn: func(title, body, base, head string) (int, error) {
			called = true
			return 974, nil
		},
	}
	if _, err := o.createPR("policy regression", "Refs #974\n\nMaestro-Backend: sol openai gpt-5.6-sol\n", "main", "feat/policy"); err == nil {
		t.Fatal("forbidden PR attribution was published")
	}
	if called {
		t.Fatal("PR creation reached the public write after policy rejection")
	}

	if _, err := o.createPR("policy regression", "Refs #974\n", "main", "feat/policy"); err != nil {
		t.Fatalf("clean PR text was rejected: %v", err)
	}
	if !called {
		t.Fatal("clean PR text did not reach the public write")
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

// mockRateLimitReset returns a fixed parseable reset time, used by reconcile
// rate-limit tests to simulate a high-confidence provider rate-limit response.
// Per #663, the orchestrator only triggers backend fallback when the
// rate-limit signal is accompanied by a parseable reset window — tests that
// want to exercise the fallback path mock this so providerRateLimitFromLog
// reports hit=true.
func mockRateLimitReset(string) *time.Time {
	r := time.Date(2027, time.January, 1, 12, 0, 0, 0, time.UTC)
	return &r
}

// TestReconcileRunningSessions_RateLimitedDeadWorker_DoesNotBurnRetryBudget
// guards #466: when reconcile observes a dead worker whose log carries a
// provider rate-limit signature, it must record the provider limit and skip
// counting the session as a failed attempt for the issue, so the per-issue
// retry budget is preserved through transient backend blocks.
func TestReconcileRunningSessions_RateLimitedDeadWorker_DoesNotBurnRetryBudget(t *testing.T) {
	s := state.NewState()
	s.Sessions["sup-77"] = &state.Session{
		IssueNumber: 353,
		IssueTitle:  "P0: detect outcome drift",
		Status:      state.StatusRunning,
		PID:         424242,
		TmuxSession: "maestro-sup-77",
		Branch:      "feat/sup-77-353-detect-outcome-drift",
		Backend:     "codex",
		StartedAt:   time.Now().Add(-2 * time.Minute),
		LogFile:     "/tmp/sup-77-rl.log",
	}

	o := &Orchestrator{
		cfg: &config.Config{
			Repo: "owner/repo",
			Model: config.ModelConfig{
				Default:  "codex",
				Backends: map[string]config.BackendDef{"codex": {Cmd: "codex"}},
			},
		},
		notifier:                &notify.Notifier{},
		pidAliveFn:              func(pid int) bool { return false },
		tmuxSessionExistsFn:     func(name string) bool { return false },
		listOpenPRsFn:           func() ([]github.PR, error) { return []github.PR{}, nil },
		isRateLimitedFn:         func(logFile string) bool { return true },
		rateLimitResetFromLogFn: mockRateLimitReset,
	}

	changed := o.reconcileRunningSessions(s)
	if !changed {
		t.Fatal("expected reconciliation to report changes")
	}

	sess := s.Sessions["sup-77"]
	if sess.Status != state.StatusDead {
		t.Fatalf("status = %q, want %q", sess.Status, state.StatusDead)
	}
	if !sess.RateLimitHit {
		t.Fatal("RateLimitHit should be true so retry budget is preserved")
	}
	if sess.LastNotifiedStatus != "rate_limit" {
		t.Fatalf("last_notified_status = %q, want %q", sess.LastNotifiedStatus, "rate_limit")
	}
	if sess.ProviderLimitBackend != "codex" {
		t.Fatalf("provider_limit_backend = %q, want codex", sess.ProviderLimitBackend)
	}
	if failed := s.FailedAttemptsForIssue(353); failed != 0 {
		t.Fatalf("FailedAttemptsForIssue(353) = %d, want 0 — rate-limited dead session must not burn retry budget", failed)
	}
	health, ok := s.BackendHealth["codex"]
	if !ok {
		t.Fatal("BackendHealth[codex] should be recorded by recordProviderLimit")
	}
	if health.State != state.BackendHealthCooldown {
		t.Fatalf("BackendHealth[codex].State = %q, want cooldown", health.State)
	}
}

// #506: when reconcile detects a rate-limited dead worker AND fallback_backends
// is configured with a viable candidate, the session must be respawned on the
// next backend (mirroring the main-loop fallover at orchestrator.go:1648),
// not marked Dead.
func TestReconcileRunningSessions_RateLimitedDeadWorker_FallsOverToNextBackend(t *testing.T) {
	s := state.NewState()
	s.Sessions["sup-90"] = &state.Session{
		IssueNumber: 471,
		IssueTitle:  "P0: ciStatusFromREST returns pending forever",
		Status:      state.StatusRunning,
		PID:         424243,
		TmuxSession: "maestro-sup-90",
		Branch:      "feat/sup-90-471-ci",
		Backend:     "claude",
		StartedAt:   time.Now().Add(-2 * time.Minute),
		LogFile:     "/tmp/sup-90-rl.log",
	}

	respawnedBackends := []string{}
	o := &Orchestrator{
		cfg: &config.Config{
			Repo: "owner/repo",
			Model: config.ModelConfig{
				Default:          "claude",
				FallbackBackends: []string{"codex", "freellm"},
				Backends: map[string]config.BackendDef{
					"claude":  {Cmd: "claude"},
					"codex":   {Cmd: "codex"},
					"freellm": {Cmd: "freellm"},
				},
			},
		},
		notifier:                &notify.Notifier{},
		pidAliveFn:              func(pid int) bool { return false },
		tmuxSessionExistsFn:     func(name string) bool { return false },
		listOpenPRsFn:           func() ([]github.PR, error) { return []github.PR{}, nil },
		isRateLimitedFn:         func(logFile string) bool { return true },
		rateLimitResetFromLogFn: mockRateLimitReset,
		getIssueFn: func(number int) (github.Issue, error) {
			return github.Issue{Number: number, Title: "ciStatusFromREST returns pending forever"}, nil
		},
		respawnWorkerFn: func(cfg *config.Config, slotName string, sess *state.Session, repo string, issue github.Issue, promptBase string, backendName string) error {
			respawnedBackends = append(respawnedBackends, backendName)
			sess.Status = state.StatusRunning
			sess.PID = 9001
			sess.Backend = backendName
			sess.StartedAt = time.Now().UTC()
			sess.FinishedAt = nil
			return nil
		},
	}

	changed := o.reconcileRunningSessions(s)
	if !changed {
		t.Fatal("expected reconciliation to report changes")
	}

	sess := s.Sessions["sup-90"]
	if sess.Status != state.StatusRunning {
		t.Fatalf("status = %q, want %q (fallover should respawn the worker)", sess.Status, state.StatusRunning)
	}
	if got := len(respawnedBackends); got != 1 {
		t.Fatalf("respawnWorkerFn called %d times, want 1; backends=%v", got, respawnedBackends)
	}
	if respawnedBackends[0] != "codex" {
		t.Fatalf("fallback respawn used backend=%q, want %q (first in fallback_backends)", respawnedBackends[0], "codex")
	}
	if sess.Backend != "codex" {
		t.Fatalf("session.Backend = %q after fallover, want %q", sess.Backend, "codex")
	}
	if len(sess.TriedBackends) != 1 || sess.TriedBackends[0] != "claude" {
		t.Fatalf("TriedBackends = %v, want [claude] (the rate-limited backend)", sess.TriedBackends)
	}
	if !sess.RateLimitHit {
		t.Fatal("RateLimitHit should be true after recordProviderLimit")
	}
	if sess.ProviderLimitBackend != "claude" {
		t.Fatalf("ProviderLimitBackend = %q, want claude (the original)", sess.ProviderLimitBackend)
	}
	if sess.BackendSelection == nil || sess.BackendSelection.SelectedBackend != "codex" || sess.BackendSelection.SelectionReason != selectionReasonProviderLimitFallback {
		t.Fatalf("BackendSelection = %+v, want SelectedBackend=codex SelectionReason=%s", sess.BackendSelection, selectionReasonProviderLimitFallback)
	}
	health, ok := s.BackendHealth["claude"]
	if !ok || health.State != state.BackendHealthCooldown {
		t.Fatalf("BackendHealth[claude] = %+v, want cooldown", health)
	}
}

// #506: when fallback respawn itself fails (worktree busy, network, etc.),
// the session must be marked Dead with rate_limit notification rather than
// silently looping in StatusRunning.
func TestReconcileRunningSessions_RateLimitedDeadWorker_FallbackRespawnFails_MarksDead(t *testing.T) {
	s := state.NewState()
	s.Sessions["sup-91"] = &state.Session{
		IssueNumber: 471,
		Status:      state.StatusRunning,
		PID:         424244,
		TmuxSession: "maestro-sup-91",
		Branch:      "feat/sup-91-471-ci",
		Backend:     "claude",
		LogFile:     "/tmp/sup-91-rl.log",
	}

	respawnAttempts := []string{}
	o := &Orchestrator{
		cfg: &config.Config{
			Repo: "owner/repo",
			Model: config.ModelConfig{
				Default:          "claude",
				FallbackBackends: []string{"codex"},
				Backends: map[string]config.BackendDef{
					"claude": {Cmd: "claude"},
					"codex":  {Cmd: "codex"},
				},
			},
		},
		notifier:                &notify.Notifier{},
		pidAliveFn:              func(pid int) bool { return false },
		tmuxSessionExistsFn:     func(name string) bool { return false },
		listOpenPRsFn:           func() ([]github.PR, error) { return []github.PR{}, nil },
		isRateLimitedFn:         func(logFile string) bool { return true },
		rateLimitResetFromLogFn: mockRateLimitReset,
		getIssueFn: func(number int) (github.Issue, error) {
			return github.Issue{Number: number, Title: "ci"}, nil
		},
		respawnWorkerFn: func(cfg *config.Config, slotName string, sess *state.Session, repo string, issue github.Issue, promptBase string, backendName string) error {
			respawnAttempts = append(respawnAttempts, backendName)
			return fmt.Errorf("worktree busy")
		},
	}

	changed := o.reconcileRunningSessions(s)
	if !changed {
		t.Fatal("expected reconciliation to report changes")
	}

	sess := s.Sessions["sup-91"]
	if sess.Status != state.StatusDead {
		t.Fatalf("status = %q, want %q (failed respawn must terminate the session)", sess.Status, state.StatusDead)
	}
	if got := len(respawnAttempts); got != 1 {
		t.Fatalf("respawnWorkerFn called %d times, want 1; attempts=%v", got, respawnAttempts)
	}
	if sess.LastNotifiedStatus != "rate_limit" {
		t.Fatalf("last_notified_status = %q, want %q", sess.LastNotifiedStatus, "rate_limit")
	}
}

// #506: when the issue can't be fetched at fallover time (transient gh outage,
// etc.), reconcile must not skip the dead-marking — otherwise the session
// would be stuck in StatusRunning with no live process.
func TestReconcileRunningSessions_RateLimitedDeadWorker_FetchIssueFails_MarksDead(t *testing.T) {
	s := state.NewState()
	s.Sessions["sup-92"] = &state.Session{
		IssueNumber: 471,
		Status:      state.StatusRunning,
		PID:         424245,
		TmuxSession: "maestro-sup-92",
		Branch:      "feat/sup-92-471-ci",
		Backend:     "claude",
		LogFile:     "/tmp/sup-92-rl.log",
	}

	respawned := false
	o := &Orchestrator{
		cfg: &config.Config{
			Repo: "owner/repo",
			Model: config.ModelConfig{
				Default:          "claude",
				FallbackBackends: []string{"codex"},
				Backends: map[string]config.BackendDef{
					"claude": {Cmd: "claude"},
					"codex":  {Cmd: "codex"},
				},
			},
		},
		notifier:                &notify.Notifier{},
		pidAliveFn:              func(pid int) bool { return false },
		tmuxSessionExistsFn:     func(name string) bool { return false },
		listOpenPRsFn:           func() ([]github.PR, error) { return []github.PR{}, nil },
		isRateLimitedFn:         func(logFile string) bool { return true },
		rateLimitResetFromLogFn: mockRateLimitReset,
		getIssueFn: func(number int) (github.Issue, error) {
			return github.Issue{}, fmt.Errorf("gh transient: rate limit")
		},
		respawnWorkerFn: func(cfg *config.Config, slotName string, sess *state.Session, repo string, issue github.Issue, promptBase string, backendName string) error {
			respawned = true
			return nil
		},
	}

	changed := o.reconcileRunningSessions(s)
	if !changed {
		t.Fatal("expected reconciliation to report changes")
	}

	sess := s.Sessions["sup-92"]
	if sess.Status != state.StatusDead {
		t.Fatalf("status = %q, want %q (fetch-issue error must terminate the session)", sess.Status, state.StatusDead)
	}
	if respawned {
		t.Fatal("respawnWorkerFn must NOT be called when getIssueFn fails")
	}
	if sess.LastNotifiedStatus != "rate_limit" {
		t.Fatalf("last_notified_status = %q, want %q", sess.LastNotifiedStatus, "rate_limit")
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

func TestRunDeployCmd_DoesNotReturnCommandOutput(t *testing.T) {
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
	if strings.Contains(err.Error(), "hello-deploy") || err.Error() != "deploy command failed" {
		t.Errorf("error = %q, want fixed public error without command output", err.Error())
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

// #427: resolveBackendWithReason returns the canonical reason string the
// dispatch loop stamps onto Session.BackendSelection so the dashboard can
// distinguish label-pinned picks from real auto-routed picks from silent
// router_error fallbacks.
func TestResolveBackendWithReason_LabelReturnsReasonLabel(t *testing.T) {
	cfg := cfgWithBackends("claude", "claude", "codex")
	o := &Orchestrator{cfg: cfg, router: router.New(cfg)}
	name, reason := o.resolveBackendWithReason(makeIssue(11, "Fix bug", "model:codex"))
	if name != "codex" || reason != router.ReasonLabel {
		t.Fatalf("resolveBackendWithReason() = (%q, %q), want (codex, label)", name, reason)
	}
}

func TestResolveBackendWithReason_DefaultReturnsReasonDefault(t *testing.T) {
	cfg := cfgWithBackends("claude", "claude", "codex")
	o := &Orchestrator{cfg: cfg, router: router.New(cfg)}
	name, reason := o.resolveBackendWithReason(makeIssue(12, "Fix bug"))
	if name != "claude" || reason != router.ReasonDefault {
		t.Fatalf("resolveBackendWithReason() = (%q, %q), want (claude, default)", name, reason)
	}
}

func TestResolveBackendWithReason_AutoReturnsReasonAuto(t *testing.T) {
	cfg := cfgWithBackends("claude", "claude", "codex")
	cfg.Routing.Mode = "auto"
	r := router.New(cfg)
	r.RouteFn = func(issue github.Issue) (string, string, error) {
		return "codex", "small bugfix", nil
	}
	o := &Orchestrator{cfg: cfg, router: r}
	name, reason := o.resolveBackendWithReason(makeIssue(13, "Small bugfix"))
	if name != "codex" || reason != router.ReasonAuto {
		t.Fatalf("resolveBackendWithReason() = (%q, %q), want (codex, auto)", name, reason)
	}
}

func TestResolveBackendWithReason_RouterFailureReturnsReasonRouterError(t *testing.T) {
	cfg := cfgWithBackends("claude", "claude", "codex")
	cfg.Routing.Mode = "auto"
	r := router.New(cfg)
	r.RouteFn = func(issue github.Issue) (string, string, error) {
		return "", "", fmt.Errorf("network error")
	}
	o := &Orchestrator{cfg: cfg, router: r}
	name, reason := o.resolveBackendWithReason(makeIssue(14, "Fix bug"))
	if name != "claude" || reason != router.ReasonRouterError {
		t.Fatalf("resolveBackendWithReason() = (%q, %q), want (claude, router_error)", name, reason)
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
		getIssueFn: func(number int) (github.Issue, error) {
			return makeIssue(number, "merge candidate"), nil
		},
		ghPRLabelsFn: func(int) ([]string, error) { return nil, nil },
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

func TestAutoMergePRs_MissingOpenPRDoesNotBecomeDoneWhenOutcomePassRequiredFails(t *testing.T) {
	cfg := &config.Config{
		Repo:          "owner/repo",
		MergeStrategy: "parallel",
		Outcome: outcome.Brief{
			DesiredOutcome:      "Live app works",
			VerifierCommand:     "check-live",
			PassRequiredForDone: boolPtr(true),
		},
	}
	o, merged := newMergeTestOrchestrator(cfg, nil)
	o.isPRMergedFn = func(prNumber int) (bool, error) {
		return prNumber == 10, nil
	}
	s := makeTestState([]github.PR{{Number: 10, HeadRefName: "feat/a"}})
	s.OutcomeHealth = &outcome.HealthCheckResult{
		CheckedAt: time.Now().UTC(),
		State:     outcome.HealthFailing,
		Signal:    "verifier_command",
		Summary:   "live verifier failed",
	}

	o.autoMergePRs(s)

	if len(*merged) != 0 {
		t.Fatalf("merged = %v, want no merge when PR is already missing", *merged)
	}
	sess := s.Sessions["slot-0"]
	if sess.Status != state.StatusCodeLanded {
		t.Fatalf("status = %q, want %q until live verification passes", sess.Status, state.StatusCodeLanded)
	}
}

func TestMergeReadyPR_RunsOutcomeVerifierAndMarksDoneOnPass(t *testing.T) {
	cfg := &config.Config{
		Repo: "owner/repo",
		Outcome: outcome.Brief{
			DesiredOutcome:      "Live app works",
			VerifierCommand:     "check-live",
			PassRequiredForDone: boolPtr(true),
		},
	}
	o, merged := newMergeTestOrchestrator(cfg, []github.PR{{Number: 10, HeadRefName: "feat/a"}})
	checked := false
	o.outcomeCheckFn = func(_ context.Context, _ outcome.Brief) outcome.HealthCheckResult {
		checked = true
		return outcome.HealthCheckResult{
			CheckedAt: time.Now().UTC(),
			State:     outcome.HealthHealthy,
			Signal:    "healthcheck_command",
			Summary:   "live verifier passed",
		}
	}
	s := makeTestState([]github.PR{{Number: 10, HeadRefName: "feat/a"}})
	sess := s.Sessions["slot-0"]

	if !o.mergeReadyPR(s, "slot-0", sess, github.PR{Number: 10, HeadRefName: "feat/a"}) {
		t.Fatal("mergeReadyPR should return true on successful merge")
	}
	if len(*merged) != 1 || (*merged)[0] != 10 {
		t.Fatalf("merged = %v, want [10]", *merged)
	}
	if !checked {
		t.Fatal("outcome verifier was not run after merge")
	}
	if s.OutcomeHealth == nil || s.OutcomeHealth.State != outcome.HealthHealthy {
		t.Fatalf("OutcomeHealth = %+v, want healthy", s.OutcomeHealth)
	}
	if sess.Status != state.StatusDone {
		t.Fatalf("status = %q, want %q after verifier pass", sess.Status, state.StatusDone)
	}
}

func TestReconcileCodeLandedSessionsMarksDoneAfterOutcomePass(t *testing.T) {
	cfg := &config.Config{
		Repo: "owner/repo",
		Outcome: outcome.Brief{
			DesiredOutcome:      "Live app works",
			VerifierCommand:     "check-live",
			PassRequiredForDone: boolPtr(true),
		},
	}
	checked := false
	o := &Orchestrator{
		cfg:      cfg,
		notifier: &notify.Notifier{},
		outcomeCheckFn: func(_ context.Context, _ outcome.Brief) outcome.HealthCheckResult {
			checked = true
			return outcome.HealthCheckResult{
				CheckedAt: time.Now().UTC(),
				State:     outcome.HealthHealthy,
				Signal:    "healthcheck_command",
				Summary:   "live verifier passed",
			}
		},
		isPRMergedFn: func(prNumber int) (bool, error) {
			return prNumber == 10, nil
		},
		isIssueClosedFn: func(issueNumber int) (bool, error) {
			return false, nil
		},
	}
	s := state.NewState()
	s.Sessions["slot-0"] = &state.Session{
		IssueNumber: 42,
		IssueTitle:  "landed issue",
		Status:      state.StatusCodeLanded,
		PRNumber:    10,
	}

	o.reconcileCodeLandedSessions(s)

	if !checked {
		t.Fatal("outcome verifier was not run")
	}
	if s.OutcomeHealth == nil || s.OutcomeHealth.State != outcome.HealthHealthy {
		t.Fatalf("OutcomeHealth = %+v, want healthy", s.OutcomeHealth)
	}
	if s.Sessions["slot-0"].Status != state.StatusDone {
		t.Fatalf("status = %q, want %q", s.Sessions["slot-0"].Status, state.StatusDone)
	}
}

func TestReconcileCodeLandedSessionsKeepsCodeLandedWhenOutcomeFails(t *testing.T) {
	cfg := &config.Config{
		Repo: "owner/repo",
		Outcome: outcome.Brief{
			DesiredOutcome:      "Live app works",
			VerifierCommand:     "check-live",
			PassRequiredForDone: boolPtr(true),
		},
	}
	o := &Orchestrator{
		cfg:      cfg,
		notifier: &notify.Notifier{},
		outcomeCheckFn: func(_ context.Context, _ outcome.Brief) outcome.HealthCheckResult {
			return outcome.HealthCheckResult{
				CheckedAt: time.Now().UTC(),
				State:     outcome.HealthFailing,
				Signal:    "healthcheck_command",
				Summary:   "live verifier failed",
			}
		},
	}
	s := state.NewState()
	s.Sessions["slot-0"] = &state.Session{
		IssueNumber: 42,
		IssueTitle:  "landed issue",
		Status:      state.StatusCodeLanded,
		PRNumber:    10,
	}

	o.reconcileCodeLandedSessions(s)

	if s.Sessions["slot-0"].Status != state.StatusCodeLanded {
		t.Fatalf("status = %q, want %q", s.Sessions["slot-0"].Status, state.StatusCodeLanded)
	}
}

func TestReconcileCodeLandedSessionsClosesIssueWhenPolicyAllows(t *testing.T) {
	cfg := &config.Config{
		Repo: "owner/repo",
		Supervisor: config.SupervisorConfig{
			SafeActions: []string{config.SupervisorActionCloseIssue},
		},
	}
	closedIssue := 0
	o := &Orchestrator{
		cfg:      cfg,
		notifier: &notify.Notifier{},
		isPRMergedFn: func(prNumber int) (bool, error) {
			return prNumber == 10, nil
		},
		isIssueClosedFn: func(issueNumber int) (bool, error) {
			return false, nil
		},
		ghCloseIssueFn: func(number int, comment string) error {
			closedIssue = number
			if !strings.Contains(comment, "PR #10") {
				t.Fatalf("close comment = %q, want PR reference", comment)
			}
			return nil
		},
	}
	s := state.NewState()
	s.Sessions["slot-0"] = &state.Session{
		IssueNumber: 42,
		IssueTitle:  "landed issue",
		Status:      state.StatusCodeLanded,
		PRNumber:    10,
	}

	o.reconcileCodeLandedSessions(s)

	if s.Sessions["slot-0"].Status != state.StatusDone {
		t.Fatalf("status = %q, want %q", s.Sessions["slot-0"].Status, state.StatusDone)
	}
	if closedIssue != 42 {
		t.Fatalf("closed issue = %d, want 42", closedIssue)
	}
}

func TestMergeReadyPR_KeepsCodeLandedWhenOutcomeVerifierFails(t *testing.T) {
	cfg := &config.Config{
		Repo: "owner/repo",
		Outcome: outcome.Brief{
			DesiredOutcome:      "Live app works",
			VerifierCommand:     "check-live",
			PassRequiredForDone: boolPtr(true),
		},
	}
	o, _ := newMergeTestOrchestrator(cfg, []github.PR{{Number: 10, HeadRefName: "feat/a"}})
	o.outcomeCheckFn = func(_ context.Context, _ outcome.Brief) outcome.HealthCheckResult {
		return outcome.HealthCheckResult{
			CheckedAt: time.Now().UTC(),
			State:     outcome.HealthFailing,
			Signal:    "healthcheck_command",
			Summary:   "live verifier failed",
		}
	}
	s := makeTestState([]github.PR{{Number: 10, HeadRefName: "feat/a"}})
	sess := s.Sessions["slot-0"]

	if !o.mergeReadyPR(s, "slot-0", sess, github.PR{Number: 10, HeadRefName: "feat/a"}) {
		t.Fatal("mergeReadyPR should return true on successful merge")
	}
	if s.OutcomeHealth == nil || s.OutcomeHealth.State != outcome.HealthFailing {
		t.Fatalf("OutcomeHealth = %+v, want failing", s.OutcomeHealth)
	}
	if sess.Status != state.StatusCodeLanded {
		t.Fatalf("status = %q, want %q until verifier passes", sess.Status, state.StatusCodeLanded)
	}
}

func TestMergeReadyPR_SkipsImmediateOutcomeVerifierWhenDeployRequiredButNotDone(t *testing.T) {
	cfg := &config.Config{
		Repo: "owner/repo",
		Outcome: outcome.Brief{
			DesiredOutcome:      "Live app works",
			VerifierCommand:     "check-live",
			RequiresDeploy:      true,
			PassRequiredForDone: boolPtr(true),
		},
	}
	o, _ := newMergeTestOrchestrator(cfg, []github.PR{{Number: 10, HeadRefName: "feat/a"}})
	o.outcomeCheckFn = func(_ context.Context, _ outcome.Brief) outcome.HealthCheckResult {
		t.Fatal("outcome verifier should wait until deploy succeeds")
		return outcome.HealthCheckResult{}
	}
	s := makeTestState([]github.PR{{Number: 10, HeadRefName: "feat/a"}})
	sess := s.Sessions["slot-0"]

	if !o.mergeReadyPR(s, "slot-0", sess, github.PR{Number: 10, HeadRefName: "feat/a"}) {
		t.Fatal("mergeReadyPR should return true on successful merge")
	}
	if sess.Status != state.StatusCodeLanded {
		t.Fatalf("status = %q, want %q while deploy is pending", sess.Status, state.StatusCodeLanded)
	}
	if sess.DeploymentFinishedAt != nil {
		t.Fatalf("DeploymentFinishedAt = %v, want nil before deploy succeeds", sess.DeploymentFinishedAt)
	}
	if s.OutcomeHealth != nil {
		t.Fatalf("OutcomeHealth = %+v, want nil before deploy succeeds", s.OutcomeHealth)
	}
}

func TestMergeReadyPR_RunsOutcomeVerifierAfterDeploySucceeds(t *testing.T) {
	cfg := &config.Config{
		Repo:                 "owner/repo",
		DeployCmd:            "true",
		DeployTimeoutMinutes: 1,
		Outcome: outcome.Brief{
			DesiredOutcome:      "Live app works",
			VerifierCommand:     "check-live",
			RequiresDeploy:      true,
			PassRequiredForDone: boolPtr(true),
		},
	}
	o, _ := newMergeTestOrchestrator(cfg, []github.PR{{Number: 10, HeadRefName: "feat/a"}})
	checked := false
	o.outcomeCheckFn = func(_ context.Context, _ outcome.Brief) outcome.HealthCheckResult {
		checked = true
		return outcome.HealthCheckResult{
			CheckedAt: time.Now().UTC(),
			State:     outcome.HealthHealthy,
			Signal:    "healthcheck_command",
			Summary:   "live verifier passed",
		}
	}
	s := makeTestState([]github.PR{{Number: 10, HeadRefName: "feat/a"}})
	sess := s.Sessions["slot-0"]

	if !o.mergeReadyPR(s, "slot-0", sess, github.PR{Number: 10, HeadRefName: "feat/a"}) {
		t.Fatal("mergeReadyPR should return true on successful merge")
	}
	if !checked {
		t.Fatal("outcome verifier was not run after deploy succeeded")
	}
	if sess.DeploymentFinishedAt == nil {
		t.Fatal("DeploymentFinishedAt should be recorded after deploy succeeds")
	}
	if sess.Status != state.StatusDone {
		t.Fatalf("status = %q, want %q after deploy and verifier pass", sess.Status, state.StatusDone)
	}
}

func TestOrderedQueueIssueDone_MergedPRWaitsForOutcomeWhenPassRequired(t *testing.T) {
	cfg := &config.Config{
		Repo: "owner/repo",
		Outcome: outcome.Brief{
			DesiredOutcome:      "Live app works",
			VerifierCommand:     "check-live",
			PassRequiredForDone: boolPtr(true),
		},
	}
	o := &Orchestrator{
		cfg: cfg,
		isIssueClosedFn: func(issueNumber int) (bool, error) {
			return false, nil
		},
		hasMergedPRForIssueFn: func(issueNumber int) (bool, error) {
			return true, nil
		},
	}
	mergedAt := time.Now().UTC().Add(-time.Minute)
	s := state.NewState()
	s.LastMergeAt = mergedAt
	s.OutcomeHealth = &outcome.HealthCheckResult{
		CheckedAt: time.Now().UTC(),
		State:     outcome.HealthFailing,
		Signal:    "healthcheck_command",
		Summary:   "live verifier failed",
	}

	done, reason, err := o.orderedQueueIssueDone(s, 42)
	if err != nil {
		t.Fatalf("orderedQueueIssueDone error: %v", err)
	}
	if done {
		t.Fatalf("done = true, want false while outcome is failing")
	}
	if !strings.Contains(reason, "outcome health is not verified") {
		t.Fatalf("reason = %q, want outcome gate reason", reason)
	}
}

func TestOrderedQueueIssueDone_MergedPRAllowedAfterOutcomePass(t *testing.T) {
	cfg := &config.Config{
		Repo: "owner/repo",
		Outcome: outcome.Brief{
			DesiredOutcome:      "Live app works",
			VerifierCommand:     "check-live",
			PassRequiredForDone: boolPtr(true),
		},
	}
	o := &Orchestrator{
		cfg: cfg,
		isIssueClosedFn: func(issueNumber int) (bool, error) {
			return false, nil
		},
		hasMergedPRForIssueFn: func(issueNumber int) (bool, error) {
			return true, nil
		},
	}
	mergedAt := time.Now().UTC().Add(-time.Minute)
	s := state.NewState()
	s.LastMergeAt = mergedAt
	s.OutcomeHealth = &outcome.HealthCheckResult{
		CheckedAt: time.Now().UTC(),
		State:     outcome.HealthHealthy,
		Signal:    "healthcheck_command",
		Summary:   "live verifier passed",
	}

	done, reason, err := o.orderedQueueIssueDone(s, 42)
	if err != nil {
		t.Fatalf("orderedQueueIssueDone error: %v", err)
	}
	if !done {
		t.Fatalf("done = false, want true after outcome pass")
	}
	if reason != "linked PR merged" {
		t.Fatalf("reason = %q, want linked PR merged", reason)
	}
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

func TestAutoMergePRs_AggregatesGreptileAndSimplicityStreams(t *testing.T) {
	prs := []github.PR{{Number: 10, HeadRefName: "feat/a"}}
	cfg := &config.Config{
		Repo:              "owner/repo",
		MergeStrategy:     "parallel",
		ReviewGate:        "greptile",
		ReviewGateStreams: []string{"greptile", "simplicity"},
	}
	o, merged := newMergeTestOrchestrator(cfg, prs)
	calledStreams := []string{}
	o.ghPRReviewGateVerdictFn = func(prNumber int, streams []string) (github.ReviewGateVerdict, error) {
		calledStreams = append(calledStreams, streams...)
		return github.ReviewGateVerdict{
			Passed: false,
			Streams: []github.ReviewStreamVerdict{
				{Name: "greptile", Passed: true},
				{Name: "simplicity", Passed: false, Findings: []github.ReviewComment{{
					Path: "internal/foo.go",
					Line: 7,
					Body: "blocking: over-engineered for one caller",
					User: "maestro-simplicity-reviewer",
				}}},
			},
		}, nil
	}
	s := makeTestState(prs)

	o.autoMergePRs(s)

	if len(*merged) != 0 {
		t.Fatalf("merged = %v, want no merge while simplicity stream has blocking findings", *merged)
	}
	if !reflect.DeepEqual(calledStreams, []string{"greptile", "simplicity"}) {
		t.Fatalf("review streams = %#v, want [greptile simplicity]", calledStreams)
	}

	o.ghPRReviewGateVerdictFn = func(prNumber int, streams []string) (github.ReviewGateVerdict, error) {
		return github.ReviewGateVerdict{
			Passed: true,
			Streams: []github.ReviewStreamVerdict{
				{Name: "greptile", Passed: true},
				{Name: "simplicity", Passed: true},
			},
		}, nil
	}
	o.autoMergePRs(s)
	if len(*merged) != 1 || (*merged)[0] != 10 {
		t.Fatalf("merged = %v, want PR #10 after both review streams pass", *merged)
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

	// All sessions should be marked code_landed
	for slotName, sess := range s.Sessions {
		if sess.Status != state.StatusCodeLanded {
			t.Errorf("session %s status = %q, want %q", slotName, sess.Status, state.StatusCodeLanded)
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

	// Verify state: sessions for PR 10 and 30 should be code_landed, PR 20 should still be pr_open
	codeLandedCount := 0
	openCount := 0
	for _, sess := range s.Sessions {
		if sess.Status == state.StatusCodeLanded {
			codeLandedCount++
		}
		if sess.Status == state.StatusPROpen {
			openCount++
		}
	}
	if codeLandedCount != 2 {
		t.Errorf("expected 2 code_landed sessions, got %d", codeLandedCount)
	}
	if openCount != 1 {
		t.Errorf("expected 1 still-open session, got %d", openCount)
	}
}

func TestAutoMergePRs_ParallelStateConsistency(t *testing.T) {
	// Verify that after parallel merges, the state is consistent:
	// - All merged sessions are StatusCodeLanded with FinishedAt set
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
		if sess.Status != state.StatusCodeLanded {
			t.Errorf("session %s: status = %q, want %q", slotName, sess.Status, state.StatusCodeLanded)
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

func TestAutoMergePRs_SequentialSkipsOlderConflictAndMergesCleanPR(t *testing.T) {
	prs := []github.PR{
		{Number: 10, HeadRefName: "feat/conflicting"},
		{Number: 20, HeadRefName: "feat/clean"},
	}

	cfg := &config.Config{Repo: "owner/repo", MergeStrategy: "sequential"}
	o, merged := newMergeTestOrchestrator(cfg, prs)
	o.ghPRMergeStatusFn = func(prNumber int) (string, string, error) {
		if prNumber == 10 {
			return "CONFLICTING", "dirty", nil
		}
		return "MERGEABLE", "clean", nil
	}
	s := makeTestState(prs)

	o.autoMergePRs(s)

	if !reflect.DeepEqual(*merged, []int{20}) {
		t.Fatalf("merged = %v, want clean PR #20; older conflicting PR must not consume the sequential merge slot", *merged)
	}
	if got := s.Sessions["slot-0"].Status; got != state.StatusPROpen {
		t.Fatalf("conflicting canonical session status = %q, want pr_open for in-place repair", got)
	}
}

func TestAutoMergePRs_PassedReviewGateDoesNotRetryAdvisoryFeedback(t *testing.T) {
	prs := []github.PR{{Number: 10, HeadRefName: "feat/a"}}
	cfg := &config.Config{
		Repo:                    "owner/repo",
		MergeStrategy:           "sequential",
		ReviewGate:              "greptile",
		AutoRetryReviewFeedback: true,
	}
	o, merged := newMergeTestOrchestrator(cfg, prs)
	o.ghCollectPRReviewFeedbackFn = func(int) (string, error) {
		return "P1 advisory finding on a head Greptile has approved", nil
	}
	s := makeTestState(prs)

	o.autoMergePRs(s)

	if !reflect.DeepEqual(*merged, []int{10}) {
		t.Fatalf("merged = %v, want approved PR #10; successful gate is authoritative", *merged)
	}
	if got := s.Sessions["slot-0"].MaintenanceRetryCount; got != 0 {
		t.Fatalf("maintenance retries = %d, want 0 after successful review gate", got)
	}
}

func TestAutoMergePRs_GreptileThreeOfFiveEntersBoundedRepairWithFindings(t *testing.T) {
	prs := []github.PR{{Number: 10, HeadRefName: "feat/a", Mergeable: "MERGEABLE"}}
	cfg := &config.Config{
		Repo:                    "owner/repo",
		MergeStrategy:           "parallel",
		ReviewGate:              "greptile",
		ReviewGateStreams:       []string{"greptile"},
		AutoRetryReviewFeedback: true,
		MaxRetriesPerIssue:      3,
		MaxRetryBackoffMs:       300000,
	}
	o, merged := newMergeTestOrchestrator(cfg, prs)
	head := strings.Repeat("a", 40)
	o.ghPRCheckRollupFn = func(int) (github.PRCheckRollup, error) {
		return github.PRCheckRollup{HeadSHA: head, Verdict: "success", Fingerprint: strings.Repeat("1", 16), Complete: true}, nil
	}
	o.ghPRHeadSHAFn = func(int) (string, error) { return head, nil }
	o.ghPRReviewGateVerdictFn = func(int, []string) (github.ReviewGateVerdict, error) {
		return github.ReviewGateVerdict{
			Passed: false,
			Streams: []github.ReviewStreamVerdict{{
				Name: "greptile", Score: 3, ScoreMax: 5, Verdict: "repair_required",
				Findings: []github.ReviewComment{{Path: "internal/foo.go", Line: 42, Body: "P1: nil pointer panic", User: "greptile-apps[bot]"}},
			}},
		}, nil
	}
	o.ghCollectPRReviewFeedbackFn = func(int) (string, error) {
		return "## Review Feedback\n\ninternal/foo.go:42 P1: nil pointer panic", nil
	}
	st := makeTestState(prs)
	st.Sessions["slot-0"].Worktree = "/tmp/maestro-slot-0"

	o.autoMergePRs(st)

	if len(*merged) != 0 {
		t.Fatalf("Greptile 3/5 must not merge: %v", *merged)
	}
	sess := st.Sessions["slot-0"]
	if sess.Status != state.StatusDead || sess.MaintenanceRetryCount != 1 || sess.NextRetryAt == nil {
		t.Fatalf("bounded review repair not scheduled: status=%q maintenance=%d next=%v", sess.Status, sess.MaintenanceRetryCount, sess.NextRetryAt)
	}
	if !strings.Contains(sess.PreviousAttemptFeedback, "internal/foo.go:42") {
		t.Fatalf("repair context lost concrete finding: %q", sess.PreviousAttemptFeedback)
	}
	snapshot := mustLatestPRGateSnapshot(t, st, sess.IssueNumber, 10)
	if snapshot.ReviewDecision != state.PRGateReviewBlocked || len(snapshot.ReviewStreams) != 1 || snapshot.ReviewStreams[0].Score != 3 {
		t.Fatalf("Greptile 3/5 fact not persisted with blocked gate: %+v", snapshot)
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

func TestAutoMergePRs_ReviewGateNoneSkipsGreptileWait(t *testing.T) {
	prs := []github.PR{{Number: 10, HeadRefName: "feat/a"}}
	cfg := &config.Config{Repo: "owner/repo", MergeStrategy: "parallel", ReviewGate: "none"}
	merged := make([]int, 0)
	greptileChecks := 0
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
			greptileChecks++
			return false, true, nil
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

	if greptileChecks != 0 {
		t.Fatalf("greptile gate should not be checked when review_gate=none, got %d checks", greptileChecks)
	}
	if len(merged) != 1 || merged[0] != 10 {
		t.Fatalf("merged = %v, want [10]", merged)
	}
}

func TestAutoMergePRs_ReviewFeedbackKeepsPROpenAndSchedulesInPlaceRetry(t *testing.T) {
	prs := []github.PR{{Number: 10, HeadRefName: "feat/a"}}
	cfg := &config.Config{
		Repo:                    "owner/repo",
		MergeStrategy:           "parallel",
		ReviewGate:              "none",
		AutoRetryReviewFeedback: true,
		MaxRetriesPerIssue:      3,
		MaxRetryBackoffMs:       300000,
	}
	merged := make([]int, 0)
	closedPRs := make([]int, 0)
	o := &Orchestrator{
		cfg:      cfg,
		notifier: &notify.Notifier{},
		listOpenPRsFn: func() ([]github.PR, error) {
			return prs, nil
		},
		ghPRCIStatusFn: func(prNumber int) (string, error) {
			return "success", nil
		},
		ghCollectPRReviewFeedbackFn: func(prNumber int) (string, error) {
			return "docs/ROADMAP.md:34 remove false cost-budget claim", nil
		},
		ghClosePRFn: func(prNumber int, comment string) error {
			closedPRs = append(closedPRs, prNumber)
			return nil
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
	s.Sessions["slot-0"].Worktree = "/tmp/maestro-slot-0"

	o.autoMergePRs(s)

	if len(merged) != 0 {
		t.Fatalf("expected review feedback to block merge, got merged=%v", merged)
	}
	if len(closedPRs) != 0 {
		t.Fatalf("closedPRs = %v, want none", closedPRs)
	}
	sess := s.Sessions["slot-0"]
	if sess.Status != state.StatusDead {
		t.Fatalf("status = %q, want %q", sess.Status, state.StatusDead)
	}
	if sess.NextRetryAt == nil {
		t.Fatal("NextRetryAt should be set")
	}
	if sess.PreviousAttemptFeedback == "" {
		t.Fatal("PreviousAttemptFeedback should be set")
	}
	if sess.PRNumber != 10 {
		t.Fatalf("PRNumber = %d, want 10", sess.PRNumber)
	}
	if sess.Worktree == "" {
		t.Fatal("Worktree should be preserved for in-place retry")
	}
}

func TestAutoMergePRs_ReviewFeedbackFallsBackToCloseWhenWorktreeMissing(t *testing.T) {
	prs := []github.PR{{Number: 10, HeadRefName: "feat/a"}}
	cfg := &config.Config{
		Repo:                    "owner/repo",
		MergeStrategy:           "parallel",
		ReviewGate:              "none",
		AutoRetryReviewFeedback: true,
		MaxRetriesPerIssue:      3,
		MaxRetryBackoffMs:       300000,
	}
	closedPRs := make([]int, 0)
	o := &Orchestrator{
		cfg:      cfg,
		notifier: &notify.Notifier{},
		listOpenPRsFn: func() ([]github.PR, error) {
			return prs, nil
		},
		ghPRCIStatusFn: func(prNumber int) (string, error) {
			return "success", nil
		},
		ghCollectPRReviewFeedbackFn: func(prNumber int) (string, error) {
			return "docs/ROADMAP.md:34 remove false cost-budget claim", nil
		},
		ghClosePRFn: func(prNumber int, comment string) error {
			closedPRs = append(closedPRs, prNumber)
			return nil
		},
	}
	s := makeTestState(prs)

	o.autoMergePRs(s)

	if len(closedPRs) != 1 || closedPRs[0] != 10 {
		t.Fatalf("closedPRs = %v, want [10]", closedPRs)
	}
	if s.Sessions["slot-0"].PRNumber != 0 {
		t.Fatalf("PRNumber = %d, want 0 after close fallback", s.Sessions["slot-0"].PRNumber)
	}
}

func TestAutoMergePRs_ReviewFeedbackImplementationRetryLimitStillSchedulesMaintenance(t *testing.T) {
	prs := []github.PR{{Number: 10, HeadRefName: "feat/a"}}
	cfg := &config.Config{
		Repo:                    "owner/repo",
		MergeStrategy:           "parallel",
		ReviewGate:              "none",
		AutoRetryReviewFeedback: true,
		MaxRetriesPerIssue:      3,
		MaxRetryBackoffMs:       300000,
	}
	notifier := notify.NewWithToken("", "123", "", "")
	notifier.SetDigestMode(true)
	o := &Orchestrator{
		cfg:      cfg,
		notifier: notifier,
		listOpenPRsFn: func() ([]github.PR, error) {
			return prs, nil
		},
		ghPRCIStatusFn: func(prNumber int) (string, error) {
			return "success", nil
		},
		ghCollectPRReviewFeedbackFn: func(prNumber int) (string, error) {
			return "docs/ROADMAP.md:34 remove false cost-budget claim", nil
		},
	}
	s := makeTestState(prs)
	sess := s.Sessions["slot-0"]
	sess.Worktree = "/tmp/maestro-slot-0"
	sess.RetryCount = 3

	o.autoMergePRs(s)

	if sess.Status != state.StatusDead {
		t.Fatalf("status = %q, want %q", sess.Status, state.StatusDead)
	}
	if sess.NextRetryAt == nil {
		t.Fatal("NextRetryAt should be set for maintenance retry")
	}
	if sess.RetryCount != 3 {
		t.Fatalf("RetryCount = %d, want 3 (implementation retry budget must not be consumed)", sess.RetryCount)
	}
	if sess.MaintenanceRetryCount != 1 {
		t.Fatalf("MaintenanceRetryCount = %d, want 1", sess.MaintenanceRetryCount)
	}
	if sess.PreviousAttemptFeedbackKind != state.RetryReasonReviewFeedback {
		t.Fatalf("PreviousAttemptFeedbackKind = %q, want review_feedback", sess.PreviousAttemptFeedbackKind)
	}
}

func TestAutoMergePRs_ReviewFeedbackMaintenanceBudgetMarksTerminal(t *testing.T) {
	prs := []github.PR{{Number: 10, HeadRefName: "feat/a"}}
	cfg := &config.Config{
		Repo:                    "owner/repo",
		MergeStrategy:           "parallel",
		ReviewGate:              "none",
		AutoRetryReviewFeedback: true,
		MaxRetriesPerIssue:      3,
		MaxRetryBackoffMs:       300000,
	}
	notifier := notify.NewWithToken("", "123", "", "")
	notifier.SetDigestMode(true)
	o := &Orchestrator{
		cfg:      cfg,
		notifier: notifier,
		listOpenPRsFn: func() ([]github.PR, error) {
			return prs, nil
		},
		ghPRCIStatusFn: func(prNumber int) (string, error) {
			return "success", nil
		},
		ghCollectPRReviewFeedbackFn: func(prNumber int) (string, error) {
			return "docs/ROADMAP.md:34 remove false cost-budget claim", nil
		},
	}
	s := makeTestState(prs)
	sess := s.Sessions["slot-0"]
	sess.Worktree = "/tmp/maestro-slot-0"
	sess.RetryCount = 3
	sess.MaintenanceRetryCount = 1

	o.autoMergePRs(s)

	if sess.Status != state.StatusRetryExhausted {
		t.Fatalf("status = %q, want %q", sess.Status, state.StatusRetryExhausted)
	}
	if sess.NextRetryAt != nil {
		t.Fatal("NextRetryAt should be nil after maintenance exhaustion")
	}
	if sess.LastNotifiedStatus != "review_retry_exhausted" {
		t.Fatalf("LastNotifiedStatus = %q, want review_retry_exhausted", sess.LastNotifiedStatus)
	}
	if notifier.Buffered() != 1 {
		t.Fatalf("notifications buffered = %d, want 1", notifier.Buffered())
	}

	o.autoMergePRs(s)

	if notifier.Buffered() != 1 {
		t.Fatalf("duplicate terminal notification buffered; got %d, want 1", notifier.Buffered())
	}
}

func TestAutoMergePRs_RetryExhaustedGreenPRNoFeedbackMerges(t *testing.T) {
	prs := []github.PR{{Number: 10, HeadRefName: "feat/a", Mergeable: "MERGEABLE"}}
	cfg := &config.Config{
		Repo:                    "owner/repo",
		MergeStrategy:           "parallel",
		ReviewGate:              "none",
		AutoRetryReviewFeedback: true,
		MaxRetriesPerIssue:      3,
		MaxRetryBackoffMs:       300000,
	}
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
		ghCollectPRReviewFeedbackFn: func(prNumber int) (string, error) {
			return "", nil
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
	s := state.NewState()
	s.Sessions["slot-0"] = &state.Session{
		IssueNumber:        100,
		IssueTitle:         "green after retry exhaustion",
		Branch:             "feat/a",
		Status:             state.StatusRetryExhausted,
		PRNumber:           10,
		RetryCount:         3,
		LastNotifiedStatus: "review_retry_exhausted",
	}

	o.autoMergePRs(s)

	if len(merged) != 1 || merged[0] != 10 {
		t.Fatalf("merged = %v, want [10]", merged)
	}
	sess := s.Sessions["slot-0"]
	if sess.Status != state.StatusCodeLanded {
		t.Fatalf("status = %q, want %q", sess.Status, state.StatusCodeLanded)
	}
}

func TestAutoMergePRs_RetryExhaustedActionableFeedbackGetsMaintenancePass(t *testing.T) {
	prs := []github.PR{{Number: 10, HeadRefName: "feat/a", Mergeable: "MERGEABLE"}}
	cfg := &config.Config{
		Repo:                    "owner/repo",
		MergeStrategy:           "parallel",
		ReviewGate:              "none",
		AutoRetryReviewFeedback: true,
		MaxRetriesPerIssue:      3,
		MaxRetryBackoffMs:       300000,
	}
	merged := make([]int, 0)
	notifier := notify.NewWithToken("", "123", "", "")
	notifier.SetDigestMode(true)
	o := &Orchestrator{
		cfg:      cfg,
		notifier: notifier,
		listOpenPRsFn: func() ([]github.PR, error) {
			return prs, nil
		},
		ghPRCIStatusFn: func(prNumber int) (string, error) {
			return "success", nil
		},
		ghCollectPRReviewFeedbackFn: func(prNumber int) (string, error) {
			return "## Review Feedback\n\ninternal/foo.go:42 P1: nil pointer panic", nil
		},
		ghMergePRFn: func(prNumber int) error {
			merged = append(merged, prNumber)
			return nil
		},
	}
	s := state.NewState()
	s.Sessions["slot-0"] = &state.Session{
		IssueNumber: 100,
		IssueTitle:  "green with current-head comments",
		Branch:      "feat/a",
		Worktree:    "/tmp/maestro-slot-0",
		Status:      state.StatusRetryExhausted,
		PRNumber:    10,
		RetryCount:  3,
	}

	o.autoMergePRs(s)

	if len(merged) != 0 {
		t.Fatalf("actionable review feedback should block merge, got merged=%v", merged)
	}
	sess := s.Sessions["slot-0"]
	if sess.Status != state.StatusDead {
		t.Fatalf("status = %q, want %q", sess.Status, state.StatusDead)
	}
	if sess.RetryCount != 3 {
		t.Fatalf("RetryCount = %d, want 3 (implementation retry budget must not be consumed)", sess.RetryCount)
	}
	if sess.MaintenanceRetryCount != 1 {
		t.Fatalf("MaintenanceRetryCount = %d, want 1", sess.MaintenanceRetryCount)
	}
	if sess.NextRetryAt == nil {
		t.Fatal("NextRetryAt should be set for maintenance pass")
	}
	if notifier.Buffered() != 1 {
		t.Fatalf("notifications buffered = %d, want 1", notifier.Buffered())
	}

	o.autoMergePRs(s)

	if len(merged) != 0 {
		t.Fatalf("actionable review feedback should still block merge, got merged=%v", merged)
	}
	if notifier.Buffered() != 1 {
		t.Fatalf("duplicate terminal notification buffered; got %d, want 1", notifier.Buffered())
	}
}

// #556: once a session is settled in retry_exhausted for a specific PR
// with unaddressed review feedback, subsequent autoMergePRs cycles must
// NOT re-emit "scheduling retry", re-sync the project board, or churn
// FinishedAt. Live dogfood (2026-06-01, issue #535 / PR #555) showed the
// pre-fix loop syncing Blocked → InReview → Blocked every poll.
func TestAutoMergePRs_RetryExhaustedFeedback_NoReSyncAfterSettled(t *testing.T) {
	prs := []github.PR{{Number: 555, HeadRefName: "feat/sup-92", Mergeable: "MERGEABLE"}}
	cfg := &config.Config{
		Repo:                    "owner/repo",
		MergeStrategy:           "parallel",
		ReviewGate:              "none",
		AutoRetryReviewFeedback: true,
		MaxRetriesPerIssue:      2,
		MaxRetryBackoffMs:       300000,
	}
	cfg.GitHubProjects.Enabled = true
	cfg.GitHubProjects.ProjectNumber = 4
	notifier := notify.NewWithToken("", "123", "", "")
	notifier.SetDigestMode(true)
	syncs := make([]string, 0)
	o := &Orchestrator{
		cfg:      cfg,
		notifier: notifier,
		listOpenPRsFn: func() ([]github.PR, error) {
			return prs, nil
		},
		ghPRCIStatusFn: func(prNumber int) (string, error) {
			return "success", nil
		},
		ghCollectPRReviewFeedbackFn: func(prNumber int) (string, error) {
			return "## Review Feedback\n\ninternal/foo.go:42 P1: unaddressed nil pointer", nil
		},
		rateLimitFn: func() (github.RateLimitStatus, error) {
			return github.RateLimitStatus{GraphQL: github.RateLimitBucket{Limit: 5000, Remaining: 5000}}, nil
		},
		syncProjectFn: func(issueNumber int, status github.ProjectStatus) bool {
			syncs = append(syncs, fmt.Sprintf("#%d:%s", issueNumber, status))
			return true
		},
	}
	s := state.NewState()
	s.Sessions["sup-92"] = &state.Session{
		IssueNumber:           535,
		IssueTitle:            "review feedback PR",
		Branch:                "feat/sup-92",
		Status:                state.StatusRetryExhausted,
		PRNumber:              555,
		RetryCount:            2,
		MaintenanceRetryCount: 1,
	}

	// First cycle marks the session retry-exhausted (one project sync).
	o.autoMergePRs(s)

	sess := s.Sessions["sup-92"]
	if sess.Status != state.StatusRetryExhausted {
		t.Fatalf("status = %q, want %q", sess.Status, state.StatusRetryExhausted)
	}
	if sess.LastNotifiedStatus != "review_retry_exhausted" {
		t.Fatalf("LastNotifiedStatus = %q, want review_retry_exhausted", sess.LastNotifiedStatus)
	}
	if len(syncs) != 1 || syncs[0] != "#535:blocked" {
		t.Fatalf("syncs after first cycle = %v, want [#535:blocked]", syncs)
	}
	settledFinishedAt := sess.FinishedAt
	if settledFinishedAt == nil {
		t.Fatalf("FinishedAt must be set after first cycle")
	}

	// Second cycle: same conditions. No additional project sync, no extra
	// notification, FinishedAt does not churn.
	o.autoMergePRs(s)

	if len(syncs) != 1 {
		t.Fatalf("project syncs after second cycle = %d (%v), want 1 (idempotent)", len(syncs), syncs)
	}
	if notifier.Buffered() != 1 {
		t.Fatalf("notifications buffered = %d, want 1 (no duplicate terminal notification)", notifier.Buffered())
	}
	if sess.FinishedAt != settledFinishedAt {
		t.Fatalf("FinishedAt churned across cycles: was %v, now %v", settledFinishedAt, sess.FinishedAt)
	}

	// Third cycle to be sure the loop has truly stabilised.
	o.autoMergePRs(s)
	if len(syncs) != 1 {
		t.Fatalf("project syncs after third cycle = %d (%v), want 1", len(syncs), syncs)
	}
}

// #556: checkSessions must not flip a settled retry_exhausted session
// back to pr_open every cycle. Doing so syncs the project to InReview,
// which in combination with autoMergePRs' Blocked sync produces the
// observed In Review ↔ Blocked flip-flop.
func TestCheckSessions_SettledRetryExhausted_NoFlipFlop(t *testing.T) {
	cfg := &config.Config{Repo: "owner/repo", MaxRuntimeMinutes: 999}
	cfg.GitHubProjects.Enabled = true
	cfg.GitHubProjects.ProjectNumber = 4
	synced := make([]string, 0)
	o, _ := newCheckSessionsOrchestrator(cfg, "")
	o.listOpenPRsFn = func() ([]github.PR, error) {
		return []github.PR{{
			Number:      555,
			HeadRefName: "feat/sup-92",
			State:       "OPEN",
		}}, nil
	}
	o.rateLimitFn = func() (github.RateLimitStatus, error) {
		return github.RateLimitStatus{GraphQL: github.RateLimitBucket{Limit: 5000, Remaining: 5000}}, nil
	}
	o.syncProjectFn = func(issueNumber int, status github.ProjectStatus) bool {
		synced = append(synced, fmt.Sprintf("#%d:%s", issueNumber, status))
		return true
	}

	s := state.NewState()
	finishedAt := time.Now().UTC().Add(-5 * time.Minute)
	s.Sessions["sup-92"] = &state.Session{
		IssueNumber:        535,
		IssueTitle:         "review feedback PR",
		Status:             state.StatusRetryExhausted,
		Branch:             "feat/sup-92",
		PRNumber:           555,
		RetryCount:         2,
		LastNotifiedStatus: "review_retry_exhausted",
		FinishedAt:         &finishedAt,
		StartedAt:          time.Now().UTC().Add(-10 * time.Minute),
	}

	o.checkSessions(s)

	sess := s.Sessions["sup-92"]
	if sess.Status != state.StatusRetryExhausted {
		t.Fatalf("status = %q, want %q (settled session must NOT be flipped to pr_open)", sess.Status, state.StatusRetryExhausted)
	}
	if sess.PRNumber != 555 {
		t.Fatalf("PRNumber = %d, want 555", sess.PRNumber)
	}
	if len(synced) != 0 {
		t.Fatalf("syncProject calls = %v, want none (settled state must not re-sync InReview)", synced)
	}
	if sess.FinishedAt == nil || !sess.FinishedAt.Equal(finishedAt) {
		t.Fatalf("FinishedAt churned: was %v, now %v", finishedAt, sess.FinishedAt)
	}

	// Second cycle: still settled. Still no flip, no sync.
	o.checkSessions(s)
	if sess.Status != state.StatusRetryExhausted {
		t.Fatalf("status after second cycle = %q, want %q", sess.Status, state.StatusRetryExhausted)
	}
	if len(synced) != 0 {
		t.Fatalf("syncProject calls after second cycle = %v, want none", synced)
	}
}

// #424: when the aggregate PRCIStatus sticks at "pending" (e.g. a legacy
// commit-status that never resolves) but GitHub's per-PR mergeable_state
// is "clean" (every required check has gone green) the orchestrator must
// trust mergeable_state and merge the PR instead of looping on CI=pending.
func TestAutoMergePRs_CIAggregateStaleButMergeStateClean_Merges(t *testing.T) {
	prs := []github.PR{{Number: 99, HeadRefName: "feat/auth"}}

	cfg := &config.Config{Repo: "owner/repo", MergeStrategy: "parallel", ReviewGate: "none"}
	merged := make([]int, 0)
	o := &Orchestrator{
		cfg:      cfg,
		notifier: &notify.Notifier{},
		listOpenPRsFn: func() ([]github.PR, error) {
			return prs, nil
		},
		ghPRCIStatusFn: func(prNumber int) (string, error) {
			return "pending", nil
		},
		ghPRMergeStatusFn: func(prNumber int) (string, string, error) {
			return "MERGEABLE", "clean", nil
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

	if len(merged) != 1 || merged[0] != 99 {
		t.Fatalf("merged = %v, want [99] (mergeable_state=clean must override stale aggregate CI=pending)", merged)
	}
}

// mergeable_state="blocked" means a required check is still failing; the
// orchestrator must NOT override CI=pending in that case.
func TestAutoMergePRs_CIPendingAndMergeStateBlocked_DoesNotMerge(t *testing.T) {
	prs := []github.PR{{Number: 101, HeadRefName: "feat/blocked"}}

	cfg := &config.Config{Repo: "owner/repo", MergeStrategy: "parallel", ReviewGate: "none"}
	merged := make([]int, 0)
	o := &Orchestrator{
		cfg:      cfg,
		notifier: &notify.Notifier{},
		listOpenPRsFn: func() ([]github.PR, error) {
			return prs, nil
		},
		ghPRCIStatusFn: func(prNumber int) (string, error) {
			return "pending", nil
		},
		ghPRMergeStatusFn: func(prNumber int) (string, string, error) {
			return "MERGEABLE", "blocked", nil
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

	if len(merged) != 0 {
		t.Fatalf("merged = %v, want [] (mergeable_state=blocked must keep CI=pending blocking)", merged)
	}
}

// mergeable_state="unstable" can coexist with failed check runs. It is not
// authoritative green evidence and must never override the check rollup.
func TestAutoMergePRs_CIPendingMergeStateUnstable_DoesNotMerge(t *testing.T) {
	prs := []github.PR{{Number: 102, HeadRefName: "feat/unstable"}}

	cfg := &config.Config{Repo: "owner/repo", MergeStrategy: "parallel", ReviewGate: "none"}
	merged := make([]int, 0)
	o := &Orchestrator{
		cfg:      cfg,
		notifier: &notify.Notifier{},
		listOpenPRsFn: func() ([]github.PR, error) {
			return prs, nil
		},
		ghPRCIStatusFn: func(prNumber int) (string, error) {
			return "pending", nil
		},
		ghPRMergeStatusFn: func(prNumber int) (string, string, error) {
			return "MERGEABLE", "unstable", nil
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

	if len(merged) != 0 {
		t.Fatalf("merged = %v, want [] (mergeable_state=unstable must not override pending/failed checks)", merged)
	}
}

func TestAutoMergePRs_CIFailureBlocksMerge(t *testing.T) {
	prs := []github.PR{
		{Number: 10, HeadRefName: "feat/a"},
		{Number: 20, HeadRefName: "feat/b"},
	}

	cfg := &config.Config{Repo: "owner/repo", MergeStrategy: "parallel", MaxRetriesPerIssue: 3, MaxRetryBackoffMs: 300000}
	merged := make([]int, 0)
	closedPRs := make([]int, 0)
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
		ghClosePRFn: func(prNumber int, comment string) error {
			closedPRs = append(closedPRs, prNumber)
			return nil
		},
		ghPRChecksOutputFn: func(prNumber int) (string, error) {
			return "Build failed: exit code 1", nil
		},
		ghCollectPRReviewFeedbackFn: func(prNumber int) (string, error) {
			return "", nil
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

	// PR #10 remains canonical while its same-session repair is scheduled.
	if len(closedPRs) != 0 {
		t.Errorf("closedPRs = %v, want none", closedPRs)
	}
	for _, sess := range s.Sessions {
		if sess.PRNumber == 10 && sess.IssueNumber == 100 {
			// This is the session for PR #10 (identity retained for in-place retry).
			if sess.Status != state.StatusDead {
				t.Errorf("CI-failed session status = %q, want %q", sess.Status, state.StatusDead)
			}
			if sess.NextRetryAt == nil {
				t.Error("CI-failed session should have NextRetryAt set")
			}
			if sess.CIFailureOutput != "Build failed: exit code 1" {
				t.Errorf("CIFailureOutput = %q, want %q", sess.CIFailureOutput, "Build failed: exit code 1")
			}
		}
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

func TestCheckSessions_TerminalSessionWithOpenBranchPRTransitionsToPROpen(t *testing.T) {
	cfg := &config.Config{Repo: "owner/repo", MaxRuntimeMinutes: 999}
	cfg.GitHubProjects.Enabled = true
	cfg.GitHubProjects.ProjectNumber = 4
	synced := make([]string, 0)
	o, _ := newCheckSessionsOrchestrator(cfg, "")
	o.listOpenPRsFn = func() ([]github.PR, error) {
		return []github.PR{{
			Number:      832,
			HeadRefName: "feat/pan-74-808-remove-or-justify-duplicate-settings-ent",
			State:       "OPEN",
		}}, nil
	}
	o.rateLimitFn = func() (github.RateLimitStatus, error) {
		return github.RateLimitStatus{GraphQL: github.RateLimitBucket{Limit: 5000, Remaining: 5000}}, nil
	}
	o.syncProjectFn = func(issueNumber int, status github.ProjectStatus) bool {
		synced = append(synced, fmt.Sprintf("#%d:%s", issueNumber, status))
		return true
	}

	s := state.NewState()
	s.Sessions["pan-74"] = &state.Session{
		IssueNumber: 808,
		IssueTitle:  "dedupe settings",
		Status:      state.StatusDead,
		Branch:      "feat/pan-74-808-remove-or-justify-duplicate-settings-ent",
		StartedAt:   time.Now().UTC().Add(-10 * time.Minute),
	}

	o.checkSessions(s)

	sess := s.Sessions["pan-74"]
	if sess.Status != state.StatusPROpen {
		t.Fatalf("status = %q, want %q", sess.Status, state.StatusPROpen)
	}
	if sess.PRNumber != 832 {
		t.Fatalf("PRNumber = %d, want 832", sess.PRNumber)
	}
	if len(synced) != 1 || synced[0] != "#808:in_review" {
		t.Fatalf("synced = %v, want #808:in_review", synced)
	}
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
	if sess.Status != state.StatusFailed {
		t.Fatalf("status = %q, want %q", sess.Status, state.StatusFailed)
	}
	if sess.LastNotifiedStatus != worker.TokenBudgetExceededOutcome || sess.WorkerOutcome != worker.TokenBudgetExceededOutcome {
		t.Fatalf("budget outcome = %q/%q, want %q", sess.LastNotifiedStatus, sess.WorkerOutcome, worker.TokenBudgetExceededOutcome)
	}
	if sess.TokensUsedAttempt != 75000 {
		t.Fatalf("tokens_used = %d, want 75000", sess.TokensUsedAttempt)
	}
	if sess.TokensUsedTotal != 75000 {
		t.Fatalf("tokens_used_total = %d, want 75000", sess.TokensUsedTotal)
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
	if sess.TokensUsedAttempt != 50000 {
		t.Fatalf("tokens_used = %d, want 50000", sess.TokensUsedAttempt)
	}
	if sess.TokensUsedTotal != 50000 {
		t.Fatalf("tokens_used_total = %d, want 50000", sess.TokensUsedTotal)
	}
	if len(*stopped) != 0 {
		t.Fatalf("stopped = %v, want empty", *stopped)
	}
}

func TestCheckSessions_CacheReadExcludedFromClaudeAndPiBudgets(t *testing.T) {
	softThreshold := 0.8
	tests := []struct {
		name       string
		backend    string
		backendDef config.BackendDef
		stream     string
		writeJSONL bool
	}{
		{
			name:       "claude",
			backend:    "claude",
			backendDef: config.BackendDef{Cmd: "claude", Provider: "anthropic", UsageStream: true},
			// fin-26 shape: almost all observed provider tokens are cache
			// replay on the second assistant turn. Inclusive telemetry is
			// 200,506, while the uncached budget measure is only 77,030.
			stream:     claudeResultFrame(10, 30, 76_990, 123_476, 0, "working"),
			writeJSONL: true,
		},
		{
			name:       "pi",
			backend:    "pi",
			backendDef: config.BackendDef{Cmd: "pi", Provider: "pi"},
			stream:     `{"type":"turn_end","message":{"role":"assistant","provider":"anthropic","model":"claude-opus","usage":{"input":10,"output":30,"cacheRead":123476,"cacheWrite":76990,"totalTokens":200506,"cost":{"total":0}}}}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			cfg := &config.Config{
				Repo:                     "owner/repo",
				StateDir:                 dir,
				WorkerMaxTokens:          160_000,
				WorkerSoftTokenThreshold: &softThreshold,
				MaxRuntimeMinutes:        999,
				Model: config.ModelConfig{
					Default:  tt.backend,
					Backends: map[string]config.BackendDef{tt.backend: tt.backendDef},
				},
			}
			tmuxOutput := tt.stream
			if tt.writeJSONL {
				tmuxOutput = "working"
			}
			o, stopped := newCheckSessionsOrchestrator(cfg, tmuxOutput)
			checkpointed := 0
			o.saveCheckpointFn = func(*state.Session) (string, error) {
				checkpointed++
				return "/tmp/CHECKPOINT.md", nil
			}

			logFile := filepath.Join(dir, "slot.log")
			if tt.writeJSONL {
				if err := os.WriteFile(worker.JSONLPathForLog(logFile), []byte(tt.stream), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			s := state.NewState()
			s.Sessions["slot"] = &state.Session{
				IssueNumber: 64,
				Status:      state.StatusRunning,
				PID:         1234,
				TmuxSession: "maestro-slot",
				Branch:      "feat/slot-64-budget",
				Backend:     tt.backend,
				LogFile:     logFile,
				StartedAt:   time.Now().Add(-time.Minute),
			}

			o.checkSessions(s)

			sess := s.Sessions["slot"]
			if sess.Status != state.StatusRunning {
				t.Fatalf("status = %q, want running; inclusive cache telemetry must not kill the worker", sess.Status)
			}
			if sess.TokensUsedAttempt != 200_506 || sess.TokensUsedTotal != 200_506 {
				t.Fatalf("inclusive telemetry = %d/%d, want 200506/200506", sess.TokensUsedAttempt, sess.TokensUsedTotal)
			}
			if sess.TokenBudgetTokensAttempt != 77_030 || sess.TokenBudgetMeasure != worker.TokenBudgetMeasureUncached {
				t.Fatalf("budget observation = %d %q, want 77030 %q", sess.TokenBudgetTokensAttempt, sess.TokenBudgetMeasure, worker.TokenBudgetMeasureUncached)
			}
			if checkpointed != 0 || len(*stopped) != 0 {
				t.Fatalf("cache replay triggered checkpoint/stop: checkpointed=%d stopped=%v", checkpointed, *stopped)
			}
		})
	}
}

func TestCheckSessions_RunningInPlaceRetryKeepsWorkerRunning(t *testing.T) {
	cfg := &config.Config{
		Repo:              "owner/repo",
		MaxRuntimeMinutes: 999,
	}
	o := &Orchestrator{
		cfg:      cfg,
		notifier: &notify.Notifier{},
		listOpenPRsFn: func() ([]github.PR, error) {
			return []github.PR{{Number: 10, HeadRefName: "feat/existing"}}, nil
		},
		isIssueClosedFn: func(number int) (bool, error) {
			return false, nil
		},
		pidAliveFn: func(pid int) bool {
			return true
		},
		tmuxCaptureFn: func(session string) (string, error) {
			return "worker still fixing review comments", nil
		},
	}
	s := state.NewState()
	s.Sessions["slot-0"] = &state.Session{
		IssueNumber: 100,
		IssueTitle:  "review retry",
		Status:      state.StatusRunning,
		PID:         1234,
		TmuxSession: "maestro-slot-0",
		Branch:      "feat/existing",
		PRNumber:    10,
		StartedAt:   time.Now().Add(-1 * time.Minute),
	}

	o.checkSessions(s)

	sess := s.Sessions["slot-0"]
	if sess.Status != state.StatusRunning {
		t.Fatalf("status = %q, want %q", sess.Status, state.StatusRunning)
	}
	if sess.PRNumber != 10 {
		t.Fatalf("PRNumber = %d, want 10", sess.PRNumber)
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
	if sess.TokensUsedAttempt != 999999 {
		t.Fatalf("tokens_used = %d, want 999999 (should track even when limit=0)", sess.TokensUsedAttempt)
	}
	if sess.TokensUsedTotal != 999999 {
		t.Fatalf("tokens_used_total = %d, want 999999 (should track even when limit=0)", sess.TokensUsedTotal)
	}
	if len(*stopped) != 0 {
		t.Fatalf("stopped = %v, want empty", *stopped)
	}
}

func TestCheckSessions_LegacyTokenNotificationDoesNotSuppressOutcome(t *testing.T) {
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
		TokensUsedAttempt:  75000,
		LastNotifiedStatus: "token_limit", // already notified
	}

	o.checkSessions(s)

	sess := s.Sessions["mae-4"]
	if sess.Status != state.StatusFailed || sess.WorkerOutcome != worker.TokenBudgetExceededOutcome {
		t.Fatalf("status/outcome = %q/%q, want failed/%q", sess.Status, sess.WorkerOutcome, worker.TokenBudgetExceededOutcome)
	}
	if len(*stopped) != 1 {
		t.Fatalf("stopped = %v, want one deterministic stop", *stopped)
	}
}

func TestCheckSessions_TokensAtExactLimit_WorkerStops(t *testing.T) {
	cfg := &config.Config{
		Repo:              "owner/repo",
		WorkerMaxTokens:   50000,
		MaxRuntimeMinutes: 999,
	}
	// The hard ceiling stops before another provider response can exceed it.
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
	if sess.Status != state.StatusFailed || sess.WorkerOutcome != worker.TokenBudgetExceededOutcome {
		t.Fatalf("status/outcome = %q/%q, want failed/%q", sess.Status, sess.WorkerOutcome, worker.TokenBudgetExceededOutcome)
	}
	if sess.TokensUsedAttempt != 50000 {
		t.Fatalf("tokens_used = %d, want 50000", sess.TokensUsedAttempt)
	}
	if sess.TokensUsedTotal != 50000 {
		t.Fatalf("tokens_used_total = %d, want 50000", sess.TokensUsedTotal)
	}
	if len(*stopped) != 1 {
		t.Fatalf("stopped = %v, want [mae-5]", *stopped)
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
	if sess6.Status != state.StatusFailed {
		t.Fatalf("mae-6 status = %q, want %q", sess6.Status, state.StatusFailed)
	}
	if sess6.TokensUsedAttempt != 75000 {
		t.Fatalf("mae-6 tokens_used = %d, want 75000", sess6.TokensUsedAttempt)
	}
	if sess6.TokensUsedTotal != 75000 {
		t.Fatalf("mae-6 tokens_used_total = %d, want 75000", sess6.TokensUsedTotal)
	}

	sess7 := s.Sessions["mae-7"]
	if sess7.Status != state.StatusRunning {
		t.Fatalf("mae-7 status = %q, want %q", sess7.Status, state.StatusRunning)
	}
	if sess7.TokensUsedAttempt != 30000 {
		t.Fatalf("mae-7 tokens_used = %d, want 30000", sess7.TokensUsedAttempt)
	}
	if sess7.TokensUsedTotal != 30000 {
		t.Fatalf("mae-7 tokens_used_total = %d, want 30000", sess7.TokensUsedTotal)
	}

	if len(stopped) != 1 || stopped[0] != "mae-6" {
		t.Fatalf("stopped = %v, want [mae-6]", stopped)
	}
}

func TestReconcileRunningSessions_TokenBudgetMarkerPreemptsOpenPR(t *testing.T) {
	dir := t.TempDir()
	logFile := dir + "/sup-906.log"
	if err := os.WriteFile(logFile, []byte("working\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	marker := worker.TokenBudgetMarker{
		Outcome:        worker.TokenBudgetExceededOutcome,
		Backend:        "claude",
		TokensObserved: 85_000,
		MaxTokens:      80_000,
		Measure:        worker.TokenBudgetMeasureUncached,
		MeasuredAt:     time.Now().UTC(),
	}
	data, err := json.Marshal(marker)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(worker.TokenBudgetMarkerPathForLog(logFile), data, 0o644); err != nil {
		t.Fatal(err)
	}

	o := &Orchestrator{
		cfg:      &config.Config{StateDir: dir, WorkerMaxTokens: 80_000},
		notifier: &notify.Notifier{},
		listOpenPRsFn: func() ([]github.PR, error) {
			return []github.PR{{Number: 906, HeadRefName: "feat/sup-906"}}, nil
		},
		pidAliveFn:          func(int) bool { return false },
		tmuxSessionExistsFn: func(string) bool { return false },
	}
	s := state.NewState()
	s.Sessions["sup-906"] = &state.Session{
		IssueNumber: 906,
		Status:      state.StatusRunning,
		PID:         1234,
		Branch:      "feat/sup-906",
		LogFile:     logFile,
		Backend:     "claude",
		StartedAt:   time.Now().UTC().Add(-time.Minute),
	}

	if !o.reconcileRunningSessions(s) {
		t.Fatal("expected reconciliation change")
	}
	sess := s.Sessions["sup-906"]
	if sess.Status != state.StatusFailed || sess.WorkerOutcome != worker.TokenBudgetExceededOutcome {
		t.Fatalf("status/outcome = %q/%q, want failed/%q", sess.Status, sess.WorkerOutcome, worker.TokenBudgetExceededOutcome)
	}
	if sess.PRNumber != 0 || sess.NextRetryAt != nil || sess.PID != 0 {
		t.Fatalf("budget stop was reclassified as PR/retry/running: %+v", sess)
	}
	if sess.TokenBudgetTokensAttempt != 85_000 || sess.TokenBudgetMeasure != worker.TokenBudgetMeasureUncached {
		t.Fatalf("budget observation = %d %q, want 85000 %q", sess.TokenBudgetTokensAttempt, sess.TokenBudgetMeasure, worker.TokenBudgetMeasureUncached)
	}
	if sess.TokensUsedAttempt != 0 || sess.TokensUsedTotal != 0 {
		t.Fatalf("uncached marker polluted inclusive telemetry: attempt=%d total=%d", sess.TokensUsedAttempt, sess.TokensUsedTotal)
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
		rebaseWorktreeFn: func(worktreePath, branch string, autoResolveFiles, autoRestoreFiles []string) error {
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

	result := o.mergeReadyPR(state.NewState(), "slot-0", sess, pr)

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
		rebaseWorktreeFn: func(worktreePath, branch string, autoResolveFiles, autoRestoreFiles []string) error {
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

	result := o.mergeReadyPR(state.NewState(), "slot-0", sess, pr)

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

func TestHandleRebaseConflictRetry_SchedulesInPlaceRetry(t *testing.T) {
	cfg := &config.Config{
		Repo:                     "owner/repo",
		AutoRetryRebaseConflicts: true,
		MaxRetriesPerIssue:       3,
		MaxRetryBackoffMs:        300000,
	}
	o := &Orchestrator{cfg: cfg, notifier: &notify.Notifier{}}
	s := state.NewState()
	sess := &state.Session{
		IssueNumber: 42,
		IssueTitle:  "docs refresh",
		Branch:      "feat/docs",
		Worktree:    "/tmp/wt",
		Status:      state.StatusPROpen,
		PRNumber:    10,
		Backend:     "claude",
		RetryCount:  3,
	}
	s.Sessions["slot-0"] = sess

	o.handleRebaseConflictRetry(s, "slot-0", sess, 10, fmt.Errorf("CONFLICT (content): docs/FEATURES.md"))

	if sess.Status != state.StatusDead {
		t.Fatalf("status = %q, want %q", sess.Status, state.StatusDead)
	}
	if sess.PRNumber != 10 {
		t.Fatalf("PRNumber = %d, want 10", sess.PRNumber)
	}
	if sess.RetryCount != 3 {
		t.Fatalf("RetryCount = %d, want 3 (implementation retry budget must not be consumed)", sess.RetryCount)
	}
	if sess.MaintenanceRetryCount != 1 {
		t.Fatalf("MaintenanceRetryCount = %d, want 1", sess.MaintenanceRetryCount)
	}
	if sess.NextRetryAt == nil {
		t.Fatal("NextRetryAt should be set")
	}
	if !sess.RebaseAttempted {
		t.Fatal("RebaseAttempted should be true")
	}
	if sess.PreviousAttemptFeedbackKind != "rebase_conflict" {
		t.Fatalf("PreviousAttemptFeedbackKind = %q, want rebase_conflict", sess.PreviousAttemptFeedbackKind)
	}
	if !strings.Contains(sess.PreviousAttemptFeedback, "docs/FEATURES.md") {
		t.Fatalf("PreviousAttemptFeedback should include conflict details, got %q", sess.PreviousAttemptFeedback)
	}
}

func TestHandleRebaseConflictRetry_IndependentFromReviewFeedbackRetry(t *testing.T) {
	cfg := &config.Config{
		Repo:                    "owner/repo",
		AutoRetryReviewFeedback: true,
		MaxRetriesPerIssue:      3,
		MaxRetryBackoffMs:       300000,
	}
	o := &Orchestrator{
		cfg:      cfg,
		notifier: &notify.Notifier{},
		addIssueLabelFn: func(number int, label string) error {
			if number != 42 || label != "blocked" {
				t.Fatalf("AddIssueLabel(%d, %q), want (42, blocked)", number, label)
			}
			return nil
		},
	}
	s := state.NewState()
	sess := &state.Session{
		IssueNumber: 42,
		IssueTitle:  "docs refresh",
		Status:      state.StatusPROpen,
		PRNumber:    10,
	}
	s.Sessions["slot-0"] = sess

	o.handleRebaseConflictRetry(s, "slot-0", sess, 10, fmt.Errorf("CONFLICT (content): docs/FEATURES.md"))

	if sess.Status != state.StatusConflictFailed {
		t.Fatalf("status = %q, want %q", sess.Status, state.StatusConflictFailed)
	}
	if sess.NextRetryAt != nil {
		t.Fatal("NextRetryAt should not be set when rebase retry is disabled")
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
		rebaseWorktreeFn: func(worktreePath, branch string, autoResolveFiles, autoRestoreFiles []string) error {
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

	result := o.mergeReadyPR(state.NewState(), "slot-0", sess, pr)

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
		rebaseWorktreeFn: func(worktreePath, branch string, autoResolveFiles, autoRestoreFiles []string) error {
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

	result := o.mergeReadyPR(state.NewState(), "slot-0", sess, pr)

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

// #602 — A real merge conflict on a StatusPROpen session must route through
// the conflict-resolution path (rebase worktree → markRebaseQueued on success)
// instead of looping with merge_failed forever.
func TestMergeReadyPR_RealConflictRoutesToRebaseForPROpen(t *testing.T) {
	rebased := false
	o := &Orchestrator{
		cfg:      &config.Config{Repo: "owner/repo", AutoRebase: true},
		notifier: &notify.Notifier{},
		ghMergePRFn: func(prNumber int) error {
			return fmt.Errorf("gh pr merge 10: merge commit cannot be cleanly created")
		},
		ghPRMergeStatusFn: func(prNumber int) (string, string, error) {
			return "CONFLICTING", "dirty", nil
		},
		rebaseWorktreeFn: func(worktreePath, branch string, autoResolveFiles, autoRestoreFiles []string) error {
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

	result := o.mergeReadyPR(state.NewState(), "slot-0", sess, pr)

	if result {
		t.Fatal("mergeReadyPR should return false for a CONFLICTING merge failure")
	}
	if !rebased {
		t.Fatal("rebase should be triggered when GitHub reports CONFLICTING")
	}
	if sess.Status != state.StatusQueued {
		t.Errorf("session status = %q, want %q after successful rebase", sess.Status, state.StatusQueued)
	}
	if !sess.RebaseAttempted {
		t.Error("RebaseAttempted should be true after rebase")
	}
	if sess.LastNotifiedStatus == "merge_failed" {
		t.Error("LastNotifiedStatus must not be merge_failed — the conflict was routed, not surfaced as a generic failure")
	}
}

// #602 — When the merge fails on a retry_exhausted convergence candidate and
// GitHub reports CONFLICTING, the session must reach a terminal advanced state
// (conflict_failed + blocked label) so the slot frees and the dynamic-wave
// queue advances on the next cycle. No worker respawn.
func TestMergeReadyPR_RealConflictMarksRetryExhaustedUnresolvable(t *testing.T) {
	labels := make([]string, 0)
	o := &Orchestrator{
		cfg:      &config.Config{Repo: "owner/repo", AutoRebase: true},
		notifier: &notify.Notifier{},
		ghMergePRFn: func(prNumber int) error {
			return fmt.Errorf("gh pr merge 10: merge commit cannot be cleanly created")
		},
		ghPRMergeStatusFn: func(prNumber int) (string, string, error) {
			return "CONFLICTING", "dirty", nil
		},
		rebaseWorktreeFn: func(worktreePath, branch string, autoResolveFiles, autoRestoreFiles []string) error {
			return fmt.Errorf("CONFLICT (content): main.go")
		},
		addIssueLabelFn: func(number int, label string) error {
			labels = append(labels, label)
			return nil
		},
	}

	sess := &state.Session{
		IssueNumber:        100,
		IssueTitle:         "test issue",
		Branch:             "feat/a",
		Worktree:           "/tmp/wt",
		Status:             state.StatusRetryExhausted,
		PRNumber:           10,
		LastNotifiedStatus: "review_retry_exhausted",
	}
	pr := github.PR{Number: 10, HeadRefName: "feat/a"}

	result := o.mergeReadyPR(state.NewState(), "slot-0", sess, pr)

	if result {
		t.Fatal("mergeReadyPR should return false on real conflict")
	}
	if sess.Status != state.StatusConflictFailed {
		t.Errorf("session status = %q, want %q (slot must free)", sess.Status, state.StatusConflictFailed)
	}
	if sess.FinishedAt == nil {
		t.Error("FinishedAt should be set so the slot is no longer active")
	}
	if !sess.RebaseAttempted {
		t.Error("RebaseAttempted should be true")
	}
	if len(labels) == 0 || labels[0] != "blocked" {
		t.Errorf("issue must be labelled blocked, got %v", labels)
	}
	if sess.LastNotifiedStatus == "merge_failed" {
		t.Error("LastNotifiedStatus must not stay merge_failed — conflict was reconciled, not silently logged")
	}
}

// #602 — A retry_exhausted convergence candidate with no remaining worktree
// (worker already cleaned up after retry exhaustion) must still reach
// conflict_failed without trying to respawn a worker via handleRebaseConflictRetry.
func TestMergeReadyPR_RealConflictRetryExhaustedNoWorktree(t *testing.T) {
	labels := make([]string, 0)
	rebaseCalled := false
	o := &Orchestrator{
		cfg:      &config.Config{Repo: "owner/repo", AutoRebase: true},
		notifier: &notify.Notifier{},
		ghMergePRFn: func(prNumber int) error {
			return fmt.Errorf("gh pr merge 10: merge commit cannot be cleanly created")
		},
		ghPRMergeStatusFn: func(prNumber int) (string, string, error) {
			return "CONFLICTING", "dirty", nil
		},
		rebaseWorktreeFn: func(worktreePath, branch string, autoResolveFiles, autoRestoreFiles []string) error {
			rebaseCalled = true
			return nil
		},
		addIssueLabelFn: func(number int, label string) error {
			labels = append(labels, label)
			return nil
		},
	}

	sess := &state.Session{
		IssueNumber:        100,
		IssueTitle:         "test issue",
		Branch:             "feat/a",
		Worktree:           "",
		Status:             state.StatusRetryExhausted,
		PRNumber:           10,
		LastNotifiedStatus: "review_retry_exhausted",
	}
	pr := github.PR{Number: 10, HeadRefName: "feat/a"}

	result := o.mergeReadyPR(state.NewState(), "slot-0", sess, pr)

	if result {
		t.Fatal("mergeReadyPR should return false on real conflict")
	}
	if rebaseCalled {
		t.Error("rebase must NOT be called when there is no worktree")
	}
	if sess.Status != state.StatusConflictFailed {
		t.Errorf("session status = %q, want %q (slot must free)", sess.Status, state.StatusConflictFailed)
	}
	if len(labels) == 0 || labels[0] != "blocked" {
		t.Errorf("issue must be labelled blocked, got %v", labels)
	}
}

// #602 — AutoRebase disabled: a real conflict still drives to a terminal state
// instead of looping silently.
func TestMergeReadyPR_RealConflictAutoRebaseDisabledMarksUnresolvable(t *testing.T) {
	labels := make([]string, 0)
	o := &Orchestrator{
		cfg:      &config.Config{Repo: "owner/repo", AutoRebase: false},
		notifier: &notify.Notifier{},
		ghMergePRFn: func(prNumber int) error {
			return fmt.Errorf("gh pr merge 10: merge commit cannot be cleanly created")
		},
		ghPRMergeStatusFn: func(prNumber int) (string, string, error) {
			return "CONFLICTING", "dirty", nil
		},
		addIssueLabelFn: func(number int, label string) error {
			labels = append(labels, label)
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

	if o.mergeReadyPR(state.NewState(), "slot-0", sess, pr) {
		t.Fatal("mergeReadyPR should return false on real conflict")
	}
	if sess.Status != state.StatusConflictFailed {
		t.Errorf("session status = %q, want %q", sess.Status, state.StatusConflictFailed)
	}
	if len(labels) == 0 || labels[0] != "blocked" {
		t.Errorf("issue must be labelled blocked, got %v", labels)
	}
}

// #602 — End-to-end: a retry_exhausted convergence candidate whose PR is
// CONFLICTING must NOT be re-selected on the next autoMergePRs cycle. After
// one cycle it should be in conflict_failed (slot freed) and on the second
// cycle mergePR must not be called again.
func TestAutoMergePRs_RetryExhaustedConflictDoesNotLoopHaltQueue(t *testing.T) {
	mergeAttempts := 0
	labels := make([]string, 0)
	prs := []github.PR{{Number: 10, HeadRefName: "feat/a"}}
	o := &Orchestrator{
		cfg: &config.Config{
			Repo:                            "owner/repo",
			MergeStrategy:                   "parallel",
			AutoRebase:                      true,
			AutoRetryReviewFeedback:         true,
			MergeExhaustedNonCriticalReview: boolPtr(true),
		},
		notifier: &notify.Notifier{},
		listOpenPRsFn: func() ([]github.PR, error) {
			return prs, nil
		},
		ghPRCIStatusFn: func(prNumber int) (string, error) {
			return "success", nil
		},
		ghCollectPRReviewFeedbackFn: func(prNumber int) (string, error) {
			return "P3 nit: rename foo", nil
		},
		ghPRHasCriticalReviewFn: func(prNumber int) (bool, error) {
			return false, nil
		},
		ghMergePRFn: func(prNumber int) error {
			mergeAttempts++
			return fmt.Errorf("gh pr merge %d: merge commit cannot be cleanly created", prNumber)
		},
		ghPRMergeStatusFn: func(prNumber int) (string, string, error) {
			return "CONFLICTING", "dirty", nil
		},
		rebaseWorktreeFn: func(worktreePath, branch string, autoResolveFiles, autoRestoreFiles []string) error {
			return fmt.Errorf("CONFLICT (content): conflicted.go")
		},
		addIssueLabelFn: func(number int, label string) error {
			labels = append(labels, label)
			return nil
		},
	}

	s := state.NewState()
	s.Sessions["slot-0"] = &state.Session{
		IssueNumber:        100,
		IssueTitle:         "test issue",
		Branch:             "feat/a",
		Worktree:           "/tmp/wt",
		Status:             state.StatusRetryExhausted,
		PRNumber:           10,
		LastNotifiedStatus: "review_retry_exhausted",
	}

	// First cycle: convergence picks the retry_exhausted PR, merge fails on
	// real conflict, mergeReadyPR drives the session to conflict_failed.
	o.autoMergePRs(s)
	if mergeAttempts != 1 {
		t.Fatalf("first cycle: mergeAttempts = %d, want 1", mergeAttempts)
	}
	sess := s.Sessions["slot-0"]
	if sess.Status != state.StatusConflictFailed {
		t.Fatalf("after first cycle status = %q, want %q", sess.Status, state.StatusConflictFailed)
	}
	if sess.FinishedAt == nil {
		t.Fatal("FinishedAt must be set so the supervisor stops treating the slot as active")
	}
	if len(labels) == 0 || labels[0] != "blocked" {
		t.Errorf("issue must be labelled blocked, got %v", labels)
	}

	// Second cycle: the session is no longer retry_exhausted, so convergence
	// must not select it again. mergePR must NOT be called a second time.
	o.autoMergePRs(s)
	if mergeAttempts != 1 {
		t.Fatalf("second cycle: mergeAttempts = %d, want 1 (queue must not loop)", mergeAttempts)
	}
}

// #602 — rebaseConflicts loop safety net: if a retry_exhausted session has a
// CONFLICTING open PR (e.g., entered this state via another path), the loop
// must drive it to conflict_failed without respawning a worker.
func TestRebaseConflicts_RetryExhaustedConflictingMarksUnresolvable(t *testing.T) {
	labels := make([]string, 0)
	prs := []github.PR{{Number: 10, HeadRefName: "feat/a"}}
	o := &Orchestrator{
		cfg:      &config.Config{Repo: "owner/repo", AutoRebase: true},
		notifier: &notify.Notifier{},
		listOpenPRsFn: func() ([]github.PR, error) {
			return prs, nil
		},
		ghPRMergeStatusFn: func(prNumber int) (string, string, error) {
			return "CONFLICTING", "dirty", nil
		},
		rebaseWorktreeFn: func(worktreePath, branch string, autoResolveFiles, autoRestoreFiles []string) error {
			return fmt.Errorf("CONFLICT (content): main.go")
		},
		addIssueLabelFn: func(number int, label string) error {
			labels = append(labels, label)
			return nil
		},
	}

	s := state.NewState()
	s.Sessions["slot-0"] = &state.Session{
		IssueNumber:        100,
		IssueTitle:         "test issue",
		Branch:             "feat/a",
		Worktree:           "/tmp/wt",
		Status:             state.StatusRetryExhausted,
		PRNumber:           10,
		LastNotifiedStatus: "review_retry_exhausted",
	}

	o.rebaseConflicts(s)

	sess := s.Sessions["slot-0"]
	if sess.Status != state.StatusConflictFailed {
		t.Errorf("session status = %q, want %q", sess.Status, state.StatusConflictFailed)
	}
	if !sess.RebaseAttempted {
		t.Error("RebaseAttempted should be true after the safety-net rebase attempt")
	}
	if sess.FinishedAt == nil {
		t.Error("FinishedAt must be set")
	}
	if len(labels) == 0 || labels[0] != "blocked" {
		t.Errorf("issue must be labelled blocked, got %v", labels)
	}
}

func TestRebaseConflicts_OpenPRBehindMainUpdatesUnderMaintenance(t *testing.T) {
	updateCalled := 0
	prs := []github.PR{{Number: 10, HeadRefName: "feat/a"}}
	o := &Orchestrator{
		cfg:      &config.Config{Repo: "owner/repo", AutoRebase: true},
		notifier: &notify.Notifier{},
		listOpenPRsFn: func() ([]github.PR, error) {
			return prs, nil
		},
		ghPRMergeStatusFn: func(prNumber int) (string, string, error) {
			return "MERGEABLE", "behind", nil
		},
		ghUpdateBranchFn: func(prNumber int) error {
			updateCalled++
			return nil
		},
	}

	s := state.NewState()
	s.Sessions["slot-0"] = &state.Session{
		IssueNumber: 100,
		IssueTitle:  "test issue",
		Branch:      "feat/a",
		Status:      state.StatusPROpen,
		PRNumber:    10,
		RetryCount:  3,
	}

	o.rebaseConflicts(s)

	sess := s.Sessions["slot-0"]
	if updateCalled != 1 {
		t.Fatalf("updateBranch called %d times, want 1", updateCalled)
	}
	if sess.Status != state.StatusQueued {
		t.Fatalf("status = %q, want %q", sess.Status, state.StatusQueued)
	}
	if !sess.RebaseAttempted {
		t.Fatal("RebaseAttempted should be true after behind update")
	}
	if sess.RetryCount != 3 {
		t.Fatalf("RetryCount = %d, want 3 (behind maintenance must not consume implementation retries)", sess.RetryCount)
	}
	if sess.MaintenanceRetryCount != 0 {
		t.Fatalf("MaintenanceRetryCount = %d, want 0 (clean branch update is not a worker retry)", sess.MaintenanceRetryCount)
	}
}

func TestRebaseConflicts_SharedPRMutatesOnlyNewestContinuation(t *testing.T) {
	updateCalled := 0
	prs := []github.PR{{Number: 388, HeadRefName: "feat/shared-pr"}}
	o := &Orchestrator{
		cfg:      &config.Config{Repo: "owner/repo", AutoRebase: true},
		notifier: &notify.Notifier{},
		listOpenPRsFn: func() ([]github.PR, error) {
			return prs, nil
		},
		ghPRMergeStatusFn: func(prNumber int) (string, string, error) {
			return "MERGEABLE", "behind", nil
		},
		ghUpdateBranchFn: func(prNumber int) error {
			updateCalled++
			return nil
		},
	}
	s := state.NewState()
	s.Sessions["ok-player-273"] = &state.Session{
		IssueNumber: 345, Status: state.StatusPROpen, PRNumber: 388, Branch: "feat/shared-pr",
		StartedAt: time.Date(2026, 7, 17, 23, 6, 0, 0, time.UTC),
	}
	s.Sessions["ok-player-302"] = &state.Session{
		IssueNumber: 406, Status: state.StatusPROpen, PRNumber: 388, Branch: "feat/shared-pr",
		StartedAt: time.Date(2026, 7, 18, 4, 57, 43, 0, time.UTC),
	}

	o.rebaseConflicts(s)

	if updateCalled != 1 {
		t.Fatalf("updateBranch called %d times, want exactly one canonical update", updateCalled)
	}
	if got := s.Sessions["ok-player-273"].Status; got != state.StatusPROpen {
		t.Fatalf("historical session status = %q, want untouched pr_open", got)
	}
	if got := s.Sessions["ok-player-302"].Status; got != state.StatusQueued {
		t.Fatalf("canonical continuation status = %q, want queued after update", got)
	}
}

// #602 — When the merge fails with a non-"not up to date" error AND GitHub
// reports MERGEABLE (so it's not actually a conflict), fall through to the
// existing single-notification merge_failed path instead of mis-routing.
func TestMergeReadyPR_NonConflictingMergeErrorFallsThroughToMergeFailed(t *testing.T) {
	o := &Orchestrator{
		cfg:      &config.Config{Repo: "owner/repo", AutoRebase: true},
		notifier: &notify.Notifier{},
		ghMergePRFn: func(prNumber int) error {
			return fmt.Errorf("gh pr merge 10: branch protection rule blocked the merge")
		},
		ghPRMergeStatusFn: func(prNumber int) (string, string, error) {
			return "MERGEABLE", "blocked", nil
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

	if o.mergeReadyPR(state.NewState(), "slot-0", sess, pr) {
		t.Fatal("mergeReadyPR should return false")
	}
	if sess.Status != state.StatusPROpen {
		t.Errorf("session status = %q, want %q (unchanged)", sess.Status, state.StatusPROpen)
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
	legacyOnly := false
	labels := make([]string, 0)
	return &Orchestrator{
		cfg: &config.Config{
			Repo:                       "owner/repo",
			WorkerSilentTimeoutMinutes: timeoutMinutes,
			StalledProgressWatchdog:    config.StalledProgressWatchdogConfig{Enabled: &legacyOnly},
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
	// auto-label blocked is disabled — verify no blocked label was added
	for _, label := range *labels {
		if label == "blocked" {
			t.Error("blocked label should not be added (auto-label blocked is disabled)")
		}
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

	result := o.mergeReadyPR(state.NewState(), "slot-0", sess, pr)

	if !result {
		t.Fatal("mergeReadyPR should return true on successful merge")
	}
	if !stopped {
		t.Fatal("worker should be stopped when cleanup_worktrees_on_merge is true")
	}
	if sess.Worktree != "" {
		t.Errorf("Worktree = %q, want empty (should be cleared after cleanup)", sess.Worktree)
	}
	if sess.Status != state.StatusCodeLanded {
		t.Errorf("Status = %q, want %q", sess.Status, state.StatusCodeLanded)
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

	result := o.mergeReadyPR(state.NewState(), "slot-0", sess, pr)

	if !result {
		t.Fatal("mergeReadyPR should return true on successful merge")
	}
	if stopped {
		t.Fatal("worker should NOT be stopped when cleanup_worktrees_on_merge is false")
	}
	if sess.Worktree != "/tmp/wt" {
		t.Errorf("Worktree = %q, want %q (should be preserved)", sess.Worktree, "/tmp/wt")
	}
	if sess.Status != state.StatusCodeLanded {
		t.Errorf("Status = %q, want %q", sess.Status, state.StatusCodeLanded)
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

	result := o.mergeReadyPR(state.NewState(), "slot-0", sess, pr)

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
	legacyOnly := false
	o := &Orchestrator{
		cfg: &config.Config{
			Repo:                       "owner/repo",
			WorkerSilentTimeoutMinutes: 10,
			StalledProgressWatchdog:    config.StalledProgressWatchdogConfig{Enabled: &legacyOnly},
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
	legacyOnly := false
	o := &Orchestrator{
		cfg: &config.Config{
			Repo:                       "owner/repo",
			WorkerSilentTimeoutMinutes: 10,
			StalledProgressWatchdog:    config.StalledProgressWatchdogConfig{Enabled: &legacyOnly},
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
		hasMergedPRForIssueFn: func(issueNumber int) (bool, error) {
			return false, nil
		},
		isPRMergedFn: func(prNumber int) (bool, error) {
			return false, nil
		},
		isIssueClosedFn: func(issueNumber int) (bool, error) {
			return false, nil
		},
		getIssueFn: func(number int) (github.Issue, error) {
			for _, issue := range issues {
				if issue.Number == number {
					return issue, nil
				}
			}
			return github.Issue{}, fmt.Errorf("issue #%d not found", number)
		},
		addIssueLabelFn: func(number int, label string) error {
			labels = append(labels, fmt.Sprintf("#%d:%s", number, label))
			return nil
		},
		workerStartFn: func(cfg *config.Config, s *state.State, repo string, issue github.Issue, promptBase, backend string) (string, error) {
			slotCounter++
			slotName := fmt.Sprintf("slot-%d", slotCounter)
			startedAt := time.Now().UTC()
			s.Sessions[slotName] = &state.Session{
				IssueNumber: issue.Number,
				IssueTitle:  issue.Title,
				Status:      state.StatusRunning,
				PID:         1000 + slotCounter,
				Branch:      fmt.Sprintf("feat/%s", slotName),
				StartedAt:   startedAt,
				Backend:     backend,
				Attribution: []state.BackendAttribution{
					{
						Backend:   backend,
						StartedAt: startedAt,
						Reason:    "initial_spawn",
					},
				},
			}
			started = append(started, issue.Number)
			return slotName, nil
		},
	}, &started, &labels
}

func TestStartNewWorkers_SkipsClosedIssueWithDoneSession(t *testing.T) {
	cfg := cfgWithBackends("claude", "claude")
	issues := []github.Issue{
		makeIssue(283, "already merged issue"),
	}

	o, started, _ := newStartWorkersOrchestrator(cfg, issues)
	o.isIssueClosedFn = func(issueNumber int) (bool, error) {
		return issueNumber == 283, nil
	}
	s := state.NewState()
	s.Sessions["slot-1"] = &state.Session{
		IssueNumber: 283,
		Status:      state.StatusDone,
	}

	o.startNewWorkers(s, 5)

	if len(*started) != 0 {
		t.Fatalf("started %d workers, want 0 for already closed issue", len(*started))
	}
}

func TestStartNewWorkers_RepairSpawnCannotBypassExcludedLabel(t *testing.T) {
	cfg := cfgWithBackends("claude", "claude")
	cfg.ExcludeLabels = []string{"needs-maintenance-triage"}
	cfg.Supervisor.ReviewRepair.MaxRetries = 1
	issues := []github.Issue{makeIssue(669, "repair open PR", "needs-maintenance-triage")}

	o, started, _ := newStartWorkersOrchestrator(cfg, issues)
	s := state.NewState()
	s.RecordSupervisorDecision(state.SupervisorDecision{
		ID:                "repair-1",
		CreatedAt:         time.Now().UTC(),
		RecommendedAction: supervisor.ActionSpawnReviewRepair,
		Risk:              supervisor.RiskMutating,
		RequiresApproval:  false,
		Target:            &state.SupervisorTarget{Issue: 669, PR: 1001, HeadSHA: "deadbeef"},
		ReviewRepair: &state.SupervisorReviewRepairPayload{
			HeadSHA:    "deadbeef",
			MaxRetries: 1,
			Backend:    "claude",
			Findings: []state.SupervisorReviewFinding{{
				Path:     "internal/foo.go",
				Line:     10,
				Body:     "P1: address this",
				Severity: "P1",
			}},
		},
	}, state.DefaultSupervisorDecisionLimit)

	o.startNewWorkers(s, 1)

	if len(*started) != 0 {
		t.Fatalf("started = %v, want no worker: current blocked label outranks delayed repair intent", *started)
	}
	if track, ok := s.LookupReviewRepairTrack(1001, "deadbeef"); ok || track.Attempts != 0 {
		t.Fatalf("review repair track = %+v, ok=%v; blocked dispatch must not consume an attempt", track, ok)
	}
}

// #816 regression: when the supervisor owns the ready label and recommends a
// review-repair spawn (policy rule supervisor.review_repair, not
// supervisor.dynamic_wave), the orchestrator must still treat the targeted
// issue as the selected candidate. Otherwise applySupervisorOwnedReadyFilter
// filters it out and the approved repair spawn never dispatches.
func TestStartNewWorkers_ReviewRepairSpawnBypassesSupervisorOwnedReadyFilter(t *testing.T) {
	cfg := cfgWithBackends("claude", "claude")
	dynamicWaveEnabled := true
	cfg.Supervisor.DynamicWave.Enabled = &dynamicWaveEnabled
	cfg.Supervisor.DynamicWave.OwnsReadyLabel = true
	cfg.Supervisor.ReviewRepair.MaxRetries = 3
	issues := []github.Issue{makeIssue(816, "wedged supervise loop", "maestro-ready")}

	o, started, _ := newStartWorkersOrchestrator(cfg, issues)
	s := state.NewState()
	now := time.Now().UTC()
	decision := state.SupervisorDecision{
		ID:                "repair-816",
		CreatedAt:         now,
		PolicyRule:        supervisor.PolicyRuleReviewRepair,
		RecommendedAction: supervisor.ActionSpawnReviewRepair,
		Risk:              supervisor.RiskMutating,
		RequiresApproval:  true,
		Target:            &state.SupervisorTarget{Issue: 816, PR: 820, HeadSHA: "4484a21c50b4"},
		ReviewRepair: &state.SupervisorReviewRepairPayload{
			HeadSHA:    "4484a21c50b4",
			MaxRetries: 3,
			Backend:    "claude",
			Findings: []state.SupervisorReviewFinding{{
				Path:     "internal/daemon/supervise.go",
				Line:     136,
				Body:     "P1: kick waits on wedged cycle",
				Severity: "P1",
			}},
		},
	}
	s.RecordSupervisorDecision(decision, state.DefaultSupervisorDecisionLimit)
	approval := s.RecordPendingApprovalForDecision(decision, now)
	approval.Status = state.ApprovalStatusAwaitingDispatch

	o.startNewWorkers(s, 1)

	if len(*started) != 1 || (*started)[0] != 816 {
		t.Fatalf("started = %v, want [816] for supervisor-selected review-repair under owned ready label", *started)
	}
}

func TestStartNewWorkers_RecordsProjectStatusSyncOnStart(t *testing.T) {
	cfg := cfgWithBackends("claude", "claude")
	cfg.GitHubProjects = config.GitHubProjectsConfig{Enabled: true, ProjectNumber: 3}
	issues := []github.Issue{makeIssue(347, "ready issue")}

	o, started, _ := newStartWorkersOrchestrator(cfg, issues)
	o.rateLimitFn = func() (github.RateLimitStatus, error) {
		return github.RateLimitStatus{
			GraphQL: github.RateLimitBucket{Limit: 5000, Remaining: 5000},
		}, nil
	}
	o.syncProjectFn = func(issueNumber int, status github.ProjectStatus) bool {
		if issueNumber != 347 {
			t.Fatalf("sync issue = %d, want 347", issueNumber)
		}
		if status != github.ProjectStatusInProgress {
			t.Fatalf("sync status = %q, want %q", status, github.ProjectStatusInProgress)
		}
		return true
	}

	s := state.NewState()
	o.startNewWorkers(s, 1)

	if len(*started) != 1 || (*started)[0] != 347 {
		t.Fatalf("started = %v, want [347]", *started)
	}
	if !s.ProjectStatusSynced(347, string(github.ProjectStatusInProgress)) {
		t.Fatal("startNewWorkers did not record project status sync")
	}
}

// #427: when a new worker is dispatched, the orchestrator must stamp the
// session's BackendSelection with a canonical reason so the dashboard /
// fleet API can surface label vs default vs auto vs router_error.
func TestStartNewWorkers_StampsBackendSelectionReason_Label(t *testing.T) {
	cfg := cfgWithBackends("claude", "claude", "codex")
	issues := []github.Issue{makeIssue(427, "Fix bug", "model:codex")}
	o, started, _ := newStartWorkersOrchestrator(cfg, issues)

	s := state.NewState()
	o.startNewWorkers(s, 1)

	if len(*started) != 1 {
		t.Fatalf("started = %d, want 1", len(*started))
	}
	sess := s.Sessions["slot-1"]
	if sess == nil || sess.BackendSelection == nil {
		t.Fatalf("session.BackendSelection not stamped: %+v", sess)
	}
	if sess.BackendSelection.SelectionReason != router.ReasonLabel {
		t.Errorf("BackendSelection.SelectionReason = %q, want %q", sess.BackendSelection.SelectionReason, router.ReasonLabel)
	}
	if sess.BackendSelection.SelectedBackend != "codex" {
		t.Errorf("BackendSelection.SelectedBackend = %q, want %q", sess.BackendSelection.SelectedBackend, "codex")
	}
}

func TestStartNewWorkers_StampsBackendSelectionReason_Default(t *testing.T) {
	cfg := cfgWithBackends("claude", "claude", "codex")
	issues := []github.Issue{makeIssue(428, "Fix bug")}
	o, started, _ := newStartWorkersOrchestrator(cfg, issues)

	s := state.NewState()
	o.startNewWorkers(s, 1)

	if len(*started) != 1 {
		t.Fatalf("started = %d, want 1", len(*started))
	}
	sess := s.Sessions["slot-1"]
	if sess == nil || sess.BackendSelection == nil {
		t.Fatalf("session.BackendSelection not stamped: %+v", sess)
	}
	if sess.BackendSelection.SelectionReason != router.ReasonDefault {
		t.Errorf("BackendSelection.SelectionReason = %q, want %q", sess.BackendSelection.SelectionReason, router.ReasonDefault)
	}
}

func TestStartNewWorkers_StampsBackendSelectionReason_Auto(t *testing.T) {
	cfg := cfgWithBackends("claude", "claude", "codex")
	cfg.Routing.Mode = "auto"
	issues := []github.Issue{makeIssue(429, "Refactor module")}
	o, started, _ := newStartWorkersOrchestrator(cfg, issues)
	o.router = router.New(cfg)
	o.router.DecisionFn = func(issue github.Issue) (router.Decision, error) {
		return router.Decision{Backend: "codex", TaskType: router.TaskTypeRefactor, Reason: "tight loop"}, nil
	}

	s := state.NewState()
	o.startNewWorkers(s, 1)

	if len(*started) != 1 {
		t.Fatalf("started = %d, want 1", len(*started))
	}
	sess := s.Sessions["slot-1"]
	if sess == nil || sess.BackendSelection == nil {
		t.Fatalf("session.BackendSelection not stamped: %+v", sess)
	}
	if sess.BackendSelection.SelectionReason != router.ReasonAuto {
		t.Errorf("BackendSelection.SelectionReason = %q, want %q", sess.BackendSelection.SelectionReason, router.ReasonAuto)
	}
	if sess.BackendSelection.TaskType != router.TaskTypeRefactor {
		t.Errorf("BackendSelection.TaskType = %q, want %q", sess.BackendSelection.TaskType, router.TaskTypeRefactor)
	}
	if len(sess.Attribution) != 1 || sess.Attribution[0].TaskType != router.TaskTypeRefactor {
		t.Fatalf("Attribution = %+v, want task_type %q on first segment", sess.Attribution, router.TaskTypeRefactor)
	}
}

func TestStartNewWorkers_StampsBackendSelectionReason_RouterError(t *testing.T) {
	cfg := cfgWithBackends("claude", "claude", "codex")
	cfg.Routing.Mode = "auto"
	issues := []github.Issue{makeIssue(430, "Routing fails")}
	o, started, _ := newStartWorkersOrchestrator(cfg, issues)
	o.router = router.New(cfg)
	o.router.RouteFn = func(issue github.Issue) (string, string, error) {
		return "", "", fmt.Errorf("network error")
	}

	s := state.NewState()
	o.startNewWorkers(s, 1)

	if len(*started) != 1 {
		t.Fatalf("started = %d, want 1", len(*started))
	}
	sess := s.Sessions["slot-1"]
	if sess == nil || sess.BackendSelection == nil {
		t.Fatalf("session.BackendSelection not stamped: %+v", sess)
	}
	if sess.BackendSelection.SelectionReason != router.ReasonRouterError {
		t.Errorf("BackendSelection.SelectionReason = %q, want %q", sess.BackendSelection.SelectionReason, router.ReasonRouterError)
	}
}

func TestStartNewWorkers_PipelineFullLabelSelectsPlannerPhase(t *testing.T) {
	cfg := cfgWithBackends("claude", "claude", "planner")
	cfg.Pipeline.Enabled = false
	cfg.Pipeline.Planner.Backend = "planner"
	issues := []github.Issue{makeIssue(641, "Use full pipeline", "pipeline:full")}
	o, started, _ := newStartWorkersOrchestrator(cfg, issues)

	var startCfg *config.Config
	var promptBase string
	o.workerStartFn = func(cfg *config.Config, s *state.State, repo string, issue github.Issue, prompt, backend string) (string, error) {
		startCfg = cfg
		promptBase = prompt
		s.Sessions["slot-1"] = &state.Session{
			IssueNumber: issue.Number,
			IssueTitle:  issue.Title,
			Status:      state.StatusRunning,
			PID:         1001,
			StartedAt:   time.Now().UTC(),
		}
		*started = append(*started, issue.Number)
		return "slot-1", nil
	}

	s := state.NewState()
	o.startNewWorkers(s, 1)

	if len(*started) != 1 || (*started)[0] != 641 {
		t.Fatalf("started = %v, want [641]", *started)
	}
	if cfg.Pipeline.Enabled {
		t.Fatal("global config Pipeline.Enabled was mutated")
	}
	if startCfg == nil {
		t.Fatal("worker config was not captured")
	}
	if !startCfg.Pipeline.Enabled || !startCfg.Pipeline.Planner.Enabled || !startCfg.Pipeline.Validator.Enabled {
		t.Fatalf("worker config did not enable full pipeline: %+v", startCfg.Pipeline)
	}
	sess := s.Sessions["slot-1"]
	if sess == nil {
		t.Fatal("session not recorded")
	}
	if sess.Phase != state.PhasePlan {
		t.Fatalf("session phase = %q, want %q", sess.Phase, state.PhasePlan)
	}
	if !sess.PipelineFull {
		t.Fatal("session PipelineFull was not recorded")
	}
	if sess.BackendSelection == nil || sess.BackendSelection.SelectedBackend != "planner" || sess.BackendSelection.SelectionReason != "phase" {
		t.Fatalf("backend selection = %+v, want planner phase", sess.BackendSelection)
	}
	if !strings.Contains(promptBase, pipeline.PlanFile) || !strings.Contains(promptBase, pipeline.ValidationFile) {
		t.Fatalf("planner prompt does not mention handoff artifacts %s/%s", pipeline.PlanFile, pipeline.ValidationFile)
	}
}

func TestStartNewWorkers_WithoutPipelineFullLabelKeepsSingleSession(t *testing.T) {
	cfg := cfgWithBackends("claude", "claude")
	cfg.Pipeline.Enabled = false
	issues := []github.Issue{makeIssue(642, "Routine issue")}
	o, started, _ := newStartWorkersOrchestrator(cfg, issues)

	var startCfg *config.Config
	o.workerStartFn = func(cfg *config.Config, s *state.State, repo string, issue github.Issue, prompt, backend string) (string, error) {
		startCfg = cfg
		s.Sessions["slot-1"] = &state.Session{
			IssueNumber: issue.Number,
			IssueTitle:  issue.Title,
			Status:      state.StatusRunning,
			PID:         1001,
			StartedAt:   time.Now().UTC(),
		}
		*started = append(*started, issue.Number)
		return "slot-1", nil
	}

	s := state.NewState()
	o.startNewWorkers(s, 1)

	if len(*started) != 1 || (*started)[0] != 642 {
		t.Fatalf("started = %v, want [642]", *started)
	}
	if startCfg != cfg {
		t.Fatal("unlabeled issue should use the original config")
	}
	sess := s.Sessions["slot-1"]
	if sess == nil {
		t.Fatal("session not recorded")
	}
	if sess.Phase != state.PhaseNone {
		t.Fatalf("session phase = %q, want no pipeline phase", sess.Phase)
	}
	if sess.PipelineFull {
		t.Fatal("unlabeled session should not record PipelineFull")
	}
}

func TestStartNewWorkers_SupervisorRepairSpawnRepairsReservedSessionInPlace(t *testing.T) {
	cfg := cfgWithBackends("codex", "codex")
	issues := []github.Issue{makeIssue(767, "repair stale PR")}
	o, started, _ := newStartWorkersOrchestrator(cfg, issues)
	authorizeCurrentFailedRepairGate(o, 769)
	o.hasOpenPRForIssueFn = func(issueNumber int) (bool, error) {
		return issueNumber == 767, nil
	}
	respawned := ""
	o.respawnInPlaceFn = func(cfg *config.Config, slotName string, sess *state.Session, repo string, issue github.Issue, promptBase string, backend string) error {
		respawned = slotName
		sess.Status = state.StatusRunning
		sess.PID = 5555
		return nil
	}

	s := state.NewState()
	s.Sessions["pan-12"] = &state.Session{
		IssueNumber: 767,
		IssueTitle:  "repair stale PR",
		Status:      state.StatusPROpen,
		PRNumber:    769,
		Branch:      "codex/old-pr",
		Worktree:    "/work/pan-12",
		Backend:     "codex",
	}
	s.RecordSupervisorDecision(state.SupervisorDecision{
		ID:                "sup-repair",
		CreatedAt:         time.Now().UTC(),
		RecommendedAction: supervisor.ActionSpawnRepairWorker,
		Risk:              supervisor.RiskMutating,
		RequiresApproval:  false,
		Target:            &state.SupervisorTarget{Issue: 767, PR: 769, Session: "pan-12"},
	}, state.DefaultSupervisorDecisionLimit)

	o.startNewWorkers(s, 1)

	if len(*started) != 0 {
		t.Fatalf("fresh starts = %v, want none for same-session repair", *started)
	}
	if respawned != "pan-12" {
		t.Fatalf("respawned = %q, want reserved session pan-12", respawned)
	}
	if len(s.Sessions) != 1 || s.Sessions["pan-12"].Status != state.StatusRunning {
		t.Fatalf("sessions = %+v, want only pan-12 running", s.Sessions)
	}
}

func TestStartNewWorkers_ApprovedRepairIgnoresOlderDoneTerminalClaim(t *testing.T) {
	cfg := cfgWithBackends("codex", "codex")
	cfg.WorktreeBase = t.TempDir()
	restoredWorktree := filepath.Join(cfg.WorktreeBase, "sup-360")
	if err := os.MkdirAll(restoredWorktree, 0o755); err != nil {
		t.Fatalf("mkdir restored worktree: %v", err)
	}
	if err := os.WriteFile(filepath.Join(restoredWorktree, ".git"), []byte("gitdir: test\n"), 0o644); err != nil {
		t.Fatalf("write restored worktree metadata: %v", err)
	}
	issues := []github.Issue{makeIssue(887, "finish watchdog recovery")}
	o, started, _ := newStartWorkersOrchestrator(cfg, issues)
	authorizeCurrentFailedRepairGate(o, 914)
	o.hasOpenPRForIssueFn = func(issueNumber int) (bool, error) {
		return issueNumber == 887, nil
	}
	respawned := ""
	o.respawnInPlaceFn = func(cfg *config.Config, slotName string, sess *state.Session, repo string, issue github.Issue, promptBase string, backend string) error {
		respawned = slotName
		sess.Status = state.StatusRunning
		sess.PID = 5555
		return nil
	}

	oldFinished := time.Date(2026, 7, 14, 18, 59, 0, 0, time.UTC)
	s := state.NewState()
	s.Sessions["sup-318"] = &state.Session{
		IssueNumber: 887,
		Status:      state.StatusDone,
		PRNumber:    893,
		StartedAt:   oldFinished.Add(-time.Hour),
		FinishedAt:  &oldFinished,
	}
	s.Sessions["sup-360"] = &state.Session{
		IssueNumber: 887,
		IssueTitle:  "finish watchdog recovery",
		Status:      state.StatusRetryExhausted,
		PRNumber:    914,
		Branch:      "feat/sup-360-887-watchdog-recovery",
		Backend:     "codex",
		// Cleanup/import can lose the attempt timestamp. Canonical PR identity,
		// not an optional timestamp, orders an older completed PR claim.
		StartedAt: time.Time{},
	}
	repair := repairApproval("repair-newer-pr", 887, 914, state.ApprovalStatusPending, oldFinished.Add(24*time.Hour))
	repair.Target.Session = "sup-360"
	s.Approvals = []state.Approval{repair}
	if _, err := s.ApproveApproval("repair-newer-pr", oldFinished.Add(25*time.Hour), "operator", "approve exact repair"); err != nil {
		t.Fatalf("approve repair: %v", err)
	}

	o.startNewWorkers(s, 1)

	if len(*started) != 0 {
		t.Fatalf("fresh starts = %v, want exact in-place repair", *started)
	}
	if respawned != "sup-360" {
		t.Fatalf("respawned = %q, want sup-360", respawned)
	}
	if got := s.Sessions["sup-318"].Status; got != state.StatusDone {
		t.Fatalf("older terminal session status = %q, want done", got)
	}
	if got := s.Sessions["sup-360"].Worktree; got != restoredWorktree {
		t.Fatalf("restored worktree = %q, want %q", got, restoredWorktree)
	}
	if got := approvalStatus(t, s, "repair-newer-pr"); got != state.ApprovalStatusSuperseded {
		t.Fatalf("repair approval = %q, want consumed/superseded", got)
	}
	if len(s.Sessions) != 2 {
		t.Fatalf("sessions = %d, want no new session", len(s.Sessions))
	}
}

func TestStartNewWorkers_SupervisorRepairSpawnHonorsCurrentModelLabelInPlace(t *testing.T) {
	cfg := cfgWithBackends("codex", "codex", "sol")
	issues := []github.Issue{makeIssue(345, "resume retained packaging work", "model:sol")}
	o, started, _ := newStartWorkersOrchestrator(cfg, issues)
	o.hasOpenPRForIssueFn = func(int) (bool, error) { return false, nil }
	gotBackend := ""
	o.respawnInPlaceFn = func(cfg *config.Config, slotName string, sess *state.Session, repo string, issue github.Issue, promptBase string, backend string) error {
		gotBackend = backend
		sess.Status = state.StatusRunning
		sess.Backend = backend
		return nil
	}

	s := state.NewState()
	s.Sessions["ok-player-273"] = &state.Session{
		IssueNumber: 345,
		IssueTitle:  "resume retained packaging work",
		Status:      state.StatusDead,
		Worktree:    "/work/ok-player-273",
		Branch:      "feat/ok-player-273-345",
		Backend:     "codex",
	}
	s.RecordSupervisorDecision(state.SupervisorDecision{
		ID:                "sup-repair-label",
		CreatedAt:         time.Now().UTC(),
		RecommendedAction: supervisor.ActionSpawnRepairWorker,
		Risk:              supervisor.RiskMutating,
		RequiresApproval:  false,
		Target:            &state.SupervisorTarget{Issue: 345, Session: "ok-player-273"},
	}, state.DefaultSupervisorDecisionLimit)

	o.startNewWorkers(s, 1)

	if len(*started) != 0 {
		t.Fatalf("fresh starts = %v, want retained-session repair", *started)
	}
	if gotBackend != "sol" {
		t.Fatalf("repair backend = %q, want current explicit label backend sol", gotBackend)
	}
	if got := s.Sessions["ok-player-273"].Backend; got != "sol" {
		t.Fatalf("session backend = %q, want sol", got)
	}
}

func TestStartNewWorkers_SupervisorRepairSpawnDoesNotDuplicateRunningWorker(t *testing.T) {
	cfg := cfgWithBackends("codex", "codex")
	issues := []github.Issue{makeIssue(808, "repair retry exhausted")}
	o, started, _ := newStartWorkersOrchestrator(cfg, issues)
	o.pidAliveFn = func(pid int) bool { return pid == 5555 }

	s := state.NewState()
	s.Sessions["pan-74"] = &state.Session{
		IssueNumber: 808,
		IssueTitle:  "repair retry exhausted",
		Status:      state.StatusRunning,
		PID:         5555,
	}
	s.Sessions["pan-72"] = &state.Session{
		IssueNumber: 808,
		IssueTitle:  "repair retry exhausted",
		Status:      state.StatusRetryExhausted,
	}
	s.RecordSupervisorDecision(state.SupervisorDecision{
		ID:                "sup-repair",
		CreatedAt:         time.Now().UTC(),
		RecommendedAction: supervisor.ActionSpawnRepairWorker,
		Risk:              supervisor.RiskMutating,
		RequiresApproval:  false,
		Target:            &state.SupervisorTarget{Issue: 808, Session: "pan-72"},
	}, state.DefaultSupervisorDecisionLimit)

	o.startNewWorkers(s, 1)

	if len(*started) != 0 {
		t.Fatalf("started = %v, want no duplicate worker while pan-74 is running", *started)
	}
}

func TestStartNewWorkers_SupervisorOwnedReadyLabelStartsOnlySelectedCandidate(t *testing.T) {
	cfg := cfgWithBackends("claude", "claude")
	cfg.IssueLabels = []string{"maestro-ready"}
	dynamicWaveEnabled := true
	cfg.Supervisor.DynamicWave.Enabled = &dynamicWaveEnabled
	cfg.Supervisor.DynamicWave.OwnsReadyLabel = true
	issues := []github.Issue{
		makeIssue(292, "stale ready", "maestro-ready", "p0"),
		makeIssue(287, "selected ready", "maestro-ready", "p1"),
		makeIssue(291, "also stale", "maestro-ready", "p2"),
	}

	o, started, _ := newStartWorkersOrchestrator(cfg, issues)
	s := state.NewState()
	s.RecordSupervisorDecision(state.SupervisorDecision{
		CreatedAt:  time.Now().UTC(),
		PolicyRule: supervisor.PolicyRuleDynamicWave,
		QueueAnalysis: &state.SupervisorQueueAnalysis{
			PolicyRule: supervisor.PolicyRuleDynamicWave,
			SelectedCandidate: &state.SupervisorIssueCandidate{
				Number: 287,
			},
		},
	}, state.DefaultSupervisorDecisionLimit)

	var logs strings.Builder
	previousLogOutput := log.Writer()
	log.SetOutput(&logs)
	defer log.SetOutput(previousLogOutput)

	o.startNewWorkers(s, 5)

	if len(*started) != 1 {
		t.Fatalf("started = %v, want only selected issue #287", *started)
	}
	if (*started)[0] != 287 {
		t.Fatalf("started issue #%d, want #287", (*started)[0])
	}
	if strings.Contains(logs.String(), "starting worker for issue #292") {
		t.Fatalf("logs show stale issue #292 started:\n%s", logs.String())
	}
	if !strings.Contains(logs.String(), "skipping issue #292: not supervisor-selected candidate #287") {
		t.Fatalf("logs = %q, want skip reason for stale ready issue", logs.String())
	}
}

func TestStartNewWorkers_SupervisorOwnedReadyLabelFetchesSelectedCandidateOutsideReadyListing(t *testing.T) {
	cfg := cfgWithBackends("claude", "claude")
	cfg.IssueLabels = []string{"maestro-ready"}
	dynamicWaveEnabled := true
	cfg.Supervisor.DynamicWave.Enabled = &dynamicWaveEnabled
	cfg.Supervisor.DynamicWave.OwnsReadyLabel = true

	listedIssues := []github.Issue{
		makeIssue(292, "stale ready", "maestro-ready", "p0"),
		makeIssue(291, "also stale", "maestro-ready", "p2"),
	}
	selectedIssue := makeIssue(287, "selected candidate not yet relisted", "p1")

	o, started, _ := newStartWorkersOrchestrator(cfg, listedIssues)
	o.getIssueFn = func(number int) (github.Issue, error) {
		if number == 287 {
			return selectedIssue, nil
		}
		for _, issue := range listedIssues {
			if issue.Number == number {
				return issue, nil
			}
		}
		return github.Issue{}, fmt.Errorf("issue #%d not found", number)
	}

	s := state.NewState()
	s.RecordSupervisorDecision(state.SupervisorDecision{
		CreatedAt:  time.Now().UTC(),
		PolicyRule: supervisor.PolicyRuleDynamicWave,
		QueueAnalysis: &state.SupervisorQueueAnalysis{
			PolicyRule: supervisor.PolicyRuleDynamicWave,
			SelectedCandidate: &state.SupervisorIssueCandidate{
				Number: 287,
			},
		},
	}, state.DefaultSupervisorDecisionLimit)

	var logs strings.Builder
	previousLogOutput := log.Writer()
	log.SetOutput(&logs)
	defer log.SetOutput(previousLogOutput)

	o.startNewWorkers(s, 5)

	if len(*started) != 1 {
		t.Fatalf("started = %v, want only fetched selected issue #287", *started)
	}
	if (*started)[0] != 287 {
		t.Fatalf("started issue #%d, want #287", (*started)[0])
	}
	if !strings.Contains(logs.String(), "fetched supervisor-selected candidate #287 directly for immediate dispatch") {
		t.Fatalf("logs = %q, want direct-fetch dispatch log", logs.String())
	}
}

func TestStartNewWorkers_ManualIssueLabelsUnchangedWhenSupervisorDoesNotOwnReadyLabel(t *testing.T) {
	cfg := cfgWithBackends("claude", "claude")
	cfg.IssueLabels = []string{"maestro-ready"}
	issues := []github.Issue{
		makeIssue(292, "first ready", "maestro-ready"),
		makeIssue(287, "second ready", "maestro-ready"),
	}

	o, started, _ := newStartWorkersOrchestrator(cfg, issues)
	s := state.NewState()
	s.RecordSupervisorDecision(state.SupervisorDecision{
		CreatedAt:  time.Now().UTC(),
		PolicyRule: supervisor.PolicyRuleDynamicWave,
		QueueAnalysis: &state.SupervisorQueueAnalysis{
			PolicyRule: supervisor.PolicyRuleDynamicWave,
			SelectedCandidate: &state.SupervisorIssueCandidate{
				Number: 287,
			},
		},
	}, state.DefaultSupervisorDecisionLimit)

	o.startNewWorkers(s, 5)

	if got, want := fmt.Sprint(*started), "[292 287]"; got != want {
		t.Fatalf("started = %s, want %s", got, want)
	}
}

func TestStartNewWorkers_OrderedQueueStartsOnlyFirstPendingIssue(t *testing.T) {
	cfg := cfgWithBackends("claude", "claude")
	cfg.Supervisor.OrderedQueue = config.SupervisorOrderedQueueConfig{Enabled: true, Issues: []int{308, 306, 305}}
	issues := []github.Issue{
		makeIssue(308, "first"),
		makeIssue(306, "second"),
		makeIssue(305, "third"),
	}

	o, started, _ := newStartWorkersOrchestrator(cfg, issues)
	s := state.NewState()
	o.startNewWorkers(s, 5)

	if len(*started) != 1 {
		t.Fatalf("started %d workers, want 1", len(*started))
	}
	if (*started)[0] != 308 {
		t.Fatalf("started issue #%d, want #308", (*started)[0])
	}
}

func TestStartNewWorkers_OrderedQueueWaitsWhileCurrentRunning(t *testing.T) {
	cfg := cfgWithBackends("claude", "claude")
	cfg.Supervisor.OrderedQueue = config.SupervisorOrderedQueueConfig{Enabled: true, Issues: []int{308, 306}}
	issues := []github.Issue{makeIssue(308, "current"), makeIssue(306, "next")}

	o, started, _ := newStartWorkersOrchestrator(cfg, issues)
	s := state.NewState()
	s.Sessions["slot-1"] = &state.Session{IssueNumber: 308, Status: state.StatusRunning}
	o.startNewWorkers(s, 5)

	if len(*started) != 0 {
		t.Fatalf("started %v, want no worker while ordered issue #308 is running", *started)
	}
}

func TestStartNewWorkers_OrderedQueueAdvancesAfterClosedIssue(t *testing.T) {
	cfg := cfgWithBackends("claude", "claude")
	cfg.Supervisor.OrderedQueue = config.SupervisorOrderedQueueConfig{Enabled: true, Issues: []int{308, 306}}
	issues := []github.Issue{makeIssue(306, "next")}

	o, started, _ := newStartWorkersOrchestrator(cfg, issues)
	o.isIssueClosedFn = func(issueNumber int) (bool, error) {
		return issueNumber == 308, nil
	}
	s := state.NewState()
	o.startNewWorkers(s, 5)

	if len(*started) != 1 || (*started)[0] != 306 {
		t.Fatalf("started = %v, want [306]", *started)
	}
}

func TestStartNewWorkers_OrderedQueuePausesOnBlockedCurrentIssue(t *testing.T) {
	cfg := cfgWithBackends("claude", "claude")
	cfg.BlockerPatterns = []string{`blocked by #(\d+)`}
	cfg.Supervisor.OrderedQueue = config.SupervisorOrderedQueueConfig{Enabled: true, Issues: []int{308, 306}}
	issues := []github.Issue{
		{Number: 308, Title: "blocked", Body: "blocked by #100"},
		makeIssue(306, "next"),
	}

	o, started, _ := newStartWorkersOrchestrator(cfg, issues)
	o.isIssueClosedFn = func(issueNumber int) (bool, error) {
		return false, nil
	}
	s := state.NewState()
	o.startNewWorkers(s, 5)

	if len(*started) != 0 {
		t.Fatalf("started = %v, want none while #308 is blocked", *started)
	}
}

func TestStartNewWorkers_OrderedQueuePausesOnRetryExhaustedCurrentIssue(t *testing.T) {
	cfg := cfgWithBackends("claude", "claude")
	cfg.MaxRetriesPerIssue = 2
	cfg.Supervisor.OrderedQueue = config.SupervisorOrderedQueueConfig{Enabled: true, Issues: []int{308, 306}}
	issues := []github.Issue{makeIssue(308, "flaky"), makeIssue(306, "next")}

	o, started, _ := newStartWorkersOrchestrator(cfg, issues)
	s := state.NewState()
	now := time.Now().UTC()
	for i := 0; i < 2; i++ {
		finished := now.Add(-time.Duration(i+1) * time.Hour)
		s.Sessions[fmt.Sprintf("old-%d", i)] = &state.Session{
			IssueNumber: 308,
			Status:      state.StatusDead,
			FinishedAt:  &finished,
		}
	}
	o.startNewWorkers(s, 5)

	if len(*started) != 0 {
		t.Fatalf("started = %v, want none while #308 is retry-exhausted", *started)
	}
	if !s.IssueRetryExhausted(308) {
		t.Fatal("issue #308 should be marked retry_exhausted")
	}
}

func TestStartNewWorkers_OrderedQueueAdvancesAfterLinkedPRMerged(t *testing.T) {
	cfg := cfgWithBackends("claude", "claude")
	cfg.Supervisor.OrderedQueue = config.SupervisorOrderedQueueConfig{Enabled: true, Issues: []int{308, 306}}
	issues := []github.Issue{makeIssue(306, "next")}

	o, started, _ := newStartWorkersOrchestrator(cfg, issues)
	o.hasMergedPRForIssueFn = func(issueNumber int) (bool, error) {
		return issueNumber == 308, nil
	}
	s := state.NewState()
	o.startNewWorkers(s, 5)

	if len(*started) != 1 || (*started)[0] != 306 {
		t.Fatalf("started = %v, want [306]", *started)
	}
}

func TestStartNewWorkers_OrderedQueueAdvancesAfterDoneSessionPRMerged(t *testing.T) {
	cfg := cfgWithBackends("claude", "claude")
	cfg.Supervisor.OrderedQueue = config.SupervisorOrderedQueueConfig{Enabled: true, Issues: []int{308, 306}}
	issues := []github.Issue{makeIssue(306, "next")}

	o, started, _ := newStartWorkersOrchestrator(cfg, issues)
	o.isPRMergedFn = func(prNumber int) (bool, error) {
		return prNumber == 77, nil
	}
	s := state.NewState()
	s.Sessions["old"] = &state.Session{IssueNumber: 308, Status: state.StatusDone, PRNumber: 77}
	o.startNewWorkers(s, 5)

	if len(*started) != 1 || (*started)[0] != 306 {
		t.Fatalf("started = %v, want [306]", *started)
	}
}

func TestStartNewWorkers_OrderedQueueAdvancesAfterPolicyDone(t *testing.T) {
	cfg := cfgWithBackends("claude", "claude")
	cfg.Supervisor.OrderedQueue = config.SupervisorOrderedQueueConfig{Enabled: true, Issues: []int{308, 306}, DoneIssues: []int{308}}
	issues := []github.Issue{makeIssue(306, "next")}

	o, started, _ := newStartWorkersOrchestrator(cfg, issues)
	s := state.NewState()
	o.startNewWorkers(s, 5)

	if len(*started) != 1 || (*started)[0] != 306 {
		t.Fatalf("started = %v, want [306]", *started)
	}
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

	// auto-label blocked is disabled — verify no blocked label was added
	for _, label := range *labels {
		if label == "#42:blocked" {
			t.Errorf("blocked label should not be added (auto-label blocked is disabled), labels = %v", *labels)
		}
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

	// First cycle: should mark retry_exhausted (but no blocked label — disabled)
	o.startNewWorkers(s, 5)
	if !s.IssueRetryExhausted(42) {
		t.Fatal("issue #42 should be retry_exhausted after first detection")
	}
	// auto-label blocked is disabled — no labels should be added
	for _, label := range *labels {
		if label == "#42:blocked" {
			t.Errorf("blocked label should not be added (auto-label blocked is disabled)")
		}
	}
	firstLabelCount := len(*labels)

	// Second cycle: should skip and not add any labels
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

func TestCheckSessions_ClosedNoDeliveryIssueBecomesDoneWhenProjectOutcomeFails(t *testing.T) {
	now := time.Now().UTC()
	worktree := t.TempDir()
	preserved := filepath.Join(worktree, "preserved.txt")
	if err := os.WriteFile(preserved, []byte("completed work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	o := &Orchestrator{
		cfg: &config.Config{
			Repo:              "owner/repo",
			MaxRuntimeMinutes: 120,
			Outcome: outcome.Brief{
				DesiredOutcome:      "Live app works",
				VerifierCommand:     "check-live",
				PassRequiredForDone: boolPtr(true),
			},
		},
		notifier: &notify.Notifier{},
		listOpenPRsFn: func() ([]github.PR, error) {
			return []github.PR{}, nil
		},
		isIssueClosedFn: func(issueNumber int) (bool, error) {
			return true, nil
		},
		mergedPRForBranchFn: func(branch string) (int, error) {
			return 0, nil
		},
		mergedPRForIssueFn: func(issueNumber int) (int, error) {
			// A prior lifecycle of this issue shipped PR #77 before the issue was
			// reopened. That historical issue-level link must not be attributed to
			// this later no-delivery session.
			return 77, nil
		},
		workerStopFn: func(cfg *config.Config, slotName string, sess *state.Session) error {
			t.Fatalf("terminal reconciliation must preserve the retained worktree; workerStopFn must not run")
			return nil
		},
	}

	s := state.NewState()
	s.OutcomeHealth = &outcome.HealthCheckResult{
		CheckedAt: now,
		State:     outcome.HealthFailing,
		Signal:    "verifier_command",
		Summary:   "live verifier failed",
	}
	s.Sessions["pan-10"] = &state.Session{
		IssueNumber: 100,
		IssueTitle:  "dead worker",
		Status:      state.StatusDead,
		Branch:      "feat/pan-10-100-dead",
		Worktree:    worktree,
		FinishedAt:  &now,
	}

	o.checkSessions(s)

	sess := s.Sessions["pan-10"]
	if sess.Status != state.StatusDone {
		t.Fatalf("status = %q, want %q: no merged revision is owned by this closed issue", sess.Status, state.StatusDone)
	}
	if sess.Branch != "feat/pan-10-100-dead" {
		t.Fatalf("branch = %q, want retained branch unchanged", sess.Branch)
	}
	if sess.Worktree != worktree {
		t.Fatalf("worktree = %q, want retained %q", sess.Worktree, worktree)
	}
	if _, err := os.Stat(preserved); err != nil {
		t.Fatalf("retained work disappeared: %v", err)
	}
}

func TestCheckSessions_ClosedMergedIssueBecomesDoneWhileProjectOutcomeFails(t *testing.T) {
	now := time.Now().UTC()
	o := &Orchestrator{
		cfg: &config.Config{
			Repo:              "owner/repo",
			MaxRuntimeMinutes: 120,
			Outcome: outcome.Brief{
				DesiredOutcome:      "Live app works",
				VerifierCommand:     "check-live",
				PassRequiredForDone: boolPtr(true),
			},
		},
		notifier:        &notify.Notifier{},
		listOpenPRsFn:   func() ([]github.PR, error) { return nil, nil },
		isIssueClosedFn: func(issueNumber int) (bool, error) { return true, nil },
		isPRMergedFn:    func(prNumber int) (bool, error) { return prNumber == 77, nil },
	}

	s := state.NewState()
	s.OutcomeHealth = &outcome.HealthCheckResult{
		CheckedAt: now,
		State:     outcome.HealthFailing,
		Signal:    "verifier_command",
		Summary:   "live verifier failed",
	}
	s.Sessions["pan-10"] = &state.Session{
		IssueNumber:        100,
		IssueTitle:         "merged worker",
		Status:             state.StatusFailed,
		LastClosedPRNumber: 77,
		FinishedAt:         &now,
	}

	o.checkSessions(s)

	sess := s.Sessions["pan-10"]
	if sess.Status != state.StatusDone || sess.PRNumber != 77 || sess.IssueClosedAt == nil {
		t.Fatalf("session = status %q PR #%d closed_at=%v, want terminal done with merged PR #77 retained", sess.Status, sess.PRNumber, sess.IssueClosedAt)
	}
	if s.OutcomeHealth == nil || s.OutcomeHealth.State != outcome.HealthFailing {
		t.Fatalf("project outcome = %+v, want independently failing outcome retained", s.OutcomeHealth)
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

func TestCheckSessions_RetryExhaustedClosedIssue_TransitionsToDone(t *testing.T) {
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
	s.Sessions["pan-12"] = &state.Session{
		IssueNumber: 102,
		IssueTitle:  "retry exhausted but closed",
		Status:      state.StatusRetryExhausted,
		Branch:      "feat/pan-12-102-retry",
		FinishedAt:  &now,
	}

	o.checkSessions(s)

	sess := s.Sessions["pan-12"]
	if sess.Status != state.StatusDone {
		t.Fatalf("status = %q, want %q", sess.Status, state.StatusDone)
	}
}

func TestCheckSessions_RetryExhaustedMergedPR_TransitionsToCodeLanded(t *testing.T) {
	now := time.Now().UTC()
	o := &Orchestrator{
		cfg:      &config.Config{Repo: "owner/repo", MaxRuntimeMinutes: 120},
		notifier: &notify.Notifier{},
		listOpenPRsFn: func() ([]github.PR, error) {
			return []github.PR{}, nil
		},
		isPRMergedFn: func(prNumber int) (bool, error) {
			return prNumber == 77, nil
		},
		isIssueClosedFn: func(issueNumber int) (bool, error) {
			return false, nil
		},
		workerStopFn: func(cfg *config.Config, slotName string, sess *state.Session) error {
			return nil
		},
	}

	s := state.NewState()
	s.Sessions["pan-13"] = &state.Session{
		IssueNumber: 103,
		IssueTitle:  "retry exhausted but merged",
		Status:      state.StatusRetryExhausted,
		Branch:      "feat/pan-13-103-retry",
		PRNumber:    77,
		FinishedAt:  &now,
	}

	o.checkSessions(s)

	sess := s.Sessions["pan-13"]
	if sess.Status != state.StatusCodeLanded {
		t.Fatalf("status = %q, want %q", sess.Status, state.StatusCodeLanded)
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
//
// When rateLimited is true the fixture also mocks rateLimitResetFromLogFn to
// return a non-nil reset time, mirroring a real high-confidence provider
// usage-limit response. Per #663, the orchestrator only triggers a backend
// fallback on a high-confidence signal (parseable reset window); a tests that
// wants to verify low-confidence behavior should leave rateLimitResetFromLogFn
// nil (or override it) so providerRateLimitFromLog reports no positive signal.
func newFallbackTestOrchestrator(cfg *config.Config, rateLimited bool) (*Orchestrator, *[]string) {
	respawnedBackends := make([]string, 0)
	fixedReset := time.Date(2027, time.January, 1, 12, 0, 0, 0, time.UTC)
	o := &Orchestrator{
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
	}
	if rateLimited {
		o.rateLimitResetFromLogFn = func(string) *time.Time {
			r := fixedReset
			return &r
		}
	}
	return o, &respawnedBackends
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
	o, stopped, _ := newRateLimitOrchestrator(cfg, "Error: You've hit your limit for today. Try again at January 1, 2027 12:00 PM.")

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
	o, stopped, respawned := newRateLimitOrchestrator(cfg, "rate limit exceeded. Try again at January 1, 2027 12:00 PM.")

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

func TestCheckSessions_RateLimitDetected_DefaultBackendFallback_Respawns(t *testing.T) {
	cfg := &config.Config{
		Repo:              "owner/repo",
		MaxRuntimeMinutes: 999,
		Model: config.ModelConfig{
			Default: "codex",
			Backends: map[string]config.BackendDef{
				"claude": {Cmd: "claude"},
				"codex":  {Cmd: "codex"},
			},
		},
	}
	o, stopped, respawned := newRateLimitOrchestrator(cfg, "Error: You've hit your limit for today. Try again at January 1, 2027 12:00 PM.")

	s := state.NewState()
	s.Sessions["mae-default"] = &state.Session{
		IssueNumber: 142,
		IssueTitle:  "test issue",
		Status:      state.StatusRunning,
		PID:         4242,
		TmuxSession: "maestro-mae-default",
		Branch:      "feat/mae-default-142-test",
		Backend:     "claude",
		StartedAt:   time.Now().Add(-10 * time.Minute),
	}

	o.checkSessions(s)

	sess := s.Sessions["mae-default"]
	if len(*stopped) != 1 || (*stopped)[0] != "mae-default" {
		t.Fatalf("stopped = %v, want [mae-default]", *stopped)
	}
	if len(*respawned) != 1 || (*respawned)[0][1] != "codex" {
		t.Fatalf("respawned = %v, want fallback to codex", *respawned)
	}
	if sess.Backend != "codex" || sess.Status != state.StatusRunning {
		t.Fatalf("session backend/status = %q/%q, want codex/running", sess.Backend, sess.Status)
	}
	if sess.ProviderLimitBackend != "claude" || sess.ProviderLimitReason == "" {
		t.Fatalf("provider limit = %q/%q, want claude with reason", sess.ProviderLimitBackend, sess.ProviderLimitReason)
	}
	if health := s.BackendHealth["claude"]; health.State != state.BackendHealthCooldown || health.Reason != state.BackendBlockProviderLimit {
		t.Fatalf("claude health = %+v, want provider-limit cooldown", health)
	}
	if sess.BackendSelection == nil {
		t.Fatal("backend selection should be recorded")
	}
	if sess.BackendSelection.SelectedBackend != "codex" || sess.BackendSelection.SelectionReason != selectionReasonProviderLimitFallback {
		t.Fatalf("selection = %+v, want codex provider-limit fallback", sess.BackendSelection)
	}
}

func TestCheckSessions_RateLimitDetected_NoAvailableFallback_RecordsSelection(t *testing.T) {
	cfg := &config.Config{
		Repo:              "owner/repo",
		MaxRuntimeMinutes: 999,
		Model: config.ModelConfig{
			Default: "claude",
			Backends: map[string]config.BackendDef{
				"claude": {Cmd: "claude"},
			},
		},
	}
	o, stopped, respawned := newRateLimitOrchestrator(cfg, "Error: You've hit your limit for today. Try again at January 1, 2027 12:00 PM.")

	s := state.NewState()
	s.Sessions["mae-none"] = &state.Session{
		IssueNumber: 143,
		IssueTitle:  "test issue",
		Status:      state.StatusRunning,
		PID:         4343,
		TmuxSession: "maestro-mae-none",
		Branch:      "feat/mae-none-143-test",
		Backend:     "claude",
		StartedAt:   time.Now().Add(-10 * time.Minute),
	}

	o.checkSessions(s)

	sess := s.Sessions["mae-none"]
	if len(*stopped) != 1 || (*stopped)[0] != "mae-none" {
		t.Fatalf("stopped = %v, want [mae-none]", *stopped)
	}
	if len(*respawned) != 0 {
		t.Fatalf("respawned = %v, want no fallback", *respawned)
	}
	if sess.Status != state.StatusDead || sess.LastNotifiedStatus != "rate_limit" {
		t.Fatalf("session status/notification = %q/%q, want dead/rate_limit", sess.Status, sess.LastNotifiedStatus)
	}
	if sess.BackendSelection == nil || sess.BackendSelection.SelectedBackend != "" {
		t.Fatalf("selection = %+v, want no selected backend", sess.BackendSelection)
	}
	attention := state.SessionAttentionFor(sess, nil)
	if !attention.NeedsAttention || !strings.Contains(attention.Reason, "provider capacity limit") {
		t.Fatalf("attention = %+v, want provider capacity attention", attention)
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
	o, stopped, respawned := newRateLimitOrchestrator(cfg, "Error 429: too many requests. Try again at January 1, 2027 12:00 PM.")

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
	o, stopped, _ := newRateLimitOrchestrator(cfg, "API returned status 429. Try again at January 1, 2027 12:00 PM.")

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

// TestCheckSessions_RateLimit_LowConfidenceNoFallback_LiveWorker is the
// #663 regression guard for the live tmux path: when the classifier matches a
// rate-limit pattern in worker output but no provider-stated reset window is
// present, the orchestrator must NOT kill the worker and MUST NOT switch
// backends. The apertune session apt-2 hit a codex tools-router
// `write_stdin failed: stdin is closed` error and recovered to open a PR;
// any classifier match without a reset hint is treated the same way — a
// pass-through, low-confidence signal that the worker is left to resolve.
func TestCheckSessions_RateLimit_LowConfidenceNoFallback_LiveWorker(t *testing.T) {
	cfg := &config.Config{
		Repo:              "owner/repo",
		MaxRuntimeMinutes: 999,
		Model: config.ModelConfig{
			Default:          "codex",
			FallbackBackends: []string{"claude"},
			Backends: map[string]config.BackendDef{
				"codex":  {Cmd: "codex"},
				"claude": {Cmd: "claude"},
			},
		},
	}
	// "rate limit exceeded" matches the classifier but carries no parseable
	// reset — the orchestrator must treat this as low-confidence (per #663).
	o, stopped, respawned := newRateLimitOrchestrator(cfg, "transient: rate limit exceeded once, retrying")

	s := state.NewState()
	s.Sessions["mae-663-live"] = &state.Session{
		IssueNumber: 663,
		IssueTitle:  "issue under test",
		Status:      state.StatusRunning,
		PID:         42421,
		TmuxSession: "maestro-mae-663-live",
		Branch:      "feat/mae-663-live",
		Backend:     "codex",
		StartedAt:   time.Now().Add(-2 * time.Minute),
	}

	o.checkSessions(s)

	sess := s.Sessions["mae-663-live"]
	if sess.Status != state.StatusRunning {
		t.Fatalf("status = %q, want %q — low-confidence signal must not kill a live worker (#663)", sess.Status, state.StatusRunning)
	}
	if sess.RateLimitHit {
		t.Fatal("rate_limit_hit must remain false on low-confidence signal (#663)")
	}
	if sess.Backend != "codex" {
		t.Fatalf("backend = %q, want %q — must not switch backends on low-confidence signal", sess.Backend, "codex")
	}
	if len(*stopped) != 0 {
		t.Fatalf("stopped = %v, want empty — must not stop the worker on low-confidence signal", *stopped)
	}
	if len(*respawned) != 0 {
		t.Fatalf("respawned = %v, want empty — must not respawn on low-confidence signal", *respawned)
	}
}

// TestCheckSessions_RateLimit_CodexWriteStdinError_NotClassified is the
// direct #663 regression: a codex tools-router `write_stdin failed: stdin is
// closed` error, in the exact shape that wedged apertune apt-2, MUST be
// passed through as ordinary worker output — no kill, no provider-limit
// record, no fallback selection.
func TestCheckSessions_RateLimit_CodexWriteStdinError_NotClassified(t *testing.T) {
	cfg := &config.Config{
		Repo:              "owner/repo",
		MaxRuntimeMinutes: 999,
		Model: config.ModelConfig{
			Default:          "codex",
			FallbackBackends: []string{"claude"},
			Backends: map[string]config.BackendDef{
				"codex":  {Cmd: "codex"},
				"claude": {Cmd: "claude"},
			},
		},
	}
	tmuxOutput := "2026-06-04T19:41:32Z ERROR codex_core::tools::router: error=write_stdin failed: stdin is closed\nfor this session; rerun exec_command with tty=true to keep stdin open\nworker continued and opened PR.\n"
	o, stopped, respawned := newRateLimitOrchestrator(cfg, tmuxOutput)

	s := state.NewState()
	s.Sessions["mae-663-wstdin"] = &state.Session{
		IssueNumber: 663,
		IssueTitle:  "apertune apt-2 repro",
		Status:      state.StatusRunning,
		PID:         42422,
		TmuxSession: "maestro-mae-663-wstdin",
		Branch:      "feat/mae-663-wstdin",
		Backend:     "codex",
		StartedAt:   time.Now().Add(-2 * time.Minute),
	}

	o.checkSessions(s)

	sess := s.Sessions["mae-663-wstdin"]
	if sess.Status != state.StatusRunning {
		t.Fatalf("status = %q, want %q — codex write_stdin error must not be classified as rate-limit (#663)", sess.Status, state.StatusRunning)
	}
	if sess.RateLimitHit {
		t.Fatal("rate_limit_hit must remain false for write_stdin error (#663)")
	}
	if sess.ProviderLimitBackend != "" {
		t.Fatalf("provider_limit_backend = %q, want empty — must not record provider limit (#663)", sess.ProviderLimitBackend)
	}
	if sess.BackendSelection != nil {
		t.Fatalf("backend_selection = %+v, want nil — must not run fallback selector (#663)", sess.BackendSelection)
	}
	if len(*stopped) != 0 {
		t.Fatalf("stopped = %v, want empty (#663)", *stopped)
	}
	if len(*respawned) != 0 {
		t.Fatalf("respawned = %v, want empty (#663)", *respawned)
	}
}

// TestReconcileRunningSessions_RateLimit_LowConfidenceNoFallback is the #663
// regression guard for the reconcile path: when the dead worker's log shows a
// rate-limit pattern WITHOUT a parseable reset window, the orchestrator must
// fall through to the ordinary running->dead handling instead of triggering a
// backend fallback. The reconcile log line `rate-limited on backend=...
// reset=unknown — respawned with backend=...` that wedged apertune MUST NOT
// be emitted in this case.
func TestReconcileRunningSessions_RateLimit_LowConfidenceNoFallback(t *testing.T) {
	s := state.NewState()
	s.Sessions["sup-663"] = &state.Session{
		IssueNumber: 663,
		IssueTitle:  "apertune apt-2 repro",
		Status:      state.StatusRunning,
		PID:         42423,
		TmuxSession: "maestro-sup-663",
		Branch:      "feat/sup-663-663-apertune-repro",
		Backend:     "codex",
		StartedAt:   time.Now().Add(-2 * time.Minute),
		LogFile:     "/tmp/sup-663-rl.log",
	}

	respawnAttempts := []string{}
	o := &Orchestrator{
		cfg: &config.Config{
			Repo: "owner/repo",
			Model: config.ModelConfig{
				Default:          "codex",
				FallbackBackends: []string{"claude"},
				Backends: map[string]config.BackendDef{
					"codex":  {Cmd: "codex"},
					"claude": {Cmd: "claude"},
				},
			},
		},
		notifier:            &notify.Notifier{},
		pidAliveFn:          func(pid int) bool { return false },
		tmuxSessionExistsFn: func(name string) bool { return false },
		listOpenPRsFn:       func() ([]github.PR, error) { return []github.PR{}, nil },
		// Classifier reports a hit (e.g. stale prompt context echoed "rate limit")...
		isRateLimitedFn: func(logFile string) bool { return true },
		// ...but the log carries no parseable reset hint — low confidence.
		rateLimitResetFromLogFn: func(string) *time.Time { return nil },
		getIssueFn: func(number int) (github.Issue, error) {
			return github.Issue{Number: number, Title: "apertune apt-2 repro"}, nil
		},
		respawnWorkerFn: func(cfg *config.Config, slotName string, sess *state.Session, repo string, issue github.Issue, promptBase string, backendName string) error {
			respawnAttempts = append(respawnAttempts, backendName)
			return nil
		},
	}

	changed := o.reconcileRunningSessions(s)
	if !changed {
		t.Fatal("expected reconciliation to mark dead worker dead")
	}

	sess := s.Sessions["sup-663"]
	if sess.Status != state.StatusDead {
		t.Fatalf("status = %q, want %q (ordinary dead path)", sess.Status, state.StatusDead)
	}
	if sess.LastNotifiedStatus == "rate_limit" {
		t.Fatal("last_notified_status must NOT be 'rate_limit' on low-confidence detection (#663)")
	}
	if sess.RateLimitHit {
		t.Fatal("rate_limit_hit must remain false on low-confidence detection (#663)")
	}
	if sess.ProviderLimitBackend != "" {
		t.Fatalf("provider_limit_backend = %q, want empty (#663)", sess.ProviderLimitBackend)
	}
	if len(respawnAttempts) != 0 {
		t.Fatalf("respawnWorkerFn called %v, want no fallback respawn on low-confidence signal (#663)", respawnAttempts)
	}
	if _, recorded := s.BackendHealth["codex"]; recorded {
		t.Fatal("BackendHealth[codex] must not be recorded on low-confidence signal (#663)")
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
	// Provider capacity is not a code failure; it should not consume normal retry budget.
	if sess2.Status != state.StatusDead {
		t.Fatalf("status = %q, want %q", sess2.Status, state.StatusDead)
	}
	if sess2.NextRetryAt != nil {
		t.Fatal("NextRetryAt should not be set for provider-capacity exhaustion")
	}
	if len(*respawned) != 0 {
		t.Fatalf("respawned = %v, want []", *respawned)
	}
	if sess2.RetryCount != 0 {
		t.Fatalf("retry_count = %d, want 0", sess2.RetryCount)
	}
	if sess2.LastNotifiedStatus != "rate_limit" {
		t.Fatalf("last_notified_status = %q, want rate_limit", sess2.LastNotifiedStatus)
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

func TestCheckSessions_RateLimitNoFallbackConfigured_DoesNotUseBackendMapOrder(t *testing.T) {
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
	if sess2.Status != state.StatusDead {
		t.Fatalf("status = %q, want dead with no configured fallback", sess2.Status)
	}
	if sess2.Backend != "claude" {
		t.Fatalf("backend = %q, want unchanged claude", sess2.Backend)
	}
	if len(*respawned) != 0 {
		t.Fatalf("respawned = %v, want none", *respawned)
	}
	if sess2.RetryCount != 0 {
		t.Fatalf("retry_count = %d, want 0", sess2.RetryCount)
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

// #814: with max_live_workers set, pr_open PR-gate sessions no longer consume
// spawn capacity, so a gate-bound queue keeps dispatching live workers up to the
// configured live-worker limit.
func TestAvailableSlots_MaxLiveWorkersIgnoresPROpen(t *testing.T) {
	cfg := &config.Config{MaxParallel: 4, MaxLiveWorkers: 4}
	s := state.NewState()
	s.Sessions["slot-1"] = &state.Session{Status: state.StatusRunning}
	s.Sessions["slot-2"] = &state.Session{Status: state.StatusRunning}
	s.Sessions["slot-3"] = &state.Session{Status: state.StatusPROpen}
	s.Sessions["slot-4"] = &state.Session{Status: state.StatusPROpen}

	got := availableSlots(cfg, s, len(s.ActiveSessions()))
	if got != 2 {
		t.Errorf("availableSlots() = %d, want 2 (4 live limit - 2 running; pr_open ignored)", got)
	}
}

// Acceptance: a project with N PR-open sessions can still spawn live workers up
// to the live-worker limit — the case where legacy counting reported 0.
func TestAvailableSlots_MaxLiveWorkersAllGatesStillDispatches(t *testing.T) {
	cfg := &config.Config{MaxParallel: 2, MaxLiveWorkers: 3}
	s := state.NewState()
	for i := 1; i <= 4; i++ {
		s.Sessions[fmt.Sprintf("gate-%d", i)] = &state.Session{Status: state.StatusPROpen}
	}

	// Legacy counting would give max_parallel(2) - active(4) => 0 slots.
	if got := availableSlots(cfg, s, len(s.ActiveSessions())); got != 3 {
		t.Errorf("availableSlots() = %d, want 3 (keep dispatching despite 4 open PRs)", got)
	}
}

func TestPendingRetryReservations_CountsOnlyScheduledDeadRetries(t *testing.T) {
	now := time.Now().UTC()
	past := now.Add(-time.Second)
	future := now.Add(time.Minute)
	s := state.NewState()
	s.Sessions["retry-due"] = &state.Session{Status: state.StatusDead, NextRetryAt: &past}
	s.Sessions["retry-now"] = &state.Session{Status: state.StatusDead, NextRetryAt: &now}
	s.Sessions["retry-waiting"] = &state.Session{Status: state.StatusDead, NextRetryAt: &future}
	s.Sessions["plain-dead"] = &state.Session{Status: state.StatusDead}
	s.Sessions["running-with-retry"] = &state.Session{Status: state.StatusRunning, NextRetryAt: &now}

	got := pendingRetryReservations(s)
	if got != 2 {
		t.Fatalf("pendingRetryReservations() = %d, want 2", got)
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

func TestFindOpenBlockersExceptEpics_IgnoresOpenEpic(t *testing.T) {
	o := &Orchestrator{
		isIssueClosedFn: func(number int) (bool, error) {
			return false, nil
		},
	}
	issues := []github.Issue{
		{Number: 307, Title: "Epic: parent work", Labels: []struct {
			Name string `json:"name"`
		}{{Name: "epic"}}},
	}

	got := o.findOpenBlockersExceptEpics([]int{307, 42}, issues)
	if len(got) != 1 || got[0] != 42 {
		t.Errorf("findOpenBlockersExceptEpics() = %v, want [42]", got)
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
	// Blocker #10 is closed; the dispatched issue #42 is open (the pre-spawn guard
	// from issue #456 also calls isIssueClosed on the candidate itself).
	o.isIssueClosedFn = func(number int) (bool, error) {
		return number == 10, nil
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
		MaxLiveWorkers:     1,
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
		notifier: notify.NewWithToken("", "", "", ""),
		router:   router.New(cfg),
	}

	newCfg := &config.Config{
		Repo:               "owner/repo",
		MaxParallel:        10,
		MaxLiveWorkers:     3,
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
	if o.cfg.MaxLiveWorkers != 3 {
		t.Errorf("MaxLiveWorkers = %d, want 3", o.cfg.MaxLiveWorkers)
	}
	if o.cfg.MaxRuntimeMinutes != 60 {
		t.Errorf("MaxRuntimeMinutes = %d, want 60", o.cfg.MaxRuntimeMinutes)
	}
	if o.cfg.MaxRetriesPerIssue != 5 {
		t.Errorf("MaxRetriesPerIssue = %d, want 5", o.cfg.MaxRetriesPerIssue)
	}
}

func TestReloadConfig_MaxLiveWorkersIncreaseExpandsCapacity(t *testing.T) {
	cfg := cfgWithBackends("claude", "claude")
	cfg.MaxParallel = 3
	cfg.MaxLiveWorkers = 1
	o, started, _ := newStartWorkersOrchestrator(cfg, []github.Issue{
		makeIssue(901, "first ready issue"),
		makeIssue(902, "second ready issue"),
		makeIssue(903, "third ready issue"),
	})

	s := state.NewState()
	s.Sessions["existing"] = &state.Session{IssueNumber: 900, Status: state.StatusRunning}
	if got := s.Capacity(capacityInput(o.cfg)).AvailableSlots; got != 0 {
		t.Fatalf("initial available slots = %d, want 0 at max_live_workers=1", got)
	}

	newCfg := *cfg
	newCfg.MaxLiveWorkers = 3
	ticker := time.NewTicker(10 * time.Minute)
	defer ticker.Stop()
	o.reloadConfig(&newCfg, &ticker)

	slots := s.Capacity(capacityInput(o.cfg)).AvailableSlots
	if slots != 2 {
		t.Fatalf("available slots after reload = %d, want 2", slots)
	}
	o.startNewWorkers(s, slots)
	if len(*started) != 2 {
		t.Fatalf("started workers after reload = %v, want two new workers", *started)
	}
	if got := s.Capacity(capacityInput(o.cfg)).LiveWorkers; got != 3 {
		t.Fatalf("live workers after dispatch = %d, want 3", got)
	}
}

func TestReloadConfig_MaxLiveWorkersDecreaseStopsNewDispatch(t *testing.T) {
	cfg := cfgWithBackends("claude", "claude")
	cfg.MaxParallel = 3
	cfg.MaxLiveWorkers = 3
	o, started, _ := newStartWorkersOrchestrator(cfg, []github.Issue{
		makeIssue(904, "ready after downshift"),
		makeIssue(905, "another ready after downshift"),
	})

	s := state.NewState()
	s.Sessions["one"] = &state.Session{IssueNumber: 901, Status: state.StatusRunning}
	s.Sessions["two"] = &state.Session{IssueNumber: 902, Status: state.StatusRunning}
	if got := s.Capacity(capacityInput(o.cfg)).AvailableSlots; got != 1 {
		t.Fatalf("initial available slots = %d, want 1 at max_live_workers=3", got)
	}

	newCfg := *cfg
	newCfg.MaxLiveWorkers = 1
	ticker := time.NewTicker(10 * time.Minute)
	defer ticker.Stop()
	o.reloadConfig(&newCfg, &ticker)

	slots := s.Capacity(capacityInput(o.cfg)).AvailableSlots
	if slots != 0 {
		t.Fatalf("available slots after downshift = %d, want 0", slots)
	}
	o.startNewWorkers(s, 2)
	if len(*started) != 0 {
		t.Fatalf("started workers after downshift = %v, want none beyond lowered limit", *started)
	}
	if got := s.Capacity(capacityInput(o.cfg)).LiveWorkers; got != 2 {
		t.Fatalf("live workers after blocked dispatch = %d, want existing workers only", got)
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
		notifier: notify.NewWithToken("", "", "", ""),
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
		notifier: notify.NewWithToken("", "", "", ""),
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
		notifier: notify.NewWithToken("", "", "", ""),
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
		notifier:   notify.NewWithToken("", "", "", ""),
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
		Repo:               "owner/repo",
		MaxRetryBackoffMs:  300000,
		MaxRuntimeMinutes:  999,
		MaxRetriesPerIssue: 3, // explicit: allow retries (0 would mean unlimited)
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
		Repo:               "owner/repo",
		MaxRetryBackoffMs:  300000,
		MaxRuntimeMinutes:  999,
		MaxRetriesPerIssue: 1, // fail after 1 retry
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
	// auto-label blocked is disabled — verify no blocked label was added
	for _, label := range labeled {
		if label == "blocked" {
			t.Error("blocked label should not be added (auto-label blocked is disabled)")
		}
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

	o.respawnDueRetries(s, 10)

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

func TestRespawnDueRetries_RespectsAvailableSlots(t *testing.T) {
	cfg := &config.Config{
		Repo:              "owner/repo",
		MaxRetryBackoffMs: 300000,
		MaxRuntimeMinutes: 999,
	}
	respawned := make([]string, 0)
	o := &Orchestrator{
		cfg:        cfg,
		notifier:   &notify.Notifier{},
		promptBase: "test prompt",
		getIssueFn: func(number int) (github.Issue, error) {
			return makeIssue(number, "test issue"), nil
		},
		respawnWorkerFn: func(cfg *config.Config, slotName string, sess *state.Session, repo string, issue github.Issue, promptBase string, backend string) error {
			respawned = append(respawned, slotName)
			sess.Status = state.StatusRunning
			sess.PID = 5555
			return nil
		},
	}

	pastTime := time.Now().UTC().Add(-1 * time.Second)
	s := state.NewState()
	s.Sessions["mae-12"] = &state.Session{
		IssueNumber: 112,
		IssueTitle:  "first retry",
		Status:      state.StatusDead,
		RetryCount:  1,
		NextRetryAt: &pastTime,
		Branch:      "feat/mae-12-112-test",
	}
	s.Sessions["mae-13"] = &state.Session{
		IssueNumber: 113,
		IssueTitle:  "second retry",
		Status:      state.StatusDead,
		RetryCount:  1,
		NextRetryAt: &pastTime,
		Branch:      "feat/mae-13-113-test",
	}

	o.respawnDueRetries(s, 1)

	if len(respawned) != 1 {
		t.Fatalf("respawned %d workers, want 1", len(respawned))
	}
	if s.Sessions["mae-12"].Status != state.StatusRunning {
		t.Fatalf("mae-12 status = %q, want %q", s.Sessions["mae-12"].Status, state.StatusRunning)
	}
	if s.Sessions["mae-13"].Status != state.StatusDead {
		t.Fatalf("mae-13 status = %q, want %q", s.Sessions["mae-13"].Status, state.StatusDead)
	}
	if s.Sessions["mae-13"].NextRetryAt == nil {
		t.Fatal("mae-13 NextRetryAt should remain set when no retry slot is available")
	}
}

func TestRespawnDueRetries_WithOpenPRRespawnsInPlace(t *testing.T) {
	cfg := &config.Config{
		Repo:              "owner/repo",
		MaxRetryBackoffMs: 300000,
		MaxRuntimeMinutes: 999,
	}
	respawnedFresh := false
	respawnedInPlace := false
	o := &Orchestrator{
		cfg:        cfg,
		notifier:   &notify.Notifier{},
		promptBase: "test prompt",
		getIssueFn: func(number int) (github.Issue, error) {
			return makeIssue(number, "test issue"), nil
		},
		respawnWorkerFn: func(cfg *config.Config, slotName string, sess *state.Session, repo string, issue github.Issue, promptBase string, backend string) error {
			respawnedFresh = true
			return nil
		},
		respawnInPlaceFn: func(cfg *config.Config, slotName string, sess *state.Session, repo string, issue github.Issue, promptBase string, backend string) error {
			respawnedInPlace = true
			sess.Status = state.StatusRunning
			sess.PID = 5555
			return nil
		},
	}

	pastTime := time.Now().UTC().Add(-1 * time.Second)
	s := state.NewState()
	s.Sessions["mae-12"] = &state.Session{
		IssueNumber:             112,
		IssueTitle:              "review retry",
		Status:                  state.StatusDead,
		RetryCount:              1,
		NextRetryAt:             &pastTime,
		Branch:                  "feat/mae-12-112-test",
		Worktree:                "/tmp/maestro-mae-12",
		PRNumber:                10,
		PreviousAttemptFeedback: "review feedback",
	}

	o.respawnDueRetries(s, 1)

	if !respawnedInPlace {
		t.Fatal("expected in-place respawn for retry with open PR and worktree")
	}
	if respawnedFresh {
		t.Fatal("fresh respawn should not be used for retry with open PR and worktree")
	}
	if s.Sessions["mae-12"].PRNumber != 10 {
		t.Fatalf("PRNumber = %d, want 10", s.Sessions["mae-12"].PRNumber)
	}
	if s.Sessions["mae-12"].PreviousAttemptFeedback != "" {
		t.Fatal("PreviousAttemptFeedback should be consumed before respawn")
	}
}

func TestRespawnDueRetries_StalledProgressResumesExactWorktreeInPlace(t *testing.T) {
	cfg := &config.Config{
		Repo:              "owner/repo",
		MaxRetryBackoffMs: 300000,
		MaxRuntimeMinutes: 999,
	}
	respawnedFresh := false
	respawnedInPlace := false
	o := &Orchestrator{
		cfg:        cfg,
		notifier:   &notify.Notifier{},
		promptBase: "test prompt",
		getIssueFn: func(number int) (github.Issue, error) {
			return makeIssue(number, "test issue"), nil
		},
		respawnWorkerFn: func(cfg *config.Config, slotName string, sess *state.Session, repo string, issue github.Issue, promptBase string, backend string) error {
			respawnedFresh = true
			return nil
		},
		respawnInPlaceFn: func(cfg *config.Config, slotName string, sess *state.Session, repo string, issue github.Issue, promptBase string, backend string) error {
			respawnedInPlace = true
			sess.Status = state.StatusRunning
			sess.PID = 5555
			return nil
		},
	}

	pastTime := time.Now().UTC().Add(-time.Second)
	s := state.NewState()
	s.Sessions["mae-12"] = &state.Session{
		IssueNumber: 112,
		IssueTitle:  "test issue",
		Status:      state.StatusDead,
		RetryCount:  1,
		NextRetryAt: &pastTime,
		RetryReason: state.RetryReasonStalledProgress,
		Branch:      "feat/mae-12-112-test",
		Worktree:    "/tmp/maestro-mae-12",
	}

	o.respawnDueRetries(s, 1)

	if !respawnedInPlace || respawnedFresh {
		t.Fatalf("stalled-progress respawn: in_place=%t fresh=%t", respawnedInPlace, respawnedFresh)
	}
	if s.Sessions["mae-12"].Worktree != "/tmp/maestro-mae-12" {
		t.Fatalf("stalled-progress retry changed worktree: %+v", s.Sessions["mae-12"])
	}
}

// #874: restart_worker on a finished pr_open session tears down the worktree
// and (via the restart controller) clears sess.Worktree/PRNumber. The dead
// session that respawnDueRetries then picks up must take the FRESH respawn
// path, never RespawnInPlace against the directory that was just removed.
func TestRespawnDueRetries_ClearedWorktreeRespawnsFresh(t *testing.T) {
	cfg := &config.Config{
		Repo:              "owner/repo",
		MaxRetryBackoffMs: 300000,
		MaxRuntimeMinutes: 999,
	}
	respawnedFresh := false
	respawnedInPlace := false
	o := &Orchestrator{
		cfg:        cfg,
		notifier:   &notify.Notifier{},
		promptBase: "test prompt",
		getIssueFn: func(number int) (github.Issue, error) {
			return makeIssue(number, "test issue"), nil
		},
		respawnWorkerFn: func(cfg *config.Config, slotName string, sess *state.Session, repo string, issue github.Issue, promptBase string, backend string) error {
			respawnedFresh = true
			sess.Status = state.StatusRunning
			sess.PID = 4444
			return nil
		},
		respawnInPlaceFn: func(cfg *config.Config, slotName string, sess *state.Session, repo string, issue github.Issue, promptBase string, backend string) error {
			respawnedInPlace = true
			return nil
		},
	}

	pastTime := time.Now().UTC().Add(-1 * time.Second)
	s := state.NewState()
	// The restart controller already ran worker.Stop (removed the worktree)
	// and cleared the pointers: Worktree="" and PRNumber=0.
	s.Sessions["mae-12"] = &state.Session{
		IssueNumber: 112,
		IssueTitle:  "restarted worker",
		Status:      state.StatusDead,
		RetryCount:  1,
		NextRetryAt: &pastTime,
		Branch:      "feat/mae-12-112-test",
		Worktree:    "",
		PRNumber:    0,
	}

	o.respawnDueRetries(s, 1)

	if respawnedInPlace {
		t.Fatal("must NOT choose RespawnInPlace after the restart cleared the worktree")
	}
	if !respawnedFresh {
		t.Fatal("expected a fresh respawn once the stale worktree pointer was cleared")
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

	o.respawnDueRetries(s, 10)

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

	o.respawnDueRetries(s, 10)

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

func TestSyncProject_SkipsWhenGraphQLBudgetLow(t *testing.T) {
	cfg := &config.Config{
		Repo: "owner/repo",
		GitHubProjects: config.GitHubProjectsConfig{
			Enabled:       true,
			ProjectNumber: 3,
		},
	}
	calls := 0
	o := &Orchestrator{
		cfg: cfg,
		rateLimitFn: func() (github.RateLimitStatus, error) {
			return github.RateLimitStatus{
				GraphQL: github.RateLimitBucket{Limit: 5000, Remaining: 0, Used: 5000},
			}, nil
		},
		syncProjectFn: func(issueNumber int, status github.ProjectStatus) bool {
			calls++
			return true
		},
	}

	if o.syncProject(42, github.ProjectStatusInProgress) {
		t.Fatal("syncProject returned true, want false when GraphQL budget is depleted")
	}
	if calls != 0 {
		t.Fatalf("syncProjectFn calls = %d, want 0 when budget is depleted", calls)
	}
}

func TestSyncProject_MissingLifecycleStatusIsHandled(t *testing.T) {
	cfg := &config.Config{
		Repo: "owner/repo",
		GitHubProjects: config.GitHubProjectsConfig{
			Enabled:       true,
			ProjectNumber: 3,
		},
	}
	o := &Orchestrator{
		cfg: cfg,
		projectField: &github.ProjectField{
			ProjectID: "PVT_test",
			FieldID:   "FIELD_test",
			Options:   map[string]string{"Todo": "opt-todo", "In Progress": "opt-progress", "Done": "opt-done"},
		},
		rateLimitFn: func() (github.RateLimitStatus, error) {
			return github.RateLimitStatus{
				GraphQL: github.RateLimitBucket{Limit: 5000, Remaining: 5000},
			}, nil
		},
	}

	if !o.syncProject(42, github.ProjectStatusLiveVerify) {
		t.Fatal("syncProject should report handled when board lacks lifecycle status candidates")
	}
}

func TestSyncProject_MissingLifecycleStatusDoesNotCheckRateBudgetWhenFieldKnown(t *testing.T) {
	cfg := &config.Config{
		Repo: "owner/repo",
		GitHubProjects: config.GitHubProjectsConfig{
			Enabled:       true,
			ProjectNumber: 3,
		},
	}
	rateCalls := 0
	o := &Orchestrator{
		cfg: cfg,
		projectField: &github.ProjectField{
			ProjectID: "PVT_test",
			FieldID:   "FIELD_test",
			Options:   map[string]string{"Todo": "opt-todo", "In Progress": "opt-progress", "Done": "opt-done"},
		},
		rateLimitFn: func() (github.RateLimitStatus, error) {
			rateCalls++
			return github.RateLimitStatus{
				GraphQL: github.RateLimitBucket{Limit: 5000, Remaining: 0, Used: 5000},
			}, nil
		},
	}

	if !o.syncProject(42, github.ProjectStatusLiveVerify) {
		t.Fatal("syncProject should report handled when board lacks lifecycle status candidates")
	}
	if rateCalls != 0 {
		t.Fatalf("rateLimit calls = %d, want 0 for unsupported status with known project field", rateCalls)
	}
}

func TestReconcileSessionsToProjectBoard_SkipsAlreadySyncedStatus(t *testing.T) {
	cfg := &config.Config{
		Repo: "owner/repo",
		GitHubProjects: config.GitHubProjectsConfig{
			Enabled:       true,
			ProjectNumber: 3,
		},
	}
	calls := 0
	var lastStatus github.ProjectStatus
	o := &Orchestrator{
		cfg: cfg,
		rateLimitFn: func() (github.RateLimitStatus, error) {
			return github.RateLimitStatus{
				GraphQL: github.RateLimitBucket{Limit: 5000, Remaining: 5000},
			}, nil
		},
		syncProjectFn: func(issueNumber int, status github.ProjectStatus) bool {
			calls++
			lastStatus = status
			return true
		},
	}
	s := state.NewState()
	s.Sessions["slot-1"] = &state.Session{IssueNumber: 42, Status: state.StatusRunning}
	s.MarkProjectStatusSynced(42, string(github.ProjectStatusInProgress), time.Now().UTC())

	o.reconcileSessionsToProjectBoard(s)
	if calls != 0 {
		t.Fatalf("sync calls = %d, want 0 for already-synced status", calls)
	}

	s.Sessions["slot-1"].Status = state.StatusPROpen
	o.reconcileSessionsToProjectBoard(s)
	if calls != 1 {
		t.Fatalf("sync calls = %d, want 1 after status transition", calls)
	}
	if lastStatus != github.ProjectStatusInReview {
		t.Fatalf("last status = %q, want %q", lastStatus, github.ProjectStatusInReview)
	}
	if !s.ProjectStatusSynced(42, string(github.ProjectStatusInReview)) {
		t.Fatal("state did not record synced in_review status")
	}
}

func TestRunOncePersistsProjectStatusSyncAfterReconcile(t *testing.T) {
	cfg := &config.Config{
		Repo:              "owner/repo",
		StateDir:          t.TempDir(),
		MaxParallel:       1,
		MaxRuntimeMinutes: 60,
		GitHubProjects: config.GitHubProjectsConfig{
			Enabled:       true,
			ProjectNumber: 3,
		},
	}

	s := state.NewState()
	s.Sessions["slot-1"] = &state.Session{
		IssueNumber: 42,
		IssueTitle:  "Persist project sync",
		Status:      state.StatusRunning,
		PID:         1234,
		Branch:      "codex/persist-project-sync",
		TmuxSession: "maestro-slot-1",
		StartedAt:   time.Now().UTC(),
	}
	if err := state.Save(cfg.StateDir, s); err != nil {
		t.Fatalf("save state: %v", err)
	}

	syncCalls := 0
	o := &Orchestrator{
		cfg:      cfg,
		gh:       github.New(cfg.Repo),
		notifier: &notify.Notifier{},
		router:   router.New(cfg),
		repo:     cfg.Repo,
		projectField: &github.ProjectField{
			ProjectID: "PVT_test",
			FieldID:   "FIELD_test",
			Options:   map[string]string{"In Progress": "opt-progress"},
		},
		listOpenPRsFn: func() ([]github.PR, error) {
			return nil, nil
		},
		isIssueClosedFn: func(issueNumber int) (bool, error) {
			return false, nil
		},
		pidAliveFn: func(pid int) bool {
			return true
		},
		tmuxSessionExistsFn: func(name string) bool {
			return true
		},
		captureTmuxFn: func(session string) (string, error) {
			return "", nil
		},
		rateLimitFn: func() (github.RateLimitStatus, error) {
			return github.RateLimitStatus{
				GraphQL: github.RateLimitBucket{Limit: 5000, Remaining: 5000},
			}, nil
		},
		syncProjectFn: func(issueNumber int, status github.ProjectStatus) bool {
			if issueNumber != 42 {
				t.Fatalf("sync issue = %d, want 42", issueNumber)
			}
			if status != github.ProjectStatusInProgress {
				t.Fatalf("sync status = %q, want %q", status, github.ProjectStatusInProgress)
			}
			syncCalls++
			return true
		},
		listNonDoneProjectItemsFn: func(pf *github.ProjectField) ([]github.ProjectItem, error) {
			return nil, nil
		},
	}

	if err := o.RunOnce(); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if syncCalls != 1 {
		t.Fatalf("sync calls = %d, want 1", syncCalls)
	}
	loaded, err := state.Load(cfg.StateDir)
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	if !loaded.ProjectStatusSynced(42, string(github.ProjectStatusInProgress)) {
		t.Fatal("RunOnce did not persist reconciled project status sync")
	}

	if err := o.RunOnce(); err != nil {
		t.Fatalf("RunOnce second pass: %v", err)
	}
	if syncCalls != 1 {
		t.Fatalf("sync calls = %d, want 1 after cached second pass", syncCalls)
	}
}

func TestReconcileProjectBoard_DoesNotCheckRateLimitWhenSweepThrottled(t *testing.T) {
	cfg := &config.Config{
		Repo: "owner/repo",
		GitHubProjects: config.GitHubProjectsConfig{
			Enabled:       true,
			ProjectNumber: 3,
		},
	}

	rateCalls := 0
	o := &Orchestrator{
		cfg: cfg,
		projectField: &github.ProjectField{
			ProjectID: "PVT_test",
			FieldID:   "FIELD_test",
			Options:   map[string]string{"Todo": "opt-todo", "In Progress": "opt-progress", "Done": "opt-done"},
		},
		projectItemsSweepAt: time.Now().UTC(),
		rateLimitFn: func() (github.RateLimitStatus, error) {
			rateCalls++
			return github.RateLimitStatus{
				GraphQL: github.RateLimitBucket{Limit: 5000, Remaining: 5000},
			}, nil
		},
		listNonDoneProjectItemsFn: func(pf *github.ProjectField) ([]github.ProjectItem, error) {
			t.Fatal("throttled reconcile should not sweep project items")
			return nil, nil
		},
	}

	if o.reconcileProjectBoard(state.NewState()) {
		t.Fatal("throttled reconcile without session transitions should be unchanged")
	}
	if rateCalls != 0 {
		t.Fatalf("rateLimit calls = %d, want 0 while sweep is throttled", rateCalls)
	}
}

func TestReconcileProjectBoard_ThrottlesNonDoneItemSweep(t *testing.T) {
	cfg := &config.Config{
		Repo: "owner/repo",
		GitHubProjects: config.GitHubProjectsConfig{
			Enabled:       true,
			ProjectNumber: 3,
		},
	}

	sweeps := 0
	synced := make(map[int]github.ProjectStatus)
	o := &Orchestrator{
		cfg: cfg,
		projectField: &github.ProjectField{
			ProjectID: "PVT_test",
			FieldID:   "FIELD_test",
			Options:   map[string]string{"Todo": "opt-todo", "In Progress": "opt-progress"},
		},
		rateLimitFn: func() (github.RateLimitStatus, error) {
			return github.RateLimitStatus{
				GraphQL: github.RateLimitBucket{Limit: 5000, Remaining: 5000},
			}, nil
		},
		syncProjectFn: func(issueNumber int, status github.ProjectStatus) bool {
			synced[issueNumber] = status
			return true
		},
		listNonDoneProjectItemsFn: func(pf *github.ProjectField) ([]github.ProjectItem, error) {
			sweeps++
			return []github.ProjectItem{{IssueNumber: 99, HasStatus: false}}, nil
		},
	}

	s := state.NewState()
	if !o.reconcileProjectBoard(s) {
		t.Fatal("first reconcile should record no-status project item sync")
	}
	if sweeps != 1 {
		t.Fatalf("sweeps = %d, want 1", sweeps)
	}
	if synced[99] != github.ProjectStatusTodo {
		t.Fatalf("issue #99 status = %q, want %q", synced[99], github.ProjectStatusTodo)
	}

	if o.reconcileProjectBoard(s) {
		t.Fatal("second reconcile should be a no-op while project item sweep is throttled")
	}
	if sweeps != 1 {
		t.Fatalf("sweeps = %d, want 1 while throttled", sweeps)
	}

	s.Sessions["slot-1"] = &state.Session{IssueNumber: 42, Status: state.StatusRunning}
	if !o.reconcileProjectBoard(s) {
		t.Fatal("session transition sync should still run while item sweep is throttled")
	}
	if sweeps != 1 {
		t.Fatalf("sweeps = %d, want 1 after transition-only sync", sweeps)
	}
	if synced[42] != github.ProjectStatusInProgress {
		t.Fatalf("issue #42 status = %q, want %q", synced[42], github.ProjectStatusInProgress)
	}
}

// A CLOSED issue sitting in NO STATUS must move to Done on the next reconcile
// sweep — not get bounced to Todo by the no-status branch — and must work for
// externally-closed issues the reconcile never transitioned itself. This is the
// core regression guard for #741.
func TestReconcileProjectBoard_ClosedNoStatusItemMovesToDone(t *testing.T) {
	cfg := &config.Config{
		Repo: "owner/repo",
		GitHubProjects: config.GitHubProjectsConfig{
			Enabled:       true,
			ProjectNumber: 5,
		},
	}

	synced := make(map[int]github.ProjectStatus)
	o := &Orchestrator{
		cfg: cfg,
		projectField: &github.ProjectField{
			ProjectID: "PVT_test",
			FieldID:   "FIELD_test",
			Options:   map[string]string{"Todo": "opt-todo", "In Progress": "opt-progress", "Done": "opt-done"},
		},
		rateLimitFn: func() (github.RateLimitStatus, error) {
			return github.RateLimitStatus{
				GraphQL: github.RateLimitBucket{Limit: 5000, Remaining: 5000},
			}, nil
		},
		syncProjectFn: func(issueNumber int, status github.ProjectStatus) bool {
			synced[issueNumber] = status
			return true
		},
		listNonDoneProjectItemsFn: func(pf *github.ProjectField) ([]github.ProjectItem, error) {
			return []github.ProjectItem{
				// Externally-closed issue that never had a board Status.
				{IssueNumber: 457, IssueClosed: true, HasStatus: false},
				// Closed issue parked in a non-terminal column.
				{IssueNumber: 458, IssueClosed: true, HasStatus: true},
				// Open no-status item still belongs in the backlog (Todo).
				{IssueNumber: 459, IssueClosed: false, HasStatus: false},
			}, nil
		},
	}

	s := state.NewState()
	if !o.reconcileProjectBoard(s) {
		t.Fatal("reconcile should report changes for closed/no-status drift")
	}
	if synced[457] != github.ProjectStatusDone {
		t.Fatalf("closed no-status issue #457 status = %q, want %q", synced[457], github.ProjectStatusDone)
	}
	if synced[458] != github.ProjectStatusDone {
		t.Fatalf("closed in-progress issue #458 status = %q, want %q", synced[458], github.ProjectStatusDone)
	}
	if synced[459] != github.ProjectStatusTodo {
		t.Fatalf("open no-status issue #459 status = %q, want %q", synced[459], github.ProjectStatusTodo)
	}
	if !s.ProjectStatusSynced(457, string(github.ProjectStatusDone)) {
		t.Fatal("reconcile did not persist Done sync for closed no-status issue #457")
	}
}

func TestDeferProjectBoardSweepDelaysOnlyBroadSweep(t *testing.T) {
	o := &Orchestrator{}
	now := time.Now().UTC()

	if !o.projectBoardSweepDue(now) {
		t.Fatal("zero-value orchestrator should allow initial sweep")
	}

	o.deferProjectBoardSweep(now)
	if o.projectBoardSweepDue(now.Add(projectBoardSweepInterval - time.Second)) {
		t.Fatal("deferred startup sweep should wait for throttle interval")
	}
	if !o.projectBoardSweepDue(now.Add(projectBoardSweepInterval + time.Second)) {
		t.Fatal("deferred startup sweep should become due after throttle interval")
	}

	first := o.projectItemsSweepAt
	o.deferProjectBoardSweep(now.Add(time.Hour))
	if !o.projectItemsSweepAt.Equal(first) {
		t.Fatal("deferProjectBoardSweep should not overwrite an existing sweep timestamp")
	}
}

func TestProjectStatusForSession_MirrorsRuntime(t *testing.T) {
	soon := time.Now().UTC().Add(30 * time.Second)
	deployed := time.Now().UTC()

	tests := []struct {
		name           string
		sess           *state.Session
		requiresDeploy bool
		want           github.ProjectStatus
		wantOK         bool
	}{
		{name: "nil session is no-op", want: "", wantOK: false},
		{name: "running -> in_progress", sess: &state.Session{IssueNumber: 1, Status: state.StatusRunning}, want: github.ProjectStatusInProgress, wantOK: true},
		{name: "queued -> in_review", sess: &state.Session{IssueNumber: 2, Status: state.StatusQueued}, want: github.ProjectStatusInReview, wantOK: true},
		{name: "pr_open -> in_review", sess: &state.Session{IssueNumber: 3, Status: state.StatusPROpen, PRNumber: 9}, want: github.ProjectStatusInReview, wantOK: true},
		{name: "code_landed -> live_verification without deploy", sess: &state.Session{IssueNumber: 4, Status: state.StatusCodeLanded, PRNumber: 10}, want: github.ProjectStatusLiveVerify, wantOK: true},
		{name: "code_landed -> deploying when deploy required", sess: &state.Session{IssueNumber: 4, Status: state.StatusCodeLanded, PRNumber: 10}, requiresDeploy: true, want: github.ProjectStatusDeploying, wantOK: true},
		{name: "code_landed -> live_verification after deploy succeeds", sess: &state.Session{IssueNumber: 4, Status: state.StatusCodeLanded, PRNumber: 10, DeploymentFinishedAt: &deployed}, requiresDeploy: true, want: github.ProjectStatusLiveVerify, wantOK: true},
		{name: "done -> done", sess: &state.Session{IssueNumber: 5, Status: state.StatusDone}, want: github.ProjectStatusDone, wantOK: true},
		{name: "retry_exhausted -> blocked", sess: &state.Session{IssueNumber: 6, Status: state.StatusRetryExhausted}, want: github.ProjectStatusBlocked, wantOK: true},
		{name: "conflict_failed -> blocked", sess: &state.Session{IssueNumber: 7, Status: state.StatusConflictFailed}, want: github.ProjectStatusBlocked, wantOK: true},
		{name: "failed -> blocked", sess: &state.Session{IssueNumber: 8, Status: state.StatusFailed}, want: github.ProjectStatusBlocked, wantOK: true},
		{name: "failed+released -> todo", sess: &state.Session{IssueNumber: 8, Status: state.StatusFailed, ReleasedForRedispatch: true}, want: github.ProjectStatusTodo, wantOK: true},
		{name: "dead awaiting retry stays in_progress", sess: &state.Session{IssueNumber: 9, Status: state.StatusDead, NextRetryAt: &soon}, want: github.ProjectStatusInProgress, wantOK: true},
		{name: "dead without retry -> blocked", sess: &state.Session{IssueNumber: 10, Status: state.StatusDead}, want: github.ProjectStatusBlocked, wantOK: true},
		{name: "unknown status returns no mapping", sess: &state.Session{IssueNumber: 11, Status: state.SessionStatus("weird")}, want: "", wantOK: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := projectStatusForSession(tc.sess, tc.requiresDeploy)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if got != tc.want {
				t.Errorf("status = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestSessionRecency_PicksFreshestSignal(t *testing.T) {
	now := time.Now().UTC()
	finished := now.Add(-time.Minute)
	older := now.Add(-time.Hour)

	sess := &state.Session{
		StartedAt:           older,
		LastOutputChangedAt: now,
		FinishedAt:          &finished,
	}

	if got := sessionRecency(sess); !got.Equal(now) {
		t.Fatalf("sessionRecency = %v, want freshest signal %v", got, now)
	}
	if got := sessionRecency(nil); !got.IsZero() {
		t.Fatalf("sessionRecency(nil) = %v, want zero", got)
	}
}

// --- CI failure retry tests (#226) ---

// newCIFailureRetryOrchestrator creates an Orchestrator wired with test fakes
// for testing the CI failure auto-retry flow in autoMergePRs.
func newCIFailureRetryOrchestrator(cfg *config.Config, prs []github.PR, ciStatuses map[int]string) (*Orchestrator, *[]int, *[]int) {
	merged := make([]int, 0)
	closedPRs := make([]int, 0)
	return &Orchestrator{
		cfg:      cfg,
		notifier: &notify.Notifier{},
		listOpenPRsFn: func() ([]github.PR, error) {
			return prs, nil
		},
		ghPRCIStatusFn: func(prNumber int) (string, error) {
			if s, ok := ciStatuses[prNumber]; ok {
				return s, nil
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
		ghClosePRFn: func(prNumber int, comment string) error {
			closedPRs = append(closedPRs, prNumber)
			return nil
		},
		ghPRChecksOutputFn: func(prNumber int) (string, error) {
			return fmt.Sprintf("CI checks failed for PR #%d", prNumber), nil
		},
		ghCloseIssueFn: func(number int, comment string) error {
			return nil
		},
		addIssueLabelFn: func(number int, label string) error {
			return nil
		},
		workerStopFn: func(cfg *config.Config, slotName string, sess *state.Session) error {
			return nil
		},
		ghCollectPRReviewFeedbackFn: func(prNumber int) (string, error) {
			return "", nil
		},
	}, &merged, &closedPRs
}

func TestAutoMergePRs_CIFailure_KeepsCanonicalPRAndSchedulesInPlaceRetry(t *testing.T) {
	prs := []github.PR{
		{Number: 10, HeadRefName: "feat/a"},
	}
	cfg := &config.Config{Repo: "owner/repo", MergeStrategy: "parallel", MaxRetriesPerIssue: 3, MaxRetryBackoffMs: 300000}
	o, merged, closedPRs := newCIFailureRetryOrchestrator(cfg, prs, map[int]string{10: "failure"})
	s := makeTestState(prs)

	o.autoMergePRs(s)

	// Should not merge
	if len(*merged) != 0 {
		t.Fatalf("expected 0 merges, got %d", len(*merged))
	}

	// A failed check must not destroy canonical PR identity.
	if len(*closedPRs) != 0 {
		t.Fatalf("closedPRs = %v, want none for in-place retry", *closedPRs)
	}

	// Session should be dead with NextRetryAt scheduled
	sess := s.Sessions["slot-0"]
	if sess.Status != state.StatusDead {
		t.Fatalf("status = %q, want %q", sess.Status, state.StatusDead)
	}
	if sess.NextRetryAt == nil {
		t.Fatal("NextRetryAt should be set for CI failure retry")
	}
	if sess.RetryCount != 1 {
		t.Fatalf("RetryCount = %d, want 1", sess.RetryCount)
	}
	if sess.PRNumber != 10 {
		t.Fatalf("PRNumber = %d, want 10 (same PR retained)", sess.PRNumber)
	}
	if sess.CIFailureOutput == "" {
		t.Fatal("CIFailureOutput should be set")
	}
	if sess.FinishedAt == nil {
		t.Fatal("FinishedAt should be set")
	}
}

func TestAutoMergePRs_SharedPRFailureMutatesOnlyNewestContinuation(t *testing.T) {
	prs := []github.PR{{Number: 388, HeadRefName: "feat/shared-pr"}}
	cfg := &config.Config{Repo: "owner/repo", MergeStrategy: "parallel", MaxRetriesPerIssue: 3, MaxRetryBackoffMs: 300000}
	o, _, closedPRs := newCIFailureRetryOrchestrator(cfg, prs, map[int]string{388: "failure"})
	s := state.NewState()
	s.Sessions["ok-player-273"] = &state.Session{
		IssueNumber: 345, Status: state.StatusPROpen, PRNumber: 388, Branch: "feat/shared-pr",
		StartedAt: time.Date(2026, 7, 17, 23, 6, 0, 0, time.UTC), RetryCount: 3,
	}
	s.Sessions["ok-player-302"] = &state.Session{
		IssueNumber: 406, Status: state.StatusPROpen, PRNumber: 388, Branch: "feat/shared-pr",
		StartedAt: time.Date(2026, 7, 18, 4, 57, 43, 0, time.UTC),
	}

	o.autoMergePRs(s)

	historical := s.Sessions["ok-player-273"]
	if historical.Status != state.StatusPROpen || historical.LastNotifiedStatus != "" {
		t.Fatalf("historical session mutated by shared PR failure: %+v", historical)
	}
	canonical := s.Sessions["ok-player-302"]
	if canonical.Status != state.StatusDead || canonical.NextRetryAt == nil || canonical.RetryCount != 1 {
		t.Fatalf("canonical continuation = %+v, want one scheduled in-place retry", canonical)
	}
	if canonical.PRNumber != 388 || len(*closedPRs) != 0 {
		t.Fatalf("canonical identity changed: pr=%d closed=%v", canonical.PRNumber, *closedPRs)
	}
}

func TestAutoMergePRs_SharedPRLiveContinuationBlocksHistoricalGateMutation(t *testing.T) {
	prs := []github.PR{{Number: 388, HeadRefName: "feat/shared-pr"}}
	cfg := &config.Config{Repo: "owner/repo", MergeStrategy: "parallel", MaxRetriesPerIssue: 3, MaxRetryBackoffMs: 300000}
	o, _, closedPRs := newCIFailureRetryOrchestrator(cfg, prs, map[int]string{388: "failure"})
	s := state.NewState()
	s.Sessions["ok-player-273"] = &state.Session{
		IssueNumber: 345, Status: state.StatusPROpen, PRNumber: 388, Branch: "feat/shared-pr",
		StartedAt: time.Date(2026, 7, 17, 23, 6, 0, 0, time.UTC), RetryCount: 3,
	}
	s.Sessions["ok-player-302"] = &state.Session{
		IssueNumber: 406, Status: state.StatusRunning, PRNumber: 388, Branch: "feat/shared-pr", PID: 12345,
		StartedAt: time.Date(2026, 7, 18, 4, 57, 43, 0, time.UTC),
	}

	o.autoMergePRs(s)

	if got := s.Sessions["ok-player-273"]; got.Status != state.StatusPROpen || got.LastNotifiedStatus != "" {
		t.Fatalf("historical session mutated while canonical repair is live: %+v", got)
	}
	if got := s.Sessions["ok-player-302"]; got.Status != state.StatusRunning || got.RetryCount != 0 {
		t.Fatalf("live canonical continuation mutated by merge flow: %+v", got)
	}
	if len(*closedPRs) != 0 {
		t.Fatalf("shared PR closed while canonical repair is live: %v", *closedPRs)
	}
}

func TestAutoMergePRs_CIFailure_RetryLimitExhausted_NoRetry(t *testing.T) {
	prs := []github.PR{
		{Number: 10, HeadRefName: "feat/a"},
	}
	cfg := &config.Config{Repo: "owner/repo", MergeStrategy: "parallel", MaxRetriesPerIssue: 2, MaxRetryBackoffMs: 300000}
	o, _, closedPRs := newCIFailureRetryOrchestrator(cfg, prs, map[int]string{10: "failure"})
	notifier := notify.NewWithToken("", "123", "", "")
	notifier.SetDigestMode(true)
	o.notifier = notifier
	s := makeTestState(prs)

	// Simulate 2 prior failed attempts for this issue
	now := time.Now().UTC()
	for i := 0; i < 2; i++ {
		slotName := fmt.Sprintf("old-%d", i)
		finished := now.Add(-time.Duration(2-i) * time.Hour)
		s.Sessions[slotName] = &state.Session{
			IssueNumber: 100,
			Status:      state.StatusDead,
			PRNumber:    0,
			StartedAt:   finished.Add(-30 * time.Minute),
			FinishedAt:  &finished,
		}
	}

	o.autoMergePRs(s)

	// PR should NOT be closed (retry limit exhausted — leave PR open for manual review)
	if len(*closedPRs) != 0 {
		t.Fatalf("closedPRs = %v, want empty (retry limit exhausted)", *closedPRs)
	}

	// Session should be terminal, but keep the PR open for manual review/merge.
	sess := s.Sessions["slot-0"]
	if sess.Status != state.StatusRetryExhausted {
		t.Fatalf("status = %q, want %q (retry limit exhausted, PR stays open)", sess.Status, state.StatusRetryExhausted)
	}
	if sess.PRNumber != 10 {
		t.Fatalf("PRNumber = %d, want 10 (retry-exhausted PR remains open)", sess.PRNumber)
	}
	if sess.NextRetryAt != nil {
		t.Fatal("NextRetryAt should be nil after retry exhaustion")
	}
	if sess.LastNotifiedStatus != "ci_retry_exhausted" {
		t.Fatalf("LastNotifiedStatus = %q, want ci_retry_exhausted", sess.LastNotifiedStatus)
	}
	if notifier.Buffered() != 1 {
		t.Fatalf("notifications buffered = %d, want 1", notifier.Buffered())
	}

	o.autoMergePRs(s)

	if len(*closedPRs) != 0 {
		t.Fatalf("closedPRs after duplicate cycle = %v, want empty", *closedPRs)
	}
	if notifier.Buffered() != 1 {
		t.Fatalf("duplicate terminal notification buffered; got %d, want 1", notifier.Buffered())
	}
}

func TestAutoMergePRs_CIFailure_UnlimitedRetries(t *testing.T) {
	prs := []github.PR{
		{Number: 10, HeadRefName: "feat/a"},
	}
	cfg := &config.Config{Repo: "owner/repo", MergeStrategy: "parallel", MaxRetriesPerIssue: 0, MaxRetryBackoffMs: 300000} // 0 = unlimited
	o, _, closedPRs := newCIFailureRetryOrchestrator(cfg, prs, map[int]string{10: "failure"})
	s := makeTestState(prs)

	// Set high retry count — should still retry because unlimited
	s.Sessions["slot-0"].RetryCount = 10

	o.autoMergePRs(s)

	// Should preserve the PR and schedule an in-place retry.
	if len(*closedPRs) != 0 {
		t.Fatalf("closedPRs = %v, want none (unlimited in-place retries)", *closedPRs)
	}
	sess := s.Sessions["slot-0"]
	if sess.Status != state.StatusDead {
		t.Fatalf("status = %q, want %q", sess.Status, state.StatusDead)
	}
	if sess.RetryCount != 11 {
		t.Fatalf("RetryCount = %d, want 11", sess.RetryCount)
	}
}

func TestAutoMergePRs_CIFailure_DoesNotDependOnClosePR(t *testing.T) {
	prs := []github.PR{
		{Number: 10, HeadRefName: "feat/a"},
	}
	cfg := &config.Config{Repo: "owner/repo", MergeStrategy: "parallel", MaxRetriesPerIssue: 3, MaxRetryBackoffMs: 300000}
	o := &Orchestrator{
		cfg:      cfg,
		notifier: &notify.Notifier{},
		listOpenPRsFn: func() ([]github.PR, error) {
			return prs, nil
		},
		ghPRCIStatusFn: func(prNumber int) (string, error) {
			return "failure", nil
		},
		ghPRGreptileApprovedFn: func(prNumber int) (bool, bool, error) {
			return true, false, nil
		},
		ghMergePRFn: func(prNumber int) error {
			return nil
		},
		ghClosePRFn: func(prNumber int, comment string) error {
			return fmt.Errorf("network error")
		},
		ghPRChecksOutputFn: func(prNumber int) (string, error) {
			return "some CI output", nil
		},
		ghCollectPRReviewFeedbackFn: func(prNumber int) (string, error) {
			return "", nil
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

	// closePR is deliberately absent; the in-place retry must still schedule.
	sess := s.Sessions["slot-0"]
	if sess.Status != state.StatusDead {
		t.Fatalf("status = %q, want %q (closePR is not part of retry)", sess.Status, state.StatusDead)
	}
	if sess.NextRetryAt == nil {
		t.Fatal("NextRetryAt should be set without closing the PR")
	}
	if sess.PRNumber != 10 {
		t.Fatalf("PRNumber = %d, want canonical PR 10 retained", sess.PRNumber)
	}
}

func TestAutoMergePRs_CIFailure_AlreadyNotified_NoDoubleRetry(t *testing.T) {
	prs := []github.PR{
		{Number: 10, HeadRefName: "feat/a"},
	}
	cfg := &config.Config{Repo: "owner/repo", MergeStrategy: "parallel", MaxRetriesPerIssue: 3, MaxRetryBackoffMs: 300000}
	closedPRs := make([]int, 0)
	o := &Orchestrator{
		cfg:      cfg,
		notifier: &notify.Notifier{},
		listOpenPRsFn: func() ([]github.PR, error) {
			return prs, nil
		},
		ghPRCIStatusFn: func(prNumber int) (string, error) {
			return "failure", nil
		},
		ghPRGreptileApprovedFn: func(prNumber int) (bool, bool, error) {
			return true, false, nil
		},
		ghMergePRFn: func(prNumber int) error {
			return nil
		},
		ghClosePRFn: func(prNumber int, comment string) error {
			closedPRs = append(closedPRs, prNumber)
			return nil
		},
		ghPRChecksOutputFn: func(prNumber int) (string, error) {
			return "CI output", nil
		},
		ghCollectPRReviewFeedbackFn: func(prNumber int) (string, error) {
			return "", nil
		},
		ghCloseIssueFn: func(number int, comment string) error {
			return nil
		},
		workerStopFn: func(cfg *config.Config, slotName string, sess *state.Session) error {
			return nil
		},
	}
	s := makeTestState(prs)

	// Mark as already notified — should not trigger another retry
	s.Sessions["slot-0"].LastNotifiedStatus = "ci_failure"
	s.Sessions["slot-0"].NotifiedCIFail = true

	o.autoMergePRs(s)

	if len(closedPRs) != 0 {
		t.Fatalf("closedPRs = %v, want empty (already notified, no double retry)", closedPRs)
	}
}

func TestCanRetryIssue_RespectsMaxRetries(t *testing.T) {
	cfg := &config.Config{Repo: "owner/repo", MaxRetriesPerIssue: 3}
	o := &Orchestrator{cfg: cfg}
	s := state.NewState()

	sess := &state.Session{IssueNumber: 42, RetryCount: 0}
	if !o.canRetryIssue(s, sess) {
		t.Error("canRetryIssue should return true when retry count is 0")
	}

	sess.RetryCount = 2
	if !o.canRetryIssue(s, sess) {
		t.Error("canRetryIssue should return true when retry count < max")
	}

	sess.RetryCount = 3
	if o.canRetryIssue(s, sess) {
		t.Error("canRetryIssue should return false when retry count >= max")
	}
}

func TestCanRetryIssue_UnlimitedWhenZero(t *testing.T) {
	cfg := &config.Config{Repo: "owner/repo", MaxRetriesPerIssue: 0}
	o := &Orchestrator{cfg: cfg}
	s := state.NewState()

	sess := &state.Session{IssueNumber: 42, RetryCount: 100}
	if !o.canRetryIssue(s, sess) {
		t.Error("canRetryIssue should return true when MaxRetriesPerIssue is 0 (unlimited)")
	}
}

func TestCanRetryIssue_CountsFailedAttempts(t *testing.T) {
	cfg := &config.Config{Repo: "owner/repo", MaxRetriesPerIssue: 3}
	o := &Orchestrator{cfg: cfg}
	s := state.NewState()

	// Add 2 prior failed attempts (no PR)
	now := time.Now().UTC()
	for i := 0; i < 2; i++ {
		finished := now.Add(-time.Duration(2-i) * time.Hour)
		s.Sessions[fmt.Sprintf("old-%d", i)] = &state.Session{
			IssueNumber: 42,
			Status:      state.StatusDead,
			PRNumber:    0,
			FinishedAt:  &finished,
		}
	}

	sess := &state.Session{IssueNumber: 42, RetryCount: 0}
	if !o.canRetryIssue(s, sess) {
		t.Error("canRetryIssue should return true (2 failed + 0 retries < 3)")
	}

	sess.RetryCount = 1
	if o.canRetryIssue(s, sess) {
		t.Error("canRetryIssue should return false (2 failed + 1 retry >= 3)")
	}
}

func TestAppendCIFailureContext_AddsSection(t *testing.T) {
	base := "You are a coding agent."
	output := "Build failed: exit code 1\nError in main.go:42"
	result := appendCIFailureContext(base, output, 2)

	if !strings.Contains(result, "You are a coding agent.") {
		t.Error("result should contain original prompt base")
	}
	if !strings.Contains(result, "Previous CI Failure (Attempt 2)") {
		t.Error("result should contain CI failure header with attempt number")
	}
	if !strings.Contains(result, "Build failed: exit code 1") {
		t.Error("result should contain CI output")
	}
	if !strings.Contains(result, "Error in main.go:42") {
		t.Error("result should contain full CI output")
	}
}

func TestRespawnDueRetries_CIFailureContext_IncludedInPrompt(t *testing.T) {
	cfg := &config.Config{
		Repo:               "owner/repo",
		MaxRetriesPerIssue: 3,
		MaxRetryBackoffMs:  300000,
		Model: config.ModelConfig{
			Default:  "claude",
			Backends: map[string]config.BackendDef{"claude": {Cmd: "claude"}},
		},
	}

	respawnedPrompt := ""
	o := &Orchestrator{
		cfg:        cfg,
		notifier:   &notify.Notifier{},
		promptBase: "base prompt {{ISSUE_NUMBER}}",
		getIssueFn: func(number int) (github.Issue, error) {
			return github.Issue{Number: 42, Title: "test issue", Body: "fix this"}, nil
		},
		respawnWorkerFn: func(cfg *config.Config, slotName string, sess *state.Session, repo string, issue github.Issue, promptBase string, backendName string) error {
			respawnedPrompt = promptBase
			return nil
		},
	}

	s := state.NewState()
	retryAt := time.Now().UTC().Add(-1 * time.Minute) // backoff elapsed
	s.Sessions["slot-1"] = &state.Session{
		IssueNumber:     42,
		IssueTitle:      "test issue",
		Status:          state.StatusDead,
		RetryCount:      1,
		NextRetryAt:     &retryAt,
		Backend:         "claude",
		CIFailureOutput: "tests failed: FAIL main_test.go:15",
	}

	o.respawnDueRetries(s, 10)

	if respawnedPrompt == "" {
		t.Fatal("respawnWorkerFn should have been called")
	}
	if !strings.Contains(respawnedPrompt, "Previous CI Failure") {
		t.Error("prompt should contain CI failure context section")
	}
	if !strings.Contains(respawnedPrompt, "tests failed: FAIL main_test.go:15") {
		t.Error("prompt should contain actual CI output")
	}

	// CIFailureOutput should be consumed (cleared)
	sess := s.Sessions["slot-1"]
	if sess.CIFailureOutput != "" {
		t.Errorf("CIFailureOutput should be cleared after consumption, got %q", sess.CIFailureOutput)
	}
}

func TestRespawnDueRetries_NoCIContext_NormalPrompt(t *testing.T) {
	cfg := &config.Config{
		Repo:               "owner/repo",
		MaxRetriesPerIssue: 3,
		MaxRetryBackoffMs:  300000,
		Model: config.ModelConfig{
			Default:  "claude",
			Backends: map[string]config.BackendDef{"claude": {Cmd: "claude"}},
		},
	}

	respawnedPrompt := ""
	o := &Orchestrator{
		cfg:        cfg,
		notifier:   &notify.Notifier{},
		promptBase: "base prompt",
		getIssueFn: func(number int) (github.Issue, error) {
			return github.Issue{Number: 42, Title: "test issue"}, nil
		},
		respawnWorkerFn: func(cfg *config.Config, slotName string, sess *state.Session, repo string, issue github.Issue, promptBase string, backendName string) error {
			respawnedPrompt = promptBase
			return nil
		},
	}

	s := state.NewState()
	retryAt := time.Now().UTC().Add(-1 * time.Minute)
	s.Sessions["slot-1"] = &state.Session{
		IssueNumber: 42,
		IssueTitle:  "test issue",
		Status:      state.StatusDead,
		RetryCount:  1,
		NextRetryAt: &retryAt,
		Backend:     "claude",
		// No CIFailureOutput — normal dead worker retry
	}

	o.respawnDueRetries(s, 10)

	if respawnedPrompt == "" {
		t.Fatal("respawnWorkerFn should have been called")
	}
	if strings.Contains(respawnedPrompt, "Previous CI Failure") {
		t.Error("prompt should NOT contain CI failure context for non-CI retries")
	}
}

func TestCheckSessions_SoftThreshold_CheckpointAndRespawn(t *testing.T) {
	softThreshold := 0.8
	cfg := &config.Config{
		Repo:                     "owner/repo",
		WorkerMaxTokens:          100000,
		WorkerSoftTokenThreshold: &softThreshold,
		MaxRuntimeMinutes:        999,
	}
	// Worker output: 85,000 tokens — above 80% soft threshold (80,000) but below hard limit (100,000)
	stopped := make([]string, 0)
	checkpointed := make([]string, 0)
	respawnedInPlace := make([]string, 0)

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
			return "tokens 85000 (in 25000 / out 60000)", nil
		},
		workerStopFn: func(cfg *config.Config, slotName string, sess *state.Session) error {
			stopped = append(stopped, slotName)
			return nil
		},
		saveCheckpointFn: func(sess *state.Session) (string, error) {
			checkpointed = append(checkpointed, fmt.Sprintf("issue-%d", sess.IssueNumber))
			return "/tmp/CHECKPOINT.md", nil
		},
		getIssueFn: func(number int) (github.Issue, error) {
			return github.Issue{Number: number, Title: "test issue"}, nil
		},
		respawnInPlaceFn: func(cfg *config.Config, slotName string, sess *state.Session, repo string, issue github.Issue, promptBase string, backendName string) error {
			respawnedInPlace = append(respawnedInPlace, slotName)
			sess.Status = state.StatusRunning
			sess.TokensUsedAttempt = 0
			sess.TokenBudgetTokensAttempt = 0
			return nil
		},
	}

	s := state.NewState()
	s.Sessions["mae-1"] = &state.Session{
		IssueNumber: 42,
		Status:      state.StatusRunning,
		PID:         1234,
		TmuxSession: "maestro-mae-1",
		Branch:      "feat/mae-1-42-test",
		StartedAt:   time.Now().Add(-30 * time.Minute),
		Backend:     "claude",
	}

	o.checkSessions(s)

	sess := s.Sessions["mae-1"]
	if sess.Status != state.StatusRunning {
		t.Fatalf("status = %q, want %q (should still be running after checkpoint respawn)", sess.Status, state.StatusRunning)
	}
	if sess.CheckpointFile != "/tmp/CHECKPOINT.md" {
		t.Fatalf("checkpoint_file = %q, want /tmp/CHECKPOINT.md", sess.CheckpointFile)
	}
	if len(checkpointed) != 1 {
		t.Fatalf("checkpointed = %v, want 1 call", checkpointed)
	}
	if len(respawnedInPlace) != 1 || respawnedInPlace[0] != "mae-1" {
		t.Fatalf("respawnedInPlace = %v, want [mae-1]", respawnedInPlace)
	}
	// Worker should NOT be stopped (respawnInPlace handles the stop internally)
	if len(stopped) != 0 {
		t.Fatalf("stopped = %v, want empty (respawnInPlace handles stop)", stopped)
	}
}

// TestCheckSessions_SoftThreshold_CheckpointRespawnKeepsTierOverride is the #792
// greptile checkpoint regression guard: a policy-routed session that hits the
// soft-token checkpoint must respawn with its tier effort/model re-applied. Before
// the fix the checkpoint path passed the base o.cfg, so RespawnInPlace saw empty
// tier fields and the resumed worker ran the rest of the session off-tier.
func TestCheckSessions_SoftThreshold_CheckpointRespawnKeepsTierOverride(t *testing.T) {
	softThreshold := 0.8
	cfg := &config.Config{
		Repo:                     "owner/repo",
		WorkerMaxTokens:          100000,
		WorkerSoftTokenThreshold: &softThreshold,
		MaxRuntimeMinutes:        999,
		Model: config.ModelConfig{
			Default:  "claude",
			Backends: map[string]config.BackendDef{"claude": {Cmd: "claude"}},
		},
	}

	var respawnCfg *config.Config
	respawned := 0
	o := &Orchestrator{
		cfg:             cfg,
		notifier:        &notify.Notifier{},
		listOpenPRsFn:   func() ([]github.PR, error) { return []github.PR{}, nil },
		isIssueClosedFn: func(int) (bool, error) { return false, nil },
		pidAliveFn:      func(int) bool { return true },
		captureTmuxFn:   func(string) (string, error) { return "tokens 85000 (in 25000 / out 60000)", nil },
		workerStopFn:    func(*config.Config, string, *state.Session) error { return nil },
		saveCheckpointFn: func(sess *state.Session) (string, error) {
			return "/tmp/CHECKPOINT.md", nil
		},
		getIssueFn: func(number int) (github.Issue, error) {
			return github.Issue{Number: number, Title: "test issue"}, nil
		},
		respawnInPlaceFn: func(c *config.Config, slotName string, sess *state.Session, repo string, issue github.Issue, promptBase string, backendName string) error {
			respawnCfg = c
			respawned++
			sess.Status = state.StatusRunning
			sess.TokensUsedAttempt = 0
			sess.TokenBudgetTokensAttempt = 0
			return nil
		},
	}

	s := state.NewState()
	s.Sessions["mae-1"] = &state.Session{
		IssueNumber: 42,
		Status:      state.StatusRunning,
		PID:         1234,
		TmuxSession: "maestro-mae-1",
		Branch:      "feat/mae-1-42-test",
		StartedAt:   time.Now().Add(-30 * time.Minute),
		Backend:     "claude",
		// Policy tier override recorded on the durable audit record.
		BackendSelection: &state.BackendSelection{Tier: "strong", Effort: "high", Model: "opus-4.8"},
	}

	o.checkSessions(s)

	if respawned != 1 || respawnCfg == nil {
		t.Fatalf("respawned = %d (cfg nil = %v), want 1 with a config", respawned, respawnCfg == nil)
	}
	if got := respawnCfg.Model.Backends["claude"].TierEffort; got != "high" {
		t.Fatalf("checkpoint respawn TierEffort = %q, want high (override dropped)", got)
	}
	if got := respawnCfg.Model.Backends["claude"].TierModel; got != "opus-4.8" {
		t.Fatalf("checkpoint respawn TierModel = %q, want opus-4.8 (override dropped)", got)
	}
	// Base config must be untouched — the override lives on the respawn clone only.
	if got := cfg.Model.Backends["claude"].TierEffort; got != "" {
		t.Fatalf("base config mutated: claude TierEffort = %q, want empty", got)
	}
}

func TestCheckSessions_SoftThreshold_AlreadyCheckpointed_NoRepeat(t *testing.T) {
	softThreshold := 0.8
	cfg := &config.Config{
		Repo:                     "owner/repo",
		WorkerMaxTokens:          100000,
		WorkerSoftTokenThreshold: &softThreshold,
		MaxRuntimeMinutes:        999,
	}
	checkpointed := make([]string, 0)
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
			return "tokens 90000 (in 30000 / out 60000)", nil
		},
		workerStopFn: func(cfg *config.Config, slotName string, sess *state.Session) error {
			return nil
		},
		saveCheckpointFn: func(sess *state.Session) (string, error) {
			checkpointed = append(checkpointed, "saved")
			return "/tmp/CHECKPOINT.md", nil
		},
	}

	s := state.NewState()
	s.Sessions["mae-2"] = &state.Session{
		IssueNumber:    42,
		Status:         state.StatusRunning,
		PID:            1234,
		TmuxSession:    "maestro-mae-2",
		Branch:         "feat/mae-2-42-test",
		StartedAt:      time.Now().Add(-30 * time.Minute),
		Backend:        "claude",
		CheckpointFile: "/tmp/old-CHECKPOINT.md", // already checkpointed
	}

	o.checkSessions(s)

	if len(checkpointed) != 0 {
		t.Fatalf("checkpointed = %v, want empty (already has checkpoint, should not re-checkpoint)", checkpointed)
	}
	sess := s.Sessions["mae-2"]
	if sess.Status != state.StatusRunning {
		t.Fatalf("status = %q, want %q", sess.Status, state.StatusRunning)
	}
}

func TestCheckSessions_SoftThresholdDisabled_NoCheckpoint(t *testing.T) {
	zero := 0.0
	cfg := &config.Config{
		Repo:                     "owner/repo",
		WorkerMaxTokens:          100000,
		WorkerSoftTokenThreshold: &zero, // disabled
		MaxRuntimeMinutes:        999,
	}
	checkpointed := make([]string, 0)
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
			return "tokens 85000 (in 25000 / out 60000)", nil
		},
		workerStopFn: func(cfg *config.Config, slotName string, sess *state.Session) error {
			return nil
		},
		saveCheckpointFn: func(sess *state.Session) (string, error) {
			checkpointed = append(checkpointed, "saved")
			return "/tmp/CHECKPOINT.md", nil
		},
	}

	s := state.NewState()
	s.Sessions["mae-3"] = &state.Session{
		IssueNumber: 42,
		Status:      state.StatusRunning,
		PID:         1234,
		TmuxSession: "maestro-mae-3",
		Branch:      "feat/mae-3-42-test",
		StartedAt:   time.Now().Add(-30 * time.Minute),
		Backend:     "claude",
	}

	o.checkSessions(s)

	if len(checkpointed) != 0 {
		t.Fatalf("checkpointed = %v, want empty (soft threshold disabled)", checkpointed)
	}
}

func TestCheckSessions_BelowSoftThreshold_NoCheckpoint(t *testing.T) {
	softThreshold := 0.8
	cfg := &config.Config{
		Repo:                     "owner/repo",
		WorkerMaxTokens:          100000,
		WorkerSoftTokenThreshold: &softThreshold,
		MaxRuntimeMinutes:        999,
	}
	checkpointed := make([]string, 0)
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
			return "tokens 50000 (in 10000 / out 40000)", nil // below 80k soft limit
		},
		workerStopFn: func(cfg *config.Config, slotName string, sess *state.Session) error {
			return nil
		},
		saveCheckpointFn: func(sess *state.Session) (string, error) {
			checkpointed = append(checkpointed, "saved")
			return "/tmp/CHECKPOINT.md", nil
		},
	}

	s := state.NewState()
	s.Sessions["mae-4"] = &state.Session{
		IssueNumber: 42,
		Status:      state.StatusRunning,
		PID:         1234,
		TmuxSession: "maestro-mae-4",
		Branch:      "feat/mae-4-42-test",
		StartedAt:   time.Now().Add(-30 * time.Minute),
		Backend:     "claude",
	}

	o.checkSessions(s)

	if len(checkpointed) != 0 {
		t.Fatalf("checkpointed = %v, want empty (below soft threshold)", checkpointed)
	}
	sess := s.Sessions["mae-4"]
	if sess.Status != state.StatusRunning {
		t.Fatalf("status = %q, want %q", sess.Status, state.StatusRunning)
	}
	if sess.TokensUsedAttempt != 50000 {
		t.Fatalf("tokens_used_attempt = %d, want 50000", sess.TokensUsedAttempt)
	}
}

func TestAppendReviewFeedbackContext_AddsSection(t *testing.T) {
	base := "You are a coding agent."
	feedback := "Confidence Score: 3/5\nP2: enabled flag logic inverted in bridge.rs"
	result := appendReviewFeedbackContext(base, feedback)

	if !strings.Contains(result, "You are a coding agent.") {
		t.Error("result should contain original prompt base")
	}
	if !strings.Contains(result, "Code Review Findings") {
		t.Error("result should contain review feedback header")
	}
	if !strings.Contains(result, "enabled flag logic inverted") {
		t.Error("result should contain actual review feedback")
	}
	if !strings.Contains(result, "IMPORTANT: Address ALL code review findings") {
		t.Error("result should contain instruction to address findings")
	}
}

func TestAppendReviewFeedbackContext_EmptyFeedbackNotCalled(t *testing.T) {
	// This tests that empty feedback doesn't produce output (caller should guard)
	base := "You are a coding agent."
	result := appendReviewFeedbackContext(base, "")

	// Even with empty string, the section header would be added —
	// the caller (respawnDueRetries) guards against empty string
	if !strings.Contains(result, "Code Review Findings") {
		t.Error("function always adds header — caller must guard against empty feedback")
	}
}

func TestRespawnDueRetries_ReviewFeedback_IncludedInPrompt(t *testing.T) {
	cfg := &config.Config{
		Repo:               "owner/repo",
		MaxRetriesPerIssue: 3,
		MaxRetryBackoffMs:  300000,
		Model: config.ModelConfig{
			Default:  "claude",
			Backends: map[string]config.BackendDef{"claude": {Cmd: "claude"}},
		},
	}

	respawnedPrompt := ""
	o := &Orchestrator{
		cfg:        cfg,
		notifier:   &notify.Notifier{},
		promptBase: "base prompt {{ISSUE_NUMBER}}",
		getIssueFn: func(number int) (github.Issue, error) {
			return github.Issue{Number: 42, Title: "test issue", Body: "fix this"}, nil
		},
		respawnWorkerFn: func(cfg *config.Config, slotName string, sess *state.Session, repo string, issue github.Issue, promptBase string, backendName string) error {
			respawnedPrompt = promptBase
			return nil
		},
	}

	s := state.NewState()
	retryAt := time.Now().UTC().Add(-1 * time.Minute)
	s.Sessions["slot-1"] = &state.Session{
		IssueNumber:             42,
		IssueTitle:              "test issue",
		Status:                  state.StatusDead,
		RetryCount:              1,
		NextRetryAt:             &retryAt,
		Backend:                 "claude",
		CIFailureOutput:         "tests failed: FAIL main_test.go:15",
		PreviousAttemptFeedback: "Confidence 3/5\nP2: enabled flag inverted in bridge.rs",
	}

	o.respawnDueRetries(s, 10)

	if respawnedPrompt == "" {
		t.Fatal("respawnWorkerFn should have been called")
	}
	if !strings.Contains(respawnedPrompt, "Previous CI Failure") {
		t.Error("prompt should contain CI failure context")
	}
	if !strings.Contains(respawnedPrompt, "Code Review Findings") {
		t.Error("prompt should contain review feedback section")
	}
	if !strings.Contains(respawnedPrompt, "enabled flag inverted") {
		t.Error("prompt should contain actual review feedback")
	}
	if !strings.Contains(respawnedPrompt, "IMPORTANT: Address ALL code review findings") {
		t.Error("prompt should contain instruction to fix review findings")
	}

	// Both fields should be consumed (cleared)
	sess := s.Sessions["slot-1"]
	if sess.CIFailureOutput != "" {
		t.Errorf("CIFailureOutput should be cleared, got %q", sess.CIFailureOutput)
	}
	if sess.PreviousAttemptFeedback != "" {
		t.Errorf("PreviousAttemptFeedback should be cleared, got %q", sess.PreviousAttemptFeedback)
	}
}

func TestRespawnDueRetries_RebaseConflict_IncludedInPrompt(t *testing.T) {
	cfg := &config.Config{
		Repo:               "owner/repo",
		MaxRetriesPerIssue: 3,
		MaxRetryBackoffMs:  300000,
		Model: config.ModelConfig{
			Default:  "claude",
			Backends: map[string]config.BackendDef{"claude": {Cmd: "claude"}},
		},
	}

	respawnedPrompt := ""
	o := &Orchestrator{
		cfg:        cfg,
		notifier:   &notify.Notifier{},
		promptBase: "base prompt {{ISSUE_NUMBER}}",
		getIssueFn: func(number int) (github.Issue, error) {
			return github.Issue{Number: 42, Title: "test issue", Body: "fix this"}, nil
		},
		respawnInPlaceFn: func(cfg *config.Config, slotName string, sess *state.Session, repo string, issue github.Issue, promptBase string, backendName string) error {
			respawnedPrompt = promptBase
			return nil
		},
	}

	s := state.NewState()
	retryAt := time.Now().UTC().Add(-1 * time.Minute)
	s.Sessions["slot-1"] = &state.Session{
		IssueNumber:                 42,
		IssueTitle:                  "test issue",
		Status:                      state.StatusDead,
		RetryCount:                  1,
		NextRetryAt:                 &retryAt,
		Backend:                     "claude",
		PRNumber:                    10,
		Worktree:                    "/tmp/wt",
		PreviousAttemptFeedback:     "CONFLICT (content): docs/FEATURES.md",
		PreviousAttemptFeedbackKind: "rebase_conflict",
	}

	o.respawnDueRetries(s, 10)

	if respawnedPrompt == "" {
		t.Fatal("respawnInPlaceFn should have been called")
	}
	if !strings.Contains(respawnedPrompt, "Rebase Conflict") {
		t.Error("prompt should contain rebase conflict section")
	}
	if !strings.Contains(respawnedPrompt, "docs/FEATURES.md") {
		t.Error("prompt should contain conflict details")
	}
	if strings.Contains(respawnedPrompt, "Code Review Findings") {
		t.Error("rebase conflict prompt should not be framed as review feedback")
	}
	sess := s.Sessions["slot-1"]
	if sess.PreviousAttemptFeedback != "" {
		t.Errorf("PreviousAttemptFeedback should be cleared, got %q", sess.PreviousAttemptFeedback)
	}
	if sess.PreviousAttemptFeedbackKind != "" {
		t.Errorf("PreviousAttemptFeedbackKind should be cleared, got %q", sess.PreviousAttemptFeedbackKind)
	}
}

func TestRespawnDueRetries_NoReviewFeedback_OmitsSection(t *testing.T) {
	cfg := &config.Config{
		Repo:               "owner/repo",
		MaxRetriesPerIssue: 3,
		MaxRetryBackoffMs:  300000,
		Model: config.ModelConfig{
			Default:  "claude",
			Backends: map[string]config.BackendDef{"claude": {Cmd: "claude"}},
		},
	}

	respawnedPrompt := ""
	o := &Orchestrator{
		cfg:        cfg,
		notifier:   &notify.Notifier{},
		promptBase: "base prompt {{ISSUE_NUMBER}}",
		getIssueFn: func(number int) (github.Issue, error) {
			return github.Issue{Number: 42, Title: "test issue", Body: "fix this"}, nil
		},
		respawnWorkerFn: func(cfg *config.Config, slotName string, sess *state.Session, repo string, issue github.Issue, promptBase string, backendName string) error {
			respawnedPrompt = promptBase
			return nil
		},
	}

	s := state.NewState()
	retryAt := time.Now().UTC().Add(-1 * time.Minute)
	s.Sessions["slot-1"] = &state.Session{
		IssueNumber:             42,
		IssueTitle:              "test issue",
		Status:                  state.StatusDead,
		RetryCount:              1,
		NextRetryAt:             &retryAt,
		Backend:                 "claude",
		CIFailureOutput:         "tests failed",
		PreviousAttemptFeedback: "", // no Greptile feedback
	}

	o.respawnDueRetries(s, 10)

	if respawnedPrompt == "" {
		t.Fatal("respawnWorkerFn should have been called")
	}
	if strings.Contains(respawnedPrompt, "Code Review Findings") {
		t.Error("prompt should NOT contain review feedback section when no feedback exists")
	}
}

func TestAutoMergePRs_CIFailure_CollectsReviewFeedback(t *testing.T) {
	prs := []github.PR{
		{Number: 10, HeadRefName: "feat/a"},
	}
	cfg := &config.Config{Repo: "owner/repo", MergeStrategy: "parallel", MaxRetriesPerIssue: 3, MaxRetryBackoffMs: 300000}
	o, _, _ := newCIFailureRetryOrchestrator(cfg, prs, map[int]string{10: "failure"})

	// Override with review feedback
	o.ghCollectPRReviewFeedbackFn = func(prNumber int) (string, error) {
		return "Confidence 3/5 — Not safe to merge\nP2: null dereference on pool.interface", nil
	}

	s := makeTestState(prs)
	o.autoMergePRs(s)

	sess := s.Sessions["slot-0"]
	if sess.PreviousAttemptFeedback == "" {
		t.Fatal("PreviousAttemptFeedback should be set after CI failure with review feedback")
	}
	if !strings.Contains(sess.PreviousAttemptFeedback, "null dereference") {
		t.Errorf("PreviousAttemptFeedback should contain review feedback, got %q", sess.PreviousAttemptFeedback)
	}
}

func TestAutoMergePRs_CIFailure_NoGreptileFeedback_FeedbackEmpty(t *testing.T) {
	prs := []github.PR{
		{Number: 10, HeadRefName: "feat/a"},
	}
	cfg := &config.Config{Repo: "owner/repo", MergeStrategy: "parallel", MaxRetriesPerIssue: 3, MaxRetryBackoffMs: 300000}
	o, _, _ := newCIFailureRetryOrchestrator(cfg, prs, map[int]string{10: "failure"})

	// No review feedback (returns empty)
	o.ghCollectPRReviewFeedbackFn = func(prNumber int) (string, error) {
		return "", nil
	}

	s := makeTestState(prs)
	o.autoMergePRs(s)

	sess := s.Sessions["slot-0"]
	if sess.PreviousAttemptFeedback != "" {
		t.Errorf("PreviousAttemptFeedback should be empty when no Greptile feedback, got %q", sess.PreviousAttemptFeedback)
	}
}

// --- issue #909: live model-route selector reloads ---

func TestReloadConfig_ModelDefaultChangeUpdatesLiveRouter(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{
		Repo:     "owner/repo",
		StateDir: dir,
		Model: config.ModelConfig{
			Default:  "codex",
			Backends: map[string]config.BackendDef{"codex": {Cmd: "codex"}, "claude": {Cmd: "claude"}},
		},
	}
	o := &Orchestrator{
		cfg:      cfg,
		repo:     cfg.Repo,
		notifier: notify.NewWithToken("", "", "", ""),
		router:   router.New(cfg),
	}

	newCfg := &config.Config{
		Repo:     "owner/repo",
		StateDir: dir,
		Model: config.ModelConfig{
			Default:  "claude",
			Backends: map[string]config.BackendDef{"codex": {Cmd: "codex"}, "claude": {Cmd: "claude"}},
		},
	}

	ticker := time.NewTicker(10 * time.Minute)
	defer ticker.Stop()
	o.reloadConfig(newCfg, &ticker)

	if o.restartRequired {
		t.Fatalf("model route reload unexpectedly requires restart: %s", o.restartRequiredReason)
	}
	if o.cfg.Model.Default != "claude" {
		t.Fatalf("model.default = %q, want hot-applied claude", o.cfg.Model.Default)
	}
	decision := o.router.ResolveBackendDecision(github.Issue{Number: 909, Title: "route reload"})
	if decision.Backend != "claude" {
		t.Fatalf("live router backend = %q, want claude after reload", decision.Backend)
	}
}

func TestReloadConfig_ProviderLanesUpdateLiveFallbackSelector(t *testing.T) {
	cfg := &config.Config{Model: config.ModelConfig{
		Default: "claude",
		Backends: map[string]config.BackendDef{
			"claude": {Cmd: "claude", Provider: "anthropic"},
		},
	}}
	o := &Orchestrator{cfg: cfg, router: router.New(cfg)}
	newCfg := *cfg
	newCfg.Model = cfg.Model
	newCfg.Model.Backends = map[string]config.BackendDef{
		"claude": {Cmd: "claude", Provider: "anthropic"},
		"sol":    {Cmd: "codex", Provider: "openai", Model: "gpt-5.6-sol", Effort: "high"},
		"gpt55":  {Cmd: "codex", Provider: "openai", Model: "gpt-5.5", Effort: "high"},
	}
	newCfg.Model.ProviderLanes = []config.ProviderLane{
		{Provider: "anthropic", Default: "claude"},
		{Provider: "openai", Default: "sol", FallbackBackends: []string{"gpt55"}},
	}

	ticker := time.NewTicker(10 * time.Minute)
	defer ticker.Stop()
	o.reloadConfig(&newCfg, &ticker)

	got := o.backendFallbackCandidates(nil, &state.Session{Backend: "claude"}, "")
	if !reflect.DeepEqual(got, []string{"sol", "gpt55"}) {
		t.Fatalf("live fallback selector = %v, want [sol gpt55]", got)
	}
	if got := o.cfg.Model.Backends["sol"]; got.Model != "gpt-5.6-sol" || got.Effort != "high" {
		t.Fatalf("live SOL backend = %+v, want reloaded definition", got)
	}
	decision, ok, _ := o.resolveDispatchBackend(state.NewState(), github.Issue{Number: 909}, time.Now())
	if !ok || decision.Backend != "claude" {
		t.Fatalf("live dispatch decision = %+v, ok=%t", decision, ok)
	}
}

func TestReloadConfig_RoutingChangeStillRequiresRestart(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{
		Repo:     "owner/repo",
		StateDir: dir,
		Model: config.ModelConfig{
			Default:  "claude",
			Backends: map[string]config.BackendDef{"claude": {Cmd: "claude"}, "codex": {Cmd: "codex"}},
		},
		Routing: config.RoutingConfig{Mode: "manual", RouterModel: "codex", RouterModelName: "x"},
	}
	o := &Orchestrator{
		cfg:      cfg,
		repo:     cfg.Repo,
		notifier: notify.NewWithToken("", "", "", ""),
		router:   router.New(cfg),
	}

	newCfg := &config.Config{
		Repo:     "owner/repo",
		StateDir: dir,
		Model: config.ModelConfig{
			Default:  "claude",
			Backends: map[string]config.BackendDef{"claude": {Cmd: "claude"}, "codex": {Cmd: "codex"}},
		},
		Routing: config.RoutingConfig{Mode: "manual", RouterModel: "claude", RouterModelName: "x"},
	}

	ticker := time.NewTicker(10 * time.Minute)
	defer ticker.Stop()
	o.reloadConfig(newCfg, &ticker)

	if !o.restartRequired {
		t.Fatal("restartRequired not set after routing.router_model change")
	}
	if !strings.Contains(o.restartRequiredReason, "routing") {
		t.Fatalf("restartRequiredReason = %q, want it to mention routing", o.restartRequiredReason)
	}
	if o.cfg.Routing.RouterModel != "codex" {
		t.Fatalf("routing.router_model = %q, want unchanged codex", o.cfg.Routing.RouterModel)
	}
	st, err := state.Load(dir)
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	if !st.RestartRequired || !strings.Contains(st.RestartRequiredReason, "routing") {
		t.Fatalf("state restart-required not persisted for routing change: %+v", st)
	}
}

// TestClearStaleRestartRequired_ClearsOnFreshStart verifies that a restart_required
// flag persisted by a previous process is reconciled away on a fresh daemon start
// (issue #549). The banner must not survive the very restart it asked the operator to
// perform. A genuine restart-required config change after start must still
// re-raise the signal.
func TestClearStaleRestartRequired_ClearsOnFreshStart(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{
		Repo:     "owner/repo",
		StateDir: dir,
		Model: config.ModelConfig{
			Default:  "codex",
			Backends: map[string]config.BackendDef{"codex": {Cmd: "codex"}, "claude": {Cmd: "claude"}},
		},
		Routing: config.RoutingConfig{Mode: "manual", RouterModel: "codex"},
	}

	// Seed state.json with a stale restart-required flag left over from a previous
	// process (e.g. a model.default change that has since been reverted).
	s, err := state.Load(dir)
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	s.RestartRequired = true
	s.RestartRequiredReason = "model.default changed (claude → freellm)"
	if err := state.Save(dir, s); err != nil {
		t.Fatalf("save seed state: %v", err)
	}

	// Fresh orchestrator: in-memory restartRequired is the zero value (false), which
	// is the truth right after a (re)start.
	o := &Orchestrator{
		cfg:      cfg,
		repo:     cfg.Repo,
		notifier: notify.NewWithToken("", "", "", ""),
		router:   router.New(cfg),
	}

	o.clearStaleRestartRequired()

	if o.restartRequired {
		t.Fatal("in-memory restartRequired unexpectedly set on a fresh start")
	}
	st, err := state.Load(dir)
	if err != nil {
		t.Fatalf("reload state: %v", err)
	}
	if st.RestartRequired {
		t.Fatalf("state.RestartRequired = true, want cleared after fresh start; reason=%q", st.RestartRequiredReason)
	}
	if st.RestartRequiredReason != "" {
		t.Fatalf("state.RestartRequiredReason = %q, want empty after fresh start", st.RestartRequiredReason)
	}

	newCfg := *cfg
	newCfg.Routing = config.RoutingConfig{Mode: "manual", RouterModel: "claude"}
	ticker := time.NewTicker(10 * time.Minute)
	defer ticker.Stop()
	o.reloadConfig(&newCfg, &ticker)

	if !o.restartRequired {
		t.Fatal("restartRequired not re-raised by a routing change after start")
	}
	st, err = state.Load(dir)
	if err != nil {
		t.Fatalf("reload state after change: %v", err)
	}
	if !st.RestartRequired || !strings.Contains(st.RestartRequiredReason, "routing") {
		t.Fatalf("state restart-required not re-persisted after routing change: %+v", st)
	}

}

// TestClearStaleRestartRequired_PreservesInProcessSignal verifies that if a real
// restart-required signal was already raised within this process before start
// completes, clearStaleRestartRequired keeps it (it only reconciles stale state from a
// PREVIOUS process, never a live in-process signal).
func TestClearStaleRestartRequired_PreservesInProcessSignal(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{
		Repo:     "owner/repo",
		StateDir: dir,
		Model: config.ModelConfig{
			Default:  "codex",
			Backends: map[string]config.BackendDef{"codex": {Cmd: "codex"}},
		},
	}
	o := &Orchestrator{
		cfg:      cfg,
		repo:     cfg.Repo,
		notifier: notify.NewWithToken("", "", "", ""),
		router:   router.New(cfg),
	}
	o.markRestartRequired("model.default changed (codex → claude)")

	o.clearStaleRestartRequired()

	if !o.restartRequired {
		t.Fatal("clearStaleRestartRequired wiped a live in-process restart-required signal")
	}
	st, err := state.Load(dir)
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	if !st.RestartRequired {
		t.Fatal("state restart-required cleared despite a live in-process signal")
	}
}

// --- issue #456: pre-spawn guard for already-merged / closed issues ---

// TestStartNewWorkers_SkipsIssueWithMergedPRDespiteStaleReadyLabel verifies that an
// issue whose linked PR already merged is NOT dispatched even when it still carries a
// stale maestro-ready label and the supervisor (which normally strips the label) is down.
func TestStartNewWorkers_SkipsIssueWithMergedPRDespiteStaleReadyLabel(t *testing.T) {
	cfg := cfgWithBackends("codex", "codex")
	cfg.IssueLabels = []string{"maestro-ready"}
	issues := []github.Issue{
		makeIssue(248, "already merged, stale ready", "maestro-ready"),
		makeIssue(247, "genuinely ready", "maestro-ready"),
	}

	o, started, _ := newStartWorkersOrchestrator(cfg, issues)
	o.hasMergedPRForIssueFn = func(issueNumber int) (bool, error) {
		return issueNumber == 248, nil
	}

	var logs strings.Builder
	prev := log.Writer()
	log.SetOutput(&logs)
	defer log.SetOutput(prev)

	s := state.NewState()
	o.startNewWorkers(s, 5)

	for _, n := range *started {
		if n == 248 {
			t.Fatalf("started worker for already-merged issue #248; started=%v", *started)
		}
	}
	if len(*started) != 1 || (*started)[0] != 247 {
		t.Fatalf("started = %v, want only [247]", *started)
	}
	if !strings.Contains(logs.String(), "skipping issue #248: has merged PR") {
		t.Fatalf("logs missing merged-PR skip reason:\n%s", logs.String())
	}
}

// TestStartNewWorkers_SkipsClosedIssueDespiteStaleReadyLabel verifies the same guard
// for a closed issue that still carries a stale ready label and has no session at all.
func TestStartNewWorkers_SkipsClosedIssueDespiteStaleReadyLabel(t *testing.T) {
	cfg := cfgWithBackends("codex", "codex")
	cfg.IssueLabels = []string{"maestro-ready"}
	issues := []github.Issue{
		makeIssue(249, "closed, stale ready", "maestro-ready"),
	}

	o, started, _ := newStartWorkersOrchestrator(cfg, issues)
	o.isIssueClosedFn = func(issueNumber int) (bool, error) {
		return issueNumber == 249, nil
	}

	s := state.NewState() // no session for #249 — guard must not depend on session state
	o.startNewWorkers(s, 5)

	if len(*started) != 0 {
		t.Fatalf("started = %v, want none for closed issue with stale ready label", *started)
	}
}

// --- issue #457: --once dispatch must respect max_parallel ---

// TestStartNewWorkers_RespectsMaxParallelHardCap verifies a single dispatch pass never
// starts more workers than max_parallel even when more ready issues than slots exist.
func TestStartNewWorkers_RespectsMaxParallelHardCap(t *testing.T) {
	cfg := cfgWithBackends("codex", "codex")
	cfg.MaxParallel = 3
	issues := []github.Issue{
		makeIssue(255, "ready a"),
		makeIssue(254, "ready b"),
		makeIssue(250, "ready c"),
		makeIssue(249, "ready d"),
		makeIssue(248, "ready e"),
		makeIssue(247, "ready f"),
	}

	o, started, _ := newStartWorkersOrchestrator(cfg, issues)
	s := state.NewState()

	// availableSlots(cfg, s, active=0) == 3
	slots := availableSlots(cfg, s, len(s.ActiveSessions()))
	if slots != 3 {
		t.Fatalf("precondition: availableSlots = %d, want 3", slots)
	}

	o.startNewWorkers(s, slots)

	if len(*started) > cfg.MaxParallel {
		t.Fatalf("started %d workers (%v), want at most max_parallel=%d", len(*started), *started, cfg.MaxParallel)
	}
	if len(*started) != 3 {
		t.Fatalf("started %d workers (%v), want exactly 3", len(*started), *started)
	}
	if len(s.ActiveSessions()) > cfg.MaxParallel {
		t.Fatalf("active sessions = %d, want at most max_parallel=%d", len(s.ActiveSessions()), cfg.MaxParallel)
	}
}

func TestAutoMergePRs_ConvergenceMergesRetryExhaustedNonCritical(t *testing.T) {
	pr := github.PR{Number: 42, HeadRefName: "feat/x"}
	merged := make([]int, 0)
	cfg := &config.Config{Repo: "owner/repo", AutoRetryReviewFeedback: true, MergeStrategy: "parallel"}
	o := &Orchestrator{
		cfg:                         cfg,
		notifier:                    &notify.Notifier{},
		listOpenPRsFn:               func() ([]github.PR, error) { return []github.PR{pr}, nil },
		ghPRCIStatusFn:              func(int) (string, error) { return "success", nil },
		ghCollectPRReviewFeedbackFn: func(int) (string, error) { return "P1: non-blocking nit", nil },
		ghPRHasCriticalReviewFn:     func(int) (bool, error) { return false, nil },
		ghPRGreptileApprovedFn:      func(int) (bool, bool, error) { return false, false, nil },
		ghMergePRFn:                 func(n int) error { merged = append(merged, n); return nil },
		ghCloseIssueFn:              func(int, string) error { return nil },
		workerStopFn:                func(*config.Config, string, *state.Session) error { return nil },
	}
	s := state.NewState()
	s.Sessions["slot-0"] = &state.Session{
		IssueNumber: 100, Status: state.StatusRetryExhausted, PRNumber: 42,
		Branch: "feat/x", LastNotifiedStatus: "review_retry_exhausted",
	}

	o.autoMergePRs(s)

	if len(merged) != 1 || merged[0] != 42 {
		t.Fatalf("convergence: merged = %v, want [42] (retry-exhausted green PR, no P0 on head, must merge #565)", merged)
	}
}

func TestAutoMergePRs_ConvergenceHoldsOnCritical(t *testing.T) {
	pr := github.PR{Number: 42, HeadRefName: "feat/x"}
	merged := make([]int, 0)
	cfg := &config.Config{Repo: "owner/repo", AutoRetryReviewFeedback: true, MergeStrategy: "parallel"}
	o := &Orchestrator{
		cfg:                         cfg,
		notifier:                    &notify.Notifier{},
		listOpenPRsFn:               func() ([]github.PR, error) { return []github.PR{pr}, nil },
		ghPRCIStatusFn:              func(int) (string, error) { return "success", nil },
		ghCollectPRReviewFeedbackFn: func(int) (string, error) { return "P0: critical", nil },
		ghPRHasCriticalReviewFn:     func(int) (bool, error) { return true, nil },
		ghMergePRFn:                 func(n int) error { merged = append(merged, n); return nil },
		ghCloseIssueFn:              func(int, string) error { return nil },
		workerStopFn:                func(*config.Config, string, *state.Session) error { return nil },
	}
	s := state.NewState()
	s.Sessions["slot-0"] = &state.Session{
		IssueNumber: 100, Status: state.StatusRetryExhausted, PRNumber: 42,
		Branch: "feat/x", LastNotifiedStatus: "review_retry_exhausted",
	}

	o.autoMergePRs(s)

	if len(merged) != 0 {
		t.Fatalf("convergence: a P0 finding on head must hard-block; merged = %v, want []", merged)
	}
}

func TestMergeReadyPR_BehindBaseNoWorktreeFallsBackToUpdateBranch(t *testing.T) {
	updateCalled := 0
	cfg := &config.Config{Repo: "owner/repo", AutoRebase: true}
	o := &Orchestrator{
		cfg:      cfg,
		notifier: &notify.Notifier{},
		ghMergePRFn: func(int) error {
			return fmt.Errorf("Pull request is not mergeable: the head branch is not up to date with the base branch")
		},
		ghUpdateBranchFn: func(int) error { updateCalled++; return nil },
	}
	sess := &state.Session{IssueNumber: 100, PRNumber: 10, Branch: "feat/a", Status: state.StatusRetryExhausted}
	s := state.NewState()
	s.Sessions["slot-0"] = sess
	pr := github.PR{Number: 10, HeadRefName: "feat/a"}

	o.mergeReadyPR(s, "slot-0", sess, pr)

	if updateCalled != 1 {
		t.Fatalf("server-side update-branch called %d times, want 1 (behind base + no worktree)", updateCalled)
	}
	if sess.Status != state.StatusQueued {
		t.Fatalf("status = %q, want queued for re-validation after update-branch", sess.Status)
	}
}

// #577: a worker that exhausts retries without ever opening a PR (e.g. because
// the issue was already implemented by a prior `Refs #N` merge) used to leave
// autoMergePRs logging "waiting for reconciliation" every cycle with no
// recovery action, halting the dynamic-wave queue at max_parallel=1. The
// reconciler must apply the configured blocked label so the supervisor's
// dynamic-wave drops the issue and selects the next candidate.
func TestAutoMergePRs_NoPRRetryExhaustedAppliesBlockedLabel(t *testing.T) {
	cfg := &config.Config{
		Repo: "owner/repo",
		Supervisor: config.SupervisorConfig{
			BlockedLabel: "blocked",
		},
	}
	addedLabels := make(map[int]string)
	o := &Orchestrator{
		cfg:           cfg,
		notifier:      &notify.Notifier{},
		listOpenPRsFn: func() ([]github.PR, error) { return nil, nil },
		isIssueClosedFn: func(issueNumber int) (bool, error) {
			return false, nil
		},
		hasMergedPRForIssueFn: func(issueNumber int) (bool, error) {
			return false, nil
		},
		addIssueLabelFn: func(number int, label string) error {
			addedLabels[number] = label
			return nil
		},
	}
	s := state.NewState()
	s.Sessions["sup-7"] = &state.Session{
		IssueNumber: 488,
		IssueTitle:  "already-done issue",
		Branch:      "feat/sup-7-already-done",
		Status:      state.StatusRetryExhausted,
		PRNumber:    0,
	}

	o.autoMergePRs(s)

	if got, want := addedLabels[488], "blocked"; got != want {
		t.Fatalf("issue #488 label = %q, want %q (no-PR retry_exhausted must mark blocked)", got, want)
	}
	sess := s.Sessions["sup-7"]
	if sess.LastNotifiedStatus != noPRReconciledStatus {
		t.Fatalf("LastNotifiedStatus = %q, want %q (reconciler must mark idempotent)", sess.LastNotifiedStatus, noPRReconciledStatus)
	}

	// Second autoMergePRs cycle must NOT re-apply the label or re-notify.
	clear(addedLabels)
	o.autoMergePRs(s)
	if len(addedLabels) != 0 {
		t.Fatalf("second cycle re-applied labels = %v, want none (idempotency)", addedLabels)
	}
}

func TestAutoMergePRs_NoPRRetryExhaustedGenuinelyStuckIssueBlocksOnceAcrossSlots(t *testing.T) {
	cfg := &config.Config{
		Repo:       "owner/repo",
		Supervisor: config.SupervisorConfig{BlockedLabel: "blocked"},
	}
	labelCalls := 0
	o := &Orchestrator{
		cfg:             cfg,
		notifier:        &notify.Notifier{},
		listOpenPRsFn:   func() ([]github.PR, error) { return nil, nil },
		isIssueClosedFn: func(issueNumber int) (bool, error) { return false, nil },
		hasMergedPRForIssueFn: func(issueNumber int) (bool, error) {
			return false, nil
		},
		addIssueLabelFn: func(number int, label string) error {
			labelCalls++
			return nil
		},
	}
	s := state.NewState()
	for _, slot := range []string{"slot-a", "slot-b", "slot-c"} {
		s.Sessions[slot] = &state.Session{
			IssueNumber: 836,
			Status:      state.StatusRetryExhausted,
			Branch:      slot,
		}
	}

	o.autoMergePRs(s)

	if labelCalls != 1 {
		t.Fatalf("blocked label calls = %d, want one aggregate issue action", labelCalls)
	}
	for _, slot := range []string{"slot-a", "slot-b", "slot-c"} {
		if got := s.Sessions[slot].LastNotifiedStatus; got != noPRReconciledStatus {
			t.Fatalf("%s LastNotifiedStatus = %q, want %q", slot, got, noPRReconciledStatus)
		}
	}
}

func TestAutoMergePRs_NoPRRetryExhaustedSlotsDoNotBlockSiblingOpenPR(t *testing.T) {
	cfg := &config.Config{
		Repo:       "owner/repo",
		ReviewGate: "none",
		Supervisor: config.SupervisorConfig{BlockedLabel: "blocked"},
	}
	addedLabels := make(map[int]string)
	o := &Orchestrator{
		cfg:      cfg,
		notifier: &notify.Notifier{},
		listOpenPRsFn: func() ([]github.PR, error) {
			// Deliberately omit an issue reference: aggregate ownership must come
			// from the sibling session's issue/PR/branch identity, not PR text.
			return []github.PR{{Number: 850, HeadRefName: "slot-d"}}, nil
		},
		isIssueClosedFn: func(issueNumber int) (bool, error) { return false, nil },
		ghPRCIStatusFn:  func(int) (string, error) { return "pending", nil },
		ghPRMergeStatusFn: func(int) (string, string, error) {
			return "MERGEABLE", "blocked", nil
		},
		addIssueLabelFn: func(number int, label string) error {
			addedLabels[number] = label
			return nil
		},
	}
	s := state.NewState()
	for _, slot := range []string{"slot-a", "slot-b", "slot-c"} {
		s.Sessions[slot] = &state.Session{
			IssueNumber: 835,
			Status:      state.StatusRetryExhausted,
			Branch:      slot,
		}
	}
	s.Sessions["slot-d"] = &state.Session{
		IssueNumber: 835,
		Status:      state.StatusPROpen,
		PRNumber:    850,
		Branch:      "slot-d",
	}

	var logs strings.Builder
	previousLogOutput := log.Writer()
	log.SetOutput(&logs)
	defer log.SetOutput(previousLogOutput)

	o.autoMergePRs(s)

	if len(addedLabels) != 0 {
		t.Fatalf("dead sibling slots applied labels behind open PR: %v", addedLabels)
	}
	for _, slot := range []string{"slot-a", "slot-b", "slot-c"} {
		sess := s.Sessions[slot]
		if sess.Status != state.StatusRetryExhausted {
			t.Fatalf("%s status = %q, want retry_exhausted while sibling PR is in flight", slot, sess.Status)
		}
		if sess.LastNotifiedStatus == noPRReconciledStatus {
			t.Fatalf("%s was falsely reconciled as a blocking no-PR outcome", slot)
		}
	}
	if !strings.Contains(logs.String(), "issue #835 has open PR #850 in aggregate session state") ||
		!strings.Contains(logs.String(), "without applying blocked") {
		t.Fatalf("journal does not explain aggregate open-PR suppression:\n%s", logs.String())
	}
}

func TestAutoMergePRs_NoPRRetryExhaustedClosedIssueSettlesAllStaleSlots(t *testing.T) {
	now := time.Now().UTC()
	cfg := &config.Config{
		Repo: "owner/repo",
		GitHubProjects: config.GitHubProjectsConfig{
			Enabled:       true,
			ProjectNumber: 1,
		},
		Supervisor: config.SupervisorConfig{BlockedLabel: "blocked"},
	}
	labelCalls := 0
	var projectStatuses []github.ProjectStatus
	o := &Orchestrator{
		cfg:                  cfg,
		notifier:             &notify.Notifier{},
		projectRateAllowed:   true,
		projectRateCheckedAt: now,
		listOpenPRsFn:        func() ([]github.PR, error) { return nil, nil },
		isIssueClosedFn:      func(issueNumber int) (bool, error) { return true, nil },
		addIssueLabelFn: func(number int, label string) error {
			labelCalls++
			return nil
		},
		syncProjectFn: func(issueNumber int, status github.ProjectStatus) bool {
			projectStatuses = append(projectStatuses, status)
			return true
		},
		hasMergedPRForIssueFn: func(issueNumber int) (bool, error) {
			t.Fatalf("merged-PR lookup must not run after authoritative issue closure")
			return false, nil
		},
	}
	s := state.NewState()
	for _, slot := range []string{"slot-a", "slot-b", "slot-c"} {
		s.Sessions[slot] = &state.Session{
			IssueNumber: 835,
			Status:      state.StatusRetryExhausted,
			Branch:      slot,
		}
	}

	var logs strings.Builder
	previousLogOutput := log.Writer()
	log.SetOutput(&logs)
	defer log.SetOutput(previousLogOutput)

	o.autoMergePRs(s)

	if labelCalls != 0 {
		t.Fatalf("closed issue received %d blocked-label call(s), want 0", labelCalls)
	}
	if len(projectStatuses) != 0 {
		t.Fatalf("closed issue received stale-slot project syncs %v, want none", projectStatuses)
	}
	for _, slot := range []string{"slot-a", "slot-b", "slot-c"} {
		sess := s.Sessions[slot]
		if sess.Status != state.StatusDone || sess.IssueClosedAt == nil || sess.LastNotifiedStatus != noPRReconciledStatus {
			t.Fatalf("%s = status %q closed_at=%v notified=%q, want reconciled done", slot, sess.Status, sess.IssueClosedAt, sess.LastNotifiedStatus)
		}
	}
	if !strings.Contains(logs.String(), "already closed; reconciled 3 stale no-PR slot(s) to done") {
		t.Fatalf("journal missing closed-issue reconciliation reason:\n%s", logs.String())
	}

	logBefore := logs.Len()
	o.autoMergePRs(s)
	if labelCalls != 0 || len(projectStatuses) != 0 || logs.Len() != logBefore {
		t.Fatalf("second cycle was not quiet: labels=%d statuses=%v logs=%q", labelCalls, projectStatuses, logs.String()[logBefore:])
	}
}

func TestAutoMergePRs_NoPRRetryExhaustedMergedSiblingSettlesAllStaleSlots(t *testing.T) {
	cfg := &config.Config{
		Repo:       "owner/repo",
		Supervisor: config.SupervisorConfig{BlockedLabel: "blocked"},
	}
	labelCalls := 0
	o := &Orchestrator{
		cfg:             cfg,
		notifier:        &notify.Notifier{},
		listOpenPRsFn:   func() ([]github.PR, error) { return nil, nil },
		isIssueClosedFn: func(issueNumber int) (bool, error) { return false, nil },
		isPRMergedFn:    func(prNumber int) (bool, error) { return prNumber == 850, nil },
		addIssueLabelFn: func(number int, label string) error {
			labelCalls++
			return nil
		},
		hasMergedPRForIssueFn: func(issueNumber int) (bool, error) {
			t.Fatalf("same-issue session PR identity should establish aggregate merge state")
			return false, nil
		},
	}
	s := state.NewState()
	for _, slot := range []string{"slot-a", "slot-b", "slot-c"} {
		s.Sessions[slot] = &state.Session{
			IssueNumber: 835,
			Status:      state.StatusRetryExhausted,
			Branch:      slot,
		}
	}
	// The canonical slot already settled after its non-closing `Refs #835` PR
	// merged. The stale no-PR slots must use that durable PR identity rather
	// than the legacy closing-keyword lookup.
	s.Sessions["slot-d"] = &state.Session{
		IssueNumber: 835,
		Status:      state.StatusDone,
		PRNumber:    850,
		PRMerged:    true,
		Branch:      "slot-d",
	}

	o.autoMergePRs(s)

	if labelCalls != 0 {
		t.Fatalf("merged sibling PR still allowed %d blocked-label call(s)", labelCalls)
	}
	for _, slot := range []string{"slot-a", "slot-b", "slot-c"} {
		sess := s.Sessions[slot]
		if sess.Status != state.StatusDone || sess.LastNotifiedStatus != noPRReconciledStatus {
			t.Fatalf("%s = status %q notified=%q, want reconciled done", slot, sess.Status, sess.LastNotifiedStatus)
		}
	}

	o.autoMergePRs(s)
	if labelCalls != 0 {
		t.Fatalf("second cycle re-applied blocked label after merged sibling reconciliation")
	}
}

// #577: when a merged PR already closes the issue (closing-keyword link),
// the reconciler should auto-close the issue if close_issue is allowed as a
// safe action without an approval gate. The dynamic-wave queue then sees
// the issue as closed and advances.
func TestAutoMergePRs_NoPRRetryExhaustedAutoClosesWhenMergedPRDetected(t *testing.T) {
	cfg := &config.Config{
		Repo: "owner/repo",
		Supervisor: config.SupervisorConfig{
			BlockedLabel: "blocked",
			SafeActions:  []string{config.SupervisorActionCloseIssue},
		},
	}
	closed := make(map[int]string)
	labelled := 0
	o := &Orchestrator{
		cfg:           cfg,
		notifier:      &notify.Notifier{},
		listOpenPRsFn: func() ([]github.PR, error) { return nil, nil },
		isIssueClosedFn: func(issueNumber int) (bool, error) {
			return false, nil
		},
		hasMergedPRForIssueFn: func(issueNumber int) (bool, error) {
			return issueNumber == 490, nil
		},
		ghCloseIssueFn: func(number int, comment string) error {
			closed[number] = comment
			return nil
		},
		addIssueLabelFn: func(number int, label string) error {
			labelled++
			return nil
		},
	}
	s := state.NewState()
	s.Sessions["sup-9"] = &state.Session{
		IssueNumber: 490,
		IssueTitle:  "merged via Refs only",
		Branch:      "feat/sup-9-merged-elsewhere",
		Status:      state.StatusRetryExhausted,
		PRNumber:    0,
	}

	o.autoMergePRs(s)

	if _, ok := closed[490]; !ok {
		t.Fatalf("issue #490 not auto-closed; closed=%v (merged PR detected + close_issue safe action must close)", closed)
	}
	if labelled != 0 {
		t.Fatalf("blocked label was applied %d times, want 0 (auto-close branch must not also label)", labelled)
	}
	if sess := s.Sessions["sup-9"]; sess.LastNotifiedStatus != noPRReconciledStatus {
		t.Fatalf("LastNotifiedStatus = %q, want %q", sess.LastNotifiedStatus, noPRReconciledStatus)
	}
}

// #577: when close_issue is not granted as a safe action (or requires
// approval) the reconciler must surface the merged-elsewhere issue as a
// close-candidate without silently re-spawning workers — it must still
// idempotently mark the session reconciled so the queue advances.
func TestAutoMergePRs_NoPRRetryExhaustedMergedPRSurfacesCloseCandidate(t *testing.T) {
	cfg := &config.Config{
		Repo: "owner/repo",
		Supervisor: config.SupervisorConfig{
			BlockedLabel: "blocked",
			ApprovalRequired: []string{
				config.SupervisorActionCloseIssue,
			},
		},
	}
	closeCalls := 0
	labelled := 0
	o := &Orchestrator{
		cfg:           cfg,
		notifier:      &notify.Notifier{},
		listOpenPRsFn: func() ([]github.PR, error) { return nil, nil },
		isIssueClosedFn: func(issueNumber int) (bool, error) {
			return false, nil
		},
		hasMergedPRForIssueFn: func(issueNumber int) (bool, error) {
			return true, nil
		},
		ghCloseIssueFn: func(number int, comment string) error {
			closeCalls++
			return nil
		},
		addIssueLabelFn: func(number int, label string) error {
			labelled++
			return nil
		},
	}
	s := state.NewState()
	s.Sessions["sup-11"] = &state.Session{
		IssueNumber: 491,
		IssueTitle:  "merged via Refs; close gated",
		Branch:      "feat/sup-11",
		Status:      state.StatusRetryExhausted,
		PRNumber:    0,
	}

	o.autoMergePRs(s)

	if closeCalls != 0 {
		t.Fatalf("close_issue without safe-action grant must not auto-close; calls=%d", closeCalls)
	}
	if labelled != 0 {
		t.Fatalf("merged-elsewhere branch must not apply blocked label; labelled=%d", labelled)
	}
	if sess := s.Sessions["sup-11"]; sess.LastNotifiedStatus != noPRReconciledStatus {
		t.Fatalf("LastNotifiedStatus = %q, want %q (must still mark reconciled to stop log spam)", sess.LastNotifiedStatus, noPRReconciledStatus)
	}
}

// #577: a transient GitHub failure when probing for a merged PR must NOT
// mark the session reconciled — the next cycle should try again instead of
// silently dropping the issue.
func TestAutoMergePRs_NoPRRetryExhaustedHasMergedPRErrorDefersReconcile(t *testing.T) {
	cfg := &config.Config{
		Repo: "owner/repo",
		Supervisor: config.SupervisorConfig{
			BlockedLabel: "blocked",
		},
	}
	o := &Orchestrator{
		cfg:           cfg,
		notifier:      &notify.Notifier{},
		listOpenPRsFn: func() ([]github.PR, error) { return nil, nil },
		isIssueClosedFn: func(issueNumber int) (bool, error) {
			return false, nil
		},
		hasMergedPRForIssueFn: func(issueNumber int) (bool, error) {
			return false, fmt.Errorf("gh: transient API failure")
		},
		addIssueLabelFn: func(number int, label string) error {
			t.Fatalf("addIssueLabel(%d, %q) must not run when merged-PR probe failed", number, label)
			return nil
		},
		ghCloseIssueFn: func(number int, comment string) error {
			t.Fatalf("closeIssue(%d) must not run when merged-PR probe failed", number)
			return nil
		},
	}
	s := state.NewState()
	s.Sessions["sup-13"] = &state.Session{
		IssueNumber: 492,
		IssueTitle:  "transient gh failure",
		Branch:      "feat/sup-13",
		Status:      state.StatusRetryExhausted,
		PRNumber:    0,
	}

	o.autoMergePRs(s)

	if sess := s.Sessions["sup-13"]; sess.LastNotifiedStatus == noPRReconciledStatus {
		t.Fatalf("transient probe failure must not mark reconciled; LastNotifiedStatus=%q", sess.LastNotifiedStatus)
	}
}

// #818: a retry_exhausted session whose PR was closed without merge must be
// released for fresh dispatch. The session goes terminal (failed) with its
// recorded PR cleared, which drops the IssueRetryExhausted slot-hold and makes
// the attempt count via FailedAttemptsForIssue (so re-dispatch stays subject to
// max_retries_per_issue). No blocked label is applied: only the supervisor
// removes that label, and #818 must clear without the supervisor.
func TestAutoMergePRs_ClosedPRRetryExhaustedReleasesIssueForFreshDispatch(t *testing.T) {
	cfg := &config.Config{
		Repo: "owner/repo",
		Supervisor: config.SupervisorConfig{
			BlockedLabel: "blocked",
		},
	}
	addedLabels := make(map[int]string)
	o := &Orchestrator{
		cfg:           cfg,
		notifier:      &notify.Notifier{},
		listOpenPRsFn: func() ([]github.PR, error) { return nil, nil },
		isPRMergedFn: func(prNumber int) (bool, error) {
			return false, nil
		},
		addIssueLabelFn: func(number int, label string) error {
			addedLabels[number] = label
			return nil
		},
	}
	s := state.NewState()
	s.Sessions["sup-20"] = &state.Session{
		IssueNumber: 500,
		IssueTitle:  "PR closed without merge",
		Branch:      "feat/sup-20",
		Status:      state.StatusRetryExhausted,
		PRNumber:    218,
	}

	o.autoMergePRs(s)

	if len(addedLabels) != 0 {
		t.Fatalf("blocked label applied %v, want none (closed-PR retry_exhausted must release, not block)", addedLabels)
	}
	sess := s.Sessions["sup-20"]
	if sess.Status != state.StatusFailed {
		t.Fatalf("status = %q, want %q (closed-unmerged retry_exhausted must go failed)", sess.Status, state.StatusFailed)
	}
	if sess.PRNumber != 0 {
		t.Fatalf("PRNumber = %d, want 0 (closed PR must be cleared so the attempt counts and the issue re-dispatches)", sess.PRNumber)
	}
	if sess.LastClosedPRNumber != 218 {
		t.Fatalf("LastClosedPRNumber = %d, want 218", sess.LastClosedPRNumber)
	}
	if !sess.ReleasedForRedispatch {
		t.Fatalf("ReleasedForRedispatch = false, want true (released session must mirror as runnable Todo, not Blocked)")
	}
	// #818: the released StatusFailed session must map to a runnable board
	// status. Otherwise reconcileSessionsToProjectBoard re-pushes Blocked over
	// the Todo the reconcile set, and the dynamic wave (Blocked = non-runnable)
	// re-strands the issue when a fresh worker does not start the same cycle.
	if status, ok := projectStatusForSession(sess, false); !ok || status != github.ProjectStatusTodo {
		t.Fatalf("projectStatusForSession(released) = (%q, %v), want (%q, true)", status, ok, github.ProjectStatusTodo)
	}
	if sess.FinishedAt == nil {
		t.Fatalf("FinishedAt must be set on a reconciled terminal session")
	}
	if s.IssueRetryExhausted(500) {
		t.Fatalf("issue #500 still retry-exhausted; slot not released")
	}
	if got := s.FailedAttemptsForIssue(500); got != 1 {
		t.Fatalf("FailedAttemptsForIssue(500) = %d, want 1 (attempt must count toward max_retries_per_issue)", got)
	}
	// The released session ages out of the live window instead of staying live
	// forever (#818 / no permanent "waiting for reconciliation" state).
	aged := sess.FinishedAt.Add(state.LiveSessionRecentWindow + time.Hour)
	if state.SessionLiveAt(sess, aged) {
		t.Fatalf("failed session still live after the aging window; must not be a permanent live state")
	}

	// Second cycle is a structural no-op: a failed session is not merge-flow
	// eligible, so the reconciler does not run again.
	clear(addedLabels)
	o.autoMergePRs(s)
	if len(addedLabels) != 0 {
		t.Fatalf("second cycle applied labels = %v, want none (idempotency)", addedLabels)
	}
	if s.Sessions["sup-20"].Status != state.StatusFailed {
		t.Fatalf("second cycle changed status to %q, want failed (idempotency)", s.Sessions["sup-20"].Status)
	}
}

// #818: a retry_exhausted session whose PR was merged settles the session done
// (so it stops being reported live/needs_attention and releases the slot) and
// auto-closes the issue when close_issue is a safe action.
func TestAutoMergePRs_ClosedPRRetryExhaustedAutoClosesWhenMerged(t *testing.T) {
	cfg := &config.Config{
		Repo: "owner/repo",
		Supervisor: config.SupervisorConfig{
			BlockedLabel: "blocked",
			SafeActions:  []string{config.SupervisorActionCloseIssue},
		},
	}
	closed := make(map[int]string)
	labelled := 0
	o := &Orchestrator{
		cfg:           cfg,
		notifier:      &notify.Notifier{},
		listOpenPRsFn: func() ([]github.PR, error) { return nil, nil },
		isPRMergedFn: func(prNumber int) (bool, error) {
			return prNumber == 220, nil
		},
		ghCloseIssueFn: func(number int, comment string) error {
			closed[number] = comment
			return nil
		},
		addIssueLabelFn: func(number int, label string) error {
			labelled++
			return nil
		},
	}
	s := state.NewState()
	s.Sessions["sup-22"] = &state.Session{
		IssueNumber: 501,
		IssueTitle:  "PR merged after retry exhaust",
		Branch:      "feat/sup-22",
		Status:      state.StatusRetryExhausted,
		PRNumber:    220,
	}

	o.autoMergePRs(s)

	if _, ok := closed[501]; !ok {
		t.Fatalf("issue #501 not auto-closed; closed=%v (merged PR + close_issue safe action must close)", closed)
	}
	if labelled != 0 {
		t.Fatalf("blocked label applied %d times, want 0 (merged branch must not label)", labelled)
	}
	sess := s.Sessions["sup-22"]
	if sess.Status != state.StatusDone {
		t.Fatalf("status = %q, want %q (merged retry_exhausted must settle done)", sess.Status, state.StatusDone)
	}
	if sess.FinishedAt == nil {
		t.Fatalf("FinishedAt must be set on a settled-done session")
	}
	if s.IssueRetryExhausted(501) {
		t.Fatalf("issue #501 still retry-exhausted after merge; slot not released")
	}
	// A done session no longer flags for attention and ages out of the live
	// window — it is not a permanent live/needs_attention state (#818).
	if state.SessionAttentionForAt(sess, nil, *sess.FinishedAt).NeedsAttention {
		t.Fatalf("done session must not need attention")
	}
	aged := sess.FinishedAt.Add(state.LiveSessionRecentWindow + time.Hour)
	if state.SessionLiveAt(sess, aged) {
		t.Fatalf("done session still live after the aging window; must not be a permanent live state")
	}
}

// #818: a transient GitHub failure when checking if the PR was merged must NOT
// transition the session — it stays retry_exhausted so reconcile retries next
// cycle rather than settling the outcome on a guess.
func TestAutoMergePRs_ClosedPRRetryExhaustedMergeCheckErrorDefersReconcile(t *testing.T) {
	cfg := &config.Config{
		Repo: "owner/repo",
		Supervisor: config.SupervisorConfig{
			BlockedLabel: "blocked",
		},
	}
	o := &Orchestrator{
		cfg:           cfg,
		notifier:      &notify.Notifier{},
		listOpenPRsFn: func() ([]github.PR, error) { return nil, nil },
		isPRMergedFn: func(prNumber int) (bool, error) {
			return false, fmt.Errorf("gh: transient API failure")
		},
		addIssueLabelFn: func(number int, label string) error {
			t.Fatalf("addIssueLabel(%d, %q) must not run when merge probe failed", number, label)
			return nil
		},
	}
	s := state.NewState()
	s.Sessions["sup-24"] = &state.Session{
		IssueNumber: 502,
		IssueTitle:  "transient gh failure on closed PR",
		Branch:      "feat/sup-24",
		Status:      state.StatusRetryExhausted,
		PRNumber:    221,
	}

	o.autoMergePRs(s)

	sess := s.Sessions["sup-24"]
	if sess.Status != state.StatusRetryExhausted {
		t.Fatalf("status = %q, want %q (transient probe failure must not transition the session)", sess.Status, state.StatusRetryExhausted)
	}
	if sess.PRNumber != 221 {
		t.Fatalf("PRNumber = %d, want 221 (must not be cleared on probe failure)", sess.PRNumber)
	}
}

// #818 follow-up: a merged retry_exhausted session whose auto-close hits a
// transient GitHub failure must NOT settle done — it stays retry_exhausted so
// the next cycle retries the close (otherwise the issue can stay open forever
// because the settled session leaves the merge flow). The retry must not
// re-post the close comment, and once the close succeeds the session settles.
func TestAutoMergePRs_ClosedPRRetryExhaustedTransientCloseFailureRetries(t *testing.T) {
	cfg := &config.Config{
		Repo: "owner/repo",
		Supervisor: config.SupervisorConfig{
			BlockedLabel: "blocked",
			SafeActions:  []string{config.SupervisorActionCloseIssue},
		},
	}
	var closeComments []string
	closeErr := fmt.Errorf("gh: transient API failure")
	o := &Orchestrator{
		cfg:           cfg,
		notifier:      &notify.Notifier{},
		listOpenPRsFn: func() ([]github.PR, error) { return nil, nil },
		isPRMergedFn: func(prNumber int) (bool, error) {
			return prNumber == 223, nil
		},
		ghCloseIssueFn: func(number int, comment string) error {
			closeComments = append(closeComments, comment)
			return closeErr
		},
	}
	s := state.NewState()
	s.Sessions["sup-26"] = &state.Session{
		IssueNumber: 503,
		IssueTitle:  "merged PR, transient close failure",
		Branch:      "feat/sup-26",
		Status:      state.StatusRetryExhausted,
		PRNumber:    223,
	}

	// Cycle 1: close fails transiently — session must stay retry_exhausted.
	o.autoMergePRs(s)

	sess := s.Sessions["sup-26"]
	if sess.Status != state.StatusRetryExhausted {
		t.Fatalf("status = %q, want %q (transient close failure must not settle done)", sess.Status, state.StatusRetryExhausted)
	}
	if sess.PRNumber != 223 {
		t.Fatalf("PRNumber = %d, want 223 (recorded PR must survive so reconcile retries the close)", sess.PRNumber)
	}
	if len(closeComments) != 1 {
		t.Fatalf("close attempts = %d, want 1", len(closeComments))
	}
	if closeComments[0] == "" {
		t.Fatalf("first close attempt posted an empty comment, want the close note")
	}
	if sess.LastNotifiedStatus != closedPRCloseRetryStatus {
		t.Fatalf("LastNotifiedStatus = %q, want %q (marker gates comment/notify re-spam)", sess.LastNotifiedStatus, closedPRCloseRetryStatus)
	}

	// Cycle 2: still transient — retry must not re-post the close comment.
	o.autoMergePRs(s)
	if len(closeComments) != 2 {
		t.Fatalf("close attempts after retry = %d, want 2 (close must be retried)", len(closeComments))
	}
	if closeComments[1] != "" {
		t.Fatalf("retry close comment = %q, want empty (must not re-post the close note)", closeComments[1])
	}
	if s.Sessions["sup-26"].Status != state.StatusRetryExhausted {
		t.Fatalf("status = %q, want %q while close keeps failing", s.Sessions["sup-26"].Status, state.StatusRetryExhausted)
	}

	// Cycle 3: close succeeds — session settles done and releases the slot.
	closeErr = nil
	o.autoMergePRs(s)
	if s.Sessions["sup-26"].Status != state.StatusDone {
		t.Fatalf("status = %q, want %q after the close succeeds", s.Sessions["sup-26"].Status, state.StatusDone)
	}
	if s.IssueRetryExhausted(503) {
		t.Fatalf("issue #503 still retry-exhausted after close succeeded; slot not released")
	}
}

// #730: a Pi-backed session's JSON event stream is parsed for usage, and the
// orchestrator stamps model/tokens/cost_usd onto the session instead of the
// generic token regex (which sees none of the JSON shapes).
func TestUpdateTokensUsedFromOutput_PiBackendStampsModelTokensCost(t *testing.T) {
	const piLog = `{"type":"session","version":3,"id":"abc"}
{"type":"turn_end","message":{"role":"assistant","provider":"ollama","model":"glm-5.2:cloud","usage":{"input":770,"output":3,"cacheRead":0,"cacheWrite":0,"totalTokens":773,"cost":{"input":0.001078,"output":0.0000132,"total":0.0010912}}},"toolResults":[]}
`
	o := &Orchestrator{
		cfg: &config.Config{
			StateDir: t.TempDir(),
			Model: config.ModelConfig{
				Default: "pi-ollama",
				Backends: map[string]config.BackendDef{
					"pi-ollama": {Provider: "ollama", Cmd: "pi", Model: "glm-5.2:cloud"},
				},
			},
		},
	}
	sess := &state.Session{Backend: "pi-ollama"}

	changed := o.updateTokensUsedFromOutput("sup-730", sess, piLog)
	if !changed {
		t.Fatal("expected updateTokensUsedFromOutput to report a change for the Pi event stream")
	}
	if sess.TokensUsedAttempt != 773 {
		t.Errorf("TokensUsedAttempt = %d, want 773", sess.TokensUsedAttempt)
	}
	if sess.TokensUsedTotal != 773 {
		t.Errorf("TokensUsedTotal = %d, want 773", sess.TokensUsedTotal)
	}
	if sess.Model != "glm-5.2:cloud" {
		t.Errorf("Model = %q, want glm-5.2:cloud", sess.Model)
	}
	if sess.CostUSDBackend < 0.0010911 || sess.CostUSDBackend > 0.0010913 {
		t.Errorf("CostUSDBackend = %v, want ~0.0010912", sess.CostUSDBackend)
	}
}

// #739: a Pi-backed session's usage stream carries the cache-aware split
// (input/output/cacheRead/cacheWrite); the orchestrator stamps each dimension
// onto the session so the cost panel can apply the cache-read discount.
func TestUpdateTokensUsedFromOutput_PiBackendStampsSplitTokens(t *testing.T) {
	// Cache-heavy turn: the bulk of the tokens are reused cache_read.
	const piLog = `{"type":"turn_end","message":{"role":"assistant","provider":"ollama","model":"glm-5.2:cloud","usage":{"input":1000,"output":500,"cacheRead":50000,"cacheWrite":2000,"totalTokens":53500,"cost":{"input":0,"output":0,"total":0}}}}
`
	o := &Orchestrator{
		cfg: &config.Config{
			StateDir: t.TempDir(),
			Model: config.ModelConfig{
				Default: "pi-ollama",
				Backends: map[string]config.BackendDef{
					"pi-ollama": {Provider: "ollama", Cmd: "pi", Model: "glm-5.2:cloud"},
				},
			},
		},
	}
	sess := &state.Session{Backend: "pi-ollama"}

	if !o.updateTokensUsedFromOutput("sup-739", sess, piLog) {
		t.Fatal("expected a change for the Pi event stream")
	}
	if sess.TokensUsedTotal != 53500 {
		t.Errorf("TokensUsedTotal = %d, want 53500 (combined total preserved)", sess.TokensUsedTotal)
	}
	if sess.TokensInput != 1000 {
		t.Errorf("TokensInput = %d, want 1000", sess.TokensInput)
	}
	if sess.TokensOutput != 500 {
		t.Errorf("TokensOutput = %d, want 500", sess.TokensOutput)
	}
	if sess.TokensCacheRead != 50000 {
		t.Errorf("TokensCacheRead = %d, want 50000", sess.TokensCacheRead)
	}
	if sess.TokensCacheWrite != 2000 {
		t.Errorf("TokensCacheWrite = %d, want 2000", sess.TokensCacheWrite)
	}
	if !sess.HasSplitTokens() {
		t.Error("HasSplitTokens() = false, want true after stamping a split")
	}
}

// #730: a non-Pi backend (claude/codex) keeps the generic token parser — the
// Pi usage path must not engage just because the log happens to contain JSON.
func TestUpdateTokensUsedFromOutput_ClaudeBackendKeepsGenericParser(t *testing.T) {
	const claudeLog = "worker output\n\ntokens used\n100,965\nImplemented and opened PR\n"
	o := &Orchestrator{
		cfg: &config.Config{
			Model: config.ModelConfig{
				Default: "claude",
				Backends: map[string]config.BackendDef{
					"claude": {Cmd: "claude"},
				},
			},
		},
	}
	sess := &state.Session{Backend: "claude"}
	changed := o.updateTokensUsedFromOutput("sup-150", sess, claudeLog)
	if !changed {
		t.Fatal("expected generic parser to capture tokens")
	}
	if sess.TokensUsedAttempt != 100965 {
		t.Errorf("TokensUsedAttempt = %d, want 100965", sess.TokensUsedAttempt)
	}
	if sess.Model != "" {
		t.Errorf("Model = %q, want empty (claude does not self-report a model here)", sess.Model)
	}
	if sess.CostUSDBackend != 0 {
		t.Errorf("CostUSDBackend = %v, want 0", sess.CostUSDBackend)
	}
}

// approxCostEq compares two USD floats with a 1e-9 tolerance (avoids pulling
// in math just for cost assertions in the Pi usage tests).
func approxCostEq(a, b float64) bool {
	d := a - b
	if d < 0 {
		d = -d
	}
	return d <= 1e-9
}

// #730/#732: Pi re-parses the full appended slot log on every call, so
// usage.TotalTokens + CostUSD are cumulative run totals. Cost must keep
// updating past the first non-zero turn (the old `== 0` freeze would
// permanently report the first turn's cost) — guard with `>` like tokens.
func TestUpdateTokensUsedFromOutput_PiBackendCostGrowsPastFirstTurn(t *testing.T) {
	const turn1 = `{"type":"turn_end","message":{"role":"assistant","provider":"ollama","model":"glm-5.2:cloud","usage":{"input":770,"output":3,"cacheRead":0,"cacheWrite":0,"totalTokens":773,"cost":{"input":0.001078,"output":0.0000132,"total":0.0010912}}}}
`
	// Second parse simulates the appended log now containing two turns:
	// turn 1 (773 tokens, $0.0010912) + turn 2 (2220 tokens, $0.003168).
	const turn1And2 = turn1 + `{"type":"turn_end","message":{"role":"assistant","provider":"ollama","model":"glm-5.2:cloud","usage":{"input":2200,"output":20,"cacheRead":0,"cacheWrite":0,"totalTokens":2220,"cost":{"input":0.00308,"output":0.000088,"total":0.003168}}}}
`
	o := &Orchestrator{
		cfg: &config.Config{
			StateDir: t.TempDir(),
			Model: config.ModelConfig{
				Default: "pi-ollama",
				Backends: map[string]config.BackendDef{
					"pi-ollama": {Provider: "ollama", Cmd: "pi", Model: "glm-5.2:cloud"},
				},
			},
		},
	}
	sess := &state.Session{Backend: "pi-ollama"}

	if !o.updateTokensUsedFromOutput("sup-732", sess, turn1) {
		t.Fatal("first turn: expected a change")
	}
	if sess.TokensUsedAttempt != 773 || sess.TokensUsedTotal != 773 {
		t.Fatalf("after turn1: attempt=%d total=%d, want 773/773", sess.TokensUsedAttempt, sess.TokensUsedTotal)
	}
	if sess.UsageTokensWatermark != 773 {
		t.Errorf("UsageTokensWatermark = %d, want 773", sess.UsageTokensWatermark)
	}
	if !approxCostEq(sess.CostUSDBackend, 0.0010912) {
		t.Errorf("after turn1: CostUSDBackend = %v, want 0.0010912", sess.CostUSDBackend)
	}

	// Re-parsing the appended log (cumulative) must update cost past the
	// first turn instead of freezing at $0.0010912.
	if !o.updateTokensUsedFromOutput("sup-732", sess, turn1And2) {
		t.Fatal("second turn: expected a change (cost/tokens must grow)")
	}
	wantCost := 0.0010912 + 0.003168
	if !approxCostEq(sess.CostUSDBackend, wantCost) {
		t.Errorf("after turn2: CostUSDBackend = %v, want %v (frozen-cost bug)", sess.CostUSDBackend, wantCost)
	}
	// Cumulative tokens = 773 + 2220 = 2993; only the delta (2220) is added.
	if sess.TokensUsedAttempt != 2993 || sess.TokensUsedTotal != 2993 {
		t.Errorf("after turn2: attempt=%d total=%d, want 2993/2993", sess.TokensUsedAttempt, sess.TokensUsedTotal)
	}
	if sess.UsageTokensWatermark != 2993 {
		t.Errorf("UsageTokensWatermark = %d, want 2993", sess.UsageTokensWatermark)
	}
}

// #730/#732: on respawn/checkpoint resume the runner keeps appending to the
// same slot log, so the full-log parse returns the all-attempt cumulative
// token count. TokensUsedAttempt resets to 0 but the UsageTokensWatermark
// persists, so only the new attempt's delta is added — the prior attempts'
// tokens are NOT re-counted into TokensUsedTotal.
func TestUpdateTokensUsedFromOutput_PiBackendRetryNoDoubleCount(t *testing.T) {
	// Attempt 1: single turn, 773 tokens. Watermark + total = 773.
	const attempt1 = `{"type":"turn_end","message":{"role":"assistant","provider":"ollama","model":"glm-5.2:cloud","usage":{"input":770,"output":3,"cacheRead":0,"cacheWrite":0,"totalTokens":773,"cost":{"total":0.0010912}}}}
`
	// Attempt 2: the appended log now carries the original turn (773) plus a
	// NEW turn (1000 tokens). The full-log parse yields cumulative 1773.
	const attempt1And2 = attempt1 + `{"type":"turn_end","message":{"role":"assistant","provider":"ollama","model":"glm-5.2:cloud","usage":{"input":900,"output":100,"cacheRead":0,"cacheWrite":0,"totalTokens":1000,"cost":{"total":0.002}}}}
`

	o := &Orchestrator{
		cfg: &config.Config{
			StateDir: t.TempDir(),
			Model: config.ModelConfig{
				Default: "pi-ollama",
				Backends: map[string]config.BackendDef{
					"pi-ollama": {Provider: "ollama", Cmd: "pi", Model: "glm-5.2:cloud"},
				},
			},
		},
	}
	sess := &state.Session{Backend: "pi-ollama"}

	if !o.updateTokensUsedFromOutput("sup-732", sess, attempt1) {
		t.Fatal("attempt1: expected a change")
	}
	if sess.TokensUsedTotal != 773 || sess.UsageTokensWatermark != 773 || sess.TokensUsedAttempt != 773 {
		t.Fatalf("attempt1: total=%d wm=%d attempt=%d, want 773/773/773",
			sess.TokensUsedTotal, sess.UsageTokensWatermark, sess.TokensUsedAttempt)
	}
	if sess.TokenBudgetTokensAttempt != 773 || sess.TokenBudgetTokensWatermark != 773 || sess.TokenBudgetMeasure != worker.TokenBudgetMeasureUncached {
		t.Fatalf("attempt1 budget=%d watermark=%d measure=%q, want 773/773/%q", sess.TokenBudgetTokensAttempt, sess.TokenBudgetTokensWatermark, sess.TokenBudgetMeasure, worker.TokenBudgetMeasureUncached)
	}

	// Simulate respawn: per-attempt counter resets to 0 (checkpoint/worker/phase
	// reset it), but UsageTokensWatermark persists across respawns.
	sess.TokensUsedAttempt = 0
	sess.TokenBudgetTokensAttempt = 0

	// Re-parse the SAME appended log (cumulative 773) before the new turn
	// lands — must NOT re-add the prior attempt's tokens.
	if o.updateTokensUsedFromOutput("sup-732", sess, attempt1) {
		t.Fatal("re-parse of unchanged cumulative log must not report a change (would double-count)")
	}
	if sess.TokensUsedTotal != 773 {
		t.Fatalf("after re-parse: TokensUsedTotal = %d, want 773 (no double-count)", sess.TokensUsedTotal)
	}
	if sess.TokensUsedAttempt != 0 {
		t.Errorf("TokensUsedAttempt = %d, want 0 (no delta yet)", sess.TokensUsedAttempt)
	}

	// New turn lands: cumulative 1773. Only the delta (1000) is added.
	if !o.updateTokensUsedFromOutput("sup-732", sess, attempt1And2) {
		t.Fatal("new turn: expected a change")
	}
	if sess.TokensUsedTotal != 1773 {
		t.Errorf("after new turn: TokensUsedTotal = %d, want 1773 (delta only)", sess.TokensUsedTotal)
	}
	if sess.TokensUsedAttempt != 1000 {
		t.Errorf("TokensUsedAttempt = %d, want 1000 (current attempt delta)", sess.TokensUsedAttempt)
	}
	if sess.TokenBudgetTokensAttempt != 1000 {
		t.Errorf("TokenBudgetTokensAttempt = %d, want 1000", sess.TokenBudgetTokensAttempt)
	}
	if sess.UsageTokensWatermark != 1773 {
		t.Errorf("UsageTokensWatermark = %d, want 1773", sess.UsageTokensWatermark)
	}
}

// claudeResultFrame builds one terminal claude stream-json `result` frame with
// the given token totals + cost, as the stream-splitter would append it.
func claudeResultFrame(in, out, cacheWrite, cacheRead int, cost float64, result string) string {
	return fmt.Sprintf(`{"type":"result","subtype":"success","is_error":false,"num_turns":1,"result":%q,"total_cost_usd":%g,"usage":{"input_tokens":%d,"output_tokens":%d,"cache_creation_input_tokens":%d,"cache_read_input_tokens":%d}}`+"\n",
		result, cost, in, out, cacheWrite, cacheRead)
}

// claudeTestOrchestrator wires a session to a claude backend whose slot.jsonl
// lives at <dir>/<slot>.jsonl, returning the orchestrator, session, and jsonl
// path. The session LogFile points at the sibling .log so the orchestrator
// derives the .jsonl side channel (#737).
func claudeTestOrchestrator(t *testing.T, slot string) (*Orchestrator, *state.Session, string) {
	t.Helper()
	dir := t.TempDir()
	o := &Orchestrator{
		cfg: &config.Config{
			StateDir: dir,
			Model: config.ModelConfig{
				Default:  "claude",
				Backends: map[string]config.BackendDef{"claude": {Cmd: "claude", UsageStream: true}},
			},
		},
	}
	logFile := dir + "/" + slot + ".log"
	sess := &state.Session{
		Backend: "claude",
		LogFile: logFile,
		Attribution: []state.BackendAttribution{{
			Backend:   "claude",
			StartedAt: time.Now().UTC(),
		}},
	}
	return o, sess, dir + "/" + slot + ".jsonl"
}

// #737: a claude session reads usage from the slot.jsonl side channel (not the
// human-readable slot.log) and stamps non-zero tokens + USD cost + model.
func TestUpdateTokensUsedFromOutput_ClaudeBackendStampsUsage(t *testing.T) {
	o, sess, jsonlPath := claudeTestOrchestrator(t, "sup-737")
	frames := `{"type":"system","subtype":"init","model":"claude-opus-4-8[1m]"}` + "\n" +
		claudeResultFrame(2487, 4, 1924, 15661, 0.0401785, "pong")
	if err := os.WriteFile(jsonlPath, []byte(frames), 0644); err != nil {
		t.Fatal(err)
	}

	// The passed output (slot.log/tmux) is intentionally ignored for claude.
	if !o.updateTokensUsedFromOutput("sup-737", sess, "human readable log text, no token total") {
		t.Fatal("expected a change from the claude jsonl side channel")
	}
	// 2487 + 4 + 1924 + 15661 = 20076
	if sess.TokensUsedTotal != 20076 || sess.TokensUsedAttempt != 20076 {
		t.Errorf("tokens total=%d attempt=%d, want 20076/20076", sess.TokensUsedTotal, sess.TokensUsedAttempt)
	}
	if sess.UsageTokensWatermark != 20076 {
		t.Errorf("UsageTokensWatermark = %d, want 20076", sess.UsageTokensWatermark)
	}
	if sess.TokenBudgetTokensAttempt != 4415 || sess.TokenBudgetTokensWatermark != 4415 || sess.TokenBudgetMeasure != worker.TokenBudgetMeasureUncached {
		t.Errorf("budget usage=%d watermark=%d measure=%q, want 4415/4415/%q", sess.TokenBudgetTokensAttempt, sess.TokenBudgetTokensWatermark, sess.TokenBudgetMeasure, worker.TokenBudgetMeasureUncached)
	}
	if !approxCostEq(sess.CostUSDBackend, 0.0401785) {
		t.Errorf("CostUSDBackend = %v, want 0.0401785", sess.CostUSDBackend)
	}
	if sess.Model != "claude-opus-4-8[1m]" {
		t.Errorf("Model = %q, want claude-opus-4-8[1m]", sess.Model)
	}
	// #739: the cache-aware split is stamped per dimension so the cost panel
	// can apply the cache-read discount (in=2487, out=4, cacheWrite=1924,
	// cacheRead=15661 per the claudeResultFrame arg order).
	if sess.TokensInput != 2487 || sess.TokensOutput != 4 {
		t.Errorf("input/output = %d/%d, want 2487/4", sess.TokensInput, sess.TokensOutput)
	}
	if sess.TokensCacheRead != 15661 || sess.TokensCacheWrite != 1924 {
		t.Errorf("cache read/write = %d/%d, want 15661/1924", sess.TokensCacheRead, sess.TokensCacheWrite)
	}
	if !sess.HasSplitTokens() {
		t.Error("HasSplitTokens() = false, want true")
	}
}

// A translated upstream that completes successfully but emits zero usage is
// explicit degraded observability, never zero-token progress. The worker stays
// alive/terminal according to its own lifecycle; token budget enforcement does
// not synthesize a kill from missing data.
func TestUpdateTokensUsedFromOutput_ClaudeZeroUsageMarksAttributionWithoutProgress(t *testing.T) {
	o, sess, jsonlPath := claudeTestOrchestrator(t, "sup-946")
	frames := `{"type":"system","subtype":"init","model":"proxy-model"}` + "\n" +
		`{"type":"result","subtype":"success","result":"done","usage":{"input_tokens":0,"output_tokens":0}}` + "\n"
	if err := os.WriteFile(jsonlPath, []byte(frames), 0644); err != nil {
		t.Fatal(err)
	}

	if !o.updateTokensUsedFromOutput("sup-946", sess, "healthy rendered output") {
		t.Fatal("expected the reliability marker to change durable state")
	}
	if sess.TokensUsedAttempt != 0 || sess.TokensUsedTotal != 0 || sess.UsageTokensWatermark != 0 {
		t.Fatalf("zero usage advanced token progress: attempt=%d total=%d watermark=%d",
			sess.TokensUsedAttempt, sess.TokensUsedTotal, sess.UsageTokensWatermark)
	}
	seg := sess.Attribution[len(sess.Attribution)-1]
	if !seg.UsageUnreliable || seg.UsageUnreliableReason != "terminal_result_zero_input_or_output" || seg.UsageUnreliableScope != state.UsageUnreliableScopeAccounting {
		t.Fatalf("attribution = %+v, want explicit usage-unreliable marker", seg)
	}
	if o.updateTokensUsedFromOutput("sup-946", sess, "healthy rendered output") {
		t.Fatal("unchanged unreliable stream should not report another state change")
	}
}

// #737: a forced retry/respawn appends a second run's result frame to the same
// slot.jsonl. The full-jsonl parse returns the cumulative total, but the
// monotonic watermark ensures only the new run's delta is added — no
// double-count of the prior attempt's tokens.
func TestUpdateTokensUsedFromOutput_ClaudeBackendRetryNoDoubleCount(t *testing.T) {
	o, sess, jsonlPath := claudeTestOrchestrator(t, "sup-737")

	run1 := claudeResultFrame(770, 3, 0, 0, 0.001, "a") // total 773
	if err := os.WriteFile(jsonlPath, []byte(run1), 0644); err != nil {
		t.Fatal(err)
	}
	if !o.updateTokensUsedFromOutput("sup-737", sess, "") {
		t.Fatal("run1: expected a change")
	}
	if sess.TokensUsedTotal != 773 || sess.UsageTokensWatermark != 773 || sess.TokensUsedAttempt != 773 {
		t.Fatalf("run1: total=%d wm=%d attempt=%d, want 773/773/773",
			sess.TokensUsedTotal, sess.UsageTokensWatermark, sess.TokensUsedAttempt)
	}

	// Respawn: per-attempt counter resets, watermark persists.
	sess.TokensUsedAttempt = 0
	sess.TokenBudgetTokensAttempt = 0

	// Re-parse the unchanged jsonl before the new run lands — must NOT re-add.
	if o.updateTokensUsedFromOutput("sup-737", sess, "") {
		t.Fatal("re-parse of unchanged jsonl must not report a change (would double-count)")
	}
	if sess.TokensUsedTotal != 773 {
		t.Fatalf("after re-parse: TokensUsedTotal = %d, want 773 (no double-count)", sess.TokensUsedTotal)
	}

	// New run appends its result frame: cumulative 1773, delta only (1000).
	run2 := claudeResultFrame(900, 100, 0, 0, 0.002, "b") // total 1000
	if err := os.WriteFile(jsonlPath, []byte(run1+run2), 0644); err != nil {
		t.Fatal(err)
	}
	if !o.updateTokensUsedFromOutput("sup-737", sess, "") {
		t.Fatal("run2: expected a change")
	}
	if sess.TokensUsedTotal != 1773 {
		t.Errorf("after run2: TokensUsedTotal = %d, want 1773 (delta only)", sess.TokensUsedTotal)
	}
	if sess.TokensUsedAttempt != 1000 {
		t.Errorf("TokensUsedAttempt = %d, want 1000 (current attempt delta)", sess.TokensUsedAttempt)
	}
	if sess.TokenBudgetTokensAttempt != 1000 {
		t.Errorf("TokenBudgetTokensAttempt = %d, want 1000", sess.TokenBudgetTokensAttempt)
	}
	if sess.UsageTokensWatermark != 1773 {
		t.Errorf("UsageTokensWatermark = %d, want 1773", sess.UsageTokensWatermark)
	}
	if !approxCostEq(sess.CostUSDBackend, 0.003) {
		t.Errorf("CostUSDBackend = %v, want 0.003 (cumulative)", sess.CostUSDBackend)
	}
}

func TestUpdateTokensUsedFromOutput_KimiFixtureStampsWatermarkAndSplitTokens(t *testing.T) {
	dir := t.TempDir()
	o := &Orchestrator{
		cfg: &config.Config{
			StateDir: dir,
			Model: config.ModelConfig{
				Default: "moonshot-primary",
				Backends: map[string]config.BackendDef{
					"moonshot-primary": {Cmd: "kimi", Provider: "moonshot"},
				},
			},
		},
	}
	logFile := filepath.Join(dir, "sup-kimi.log")
	sess := &state.Session{Backend: "moonshot-primary", LogFile: logFile}
	fixture, err := os.ReadFile(filepath.Join("..", "worker", "testdata", "kimi_stream.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(worker.JSONLPathForLog(logFile), fixture, 0644); err != nil {
		t.Fatal(err)
	}

	if !o.updateTokensUsedFromOutput("sup-kimi", sess, "rendered Kimi output") {
		t.Fatal("expected Kimi JSONL usage to update the session")
	}
	if sess.TokensUsedAttempt != 2650 || sess.TokensUsedTotal != 2650 || sess.UsageTokensWatermark != 2650 {
		t.Fatalf("tokens attempt/total/watermark = %d/%d/%d, want 2650/2650/2650",
			sess.TokensUsedAttempt, sess.TokensUsedTotal, sess.UsageTokensWatermark)
	}
	if sess.TokensInput != 2100 || sess.TokensOutput != 180 || sess.TokensCacheRead != 350 || sess.TokensCacheWrite != 20 {
		t.Fatalf("split tokens = %d/%d/%d/%d, want 2100/180/350/20",
			sess.TokensInput, sess.TokensOutput, sess.TokensCacheRead, sess.TokensCacheWrite)
	}
	if sess.Model != "kimi-k2.5" {
		t.Fatalf("Model = %q, want kimi-k2.5", sess.Model)
	}
	if o.updateTokensUsedFromOutput("sup-kimi", sess, "same rendered output") {
		t.Fatal("unchanged cumulative Kimi usage must not advance the watermark")
	}
}

// codexTurnFrame builds one terminal codex `exec --json` turn.completed event
// with the given token totals, as the stream-splitter would append it.
// cached_input_tokens is a subset of input_tokens (OpenAI semantics).
func codexTurnFrame(in, cached, out int) string {
	return fmt.Sprintf(`{"type":"turn.completed","usage":{"input_tokens":%d,"cached_input_tokens":%d,"output_tokens":%d,"reasoning_output_tokens":0}}`+"\n",
		in, cached, out)
}

// codexTestOrchestrator wires a session to a codex backend (usage_stream on,
// with pricing so virtual cost is exercisable) whose slot.jsonl lives at
// <dir>/<slot>.jsonl. Returns the orchestrator, session, and jsonl path.
func codexTestOrchestrator(t *testing.T, slot string) (*Orchestrator, *state.Session, string) {
	t.Helper()
	dir := t.TempDir()
	o := &Orchestrator{
		cfg: &config.Config{
			StateDir: dir,
			Model: config.ModelConfig{
				Default: "codex",
				Backends: map[string]config.BackendDef{
					"codex": {
						Cmd:         "codex",
						UsageStream: true,
						Pricing:     config.BackendPricing{InputUSDPerMtok: 1.25, OutputUSDPerMtok: 10},
					},
				},
			},
		},
	}
	logFile := dir + "/" + slot + ".log"
	sess := &state.Session{Backend: "codex", LogFile: logFile}
	return o, sess, dir + "/" + slot + ".jsonl"
}

// #738: a codex session reads usage from the slot.jsonl side channel (not the
// human-readable slot.log) and stamps non-zero tokens. codex reports no USD,
// so CostUSDBackend stays 0 — the dollar figure is virtual (configured pricing
// via SessionCostEstimate). cached_input_tokens is a subset of input_tokens,
// so the stamped total is input+output only.
func TestUpdateTokensUsedFromOutput_CodexBackendStampsUsage(t *testing.T) {
	o, sess, jsonlPath := codexTestOrchestrator(t, "sup-738")
	frames := `{"type":"thread.started","thread_id":"REDACTED"}` + "\n" +
		`{"type":"item.completed","item":{"id":"item_0","type":"agent_message","text":"pong"}}` + "\n" +
		codexTurnFrame(12087, 4992, 5)
	if err := os.WriteFile(jsonlPath, []byte(frames), 0644); err != nil {
		t.Fatal(err)
	}

	// The passed output (slot.log/tmux) is intentionally ignored for codex.
	if !o.updateTokensUsedFromOutput("sup-738", sess, "human readable log text, no token total") {
		t.Fatal("expected a change from the codex jsonl side channel")
	}
	// input 12087 + output 5 = 12092 (cache is a subset of input, not added).
	if sess.TokensUsedTotal != 12092 || sess.TokensUsedAttempt != 12092 {
		t.Errorf("tokens total=%d attempt=%d, want 12092/12092", sess.TokensUsedTotal, sess.TokensUsedAttempt)
	}
	if sess.UsageTokensWatermark != 12092 {
		t.Errorf("UsageTokensWatermark = %d, want 12092", sess.UsageTokensWatermark)
	}
	// codex never self-reports USD — cost stays virtual (CostUSDBackend == 0).
	if sess.CostUSDBackend != 0 {
		t.Errorf("CostUSDBackend = %v, want 0 (codex cost is virtual, never self-reported)", sess.CostUSDBackend)
	}
}

// #738: a forced retry/respawn appends a second `codex exec` invocation's
// turn.completed event to the same slot.jsonl. The full-jsonl parse returns
// the cumulative total, but the monotonic watermark ensures only the new run's
// delta is added — no double-count of the prior attempt's tokens.
func TestUpdateTokensUsedFromOutput_CodexBackendRetryNoDoubleCount(t *testing.T) {
	o, sess, jsonlPath := codexTestOrchestrator(t, "sup-738")

	run1 := codexTurnFrame(770, 0, 3) // total 773
	if err := os.WriteFile(jsonlPath, []byte(run1), 0644); err != nil {
		t.Fatal(err)
	}
	if !o.updateTokensUsedFromOutput("sup-738", sess, "") {
		t.Fatal("run1: expected a change")
	}
	if sess.TokensUsedTotal != 773 || sess.UsageTokensWatermark != 773 || sess.TokensUsedAttempt != 773 {
		t.Fatalf("run1: total=%d wm=%d attempt=%d, want 773/773/773",
			sess.TokensUsedTotal, sess.UsageTokensWatermark, sess.TokensUsedAttempt)
	}

	// Respawn: per-attempt counter resets, watermark persists.
	sess.TokensUsedAttempt = 0
	sess.TokenBudgetTokensAttempt = 0

	// Re-parse the unchanged jsonl before the new run lands — must NOT re-add.
	if o.updateTokensUsedFromOutput("sup-738", sess, "") {
		t.Fatal("re-parse of unchanged jsonl must not report a change (would double-count)")
	}
	if sess.TokensUsedTotal != 773 {
		t.Fatalf("after re-parse: TokensUsedTotal = %d, want 773 (no double-count)", sess.TokensUsedTotal)
	}

	// New run appends its turn.completed: cumulative 1773, delta only (1000).
	run2 := codexTurnFrame(900, 0, 100) // total 1000
	if err := os.WriteFile(jsonlPath, []byte(run1+run2), 0644); err != nil {
		t.Fatal(err)
	}
	if !o.updateTokensUsedFromOutput("sup-738", sess, "") {
		t.Fatal("run2: expected a change")
	}
	if sess.TokensUsedTotal != 1773 {
		t.Errorf("after run2: TokensUsedTotal = %d, want 1773 (delta only)", sess.TokensUsedTotal)
	}
	if sess.TokensUsedAttempt != 1000 {
		t.Errorf("TokensUsedAttempt = %d, want 1000 (current attempt delta)", sess.TokensUsedAttempt)
	}
	if sess.UsageTokensWatermark != 1773 {
		t.Errorf("UsageTokensWatermark = %d, want 1773", sess.UsageTokensWatermark)
	}
}
