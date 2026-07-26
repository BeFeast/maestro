//go:build linux

// Package tmpfshygiene implements Maestro's conservative, protect-aware tmpfs
// sweeper. It only considers top-level entries that match the compiled policy
// table, refuses non-tmpfs roots, and removes entries relative to an already
// opened root directory so symlink swaps cannot redirect deletion elsewhere.
package tmpfshygiene

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// policy is one explicit top-level allowlist entry. The sweeper never deletes
// an entry that does not match one of these patterns.
type policy struct {
	Category string
	Pattern  string
	MinAge   time.Duration
	// GeneratedName, when set, must also match the entry basename. The glob
	// alone is far too loose for the leak categories: `.*-00000000.so` happily
	// matches an unrelated `.my-plugin-00000000.so`. RegularOnly additionally
	// refuses directories, which the same glob matched. Without both, every
	// apply sweep recursively deleted foreign entries that merely shared the
	// suffix.
	GeneratedName *regexp.Regexp
	RegularOnly   bool
}

// nativeLibLeakName pins the documented generated basename: a hex stem, the
// fixed -00000000 discriminator, and the extension the extractor writes.
var nativeLibLeakName = regexp.MustCompile(`^\.[0-9a-f]+-00000000\.(so|hm)$`)

// defaultPolicies is the compiled policy table used by both the CLI and
// daemon. It is intentionally not configurable at runtime: broadening the
// deletion surface requires a reviewed code-and-test change.
var defaultPolicies = []policy{
	// Bun/Zig-compiled CLIs (opencode) extract a ~5.6MB embedded native
	// library to $TMPDIR/.<hex>-00000000.so on every invocation and never
	// delete it (plus an empty .hm marker). Supervisor backend probes mint
	// these around the clock; on the RAM-backed /tmp this leaked ~10.5GB
	// across 1978 copies before the 2026-07-23 overload. Deleting a mapped
	// .so is safe on Linux: the inode survives until the mapping is gone.
	{Category: "native_lib_leak", Pattern: ".*-00000000.so", MinAge: time.Hour, GeneratedName: nativeLibLeakName, RegularOnly: true},
	{Category: "native_lib_leak", Pattern: ".*-00000000.hm", MinAge: time.Hour, GeneratedName: nativeLibLeakName, RegularOnly: true},
	{Category: "outcome_snapshot", Pattern: "tmp.*", MinAge: time.Hour},
	{Category: "browser_profile", Pattern: "playwright-*", MinAge: 2 * time.Hour},
	{Category: "browser_profile", Pattern: "playwright_chromiumdev_profile-*", MinAge: 2 * time.Hour},
	{Category: "browser_profile", Pattern: "chrome-profile-*", MinAge: 2 * time.Hour},
	{Category: "browser_profile", Pattern: ".org.chromium.Chromium.*", MinAge: 2 * time.Hour},
	{Category: "tooling_cache", Pattern: "go-build*", MinAge: 12 * time.Hour},
	{Category: "tooling_cache", Pattern: "node-compile-cache*", MinAge: 12 * time.Hour},
	{Category: "tooling_cache", Pattern: "npm-*", MinAge: 12 * time.Hour},
	{Category: "tooling_cache", Pattern: "yarn-*", MinAge: 12 * time.Hour},
	{Category: "tooling_cache", Pattern: "pnpm-*", MinAge: 12 * time.Hour},
	{Category: "tooling_cache", Pattern: "uv-*", MinAge: 12 * time.Hour},
	{Category: "worker_scratch", Pattern: "maestro-*", MinAge: 6 * time.Hour},
	{Category: "worker_scratch", Pattern: "claude-*", MinAge: 6 * time.Hour},
	{Category: "worker_scratch", Pattern: "codex-*", MinAge: 6 * time.Hour},
	{Category: "worker_scratch", Pattern: "opencode-*", MinAge: 6 * time.Hour},
}

type treeInfo struct {
	bytes       int64
	newest      time.Time
	hasGit      bool
	crossDevice bool
	ownerUID    int
}

type candidate struct {
	name     string
	path     string
	policy   policy
	info     treeInfo
	protects map[string]struct{}
}

