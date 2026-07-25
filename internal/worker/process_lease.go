package worker

import (
	"fmt"
	"strings"
	"time"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/state"
	"github.com/befeast/maestro/internal/tmuxsession"
	"github.com/befeast/maestro/internal/workerlease"
)

var (
	terminateWorkerProcessLease = tmuxsession.TerminateProcessLease
	workerProcessLeaseActive    = tmuxsession.ProcessLeaseActive
	workerProcessLeaseAnchored  = tmuxsession.ProcessLeaseAnchoredAtPID
	confirmWorkerProcessLease   = waitWorkerProcessLeaseReady
	killWorkerTmuxSession       = tmuxsession.KillSession
	workerTmuxSessionExists     = tmuxsession.HasSession
	workerScratchCleanup        = workerlease.CleanupManifest
)

const (
	processLeaseStartWait    = 2 * time.Second
	processLeaseTmuxExitWait = 500 * time.Millisecond
	processLeaseTmuxPoll     = 25 * time.Millisecond
)

func launchWorkerProcessLease(cfg *config.Config, slotName, tmuxName, worktree, runnerPath string, generation uint64, previousPID int, reason string) (int, tmuxsession.ProcessLease, error) {
	if cfg == nil {
		return 0, tmuxsession.ProcessLease{}, fmt.Errorf("launch worker process lease: nil config")
	}
	lease, err := workerProcessLease(cfg, slotName, generation)
	if err != nil {
		return 0, tmuxsession.ProcessLease{}, err
	}
	scratchLease, runtime, err := prepareWorkerScratchLease(cfg, slotName, reason, lease)
	if err != nil {
		return 0, tmuxsession.ProcessLease{}, err
	}
	lease.Runtime = runtime
	pid, err := startOrReconcileTmuxSession(tmuxName, worktree, runnerPath, lease, previousPID)
	if err == nil {
		ready, readyErr := confirmWorkerProcessLease(lease, pid, processLeaseStartWait)
		switch {
		case readyErr != nil:
			err = fmt.Errorf("confirm worker process lease %s ownership: %w", lease.Unit, readyErr)
		case !ready:
			err = fmt.Errorf("worker process lease %s did not own pane pid %d before startup deadline", lease.Unit, pid)
		default:
			return pid, lease, nil
		}
	}

	// A failed or ambiguous start must not strand a partially-created cgroup.
	// startOrReconcile already adopts an exact matching tmux pane when creation
	// succeeded despite a transport error; reaching this branch means no owned
	// runtime was proven, so tear down only this newly-prepared lease.
	terminateErr := terminateWorkerProcessLease(lease)
	cleanupErr := error(nil)
	if terminateErr == nil && scratchLease != nil {
		cleanupErr = workerScratchCleanup(scratchLease.ManifestPath, scratchLease.ID)
	}
	if terminateErr != nil || cleanupErr != nil {
		// Return the exact receipt to the caller so it can remain durable until a
		// later daemon confirms cleanup. Do not kill tmux by name here: the spawn
		// error may have come from a pre-existing same-name foreign session.
		return 0, lease, fmt.Errorf("%w (failed-start lease cleanup: process=%v scratch=%v)", err, terminateErr, cleanupErr)
	}
	return 0, tmuxsession.ProcessLease{}, err
}

func waitWorkerProcessLeaseReady(lease tmuxsession.ProcessLease, pid int, timeout time.Duration) (bool, error) {
	deadline := time.Now().Add(timeout)
	var ownershipErr error
	for {
		active, err := workerProcessLeaseActive(lease)
		if err != nil {
			return false, err
		}
		if active {
			owned, err := workerProcessLeaseAnchored(lease, pid)
			if err == nil && owned {
				return true, nil
			}
			ownershipErr = err
		}
		if !time.Now().Before(deadline) {
			return false, ownershipErr
		}
		time.Sleep(processLeaseTmuxPoll)
	}
}

func workerProcessLease(cfg *config.Config, slotName string, generation uint64) (tmuxsession.ProcessLease, error) {
	if cfg == nil {
		return tmuxsession.ProcessLease{}, fmt.Errorf("worker process lease: nil config")
	}
	if cfg.WorkerRuntime.IsolatedEnabled() {
		return tmuxsession.WorkerProcessServiceLease(processLeaseProjectIdentity(cfg), slotName, generation, cfg.WorkerRuntime.EffectiveScope())
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
	if lease.Runtime != nil {
		receipt := workerScratchLeaseReceipt(lease)
		assignWorkerLease(sess, &receipt)
	}
}

func workerScratchLeaseReceipt(lease tmuxsession.ProcessLease) workerlease.Lease {
	runtime := lease.Runtime
	if runtime == nil {
		return workerlease.Lease{}
	}
	return workerlease.Lease{
		ID: runtime.ScratchID, Unit: lease.Unit, Scope: lease.Manager,
		ScratchDir: runtime.ScratchDir, TempDir: runtime.TempDir, GoTempDir: runtime.GoTempDir,
		CargoTarget: runtime.CargoTarget, ManifestPath: runtime.ManifestPath,
	}
}

func attachWorkerScratchReceipt(lease *tmuxsession.ProcessLease, scratch *workerlease.Lease) {
	if lease == nil || scratch == nil {
		return
	}
	lease.Runtime = &tmuxsession.ProcessLeaseRuntime{
		ScratchID: scratch.ID, ScratchDir: scratch.ScratchDir, TempDir: scratch.TempDir,
		GoTempDir: scratch.GoTempDir, CargoTarget: scratch.CargoTarget, ManifestPath: scratch.ManifestPath,
	}
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
