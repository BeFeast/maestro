package worker

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/befeast/maestro/internal/aiexecution"
	"github.com/befeast/maestro/internal/config"
)

// The native forge helpers read this identity inside the sandbox, so its
// schema lives with them.
const nativeCloneMarker = aiexecution.NativeCloneMarker

type nativeCloneIdentity = aiexecution.NativeCloneIdentity

func nativeCloneGit(dir string, args ...string) ([]byte, error) {
	cmd := aiexecution.NativeGitCommand(append([]string{"-C", dir, "-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor=false", "-c", "credential.helper=", "-c", "protocol.ext.allow=never", "-c", "http.followRedirects=false", "--no-replace-objects"}, args...)...)
	cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=/nonexistent", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0"}
	return cmd.CombinedOutput()
}

// nativeCloneOriginForProject derives the only origin a native clone may carry
// from the project's forge base URL and repository, both covered by the
// execution manifest's project config digest. The native helpers speak the
// Forgejo API, so every other forge kind fails closed.
func nativeCloneOriginForProject(cfg *config.Config) (string, error) {
	if cfg == nil || !cfg.Forge.IsForgejo() {
		return "", aiexecution.Held("containment_clone_origin_unsupported")
	}
	origin, err := aiexecution.NativeForgejoOrigin(cfg.Forge.BaseURL, cfg.Repo)
	if err != nil {
		return "", aiexecution.Held("containment_clone_origin_unsupported")
	}
	return origin, nil
}

// nativeCloneOrigin reads dir's origin and accepts only the canonical native
// forge shape. The project's pinned host and repository are enforced by
// canonicalNativeOrigin whenever a clone is materialized or reused.
func nativeCloneOrigin(dir string) (string, error) {
	b, err := nativeCloneGit(dir, "remote", "get-url", "origin")
	if err != nil {
		return "", aiexecution.Held("containment_clone_origin_unavailable")
	}
	origin := strings.TrimSpace(string(b))
	if _, err := aiexecution.ParseNativeForgejoOrigin(origin); err != nil {
		return "", aiexecution.Held("containment_clone_origin_unsupported")
	}
	return origin, nil
}

// canonicalNativeOrigin returns dir's origin only when it is byte-for-byte the
// origin derived from the project's pinned forge configuration.
func canonicalNativeOrigin(dir, expected string) (string, error) {
	origin, err := nativeCloneOrigin(dir)
	if err != nil {
		return "", err
	}
	if origin != expected {
		return "", aiexecution.Held("containment_clone_origin_unsupported")
	}
	return origin, nil
}

func materializeNativeCloneForProject(cfg *config.Config, parent, worktree, branch string) error {
	origin, err := nativeCloneOriginForProject(cfg)
	if err != nil {
		return err
	}
	return materializeNativeClone(parent, worktree, branch, origin)
}

// verifyNativeRelaunchCloneOrigin is the in-place relaunch counterpart of the
// materialize check. RespawnInPlace and StartPhase reuse the retained clone
// without materializing it, and the sandboxed forge helpers take the host from
// that clone's daemon-written identity. A forge.base_url or repo change made
// after the clone was created must hold the relaunch instead of silently
// keeping the old destination.
//
// Only an identity that exists can carry a host into the sandbox. Without one
// (no .git directory, or no identity file in it) the native monitor's
// read-only identity bind and the in-sandbox helpers already fail closed, so
// there is nothing stale to check here. An identity that exists but cannot be
// read exactly holds. Git never runs in the retained clone here.
func verifyNativeRelaunchCloneOrigin(cfg *config.Config, worktree string) error {
	if cfg == nil || !cfg.AIExecution.RequireVerifiedRoute {
		return nil
	}
	if !filepath.IsAbs(worktree) || filepath.Clean(worktree) != worktree {
		return aiexecution.Held("containment_clone_identity_unavailable")
	}
	gitDir := filepath.Join(worktree, ".git")
	st, err := os.Lstat(gitDir)
	switch {
	case os.IsNotExist(err) || err == nil && st.Mode().IsRegular():
		return nil
	case err != nil || !st.IsDir():
		return aiexecution.Held("containment_clone_identity_unavailable")
	}
	marker := filepath.Join(gitDir, nativeCloneMarker)
	if _, err := os.Lstat(marker); os.IsNotExist(err) {
		return nil
	}
	b, err := readOwnedRegularNoFollow(marker, 16<<10)
	var identity nativeCloneIdentity
	if err != nil || aiexecution.DecodeStrict(b, &identity) != nil || identity.Version != 1 || identity.Worktree != worktree {
		return aiexecution.Held("containment_clone_identity_unavailable")
	}
	expected, err := nativeCloneOriginForProject(cfg)
	if err != nil {
		return err
	}
	if identity.Origin != expected {
		return aiexecution.Held("containment_clone_origin_unsupported")
	}
	return nil
}

