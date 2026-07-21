package worker

import (
	"context"
	"log"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

// Orphan container-client wrapper reaping (#977).
//
// A container-backed verification command (e.g. `sudo docker run --name X ...`)
// spawns a client wrapper chain that blocks until the named container exits. If
// the owning worker/session dies while a wrapper is still blocked, the container
// can go terminal (exited) while the wrapper is reparented to init (PPID=1) and
// lingers indefinitely — the 11-hour zombie that motivated this. Maestro may
// have already completed the authoritative later session, so the orphan is not
// active work and must not be counted as progress.
//
// Invariant: a container-backed verification command must not outlive its
// worker/session once the container is terminal. A parentless wrapper tied to a
// terminal container and a non-live/superseded session is a zombie, not work.

// ContainerState is a snapshot of one docker container as reported by
// `docker ps -a`. Session is the owning maestro session, correlated from a
// container label when the operator's verification command sets one; it is empty
// when no correlation is available, in which case the parentless-wrapper +
// terminal-container signal alone governs reaping.
type ContainerState struct {
	ID      string
	Name    string
	Running bool
	Session string
}

// WrapperProc is a docker-client wrapper process (the `sudo docker run` /
// `docker run` chain) blocked on a named container. PPID==1 means the wrapper
// was reparented to init because its owning worker exited — the strongest
// host-global signal that the owning session is no longer live.
type WrapperProc struct {
	PID       int
	PPID      int
	Container string // container name parsed from the wrapper argv
}

// OrphanCleanupPlan is the pure decision output: wrapper PIDs whose process
// group must be terminated, and terminal container names safe to remove.
type OrphanCleanupPlan struct {
	KillPIDs         []int
	RemoveContainers []string
}

// OrphanCleanupReport is the durable operator-evidence record of one reap pass.
// It is what the watchdog logs and returns; the orphan it clears is never folded
// back into the material-progress watermark.
type OrphanCleanupReport struct {
	KilledWrappers    []int
	RemovedContainers []string
}

// Empty reports whether the pass reaped nothing (the overwhelming common case on
// a host with no leftover docker verification wrappers).
func (r OrphanCleanupReport) Empty() bool {
	return len(r.KilledWrappers) == 0 && len(r.RemovedContainers) == 0
}

// selectOrphanContainerReaps decides which wrapper process groups to terminate
// and which exited containers to remove, given a container snapshot, the live
// wrapper processes, and the set of genuinely live session identifiers.
//
// It is conservative by construction. A wrapper is reaped only when ALL hold:
//   - it is parentless (PPID==1): its owning worker/session is gone;
//   - it references a container that exists and is terminal (not running);
//   - that container's owning session is not live.
//
// A running container, a live owning session, or a wrapper still attached to a
// live parent (PPID!=1) is always preserved — a genuinely active verification
// command is never reaped. A container is removed only when maestro reaped an
// orphan wrapper that referenced it (proving maestro launched it) and no
// preserved wrapper still holds it; unrelated host containers are never touched.
func selectOrphanContainerReaps(containers []ContainerState, wrappers []WrapperProc, liveSessions map[string]bool) OrphanCleanupPlan {
	byName := make(map[string]ContainerState, len(containers))
	for _, c := range containers {
		if c.Name != "" {
			byName[c.Name] = c
		}
	}
	sessionLive := func(s string) bool { return s != "" && liveSessions[s] }

	var plan OrphanCleanupPlan
	killed := make(map[int]bool)
	reapedContainer := make(map[string]bool)
	preservedContainer := make(map[string]bool)

	for _, w := range wrappers {
		c, known := byName[w.Container]
		if !known {
			continue
		}
		reapable := w.PPID == 1 && !c.Running && !sessionLive(c.Session)
		if reapable {
			if w.PID > 1 && !killed[w.PID] {
				plan.KillPIDs = append(plan.KillPIDs, w.PID)
				killed[w.PID] = true
			}
			reapedContainer[c.Name] = true
		} else {
			// A running/live/live-parented wrapper still holds this container.
			preservedContainer[c.Name] = true
		}
	}

	// Iterate containers (not the map) so removal order is deterministic.
	for _, c := range containers {
		if reapedContainer[c.Name] && !preservedContainer[c.Name] {
			plan.RemoveContainers = append(plan.RemoveContainers, c.Name)
		}
	}
	return plan
}

// isDockerRunWrapper reports whether a NUL-joined-then-space-collapsed cmdline is
// a `docker run` / `sudo docker run` client wrapper. It matches the docker
// binary by basename so an absolute path (`/usr/bin/docker`) still counts, and
// requires the `run` subcommand to appear after it.
func isDockerRunWrapper(cmdline string) bool {
	sawDocker, sawRun := false, false
	for _, f := range strings.Fields(cmdline) {
		base := f
		if idx := strings.LastIndexByte(base, '/'); idx >= 0 {
			base = base[idx+1:]
		}
		if base == "docker" {
			sawDocker = true
			continue
		}
		if sawDocker && f == "run" {
			sawRun = true
		}
	}
	return sawDocker && sawRun
}

// parseWrapperContainer extracts the `--name` container from a docker-run
// wrapper cmdline. It returns ok=false for any process that is not a docker-run
// wrapper or does not name a container (an anonymous container cannot be
// correlated to a snapshot entry and is left alone).
func parseWrapperContainer(cmdline string) (string, bool) {
	if !isDockerRunWrapper(cmdline) {
		return "", false
	}
	fields := strings.Fields(cmdline)
	for i, f := range fields {
		if f == "--name" && i+1 < len(fields) {
			return fields[i+1], true
		}
		if strings.HasPrefix(f, "--name=") {
			if name := strings.TrimPrefix(f, "--name="); name != "" {
				return name, true
			}
		}
	}
	return "", false
}

// collectDockerWrappers builds the WrapperProc set from a /proc snapshot,
// reading each candidate's live PPID so a wrapper that has since been reparented
// to init is correctly recognised as parentless.
func collectDockerWrappers(procs []procInfo) []WrapperProc {
	var wrappers []WrapperProc
	for _, p := range procs {
		name, ok := parseWrapperContainer(p.Cmdline)
		if !ok {
			continue
		}
		ppid, ok := readPPID(p.PID)
		if !ok {
			continue
		}
		wrappers = append(wrappers, WrapperProc{PID: p.PID, PPID: ppid, Container: name})
	}
	return wrappers
}

// reaperDeps injects the side-effecting operations so the sweep is unit-testable
// without a real docker daemon or /proc.
type reaperDeps struct {
	listContainers  func() ([]ContainerState, error)
	killGroup       func(pid int)
	removeContainer func(name string) error
}

// sweepOrphanContainerWrappers applies the pure decision to the current
// container snapshot: it terminates each orphan wrapper's whole process group,
// removes the exited containers, and returns a report for operator evidence.
func sweepOrphanContainerWrappers(wrappers []WrapperProc, deps reaperDeps, liveSessions map[string]bool) (OrphanCleanupReport, error) {
	containers, err := deps.listContainers()
	if err != nil {
		return OrphanCleanupReport{}, err
	}
	plan := selectOrphanContainerReaps(containers, wrappers, liveSessions)

	var report OrphanCleanupReport
	for _, pid := range plan.KillPIDs {
		log.Printf("[sweep] reaping orphan container-client wrapper pid=%d (owning worker gone, container terminal)", pid)
		deps.killGroup(pid)
		report.KilledWrappers = append(report.KilledWrappers, pid)
	}
	for _, name := range plan.RemoveContainers {
		if err := deps.removeContainer(name); err != nil {
			log.Printf("[sweep] remove exited orphan container %s: %v", name, err)
			continue
		}
		log.Printf("[sweep] removed exited orphan container %s", name)
		report.RemovedContainers = append(report.RemovedContainers, name)
	}
	return report, nil
}

// ReapOrphanContainerWrappers scans the host for parentless docker-client
// wrapper chains whose named container has gone terminal, terminates those
// wrapper process groups, and removes the exited containers. liveSessions may be
// nil: the parentless-wrapper + terminal-container signal is sufficient on its
// own, and a genuinely active verification command (running container, or a
// wrapper still attached to a live parent) is never reaped either way.
//
// It is a no-op off Linux or when the docker CLI is unavailable, and returns
// early before shelling out to docker when the host has no docker-run wrappers
// at all — the common case. Safe to call repeatedly on the watchdog interval: it
// is idempotent and never touches a running container or a live wrapper.
func ReapOrphanContainerWrappers(liveSessions map[string]bool) OrphanCleanupReport {
	if runtime.GOOS != "linux" {
		return OrphanCleanupReport{}
	}
	dockerBin, err := exec.LookPath("docker")
	if err != nil {
		return OrphanCleanupReport{} // no docker on this host → nothing to reap
	}
	wrappers := collectDockerWrappers(scanProcs())
	if len(wrappers) == 0 {
		return OrphanCleanupReport{}
	}
	deps := reaperDeps{
		listContainers:  func() ([]ContainerState, error) { return listDockerContainers(dockerBin) },
		killGroup:       KillProcessTree,
		removeContainer: func(name string) error { return removeDockerContainer(dockerBin, name) },
	}
	report, err := sweepOrphanContainerWrappers(wrappers, deps, liveSessions)
	if err != nil {
		log.Printf("[sweep] orphan container reap: %v", err)
	}
	return report
}

// listDockerContainers snapshots every container (running and exited) with the
// fields the reaper correlates on. It runs docker without sudo: maestro's daemon
// user is expected to have docker access, and shelling out through sudo from a
// watchdog would be an unbounded privilege surface.
func listDockerContainers(dockerBin string) ([]ContainerState, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, dockerBin, "ps", "-a", "--no-trunc",
		"--format", "{{.Names}}\t{{.State}}\t{{.ID}}\t{{.Label \"maestro.session\"}}").Output()
	if err != nil {
		return nil, err
	}
	return parseDockerContainers(string(out)), nil
}

// parseDockerContainers parses the tab-delimited `docker ps -a` output produced
// by listDockerContainers. A container is Running only when docker reports its
// state as "running"; every other state (exited, dead, created, paused) is
// treated as terminal-or-not-eligible and, for exited, safe to reap.
func parseDockerContainers(out string) []ContainerState {
	var cs []ContainerState
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.Split(line, "\t")
		if len(fields) < 2 {
			continue
		}
		c := ContainerState{
			Name:    strings.TrimSpace(fields[0]),
			Running: strings.EqualFold(strings.TrimSpace(fields[1]), "running"),
		}
		if len(fields) >= 3 {
			c.ID = strings.TrimSpace(fields[2])
		}
		if len(fields) >= 4 {
			c.Session = strings.TrimSpace(fields[3])
		}
		if c.Name != "" {
			cs = append(cs, c)
		}
	}
	return cs
}

// removeDockerContainer force-removes an exited named container.
func removeDockerContainer(dockerBin, name string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return exec.CommandContext(ctx, dockerBin, "rm", "-f", name).Run()
}
