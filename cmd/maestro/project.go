package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/befeast/maestro/internal/configstore"
	"github.com/befeast/maestro/internal/daemon"
)

// The `maestro project plan|apply` genesis flow (#871) replaces the retired
// `maestro init` wizard. Instead of scaffolding a per-project maestro.yaml plus a
// systemd/launchd unit that runs `maestro run`, it turns a portable project YAML
// into a single config-store row that the one long-lived
// `maestro daemon --watch-store` observes and starts as one flow.
//
//   - `plan`  — pure, zero-write: strict-validate the config, inspect store
//     preconditions, and report the predicted effect (create/update/no-op/
//     conflict) plus warnings. It never writes.
//   - `apply` — idempotent upsert of exactly one row, gated on an explicit
//     --confirm <project_id> and (optionally) the plan-time --fingerprint. A
//     matching row is a reported no-op; an identity conflict is a hard stop that
//     never overwrites.
//
// Both emit a stable machine-readable receipt so an external bootstrap adapter
// can drive them by JSON: store, project id, fingerprint, effect, whether a
// write happened, the daemon-reconciliation expectation, and the exact next
// verification commands.

// genesisReconciliation reports the single-daemon expectation after a
// plan/apply, plus a best-effort probe of whether a running
// `maestro daemon --watch-store` can actually hot-reconcile the row. Removal and
// rollback deliberately stay a separate explicit operator action.
type genesisReconciliation struct {
	Expectation string `json:"expectation"`
	// WatchStore is a best-effort classification of the local daemon:
	// observed | running-without-watch-store | not-observed | unknown.
	WatchStore string `json:"watch_store"`
	Note       string `json:"note,omitempty"`
}

// genesisReceipt is the CLI-level receipt: the library GenesisReport augmented
// with the store path, the daemon-reconciliation expectation, and the exact next
// verification commands. Its JSON shape is the contract the private vault-side
// bootstrap adapter consumes.
type genesisReceipt struct {
	Command        string                   `json:"command"` // "plan" | "apply"
	Store          string                   `json:"store"`
	Project        string                   `json:"project"`
	ProjectID      string                   `json:"project_id"`
	Fingerprint    string                   `json:"fingerprint"`
	Effect         string                   `json:"effect"`
	Wrote          bool                     `json:"wrote"`
	Conflict       string                   `json:"conflict,omitempty"`
	Existing       *configstore.ExistingRow `json:"existing,omitempty"`
	Warnings       []string                 `json:"warnings,omitempty"`
	Reconciliation genesisReconciliation    `json:"reconciliation"`
	Next           []string                 `json:"next"`
}

// projectCmd dispatches the `maestro project <plan|apply>` genesis subcommands.
func projectCmd(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: maestro project <plan|apply> --file <portable-project.yaml> --db <store> [--json]")
		fmt.Fprintln(os.Stderr, "  plan   validate a portable project config and preview its effect on the store (zero writes)")
		fmt.Fprintln(os.Stderr, "  apply  upsert exactly one config-store row after --confirm <project-id> (idempotent)")
		os.Exit(1)
	}
	switch args[0] {
	case "plan":
		projectPlanCmd(args[1:])
	case "apply":
		projectApplyCmd(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "unknown project command: %s (want plan|apply)\n", args[0])
		os.Exit(1)
	}
}

// projectPlanCmd implements `maestro project plan --file <yaml> --db <store>
// [--json]`. It is strictly read-only: it validates the config, previews the
// effect against the store, and prints a receipt. Exit is non-zero on a
// validation failure or an identity conflict so an adapter can gate on it.
func projectPlanCmd(args []string) {
	fs := flag.NewFlagSet("project plan", flag.ExitOnError)
	file := fs.String("file", "", "Path to the portable project YAML")
	dbPath := fs.String("db", defaultConfigStorePath(), "Path to the SQLite config store")
	asJSON := fs.Bool("json", false, "Emit the receipt as JSON")
	fs.Parse(args)

	prepared := prepareGenesisFile(*file, "project plan")

	report := planReport(*dbPath, prepared)
	receipt := buildGenesisReceipt("plan", *dbPath, *file, prepared, report)
	emitGenesisReceipt(receipt, *asJSON)
	if receipt.Effect == configstore.EffectConflict {
		os.Exit(1)
	}
}

