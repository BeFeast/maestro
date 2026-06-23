// Package daemon runs every config-store project as one long-lived process
// (#756, epic #754 single-service redesign). Before this, each project was its
// own systemd unit (maestro@<project>.service) plus a per-project supervise
// loop and a per-project fleet web server. The daemon collapses that into a
// single process: one orchestrator + one supervisor loop per project flow, and
// a single FleetServer aggregating every project.
//
// The orchestrator was already multi-project in one process (runCmd spawns a
// goroutine per config); the daemon extends that model to supervise and the
// web layer. It is strictly opt-in — the legacy units and `maestro serve`
// keep working unchanged.
package daemon

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/server"
	"github.com/befeast/maestro/internal/supervisor"
)

// Default loop intervals, shared by the daemon command's flag defaults and the
// clamp in New so an explicit non-positive operator value falls back to a safe
// cadence instead of disabling supervise or panicking time.NewTicker (#764).
const (
	DefaultRunInterval       = 10 * time.Minute
	DefaultSuperviseInterval = 5 * time.Minute
)

// ConfigLoader yields the set of project configs the daemon supervises. It is
// satisfied by *configstore.Store; tests inject an in-memory fake so the
// daemon's lifecycle can be exercised without a real SQLite store.
type ConfigLoader interface {
	LoadAll(ctx context.Context) ([]*config.Config, error)
}

// Options configures a Daemon.
type Options struct {
	// Host and Port bind the single aggregating FleetServer (#516: one web
	// for the whole fleet, default :8786).
	Host string
	Port int

	// RunInterval is the orchestrator poll interval; a project's
	// poll_interval_seconds in config overrides it per flow.
	RunInterval time.Duration
	// SuperviseInterval is the supervisor decision-loop interval per flow.
	SuperviseInterval time.Duration

	// PromptPath is an optional worker prompt base file applied to every flow.
	PromptPath string
	// Version is the running binary version, surfaced on the fleet API for
	// the self-deploy health probe (#698).
	Version string
	// ReadOnly disables the fleet's mutating HTTP endpoints.
	ReadOnly bool
}

// Daemon owns the running project flows and the shared FleetServer.
type Daemon struct {
	store ConfigLoader
	opts  Options

	mu    sync.Mutex
	fleet *server.FleetServer
	flows map[string]*projectFlow

	// runLoop, superviseLoop, and watchdogLoop build the per-project loops.
	// They default to the production orchestrator + supervisor wiring; tests
	// override them to drive flows (and assert WaitGroup tracking) without
	// reaching GitHub.
	runLoop       func(ctx context.Context, cfg *config.Config, opts Options)
	superviseLoop func(ctx context.Context, name string, cfg *config.Config, opts Options)
	watchdogLoop  func(ctx context.Context, name, stateDir string, interval time.Duration)
}

// New constructs a Daemon that loads projects from store on Run.
//
// Non-positive intervals are clamped to the package defaults with a loud warn
// rather than silently honored: a 0 run-interval would panic time.NewTicker
// (caught by recoverFlow, killing the orchestrator silently) and a 0
// supervise-interval would run a single cycle then leave the supervise loop and
// its watchdog dead while the flow still reports healthy (#764). The 10m/5m
// defaults are safe, so this only fires on an explicit non-positive value.
func New(store ConfigLoader, opts Options) *Daemon {
	if opts.RunInterval <= 0 {
		log.Printf("[daemon] run-interval %s is not positive; clamping to default %s", opts.RunInterval, DefaultRunInterval)
		opts.RunInterval = DefaultRunInterval
	}
	if opts.SuperviseInterval <= 0 {
		log.Printf("[daemon] supervise-interval %s is not positive; clamping to default %s", opts.SuperviseInterval, DefaultSuperviseInterval)
		opts.SuperviseInterval = DefaultSuperviseInterval
	}
	d := &Daemon{
		store: store,
		opts:  opts,
		flows: make(map[string]*projectFlow),
	}
	d.runLoop = runOrchestrator
	d.superviseLoop = func(ctx context.Context, name string, cfg *config.Config, opts Options) {
		runSupervise(ctx, name, cfg, opts.SuperviseInterval)
	}
	d.watchdogLoop = supervisor.Watchdog
	return d
}

