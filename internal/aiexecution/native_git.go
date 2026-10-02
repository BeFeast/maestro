//go:build linux

package aiexecution

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Native Git registration lives outside the writable clone. Removing or
// poisoning .git must never turn a sandboxed checkout back into a host checkout.
type nativeGitRegistration struct {
	Version    int       `json:"version"`
	Worktree   string    `json:"worktree"`
	Bubblewrap FileProof `json:"bubblewrap"`
	Git        FileProof `json:"git"`
	Shell      FileProof `json:"shell"`
}

func nativeGitRegistrationPath(worktree string) string {
	return filepath.Join(filepath.Dir(worktree), ".maestro-native-git", digest([]byte(worktree))+".json")
}

func NativeGitRegistered(worktree string) bool {
	return nativeGitRoot(worktree) != ""
}

func nativeGitRoot(path string) string {
	path, err := filepath.Abs(path)
	if err != nil {
		return "/"
	}
	for path != "/" {
		if _, err := os.Lstat(nativeGitRegistrationPath(path)); !os.IsNotExist(err) {
			return path
		}
		path = filepath.Dir(path)
	}
	return ""
}

// RegisterNativeGit is called before exposing a fresh clone to a native tool.
// Pins come from root-owned installed binaries. No credential/config from the
// host is subsequently exposed to Git, including during recovery/checkpoint.
func RegisterNativeGit(worktree string) error {
	if !filepath.IsAbs(worktree) || filepath.Clean(worktree) != worktree || verifyOwnedPath(worktree, uint32(os.Getuid()), true) != nil {
		return Held("native_git_worktree_unsafe")
	}
	if NativeGitRegistered(worktree) {
		_, err := readNativeGitRegistration(worktree)
		return err
	}
	r := nativeGitRegistration{Version: 1, Worktree: worktree}
	for _, target := range []*FileProof{&r.Bubblewrap, &r.Git, &r.Shell} {
		path := "/usr/bin/git"
		if target == &r.Shell {
			path = "/usr/bin/dash"
		}
		if target == &r.Bubblewrap {
			path = "/usr/bin/bwrap"
		}
		if verifyOwnedPath(path, 0, false) != nil {
			return Held("native_git_executable_unsafe")
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return Held("native_git_executable_unavailable")
		}
		*target = FileProof{Path: path, SHA256: digest(b)}
	}
	path := nativeGitRegistrationPath(worktree)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil || verifyOwnedPath(filepath.Dir(path), uint32(os.Getuid()), true) != nil {
		return Held("native_git_registry_unsafe")
	}
	filter, err := nativeSeccompBytes()
	if err != nil {
		return err
	}
	if err := replaceRevisionFile(path+".bpf", filter); err != nil {
		return err
	}
	b, _ := json.Marshal(r)
	return replaceRevisionFile(path, b)
}

func readNativeGitRegistration(worktree string) (nativeGitRegistration, error) {
	var r nativeGitRegistration
	b, err := readNativeEvidence(nativeGitRegistrationPath(worktree), uint32(os.Getuid()), 16<<10)
	if err != nil || DecodeStrict(b, &r) != nil || r.Version != 1 || r.Worktree != worktree {
		return r, Held("native_git_registration_invalid")
	}
	for _, p := range []FileProof{r.Bubblewrap, r.Git, r.Shell} {
		if verifyOwnedPath(p.Path, 0, false) != nil || VerifyFile(p) != nil {
			return r, Held("native_git_executable_drift")
		}
	}
	return r, nil
}

// NativeGitCommandContext preserves legacy Git behavior outside registered
// clones. A registered clone always runs inside an offline mount/PID/user/net
// namespace, even if its Git metadata was removed or concurrently poisoned.
// Absolute paths are preserved inside this namespace for existing callers.
// cmd.Env may be set by callers: --clearenv still prevents inheritance by Git.
func NativeGitCommandContext(ctx context.Context, args ...string) *exec.Cmd {
	var worktree string
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "-C" {
			worktree = args[i+1]
			break
		}
	}
	if worktree == "" || !NativeGitRegistered(worktree) {
		return exec.CommandContext(ctx, "git", args...)
	}
	worktree, err := filepath.Abs(worktree)
	root := nativeGitRoot(worktree)
	var r nativeGitRegistration
	if err == nil {
		r, err = readNativeGitRegistration(root)
	}
	failed := func(err error) *exec.Cmd {
		cmd := exec.CommandContext(ctx, "/nonexistent/maestro-native-git-held")
		cmd.Err = err
		return cmd
	}
	if err != nil {
		return failed(err)
	}
	if verifyOwnedPath(root, uint32(os.Getuid()), true) != nil {
		return failed(Held("native_git_worktree_unsafe"))
	}
	for _, arg := range args {
		if arg == "fetch" || arg == "push" || arg == "ls-remote" || arg == "clone" {
			return failed(Held("native_git_network_requires_native_tool"))
		}
	}
	bwrap := []string{"--unshare-all", "--unshare-user", "--seccomp", "3", "--die-with-parent", "--new-session", "--cap-drop", "ALL", "--clearenv", "--ro-bind", "/usr", "/usr", "--symlink", "usr/bin", "/bin", "--symlink", "usr/sbin", "/sbin", "--symlink", "usr/lib", "/lib", "--symlink", "usr/lib64", "/lib64", "--proc", "/proc", "--dev", "/dev", "--tmpfs", "/tmp", "--tmpfs", "/home", "--dir", "/home/native", "--bind", root, root, "--chdir", worktree,
		"--setenv", "PATH", "/usr/bin:/bin", "--setenv", "HOME", "/home/native", "--setenv", "LANG", "C.UTF-8", "--setenv", "GIT_CONFIG_NOSYSTEM", "1", "--setenv", "GIT_CONFIG_GLOBAL", "/dev/null", "--setenv", "GIT_TERMINAL_PROMPT", "0", "--setenv", "GIT_AUTHOR_NAME", "Maestro", "--setenv", "GIT_AUTHOR_EMAIL", "maestro@localhost", "--setenv", "GIT_COMMITTER_NAME", "Maestro", "--setenv", "GIT_COMMITTER_EMAIL", "maestro@localhost",
		r.Git.Path, "--no-replace-objects", "-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor=false", "-c", "credential.helper=", "-c", "protocol.ext.allow=never", "-c", "diff.external=", "-c", "core.pager=cat"}
	// Keep exactly one cwd. No caller may point a second -C outside the clone.
	for i := 0; i < len(args); i++ {
		if args[i] == "-C" {
			i++
			if i >= len(args) || filepath.Clean(args[i]) != worktree {
				return failed(Held("native_git_directory_invalid"))
			}
			continue
		}
		if strings.IndexByte(args[i], 0) >= 0 {
			return failed(Held("native_git_arguments_invalid"))
		}
		bwrap = append(bwrap, args[i])
	}
	filter, err := nativeSeccompBytes()
	if err != nil {
		return failed(err)
	}
	saved, err := readNativeEvidence(nativeGitRegistrationPath(root)+".bpf", uint32(os.Getuid()), 4096)
	if err != nil || digest(saved) != digest(filter) {
		return failed(Held("native_git_seccomp_drift"))
	}
	shellArgs := []string{"-c", `exec 3<"$1"; shift; exec "$@"`, "maestro-native-git", nativeGitRegistrationPath(root) + ".bpf", r.Bubblewrap.Path}
	return exec.CommandContext(ctx, r.Shell.Path, append(shellArgs, bwrap...)...)
}

func NativeGitCommand(args ...string) *exec.Cmd {
	return NativeGitCommandContext(context.Background(), args...)
}
