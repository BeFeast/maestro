package orchestrator

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/github"
	"github.com/befeast/maestro/internal/notify"
	"github.com/befeast/maestro/internal/state"
)

func TestReconcileFalseDoneSessionAdoptsSoleOpenCanonicalPR(t *testing.T) {
	finished := time.Date(2026, 7, 17, 17, 13, 56, 0, time.UTC)
	s := state.NewState()
	s.Sessions["ok-player-273"] = &state.Session{
		IssueNumber: 345,
		Status:      state.StatusDone,
		PRNumber:    389,
		Branch:      "feat/duplicate-389",
		FinishedAt:  &finished,
	}
	o := &Orchestrator{
		isPRMergedFn:    func(int) (bool, error) { return false, nil },
		isIssueClosedFn: func(int) (bool, error) { return false, nil },
	}

	o.reconcileTerminalSessionsWithOpenPRs(s, []github.PR{{
		Number:      388,
		HeadRefName: "feat/ok-player-345-flatpak-beta-retry",
		Title:       "Linux packaging for #345",
	}})

	sess := s.Sessions["ok-player-273"]
	if sess.Status != state.StatusPROpen {
		t.Fatalf("status = %q, want pr_open", sess.Status)
	}
	if sess.PRNumber != 388 || sess.Branch != "feat/ok-player-345-flatpak-beta-retry" {
		t.Fatalf("canonical identity = PR #%d branch %q, want PR #388 canonical branch", sess.PRNumber, sess.Branch)
	}
	if sess.LastClosedPRNumber != 389 {
		t.Fatalf("last closed PR = %d, want 389", sess.LastClosedPRNumber)
	}
	if sess.FinishedAt != nil {
		t.Fatalf("finished_at retained after reopening canonical PR: %v", sess.FinishedAt)
	}
}

func TestReconcileFalseDoneSessionRefusesAmbiguousOpenPRs(t *testing.T) {
	s := state.NewState()
	s.Sessions["slot"] = &state.Session{IssueNumber: 345, Status: state.StatusDone, PRNumber: 389}
	o := &Orchestrator{}

	o.reconcileTerminalSessionsWithOpenPRs(s, []github.PR{
		{Number: 388, Title: "first #345"},
		{Number: 390, Title: "second #345"},
	})

	if got := s.Sessions["slot"]; got.Status != state.StatusDone || got.PRNumber != 389 {
		t.Fatalf("ambiguous reconcile mutated session: %+v", got)
	}
}

func TestReconcileUnscheduledDeadSessionAdoptsSoleOpenPR(t *testing.T) {
	repo := newReconcileBranchRepo(t)
	canonicalBranch := "feat/ok-player-345-flatpak-beta-retry"
	runReconcileGit(t, repo, "switch", "-c", canonicalBranch)
	finished := time.Now().UTC().Add(-time.Minute)
	s := state.NewState()
	s.Sessions["ok-player-302"] = &state.Session{
		IssueNumber: 406,
		Status:      state.StatusDead,
		Branch:      "feat/ok-player-302-406-synthetic",
		Worktree:    repo,
		FinishedAt:  &finished,
	}
	o := &Orchestrator{
		hasMergedPRForIssueFn: func(int) (bool, error) { return false, nil },
		isIssueClosedFn:       func(int) (bool, error) { return false, nil },
	}

	o.reconcileTerminalSessionsWithOpenPRs(s, []github.PR{{
		Number:      388,
		HeadRefName: canonicalBranch,
		Title:       "Flatpak continuation",
		Body:        "Refs #406",
	}})

	sess := s.Sessions["ok-player-302"]
	if sess.Status != state.StatusPROpen || sess.PRNumber != 388 || sess.Branch != canonicalBranch {
		t.Fatalf("reconciled session = %+v, want pr_open on existing PR #388", sess)
	}
	if sess.Worktree != repo || sess.FinishedAt != nil {
		t.Fatalf("canonical worktree/timing changed: worktree=%q finished=%v", sess.Worktree, sess.FinishedAt)
	}
}

