package supervisor

import (
	"context"
	"fmt"
	"log"
	"strings"
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
// CLI starts one, and `maestro daemon` starts one per project flow (#756). The
// name (project / session prefix) is woven into every log line so that, with N
// flows in one daemon, an operator can tell whose LastRunOnceAt went stale
// (#764) instead of reading N identical `[supervise/watchdog]` prefixes.
//
// When kickCh is non-nil and the watchdog observes 2+ consecutive stuck ticks
// (warn + confirmation) it sends a kick signal so the supervise loop can
// self-heal by aborting the in-flight RunOnce and starting a fresh cycle
// (#816).
func Watchdog(ctx context.Context, name, stateDir string, interval time.Duration, kickCh chan<- struct{}) {
	if interval <= 0 {
		return
	}
	logPrefix := "supervise/watchdog"
	if name = strings.TrimSpace(name); name != "" {
		logPrefix = name + " supervise/watchdog"
	}
	stuckThreshold := 3 * interval
	startedAt := time.Now()

	tick := time.NewTicker(interval)
	defer tick.Stop()

	var consecutiveStuck int

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
			log.Printf("[%s] could not read state: %v (will retry)", logPrefix, err)
			continue
		}
		if st.LastRunOnceAt.IsZero() {
			// Daemon never completed a cycle. Loud warning, but only
			// after the startup grace.
			log.Printf("[%s] WARNING: no RunOnce has stamped state.LastRunOnceAt since the daemon started %s ago; check whether RunOnce is wedged",
				logPrefix, time.Since(startedAt).Round(time.Second))
			persistSupervisorStuck(logPrefix, stateDir, "no RunOnce has completed since daemon start")
			consecutiveStuck++
			tryKick(logPrefix, kickCh, consecutiveStuck)
			continue
		}
		gap := time.Since(st.LastRunOnceAt)
		if gap > stuckThreshold {
			reason := fmt.Sprintf("LastRunOnceAt=%s is %s old (threshold %s)",
				st.LastRunOnceAt.Format(time.RFC3339), gap.Round(time.Second), stuckThreshold)
			log.Printf("[%s] WARNING: supervise loop appears stuck — %s", logPrefix, reason)
			persistSupervisorStuck(logPrefix, stateDir, reason)
			consecutiveStuck++
			tryKick(logPrefix, kickCh, consecutiveStuck)
		} else {
			consecutiveStuck = 0
		}
	}
}

// tryKick sends a kick signal on kickCh when consecutiveStuck >= 2,
// so the supervise loop can self-heal by aborting its in-flight RunOnce
// and starting a fresh cycle. Non-blocking send so the watchdog never
// blocks if the supervise loop hasn't drained the previous kick.
func tryKick(logPrefix string, kickCh chan<- struct{}, consecutiveStuck int) {
	if kickCh == nil || consecutiveStuck < 2 {
		return
	}
	log.Printf("[%s] kick: supervise loop is wedged — sending recovery signal (consecutive stuck ticks=%d)", logPrefix, consecutiveStuck)
	select {
	case kickCh <- struct{}{}:
	default:
	}
}

// persistSupervisorStuck flips SupervisorStuck=true in state.json so the
// Fleet API surfaces it. Best-effort: a save failure is logged but does
// not stop the watchdog.
func persistSupervisorStuck(logPrefix, stateDir, reason string) {
	st, err := state.Load(stateDir)
	if err != nil {
		log.Printf("[%s] persist: load state: %v", logPrefix, err)
		return
	}
	if st.SupervisorStuck && st.SupervisorStuckReason == reason {
		return // already set with the same reason; avoid log/save churn
	}
	st.SupervisorStuck = true
	st.SupervisorStuckReason = reason
	if err := state.Save(stateDir, st); err != nil {
		log.Printf("[%s] persist: save state: %v", logPrefix, err)
	}
}
