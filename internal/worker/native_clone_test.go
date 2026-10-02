package worker

import (
	"encoding/json"
	"errors"
	"github.com/befeast/maestro/internal/aiexecution"
	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/github"
	"github.com/befeast/maestro/internal/state"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const fixtureNativeOrigin = "https://forge.example.test/acme/widget.git"

// fixtureForgejoConfig is a project config whose pinned forge base URL and
// repository derive fixtureNativeOrigin when base and repo are the defaults.
func fixtureForgejoConfig(base, repo string) *config.Config {
	return &config.Config{Repo: repo, Forge: config.ForgeConfig{Kind: config.ForgeKindForgejo, BaseURL: base}}
}

func expectAIExecutionHold(t *testing.T, err error, code string) {
	t.Helper()
	var hold *aiexecution.Hold
	if !errors.As(err, &hold) || hold.Code != code {
		t.Fatalf("err=%v, want hold %s", err, code)
	}
}

// mismatchedPinnedForgeConfigs are well-formed Forgejo configs that each
// derive a canonical origin other than fixtureNativeOrigin.
func mismatchedPinnedForgeConfigs() map[string]*config.Config {
	return map[string]*config.Config{
		"other host":     fixtureForgejoConfig("https://forge2.example.test", "acme/widget"),
		"subdomain host": fixtureForgejoConfig("https://evil.forge.example.test", "acme/widget"),
		"suffix host":    fixtureForgejoConfig("https://forge.example.test.evil.test", "acme/widget"),
		"other org":      fixtureForgejoConfig("https://forge.example.test", "other/widget"),
		"other repo":     fixtureForgejoConfig("https://forge.example.test", "acme/other"),
		"org case":       fixtureForgejoConfig("https://forge.example.test", "Acme/widget"),
		"repo case":      fixtureForgejoConfig("https://forge.example.test", "acme/Widget"),
	}
}

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
	forgejo := fixtureForgejoConfig
	origin, err := nativeCloneOriginForProject(forgejo("https://forge.example.test", "acme/widget"))
	if err != nil || origin != fixtureNativeOrigin {
		t.Fatalf("configured forge origin: %q %v", origin, err)
	}
	for name, cfg := range map[string]*config.Config{
		"no config":   nil,
		"github kind": {Repo: "acme/widget"},
		// A well-formed base URL must not stand in for the Forgejo kind: the
		// native helpers speak only the Forgejo API.
		"github kind with base url":  {Repo: "acme/widget", Forge: config.ForgeConfig{Kind: config.ForgeKindGitHub, BaseURL: "https://forge.example.test"}},
		"default kind with base url": {Repo: "acme/widget", Forge: config.ForgeConfig{BaseURL: "https://forge.example.test"}},
		"http":                       forgejo("http://forge.example.test", "acme/widget"),
		"port":                       forgejo("https://forge.example.test:8443", "acme/widget"),
		"userinfo":                   forgejo("https://user@forge.example.test", "acme/widget"),
		"path prefix":                forgejo("https://forge.example.test/forgejo", "acme/widget"),
		"host case":                  forgejo("https://FORGE.example.test", "acme/widget"),
		"trailing dot":               forgejo("https://forge.example.test.", "acme/widget"),
		"repo shape":                 forgejo("https://forge.example.test", "acme"),
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

// The native form is stricter than config.RepositoryIdentity but must agree
// with it wherever both accept the base URL, so the two never name different
// repositories for the same project config.
func TestNativeCloneOriginAgreesWithRepositoryIdentityForCanonicalInputs(t *testing.T) {
	for _, base := range []string{"https://forge.example.test", "https://forge.example.test/"} {
		for _, repo := range []string{"acme/widget", "Acme/Widget", "acme-1/widget_2.js"} {
			cfg := fixtureForgejoConfig(base, repo)
			native, err := nativeCloneOriginForProject(cfg)
			if err != nil {
				t.Fatalf("%s %s: %v", base, repo, err)
			}
			identity, err := cfg.Forge.RepositoryIdentity(cfg.Repo)
			if err != nil || identity.FetchURL() != native || !identity.MatchesOrigin(native, true) {
				t.Fatalf("%s %s: native %q, identity %q %v", base, repo, native, identity.FetchURL(), err)
			}
		}
	}
}

// The daemon refuses to create a native clone unless the parent checkout's
// origin is byte for byte the origin derived from the pinned project config.
// The refusal happens before any clone or kernel step, so it runs everywhere.
func TestNativeCloneMaterializeHoldsWhenParentOriginIsNotPinned(t *testing.T) {
	parent := newBranchTestRepo(t)
	runBranchGit(t, parent, "remote", "add", "origin", fixtureNativeOrigin)
	runBranchGit(t, parent, "update-ref", "refs/remotes/origin/main", "HEAD")
	refuse := func(name string, cfg *config.Config) {
		t.Helper()
		path := filepath.Join(t.TempDir(), "assigned")
		err := materializeNativeCloneForProject(cfg, parent, path, "codex/fixture-1")
		var hold *aiexecution.Hold
		if !errors.As(err, &hold) || hold.Code != "containment_clone_origin_unsupported" {
			t.Fatalf("%s: err=%v, want containment_clone_origin_unsupported", name, err)
		}
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatalf("%s: clone path created before the origin was accepted: %v", name, err)
		}
	}
	// Config pins another destination than the parent's (canonical) origin.
	for name, cfg := range mismatchedPinnedForgeConfigs() {
		refuse("config "+name, cfg)
	}
	// Config is correct; the parent's origin is a lookalike of the pinned one.
	pinned := fixtureForgejoConfig("https://forge.example.test", "acme/widget")
	for _, lookalike := range []string{
		"https://forge2.example.test/acme/widget.git",
		"https://evil.forge.example.test/acme/widget.git",
		"https://forge.example.test.evil.test/acme/widget.git",
		"https://forge.example.test/other/widget.git",
		"https://forge.example.test/acme/other.git",
		"https://forge.example.test/Acme/widget.git",
		"https://FORGE.example.test/acme/widget.git",
		"https://forge.example.test./acme/widget.git",
		"https://forge.example.test:443/acme/widget.git",
		"https://forge.example.test:8443/acme/widget.git",
		"http://forge.example.test/acme/widget.git",
		"https://user@forge.example.test/acme/widget.git",
	} {
		runBranchGit(t, parent, "remote", "set-url", "origin", lookalike)
		refuse("parent "+lookalike, pinned)
	}
}

// startReserved must hand the project config to the clone check, so a parent
// origin that differs from the pinned forge holds the spawn before a clone
// exists or a process starts.
func TestNativeStartHoldsWhenParentOriginDiffersFromPinnedForge(t *testing.T) {
	f := nativeTestFixture(t)
	f.cfg.WorkerRuntime = config.WorkerRuntimeConfig{Mode: config.WorkerRuntimeModeIsolated, Scope: config.WorkerRuntimeScopeSystem}
	worktree := filepath.Join(f.cfg.WorktreeBase, f.slot)
	runBranchGit(t, f.cfg.LocalPath, "worktree", "remove", "--force", worktree)
	f.cfg.Repo = "acme/widget"
	f.cfg.Forge = config.ForgeConfig{Kind: config.ForgeKindForgejo, BaseURL: "https://forge.example.test"}
	runBranchGit(t, f.cfg.LocalPath, "remote", "set-url", "origin", "https://forge2.example.test/acme/widget.git")
	f.cfg.AIExecution.RequireVerifiedRoute = true
	f.cfg.AIExecution = f.cfg.AIExecution.BindController(f.cfg.AIExecution, f.cfg.StateDir)
	_, err := f.start()
	expectAIExecutionHold(t, err, "containment_clone_origin_unsupported")
	if f.spawned != 0 {
		t.Fatal("spawned with an unpinned parent origin")
	}
	if _, err := os.Lstat(worktree); !os.IsNotExist(err) {
		t.Fatalf("clone created with an unpinned parent origin: %v", err)
	}
}

func writeNativeCloneIdentityFixture(t *testing.T, worktree string, identity nativeCloneIdentity) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(worktree, ".git"), 0700); err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(identity)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worktree, ".git", nativeCloneMarker), b, 0600); err != nil {
		t.Fatal(err)
	}
}

