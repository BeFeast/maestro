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
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/befeast/maestro/internal/approvalstore"
	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/configwatch"
	"github.com/befeast/maestro/internal/github"
	"github.com/befeast/maestro/internal/mirrorstore"
	"github.com/befeast/maestro/internal/selfdeploy"
	"github.com/befeast/maestro/internal/server"
	"github.com/befeast/maestro/internal/state"
	"github.com/befeast/maestro/internal/statestore"
	"github.com/befeast/maestro/internal/supervisor"
	"github.com/befeast/maestro/internal/webhook"
	"github.com/befeast/maestro/internal/webhookstore"
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
	// DefaultDrainTimeout bounds the graceful in-process drain the daemon runs on
	// SIGTERM (#761, single-service cutover): it sets SpawnDrain on every flow so
	// no new workers are claimed, then waits for in-flight workers to finish. It
	// is deliberately UNDER the unit's TimeoutStopSec so the drain finishes
	// before systemd escalates to SIGKILL. The 5m default covers the vast
	// majority of in-flight workers; a long-running worker that exceeds it is
	// an external tmux process the daemon should not wait for (#817).
	DefaultDrainTimeout = 5 * time.Minute
)

// drainPollInterval is how often Drain re-reads each flow's state to see whether
// the in-flight worker count has reached zero. A var so tests can shorten it.
var drainPollInterval = 5 * time.Second

