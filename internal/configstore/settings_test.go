package configstore

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/befeast/maestro/internal/config"
)

const settingsProjectYAML = `
repo: owner/%s
model:
  default: codex
  backends:
    codex:
      cmd: codex
      prompt_mode: stdin
supervisor:
  enabled: true
`

func seedProject(t *testing.T, store *Store, name string) {
	t.Helper()
	if err := store.UpsertProject(context.Background(), name, strings.Replace(settingsProjectYAML, "%s", name, 1)); err != nil {
		t.Fatalf("UpsertProject %s: %v", name, err)
	}
}

// A fleet default applies to a project that does not override the key; the
// project's own YAML value wins where present.
func TestFleetDefaultLayering(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	seedProject(t, store, "idle")   // supervisor.enabled: true (own value)
	seedProject(t, store, "worked") // supervisor.enabled: true (own value)

	// worker_max_tokens is unset in both projects -> fleet default should land.
	if err := store.SetFleetSetting(ctx, "worker_max_tokens", "250000", "tester"); err != nil {
		t.Fatalf("SetFleetSetting: %v", err)
	}
	cfg, err := store.Load(ctx, "idle")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.WorkerMaxTokens != 250000 {
		t.Fatalf("worker_max_tokens = %d, want 250000 (fleet default)", cfg.WorkerMaxTokens)
	}
	if got := cfg.SettingsSources["worker_max_tokens"]; got != config.SettingSourceFleet {
		t.Fatalf("source = %q, want fleet", got)
	}
	// supervisor.enabled is set in the project YAML -> project wins over any fleet default.
	if err := store.SetFleetSetting(ctx, "supervisor.enabled", "false", "tester"); err != nil {
		t.Fatalf("SetFleetSetting enabled: %v", err)
	}
	cfg, err = store.Load(ctx, "idle")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.Supervisor.Enabled {
		t.Fatal("project override should keep supervisor.enabled true despite fleet default false")
	}
	if got := cfg.SettingsSources["supervisor.enabled"]; got != config.SettingSourceProject {
		t.Fatalf("source = %q, want project", got)
	}
	// A key with neither project value nor fleet default resolves to builtin.
	if got := cfg.SettingsSources["poll_interval_seconds"]; got != config.SettingSourceBuiltin {
		t.Fatalf("source = %q, want builtin", got)
	}
}

// A per-project override wins over the fleet default and is scoped to that project.
func TestProjectOverrideBeatsFleet(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	seedProject(t, store, "a")
	seedProject(t, store, "b")

	if err := store.SetFleetSetting(ctx, "supervisor.enabled", "false", "tester"); err != nil {
		t.Fatalf("SetFleetSetting: %v", err)
	}
	if err := store.SetProjectSetting(ctx, "a", "supervisor.enabled", "false", "tester"); err != nil {
		t.Fatalf("SetProjectSetting: %v", err)
	}
	// Project a explicitly set false -> project source, value false.
	cfgA, err := store.Load(ctx, "a")
	if err != nil {
		t.Fatalf("Load a: %v", err)
	}
	if cfgA.Supervisor.Enabled {
		t.Fatal("project a supervisor.enabled should be false")
	}
	if got := cfgA.SettingsSources["supervisor.enabled"]; got != config.SettingSourceProject {
		t.Fatalf("a source = %q, want project", got)
	}
	// Project b never overrode it -> keeps its own YAML value (true, project source),
	// since the seed YAML sets supervisor.enabled explicitly.
	cfgB, err := store.Load(ctx, "b")
	if err != nil {
		t.Fatalf("Load b: %v", err)
	}
	if !cfgB.Supervisor.Enabled {
		t.Fatal("project b keeps its own supervisor.enabled true")
	}
}

