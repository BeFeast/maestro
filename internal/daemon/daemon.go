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
	"github.com/befeast/maestro/internal/approver"
	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/configwatch"
	"github.com/befeast/maestro/internal/emergencystore"
	"github.com/befeast/maestro/internal/github"
	"github.com/befeast/maestro/internal/mirrorstore"
	"github.com/befeast/maestro/internal/notify"
	"github.com/befeast/maestro/internal/selfdeploy"
	"github.com/befeast/maestro/internal/server"
	"github.com/befeast/maestro/internal/state"
	"github.com/befeast/maestro/internal/statestore"
	"github.com/befeast/maestro/internal/supervisor"
	"github.com/befeast/maestro/internal/webhook"
	"github.com/befeast/maestro/internal/webhookstore"
	"github.com/befeast/maestro/internal/worker"
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
	// DefaultDrainTimeout bounds the ENTIRE graceful SIGTERM shutdown (#761,
	// #966): SpawnDrain propagation, the in-flight worker wait, flow joins,
	// restart markers, and process handoff all fit inside four minutes. The command
	// reserves a small tail inside this budget for teardown, and maestro.service's
	// longer TimeoutStopSec remains only a systemd backstop. A longer-running
	// isolated worker is checkpointed/adopted across restart rather than waited on
	// indefinitely (#817, #891).
	DefaultDrainTimeout = 4 * time.Minute
	// DefaultEmergencyWatchInterval is how often the daemon re-reads the
	// fleet-wide EMERGENCY STOP switch from the unified DB (#840). It is short so
	// the switch takes effect well within one supervise/orchestrate poll interval
	// — the acceptance guarantee is "within one cycle (≤ poll interval)" — while
	// polling one SQLite row is negligible. It runs independently of
	// --watch-store, since the emergency switch must always be honored.
	DefaultEmergencyWatchInterval = 5 * time.Second
	// DefaultTmpfsHygieneInterval is the host-level /tmp apply cadence. It sits
	// inside the requested 5–10 minute window and is independent of project
	// orchestration/supervisor polling.
	DefaultTmpfsHygieneInterval = 10 * time.Minute
)

// drainPollInterval is how often Drain re-reads each flow's state to see whether
// the in-flight worker count has reached zero. A var so tests can shorten it.
var drainPollInterval = 5 * time.Second

func runtimeIntervalSeconds(interval time.Duration) int {
	if interval <= 0 {
		return 0
	}
	seconds := int(interval / time.Second)
	if seconds == 0 {
		return 1
	}
	return seconds
}

// shutdownFlowTimeout is the maximum time stopAll waits for a single flow's
// goroutines (orchestrator, supervisor, watchdog) to exit after context
// cancellation. If a flow hangs beyond this deadline stopAll logs a warning
// and moves on, so the daemon never blocks indefinitely on shutdown (#817).
// A var so tests can shorten it.
var shutdownFlowTimeout = 10 * time.Second

// shutdownCheckpointBudget caps the wall-clock the shutdown restart-checkpoint
// pass (#877) spends generating the best-effort CHECKPOINT.md context blobs
// (which shell out to git per session). Once it elapses, the pass stops
// generating context files but STILL stamps the correctness-critical
// RestartCheckpointAt marker (cheap: a single field + state write), so the
// marker — the thing that actually prevents the false running->dead — always
// lands within the stop window even when many sessions are in-flight (#877
// review comment 1). A var so tests can shorten it.
var shutdownCheckpointBudget = 60 * time.Second

// restartCheckpointRetries bounds how many times the shutdown checkpoint pass
// re-loads and re-applies markers after losing a 3-way state merge to a
// concurrent flow save. stopAll abandons a flow whose RunOnce cycle outruns
// shutdownFlowTimeout, so that still-live flow can Save between this pass's Load
// and Save. On ErrStateConflict the pass reloads (picking up the flow's fresher
// session, re-evaluating its status) and retries, so a late flow save neither
// drops the marker nor gets clobbered by it (#877 review comment 2). A var so
// tests can exercise the retry path deterministically.
var restartCheckpointRetries = 5