// shutdownFlowTimeout is the maximum time stopAll waits for a single flow's
// goroutines (orchestrator, supervisor, watchdog) to exit after context
// cancellation. If a flow hangs beyond this deadline stopAll logs a warning
// and moves on, so the daemon never blocks indefinitely on shutdown (#817).
// A var so tests can shorten it.
var shutdownFlowTimeout = 10 * time.Second

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

	// ApprovalsStore selects the store backing the fleet approve/reject
	// endpoint (#759): "json" (default) keeps the legacy per-project JSON
	// read-merge-write; "sqlite" routes the pending→approved/rejected claim
	// through the transactional claim-once store at ApprovalsDBPath. Empty is
	// treated as json.
	ApprovalsStore string
	// ApprovalsDBPath is the shared SQLite approvals database used when
	// ApprovalsStore is "sqlite".
	ApprovalsDBPath string

	// StateStore selects the store backing the remaining orchestration state —
	// sessions/decisions/backend-health/missions (#760): "json" (default) keeps
	// per-project state.json as the only store; "sqlite" write-through-mirrors
	// every project's state into the shared maestro.db on startup and after each
	// Save so cross-project SQL queries are available. Empty is treated as json.
	StateStore string
	// StateDBPath is the shared SQLite state database used when StateStore is
	// "sqlite" (defaults to the same maestro.db the approvals store uses).
	StateDBPath string

	// WebhookSecretFile is the path to a file holding the GitHub webhook secret
	// (epic #811 phase B, #824). When set and readable, the daemon exposes the
	// inbound webhook ingestion endpoint on the fleet port, validating
	// X-Hub-Signature-256 against this secret. Only the PATH is configured here;
	// the secret is read from disk at startup, never logged, and never stored in
	// the config store. Empty disables ingestion.
	WebhookSecretFile string
	// WebhookPath is the endpoint path served under the fleet port (default
	// webhook.DefaultPath). Configurable so operators can obscure it or align it
	// with a reverse-proxy route.
	WebhookPath string
	// WebhookDBPath is the shared SQLite database webhook deliveries land in
	// (defaults to the same maestro.db the state/approvals stores use).
	WebhookDBPath string
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

	// mirror is the shared GitHub read-model mirror (#825/#826). Opened once in
	// Run when webhook ingestion is configured — the ingestor projects accepted
	// deliveries into it, and the per-flow mirror-first read sources serve the
	// supervisor/orchestrator poll loops from it. nil when ingestion is disabled,
	// in which case every flow reads GitHub directly (mirror-first has nothing to
	// read from without inbound deliveries).
	mirror        *mirrorstore.Store
	mirrorHorizon time.Duration

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
	// selfDeployWindow is the longest debounce window any flow has requested, so
	// the shared marker debounces on the max — a short-window flow cannot bypass
	// a long-window flow's in-flight deploy (#758). Guarded by selfDeployMu.
	selfDeployWindow time.Duration
	// fleetAuthTokenEnv is the env var of the fleet's single shared auth token
	// (server.FleetAuthFromProjects over the startup project set), used to
	// authenticate the post-merge self-deploy health probe against /api/v1/fleet
	// (#758). Set once in Run before flows start, then read-only.
	fleetAuthTokenEnv string

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
		// #826: build the flow's mirror-first read source (nil when the mirror is
		// disabled). Its escape hatch reads getCfg() — the holder the reload pump
		// swaps — so a config-store edit flips the supervisor between mirror-first
		// and API-direct live.
		var reader supervisor.Reader
		if src := d.newReadSource(getCfg().Repo, getCfg); src != nil {
			reader = src
		}
		runSupervise(ctx, name, getCfg, reader, opts.SuperviseInterval)
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
	// importCfgs mirrors the deduped project configs so the startup state import
	// (#760) and the fleet wiring see exactly the projects that became flows.
	importCfgs := make([]*config.Config, 0, len(named))
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
		importCfgs = append(importCfgs, cfg)
	}

	// One FleetServer for the whole fleet — replaces the N per-project
	// server.New instances the legacy run/serve paths spun up (#516).
	fleet := server.NewFleet(projects, d.opts.Host, d.opts.Port, d.opts.ReadOnly)
	// Back the dashboard project-CRUD endpoint (#761) with the SAME config store
	// the daemon reads, when the store supports the write API. A UI add/remove
	// then writes the store and the --watch-store diff-loop reconciles it into a
	// started/drained flow. The in-memory test loader does not satisfy the
	// interface, so the endpoint reports 501 there.
	if ps, ok := d.store.(server.FleetProjectStore); ok {
		fleet.SetProjectStore(ps)
	}
	approvalsMode, err := approvalstore.ParseMode(d.opts.ApprovalsStore)
	if err != nil {
		return err
	}
	if err := fleet.SetApprovalStore(approvalsMode, d.opts.ApprovalsDBPath); err != nil {
		return fmt.Errorf("open approvals store %s: %w", d.opts.ApprovalsDBPath, err)
	}
	stateMode, err := statestore.ParseMode(d.opts.StateStore)
	if err != nil {
		return err
	}
	if err := fleet.SetStateStore(stateMode, d.opts.StateDBPath); err != nil {
		return fmt.Errorf("open state store %s: %w", d.opts.StateDBPath, err)
	}
	// One-shot startup import (#760): mirror each project's existing state.json
	// into the shared maestro.db so a cross-project query is answered from
	// SQLite from the first tick. Idempotent — ImportState replaces a project's
	// rows wholesale, so a re-run (daemon restart) re-converges. A per-project
	// import failure is non-fatal: JSON stays authoritative and the fleet read
	// path re-mirrors on its next snapshot.
	if store := fleet.StateStore(); store != nil {
		// keep is the CURRENT fleet's state dirs, built from importCfgs regardless
		// of per-project import success, so a transient load/import error below
		// never prunes a live project's rows.
		keep := make([]string, 0, len(importCfgs))
		for _, cfg := range importCfgs {
			keep = append(keep, cfg.StateDir)
			st, lerr := state.Load(cfg.StateDir)
			if lerr != nil {
				log.Printf("[daemon] state import skip (repo=%s state_dir=%s): load failed: %v", cfg.Repo, cfg.StateDir, lerr)
				continue
			}
			rb := statestore.RowBinding{Project: cfg.Repo, Repo: cfg.Repo, StateDir: cfg.StateDir}
			if ierr := store.ImportState(ctx, rb, st); ierr != nil {
				log.Printf("[daemon] state import failed (repo=%s state_dir=%s): %v", cfg.Repo, cfg.StateDir, ierr)
				continue
			}
			log.Printf("[daemon] imported state.json → maestro.db (repo=%s state_dir=%s, %d sessions)", cfg.Repo, cfg.StateDir, len(st.Sessions))
		}
		// Drop rows for projects no longer in the fleet (removed from the config
		// store while the daemon was stopped). Without this, a cross-project query
		// keeps returning sessions/health for a project that is not running (#760).
		if perr := store.RetainStateDirs(ctx, keep); perr != nil {
			log.Printf("[daemon] state prune (drop removed-project rows) failed: %v", perr)
		}
	}
	// #824: expose the inbound GitHub webhook ingestion endpoint on the fleet
	// port when a webhook secret file is configured. Non-fatal on any error —
	// the daemon logs it loudly and comes up without ingestion (the fleet keeps
	// polling), rather than aborting startup over an unreadable secret.
	d.configureWebhookIngestion(fleet)

	fleetAuth := server.FleetAuthFromProjects(projects)
	fleet.SetAuth(fleetAuth)
	// Capture the fleet's shared auth token env so the self-deploy health probe
	// authenticates against /api/v1/fleet with the SAME token the server enforces
	// — not the triggering flow's, which may differ or be empty (#758). Set here,
	// before any flow starts, so RequestSelfDeploy reads it race-free.
	d.fleetAuthTokenEnv = strings.TrimSpace(fleetAuth.TokenEnv)

	// #823: configure GitHub App installation-token auth process-wide before any
	// flow issues a GitHub call. The token source is a package-level singleton
	// (all flows share one gh wrapper and one rate-limit bucket), so the first
	// project carrying a complete github_app block wins. When no project sets it,
	// or the initial token fetch fails, the daemon keeps the PAT/`gh` path and
	// logs which bucket is in use.
	configureFleetGitHubAppAuth(projects)

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
		// Don't block indefinitely on fleet server shutdown: the server's
		// Shutdown goroutine has a 5s timeout, but bound the read so a
		// pathological case never keeps the process alive (#817).
		select {
		case err := <-fleetErr:
			return err
		case <-time.After(shutdownFlowTimeout):
			return nil
		}
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

