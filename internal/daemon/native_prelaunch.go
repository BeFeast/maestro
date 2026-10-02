package daemon

import (
	"fmt"
	"github.com/befeast/maestro/internal/orchestrator"
)

func validateNativePrelaunchProjects(projects []namedConfig, requests []orchestrator.NativePrelaunchRecovery) error {
	for _, request := range requests {
		matches := 0
		for _, project := range projects {
			if project.cfg != nil && project.cfg.ProjectID == request.ProjectID {
				matches++
			}
		}
		if matches != 1 {
			return fmt.Errorf("native prelaunch recovery project must match exactly one selected config row")
		}
	}
	return nil
}

// Remove exact-project startup decisions under the daemon lock. A restarted
// project flow in this daemon cannot turn the same operator flag into a loop.
func (d *Daemon) takeNativePrelaunchRecoveries(projectID string) []orchestrator.NativePrelaunchRecovery {
	d.mu.Lock()
	defer d.mu.Unlock()
	var taken, remaining []orchestrator.NativePrelaunchRecovery
	for _, request := range d.nativePrelaunchRecoveries {
		if request.ProjectID == projectID {
			taken = append(taken, request)
		} else {
			remaining = append(remaining, request)
		}
	}
	d.nativePrelaunchRecoveries = remaining
	return taken
}