func TestProviderLanesFleetDefaultAndProjectOverride(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	projectYAML := `
repo: owner/routes
model:
  default: claude
  backends:
    claude: {cmd: claude, provider: anthropic}
    sol: {cmd: codex, provider: openai, model: gpt-5.6-sol, effort: high}
    gpt55: {cmd: codex, provider: openai, model: gpt-5.5, effort: high}
`
	if err := store.UpsertProject(ctx, "routes", projectYAML); err != nil {
		t.Fatalf("UpsertProject: %v", err)
	}
	fleetValue := `[
  {"provider":"anthropic","default":"claude"},
  {"provider":"openai","default":"sol","fallback_backends":["gpt55"]}
]`
	if err := store.SetFleetSetting(ctx, "model.provider_lanes", fleetValue, "fleet-admin"); err != nil {
		t.Fatalf("SetFleetSetting: %v", err)
	}
	cfg, err := store.Load(ctx, "routes")
	if err != nil {
		t.Fatalf("Load fleet route: %v", err)
	}
	if got := cfg.Model.ResolvedRoute().Backends; !reflect.DeepEqual(got, []string{"claude", "sol", "gpt55"}) {
		t.Fatalf("fleet route = %v", got)
	}
	if got := cfg.SettingsSources["model.provider_lanes"]; got != config.SettingSourceFleet {
		t.Fatalf("fleet route source = %q", got)
	}

	projectValue := `[{"provider":"openai","default":"sol","fallback_backends":["gpt55"]}]`
	if err := store.SetProjectSetting(ctx, "routes", "model.provider_lanes", projectValue, "project-owner"); err != nil {
		t.Fatalf("SetProjectSetting: %v", err)
	}
	cfg, err = store.Load(ctx, "routes")
	if err != nil {
		t.Fatalf("Load project route: %v", err)
	}
	if got := cfg.Model.ResolvedRoute().Backends; !reflect.DeepEqual(got, []string{"sol", "gpt55"}) {
		t.Fatalf("project route = %v", got)
	}
	if got := cfg.SettingsSources["model.provider_lanes"]; got != config.SettingSourceProject {
		t.Fatalf("project route source = %q", got)
	}
	audit, err := store.SettingsAudit(ctx, 2)
	if err != nil {
		t.Fatalf("SettingsAudit: %v", err)
	}
	if len(audit) != 2 || audit[0].Scope != scopeProject("routes") || audit[1].Scope != scopeFleet {
		t.Fatalf("route audit = %+v", audit)
	}
}

func TestProviderLanesFleetDefaultDoesNotInvalidateExplicitProjectChain(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	if err := store.UpsertProject(ctx, "legacy-route", `
repo: owner/legacy-route
model:
  default: claude
  fallback_backends: [codex]
  backends:
    claude: {cmd: claude, provider: anthropic}
    codex: {cmd: codex, provider: openai}
`); err != nil {
		t.Fatalf("UpsertProject: %v", err)
	}
	if err := store.SetFleetSetting(ctx, "model.provider_lanes", `[
  {"provider":"anthropic","default":"fleet-claude"},
  {"provider":"openai","default":"fleet-sol"}
]`, "fleet-admin"); err != nil {
		t.Fatalf("SetFleetSetting: %v", err)
	}

	cfg, err := store.Load(ctx, "legacy-route")
	if err != nil {
		t.Fatalf("Load legacy project with inactive fleet lanes: %v", err)
	}
	route := cfg.Model.ResolvedRoute()
	if route.SelectionReason != config.ModelRouteExplicitBackendChain || !reflect.DeepEqual(route.Backends, []string{"claude", "codex"}) {
		t.Fatalf("route = %+v, want explicit project chain", route)
	}
	if got := cfg.SettingsSources["model.provider_lanes"]; got != config.SettingSourceFleet {
		t.Fatalf("injected lane source = %q, want fleet", got)
	}
}

