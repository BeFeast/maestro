package daemon

import (
	"context"
	"log"
	"time"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/github"
	"github.com/befeast/maestro/internal/supervisor"
)

// runSupervise is the per-project supervisor loop, extracted from the
// single-project `maestro supervise` command (cmd/maestro/main.go). The daemon
// runs one per flow.
//
// Key difference from the CLI: the first cycle is log-and-continue, NOT
// log.Fatalf. Under `systemctl start maestro@<project>`, a fatal first cycle
// made a broken setup fail loudly — correct for one unit per project. In the
// daemon a single broken project must not abort the process or the other
// flows, so every cycle (including the first) is logged and retried. Persistent
// failures still surface: the #499 watchdog flags SupervisorStuck when
// LastRunOnceAt stops advancing.
func runSupervise(ctx context.Context, cfg *config.Config, interval time.Duration) {
	gh := github.New(cfg.Repo)
	runOnce := func() error {
		_, err := supervisor.RunOnce(cfg, gh)
		return err
	}

	if err := runOnce(); err != nil {
		log.Printf("[%s] supervise: first cycle failed (will retry): %v", cfg.SessionPrefix, err)
	}

	if interval <= 0 {
		return
	}

	// Phase 1.2 (#499) liveness watchdog, one per flow.
	go supervisor.Watchdog(ctx, cfg.StateDir, interval)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// #689: a failed cycle is logged and retried on the next tick,
			// never fatal — a transient GitHub/decision-layer failure must
			// not crash the daemon.
			if err := runOnce(); err != nil {
				log.Printf("[%s] supervise: cycle failed (will retry in %s): %v", cfg.SessionPrefix, interval, err)
			}
		}
	}
}
