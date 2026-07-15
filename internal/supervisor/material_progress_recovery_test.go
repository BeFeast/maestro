package supervisor

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/progress"
	"github.com/befeast/maestro/internal/state"
)

func stalledRecoveryConfig(t *testing.T) *config.Config {
	t.Helper()
	enabled := true
	cfg := &config.Config{StateDir: t.TempDir(), MaxRetriesPerIssue: 3}
	cfg.StalledProgressWatchdog.Enabled = &enabled
	cfg.StalledProgressWatchdog.MaxSilenceMinutes = 20
	cfg.StalledProgressWatchdog.EvalIntervalSecs = 60
	return cfg
}

func seedStalledRecovery(t *testing.T, cfg *config.Config, now time.Time, retryCount int) (progress.Target, string) {
	t.Helper()
	started := now.Add(-30 * time.Minute)
	sess := &state.Session{
		IssueNumber:             887,
		IssueTitle:              "stalled watchdog canary",
		Status:                  state.StatusRunning,
		PID:                     4242,
		TmuxSession:             "maestro-slot-1",
		StartedAt:               started,
		RetryCount:              retryCount,
		PreviousAttemptFeedback: "latest review feedback",
		FailingCheckContext:     "latest failing check",
	}
	target := progress.Target{
		Kind:        progress.TargetWorker,
		IssueNumber: sess.IssueNumber,
		Slot:        "slot-1",
		SessionID:   materialProgressSessionID("slot-1", sess),
		TmuxSession: sess.TmuxSession,
		ProcessID:   sess.PID,
	}
	target.LeaseID = "spawn:" + target.SessionID
	observation := progress.Observation{
		Target: target,
		Phase:  progress.PhasePreDelivery,
		Signals: progress.SignalSet{
			{Kind: progress.SignalIssueSession, Fingerprint: progress.Fingerprint("running")},
			{Kind: progress.SignalProcessTmux, Fingerprint: progress.Fingerprint("4242", target.TmuxSession)},
			{Kind: progress.SignalWorktreeGit, Fingerprint: progress.Fingerprint("head-1")},
		},
	}
	st := state.NewState()
	st.Sessions[target.Slot] = sess
	if _, err := st.RecordMaterialProgress([]progress.Observation{observation}, 20*time.Minute, time.Minute, now.Add(-21*time.Minute)); err != nil {
		t.Fatal(err)
	}
	decisions, err := st.RecordMaterialProgress([]progress.Observation{observation}, 20*time.Minute, time.Minute, now)
	if err != nil || len(decisions) != 1 || decisions[0].Action != progress.ActionStopAndRetry {
		t.Fatalf("seed recommendation = %+v err=%v", decisions, err)
	}
	if err := state.Save(cfg.StateDir, st); err != nil {
		t.Fatal(err)
	}
	return target, decisions[0].RecommendationID
}

func TestMaterialProgressRecovery_StopsExactWorkerAndSchedulesExistingRetry(t *testing.T) {
	cfg := stalledRecoveryConfig(t)
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	_, _ = seedStalledRecovery(t, cfg, now, 0)
	var stops atomic.Int32
	runtime := materialProgressRecoveryRuntime{
		inspect: func(progress.Target) exactWorkerLeaseState { return exactWorkerLeaseExact },
		stop: func(progress.Target) exactWorkerLeaseState {
			stops.Add(1)
			return exactWorkerLeaseAbsent
		},
	}
	if err := reconcileMaterialProgressRecoveries(cfg, now, runtime); err != nil {
		t.Fatal(err)
	}
	if err := reconcileMaterialProgressRecoveries(cfg, now.Add(time.Minute), runtime); err != nil {
		t.Fatal(err)
	}
	if got := stops.Load(); got != 1 {
		t.Fatalf("exact stop calls = %d, want 1", got)
	}
	st, err := state.Load(cfg.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	sess := st.Sessions["slot-1"]
	if sess.Status != state.StatusDead || sess.NextRetryAt == nil || sess.RetryCount != 1 {
		t.Fatalf("scheduled retry session = %+v", sess)
	}
	if sess.PID != 0 || sess.TmuxSession != "" || sess.RetryReason != state.RetryReasonStalledProgress {
		t.Fatalf("stopped exact identity was not retired: %+v", sess)
	}
	if sess.PreviousAttemptFeedback != "latest review feedback" || sess.FailingCheckContext != "latest failing check" {
		t.Fatalf("late repair context was lost: %+v", sess)
	}
	recovery := latestMaterialRecovery(t, st)
	if recovery.Outcome != progress.RecoverySucceeded || recovery.Stage != progress.RecoveryStageRetryScheduled ||
		recovery.Reason != progress.RecoveryReasonRetryScheduled {
		t.Fatalf("recovery = %+v", recovery)
	}
}

func TestMaterialProgressRecovery_ConcurrentCyclesClaimOnce(t *testing.T) {
	cfg := stalledRecoveryConfig(t)
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	seedStalledRecovery(t, cfg, now, 0)
	var stops atomic.Int32
	entered := make(chan struct{})
	release := make(chan struct{})
	runtime := materialProgressRecoveryRuntime{
		inspect: func(progress.Target) exactWorkerLeaseState { return exactWorkerLeaseExact },
		stop: func(progress.Target) exactWorkerLeaseState {
			if stops.Add(1) == 1 {
				close(entered)
				<-release
			}
			return exactWorkerLeaseAbsent
		},
	}
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- reconcileMaterialProgressRecoveries(cfg, now, runtime)
		}()
	}
	<-entered
	close(release)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if got := stops.Load(); got != 1 {
		t.Fatalf("concurrent stop calls = %d, want 1", got)
	}
}