// projectApplyCmd implements `maestro project apply --file <yaml> --db <store>
// --confirm <project-id> [--fingerprint <sha256:...>] [--json]`. It upserts
// exactly one row idempotently after the confirm/fingerprint gates. Exit is
// non-zero on any refusal (missing/wrong confirm, stale fingerprint, conflict).
func projectApplyCmd(args []string) {
	fs := flag.NewFlagSet("project apply", flag.ExitOnError)
	file := fs.String("file", "", "Path to the portable project YAML")
	dbPath := fs.String("db", defaultConfigStorePath(), "Path to the SQLite config store")
	confirm := fs.String("confirm", "", "Exact project_id to confirm the write (required)")
	fingerprint := fs.String("fingerprint", "", "Plan-time config fingerprint to gate on (optional; refuses if the config changed since plan)")
	asJSON := fs.Bool("json", false, "Emit the receipt as JSON")
	fs.Parse(args)

	prepared := prepareGenesisFile(*file, "project apply")
	store, err := configstore.Open(*dbPath)
	if err != nil {
		log.Fatalf("project apply: open config store %s: %v", *dbPath, err)
	}
	defer store.Close()

	report, err := store.ApplyProject(context.Background(), prepared, *confirm, *fingerprint)
	if err != nil {
		// An identity conflict still returns a report; print it so the adapter
		// sees the reason, then exit non-zero. Other errors (bad confirm, stale
		// fingerprint) carry no report.
		if report != nil {
			receipt := buildGenesisReceipt("apply", *dbPath, *file, prepared, report)
			emitGenesisReceipt(receipt, *asJSON)
		}
		log.Fatalf("project apply: %v", err)
	}
	receipt := buildGenesisReceipt("apply", *dbPath, *file, prepared, report)
	emitGenesisReceipt(receipt, *asJSON)
}

// prepareGenesisFile reads and strict-validates the portable project file,
// sharing the flag-plumbing between plan and apply. It fatals (with a clear
// cmd-prefixed message) on any read/validation failure. Opening the store is the
// caller's job so `plan` can avoid creating an absent store.
func prepareGenesisFile(file, cmd string) *configstore.PreparedProject {
	if strings.TrimSpace(file) == "" {
		log.Fatalf("%s: --file is required (a portable project YAML)", cmd)
	}
	data, err := os.ReadFile(file)
	if err != nil {
		log.Fatalf("%s: read %s: %v", cmd, file, err)
	}
	prepared, err := configstore.PrepareProject(file, data)
	if err != nil {
		log.Fatalf("%s: %v", cmd, err)
	}
	return prepared
}

// planReport previews prepared against the store at dbPath without any write. A
// store file that does not exist yet is treated as empty (effect create) WITHOUT
// opening it, so `plan` never creates a schema-only DB — it changes no files.
func planReport(dbPath string, prepared *configstore.PreparedProject) *configstore.GenesisReport {
	if !storeFileExists(dbPath) {
		return configstore.PlanEmpty(prepared)
	}
	store, err := configstore.Open(dbPath)
	if err != nil {
		log.Fatalf("project plan: open config store %s: %v", dbPath, err)
	}
	defer store.Close()
	report, err := store.PlanProject(context.Background(), prepared)
	if err != nil {
		log.Fatalf("project plan: %v", err)
	}
	return report
}

