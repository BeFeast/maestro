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
	if cfg.SelfDeploy.InstallViaSudo {
		t.Fatal("self_deploy.install_via_sudo must default to false")
	}
	// #716: scope defaults to "user" for back-compat.
	if got := cfg.SelfDeploy.EffectiveScope(); got != SelfDeployScopeUser {
		t.Fatalf("EffectiveScope() = %q, want %q", got, SelfDeployScopeUser)
	}
}

// #716: scope is config-driven, defaults to "user", and only "system"
// (case-insensitive) selects the system manager.
func TestSelfDeployScope(t *testing.T) {
	cases := []struct {
		raw  string
		want string
	}{
		{"", SelfDeployScopeUser},
		{"   ", SelfDeployScopeUser},
		{"user", SelfDeployScopeUser},
		{"bogus", SelfDeployScopeUser},
		{"system", SelfDeployScopeSystem},
		{" System ", SelfDeployScopeSystem},
	}
	for _, tc := range cases {
		sd := SelfDeployConfig{Scope: tc.raw}
		if got := sd.EffectiveScope(); got != tc.want {
			t.Errorf("EffectiveScope(%q) = %q, want %q", tc.raw, got, tc.want)
		}
	}

	cfg, err := Parse([]byte("repo: owner/repo\nself_deploy:\n  enabled: true\n  scope: system\n"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got := cfg.SelfDeploy.EffectiveScope(); got != SelfDeployScopeSystem {
		t.Errorf("parsed scope EffectiveScope() = %q, want %q", got, SelfDeployScopeSystem)
	}
}

func TestSelfDeployParse(t *testing.T) {
	yaml := `
repo: owner/repo
self_deploy:
  enabled: true
  bin_path: /usr/local/bin/maestro
  install_via_sudo: true
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
	if !sd.InstallViaSudo {
		t.Error("install_via_sudo = false, want true")
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
	// #722: the debounce window defaults to the deploy timeout so at most one
	// deploy runs per budget.
	if got := sd.EffectiveMinIntervalMinutes(); got != 30 {
		t.Errorf("EffectiveMinIntervalMinutes() default = %d, want 30 (timeout)", got)
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

// #722: the debounce window defaults to the deploy timeout (so re-triggers
// cannot stack while a deploy may still be in flight) and an explicit
// min_interval_minutes overrides it for faster back-to-back deploys.
func TestSelfDeployEffectiveMinInterval(t *testing.T) {
	// Default: falls back to EffectiveTimeoutMinutes().
	if got := (SelfDeployConfig{}).EffectiveMinIntervalMinutes(); got != 30 {
		t.Errorf("default EffectiveMinIntervalMinutes() = %d, want 30", got)
	}
	if got := (SelfDeployConfig{TimeoutMinutes: 45}).EffectiveMinIntervalMinutes(); got != 45 {
		t.Errorf("EffectiveMinIntervalMinutes() with timeout 45 = %d, want 45", got)
	}
	// Explicit override wins over the timeout default.
	if got := (SelfDeployConfig{TimeoutMinutes: 45, MinIntervalMinutes: 5}).EffectiveMinIntervalMinutes(); got != 5 {
		t.Errorf("EffectiveMinIntervalMinutes() override = %d, want 5", got)
	}

	cfg, err := Parse([]byte("repo: owner/repo\nself_deploy:\n  enabled: true\n  min_interval_minutes: 7\n"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got := cfg.SelfDeploy.EffectiveMinIntervalMinutes(); got != 7 {
		t.Errorf("parsed min_interval_minutes EffectiveMinIntervalMinutes() = %d, want 7", got)
	}
}
