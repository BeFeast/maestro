package daemon

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/configwatch"
	"github.com/befeast/maestro/internal/github"
	"github.com/befeast/maestro/internal/orchestrator"
	"github.com/befeast/maestro/internal/server"
)

// projectFlow is one project running inside the daemon: an orchestrator loop, a
// supervisor loop, and (when supervising) a liveness watchdog sharing a child
// context. Cancelling that context (stopFlow) tears every goroutine down.
type projectFlow struct {
	name      string
	key       string         // stable flow identity (StateDir/Repo); the flows-map key
	storeName string         // config-store project name; "" when not store-backed (#757)
	cfg       *config.Config // startup config; identity (key) + logging source
	// holder is the flow's current config, swapped by the reload pump on every
	// config-store edit. The supervise loop reads it each cycle and the
	// dashboard snapshot is rebuilt from it, so a live edit reaches all three
	// readers — supervisor, dashboard, orchestrator — race-free (#768).
	holder *configHolder
	cancel context.CancelFunc
	done   chan struct{} // closed once every flow goroutine has exited
	// refreshCh coalesces webhook gate updates into immediate orchestrator
	// reconciliations for this repository. One buffered wake-up is sufficient:
	// a RunOnce reads the complete current PR head/check rollup.
	refreshCh chan struct{}
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
func (d *Daemon) startFlow(parent context.Context, storeName string, proj server.FleetProject) *projectFlow {
	cfg := proj.Cfg()
	if cfg != nil {
		cfg.RuntimeSuperviseIntervalSeconds = runtimeIntervalSeconds(d.opts.SuperviseInterval)
		if d.spawnLimiter != nil {
			d.spawnLimiter.RegisterProject(cfg.StateDir, cfg.Repo, d.opts.SuperviseInterval)
		}
	}
	fctx, cancel := context.WithCancel(parent)
	flow := &projectFlow{
		name:      proj.Name,
		key:       flowKey(cfg),
		storeName: storeName,
		cfg:       cfg,
		holder:    newConfigHolder(cfg),
		cancel:    cancel,
		done:      make(chan struct{}),
		refreshCh: make(chan struct{}, 1),
	}

	// Per-flow hot-reload (#757, #768): when store-watch is enabled and this
	// flow is store-backed, watch the project's row so an operator editing the
	// config (config-store edit) is applied live, without restarting the flow or
	// disturbing the rest of the fleet. The watcher is a child of fctx, so
	// stopFlow drains it.
	//
	// The raw watcher channel feeds a single reload PUMP (runReloadPump) — not
	// the orchestrator directly — because a config edit must reach all three
	// readers: the supervise loop and dashboard (via the holder, #768) and the
	// orchestrator (via orchReloadCh, which keeps its selective reloadConfig +
	// restart-required detection). One channel can have only one consumer, so
	// the pump fans out. A nil watcher leaves orchReloadCh nil too, so the
	// orchestrator's reload select arm stays inert.
	var watchCh <-chan *config.Config
	var orchReloadCh chan *config.Config
	if d.projectStore != nil && storeName != "" {
		watchCh = configwatch.WatchStore(fctx, d.projectStore, storeName, d.opts.WatchStoreInterval)
		orchReloadCh = make(chan *config.Config, 1)
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
		// orchReloadCh (fed by the pump) is typed as <-chan; a nil channel
		// leaves the orchestrator's reload arm inert (store-watch disabled).
		flowOpts := d.opts
		flowOpts.refreshCh = flow.refreshCh
		d.runLoop(fctx, cfg, flowOpts, orchReloadCh)
	}()
	go func() {
		defer wg.Done()
		defer recoverFlow(flow, "supervise")
		// flow.name is the unique fleet display name; the supervise loop keys
		// its decision/cycle logs on it so two same-basename repos are
		// distinguishable in the journal, matching the watchdog (#764). It reads
		// the flow's CURRENT config through the holder each cycle (#768).
		d.superviseLoop(fctx, flow.name, flow.holder.Load, d.opts)
	}()
	// Reload pump (#768): consume the store watcher and fan a config-store edit
	// out to the holder (supervisor + dashboard) and the orchestrator. Tracked
	// in the flow WaitGroup so stopFlow drains it before the flow is considered
	// stopped. Only started when store-watch wired a channel.
	if watchCh != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer recoverFlow(flow, "config-reload")
			d.runReloadPump(fctx, flow, watchCh, orchReloadCh)
		}()
	}
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
	// #887: the material-progress evaluator owns a separate per-project clock.
	// It must keep evaluating when Engine.Decide, GitHub, or the supervisor LLM
	// fails, so it is a sibling of the supervise loop rather than a callback from
	// RunOnce. It reads the live config holder on every wake-up and participates
	// in the flow WaitGroup for race-free stop/restart.
	if d.materialProgressLoop != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer recoverFlow(flow, "material-progress-watchdog")
			d.materialProgressLoop(fctx, flow.name, flow.holder.Load)
		}()
	}
	// Mirror reconciliation loop (#827, phase E): a low-frequency GitHub snapshot
	// that repairs mirror drift a missed webhook left behind, keeping the read
	// model correct without operator action. Only started when the daemon has an
	// open mirror (webhook ingestion configured) — without inbound deliveries there
	// is nothing to reconcile. Tracked in the flow WaitGroup so stopFlow drains it,
	// and reads its cadence live from the holder (a config-store edit retimes it).
	if d.mirror != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer recoverFlow(flow, "mirror-reconcile")
			d.runMirrorReconcile(fctx, flow.name, cfg.Repo, flow.holder.Load)
		}()
	}
	go func() {
		wg.Wait()
		close(flow.done)
		// Deregister once every goroutine has exited, so a flow that stopped on
		// its own — e.g. a panic that recoverFlow cancelled — does not linger in
		// the registry reporting as live. Guarded against a restart that reused
		// the same key (stopFlow may have already removed and replaced it).
		removed := false
		d.mu.Lock()
		if d.flows[flow.key] == flow {
			delete(d.flows, flow.key)
			removed = true
		}
		d.mu.Unlock()
		if removed && d.spawnLimiter != nil && flow.cfg != nil {
			d.spawnLimiter.UnregisterStateDir(flow.cfg.StateDir)
		}
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
	if d.spawnLimiter != nil && flow.cfg != nil {
		d.spawnLimiter.UnregisterStateDir(flow.cfg.StateDir)
	}
	log.Printf("[daemon] stopped flow %q", flow.name)
}

