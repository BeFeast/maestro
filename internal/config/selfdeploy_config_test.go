package config

import (
	"path/filepath"
	"testing"
)

// #698 AC3: flag default OFF — a config without a self_deploy block must not
// enable self-deploy or change any behavior.
func TestSelfDeployDefaultOff(t *testing.T) {
	cfg, err := Parse([]byte("repo: owner/repo\n"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if cfg.SelfDeploy.Enabled {
		t.Fatal("self_deploy.enabled must default to false")
	}
}

func TestSelfDeployParse(t *testing.T) {
	yaml := `
repo: owner/repo
self_deploy:
  enabled: true
  bin_path: /usr/local/bin/maestro
  units: ["maestro.service", " maestro-fleet.service "]
  health_url: http://127.0.0.1:9999/api/v1/state
  timeout_minutes: 45
`
	cfg, err := Parse([]byte(yaml))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	sd := cfg.SelfDeploy
	if !sd.Enabled {
		t.Fatal("enabled = false, want true")
	}
	if sd.BinPath != "/usr/local/bin/maestro" {
		t.Errorf("bin_path = %q", sd.BinPath)
	}
	if got := sd.EffectiveUnits(); len(got) != 2 || got[0] != "maestro.service" || got[1] != "maestro-fleet.service" {
		t.Errorf("EffectiveUnits() = %v", got)
	}
	if got := sd.EffectiveHealthURL(cfg.Server); got != "http://127.0.0.1:9999/api/v1/state" {
		t.Errorf("EffectiveHealthURL() = %q", got)
	}
	if got := sd.EffectiveTimeoutMinutes(); got != 45 {
		t.Errorf("EffectiveTimeoutMinutes() = %d", got)
	}
}

func TestSelfDeployEffectiveDefaults(t *testing.T) {
	var sd SelfDeployConfig

	if got := sd.EffectiveScript("/srv/maestro"); got != filepath.Join("/srv/maestro", "scripts", "self-deploy.sh") {
		t.Errorf("EffectiveScript() = %q", got)
	}
	if got := sd.EffectiveScript(""); got != "" {
		t.Errorf("EffectiveScript(\"\") = %q, want empty", got)
	}
	if got := sd.EffectiveUnits(); len(got) != 1 || got[0] != "maestro.service" {
		t.Errorf("EffectiveUnits() = %v", got)
	}
	if got := sd.EffectiveTimeoutMinutes(); got != 30 {
		t.Errorf("EffectiveTimeoutMinutes() = %d, want 30", got)
	}

	// No server port → no health URL (CLI + unit checks only).
	if got := sd.EffectiveHealthURL(ServerConfig{}); got != "" {
		t.Errorf("EffectiveHealthURL(no port) = %q, want empty", got)
	}
	// Server port set → default to the project state endpoint.
	if got := sd.EffectiveHealthURL(ServerConfig{Port: 8788}); got != "http://127.0.0.1:8788/api/v1/state" {
		t.Errorf("EffectiveHealthURL(port) = %q", got)
	}
	if got := sd.EffectiveHealthTokenEnv(ServerConfig{Auth: ServerAuthConfig{TokenEnv: "MC_TOKEN"}}); got != "MC_TOKEN" {
		t.Errorf("EffectiveHealthTokenEnv() = %q", got)
	}
}
