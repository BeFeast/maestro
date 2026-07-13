package server

import (
	"testing"
	"time"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/progress"
	"github.com/befeast/maestro/internal/state"
)

// The supervisor pulse must report the orchestrator poll cadence and the
// watchdog evaluation cadence as separate numbers (#887 requirement 1/19), not
// one interval standing in for both.
func TestFleetPulse_SeparateCadences(t *testing.T) {
	cfg := &config.Config{PollIntervalSeconds: 120}
	cfg.StalledProgressWatchdog.EvalIntervalSecs = 15
	now := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)

	pulse := buildFleetSupervisorPulse(cfg, state.NewState(), now)
	if pulse.OrchestratorIntervalSeconds != 120 {
		t.Errorf("OrchestratorIntervalSeconds = %d, want 120", pulse.OrchestratorIntervalSeconds)
	}
	if pulse.WatchdogEvalIntervalSeconds != 15 {
		t.Errorf("WatchdogEvalIntervalSeconds = %d, want 15", pulse.WatchdogEvalIntervalSeconds)
	}
	if pulse.OrchestratorIntervalSeconds == pulse.WatchdogEvalIntervalSeconds {
		t.Errorf("cadences collapsed to one value")
	}
}

// The watchdog view reports the configured silence budget, the last material
// watermark, the derived next deadline, the per-signal progress, and the last
// recovery — separately and truthfully.
func TestFleetPulse_StalledWatchdogView(t *testing.T) {
	cfg := &config.Config{}
	cfg.StalledProgressWatchdog.MaxSilenceMinutes = 20
	now := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)

	st := state.NewState()
	// Record a frozen worker, then push past the deadline so a recovery lands.
	frozen := progress.SignalSet{
		{Kind: progress.SignalProcessTmux, Fingerprint: progress.Fingerprint("pid-1"), ObservedAt: now.Add(-25 * time.Minute)},
		{Kind: progress.SignalWorktreeGit, Fingerprint: progress.Fingerprint("head-1"), ObservedAt: now.Add(-25 * time.Minute)},
	}
	st.RecordMaterialProgress(frozen, progress.PhasePreDelivery, 20*time.Minute, time.Minute, now.Add(-25*time.Minute))
	dec := st.RecordMaterialProgress(frozen, progress.PhasePreDelivery, 20*time.Minute, time.Minute, now)
	if dec.Action != progress.ActionStopAndRetry {
		t.Fatalf("setup: action = %q, want stop_and_retry", dec.Action)
	}

	pulse := buildFleetSupervisorPulse(cfg, st, now)
	w := pulse.StalledProgressWatchdog
	if w == nil {
		t.Fatalf("watchdog view nil")
	}
	if !w.Enabled {
		t.Errorf("watchdog reported disabled, want enabled")
	}
	if w.SilenceBudgetSeconds != 1200 {
		t.Errorf("SilenceBudgetSeconds = %d, want 1200", w.SilenceBudgetSeconds)
	}
	if w.Contract != progress.ContractVersion {
		t.Errorf("Contract = %q, want %q", w.Contract, progress.ContractVersion)
	}
	if w.LastMaterialProgressAt == "" {
		t.Errorf("LastMaterialProgressAt empty")
	}
	if w.NextDeadlineAt == "" {
		t.Errorf("NextDeadlineAt empty")
	}
	if !w.PastDeadline {
		t.Errorf("PastDeadline = false, want true (25m > 20m budget)")
	}
	if len(w.SignalProgress) != 2 {
		t.Fatalf("SignalProgress len = %d, want 2", len(w.SignalProgress))
	}
	if w.LastRecovery == nil || w.LastRecovery.Action != string(progress.ActionStopAndRetry) {
		t.Errorf("LastRecovery = %+v, want stop_and_retry", w.LastRecovery)
	}
}

// A disabled watchdog reports enabled=false and no deadline — never a
// fabricated countdown.
func TestFleetPulse_DisabledWatchdog(t *testing.T) {
	cfg := &config.Config{}
	disabled := false
	cfg.StalledProgressWatchdog.Enabled = &disabled
	now := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)

	pulse := buildFleetSupervisorPulse(cfg, state.NewState(), now)
	w := pulse.StalledProgressWatchdog
	if w == nil {
		t.Fatalf("watchdog view nil")
	}
	if w.Enabled {
		t.Errorf("watchdog reported enabled after explicit disable")
	}
	if w.NextDeadlineAt != "" || w.SilenceBudgetSeconds != 0 {
		t.Errorf("disabled watchdog reported a budget/deadline: %+v", w)
	}
}

// A watchdog disabled in config must report no deadline even when durable state
// still carries a previously-enabled budget and a past watermark; otherwise
// Fleet raises a false overdue alert (enabled=false with past_deadline=true)
// until the next supervisor evaluation rewrites the stored budget (#887 review).
func TestFleetPulse_DisabledWatchdogIgnoresStaleBudget(t *testing.T) {
	cfg := &config.Config{}
	disabled := false
	cfg.StalledProgressWatchdog.Enabled = &disabled
	now := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)

	// Durable state from when the watchdog was enabled: a 20m budget and a
	// watermark 25m old (past the old deadline).
	st := state.NewState()
	frozen := progress.SignalSet{{Kind: progress.SignalProcessTmux, Fingerprint: progress.Fingerprint("pid-1")}}
	st.RecordMaterialProgress(frozen, progress.PhasePreDelivery, 20*time.Minute, time.Minute, now.Add(-25*time.Minute))

	w := buildFleetSupervisorPulse(cfg, st, now).StalledProgressWatchdog
	if w == nil {
		t.Fatalf("watchdog view nil")
	}
	if w.Enabled {
		t.Errorf("watchdog reported enabled after explicit disable")
	}
	if w.SilenceBudgetSeconds != 0 {
		t.Errorf("disabled watchdog restored stale budget: %d", w.SilenceBudgetSeconds)
	}
	if w.NextDeadlineAt != "" || w.NextDeadlineInSeconds != 0 {
		t.Errorf("disabled watchdog reported a deadline: at=%q in=%d", w.NextDeadlineAt, w.NextDeadlineInSeconds)
	}
	if w.PastDeadline {
		t.Errorf("disabled watchdog reported past_deadline=true (false overdue alert)")
	}
}
