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
	cfg := &config.Config{PollIntervalSeconds: 120, RuntimeSuperviseIntervalSeconds: 300}
	enabled := true
	cfg.StalledProgressWatchdog.Enabled = &enabled
	cfg.StalledProgressWatchdog.EvalIntervalSecs = 15
	now := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)

	pulse := buildFleetSupervisorPulse(cfg, state.NewState(), now)
	if pulse.OrchestratorIntervalSeconds != 120 {
		t.Errorf("OrchestratorIntervalSeconds = %d, want 120", pulse.OrchestratorIntervalSeconds)
	}
	if pulse.WatchdogEvalIntervalSeconds != 15 {
		t.Errorf("WatchdogEvalIntervalSeconds = %d, want 15", pulse.WatchdogEvalIntervalSeconds)
	}
	if pulse.SupervisorIntervalSeconds != 300 {
		t.Errorf("SupervisorIntervalSeconds = %d, want actual daemon cadence 300", pulse.SupervisorIntervalSeconds)
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
	enabled := true
	cfg.StalledProgressWatchdog.Enabled = &enabled
	cfg.StalledProgressWatchdog.MaxSilenceMinutes = 20
	now := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)

	st := state.NewState()
	// Record a frozen worker, then push past the deadline so a recovery lands.
	frozen := progress.SignalSet{
		{Kind: progress.SignalProcessTmux, Fingerprint: progress.Fingerprint("pid-1"), ObservedAt: now.Add(-25 * time.Minute)},
		{Kind: progress.SignalWorktreeGit, Fingerprint: progress.Fingerprint("head-1"), ObservedAt: now.Add(-25 * time.Minute)},
	}
	target := progress.Target{Kind: progress.TargetWorker, IssueNumber: 1, Slot: "slot-1", SessionID: "spawn-1", ProcessID: 1, LeaseID: "lease-1"}
	observations := []progress.Observation{{Target: target, Signals: frozen, Phase: progress.PhasePreDelivery}}
	if _, err := st.RecordMaterialProgress(observations, 20*time.Minute, time.Minute, now.Add(-25*time.Minute)); err != nil {
		t.Fatalf("record baseline: %v", err)
	}
	decisions, err := st.RecordMaterialProgress(observations, 20*time.Minute, time.Minute, now)
	if err != nil || len(decisions) != 1 {
		t.Fatalf("record overdue: decisions=%d err=%v", len(decisions), err)
	}
	dec := decisions[0]
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
	if w.EvaluationIntervalSeconds != 60 {
		t.Errorf("EvaluationIntervalSeconds = %d, want 60", w.EvaluationIntervalSeconds)
	}
	if w.Mode != progress.ContractVersion {
		t.Errorf("Mode = %q, want %q", w.Mode, progress.ContractVersion)
	}
	if w.Contract != "" {
		t.Errorf("Contract = %q after recording-only evaluation; runtime-live capability must stay unclaimed", w.Contract)
	}
	if !w.ContractPending {
		t.Errorf("ContractPending = false before actuator/live-canary proof")
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
	if w.LastRecommendation == nil || w.LastRecommendation.Action != string(progress.ActionStopAndRetry) {
		t.Errorf("LastRecommendation = %+v, want stop_and_retry", w.LastRecommendation)
	}
	if w.LastRecovery != nil {
		t.Errorf("LastRecovery = %+v after evaluation only; recommendation is not actuation", w.LastRecovery)
	}
	if err := st.RecordMaterialRecovery(target.Key(), dec.RecommendationID, progress.RecoveryAttempted, now.Add(time.Second)); err != nil {
		t.Fatalf("record actual recovery: %v", err)
	}
	w = buildFleetSupervisorPulse(cfg, st, now.Add(time.Second)).StalledProgressWatchdog
	if w.LastRecovery == nil || w.LastRecovery.Outcome != string(progress.RecoveryAttempted) {
		t.Errorf("LastRecovery = %+v, want actual attempted recovery", w.LastRecovery)
	}
	if w.LastRecovery.Stage != string(progress.RecoveryStageClaimed) {
		t.Errorf("LastRecovery.Stage = %q, want claimed", w.LastRecovery.Stage)
	}
}

