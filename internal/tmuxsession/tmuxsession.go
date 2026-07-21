// Package tmuxsession owns Maestro's tmux control boundary.
//
// Workers use a private tmux server instead of the caller's default server.
// When Maestro itself runs as a systemd service, the first worker starts that
// server in a sibling transient scope. Each pane then launches its runner in a
// unique per-worker scope, so both the tmux control plane and the independently
// owned worker leases survive a restart of maestro.service.
package tmuxsession

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	tmuxName       = "tmux"
	bashPath       = "/usr/bin/bash"
	sudoName       = "sudo"
	systemdRunName = "systemd-run"
	systemctlName  = "systemctl"
	privateSocket  = "maestro-workers"
	workerSlice    = "maestro-workers.slice"

	ProcessLeaseManagerSystem = "system"
	ProcessLeaseManagerUser   = "user"

	processLeaseGracePeriod  = 2 * time.Second
	processLeaseForcePeriod  = 1 * time.Second
	processLeasePollInterval = 100 * time.Millisecond
)

var startMu sync.Mutex

var processLeaseUnitPattern = regexp.MustCompile(`^maestro-worker-[0-9a-f]{32}-g[1-9][0-9]*\.scope$`)

// ProcessLease identifies the exact systemd scope that owns one worker
// attempt. The unit name is deterministic for project + slot + generation, so
// an ambiguous tmux response can be reconciled without replaying the runner.
// Manager is persisted because production workers use the system manager while
// non-service development launches use the user manager.
type ProcessLease struct {
	Unit    string
	Manager string
}

// WorkerProcessLease returns the unique process boundary for one worker
// generation. Hashing durable project identity + slot keeps the unit bounded and systemd-safe
// without putting repository names or host paths into the cgroup name.
func WorkerProcessLease(projectIdentity, slot string, generation uint64) (ProcessLease, error) {
	projectIdentity = strings.TrimSpace(projectIdentity)
	slot = strings.TrimSpace(slot)
	if projectIdentity == "" || slot == "" || generation == 0 {
		return ProcessLease{}, fmt.Errorf("worker process lease requires project identity, slot, and positive generation")
	}
	sum := sha256.Sum256([]byte(projectIdentity + "\x00" + slot))
	unit := fmt.Sprintf("maestro-worker-%x-g%d.scope", sum[:16], generation)
	lease := ProcessLease{Unit: unit, Manager: ProcessLeaseManagerForEnvironment()}
	if err := validateProcessLease(lease); err != nil {
		return ProcessLease{}, err
	}
	return lease, nil
}

// ProcessLeaseManagerForEnvironment selects the production-valid manager.
// maestro.service is a system unit and intentionally has no usable user bus, so
// it must create sibling scopes through the system manager. Standalone CLI
// development uses the user manager and never changes the production path.
func ProcessLeaseManagerForEnvironment() string {
	if strings.TrimSpace(os.Getenv("INVOCATION_ID")) != "" {
		return ProcessLeaseManagerSystem
	}
	return ProcessLeaseManagerUser
}

// HasSession checks the restart-safe private server first and then the legacy
// default server.  The fallback keeps reconciliation able to see sessions that
// were launched before the private-socket cutover.
func HasSession(name string) bool {
	return hasPrivateSession(name) || legacyCommand("has-session", "-t", exactTarget(name)).Run() == nil
}

// CommandForSession returns a tmux command addressed to the server that owns
// name.  New sessions always live on the private server; legacy fallback is
// read/cleanup compatibility only.
func CommandForSession(name string, args ...string) *exec.Cmd {
	if hasPrivateSession(name) {
		return privateCommand(args...)
	}
	return legacyCommand(args...)
}

// CommandContextForSession is CommandForSession with cancellation support.
func CommandContextForSession(ctx context.Context, name string, args ...string) *exec.Cmd {
	if hasPrivateSession(name) {
		return exec.CommandContext(ctx, resolvedPath(tmuxName), append([]string{"-L", privateSocket}, args...)...)
	}
	return exec.CommandContext(ctx, resolvedPath(tmuxName), args...)
}

// ClientArgsForSession returns argv suitable for syscall.Exec of the tmux
// client. The binary path is deterministic and included as argv[0].
func ClientArgsForSession(name string, args ...string) (string, []string) {
	clientArgs := []string{"tmux"}
	if hasPrivateSession(name) {
		clientArgs = append(clientArgs, "-L", privateSocket)
	}
	return resolvedPath(tmuxName), append(clientArgs, args...)
}

