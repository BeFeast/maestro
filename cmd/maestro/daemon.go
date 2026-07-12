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
	host := fs.String("host", "127.0.0.1", "Host/interface to bind the fleet web server")
	port := fs.Int("port", 8786, "Port to bind the fleet web server")
	promptPath := fs.String("prompt", "", "Path to worker prompt base file")
	readOnly := fs.Bool("read-only", false, "Disable mutating fleet HTTP endpoints")
	watchStore := fs.Bool("watch-store", false, "Hot add/remove/reload projects from the config store without a restart (#757)")
	watchStoreInterval := fs.Duration("watch-store-interval", daemon.DefaultWatchStoreInterval, "Config-store diff/reload poll interval (with --watch-store)")
	approvalsStore := fs.String("approvals-store", "json", "Approvals store backend for the fleet approve/reject endpoint: json|sqlite (#759)")
	approvalsDB := fs.String("approvals-db", approvalstore.DefaultDBPath(), "Shared SQLite approvals db path (used with --approvals-store=sqlite)")
	stateStore := fs.String("state-store", "json", "State store backend for sessions/decisions/health/missions: json|sqlite (write-through mirror, #760)")
	stateDB := fs.String("state-db", statestore.DefaultDBPath(), "Shared SQLite state db path (used with --state-store=sqlite)")
	webhookSecretFile := fs.String("webhook-secret-file", "", "Path to a file holding the GitHub webhook secret; enables inbound webhook ingestion on the fleet port (#824)")
	webhookPath := fs.String("webhook-path", webhook.DefaultPath, "HTTP path the webhook ingestion endpoint is served on (#824)")
	webhookDB := fs.String("webhook-db", webhookstore.DefaultDBPath(), "Shared SQLite db path webhook deliveries land in (#824)")
	emergencyDB := fs.String("emergency-db", emergencystore.DefaultDBPath(), "Shared SQLite db path the fleet-wide EMERGENCY STOP switch lives in (#840)")
	drainTimeout := fs.Duration("drain-timeout", daemon.DefaultDrainTimeout, "Max time the SIGTERM in-process drain waits for in-flight workers to finish (#761)")
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
		Host:              *host,
		Port:              *port,
		RunInterval:       *runInterval,
		SuperviseInterval: *superviseInterval,
		PromptPath:        *promptPath,
		Version:           resolveVersion(),
		ReadOnly:          *readOnly,
		// Centralized self-deploy debounce marker (#758): one shared location next
		// to the config store so every flow's RequestSelfDeploy debounces on the
		// same marker, and it survives the daemon being restarted by its own
		// deploy (#722).
		SelfDeployStateDir: filepath.Join(filepath.Dir(*storePath), "self-deploy"),
		WatchStore:         *watchStore,
		WatchStoreInterval: *watchStoreInterval,
		ApprovalsStore:     *approvalsStore,
		ApprovalsDBPath:    *approvalsDB,
		StateStore:         *stateStore,
		StateDBPath:        *stateDB,
		WebhookSecretFile:  *webhookSecretFile,
		WebhookPath:        *webhookPath,
		WebhookDBPath:      *webhookDB,
		EmergencyDBPath:    *emergencyDB,
	})

	go handleDaemonSignals(ctx, cancel, d, *drainTimeout)

	log.Printf("starting maestro daemon — store=%s addr=%s:%d", *storePath, *host, *port)
	if err := d.Run(ctx); err != nil {
		log.Fatalf("daemon: %v", err)
	}
}

// handleDaemonSignals implements the two-phase shutdown the single-service
// cutover needs (#761). The FIRST SIGINT/SIGTERM triggers a graceful, in-process
// drain — SpawnDrain on every flow, then wait for in-flight workers — while the
// flows are still live; only then is the daemon ctx cancelled, tearing the now
// idle flows down. A SECOND signal aborts the drain wait and forces immediate
// shutdown. It returns if the daemon stops on its own (ctx already done) before
// any signal arrives.
func handleDaemonSignals(ctx context.Context, cancel context.CancelFunc, d *daemon.Daemon, drainTimeout time.Duration) {
	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	select {
	case <-ctx.Done():
		return
	case <-sigCh:
	}
	log.Printf("received signal — draining flows in-process (no new workers; up to %s); send the signal again to force shutdown", drainTimeout)
	drainCtx, abortDrain := context.WithCancel(ctx)
	defer abortDrain()
	go func() {
		select {
		case <-sigCh:
			log.Printf("received second signal — aborting drain and shutting down now")
			abortDrain()
		case <-drainCtx.Done():
		}
	}()
	d.Drain(drainCtx, drainTimeout)
	cancel()
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
			fmt.Fprintf(os.Stderr, "warning: could not inspect legacy Maestro config-store compatibility: %v\n", err)
		})
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