func TestReconcileDeadSessionWithPendingRetryDoesNotAdoptOpenPR(t *testing.T) {
	retryAt := time.Now().UTC().Add(time.Minute)
	s := state.NewState()
	s.Sessions["slot"] = &state.Session{
		IssueNumber: 406,
		Status:      state.StatusDead,
		Branch:      "synthetic",
		NextRetryAt: &retryAt,
	}
	o := &Orchestrator{}

	o.reconcileTerminalSessionsWithOpenPRs(s, []github.PR{{
		Number:      388,
		HeadRefName: "canonical",
		Body:        "Refs #406",
	}})

	sess := s.Sessions["slot"]
	if sess.Status != state.StatusDead || sess.PRNumber != 0 || sess.NextRetryAt == nil {
		t.Fatalf("pending retry was consumed by PR adoption: %+v", sess)
	}
}

func TestReconcileDoneSessionKeepsAuthoritativeMergedOutcome(t *testing.T) {
	s := state.NewState()
	s.Sessions["slot"] = &state.Session{IssueNumber: 345, Status: state.StatusDone, PRNumber: 389}
	o := &Orchestrator{
		isPRMergedFn: func(pr int) (bool, error) { return pr == 389, nil },
		isIssueClosedFn: func(int) (bool, error) {
			t.Fatal("merged PR is authoritative; issue state should not be queried")
			return false, nil
		},
	}

	o.reconcileTerminalSessionsWithOpenPRs(s, []github.PR{{Number: 388, Title: "follow-up #345"}})

	if got := s.Sessions["slot"]; got.Status != state.StatusDone || got.PRNumber != 389 {
		t.Fatalf("authoritative merged session mutated: %+v", got)
	}
}

func TestReconcileCanonicalPRSelectsExactHistoricalSessionAfterCompetingWorkerStops(t *testing.T) {
	s := state.NewState()
	s.Sessions["ok-player-272"] = &state.Session{
		IssueNumber:        346,
		Status:             state.StatusFailed,
		Branch:             "feat/older-duplicate",
		LastClosedPRNumber: 372,
	}
	s.Sessions["ok-player-277"] = &state.Session{
		IssueNumber:        346,
		Status:             state.StatusFailed,
		Branch:             "feat/canonical-fedora",
		LastClosedPRNumber: 397,
	}
	s.Sessions["ok-player-294"] = &state.Session{
		IssueNumber: 346,
		Status:      state.StatusRunning,
		Branch:      "feat/new-duplicate",
	}
	o := &Orchestrator{
		isIssueClosedFn: func(int) (bool, error) { return false, nil },
	}
	prs := []github.PR{{Number: 397, HeadRefName: "feat/canonical-fedora", Title: "Fedora fix for #346"}}

	// A live competing lease makes reconciliation fail closed.
	o.reconcileTerminalSessionsWithOpenPRs(s, prs)
	if got := s.Sessions["ok-player-277"].Status; got != state.StatusFailed {
		t.Fatalf("canonical session activated while competing worker runs: %q", got)
	}

	// Once the duplicate is terminal, the exact branch/PR match wins over
	// every other historical session without allocating a new identity.
	s.Sessions["ok-player-294"].Status = state.StatusFailed
	o.reconcileTerminalSessionsWithOpenPRs(s, prs)
	canonical := s.Sessions["ok-player-277"]
	if canonical.Status != state.StatusPROpen || canonical.PRNumber != 397 || canonical.Branch != "feat/canonical-fedora" {
		t.Fatalf("canonical session = %+v, want ok-player-277 bound to open PR #397", canonical)
	}
	if got := s.Sessions["ok-player-272"].Status; got != state.StatusFailed {
		t.Fatalf("older historical session activated: %q", got)
	}
	if got := s.Sessions["ok-player-294"].Status; got != state.StatusFailed {
		t.Fatalf("new duplicate historical session activated: %q", got)
	}
}