// checkpointFn writes the best-effort CHECKPOINT.md context blob for one
// in-flight session and returns its path. It defaults to worker.SaveCheckpoint,
// which shells out to git; a var so tests can inject a fast or deliberately
// wedged stub. It is called through saveCheckpointWithin so a git invocation
// contending with a live worker's index.lock cannot hang the shutdown pass past
// the drain deadline (#966).
var checkpointFn = worker.SaveCheckpoint

// saveCheckpointWithin runs checkpointFn but abandons it if it does not return
// within budget. Checkpoint capture shells out to git against worktrees that are
// still owned by surviving workers, so it must not be allowed to defeat the same
// global shutdown deadline as a wedged flow callback (#966). The marker is
// persisted before this helper runs; the abandoned goroutine is harmless and is
// reaped when the process exits. A non-positive budget skips the optional capture
// entirely (timedOut=true).
func saveCheckpointWithin(sess *state.Session, budget time.Duration) (path string, err error, timedOut bool) {
	if budget <= 0 {
		return "", nil, true
	}
	type result struct {
		path string
		err  error
	}
	ch := make(chan result, 1)
	go func() { p, e := checkpointFn(sess); ch <- result{p, e} }()
	timer := time.NewTimer(budget)
	defer timer.Stop()
	select {
	case r := <-ch:
		return r.path, r.err, false
	case <-timer.C:
		return "", nil, true
	}
}

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
	// refreshCh is the daemon-internal edge trigger for an immediate project
	// reconciliation. It is populated per flow and intentionally unexported:
	// callers configure cadence, while accepted GitHub gate webhooks wake the
	// matching flow without waiting for that cadence.
	refreshCh <-chan struct{}
	// SuperviseInterval is the supervisor decision-loop interval per flow.
	SuperviseInterval time.Duration
	// TmpfsHygieneInterval is the daemon-owned protect-aware /tmp apply cadence.
	// Non-positive values are clamped to DefaultTmpfsHygieneInterval.
	TmpfsHygieneInterval time.Duration

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
	// DrainTimeout is the daemon command's configured whole-shutdown budget. It
	// is also forwarded to centralized self-deploy so the helper's restart-only
	// timeout tracks a non-default --drain-timeout (#966).
	DrainTimeout time.Duration

	// ApprovalsStore selects the store backing the fleet approve/reject
	// endpoint (#759): "json" (default) keeps the legacy per-project JSON
	// read-merge-write; "sqlite" routes the pending→approved/rejected claim
	// through the transactional claim-once store at ApprovalsDBPath. Empty is
	// treated as json.
	ApprovalsStore string
	// ApprovalsDBPath is the shared SQLite approvals database used by the
	// generic gate when ApprovalsStore is "sqlite" and by deploy_project in
	// every mode (delivery always requires a durable execution claim).
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

	// EmergencyDBPath is the shared SQLite database the fleet-wide EMERGENCY STOP
	// switch lives in (#840), defaulting to the same maestro.db the other stores
	// use. The daemon reads it every EmergencyWatchInterval so an operator's
	// `maestro emergency stop-llm` takes effect within one cycle without a
	// restart, and it is re-read on startup so a switch set while the daemon was
	// down is honored on the next start.
	EmergencyDBPath string
}

