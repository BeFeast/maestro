package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/configstore"
)

const projectTestYAML = `repo: BeFeast/demo
local_path: /srv/example-src/demo
worktree_base: /srv/example-worktrees/demo
project_id: 3f2504e0-4f89-41d3-9a0c-0305e82c3301
management_home:
  kind: obsidian
  path: /srv/example-vault/Dev/Areas/demo
  vault: Example Vault
  vault_path: Dev/Areas/demo
`

const projectTestSharedBackendYAML = projectTestYAML + `model:
  default: claude
  fallback_backends: [codex]
  backends:
    claude:
      cmd: claude
    codex:
      cmd: codex
      prompt_mode: stdin
`

const projectTestSharedBackendRawYAML = projectTestYAML + `model:
  default: claude
  fallback_backends: [codex]
`

func TestUnifiedDefaultStorePath(t *testing.T) {
	t.Setenv("HOME", "/srv/example-home")
	want := "/srv/example-home/.maestro/maestro.db"
	if got := defaultConfigStorePath(); got != want {
		t.Fatalf("defaultConfigStorePath() = %q, want %q", got, want)
	}
	if got := defaultProjectStorePath(); got != want {
		t.Fatalf("defaultProjectStorePath() = %q, want %q", got, want)
	}
}

func TestDefaultStoreProtectsLegacyProjectsUntilMigration(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.Mkdir(filepath.Join(home, ".maestro"), 0o755); err != nil {
		t.Fatal(err)
	}
	legacy := legacyConfigStorePath()
	legacyStore, err := configstore.Open(legacy)
	if err != nil {
		t.Fatalf("open legacy store: %v", err)
	}
	if err := legacyStore.UpsertProject(context.Background(), "owner-demo", "repo: owner/demo\n"); err != nil {
		t.Fatalf("seed legacy store: %v", err)
	}
	if err := legacyStore.Close(); err != nil {
		t.Fatal(err)
	}

	if got := defaultConfigStorePath(); got != legacy {
		t.Fatalf("default with unmigrated projects = %q, want legacy %q", got, legacy)
	}
	canonical := canonicalConfigStorePath()
	if err := refuseUnmigratedCanonicalStore(canonical); err == nil || !strings.Contains(err.Error(), "refusing an empty-fleet start") {
		t.Fatalf("canonical service guard err = %v", err)
	}

	canonicalStore, err := configstore.Open(canonical)
	if err != nil {
		t.Fatalf("open canonical store: %v", err)
	}
	if err := canonicalStore.UpsertProject(context.Background(), "owner-demo", "repo: owner/demo\n"); err != nil {
		t.Fatalf("seed canonical store: %v", err)
	}
	if err := canonicalStore.Close(); err != nil {
		t.Fatal(err)
	}
	if got := defaultConfigStorePath(); got != canonical {
		t.Fatalf("default after migration = %q, want canonical %q", got, canonical)
	}
	if err := refuseUnmigratedCanonicalStore(canonical); err != nil {
		t.Fatalf("canonical guard after migration: %v", err)
	}
}