// In-place relaunches reuse the retained clone without materializing it, while
// the sandboxed helpers take the host from its identity. The identity must
// still name the origin derived from the current pinned config.
func TestNativeRelaunchHoldsWhenRetainedCloneOriginIsNotPinned(t *testing.T) {
	pinned := fixtureForgejoConfig("https://forge.example.test", "acme/widget")
	pinned.AIExecution.RequireVerifiedRoute = true
	worktree := filepath.Join(t.TempDir(), "assigned")
	identity := nativeCloneIdentity{Version: 1, Parent: "/parent", Worktree: worktree, Origin: fixtureNativeOrigin, BaseCommit: strings.Repeat("a", 40)}
	writeNativeCloneIdentityFixture(t, worktree, identity)
	if err := verifyNativeRelaunchCloneOrigin(pinned, worktree); err != nil {
		t.Fatalf("pinned identity refused: %v", err)
	}
	// The project config changed after the clone was created.
	for name, cfg := range mismatchedPinnedForgeConfigs() {
		cfg.AIExecution.RequireVerifiedRoute = true
		err := verifyNativeRelaunchCloneOrigin(cfg, worktree)
		var hold *aiexecution.Hold
		if !errors.As(err, &hold) || hold.Code != "containment_clone_origin_unsupported" {
			t.Fatalf("config %s: err=%v", name, err)
		}
	}
	unpinned := fixtureForgejoConfig("https://forge.example.test:8443", "acme/widget")
	unpinned.AIExecution.RequireVerifiedRoute = true
	expectAIExecutionHold(t, verifyNativeRelaunchCloneOrigin(unpinned, worktree), "containment_clone_origin_unsupported")
	// The retained identity names a lookalike of the pinned origin.
	for _, lookalike := range []string{
		"https://forge2.example.test/acme/widget.git",
		"https://evil.forge.example.test/acme/widget.git",
		"https://forge.example.test/other/widget.git",
		"https://forge.example.test/Acme/widget.git",
		"https://FORGE.example.test/acme/widget.git",
		"https://forge.example.test./acme/widget.git",
		"https://forge.example.test:443/acme/widget.git",
		"http://forge.example.test/acme/widget.git",
		"https://user@forge.example.test/acme/widget.git",
	} {
		stale := identity
		stale.Origin = lookalike
		writeNativeCloneIdentityFixture(t, worktree, stale)
		expectAIExecutionHold(t, verifyNativeRelaunchCloneOrigin(pinned, worktree), "containment_clone_origin_unsupported")
	}
	// Outside strict native execution there is no clone identity to check.
	legacy := fixtureForgejoConfig("https://forge2.example.test", "acme/widget")
	if err := verifyNativeRelaunchCloneOrigin(legacy, worktree); err != nil {
		t.Fatalf("non-native relaunch held: %v", err)
	}
	// An identity that exists but is not exactly the daemon's fails closed.
	other := identity
	other.Worktree = filepath.Join(filepath.Dir(worktree), "other")
	writeNativeCloneIdentityFixture(t, worktree, other)
	expectAIExecutionHold(t, verifyNativeRelaunchCloneOrigin(pinned, worktree), "containment_clone_identity_unavailable")
	if err := os.WriteFile(filepath.Join(worktree, ".git", nativeCloneMarker), []byte(`{"version":1,"worktree":"`+worktree+`","origin":"`+fixtureNativeOrigin+`","extra":1}`), 0600); err != nil {
		t.Fatal(err)
	}
	expectAIExecutionHold(t, verifyNativeRelaunchCloneOrigin(pinned, worktree), "containment_clone_identity_unavailable")
	if err := os.Remove(filepath.Join(worktree, ".git", nativeCloneMarker)); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(worktree, ".git", nativeCloneMarker), 0700); err != nil {
		t.Fatal(err)
	}
	expectAIExecutionHold(t, verifyNativeRelaunchCloneOrigin(pinned, worktree), "containment_clone_identity_unavailable")
	expectAIExecutionHold(t, verifyNativeRelaunchCloneOrigin(pinned, "assigned"), "containment_clone_identity_unavailable")
	// Without an identity nothing carries a host into the sandbox: the
	// monitor's read-only identity bind and the in-sandbox helpers fail closed
	// on their own, so only a present identity is checked here.
	if err := os.Remove(filepath.Join(worktree, ".git", nativeCloneMarker)); err != nil {
		t.Fatal(err)
	}
	if err := verifyNativeRelaunchCloneOrigin(pinned, worktree); err != nil {
		t.Fatalf("clone without identity: %v", err)
	}
	if err := verifyNativeRelaunchCloneOrigin(pinned, filepath.Join(t.TempDir(), "absent")); err != nil {
		t.Fatalf("absent checkout: %v", err)
	}
	// A symlinked .git is never followed to an identity elsewhere.
	linked, real := filepath.Join(t.TempDir(), "linked"), filepath.Join(t.TempDir(), "real")
	writeNativeCloneIdentityFixture(t, real, nativeCloneIdentity{Version: 1, Parent: "/parent", Worktree: linked, Origin: fixtureNativeOrigin, BaseCommit: strings.Repeat("a", 40)})
	if err := os.MkdirAll(linked, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(real, ".git"), filepath.Join(linked, ".git")); err != nil {
		t.Fatal(err)
	}
	expectAIExecutionHold(t, verifyNativeRelaunchCloneOrigin(pinned, linked), "containment_clone_identity_unavailable")
}

