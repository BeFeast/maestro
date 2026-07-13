package worker

import (
	"log"
	"os"
	"os/exec"
	"strings"
)

// spawn.go centralizes how a worker's detached tmux session is launched.
//
// The bug (#877): `maestro daemon` runs as maestro.service; each worker's tmux
// server is started with `exec.Command("tmux", "new-session", ...)`, so — being
// a descendant of the daemon — it lands in maestro.service's control group. On
// a self-deploy the deploy script runs `systemctl restart maestro.service`;
// with the unit's kill semantics that restart terminated the WHOLE cgroup —
// tmux server and in-flight Claude workers included — even while the daemon's
// in-process drain still reported those workers running, and the next daemon
// marked them running->dead over surviving dirty worktrees.
//
// The fix: when running under systemd, launch the tmux server inside a transient
// `systemd-run --user --scope` cgroup. A scope unit is a SIBLING of
// maestro.service, not a child, so a restart of maestro.service no longer sweeps
// the worker out — the worker (and its dirty worktree) spans the deploy and the
// next daemon re-attaches to the still-live pane. This makes permanent the
// operator's manual workaround (resume the run scripts in a tmux server outside
// the service cgroup). On a host without a usable user manager the scope launch
// degrades to a bare in-cgroup tmux launch (protected in turn by the unit's
// KillMode=process), so worker spawning never hard-fails on scope unavailability.

// tmuxNewSessionArgv is the bare argv that starts a worker's detached tmux
// session running runnerPath (via bash) in workdir.
func tmuxNewSessionArgv(tmuxName, workdir, runnerPath string) []string {
	return []string{"tmux", "new-session", "-d", "-s", tmuxName, "-c", workdir, "bash", runnerPath}
}

// scopeWrapArgv wraps a command argv in `systemd-run --user --scope` so the
// process it starts — here the detached tmux server — is placed in a transient
// scope cgroup that is a sibling of maestro.service rather than a child of it
// (#877). --collect unloads the scope once it empties (the last worker exits);
// the `--` terminates systemd-run's own option parsing so the wrapped argv is
// passed through verbatim.
func scopeWrapArgv(argv []string) []string {
	wrap := []string{
		"systemd-run", "--user", "--scope", "--collect",
		"--description=maestro worker tmux server (out-of-cgroup, #877)",
		"--",
	}
	return append(wrap, argv...)
}

// runUnderSystemdScope reports whether worker tmux servers should be launched in
// a transient systemd scope outside the daemon's service cgroup. True only when
// the daemon is running inside a systemd unit (systemd sets INVOCATION_ID for a
// unit's processes) AND `systemd-run` is on PATH. Otherwise a bare tmux launch
// is used — non-systemd hosts, CI, and tests, where the cgroup hazard does not
// exist. MAESTRO_TMUX_NO_SCOPE=1 force-disables the scope path as an operator
// escape hatch. Indirected through a var so tests can pin the decision.
var runUnderSystemdScope = func() bool {
	if os.Getenv("MAESTRO_TMUX_NO_SCOPE") == "1" {
		return false
	}
	if os.Getenv("INVOCATION_ID") == "" {
		return false
	}
	_, err := exec.LookPath("systemd-run")
	return err == nil
}

// execTmuxCombined runs argv and returns its combined output. Indirected through
// a var so tests can observe the exact argv and simulate launch failures without
// spawning a real tmux/systemd-run.
var execTmuxCombined = func(argv []string) ([]byte, error) {
	return exec.Command(argv[0], argv[1:]...).CombinedOutput()
}

// LaunchTmuxSession starts a worker's detached tmux session running runnerPath
// in workdir. Under systemd it launches the tmux server inside a transient
// `systemd-run --user --scope` cgroup so a `systemctl restart` of maestro.service
// (self-deploy) does not sweep the in-flight worker out with the daemon (#877).
// If the scope launch fails (e.g. no user manager / DBus in the daemon's
// environment) it clears any partially-created session and falls back to a bare
// in-cgroup tmux launch, so worker spawning still succeeds — it just won't
// survive a service restart on that host. Returns the launcher's combined output
// (for error context) and error.
func LaunchTmuxSession(tmuxName, workdir, runnerPath string) ([]byte, error) {
	base := tmuxNewSessionArgv(tmuxName, workdir, runnerPath)
	if !runUnderSystemdScope() {
		return execTmuxCombined(base)
	}
	out, err := execTmuxCombined(scopeWrapArgv(base))
	if err == nil {
		return out, nil
	}
	// The scope launch failed. systemd-run typically errors before exec (no
	// user manager / DBus), so the session usually was never created — but be
	// defensive: clear any partial session so the fallback new-session cannot
	// fail on a duplicate name, then retry in-cgroup.
	log.Printf("[worker] systemd-run scope launch of tmux session %s failed (%v); falling back to in-cgroup launch — this worker will NOT survive a service restart on this host: %s",
		tmuxName, err, strings.TrimSpace(string(out)))
	_, _ = execTmuxCombined([]string{"tmux", "kill-session", "-t", tmuxName})
	return execTmuxCombined(base)
}
