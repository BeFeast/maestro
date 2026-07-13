package configstore

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/befeast/maestro/internal/config"
)

func TestImportDirLoadAllSharesBackends(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	yaml := `
repo: BeFeast/maestro
local_path: ~/src/maestro
model:
  default: codex
  fallback_backends: [claude]
  backends:
    codex:
      cmd: codex
      extra_args: ["--model", "gpt-5.4"]
      prompt_mode: stdin
    claude:
      cmd: claude
      prompt_mode: arg
supervisor:
  enabled: true
  backend: claude
`
	path := filepath.Join(dir, "maestro-supervisor-dogfood.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0644); err != nil {
		t.Fatalf("write yaml: %v", err)
	}

	store := openTestStore(t)
	if err := store.ImportDir(ctx, dir); err != nil {
		t.Fatalf("ImportDir: %v", err)
	}

	var projectYAML string
	if err := store.db.QueryRowContext(ctx, `SELECT config_yaml FROM project WHERE name = ?`, "befeast-maestro").Scan(&projectYAML); err != nil {
		t.Fatalf("query project: %v", err)
	}
	if strings.Contains(projectYAML, "\n  backends:") {
		t.Fatalf("project row should not duplicate backend definitions:\n%s", projectYAML)
	}
	var backendCount int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM backends`).Scan(&backendCount); err != nil {
		t.Fatalf("query backend count: %v", err)
	}
	if backendCount != 2 {
		t.Fatalf("backend count = %d, want 2", backendCount)
	}

	fromYAML, err := config.Parse([]byte(yaml))
	if err != nil {
		t.Fatalf("parse yaml: %v", err)
	}
	fromStore, err := store.Load(ctx, "befeast-maestro")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	fromYAML.SourcePath = ""
	fromStore.SourcePath = ""
	fromStore.SettingsSources = nil // provenance map is set by Load, absent from Parse
	if !reflect.DeepEqual(fromStore, fromYAML) {
		t.Fatalf("store config differs from yaml\nstore=%#v\nyaml=%#v", fromStore.Model, fromYAML.Model)
	}
}

func TestApplySQLiteWALValidatesHeaderChecksum(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "maestro.db")
	store, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()
	if err := store.UpsertProject(ctx, "owner-demo", "repo: owner/demo\n"); err != nil {
		t.Fatalf("seed WAL: %v", err)
	}
	mainDB, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read main database: %v", err)
	}
	wal, err := os.ReadFile(path + "-wal")
	if err != nil {
		t.Fatalf("read WAL: %v", err)
	}
	if _, err := applySQLiteWAL(mainDB, wal); err != nil {
		t.Fatalf("valid WAL: %v", err)
	}
	corrupt := append([]byte(nil), wal...)
	corrupt[24] ^= 0xff
	if _, err := applySQLiteWAL(mainDB, corrupt); err == nil || !strings.Contains(err.Error(), "header checksum") {
		t.Fatalf("corrupt WAL err = %v, want header checksum rejection", err)
	}
	badVersion := append([]byte(nil), wal...)
	badVersion[7] ^= 0xff
	if _, err := applySQLiteWAL(mainDB, badVersion); err == nil || !strings.Contains(err.Error(), "format version") {
		t.Fatalf("bad-version WAL err = %v, want format-version rejection", err)
	}
	want, err := applySQLiteWAL(mainDB, wal)
	if err != nil {
		t.Fatal(err)
	}
	got, err := applySQLiteWAL(mainDB, append(append([]byte(nil), wal...), 1, 2, 3, 4, 5))
	if err != nil {
		t.Fatalf("partial WAL tail should be ignored: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("partial WAL tail changed reconstructed snapshot")
	}
}

// A row imported from a file keeps that file as its SourcePath; the
// "store:<name>" pseudo path is only for rows with no originating file (#801).
func TestLoadKeepsImportedSourcePath(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	yaml := "repo: BeFeast/maestro\n"
	path := filepath.Join(dir, "befeast-maestro.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0644); err != nil {
		t.Fatalf("write yaml: %v", err)
	}

	store := openTestStore(t)
	if err := store.ImportDir(ctx, dir); err != nil {
		t.Fatalf("ImportDir: %v", err)
	}
	cfg, err := store.Load(ctx, "befeast-maestro")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.SourcePath != path {
		t.Fatalf("SourcePath = %q, want %q", cfg.SourcePath, path)
	}
}

func TestExportDirRoundTrips(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	yaml := `
repo: owner/project
issue_labels: [maestro-ready]
model:
  default: codex
  backends:
    codex:
      cmd: codex
      prompt_mode: stdin
`
	if err := os.WriteFile(filepath.Join(dir, "project.yaml"), []byte(yaml), 0644); err != nil {
		t.Fatalf("write yaml: %v", err)
	}

	store := openTestStore(t)
	if err := store.ImportDir(ctx, dir); err != nil {
		t.Fatalf("ImportDir: %v", err)
	}
	exportDir := t.TempDir()
	if err := store.ExportDir(ctx, exportDir); err != nil {
		t.Fatalf("ExportDir: %v", err)
	}
	exported, err := os.ReadFile(filepath.Join(exportDir, "owner-project.yaml"))
	if err != nil {
		t.Fatalf("read export: %v", err)
	}
	if !strings.Contains(string(exported), "backends:") {
		t.Fatalf("export should restore shared backends into portable YAML:\n%s", exported)
	}
	roundTrip, err := config.Parse(exported)
	if err != nil {
		t.Fatalf("parse exported yaml: %v", err)
	}
	original, err := config.Parse([]byte(yaml))
	if err != nil {
		t.Fatalf("parse original yaml: %v", err)
	}
	if !reflect.DeepEqual(roundTrip.Model, original.Model) {
		t.Fatalf("round-trip model = %#v, want %#v", roundTrip.Model, original.Model)
	}
}

// #841: the per-phase backend + effort fields (pipeline.{planner,implementer,
// validator}.{backend,effort}) live in the project document, so the store must
// round-trip them through import → export → parse without a schema change, and a
// row that omits them must load with empty defaults.
func TestExportDirRoundTripsPerPhaseBackendEffort(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	yaml := `
repo: owner/gsd
model:
  default: fable
  backends:
    fable:
      cmd: claude --model fable
    codex:
      cmd: codex
pipeline:
  enabled: true
  planner:
    enabled: true
    backend: fable
    effort: xhigh
  implementer:
    backend: codex
    effort: low
  validator:
    enabled: true
    backend: fable
    effort: high
`
	if err := os.WriteFile(filepath.Join(dir, "gsd.yaml"), []byte(yaml), 0644); err != nil {
		t.Fatalf("write yaml: %v", err)
	}

	store := openTestStore(t)
	if err := store.ImportDir(ctx, dir); err != nil {
		t.Fatalf("ImportDir: %v", err)
	}
	exportDir := t.TempDir()
	if err := store.ExportDir(ctx, exportDir); err != nil {
		t.Fatalf("ExportDir: %v", err)
	}
	exported, err := os.ReadFile(filepath.Join(exportDir, "owner-gsd.yaml"))
	if err != nil {
		t.Fatalf("read export: %v", err)
	}
	roundTrip, err := config.Parse(exported)
	if err != nil {
		t.Fatalf("parse exported yaml: %v", err)
	}
	original, err := config.Parse([]byte(yaml))
	if err != nil {
		t.Fatalf("parse original yaml: %v", err)
	}
	if !reflect.DeepEqual(roundTrip.Pipeline, original.Pipeline) {
		t.Fatalf("round-trip pipeline = %#v, want %#v", roundTrip.Pipeline, original.Pipeline)
	}
	if roundTrip.Pipeline.Implementer.Backend != "codex" || roundTrip.Pipeline.Implementer.Effort != "low" {
		t.Fatalf("implementer backend/effort not preserved: %#v", roundTrip.Pipeline.Implementer)
	}
	if roundTrip.Pipeline.Planner.Effort != "xhigh" || roundTrip.Pipeline.Validator.Effort != "high" {
		t.Fatalf("planner/validator effort not preserved: planner=%q validator=%q",
			roundTrip.Pipeline.Planner.Effort, roundTrip.Pipeline.Validator.Effort)
	}
}

// A project row that predates #841 (no pipeline effort/implementer fields) must
// still load, with the new fields defaulting to empty.
func TestLoadDefaultsPerPhaseFieldsWhenAbsent(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	yaml := `
repo: owner/legacy
model:
  default: claude
  backends:
    claude:
      cmd: claude
pipeline:
  enabled: true
  planner:
    enabled: true
    backend: claude
`
	if err := os.WriteFile(filepath.Join(dir, "legacy.yaml"), []byte(yaml), 0644); err != nil {
		t.Fatalf("write yaml: %v", err)
	}
	store := openTestStore(t)
	if err := store.ImportDir(ctx, dir); err != nil {
		t.Fatalf("ImportDir: %v", err)
	}
	cfg, err := store.Load(ctx, "owner-legacy")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Pipeline.Implementer.Backend != "" || cfg.Pipeline.Implementer.Effort != "" {
		t.Fatalf("absent implementer should default empty: %#v", cfg.Pipeline.Implementer)
	}
	if cfg.Pipeline.Planner.Effort != "" || cfg.Pipeline.Validator.Effort != "" {
		t.Fatalf("absent effort should default empty: planner=%q validator=%q",
			cfg.Pipeline.Planner.Effort, cfg.Pipeline.Validator.Effort)
	}
}

func TestImportDirRejectsDivergentSharedBackend(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	first := `
repo: owner/one
model:
  backends:
    codex:
      cmd: codex
`
	second := `
repo: owner/two
model:
  backends:
    codex:
      cmd: other-codex
`
	if err := os.WriteFile(filepath.Join(dir, "one.yaml"), []byte(first), 0644); err != nil {
		t.Fatalf("write one: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "two.yaml"), []byte(second), 0644); err != nil {
		t.Fatalf("write two: %v", err)
	}
	store := openTestStore(t)
	err := store.ImportDir(ctx, dir)
	if err == nil || !strings.Contains(err.Error(), "conflicting cmd") {
		t.Fatalf("ImportDir err = %v, want conflicting-cmd error", err)
	}
}

// Two projects may annotate the same backend with different OPTIONAL metadata
// (provider/model/pricing/effort) while keeping an identical cmd. That is not a
// conflict — the cmd dispatches the worker — so ImportDir must merge them
// (richest wins) rather than reject, which is what unblocks the single-daemon
// config-store seed across the live fleet (per-project configs drifted in
// metadata only). Reproduces the real maestro.d divergence in miniature.
func TestImportDirMergesBackendMetadataWhenCmdMatches(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	// thin: cmd only
	thin := `
repo: owner/thin
model:
  backends:
    claude:
      cmd: claude --model opus[1m]
`
	// rich: same cmd, extra metadata
	rich := `
repo: owner/rich
model:
  backends:
    claude:
      cmd: claude --model opus[1m]
      provider: anthropic
      model: opus-4.8
      pricing:
        input_usd_per_mtok: 5
        output_usd_per_mtok: 25
`
	if err := os.WriteFile(filepath.Join(dir, "a-thin.yaml"), []byte(thin), 0644); err != nil {
		t.Fatalf("write thin: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "b-rich.yaml"), []byte(rich), 0644); err != nil {
		t.Fatalf("write rich: %v", err)
	}
	store := openTestStore(t)
	if err := store.ImportDir(ctx, dir); err != nil {
		t.Fatalf("ImportDir err = %v, want merge success (cmd matches, metadata differs)", err)
	}

	// The stored shared backend must be the superset (richest wins) regardless
	// of import order.
	var def string
	if err := store.db.QueryRowContext(ctx, `SELECT definition_yaml FROM backends WHERE name = ?`, "claude").Scan(&def); err != nil {
		t.Fatalf("read merged backend: %v", err)
	}
	for _, want := range []string{"provider: anthropic", "model: opus-4.8", "input_usd_per_mtok: 5", "cmd: claude --model opus[1m]"} {
		if !strings.Contains(def, want) {
			t.Fatalf("merged backend missing %q\n--- def ---\n%s", want, def)
		}
	}
}

// Same cmd but a DIFFERENT execution-affecting field (prompt_mode) is a real
// conflict — the store keeps one global backend, so a silent overlay would make
// one project deliver its prompt the other's way. Must reject, not merge.
func TestImportDirRejectsDivergentBehavioralBackendField(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	a := `
repo: owner/a
model:
  backends:
    claude:
      cmd: claude
      prompt_mode: stdin
`
	b := `
repo: owner/b
model:
  backends:
    claude:
      cmd: claude
      prompt_mode: arg
`
	if err := os.WriteFile(filepath.Join(dir, "a.yaml"), []byte(a), 0644); err != nil {
		t.Fatalf("write a: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "b.yaml"), []byte(b), 0644); err != nil {
		t.Fatalf("write b: %v", err)
	}
	store := openTestStore(t)
	err := store.ImportDir(ctx, dir)
	if err == nil || !strings.Contains(err.Error(), "conflicting execution settings") {
		t.Fatalf("ImportDir err = %v, want conflicting-execution-settings error", err)
	}
}

// Complementary nested pricing fields must converge to the superset, not have one
// side's map overwrite the other's (a top-level overlay would drop fields).
func TestImportDirDeepMergesNestedPricing(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	a := `
repo: owner/a
model:
  backends:
    claude:
      cmd: claude
      pricing:
        input_usd_per_mtok: 5
`
	b := `
repo: owner/b
model:
  backends:
    claude:
      cmd: claude
      pricing:
        output_usd_per_mtok: 25
`
	if err := os.WriteFile(filepath.Join(dir, "a.yaml"), []byte(a), 0644); err != nil {
		t.Fatalf("write a: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "b.yaml"), []byte(b), 0644); err != nil {
		t.Fatalf("write b: %v", err)
	}
	store := openTestStore(t)
	if err := store.ImportDir(ctx, dir); err != nil {
		t.Fatalf("ImportDir err = %v, want success", err)
	}
	var def string
	if err := store.db.QueryRowContext(ctx, `SELECT definition_yaml FROM backends WHERE name = ?`, "claude").Scan(&def); err != nil {
		t.Fatalf("read merged backend: %v", err)
	}
	for _, want := range []string{"input_usd_per_mtok: 5", "output_usd_per_mtok: 25"} {
		if !strings.Contains(def, want) {
			t.Fatalf("merged pricing missing %q (deep-merge dropped a field)\n--- def ---\n%s", want, def)
		}
	}
}

func openTestStore(t *testing.T) *Store {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), "config.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}
