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
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/configwatch"
	"github.com/befeast/maestro/internal/selfdeploy"
	"github.com/befeast/maestro/internal/server"
	"github.com/befeast/maestro/internal/supervisor"
)

// Default loop intervals, shared by the daemon command's flag defaults and the
// clamp in New so an explicit non-positive operator value falls back to a safe
// cadence instead of disabling supervise or panicking time.NewTicker (#764).
const (
	DefaultRunInterval       = 10 * time.Minute
	DefaultSuperviseInterval = 5 * time.Minute
	// DefaultWatchStoreInterval is the cadence of the store diff-loop and each
	// flow's reload watcher when --watch-store is enabled (#757). Short enough
	// that operator CRUD (config-store add/rm/edit) is reflected quickly, long
	// enough that polling SQLite is negligible.
	DefaultWatchStoreInterval = 15 * time.Second
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

	// SelfDeployStateDir is the shared directory the centralized self-deploy
	// debounce marker lives in (#758). The orchestrator flows no longer each fire
	// their own selfdeploy.Trigger; they signal Daemon.RequestSelfDeploy, which
	// debounces on a SINGLE marker so N flows merging PRs near-simultaneously
	// launch exactly ONE deploy of ONE unit. A per-flow StateDir cannot dedup
	// across flows, so the daemon owns one shared marker dir that also survives
	// the daemon being restarted by its own deploy (#722). Blank disables the
	// on-disk marker; the in-process mutex still dedups within a single run.
	SelfDeployStateDir string

	// WatchStore enables DB-driven hot add/remove/reload of projects (#757):
	// a diff-loop over the store's ProjectsFingerprint starts a flow for a new
	// project name and drains one for a removed name, and each flow's
	// orchestrator hot-reloads when its project row's config changes. It
	// requires the store to satisfy configwatch.ProjectStore (the real
	// *configstore.Store does); a store that does not is a no-op with a warn.
	WatchStore bool
	// WatchStoreInterval is the diff-loop / reload poll cadence; clamped to
	// DefaultWatchStoreInterval when non-positive.
	WatchStoreInterval time.Duration
}