func TestDefaultStoreInspectionFailureStaysOnLegacyPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.Mkdir(filepath.Join(home, ".maestro"), 0o755); err != nil {
		t.Fatal(err)
	}
	legacy := legacyConfigStorePath()
	if err := os.WriteFile(legacy, []byte("not a sqlite database"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := defaultConfigStorePath(); got != legacy {
		t.Fatalf("default after legacy inspection failure = %q, want fail-closed legacy %q", got, legacy)
	}
	if err := refuseUnmigratedCanonicalStore(canonicalConfigStorePath()); err == nil {
		t.Fatal("explicit canonical service should refuse when legacy inspection fails")
	}
}

func TestClassifyDaemonWatchStore(t *testing.T) {
	cases := []struct {
		name  string
		procs [][]string
		want  string
	}{
		{
			"daemon with watch-store",
			[][]string{{"/usr/local/bin/maestro", "daemon", "--watch-store", "--store", "/x/config.db"}},
			watchStoreObserved,
		},
		{
			"daemon without watch-store",
			[][]string{{"/usr/local/bin/maestro", "daemon", "--store", "/x/config.db"}},
			watchStoreRunningWithout,
		},
		{
			"no daemon present",
			[][]string{{"/usr/local/bin/maestro", "project", "plan"}, {"/bin/bash"}},
			watchStoreNotObserved,
		},
		{
			"watch-store on a non-daemon is ignored",
			[][]string{{"/usr/local/bin/maestro", "serve", "--watch-store"}},
			watchStoreNotObserved,
		},
		{
			"watch-store=false is not observed",
			[][]string{{"maestro", "daemon", "--watch-store=false"}},
			watchStoreRunningWithout,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyDaemonWatchStore(tc.procs, "/x/config.db"); got != tc.want {
				t.Fatalf("classifyDaemonWatchStore = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestClassifyDaemonWatchStoreRequiresMatchingStore(t *testing.T) {
	procs := [][]string{{"/usr/local/bin/maestro", "daemon", "--watch-store", "--store", "/x/other.db"}}
	if got := classifyDaemonWatchStore(procs, "/x/config.db"); got != watchStoreDifferentStore {
		t.Fatalf("classify = %q, want different-store", got)
	}
}

func TestClassifyDaemonWatchStoreDoesNotGuessImplicitStore(t *testing.T) {
	t.Setenv("HOME", "/srv/example-home")
	procs := [][]string{{"/usr/local/bin/maestro", "daemon", "--watch-store"}}
	if got := classifyDaemonWatchStore(procs, "/srv/example-home/.maestro/maestro.db"); got != watchStoreImplicitStore {
		t.Fatalf("classify implicit-store daemon = %q, want implicit-store", got)
	}
}

func TestRemoteMatchesRepoGitHubHostsAndSchemes(t *testing.T) {
	tests := []struct {
		name   string
		remote string
		repo   string
		want   bool
	}{
		{"scp-style SSH", "git@github.com:BeFeast/demo.git", "BeFeast/demo", true},
		{"case-insensitive SCP host", "GIT@GITHUB.COM:BEFEAST/DEMO.GIT", "BeFeast/demo", true},
		{"HTTPS", "https://github.com/BeFeast/demo.git", "BeFeast/demo", true},
		{"HTTP", "http://github.com/BeFeast/demo", "BeFeast/demo", true},
		{"SSH URL", "ssh://git@github.com/BeFeast/demo.git", "BeFeast/demo", true},
		{"git protocol", "git://github.com/BeFeast/demo.git", "BeFeast/demo", true},
		{"case insensitive", "HTTPS://GITHUB.COM/BEFEAST/DEMO.GIT", "BeFeast/demo", true},
		{"wrong HTTPS host", "https://evil.example/BeFeast/demo.git", "BeFeast/demo", false},
		{"wrong scp host", "git@gitlab.com:BeFeast/demo.git", "BeFeast/demo", false},
		{"unsupported file scheme", "file:///tmp/BeFeast/demo.git", "BeFeast/demo", false},
		{"unsupported FTP scheme", "ftp://github.com/BeFeast/demo.git", "BeFeast/demo", false},
		{"wrong repository", "https://github.com/BeFeast/other.git", "BeFeast/demo", false},
		{"extra path component", "https://github.com/mirror/BeFeast/demo.git", "BeFeast/demo", false},
		{"schemeless URL", "github.com/BeFeast/demo.git", "BeFeast/demo", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := remoteMatchesRepo(tc.remote, tc.repo); got != tc.want {
				t.Fatalf("remoteMatchesRepo(%q, %q) = %t, want %t", tc.remote, tc.repo, got, tc.want)
			}
		})
	}
}

func TestArgvHasWatchStore(t *testing.T) {
	cases := []struct {
		argv []string
		want bool
	}{
		{[]string{"maestro", "daemon", "--watch-store"}, true},
		{[]string{"maestro", "daemon", "-watch-store"}, true},
		{[]string{"maestro", "daemon", "--watch-store=true"}, true},
		{[]string{"maestro", "daemon", "--watch-store=1"}, true},
		{[]string{"maestro", "daemon", "--watch-store=false"}, false},
		{[]string{"maestro", "daemon", "--watch-store=0"}, false},
		{[]string{"maestro", "daemon"}, false},
	}
	for _, tc := range cases {
		if got := argvHasWatchStore(tc.argv); got != tc.want {
			t.Fatalf("argvHasWatchStore(%v) = %v, want %v", tc.argv, got, tc.want)
		}
	}
}

func TestSplitProcCmdline(t *testing.T) {
	// /proc/<pid>/cmdline is NUL-separated with a trailing NUL.
	raw := []byte("/usr/local/bin/maestro\x00daemon\x00--watch-store\x00")
	argv := splitProcCmdline(raw)
	want := []string{"/usr/local/bin/maestro", "daemon", "--watch-store"}
	if strings.Join(argv, "|") != strings.Join(want, "|") {
		t.Fatalf("splitProcCmdline = %v, want %v", argv, want)
	}
}

func TestGenesisNextCommands(t *testing.T) {
	// plan → the exact apply command with the confirm id.
	next := genesisNextCommands("plan", "/x/store.db", "/p.yaml", "the-id", "sha256:desired", configstore.BaselineAbsent, configstore.EffectCreate)
	if len(next) != 1 || !strings.Contains(next[0], "project apply") || !strings.Contains(next[0], "--confirm the-id") || !strings.Contains(next[0], "--fingerprint sha256:desired") || !strings.Contains(next[0], "--baseline absent") {
		t.Fatalf("plan next = %v, want an apply command with confirm + desired/baseline fingerprints", next)
	}
	// apply → re-run plan and expect a no-op (scriptable confirmation).
	next = genesisNextCommands("apply", "/x/store.db", "/p.yaml", "the-id", "sha256:desired", configstore.BaselineAbsent, configstore.EffectCreate)
	if len(next) != 1 || !strings.Contains(next[0], "project plan") || !strings.Contains(next[0], "no-op") {
		t.Fatalf("apply next = %v, want a plan re-run expecting no-op", next)
	}
	// conflict → no runnable next command, just guidance.
	next = genesisNextCommands("plan", "/x/store.db", "/p.yaml", "the-id", "sha256:desired", configstore.BaselineAbsent, configstore.EffectConflict)
	if len(next) != 1 || !strings.Contains(next[0], "conflict") {
		t.Fatalf("conflict next = %v, want conflict guidance", next)
	}
}

func TestShellQuote(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"safe", "/tmp/project.yaml", "/tmp/project.yaml"},
		{"spaces", "/tmp/project files/config.yaml", "'/tmp/project files/config.yaml'"},
		{"single quote", "/tmp/it's ready/config.yaml", `'/tmp/it'"'"'s ready/config.yaml'`},
		{"empty", "", "''"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := shellQuote(tc.in); got != tc.want {
				t.Fatalf("shellQuote(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestArgsRequestJSON(t *testing.T) {
	tests := []struct {
		args []string
		want bool
	}{
		{[]string{"--json"}, true},
		{[]string{"--file", "p.yaml", "--json=true"}, true},
		{[]string{"--json=garbage"}, true},
		{[]string{"--json=false"}, false},
		{[]string{"--file", "p.yaml"}, false},
	}
	for _, tc := range tests {
		if got := argsRequestJSON(tc.args); got != tc.want {
			t.Fatalf("argsRequestJSON(%q) = %t, want %t", tc.args, got, tc.want)
		}
	}
}

func TestValidateApplyApprovalRequiresExactPlanEnvelope(t *testing.T) {
	p := &configstore.PreparedProject{ProjectID: "the-id", Fingerprint: "sha256:desired"}
	if err := validateApplyApproval(p, "the-id", "sha256:desired", configstore.BaselineAbsent); err != nil {
		t.Fatalf("valid approval: %v", err)
	}
	for _, tc := range []struct {
		confirm, fingerprint, baseline string
	}{
		{"", "sha256:desired", configstore.BaselineAbsent},
		{"wrong", "sha256:desired", configstore.BaselineAbsent},
		{"the-id", "", configstore.BaselineAbsent},
		{"the-id", "sha256:wrong", configstore.BaselineAbsent},
		{"the-id", "sha256:desired", ""},
	} {
		if err := validateApplyApproval(p, tc.confirm, tc.fingerprint, tc.baseline); err == nil {
			t.Fatalf("validateApplyApproval(%q,%q,%q) = nil, want refusal", tc.confirm, tc.fingerprint, tc.baseline)
		}
	}
}

func TestGenesisReconcileForFoldsWatchStore(t *testing.T) {
	// A create with an observed daemon carries the single-daemon expectation and
	// no extra note.
	r := genesisReconcileFor("apply", configstore.EffectCreate, "", watchStoreObserved)
	if !strings.Contains(r.Expectation, "watch-store") {
		t.Fatalf("expectation = %q, want to mention watch-store", r.Expectation)
	}
	if r.Note != "" {
		t.Fatalf("observed daemon should not add a note, got %q", r.Note)
	}
	// When the daemon is not observed, the note warns hot reconciliation can't be
	// confirmed.
	r = genesisReconcileFor("apply", configstore.EffectCreate, "", watchStoreNotObserved)
	if !strings.Contains(r.Note, "no running maestro daemon") {
		t.Fatalf("not-observed note = %q, want a daemon warning", r.Note)
	}
	// A running-without-watch-store daemon is called out explicitly.
	r = genesisReconcileFor("apply", configstore.EffectCreate, "", watchStoreRunningWithout)
	if !strings.Contains(r.Note, "WITHOUT --watch-store") {
		t.Fatalf("running-without note = %q, want a --watch-store warning", r.Note)
	}
	r = genesisReconcileFor("apply", configstore.EffectCreate, "", watchStoreImplicitStore)
	if !strings.Contains(r.Note, "startup-time default") || !strings.Contains(r.Note, "explicit --store") {
		t.Fatalf("implicit-store note = %q, want migration-safe ambiguity guidance", r.Note)
	}
	// A conflict is a hard stop with no reconciliation.
	r = genesisReconcileFor("plan", configstore.EffectConflict, "boom", watchStoreObserved)
	if !strings.Contains(r.Expectation, "hard stop") || r.Note != "boom" {
		t.Fatalf("conflict reconcile = %+v, want hard-stop expectation and the conflict note", r)
	}
}

// planReport against a store that does not exist must NOT create the store file:
// `project plan` changes no files.
func TestPlanReportEmptyStoreCreatesNoFile(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "absent.db")
	prepared, err := configstore.PrepareProject("p.yaml", []byte(projectTestYAML))
	if err != nil {
		t.Fatalf("PrepareProject: %v", err)
	}
	report := planReport(dbPath, prepared, false)
	if report.Effect != configstore.EffectCreate || report.Wrote {
		t.Fatalf("empty-store plan = %+v, want create+!wrote", report)
	}
	if _, err := os.Stat(dbPath); !os.IsNotExist(err) {
		t.Fatalf("plan created the store file %s (want it absent): stat err = %v", dbPath, err)
	}
}

func TestPlanReportExistingStoreChangesNoDatabaseBytesOrSidecars(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "config.db")
	store, err := configstore.Open(dbPath)
	if err != nil {
		t.Fatalf("open writable fixture: %v", err)
	}
	if err := store.UpsertProject(context.Background(), "befeast-demo", projectTestYAML); err != nil {
		t.Fatalf("seed fixture: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close fixture: %v", err)
	}
	prepared, err := configstore.PrepareProject("p.yaml", []byte(projectTestYAML))
	if err != nil {
		t.Fatalf("PrepareProject: %v", err)
	}
	before, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatalf("read DB before plan: %v", err)
	}
	walBefore := storeFileExists(dbPath + "-wal")
	shmBefore := storeFileExists(dbPath + "-shm")

	report := planReport(dbPath, prepared, false)
	if report.Effect != configstore.EffectNoOp || report.Wrote {
		t.Fatalf("existing-store plan = %+v, want no-op+!wrote", report)
	}
	after, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatalf("read DB after plan: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("project plan changed existing SQLite database bytes")
	}
	if storeFileExists(dbPath+"-wal") != walBefore || storeFileExists(dbPath+"-shm") != shmBefore {
		t.Fatal("project plan created or removed SQLite sidecar files")
	}
}

func TestPlanReportActiveWALChangesNoDatabaseFiles(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "config.db")
	store, err := configstore.Open(dbPath)
	if err != nil {
		t.Fatalf("open writable fixture: %v", err)
	}
	defer store.Close()
	if err := store.UpsertProject(context.Background(), "befeast-demo", projectTestYAML); err != nil {
		t.Fatalf("seed active WAL fixture: %v", err)
	}
	paths := []string{dbPath, dbPath + "-wal", dbPath + "-shm"}
	before := make(map[string][]byte, len(paths))
	for _, path := range paths {
		before[path], err = os.ReadFile(path)
		if err != nil {
			t.Fatalf("read active WAL fixture %s: %v", path, err)
		}
	}
	prepared, err := configstore.PrepareProject("p.yaml", []byte(projectTestYAML))
	if err != nil {
		t.Fatalf("PrepareProject: %v", err)
	}
	report := planReport(dbPath, prepared, false)
	if report.Effect != configstore.EffectNoOp || report.Wrote {
		t.Fatalf("active-WAL plan = %+v, want no-op+!wrote", report)
	}
	for _, path := range paths {
		after, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read after plan %s: %v", path, err)
		}
		if !bytes.Equal(before[path], after) {
			t.Fatalf("project plan changed active SQLite file %s", path)
		}
	}
}

func TestPlanSharedBackendUpdateChangesNoDatabaseFiles(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "config.db")
	store, err := configstore.Open(dbPath)
	if err != nil {
		t.Fatalf("open writable fixture: %v", err)
	}
	defer store.Close()
	if err := store.UpsertProject(context.Background(), "befeast-demo", projectTestSharedBackendYAML); err != nil {
		t.Fatalf("seed shared-backend fixture: %v", err)
	}
	file := filepath.Join(dir, "project.yaml")
	desired := strings.Replace(projectTestSharedBackendRawYAML, "/srv/example-src/demo", "/srv/updated-src/demo", 1)
	if err := os.WriteFile(file, []byte(desired), 0o644); err != nil {
		t.Fatalf("write desired project: %v", err)
	}

	paths := []string{dbPath, dbPath + "-wal", dbPath + "-shm"}
	before := make(map[string][]byte, len(paths))
	for _, path := range paths {
		before[path], err = os.ReadFile(path)
		if err != nil {
			t.Fatalf("read active shared-backend fixture %s: %v", path, err)
		}
	}

	prepared := prepareGenesisFile(file, dbPath, "project plan", false)
	report := planReport(dbPath, prepared, false)
	if report.Effect != configstore.EffectUpdate || report.Wrote {
		t.Fatalf("shared-backend plan = %+v, want update+!wrote", report)
	}
	for _, path := range paths {
		after, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read after shared-backend plan %s: %v", path, err)
		}
		if !bytes.Equal(before[path], after) {
			t.Fatalf("shared-backend project plan changed active SQLite file %s", path)
		}
	}
}