// configureFleetGitHubAppAuth arms process-wide GitHub App installation-token
// auth from the first project whose github_app block is complete (#823). The
// gh wrapper's token source is a singleton shared by every flow, so a fleet
// with an org-wide App configures it once here. A configuration or initial
// fetch failure is non-fatal: the daemon logs it loudly and keeps the PAT/`gh`
// path, so a bad App block degrades to today's behavior rather than blocking
// startup.
func configureFleetGitHubAppAuth(projects []server.FleetProject) {
	for i := range projects {
		cfg := projects[i].Cfg()
		if cfg == nil || !cfg.GitHubApp.Configured() {
			continue
		}
		app := cfg.GitHubApp
		if err := github.ConfigureAppAuth(app.AppID, app.InstallationID, app.PrivateKeyPath); err != nil {
			log.Printf("[daemon] github app auth setup failed for %s (staying on PAT/gh): %v", cfg.Repo, err)
			return
		}
		log.Printf("[daemon] github app auth active (app_id=%d installation_id=%d) — daemon GitHub calls use an installation token", app.AppID, app.InstallationID)
		return
	}
	log.Printf("[daemon] github app auth not configured — using PAT/gh for GitHub calls")
}

// configureWebhookIngestion wires the inbound GitHub webhook ingestion endpoint
// (#824) onto the fleet server when opts.WebhookSecretFile is set. It reads the
// secret from disk (never logging it), opens the delivery store in the shared
// maestro.db, and hands the fleet an ingestor plus the configured path.
//
// Every failure is non-fatal: a blank/unreadable/empty secret file, or a store
// that will not open, disables ingestion with a loud warning instead of
// blocking daemon startup — webhooks are an optimisation over polling, so their
// absence degrades to today's polling behaviour.
func (d *Daemon) configureWebhookIngestion(fleet *server.FleetServer) {
	opts := d.opts
	secretPath := strings.TrimSpace(opts.WebhookSecretFile)
	if secretPath == "" {
		log.Printf("[daemon] webhook ingestion not configured (no --webhook-secret-file) — GitHub state continues to arrive by polling")
		return
	}
	secret, err := readWebhookSecret(secretPath)
	if err != nil {
		log.Printf("[daemon] webhook ingestion disabled: %v", err)
		return
	}

	dbPath := strings.TrimSpace(opts.WebhookDBPath)
	if dbPath == "" {
		dbPath = webhookstore.DefaultDBPath()
	}
	store, err := webhookstore.Open(dbPath)
	if err != nil {
		log.Printf("[daemon] webhook ingestion disabled: open delivery store %s: %v", dbPath, err)
		return
	}

	ingestor := webhook.NewIngestor(store, secret)

	// Wire the GitHub mirror read model (#825, phase C). It lives in the same
	// maestro.db as the raw deliveries; the ingestor projects each accepted
	// delivery into its normalised tables, and the fleet snapshot exposes its
	// counts/staleness diagnostics. A mirror that will not open is non-fatal:
	// ingestion still lands raw deliveries (phase B behaviour), the mirror just
	// stays empty until the next start.
	if mirror, err := mirrorstore.Open(dbPath); err != nil {
		log.Printf("[daemon] github mirror disabled: open mirror store %s: %v (raw deliveries still land)", dbPath, err)
	} else {
		ingestor.SetProjector(mirror)
		fleet.SetMirrorStore(mirror, mirrorstore.DefaultStaleHorizon)
		// Share the SAME handle with the per-flow mirror-first read sources (#826)
		// so the reads a flow serves reflect the deliveries the ingestor just
		// projected. The counters that flow through the source land in the gh
		// wrapper's hourly REST-usage journal line via this hook.
		d.mirror = mirror
		d.mirrorHorizon = mirrorstore.DefaultStaleHorizon
		github.SetMirrorReadDigest(mirrorReadDigestLine)
		log.Printf("[daemon] github mirror active — accepted deliveries project into the read model in %s", dbPath)
	}

	path := strings.TrimSpace(opts.WebhookPath)
	if path == "" {
		path = webhook.DefaultPath
	}
	fleet.SetWebhookIngestor(ingestor, path)
	log.Printf("[daemon] webhook ingestion active — POST %s validates X-Hub-Signature-256 and lands deliveries in %s", path, dbPath)
}