func TestMaterialProgressRecovery_ExpiredClaimResumesAfterStop(t *testing.T) {
	cfg := stalledRecoveryConfig(t)
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	target, recommendationID := seedStalledRecovery(t, cfg, now, 0)
	if err := state.Update(cfg.StateDir, func(st *state.State) error {
		claimed, err := st.ClaimMaterialRecovery(target.Key(), recommendationID, "recovery:crashed", time.Minute, now)
		if err != nil || !claimed {
			t.Fatalf("initial claim: claimed=%t err=%v", claimed, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var stops atomic.Int32
	runtime := materialProgressRecoveryRuntime{
		inspect: func(progress.Target) exactWorkerLeaseState { return exactWorkerLeaseAbsent },
		stop: func(progress.Target) exactWorkerLeaseState {
			stops.Add(1)
			return exactWorkerLeaseAbsent
		},
	}
	if err := reconcileMaterialProgressRecoveries(cfg, now.Add(2*time.Minute), runtime); err != nil {
		t.Fatal(err)
	}
	if stops.Load() != 0 {
		t.Fatal("already-absent exact worker was stopped again")
	}
	st, _ := state.Load(cfg.StateDir)
	if st.Sessions[target.Slot].Status != state.StatusDead || st.Sessions[target.Slot].NextRetryAt == nil {
		t.Fatalf("crash reconciliation did not schedule retry: %+v", st.Sessions[target.Slot])
	}
	if recovery := latestMaterialRecovery(t, st); recovery.LeaseGeneration != 2 || recovery.Outcome != progress.RecoverySucceeded {
		t.Fatalf("reconciled recovery = %+v", recovery)
	}
}

func TestMaterialProgressRecovery_ReplacementAndUncertainIdentityAreNeverKilled(t *testing.T) {
	for _, tc := range []struct {
		name        string
		leaseState  exactWorkerLeaseState
		replace     bool
		wantOutcome progress.RecoveryOutcome
		wantReason  progress.RecoveryReason
	}{
		{name: "replacement", leaseState: exactWorkerLeaseReplaced, replace: true, wantOutcome: progress.RecoverySucceeded, wantReason: progress.RecoveryReasonTargetReplaced},
		{name: "uncertain", leaseState: exactWorkerLeaseUncertain, wantOutcome: progress.RecoveryFailed, wantReason: progress.RecoveryReasonIdentityUncertain},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := stalledRecoveryConfig(t)
			now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
			target, _ := seedStalledRecovery(t, cfg, now, 0)
			if tc.replace {
				if err := state.Update(cfg.StateDir, func(st *state.State) error {
					sess := st.Sessions[target.Slot]
					sess.PID = 5252
					sess.StartedAt = now.Add(time.Second)
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			}
			var stops atomic.Int32
			runtime := materialProgressRecoveryRuntime{
				inspect: func(progress.Target) exactWorkerLeaseState { return tc.leaseState },
				stop: func(progress.Target) exactWorkerLeaseState {
					stops.Add(1)
					return exactWorkerLeaseAbsent
				},
			}
			if err := reconcileMaterialProgressRecoveries(cfg, now, runtime); err != nil {
				t.Fatal(err)
			}
			if stops.Load() != 0 {
				t.Fatal("non-exact worker identity was killed")
			}
			st, _ := state.Load(cfg.StateDir)
			recovery := latestMaterialRecovery(t, st)
			if recovery.Outcome != tc.wantOutcome || recovery.Reason != tc.wantReason {
				t.Fatalf("recovery = %+v", recovery)
			}
			if st.Sessions[target.Slot].Status != state.StatusRunning {
				t.Fatalf("replacement/uncertain session mutated: %+v", st.Sessions[target.Slot])
			}
		})
	}
}

func TestMaterialProgressRecovery_RetryExhaustionStopsWithoutRespawn(t *testing.T) {
	cfg := stalledRecoveryConfig(t)
	cfg.MaxRetriesPerIssue = 1
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	seedStalledRecovery(t, cfg, now, 1)
	runtime := materialProgressRecoveryRuntime{
		inspect: func(progress.Target) exactWorkerLeaseState { return exactWorkerLeaseExact },
		stop:    func(progress.Target) exactWorkerLeaseState { return exactWorkerLeaseAbsent },
	}
	if err := reconcileMaterialProgressRecoveries(cfg, now, runtime); err != nil {
		t.Fatal(err)
	}
	st, _ := state.Load(cfg.StateDir)
	sess := st.Sessions["slot-1"]
	if sess.Status != state.StatusRetryExhausted || sess.NextRetryAt != nil || sess.RetryCount != 1 {
		t.Fatalf("retry exhaustion = %+v", sess)
	}
	recovery := latestMaterialRecovery(t, st)
	if recovery.Outcome != progress.RecoveryFailed || recovery.Stage != progress.RecoveryStageRetryExhausted ||
		recovery.Reason != progress.RecoveryReasonRetryExhausted {
		t.Fatalf("retry-exhausted recovery = %+v", recovery)
	}
}

func TestMaterialProgressRecovery_HistoricalRecommendationAfterProgressDoesNotActuate(t *testing.T) {
	cfg := stalledRecoveryConfig(t)
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	target, _ := seedStalledRecovery(t, cfg, now, 0)
	if err := state.Update(cfg.StateDir, func(st *state.State) error {
		record := st.MaterialProgress.Targets[target.Key()]
		advanced := progress.Observation{
			Target: target, Phase: progress.PhasePreDelivery,
			Signals: progress.SignalSet{
				{Kind: progress.SignalIssueSession, Fingerprint: progress.Fingerprint("running")},
				{Kind: progress.SignalProcessTmux, Fingerprint: progress.Fingerprint("4242", target.TmuxSession)},
				{Kind: progress.SignalWorktreeGit, Fingerprint: progress.Fingerprint("head-2")},
			},
		}
		watermark, decision := progress.EvaluateObservation(record.Watermark, advanced, 20*time.Minute, now.Add(time.Minute))
		record.Watermark = watermark
		record.LastDecision = &decision
		if decision.Action != progress.ActionNone {
			t.Fatalf("advanced decision = %+v", decision)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	runtime := materialProgressRecoveryRuntime{
		inspect: func(progress.Target) exactWorkerLeaseState { calls.Add(1); return exactWorkerLeaseExact },
		stop:    func(progress.Target) exactWorkerLeaseState { calls.Add(1); return exactWorkerLeaseAbsent },
	}
	if err := reconcileMaterialProgressRecoveries(cfg, now.Add(time.Minute), runtime); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 0 {
		t.Fatal("historical recommendation actuated after material progress")
	}
	st, _ := state.Load(cfg.StateDir)
	if st.Sessions[target.Slot].Status != state.StatusRunning || latestMaterialRecoveryOrNil(st) != nil {
		t.Fatalf("progressing worker was mutated: session=%+v recovery=%+v", st.Sessions[target.Slot], latestMaterialRecoveryOrNil(st))
	}
}

func TestMaterialProgressRecovery_ExecutingDeliveryNeverInvokesWorkerRuntime(t *testing.T) {
	cfg := stalledRecoveryConfig(t)
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	target := progress.Target{Kind: progress.TargetDelivery, IssueNumber: 887, LeaseID: "approval:g1"}
	observation := progress.Observation{
		Target: target, Phase: progress.PhaseDeliveryExecuting,
		Signals: progress.SignalSet{{Kind: progress.SignalDelivery, Fingerprint: progress.Fingerprint("executing")}},
	}
	st := state.NewState()
	_, _ = st.RecordMaterialProgress([]progress.Observation{observation}, time.Minute, time.Minute, now.Add(-2*time.Minute))
	decisions, _ := st.RecordMaterialProgress([]progress.Observation{observation}, time.Minute, time.Minute, now)
	if len(decisions) != 1 || decisions[0].Action != progress.ActionSurfaceReconciliation || !decisions[0].ReplayBoundary {
		t.Fatalf("delivery decision = %+v", decisions)
	}
	if err := state.Save(cfg.StateDir, st); err != nil {
		t.Fatal(err)
	}
	runtime := materialProgressRecoveryRuntime{
		inspect: func(progress.Target) exactWorkerLeaseState { t.Fatal("delivery invoked worker inspect"); return "" },
		stop:    func(progress.Target) exactWorkerLeaseState { t.Fatal("delivery invoked worker stop"); return "" },
	}
	if err := reconcileMaterialProgressRecoveries(cfg, now, runtime); err != nil {
		t.Fatal(err)
	}
	loaded, _ := state.Load(cfg.StateDir)
	if recovery := latestMaterialRecoveryOrNil(loaded); recovery != nil {
		t.Fatalf("delivery recommendation created an actuator recovery: %+v", recovery)
	}
}

func latestMaterialRecovery(t *testing.T, st *state.State) *progress.Recovery {
	t.Helper()
	recovery := latestMaterialRecoveryOrNil(st)
	if recovery == nil {
		t.Fatal("material recovery is nil")
	}
	return recovery
}

func latestMaterialRecoveryOrNil(st *state.State) *progress.Recovery {
	if st == nil || st.MaterialProgress == nil {
		return nil
	}
	for _, target := range st.MaterialProgress.Targets {
		if recovery := target.LastRecovery(); recovery != nil {
			return recovery
		}
	}
	return nil
}
