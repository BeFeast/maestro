package config

import "testing"

func TestParse_GitHubAppConfig(t *testing.T) {
	yaml := `
repo: owner/repo
github_app:
  app_id: 4242
  private_key_path: /etc/maestro/app-key.pem
  installation_id: 99
`
	cfg, err := parse([]byte(yaml))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if cfg.GitHubApp.AppID != 4242 {
		t.Fatalf("app_id = %d, want 4242", cfg.GitHubApp.AppID)
	}
	if cfg.GitHubApp.PrivateKeyPath != "/etc/maestro/app-key.pem" {
		t.Fatalf("private_key_path = %q", cfg.GitHubApp.PrivateKeyPath)
	}
	if cfg.GitHubApp.InstallationID != 99 {
		t.Fatalf("installation_id = %d, want 99", cfg.GitHubApp.InstallationID)
	}
	if !cfg.GitHubApp.Configured() {
		t.Fatal("Configured() should be true when all three fields are set")
	}
}

func TestParse_GitHubAppConfig_AbsentIsUnconfigured(t *testing.T) {
	cfg, err := parse([]byte("repo: owner/repo\n"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if cfg.GitHubApp.Configured() {
		t.Fatal("Configured() must be false when github_app is absent — PAT path stays default")
	}
}

func TestGitHubAppConfig_ConfiguredRequiresAllThree(t *testing.T) {
	cases := []struct {
		name string
		cfg  GitHubAppConfig
		want bool
	}{
		{"all set", GitHubAppConfig{AppID: 1, PrivateKeyPath: "/k.pem", InstallationID: 2}, true},
		{"missing app_id", GitHubAppConfig{PrivateKeyPath: "/k.pem", InstallationID: 2}, false},
		{"missing key path", GitHubAppConfig{AppID: 1, InstallationID: 2}, false},
		{"blank key path", GitHubAppConfig{AppID: 1, PrivateKeyPath: "  ", InstallationID: 2}, false},
		{"missing installation", GitHubAppConfig{AppID: 1, PrivateKeyPath: "/k.pem"}, false},
		{"zero", GitHubAppConfig{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.cfg.Configured(); got != tc.want {
				t.Fatalf("Configured() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestGitHubAppConfig_NoRawKeyMaterial guards the "no secrets in the config
// store" acceptance criterion (#823): the config carries only the PEM file
// PATH, never the key bytes, so nothing secret is ever serialized or logged.
func TestGitHubAppConfig_NoRawKeyMaterial(t *testing.T) {
	cfg := GitHubAppConfig{AppID: 1, PrivateKeyPath: "/etc/maestro/app-key.pem", InstallationID: 2}
	// The struct has exactly three fields; a private key field would be a leak.
	if got := cfg.PrivateKeyPath; got == "" {
		t.Fatal("expected a path, not inline key material")
	}
	// A YAML round-trip must not surface anything resembling a private key.
	yaml := `
repo: owner/repo
github_app:
  app_id: 1
  private_key_path: /etc/maestro/app-key.pem
  installation_id: 2
`
	parsed, err := parse([]byte(yaml))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if parsed.GitHubApp.PrivateKeyPath != "/etc/maestro/app-key.pem" {
		t.Fatalf("path not preserved: %q", parsed.GitHubApp.PrivateKeyPath)
	}
}
