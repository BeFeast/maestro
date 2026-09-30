package worker

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/befeast/maestro/internal/aiexecution"
)

const nativeCloneMarker = "maestro-native-clone.json"

type nativeCloneIdentity struct {
	Version    int    `json:"version"`
	Parent     string `json:"parent"`
	Worktree   string `json:"worktree"`
	Origin     string `json:"origin"`
	BaseCommit string `json:"base_commit"`
}

func nativeCloneGit(dir string, args ...string) ([]byte, error) {
	cmd := aiexecution.NativeGitCommand(append([]string{"-C", dir, "-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor=false", "-c", "credential.helper=", "-c", "protocol.ext.allow=never", "-c", "http.followRedirects=false", "--no-replace-objects"}, args...)...)
	cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=/nonexistent", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0"}
	return cmd.CombinedOutput()
}

func canonicalNativeOrigin(parent string) (string, error) {
	b, err := nativeCloneGit(parent, "remote", "get-url", "origin")
	if err != nil {
		return "", aiexecution.Held("containment_clone_origin_unavailable")
	}
	origin := strings.TrimSpace(string(b))
	u, err := url.Parse(origin)
	if err != nil || u.Scheme != "https" || u.Host != "git.oklabs.uk" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || !strings.HasPrefix(u.Path, "/BeFeast/") || !strings.HasSuffix(u.Path, ".git") || strings.Count(u.Path, "/") != 2 {
		return "", aiexecution.Held("containment_clone_origin_unsupported")
	}
	return origin, nil
}

func materializeNativeClone(parent, worktree, branch string) error {
	if _, err := os.Lstat(worktree); err == nil {
		if !isNativeCloneForRepo(parent, worktree) {
			return aiexecution.Held("containment_existing_checkout_unsupported")
		}
		return validateExactWorktreeIdentity(parent, worktree, branch)
	} else if !os.IsNotExist(err) {
		return err
	}
	origin, err := canonicalNativeOrigin(parent)
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
	identity := nativeCloneIdentity{1, filepath.Clean(parent), filepath.Clean(worktree), origin, strings.TrimSpace(string(base))}
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
	origin, err := canonicalNativeOrigin(parent)
	if err != nil || origin != identity.Origin {
		return false
	}
	childOrigin, err := canonicalNativeOrigin(worktree)
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
