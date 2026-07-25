package orphan

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"syscall"
	"time"
)

// OSActuator is the real host Actuator: it terminates a wrapper process group
// with a signal to the negative pgid and removes an exited container with
// `docker rm`. The runtime injects it into a Reaper; unit tests use a fake so
// they never touch real processes or Docker.
//
// It is intentionally conservative:
//
//   - TerminateGroup sends SIGTERM to the whole group, waits a bounded grace
//     period for the group to exit, then escalates to SIGKILL only if it is
//     still present. A process group that has already exited is a success.
//   - RemoveContainer only ever removes; it never force-stops a running
//     container. The Decide plan already guarantees the container is terminal,
//     and `docker rm` of an already-absent container is treated as success.
//
// This type is complete and reviewable, but wiring it into the running daemon
// loop plus live host verification is a separate operator step (#977): reaping
// real processes and containers must be confirmed against a live host before it
// is armed automatically.
type OSActuator struct {
	// KillGrace is how long to wait after SIGTERM before escalating to SIGKILL.
	// Zero uses a 5s default.
	KillGrace time.Duration
	// DockerArgs is prepended to `rm <name>` so an operator can route removal
	// through `sudo docker` when the wrappers ran under sudo. Default: {"docker"}.
	DockerArgs []string
	// killGroup / removeContainer are indirections for tests of OSActuator
	// itself; production leaves them nil and the real implementations are used.
	killGroup       func(pgid int, sig syscall.Signal) error
	groupAlive      func(pgid int) bool
	removeContainer func(ctx context.Context, args []string) error
}

// TerminateGroup signals the whole process group, escalating SIGTERM -> SIGKILL.
func (a *OSActuator) TerminateGroup(pgid int, pids []int) error {
	if pgid <= 1 {
		// Refuse to signal pgid 0/1: 0 would target the caller's own group and 1
		// is init. An orphan wrapper always has its own real group id.
		return fmt.Errorf("refusing to signal unsafe process group %d", pgid)
	}
	kill := a.killGroup
	if kill == nil {
		kill = func(pg int, sig syscall.Signal) error { return syscall.Kill(-pg, sig) }
	}
	alive := a.groupAlive
	if alive == nil {
		alive = func(pg int) bool { return syscall.Kill(-pg, 0) == nil }
	}

	if err := kill(pgid, syscall.SIGTERM); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return nil // already gone
		}
		return fmt.Errorf("sigterm group: %w", err)
	}

	grace := a.KillGrace
	if grace <= 0 {
		grace = 5 * time.Second
	}
	deadline := time.Now().Add(grace)
	for time.Now().Before(deadline) {
		if !alive(pgid) {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !alive(pgid) {
		return nil
	}
	if err := kill(pgid, syscall.SIGKILL); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return nil
		}
		return fmt.Errorf("sigkill group: %w", err)
	}
	return nil
}

// RemoveContainer removes the exited named container via `docker rm`.
func (a *OSActuator) RemoveContainer(name string) error {
	if name == "" {
		return errors.New("empty container name")
	}
	base := a.DockerArgs
	if len(base) == 0 {
		base = []string{"docker"}
	}
	args := append(append([]string(nil), base...), "rm", name)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	run := a.removeContainer
	if run == nil {
		run = func(ctx context.Context, args []string) error {
			cmd := exec.CommandContext(ctx, args[0], args[1:]...)
			return cmd.Run()
		}
	}
	if err := run(ctx, args); err != nil {
		return fmt.Errorf("docker rm: %w", err)
	}
	return nil
}

var _ Actuator = (*OSActuator)(nil)
