package configstore

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const settingsTestYAML = `repo: BeFeast/maestro
local_path: ~/src/maestro
max_parallel: 2
model:
  default: codex
  backends:
    codex:
      cmd: codex
      prompt_mode: stdin
supervisor:
  enabled: true
  backend: claude
`

func seedSettingsProject(t *testing.T, store *Store, name string) {
	t.Helper()
	if err := store.UpsertProject(context.Background(), name, settingsTestYAML); err != nil {
		t.Fatalf("seed project %q: %v", name, err)
	}
}

// A fleet default applies to a project that does not set the key itself.
func TestFleetSettingOverlaysBuiltin(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	seedSettingsProject(t, store, "maestro")

	// worker_max_tokens is not set in the project YAML → built-in 0.
	cfg, err := store.Load(ctx, "maestro")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.WorkerMaxTokens != 0 {
		t.Fatalf("WorkerMaxTokens = %d, want 0 (built-in) before fleet default", cfg.WorkerMaxTokens)
	}
	if got := cfg.SettingSources["worker_max_tokens"]; got != SourceBuiltin {
		t.Fatalf("source = %q, want %q", got, SourceBuiltin)
	}

	if err := store.SetFleetSetting(ctx, "worker_max_tokens", "400000", "op"); err != nil {
		t.Fatalf("SetFleetSetting: %v", err)
	}
	cfg, err = store.Load(ctx, "maestro")
	if err != nil {
		t.Fatalf("Load after fleet set: %v", err)
	}
	if cfg.WorkerMaxTokens != 400000 {
		t.Fatalf("WorkerMaxTokens = %d, want 400000 from fleet default", cfg.WorkerMaxTokens)
	}
	if got := cfg.SettingSources["worker_max_tokens"]; got != SourceFleet {
		t.Fatalf("source = %q, want %q", got, SourceFleet)
	}
}

// The fleet-wide supervisor.enabled=false flip lands on a project whose YAML
// sets supervisor.enabled=true — WAIT, no: the project explicitly sets it, so it
// must WIN. This is the core precedence guarantee (#839 AC2).
func TestProjectOverrideWinsOverFleetDefault(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	seedSettingsProject(t, store, "maestro")

	// Fleet says supervisor OFF...
	if err := store.SetFleetSetting(ctx, "supervisor.enabled", "false", "op"); err != nil {
		t.Fatalf("SetFleetSetting: %v", err)
	}
	// ...but the project row explicitly sets enabled: true, so it wins.
	cfg, err := store.Load(ctx, "maestro")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.Supervisor.Enabled {
		t.Fatal("Supervisor.Enabled = false, want true (project override must beat fleet default)")
	}
	if got := cfg.SettingSources["supervisor.enabled"]; got != SourceProject {
		t.Fatalf("source = %q, want %q", got, SourceProject)
	}
}

// Fleet supervisor.enabled=false takes effect on a project WITHOUT its own
// override (#839 AC1).
func TestFleetDisableSupervisorForUnoverriddenProject(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	// A project that does NOT set supervisor.enabled.
	yaml := "repo: owner/idle\nmodel:\n  default: codex\n  backends:\n    codex:\n      cmd: codex\n      prompt_mode: stdin\n"
	if err := store.UpsertProject(ctx, "idle", yaml); err != nil {
		t.Fatalf("UpsertProject: %v", err)
	}

	if err := store.SetFleetSetting(ctx, "supervisor.enabled", "false", "op"); err != nil {
		t.Fatalf("SetFleetSetting: %v", err)
	}
	cfg, err := store.Load(ctx, "idle")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Supervisor.Enabled {
		t.Fatal("Supervisor.Enabled = true, want false from fleet default")
	}
	if got := cfg.SettingSources["supervisor.enabled"]; got != SourceFleet {
		t.Fatalf("source = %q, want %q", got, SourceFleet)
	}
}