// The greptile P1 repro: ProjectsFingerprint must advance on SetFleetSetting AND
// on DeleteFleetSetting (revert-to-builtin), even when the deleted key was the
// newest settings row.
func TestFingerprintAdvancesOnFleetSetAndDelete(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	seedProject(t, store, "p")

	fp0, err := store.ProjectsFingerprint(ctx)
	if err != nil {
		t.Fatalf("fingerprint 0: %v", err)
	}

	if err := store.SetFleetSetting(ctx, "worker_max_tokens", "100000", "tester"); err != nil {
		t.Fatalf("SetFleetSetting: %v", err)
	}
	fp1, err := store.ProjectsFingerprint(ctx)
	if err != nil {
		t.Fatalf("fingerprint 1: %v", err)
	}
	if !fp1["p"].After(fp0["p"]) {
		t.Fatalf("fingerprint did not advance on SetFleetSetting: %v -> %v", fp0["p"], fp1["p"])
	}

	// Deleting the only/newest fleet default must still advance the fingerprint
	// so --watch-store reloads the project back to the built-in value.
	if err := store.DeleteFleetSetting(ctx, "worker_max_tokens", "tester"); err != nil {
		t.Fatalf("DeleteFleetSetting: %v", err)
	}
	fp2, err := store.ProjectsFingerprint(ctx)
	if err != nil {
		t.Fatalf("fingerprint 2: %v", err)
	}
	if !fp2["p"].After(fp1["p"]) {
		t.Fatalf("fingerprint did not advance on DeleteFleetSetting: %v -> %v", fp1["p"], fp2["p"])
	}

	// And the project actually reverts to the built-in (unset -> 0).
	cfg, err := store.Load(ctx, "p")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.WorkerMaxTokens != 0 {
		t.Fatalf("worker_max_tokens = %d after delete, want 0 (builtin)", cfg.WorkerMaxTokens)
	}
	if got := cfg.SettingsSources["worker_max_tokens"]; got != config.SettingSourceBuiltin {
		t.Fatalf("source = %q after delete, want builtin", got)
	}
}

// Deleting an unset fleet default is a no-op and must NOT advance the fingerprint
// or write an audit row.
func TestDeleteUnsetFleetSettingIsNoop(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	seedProject(t, store, "p")

	fp0, err := store.ProjectsFingerprint(ctx)
	if err != nil {
		t.Fatalf("fingerprint: %v", err)
	}
	if err := store.DeleteFleetSetting(ctx, "worker_max_tokens", "tester"); err != nil {
		t.Fatalf("DeleteFleetSetting unset: %v", err)
	}
	fp1, err := store.ProjectsFingerprint(ctx)
	if err != nil {
		t.Fatalf("fingerprint: %v", err)
	}
	if fp1["p"] != fp0["p"] {
		t.Fatalf("fingerprint changed on no-op delete: %v -> %v", fp0["p"], fp1["p"])
	}
	audit, err := store.SettingsAudit(ctx, 0)
	if err != nil {
		t.Fatalf("SettingsAudit: %v", err)
	}
	if len(audit) != 0 {
		t.Fatalf("no-op delete wrote %d audit rows, want 0", len(audit))
	}
}

// Every settings change is journaled with old->new, actor, and an RFC3339 timestamp.
func TestSettingsAuditJournal(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	seedProject(t, store, "p")

	if err := store.SetFleetSetting(ctx, "supervisor.backend", "codex", "alice"); err != nil {
		t.Fatalf("set: %v", err)
	}
	if err := store.SetFleetSetting(ctx, "supervisor.backend", "claude", "bob"); err != nil {
		t.Fatalf("set 2: %v", err)
	}
	audit, err := store.SettingsAudit(ctx, 0)
	if err != nil {
		t.Fatalf("audit: %v", err)
	}
	if len(audit) != 2 {
		t.Fatalf("audit rows = %d, want 2", len(audit))
	}
	// Newest first.
	if audit[0].Actor != "bob" || audit[0].OldValue != "codex" || audit[0].NewValue != "claude" {
		t.Fatalf("newest audit = %+v", audit[0])
	}
	if audit[0].Scope != scopeFleet {
		t.Fatalf("scope = %q, want fleet", audit[0].Scope)
	}
	if _, err := time.Parse(time.RFC3339Nano, audit[0].ChangedAt); err != nil {
		t.Fatalf("changed_at %q not RFC3339: %v", audit[0].ChangedAt, err)
	}
}

// A project override is journaled under project scope.
func TestProjectSettingAudit(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	seedProject(t, store, "acme")

	if err := store.SetProjectSetting(ctx, "acme", "poll_interval_seconds", "300", "carol"); err != nil {
		t.Fatalf("SetProjectSetting: %v", err)
	}
	cfg, err := store.Load(ctx, "acme")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.PollIntervalSeconds != 300 {
		t.Fatalf("poll_interval_seconds = %d, want 300", cfg.PollIntervalSeconds)
	}
	audit, err := store.SettingsAudit(ctx, 1)
	if err != nil {
		t.Fatalf("audit: %v", err)
	}
	if len(audit) != 1 || audit[0].Scope != scopeProject("acme") || audit[0].Actor != "carol" {
		t.Fatalf("project audit = %+v", audit)
	}
}