// Daemon owns the running project flows and the shared FleetServer.
type Daemon struct {
	store ConfigLoader
	opts  Options

	// shutdownDeadline is the absolute deadline established by the daemon signal
	// handler for the whole graceful shutdown. Drain, flow joins, restart
	// checkpointing, and HTTP teardown all share it, so work after the worker
	// drain cannot extend a systemd restart past the advertised budget (#966).
	shutdownMu        sync.RWMutex
	shutdownDeadline  time.Time
	shutdownStateDirs []string

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

	// emergency is the fleet-wide EMERGENCY STOP switch store (#840), opened once
	// in Run against the unified maestro.db. emergencyState caches the last read
	// so the per-flow gate closures (supervisor LLM halt, orchestrator spawn
	// gate) resolve without a DB hit per cycle; the emergency-watch loop refreshes
	// it every EmergencyWatchInterval. emergencyNotifier sends the notify_red-grade
	// alert on activation/resume, built from the first project's Telegram config.
	// nil emergency (open failed / test loader) leaves every gate inert.
	emergency         *emergencystore.Store
	emergencyMu       sync.RWMutex
	emergencyState    emergencystore.State
	emergencyNotifier *notify.Notifier
	// floorNotifier is selected independently of the emergency switch store so
	// an unavailable safety DB cannot silence the fleet-capacity CRITICAL alert.
	floorNotifier *notify.Notifier

	// spawnLimiter owns the daemon-wide live-worker budget. It is shared by all
	// project orchestrators so concurrent flows reserve against one atomic max.
	spawnLimiter *fleetSpawnLimiter

	// tmpfsHygiene owns the host-level scheduled sweep and latest Fleet-facing
	// summary. It is independent of per-project flow state.
	tmpfsHygiene *tmpfsHygieneRuntime

	// runLoop, superviseLoop, watchdogLoop, and materialProgressLoop build the
	// per-project loops.
	// They default to the production orchestrator + supervisor wiring; tests
	// override them to drive flows (and assert WaitGroup tracking) without
	// reaching GitHub. runLoop receives the orchestrator's hot-reload channel
	// (#757), nil when store-watch is disabled. superviseLoop receives a getCfg
	// closure rather than a fixed *config.Config so it reads the flow's current
	// config each cycle through the shared holder — a config-store edit reaches
	// the supervise loop live, not just the orchestrator (#768).
	//
	// kickCh is shared between superviseLoop and watchdogLoop: when the
	// watchdog detects 2+ consecutive stuck ticks it sends a kick signal
	// on this channel so the supervise loop aborts its in-flight RunOnce
	// and starts a fresh cycle (#816).
	runLoop              func(ctx context.Context, cfg *config.Config, opts Options, reloadCh <-chan *config.Config)
	superviseLoop        func(ctx context.Context, name string, getCfg func() *config.Config, opts Options, kickCh <-chan struct{})
	watchdogLoop         func(ctx context.Context, name, stateDir string, interval time.Duration, kickCh chan<- struct{})
	materialProgressLoop func(ctx context.Context, name string, getCfg func() *config.Config)
}

// SetShutdownDeadline records the absolute deadline for the current daemon
// shutdown. Repeated calls may shorten but never extend it, so a forced/second
// shutdown request cannot accidentally grant teardown more time (#966).
func (d *Daemon) SetShutdownDeadline(deadline time.Time) {
	if deadline.IsZero() {
		return
	}
	d.shutdownMu.Lock()
	if d.shutdownDeadline.IsZero() || deadline.Before(d.shutdownDeadline) {
		d.shutdownDeadline = deadline
	}
	d.shutdownMu.Unlock()
}

func (d *Daemon) shutdownDeadlineOr(fallback time.Time) time.Time {
	d.shutdownMu.RLock()
	deadline := d.shutdownDeadline
	d.shutdownMu.RUnlock()
	if deadline.IsZero() {
		return fallback
	}
	return deadline
}

