package aiexecution

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

func verifyOwnedPath(path string, uid uint32, directory bool) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return Held("containment_path_unsafe")
	}
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	current := "/"
	for i, part := range parts {
		current = filepath.Join(current, part)
		st, err := os.Lstat(current)
		if err != nil || st.Mode()&os.ModeSymlink != 0 || st.Mode().Perm()&0022 != 0 {
			return Held("containment_path_unsafe")
		}
		stat, ok := st.Sys().(*syscall.Stat_t)
		if !ok || stat.Uid != 0 && stat.Uid != uid {
			return Held("containment_path_unsafe")
		}
		if i == len(parts)-1 {
			if stat.Uid != uid || st.IsDir() != directory {
				return Held("containment_path_unsafe")
			}
		} else if !st.IsDir() {
			return Held("containment_path_unsafe")
		}
	}
	return nil
}

func readRootContainmentEvidence(pin FileProof) ([]byte, error) {
	b, err := readNativeOwnedFile(pin.Path, 0, 128<<10, 0022)
	if err != nil || digest(b) != pin.SHA256 {
		return nil, Held("containment_evidence_invalid")
	}
	return b, nil
}

func namespaceMatches(path string, dev, ino uint64) bool {
	st, err := os.Stat(path)
	if err != nil {
		return false
	}
	s, ok := st.Sys().(*syscall.Stat_t)
	return ok && uint64(s.Dev) == dev && s.Ino == ino
}

// A bind-mounted nsfs inode can have the overflow owner inside a user namespace.
// Trust its root-controlled name and kernel namespace type, not that inode's UID.
// Ordinary files still use verifyOwnedPath with the exact required owner.
func openPinnedNetworkNamespace(path string, dev, ino uint64) (*os.File, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || verifyOwnedPath(filepath.Dir(path), 0, true) != nil {
		return nil, Held("containment_namespace_drift")
	}
	fd, err := unix.Openat2(unix.AT_FDCWD, path, &unix.OpenHow{Flags: unix.O_RDONLY | unix.O_CLOEXEC | unix.O_NONBLOCK, Resolve: unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_MAGICLINKS})
	if err != nil {
		return nil, Held("containment_namespace_drift")
	}
	f := os.NewFile(uintptr(fd), path)
	var st unix.Stat_t
	var fs unix.Statfs_t
	kind, kindErr := unix.IoctlRetInt(fd, unix.NS_GET_NSTYPE)
	if unix.Fstat(fd, &st) != nil || unix.Fstatfs(fd, &fs) != nil || fs.Type != unix.NSFS_MAGIC || kindErr != nil || kind != unix.CLONE_NEWNET || uint64(st.Dev) != dev || st.Ino != ino || namespaceMatches("/proc/self/ns/net", dev, ino) {
		f.Close()
		return nil, Held("containment_namespace_drift")
	}
	return f, nil
}

func observeContainmentNetwork(p NativeContainmentProfile) error {
	namespace, err := openPinnedNetworkNamespace(p.Namespace, p.NamespaceDev, p.NamespaceIno)
	if err != nil {
		return err
	}
	defer namespace.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	// sudo normally closes inherited descriptors. Refer to the still-open parent
	// descriptor so a pathname replacement cannot change the observed namespace.
	pinnedPath := fmt.Sprintf("/proc/%d/fd/%d", os.Getpid(), namespace.Fd())
	cmd := exec.CommandContext(ctx, p.Sudo.Path, "-n", p.Nsenter.Path, "--net="+pinnedPath, "--", p.Nft.Path, "--json", "--stateless", "list", "ruleset")
	cmd.Env = []string{"PATH=/usr/sbin:/usr/bin:/bin", "LANG=C"}
	var out boundedKernelOutput
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil || out.overflow {
		return Held("containment_rules_unobservable")
	}
	sha, err := NativeRulesDigest(out.buf.Bytes())
	if err != nil || sha != p.RulesSHA256 {
		return Held("containment_rules_drift")
	}
	return nil
}

type boundedKernelOutput struct {
	buf      bytes.Buffer
	overflow bool
}

func (b *boundedKernelOutput) Write(p []byte) (int, error) {
	n := len(p)
	left := (256 << 10) - b.buf.Len()
	if len(p) > left {
		p = p[:left]
		b.overflow = true
	}
	b.buf.Write(p)
	return n, nil
}

