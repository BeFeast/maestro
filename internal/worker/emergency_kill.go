package worker

import (
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/state"
)

const maestroServiceCgroup = "/sys/fs/cgroup/system.slice/maestro.service/cgroup.procs"

// workerSliceCgroupRoots lists candidate cgroup directories that hold worker
// scopes/services. systemd nests slices by dash hierarchy, so in production
// (system manager) `--slice=maestro-workers.slice` materializes as
// /sys/fs/cgroup/maestro.slice/maestro-workers.slice, with the isolated slice
// nested one level deeper — NOT at the cgroup root. The flat variants are kept
// as fallbacks for delegated/container layouts. Overridable in tests.
var workerSliceCgroupRoots = func() []string {
	return []string{
		"/sys/fs/cgroup/maestro.slice/maestro-workers.slice",
		"/sys/fs/cgroup/maestro-workers.slice",
		"/sys/fs/cgroup/maestro.slice/maestro-workers-isolated.slice",
		"/sys/fs/cgroup/maestro-workers-isolated.slice",
	}
}

// KillRunningSessions SIGKILLs every StatusRunning worker for cfg and marks the
// sessions StatusDead. Worktrees are preserved (unlike Stop) so an emergency
// halt can be inspected / resumed later without destroying in-progress diffs.
//
// Used by the fleet EMERGENCY STOP path (#840): engaging the switch must halt
// live LLM spend immediately, not merely refuse new spawns.
func KillRunningSessions(cfg *config.Config) int {
	if cfg == nil {
		return 0
	}
	s, err := state.Load(cfg.StateDir)
	if err != nil {
		log.Printf("[worker] emergency kill: load state %s: %v", cfg.StateDir, err)
		return 0
	}
	changed := false
	killed := 0
	for slot, sess := range s.Sessions {
		if sess == nil || sess.Status != state.StatusRunning {
			continue
		}
		if err := StopProcess(slot, sess); err != nil {
			log.Printf("[worker] emergency kill: stop %s (%s): %v", slot, cfg.Repo, err)
			// Only mark the session dead when the runtime is verifiably gone.
			// Marking a still-alive worker dead makes state.json lie: the process
			// keeps burning tokens while resume-time redispatch spawns a duplicate
			// worker for the same issue (double-PR). Leave it StatusRunning and
			// loud in the journal; the cgroup sweeps below remain the backstop.
			if sessionRuntimeAlive(sess) {
				log.Printf("[worker] EMERGENCY KILL FAILED for %s (%s): worker runtime still alive — leaving status untouched, cgroup sweep is the backstop", slot, cfg.Repo)
				continue
			}
		}
		now := time.Now().UTC()
		sess.Status = state.StatusDead
		sess.FinishedAt = &now
		changed = true
		killed++
	}
	if changed {
		if err := state.Save(cfg.StateDir, s); err != nil {
			log.Printf("[worker] emergency kill: save state %s: %v", cfg.StateDir, err)
		}
	}
	return killed
}

// KillAllRunningSessions sweeps every project config. Best-effort per project.
func KillAllRunningSessions(cfgs []*config.Config) int {
	total := 0
	for _, cfg := range cfgs {
		total += KillRunningSessions(cfg)
	}
	return total
}

// EmergencyKillResult summarizes one emergency teardown sweep.
type EmergencyKillResult struct {
	Workers  int // StatusRunning tmux/lease workers stopped
	Attached int // daemon-cgroup / worker-scope children (gh, java, go, verify, …)
}

// EmergencyKillAll stops in-flight LLM workers and maestro-attached non-LLM
// children (verify-outcome, java/gradle, gh api, go test/build, aapt2, …).
// Worktrees and the maestro daemon process itself are preserved.
func EmergencyKillAll(cfgs []*config.Config) EmergencyKillResult {
	res := EmergencyKillResult{
		Workers: KillAllRunningSessions(cfgs),
	}
	res.Attached += KillDaemonCgroupChildren(0)
	res.Attached += KillMaestroWorkerScopeChildren()
	return res
}

// KillDaemonCgroupChildren SIGKILLs every process in the maestro.service cgroup
// except the daemon (and, when called from inside the daemon, the caller).
// preservePID is the daemon PID when known; pass 0 to auto-detect any
// "maestro daemon" cmdline in the cgroup (CLI path).
//
// This is the brake for in-daemon children that are not tracked as worker
// sessions: GitHub `gh api` probes, outcome/verify scripts, Gradle/Java,
// `go test`/`go build`, Android build-tools (aapt2), etc.
func KillDaemonCgroupChildren(preservePID int) int {
	procsPath := resolveMaestroServiceCgroupProcs()
	if procsPath == "" {
		return 0
	}
	preserve := preservePIDs(preservePID, procsPath)
	return killCgroupProcs(procsPath, preserve)
}