func (d *Daemon) rememberShutdownStateDirs(dirs []string) {
	if len(dirs) == 0 {
		return
	}
	d.shutdownMu.Lock()
	seen := make(map[string]bool, len(d.shutdownStateDirs)+len(dirs))
	for _, dir := range d.shutdownStateDirs {
		seen[dir] = true
	}
	for _, dir := range dirs {
		if dir != "" && !seen[dir] {
			d.shutdownStateDirs = append(d.shutdownStateDirs, dir)
			seen[dir] = true
		}
	}
	d.shutdownMu.Unlock()
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
	if opts.TmpfsHygieneInterval <= 0 {
		opts.TmpfsHygieneInterval = DefaultTmpfsHygieneInterval
	}
	if opts.WatchStore && opts.WatchStoreInterval <= 0 {
		log.Printf("[daemon] watch-store-interval %s is not positive; clamping to default %s", opts.WatchStoreInterval, DefaultWatchStoreInterval)
		opts.WatchStoreInterval = DefaultWatchStoreInterval
	}
	if opts.DrainTimeout <= 0 {
		opts.DrainTimeout = DefaultDrainTimeout
	}
	d := &Daemon{
		store:        store,
		opts:         opts,
		flows:        make(map[string]*projectFlow),
		spawnLimiter: newFleetSpawnLimiter(store),
	}
	d.tmpfsHygiene = newTmpfsHygieneRuntime(d)
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
	d.superviseLoop = func(ctx context.Context, name string, getCfg func() *config.Config, opts Options, kickCh <-chan struct{}) {
		// #826: build the flow's mirror-first read source (nil when the mirror is
		// disabled). Its escape hatch reads getCfg() — the holder the reload pump
		// swaps — so a config-store edit flips the supervisor between mirror-first
		// and API-direct live.
		var reader supervisor.Reader
		if src := d.newReadSource(getCfg().Repo, getCfg); src != nil {
			reader = src
		}
		runSupervise(ctx, name, getCfg, reader, opts.SuperviseInterval, opts.ApprovalsDBPath, d.emergencyLLMHalt, kickCh)
	}
	d.watchdogLoop = supervisor.Watchdog
	d.materialProgressLoop = supervisor.RunMaterialProgressEvaluator
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
	if len(named) == 0 && d.projectStore == nil {
		return errors.New("config store has no projects")
	}
	if len(named) == 0 {
		log.Printf("[daemon] config store has no projects yet — serving an empty fleet and waiting for --watch-store hot-add")
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
		project := server.NewFleetProjectWithGitHubNamed(name, cfg)
		wireApprovalExecutor(&project)
		projects = append(projects, project)
		storeNames = append(storeNames, nc.name)
		importCfgs = append(importCfgs, cfg)
	}

	// One FleetServer for the whole fleet — replaces the N per-project
	// server.New instances the legacy run/serve paths spun up (#516).
	fleet := server.NewFleet(projects, d.opts.Host, d.opts.Port, d.opts.ReadOnly)
	fleet.SetTmpfsHygieneSource(d.tmpfsHygieneSummary)
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
	// The fleet floor is daemon-wide and must retain an ntfy route even if the
	// independent emergency-switch store cannot be opened.
	d.floorNotifier = fleetFloorNotifierFor(importCfgs)

	// #840: open the fleet-wide EMERGENCY STOP switch, seed the gate cache from
	// the current value (so a switch set while the daemon was down is honored on
	// this start), and expose it on the fleet snapshot. Non-fatal on any error —
	// the gate is a safety brake over the normal flows. Closed when Run returns.
	if store := d.configureEmergencyStop(ctx, importCfgs); store != nil {
		defer store.Close()
		fleet.SetEmergencySource(d.EmergencyState)
		// Back the dashboard red button (#840): the SPA POST writes the same
		// switch store the CLI does, and the daemon's emergency-watch loop picks it
		// up within one cycle. The notifier sends the notify_red-grade alert for
		// dashboard-driven changes (the CLI sends its own for CLI-driven ones).
		fleet.SetEmergencyStore(store)
		if d.emergencyNotifier != nil {
			fleet.SetEmergencyNotifier(d.emergencyNotifier)
		}
	}

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
		if cfg := projects[i].Cfg(); cfg != nil {
			if d.spawnLimiter != nil {
				d.spawnLimiter.RegisterProject(cfg.StateDir, cfg.Repo, d.opts.SuperviseInterval)
			}
		}
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
	// EMERGENCY STOP watch (#840): re-read the fleet-wide switch every interval so
	// an operator's `maestro emergency stop-llm` takes effect within one cycle
	// without a restart, and announce each transition (notify + per-flow journal
	// line). It runs unconditionally (not gated on --watch-store) since the
	// switch must always be honored, under its own context so it stops on a serve
	// error as well as ctx.Done.
	var stopEmergencyWatch func()
	if d.emergency != nil {
		ectx, emergencyCancel := context.WithCancel(ctx)
		emergencyDone := make(chan struct{})
		go func() {
			defer close(emergencyDone)
			d.watchEmergencyLoop(ectx, DefaultEmergencyWatchInterval)
		}()
		log.Printf("[daemon] EMERGENCY STOP watch enabled — poll every %s", DefaultEmergencyWatchInterval)
		stopEmergencyWatch = func() {
			emergencyCancel()
			<-emergencyDone
		}
	}
	// #1106: CRITICAL floor breach watch — live < fleet.min_live_workers must
	// ntfy the operator without waiting for a status poll.
	var stopFloorWatch func()
	if d.spawnLimiter != nil {
		fctx, floorCancel := context.WithCancel(ctx)
		floorDone := make(chan struct{})
		go func() {
			defer close(floorDone)
			d.watchFloorBreachLoop(fctx, DefaultFloorWatchInterval)
		}()
		log.Printf("[daemon] floor breach watch enabled — poll every %s (CRITICAL when live < min)", DefaultFloorWatchInterval)
		stopFloorWatch = func() {
			floorCancel()
			<-floorDone
		}
	}
	// Protect-aware /tmp sweep: one host-level apply loop for the single daemon,
	// not one per project. The first run occurs after one interval, avoiding a
	// surprise startup prune while still bounding abandoned residue to 10m past
	// its age gate. Each result is emitted as one JSON line and published to Fleet.
	var stopTmpfsHygiene func()
	if d.tmpfsHygiene != nil {
		hctx, hygieneCancel := context.WithCancel(ctx)
		hygieneDone := make(chan struct{})
		go func() {
			defer close(hygieneDone)
			d.tmpfsHygieneLoop(hctx, d.opts.TmpfsHygieneInterval)
		}()
		log.Printf("[daemon] tmpfs hygiene enabled — apply every %s", d.opts.TmpfsHygieneInterval)
		stopTmpfsHygiene = func() {
			hygieneCancel()
			<-hygieneDone
		}
	}
	drainWatch := func() {
		if stopWatch != nil {
			stopWatch()
		}
		if stopEmergencyWatch != nil {
			stopEmergencyWatch()
		}
		if stopFloorWatch != nil {
			stopFloorWatch()
		}
		if stopTmpfsHygiene != nil {
			stopTmpfsHygiene()
		}
	}

	log.Printf("[daemon] serving fleet — projects=%d addr=%s:%d read_only=%v", len(projects), d.opts.Host, d.opts.Port, d.opts.ReadOnly)
	fleetErr := make(chan error, 1)
	go func() { fleetErr <- fleet.Serve(ctx, ln) }()

	select {
	case <-ctx.Done():
		drainWatch()
		// A SIGTERM-driven shutdown installs one absolute deadline before ctx is
		// cancelled. Other embedders/tests that cancel directly keep the historical
		// bounded stop + checkpoint allowance.
		deadline := d.shutdownDeadlineOr(time.Now().Add(shutdownFlowTimeout + shutdownCheckpointBudget))
		// Snapshot flow state dirs before stopAll clears the flow registry, then
		// checkpoint any worker still in-flight so it resumes exactly once after
		// the restart instead of a false running->dead (#877).
		dirs := d.flowStateDirs()
		d.checkpointInFlightForRestart(checkpointDeadline(deadline), dirs)
		d.stopAllUntil(deadline)
		// Don't block indefinitely on fleet server shutdown: the server's
		// Shutdown goroutine has a 5s timeout, but the read shares the same global
		// shutdown deadline so it cannot extend a restart (#817, #966).
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return nil
		}
		timer := time.NewTimer(remaining)
		defer timer.Stop()
		select {
		case err := <-fleetErr:
			return err
		case <-timer.C:
			return nil
		}
	case err := <-fleetErr:
		// Serve returned before shutdown — a runtime serve error after a
		// successful bind (rare; the bind failure itself was already handled by
		// Listen above). Flows are legitimately running, so drain them.
		drainWatch()
		deadline := d.shutdownDeadlineOr(time.Now().Add(shutdownFlowTimeout + shutdownCheckpointBudget))
		dirs := d.flowStateDirs()
		d.checkpointInFlightForRestart(checkpointDeadline(deadline), dirs)
		d.stopAllUntil(deadline)
		if err != nil {
			log.Printf("[daemon] fleet server failed on %s:%d: %v", d.opts.Host, d.opts.Port, err)
			return err
		}
		<-ctx.Done()
		return nil
	}
}

