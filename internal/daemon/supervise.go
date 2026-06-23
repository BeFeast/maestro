package daemon

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/github"
	"github.com/befeast/maestro/internal/state"
	"github.com/befeast/maestro/internal/supervisor"
)

// runSupervise is the per-project supervisor loop, extracted from the
// single-project `maestro supervise` command (cmd/maestro/main.go). The daemon
// runs one per flow; the liveness watchdog is owned by startFlow so it joins
// the flow WaitGroup (#764 P2).
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
	// Capture and log each cycle's SupervisorDecision the way `maestro
	// supervise` prints it. The daemon still acts (label/comment/approve), but
	// without this the structural "why did the supervisor do that" trail was
	// dropped — turning debugging into journal archaeology (#764).
	runOnce := func() error {
		decision, err := supervisor.RunOnce(cfg, gh)
		if err != nil {
			return err
		}
		logSupervisorDecision(cfg, decision)
		return nil
	}

	if err := runOnce(); err != nil {
		log.Printf("[%s] supervise: first cycle failed (will retry): %v", cfg.SessionPrefix, err)
	}

	if interval <= 0 {
		return
	}

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

// logSupervisorDecision emits a compact, per-cycle record of the supervisor's
// decision so the daemon journal carries the same structural trail the CLI
// `maestro supervise` printed (#764). One line, greppable by project prefix.
func logSupervisorDecision(cfg *config.Config, decision state.SupervisorDecision) {
	parts := []string{fmt.Sprintf("action=%s", decision.RecommendedAction)}
	if decision.Status != "" {
		parts = append(parts, fmt.Sprintf("status=%s", decision.Status))
	}
	if decision.Risk != "" {
		parts = append(parts, fmt.Sprintf("risk=%s", decision.Risk))
	}
	parts = append(parts, fmt.Sprintf("confidence=%.2f", decision.Confidence))
	if decision.RequiresApproval {
		parts = append(parts, "requires_approval=true")
	}
	if decision.ApprovalID != "" {
		parts = append(parts, fmt.Sprintf("approval=%s", decision.ApprovalID))
	}
	if summary := strings.TrimSpace(decision.Summary); summary != "" {
		parts = append(parts, fmt.Sprintf("summary=%q", summary))
	}
	log.Printf("[%s] supervise decision: %s", cfg.SessionPrefix, strings.Join(parts, " "))
}
