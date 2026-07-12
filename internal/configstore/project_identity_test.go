package configstore

import (
	"context"
	"strings"
	"testing"

	"github.com/befeast/maestro/internal/config"
)

const identityProjectYAML = `repo: BeFeast/maestro
local_path: ~/src/maestro
project_id: 3f2504e0-4f89-41d3-9a0c-0305e82c3301
management_home:
  kind: obsidian
  path: /home/god/Obsidian/Dev
  vault: Obsidian Vault
  vault_path: Dev/Areas/maestro
`

// Config-store upsert -> reload -> export is lossless for project_id and the
// management_home block (#869).
func TestUpsertReloadExportLosslessForIdentity(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)

	if err := store.UpsertProject(ctx, "befeast-maestro", identityProjectYAML); err != nil {
		t.Fatalf("UpsertProject: %v", err)
	}

	// Reload through Load: both fields survive parse.
	cfg, err := store.Load(ctx, "befeast-maestro")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.ProjectID != "3f2504e0-4f89-41d3-9a0c-0305e82c3301" {
		t.Fatalf("reloaded project_id = %q", cfg.ProjectID)
	}
	if cfg.ManagementHome.Kind != "obsidian" || cfg.ManagementHome.VaultPath != "Dev/Areas/maestro" ||
		cfg.ManagementHome.Vault != "Obsidian Vault" || cfg.ManagementHome.Path != "/home/god/Obsidian/Dev" {
		t.Fatalf("reloaded management_home lost fields: %+v", cfg.ManagementHome)
	}

	// Export round-trips the fields back into portable YAML.
	exported, err := store.ExportProject(ctx, "befeast-maestro")
	if err != nil {
		t.Fatalf("ExportProject: %v", err)
	}
	reparsed, err := config.Parse(exported)
	if err != nil {
		t.Fatalf("re-parse export: %v", err)
	}
	if reparsed.ProjectID != cfg.ProjectID || reparsed.ManagementHome != cfg.ManagementHome {
		t.Fatalf("export not lossless: id=%q mh=%+v", reparsed.ProjectID, reparsed.ManagementHome)
	}
	if !strings.Contains(string(exported), "project_id: 3f2504e0-4f89-41d3-9a0c-0305e82c3301") {
		t.Fatalf("export missing project_id line:\n%s", exported)
	}
	if !strings.Contains(string(exported), "vault_path: Dev/Areas/maestro") {
		t.Fatalf("export missing management_home vault_path:\n%s", exported)
	}
}

// Adding an id to a legacy id-less row is allowed; changing a set id to a
// different non-empty one is rejected as an identity mismatch (#869).
func TestUpsertProjectImmutableProjectID(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)

	legacy := "repo: BeFeast/maestro\nlocal_path: ~/src/maestro\n"
	if err := store.UpsertProject(ctx, "befeast-maestro", legacy); err != nil {
		t.Fatalf("seed legacy row: %v", err)
	}

	// Adding an id to the legacy id-less row is allowed.
	if err := store.UpsertProject(ctx, "befeast-maestro", identityProjectYAML); err != nil {
		t.Fatalf("adding id to legacy row should be allowed: %v", err)
	}

	// Re-upserting the SAME id is a no-op-identity change, allowed.
	if err := store.UpsertProject(ctx, "befeast-maestro", identityProjectYAML); err != nil {
		t.Fatalf("re-upsert with same id should be allowed: %v", err)
	}

	// Changing to a DIFFERENT non-empty id is rejected.
	changed := strings.Replace(identityProjectYAML,
		"3f2504e0-4f89-41d3-9a0c-0305e82c3301",
		"11111111-2222-3333-4444-555555555555", 1)
	err := store.UpsertProject(ctx, "befeast-maestro", changed)
	if err == nil {
		t.Fatalf("changing project_id should be rejected")
	}
	if !strings.Contains(err.Error(), "immutable") {
		t.Fatalf("mismatch error should mention immutability, got: %v", err)
	}

	// And the stored row must be unchanged (write-nothing on the rejected edit).
	cfg, err := store.Load(ctx, "befeast-maestro")
	if err != nil {
		t.Fatalf("Load after rejected edit: %v", err)
	}
	if cfg.ProjectID != "3f2504e0-4f89-41d3-9a0c-0305e82c3301" {
		t.Fatalf("stored id changed after a rejected edit: %q", cfg.ProjectID)
	}
}

// A strict-decode failure (unknown/misspelled key) on the write path must be
// rejected and must not persist a row (#869).
func TestUpsertProjectStrictDecodeWritesNothing(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)

	bad := identityProjectYAML + "  vault_pathh: typo\n"
	err := store.UpsertProject(ctx, "befeast-maestro", bad)
	if err == nil {
		t.Fatalf("misspelled key should be rejected on write")
	}
	if !strings.Contains(err.Error(), "vault_pathh") {
		t.Fatalf("error should name the misspelled key, got: %v", err)
	}
	// No row was written.
	if _, err := store.Load(ctx, "befeast-maestro"); err == nil {
		t.Fatalf("no project row should exist after a rejected strict-decode write")
	}
}