func checkpointDeadline(shutdownDeadline time.Time) time.Time {
	now := time.Now()
	budget := shutdownCheckpointBudget
	if !shutdownDeadline.IsZero() {
		remaining := time.Until(shutdownDeadline)
		// CHECKPOINT.md is optional; keep at least half of the remaining global
		// tail available for flow detach logs and HTTP/process handoff (#966).
		if half := remaining / 2; half < budget {
			budget = half
		}
	}
	if budget < 0 {
		budget = 0
	}
	return now.Add(budget)
}

// wireApprovalExecutor gives a fleet project the same in-process approval
// executor used by the supervisor, but bound to the state snapshot loaded by
// the authenticated fleet request. Keeping one executor per project preserves
// its per-approval concurrency fence while the FleetProject mutex serializes
// the state pointer updates below.
func wireApprovalExecutor(project *server.FleetProject) {
	if project == nil {
		return
	}
	cfg := project.Cfg()
	executor := &approver.Executor{
		Cfg:     cfg,
		Workers: supervisor.NewWorkerController(cfg),
	}
	project.SetApprovalExecutor(func(st *state.State, approval state.Approval) server.ApprovalExecutionResult {
		if st == nil {
			return server.ApprovalExecutionResult{
				Status:  state.ApprovalStatusExecutionFailed,
				Summary: "stop_worker immediate executor received nil state",
			}
		}
		executor.Sessions = approver.SessionLookupFunc(st.SessionAt)
		executor.State = st
		result := executor.Execute(&approval)
		return server.ApprovalExecutionResult{Status: result.Status, Summary: result.Summary}
	})
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
	ingestor.SetAfterAccepted(d.refreshPRGateFromWebhook)

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

// refreshPRGateFromWebhook coalesces accepted GitHub gate updates into one
// immediate orchestrator cycle for every flow serving that repository. The
// channel is buffered and the send is non-blocking, so a burst of check-run
// updates cannot queue concurrent RunOnce calls or delay the webhook response.
func (d *Daemon) refreshPRGateFromWebhook(eventType, repo string) {
	switch strings.TrimSpace(eventType) {
	case "check_run", "check_suite", "status":
	default:
		return
	}
	repo = strings.TrimSpace(repo)
	if repo == "" {
		return
	}

	d.mu.Lock()
	channels := make([]chan struct{}, 0, 1)
	for _, flow := range d.flows {
		if flow == nil || flow.cfg == nil || flow.refreshCh == nil || !strings.EqualFold(strings.TrimSpace(flow.cfg.Repo), repo) {
			continue
		}
		channels = append(channels, flow.refreshCh)
	}
	d.mu.Unlock()

	for _, refreshCh := range channels {
		select {
		case refreshCh <- struct{}{}:
		default:
		}
	}
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
	if timeout <= 0 {
		timeout = DefaultDrainTimeout
	}
	d.DrainUntil(ctx, time.Now().Add(timeout))
}

// DrainUntil is Drain with an absolute deadline. The signal path uses it so
// time spent setting drain flags is part of the same global shutdown budget as
// the worker wait and post-drain teardown (#966).
func (d *Daemon) DrainUntil(ctx context.Context, deadline time.Time) {
	// Fail fast on GitHub rate limits from here on (#797): the flows keep
	// polling while the drain waits for in-flight workers, and a rate-limited
	// window at that moment must degrade to failed cycles — not minutes of
	// backoff that stall flow stop past the drain deadline (observed
	// 2026-07-02: a 0-worker drain stretched past 20 minutes).
	github.BeginShutdown()

	if deadline.IsZero() {
		deadline = time.Now().Add(DefaultDrainTimeout)
	}
	started := time.Now()

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
	// Preserve this pre-cancel snapshot. Once the parent daemon context is
	// cancelled, responsive flows deregister immediately; reconstructing dirs
	// from the live registry in Run teardown would then miss their still-running
	// isolated workers and fail to checkpoint them (#966).
	d.rememberShutdownStateDirs(dirs)
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
		// Record that this drain is part of an actual daemon shutdown. The
		// orchestrator uses this persisted cause marker to preserve only a
		// shutdown-interrupted process loss across restart; a standalone
		// `maestro drain` must not revive an unrelated crash (#967).
		s.SetShutdownDrain(time.Now().UTC())
		if err := state.Save(dir, s); err != nil {
			log.Printf("[daemon] drain: set drain flag for flow %q (%s) failed: %v", names[i], dir, err)
			continue
		}
		requested++
	}
	remaining := time.Until(deadline)
	if remaining < 0 {
		remaining = 0
	}
	log.Printf("[daemon] drain: requested on %d/%d flow(s) — no new workers; waiting up to %s more for in-flight workers to finish", requested, len(dirs), remaining.Round(time.Millisecond))

	// Phase 2: wait for in-flight workers to finish across every flow.
	ticker := time.NewTicker(drainPollInterval)
	defer ticker.Stop()
	deadlineTimer := time.NewTimer(remaining)
	defer deadlineTimer.Stop()
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
		select {
		case <-deadlineTimer.C:
			log.Printf("[daemon] drain: timed out after %s with %d worker(s) still running; proceeding with bounded shutdown", time.Since(started).Round(time.Millisecond), running)
			return
		default:
		}
		select {
		case <-ctx.Done():
			log.Printf("[daemon] drain: aborted (forced shutdown) with %d worker(s) still running", running)
			return
		case <-deadlineTimer.C:
			log.Printf("[daemon] drain: timed out after %s with %d worker(s) still running; proceeding with bounded shutdown", time.Since(started).Round(time.Millisecond), running)
			return
		case <-ticker.C:
		}
	}
}