// NativeRulesDigest normalizes only nft's runtime metadata. Everything else
// in the live ruleset (including additional tables/chains/rules) is pinned.
func NativeRulesDigest(data []byte) (string, error) {
	var rules struct {
		NFTables []map[string]json.RawMessage `json:"nftables"`
	}
	if DecodeStrict(data, &rules) != nil {
		return "", Held("containment_rules_invalid")
	}
	kept := make([]map[string]json.RawMessage, 0, len(rules.NFTables))
	for _, item := range rules.NFTables {
		if _, ok := item["metainfo"]; ok {
			if len(item) != 1 {
				return "", Held("containment_rules_invalid")
			}
			continue
		}
		kept = append(kept, item)
	}
	if len(kept) < 3 {
		return "", Held("containment_rules_invalid")
	}
	b, err := json.Marshal(kept)
	if err != nil {
		return "", err
	}
	return digest(b), nil
}

type nativeNamespaceClaim struct {
	Version         int    `json:"version"`
	NativeSessionID string `json:"native_session_id"`
	Unit            string `json:"unit"`
	Cgroup          string `json:"cgroup"`
	NamespaceDev    uint64 `json:"namespace_dev"`
	NamespaceIno    uint64 `json:"namespace_ino"`
}

func cgroupEmpty(path string) bool {
	if !strings.HasPrefix(path, "/") || filepath.Clean(path) != path || path == "/" {
		return false
	}
	data, err := os.ReadFile(filepath.Join("/sys/fs/cgroup", path, "cgroup.events"))
	if os.IsNotExist(err) {
		return true
	}
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(data), "\n") {
		if line == "populated 0" {
			return true
		}
	}
	return false
}

func currentNativeCgroup(unit string) (string, error) {
	data, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return "", Held("containment_cgroup_unobservable")
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "0::/") {
			path := strings.TrimPrefix(line, "0::")
			if filepath.Base(path) == unit {
				return path, nil
			}
		}
	}
	return "", Held("containment_cgroup_mismatch")
}

func claimNativeNamespace(p NativeContainmentProfile, e nativeLaunchEnvelope) (*os.File, error) {
	if verifyOwnedPath(p.ClaimDir, p.UID, true) != nil {
		return nil, Held("containment_claim_store_unsafe")
	}
	key := fmt.Sprintf("%d-%d", p.NamespaceDev, p.NamespaceIno)
	lockPath := filepath.Join(p.ClaimDir, key+".lock")
	fd, err := unix.Open(lockPath, unix.O_RDWR|unix.O_CREAT|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, Held("containment_claim_unavailable")
	}
	lock := os.NewFile(uintptr(fd), lockPath)
	failed := true
	defer func() {
		if failed {
			lock.Close()
		}
	}()
	if verifyOwnedPath(lockPath, p.UID, false) != nil || unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB) != nil {
		return nil, Held("containment_namespace_busy")
	}
	claimPath := filepath.Join(p.ClaimDir, key+".json")
	b, err := readNativeEvidence(claimPath, p.UID, 16<<10)
	if err == nil {
		var old nativeNamespaceClaim
		if verifyOwnedPath(claimPath, p.UID, false) != nil || len(b) > 16<<10 || DecodeStrict(b, &old) != nil || old.Version != 1 || old.NamespaceDev != p.NamespaceDev || old.NamespaceIno != p.NamespaceIno || !cgroupEmpty(old.Cgroup) {
			return nil, Held("containment_previous_cgroup_unresolved")
		}
	} else if !os.IsNotExist(err) {
		return nil, Held("containment_claim_unavailable")
	}
	cgroup, err := currentNativeCgroup(e.Unit)
	if err != nil {
		return nil, err
	}
	claim := nativeNamespaceClaim{1, e.NativeSessionID, e.Unit, cgroup, p.NamespaceDev, p.NamespaceIno}
	b, err = json.Marshal(claim)
	if err != nil || replaceRevisionFile(claimPath, b) != nil {
		return nil, Held("containment_claim_persistence_failed")
	}
	failed = false
	return lock, nil
}

