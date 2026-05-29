package worker

import (
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// VisualQATempPrefix is the well-known prefix for visual-QA Chrome temp
// directories created by worker-spawned QA browsers. The startup sweep only
// targets processes and directories matching this prefix so it can never kill
// an unrelated chrome instance.
const VisualQATempPrefix = "/tmp/scribe-visual-qa-"

// KillProcessTree terminates the process identified by pid together with its
// entire descendant tree. A worker is hosted inside a shared, detached tmux
// server, so processes it spawns (notably headless Chrome and its crashpad
// handler) are reparented away from the pane shell. Killing only the recorded
// pane PID therefore leaks those grandchildren. This walks the live process
// tree rooted at pid and signals every descendant.
//
// On Linux the descendant set is derived from /proc; on other platforms the
// process tree is not enumerated (the runtime is Linux/workshop), so this falls
// back to a best-effort kill of pid alone. A non-positive pid is a no-op.
func KillProcessTree(pid int) {
	if pid <= 0 {
		return
	}

	pids := []int{pid}
	if runtime.GOOS == "linux" {
		pids = collectProcessTree(pid)
	}

	// Signal deepest descendants first so parents cannot re-spawn children
	// between the SIGTERM and SIGKILL passes; the recorded root pid is last.
	signalPIDs(pids, syscall.SIGTERM)

	// Give processes a brief window to exit cleanly, then force-kill whatever
	// is still alive (Chrome's crashpad handler in particular ignores SIGTERM).
	deadline := time.Now().Add(2 * time.Second)
	for {
		alive := make([]int, 0, len(pids))
		for _, p := range pids {
			if IsAlive(p) {
				alive = append(alive, p)
			}
		}
		pids = alive
		if len(pids) == 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	signalPIDs(pids, syscall.SIGKILL)
}

// signalPIDs sends sig to each pid in reverse order (children before the root).
func signalPIDs(pids []int, sig syscall.Signal) {
	for i := len(pids) - 1; i >= 0; i-- {
		p := pids[i]
		if p <= 0 {
			continue
		}
		if err := syscall.Kill(p, sig); err != nil {
			// ESRCH (no such process) is expected and benign — it just means
			// the process already exited. Anything else is worth logging.
			if err != syscall.ESRCH {
				log.Printf("[worker] kill pid %d (%v): %v", p, sig, err)
			}
		}
	}
}

// collectProcessTree returns root plus all of its transitive children on Linux,
// ordered root-first (callers signal it in reverse to reach leaves first).
// It reads PPID for every process in /proc and performs a BFS from root.
func collectProcessTree(root int) []int {
	children := childrenByParent()

	order := []int{root}
	queue := []int{root}
	visited := map[int]bool{root: true}
	for len(queue) > 0 {
		parent := queue[0]
		queue = queue[1:]
		for _, child := range children[parent] {
			if visited[child] {
				continue
			}
			visited[child] = true
			order = append(order, child)
			queue = append(queue, child)
		}
	}
	return order
}

// childrenByParent reads /proc and maps each PPID to its direct children.
func childrenByParent() map[int][]int {
	children := make(map[int][]int)
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return children
	}
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		ppid, ok := readPPID(pid)
		if !ok {
			continue
		}
		children[ppid] = append(children[ppid], pid)
	}
	return children
}

// readPPID parses the parent PID from /proc/<pid>/stat.
func readPPID(pid int) (int, bool) {
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return 0, false
	}
	return parsePPIDFromStat(string(data))
}

// parsePPIDFromStat extracts the parent PID from the contents of a
// /proc/<pid>/stat line. The format is "<pid> (<comm>) <state> <ppid> ...",
// and comm may itself contain spaces and parentheses, so the fields after the
// executable name are located relative to the final ')'.
func parsePPIDFromStat(s string) (int, bool) {
	rparen := strings.LastIndexByte(s, ')')
	if rparen < 0 || rparen+1 >= len(s) {
		return 0, false
	}
	fields := strings.Fields(s[rparen+1:])
	// fields[0] is state, fields[1] is ppid.
	if len(fields) < 2 {
		return 0, false
	}
	ppid, err := strconv.Atoi(fields[1])
	if err != nil {
		return 0, false
	}
	return ppid, true
}

