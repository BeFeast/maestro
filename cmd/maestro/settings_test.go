package main

import "testing"

func TestParseSettingAssignment(t *testing.T) {
	cases := []struct {
		arg       string
		wantKey   string
		wantValue string
	}{
		{"supervisor.enabled=false", "supervisor.enabled", "false"},
		{"worker_max_tokens=400000", "worker_max_tokens", "400000"},
		{"  supervisor.backend = claude ", "supervisor.backend", "claude"},
		{"supervisor.enabled=", "supervisor.enabled", ""}, // clear form
		{"supervisor.enabled", "supervisor.enabled", ""},  // bare key (used with --unset)
		{"key=a=b", "key", "a=b"},                         // only the first '=' splits
	}
	for _, c := range cases {
		k, v := parseSettingAssignment(c.arg)
		if k != c.wantKey || v != c.wantValue {
			t.Errorf("parseSettingAssignment(%q) = (%q, %q), want (%q, %q)", c.arg, k, v, c.wantKey, c.wantValue)
		}
	}
}

func TestDefaultSettingsActor(t *testing.T) {
	t.Setenv("USER", "alice")
	if got := defaultSettingsActor(); got != "alice (cli)" {
		t.Errorf("defaultSettingsActor() = %q, want \"alice (cli)\"", got)
	}
	t.Setenv("USER", "")
	if got := defaultSettingsActor(); got != "cli" {
		t.Errorf("defaultSettingsActor() with empty USER = %q, want \"cli\"", got)
	}
}
