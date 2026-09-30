package worker

import (
	"github.com/befeast/maestro/internal/github"
	"os"
	"path/filepath"
	"testing"
)

func TestNativeCloneMaterializesAssignedPathWithoutSharedGitMetadata(t *testing.T) {
	if os.Getenv("MAESTRO_NATIVE_KERNEL_TESTS") != "1" {
		t.Skip("set MAESTRO_NATIVE_KERNEL_TESTS=1 for native clone kernel fixture")
	}
	parent := newBranchTestRepo(t)
	runBranchGit(t, parent, "remote", "add", "origin", "https://git.oklabs.uk/BeFeast/maestro.git")
	runBranchGit(t, parent, "update-ref", "refs/remotes/origin/main", "HEAD")
	path := filepath.Join(t.TempDir(), "assigned")
	if err := materializeNativeClone(parent, path, "codex/fixture-1"); err != nil {
		t.Fatal(err)
	}
	if !isNativeCloneForRepo(parent, path) || !isGitWorktreeForRepo(parent, path) {
		out, err := nativeCloneGit(path, "rev-parse", "--show-toplevel")
		t.Fatalf("clone identity lost: %s %v", out, err)
	}
	if err := validateExactWorktreeIdentity(parent, path, "codex/fixture-1"); err != nil {
		t.Fatal(err)
	}
	if err := materializeNativeClone(parent, path, "codex/fixture-2"); err == nil {
		t.Fatal("wrong branch adopted")
	}
	if b, err := os.ReadFile(filepath.Join(path, ".git", "objects", "info", "alternates")); err != nil || len(b) != 0 {
		t.Fatal("shared objects", err)
	}
	outside := filepath.Join(t.TempDir(), "must-not-be-created")
	contractPath := filepath.Join(path, "VALIDATION.md")
	if err := os.Symlink(outside, contractPath); err != nil {
		t.Fatal(err)
	}
	if _, err := GenerateValidationContract(github.Issue{Number: 1, Title: "test"}, path); err == nil {
		t.Fatal("validation writer followed dangling symlink")
	}
	if _, err := os.Stat(outside); !os.IsNotExist(err) {
		t.Fatal("validation writer created outside file")
	}
	os.Remove(contractPath)
	if err := RemoveWorktree(parent, path); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("owned clone not removed", err)
	}
	if _, err := os.Stat(filepath.Join(parent, ".git")); err != nil {
		t.Fatal("parent damaged", err)
	}
}
