package config

import (
	"testing"
	"time"

	"github.com/befeast/maestro/internal/progress"
)

// A new hands-off project with no watchdog config gets the durable 20-minute
// max-silence contract by default (#887), and the watchdog is enabled — unlike
// the legacy worker_silent_timeout_minutes, which defaults to disabled.
func TestStalledProgressWatchdog_DefaultsForHandsOffProject(t *testing.T) {
	path := writeConfig(t, "repo: owner/repo\n")
	cfg, err := LoadFrom(path)
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	w := cfg.StalledProgressWatchdog
	if !w.IsEnabled() {
		t.Fatalf("watchdog disabled by default, want enabled for hands-off projects")
	}
	if got := w.EffectiveMaxSilence(); got != 20*time.Minute {
		t.Errorf("EffectiveMaxSilence = %s, want 20m", got)
	}
	if got := w.EffectiveMaxSilence(); got != progress.DefaultMaxSilence {
		t.Errorf("default silence budget = %s, want progress.DefaultMaxSilence", got)
	}
	if got := w.EffectiveEvalInterval(); got != 60*time.Second {
		t.Errorf("EffectiveEvalInterval = %s, want 60s", got)
	}
	// The legacy terminal-only timeout stays disabled by default: it is never
	// silently emitted for a new project.
	if cfg.WorkerSilentTimeoutMinutes != 0 {
		t.Errorf("legacy worker_silent_timeout_minutes = %d, want 0 (disabled)", cfg.WorkerSilentTimeoutMinutes)
	}
}

func TestStalledProgressWatchdog_ExplicitOverrides(t *testing.T) {
	path := writeConfig(t, `repo: owner/repo
stalled_progress_watchdog:
  enabled: true
  max_silence_minutes: 30
  eval_interval_seconds: 15
`)
	cfg, err := LoadFrom(path)
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	w := cfg.StalledProgressWatchdog
	if got := w.EffectiveMaxSilence(); got != 30*time.Minute {
		t.Errorf("EffectiveMaxSilence = %s, want 30m", got)
	}
	if got := w.EffectiveEvalInterval(); got != 15*time.Second {
		t.Errorf("EffectiveEvalInterval = %s, want 15s", got)
	}
}

// An explicit disable (or a negative budget) collapses to a zero silence
// budget, which the evaluator reads as "disabled" so no quiet worker is killed.
func TestStalledProgressWatchdog_DisabledYieldsZeroBudget(t *testing.T) {
	disabled := writeConfig(t, `repo: owner/repo
stalled_progress_watchdog:
  enabled: false
`)
	cfg, err := LoadFrom(disabled)
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	if cfg.StalledProgressWatchdog.IsEnabled() {
		t.Fatalf("watchdog enabled after explicit disable")
	}
	if got := cfg.StalledProgressWatchdog.EffectiveMaxSilence(); got != 0 {
		t.Errorf("EffectiveMaxSilence = %s, want 0 (disabled)", got)
	}

	negative := writeConfig(t, `repo: owner/repo
stalled_progress_watchdog:
  max_silence_minutes: -1
`)
	cfgNeg, err := LoadFrom(negative)
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	if got := cfgNeg.StalledProgressWatchdog.EffectiveMaxSilence(); got != 0 {
		t.Errorf("negative budget EffectiveMaxSilence = %s, want 0 (disabled)", got)
	}
}

// The watchdog knobs are fleet-controllable so an operator can flip them
// fleet-wide or per project via `maestro settings`.
func TestStalledProgressWatchdog_FleetSettingsRegistered(t *testing.T) {
	for _, key := range []string{
		"stalled_progress_watchdog.enabled",
		"stalled_progress_watchdog.max_silence_minutes",
	} {
		if _, ok := FleetSettingSpecByKey(key); !ok {
			t.Errorf("fleet setting %q not registered", key)
		}
	}
	// max_silence_minutes validates as an int.
	if _, err := NormalizeSettingValue("stalled_progress_watchdog.max_silence_minutes", "25"); err != nil {
		t.Errorf("NormalizeSettingValue(int): %v", err)
	}
	if _, err := NormalizeSettingValue("stalled_progress_watchdog.enabled", "true"); err != nil {
		t.Errorf("NormalizeSettingValue(bool): %v", err)
	}
}
