//go:build linux

package tmpfshygiene

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestSweepRefusesNonTmpfsWithoutMutation(t *testing.T) {
	root, proc, now := fakeRoots(t)
	candidate := oldDir(t, root, "tmp.abandoned", now, 2*time.Hour, "payload")

	summary, err := Sweep(context.Background(), fakeOptions(root, proc, now, ModeApply, 44, false))
	if err == nil || !strings.Contains(err.Error(), "is not tmpfs") {
		t.Fatalf("Sweep error = %v, want non-tmpfs refusal", err)
	}
	if summary.Error == "" || summary.Tmpfs {
		t.Fatalf("summary = %+v, want recorded non-tmpfs error", summary)
	}
	assertExists(t, candidate)
}

func TestSweepDryRunReportsAllowlistedResidueWithoutDeleting(t *testing.T) {
	root, proc, now := fakeRoots(t)
	allowed := oldDir(t, root, "tmp.abandoned", now, 2*time.Hour, "payload")
	unlisted := oldDir(t, root, "customer-data", now, 48*time.Hour, "keep")
	young := oldDir(t, root, "tmp.young", now, 10*time.Minute, "new")

	summary, err := Sweep(context.Background(), fakeOptions(root, proc, now, ModeDryRun, 80, true))
	if err != nil {
		t.Fatal(err)
	}
	if summary.MatchedEntries != 2 || summary.ReclaimableBytes != int64(len("payload")) || summary.FreedBytes != 0 {
		t.Fatalf("summary = %+v", summary)
	}
	if summary.ProtectHits["too_young"] != 1 {
		t.Fatalf("protect hits = %#v, want too_young=1", summary.ProtectHits)
	}
	assertExists(t, allowed)
	assertExists(t, unlisted)
	assertExists(t, young)
}

func TestSweepApplyDeletesOnlyOldAllowlistedEntries(t *testing.T) {
	root, proc, now := fakeRoots(t)
	removed := oldDir(t, root, "playwright-old", now, 3*time.Hour, "browser")
	kept := oldDir(t, root, "unlisted-old", now, 72*time.Hour, "important")

	summary, err := Sweep(context.Background(), fakeOptions(root, proc, now, ModeApply, 86, true))
	if err != nil {
		t.Fatal(err)
	}
	if summary.DeletedEntries != 1 || summary.FreedBytes != int64(len("browser")) {
		t.Fatalf("summary = %+v", summary)
	}
	if !summary.Pressure || summary.AttentionCode != "tmpfs_pressure" {
		t.Fatalf("pressure summary = %+v", summary)
	}
	assertMissing(t, removed)
	assertExists(t, kept)
}