// A per-project override is written into the row YAML and beats the fleet
// default going forward.
func TestSetProjectSettingWritesOverride(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	yaml := "repo: owner/idle\nmodel:\n  default: codex\n  backends:\n    codex:\n      cmd: codex\n      prompt_mode: stdin\n"
	if err := store.UpsertProject(ctx, "idle", yaml); err != nil {
		t.Fatalf("UpsertProject: %v", err)
	}
	if err := store.SetFleetSetting(ctx, "poll_interval_seconds", "600", "op"); err != nil {
		t.Fatalf("SetFleetSetting: %v", err)
	}
	if err := store.SetProjectSetting(ctx, "idle", "poll_interval_seconds", "30", "op"); err != nil {
		t.Fatalf("SetProjectSetting: %v", err)
	}
	cfg, err := store.Load(ctx, "idle")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.PollIntervalSeconds != 30 {
		t.Fatalf("PollIntervalSeconds = %d, want 30 (project override)", cfg.PollIntervalSeconds)
	}
	if got := cfg.SettingSources["poll_interval_seconds"]; got != SourceProject {
		t.Fatalf("source = %q, want %q", got, SourceProject)
	}
}

// Every change is journaled with old→new, actor, and an RFC3339 timestamp
// (#839 AC4).
func TestSettingsAuditTrail(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	seedSettingsProject(t, store, "maestro")

	if err := store.SetFleetSetting(ctx, "supervisor.backend", "claude", "alice"); err != nil {
		t.Fatalf("set 1: %v", err)
	}
	if err := store.SetFleetSetting(ctx, "supervisor.backend", "gemini", "bob"); err != nil {
		t.Fatalf("set 2: %v", err)
	}
	records, err := store.SettingsAudit(ctx, 0)
	if err != nil {
		t.Fatalf("SettingsAudit: %v", err)
	}
	if len(records) != 2 {
		t.Fatalf("audit records = %d, want 2", len(records))
	}
	// Most recent first.
	latest := records[0]
	if latest.Key != "supervisor.backend" || latest.Scope != FleetScope {
		t.Fatalf("latest key/scope = %q/%q", latest.Key, latest.Scope)
	}
	if latest.OldValue != "claude" || latest.NewValue != "gemini" {
		t.Fatalf("latest old→new = %q→%q, want claude→gemini", latest.OldValue, latest.NewValue)
	}
	if latest.Actor != "bob" {
		t.Fatalf("actor = %q, want bob", latest.Actor)
	}
	if _, err := time.Parse(time.RFC3339, latest.At); err != nil {
		t.Fatalf("timestamp %q not RFC3339: %v", latest.At, err)
	}
	// The first change recorded an empty old value (backend was unset).
	if records[1].OldValue != "" || records[1].NewValue != "claude" {
		t.Fatalf("first old→new = %q→%q, want (empty)→claude", records[1].OldValue, records[1].NewValue)
	}
}

// A fleet setting change advances every project's fingerprint so store-backed
// watchers hot-reload without a restart (#839 AC1).
func TestFleetSettingAdvancesFingerprint(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	seedSettingsProject(t, store, "maestro")

	before, err := store.ProjectsFingerprint(ctx)
	if err != nil {
		t.Fatalf("fingerprint before: %v", err)
	}
	if err := store.SetFleetSetting(ctx, "supervisor.enabled", "false", "op"); err != nil {
		t.Fatalf("SetFleetSetting: %v", err)
	}
	after, err := store.ProjectsFingerprint(ctx)
	if err != nil {
		t.Fatalf("fingerprint after: %v", err)
	}
	if !after["maestro"].After(before["maestro"]) {
		t.Fatalf("fingerprint did not advance: before=%v after=%v", before["maestro"], after["maestro"])
	}
}

// An invalid value for a typed key is rejected before it can land.
func TestSetFleetSettingRejectsBadValue(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	if err := store.SetFleetSetting(ctx, "worker_max_tokens", "banana", "op"); err == nil {
		t.Fatal("SetFleetSetting with non-int value: want error, got nil")
	}
	if err := store.SetFleetSetting(ctx, "supervisor.enabled", "maybe", "op"); err == nil {
		t.Fatal("SetFleetSetting with non-bool value: want error, got nil")
	}
	if err := store.SetFleetSetting(ctx, "not.a.key", "1", "op"); err == nil {
		t.Fatal("SetFleetSetting with unknown key: want error, got nil")
	}
}