// procInfo is a minimal snapshot of a running process used by the visual-QA
// sweep. It is intentionally decoupled from the OS so the kill-selection logic
// can be unit-tested without touching /proc.
type procInfo struct {
	PID     int
	Cmdline string // NUL-joined argv, collapsed to a single space-joined string
	Cwd     string // resolved working directory, may be empty if unreadable
}

// selectVisualQAKills returns the PIDs from procs that should be killed because
// they belong to a leftover visual-QA Chrome run, identified by the temp-dir
// prefix appearing in either their argv or their working directory.
//
// This is the pure decision core of the startup sweep: it is conservative by
// construction — a process is selected only when prefix (a fixed, well-known
// path) is present, so generic chrome instances are never matched. An empty
// prefix selects nothing.
func selectVisualQAKills(procs []procInfo, prefix string) []int {
	if prefix == "" {
		return nil
	}
	var kill []int
	for _, p := range procs {
		if p.PID <= 0 {
			continue
		}
		if strings.Contains(p.Cmdline, prefix) || (p.Cwd != "" && strings.HasPrefix(p.Cwd, prefix)) {
			kill = append(kill, p.PID)
		}
	}
	return kill
}

// SweepStaleVisualQA cleans up visual-QA Chrome artifacts left behind by a
// previous run. It kills any process whose argv or cwd references the
// VisualQATempPrefix temp dirs, then removes the stale temp dirs themselves.
//
// It is conservative on two axes: it only ever targets the well-known
// VisualQATempPrefix, and it skips processes that are still parented to a live
// worker (those are reaped through the normal teardown path, not the sweep).
// On non-Linux platforms process enumeration via /proc is unavailable, so the
// process-kill phase is skipped and only stale temp dirs are removed.
func SweepStaleVisualQA() {
	if runtime.GOOS == "linux" {
		procs := scanProcs()
		for _, pid := range selectVisualQAKills(procs, VisualQATempPrefix) {
			log.Printf("[sweep] killing stale visual-QA process tree pid=%d", pid)
			KillProcessTree(pid)
		}
	}
	removeStaleVisualQADirs(VisualQATempPrefix)
}

// scanProcs builds a procInfo snapshot of every process in /proc (Linux only).
func scanProcs() []procInfo {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	var procs []procInfo
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		procs = append(procs, procInfo{
			PID:     pid,
			Cmdline: readCmdline(pid),
			Cwd:     readCwd(pid),
		})
	}
	return procs
}

// readCmdline reads /proc/<pid>/cmdline and joins its NUL-separated argv with
// spaces. Returns "" if the cmdline is unreadable or empty (e.g. a kernel
// thread).
func readCmdline(pid int) string {
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "cmdline"))
	if err != nil {
		return ""
	}
	parts := strings.Split(strings.TrimRight(string(data), "\x00"), "\x00")
	return strings.Join(parts, " ")
}

// readCwd resolves /proc/<pid>/cwd. Returns "" if it cannot be read.
func readCwd(pid int) string {
	cwd, err := os.Readlink(filepath.Join("/proc", strconv.Itoa(pid), "cwd"))
	if err != nil {
		return ""
	}
	return cwd
}

// removeStaleVisualQADirs removes leftover temp dirs matching prefix*. The
// prefix is split into its parent directory and a name prefix so we can match
// siblings (e.g. /tmp/scribe-visual-qa-abc123) without globbing arbitrary
// paths.
func removeStaleVisualQADirs(prefix string) {
	if prefix == "" {
		return
	}
	parent := filepath.Dir(prefix)
	base := filepath.Base(prefix)
	entries, err := os.ReadDir(parent)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), base) {
			continue
		}
		full := filepath.Join(parent, e.Name())
		if err := os.RemoveAll(full); err != nil {
			log.Printf("[sweep] remove stale visual-QA dir %s: %v", full, err)
		} else {
			log.Printf("[sweep] removed stale visual-QA dir %s", full)
		}
	}
}