func TestSweepProtectsProcessCWDOpenFDAndCmdline(t *testing.T) {
	root, proc, now := fakeRoots(t)
	cwd := oldDir(t, root, "tmp.cwd", now, 3*time.Hour, "cwd")
	fd := oldDir(t, root, "tmp.fd", now, 3*time.Hour, "fd")
	cmd := oldDir(t, root, "tmp.cmd", now, 3*time.Hour, "cmd")
	dead := oldDir(t, root, "tmp.dead", now, 3*time.Hour, "dead")

	processDir := filepath.Join(proc, "123")
	if err := os.MkdirAll(filepath.Join(processDir, "fd"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(cwd, filepath.Join(processDir, "cwd")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(fd, "payload"), filepath.Join(processDir, "fd", "9")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(processDir, "cmdline"), []byte("worker\x00--snapshot="+cmd+"/state.json\x00"), 0o644); err != nil {
		t.Fatal(err)
	}

	summary, err := Sweep(context.Background(), fakeOptions(root, proc, now, ModeApply, 40, true))
	if err != nil {
		t.Fatal(err)
	}
	for _, reason := range []string{"process_cwd", "process_fd", "process_cmdline"} {
		if summary.ProtectHits[reason] != 1 {
			t.Fatalf("protect hits = %#v, want %s=1", summary.ProtectHits, reason)
		}
	}
	assertExists(t, cwd)
	assertExists(t, fd)
	assertExists(t, cmd)
	assertMissing(t, dead)
}

func TestSweepProtectsConfiguredPathsAndGitWorktrees(t *testing.T) {
	root, proc, now := fakeRoots(t)
	configured := oldDir(t, root, "tmp.configured", now, 3*time.Hour, "configured")
	gitTree := oldDir(t, root, "tmp.git", now, 3*time.Hour, "git")
	if err := os.WriteFile(filepath.Join(gitTree, ".git"), []byte("gitdir: elsewhere"), 0o644); err != nil {
		t.Fatal(err)
	}
	setTreeAge(t, gitTree, now.Add(-3*time.Hour))

	opts := fakeOptions(root, proc, now, ModeApply, 40, true)
	opts.ProtectedPaths = []string{filepath.Join(configured, "worktrees")}
	summary, err := Sweep(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if summary.ProtectHits["configured_path"] != 1 || summary.ProtectHits["git_worktree"] != 1 {
		t.Fatalf("protect hits = %#v", summary.ProtectHits)
	}
	assertExists(t, configured)
	assertExists(t, gitTree)
}

func TestSweepProtectsConfiguredPathReachedThroughSymlink(t *testing.T) {
	root, proc, now := fakeRoots(t)
	candidate := oldDir(t, root, "tmp.aliased-worktrees", now, 3*time.Hour, "worktree")
	alias := filepath.Join(t.TempDir(), "worktree-alias")
	if err := os.Symlink(candidate, alias); err != nil {
		t.Fatal(err)
	}
	opts := fakeOptions(root, proc, now, ModeApply, 40, true)
	opts.ProtectedPaths = []string{alias}
	summary, err := Sweep(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if summary.ProtectHits["configured_path"] != 1 {
		t.Fatalf("protect hits = %#v", summary.ProtectHits)
	}
	assertExists(t, candidate)
}

func TestSweepNeverFollowsSymlinksOutsideRoot(t *testing.T) {
	root, proc, now := fakeRoots(t)
	external := t.TempDir()
	externalFile := filepath.Join(external, "outside")
	if err := os.WriteFile(externalFile, []byte("must survive"), 0o644); err != nil {
		t.Fatal(err)
	}
	topLink := filepath.Join(root, "tmp.escape")
	if err := os.Symlink(external, topLink); err != nil {
		t.Fatal(err)
	}
	candidate := oldDir(t, root, "tmp.nested-link", now, 3*time.Hour, "inside")
	if err := os.Symlink(externalFile, filepath.Join(candidate, "outside-link")); err != nil {
		t.Fatal(err)
	}
	setTreeAge(t, candidate, now.Add(-3*time.Hour))

	summary, err := Sweep(context.Background(), fakeOptions(root, proc, now, ModeApply, 40, true))
	if err != nil {
		t.Fatal(err)
	}
	if summary.ProtectHits["symlink"] != 1 {
		t.Fatalf("protect hits = %#v, want top-level symlink protection", summary.ProtectHits)
	}
	assertExists(t, topLink)
	assertMissing(t, candidate)
	if got, err := os.ReadFile(externalFile); err != nil || string(got) != "must survive" {
		t.Fatalf("outside file changed: got=%q err=%v", got, err)
	}
}

func TestSweepAlwaysKeepsSocketAndLockNames(t *testing.T) {
	root, proc, now := fakeRoots(t)
	socketName := oldDir(t, root, "tmp.worker.sock", now, 3*time.Hour, "socket")
	lockName := oldDir(t, root, "tmp.worker.lock", now, 3*time.Hour, "lock")
	removed := oldDir(t, root, "tmp.worker.done", now, 3*time.Hour, "done")

	summary, err := Sweep(context.Background(), fakeOptions(root, proc, now, ModeApply, 40, true))
	if err != nil {
		t.Fatal(err)
	}
	if summary.ProtectHits["always_keep"] != 2 {
		t.Fatalf("protect hits = %#v, want always_keep=2", summary.ProtectHits)
	}
	assertExists(t, socketName)
	assertExists(t, lockName)
	assertMissing(t, removed)
}

func TestDefaultPolicyIsTopLevelAllowlistNotCatchAll(t *testing.T) {
	for _, policy := range defaultPolicies {
		if policy.Pattern == "*" || strings.Contains(policy.Pattern, string(filepath.Separator)) {
			t.Fatalf("unsafe default policy: %+v", policy)
		}
		if policy.Category == "" || policy.MinAge <= 0 {
			t.Fatalf("incomplete default policy: %+v", policy)
		}
	}
}

func TestCandidateNamesFromValueFindsEveryCmdlinePath(t *testing.T) {
	got := candidateNamesFromValue("--from=/tmp/tmp.one,/tmp/tmp.two/file --other=/tmp/tmp.three", "/tmp")
	want := []string{"tmp.one", "tmp.two", "tmp.three"}
	if len(got) != len(want) {
		t.Fatalf("names = %#v, want %#v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("names = %#v, want %#v", got, want)
		}
	}
}

func TestCollectProcessProtectsSkipsSweeperProcess(t *testing.T) {
	root, proc, now := fakeRoots(t)
	candidate := oldDir(t, root, "tmp.self", now, 2*time.Hour, "payload")
	processDir := filepath.Join(proc, "789")
	if err := os.MkdirAll(filepath.Join(processDir, "fd"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(candidate, filepath.Join(processDir, "fd", "5")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(processDir, "cmdline"), []byte("maestro\x00"), 0o644); err != nil {
		t.Fatal(err)
	}

	protected, scan, err := collectProcessProtects(proc, root, map[string]struct{}{"tmp.self": {}}, 789)
	if err != nil {
		t.Fatal(err)
	}
	if scan.errors != 0 || len(protected) != 0 {
		t.Fatalf("protected = %#v, scan = %+v", protected, scan)
	}
}

func TestCollectProcessProtectsMapsIsolatedCandidatePath(t *testing.T) {
	root, proc, _ := fakeRoots(t)
	quarantineName := ".maestro-tmpfs-hygiene-deadbeef"
	isolated := filepath.Join(root, quarantineName, "tmp.live")
	processDir := filepath.Join(proc, "790")
	if err := os.MkdirAll(filepath.Join(processDir, "fd"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(isolated, filepath.Join(processDir, "cwd")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(processDir, "cmdline"), []byte("worker\x00"), 0o644); err != nil {
		t.Fatal(err)
	}

	protected, scan, err := collectProcessProtects(
		proc,
		root,
		map[string]struct{}{quarantineName: {}},
		-1,
	)
	if err != nil {
		t.Fatal(err)
	}
	if scan.errors != 0 {
		t.Fatalf("scan = %+v", scan)
	}
	if _, ok := protected[quarantineName]["process_cwd"]; !ok {
		t.Fatalf("protected = %#v, want isolated cwd hit", protected)
	}
}

func TestSweepDeletesLeakedNativeLibsAndKeepsMappedOnes(t *testing.T) {
	root, proc, now := fakeRoots(t)
	leakedSo := filepath.Join(root, ".bcddfd9eb5fdb63f-00000000.so")
	leakedHm := filepath.Join(root, ".18c737bd3e5dfeff-00000000.hm")
	mappedSo := filepath.Join(root, ".bcddfddebceffe6f-00000000.so")
	for _, path := range []string{leakedSo, leakedHm, mappedSo} {
		if err := os.WriteFile(path, []byte("elf"), 0o644); err != nil {
			t.Fatal(err)
		}
		old := now.Add(-3 * time.Hour)
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatal(err)
		}
	}
	// A live process holds the third copy open through an fd link.
	processDir := filepath.Join(proc, "321")
	if err := os.MkdirAll(filepath.Join(processDir, "fd"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(mappedSo, filepath.Join(processDir, "fd", "3")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(processDir, "cmdline"), []byte("opencode\x00"), 0o644); err != nil {
		t.Fatal(err)
	}

	summary, err := Sweep(context.Background(), fakeOptions(root, proc, now, ModeApply, 40, true))
	if err != nil {
		t.Fatal(err)
	}
	if summary.DeletedEntries != 2 {
		t.Fatalf("summary = %+v, want the two unreferenced leak files deleted", summary)
	}
	stats := summary.Categories["native_lib_leak"]
	if stats.Candidates != 3 || stats.Deleted != 2 || stats.Protected != 1 {
		t.Fatalf("native_lib_leak stats = %+v", stats)
	}
	assertMissing(t, leakedSo)
	assertMissing(t, leakedHm)
	assertExists(t, mappedSo)
}

func TestSweepIgnoresForeignProcessPermissionDenials(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permission denials cannot be simulated as root")
	}
	root, proc, now := fakeRoots(t)
	stale := oldDir(t, root, "tmp.stale", now, 3*time.Hour, "stale")
	// The fixture cannot chown itself to another UID without root, and
	// ownership is what separates a foreign process from a same-UID one that
	// cleared PR_SET_DUMPABLE, so state the foreign ownership explicitly.
	restoreOwner := procOwnerIsSelf
	procOwnerIsSelf = func(string) bool { return false }
	t.Cleanup(func() { procOwnerIsSelf = restoreOwner })
	// A foreign process whose /proc internals are unreadable (EACCES), like
	// every other user's process on a shared host. It must not freeze the
	// sweep: before the 2026-07-23 fix this blanket-protected every candidate.
	processDir := filepath.Join(proc, "654")
	if err := os.MkdirAll(processDir, 0o755); err != nil {
		t.Fatal(err)
	}
	fdDir := filepath.Join(processDir, "fd")
	if err := os.MkdirAll(fdDir, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(fdDir, 0o755) })
	if err := os.WriteFile(filepath.Join(processDir, "cmdline"), []byte("foreign\x00"), 0o644); err != nil {
		t.Fatal(err)
	}

	summary, err := Sweep(context.Background(), fakeOptions(root, proc, now, ModeApply, 40, true))
	if err != nil {
		t.Fatal(err)
	}
	if summary.ProcScanErrors != 0 || summary.ProcPermissionSkips == 0 {
		t.Fatalf("summary = %+v, want permission skips counted without scan errors", summary)
	}
	if summary.ProtectHits["proc_scan_error"] != 0 || summary.DeletedEntries != 1 {
		t.Fatalf("summary = %+v, want stale candidate deleted despite foreign EACCES", summary)
	}
	assertMissing(t, stale)
}

func TestSweepRevalidatesAgeImmediatelyBeforeApply(t *testing.T) {
	root, proc, now := fakeRoots(t)
	candidate := oldDir(t, root, "tmp.changed", now, 2*time.Hour, "old")
	opts := fakeOptions(root, proc, now, ModeApply, 40, true)
	opts.beforeApply = func(string) {
		fresh := now.Add(-time.Minute)
		if err := os.Chtimes(filepath.Join(candidate, "payload"), fresh, fresh); err != nil {
			t.Fatal(err)
		}
	}
	summary, err := Sweep(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if summary.ProtectHits["too_young"] != 1 {
		t.Fatalf("summary = %+v", summary)
	}
	assertExists(t, candidate)
}

func TestSweepRevalidatesTheObjectMovedIntoQuarantine(t *testing.T) {
	root, proc, now := fakeRoots(t)
	candidate := oldDir(t, root, "tmp.changed", now, 2*time.Hour, "original")
	original := filepath.Join(root, "saved-original")
	replaced := false
	opts := fakeOptions(root, proc, now, ModeApply, 40, true)
	opts.beforeApply = func(string) {
		if replaced {
			return
		}
		replaced = true
		if err := os.Rename(candidate, original); err != nil {
			t.Fatal(err)
		}
		replacement := oldDir(t, root, "tmp.changed", now, 2*time.Hour, "replacement")
		if err := os.WriteFile(filepath.Join(replacement, ".git"), []byte("gitdir: elsewhere"), 0o644); err != nil {
			t.Fatal(err)
		}
		setTreeAge(t, replacement, now.Add(-2*time.Hour))
	}

	summary, err := Sweep(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if summary.ProtectHits["git_worktree"] != 1 || summary.DeletedEntries != 0 {
		t.Fatalf("summary = %+v", summary)
	}
	assertExists(t, original)
	got, err := os.ReadFile(filepath.Join(candidate, "payload"))
	if err != nil || string(got) != "replacement" {
		t.Fatalf("replacement payload = %q, err=%v", got, err)
	}
	assertNoSweepQuarantine(t, root)
}

func TestSweepRefreshesProcessProtectionAfterIsolation(t *testing.T) {
	root, proc, now := fakeRoots(t)
	candidate := oldDir(t, root, "tmp.became-live", now, 2*time.Hour, "payload")
	opts := fakeOptions(root, proc, now, ModeApply, 40, true)
	opts.beforeApply = func(path string) {
		processDir := filepath.Join(proc, "456")
		if err := os.MkdirAll(filepath.Join(processDir, "fd"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(path, filepath.Join(processDir, "cwd")); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(processDir, "cmdline"), []byte("worker\x00"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	summary, err := Sweep(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if summary.ProtectHits["process_cwd"] != 1 || summary.DeletedEntries != 0 {
		t.Fatalf("summary = %+v", summary)
	}
	assertExists(t, candidate)
	assertNoSweepQuarantine(t, root)
}

func TestSweepRootCanDeleteOtherOwnerCandidates(t *testing.T) {
	root, proc, now := fakeRoots(t)
	candidate := oldDir(t, root, "tmp.other-owner", now, 2*time.Hour, "payload")
	if os.Geteuid() == 0 {
		chownTree(t, candidate, 12345, 12345)
	}
	opts := fakeOptions(root, proc, now, ModeApply, 40, true)
	opts.EffectiveUID = func() int { return 0 }

	summary, err := Sweep(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if summary.ProtectHits["owner_mismatch"] != 0 || summary.DeletedEntries != 1 {
		t.Fatalf("summary = %+v", summary)
	}
	assertMissing(t, candidate)
}

func TestSweepNonRootProtectsOtherOwnerCandidates(t *testing.T) {
	root, proc, now := fakeRoots(t)
	candidate := oldDir(t, root, "tmp.other-owner", now, 2*time.Hour, "payload")
	fakeUID := os.Geteuid() + 1
	if fakeUID == 0 {
		fakeUID = 1
	}
	opts := fakeOptions(root, proc, now, ModeApply, 40, true)
	opts.EffectiveUID = func() int { return fakeUID }

	summary, err := Sweep(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if summary.ProtectHits["owner_mismatch"] != 1 || summary.DeletedEntries != 0 {
		t.Fatalf("summary = %+v", summary)
	}
	assertExists(t, candidate)
}

func TestSweepReportsPartialDeletionBeforeReturningError(t *testing.T) {
	root, proc, now := fakeRoots(t)
	candidate := oldDir(t, root, "tmp.partial", now, 2*time.Hour, "payload")
	opts := fakeOptions(root, proc, now, ModeApply, 40, true)
	opts.removeEntry = func(_ context.Context, parentFD int, name string, _ uint64) (removalResult, error) {
		fd, err := unix.Openat(parentFD, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err != nil {
			return removalResult{}, err
		}
		defer unix.Close(fd)
		var st unix.Stat_t
		if err := unix.Fstatat(fd, "payload", &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			return removalResult{}, err
		}
		if err := unix.Unlinkat(fd, "payload", 0); err != nil {
			return removalResult{}, err
		}
		return removalResult{freedBytes: st.Size, removedObjects: 1}, errors.New("injected recursive removal failure")
	}

	summary, err := Sweep(context.Background(), opts)
	if err == nil || !strings.Contains(err.Error(), "injected recursive removal failure") {
		t.Fatalf("error = %v, want injected removal failure", err)
	}
	if summary.FreedBytes != int64(len("payload")) || summary.PartialEntries != 1 || summary.DeletedEntries != 0 {
		t.Fatalf("summary = %+v", summary)
	}
	stats := summary.Categories["outcome_snapshot"]
	if stats.FreedBytes != int64(len("payload")) || stats.Partial != 1 || stats.Deleted != 0 {
		t.Fatalf("category stats = %+v", stats)
	}
	assertExists(t, candidate)
	assertMissing(t, filepath.Join(candidate, "payload"))
	assertNoSweepQuarantine(t, root)
}

func TestSweepRefusesSymlinkRoot(t *testing.T) {
	base := t.TempDir()
	realRoot := filepath.Join(base, "real-tmp")
	root := filepath.Join(base, "tmp")
	proc := filepath.Join(base, "proc")
	if err := os.MkdirAll(realRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(proc, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realRoot, root); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 21, 18, 0, 0, 0, time.UTC)
	_, err := Sweep(context.Background(), fakeOptions(root, proc, now, ModeApply, 40, true))
	if err == nil || !strings.Contains(err.Error(), "not a direct directory") {
		t.Fatalf("error = %v, want symlink-root refusal", err)
	}
}

func fakeRoots(t *testing.T) (root, proc string, now time.Time) {
	t.Helper()
	base := t.TempDir()
	root = filepath.Join(base, "tmp")
	proc = filepath.Join(base, "proc")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(proc, 0o755); err != nil {
		t.Fatal(err)
	}
	return root, proc, time.Date(2026, 7, 21, 18, 0, 0, 0, time.UTC)
}

func fakeOptions(root, proc string, now time.Time, mode string, usePct int, tmpfs bool) Options {
	return Options{
		Root:     root,
		ProcRoot: proc,
		Mode:     mode,
		Now:      func() time.Time { return now },
		InspectMount: func(string) (MountUsage, error) {
			// Model the real mount: a 16GiB RAM-backed /tmp. The pressure verdict
			// is measured in absolute free bytes (#1128), so the fixture has to
			// express a plausible capacity rather than a 100-byte stub.
			const total = int64(16) << 30
			used := total * int64(usePct) / 100
			return MountUsage{Tmpfs: tmpfs, UsePct: usePct, TotalBytes: total, UsedBytes: used, AvailableBytes: total - used}, nil
		},
		EffectiveUID: os.Geteuid,
		processID:    func() int { return -1 },
	}
}

func oldDir(t *testing.T, root, name string, now time.Time, age time.Duration, payload string) string {
	t.Helper()
	path := filepath.Join(root, name)
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "payload"), []byte(payload), 0o644); err != nil {
		t.Fatal(err)
	}
	setTreeAge(t, path, now.Add(-age))
	return path
}

func setTreeAge(t *testing.T, root string, at time.Time) {
	t.Helper()
	if err := filepath.Walk(root, func(path string, _ os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		return os.Chtimes(path, at, at)
	}); err != nil {
		t.Fatal(err)
	}
}

func chownTree(t *testing.T, root string, uid, gid int) {
	t.Helper()
	if err := filepath.Walk(root, func(path string, _ os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		return os.Chown(path, uid, gid)
	}); err != nil {
		t.Fatal(err)
	}
}

func assertNoSweepQuarantine(t *testing.T, root string) {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".maestro-tmpfs-hygiene-") {
			t.Fatalf("unexpected sweep quarantine %s", filepath.Join(root, entry.Name()))
		}
	}
}

func assertExists(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); err != nil {
		t.Fatalf("expected %s to exist: %v", path, err)
	}
}

func assertMissing(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("expected %s to be absent, err=%v", path, err)
	}
}

// Codex review catch (P1): the `.*-00000000.so` glob enforces neither the
// documented hex stem nor a regular-file type, so an unrelated plugin file —
// or a directory that merely ends the same way — was recursively deleted by
// every apply sweep.
func TestSweepLeavesNonGeneratedNativeLibLookalikes(t *testing.T) {
	root, proc, now := fakeRoots(t)
	old := now.Add(-3 * time.Hour)

	// Not a hex stem: a real plugin that happens to share the suffix.
	foreignFile := filepath.Join(root, ".my-plugin-00000000.so")
	if err := os.WriteFile(foreignFile, []byte("elf"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A directory the glob also matched.
	foreignDir := filepath.Join(root, ".bcddfd9eb5fdb63f-00000000.so.d")
	if err := os.MkdirAll(foreignDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(foreignDir, "keep"), []byte("payload"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A genuine leak, to prove the policy still fires.
	leaked := filepath.Join(root, ".18c737bd3e5dfeff-00000000.so")
	if err := os.WriteFile(leaked, []byte("elf"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{foreignFile, foreignDir, leaked} {
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := Sweep(context.Background(), fakeOptions(root, proc, now, ModeApply, 40, true)); err != nil {
		t.Fatal(err)
	}
	assertExists(t, foreignFile)
	assertExists(t, filepath.Join(foreignDir, "keep"))
	assertMissing(t, leaked)
}

// EACCES does not prove the process belongs to another user — Linux denies the
// same read for a same-UID process that cleared PR_SET_DUMPABLE. Such a process
// still protects what it demonstrably references, but only that: charging its
// denial to every candidate is what made the sweeper a permanent no-op on the
// fleet host (#1125).
func TestSweepScopesSameUIDDenialToTheProcessThatDenied(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permission denials cannot be simulated as root")
	}
	root, proc, now := fakeRoots(t)
	held := oldDir(t, root, "tmp.held", now, 3*time.Hour, "held")
	stale := oldDir(t, root, "tmp.stale", now, 3*time.Hour, "stale")

	processDir := filepath.Join(proc, "655")
	if err := os.MkdirAll(processDir, 0o755); err != nil {
		t.Fatal(err)
	}
	fdDir := filepath.Join(processDir, "fd")
	if err := os.MkdirAll(fdDir, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(fdDir, 0o755) })
	// Its fd table is unreadable, but its world-readable command line still ties
	// it to one candidate.
	cmdline := []byte("dumpable-off\x00--state=" + filepath.Join(held, "state.json") + "\x00")
	if err := os.WriteFile(filepath.Join(processDir, "cmdline"), cmdline, 0o644); err != nil {
		t.Fatal(err)
	}

	summary, err := Sweep(context.Background(), fakeOptions(root, proc, now, ModeApply, 40, true))
	if err != nil {
		t.Fatal(err)
	}
	if summary.ProcScanErrors == 0 || summary.ProcUnresolvedProcesses == 0 {
		t.Fatalf("summary = %+v, want the same-UID denial reported", summary)
	}
	if summary.ProtectHits["proc_scan_error"] != 1 {
		t.Fatalf("protect hits = %#v, want only the referenced candidate unresolvable", summary.ProtectHits)
	}
	if summary.DeletedEntries != 1 {
		t.Fatalf("summary = %+v, want the unreferenced candidate swept", summary)
	}
	assertExists(t, held)
	assertMissing(t, stale)
}

// Regression for #1125: with a /proc scan that fails for a subset of pids, the
// failing pids must not change the verdict for candidates they never touched,
// and a candidate a live worker really holds must still be protected.
func TestSweepKeepsPerCandidateVerdictsWhenSomeProcessesFailToScan(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permission denials cannot be simulated as root")
	}
	root, proc, now := fakeRoots(t)
	live := oldDir(t, root, "tmp.live", now, 3*time.Hour, "live")
	garbage := oldDir(t, root, "tmp.garbage", now, 3*time.Hour, "garbage")

	// A healthy worker sitting in tmp.live: fully resolvable, so tmp.live is
	// protected on the evidence itself.
	worker := filepath.Join(proc, "700")
	if err := os.MkdirAll(filepath.Join(worker, "fd"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(live, filepath.Join(worker, "cwd")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worker, "cmdline"), []byte("claude\x00"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Two same-UID session daemons that deny every read, like `systemd --user`
	// and `(sd-pam)` on the fleet host. They reference no candidate.
	for _, pid := range []string{"701", "702"} {
		processDir := filepath.Join(proc, pid)
		if err := os.MkdirAll(processDir, 0o755); err != nil {
			t.Fatal(err)
		}
		fdDir := filepath.Join(processDir, "fd")
		if err := os.MkdirAll(fdDir, 0o000); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.Chmod(fdDir, 0o755) })
		if err := os.WriteFile(filepath.Join(processDir, "cmdline"), []byte("session-daemon\x00"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	summary, err := Sweep(context.Background(), fakeOptions(root, proc, now, ModeDryRun, 1, true))
	if err != nil {
		t.Fatal(err)
	}
	if summary.ProcUnresolvedProcesses != 2 || summary.ProcScanErrors == 0 {
		t.Fatalf("summary = %+v, want both denying processes counted", summary)
	}
	if summary.ProtectHits["proc_scan_error"] != 0 {
		t.Fatalf("protect hits = %#v, want no candidate charged with an unrelated denial", summary.ProtectHits)
	}
	if summary.ProtectHits["process_cwd"] != 1 || summary.ProtectedEntries != 1 {
		t.Fatalf("summary = %+v, want only the worker-held candidate protected", summary)
	}
	if summary.ReclaimableBytes != int64(len("garbage")) || summary.SweepIneffective {
		t.Fatalf("summary = %+v, want the abandoned candidate reported as reclaimable", summary)
	}
	assertExists(t, live)
	assertExists(t, garbage)
}

// #1125: a sweep that protects everything it matched and reclaims nothing is
// the shape of a broken reaper, not of a clean /tmp, so it must not read as a
// quiet successful tick.
func TestSweepFlagsFullyProtectedZeroReclaimSweep(t *testing.T) {
	root, proc, now := fakeRoots(t)
	young := oldDir(t, root, "tmp.young", now, 10*time.Minute, "new")

	summary, err := Sweep(context.Background(), fakeOptions(root, proc, now, ModeDryRun, 40, true))
	if err != nil {
		t.Fatal(err)
	}
	if !summary.SweepIneffective || summary.AttentionCode != "tmpfs_sweep_ineffective" {
		t.Fatalf("summary = %+v, want the no-op sweep flagged", summary)
	}

	setTreeAge(t, young, now.Add(-3*time.Hour))
	summary, err = Sweep(context.Background(), fakeOptions(root, proc, now, ModeDryRun, 40, true))
	if err != nil {
		t.Fatal(err)
	}
	if summary.SweepIneffective || summary.AttentionCode != "" {
		t.Fatalf("summary = %+v, want a productive sweep left unflagged", summary)
	}
}

// The 2026-07-25 event: 8.5GB used of a ~15GiB RAM-backed /tmp. That is only
// 55% of the mount, so the old "85% utilization" rule stayed silent while 8.5GB
// of a 24GiB host's RAM was already gone. Pressure is decided on the remaining
// absolute bytes (#1128).
func TestSweepPressureUsesFreeByteBudgetNotUtilizationPct(t *testing.T) {
	root, proc, now := fakeRoots(t)

	tight := fakeOptions(root, proc, now, ModeApply, 55, true)
	tight.InspectMount = func(string) (MountUsage, error) {
		const total = int64(15) << 30
		const used = int64(8_500_000_000)
		return MountUsage{Tmpfs: true, UsePct: 55, TotalBytes: total, UsedBytes: used, AvailableBytes: total - used}, nil
	}
	summary, err := Sweep(context.Background(), tight)
	if err != nil {
		t.Fatal(err)
	}
	if !summary.Pressure || summary.AttentionCode != "tmpfs_pressure" {
		t.Fatalf("summary at %d%% use with %d free bytes = %+v, want pressure", summary.UsePct, summary.AvailableBytes, summary)
	}

	// And the mirror image: a high utilization ratio on a mount that still has
	// tens of gigabytes free is not an emergency and must not page anyone.
	roomy := fakeOptions(root, proc, now, ModeApply, 92, true)
	roomy.InspectMount = func(string) (MountUsage, error) {
		const total = int64(400) << 30
		const available = int64(32) << 30
		return MountUsage{Tmpfs: true, UsePct: 92, TotalBytes: total, UsedBytes: total - available, AvailableBytes: available}, nil
	}
	summary, err = Sweep(context.Background(), roomy)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Pressure || summary.AttentionCode != "" {
		t.Fatalf("summary at %d%% use with %d free bytes = %+v, want no pressure", summary.UsePct, summary.AvailableBytes, summary)
	}
}
