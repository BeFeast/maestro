package config

import (
	"strings"
	"testing"
	"time"
)

// model.hold_on_cooldown parses, defaults max_wait to 6h, and rejects a
// negative window.
func TestParse_HoldOnCooldown(t *testing.T) {
	yaml := `
repo: owner/repo
model:
  default: claude
  hold_on_cooldown:
    enabled: true
  backends:
    claude:
      cmd: claude
`
	cfg, err := parse([]byte(yaml))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	hold := cfg.Model.HoldOnCooldown
	if !hold.Enabled {
		t.Fatal("hold_on_cooldown.enabled should parse true")
	}
	if got := hold.MaxWait(); got != 360*time.Minute {
		t.Fatalf("MaxWait() = %v, want default 6h", got)
	}
}

func TestParse_HoldOnCooldownExplicitWindow(t *testing.T) {
	yaml := `
repo: owner/repo
model:
  default: claude
  hold_on_cooldown:
    enabled: true
    max_wait_minutes: 90
  backends:
    claude:
      cmd: claude
`
	cfg, err := parse([]byte(yaml))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got := cfg.Model.HoldOnCooldown.MaxWait(); got != 90*time.Minute {
		t.Fatalf("MaxWait() = %v, want 90m", got)
	}
}

func TestParse_HoldOnCooldownRejectsNegativeWindow(t *testing.T) {
	yaml := `
repo: owner/repo
model:
  default: claude
  hold_on_cooldown:
    max_wait_minutes: -5
  backends:
    claude:
      cmd: claude
`
	_, err := parse([]byte(yaml))
	if err == nil || !strings.Contains(err.Error(), "hold_on_cooldown.max_wait_minutes") {
		t.Fatalf("err = %v, want max_wait_minutes rejection", err)
	}
}
