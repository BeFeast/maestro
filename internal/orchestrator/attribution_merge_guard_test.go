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
