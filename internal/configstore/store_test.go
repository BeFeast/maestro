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
	if err == nil || !strings.Contains(err.Error(), "differs") {
		t.Fatalf("ImportDir err = %v, want divergent backend error", err)
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
