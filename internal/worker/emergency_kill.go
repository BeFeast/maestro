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

const (
	maestroServiceCgroup = "/sys/fs/cgroup/system.slice/maestro.service/cgroup.procs"
	maestroWorkersSlice  = "/sys/fs/cgroup/maestro-workers.slice"
	maestroWorkersIso    = "/sys/fs/cgroup/maestro-workers-isolated.slice"
)

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
			// Still mark dead so the orchestrator does not keep treating it as live
			// LLM spend after a partial teardown.
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

// KillMaestroWorkerScopeChildren SIGKILLs processes living in sibling
// maestro-workers*.slice worker scopes/services. Best-effort: missing slices
// are ignored. Worktrees are not touched.
func KillMaestroWorkerScopeChildren() int {
	killed := 0
	for _, sliceRoot := range []string{maestroWorkersSlice, maestroWorkersIso} {
		entries, err := os.ReadDir(sliceRoot)
		if err != nil {
			continue
		}
		for _, e := range entries {
			name := e.Name()
			if !strings.HasPrefix(name, "maestro-worker") && !strings.HasPrefix(name, "maestro-workers-") {
				continue
			}
			procs := filepath.Join(sliceRoot, name, "cgroup.procs")
			killed += killCgroupProcs(procs, nil)
		}
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
	alive := make([]int, 0, len(targets))
	for _, pid := range targets {
		if IsAlive(pid) {
			alive = append(alive, pid)
		}
	}
	signalPIDs(alive, syscall.SIGKILL)
	killed := 0
	for _, pid := range targets {
		if !IsAlive(pid) {
			killed++
		}
	}
	return killed
}