// An unknown key or ill-typed value is rejected before it can land.
func TestSettingValidation(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	seedProject(t, store, "p")

	if err := store.SetFleetSetting(ctx, "not.a.key", "x", "t"); err == nil {
		t.Fatal("expected error for unknown key")
	}
	if err := store.SetFleetSetting(ctx, "supervisor.enabled", "maybe", "t"); err == nil {
		t.Fatal("expected error for non-bool value")
	}
	if err := store.SetFleetSetting(ctx, "worker_max_tokens", "lots", "t"); err == nil {
		t.Fatal("expected error for non-int value")
	}
}

func TestFleetConcurrencySettingsStoredAtFleetScopeOnly(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	seedProject(t, store, "p")

	if err := store.SetFleetSetting(ctx, config.FleetMaxLiveWorkersKey, "10", "tester"); err != nil {
		t.Fatalf("SetFleetSetting: %v", err)
	}
	if err := store.SetFleetSetting(ctx, config.FleetMaxLiveWorkersKey, "4", "tester"); err == nil {
		t.Fatal("expected fleet max below the default min to be rejected")
	}
	settings, err := store.FleetConcurrencySettings(ctx)
	if err != nil {
		t.Fatalf("FleetConcurrencySettings: %v", err)
	}
	if settings.MinLiveWorkers != config.DefaultFleetMinLiveWorkers || settings.MaxLiveWorkers != 10 {
		t.Fatalf("settings = %+v", settings)
	}
	if err := store.SetProjectSetting(ctx, "p", config.FleetMaxLiveWorkersKey, "7", "tester"); err == nil || !strings.Contains(err.Error(), "fleet-only") {
		t.Fatalf("SetProjectSetting error = %v, want fleet-only rejection", err)
	}
	if err := store.DeleteProjectSetting(ctx, "p", config.FleetMaxLiveWorkersKey, "tester"); err == nil || !strings.Contains(err.Error(), "fleet-only") {
		t.Fatalf("DeleteProjectSetting error = %v, want fleet-only rejection", err)
	}

	cfg, err := store.Load(ctx, "p")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if _, ok := cfg.SettingsSources[config.FleetMaxLiveWorkersKey]; !ok {
		t.Fatalf("settings sources = %#v, want fleet-only provenance", cfg.SettingsSources)
	}
	if got := cfg.FleetOnlySettings[config.FleetMaxLiveWorkersKey]; got != "10" {
		t.Fatalf("fleet-only max = %q, want 10", got)
	}
	if got := cfg.FleetOnlySettings[config.FleetMinLiveWorkersKey]; got != "5" {
		t.Fatalf("fleet-only min = %q, want builtin 5", got)
	}
}

// config-store export writes the fleet settings file; a fresh store re-imports it.
func TestExportImportRoundTripsFleetSettings(t *testing.T) {
	ctx := context.Background()
	src := openTestStore(t)
	seedProject(t, src, "p")
	if err := src.SetFleetSetting(ctx, "supervisor.enabled", "false", "tester"); err != nil {
		t.Fatalf("set enabled: %v", err)
	}
	if err := src.SetFleetSetting(ctx, "worker_max_tokens", "123456", "tester"); err != nil {
		t.Fatalf("set tokens: %v", err)
	}
	route := `[{"provider":"anthropic","default":"claude"},{"provider":"openai","default":"sol","fallback_backends":["gpt55"]}]`
	if err := src.SetFleetSetting(ctx, "model.provider_lanes", route, "tester"); err != nil {
		t.Fatalf("set provider lanes: %v", err)
	}

	dir := t.TempDir()
	if err := src.ExportDir(ctx, dir); err != nil {
		t.Fatalf("ExportDir: %v", err)
	}

	dst := openTestStore(t)
	if err := dst.ImportDir(ctx, dir); err != nil {
		t.Fatalf("ImportDir: %v", err)
	}
	got, err := dst.FleetSettings(ctx)
	if err != nil {
		t.Fatalf("FleetSettings: %v", err)
	}
	if got["supervisor.enabled"] != "false" || got["worker_max_tokens"] != "123456" {
		t.Fatalf("round-tripped fleet settings = %v", got)
	}
	if got["model.provider_lanes"] != route {
		t.Fatalf("round-tripped provider lanes = %q, want %q", got["model.provider_lanes"], route)
	}
}