// stopAll drains every running flow. Called on daemon shutdown. It cancels
// every flow up front and only then waits, so the per-flow drains overlap and
// total shutdown latency is the slowest single flow — not the sum across flows.
// A serial cancel-then-wait would stack the drains, and each flow's first
// RunOnce is not ctx-cancellable mid-cycle, so the sum could be many minutes
// (#764).
func (d *Daemon) stopAll() {
	d.stopAllUntil(time.Now().Add(shutdownFlowTimeout))
}

// stopAllUntil cancels every flow and waits for them concurrently until the
// earlier of shutdownFlowTimeout or the global shutdown deadline. A non-returning
// flow is logged and detached from daemon shutdown; it cannot serialize another
// per-flow timeout or hold the systemd restart job (#966).
func (d *Daemon) stopAllUntil(deadline time.Time) {
	// Covers the teardown paths where Drain never ran (e.g. a serve error):
	// gh reads fail fast on rate limits instead of blocking flow stop (#797).
	github.BeginShutdown()

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
	if len(flows) == 0 {
		return
	}

	cutoff := time.Now().Add(shutdownFlowTimeout)
	if !deadline.IsZero() && deadline.Before(cutoff) {
		cutoff = deadline
	}
	type stoppedFlow struct{ flow *projectFlow }
	stopped := make(chan stoppedFlow, len(flows))
	pending := make(map[*projectFlow]struct{}, len(flows))
	for _, flow := range flows {
		pending[flow] = struct{}{}
		go func(f *projectFlow) {
			<-f.done
			stopped <- stoppedFlow{flow: f}
		}(flow)
	}

	remaining := time.Until(cutoff)
	if remaining < 0 {
		remaining = 0
	}
	timer := time.NewTimer(remaining)
	defer timer.Stop()
	for len(pending) > 0 {
		select {
		case result := <-stopped:
			if _, ok := pending[result.flow]; ok {
				delete(pending, result.flow)
				log.Printf("[daemon] stopped flow %q", result.flow.name)
			}
		case <-timer.C:
			for flow := range pending {
				log.Printf("[daemon] stopAll: deadline reached waiting for flow %q — detaching it from daemon shutdown; restart checkpoint pass will preserve any running worker", flow.name)
			}
			return
		}
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

// runReloadPump is the single consumer of a flow's store watcher (#768). On
// each config-store edit it fans the freshly loaded config out so all three
// runtime readers see it without a flow restart:
//
//   - the holder — read each cycle by the supervise loop and used to rebuild
//     the dashboard snapshot, so a changed supervisor policy / labels / etc.
//     is applied LIVE (the gap #767 left: reload reached only the orchestrator);
//   - the FleetServer project entry — replaced in place so /api/v1/fleet
//     reflects the new config, and AddProject re-derives fleet auth so a newly
//     token-bearing edit starts enforcing auth (#768);
//   - the orchestrator — forwarded on orchReloadCh, which keeps its selective
//     reloadConfig (restart-required detection, ticker reset) over a private
//     shallow copy.
//
// The pump exits on ctx cancellation or when the watcher closes its channel on
// shutdown. It never closes orchReloadCh: a closed channel would spin the
// orchestrator's select on a nil config.
func (d *Daemon) runReloadPump(ctx context.Context, flow *projectFlow, watchCh <-chan *config.Config, orchReloadCh chan *config.Config) {
	for {
		select {
		case <-ctx.Done():
			return
		case newCfg, ok := <-watchCh:
			if !ok {
				// Watcher closed its channel (flow shutdown); nothing more to pump.
				return
			}
			if newCfg == nil {
				continue
			}
			// Identity / restart-required fields (repo, state_dir, session_prefix,
			// local_path) cannot be hot-applied: the orchestrator, supervisor,
			// watchdog, and the
			// supervisor's GitHub client were all built from the flow's STARTUP
			// identity. Storing a changed identity in the holder would make
			// supervisor.RunOnce(getCfg(), gh) read a repo/state_dir the rest of the
			// flow is not running on (gh is pinned to the startup repo). Skip the
			// live update and tell the operator to restart the project (rm+add, which
			// the diff-loop handles as a fresh flow); hot-reloadable edits still
			// apply (#768, Codex).
			if identityChanged(flow.cfg, newCfg) {
				log.Printf("[%s] config reload: restart-required field (repo/state_dir/session_prefix/local_path) changed — restart required, live reload skipped", flow.name)
				continue
			}
			newCfg.RuntimeSuperviseIntervalSeconds = runtimeIntervalSeconds(d.opts.SuperviseInterval)
			// Holder first, so the supervise loop's next cycle and the updated
			// dashboard snapshot below both observe the new config.
			flow.holder.Store(newCfg)

			// Update the project's config IN PLACE so /api/v1/fleet reflects the edit
			// and fleet auth is re-derived — WITHOUT rebuilding the FleetProject,
			// which would drop the live board refresher + action GH client a full
			// replacement discards (#768). d.Fleet() is nil only during the brief
			// startup window before Run publishes it; no reload can arrive that
			// early, but tolerate nil to be safe.
			if fleet := d.Fleet(); fleet != nil {
				fleet.UpdateProjectConfig(flow.name, newCfg)
			}

			// Forward to the orchestrator with latest-value coalescing. Dropping the
			// newest edit when the depth-1 channel is full can leave delivery config
			// permanently stale (and every fresh approval digest fail-closed) until
			// an unrelated later edit or restart. Replace the queued stale snapshot,
			// then guarantee the latest one is enqueued or shutdown wins.
			select {
			case orchReloadCh <- newCfg:
			default:
				select {
				case <-orchReloadCh:
				default:
				}
				select {
				case orchReloadCh <- newCfg:
				case <-ctx.Done():
					return
				}
			}
		}
	}
}

// identityChanged reports whether two configs differ in a flow-identity /
// restart-required field that cannot be hot-applied to a running flow: the
// orchestrator, supervisor, watchdog, and the supervisor's GitHub client are
// all built once from these at startup. flowKey already keys on StateDir/Repo;
// session_prefix names worker sessions. LocalPath is also part of the delivery
// approval digest and selects the supervisor/executor checkout. Applying it to
// only one reader would make every newly minted delivery approval fail closed
// with a config mismatch. A change here therefore needs one atomic flow restart.
func identityChanged(a, b *config.Config) bool {
	if a == nil || b == nil {
		return a != b
	}
	return a.Repo != b.Repo || a.StateDir != b.StateDir || a.SessionPrefix != b.SessionPrefix || a.LocalPath != b.LocalPath
}

// runOrchestrator is the production run loop for one flow. It mirrors the
// single-project branch of `maestro run` minus the per-project HTTP server
// (the daemon serves one shared FleetServer instead) and minus the fatal exit
// on run error — a failing orchestrator must log-and-continue, never take the
// daemon down (#756). orchestrator.Run already log-and-continues on per-cycle
// errors and returns nil on ctx cancellation.
//
// Store-driven hot-reload (#757): reloadCh is fed by the flow's reload pump
// (runReloadPump), which sources it from configwatch.WatchStore over the
// project's config-store row (the daemon's source of truth, #754). The daemon
// deliberately does NOT watch the import-time YAML — a file the store supersedes
// — which would let the run loop drift from the supervise loop. A nil reloadCh
// (store-watch disabled) leaves the orchestrator's reload select arm inert.
//
// It is a method so the orchestrator's self-deploy launcher can be wired to the
// daemon's centralized, cross-flow-debounced RequestSelfDeploy (#758): each
// flow signals the daemon instead of firing its own selfdeploy.Trigger, so a
// burst of merges across flows launches exactly ONE deploy of ONE unit.
func (d *Daemon) runOrchestrator(ctx context.Context, cfg *config.Config, opts Options, reloadCh <-chan *config.Config) {
	// Hot-reload (reloadCh, #757) makes the orchestrator's reloadConfig mutate
	// its config in place (field replacement). The supervise loop and the
	// FleetProject HTTP snapshot read the flow's config through a shared holder
	// (#768) that the reload pump swaps atomically, so hand the orchestrator a
	// private shallow copy to mutate — nothing the other goroutines read is ever
	// written in place, keeping the #767 reload data race closed. reloadConfig
	// only REPLACES fields (o.cfg.X = newCfg.X), never mutates a slice/map
	// element in place, so a shallow copy fully isolates it.
	//
	// As of #768 a config-store edit reaches all three readers live: the pump
	// stores the new config in the holder (supervisor + dashboard) and forwards
	// it here for the orchestrator's selective reload. The orchestrator keeps the
	// private-copy reload (rather than reading the holder) because reloadConfig
	// detects restart-required fields and resets the poll ticker — it needs the
	// reload event, not just the latest value.
	orchCfg := *cfg
	orch := orchestrator.New(&orchCfg)
	orch.SetBinaryVersion(opts.Version)
	// #866: let the orchestrator's standing repair-approval reconciler mirror a
	// moot-approval stale transition into the same SQLite approval store the
	// fleet approve/reject endpoint uses, so JSON state and SQLite never diverge
	// under --approvals-store sqlite. json mode makes the mirror a no-op.
	orch.SetApprovalStore(opts.ApprovalsStore, opts.ApprovalsDBPath)
	// Fleet-wide EMERGENCY STOP spawn gate (#840): the orchestrator consults this
	// at the top of startNewWorkers each cycle, so an active stop halts new
	// spawns (and the router LLM calls that only happen inside the spawn loop)
	// within one poll interval. nil emergency store (open failed / test loader)
	// leaves the closure reading a permanently-normal cache, i.e. inert.
	orch.SetEmergencyHalt(d.emergencySpawnHalt)
	orch.SetFleetSpawnCeiling(d.fleetSpawnCeilingReached)
	orch.SetFleetSpawnReserve(func() (func(string), func(), bool) {
		return d.reserveFleetSpawn(orchCfg.StateDir)
	})
	// Mirror-first reads (#826): serve the orchestrator's high-volume poll reads
	// from the shared mirror when github_mirror.source is mirror-first, falling
	// back to the API on a miss/stale. The closure reads &orchCfg — the live,
	// reload-mutated config (reloadConfig copies GitHubMirror), so the escape
	// hatch flips this flow without a redeploy. nil source (no mirror) leaves the
	// orchestrator reading GitHub directly, unchanged.
	if src := d.newReadSource(orchCfg.Repo, func() *config.Config { return &orchCfg }); src != nil {
		orch.SetReadSource(src)
	}
	// Route post-merge self-deploy through the daemon's centralized,
	// cross-flow-debounced launcher (#758) instead of this flow firing its own
	// selfdeploy.Trigger. The closure passes the orchestrator's OWN config
	// pointer (&orchCfg, == o.cfg) — the live, reload-mutated config the prior
	// per-flow selfdeploy.Trigger(o.cfg, …) used, supplying the build inputs
	// (local_path/self_deploy/state_dir). It runs in the orchestrator goroutine
	// (synchronous in triggerSelfDeploy), the same goroutine that mutates orchCfg
	// on reload, so reading it here is race-free. The daemon overlays the
	// single-endpoint health probe.
	orch.SetSelfDeployStartFn(func(prNumber int) error {
		return d.RequestSelfDeploy(&orchCfg, prNumber)
	})
	if err := orch.LoadPromptBase(opts.PromptPath); err != nil {
		log.Printf("[%s] warn: load prompt: %v", cfg.SessionPrefix, err)
	}
	orch.SetConfigReloadCh(reloadCh)

	runInterval := opts.RunInterval
	if cfg.PollIntervalSeconds > 0 {
		runInterval = time.Duration(cfg.PollIntervalSeconds) * time.Second
	}

	log.Printf("[%s] starting orchestrator — repo=%s interval=%s", cfg.SessionPrefix, cfg.Repo, runInterval)
	if err := orch.Run(ctx, runInterval, false, opts.refreshCh); err != nil {
		log.Printf("[%s] orchestrator exited: %v", cfg.SessionPrefix, err)
	}
}
