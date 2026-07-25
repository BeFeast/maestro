package worker

import (
	"path/filepath"
	"strings"
	"testing"
)

// setupOriginAndClone builds a bare "origin" with a single commit on main and a
// local clone of it (mirroring the orchestrator's local_path checkout). It
// returns the origin path, the local clone path, and a separate seed clone used
// to push new commits to origin (simulating PRs merged on GitHub).
func setupOriginAndClone(t *testing.T) (origin, local, seed string) {
	t.Helper()
	root := t.TempDir()
	origin = filepath.Join(root, "origin.git")
	seed = filepath.Join(root, "seed")
	local = filepath.Join(root, "local")

	gitTest(t, root, "init", "--bare", origin)
	gitTest(t, origin, "symbolic-ref", "HEAD", "refs/heads/main")

	gitTest(t, root, "clone", origin, seed)
	gitTest(t, seed, "config", "user.email", "test@example.com")
	gitTest(t, seed, "config", "user.name", "Test User")
	gitTest(t, seed, "checkout", "-b", "main")
	writeTestFile(t, seed, "README.md", "v1\n")
	gitTest(t, seed, "add", "README.md")
	gitTest(t, seed, "commit", "-m", "initial")
	gitTest(t, seed, "push", "-u", "origin", "main")

	gitTest(t, root, "clone", origin, local)
	gitTest(t, local, "config", "user.email", "test@example.com")
	gitTest(t, local, "config", "user.name", "Test User")
	return origin, local, seed
}

// advanceOrigin pushes a new commit to origin/main via the seed clone, leaving
// the local clone stale.
func advanceOrigin(t *testing.T, seed, content string) {
	t.Helper()
	writeTestFile(t, seed, "README.md", content)
	gitTest(t, seed, "commit", "-am", "advance")
	gitTest(t, seed, "push", "origin", "main")
}

func revParse(t *testing.T, dir, ref string) string {
	t.Helper()
	out, err := runGit(dir, "rev-parse", ref)
	if err != nil {
		t.Fatalf("rev-parse %s in %s: %v", ref, dir, err)
	}
	return strings.TrimSpace(out)
}

func TestSyncBaseBranch_FastForwardsStaleLocalMain(t *testing.T) {
	_, local, seed := setupOriginAndClone(t)
	advanceOrigin(t, seed, "v2\n")

	// Local main still points at v1 until we sync.
	if err := SyncBaseBranch(local, "main"); err != nil {
		t.Fatalf("SyncBaseBranch: %v", err)
	}

	got := revParse(t, local, "main")
	want := revParse(t, local, "origin/main")
	if got != want {
		t.Fatalf("local main = %s, want origin/main %s after sync", got, want)
	}
}

func TestSyncBaseBranch_NoopWhenUpToDate(t *testing.T) {
	_, local, _ := setupOriginAndClone(t)
	before := revParse(t, local, "main")
	if err := SyncBaseBranch(local, "main"); err != nil {
		t.Fatalf("SyncBaseBranch: %v", err)
	}
	if after := revParse(t, local, "main"); after != before {
		t.Fatalf("main moved on no-op sync: %s -> %s", before, after)
	}
}

func TestSyncBaseBranch_DivergedFailsLoudly(t *testing.T) {
	_, local, seed := setupOriginAndClone(t)
	advanceOrigin(t, seed, "v2-remote\n")

	// Make local main diverge: commit a different change without pulling.
	writeTestFile(t, local, "README.md", "v2-local\n")
	gitTest(t, local, "commit", "-am", "local divergent")

	err := SyncBaseBranch(local, "main")
	if err == nil {
		t.Fatal("expected error for diverged local main")
	}
	if !strings.Contains(err.Error(), "diverged") {
		t.Fatalf("expected diverged error, got: %v", err)
	}
}

func TestSyncBaseBranch_DirtyCheckoutFailsLoudly(t *testing.T) {
	_, local, seed := setupOriginAndClone(t)
	advanceOrigin(t, seed, "v2\n")

	// Stale local main that also has a dirty working tree.
	writeTestFile(t, local, "README.md", "dirty local edit\n")

	err := SyncBaseBranch(local, "main")
	if err == nil {
		t.Fatal("expected error for dirty base checkout")
	}
	if !strings.Contains(err.Error(), "dirty") {
		t.Fatalf("expected dirty error, got: %v", err)
	}
}

