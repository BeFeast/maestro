package configstore

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/befeast/maestro/internal/config"
)

const writeTestYAML = `
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

func TestUpsertProjectLoadAllRoundTrip(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)

	if err := store.UpsertProject(ctx, "befeast-maestro", writeTestYAML); err != nil {
		t.Fatalf("UpsertProject: %v", err)
	}

	// Backends were detached into the shared table, not duplicated in the row.
	var projectYAML string
	if err := store.db.QueryRowContext(ctx, `SELECT config_yaml FROM project WHERE name = ?`, "befeast-maestro").Scan(&projectYAML); err != nil {
		t.Fatalf("query project: %v", err)
	}
	if strings.Contains(projectYAML, "\n  backends:") {
		t.Fatalf("project row should not duplicate backend definitions:\n%s", projectYAML)
	}

	want, err := config.Parse([]byte(writeTestYAML))
	if err != nil {
		t.Fatalf("parse yaml: %v", err)
	}

	got, err := store.Load(ctx, "befeast-maestro")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want.SourcePath = ""
	got.SourcePath = ""
	got.SettingsSources = nil   // provenance map is set by Load, absent from Parse
	got.FleetOnlySettings = nil // daemon-only settings are set by Load, absent from Parse
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("round-trip config differs\n got=%#v\nwant=%#v", got.Model, want.Model)
	}

	all, err := store.LoadAll(ctx)
	if err != nil {
		t.Fatalf("LoadAll: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("LoadAll len = %d, want 1", len(all))
	}
}

// A write-API row has no originating file (source_path = ”). Load must stamp
// the store row reference on SourcePath — not leave it empty — otherwise
// ResolvePath falls back to a literal "maestro.yaml" and the fleet snapshot
// reports a vestigial config_path for a file that does not exist post-#761
// (#801).
func TestLoadStampsStoreRowSourcePath(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)

	if err := store.UpsertProject(ctx, "befeast-ok-player", writeTestYAML); err != nil {
		t.Fatalf("UpsertProject: %v", err)
	}
	cfg, err := store.Load(ctx, "befeast-ok-player")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	const want = "store:befeast-ok-player"
	if cfg.SourcePath != want {
		t.Fatalf("SourcePath = %q, want %q", cfg.SourcePath, want)
	}
	if got := cfg.ResolvePath(); got != want {
		t.Fatalf("ResolvePath() = %q, want %q", got, want)
	}
}

func TestUpsertProjectOverwrites(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)

	first := `
repo: owner/project
model:
  default: codex
  backends:
    codex:
      cmd: codex
      prompt_mode: stdin
`
	if err := store.UpsertProject(ctx, "p", first); err != nil {
		t.Fatalf("UpsertProject first: %v", err)
	}

	second := `
repo: owner/project
issue_labels: [maestro-ready]
model:
  default: codex
  backends:
    codex:
      cmd: codex
      prompt_mode: stdin
`
	if err := store.UpsertProject(ctx, "p", second); err != nil {
		t.Fatalf("UpsertProject second: %v", err)
	}

	got, err := store.Load(ctx, "p")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(got.IssueLabels) != 1 || got.IssueLabels[0] != "maestro-ready" {
		t.Fatalf("overwrite did not take effect: issue_labels = %v", got.IssueLabels)
	}

	var count int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM project`).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Fatalf("project count = %d, want 1 (upsert, not insert)", count)
	}
}