// RespawnInPlace and StartPhase must run the retained-clone check before a
// successor is registered or the running worker is stopped.
func TestNativeInPlaceRelaunchHoldsWhenRetainedCloneOriginIsStale(t *testing.T) {
	for _, entry := range []string{"in_place", "phase"} {
		t.Run(entry, func(t *testing.T) {
			f := nativeTestFixture(t)
			if _, err := f.start(); err != nil {
				t.Fatal(err)
			}
			f.cfg.WorkerLaunchContext = nil
			sess := f.st.Sessions[f.slot]
			// Stand-in retained native clone created while the project
			// pointed at another forge host.
			clone := filepath.Join(t.TempDir(), "assigned")
			writeNativeCloneIdentityFixture(t, clone, nativeCloneIdentity{Version: 1, Parent: f.cfg.LocalPath, Worktree: clone, Origin: "https://forge2.example.test/acme/widget.git", BaseCommit: strings.Repeat("a", 40)})
			sess.Worktree = clone
			f.cfg.Repo = "acme/widget"
			f.cfg.Forge = config.ForgeConfig{Kind: config.ForgeKindForgejo, BaseURL: "https://forge.example.test"}
			f.cfg.AIExecution.RequireVerifiedRoute = true
			spawned, registered := f.spawned, len(f.registered)
			var err error
			switch entry {
			case "in_place":
				err = RespawnInPlace(f.cfg, f.slot, sess, f.cfg.Repo, f.issue, "prompt", "claude")
			case "phase":
				sess.Phase = state.PhaseAdvisor
				err = StartPhase(f.cfg, sess, f.slot, "prompt", "claude")
			}
			expectAIExecutionHold(t, err, "containment_clone_origin_unsupported")
			if f.spawned != spawned || len(f.registered) != registered || f.stopped != 0 || sess.WorkerGeneration != 1 {
				t.Fatalf("stale clone origin changed runtime: spawned=%d registered=%d stopped=%d generation=%d", f.spawned, len(f.registered), f.stopped, sess.WorkerGeneration)
			}
		})
	}
}