func TestReconcileActiveCanonicalPRReceivesTerminalSiblingCommitHandoffOnce(t *testing.T) {
	repo := newReconcileBranchRepo(t)
	runReconcileGit(t, repo, "branch", "canonical")
	runReconcileGit(t, repo, "branch", "equivalent-sibling")
	runReconcileGit(t, repo, "switch", "-c", "preserved-sibling")
	if err := os.WriteFile(filepath.Join(repo, "repair.txt"), []byte("preserved repair\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runReconcileGit(t, repo, "add", "repair.txt")
	runReconcileGit(t, repo, "commit", "-m", "preserved repair")
	wantCommit := strings.TrimSpace(runReconcileGit(t, repo, "rev-parse", "HEAD"))

	s := state.NewState()
	s.Sessions["canonical-slot"] = &state.Session{
		IssueNumber: 346,
		Status:      state.StatusPROpen,
		PRNumber:    397,
		Branch:      "canonical",
	}
	s.Sessions["preserved-slot"] = &state.Session{
		IssueNumber: 346,
		Status:      state.StatusRetryExhausted,
		Branch:      "preserved-sibling",
	}
	s.Sessions["equivalent-slot"] = &state.Session{
		IssueNumber: 346,
		Status:      state.StatusFailed,
		Branch:      "equivalent-sibling",
	}
	o := &Orchestrator{cfg: &config.Config{LocalPath: repo}}
	prs := []github.PR{{Number: 397, HeadRefName: "canonical", Title: "Fedora fix for #346"}}

	o.reconcileTerminalSessionsWithOpenPRs(s, prs)
	feedback := s.Sessions["canonical-slot"].PreviousAttemptFeedback
	for _, want := range []string{"preserved-slot", "preserved-sibling", wantCommit} {
		if !strings.Contains(feedback, want) {
			t.Fatalf("handoff missing %q: %s", want, feedback)
		}
	}
	if got := s.Sessions["canonical-slot"].PreviousAttemptFeedbackKind; got != "recovery_handoff" {
		t.Fatalf("feedback kind = %q, want recovery_handoff", got)
	}
	if got := s.Sessions["canonical-slot"].Status; got != state.StatusPROpen {
		t.Fatalf("canonical status = %q, want pr_open", got)
	}
	if sibling := s.Sessions["preserved-slot"]; !sibling.ReleasedForRedispatch || sibling.WorkerOutcome != "duplicate_dispatch_reconciled" {
		t.Fatalf("preserved sibling claim was not released after durable handoff: %+v", sibling)
	}
	if sibling := s.Sessions["equivalent-slot"]; !sibling.ReleasedForRedispatch || sibling.WorkerOutcome != "duplicate_dispatch_reconciled" {
		t.Fatalf("patch-equivalent sibling claim was not released: %+v", sibling)
	}

	// Repeated minute-level reconciliation must not grow the prompt forever.
	o.reconcileTerminalSessionsWithOpenPRs(s, prs)
	if got := s.Sessions["canonical-slot"].PreviousAttemptFeedback; got != feedback {
		t.Fatalf("handoff was duplicated:\n%s", got)
	}
}

func newReconcileBranchRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	runReconcileGit(t, repo, "init", "-b", "main")
	runReconcileGit(t, repo, "config", "user.email", "test@example.com")
	runReconcileGit(t, repo, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("seed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runReconcileGit(t, repo, "add", "README.md")
	runReconcileGit(t, repo, "commit", "-m", "seed")
	return repo
}

func runReconcileGit(t *testing.T, repo string, args ...string) string {
	t.Helper()
	cmdArgs := append([]string{"-C", repo}, args...)
	out, err := exec.Command("git", cmdArgs...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return string(out)
}

func TestAppendRecoveryHandoffContextRequiresExistingCanonicalPR(t *testing.T) {
	prompt := appendRecoveryHandoffContext("base", "- session duplicate: unique commit `abc123`")
	for _, want := range []string{"existing PR", "abc123", "do NOT open another PR"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("recovery prompt missing %q: %s", want, prompt)
		}
	}
}

func TestAutoMergeClosedUnmergedPRReleasesOpenIssue(t *testing.T) {
	o := &Orchestrator{
		cfg:             &config.Config{Repo: "owner/repo"},
		notifier:        &notify.Notifier{},
		listOpenPRsFn:   func() ([]github.PR, error) { return nil, nil },
		isPRMergedFn:    func(int) (bool, error) { return false, nil },
		isIssueClosedFn: func(int) (bool, error) { return false, nil },
	}
	s := state.NewState()
	s.Sessions["slot"] = &state.Session{
		IssueNumber: 345,
		Status:      state.StatusPROpen,
		PRNumber:    389,
		Branch:      "feat/duplicate-389",
	}

	o.autoMergePRs(s)

	sess := s.Sessions["slot"]
	if sess.Status != state.StatusFailed || !sess.ReleasedForRedispatch {
		t.Fatalf("session = %+v, want failed/released after closed-unmerged PR", sess)
	}
	if sess.PRNumber != 0 || sess.LastClosedPRNumber != 389 {
		t.Fatalf("PR identity = current %d last_closed %d, want 0/389", sess.PRNumber, sess.LastClosedPRNumber)
	}
}