// storeFileExists reports whether the config store path already exists on disk.
// A missing store is planned as empty rather than created.
func storeFileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// buildGenesisReceipt combines the library report with the CLI-only fields (store
// path, reconciliation expectation, next verification commands). It is pure so
// the receipt shape is unit-testable without a store or a live daemon.
func buildGenesisReceipt(command, storePath, file string, p *configstore.PreparedProject, report *configstore.GenesisReport) genesisReceipt {
	return genesisReceipt{
		Command:        command,
		Store:          storePath,
		Project:        report.Project,
		ProjectID:      report.ProjectID,
		Fingerprint:    report.Fingerprint,
		Effect:         report.Effect,
		Wrote:          report.Wrote,
		Conflict:       report.Conflict,
		Existing:       report.Existing,
		Warnings:       report.Warnings,
		Reconciliation: genesisReconcileFor(command, report.Effect, report.Conflict, probeDaemonWatchStore()),
		Next:           genesisNextCommands(command, storePath, file, report.ProjectID, report.Effect),
	}
}

// genesisReconcileFor renders the single-daemon reconciliation expectation for
// an effect, folding in the watch-store probe status. It is pure: the probe
// result is injected so tests can drive every branch.
func genesisReconcileFor(command, effect, conflict, watchStore string) genesisReconciliation {
	r := genesisReconciliation{WatchStore: watchStore}
	switch effect {
	case configstore.EffectConflict:
		r.Expectation = "no reconciliation — the identity conflict is a hard stop; the row is not written and no flow changes"
		if conflict != "" {
			r.Note = conflict
		}
		return r
	case configstore.EffectNoOp:
		if command == "apply" {
			r.Expectation = "no change — the matching row already exists; the daemon's existing flow is unaffected"
		} else {
			r.Expectation = "applying would be a no-op — the matching row already exists; the daemon's existing flow is unaffected"
		}
	default: // create | update
		verb := "applying writes"
		if command == "apply" {
			verb = "the write lands"
		}
		r.Expectation = fmt.Sprintf("once %s the row, a single `maestro daemon --watch-store` observes it within one poll interval (default %s) and reconciles exactly one flow; removal/rollback stays a separate explicit operator action", verb, daemon.DefaultWatchStoreInterval)
	}
	// Fold the watch-store observability into the note so an operator sees when
	// hot reconciliation cannot actually be confirmed from this host.
	switch watchStore {
	case watchStoreObserved:
		// Expectation already covers it; no extra note needed.
	case watchStoreRunningWithout:
		r.Note = appendNote(r.Note, "a maestro daemon is running WITHOUT --watch-store on this host; it will not hot-reconcile the row — restart it with --watch-store or reconcile manually")
	case watchStoreNotObserved:
		r.Note = appendNote(r.Note, "no running maestro daemon was observed from this host; confirm the fleet daemon runs with --watch-store so the row is hot-reconciled")
	default: // unknown
		r.Note = appendNote(r.Note, "could not probe the local daemon; confirm the fleet daemon runs with --watch-store so the row is hot-reconciled")
	}
	return r
}

func appendNote(existing, add string) string {
	if strings.TrimSpace(existing) == "" {
		return add
	}
	return existing + "; " + add
}

// genesisNextCommands returns the exact next verification commands an adapter/
// operator should run. It is pure and deterministic so the JSON contract is
// stable.
func genesisNextCommands(command, storePath, file, projectID, effect string) []string {
	switch command {
	case "plan":
		if effect == configstore.EffectConflict {
			return []string{
				"# resolve the identity conflict above before applying; plan/apply never overwrite by name",
			}
		}
		return []string{
			fmt.Sprintf("maestro project apply --file %s --db %s --confirm %s --json", file, storePath, projectID),
		}
	case "apply":
		if effect == configstore.EffectConflict {
			return []string{
				"# resolve the identity conflict above; nothing was written",
			}
		}
		return []string{
			// Re-running plan is the scriptable confirmation the row landed: it
			// must now report effect "no-op".
			fmt.Sprintf("maestro project plan --file %s --db %s --json   # expect effect \"no-op\": the row is stored", file, storePath),
		}
	}
	return nil
}