// StartDetached starts one worker session.  When the private tmux server does
// not yet exist, a system-service Maestro launches it in a sibling systemd
// scope. Subsequent sessions are created by that already-isolated server, but
// each pane immediately launches its runner in the supplied per-worker scope;
// the shared tmux scope is never the worker kill boundary.
func StartDetached(name, worktree, runnerPath string, lease ProcessLease) ([]byte, error) {
	startMu.Lock()
	defer startMu.Unlock()

	workerCommand, err := processLeaseLaunchCommand(lease, runnerPath, os.Getuid(), os.Getenv("PATH"))
	if err != nil {
		return nil, err
	}
	args := append([]string{"new-session", "-d", "-s", name, "-c", worktree}, workerCommand...)
	if privateServerAlive() {
		return privateCommand(args...).CombinedOutput()
	}

	// INVOCATION_ID is set by systemd for both persistent and transient units.
	// The fleet daemon is a system unit without a user-bus environment, so use
	// the system manager through the same passwordless sudo boundary as
	// self-deploy.  Failing this launch fails closed: launching directly here
	// would silently put workers back into maestro.service's kill cgroup.
	if strings.TrimSpace(os.Getenv("INVOCATION_ID")) != "" {
		unit := fmt.Sprintf("maestro-workers-%d-%d", os.Getpid(), time.Now().Unix())
		scopeArgs := scopeLaunchArgs(unit, os.Getuid(), os.Getenv("PATH"), args)
		return exec.Command(resolvedPath(sudoName), scopeArgs...).CombinedOutput()
	}

	// CLI development runs are not tied to a long-lived service cgroup.  Keep
	// them functional while still using the private socket so behavior matches
	// the daemon once the command is imported/reconciled later.
	return privateCommand(args...).CombinedOutput()
}

// PanePID returns the pane shell PID for name.
func PanePID(name string) ([]byte, error) {
	return CommandForSession(name, "list-panes", "-t", exactTarget(name), "-F", "#{pane_pid}").Output()
}

// KillSession terminates exactly name without fuzzy tmux target matching.
func KillSession(name string) ([]byte, error) {
	return CommandForSession(name, "kill-session", "-t", exactTarget(name)).CombinedOutput()
}

func hasPrivateSession(name string) bool {
	return privateCommand("has-session", "-t", exactTarget(name)).Run() == nil
}

func privateServerAlive() bool {
	return privateCommand("list-sessions").Run() == nil
}

func privateCommand(args ...string) *exec.Cmd {
	return exec.Command(resolvedPath(tmuxName), append([]string{"-L", privateSocket}, args...)...)
}

func legacyCommand(args ...string) *exec.Cmd {
	return exec.Command(resolvedPath(tmuxName), args...)
}

func exactTarget(name string) string {
	return "=" + strings.TrimSpace(name) + ":"
}

func scopeLaunchArgs(unit string, uid int, path string, tmuxArgs []string) []string {
	args := []string{
		"-n",
		resolvedPath(systemdRunName),
		"--scope",
		"--collect",
		"--uid=" + strconv.Itoa(uid),
		"--unit=" + unit,
		"--slice=" + workerSlice,
	}
	if strings.TrimSpace(path) != "" {
		args = append(args, "--setenv=PATH="+path)
	}
	args = append(args, resolvedPath(tmuxName), "-L", privateSocket)
	return append(args, tmuxArgs...)
}

func processLeaseLaunchCommand(lease ProcessLease, runnerPath string, uid int, path string) ([]string, error) {
	if err := validateProcessLease(lease); err != nil {
		return nil, err
	}
	if strings.TrimSpace(runnerPath) == "" {
		return nil, fmt.Errorf("worker process lease requires runner path")
	}

	common := []string{
		"--scope",
		"--collect",
		"--quiet",
		"--unit=" + lease.Unit,
		"--slice=" + workerSlice,
		"--property=KillMode=control-group",
	}
	if strings.TrimSpace(path) != "" {
		common = append(common, "--setenv=PATH="+path)
	}
	common = append(common, bashPath, runnerPath)

	switch lease.Manager {
	case ProcessLeaseManagerSystem:
		return append([]string{
			resolvedPath(sudoName),
			"-n",
			resolvedPath(systemdRunName),
			"--uid=" + strconv.Itoa(uid),
		}, common...), nil
	case ProcessLeaseManagerUser:
		return append([]string{resolvedPath(systemdRunName), "--user"}, common...), nil
	default:
		return nil, fmt.Errorf("unsupported worker process lease manager %q", lease.Manager)
	}
}

