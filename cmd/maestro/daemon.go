package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/befeast/maestro/internal/approvalstore"
	"github.com/befeast/maestro/internal/configstore"
	"github.com/befeast/maestro/internal/daemon"
	"github.com/befeast/maestro/internal/emergencystore"
	"github.com/befeast/maestro/internal/statestore"
	"github.com/befeast/maestro/internal/webhook"
	"github.com/befeast/maestro/internal/webhookstore"
)

// daemonCmd runs every project in the config store as one long-lived process:
// an orchestrator + supervisor loop per project flow, plus a single
// FleetServer aggregating them all (#756, epic #754). Legacy per-project units
// and a separate `maestro serve` process are migration sources, not peers to a
// running daemon.
func daemonCmd(args []string) {
	fs := flag.NewFlagSet("daemon", flag.ExitOnError)
	storePath := fs.String("store", defaultConfigStorePath(), "Path to SQLite config store")
	runInterval := fs.Duration("run-interval", daemon.DefaultRunInterval, "Orchestrator loop interval")
	superviseInterval := fs.Duration("supervise-interval", daemon.DefaultSuperviseInterval, "Supervisor loop interval")
	tmpfsHygieneInterval := fs.Duration("tmpfs-hygiene-interval", daemon.DefaultTmpfsHygieneInterval, "Protect-aware /tmp apply interval")
	host := fs.String("host", "127.0.0.1", "Host/interface to bind the fleet web server")
	port := fs.Int("port", 8786, "Port to bind the fleet web server")
	promptPath := fs.String("prompt", "", "Path to worker prompt base file")
	readOnly := fs.Bool("read-only", false, "Disable mutating fleet HTTP endpoints")
	watchStore := fs.Bool("watch-store", false, "Hot add/remove/reload projects from the config store without a restart (#757)")
	watchStoreInterval := fs.Duration("watch-store-interval", daemon.DefaultWatchStoreInterval, "Config-store diff/reload poll interval (with --watch-store)")
	approvalsStore := fs.String("approvals-store", "json", "Approvals store backend for the fleet approve/reject endpoint: json|sqlite (#759)")
	approvalsDB := fs.String("approvals-db", approvalstore.DefaultDBPath(), "Shared SQLite approvals DB (always used for delivery; generic gate uses it with --approvals-store=sqlite)")
	stateStore := fs.String("state-store", "json", "State store backend for sessions/decisions/health/missions: json|sqlite (write-through mirror, #760)")
	stateDB := fs.String("state-db", statestore.DefaultDBPath(), "Shared SQLite state db path (used with --state-store=sqlite)")
	webhookSecretFile := fs.String("webhook-secret-file", "", "Path to a file holding the GitHub webhook secret; enables inbound webhook ingestion on the fleet port (#824)")
	webhookPath := fs.String("webhook-path", webhook.DefaultPath, "HTTP path the webhook ingestion endpoint is served on (#824)")
	webhookDB := fs.String("webhook-db", webhookstore.DefaultDBPath(), "Shared SQLite db path webhook deliveries land in (#824)")
	emergencyDB := fs.String("emergency-db", emergencystore.DefaultDBPath(), "Shared SQLite db path the fleet-wide EMERGENCY STOP switch lives in (#840)")
	drainTimeout := fs.Duration("drain-timeout", daemon.DefaultDrainTimeout, "Whole SIGTERM drain + shutdown deadline; includes flow joins and restart checkpointing (#761, #966)")
	fs.Parse(args)
	if err := refuseUnmigratedCanonicalStore(*storePath); err != nil {
		log.Fatalf("daemon: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	store, err := configstore.Open(*storePath)
	if err != nil {
		log.Fatalf("daemon: open config store %s: %v", *storePath, err)
	}
	defer store.Close()

	d := daemon.New(store, daemon.Options{
		Host:                 *host,
		Port:                 *port,
		RunInterval:          *runInterval,
		SuperviseInterval:    *superviseInterval,
		TmpfsHygieneInterval: *tmpfsHygieneInterval,
		PromptPath:           *promptPath,
		Version:              resolveVersion(),
		ReadOnly:             *readOnly,
		// Centralized self-deploy debounce marker (#758): one shared location next
		// to the config store so every flow's RequestSelfDeploy debounces on the
		// same marker, and it survives the daemon being restarted by its own
		// deploy (#722).
		SelfDeployStateDir: filepath.Join(filepath.Dir(*storePath), "self-deploy"),
		WatchStore:         *watchStore,
		WatchStoreInterval: *watchStoreInterval,
		DrainTimeout:       *drainTimeout,
		ApprovalsStore:     *approvalsStore,
		ApprovalsDBPath:    *approvalsDB,
		StateStore:         *stateStore,
		StateDBPath:        *stateDB,
		WebhookSecretFile:  *webhookSecretFile,
		WebhookPath:        *webhookPath,
		WebhookDBPath:      *webhookDB,
		EmergencyDBPath:    *emergencyDB,
	})

	// runDone is closed when Run returns, so the signal handler's exact-deadline
	// backstop can distinguish a clean shutdown from a wedged one (#966).
	runDone := make(chan struct{})
	go handleDaemonSignals(ctx, cancel, d, *drainTimeout, runDone)

	log.Printf("starting maestro daemon — store=%s addr=%s:%d", *storePath, *host, *port)
	err = d.Run(ctx)
	close(runDone)
	if err != nil {
		log.Fatalf("daemon: %v", err)
	}
}

// shutdownHandoffGrace is reserved INSIDE the configured drain timeout for flow
// cancellation, marker checkpointing, and HTTP/process handoff. The worker wait
// gets the rest of the budget. Keeping this tail small leaves Fleet available
// throughout the useful drain window while still guaranteeing the old process
// exits by the advertised deadline (#966). A var so tests can shorten it.
var shutdownHandoffGrace = 5 * time.Second

// forceExit is the process-exit call the hard-shutdown backstop uses. A var so
// tests can observe it firing without terminating the test binary.
var forceExit = func(code int) { os.Exit(code) }

type daemonShutdown interface {
	SetShutdownDeadline(time.Time)
	DrainUntil(context.Context, time.Time)
}

// handleDaemonSignals implements the two-phase shutdown the single-service
// cutover needs (#761). The FIRST SIGINT/SIGTERM triggers a graceful, in-process
// drain — SpawnDrain on every flow, then wait for in-flight workers — while the
// flows are still live; only then is the daemon ctx cancelled, tearing the now
// idle flows down. A SECOND signal aborts the drain wait and forces immediate
// shutdown. It returns if the daemon stops on its own (ctx already done) before
// any signal arrives.
//
// The bounded drain deadline governs the ENTIRE shutdown, not just the wait for
// in-flight workers (#966). A small handoff tail is reserved inside that budget;
// if any non-context-aware operation still wedges, the exact-deadline backstop
// force-exits once so systemd never needs an operator's second signal.
func handleDaemonSignals(ctx context.Context, cancel context.CancelFunc, d *daemon.Daemon, drainTimeout time.Duration, runDone <-chan struct{}) {
	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigCh)
	handleDaemonSignalStream(ctx, cancel, d, drainTimeout, runDone, sigCh)
}

// handleDaemonSignalStream is the testable signal-state machine. signals must
// deliver the first shutdown signal and may deliver a second force signal.
func handleDaemonSignalStream(ctx context.Context, cancel context.CancelFunc, d daemonShutdown, drainTimeout time.Duration, runDone <-chan struct{}, signals <-chan os.Signal) {
	select {
	case <-ctx.Done():
		return
	case <-signals:
	}
	if drainTimeout <= 0 {
		drainTimeout = daemon.DefaultDrainTimeout
	}
	// Anchor the whole-shutdown deadline at the moment the first signal lands so
	// the drain, the flow teardown, and the checkpoint pass all share one budget.
	shutdownDeadline := time.Now().Add(drainTimeout)
	d.SetShutdownDeadline(shutdownDeadline)
	handoffGrace := shutdownHandoffGraceFor(drainTimeout)
	drainDeadline := shutdownDeadline.Add(-handoffGrace)
	log.Printf("received signal — draining flows in-process (no new workers; up to %s total, reserving %s for bounded handoff); the daemon exits on its own by the deadline even if a flow stop wedges — send the signal again to force shutdown now", drainTimeout, handoffGrace)
	// Start the hard backstop before any drain state I/O. Even a wedged state
	// load/save cannot leave maestro.service deactivating past the deadline.
	forceShutdown := make(chan struct{})
	go awaitShutdownOrForceExit(shutdownDeadline, runDone, forceShutdown)
	drainCtx, abortDrain := context.WithCancel(ctx)
	defer abortDrain()
	go func() {
		select {
		case <-signals:
			log.Printf("received second signal — aborting drain and shutting down now")
			// Shorten both Run's teardown deadline and the independent process-exit
			// backstop. Cancelling DrainUntil alone would still let checkpointing or
			// a stuck flow join consume the original multi-minute budget.
			d.SetShutdownDeadline(time.Now())
			abortDrain()
			cancel()
			close(forceShutdown)
		case <-drainCtx.Done():
		}
	}()
	d.DrainUntil(drainCtx, drainDeadline)
	cancel()
}

func shutdownHandoffGraceFor(timeout time.Duration) time.Duration {
	grace := shutdownHandoffGrace
	if grace <= 0 {
		return 0
	}
	if timeout <= grace {
		return timeout / 2
	}
	return grace
}

// awaitShutdownOrForceExit blocks until Run finishes its post-cancel teardown
// (runDone closed) or the hard shutdown deadline elapses, whichever comes first.
// On the deadline it force-exits: some non-context-aware shutdown work overran
// its budget, so detaching it is the correct recovery. Surviving isolated workers
// keep their worktrees and are reconciled by the new daemon while Fleet comes
// back instead of hanging in deactivating (#877, #966).
func awaitShutdownOrForceExit(hardDeadline time.Time, runDone, forceNow <-chan struct{}) {
	remaining := time.Until(hardDeadline)
	if remaining < 0 {
		remaining = 0
	}
	timer := time.NewTimer(remaining)
	defer timer.Stop()
	select {
	case <-runDone:
		return
	case <-forceNow:
		// Resolve a simultaneous Run completion in favor of the clean return.
		select {
		case <-runDone:
			return
		default:
		}
		log.Printf("second shutdown signal received — force-exiting now so the restart handoff completes")
		forceExit(0)
	case <-timer.C:
		// Resolve the timer/runDone race in favor of a clean return when Run closed
		// concurrently with the deadline tick.
		select {
		case <-runDone:
			return
		default:
		}
		log.Printf("shutdown reached the configured drain deadline — force-exiting once so the restart handoff completes (#966)")
		forceExit(0)
	}
}

var legacyStoreWarningOnce sync.Once

func canonicalConfigStorePath() string {
	return filepath.Join(os.Getenv("HOME"), ".maestro", "maestro.db")
}

func legacyConfigStorePath() string {
	return filepath.Join(os.Getenv("HOME"), ".maestro", "config.db")
}

// defaultConfigStorePath is the one unified fleet database used by new
// installations. An upgrade host whose legacy config.db still contains project
// rows while maestro.db contains none stays on the legacy path with an explicit
// migration warning instead of silently presenting an empty fleet.
func defaultConfigStorePath() string {
	canonical := canonicalConfigStorePath()
	legacy := legacyConfigStorePath()
	needsMigration, err := legacyStoreNeedsMigration(canonical, legacy)
	if err != nil {
		legacyStoreWarningOnce.Do(func() {
			fmt.Fprintf(os.Stderr, "warning: could not inspect legacy Maestro config-store compatibility: %v; keeping the legacy path fail-closed so default commands cannot split the fleet\n", err)
		})
		if _, statErr := os.Stat(legacy); statErr == nil {
			return legacy
		}
		return canonical
	}
	if needsMigration {
		legacyStoreWarningOnce.Do(func() {
			fmt.Fprintf(os.Stderr, "warning: legacy project store %s still contains projects while %s does not; defaulting to the legacy store. Export it and migrate into maestro.db before switching the daemon service.\n", legacy, canonical)
		})
		return legacy
	}
	return canonical
}

func legacyStoreNeedsMigration(canonical, legacy string) (bool, error) {
	legacyCount, err := existingProjectCountIfPresent(legacy)
	if err != nil {
		return false, fmt.Errorf("inspect %s: %w", legacy, err)
	}
	if legacyCount == 0 {
		return false, nil
	}
	canonicalCount, err := existingProjectCountIfPresent(canonical)
	if err != nil {
		return false, fmt.Errorf("inspect %s: %w", canonical, err)
	}
	return canonicalCount == 0, nil
}

func existingProjectCountIfPresent(path string) (int, error) {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return 0, nil
	} else if err != nil {
		return 0, err
	}
	return configstore.ExistingProjectCount(path)
}

// refuseUnmigratedCanonicalStore protects the shipped service, whose explicit
// --store path bypasses the compatibility default above. It must not come up as
// an empty fleet while an older config.db still owns the project rows.
func refuseUnmigratedCanonicalStore(storePath string) error {
	canonical := canonicalConfigStorePath()
	if !sameStorePath(storePath, canonical) {
		return nil
	}
	legacy := legacyConfigStorePath()
	needsMigration, err := legacyStoreNeedsMigration(canonical, legacy)
	if err != nil {
		return err
	}
	if !needsMigration {
		return nil
	}
	backupDir := filepath.Join(os.Getenv("HOME"), ".maestro", "config-migration")
	return fmt.Errorf("legacy project store %s contains projects but canonical store %s does not; refusing an empty-fleet start. Migrate explicitly: maestro config-store export --db %s --dir %s && maestro config-store migrate --db %s --dir %s", legacy, canonical, legacy, backupDir, canonical, backupDir)
}
