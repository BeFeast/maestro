package worker

import (
	"github.com/befeast/maestro/internal/aiexecution"
	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/github"
	"os"
	"path/filepath"
	"testing"
)

const fixtureNativeOrigin = "https://forge.example.test/acme/widget.git"

func TestNativeCloneMaterializesAssignedPathWithoutSharedGitMetadata(t *testing.T) {
	if os.Getenv("MAESTRO_NATIVE_KERNEL_TESTS") != "1" {
		t.Skip("set MAESTRO_NATIVE_KERNEL_TESTS=1 for native clone kernel fixture")
	}
	parent := newBranchTestRepo(t)
	runBranchGit(t, parent, "remote", "add", "origin", fixtureNativeOrigin)
	runBranchGit(t, parent, "update-ref", "refs/remotes/origin/main", "HEAD")
	path := filepath.Join(t.TempDir(), "assigned")
	if err := materializeNativeClone(parent, path, "codex/fixture-1", fixtureNativeOrigin); err != nil {
		t.Fatal(err)
	}
	if !isNativeCloneForRepo(parent, path) || !isGitWorktreeForRepo(parent, path) {
		out, err := nativeCloneGit(path, "rev-parse", "--show-toplevel")
		t.Fatalf("clone identity lost: %s %v", out, err)
	}
	if err := validateExactWorktreeIdentity(parent, path, "codex/fixture-1"); err != nil {
		t.Fatal(err)
	}
	if err := materializeNativeClone(parent, path, "codex/fixture-2", fixtureNativeOrigin); err == nil {
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

func TestNativeCloneRecoveryRegistersSandboxBeforeInspectingRetainedClone(t *testing.T) {
	if os.Getenv("MAESTRO_NATIVE_KERNEL_TESTS") != "1" {
		t.Skip("set MAESTRO_NATIVE_KERNEL_TESTS=1 for native clone kernel fixture")
	}
	parent := newBranchTestRepo(t)
	runBranchGit(t, parent, "remote", "add", "origin", fixtureNativeOrigin)
	runBranchGit(t, parent, "update-ref", "refs/remotes/origin/main", "HEAD")
	ancestor := t.TempDir()
	if err := os.Chmod(ancestor, 0775); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(ancestor, "assigned")
	if err := materializeNativeClone(parent, path, "codex/fixture-1", fixtureNativeOrigin); err == nil {
		t.Fatal("writable ancestor accepted")
	}
	if aiexecution.NativeGitRegistered(path) {
		t.Fatal("unsafe path registered")
	}
	if err := os.Chmod(ancestor, 0700); err != nil {
		t.Fatal(err)
	}
	if err := materializeNativeClone(parent, path, "codex/fixture-1", fixtureNativeOrigin); err != nil {
		t.Fatal(err)
	}
	if !aiexecution.NativeGitRegistered(path) || !isNativeCloneForRepo(parent, path) {
		t.Fatal("retained clone lacks sandbox registration")
	}
}

func TestNativeCloneOriginComesFromPinnedProjectForge(t *testing.T) {
	forgejo := func(base, repo string) *config.Config {
		return &config.Config{Repo: repo, Forge: config.ForgeConfig{Kind: config.ForgeKindForgejo, BaseURL: base}}
	}
	origin, err := nativeCloneOriginForProject(forgejo("https://forge.example.test", "acme/widget"))
	if err != nil || origin != fixtureNativeOrigin {
		t.Fatalf("configured forge origin: %q %v", origin, err)
	}
	for name, cfg := range map[string]*config.Config{
		"no config":    nil,
		"github kind":  {Repo: "acme/widget"},
		"http":         forgejo("http://forge.example.test", "acme/widget"),
		"port":         forgejo("https://forge.example.test:8443", "acme/widget"),
		"userinfo":     forgejo("https://user@forge.example.test", "acme/widget"),
		"path prefix":  forgejo("https://forge.example.test/forgejo", "acme/widget"),
		"host case":    forgejo("https://FORGE.example.test", "acme/widget"),
		"trailing dot": forgejo("https://forge.example.test.", "acme/widget"),
		"repo shape":   forgejo("https://forge.example.test", "acme"),
	} {
		if got, err := nativeCloneOriginForProject(cfg); err == nil {
			t.Fatalf("%s: accepted origin %q", name, got)
		}
	}
	parent := newBranchTestRepo(t)
	runBranchGit(t, parent, "remote", "add", "origin", fixtureNativeOrigin)
	if got, err := canonicalNativeOrigin(parent, origin); err != nil || got != origin {
		t.Fatalf("configured parent origin: %q %v", got, err)
	}
	for _, lookalike := range []string{
		"https://evil.forge.example.test/acme/widget.git",
		"https://forge.example.test.evil.test/acme/widget.git",
		"https://forge.example.test:443/acme/widget.git",
		"https://forge.example.test:8443/acme/widget.git",
		"http://forge.example.test/acme/widget.git",
		"https://forge.example.test/other/widget.git",
		"https://forge.example.test/acme/other.git",
		"https://FORGE.example.test/acme/widget.git",
		"https://forge.example.test/Acme/widget.git",
		"https://forge.example.test./acme/widget.git",
		"https://user@forge.example.test/acme/widget.git",
	} {
		runBranchGit(t, parent, "remote", "set-url", "origin", lookalike)
		if got, err := canonicalNativeOrigin(parent, origin); err == nil {
			t.Fatalf("parent origin %q accepted as %q", lookalike, got)
		}
	}
}