// DeleteFleetSetting reverts affected projects to built-in and journals it.
func TestDeleteFleetSetting(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	yaml := "repo: owner/idle\nmodel:\n  default: codex\n  backends:\n    codex:\n      cmd: codex\n      prompt_mode: stdin\n"
	if err := store.UpsertProject(ctx, "idle", yaml); err != nil {
		t.Fatalf("UpsertProject: %v", err)
	}
	if err := store.SetFleetSetting(ctx, "worker_max_tokens", "5000", "op"); err != nil {
		t.Fatalf("SetFleetSetting: %v", err)
	}
	if err := store.DeleteFleetSetting(ctx, "worker_max_tokens", "op"); err != nil {
		t.Fatalf("DeleteFleetSetting: %v", err)
	}
	cfg, err := store.Load(ctx, "idle")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.WorkerMaxTokens != 0 {
		t.Fatalf("WorkerMaxTokens = %d, want 0 after delete", cfg.WorkerMaxTokens)
	}
	if got := cfg.SettingSources["worker_max_tokens"]; got != SourceBuiltin {
		t.Fatalf("source = %q, want %q after delete", got, SourceBuiltin)
	}
}

// The fleet-settings layer round-trips through export → import (#839 AC5).
func TestSettingsExportImportRoundTrip(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	// Name the row after its repo derivation so export→import keeps a stable
	// name (ImportDir re-derives the project name from the `repo` field).
	const projName = "befeast-maestro"
	seedSettingsProject(t, store, projName)
	if err := store.SetFleetSetting(ctx, "supervisor.enabled", "false", "op"); err != nil {
		t.Fatalf("SetFleetSetting: %v", err)
	}
	if err := store.SetFleetSetting(ctx, "worker_max_tokens", "250000", "op"); err != nil {
		t.Fatalf("SetFleetSetting: %v", err)
	}

	dir := t.TempDir()
	if err := store.ExportDir(ctx, dir); err != nil {
		t.Fatalf("ExportDir: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, fleetSettingsFileName)); err != nil {
		t.Fatalf("expected %s in export dir: %v", fleetSettingsFileName, err)
	}
	// The per-project row must NOT have the fleet default baked in (the layer is
	// exported separately so the distinction survives).
	projData, err := os.ReadFile(filepath.Join(dir, projName+".yaml"))
	if err != nil {
		t.Fatalf("read exported project: %v", err)
	}
	if strings.Contains(string(projData), "worker_max_tokens") {
		t.Fatal("exported project row should not carry the fleet-level worker_max_tokens default")
	}

	// Re-import into a fresh store and confirm the fleet layer is restored.
	fresh, err := Open(filepath.Join(t.TempDir(), "config.db"))
	if err != nil {
		t.Fatalf("Open fresh: %v", err)
	}
	defer fresh.Close()
	if err := fresh.ImportDir(ctx, dir); err != nil {
		t.Fatalf("ImportDir: %v", err)
	}
	fleet, err := fresh.FleetSettings(ctx)
	if err != nil {
		t.Fatalf("FleetSettings: %v", err)
	}
	if fleet["supervisor.enabled"] != "false" || fleet["worker_max_tokens"] != "250000" {
		t.Fatalf("round-tripped fleet settings = %v", fleet)
	}
	cfg, err := fresh.Load(ctx, projName)
	if err != nil {
		t.Fatalf("Load from fresh: %v", err)
	}
	if cfg.WorkerMaxTokens != 250000 {
		t.Fatalf("WorkerMaxTokens = %d, want 250000 after round-trip", cfg.WorkerMaxTokens)
	}
}

// ResolveProjectSettings reports value+source for every knob.
func TestResolveProjectSettings(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	seedSettingsProject(t, store, "maestro")
	if err := store.SetFleetSetting(ctx, "worker_max_tokens", "123", "op"); err != nil {
		t.Fatalf("SetFleetSetting: %v", err)
	}
	resolved, err := store.ResolveProjectSettings(ctx, "maestro")
	if err != nil {
		t.Fatalf("ResolveProjectSettings: %v", err)
	}
	got := map[string]ResolvedSetting{}
	for _, r := range resolved {
		got[r.Key] = r
	}
	if r := got["supervisor.enabled"]; r.Value != "true" || r.Source != SourceProject {
		t.Fatalf("supervisor.enabled resolved = %+v, want value=true source=project", r)
	}
	if r := got["worker_max_tokens"]; r.Value != "123" || r.Source != SourceFleet {
		t.Fatalf("worker_max_tokens resolved = %+v, want value=123 source=fleet", r)
	}
	if r := got["poll_interval_seconds"]; r.Value != "0" || r.Source != SourceBuiltin {
		t.Fatalf("poll_interval_seconds resolved = %+v, want value=0 source=builtin", r)
	}
}