func TestSyncBaseBranch_IgnoresUntrackedAgentHarnessDirs(t *testing.T) {
	_, local, seed := setupOriginAndClone(t)
	advanceOrigin(t, seed, "v2\n")

	harnessFiles := map[string]string{
		".claude/settings.json": "{}\n",
		".codex/session.json":   "{}\n",
		".cursor/rules.md":      "local rules\n",
		".entire/state.json":    "{}\n",
	}
	for path, content := range harnessFiles {
		writeTestFile(t, local, path, content)
	}

	if err := SyncBaseBranch(local, "main"); err != nil {
		t.Fatalf("SyncBaseBranch with agent harness dirs: %v", err)
	}
	if got, want := revParse(t, local, "main"), revParse(t, local, "origin/main"); got != want {
		t.Fatalf("local main = %s, want origin/main %s", got, want)
	}
	for path, want := range harnessFiles {
		if got := readTestFile(t, local, path); got != want {
			t.Fatalf("harness file %s = %q, want preserved %q", path, got, want)
		}
	}
}

func TestSyncBaseBranch_UnrelatedUntrackedFileStillFails(t *testing.T) {
	_, local, seed := setupOriginAndClone(t)
	advanceOrigin(t, seed, "v2\n")
	writeTestFile(t, local, "scratch.txt", "local scratch\n")

	err := SyncBaseBranch(local, "main")
	if err == nil {
		t.Fatal("expected error for unrelated untracked file")
	}
	if !strings.Contains(err.Error(), "dirty") || !strings.Contains(err.Error(), "scratch.txt") {
		t.Fatalf("expected dirty scratch.txt error, got: %v", err)
	}
}

func TestSyncBaseBranch_AdvancesUncheckedBaseBranch(t *testing.T) {
	_, local, seed := setupOriginAndClone(t)
	advanceOrigin(t, seed, "v2\n")

	// Local checkout sits on a feature branch, not main.
	gitTest(t, local, "checkout", "-b", "feature")

	if err := SyncBaseBranch(local, "main"); err != nil {
		t.Fatalf("SyncBaseBranch: %v", err)
	}

	if got, want := revParse(t, local, "main"), revParse(t, local, "origin/main"); got != want {
		t.Fatalf("local main = %s, want origin/main %s", got, want)
	}
	head, err := runGit(local, "symbolic-ref", "--short", "HEAD")
	if err != nil {
		t.Fatalf("symbolic-ref HEAD: %v", err)
	}
	if strings.TrimSpace(head) != "feature" {
		t.Fatalf("HEAD = %q, want to stay on feature branch", strings.TrimSpace(head))
	}
}

// TestAddWorktreeFromBase_MergeBaseIsOriginMain is the core acceptance check:
// after syncing, a worker worktree's merge-base with origin/main equals
// origin/main, i.e. it descends from the real remote base rather than a stale
// or sibling commit.
func TestAddWorktreeFromBase_MergeBaseIsOriginMain(t *testing.T) {
	root := t.TempDir()
	_, local, seed := setupOriginAndClone(t)
	advanceOrigin(t, seed, "v2\n")

	if err := SyncBaseBranch(local, "main"); err != nil {
		t.Fatalf("SyncBaseBranch: %v", err)
	}

	worktreePath := filepath.Join(root, "wt")
	if err := addWorktreeFromBase(local, worktreePath, "feat/sup-1-thing"); err != nil {
		t.Fatalf("addWorktreeFromBase: %v", err)
	}

	mergeBase, err := runGit(worktreePath, "merge-base", "HEAD", "origin/main")
	if err != nil {
		t.Fatalf("merge-base: %v", err)
	}
	originMain := revParse(t, worktreePath, "origin/main")
	if strings.TrimSpace(mergeBase) != originMain {
		t.Fatalf("worktree merge-base with origin/main = %s, want origin/main %s",
			strings.TrimSpace(mergeBase), originMain)
	}
}