func TestFleetPulse_ReportsRecoveryStageAndBoundedReason(t *testing.T) {
	enabled := true
	cfg := &config.Config{}
	cfg.StalledProgressWatchdog.Enabled = &enabled
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	st := state.NewState()
	target := progress.Target{Kind: progress.TargetWorker, IssueNumber: 1, Slot: "slot-1", SessionID: "spawn-1", TmuxSession: "tmux-1", ProcessID: 101, LeaseID: "lease-1"}
	observation := progress.Observation{
		Target: target, Phase: progress.PhasePreDelivery,
		Signals: progress.SignalSet{{Kind: progress.SignalProcessTmux, Fingerprint: progress.Fingerprint("pid-1")}},
	}
	_, _ = st.RecordMaterialProgress([]progress.Observation{observation}, time.Minute, time.Minute, now.Add(-2*time.Minute))
	decisions, _ := st.RecordMaterialProgress([]progress.Observation{observation}, time.Minute, time.Minute, now)
	claimed, err := st.ClaimMaterialRecovery(target.Key(), decisions[0].RecommendationID, "opaque-lease", time.Minute, now)
	if err != nil || !claimed {
		t.Fatalf("claim: claimed=%t err=%v", claimed, err)
	}
	if err := st.CompleteMaterialRecovery(target.Key(), decisions[0].RecommendationID, "opaque-lease", progress.RecoverySucceeded,
		progress.RecoveryStageRetryScheduled, progress.RecoveryReasonRetryScheduled, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	recovery := buildFleetSupervisorPulse(cfg, st, now.Add(time.Second)).StalledProgressWatchdog.LastRecovery
	if recovery == nil || recovery.Stage != "retry_scheduled" || recovery.Reason != "retry_scheduled" || recovery.LeaseGeneration != 1 {
		t.Fatalf("Fleet recovery = %+v", recovery)
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
	if w.EvaluationIntervalSeconds != 0 || pulse.WatchdogEvalIntervalSeconds != 0 {
		t.Errorf("disabled watchdog reported a stale cadence: watchdog=%d pulse=%d", w.EvaluationIntervalSeconds, pulse.WatchdogEvalIntervalSeconds)
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
	target := progress.Target{Kind: progress.TargetWorker, IssueNumber: 1, Slot: "slot-1", SessionID: "spawn-1", ProcessID: 1, LeaseID: "lease-1"}
	if _, err := st.RecordMaterialProgress([]progress.Observation{{Target: target, Signals: frozen, Phase: progress.PhasePreDelivery}}, 20*time.Minute, time.Minute, now.Add(-25*time.Minute)); err != nil {
		t.Fatalf("record stale state: %v", err)
	}

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
	if w.EvaluationIntervalSeconds != 0 {
		t.Errorf("disabled watchdog restored stale evaluation cadence: %d", w.EvaluationIntervalSeconds)
	}
}

// Fleet reports the cadence the independent evaluator last consumed, not a
// just-edited config value that the runtime loop has not scheduled yet.
func TestFleetPulse_ReportsPersistedRuntimeCadence(t *testing.T) {
	cfg := &config.Config{}
	enabled := true
	cfg.StalledProgressWatchdog.Enabled = &enabled
	cfg.StalledProgressWatchdog.MaxSilenceMinutes = 20
	cfg.StalledProgressWatchdog.EvalIntervalSecs = 15
	now := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
	st := state.NewState()
	if _, err := st.RecordMaterialProgress(nil, 20*time.Minute, 7*time.Second, now); err != nil {
		t.Fatalf("RecordMaterialProgress: %v", err)
	}

	pulse := buildFleetSupervisorPulse(cfg, st, now)
	if got := pulse.WatchdogEvalIntervalSeconds; got != 7 {
		t.Fatalf("WatchdogEvalIntervalSeconds = %d, want persisted runtime cadence 7", got)
	}
	if got := pulse.StalledProgressWatchdog.EvaluationIntervalSeconds; got != 7 {
		t.Fatalf("watchdog evaluation_interval_seconds = %d, want 7", got)
	}
}

func TestFleetPulse_KeepsContractPendingWithoutDurableCanaryProof(t *testing.T) {
	enabled := true
	cfg := &config.Config{}
	cfg.StalledProgressWatchdog.Enabled = &enabled

	w := buildFleetSupervisorPulse(cfg, state.NewState(), time.Now().UTC()).StalledProgressWatchdog
	if w.Contract != "" || !w.ContractPending {
		t.Fatalf("recording-only contract state = contract:%q pending:%t, want empty/pending", w.Contract, w.ContractPending)
	}
}

func TestFleetPulse_ExposesIncompleteEvidenceAndSuppressedRecovery(t *testing.T) {
	enabled := true
	cfg := &config.Config{}
	cfg.StalledProgressWatchdog.Enabled = &enabled
	now := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
	st := state.NewState()
	target := progress.Target{Kind: progress.TargetWorker, IssueNumber: 1, Slot: "slot-1", SessionID: "spawn-1", ProcessID: 1, LeaseID: "lease-1"}
	complete := progress.Observation{
		Target: target, Phase: progress.PhasePreDelivery,
		Signals: progress.SignalSet{{Kind: progress.SignalProcessTmux, Fingerprint: progress.Fingerprint("pid-1")}},
	}
	if _, err := st.RecordMaterialProgress([]progress.Observation{complete}, 20*time.Minute, time.Minute, now.Add(-25*time.Minute)); err != nil {
		t.Fatal(err)
	}
	incomplete := complete
	incomplete.Incomplete = true
	incomplete.UnavailableSignals = []progress.SignalKind{progress.SignalWorktreeGit, progress.SignalTerminalCheckpoint}
	decisions, err := st.RecordMaterialProgress([]progress.Observation{incomplete}, 20*time.Minute, time.Minute, now)
	if err != nil || len(decisions) != 1 || decisions[0].Action != progress.ActionEvidenceUnavailable {
		t.Fatalf("incomplete decision = %+v err=%v", decisions, err)
	}

	w := buildFleetSupervisorPulse(cfg, st, now).StalledProgressWatchdog
	if w == nil || !w.ObservationIncomplete || len(w.UnavailableSignals) != 2 {
		t.Fatalf("watchdog incomplete evidence = %+v", w)
	}
	if w.LastDecision == nil || w.LastDecision.Action != string(progress.ActionEvidenceUnavailable) || !w.LastDecision.ObservationIncomplete {
		t.Fatalf("last decision did not expose suppression: %+v", w.LastDecision)
	}
	if w.LastRecommendation != nil || w.LastRecovery != nil {
		t.Fatalf("incomplete evidence invented recommendation/recovery: recommendation=%+v recovery=%+v", w.LastRecommendation, w.LastRecovery)
	}
	if len(w.Targets) != 1 || !w.Targets[0].ObservationIncomplete || len(w.Targets[0].UnavailableSignals) != 2 {
		t.Fatalf("exact target incomplete evidence = %+v", w.Targets)
	}
}
