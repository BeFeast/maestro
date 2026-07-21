package config

import "testing"

func TestNtfyConfig_Enabled(t *testing.T) {
	cases := []struct {
		name string
		cfg  NtfyConfig
		want bool
	}{
		{"empty", NtfyConfig{}, false},
		{"base only", NtfyConfig{BaseURL: "https://ntfy.sh"}, false},
		{"topic only", NtfyConfig{Topic: "proj"}, false},
		{"base and topic", NtfyConfig{BaseURL: "https://ntfy.sh", Topic: "proj"}, true},
		{"whitespace", NtfyConfig{BaseURL: "  ", Topic: "proj"}, false},
	}
	for _, c := range cases {
		if got := c.cfg.Enabled(); got != c.want {
			t.Errorf("%s: Enabled() = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestNtfyConfig_Token_FromEnv(t *testing.T) {
	// Unset TokenEnv -> empty (public topic, no auth).
	if got := (NtfyConfig{}).Token(); got != "" {
		t.Errorf("Token() with no TokenEnv = %q, want empty", got)
	}

	// TokenEnv naming an unset variable -> empty.
	if got := (NtfyConfig{TokenEnv: "MAESTRO_TEST_NTFY_UNSET"}).Token(); got != "" {
		t.Errorf("Token() with unset env = %q, want empty", got)
	}

	// TokenEnv naming a populated variable -> the value, trimmed.
	t.Setenv("MAESTRO_TEST_NTFY_TOKEN", "  tok-value  ")
	if got := (NtfyConfig{TokenEnv: "MAESTRO_TEST_NTFY_TOKEN"}).Token(); got != "tok-value" {
		t.Errorf("Token() = %q, want tok-value", got)
	}
}

func TestNtfyConfig_ParsedFromYAML(t *testing.T) {
	yaml := []byte(`
repo: owner/name
notify:
  ntfy:
    base_url: https://ntfy.example
    topic: proj-alerts
    token_env: MY_NTFY_TOKEN
`)
	cfg, err := parse(yaml)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if cfg.Notify.Ntfy.BaseURL != "https://ntfy.example" {
		t.Errorf("BaseURL = %q", cfg.Notify.Ntfy.BaseURL)
	}
	if cfg.Notify.Ntfy.Topic != "proj-alerts" {
		t.Errorf("Topic = %q", cfg.Notify.Ntfy.Topic)
	}
	if cfg.Notify.Ntfy.TokenEnv != "MY_NTFY_TOKEN" {
		t.Errorf("TokenEnv = %q", cfg.Notify.Ntfy.TokenEnv)
	}
	if !cfg.Notify.Ntfy.Enabled() {
		t.Errorf("Enabled() = false, want true for a fully configured ntfy block")
	}
}