// flowStateDirs returns the deduped per-flow state dirs of the currently
// registered flows. It is captured BEFORE stopAll (which clears the flow
// registry) so the shutdown restart-checkpoint pass (#877) knows which project
// states to scan.
func (d *Daemon) flowStateDirs() []string {
	d.mu.Lock()
	dirs := make([]string, 0, len(d.flows))
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
	}
	d.mu.Unlock()

	d.shutdownMu.RLock()
	remembered := append([]string(nil), d.shutdownStateDirs...)
	d.shutdownMu.RUnlock()
	for _, dir := range remembered {
		if dir == "" || seen[dir] {
			continue
		}
		seen[dir] = true
		dirs = append(dirs, dir)
	}
	return dirs
}

// checkpointInFlightForRestart records a deliberate, resumable checkpoint for
// every worker still in-flight at daemon shutdown (#877). It is the safety net
// that makes a self-deploy/operator restart honor drain semantics even when a
// worker did not finish within the drain window.
//
// Timing: isolated worker scopes survive maestro.service while the daemon drains
// and restarts (#891). The marker lets the replacement daemon prove the restart
// was deliberate and either adopt that same surviving PID/worktree or, for a
// legacy/non-isolated worker that did exit, resume the same logical session in
// place exactly once. CHECKPOINT.md remains best-effort diagnostic context.
//
// It runs after daemon-context cancellation but BEFORE waiting on stopAll, so a
// non-returning flow cannot consume the deadline before markers are durable. A
// still-finishing RunOnce may Save concurrently, so this pass does Load/stamp/Save with a
// retry-on-conflict loop (checkpointDir): a lost 3-way merge reloads and
// re-evaluates, so the marker is never dropped and a late flow save is never
// clobbered (#877 review comment 2). dirs must be captured (flowStateDirs) before
// stopAll clears the flow registry. It is deliberately conservative: it never
// kills or replays anything (P1 #877 review — no destructive action on
// shutdown), only persists a resumable marker (plus a best-effort context blob).
//
// deadline caps the wall-clock spent on the best-effort CHECKPOINT.md context
// blobs; the marker itself is always stamped, so the correctness-critical part
// completes even under stop-window pressure (#877 review comment 1).
func (d *Daemon) checkpointInFlightForRestart(deadline time.Time, dirs []string) {
	now := time.Now().UTC()
	type contextPass struct {
		dir    string
		state  *state.State
		marked []string
	}
	contexts := make([]contextPass, 0, len(dirs))
	// First sweep: persist markers across EVERY flow before any optional git
	// capture can consume the remaining shutdown budget (#966).
	for _, dir := range dirs {
		s, marked := d.checkpointDirForRestart(dir, now)
		if len(marked) > 0 {
			contexts = append(contexts, contextPass{dir: dir, state: s, marked: marked})
		}
	}
	// Second sweep: add best-effort context only after all correctness markers
	// are durable. A wedged capture can no longer starve another flow's marker.
	for _, pass := range contexts {
		d.saveRestartCheckpointContext(pass.dir, pass.state, pass.marked, deadline)
	}
}

