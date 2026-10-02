package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/befeast/maestro/internal/configstore"
	"github.com/befeast/maestro/internal/daemon"
)

// auxiliaryCmd is the operator surface for fleet auxiliary capacity (#1232).
//
//	maestro auxiliary reconcile [--db <path>] [--root <state_dir>] [--json]
//
// reconcile walks every project state dir persisted in auxiliary_receipt_roots
// — including roots of removed projects — and runs the same supported replay
// path the daemon uses at flow start. An abandoned consultation whose receipt
// and authority still match is sealed and its launch marker cleared; anything
// else is reported with the identity, intent and age that keep it occupied.
// Nothing is launched. Exit status is 1 when any root remains held.
func auxiliaryCmd(args []string) {
	if len(args) == 0 || args[0] != "reconcile" {
		fmt.Fprintln(os.Stderr, "usage: maestro auxiliary reconcile [--db <path>] [--root <state_dir>] [--json]")
		os.Exit(2)
	}
	fs := flag.NewFlagSet("auxiliary reconcile", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	dbPath := fs.String("db", defaultConfigStorePath(), "Unified SQLite db holding the auxiliary receipt index")
	root := fs.String("root", "", "Reconcile only this indexed project state dir")
	jsonOutput := fs.Bool("json", false, "Print one JSON object per root")
	if err := fs.Parse(reorderArgs(fs, args[1:])); err != nil {
		os.Exit(2)
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "[maestro] auxiliary reconcile: unexpected positional arguments")
		os.Exit(2)
	}
	// The index and project configs are only read; receipts are the sole
	// write target, through the same locked replay path the daemon uses.
	store, err := configstore.OpenReadOnly(*dbPath)
	if err != nil {
		log.Fatalf("auxiliary reconcile: open db %s: %v", *dbPath, err)
	}
	defer store.Close()
	ctx := context.Background()
	configured, err := store.LoadAll(ctx)
	if err != nil {
		log.Fatalf("auxiliary reconcile: load projects: %v", err)
	}
	only := ""
	if *root != "" {
		if only, err = filepath.Abs(*root); err != nil {
			log.Fatalf("auxiliary reconcile: --root: %v", err)
		}
	}
	reports, err := daemon.ReconcileIndexedAuxiliaryRoots(ctx, store, configured, nil, only, false)
	if err != nil {
		log.Fatalf("auxiliary reconcile: %v", err)
	}
	held := 0
	for _, report := range reports {
		if report.Result == "held" {
			held++
		}
		if *jsonOutput {
			line, _ := json.Marshal(report)
			fmt.Println(string(line))
			continue
		}
		fmt.Println(report.String())
	}
	if !*jsonOutput {
		fmt.Printf("roots=%d held=%d\n", len(reports), held)
	}
	if held > 0 {
		os.Exit(1)
	}
}