// Kernel fixture: reusing a retained clone after the pinned config changed is
// refused on both the materialize (Respawn/start) and the relaunch path, and
// the original config still reuses it.
func TestNativeCloneReuseHoldsWhenPinnedOriginChanges(t *testing.T) {
	if os.Getenv("MAESTRO_NATIVE_KERNEL_TESTS") != "1" {
		t.Skip("set MAESTRO_NATIVE_KERNEL_TESTS=1 for native clone kernel fixture")
	}
	parent := newBranchTestRepo(t)
	runBranchGit(t, parent, "remote", "add", "origin", fixtureNativeOrigin)
	runBranchGit(t, parent, "update-ref", "refs/remotes/origin/main", "HEAD")
	pinned := fixtureForgejoConfig("https://forge.example.test", "acme/widget")
	pinned.AIExecution.RequireVerifiedRoute = true
	path := filepath.Join(t.TempDir(), "assigned")
	if err := materializeNativeCloneForProject(pinned, parent, path, "codex/fixture-1"); err != nil {
		t.Fatal(err)
	}
	if err := verifyNativeRelaunchCloneOrigin(pinned, path); err != nil {
		t.Fatalf("relaunch of pinned clone: %v", err)
	}
	for name, cfg := range mismatchedPinnedForgeConfigs() {
		cfg.AIExecution.RequireVerifiedRoute = true
		err := materializeNativeCloneForProject(cfg, parent, path, "codex/fixture-1")
		var hold *aiexecution.Hold
		if !errors.As(err, &hold) || hold.Code != "containment_clone_origin_unsupported" {
			t.Fatalf("reuse under %s: err=%v", name, err)
		}
		if err := verifyNativeRelaunchCloneOrigin(cfg, path); !errors.As(err, &hold) || hold.Code != "containment_clone_origin_unsupported" {
			t.Fatalf("relaunch under %s: err=%v", name, err)
		}
	}
	if err := materializeNativeCloneForProject(pinned, parent, path, "codex/fixture-1"); err != nil {
		t.Fatalf("reuse under pinned config: %v", err)
	}
}
