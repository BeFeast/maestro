package main

import (
	"context"
	"flag"
	"os"
	"path/filepath"
	"testing"

	"github.com/befeast/maestro/internal/configstore"
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

func TestConfigStoreDBFlagDefaultsToMaestroDB(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	fs := flag.NewFlagSet("test", flag.ContinueOnError)

	got := configStoreDBFlag(fs)

	want := filepath.Join(home, ".maestro", "maestro.db")
	if *got != want {
		t.Fatalf("config store db default = %q, want %q", *got, want)
	}
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
