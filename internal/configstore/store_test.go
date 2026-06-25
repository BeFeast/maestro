package configstore

import (
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
	if !reflect.DeepEqual(fromStore, fromYAML) {
		t.Fatalf("store config differs from yaml\nstore=%#v\nyaml=%#v", fromStore.Model, fromYAML.Model)
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

func openTestStore(t *testing.T) *Store {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), "config.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}