func validateProcessLease(lease ProcessLease) error {
	if !processLeaseUnitPattern.MatchString(strings.TrimSpace(lease.Unit)) {
		return fmt.Errorf("invalid worker process lease unit %q", lease.Unit)
	}
	if lease.Manager != ProcessLeaseManagerSystem && lease.Manager != ProcessLeaseManagerUser {
		return fmt.Errorf("invalid worker process lease manager %q", lease.Manager)
	}
	return nil
}

type processLeaseCommandRunner func(context.Context, string, ...string) ([]byte, error)

func runProcessLeaseCommand(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

// ProcessLeaseActive reports whether the exact persisted scope still owns any
// processes. A collected/missing unit is inactive, making repeated teardown
// safe after daemon restart.
func ProcessLeaseActive(lease ProcessLease) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return processLeaseActive(ctx, lease, runProcessLeaseCommand)
}

// ProcessLeaseOwnsPID proves that pid is currently a member of the exact
// persisted worker scope. Scope liveness alone is not enough for adoption: a
// same-name tmux pane and an independently-active deterministic unit could
// otherwise be mistaken for one another after an ambiguous launch response.
func ProcessLeaseOwnsPID(lease ProcessLease, pid int) (bool, error) {
	if err := validateProcessLease(lease); err != nil {
		return false, err
	}
	if pid <= 0 {
		return false, fmt.Errorf("worker process lease requires positive pid")
	}
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/cgroup", pid))
	if err != nil {
		return false, fmt.Errorf("inspect pid %d cgroup for worker process lease %s: %w", pid, lease.Unit, err)
	}
	return processLeaseUnitInCgroup(data, lease.Unit), nil
}

// ProcessLeaseAnchoredAtPID proves that the pane process or one of its current
// descendants belongs to lease. The pane may be a sudo/systemd-run wrapper in
// the shared tmux-server scope, so requiring the pane PID itself to have moved
// would reject a valid launch. An ancestry check is safe here because it is
// used only to bind the newly-observed pane to its exact cgroup; all subsequent
// ownership and teardown use the durable cgroup receipt, not PPID ancestry.
func ProcessLeaseAnchoredAtPID(lease ProcessLease, panePID int) (bool, error) {
	return processLeaseAnchoredAtPID(lease, panePID, ProcessLeaseOwnsPID, readProcessChildren)
}

type processLeasePIDOwner func(ProcessLease, int) (bool, error)
type processChildrenReader func(int) ([]int, error)

func processLeaseAnchoredAtPID(lease ProcessLease, panePID int, owns processLeasePIDOwner, children processChildrenReader) (bool, error) {
	if err := validateProcessLease(lease); err != nil {
		return false, err
	}
	if panePID <= 0 {
		return false, fmt.Errorf("worker process lease requires positive pane pid")
	}
	queue := []int{panePID}
	seen := make(map[int]struct{})
	for len(queue) > 0 {
		pid := queue[0]
		queue = queue[1:]
		if _, ok := seen[pid]; ok {
			continue
		}
		seen[pid] = struct{}{}
		if len(seen) > 4096 {
			return false, fmt.Errorf("worker process lease pane tree exceeds safety limit")
		}
		owned, err := owns(lease, pid)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return false, err
		}
		if owned {
			return true, nil
		}
		descendants, err := children(pid)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return false, err
		}
		queue = append(queue, descendants...)
	}
	return false, nil
}

func readProcessChildren(pid int) ([]int, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/task/%d/children", pid, pid))
	if err != nil {
		return nil, err
	}
	fields := strings.Fields(string(data))
	children := make([]int, 0, len(fields))
	for _, field := range fields {
		child, err := strconv.Atoi(field)
		if err != nil || child <= 0 {
			return nil, fmt.Errorf("parse child pid %q for process %d", field, pid)
		}
		children = append(children, child)
	}
	return children, nil
}

func processLeaseUnitInCgroup(data []byte, unit string) bool {
	for _, line := range strings.Split(string(data), "\n") {
		parts := strings.SplitN(line, ":", 3)
		if len(parts) != 3 {
			continue
		}
		for _, component := range strings.Split(parts[2], "/") {
			if component == unit {
				return true
			}
		}
	}
	return false
}

