package daemon

import (
	"context"
	"fmt"
	"log"
	"math/rand"
	"strings"
	"time"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/github"
	"github.com/befeast/maestro/internal/state"
	"github.com/befeast/maestro/internal/supervisor"
)

// superviseJitterCap bounds the random first-cycle phase offset so a freshly
// (re)started daemon still runs its first supervise cycle promptly on long
// poll intervals (#794).
const superviseJitterCap = 60 * time.Second

// superviseJitterFrac yields a random fraction in [0,1); a package var so tests
// can make the offset deterministic.
var superviseJitterFrac = rand.Float64

// superviseStartupJitter returns a random phase offset in
// [0, min(interval, cap)) used to de-synchronize this flow's supervise GitHub
// reads from the rest of the fleet — and the ticker anchored to the first
// cycle — so the flows do not burst on the shared PAT in one window (#794).
// Returns 0 for a non-positive interval.
func superviseStartupJitter(interval time.Duration) time.Duration {
	return computeSuperviseJitter(interval, superviseJitterFrac())
}

// computeSuperviseJitter is the pure phase-offset calculation, split out so it
// is unit testable without touching the rng.
func computeSuperviseJitter(interval time.Duration, frac float64) time.Duration {
	if interval <= 0 {
		return 0
	}
	span := interval
	if span > superviseJitterCap {
		span = superviseJitterCap
	}
	return time.Duration(frac * float64(span))
}

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
//
// getCfg yields the flow's CURRENT config: the loop reads it once per cycle so
// a live config-store edit (supervisor policy, labels, …) is applied without a
// flow restart (#768). The GitHub client is built once from the startup repo —
// repo is a restart-required field the orchestrator refuses to hot-apply, so it
// is stable for the flow's lifetime.
func runSupervise(ctx context.Context, name string, getCfg func() *config.Config, interval time.Duration) {
	gh := github.New(getCfg().Repo)
	// Capture and log each cycle's SupervisorDecision. The daemon still acts
	// (label/comment/approve), but without this the structural "why did the
	// supervisor do that" trail was dropped — turning debugging into journal
	// archaeology (#764). Logs key on the unique fleet name (not the possibly
	// shared session prefix) so two same-basename repos stay distinguishable.
	runOnce := func() error {
		decision, err := supervisor.RunOnce(getCfg(), gh)
		if err != nil {
			return err
		}
		logSupervisorDecision(name, decision)
		return nil
	}

	// #794: de-phase this flow's supervise reads from the rest of the fleet.
	// Like the orchestrator loop, the first runOnce anchors the ticker, so a
	// random phase offset here spreads the supervise GitHub reads across the
	// poll interval instead of bursting in lockstep with the other flows on the
	// shared PAT. Only the looping (interval > 0) path jitters — a one-shot
	// cycle runs immediately. The wait honors ctx so shutdown is not delayed.
	if interval > 0 {
		if d := superviseStartupJitter(interval); d > 0 {
			log.Printf("[%s] supervise: startup jitter — first cycle in %s", name, d.Round(time.Second))
			t := time.NewTimer(d)
			select {
			case <-ctx.Done():
				t.Stop()
				return
			case <-t.C:
			}
		}
	}

	if err := runOnce(); err != nil {
		log.Printf("[%s] supervise: first cycle failed (will retry): %v", name, err)
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
				log.Printf("[%s] supervise: cycle failed (will retry in %s): %v", name, interval, err)
			}
		}
	}
}

// logSupervisorDecision emits a compact, single-line, per-cycle record of the
// supervisor's decision so the daemon journal keeps the structural "why" trail
// (#764). It is a one-line, greppable digest — action, status, risk,
// confidence, error_class, approval, target, and a reasons count — not the
// CLI's full multi-line dump (`printSupervisorDecision` also expands every
// reason, the queue analysis, and each mutation); the count + summary point an
// operator at the right cycle, and `maestro supervise --json` remains the
// source for the full record. Keyed on the unique fleet name passed in.
func logSupervisorDecision(name string, decision state.SupervisorDecision) {
	parts := []string{fmt.Sprintf("action=%s", decision.RecommendedAction)}
	if decision.Status != "" {
		parts = append(parts, fmt.Sprintf("status=%s", decision.Status))
	}
	if decision.Risk != "" {
		parts = append(parts, fmt.Sprintf("risk=%s", decision.Risk))
	}
	parts = append(parts, fmt.Sprintf("confidence=%.2f", decision.Confidence))
	if decision.ErrorClass != "" {
		parts = append(parts, fmt.Sprintf("error_class=%s", decision.ErrorClass))
	}
	if decision.RequiresApproval {
		parts = append(parts, "requires_approval=true")
	}
	if decision.ApprovalID != "" {
		parts = append(parts, fmt.Sprintf("approval=%s", decision.ApprovalID))
	}
	if tgt := supervisorTargetLabel(decision.Target); tgt != "" {
		parts = append(parts, fmt.Sprintf("target=%s", tgt))
	}
	if n := len(decision.Reasons); n > 0 {
		parts = append(parts, fmt.Sprintf("reasons=%d", n))
	}
	if summary := strings.TrimSpace(decision.Summary); summary != "" {
		parts = append(parts, fmt.Sprintf("summary=%q", summary))
	}
	log.Printf("[%s] supervise decision: %s", name, strings.Join(parts, " "))
}

// supervisorTargetLabel renders a decision target compactly (issue/PR/session)
// for the one-line journal digest. Empty when there is no target.
func supervisorTargetLabel(t *state.SupervisorTarget) string {
	if t == nil {
		return ""
	}
	var b []string
	if t.Issue > 0 {
		b = append(b, fmt.Sprintf("issue#%d", t.Issue))
	}
	if t.PR > 0 {
		b = append(b, fmt.Sprintf("pr#%d", t.PR))
	}
	if s := strings.TrimSpace(t.Session); s != "" {
		b = append(b, "session="+s)
	}
	return strings.Join(b, "/")
}