// A full plan→apply→plan receipt cycle through a real store: the JSON is
// well-formed and the effect transitions create → no-op, exactly one row lands.
func TestGenesisReceiptCycle(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "config.db")
	file := filepath.Join(dir, "p.yaml")
	if err := os.WriteFile(file, []byte(projectTestYAML), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	prepared, err := configstore.PrepareProject(file, []byte(projectTestYAML))
	if err != nil {
		t.Fatalf("PrepareProject: %v", err)
	}

	// plan (absent store) → create.
	planRep := planReport(dbPath, prepared, false)
	planReceipt := buildGenesisReceipt("plan", dbPath, file, prepared, planRep)
	if planReceipt.Effect != configstore.EffectCreate || planReceipt.Wrote {
		t.Fatalf("plan receipt = %+v, want create+!wrote", planReceipt)
	}
	if planReceipt.SchemaVersion != 1 || planReceipt.BaselineFingerprint != configstore.BaselineAbsent {
		t.Fatalf("plan receipt envelope = %+v, want schema v1 + absent baseline", planReceipt)
	}
	// The receipt must round-trip through JSON (adapter contract).
	if _, err := json.Marshal(planReceipt); err != nil {
		t.Fatalf("marshal plan receipt: %v", err)
	}

	// apply → create + wrote, exactly one row.
	store, err := configstore.Open(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()
	applyRep, err := store.ApplyProject(ctx, prepared, prepared.ProjectID, prepared.Fingerprint, planReceipt.BaselineFingerprint)
	if err != nil {
		t.Fatalf("ApplyProject: %v", err)
	}
	applyReceipt := buildGenesisReceipt("apply", dbPath, file, prepared, applyRep)
	if applyReceipt.Effect != configstore.EffectCreate || !applyReceipt.Wrote {
		t.Fatalf("apply receipt = %+v, want create+wrote", applyReceipt)
	}
	if len(applyReceipt.Next) == 0 || !strings.Contains(applyReceipt.Next[0], "project plan") {
		t.Fatalf("apply next = %v, want a plan re-run", applyReceipt.Next)
	}

	// Re-plan → no-op (the scriptable confirmation the row landed).
	prepared2, err := configstore.PrepareProject(file, []byte(projectTestYAML))
	if err != nil {
		t.Fatalf("re-prepare: %v", err)
	}
	rep2, err := store.PlanProject(ctx, prepared2)
	if err != nil {
		t.Fatalf("re-plan: %v", err)
	}
	if rep2.Effect != configstore.EffectNoOp {
		t.Fatalf("re-plan effect = %q, want no-op", rep2.Effect)
	}
}

func TestStoreFileExists(t *testing.T) {
	dir := t.TempDir()
	if storeFileExists(filepath.Join(dir, "nope.db")) {
		t.Fatal("absent file reported as existing")
	}
	f := filepath.Join(dir, "there.db")
	if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if !storeFileExists(f) {
		t.Fatal("present file reported as absent")
	}
	if storeFileExists(dir) {
		t.Fatal("directory reported as a store file")
	}
}

func TestInspectStoreFileRejectsNonRegularAndStatErrors(t *testing.T) {
	dir := t.TempDir()

	exists, err := inspectStoreFile(dir)
	if err == nil || exists || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("inspectStoreFile(directory) = (%t, %v), want false + non-regular error", exists, err)
	}

	loop := filepath.Join(dir, "symlink-loop")
	if err := os.Symlink("symlink-loop", loop); err != nil {
		t.Fatalf("create symlink loop: %v", err)
	}
	exists, err = inspectStoreFile(loop)
	if err == nil || exists {
		t.Fatalf("inspectStoreFile(stat error) = (%t, %v), want false + error", exists, err)
	}

	readOnlyParent := filepath.Join(dir, "read-only")
	if err := os.Mkdir(readOnlyParent, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(readOnlyParent, 0o755) })
	exists, err = inspectStoreFile(filepath.Join(readOnlyParent, "maestro.db"))
	if err == nil || exists || !strings.Contains(err.Error(), "not writable") {
		t.Fatalf("inspectStoreFile(unwritable parent) = (%t, %v), want false + not-writable error", exists, err)
	}

	exists, err = inspectStoreFile(filepath.Join(dir, "new", "nested", "maestro.db"))
	if err == nil || exists || !strings.Contains(err.Error(), "unavailable") {
		t.Fatalf("inspectStoreFile(missing immediate parent) = (%t, %v), want false + unavailable error", exists, err)
	}
}