// TerminateProcessLease gracefully terminates every process in lease, waits a
// bounded interval, then SIGKILLs only survivors in that same scope. It is
// idempotent: an already-collected unit is success.
func TerminateProcessLease(lease ProcessLease) error {
	ctx, cancel := context.WithTimeout(context.Background(), processLeaseGracePeriod+processLeaseForcePeriod+5*time.Second)
	defer cancel()
	return terminateProcessLease(ctx, lease, processLeaseGracePeriod, processLeaseForcePeriod, processLeasePollInterval, runProcessLeaseCommand)
}

func terminateProcessLease(ctx context.Context, lease ProcessLease, grace, force, poll time.Duration, run processLeaseCommandRunner) error {
	if err := validateProcessLease(lease); err != nil {
		return err
	}
	active, err := processLeaseActive(ctx, lease, run)
	if err != nil {
		return err
	}
	if !active {
		return nil
	}

	if out, err := runProcessLeaseControl(ctx, lease, run, "kill", "--kill-whom=all", "--signal=SIGTERM", lease.Unit); err != nil {
		if !processLeaseMissing(out, err) {
			return fmt.Errorf("terminate worker process lease %s: %w: %s", lease.Unit, err, strings.TrimSpace(string(out)))
		}
	}
	stopped, err := waitProcessLeaseInactive(ctx, lease, grace, poll, run)
	if err != nil {
		return err
	}
	if stopped {
		return nil
	}

	if out, err := runProcessLeaseControl(ctx, lease, run, "kill", "--kill-whom=all", "--signal=SIGKILL", lease.Unit); err != nil {
		if !processLeaseMissing(out, err) {
			return fmt.Errorf("force-kill worker process lease %s: %w: %s", lease.Unit, err, strings.TrimSpace(string(out)))
		}
	}
	stopped, err = waitProcessLeaseInactive(ctx, lease, force, poll, run)
	if err != nil {
		return err
	}
	if !stopped {
		return fmt.Errorf("worker process lease %s still active after SIGKILL", lease.Unit)
	}
	return nil
}

func waitProcessLeaseInactive(ctx context.Context, lease ProcessLease, timeout, poll time.Duration, run processLeaseCommandRunner) (bool, error) {
	if timeout <= 0 {
		active, err := processLeaseActive(ctx, lease, run)
		return !active, err
	}
	if poll <= 0 {
		poll = processLeasePollInterval
	}
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(poll)
	defer ticker.Stop()
	for {
		active, err := processLeaseActive(ctx, lease, run)
		if err != nil {
			return false, err
		}
		if !active {
			return true, nil
		}
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-deadline.C:
			return false, nil
		case <-ticker.C:
		}
	}
}

func processLeaseActive(ctx context.Context, lease ProcessLease, run processLeaseCommandRunner) (bool, error) {
	if err := validateProcessLease(lease); err != nil {
		return false, err
	}
	out, err := runProcessLeaseControl(ctx, lease, run, "show", "--property=ActiveState", "--value", lease.Unit)
	if err != nil {
		if processLeaseMissing(out, err) {
			return false, nil
		}
		return false, fmt.Errorf("inspect worker process lease %s: %w: %s", lease.Unit, err, strings.TrimSpace(string(out)))
	}
	switch strings.TrimSpace(string(out)) {
	case "active", "activating", "deactivating", "reloading":
		return true, nil
	case "", "inactive", "failed", "dead":
		return false, nil
	default:
		return false, fmt.Errorf("inspect worker process lease %s: unexpected active state %q", lease.Unit, strings.TrimSpace(string(out)))
	}
}

func runProcessLeaseControl(ctx context.Context, lease ProcessLease, run processLeaseCommandRunner, args ...string) ([]byte, error) {
	switch lease.Manager {
	case ProcessLeaseManagerSystem:
		return run(ctx, resolvedPath(sudoName), append([]string{"-n", resolvedPath(systemctlName)}, args...)...)
	case ProcessLeaseManagerUser:
		return run(ctx, resolvedPath(systemctlName), append([]string{"--user"}, args...)...)
	default:
		return nil, fmt.Errorf("unsupported worker process lease manager %q", lease.Manager)
	}
}

func processLeaseMissing(out []byte, err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(string(out) + " " + err.Error())
	for _, marker := range []string{"not loaded", "not found", "could not be found", "no such unit"} {
		if strings.Contains(message, marker) {
			return true
		}
	}
	return false
}

func resolvedPath(name string) string {
	if path, err := exec.LookPath(name); err == nil && strings.TrimSpace(path) != "" {
		return path
	}
	return name
}
