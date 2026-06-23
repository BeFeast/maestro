package daemon

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/orchestrator"
	"github.com/befeast/maestro/internal/server"
)

// projectFlow is one project running inside the daemon: an orchestrator loop, a
// supervisor loop, and (when supervising) a liveness watchdog sharing a child
// context. Cancelling that context (stopFlow) tears every goroutine down.
type projectFlow struct {
	name   string
	key    string // stable flow identity (StateDir/Repo); the flows-map key
	cfg    *config.Config
	cancel context.CancelFunc
	done   chan struct{} // closed once every flow goroutine has exited
}

// startFlow launches the orchestrator + supervisor loops (and the liveness
// watchdog) for proj under a child of parent and registers the flow. Each loop
// runs in its own goroutine with a panic recover so a single project's crash
// can never take the daemon (or the other flows) down (#756).
//
// Every goroutine the flow spawns is tracked in one WaitGroup, so close(done)
// — and therefore stopFlow — does not return until the watchdog has also
// exited. An untracked watchdog could otherwise Load/Save StateDir after the
// flow is considered stopped, racing a restarted flow for the same project
// (#764 P2).
func (d *Daemon) startFlow(parent context.Context, proj server.FleetProject) *projectFlow {
	cfg := proj.Cfg()
	fctx, cancel := context.WithCancel(parent)
	flow := &projectFlow{
		name:   proj.Name,
		key:    flowKey(cfg),
		cfg:    cfg,
		cancel: cancel,
		done:   make(chan struct{}),
	}

	// Register BEFORE launching any goroutine (including the reaper below), so
	// the reaper's deregister can never run ahead of the insert. A fast-failing
	// flow — a loop that panics or returns on its first cycle — could otherwise
	// have its reaper acquire d.mu, find no entry (insert hadn't happened yet),
	// skip the delete, and then the insert would re-add the now-dead flow
	// permanently: exactly the lingering-flow leak this path exists to prevent.
	d.mu.Lock()
	d.flows[flow.key] = flow
	d.mu.Unlock()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		defer recoverFlow(flow, "orchestrator")
		d.runLoop(fctx, cfg, d.opts)
	}()
	go func() {
		defer wg.Done()
		defer recoverFlow(flow, "supervise")
		// flow.name is the unique fleet display name; the supervise loop keys
		// its decision/cycle logs on it so two same-basename repos are
		// distinguishable in the journal, matching the watchdog (#764).
		d.superviseLoop(fctx, flow.name, cfg, d.opts)
	}()
	// Phase 1.2 (#499) liveness watchdog, one per flow — tracked in the flow
	// WaitGroup (#764 P2). The watchdog itself no-ops on a non-positive
	// interval; gate here too so we never spawn a goroutine that immediately
	// returns. The flow's fleet display name (unique across the daemon, #764)
	// is woven into the watchdog's log lines so an operator can tell whose
	// LastRunOnceAt went stale.
	if d.opts.SuperviseInterval > 0 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer recoverFlow(flow, "watchdog")
			d.watchdogLoop(fctx, flow.name, cfg.StateDir, d.opts.SuperviseInterval)
		}()
	}
	go func() {
		wg.Wait()
		close(flow.done)
		// Deregister once every goroutine has exited, so a flow that stopped on
		// its own — e.g. a panic that recoverFlow cancelled — does not linger in
		// the registry reporting as live. Guarded against a restart that reused
		// the same key (stopFlow may have already removed and replaced it).
		d.mu.Lock()
		if d.flows[flow.key] == flow {
			delete(d.flows, flow.key)
		}
		d.mu.Unlock()
	}()

	return flow
}

// stopFlow cancels a flow's context, waits for every flow goroutine to drain,
// and deregisters it. Keyed on the flow identity (flowKey). Safe to call for an
// unknown key (no-op).
func (d *Daemon) stopFlow(key string) {
	d.mu.Lock()
	flow, ok := d.flows[key]
	if ok {
		delete(d.flows, key)
	}
	d.mu.Unlock()
	if !ok {
		return
	}
	flow.cancel()
	<-flow.done
	log.Printf("[daemon] stopped flow %q", flow.name)
}

// stopAll drains every running flow. Called on daemon shutdown. It cancels
// every flow up front and only then waits, so the per-flow drains overlap and
// total shutdown latency is the slowest single flow — not the sum across flows.
// A serial cancel-then-wait would stack the drains, and each flow's first
// RunOnce is not ctx-cancellable mid-cycle, so the sum could be many minutes
// (#764).
func (d *Daemon) stopAll() {
	d.mu.Lock()
	flows := make([]*projectFlow, 0, len(d.flows))
	for _, flow := range d.flows {
		flows = append(flows, flow)
	}
	d.flows = make(map[string]*projectFlow)
	d.mu.Unlock()

	for _, flow := range flows {
		flow.cancel()
	}
	for _, flow := range flows {
		<-flow.done
		log.Printf("[daemon] stopped flow %q", flow.name)
	}
}

// recoverFlow swallows a panic in a flow goroutine so it stays contained to
// that project (a panicking goroutine would otherwise crash the whole daemon),
// and cancels the flow so its OTHER goroutines drain too. Without the cancel a
// panic in one loop would leave the flow half-alive — the surviving loops still
// running and the flow still registered as healthy — while the log claimed it
// stopped (#764).
func recoverFlow(flow *projectFlow, loop string) {
	if r := recover(); r != nil {
		log.Printf("[daemon] flow %q %s loop panicked (contained, cancelling flow): %v", flow.name, loop, r)
		flow.cancel()
	}
}

// runOrchestrator is the production run loop for one flow. It mirrors the
// single-project branch of `maestro run` minus the per-project HTTP server
// (the daemon serves one shared FleetServer instead) and minus the fatal exit
// on run error — a failing orchestrator must log-and-continue, never take the
// daemon down (#756). orchestrator.Run already log-and-continues on per-cycle
// errors and returns nil on ctx cancellation.
//
// Hot-reload note (#764 P3): the daemon's source of truth is the config store
// (#754), not the import-time YAML, so we deliberately do NOT wire a YAML file
// watcher here — watching a file the store supersedes would let the run loop
// drift from the supervise loop, which has no such watcher. Store-driven reload
// is Phase 2 (#757).
func runOrchestrator(ctx context.Context, cfg *config.Config, opts Options) {
	orch := orchestrator.New(cfg)
	orch.SetBinaryVersion(opts.Version)
	if err := orch.LoadPromptBase(opts.PromptPath); err != nil {
		log.Printf("[%s] warn: load prompt: %v", cfg.SessionPrefix, err)
	}

	runInterval := opts.RunInterval
	if cfg.PollIntervalSeconds > 0 {
		runInterval = time.Duration(cfg.PollIntervalSeconds) * time.Second
	}

	// The daemon has no API-triggered refresh channel: the per-project HTTP
	// server that drove one in `maestro run` is replaced by the shared
	// FleetServer, which does not signal individual flows. Pass nil rather than
	// a buffered channel nothing ever sends on, so orch.Run's `case <-refreshCh`
	// arm is plainly inert instead of looking like a wired-but-dead feature
	// (#764). Store-driven reload is Phase 2 (#757).
	log.Printf("[%s] starting orchestrator — repo=%s interval=%s", cfg.SessionPrefix, cfg.Repo, runInterval)
	if err := orch.Run(ctx, runInterval, false, nil); err != nil {
		log.Printf("[%s] orchestrator exited: %v", cfg.SessionPrefix, err)
	}
}