// RunNativeMonitor is unprivileged and belongs to the same transient service
// that owns the CLI. Its private flock is never visible inside bubblewrap.
func RunNativeMonitor() error {
	e, err := readNativeEnvelope(os.Stdin)
	if err != nil {
		return err
	}
	p, err := readContainmentProfile(e.Profile)
	if err != nil {
		return err
	}
	if p.ProjectID != e.ProjectID || uint32(os.Getuid()) != p.UID || !namespaceMatches("/proc/self/ns/net", p.NamespaceDev, p.NamespaceIno) || !containedPath(p.WorktreeRoot, e.Worktree) || !containedPath(p.ScratchRoot, e.Scratch) {
		return Held("containment_monitor_binding_invalid")
	}
	if VerifyFile(FileProof{Path: "/proc/self/exe", SHA256: p.Maestro.SHA256}) != nil {
		return Held("containment_launcher_drift")
	}
	lock, err := claimNativeNamespace(p, e)
	if err != nil {
		return err
	}
	defer lock.Close()
	args, err := nativeBubblewrapArguments(p, e)
	if err != nil {
		return err
	}
	frame, err := encodeNativeEnvelope(e)
	if err != nil {
		return err
	}
	filter, err := nativeSeccompFile()
	if err != nil {
		return err
	}
	defer filter.Close()
	cmd := exec.Command(p.Bubblewrap.Path, args...)
	cmd.ExtraFiles = []*os.File{filter}
	cmd.Env = []string{"PATH=/usr/bin:/bin", "LANG=C.UTF-8"}
	cmd.Stdin = ioMultiFrame(frame)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cgroup, err := currentNativeCgroup(e.Unit)
	if err != nil {
		return err
	}
	proof := NativeProcessTermination{Version: 1, Profile: e.Profile, NativeSessionID: e.NativeSessionID, Unit: e.Unit, Cgroup: cgroup, StartedAt: time.Now().UTC(), LocalStatus: "launch_intent", ExitCode: -1}
	boot, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return Held("containment_boot_unobservable")
	}
	proof.BootID = strings.TrimSpace(string(boot))
	proof.InvocationID = os.Getenv("INVOCATION_ID")
	if len(proof.InvocationID) != 32 {
		return Held("containment_process_incarnation_unobservable")
	}
	proof.Digest = nativeTerminationDigest(proof)
	b, err := json.Marshal(proof)
	if err != nil || replaceRevisionFile(filepath.Join(p.ClaimDir, e.NativeSessionID+".launch.json"), b) != nil {
		return Held("containment_launch_persistence_failed")
	}
	runErr := cmd.Run()
	proof.EndedAt = time.Now().UTC()
	proof.LocalStatus = "failed"
	if cmd.ProcessState != nil {
		proof.ExitCode = cmd.ProcessState.ExitCode()
	}
	if runErr == nil {
		proof.LocalStatus = "succeeded"
	}
	proof.Digest = nativeTerminationDigest(proof)
	b, err = json.Marshal(proof)
	if err != nil || replaceRevisionFile(filepath.Join(p.ClaimDir, e.NativeSessionID+".termination.json"), b) != nil {
		return Held("containment_termination_persistence_failed")
	}
	return runErr
}

