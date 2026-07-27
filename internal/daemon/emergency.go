package daemon

import (
	"context"
	"log"
	"strings"
	"time"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/emergencystore"
	"github.com/befeast/maestro/internal/notify"
)

// configureEmergencyStop opens the fleet-wide EMERGENCY STOP switch store (#840)
// against the unified maestro.db, seeds the cached state, wires the fleet
// snapshot's emergency source, and builds the notifier used on activation/resume.
// Every failure is non-fatal: a store that will not open disables the gate with
// a loud warning rather than blocking daemon startup — the switch is a safety
// brake layered over the normal flows, and its absence degrades to today's
// behavior. cfgs are the deduped project configs (for the notifier's Telegram
// settings). Returns the opened store (nil on failure) so Run can close it.
func (d *Daemon) configureEmergencyStop(ctx context.Context, cfgs []*config.Config) *emergencystore.Store {
	dbPath := strings.TrimSpace(d.opts.EmergencyDBPath)
	if dbPath == "" {
		dbPath = emergencystore.DefaultDBPath()
	}
	store, err := emergencystore.Open(dbPath)
	if err != nil {
		log.Printf("[daemon] EMERGENCY STOP gate disabled: open switch store %s: %v", dbPath, err)
		return nil
	}

	d.emergencyNotifier = emergencyNotifierFor(cfgs)

	// Seed the cache from the current switch so the gates are correct from the
	// first cycle — a switch set while the daemon was down is honored on this
	// start (#840 AC: "honored on next daemon start").
	if st, gerr := store.Get(ctx); gerr != nil {
		log.Printf("[daemon] EMERGENCY STOP: initial switch read failed (assuming normal): %v", gerr)
	} else {
		d.setEmergencyState(st)
		if st.Active() {
			log.Printf("[daemon] EMERGENCY STOP active on startup — level=%s since=%s by=%s reason=%q: no LLM calls, no new spawns until `maestro emergency resume`",
				st.Level, formatEmergencySince(st.Since), st.Actor, st.Reason)
		}
	}

	d.emergency = store
	return store
}

// EmergencyState returns the last-read fleet-wide emergency switch. It is safe
// for concurrent use and never hits the DB — the emergency-watch loop refreshes
// the cache.
func (d *Daemon) EmergencyState() emergencystore.State {
	d.emergencyMu.RLock()
	defer d.emergencyMu.RUnlock()
	return d.emergencyState
}

func (d *Daemon) setEmergencyState(st emergencystore.State) {
	d.emergencyMu.Lock()
	d.emergencyState = st
	d.emergencyMu.Unlock()
}

// emergencyLLMHalt reports whether the supervisor LLM path must drop to
// deterministic-only this cycle. Wired into the supervise loop.
func (d *Daemon) emergencyLLMHalt() bool { return d.EmergencyState().HaltsLLM() }

// emergencySpawnHalt reports whether the orchestrator spawn gate must be closed
// this cycle. Wired into runOrchestrator via SetEmergencyHalt.
func (d *Daemon) emergencySpawnHalt() bool { return d.EmergencyState().HaltsSpawns() }

// watchEmergencyLoop is the daemon's EMERGENCY STOP poll loop (#840). On each
// tick it re-reads the switch from the unified DB and, when the level changes,
// refreshes the cache the gates read and announces the transition — a
// notify_red-grade alert on activation and on resume, plus one journal line per
// running flow so the operator sees every project confirm the change.
//
// This is the "existing store watch" the CLI relies on: `maestro emergency
// stop-llm --db …` writes the switch directly, and this loop picks it up within
// one interval without a daemon restart. A transient read error is logged and
// retried on the next tick — it never kills the loop or flips the gate.
func (d *Daemon) watchEmergencyLoop(ctx context.Context, interval time.Duration) {
	if d.emergency == nil {
		return
	}
	if interval <= 0 {
		interval = DefaultEmergencyWatchInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			st, err := d.emergency.Get(ctx)
			if err != nil {
				log.Printf("[daemon] EMERGENCY STOP watch: read failed (keeping last state, will retry): %v", err)
				continue
			}
			prev := d.EmergencyState()
			if st.Level == prev.Level {
				continue
			}
			d.setEmergencyState(st)
			d.announceEmergencyTransition(prev, st)
		}
	}
}

// announceEmergencyTransition logs one confirmation line per running flow on a
// level change (#840 AC: "one journal line per flow confirming"). It does NOT
// send the notify_red-grade alert — that is sent by whichever surface engaged
// the switch (the CLI verb, or the dashboard endpoint), so the operator gets
// exactly one alert per action even though every daemon flow observes the flip.
func (d *Daemon) announceEmergencyTransition(prev, cur emergencystore.State) {
	flows := d.flowNames()
	if cur.Active() {
		log.Printf("[daemon] EMERGENCY STOP engaged — level=%s since=%s by=%s reason=%q",
			cur.Level, formatEmergencySince(cur.Since), cur.Actor, cur.Reason)
		for _, name := range flows {
			log.Printf("[%s] EMERGENCY STOP (%s) — no LLM calls, no new spawns this flow", name, cur.Level)
		}
		return
	}
	// cur is LevelNone (a resume).
	log.Printf("[daemon] EMERGENCY STOP cleared — normal operation restored (by=%s)", cur.Actor)
	for _, name := range flows {
		log.Printf("[%s] EMERGENCY STOP cleared — LLM calls and spawns resume next cycle", name)
	}
}

func (d *Daemon) flowNames() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	names := make([]string, 0, len(d.flows))
	for _, flow := range d.flows {
		names = append(names, flow.name)
	}
	return names
}

// emergencyNotifierFor builds a fleet-level notifier from the first project that
// has a Telegram target configured, so the dashboard endpoint's activation/resume
// alert reaches the operator's channel. Returns nil when no project configures
// notifications.
func emergencyNotifierFor(cfgs []*config.Config) *notify.Notifier {
	for _, cfg := range cfgs {
		if cfg == nil {
			continue
		}
		// Prefer a project that configures notifications (Telegram target or the
		// ntfy transport) so the notify_red-grade alert reaches an operator via
		// at least one channel (#1018).
		if strings.TrimSpace(cfg.Telegram.Target) == "" && !cfg.Notify.Ntfy.Enabled() {
			continue
		}
		n := notify.NewWithToken(cfg.Telegram.Token(), cfg.Telegram.Target, cfg.Telegram.Mode, cfg.Telegram.OpenclawURL)
		n.WithNtfy(cfg.Notify.Ntfy.BaseURL, cfg.Notify.Ntfy.Topic, cfg.Notify.Ntfy.Token())
		return n
	}
	return nil
}

func formatEmergencySince(t time.Time) string {
	if t.IsZero() {
		return "unknown"
	}
	return t.UTC().Format(time.RFC3339)
}
