package worker

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestEnsureWorktreeBranchSwitchesCleanRetainedCheckout(t *testing.T) {
	repo := newBranchTestRepo(t)
	runBranchGit(t, repo, "branch", "canonical")
	runBranchGit(t, repo, "switch", "-c", "duplicate")

	if err := EnsureWorktreeBranch(repo, "canonical"); err != nil {
		t.Fatalf("EnsureWorktreeBranch: %v", err)
	}
	if got := strings.TrimSpace(runBranchGit(t, repo, "branch", "--show-current")); got != "canonical" {
		t.Fatalf("current branch = %q, want canonical", got)
	}
}

func TestEnsureWorktreeBranchPreservesDirtyRetainedCheckout(t *testing.T) {
	repo := newBranchTestRepo(t)
	runBranchGit(t, repo, "branch", "canonical")
	runBranchGit(t, repo, "switch", "-c", "duplicate")
	path := filepath.Join(repo, "work.txt")
	if err := os.WriteFile(path, []byte("preserve me\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	err := EnsureWorktreeBranch(repo, "canonical")
	if err == nil || !strings.Contains(err.Error(), "dirty") {
		t.Fatalf("error = %v, want dirty-worktree refusal", err)
	}
	if got := strings.TrimSpace(runBranchGit(t, repo, "branch", "--show-current")); got != "duplicate" {
		t.Fatalf("current branch = %q, want duplicate retained", got)
	}
	if got, readErr := os.ReadFile(path); readErr != nil || string(got) != "preserve me\n" {
		t.Fatalf("dirty change was not preserved: %q, %v", got, readErr)
	}
}

func TestUniqueBranchCommitsReturnsOnlySiblingPatch(t *testing.T) {
	repo := newBranchTestRepo(t)
	runBranchGit(t, repo, "branch", "canonical")
	runBranchGit(t, repo, "switch", "-c", "duplicate")
	if err := os.WriteFile(filepath.Join(repo, "repair.txt"), []byte("preserved repair\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runBranchGit(t, repo, "add", "repair.txt")
	runBranchGit(t, repo, "commit", "-m", "preserved repair")
	want := strings.TrimSpace(runBranchGit(t, repo, "rev-parse", "HEAD"))

	got, err := UniqueBranchCommits(repo, "canonical", "duplicate")
	if err != nil {
		t.Fatalf("UniqueBranchCommits: %v", err)
	}
	if len(got) != 1 || got[0] != want {
		t.Fatalf("unique commits = %v, want [%s]", got, want)
	}
}

func newBranchTestRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	runBranchGit(t, repo, "init", "-b", "main")
	runBranchGit(t, repo, "config", "user.email", "test@example.com")
	runBranchGit(t, repo, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("seed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runBranchGit(t, repo, "add", "README.md")
	runBranchGit(t, repo, "commit", "-m", "seed")
	return repo
}

func runBranchGit(t *testing.T, repo string, args ...string) string {
	t.Helper()
	cmdArgs := append([]string{"-C", repo}, args...)
	out, err := exec.Command("git", cmdArgs...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return string(out)
}