// Daemon owns the running project flows and the shared FleetServer.
type Daemon struct {
	store ConfigLoader
	opts  Options

	// projectStore is store viewed as a configwatch.ProjectStore — non-nil only
	// when the store supports per-project Load + ProjectsFingerprint, which the
	// store diff-loop and per-flow reload watchers need (#757). The in-memory
	// test ConfigLoader does not satisfy it, so the watch path is simply inert
	// for those tests.
	projectStore configwatch.ProjectStore

	mu    sync.Mutex
	fleet *server.FleetServer
	flows map[string]*projectFlow

	// Centralized self-deploy (#758). RequestSelfDeploy serializes on
	// selfDeployMu and debounces on a single marker — the in-memory
	// selfDeployLast (deterministic within this process) plus the on-disk marker
	// in opts.SelfDeployStateDir (survives the daemon restarting itself) — so a
	// burst of merges across flows fires exactly ONE deploy. selfDeployTrigger is
	// the launcher, defaulting to selfdeploy.Trigger; tests override it to count
	// launches without touching systemd.
	selfDeployMu      sync.Mutex
	selfDeployLast    time.Time
	selfDeployLastPR  int
	selfDeployTrigger func(cfg *config.Config, prNumber int) error

	// runLoop, superviseLoop, and watchdogLoop build the per-project loops.
	// They default to the production orchestrator + supervisor wiring; tests
	// override them to drive flows (and assert WaitGroup tracking) without
	// reaching GitHub. runLoop receives the orchestrator's hot-reload channel
	// (#757), nil when store-watch is disabled. superviseLoop receives a getCfg
	// closure rather than a fixed *config.Config so it reads the flow's current
	// config each cycle through the shared holder — a config-store edit reaches
	// the supervise loop live, not just the orchestrator (#768).
	runLoop       func(ctx context.Context, cfg *config.Config, opts Options, reloadCh <-chan *config.Config)
	superviseLoop func(ctx context.Context, name string, getCfg func() *config.Config, opts Options)
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
	if opts.WatchStore && opts.WatchStoreInterval <= 0 {
		log.Printf("[daemon] watch-store-interval %s is not positive; clamping to default %s", opts.WatchStoreInterval, DefaultWatchStoreInterval)
		opts.WatchStoreInterval = DefaultWatchStoreInterval
	}
	d := &Daemon{
		store: store,
		opts:  opts,
		flows: make(map[string]*projectFlow),
	}
	if opts.WatchStore {
		// Only the richer store (Load + ProjectsFingerprint) can drive hot
		// add/remove/reload. Degrade loudly rather than silently if an embedder
		// flips the flag on a loader that cannot support it.
		if ps, ok := store.(configwatch.ProjectStore); ok {
			d.projectStore = ps
		} else {
			log.Printf("[daemon] --watch-store requested but the config store does not support per-project reload; disabling hot add/remove/reload")
		}
	}
	d.runLoop = d.runOrchestrator
	d.superviseLoop = func(ctx context.Context, name string, getCfg func() *config.Config, opts Options) {
		runSupervise(ctx, name, getCfg, opts.SuperviseInterval)
	}
	d.watchdogLoop = supervisor.Watchdog
	// Default to the real launcher; tests swap it for a counter (#758).
	d.selfDeployTrigger = selfdeploy.Trigger
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
	named, err := d.loadNamedConfigs(ctx)
	if err != nil {
		return err
	}
	if len(named) == 0 {
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
	seen := make(map[string]bool, len(named))
	usedNames := make(map[string]bool, len(named))
	projects := make([]server.FleetProject, 0, len(named))
	storeNames := make([]string, 0, len(named))
	for _, nc := range named {
		cfg := nc.cfg
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
		storeNames = append(storeNames, nc.name)
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
		flow := d.startFlow(ctx, storeNames[i], projects[i])
		log.Printf("[daemon] started flow %q (repo=%s state_dir=%s)", flow.name, flow.cfg.Repo, flow.cfg.StateDir)
	}

	// Expose the fleet only after the flows are started so callers that observe
	// d.Fleet() can rely on the flows being live.
	d.mu.Lock()
	d.fleet = fleet
	d.mu.Unlock()

	// Store diff-loop (#757): hot add/remove of project flows. It runs under its
	// own context so it can be stopped on a serve error (where ctx is not yet
	// cancelled) as well as on ctx.Done. New flows it starts are parented on the
	// daemon ctx, not the loop's, so stopAll drains them; stopWatch always runs
	// before stopAll, so the loop has exited and started no flow stopAll misses.
	var stopWatch func()
	if d.projectStore != nil {
		wctx, watchCancel := context.WithCancel(ctx)
		watchDone := make(chan struct{})
		go func() {
			defer close(watchDone)
			d.watchStoreLoop(wctx, ctx, d.opts.WatchStoreInterval)
		}()
		log.Printf("[daemon] store watch enabled — diff-loop poll every %s", d.opts.WatchStoreInterval)
		stopWatch = func() {
			watchCancel()
			<-watchDone
		}
	}
	drainWatch := func() {
		if stopWatch != nil {
			stopWatch()
		}
	}

	log.Printf("[daemon] serving fleet — projects=%d addr=%s:%d read_only=%v", len(projects), d.opts.Host, d.opts.Port, d.opts.ReadOnly)
	fleetErr := make(chan error, 1)
	go func() { fleetErr <- fleet.Serve(ctx, ln) }()

	select {
	case <-ctx.Done():
		drainWatch()
		d.stopAll()
		return <-fleetErr
	case err := <-fleetErr:
		// Serve returned before shutdown — a runtime serve error after a
		// successful bind (rare; the bind failure itself was already handled by
		// Listen above). Flows are legitimately running, so drain them.
		drainWatch()
		d.stopAll()
		if err != nil {
			log.Printf("[daemon] fleet server failed on %s:%d: %v", d.opts.Host, d.opts.Port, err)
			return err
		}
		<-ctx.Done()
		return nil
	}
}

// namedConfig pairs a config-store project name with its loaded config. The
// name (the store's primary key) is what the diff-loop and per-flow reload
// watcher key on; it is empty on the LoadAll fallback path, which disables
// per-flow reload for that flow but leaves it otherwise fully functional.
type namedConfig struct {
	name string
	cfg  *config.Config
}

// loadNamedConfigs resolves the projects to start. When the store supports
// per-project reload (projectStore != nil), it loads via ProjectsFingerprint so
// each config carries its store name for the diff-loop; otherwise it falls back
// to LoadAll with empty names, preserving the Phase 1 behaviour for plain
// ConfigLoaders and the in-memory test loader.
func (d *Daemon) loadNamedConfigs(ctx context.Context) ([]namedConfig, error) {
	if d.projectStore != nil {
		fp, err := d.projectStore.ProjectsFingerprint(ctx)
		if err != nil {
			return nil, fmt.Errorf("fingerprint config store: %w", err)
		}
		names := make([]string, 0, len(fp))
		for name := range fp {
			names = append(names, name)
		}
		sort.Strings(names)
		out := make([]namedConfig, 0, len(names))
		for _, name := range names {
			cfg, err := d.projectStore.Load(ctx, name)
			if err != nil {
				return nil, fmt.Errorf("load project %s: %w", name, err)
			}
			out = append(out, namedConfig{name: name, cfg: cfg})
		}
		return out, nil
	}
	cfgs, err := d.store.LoadAll(ctx)
	if err != nil {
		return nil, fmt.Errorf("load configs: %w", err)
	}
	out := make([]namedConfig, 0, len(cfgs))
	for _, cfg := range cfgs {
		out = append(out, namedConfig{cfg: cfg})
	}
	return out, nil
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