// emitGenesisReceipt prints the receipt as JSON (stable, adapter-facing) or as a
// short human summary.
func emitGenesisReceipt(r genesisReceipt, asJSON bool) {
	if asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(r); err != nil {
			log.Fatalf("encode receipt: %v", err)
		}
		return
	}
	fmt.Printf("project:     %s\n", r.Project)
	fmt.Printf("project_id:  %s\n", r.ProjectID)
	fmt.Printf("store:       %s\n", r.Store)
	fmt.Printf("fingerprint: %s\n", r.Fingerprint)
	fmt.Printf("effect:      %s (wrote=%t)\n", r.Effect, r.Wrote)
	if r.Conflict != "" {
		fmt.Printf("conflict:    %s\n", r.Conflict)
	}
	for _, w := range r.Warnings {
		fmt.Printf("warning:     %s\n", w)
	}
	fmt.Printf("reconcile:   %s\n", r.Reconciliation.Expectation)
	if r.Reconciliation.Note != "" {
		fmt.Printf("note:        %s\n", r.Reconciliation.Note)
	}
	for _, n := range r.Next {
		fmt.Printf("next:        %s\n", n)
	}
}

// watch-store probe classifications.
const (
	watchStoreObserved       = "observed"
	watchStoreRunningWithout = "running-without-watch-store"
	watchStoreNotObserved    = "not-observed"
	watchStoreUnknown        = "unknown"
)

// probeDaemonWatchStore best-effort inspects local processes for a
// `maestro daemon --watch-store`. It returns watchStoreUnknown when the process
// table cannot be read (non-Linux, restricted /proc). The classification itself
// is delegated to the pure classifyDaemonWatchStore so it stays testable.
func probeDaemonWatchStore() string {
	procs, ok := readProcCmdlines()
	if !ok {
		return watchStoreUnknown
	}
	return classifyDaemonWatchStore(procs)
}

// classifyDaemonWatchStore decides the watch-store status from a snapshot of
// process argv slices. Pure and deterministic for testing.
func classifyDaemonWatchStore(procs [][]string) string {
	daemonFound := false
	for _, argv := range procs {
		if !looksLikeMaestroDaemon(argv) {
			continue
		}
		daemonFound = true
		if argvHasWatchStore(argv) {
			return watchStoreObserved
		}
	}
	if daemonFound {
		return watchStoreRunningWithout
	}
	return watchStoreNotObserved
}

// looksLikeMaestroDaemon reports whether argv is a `maestro daemon ...`
// invocation (argv[0] a maestro binary, a "daemon" subcommand token present).
func looksLikeMaestroDaemon(argv []string) bool {
	if len(argv) < 2 {
		return false
	}
	if filepath.Base(argv[0]) != "maestro" && !strings.Contains(argv[0], "maestro") {
		return false
	}
	for _, a := range argv[1:] {
		if a == "daemon" {
			return true
		}
	}
	return false
}

// argvHasWatchStore reports whether argv enables --watch-store (in either
// dash form, and not explicitly set to a false value).
func argvHasWatchStore(argv []string) bool {
	for _, a := range argv {
		if a == "--watch-store" || a == "-watch-store" {
			return true
		}
		if strings.HasPrefix(a, "--watch-store=") || strings.HasPrefix(a, "-watch-store=") {
			v := a[strings.IndexByte(a, '=')+1:]
			return !(v == "false" || v == "0")
		}
	}
	return false
}

// readProcCmdlines returns the argv of every readable process from /proc. The
// bool is false when the process table cannot be read (non-Linux, restricted
// environment) so the caller reports "unknown" rather than a false negative.
func readProcCmdlines() ([][]string, bool) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, false
	}
	var procs [][]string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if name == "" || name[0] < '0' || name[0] > '9' {
			continue // only numeric PID dirs
		}
		raw, err := os.ReadFile(filepath.Join("/proc", name, "cmdline"))
		if err != nil {
			continue // process gone / not ours
		}
		argv := splitProcCmdline(raw)
		if len(argv) > 0 {
			procs = append(procs, argv)
		}
	}
	return procs, true
}

// splitProcCmdline splits a NUL-separated /proc/<pid>/cmdline into argv,
// dropping the trailing empty field the kernel appends.
func splitProcCmdline(raw []byte) []string {
	parts := strings.Split(string(raw), "\x00")
	argv := parts[:0]
	for _, p := range parts {
		if p != "" {
			argv = append(argv, p)
		}
	}
	return argv
}
