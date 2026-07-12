package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/befeast/maestro/internal/configstore"
)

const projectTestYAML = `repo: BeFeast/demo
local_path: ~/src/demo
worktree_base: ~/.worktrees/demo
project_id: 3f2504e0-4f89-41d3-9a0c-0305e82c3301
management_home:
  kind: obsidian
  path: /home/god/Obsidian/Dev
  vault: Obsidian Vault
  vault_path: Dev/Areas/demo
`

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
			if got := classifyDaemonWatchStore(tc.procs); got != tc.want {
				t.Fatalf("classifyDaemonWatchStore = %q, want %q", got, tc.want)
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
	next := genesisNextCommands("plan", "/x/store.db", "/p.yaml", "the-id", configstore.EffectCreate)
	if len(next) != 1 || !strings.Contains(next[0], "project apply") || !strings.Contains(next[0], "--confirm the-id") {
		t.Fatalf("plan next = %v, want an apply command with --confirm", next)
	}
	// apply → re-run plan and expect a no-op (scriptable confirmation).
	next = genesisNextCommands("apply", "/x/store.db", "/p.yaml", "the-id", configstore.EffectCreate)
	if len(next) != 1 || !strings.Contains(next[0], "project plan") || !strings.Contains(next[0], "no-op") {
		t.Fatalf("apply next = %v, want a plan re-run expecting no-op", next)
	}
	// conflict → no runnable next command, just guidance.
	next = genesisNextCommands("plan", "/x/store.db", "/p.yaml", "the-id", configstore.EffectConflict)
	if len(next) != 1 || !strings.Contains(next[0], "conflict") {
		t.Fatalf("conflict next = %v, want conflict guidance", next)
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
	report := planReport(dbPath, prepared)
	if report.Effect != configstore.EffectCreate || report.Wrote {
		t.Fatalf("empty-store plan = %+v, want create+!wrote", report)
	}
	if _, err := os.Stat(dbPath); !os.IsNotExist(err) {
		t.Fatalf("plan created the store file %s (want it absent): stat err = %v", dbPath, err)
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
	planRep := planReport(dbPath, prepared)
	planReceipt := buildGenesisReceipt("plan", dbPath, file, prepared, planRep)
	if planReceipt.Effect != configstore.EffectCreate || planReceipt.Wrote {
		t.Fatalf("plan receipt = %+v, want create+!wrote", planReceipt)
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
	applyRep, err := store.ApplyProject(ctx, prepared, prepared.ProjectID, prepared.Fingerprint)
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