func materializeNativeClone(parent, worktree, branch, expectedOrigin string) error {
	if _, err := os.Lstat(worktree); err == nil {
		// Initial setup can stop after writing clone identity but before Git
		// registration (for example, a writable ancestor). Recovery must install
		// the sandbox before inspecting even that partially prepared checkout.
		if err := aiexecution.RegisterNativeGit(worktree); err != nil {
			return err
		}
		// The retained identity must still name the currently pinned origin;
		// isNativeCloneForRepo ties it to the parent's origin.
		if _, err := canonicalNativeOrigin(parent, expectedOrigin); err != nil {
			return err
		}
		if !isNativeCloneForRepo(parent, worktree) {
			return aiexecution.Held("containment_existing_checkout_unsupported")
		}
		return validateExactWorktreeIdentity(parent, worktree, branch)
	} else if !os.IsNotExist(err) {
		return err
	}
	origin, err := canonicalNativeOrigin(parent, expectedOrigin)
	if err != nil {
		return err
	}
	base, err := nativeCloneGit(parent, "rev-parse", "--verify", "origin/"+defaultBaseBranch+"^{commit}")
	if err != nil {
		return aiexecution.Held("containment_clone_base_unavailable")
	}
	if out, err := nativeCloneGit(parent, "clone", "--no-local", "--no-hardlinks", "--no-checkout", "--template=", "--config", "core.hooksPath=/dev/null", "--", parent, worktree); err != nil {
		return fmt.Errorf("materialize native clone: %w: %s", err, out)
	}
	if out, err := nativeCloneGit(worktree, "remote", "set-url", "origin", origin); err != nil {
		return fmt.Errorf("native clone origin: %w: %s", err, out)
	}
	if out, err := nativeCloneGit(worktree, "checkout", "-b", branch, strings.TrimSpace(string(base))); err != nil {
		return fmt.Errorf("native clone branch: %w: %s", err, out)
	}
	config := "[core]\n\trepositoryformatversion = 0\n\tfilemode = true\n\tbare = false\n\tlogallrefupdates = true\n\thooksPath = /dev/null\n\tfsmonitor = false\n[protocol \"ext\"]\n\tallow = never\n[http]\n\tfollowRedirects = false\n[remote \"origin\"]\n\turl = " + origin + "\n\tfetch = +refs/heads/*:refs/remotes/origin/*\n"
	if err := os.WriteFile(filepath.Join(worktree, ".git", "config"), []byte(config), 0600); err != nil {
		return err
	}
	for rel, value := range map[string]string{"commondir": ".\n", "config.worktree": "", "objects/info/alternates": "", "objects/info/http-alternates": ""} {
		path := filepath.Join(worktree, ".git", rel)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			return err
		}
		if err := os.WriteFile(path, []byte(value), 0600); err != nil {
			return err
		}
	}
	if err := os.Chmod(worktree, 0700); err != nil {
		return err
	}
	identity := nativeCloneIdentity{Version: 1, Parent: filepath.Clean(parent), Worktree: filepath.Clean(worktree), Origin: origin, BaseCommit: strings.TrimSpace(string(base))}
	b, err := json.Marshal(identity)
	if err != nil {
		return err
	}
	if err := writeFileAtomicMode(filepath.Join(worktree, ".git"), filepath.Join(worktree, ".git", nativeCloneMarker), string(b), 0600); err != nil {
		return err
	}
	return aiexecution.RegisterNativeGit(worktree)
}

func isNativeCloneForRepo(parent, worktree string) bool {
	st, err := os.Lstat(filepath.Join(worktree, ".git"))
	if err != nil || !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
		return false
	}
	b, err := readOwnedRegularNoFollow(filepath.Join(worktree, ".git", nativeCloneMarker), 16<<10)
	var identity nativeCloneIdentity
	if err != nil || aiexecution.DecodeStrict(b, &identity) != nil || identity.Version != 1 || identity.Parent != filepath.Clean(parent) || identity.Worktree != filepath.Clean(worktree) || len(identity.BaseCommit) != 40 {
		return false
	}
	origin, err := nativeCloneOrigin(parent)
	if err != nil || origin != identity.Origin {
		return false
	}
	childOrigin, err := nativeCloneOrigin(worktree)
	if err != nil || childOrigin != origin {
		return false
	}
	for rel, value := range map[string]string{"commondir": ".\n", "config.worktree": "", "objects/info/alternates": "", "objects/info/http-alternates": ""} {
		b, err := readOwnedRegularNoFollow(filepath.Join(worktree, ".git", rel), 16)
		if err != nil || string(b) != value {
			return false
		}
	}
	top, err := nativeCloneGit(worktree, "rev-parse", "--show-toplevel")
	if err != nil || strings.TrimSpace(string(top)) != filepath.Clean(worktree) {
		return false
	}
	_, err = nativeCloneGit(worktree, "merge-base", "--is-ancestor", identity.BaseCommit, "HEAD")
	return err == nil
}