// checkpointDirForRestart stamps every correctness-critical resumable marker
// before attempting any best-effort CHECKPOINT.md capture. A single wedged git
// call must never consume the global shutdown tail and leave later workers
// unmarked (#966). The marker save retries on a lost 3-way merge so a still-live,
// detached flow neither drops the marker nor has fresher state overwritten.
func (d *Daemon) checkpointDirForRestart(dir string, now time.Time) (*state.State, []string) {
	for attempt := 0; attempt < restartCheckpointRetries; attempt++ {
		s, err := state.Load(dir)
		if err != nil {
			log.Printf("[daemon] restart-checkpoint: load state %s failed: %v", dir, err)
			return nil, nil
		}
		marked := make([]string, 0)
		for slot, sess := range s.Sessions {
			if sess == nil || sess.Status != state.StatusRunning {
				continue
			}
			// Idempotent across a repeated shutdown pass (e.g. the fleetErr and
			// ctx.Done paths both firing) and across a merge retry: a marker
			// already set is left as-is.
			if sess.RestartCheckpointAt != nil {
				continue
			}
			// Only a session whose dirty worktree is still on disk can be resumed
			// in place. A missing worktree has nothing to preserve, so leave it to
			// the normal reconcile/retry path rather than stamping a marker the
			// resume could not honor.
			if strings.TrimSpace(sess.Worktree) == "" {
				continue
			}
			if _, statErr := os.Stat(sess.Worktree); statErr != nil {
				continue
			}
			stamp := now
			sess.RestartCheckpointAt = &stamp
			marked = append(marked, slot)
			log.Printf("[daemon] restart-checkpoint: %s (issue #%d) marked resumable across restart — worktree preserved for exactly-once in-place resume", slot, sess.IssueNumber)
		}
		if len(marked) == 0 {
			return nil, nil
		}
		err = state.Save(dir, s)
		if err == nil {
			return s, marked
		}
		if errors.Is(err, state.ErrStateConflict) && attempt < restartCheckpointRetries-1 {
			log.Printf("[daemon] restart-checkpoint: save %s lost a merge to a concurrent flow save; reloading and retrying (attempt %d/%d)", dir, attempt+1, restartCheckpointRetries)
			continue
		}
		log.Printf("[daemon] restart-checkpoint: save state %s failed: %v", dir, err)
		return nil, nil
	}
	return nil, nil
}

