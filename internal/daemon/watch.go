package daemon

import (
	"context"
	"log"
	"time"

	"github.com/befeast/maestro/internal/server"
)

// watchStoreLoop is the daemon's DB diff-loop (#757). On each tick it diffs the
// store's project set (ProjectsFingerprint) against the running store-backed
// flows: a name present in the store but not running is started and registered
// in the fleet, and a running flow whose name has vanished from the store is
// drained and removed from the fleet. Per-project config CHANGES are handled
// separately by each flow's configwatch.WatchStore reload channel (wired in
// startFlow), so this loop reconciles membership only.
//
// loopCtx controls the loop's own lifetime; flowParent parents every flow it
// starts (the daemon ctx) so daemon shutdown's stopAll drains them. Run always
// stops this loop before stopAll, so the loop never starts a flow that stopAll
// would miss.
func (d *Daemon) watchStoreLoop(loopCtx, flowParent context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = DefaultWatchStoreInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	// Log a duplicate-identity skip at most once per offending name so a
	// misconfigured store (two projects sharing one StateDir) does not spam the
	// journal every tick.
	loggedDupSkip := map[string]bool{}

	for {
		select {
		case <-loopCtx.Done():
			return
		case <-ticker.C:
			fp, err := d.projectStore.ProjectsFingerprint(loopCtx)
			if err != nil {
				// A transient store read must not kill the loop; retry next tick.
				log.Printf("[daemon] store watch: fingerprint failed (will retry): %v", err)
				continue
			}
			d.reconcileStore(flowParent, fp, loggedDupSkip)
		}
	}
}

// reconcileStore applies one diff between the store's project set (fp) and the
// running store-backed flows: drain flows whose project row is gone, start
// flows for project rows with no running flow. loggedDupSkip carries across
// ticks so a duplicate-identity skip is logged once, not every cycle.
func (d *Daemon) reconcileStore(flowParent context.Context, fp map[string]time.Time, loggedDupSkip map[string]bool) {
	// Snapshot the running store-backed flows and the taken fleet identities
	// under one lock so the add/remove decisions are internally consistent.
	d.mu.Lock()
	running := make(map[string]*projectFlow, len(d.flows)) // storeName -> flow
	takenNames := make(map[string]bool, len(d.flows))      // fleet display names
	takenKeys := make(map[string]bool, len(d.flows))       // flow identities (StateDir/Repo)
	for _, flow := range d.flows {
		takenNames[flow.name] = true
		takenKeys[flow.key] = true
		if flow.storeName != "" {
			running[flow.storeName] = flow
		}
	}
	d.mu.Unlock()

	fleet := d.Fleet()

	// Removals: a running store-backed flow whose name is gone from the store.
	for name, flow := range running {
		if _, ok := fp[name]; ok {
			continue
		}
		log.Printf("[daemon] store watch: project %q removed — draining flow %q", name, flow.name)
		if fleet != nil {
			fleet.RemoveProject(flow.name)
		}
		d.stopFlow(flow.key)
	}

	// Additions: a store project with no running flow.
	for name := range fp {
		if _, ok := running[name]; ok {
			continue
		}
		cfg, err := d.projectStore.Load(flowParent, name)
		if err != nil {
			log.Printf("[daemon] store watch: load new project %q failed (will retry): %v", name, err)
			continue
		}
		if cfg == nil {
			continue
		}
		key := flowKey(cfg)
		if takenKeys[key] {
			if !loggedDupSkip[name] {
				log.Printf("[daemon] store watch: skip project %q (repo=%s state_dir=%s): duplicate flow identity already running", name, cfg.Repo, cfg.StateDir)
				loggedDupSkip[name] = true
			}
			continue
		}
		fleetName := server.UniqueFleetName(cfg.Repo, takenNames)
		proj := server.NewFleetProjectWithGitHubNamed(fleetName, cfg)
		if fleet != nil {
			fleet.AddProject(proj)
		}
		takenKeys[key] = true
		flow := d.startFlow(flowParent, name, proj)
		delete(loggedDupSkip, name) // recovered: it now has a running flow
		log.Printf("[daemon] store watch: started flow %q for new project %q (repo=%s state_dir=%s)", flow.name, name, cfg.Repo, cfg.StateDir)
	}
}
