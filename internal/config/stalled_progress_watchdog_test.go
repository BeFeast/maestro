package config

import (
	"testing"
	"time"
)

// Missing config is inactive. New hands-off projects opt in through the
// lifecycle/genesis template; upgrading must never arm recovery for every
// legacy project implicitly.
func TestStalledProgressWatchdog_MissingConfigIsInactive(t *testing.T) {
	path := writeConfig(t, "repo: owner/repo\n")
	cfg, err := LoadFrom(path)
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	w := cfg.StalledProgressWatchdog
	if w.IsEnabled() || w.IsActive() {
		t.Fatalf("watchdog active without explicit opt-in")
	}
	if got := w.EffectiveMaxSilence(); got != 0 {
		t.Errorf("EffectiveMaxSilence = %s, want disabled", got)
	}
	if got := w.EffectiveEvalInterval(); got != 60*time.Second {
		t.Errorf("EffectiveEvalInterval = %s, want 60s", got)
	}
	// The legacy terminal-only timeout also stays disabled by default.
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
		"stalled_progress_watchdog.eval_interval_seconds",
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
	if _, err := NormalizeSettingValue("stalled_progress_watchdog.eval_interval_seconds", "15"); err != nil {
		t.Errorf("NormalizeSettingValue(cadence): %v", err)
	}
}

func TestStalledProgressWatchdog_LegacyAndV1NeverRunInParallel(t *testing.T) {
	t.Run("explicit v1 budget wins", func(t *testing.T) {
		cfg, err := Parse([]byte(`repo: owner/repo
worker_silent_timeout_minutes: 25
stalled_progress_watchdog:
  enabled: true
  max_silence_minutes: 30
`))
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}
		if cfg.WorkerSilentTimeoutMinutes != 0 || cfg.EffectiveWorkerSilentTimeout() != 0 {
			t.Fatalf("legacy timeout remained active: raw=%d effective=%s", cfg.WorkerSilentTimeoutMinutes, cfg.EffectiveWorkerSilentTimeout())
		}
		if got := cfg.StalledProgressWatchdog.EffectiveMaxSilence(); got != 30*time.Minute {
			t.Fatalf("v1 budget = %s, want 30m", got)
		}
	})

	t.Run("explicit v1 disable preserves compatibility escape hatch", func(t *testing.T) {
		cfg, err := Parse([]byte(`repo: owner/repo
worker_silent_timeout_minutes: 25
stalled_progress_watchdog:
  enabled: false
`))
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}
		if got := cfg.EffectiveWorkerSilentTimeout(); got != 25*time.Minute {
			t.Fatalf("effective legacy timeout = %s, want 25m", got)
		}
	})

	t.Run("programmatic config is mutually excluded at runtime", func(t *testing.T) {
		enabled := true
		cfg := Config{
			WorkerSilentTimeoutMinutes: 25,
			StalledProgressWatchdog:    StalledProgressWatchdogConfig{Enabled: &enabled},
		}
		if got := cfg.EffectiveWorkerSilentTimeout(); got != 0 {
			t.Fatalf("effective legacy timeout = %s while v1 is active, want 0", got)
		}
	})

	for _, tt := range []struct {
		name       string
		stanza     string
		wantLegacy time.Duration
		wantV1     time.Duration
	}{
		{name: "enabled default suppresses legacy", stanza: "  enabled: true\n", wantV1: 20 * time.Minute},
		{name: "enabled negative budget fails closed", stanza: "  enabled: true\n  max_silence_minutes: -1\n"},
		{name: "implicit negative budget suppresses legacy", stanza: "  max_silence_minutes: -1\n"},
		{name: "cadence-only stanza suppresses legacy", stanza: "  eval_interval_seconds: 15\n"},
		{name: "explicit disable is escape hatch", stanza: "  enabled: false\n", wantLegacy: 25 * time.Minute},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := Parse([]byte("repo: owner/repo\nworker_silent_timeout_minutes: 25\nstalled_progress_watchdog:\n" + tt.stanza))
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if got := cfg.EffectiveWorkerSilentTimeout(); got != tt.wantLegacy {
				t.Fatalf("legacy timeout = %s, want %s (raw=%d)", got, tt.wantLegacy, cfg.WorkerSilentTimeoutMinutes)
			}
			if got := cfg.StalledProgressWatchdog.EffectiveMaxSilence(); got != tt.wantV1 {
				t.Fatalf("v1 budget = %s, want %s", got, tt.wantV1)
			}
			if tt.wantLegacy == 0 && cfg.WorkerSilentTimeoutMinutes != 0 {
				t.Fatalf("explicit v1 stanza left raw legacy timeout=%d", cfg.WorkerSilentTimeoutMinutes)
			}
		})
	}

	t.Run("programmatic enabled invalid budget still suppresses legacy", func(t *testing.T) {
		enabled := true
		cfg := Config{
			WorkerSilentTimeoutMinutes: 25,
			StalledProgressWatchdog: StalledProgressWatchdogConfig{
				Enabled: &enabled, MaxSilenceMinutes: -1,
			},
		}
		if got := cfg.EffectiveWorkerSilentTimeout(); got != 0 {
			t.Fatalf("invalid enabled v1 fell back to legacy timeout %s", got)
		}
	})
}
