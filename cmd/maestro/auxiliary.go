package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/befeast/maestro/internal/config"
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
	os.Exit(runAuxiliaryReconcile(args, os.Stdout, os.Stderr))
}

func runAuxiliaryReconcile(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] != "reconcile" {
		fmt.Fprintln(stderr, "usage: maestro auxiliary reconcile [--db <path>] [--root <state_dir>] [--json]")
		return 2
	}
	fs := flag.NewFlagSet("auxiliary reconcile", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dbPath := fs.String("db", defaultConfigStorePath(), "Unified SQLite db holding the auxiliary receipt index")
	root := fs.String("root", "", "Reconcile only this indexed project state dir")
	jsonOutput := fs.Bool("json", false, "Print one JSON object per root")
	if err := fs.Parse(reorderArgs(fs, args[1:])); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "[maestro] auxiliary reconcile: unexpected positional arguments")
		return 2
	}
	only := ""
	if *root != "" {
		abs, err := filepath.Abs(*root)
		if err != nil {
			fmt.Fprintf(stderr, "[maestro] auxiliary reconcile: --root: %v\n", err)
			return 1
		}
		only = abs
	}
	// The index and project configs are only read; receipts are the sole
	// write target, through the same locked replay path the daemon uses.
	store, err := configstore.OpenReadOnly(*dbPath)
	if err != nil {
		fmt.Fprintf(stderr, "[maestro] auxiliary reconcile: open db %s: %v\n", *dbPath, err)
		return 1
	}
	defer store.Close()
	ctx := context.Background()
	configured, err := loadAuxiliaryReconcileProjects(ctx, store, only, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "[maestro] auxiliary reconcile: %v\n", err)
		return 1
	}
	reports, err := daemon.ReconcileIndexedAuxiliaryRoots(ctx, store, configured, nil, only, false)
	if err != nil {
		fmt.Fprintf(stderr, "[maestro] auxiliary reconcile: %v\n", err)
		return 1
	}
	held := 0
	for _, report := range reports {
		if report.Result == "held" {
			held++
		}
		if *jsonOutput {
			line, _ := json.Marshal(report)
			fmt.Fprintln(stdout, string(line))
			continue
		}
		fmt.Fprintln(stdout, report.String())
	}
	if !*jsonOutput {
		fmt.Fprintf(stdout, "roots=%d held=%d\n", len(reports), held)
	}
	if held > 0 {
		return 1
	}
	return 0
}

// loadAuxiliaryReconcileProjects loads the project rows one at a time. A row
// that no longer loads — a routing tier pointing at a backend that has since
// been disabled, for instance — is skipped with one warning line instead of
// aborting the reconcile of every other root, which is what the all-or-nothing
// store.LoadAll did. Every loadable config is returned even under --root: a
// root of a removed project is replayed against the native authorities of all
// configured projects, so the set cannot be narrowed to the row owning the
// root. A root whose own row was skipped is replayed from its receipt, the
// same way a removed project's root is. Without --root, an empty result means
// no root can be reconciled under a project config and is an error.
func loadAuxiliaryReconcileProjects(ctx context.Context, store *configstore.Store, only string, stderr io.Writer) ([]*config.Config, error) {
	names, err := store.ProjectNames(ctx)
	if err != nil {
		return nil, fmt.Errorf("load projects: %w", err)
	}
	if len(names) == 0 {
		return nil, errors.New("load projects: no projects in config store")
	}
	var configured []*config.Config
	skipped := 0
	for _, name := range names {
		cfg, err := store.Load(ctx, name)
		if err != nil {
			skipped++
			fmt.Fprintf(stderr, "[maestro] auxiliary reconcile: skipping project %s: %s\n", name, singleLineError(err))
			continue
		}
		configured = append(configured, cfg)
	}
	if len(configured) == 0 && only == "" {
		return nil, fmt.Errorf("load projects: all %d project rows were skipped; no root can be reconciled", skipped)
	}
	return configured, nil
}

// singleLineError renders err on one line. A parse error can span several
// lines (yaml lists each type mismatch on its own indented line), which would
// split one skip warning across stderr.
func singleLineError(err error) string {
	var parts []string
	for _, line := range strings.Split(err.Error(), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			parts = append(parts, line)
		}
	}
	return strings.Join(parts, " ")
}
