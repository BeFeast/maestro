// Package tmpfshygiene implements Maestro's conservative, protect-aware tmpfs
// sweeper. It only considers top-level entries that match the compiled policy
// table, refuses non-tmpfs roots, and removes entries relative to an already
// opened root directory so symlink swaps cannot redirect deletion elsewhere.
package tmpfshygiene

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const (
	// PressureThresholdPct is the post-sweep tmpfs utilization that Fleet
	// promotes to the tmpfs_pressure attention signal.
	PressureThresholdPct = 85

	ModeDryRun = "dry-run"
	ModeApply  = "apply"
)

// policy is one explicit top-level allowlist entry. The sweeper never deletes
// an entry that does not match one of these patterns.
type policy struct {
	Category string
	Pattern  string
	MinAge   time.Duration
}

// defaultPolicies is the compiled policy table used by both the CLI and
// daemon. It is intentionally not configurable at runtime: broadening the
// deletion surface requires a reviewed code-and-test change.
var defaultPolicies = []policy{
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

// MountUsage is the filesystem identity and capacity snapshot used before and
// after a sweep. InspectMount is injectable so tests never inspect or mutate the
// live host /tmp.
type MountUsage struct {
	Tmpfs          bool  `json:"tmpfs"`
	UsePct         int   `json:"use_pct"`
	TotalBytes     int64 `json:"total_bytes"`
	UsedBytes      int64 `json:"used_bytes"`
	AvailableBytes int64 `json:"available_bytes"`
}

// CategoryStats reports what each allowlisted category matched and reclaimed.
type CategoryStats struct {
	Candidates       int   `json:"candidates"`
	Protected        int   `json:"protected"`
	Deleted          int   `json:"deleted"`
	ReclaimableBytes int64 `json:"reclaimable_bytes"`
	FreedBytes       int64 `json:"freed_bytes"`
}

// Summary is emitted as one JSON line by every CLI or scheduled sweep.
type Summary struct {
	Timestamp        time.Time                `json:"timestamp"`
	Mode             string                   `json:"mode"`
	Root             string                   `json:"root"`
	Tmpfs            bool                     `json:"tmpfs"`
	UsePct           int                      `json:"use_pct"`
	Pressure         bool                     `json:"pressure"`
	AttentionCode    string                   `json:"attention_code,omitempty"`
	ScannedEntries   int                      `json:"scanned_entries"`
	MatchedEntries   int                      `json:"matched_entries"`
	ProtectedEntries int                      `json:"protected_entries"`
	DeletedEntries   int                      `json:"deleted_entries"`
	ReclaimableBytes int64                    `json:"reclaimable_bytes"`
	FreedBytes       int64                    `json:"freed_bytes"`
	Categories       map[string]CategoryStats `json:"categories"`
	ProtectHits      map[string]int           `json:"protect_hits"`
	ProcScanErrors   int                      `json:"proc_scan_errors,omitempty"`
	Error            string                   `json:"error,omitempty"`
}

// Options supplies the sweep roots and safety dependencies. Production uses
// /tmp, /proc, InspectLinuxMount, and the process effective uid.
type Options struct {
	Root           string
	ProcRoot       string
	Mode           string
	ProtectedPaths []string
	Now            func() time.Time
	InspectMount   func(string) (MountUsage, error)
	EffectiveUID   func() int
	beforeApply    func(string) // deterministic revalidation test hook
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
		policy, matched := matchPolicy(entry.Name())
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
			if item.info.ownerUID >= 0 && item.info.ownerUID != opts.EffectiveUID() {
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

	procProtect, scanErrors, err := collectProcessProtects(opts.ProcRoot, opts.Root, nameSet)
	if err != nil {
		return fail(fmt.Errorf("build process protect set: %w", err))
	}
	summary.ProcScanErrors = scanErrors
	for i := range candidates {
		for reason := range procProtect[candidates[i].name] {
			candidates[i].protects[reason] = struct{}{}
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
			// The initial tree scan can be long on a pressured host. Revalidate the
			// candidate immediately before deletion so a newly-written file, git
			// marker, owner change, symlink swap, or bind mount cannot inherit the
			// earlier abandoned-tree decision.
			entryInfo, lerr := os.Lstat(item.path)
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
				refreshed, refreshErr := inspectTree(item.path, uint64(rootStat.Dev))
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
					if refreshed.ownerUID >= 0 && refreshed.ownerUID != opts.EffectiveUID() {
						item.protects["owner_mismatch"] = struct{}{}
					}
					if !refreshed.newest.IsZero() && summary.Timestamp.Sub(refreshed.newest) < item.policy.MinAge {
						item.protects["too_young"] = struct{}{}
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
			if err := removeEntryAt(ctx, rootFD, item.name, uint64(rootStat.Dev)); err != nil {
				summary.Categories[item.policy.Category] = stats
				return fail(fmt.Errorf("remove %s safely: %w", item.path, err))
			}
			summary.DeletedEntries++
			summary.FreedBytes += item.info.bytes
			stats.Deleted++
			stats.FreedBytes += item.info.bytes
		}
		summary.Categories[item.policy.Category] = stats
	}

	finalUsage, err := opts.InspectMount(opts.Root)
	if err != nil {
		return fail(fmt.Errorf("inspect %s after sweep: %w", opts.Root, err))
	}
	summary.Tmpfs = finalUsage.Tmpfs
	summary.UsePct = finalUsage.UsePct
	summary.Pressure = finalUsage.UsePct >= PressureThresholdPct
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
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.InspectMount == nil {
		opts.InspectMount = InspectLinuxMount
	}
	if opts.EffectiveUID == nil {
		opts.EffectiveUID = os.Geteuid
	}
	return opts
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

func matchPolicy(name string) (policy, bool) {
	for _, policy := range defaultPolicies {
		matched, err := filepath.Match(policy.Pattern, name)
		if err == nil && matched {
			return policy, true
		}
	}
	return policy{}, false
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

func collectProcessProtects(procRoot, tmpRoot string, candidates map[string]struct{}) (map[string]map[string]struct{}, int, error) {
	protected := make(map[string]map[string]struct{})
	entries, err := os.ReadDir(procRoot)
	if err != nil {
		return nil, 0, err
	}
	scanErrors := 0
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
		if _, err := strconv.Atoi(entry.Name()); err != nil {
			continue
		}
		processDir := filepath.Join(procRoot, entry.Name())
		if cwd, err := os.Readlink(filepath.Join(processDir, "cwd")); err == nil {
			add(trimDeletedSuffix(cwd), "process_cwd")
		} else if !errors.Is(err, fs.ErrNotExist) && !errors.Is(err, fs.ErrPermission) {
			scanErrors++
		} else if errors.Is(err, fs.ErrPermission) {
			scanErrors++
		}

		fdEntries, err := os.ReadDir(filepath.Join(processDir, "fd"))
		if err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				scanErrors++
			}
		} else {
			for _, fd := range fdEntries {
				target, err := os.Readlink(filepath.Join(processDir, "fd", fd.Name()))
				if err == nil {
					add(trimDeletedSuffix(target), "process_fd")
				} else if !errors.Is(err, fs.ErrNotExist) {
					scanErrors++
				}
			}
		}

		cmdline, err := os.ReadFile(filepath.Join(processDir, "cmdline"))
		if err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				scanErrors++
			}
			continue
		}
		for _, arg := range strings.Split(string(cmdline), "\x00") {
			add(arg, "process_cmdline")
		}
	}
	return protected, scanErrors, nil
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
func removeEntryAt(ctx context.Context, rootFD int, name string, rootDevice uint64) error {
	if name == "" || name == "." || name == ".." || strings.ContainsRune(name, filepath.Separator) {
		return fmt.Errorf("invalid root entry %q", name)
	}
	return removeAt(ctx, rootFD, name, rootDevice)
}

func removeAt(ctx context.Context, parentFD int, name string, rootDevice uint64) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	var st unix.Stat_t
	if err := unix.Fstatat(parentFD, name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		if err == unix.ENOENT {
			return nil
		}
		return err
	}
	if uint64(st.Dev) != rootDevice {
		return errors.New("refusing to cross filesystem boundary")
	}
	if st.Mode&unix.S_IFMT != unix.S_IFDIR {
		if err := unix.Unlinkat(parentFD, name, 0); err != nil && err != unix.ENOENT {
			return err
		}
		return nil
	}

	fd, err := unix.Openat(parentFD, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	dir := os.NewFile(uintptr(fd), name)
	entries, readErr := dir.ReadDir(-1)
	if readErr == nil {
		for _, entry := range entries {
			if err := removeAt(ctx, fd, entry.Name(), rootDevice); err != nil {
				dir.Close()
				return err
			}
		}
	}
	closeErr := dir.Close()
	if readErr != nil {
		return readErr
	}
	if closeErr != nil {
		return closeErr
	}
	if err := unix.Unlinkat(parentFD, name, unix.AT_REMOVEDIR); err != nil && err != unix.ENOENT {
		return err
	}
	return nil
}
