package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/befeast/maestro/internal/workerlease"
)

// workerLeaseCleanupCmd is the systemd ExecStopPost boundary for isolated
// workers. It accepts only an exact manifest + lease identity and delegates to
// the same idempotent ownership validation used by daemon reconciliation.
func workerLeaseCleanupCmd(args []string) {
	fs := flag.NewFlagSet("_worker-lease-cleanup", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	manifest := fs.String("manifest", "", "exact worker lease manifest")
	leaseID := fs.String("lease", "", "expected worker lease identity")
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}
	if err := workerlease.CleanupManifest(*manifest, *leaseID); err != nil {
		fmt.Fprintf(os.Stderr, "[maestro] worker lease cleanup: %v\n", err)
		os.Exit(1)
	}
}