// readWebhookSecret reads and trims the webhook secret from disk. The secret's
// bytes never enter a log line; only errors (which carry the path, not the
// content) are surfaced. An empty file is an operator error worth reporting.
func readWebhookSecret(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read webhook secret file %s: %w", path, err)
	}
	secret := strings.TrimSpace(string(data))
	if secret == "" {
		return "", fmt.Errorf("webhook secret file %s is empty", path)
	}
	return secret, nil
}

// Fleet returns the aggregating FleetServer once Run has built it, or nil
// before then. Exposed for tests and embedders that want the handle.
func (d *Daemon) Fleet() *server.FleetServer {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.fleet
}

// Drain runs the graceful, in-process shutdown the single-service cutover needs
// (#761, epic #754 Phase 6). It is called by the daemon's SIGTERM handler BEFORE
// the daemon context is cancelled: it sets SpawnDrain on every running flow's
// state so each orchestrator stops claiming new issues on its next cycle, then
// waits — the flows are still live, so their orchestrators keep reaping
// finishing workers — until no in-flight workers remain across the whole fleet
// or timeout elapses. It returns either way; the caller then cancels the daemon
// ctx, which tears the (now idle) flows down via stopAll.
//
// There is no per-unit ExecStop=maestro drain anymore: the one maestro.service
// drains in-process. ctx cancellation (a second SIGTERM) aborts the wait early
// so an operator can force shutdown. Non-positive timeout clamps to
// DefaultDrainTimeout.
func (d *Daemon) Drain(ctx context.Context, timeout time.Duration) {
	// Fail fast on GitHub rate limits from here on (#797): the flows keep
	// polling while the drain waits for in-flight workers, and a rate-limited
	// window at that moment must degrade to failed cycles — not minutes of
	// backoff that stall flow stop past the drain deadline (observed
	// 2026-07-02: a 0-worker drain stretched past 20 minutes).
	github.BeginShutdown()

	if timeout <= 0 {
		timeout = DefaultDrainTimeout
	}

	// Snapshot the live flows' state dirs under the lock. Dedup on StateDir so a
	// (degenerate) repeated dir is drained once; the flows map is already keyed
	// on flowKey (StateDir/Repo), but be defensive.
	d.mu.Lock()
	dirs := make([]string, 0, len(d.flows))
	names := make([]string, 0, len(d.flows))
	seen := make(map[string]bool, len(d.flows))
	for _, flow := range d.flows {
		if flow.cfg == nil {
			continue
		}
		dir := strings.TrimSpace(flow.cfg.StateDir)
		if dir == "" || seen[dir] {
			continue
		}
		seen[dir] = true
		dirs = append(dirs, dir)
		names = append(names, flow.name)
	}
	d.mu.Unlock()
	if len(dirs) == 0 {
		return
	}

	// Phase 1: stop new spawns fleet-wide. SetSpawnDrain stamps SpawnDrainAt and
	// the state merge resolves the flag latest-write-wins by that timestamp, so a
	// concurrent orchestrator save cannot un-set the freshly requested drain.
	requested := 0
	for i, dir := range dirs {
		s, err := state.Load(dir)
		if err != nil {
			log.Printf("[daemon] drain: load state for flow %q (%s) failed: %v", names[i], dir, err)
			continue
		}
		s.SetSpawnDrain(time.Now().UTC())
		if err := state.Save(dir, s); err != nil {
			log.Printf("[daemon] drain: set drain flag for flow %q (%s) failed: %v", names[i], dir, err)
			continue
		}
		requested++
	}
	log.Printf("[daemon] drain: requested on %d/%d flow(s) — no new workers; waiting up to %s for in-flight workers to finish", requested, len(dirs), timeout)

	// Phase 2: wait for in-flight workers to finish across every flow.
	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(drainPollInterval)
	defer ticker.Stop()
	lastReported := -1
	for {
		running := 0
		for _, dir := range dirs {
			s, err := state.Load(dir)
			if err != nil {
				continue
			}
			running += s.RunningSessionCount()
		}
		if running != lastReported {
			log.Printf("[daemon] drain: in-flight workers running: %d", running)
			lastReported = running
		}
		if running == 0 {
			log.Printf("[daemon] drain: complete — no in-flight workers remain")
			return
		}
		if !time.Now().Before(deadline) {
			log.Printf("[daemon] drain: timed out after %s with %d worker(s) still running; proceeding with shutdown", timeout, running)
			return
		}
		select {
		case <-ctx.Done():
			log.Printf("[daemon] drain: aborted (forced shutdown) with %d worker(s) still running", running)
			return
		case <-ticker.C:
		}
	}
}