// Run loads every project from the store, starts a flow (orchestrator +
// supervisor) for each, and serves one FleetServer aggregating them all. It
// blocks until ctx is cancelled, then drains every flow before returning.
//
// A project that duplicates an already-started flow identity (same StateDir)
// is logged and skipped — one bad project must never abort daemon startup or
// the other flows (#756).
func (d *Daemon) Run(ctx context.Context) error {
	cfgs, err := d.store.LoadAll(ctx)
	if err != nil {
		return fmt.Errorf("load configs: %w", err)
	}
	if len(cfgs) == 0 {
		return errors.New("config store has no projects")
	}

	// Dedup on the flow's real identity (StateDir, falling back to Repo), not
	// the fleet display name. The display name is the repo basename, so
	// org-a/api and org-b/api collapsed to one "api" and the second project's
	// orchestrator + supervisor never started; the legitimate one-repo /
	// two-StateDir layout (which serve --fleet aggregates without dedup) was
	// likewise dropped (#764). Only a literal re-load of the same StateDir is a
	// true duplicate.
	//
	// usedNames keeps every started project's fleet display name distinct so
	// the aggregating FleetServer can address each one — its findProject keys
	// on Project.Name, and two repos that share a basename ("api") would
	// otherwise route the second repo's routes/actions/audit to the first
	// (#764).
	//
	// This StateDir dedup is intentionally NOT folded into
	// server.FleetProjectsFromConfigs (which `serve --fleet` uses): the daemon
	// owns exactly one *flow* per identity, so a repeated StateDir is a true
	// duplicate to skip, whereas serve merely aggregates the configs it is
	// handed. Keeping the dedup here — over the shared per-cfg construction
	// (UniqueFleetName + NewFleetProjectWithGitHubNamed) — documents that
	// divergence instead of silently forking the shared builder (#764).
	seen := make(map[string]bool, len(cfgs))
	usedNames := make(map[string]bool, len(cfgs))
	projects := make([]server.FleetProject, 0, len(cfgs))
	for _, cfg := range cfgs {
		if cfg == nil {
			// A nil entry from the loader must not panic Run: flowKey tolerates
			// nil but the wiring below dereferences cfg. Skip it, mirroring
			// server.FleetProjectsFromConfigs' cfgRepo(nil) tolerance (#764).
			log.Printf("[daemon] skip nil config from store")
			continue
		}
		key := flowKey(cfg)
		if seen[key] {
			log.Printf("[daemon] skip project (repo=%s state_dir=%s): duplicate flow identity already started", cfg.Repo, cfg.StateDir)
			continue
		}
		seen[key] = true
		name := server.UniqueFleetName(cfg.Repo, usedNames)
		projects = append(projects, server.NewFleetProjectWithGitHubNamed(name, cfg))
	}

	// One FleetServer for the whole fleet — replaces the N per-project
	// server.New instances the legacy run/serve paths spun up (#516).
	fleet := server.NewFleet(projects, d.opts.Host, d.opts.Port, d.opts.ReadOnly)
	fleet.SetAuth(server.FleetAuthFromProjects(projects))

	// Bind the fleet port BEFORE starting any flow. If the port is already held
	// (a legacy maestro@ unit, a second daemon), Listen fails here and we abort
	// immediately — no flow has started, so there is nothing to drain. Binding
	// AFTER starting flows would force the error path through stopAll, which
	// blocks on a flow's in-flight, non-cancellable first RunOnce and hangs the
	// startup-error return (#764 P2). Listen returns a nil listener for port 0
	// (no web endpoint requested); Serve then no-ops on it.
	ln, err := fleet.Listen()
	if err != nil {
		return err
	}

	for i := range projects {
		flow := d.startFlow(ctx, projects[i])
		log.Printf("[daemon] started flow %q (repo=%s state_dir=%s)", flow.name, flow.cfg.Repo, flow.cfg.StateDir)
	}

	// Expose the fleet only after the flows are started so callers that observe
	// d.Fleet() can rely on the flows being live.
	d.mu.Lock()
	d.fleet = fleet
	d.mu.Unlock()

	log.Printf("[daemon] serving fleet — projects=%d addr=%s:%d read_only=%v", len(projects), d.opts.Host, d.opts.Port, d.opts.ReadOnly)
	fleetErr := make(chan error, 1)
	go func() { fleetErr <- fleet.Serve(ctx, ln) }()

	select {
	case <-ctx.Done():
		d.stopAll()
		return <-fleetErr
	case err := <-fleetErr:
		// Serve returned before shutdown — a runtime serve error after a
		// successful bind (rare; the bind failure itself was already handled by
		// Listen above). Flows are legitimately running, so drain them.
		d.stopAll()
		if err != nil {
			log.Printf("[daemon] fleet server failed on %s:%d: %v", d.opts.Host, d.opts.Port, err)
			return err
		}
		<-ctx.Done()
		return nil
	}
}

// flowKey is a flow's stable identity for dedup and the flows registry. The
// StateDir is the real per-flow identity (one repo can host several configs
// with distinct StateDirs); Repo is the fallback when StateDir is unset.
func flowKey(cfg *config.Config) string {
	if cfg == nil {
		return ""
	}
	if sd := strings.TrimSpace(cfg.StateDir); sd != "" {
		return sd
	}
	return strings.TrimSpace(cfg.Repo)
}

// Fleet returns the aggregating FleetServer once Run has built it, or nil
// before then. Exposed for tests and embedders that want the handle.
func (d *Daemon) Fleet() *server.FleetServer {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.fleet
}