// Sweep evaluates or applies the compiled policy. An error always carries a
// populated Summary so callers can still emit one machine-readable metric.
func Sweep(ctx context.Context, opts Options) (Summary, error) {
	opts = normalizeOptions(opts)
	summary := Summary{
		Timestamp:   opts.Now().UTC(),
		Mode:        opts.Mode,
		Root:        opts.Root,
		Categories:  make(map[string]CategoryStats),
		ProtectHits: make(map[string]int),
	}
	fail := func(err error) (Summary, error) {
		summary.Error = err.Error()
		return summary, err
	}

	if opts.Mode != ModeDryRun && opts.Mode != ModeApply {
		return fail(fmt.Errorf("tmpfs hygiene mode must be %q or %q", ModeDryRun, ModeApply))
	}
	usage, err := opts.InspectMount(opts.Root)
	if err != nil {
		return fail(fmt.Errorf("inspect %s mount: %w", opts.Root, err))
	}
	summary.Tmpfs = usage.Tmpfs
	summary.UsePct = usage.UsePct
	summary.TotalBytes = usage.TotalBytes
	summary.AvailableBytes = usage.AvailableBytes
	summary.PressureFloorBytes = opts.PressureFloorBytes
	if !usage.Tmpfs {
		return fail(fmt.Errorf("refusing tmpfs hygiene: %s is not tmpfs", opts.Root))
	}

	rootInfo, err := os.Lstat(opts.Root)
	if err != nil {
		return fail(fmt.Errorf("stat tmpfs root: %w", err))
	}
	if rootInfo.Mode()&os.ModeSymlink != 0 || !rootInfo.IsDir() {
		return fail(fmt.Errorf("refusing tmpfs hygiene: %s is not a direct directory", opts.Root))
	}
	rootStat, ok := rootInfo.Sys().(*syscall.Stat_t)
	if !ok {
		return fail(errors.New("stat tmpfs root: platform stat metadata unavailable"))
	}

	entries, err := os.ReadDir(opts.Root)
	if err != nil {
		return fail(fmt.Errorf("read tmpfs root: %w", err))
	}
	summary.ScannedEntries = len(entries)
	candidates := make([]candidate, 0)
	nameSet := make(map[string]struct{})
	for _, entry := range entries {
		policy, matched := matchPolicy(entry.Name(), entry)
		if !matched {
			continue
		}
		summary.MatchedEntries++
		stats := summary.Categories[policy.Category]
		stats.Candidates++
		summary.Categories[policy.Category] = stats

		item := candidate{
			name:     entry.Name(),
			path:     filepath.Join(opts.Root, entry.Name()),
			policy:   policy,
			protects: make(map[string]struct{}),
		}
		if alwaysKeepTopLevel(entry.Name()) {
			item.protects["always_keep"] = struct{}{}
		}
		entryInfo, lerr := entry.Info()
		if lerr != nil {
			item.protects["scan_error"] = struct{}{}
		} else if entryInfo.Mode()&os.ModeSymlink != 0 {
			item.protects["symlink"] = struct{}{}
		} else {
			if entryInfo.Mode()&os.ModeSocket != 0 {
				item.protects["always_keep"] = struct{}{}
			}
			item.info, lerr = inspectTree(item.path, uint64(rootStat.Dev))
			if lerr != nil {
				item.protects["scan_error"] = struct{}{}
			}
			if item.info.hasGit {
				item.protects["git_worktree"] = struct{}{}
			}
			if item.info.crossDevice {
				item.protects["mount_boundary"] = struct{}{}
			}
			if ownerMismatch(item.info.ownerUID, opts.EffectiveUID()) {
				item.protects["owner_mismatch"] = struct{}{}
			}
			if !item.info.newest.IsZero() && summary.Timestamp.Sub(item.info.newest) < policy.MinAge {
				item.protects["too_young"] = struct{}{}
			}
		}
		if overlapsAnyProtectedPath(item.path, opts.ProtectedPaths) {
			item.protects["configured_path"] = struct{}{}
		}
		candidates = append(candidates, item)
		nameSet[item.name] = struct{}{}
	}

	procProtect, procScan, err := collectProcessProtects(opts.ProcRoot, opts.Root, nameSet, opts.processID())
	if err != nil {
		return fail(fmt.Errorf("build process protect set: %w", err))
	}
	summary.ProcScanErrors = procScan.errors
	summary.ProcPermissionSkips = procScan.permissionSkips
	for i := range candidates {
		for reason := range procProtect[candidates[i].name] {
			candidates[i].protects[reason] = struct{}{}
		}
		// Unexpected scan failures blanket-protect the whole sweep, but
		// permission-denied reads of other users' /proc entries are the normal
		// state on any multi-user host (the daemon is not root). Treating them
		// as scan errors made every sweep a permanent no-op (2026-07-23 RCA):
		// protected==matched, deleted==0, while /tmp filled RAM. A foreign-uid
		// process can hold a candidate open, but deletion of same-uid entries
		// is still safe on Linux — the inode outlives the unlink.
		if procScan.errors > 0 {
			candidates[i].protects["proc_scan_error"] = struct{}{}
		}
	}

	var rootFD int = -1
	if opts.Mode == ModeApply {
		rootFD, err = unix.Open(opts.Root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err != nil {
			return fail(fmt.Errorf("open tmpfs root safely: %w", err))
		}
		defer unix.Close(rootFD)
	}

	var quarantine *sweepQuarantine
	defer func() {
		if quarantine != nil {
			quarantine.close(rootFD)
		}
	}()

	sort.Slice(candidates, func(i, j int) bool { return candidates[i].name < candidates[j].name })
	for _, item := range candidates {
		select {
		case <-ctx.Done():
			return fail(ctx.Err())
		default:
		}
		stats := summary.Categories[item.policy.Category]
		if opts.Mode == ModeApply && len(item.protects) == 0 {
			if opts.beforeApply != nil {
				opts.beforeApply(item.path)
			}
			if quarantine == nil {
				quarantine, err = createSweepQuarantine(rootFD, opts.Root, uint64(rootStat.Dev))
				if err != nil {
					summary.Categories[item.policy.Category] = stats
					return fail(fmt.Errorf("create private sweep quarantine: %w", err))
				}
			}
			isolated := false
			if err := unix.Renameat(rootFD, item.name, quarantine.fd, item.name); err != nil {
				if err == unix.ENOENT {
					item.protects["disappeared"] = struct{}{}
				} else {
					summary.Categories[item.policy.Category] = stats
					return fail(fmt.Errorf("isolate %s safely: %w", item.path, err))
				}
			} else {
				isolated = true
			}

			if isolated {
				// Isolation closes the replacement race: every mutable check below is
				// performed on the exact object moved into the private directory, and
				// ordinary processes can no longer open it through its public name.
				isolatedPath := filepath.Join(quarantine.path, item.name)
				entryInfo, lerr := os.Lstat(isolatedPath)
				switch {
				case errors.Is(lerr, fs.ErrNotExist):
					item.protects["disappeared"] = struct{}{}
				case lerr != nil:
					item.protects["scan_error"] = struct{}{}
				case entryInfo.Mode()&os.ModeSymlink != 0:
					item.protects["symlink"] = struct{}{}
				case entryInfo.Mode()&os.ModeSocket != 0:
					item.protects["always_keep"] = struct{}{}
				default:
					refreshed, refreshErr := inspectTree(isolatedPath, uint64(rootStat.Dev))
					if refreshErr != nil {
						item.protects["scan_error"] = struct{}{}
					} else {
						item.info = refreshed
						if refreshed.hasGit {
							item.protects["git_worktree"] = struct{}{}
						}
						if refreshed.crossDevice {
							item.protects["mount_boundary"] = struct{}{}
						}
						if ownerMismatch(refreshed.ownerUID, opts.EffectiveUID()) {
							item.protects["owner_mismatch"] = struct{}{}
						}
						if !refreshed.newest.IsZero() && summary.Timestamp.Sub(refreshed.newest) < item.policy.MinAge {
							item.protects["too_young"] = struct{}{}
						}
					}
				}

				// The initial /proc snapshot may be stale by the time a large tree is
				// reached. Scan again after isolation. cwd/fd links follow the isolated
				// path; cmdlines can still contain the former public path, so both top-
				// level names are mapped back to this candidate.
				freshNames := map[string]struct{}{item.name: {}, quarantine.name: {}}
				freshProtect, freshScan, scanErr := collectProcessProtects(opts.ProcRoot, opts.Root, freshNames, opts.processID())
				summary.ProcScanErrors += freshScan.errors
				summary.ProcPermissionSkips += freshScan.permissionSkips
				if scanErr != nil {
					if restoreErr := quarantine.restore(rootFD, item.name); restoreErr != nil {
						scanErr = fmt.Errorf("%w; also failed to restore isolated candidate: %v", scanErr, restoreErr)
					}
					summary.Categories[item.policy.Category] = stats
					return fail(fmt.Errorf("refresh process protect set for %s: %w", item.path, scanErr))
				}
				for _, name := range []string{item.name, quarantine.name} {
					for reason := range freshProtect[name] {
						item.protects[reason] = struct{}{}
					}
				}
				if freshScan.errors > 0 {
					item.protects["proc_scan_error"] = struct{}{}
				}
				if len(item.protects) > 0 {
					if restoreErr := quarantine.restore(rootFD, item.name); restoreErr != nil {
						summary.Categories[item.policy.Category] = stats
						return fail(fmt.Errorf("restore protected %s: %w", item.path, restoreErr))
					}
				}
			}
		}
		if len(item.protects) > 0 {
			summary.ProtectedEntries++
			stats.Protected++
			for reason := range item.protects {
				summary.ProtectHits[reason]++
			}
			summary.Categories[item.policy.Category] = stats
			continue
		}
		summary.ReclaimableBytes += item.info.bytes
		stats.ReclaimableBytes += item.info.bytes
		if opts.Mode == ModeApply {
			result, removeErr := opts.removeEntry(ctx, quarantine.fd, item.name, uint64(rootStat.Dev))
			summary.FreedBytes += result.freedBytes
			stats.FreedBytes += result.freedBytes
			if removeErr != nil {
				if result.removedObjects > 0 {
					summary.PartialEntries++
					stats.Partial++
				}
				if restoreErr := quarantine.restore(rootFD, item.name); restoreErr != nil {
					removeErr = fmt.Errorf("%w; partially removed candidate remains quarantined because restore failed: %v", removeErr, restoreErr)
				}
				summary.Categories[item.policy.Category] = stats
				return fail(fmt.Errorf("remove %s safely: %w", item.path, removeErr))
			}
			summary.DeletedEntries++
			stats.Deleted++
		}
		summary.Categories[item.policy.Category] = stats
	}

	finalUsage, err := opts.InspectMount(opts.Root)
	if err != nil {
		return fail(fmt.Errorf("inspect %s after sweep: %w", opts.Root, err))
	}
	summary.Tmpfs = finalUsage.Tmpfs
	summary.UsePct = finalUsage.UsePct
	summary.TotalBytes = finalUsage.TotalBytes
	summary.AvailableBytes = finalUsage.AvailableBytes
	// Pressure is an absolute free-byte budget, not a share of the mount
	// (#1128). A percentage of a RAM-backed tmpfs says nothing about how close
	// the host is to running out of memory.
	summary.Pressure = BelowFloor(finalUsage.AvailableBytes, finalUsage.TotalBytes, opts.PressureFloorBytes)
	if summary.Pressure {
		summary.AttentionCode = "tmpfs_pressure"
	}
	return summary, nil
}

func normalizeOptions(opts Options) Options {
	if strings.TrimSpace(opts.Root) == "" {
		opts.Root = "/tmp"
	}
	opts.Root = filepath.Clean(opts.Root)
	if strings.TrimSpace(opts.ProcRoot) == "" {
		opts.ProcRoot = "/proc"
	}
	opts.ProcRoot = filepath.Clean(opts.ProcRoot)
	if opts.PressureFloorBytes == 0 {
		opts.PressureFloorBytes = DefaultPressureFreeBytes
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.InspectMount == nil {
		opts.InspectMount = InspectLinuxMount
	}
	if opts.EffectiveUID == nil {
		opts.EffectiveUID = os.Geteuid
	}
	if opts.processID == nil {
		opts.processID = os.Getpid
	}
	if opts.removeEntry == nil {
		opts.removeEntry = removeEntryAt
	}
	return opts
}

func ownerMismatch(ownerUID, effectiveUID int) bool {
	return effectiveUID != 0 && ownerUID >= 0 && ownerUID != effectiveUID
}

type sweepQuarantine struct {
	name string
	path string
	fd   int
}

func createSweepQuarantine(rootFD int, root string, rootDevice uint64) (*sweepQuarantine, error) {
	for attempt := 0; attempt < 16; attempt++ {
		var random [8]byte
		if _, err := rand.Read(random[:]); err != nil {
			return nil, err
		}
		name := ".maestro-tmpfs-hygiene-" + hex.EncodeToString(random[:])
		if err := unix.Mkdirat(rootFD, name, 0o700); err != nil {
			if err == unix.EEXIST {
				continue
			}
			return nil, err
		}
		fd, err := unix.Openat(rootFD, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err != nil {
			_ = unix.Unlinkat(rootFD, name, unix.AT_REMOVEDIR)
			return nil, err
		}
		var st unix.Stat_t
		if err := unix.Fstat(fd, &st); err != nil || uint64(st.Dev) != rootDevice {
			_ = unix.Close(fd)
			_ = unix.Unlinkat(rootFD, name, unix.AT_REMOVEDIR)
			if err != nil {
				return nil, err
			}
			return nil, errors.New("quarantine crossed filesystem boundary")
		}
		return &sweepQuarantine{name: name, path: filepath.Join(root, name), fd: fd}, nil
	}
	return nil, errors.New("could not allocate unique quarantine directory")
}

func (q *sweepQuarantine) restore(rootFD int, name string) error {
	return unix.Renameat2(q.fd, name, rootFD, name, unix.RENAME_NOREPLACE)
}

func (q *sweepQuarantine) close(rootFD int) {
	_ = unix.Close(q.fd)
	_ = unix.Unlinkat(rootFD, q.name, unix.AT_REMOVEDIR)
}

// InspectLinuxMount identifies tmpfs by filesystem magic and calculates the
// percentage used from statfs blocks. It is deliberately small and injectable.
func InspectLinuxMount(root string) (MountUsage, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(root, &st); err != nil {
		return MountUsage{}, err
	}
	blockSize := int64(st.Bsize)
	total := int64(st.Blocks) * blockSize
	available := int64(st.Bavail) * blockSize
	used := total - available
	usePct := 0
	if total > 0 {
		usePct = int((used*100 + total - 1) / total)
	}
	return MountUsage{
		Tmpfs:          st.Type == unix.TMPFS_MAGIC,
		UsePct:         usePct,
		TotalBytes:     total,
		UsedBytes:      used,
		AvailableBytes: available,
	}, nil
}

func matchPolicy(name string, entry fs.DirEntry) (policy, bool) {
	for _, policy := range defaultPolicies {
		matched, err := filepath.Match(policy.Pattern, name)
		if err != nil || !matched {
			continue
		}
		if policy.GeneratedName != nil && !policy.GeneratedName.MatchString(name) {
			continue
		}
		if policy.RegularOnly && entry != nil && !entry.Type().IsRegular() {
			continue
		}
		return policy, true
	}
	return policy{}, false
}

// procOwnerIsSelf reports whether /proc/<pid> belongs to the sweeper's own UID.
//
// EACCES on /proc/<pid>/cwd does not prove the process belongs to another
// user: Linux denies the same read for a same-UID process that cleared
// PR_SET_DUMPABLE. Counting that as a routine foreign-process skip would let
// the sweep quarantine and delete a tree such a process is actively sitting
// in, so same-UID denials stay fail-closed.
// procOwnerIsSelf is a variable so tests can simulate a foreign-owned
// /proc entry: without root they cannot chown the fixture, and ownership is
// the only thing separating the two EACCES cases.
var procOwnerIsSelf = defaultProcOwnerIsSelf

func defaultProcOwnerIsSelf(processDir string) bool {
	info, err := os.Stat(processDir)
	if err != nil {
		return false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return false
	}
	return int(stat.Uid) == os.Getuid()
}

func alwaysKeepTopLevel(name string) bool {
	lower := strings.ToLower(name)
	if strings.HasSuffix(lower, ".sock") || strings.HasSuffix(lower, ".socket") || strings.HasSuffix(lower, ".lock") {
		return true
	}
	for _, exact := range []string{".x11-unix", ".ice-unix", ".xim-unix", ".test-unix", ".font-unix"} {
		if lower == exact {
			return true
		}
	}
	for _, prefix := range []string{"systemd-private-", "ssh-", "dbus-"} {
		if strings.HasPrefix(lower, prefix) {
			return true
		}
	}
	return false
}

func inspectTree(root string, rootDevice uint64) (treeInfo, error) {
	info := treeInfo{ownerUID: -1}
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		entryInfo, err := entry.Info()
		if err != nil {
			return err
		}
		stat, ok := entryInfo.Sys().(*syscall.Stat_t)
		if !ok {
			return errors.New("platform stat metadata unavailable")
		}
		if path == root {
			info.ownerUID = int(stat.Uid)
		}
		if uint64(stat.Dev) != rootDevice {
			info.crossDevice = true
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.Name() == ".git" {
			info.hasGit = true
		}
		// A nested symlink is removed as a link by removeEntryAt; its own mtime
		// must not make an otherwise abandoned tree look live. WalkDir and the
		// no-follow stat above never traverse its target.
		if entryInfo.Mode()&os.ModeSymlink != 0 {
			return nil
		}
		if entryInfo.ModTime().After(info.newest) {
			info.newest = entryInfo.ModTime()
		}
		if entryInfo.Mode().IsRegular() {
			info.bytes += entryInfo.Size()
		}
		return nil
	})
	return info, err
}

func overlapsAnyProtectedPath(candidatePath string, protected []string) bool {
	for _, path := range protected {
		path = strings.TrimSpace(path)
		if path == "" {
			continue
		}
		if pathsOverlap(candidatePath, path) {
			return true
		}
		// Config paths can themselves pass through a stable symlink (for example
		// a worktree base alias). Resolve an existing target before deciding the
		// candidate is unrelated, otherwise a lexically external alias could point
		// directly into an allowlisted /tmp tree.
		if resolved, err := filepath.EvalSymlinks(path); err == nil && pathsOverlap(candidatePath, resolved) {
			return true
		}
	}
	return false
}

func pathsOverlap(a, b string) bool {
	absA, errA := filepath.Abs(a)
	absB, errB := filepath.Abs(b)
	if errA != nil || errB != nil {
		return false
	}
	absA = filepath.Clean(absA)
	absB = filepath.Clean(absB)
	return pathWithin(absA, absB) || pathWithin(absB, absA)
}

func pathWithin(path, base string) bool {
	rel, err := filepath.Rel(base, path)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

// procScanResult splits /proc scan outcomes: errors are unexpected failures
// that blanket-protect the sweep; permissionSkips are EACCES reads of other
// users' processes, which are routine for a non-root sweeper and must not
// disable deletion (2026-07-23 RCA: they froze every apply sweep into a no-op).
type procScanResult struct {
	errors          int
	permissionSkips int
}

func collectProcessProtects(procRoot, tmpRoot string, candidates map[string]struct{}, ignoredPID int) (map[string]map[string]struct{}, procScanResult, error) {
	protected := make(map[string]map[string]struct{})
	var scan procScanResult
	entries, err := os.ReadDir(procRoot)
	if err != nil {
		return nil, scan, err
	}
	add := func(value, reason string) {
		for _, name := range candidateNamesFromValue(value, tmpRoot) {
			if _, ok := candidates[name]; !ok {
				continue
			}
			if protected[name] == nil {
				protected[name] = make(map[string]struct{})
			}
			protected[name][reason] = struct{}{}
		}
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		if pid == ignoredPID {
			continue
		}
		processDir := filepath.Join(procRoot, entry.Name())
		if cwd, err := os.Readlink(filepath.Join(processDir, "cwd")); err == nil {
			add(trimDeletedSuffix(cwd), "process_cwd")
		} else if errors.Is(err, fs.ErrPermission) {
			// Another user's process: /proc/<pid>/cwd is unreadable for a
			// non-root sweeper. Routine on every multi-user host — count it
			// separately so it never blanket-protects the sweep. cmdline
			// below is still world-readable, so foreign processes that name
			// a candidate path on their command line stay protected.
			if procOwnerIsSelf(processDir) {
				scan.errors++
			} else {
				scan.permissionSkips++
			}
		} else if !errors.Is(err, fs.ErrNotExist) {
			scan.errors++
		}

		fdEntries, err := os.ReadDir(filepath.Join(processDir, "fd"))
		if err != nil {
			if errors.Is(err, fs.ErrPermission) {
				if procOwnerIsSelf(processDir) {
					scan.errors++
				} else {
					scan.permissionSkips++
				}
			} else if !errors.Is(err, fs.ErrNotExist) {
				scan.errors++
			}
		} else {
			for _, fd := range fdEntries {
				target, err := os.Readlink(filepath.Join(processDir, "fd", fd.Name()))
				if err == nil {
					add(trimDeletedSuffix(target), "process_fd")
				} else if errors.Is(err, fs.ErrPermission) {
					if procOwnerIsSelf(processDir) {
						scan.errors++
					} else {
						scan.permissionSkips++
					}
				} else if !errors.Is(err, fs.ErrNotExist) {
					scan.errors++
				}
			}
		}

		cmdline, err := os.ReadFile(filepath.Join(processDir, "cmdline"))
		if err != nil {
			if errors.Is(err, fs.ErrPermission) {
				if procOwnerIsSelf(processDir) {
					scan.errors++
				} else {
					scan.permissionSkips++
				}
			} else if !errors.Is(err, fs.ErrNotExist) {
				scan.errors++
			}
			continue
		}
		for _, arg := range strings.Split(string(cmdline), "\x00") {
			add(arg, "process_cmdline")
		}
	}
	return protected, scan, nil
}

func trimDeletedSuffix(path string) string {
	return strings.TrimSuffix(path, " (deleted)")
}

func candidateNamesFromValue(value, root string) []string {
	root = filepath.Clean(root)
	prefix := root + string(filepath.Separator)
	var names []string
	for offset := 0; offset < len(value); {
		idx := strings.Index(value[offset:], prefix)
		if idx < 0 {
			break
		}
		start := offset + idx + len(prefix)
		rest := value[start:]
		if rest == "" {
			break
		}
		end := strings.IndexAny(rest, "/:,;\t\r\n \"'()[]{}")
		if end < 0 {
			end = len(rest)
		}
		if name := strings.TrimSpace(rest[:end]); name != "" {
			names = append(names, name)
		}
		offset = start + 1
	}
	return names
}

// removeEntryAt recursively unlinks a direct child of rootFD. Every directory
// is opened with O_NOFOLLOW and every lookup uses *at syscalls, so neither the
// target nor an intermediate component can be redirected through a symlink.
func removeEntryAt(ctx context.Context, rootFD int, name string, rootDevice uint64) (removalResult, error) {
	if name == "" || name == "." || name == ".." || strings.ContainsRune(name, filepath.Separator) {
		return removalResult{}, fmt.Errorf("invalid root entry %q", name)
	}
	return removeAt(ctx, rootFD, name, rootDevice)
}

func removeAt(ctx context.Context, parentFD int, name string, rootDevice uint64) (removalResult, error) {
	select {
	case <-ctx.Done():
		return removalResult{}, ctx.Err()
	default:
	}
	var st unix.Stat_t
	if err := unix.Fstatat(parentFD, name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		if err == unix.ENOENT {
			return removalResult{}, nil
		}
		return removalResult{}, err
	}
	if uint64(st.Dev) != rootDevice {
		return removalResult{}, errors.New("refusing to cross filesystem boundary")
	}
	if st.Mode&unix.S_IFMT != unix.S_IFDIR {
		if err := unix.Unlinkat(parentFD, name, 0); err != nil && err != unix.ENOENT {
			return removalResult{}, err
		}
		result := removalResult{removedObjects: 1}
		if st.Mode&unix.S_IFMT == unix.S_IFREG {
			result.freedBytes = st.Size
		}
		return result, nil
	}

	fd, err := unix.Openat(parentFD, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return removalResult{}, err
	}
	dir := os.NewFile(uintptr(fd), name)
	entries, readErr := dir.ReadDir(-1)
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	result := removalResult{}
	if readErr == nil {
		for _, entry := range entries {
			childResult, err := removeAt(ctx, fd, entry.Name(), rootDevice)
			result.freedBytes += childResult.freedBytes
			result.removedObjects += childResult.removedObjects
			if err != nil {
				dir.Close()
				return result, err
			}
		}
	}
	closeErr := dir.Close()
	if readErr != nil {
		return result, readErr
	}
	if closeErr != nil {
		return result, closeErr
	}
	if err := unix.Unlinkat(parentFD, name, unix.AT_REMOVEDIR); err != nil && err != unix.ENOENT {
		return result, err
	}
	result.removedObjects++
	return result, nil
}
