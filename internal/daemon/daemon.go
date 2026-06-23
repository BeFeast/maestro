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

	// runLoop and superviseLoop build the per-project loops. They default to
	// the production orchestrator + supervisor wiring; tests override them to
	// drive flows without reaching GitHub.
	runLoop       func(ctx context.Context, cfg *config.Config, opts Options)
	superviseLoop func(ctx context.Context, cfg *config.Config, opts Options)
}

// New constructs a Daemon that loads projects from store on Run.
func New(store ConfigLoader, opts Options) *Daemon {
	d := &Daemon{
		store: store,
		opts:  opts,
		flows: make(map[string]*projectFlow),
	}
	d.runLoop = runOrchestrator
	d.superviseLoop = func(ctx context.Context, cfg *config.Config, opts Options) {
		runSupervise(ctx, cfg, opts.SuperviseInterval)
	}
	return d
}

// Run loads every project from the store, starts a flow (orchestrator +
// supervisor) for each, and serves one FleetServer aggregating them all. It
// blocks until ctx is cancelled, then drains every flow before returning.
//
// A project that cannot be turned into a flow (duplicate fleet name) is
// logged and skipped — one bad project must never abort daemon startup or the
// other flows (#756).
func (d *Daemon) Run(ctx context.Context) error {
	cfgs, err := d.store.LoadAll(ctx)
	if err != nil {
		return fmt.Errorf("load configs: %w", err)
	}
	if len(cfgs) == 0 {
		return errors.New("config store has no projects")
	}

	seen := make(map[string]bool, len(cfgs))
	projects := make([]server.FleetProject, 0, len(cfgs))
	for _, cfg := range cfgs {
		proj := newFleetProject(cfg)
		if seen[proj.Name] {
			log.Printf("[daemon] skip project %q (repo=%s): duplicate fleet name already started", proj.Name, cfg.Repo)
			continue
		}
		seen[proj.Name] = true
		flow := d.startFlow(ctx, proj)
		projects = append(projects, proj)
		log.Printf("[daemon] started flow %q (repo=%s)", flow.name, cfg.Repo)
	}

	// One FleetServer for the whole fleet — replaces the N per-project
	// server.New instances the legacy run/serve paths spun up (#516).
	fleet := server.NewFleet(projects, d.opts.Host, d.opts.Port, d.opts.ReadOnly)
	fleet.SetAuth(fleetAuth(projects))
	d.mu.Lock()
	d.fleet = fleet
	d.mu.Unlock()

	log.Printf("[daemon] serving fleet — projects=%d addr=%s:%d read_only=%v", len(projects), d.opts.Host, d.opts.Port, d.opts.ReadOnly)
	fleetErr := make(chan error, 1)
	go func() { fleetErr <- fleet.Start(ctx) }()

	// Block until shutdown is requested. We wait on ctx (not fleet.Start) so
	// the daemon stays up even when the fleet is not bound (port 0).
	<-ctx.Done()
	d.stopAll()
	return <-fleetErr
}

// Fleet returns the aggregating FleetServer once Run has built it, or nil
// before then. Exposed for tests and embedders that want the handle.
func (d *Daemon) Fleet() *server.FleetServer {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.fleet
}

// fleetAuth returns the first non-empty Server.Auth config across the fleet.
// The fleet uses a single shared token (#487); per-project distinct tokens are
// intentionally out of scope.
func fleetAuth(projects []server.FleetProject) config.ServerAuthConfig {
	for i := range projects {
		cfg := projects[i].Cfg()
		if cfg == nil {
			continue
		}
		if strings.TrimSpace(cfg.Server.Auth.TokenEnv) != "" {
			return cfg.Server.Auth
		}
	}
	return config.ServerAuthConfig{}
}