// KillMaestroWorkerScopeChildren SIGKILLs processes living in the
// maestro-workers*.slice worker scopes/services (worker leases, the private
// tmux server scope, isolated worker services). The whole slice subtree is
// worker-owned by construction, so every cgroup.procs under it is swept
// recursively — this also covers the isolated slice nested inside the workers
// slice. Best-effort: missing slices are ignored. Worktrees are not touched.
func KillMaestroWorkerScopeChildren() int {
	preserve := map[int]struct{}{os.Getpid(): {}}
	killed := 0
	seen := map[string]struct{}{}
	for _, sliceRoot := range workerSliceCgroupRoots() {
		if _, dup := seen[sliceRoot]; dup {
			continue
		}
		seen[sliceRoot] = struct{}{}
		if fi, err := os.Stat(sliceRoot); err != nil || !fi.IsDir() {
			continue
		}
		_ = filepath.WalkDir(sliceRoot, func(path string, d os.DirEntry, err error) error {
			if err != nil || d == nil || !d.IsDir() {
				return nil
			}
			killed += killCgroupProcs(filepath.Join(path, "cgroup.procs"), preserve)
			return nil
		})
	}
	return killed
}

func resolveMaestroServiceCgroupProcs() string {
	// Only target the live maestro.service cgroup. Do NOT fall back to an
	// arbitrary /proc/self/cgroup: CLI/tests/pct-exec share an ambient session
	// cgroup, and sweeping that would kill the operator shell — not just
	// maestro-attached children.
	if _, err := os.Stat(maestroServiceCgroup); err == nil {
		return maestroServiceCgroup
	}
	// When the daemon itself is running under systemd, INVOCATION_ID is set and
	// /proc/self/cgroup is the service cgroup (possibly renamed). Prefer that
	// only for the daemon process, and only when the path looks like maestro.
	if strings.TrimSpace(os.Getenv("INVOCATION_ID")) == "" {
		return ""
	}
	data, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// unified: "0::/system.slice/maestro.service"
		parts := strings.SplitN(line, ":", 3)
		if len(parts) != 3 {
			continue
		}
		rel := strings.TrimSpace(parts[2])
		if rel == "" || rel == "/" {
			continue
		}
		if !strings.Contains(rel, "maestro.service") && !strings.Contains(rel, "maestro-") {
			continue
		}
		procs := filepath.Join("/sys/fs/cgroup", rel, "cgroup.procs")
		if _, err := os.Stat(procs); err == nil {
			return procs
		}
	}
	return ""
}

func preservePIDs(preservePID int, procsPath string) map[int]struct{} {
	out := map[int]struct{}{os.Getpid(): {}}
	if preservePID > 0 {
		out[preservePID] = struct{}{}
	}
	pids, err := readCgroupPIDs(procsPath)
	if err != nil {
		return out
	}
	for _, pid := range pids {
		if isMaestroDaemonPID(pid) {
			out[pid] = struct{}{}
		}
	}
	return out
}

func isMaestroDaemonPID(pid int) bool {
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "cmdline"))
	if err != nil {
		return false
	}
	parts := strings.Split(strings.TrimRight(string(data), "\x00"), "\x00")
	joined := strings.Join(parts, " ")
	if !strings.Contains(joined, "daemon") {
		return false
	}
	base := ""
	if len(parts) > 0 {
		base = filepath.Base(parts[0])
	}
	return base == "maestro" || strings.HasSuffix(parts[0], "/maestro")
}

func readCgroupPIDs(procsPath string) ([]int, error) {
	data, err := os.ReadFile(procsPath)
	if err != nil {
		return nil, err
	}
	fields := strings.Fields(string(data))
	pids := make([]int, 0, len(fields))
	for _, f := range fields {
		pid, err := strconv.Atoi(f)
		if err != nil || pid <= 0 {
			continue
		}
		pids = append(pids, pid)
	}
	return pids, nil
}

func killCgroupProcs(procsPath string, preserve map[int]struct{}) int {
	pids, err := readCgroupPIDs(procsPath)
	if err != nil {
		return 0
	}
	targets := make([]int, 0, len(pids))
	for _, pid := range pids {
		if preserve != nil {
			if _, skip := preserve[pid]; skip {
				continue
			}
		}
		targets = append(targets, pid)
	}
	if len(targets) == 0 {
		return 0
	}
	// SIGTERM first so scripts can tear down process groups, then SIGKILL.
	signalPIDs(targets, syscall.SIGTERM)
	time.Sleep(200 * time.Millisecond)
	// A pid counts as killed when it is already gone (ESRCH) or when SIGKILL
	// was delivered — the kernel guarantees delivery, so counting on immediate
	// re-probe would just race the (asynchronous) exit and undercount. An
	// unreaped zombie also probes as "alive", so IsAlive alone cannot be the
	// success signal here.
	killed := 0
	for _, pid := range targets {
		if !IsAlive(pid) {
			killed++
			continue
		}
		err := syscall.Kill(pid, syscall.SIGKILL)
		if err == nil || err == syscall.ESRCH {
			killed++
			continue
		}
		log.Printf("[worker] emergency kill: SIGKILL pid %d: %v", pid, err)
	}
	return killed
}

// sessionRuntimeAlive reports whether a session's worker runtime is provably
// still running: its durable process lease is active, or its recorded PID is
// alive. Best-effort probes; an unreadable lease is treated as not-alive so a
// torn-down worker with a corrupt receipt can still be marked dead.
func sessionRuntimeAlive(sess *state.Session) bool {
	if sess == nil {
		return false
	}
	if lease, hasLease, err := sessionProcessLease(sess); err == nil && hasLease {
		if active, aerr := workerProcessLeaseActive(lease); aerr == nil && active {
			return true
		}
	}
	return sess.PID > 0 && IsAlive(sess.PID)
}
