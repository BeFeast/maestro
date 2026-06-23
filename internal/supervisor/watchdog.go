package supervisor

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/befeast/maestro/internal/state"
)

// Watchdog emits a loud log warning + persists SupervisorStuck=true in
// state.json when the supervise loop has not stamped state.LastRunOnceAt
// within 3*interval. It is a defense against silent wedges (#499).
//
// The ticker fires every interval (matching the supervise cadence so we
// don't over-poll the state file). On startup we grant a 3*interval grace
// before the first check, since LastRunOnceAt might be old after a daemon
// restart.
//
// One Watchdog runs per supervise loop: the single-project `maestro supervise`
// CLI starts one, and `maestro daemon` starts one per project flow (#756).
func Watchdog(ctx context.Context, stateDir string, interval time.Duration) {
	if interval <= 0 {
		return
	}
	stuckThreshold := 3 * interval
	startedAt := time.Now()

	tick := time.NewTicker(interval)
	defer tick.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}

		// Startup grace: don't warn before we've had time to see one
		// full cycle complete.
		if time.Since(startedAt) < stuckThreshold {
			continue
		}

		st, err := state.Load(stateDir)
		if err != nil {
			log.Printf("[supervise/watchdog] could not read state: %v (will retry)", err)
			continue
		}
		if st.LastRunOnceAt.IsZero() {
			// Daemon never completed a cycle. Loud warning, but only
			// after the startup grace.
			log.Printf("[supervise/watchdog] WARNING: no RunOnce has stamped state.LastRunOnceAt since the daemon started %s ago; check whether RunOnce is wedged",
				time.Since(startedAt).Round(time.Second))
			persistSupervisorStuck(stateDir, "no RunOnce has completed since daemon start")
			continue
		}
		gap := time.Since(st.LastRunOnceAt)
		if gap > stuckThreshold {
			reason := fmt.Sprintf("LastRunOnceAt=%s is %s old (threshold %s)",
				st.LastRunOnceAt.Format(time.RFC3339), gap.Round(time.Second), stuckThreshold)
			log.Printf("[supervise/watchdog] WARNING: supervise loop appears stuck — %s", reason)
			persistSupervisorStuck(stateDir, reason)
		}
	}
}

// persistSupervisorStuck flips SupervisorStuck=true in state.json so the
// Fleet API surfaces it. Best-effort: a save failure is logged but does
// not stop the watchdog.
func persistSupervisorStuck(stateDir, reason string) {
	st, err := state.Load(stateDir)
	if err != nil {
		log.Printf("[supervise/watchdog] persist: load state: %v", err)
		return
	}
	if st.SupervisorStuck && st.SupervisorStuckReason == reason {
		return // already set with the same reason; avoid log/save churn
	}
	st.SupervisorStuck = true
	st.SupervisorStuckReason = reason
	if err := state.Save(stateDir, st); err != nil {
		log.Printf("[supervise/watchdog] persist: save state: %v", err)
	}
}
