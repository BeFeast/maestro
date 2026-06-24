package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/befeast/maestro/internal/configstore"
	"github.com/befeast/maestro/internal/daemon"
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
	fs.Parse(args)

	ctx := signalContext()

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
	})

	log.Printf("starting maestro daemon — store=%s addr=%s:%d", *storePath, *host, *port)
	if err := d.Run(ctx); err != nil {
		log.Fatalf("daemon: %v", err)
	}
}

// signalContext returns a context cancelled on SIGINT/SIGTERM so the daemon
// drains its flows and shuts the fleet server down gracefully.
func signalContext() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		log.Printf("received signal, shutting down daemon...")
		cancel()
	}()
	return ctx
}

// defaultConfigStorePath mirrors the config-store default used elsewhere
// (~/.maestro/config.db).
func defaultConfigStorePath() string {
	return filepath.Join(os.Getenv("HOME"), ".maestro", "config.db")
}
