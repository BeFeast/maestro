package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/befeast/maestro/internal/approvalstore"
	"github.com/befeast/maestro/internal/configstore"
	"github.com/befeast/maestro/internal/daemon"
	"github.com/befeast/maestro/internal/statestore"
	"github.com/befeast/maestro/internal/webhook"
	"github.com/befeast/maestro/internal/webhookstore"
)

// daemonCmd runs every project in the config store as one long-lived process:
// an orchestrator + supervisor loop per project flow, plus a single
// FleetServer aggregating them all (#756, epic #754). It is strictly opt-in —
// the legacy maestro@<project> units and `maestro serve` are unaffected.
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
	drainTimeout := fs.Duration("drain-timeout", daemon.DefaultDrainTimeout, "Max time the SIGTERM in-process drain waits for in-flight workers to finish (#761)")
	fs.Parse(args)

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

// defaultConfigStorePath mirrors the config-store default used elsewhere
// (~/.maestro/config.db).
func defaultConfigStorePath() string {
	return filepath.Join(os.Getenv("HOME"), ".maestro", "config.db")
}
