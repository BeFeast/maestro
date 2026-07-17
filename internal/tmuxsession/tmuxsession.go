// Package tmuxsession owns Maestro's tmux control boundary.
//
// Workers use a private tmux server instead of the caller's default server.
// When Maestro itself runs as a systemd service, the first worker starts that
// server in a sibling transient scope.  The worker tree therefore survives a
// restart of maestro.service instead of remaining in its KillMode=control-group
// cgroup and being killed with the daemon.
package tmuxsession

import (
	"context"
	"fmt"
	"os"
	"os/exec"
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
	privateSocket  = "maestro-workers"
)

var startMu sync.Mutex

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
// scope.  Subsequent sessions are created by that already-isolated server.
func StartDetached(name, worktree, runnerPath string) ([]byte, error) {
	startMu.Lock()
	defer startMu.Unlock()

	args := []string{"new-session", "-d", "-s", name, "-c", worktree, bashPath, runnerPath}
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
	}
	if strings.TrimSpace(path) != "" {
		args = append(args, "--setenv=PATH="+path)
	}
	args = append(args, resolvedPath(tmuxName), "-L", privateSocket)
	return append(args, tmuxArgs...)
}

func resolvedPath(name string) string {
	if path, err := exec.LookPath(name); err == nil && strings.TrimSpace(path) != "" {
		return path
	}
	return name
}
