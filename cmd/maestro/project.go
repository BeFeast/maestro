package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
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
//     --confirm <project_id>, the plan-time --fingerprint, and the plan-time
//     --baseline store fingerprint. A
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
	SchemaVersion       int                      `json:"schema_version"`
	OK                  bool                     `json:"ok"`
	Command             string                   `json:"command"` // "plan" | "apply"
	Store               string                   `json:"store"`
	Project             string                   `json:"project"`
	ProjectID           string                   `json:"project_id"`
	Fingerprint         string                   `json:"fingerprint"`
	BaselineFingerprint string                   `json:"baseline_fingerprint"`
	Effect              string                   `json:"effect"`
	Wrote               bool                     `json:"wrote"`
	Conflict            string                   `json:"conflict,omitempty"`
	Existing            *configstore.ExistingRow `json:"existing,omitempty"`
	Warnings            []string                 `json:"warnings,omitempty"`
	Reconciliation      genesisReconciliation    `json:"reconciliation"`
	Next                []string                 `json:"next"`
	Error               *genesisErrorDetail      `json:"error,omitempty"`
}

type genesisErrorDetail struct {
	Code    string `json:"code"`
	Message string `json:"message"`
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
	requestedJSON := argsRequestJSON(args)
	fs := flag.NewFlagSet("project plan", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	file := fs.String("file", "", "Path to the portable project YAML")
	dbPath := fs.String("db", defaultProjectStorePath(), "Path to the SQLite config store")
	asJSON := fs.Bool("json", false, "Emit the receipt as JSON")
	if err := fs.Parse(args); err != nil {
		failGenesis("plan", requestedJSON, "invalid_flags", fmt.Sprintf("project plan: invalid flags: %v", err))
	}
	if fs.NArg() != 0 {
		failGenesis("plan", requestedJSON, "invalid_flags", fmt.Sprintf("project plan: unexpected arguments: %s", strings.Join(fs.Args(), " ")))
	}
	if strings.TrimSpace(*dbPath) == "" {
		failGenesis("plan", *asJSON, "invalid_input", "project plan: --db must not be empty")
	}

	prepared := prepareGenesisFile(*file, "project plan", *asJSON)
	if err := validateGenesisRuntime(prepared); err != nil {
		failGenesis("plan", *asJSON, "preflight_failed", fmt.Sprintf("project plan: preflight: %v", err))
	}

	report := planReport(*dbPath, prepared, *asJSON)
	receipt := buildGenesisReceipt("plan", *dbPath, *file, prepared, report)
	if receipt.Effect == configstore.EffectConflict {
		receipt.OK = false
		receipt.Error = &genesisErrorDetail{Code: "identity_conflict", Message: receipt.Conflict}
		emitGenesisReceipt(receipt, *asJSON)
		os.Exit(1)
	}
	emitGenesisReceipt(receipt, *asJSON)
}

// projectApplyCmd implements `maestro project apply --file <yaml> --db <store>
// --confirm <project-id> --fingerprint <sha256:...> --baseline <fp|absent>
// [--json]`. It upserts
// exactly one row idempotently after the confirm/fingerprint gates. Exit is
// non-zero on any refusal (missing/wrong confirm, stale fingerprint, conflict).
func projectApplyCmd(args []string) {
	requestedJSON := argsRequestJSON(args)
	fs := flag.NewFlagSet("project apply", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	file := fs.String("file", "", "Path to the portable project YAML")
	dbPath := fs.String("db", defaultProjectStorePath(), "Path to the SQLite config store")
	confirm := fs.String("confirm", "", "Exact project_id to confirm the write (required)")
	fingerprint := fs.String("fingerprint", "", "Exact plan-time config fingerprint (required)")
	baseline := fs.String("baseline", "", "Exact plan-time baseline_fingerprint (required; `absent` for create)")
	asJSON := fs.Bool("json", false, "Emit the receipt as JSON")
	if err := fs.Parse(args); err != nil {
		failGenesis("apply", requestedJSON, "invalid_flags", fmt.Sprintf("project apply: invalid flags: %v", err))
	}
	if fs.NArg() != 0 {
		failGenesis("apply", requestedJSON, "invalid_flags", fmt.Sprintf("project apply: unexpected arguments: %s", strings.Join(fs.Args(), " ")))
	}
	if strings.TrimSpace(*dbPath) == "" {
		failGenesis("apply", *asJSON, "invalid_input", "project apply: --db must not be empty")
	}

	prepared := prepareGenesisFile(*file, "project apply", *asJSON)
	if err := validateGenesisRuntime(prepared); err != nil {
		failGenesis("apply", *asJSON, "preflight_failed", fmt.Sprintf("project apply: preflight: %v", err))
	}
	if err := validateApplyApproval(prepared, *confirm, *fingerprint, *baseline); err != nil {
		failGenesis("apply", *asJSON, "approval_mismatch", fmt.Sprintf("project apply: %v", err))
	}
	store, err := configstore.Open(*dbPath)
	if err != nil {
		failGenesis("apply", *asJSON, "store_open_failed", fmt.Sprintf("project apply: open config store %s: %v", *dbPath, err))
	}
	defer store.Close()

	report, err := store.ApplyProject(context.Background(), prepared, *confirm, *fingerprint, *baseline)
	if err != nil {
		// An identity conflict still returns a report; print it so the adapter
		// sees the reason, then exit non-zero. Other errors (bad confirm, stale
		// fingerprint) carry no report.
		if report != nil {
			receipt := buildGenesisReceipt("apply", *dbPath, *file, prepared, report)
			receipt.OK = false
			receipt.Error = &genesisErrorDetail{Code: "apply_refused", Message: err.Error()}
			emitGenesisReceipt(receipt, *asJSON)
			os.Exit(1)
		}
		failGenesis("apply", *asJSON, "apply_refused", fmt.Sprintf("project apply: %v", err))
	}
	receipt := buildGenesisReceipt("apply", *dbPath, *file, prepared, report)
	emitGenesisReceipt(receipt, *asJSON)
}

// argsRequestJSON preserves the machine-readable error contract even when flag
// parsing itself fails before the FlagSet can populate its --json variable.
func argsRequestJSON(args []string) bool {
	for _, arg := range args {
		if arg == "--json" || arg == "-json" {
			return true
		}
		for _, prefix := range []string{"--json=", "-json="} {
			if strings.HasPrefix(arg, prefix) {
				value, err := strconv.ParseBool(strings.TrimPrefix(arg, prefix))
				return err != nil || value
			}
		}
	}
	return false
}

func validateApplyApproval(p *configstore.PreparedProject, confirm, fingerprint, baseline string) error {
	if strings.TrimSpace(confirm) == "" || strings.TrimSpace(confirm) != p.ProjectID {
		return fmt.Errorf("--confirm must equal project_id %s", p.ProjectID)
	}
	if strings.TrimSpace(fingerprint) == "" || strings.TrimSpace(fingerprint) != p.Fingerprint {
		return fmt.Errorf("--fingerprint must equal the approved plan fingerprint %s", p.Fingerprint)
	}
	if strings.TrimSpace(baseline) == "" {
		return fmt.Errorf("--baseline must equal the approved plan baseline_fingerprint")
	}
	return nil
}

// prepareGenesisFile reads and strict-validates the portable project file,
// sharing the flag-plumbing between plan and apply. It fatals (with a clear
// cmd-prefixed message) on any read/validation failure. Opening the store is the
// caller's job so `plan` can avoid creating an absent store.
func prepareGenesisFile(file, cmd string, asJSON bool) *configstore.PreparedProject {
	if strings.TrimSpace(file) == "" {
		failGenesis(strings.TrimPrefix(cmd, "project "), asJSON, "invalid_input", fmt.Sprintf("%s: --file is required (a portable project YAML)", cmd))
	}
	data, err := os.ReadFile(file)
	if err != nil {
		failGenesis(strings.TrimPrefix(cmd, "project "), asJSON, "file_read_failed", fmt.Sprintf("%s: read %s: %v", cmd, file, err))
	}
	prepared, err := configstore.PrepareProject(file, data)
	if err != nil {
		failGenesis(strings.TrimPrefix(cmd, "project "), asJSON, "validation_failed", fmt.Sprintf("%s: %v", cmd, err))
	}
	return prepared
}

func validateGenesisRuntime(p *configstore.PreparedProject) error {
	localInfo, err := os.Stat(p.LocalPath)
	if err != nil {
		return fmt.Errorf("local_path %s is unavailable: %w", p.LocalPath, err)
	}
	if !localInfo.IsDir() {
		return fmt.Errorf("local_path %s is not a directory", p.LocalPath)
	}
	inside, err := exec.Command("git", "-C", p.LocalPath, "rev-parse", "--is-inside-work-tree").Output()
	if err != nil || strings.TrimSpace(string(inside)) != "true" {
		return fmt.Errorf("local_path %s is not a Git worktree", p.LocalPath)
	}
	remote, err := exec.Command("git", "-C", p.LocalPath, "remote", "get-url", "origin").Output()
	if err != nil {
		return fmt.Errorf("local_path %s has no readable origin remote", p.LocalPath)
	}
	if !remoteMatchesRepo(strings.TrimSpace(string(remote)), p.Repo) {
		return fmt.Errorf("local_path origin %q does not match configured repo %q", strings.TrimSpace(string(remote)), p.Repo)
	}

	worktreeInfo, err := os.Stat(p.WorktreeBase)
	switch {
	case err == nil && !worktreeInfo.IsDir():
		return fmt.Errorf("worktree_base %s exists but is not a directory", p.WorktreeBase)
	case err == nil:
		if worktreeInfo.Mode().Perm()&0o222 == 0 {
			return fmt.Errorf("worktree_base %s is not writable", p.WorktreeBase)
		}
	case os.IsNotExist(err):
		parentInfo, parentErr := os.Stat(filepath.Dir(p.WorktreeBase))
		if parentErr != nil || !parentInfo.IsDir() || parentInfo.Mode().Perm()&0o222 == 0 {
			return fmt.Errorf("worktree_base parent %s is unavailable or not writable", filepath.Dir(p.WorktreeBase))
		}
	default:
		return fmt.Errorf("inspect worktree_base %s: %w", p.WorktreeBase, err)
	}
	homeInfo, err := os.Stat(p.ManagementHome.Path)
	if err != nil {
		return fmt.Errorf("management_home.path %s is unavailable: %w", p.ManagementHome.Path, err)
	}
	if !homeInfo.IsDir() {
		return fmt.Errorf("management_home.path %s is not a directory", p.ManagementHome.Path)
	}
	return nil
}

func remoteMatchesRepo(remote, repo string) bool {
	r := strings.TrimSpace(remote)
	want := strings.ToLower(strings.TrimSuffix(strings.Trim(strings.TrimSpace(repo), "/"), ".git"))
	lowerRemote := strings.ToLower(r)
	if strings.HasPrefix(lowerRemote, "git@github.com:") {
		got := strings.TrimSuffix(strings.TrimPrefix(lowerRemote, "git@github.com:"), ".git")
		return got == want
	}
	u, err := url.Parse(r)
	if err != nil || !strings.EqualFold(u.Hostname(), "github.com") {
		return false
	}
	switch strings.ToLower(u.Scheme) {
	case "https", "http", "ssh", "git":
	default:
		return false
	}
	got := strings.TrimSuffix(strings.ToLower(strings.Trim(u.Path, "/")), ".git")
	return got == want
}

// planReport previews prepared against the store at dbPath without any write. A
// store file that does not exist yet is treated as empty (effect create) WITHOUT
// opening it, so `plan` never creates a schema-only DB — it changes no files.
func planReport(dbPath string, prepared *configstore.PreparedProject, asJSON bool) *configstore.GenesisReport {
	exists, err := inspectStoreFile(dbPath)
	if err != nil {
		failGenesis("plan", asJSON, "store_preflight_failed", fmt.Sprintf("project plan: inspect config store %s: %v", dbPath, err))
	}
	if !exists {
		return configstore.PlanEmpty(prepared)
	}
	store, err := configstore.OpenReadOnly(dbPath)
	if err != nil {
		failGenesis("plan", asJSON, "store_open_failed", fmt.Sprintf("project plan: open config store %s: %v", dbPath, err))
	}
	defer store.Close()
	report, err := store.PlanProject(context.Background(), prepared)
	if err != nil {
		failGenesis("plan", asJSON, "plan_failed", fmt.Sprintf("project plan: %v", err))
	}
	return report
}

// storeFileExists reports whether the config store path already exists on disk.
// A missing store is planned as empty rather than created.
func storeFileExists(path string) bool {
	exists, _ := inspectStoreFile(path)
	return exists
}

func inspectStoreFile(path string) (bool, error) {
	info, err := os.Stat(path)
	if os.IsNotExist(err) {
		if err := inspectStoreCreateParent(path); err != nil {
			return false, err
		}
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !info.Mode().IsRegular() {
		return false, fmt.Errorf("path exists but is not a regular file")
	}
	return true, nil
}

func inspectStoreCreateParent(path string) error {
	parent := filepath.Dir(path)
	info, err := os.Stat(parent)
	if err != nil {
		return fmt.Errorf("store parent %s is unavailable: %w", parent, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("store parent %s is not a directory", parent)
	}
	if info.Mode().Perm()&0o222 == 0 {
		return fmt.Errorf("store parent %s is not writable", parent)
	}
	return nil
}

// buildGenesisReceipt combines the library report with the CLI-only fields (store
// path, reconciliation expectation, next verification commands). It is pure so
// the receipt shape is unit-testable without a store or a live daemon.
func buildGenesisReceipt(command, storePath, file string, p *configstore.PreparedProject, report *configstore.GenesisReport) genesisReceipt {
	return genesisReceipt{
		SchemaVersion:       1,
		OK:                  true,
		Command:             command,
		Store:               storePath,
		Project:             report.Project,
		ProjectID:           report.ProjectID,
		Fingerprint:         report.Fingerprint,
		BaselineFingerprint: report.BaselineFingerprint,
		Effect:              report.Effect,
		Wrote:               report.Wrote,
		Conflict:            report.Conflict,
		Existing:            report.Existing,
		Warnings:            report.Warnings,
		Reconciliation:      genesisReconcileFor(command, report.Effect, report.Conflict, probeDaemonWatchStore(storePath)),
		Next:                genesisNextCommands(command, storePath, file, report.ProjectID, report.Fingerprint, report.BaselineFingerprint, report.Effect),
	}
}

func failGenesis(command string, asJSON bool, code, message string) {
	if asJSON {
		_ = json.NewEncoder(os.Stdout).Encode(struct {
			SchemaVersion int                 `json:"schema_version"`
			OK            bool                `json:"ok"`
			Command       string              `json:"command"`
			Error         *genesisErrorDetail `json:"error"`
		}{SchemaVersion: 1, OK: false, Command: command, Error: &genesisErrorDetail{Code: code, Message: message}})
	} else {
		fmt.Fprintln(os.Stderr, message)
	}
	os.Exit(1)
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
	case watchStoreDifferentStore:
		r.Note = appendNote(r.Note, "a maestro daemon with --watch-store is running on this host, but it watches a different --store path; it will not hot-reconcile this row")
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
func genesisNextCommands(command, storePath, file, projectID, fingerprint, baseline, effect string) []string {
	switch command {
	case "plan":
		if effect == configstore.EffectConflict {
			return []string{
				"# resolve the identity conflict above before applying; plan/apply never overwrite by name",
			}
		}
		return []string{
			fmt.Sprintf("maestro project apply --file %s --db %s --confirm %s --fingerprint %s --baseline %s --json", shellQuote(file), shellQuote(storePath), shellQuote(projectID), shellQuote(fingerprint), shellQuote(baseline)),
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
			fmt.Sprintf("maestro project plan --file %s --db %s --json   # expect effect \"no-op\": the row is stored", shellQuote(file), shellQuote(storePath)),
		}
	}
	return nil
}

func shellQuote(s string) string {
	if s != "" && strings.IndexFunc(s, func(r rune) bool {
		return !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || strings.ContainsRune("_@%+=:,./-", r))
	}) == -1 {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'"
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
	fmt.Printf("baseline:    %s\n", r.BaselineFingerprint)
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
	watchStoreDifferentStore = "running-with-different-store"
	watchStoreNotObserved    = "not-observed"
	watchStoreUnknown        = "unknown"
)

// probeDaemonWatchStore best-effort inspects local processes for a
// `maestro daemon --watch-store`. It returns watchStoreUnknown when the process
// table cannot be read (non-Linux, restricted /proc). The classification itself
// is delegated to the pure classifyDaemonWatchStore so it stays testable.
func probeDaemonWatchStore(expectedStore string) string {
	procs, ok := readProcCmdlines()
	if !ok {
		return watchStoreUnknown
	}
	return classifyDaemonWatchStore(procs, expectedStore)
}

// classifyDaemonWatchStore decides the watch-store status from a snapshot of
// process argv slices. Pure and deterministic for testing.
func classifyDaemonWatchStore(procs [][]string, expectedStore string) string {
	daemonFound := false
	watchStoreFound := false
	for _, argv := range procs {
		if !looksLikeMaestroDaemon(argv) {
			continue
		}
		daemonFound = true
		if argvHasWatchStore(argv) {
			watchStoreFound = true
			if sameStorePath(daemonStorePath(argv), expectedStore) {
				return watchStoreObserved
			}
		}
	}
	if watchStoreFound {
		return watchStoreDifferentStore
	}
	if daemonFound {
		return watchStoreRunningWithout
	}
	return watchStoreNotObserved
}

func daemonStorePath(argv []string) string {
	for i, arg := range argv {
		if (arg == "--store" || arg == "-store") && i+1 < len(argv) {
			return argv[i+1]
		}
		if strings.HasPrefix(arg, "--store=") || strings.HasPrefix(arg, "-store=") {
			return strings.SplitN(arg, "=", 2)[1]
		}
	}
	return defaultConfigStorePath()
}

func sameStorePath(got, want string) bool {
	if strings.TrimSpace(got) == "" || strings.TrimSpace(want) == "" {
		return false
	}
	gotAbs, gotErr := filepath.Abs(got)
	wantAbs, wantErr := filepath.Abs(want)
	return gotErr == nil && wantErr == nil && filepath.Clean(gotAbs) == filepath.Clean(wantAbs)
}

func defaultProjectStorePath() string {
	return defaultConfigStorePath()
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