// saveRestartCheckpointContext adds the optional CHECKPOINT.md paths after all
// resumable markers for this state dir are durable. Timeout or save conflict is
// harmless: the replacement daemon only needs RestartCheckpointAt for correct,
// exactly-once reconciliation; the context file is diagnostic assistance.
func (d *Daemon) saveRestartCheckpointContext(dir string, s *state.State, marked []string, deadline time.Time) {
	changed := false
	for _, slot := range marked {
		sess := s.Sessions[slot]
		if sess == nil || sess.CheckpointFile != "" {
			continue
		}
		cp, cpErr, timedOut := saveCheckpointWithin(sess, time.Until(deadline))
		if timedOut {
			log.Printf("[daemon] restart-checkpoint: %s (issue #%d) — shutdown budget spent or checkpoint git wedged; resumable marker is already durable without CHECKPOINT.md context", slot, sess.IssueNumber)
			continue
		}
		if cpErr != nil {
			log.Printf("[daemon] restart-checkpoint: save checkpoint for %s (issue #%d) failed: %v", slot, sess.IssueNumber, cpErr)
			continue
		}
		if cp != "" {
			sess.CheckpointFile = cp
			changed = true
		}
	}
	if !changed {
		return
	}
	if err := state.Save(dir, s); err != nil {
		log.Printf("[daemon] restart-checkpoint: save best-effort CHECKPOINT.md paths for %s failed: %v", dir, err)
	}
}
