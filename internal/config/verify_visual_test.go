package config

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParse_VerifyVisualConfig(t *testing.T) {
	yaml := `
repo: owner/repo
local_path: /tmp/repo
verify:
  visual:
    enabled: true
    command: ./scripts/capture-screenshots.sh
    paths:
      - "**/*.jsx"
      - "web/**"
    output_dir: artifacts/shots
    timeout_minutes: 5
`
	cfg, err := Parse([]byte(yaml))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	v := cfg.Verify.Visual
	if !v.Enabled {
		t.Fatal("verify.visual.enabled should parse as true")
	}
	if v.Command != "./scripts/capture-screenshots.sh" {
		t.Fatalf("command = %q", v.Command)
	}
	if len(v.Paths) != 2 || v.Paths[0] != "**/*.jsx" || v.Paths[1] != "web/**" {
		t.Fatalf("paths = %v", v.Paths)
	}
	if !v.Active() {
		t.Fatal("fully configured verify.visual should be Active")
	}
	if v.ResolvedOutputDir() != "artifacts/shots" {
		t.Fatalf("ResolvedOutputDir = %q", v.ResolvedOutputDir())
	}
	if v.Timeout() != 5*time.Minute {
		t.Fatalf("Timeout = %v", v.Timeout())
	}
}

func TestParse_VerifyVisualDefaultsOff(t *testing.T) {
	cfg, err := Parse([]byte("repo: owner/repo\nlocal_path: /tmp/repo\n"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	v := cfg.Verify.Visual
	if v.Enabled || v.Active() {
		t.Fatal("verify.visual must default to disabled")
	}
	if got, want := v.ResolvedOutputDir(), filepath.Join(".maestro", "screenshots"); got != want {
		t.Fatalf("default output dir = %q, want %q", got, want)
	}
	if v.Timeout() != 10*time.Minute {
		t.Fatalf("default timeout = %v, want 10m", v.Timeout())
	}
}

func TestVerifyVisualActive(t *testing.T) {
	cases := []struct {
		name string
		v    VerifyVisualConfig
		want bool
	}{
		{"disabled", VerifyVisualConfig{Command: "x", Paths: []string{"web/**"}}, false},
		{"no command", VerifyVisualConfig{Enabled: true, Paths: []string{"web/**"}}, false},
		{"blank command", VerifyVisualConfig{Enabled: true, Command: "  ", Paths: []string{"web/**"}}, false},
		{"no paths", VerifyVisualConfig{Enabled: true, Command: "x"}, false},
		{"configured", VerifyVisualConfig{Enabled: true, Command: "x", Paths: []string{"web/**"}}, true},
	}
	for _, tc := range cases {
		if got := tc.v.Active(); got != tc.want {
			t.Errorf("%s: Active() = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestWarnings_VerifyVisualMisconfigured(t *testing.T) {
	t.Run("enabled without command and paths", func(t *testing.T) {
		cfg := &Config{Verify: VerifyConfig{Visual: VerifyVisualConfig{Enabled: true}}}
		warnings := cfg.Warnings()
		found := false
		for _, w := range warnings {
			if strings.Contains(w, "verify.visual.command and verify.visual.paths") {
				found = true
			}
		}
		if !found {
			t.Fatalf("expected verify.visual warning, got %v", warnings)
		}
	})

	t.Run("enabled without paths", func(t *testing.T) {
		cfg := &Config{Verify: VerifyConfig{Visual: VerifyVisualConfig{Enabled: true, Command: "./capture.sh"}}}
		warnings := cfg.Warnings()
		found := false
		for _, w := range warnings {
			if strings.Contains(w, "verify.visual.paths") && !strings.Contains(w, "verify.visual.command") {
				found = true
			}
		}
		if !found {
			t.Fatalf("expected paths-only warning, got %v", warnings)
		}
	})

	t.Run("fully configured is silent", func(t *testing.T) {
		cfg := &Config{Verify: VerifyConfig{Visual: VerifyVisualConfig{
			Enabled: true, Command: "./capture.sh", Paths: []string{"web/**"},
		}}}
		for _, w := range cfg.Warnings() {
			if strings.Contains(w, "verify.visual") {
				t.Fatalf("unexpected verify.visual warning: %s", w)
			}
		}
	})

	t.Run("disabled is silent", func(t *testing.T) {
		cfg := &Config{}
		for _, w := range cfg.Warnings() {
			if strings.Contains(w, "verify.visual") {
				t.Fatalf("unexpected verify.visual warning: %s", w)
			}
		}
	})
}
