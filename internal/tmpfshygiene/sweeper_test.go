package tmpfshygiene

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
			return MountUsage{Tmpfs: tmpfs, UsePct: usePct, TotalBytes: 100, UsedBytes: int64(usePct)}, nil
		},
		EffectiveUID: os.Geteuid,
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
