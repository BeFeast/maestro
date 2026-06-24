package configstore

import (
	"context"
	"strings"
	"testing"
)

func TestProjectNameFor(t *testing.T) {
	// The repo field wins and is sanitised the way ImportDir names projects.
	name, err := ProjectNameFor("/tmp/whatever.yaml", []byte("repo: BeFeast/maestro\n"))
	if err != nil {
		t.Fatalf("ProjectNameFor(repo): %v", err)
	}
	if name != "befeast-maestro" {
		t.Fatalf("name = %q, want befeast-maestro", name)
	}

	// No repo: fall back to the file basename.
	name, err = ProjectNameFor("/tmp/My_Project.yml", []byte("max_parallel: 2\n"))
	if err != nil {
		t.Fatalf("ProjectNameFor(basename): %v", err)
	}
	if name != "my-project" {
		t.Fatalf("name = %q, want my-project", name)
	}

	if _, err := ProjectNameFor("x.yaml", []byte("::: not yaml :::\n")); err == nil {
		t.Fatal("ProjectNameFor on invalid YAML: want error, got nil")
	}
}

func TestExportProjectRoundTripThroughEdit(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)

	const yaml = `repo: owner/svc
max_parallel: 2
model:
  default: codex
  backends:
    codex:
      cmd: codex
      prompt_mode: stdin
`
	if err := store.UpsertProject(ctx, "svc", yaml); err != nil {
		t.Fatalf("UpsertProject: %v", err)
	}

	// ExportProject re-attaches the shared backend so an editor sees a complete,
	// re-importable document.
	out, err := store.ExportProject(ctx, "svc")
	if err != nil {
		t.Fatalf("ExportProject: %v", err)
	}
	if !strings.Contains(string(out), "backends:") || !strings.Contains(string(out), "codex") {
		t.Fatalf("exported YAML missing re-attached backend:\n%s", out)
	}

	// Simulate an edit (bump max_parallel) and re-import — the change persists.
	edited := strings.Replace(string(out), "max_parallel: 2", "max_parallel: 9", 1)
	if err := store.UpsertProject(ctx, "svc", edited); err != nil {
		t.Fatalf("re-import edited project: %v", err)
	}
	cfg, err := store.Load(ctx, "svc")
	if err != nil {
		t.Fatalf("Load after edit: %v", err)
	}
	if cfg.MaxParallel != 9 {
		t.Fatalf("MaxParallel = %d, want 9 after edit round-trip", cfg.MaxParallel)
	}

	if _, err := store.ExportProject(ctx, "missing"); err == nil {
		t.Fatal("ExportProject of a missing project: want error, got nil")
	}
}