func nativeBubblewrapArguments(p NativeContainmentProfile, e nativeLaunchEnvelope) ([]string, error) {
	if VerifyFile(p.Bubblewrap) != nil || verifyOwnedPath(e.Worktree, p.UID, true) != nil || verifyOwnedPath(e.Scratch, p.UID, true) != nil {
		return nil, Held("containment_launch_files_drift")
	}
	args := []string{"--unshare-user", "--unshare-pid", "--unshare-ipc", "--unshare-uts", "--unshare-cgroup", "--seccomp", "3", "--die-with-parent", "--new-session", "--cap-drop", "ALL", "--clearenv", "--tmpfs", "/home", "--dir", "/home/native"}
	for _, m := range p.ReadOnly {
		st, err := os.Stat(m.Source)
		if err != nil || verifyOwnedPath(m.Source, 0, st.IsDir()) != nil {
			return nil, Held("containment_mount_unsafe")
		}
		args = append(args, "--ro-bind", m.Source, m.Target)
	}
	args = append(args, "--symlink", "usr/bin", "/bin", "--symlink", "usr/sbin", "/sbin", "--proc", "/proc", "--dev", "/dev",
		"--bind", e.Worktree, "/work", "--bind", e.Scratch, "/scratch", "--bind", filepath.Join(e.Scratch, "tmp"), "/tmp",
		"--ro-bind", p.Maestro.Path, "/runtime/maestro", "--ro-bind", p.Harness.Path, "/runtime/claude", "--ro-bind", e.Profile.Path, "/runtime/profile.json",
	)
	if e.Role != "supervisor" && e.Role != "reviewer" {
		gitDir := filepath.Join(e.Worktree, ".git")
		if st, err := os.Lstat(gitDir); err != nil || !st.IsDir() {
			return nil, Held("containment_git_metadata_external")
		}
		args = append(args, "--bind", gitDir, "/work/.git", "--ro-bind", filepath.Join(gitDir, "config"), "/work/.git/config",
			"--ro-bind", filepath.Join(gitDir, NativeCloneMarker), nativeSandboxCloneIdentityPath,
			"--tmpfs", "/work/.git/hooks", "--remount-ro", "/work/.git/hooks")
		if err := verifyNativeGitGuards(e.Worktree, p.UID); err != nil {
			return nil, err
		}
		for _, rel := range []string{"objects", "objects/info"} {
			args = append(args, "--bind", filepath.Join(gitDir, rel), "/work/.git/"+rel)
		}
		for _, rel := range []string{"commondir", "config.worktree", "objects/info/alternates", "objects/info/http-alternates"} {
			args = append(args, "--ro-bind", filepath.Join(gitDir, rel), "/work/.git/"+rel)
		}
	}
	args = append(args, "--chdir", "/work", "/runtime/maestro", "_native-entry")
	return args, nil
}

func RunNativeEntry() error {
	e, err := readNativeEnvelope(os.Stdin)
	if err != nil {
		return err
	}
	b, err := os.ReadFile("/runtime/profile.json")
	var p NativeContainmentProfile
	if err != nil || len(b) > 128<<10 || digest(b) != e.Profile.SHA256 || DecodeStrict(b, &p) != nil || validateContainmentProfile(p) != nil {
		return Held("containment_profile_drift")
	}
	if p.ProjectID != e.ProjectID || uint32(os.Getuid()) != p.UID || !namespaceMatches("/proc/self/ns/net", p.NamespaceDev, p.NamespaceIno) {
		return Held("containment_entry_binding_invalid")
	}
	if VerifyFile(FileProof{Path: "/proc/self/exe", SHA256: p.Maestro.SHA256}) != nil || VerifyFile(FileProof{Path: "/runtime/claude", SHA256: p.Harness.SHA256}) != nil {
		return Held("containment_entry_binary_drift")
	}
	status, err := os.ReadFile("/proc/self/status")
	if err != nil || !strings.Contains(string(status), "NoNewPrivs:\t1\n") || !strings.Contains(string(status), "CapEff:\t0000000000000000\n") || !strings.Contains(string(status), "Seccomp:\t2\n") {
		return Held("containment_privilege_drift")
	}
	args, err := containedNativeArguments(e.Arguments, e.NativeSessionID, e.Role)
	if err != nil {
		return err
	}
	env, err := containedNativeEnvironment(p, e.Environment, e.Role)
	if err != nil {
		return err
	}
	return syscall.Exec("/runtime/claude", args, env)
}

func verifyNativeGitGuards(worktree string, uid uint32) error {
	for rel, value := range map[string]string{"commondir": ".\n", "config.worktree": "", "objects/info/alternates": "", "objects/info/http-alternates": ""} {
		b, err := readNativeEvidence(filepath.Join(worktree, ".git", rel), uid, 16)
		if err != nil || string(b) != value {
			return Held("containment_git_metadata_external")
		}
	}
	return nil
}

// Keep the exact unread stdin suffix; a JSON decoder must never buffer prompt
// bytes before the native CLI receives the inherited pipe.
func ioMultiFrame(frame []byte) *nativeFrameReader {
	return &nativeFrameReader{prefix: bytes.NewReader(frame)}
}

type nativeFrameReader struct{ prefix *bytes.Reader }

func (r *nativeFrameReader) Read(p []byte) (int, error) {
	if r.prefix.Len() > 0 {
		return r.prefix.Read(p)
	}
	return os.Stdin.Read(p)
}