func TestUpsertProjectRejectsInvalidYAML(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	if err := store.UpsertProject(ctx, "bad", "not: [valid"); err == nil {
		t.Fatal("UpsertProject accepted invalid YAML, want error")
	}
	var count int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM project`).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 0 {
		t.Fatalf("rejected project should not be written: count = %d", count)
	}
}

func TestDeleteProject(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)

	base := `
repo: owner/%s
model:
  default: codex
  backends:
    codex:
      cmd: codex
      prompt_mode: stdin
`
	if err := store.UpsertProject(ctx, "keep", strings.Replace(base, "%s", "keep", 1)); err != nil {
		t.Fatalf("UpsertProject keep: %v", err)
	}
	if err := store.UpsertProject(ctx, "drop", strings.Replace(base, "%s", "drop", 1)); err != nil {
		t.Fatalf("UpsertProject drop: %v", err)
	}

	if err := store.DeleteProject(ctx, "drop"); err != nil {
		t.Fatalf("DeleteProject: %v", err)
	}

	all, err := store.LoadAll(ctx)
	if err != nil {
		t.Fatalf("LoadAll: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("LoadAll len = %d, want 1", len(all))
	}
	if _, err := store.Load(ctx, "drop"); err == nil {
		t.Fatal("Load of deleted project should fail")
	}

	// Deleting a missing project is a no-op, not an error.
	if err := store.DeleteProject(ctx, "drop"); err != nil {
		t.Fatalf("DeleteProject (missing) should be a no-op: %v", err)
	}
}

func TestProjectsFingerprint(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)

	base := `
repo: owner/%s
model:
  default: codex
  backends:
    codex:
      cmd: codex
      prompt_mode: stdin
`
	if err := store.UpsertProject(ctx, "a", strings.Replace(base, "%s", "a", 1)); err != nil {
		t.Fatalf("UpsertProject a: %v", err)
	}
	if err := store.UpsertProject(ctx, "b", strings.Replace(base, "%s", "b", 1)); err != nil {
		t.Fatalf("UpsertProject b: %v", err)
	}

	fp, err := store.ProjectsFingerprint(ctx)
	if err != nil {
		t.Fatalf("ProjectsFingerprint: %v", err)
	}
	if len(fp) != 2 {
		t.Fatalf("fingerprint len = %d, want 2", len(fp))
	}
	for _, name := range []string{"a", "b"} {
		ts, ok := fp[name]
		if !ok {
			t.Fatalf("fingerprint missing project %q", name)
		}
		if ts.IsZero() {
			t.Fatalf("fingerprint for %q has zero timestamp", name)
		}
	}

	if err := store.DeleteProject(ctx, "a"); err != nil {
		t.Fatalf("DeleteProject: %v", err)
	}
	fp, err = store.ProjectsFingerprint(ctx)
	if err != nil {
		t.Fatalf("ProjectsFingerprint after delete: %v", err)
	}
	if len(fp) != 1 {
		t.Fatalf("fingerprint len after delete = %d, want 1", len(fp))
	}
	if _, ok := fp["a"]; ok {
		t.Fatal("deleted project still present in fingerprint")
	}
}

func TestUpsertBackend(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)

	if err := store.UpsertBackend(ctx, "codex", "cmd: codex\nprompt_mode: stdin\n"); err != nil {
		t.Fatalf("UpsertBackend: %v", err)
	}
	var def string
	if err := store.db.QueryRowContext(ctx, `SELECT definition_yaml FROM backends WHERE name = ?`, "codex").Scan(&def); err != nil {
		t.Fatalf("query backend: %v", err)
	}
	if !strings.Contains(def, "cmd: codex") {
		t.Fatalf("backend def = %q", def)
	}

	// Direct UpsertBackend overwrites rather than rejecting divergence.
	if err := store.UpsertBackend(ctx, "codex", "cmd: other-codex\nprompt_mode: stdin\n"); err != nil {
		t.Fatalf("UpsertBackend overwrite: %v", err)
	}
	if err := store.db.QueryRowContext(ctx, `SELECT definition_yaml FROM backends WHERE name = ?`, "codex").Scan(&def); err != nil {
		t.Fatalf("query backend: %v", err)
	}
	if !strings.Contains(def, "other-codex") {
		t.Fatalf("overwrite did not take effect: %q", def)
	}
}

func TestSetGlobal(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)

	if err := store.SetGlobal(ctx, "defaults", "model:\n  default: codex\n"); err != nil {
		t.Fatalf("SetGlobal: %v", err)
	}
	var val string
	if err := store.db.QueryRowContext(ctx, `SELECT value_yaml FROM global WHERE key = ?`, "defaults").Scan(&val); err != nil {
		t.Fatalf("query global: %v", err)
	}
	if !strings.Contains(val, "default: codex") {
		t.Fatalf("global value = %q", val)
	}

	if err := store.SetGlobal(ctx, "defaults", "model:\n  default: claude\n"); err != nil {
		t.Fatalf("SetGlobal overwrite: %v", err)
	}
	if err := store.db.QueryRowContext(ctx, `SELECT value_yaml FROM global WHERE key = ?`, "defaults").Scan(&val); err != nil {
		t.Fatalf("query global: %v", err)
	}
	if !strings.Contains(val, "default: claude") {
		t.Fatalf("overwrite did not take effect: %q", val)
	}
}
