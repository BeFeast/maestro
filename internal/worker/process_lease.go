package worker

import (
	"fmt"
	"strings"
	"time"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/state"
	"github.com/befeast/maestro/internal/tmuxsession"
)

var (
	terminateWorkerProcessLease = tmuxsession.TerminateProcessLease
	workerProcessLeaseActive    = tmuxsession.ProcessLeaseActive
	killWorkerTmuxSession       = tmuxsession.KillSession
	workerTmuxSessionExists     = tmuxsession.HasSession
)

const (
	processLeaseTmuxExitWait = 500 * time.Millisecond
	processLeaseTmuxPoll     = 25 * time.Millisecond
)

func launchWorkerProcessLease(cfg *config.Config, slotName, tmuxName, worktree, runnerPath string, generation uint64, previousPID int) (int, tmuxsession.ProcessLease, error) {
	if cfg == nil {
		return 0, tmuxsession.ProcessLease{}, fmt.Errorf("launch worker process lease: nil config")
	}
	lease, err := workerProcessLease(cfg, slotName, generation)
	if err != nil {
		return 0, tmuxsession.ProcessLease{}, err
	}
	pid, err := startOrReconcileTmuxSession(tmuxName, worktree, runnerPath, lease, previousPID)
	if err == nil {
		return pid, lease, nil
	}

	// A failed or ambiguous start must not strand a partially-created cgroup.
	// startOrReconcile already adopts an exact matching tmux pane when creation
	// succeeded despite a transport error; reaching this branch means no owned
	// runtime was proven, so tear down only this newly-prepared lease.
	terminateErr := terminateWorkerProcessLease(lease)
	if terminateErr != nil {
		// Return the exact receipt to the caller so it can remain durable until a
		// later daemon confirms cleanup. Do not kill tmux by name here: the spawn
		// error may have come from a pre-existing same-name foreign session.
		return 0, lease, fmt.Errorf("%w (failed-start process lease cleanup: %v)", err, terminateErr)
	}
	return 0, tmuxsession.ProcessLease{}, err
}

func workerProcessLease(cfg *config.Config, slotName string, generation uint64) (tmuxsession.ProcessLease, error) {
	if cfg == nil {
		return tmuxsession.ProcessLease{}, fmt.Errorf("worker process lease: nil config")
	}
	return tmuxsession.WorkerProcessLease(processLeaseProjectIdentity(cfg), slotName, generation)
}

func processLeaseProjectIdentity(cfg *config.Config) string {
	if cfg == nil {
		return ""
	}
	if id := strings.TrimSpace(cfg.ProjectID); id != "" {
		return id
	}
	if stateDir := strings.TrimSpace(cfg.StateDir); stateDir != "" {
		return strings.TrimSpace(cfg.Repo) + "\x00" + stateDir
	}
	return strings.TrimSpace(cfg.Repo)
}

func setSessionProcessLease(sess *state.Session, lease tmuxsession.ProcessLease) {
	if sess == nil {
		return
	}
	sess.ProcessLeaseUnit = lease.Unit
	sess.ProcessLeaseManager = lease.Manager
}

func clearSessionProcessLease(sess *state.Session) {
	if sess == nil {
		return
	}
	sess.ProcessLeaseUnit = ""
	sess.ProcessLeaseManager = ""
}

func sessionProcessLease(sess *state.Session) (tmuxsession.ProcessLease, bool, error) {
	if sess == nil {
		return tmuxsession.ProcessLease{}, false, nil
	}
	unit := strings.TrimSpace(sess.ProcessLeaseUnit)
	manager := strings.TrimSpace(sess.ProcessLeaseManager)
	if unit == "" && manager == "" {
		return tmuxsession.ProcessLease{}, false, nil
	}
	if unit == "" || manager == "" {
		return tmuxsession.ProcessLease{}, true, fmt.Errorf("incomplete worker process lease metadata")
	}
	return tmuxsession.ProcessLease{Unit: unit, Manager: manager}, true, nil
}

func waitWorkerTmuxSessionGone(name string, timeout time.Duration) bool {
	name = strings.TrimSpace(name)
	if name == "" || !workerTmuxSessionExists(name) {
		return true
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		time.Sleep(processLeaseTmuxPoll)
		if !workerTmuxSessionExists(name) {
			return true
		}
	}
	return !workerTmuxSessionExists(name)
}
