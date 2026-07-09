package server

import (
	"testing"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/configstore"
)

// The #839 cost-control block surfaces each knob's effective value and source
// (builtin/fleet/project) on effective_config, so Mission Control can flag
// non-default overrides.
func TestBuildFleetCostControls_ReportsSources(t *testing.T) {
	cfg := &config.Config{
		Supervisor:      config.SupervisorConfig{Enabled: true},
		WorkerMaxTokens: 400000,
		SettingSources: map[string]string{
			"supervisor.enabled": configstore.SourceProject,
			"worker_max_tokens":  configstore.SourceFleet,
		},
	}
	eff := buildFleetEffectiveConfig(cfg)
	got := map[string]fleetCostControl{}
	for _, cc := range eff.CostControls {
		got[cc.Key] = cc
	}
	if len(got) == 0 {
		t.Fatal("cost_controls empty; want one entry per knob")
	}
	if cc := got["supervisor.enabled"]; cc.Value != "true" || cc.Source != configstore.SourceProject {
		t.Errorf("supervisor.enabled = %+v, want value=true source=project", cc)
	}
	if cc := got["worker_max_tokens"]; cc.Value != "400000" || cc.Source != configstore.SourceFleet {
		t.Errorf("worker_max_tokens = %+v, want value=400000 source=fleet", cc)
	}
	// A knob with no recorded source falls back to builtin.
	if cc := got["poll_interval_seconds"]; cc.Source != configstore.SourceBuiltin {
		t.Errorf("poll_interval_seconds source = %q, want builtin", cc.Source)
	}
}

// A file-loaded config (no fleet layer, SettingSources nil) yields no
// cost-control provenance rather than a misleading all-builtin block.
func TestBuildFleetCostControls_NilForFileConfig(t *testing.T) {
	cfg := &config.Config{Supervisor: config.SupervisorConfig{Enabled: true}}
	eff := buildFleetEffectiveConfig(cfg)
	if len(eff.CostControls) != 0 {
		t.Fatalf("cost_controls = %d entries, want 0 for a config with nil SettingSources", len(eff.CostControls))
	}
}
