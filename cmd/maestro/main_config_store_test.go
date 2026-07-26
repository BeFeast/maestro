package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/befeast/maestro/internal/configstore"
	_ "modernc.org/sqlite"
)

const editTestYAML = `repo: owner/svc
max_parallel: 2
model:
  default: codex
  backends:
    codex:
      cmd: codex
      prompt_mode: stdin
`

// writeFakeEditor writes an executable shell script that mutates the file it is
// given, and points $EDITOR at it, so editStoreProject can be exercised without
// a real interactive editor.
func writeFakeEditor(t *testing.T, script string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fake-editor.sh")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script+"\n"), 0o755); err != nil {
		t.Fatalf("write fake editor: %v", err)
	}
	t.Setenv("EDITOR", path)
	t.Setenv("VISUAL", "")
}

func openEditTestStore(t *testing.T) *configstore.Store {
	t.Helper()
	store, err := configstore.Open(filepath.Join(t.TempDir(), "config.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	if err := store.UpsertProject(context.Background(), "svc", editTestYAML); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	return store
}

func TestEditStoreProjectAppliesEdit(t *testing.T) {
	store := openEditTestStore(t)
	writeFakeEditor(t, `sed -i 's/max_parallel: 2/max_parallel: 42/' "$1"`)

	if err := editStoreProject(store, "svc"); err != nil {
		t.Fatalf("editStoreProject: %v", err)
	}
	cfg, err := store.Load(context.Background(), "svc")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.MaxParallel != 42 {
		t.Fatalf("MaxParallel = %d, want 42 after edit", cfg.MaxParallel)
	}
}

func TestEditStoreProjectNoChangeKeepsConfig(t *testing.T) {
	store := openEditTestStore(t)
	writeFakeEditor(t, `true`) // touch nothing

	if err := editStoreProject(store, "svc"); err != nil {
		t.Fatalf("editStoreProject: %v", err)
	}
	cfg, err := store.Load(context.Background(), "svc")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.MaxParallel != 2 {
		t.Fatalf("MaxParallel = %d, want 2 (unchanged)", cfg.MaxParallel)
	}
}

func TestEditStoreProjectInvalidEditRejected(t *testing.T) {
	store := openEditTestStore(t)
	writeFakeEditor(t, `printf '%s' '::: not yaml :::' > "$1"`)

	if err := editStoreProject(store, "svc"); err == nil {
		t.Fatal("editStoreProject with invalid YAML: want error, got nil")
	}
	// The stored row must be untouched by a rejected edit.
	cfg, err := store.Load(context.Background(), "svc")
	if err != nil {
		t.Fatalf("Load after rejected edit: %v", err)
	}
	if cfg.MaxParallel != 2 {
		t.Fatalf("MaxParallel = %d, want 2 (edit must not land)", cfg.MaxParallel)
	}
}

// seedListTestStore creates a store with two projects and then corrupts one of
// the config documents behind UpsertProject's back — that is the state #1123
// cares about: LoadAll fails, so listing must not go through it.
func seedListTestStore(t *testing.T) string {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "maestro.db")
	store, err := configstore.Open(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	ctx := context.Background()
	for _, name := range []string{"svc", "broken"} {
		if err := store.UpsertProject(ctx, name, editTestYAML); err != nil {
			t.Fatalf("seed project %s: %v", name, err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open raw db: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(`UPDATE project SET config_yaml = ? WHERE name = ?`, "repo: [unterminated\n", "broken"); err != nil {
		t.Fatalf("corrupt project row: %v", err)
	}
	return dbPath
}

func TestListStoreProjectsListsRowWithInvalidConfig(t *testing.T) {
	dbPath := seedListTestStore(t)

	// Guard the premise: anything that loads configs is dead in this state.
	store, err := configstore.Open(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()
	if _, err := store.LoadAll(context.Background()); err == nil {
		t.Fatal("LoadAll succeeded on a corrupted row; test no longer covers the broken-config case")
	}

	var buf bytes.Buffer
	if err := listStoreProjects(&buf, dbPath, false); err != nil {
		t.Fatalf("listStoreProjects: %v", err)
	}
	got := strings.Fields(buf.String())
	want := []string{"broken", "svc"}
	if len(got) != len(want) {
		t.Fatalf("listStoreProjects printed %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("listStoreProjects printed %q, want %q", got, want)
		}
	}
}

func TestListStoreProjectsJSON(t *testing.T) {
	dbPath := seedListTestStore(t)

	var buf bytes.Buffer
	if err := listStoreProjects(&buf, dbPath, true); err != nil {
		t.Fatalf("listStoreProjects: %v", err)
	}
	var names []string
	if err := json.Unmarshal(buf.Bytes(), &names); err != nil {
		t.Fatalf("decode json output %q: %v", buf.String(), err)
	}
	if len(names) != 2 || names[0] != "broken" || names[1] != "svc" {
		t.Fatalf("json output = %q, want [broken svc]", names)
	}
}

// A read-only inspection command must never materialize the fleet source of
// truth: configstore.Open would create an empty store here, which the daemon
// would then read as a zero-project fleet (#1123).
func TestListStoreProjectsDoesNotCreateStore(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "missing.db")

	err := listStoreProjects(&bytes.Buffer{}, dbPath, false)
	if err == nil {
		t.Fatal("listStoreProjects succeeded against a non-existent store")
	}
	if !strings.Contains(err.Error(), dbPath) {
		t.Fatalf("error %q does not name the missing store path", err)
	}
	entries, readErr := os.ReadDir(dir)
	if readErr != nil {
		t.Fatalf("read dir: %v", readErr)
	}
	if len(entries) != 0 {
		t.Fatalf("listStoreProjects created %d file(s) in %s; it must create nothing", len(entries), dir)
	}
}
