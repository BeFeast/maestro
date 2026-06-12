package config

import (
	"strings"
	"testing"
)

// #704: quota.window_tokens / quota.weekly_tokens calibrate the
// subscription window estimate; dispatch_threshold defaults to 0.85.
func TestParse_BackendQuota(t *testing.T) {
	yaml := `
repo: owner/repo
model:
  default: claude
  backends:
    claude:
      cmd: claude
      quota:
        window_tokens: 10000000
        weekly_tokens: 60000000
    codex:
      cmd: codex
`
	cfg, err := parse([]byte(yaml))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	q := cfg.Model.Backends["claude"].Quota
	if !q.Configured() {
		t.Fatal("claude quota should be configured")
	}
	if q.WindowTokens != 10_000_000 || q.WeeklyTokens != 60_000_000 {
		t.Fatalf("quota = %+v, want window 10M / weekly 60M", q)
	}
	if got := q.EffectiveDispatchThreshold(); got != DefaultQuotaDispatchThreshold {
		t.Fatalf("threshold = %v, want default %v", got, DefaultQuotaDispatchThreshold)
	}
	if cfg.Model.Backends["codex"].Quota.Configured() {
		t.Fatal("codex has no quota block; Configured() must be false")
	}
}

func TestParse_BackendQuotaThresholdOverride(t *testing.T) {
	yaml := `
repo: owner/repo
model:
  default: claude
  backends:
    claude:
      cmd: claude
      quota:
        window_tokens: 1000
        dispatch_threshold: 0.7
`
	cfg, err := parse([]byte(yaml))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got := cfg.Model.Backends["claude"].Quota.EffectiveDispatchThreshold(); got != 0.7 {
		t.Fatalf("threshold = %v, want 0.7", got)
	}
}

// A percent-style threshold (85 instead of 0.85) would silently never
// trigger; reject it at parse time.
func TestParse_BackendQuotaThresholdRejectsPercentStyle(t *testing.T) {
	yaml := `
repo: owner/repo
model:
  default: claude
  backends:
    claude:
      cmd: claude
      quota:
        window_tokens: 1000
        dispatch_threshold: 85
`
	_, err := parse([]byte(yaml))
	if err == nil || !strings.Contains(err.Error(), "dispatch_threshold") {
		t.Fatalf("err = %v, want dispatch_threshold validation error", err)
	}
}

func TestParse_BackendQuotaRejectsNegativeCapacity(t *testing.T) {
	yaml := `
repo: owner/repo
model:
  default: claude
  backends:
    claude:
      cmd: claude
      quota:
        window_tokens: -5
`
	_, err := parse([]byte(yaml))
	if err == nil || !strings.Contains(err.Error(), "window_tokens") {
		t.Fatalf("err = %v, want window_tokens validation error", err)
	}
}
