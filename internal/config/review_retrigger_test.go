package config

import (
	"testing"
	"time"
)

func TestReviewRetriggerDefaults(t *testing.T) {
	cfg, err := Parse([]byte("repo: owner/repo\n"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	rt := cfg.ReviewRetrigger
	if !rt.Active() {
		t.Errorf("Active() = false, want default on")
	}
	if got := rt.EffectivePendingFor(); got != 10*time.Minute {
		t.Errorf("EffectivePendingFor() = %v, want 10m default", got)
	}
	if got := rt.EffectiveCooldown(); got != 30*time.Minute {
		t.Errorf("EffectiveCooldown() = %v, want 30m default", got)
	}
}

func TestReviewRetriggerParseOverrides(t *testing.T) {
	cfg, err := Parse([]byte(`
repo: owner/repo
review_retrigger:
  enabled: false
  pending_minutes: 15
  cooldown_minutes: 45
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	rt := cfg.ReviewRetrigger
	if rt.Active() {
		t.Errorf("Active() = true, want disabled via enabled: false")
	}
	if got := rt.EffectivePendingFor(); got != 15*time.Minute {
		t.Errorf("EffectivePendingFor() = %v, want 15m", got)
	}
	if got := rt.EffectiveCooldown(); got != 45*time.Minute {
		t.Errorf("EffectiveCooldown() = %v, want 45m", got)
	}
}

func TestReviewRetriggerNonPositiveValuesFallBackToDefaults(t *testing.T) {
	rt := ReviewRetriggerConfig{PendingMinutes: -5, CooldownMinutes: 0}
	if got := rt.EffectivePendingFor(); got != 10*time.Minute {
		t.Errorf("EffectivePendingFor() = %v, want 10m fallback", got)
	}
	if got := rt.EffectiveCooldown(); got != 30*time.Minute {
		t.Errorf("EffectiveCooldown() = %v, want 30m fallback", got)
	}
}