func TestValidateGenesisRuntime(t *testing.T) {
	dir := t.TempDir()
	repoDir := filepath.Join(dir, "repo")
	if err := os.Mkdir(repoDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"init", repoDir},
		{"-C", repoDir, "remote", "add", "origin", "git@github.com:BeFeast/demo.git"},
	} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	p := &configstore.PreparedProject{
		Repo:           "BeFeast/demo",
		LocalPath:      repoDir,
		WorktreeBase:   filepath.Join(dir, "worktrees", "demo"),
		ManagementHome: config.ManagementHomeConfig{Path: filepath.Join(dir, "management-home")},
	}
	if err := os.Mkdir(filepath.Dir(p.WorktreeBase), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(p.ManagementHome.Path, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := validateGenesisRuntime(p); err != nil {
		t.Fatalf("valid runtime preflight: %v", err)
	}
	if err := os.Remove(p.ManagementHome.Path); err != nil {
		t.Fatal(err)
	}
	if err := validateGenesisRuntime(p); err == nil || !strings.Contains(err.Error(), "management_home.path") {
		t.Fatalf("missing management home err = %v", err)
	}
	if err := os.Mkdir(p.ManagementHome.Path, 0o755); err != nil {
		t.Fatal(err)
	}
	p.Repo = "BeFeast/other"
	if err := validateGenesisRuntime(p); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("remote mismatch err = %v", err)
	}
}
