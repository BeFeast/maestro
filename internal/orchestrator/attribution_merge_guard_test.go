package orchestrator

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/github"
	"github.com/befeast/maestro/internal/state"
)

func attributionGuardGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s (in %s): %v\n%s", strings.Join(args, " "), dir, err, out)
	}
	return strings.TrimSpace(string(out))
}

func requireAttributionGuardGitRefMissing(t *testing.T, dir, ref string) {
	t.Helper()
	cmd := exec.Command("git", "-C", dir, "show-ref", "--verify", "--quiet", ref)
	if err := cmd.Run(); err == nil {
		t.Fatalf("git ref %s unexpectedly exists", ref)
	} else if exitErr, ok := err.(*exec.ExitError); !ok || exitErr.ExitCode() != 1 {
		t.Fatalf("verify missing git ref %s: %v", ref, err)
	}
}

func setupUnattributedBranch(t *testing.T) (origin, worktree, branch, head string) {
	t.Helper()
	root := t.TempDir()
	origin = filepath.Join(root, "origin.git")
	worktree = filepath.Join(root, "worktree")
	branch = "feat/sup-1000-internal-attribution"

	attributionGuardGit(t, root, "init", "--bare", origin)
	attributionGuardGit(t, origin, "symbolic-ref", "HEAD", "refs/heads/main")
	attributionGuardGit(t, root, "clone", origin, worktree)
	attributionGuardGit(t, worktree, "config", "user.email", "test@example.com")
	attributionGuardGit(t, worktree, "config", "user.name", "Test User")
	if err := os.WriteFile(filepath.Join(worktree, "README.md"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	attributionGuardGit(t, worktree, "add", "README.md")
	attributionGuardGit(t, worktree, "commit", "-m", "initial")
	attributionGuardGit(t, worktree, "push", "-u", "origin", "main")
	attributionGuardGit(t, worktree, "checkout", "-b", branch)
	if err := os.WriteFile(filepath.Join(worktree, "feature.txt"), []byte("product change\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	attributionGuardGit(t, worktree, "add", "feature.txt")
	attributionGuardGit(t, worktree, "commit", "-m", "worker: product change")
	attributionGuardGit(t, worktree, "push", "-u", "origin", branch)
	return origin, worktree, branch, attributionGuardGit(t, origin, "rev-parse", branch)
}

func advanceAttributionGuardBranch(t *testing.T, origin, branch string) string {
	t.Helper()
	worker := filepath.Join(filepath.Dir(origin), "worker")
	attributionGuardGit(t, filepath.Dir(origin), "clone", origin, worker)
	attributionGuardGit(t, worker, "config", "user.email", "worker@example.com")
	attributionGuardGit(t, worker, "config", "user.name", "Worker")
	attributionGuardGit(t, worker, "checkout", "-b", branch, "--track", "origin/"+branch)
	if err := os.WriteFile(filepath.Join(worker, "worker.txt"), []byte("concurrent worker push\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	attributionGuardGit(t, worker, "add", "worker.txt")
	attributionGuardGit(t, worker, "commit", "-m", "worker: advance feature branch")
	attributionGuardGit(t, worker, "push", "origin", "HEAD:refs/heads/"+branch)
	return attributionGuardGit(t, origin, "rev-parse", branch)
}

// Backend attribution is durable in Maestro state and Fleet, not in product
// commits. An unattributed PR head must remain byte-for-byte identical across
// orchestration cycles and must still reach the ordinary merge attempt (#1000).
func TestAutoMergePRs_UnattributedHeadStaysExactAndMergeEligibleAcrossCycles(t *testing.T) {
	origin, worktree, branch, originalHead := setupUnattributedBranch(t)
	pr := github.PR{Number: 864, HeadRefName: branch}
	cfg := &config.Config{
		Repo:          "owner/repo",
		LocalPath:     worktree,
		MergeStrategy: "parallel",
		ReviewGate:    "none",
	}
	o, _ := newMergeTestOrchestrator(cfg, []github.PR{pr})
	o.ghPRCheckRollupFn = func(int) (github.PRCheckRollup, error) {
		return github.PRCheckRollup{
			HeadSHA:     originalHead,
			Verdict:     "success",
			Fingerprint: strings.Repeat("1", 16),
			Complete:    true,
		}, nil
	}
	o.ghPRHeadSHAFn = func(int) (string, error) { return originalHead, nil }
	mergeAttempts := 0
	o.ghMergePRFn = func(int) error {
		mergeAttempts++
		return errors.New("deliberate merge stop after readiness proof")
	}
	o.ghPRMergeStatusFn = func(int) (string, string, error) {
		return "MERGEABLE", "CLEAN", nil
	}

	s := state.NewState()
	s.Sessions["sup-1000"] = &state.Session{
		IssueNumber: 1000,
		IssueTitle:  "keep backend attribution internal",
		Status:      state.StatusPROpen,
		PRNumber:    pr.Number,
		Branch:      branch,
		Worktree:    worktree,
		Attribution: []state.BackendAttribution{{
			Backend:   "claude",
			Provider:  "anthropic",
			Model:     "opus-4.8",
			Effort:    "xhigh",
			StartedAt: time.Date(2026, 7, 10, 4, 0, 0, 0, time.UTC),
		}},
	}

	for cycle := 1; cycle <= 2; cycle++ {
		o.autoMergePRs(s)
		if mergeAttempts != cycle {
			t.Fatalf("cycle %d merge attempts = %d, want %d; unattributed head was rejected before merge", cycle, mergeAttempts, cycle)
		}
		if got := attributionGuardGit(t, origin, "rev-parse", branch); got != originalHead {
			t.Fatalf("cycle %d changed exact product head: got %s want %s", cycle, got, originalHead)
		}
		if msg := attributionGuardGit(t, origin, "log", "-1", "--pretty=%B", branch); strings.Contains(msg, "Maestro-Backend:") {
			t.Fatalf("cycle %d injected backend telemetry into product commit:\n%s", cycle, msg)
		}
	}
}

// #858 originally raced because attribution reconciliation fetched a branch,
// amended its head, and force-pushed while a worker was still pushing. Backend
// attribution is now internal-only, so the safe behavior is structural: a
// concurrent worker push remains the exact remote head, stale gate evidence is
// discarded, and the next cycle still schedules review repair. Keep the
// production narrow fetch refspec in this regression so an absent
// origin/<feature> tracking ref cannot reintroduce the old starvation wedge.
func TestAutoMergePRs_ConcurrentWorkerPushWithNarrowFetchDoesNotRewriteOrStarveReviewRepair(t *testing.T) {
	origin, worktree, branch, originalHead := setupUnattributedBranch(t)
	narrowFetch := "+refs/heads/main:refs/remotes/origin/main"
	attributionGuardGit(t, worktree, "config", "remote.origin.fetch", narrowFetch)
	attributionGuardGit(t, worktree, "update-ref", "-d", "refs/remotes/origin/"+branch)
	if got := attributionGuardGit(t, worktree, "config", "--get-all", "remote.origin.fetch"); got != narrowFetch {
		t.Fatalf("remote.origin.fetch = %q, want production narrow refspec %q", got, narrowFetch)
	}
	featureTrackingRef := "refs/remotes/origin/" + branch
	requireAttributionGuardGitRefMissing(t, worktree, featureTrackingRef)

	pr := github.PR{Number: 864, HeadRefName: branch}
	cfg := &config.Config{
		Repo:                    "owner/repo",
		LocalPath:               worktree,
		MergeStrategy:           "parallel",
		ReviewGate:              "none",
		AutoRetryReviewFeedback: true,
		MaxRetriesPerIssue:      3,
		MaxRetryBackoffMs:       300000,
	}
	o, merged := newMergeTestOrchestrator(cfg, []github.PR{pr})
	currentHead := originalHead
	o.ghPRCheckRollupFn = func(int) (github.PRCheckRollup, error) {
		return github.PRCheckRollup{
			HeadSHA:     currentHead,
			Verdict:     "success",
			Fingerprint: strings.Repeat("8", 16),
			Complete:    true,
		}, nil
	}
	o.ghPRHeadSHAFn = func(int) (string, error) { return currentHead, nil }
	feedbackReads := 0
	o.ghCollectPRReviewFeedbackFn = func(int, []string) (string, error) {
		feedbackReads++
		if feedbackReads == 1 {
			currentHead = advanceAttributionGuardBranch(t, origin, branch)
			if currentHead == originalHead {
				t.Fatal("worker push did not advance the remote feature branch")
			}
		}
		return "internal/example.go:42 P1: repair on the current head", nil
	}
	o.ghPRFailingChecksFn = func(int) ([]github.FailingCheck, error) { return nil, nil }

	s := state.NewState()
	sess := &state.Session{
		IssueNumber: 858,
		IssueTitle:  "attribution amend race",
		Status:      state.StatusPROpen,
		PRNumber:    pr.Number,
		Branch:      branch,
		Worktree:    worktree,
		Attribution: []state.BackendAttribution{{
			Backend:   "claude",
			Provider:  "anthropic",
			Model:     "opus-4.8",
			Effort:    "xhigh",
			StartedAt: time.Date(2026, 7, 10, 4, 0, 0, 0, time.UTC),
		}},
	}
	s.Sessions["sup-858"] = sess

	o.autoMergePRs(s)
	if feedbackReads != 1 {
		t.Fatalf("first cycle feedback reads = %d, want 1", feedbackReads)
	}
	if sess.Status != state.StatusPROpen || sess.MaintenanceRetryCount != 0 || sess.NextRetryAt != nil {
		t.Fatalf("moving-head cycle scheduled stale repair: status=%q maintenance=%d next=%v", sess.Status, sess.MaintenanceRetryCount, sess.NextRetryAt)
	}
	if len(*merged) != 0 {
		t.Fatalf("moving-head cycle unexpectedly merged PR: %v", *merged)
	}
	staleSnapshot := mustLatestPRGateSnapshot(t, s, sess.IssueNumber, pr.Number)
	if staleSnapshot.HeadSHA != originalHead || staleSnapshot.ReviewDecision != state.PRGateReviewUnknown {
		t.Fatalf("moving-head snapshot mixed review evidence across heads: %+v", staleSnapshot)
	}

	o.autoMergePRs(s)
	if feedbackReads != 2 {
		t.Fatalf("second cycle feedback reads = %d, want 2; review processing was starved", feedbackReads)
	}
	if sess.Status != state.StatusDead || sess.MaintenanceRetryCount != 1 || sess.NextRetryAt == nil {
		t.Fatalf("review repair did not resume on worker head: status=%q maintenance=%d next=%v", sess.Status, sess.MaintenanceRetryCount, sess.NextRetryAt)
	}
	if !strings.Contains(sess.PreviousAttemptFeedback, "repair on the current head") {
		t.Fatalf("review repair lost current-head feedback: %q", sess.PreviousAttemptFeedback)
	}
	currentSnapshot := mustLatestPRGateSnapshot(t, s, sess.IssueNumber, pr.Number)
	if currentSnapshot.HeadSHA != currentHead || currentSnapshot.ReviewDecision != state.PRGateReviewBlocked {
		t.Fatalf("current worker head was not recorded with blocked review: %+v", currentSnapshot)
	}
	if got := attributionGuardGit(t, origin, "rev-parse", branch); got != currentHead {
		t.Fatalf("orchestrator rewrote concurrent worker head: got %s want %s", got, currentHead)
	}
	attributionGuardGit(t, origin, "merge-base", "--is-ancestor", originalHead, currentHead)
	if !strings.Contains(attributionGuardGit(t, origin, "log", "-1", "--pretty=%B", branch), "worker: advance feature branch") {
		t.Fatal("concurrent worker commit is no longer the remote branch head")
	}
	if msg := attributionGuardGit(t, origin, "log", "-1", "--pretty=%B", branch); strings.Contains(msg, "Maestro-Backend:") {
		t.Fatalf("orchestrator injected backend telemetry into concurrent worker commit:\n%s", msg)
	}
	if got := attributionGuardGit(t, worktree, "rev-parse", "HEAD"); got != originalHead {
		t.Fatalf("orchestrator moved the worker worktree head: got %s want %s", got, originalHead)
	}
	requireAttributionGuardGitRefMissing(t, worktree, featureTrackingRef)
}
