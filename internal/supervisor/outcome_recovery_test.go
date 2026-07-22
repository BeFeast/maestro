package supervisor

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/outcome"
	"github.com/befeast/maestro/internal/state"
)

func TestEvaluateOutcomeRecoveryOnce_ConcurrentEvaluatorsClaimOneLease(t *testing.T) {
	cfg := recoveryTestConfig(t)
	t0 := time.Date(2026, 7, 18, 7, 30, 0, 0, time.UTC)
	if err := state.Save(cfg.StateDir, state.NewState()); err != nil {
		t.Fatalf("seed state: %v", err)
	}

	originalCheck, originalRun := checkOutcomeForRecovery, runOutcomeRecovery
	t.Cleanup(func() { checkOutcomeForRecovery, runOutcomeRecovery = originalCheck, originalRun })
	checksReady := make(chan struct{}, 2)
	releaseChecks := make(chan struct{})
	var checkCalls atomic.Int32
	checkOutcomeForRecovery = func(_ context.Context, _ outcome.Brief) outcome.HealthCheckResult {
		call := checkCalls.Add(1)
		if call <= 2 {
			checksReady <- struct{}{}
			<-releaseChecks
			return outcome.HealthCheckResult{CheckedAt: t0, Signal: "healthcheck_command", State: outcome.HealthFailing}
		}
		return outcome.HealthCheckResult{CheckedAt: t0.Add(time.Minute), Signal: "healthcheck_command", State: outcome.HealthHealthy}
	}
	runStarted := make(chan struct{}, 1)
	releaseRun := make(chan struct{})
	var runCalls atomic.Int32
	runOutcomeRecovery = func(_ context.Context, _ outcome.Brief) outcome.RecoveryExecution {
		runCalls.Add(1)
		runStarted <- struct{}{}
		<-releaseRun
		return outcome.RecoveryExecution{StartedAt: t0, FinishedAt: t0.Add(time.Second), ExitCode: 0}
	}

	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := EvaluateOutcomeRecoveryOnce(cfg, t0)
			errs <- err
		}()
	}
	<-checksReady
	<-checksReady
	close(releaseChecks)
	<-runStarted
	close(releaseRun)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent evaluator: %v", err)
		}
	}
	if got := runCalls.Load(); got != 1 {
		t.Fatalf("recovery command ran %d times, want one atomic lease winner", got)
	}
	loaded, err := state.Load(cfg.StateDir)
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	if loaded.OutcomeRecovery == nil || loaded.OutcomeRecovery.Attempts != 1 {
		t.Fatalf("recovery receipt = %+v, want exactly one attempt", loaded.OutcomeRecovery)
	}
}

func TestEvaluateOutcomeRecoveryOnce_NonFailingHealthNeverRunsCommand(t *testing.T) {
	cfg := recoveryTestConfig(t)
	t0 := time.Date(2026, 7, 18, 7, 45, 0, 0, time.UTC)
	if err := state.Save(cfg.StateDir, state.NewState()); err != nil {
		t.Fatalf("seed state: %v", err)
	}

	originalCheck, originalRun := checkOutcomeForRecovery, runOutcomeRecovery
	t.Cleanup(func() { checkOutcomeForRecovery, runOutcomeRecovery = originalCheck, originalRun })
	checkOutcomeForRecovery = func(_ context.Context, _ outcome.Brief) outcome.HealthCheckResult {
		return outcome.HealthCheckResult{CheckedAt: t0, Signal: "healthcheck_command", State: outcome.HealthUnknown}
	}
	runCalls := 0
	runOutcomeRecovery = func(_ context.Context, _ outcome.Brief) outcome.RecoveryExecution {
		runCalls++
		return outcome.RecoveryExecution{}
	}
	if evaluated, err := EvaluateOutcomeRecoveryOnce(cfg, t0); err != nil || !evaluated {
		t.Fatalf("evaluation: evaluated=%t err=%v", evaluated, err)
	}
	if runCalls != 0 {
		t.Fatalf("unknown health launched recovery %d time(s)", runCalls)
	}
}

func TestEvaluateOutcomeRecoveryOnce_PendingPersistsWithoutRecoveryCooldown(t *testing.T) {
	cfg := recoveryTestConfig(t)
	t0 := time.Date(2026, 7, 18, 12, 15, 56, 0, time.UTC)
	if err := state.Save(cfg.StateDir, state.NewState()); err != nil {
		t.Fatalf("seed state: %v", err)
	}

	originalCheck, originalRun := checkOutcomeForRecovery, runOutcomeRecovery
	t.Cleanup(func() { checkOutcomeForRecovery, runOutcomeRecovery = originalCheck, originalRun })
	checkOutcomeForRecovery = func(_ context.Context, _ outcome.Brief) outcome.HealthCheckResult {
		return outcome.HealthCheckResult{
			CheckedAt: t0,
			Signal:    "healthcheck_command",
			State:     outcome.HealthPending,
			Summary:   "source-main-ci reported in_progress",
		}
	}
	runCalls := 0
	runOutcomeRecovery = func(_ context.Context, _ outcome.Brief) outcome.RecoveryExecution {
		runCalls++
		return outcome.RecoveryExecution{}
	}

	if evaluated, err := EvaluateOutcomeRecoveryOnce(cfg, t0); err != nil || !evaluated {
		t.Fatalf("evaluation: evaluated=%t err=%v", evaluated, err)
	}
	loaded, err := state.Load(cfg.StateDir)
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	if runCalls != 0 {
		t.Fatalf("pending health launched recovery %d time(s)", runCalls)
	}
	if loaded.OutcomeHealth == nil || loaded.OutcomeHealth.State != outcome.HealthPending {
		t.Fatalf("stored outcome health = %+v, want pending", loaded.OutcomeHealth)
	}
	if loaded.OutcomeRecovery != nil {
		t.Fatalf("pending health entered recovery/cooldown: %+v", loaded.OutcomeRecovery)
	}
}

func TestEvaluateOutcomeRecoveryOnce_LeasesRunsOnceAndVerifies(t *testing.T) {
	cfg := recoveryTestConfig(t)
	t0 := time.Date(2026, 7, 18, 8, 0, 0, 0, time.UTC)
	seed := state.NewState()
	if err := state.Save(cfg.StateDir, seed); err != nil {
		t.Fatalf("seed state: %v", err)
	}

	originalCheck, originalRun := checkOutcomeForRecovery, runOutcomeRecovery
	t.Cleanup(func() { checkOutcomeForRecovery, runOutcomeRecovery = originalCheck, originalRun })
	checkCalls := 0
	checkOutcomeForRecovery = func(_ context.Context, _ outcome.Brief) outcome.HealthCheckResult {
		checkCalls++
		at := t0.Add(time.Duration(checkCalls-1) * 61 * time.Second)
		stateValue := outcome.HealthFailing
		if checkCalls == 4 {
			stateValue = outcome.HealthHealthy
		}
		return outcome.HealthCheckResult{CheckedAt: at, Signal: "healthcheck_command", State: stateValue, Summary: stateValue}
	}
	runCalls := 0
	runOutcomeRecovery = func(_ context.Context, _ outcome.Brief) outcome.RecoveryExecution {
		runCalls++
		return outcome.RecoveryExecution{StartedAt: t0, FinishedAt: t0.Add(time.Second), ExitCode: 0, DurationMillis: 1000}
	}

	if evaluated, err := EvaluateOutcomeRecoveryOnce(cfg, t0); err != nil || !evaluated {
		t.Fatalf("first evaluation: evaluated=%t err=%v", evaluated, err)
	}
	loaded, err := state.Load(cfg.StateDir)
	if err != nil {
		t.Fatalf("load first receipt: %v", err)
	}
	if runCalls != 1 || loaded.OutcomeRecovery == nil || loaded.OutcomeRecovery.Status != outcome.RecoveryStatusVerificationPending {
		t.Fatalf("first receipt=%+v runCalls=%d", loaded.OutcomeRecovery, runCalls)
	}
	if loaded.OutcomeRecovery.Attempts != 1 || loaded.OutcomeRecovery.AttemptID == "" || loaded.OutcomeRecovery.ExitCode == nil || *loaded.OutcomeRecovery.ExitCode != 0 {
		t.Fatalf("incomplete durable receipt: %+v", loaded.OutcomeRecovery)
	}

	if evaluated, err := EvaluateOutcomeRecoveryOnce(cfg, t0.Add(123*time.Second)); err != nil || !evaluated {
		t.Fatalf("cooldown evaluation: evaluated=%t err=%v", evaluated, err)
	}
	if runCalls != 1 {
		t.Fatalf("known failed health replayed recovery inside cooldown: %d", runCalls)
	}

	if evaluated, err := EvaluateOutcomeRecoveryOnce(cfg, t0.Add(245*time.Second)); err != nil || !evaluated {
		t.Fatalf("verification evaluation: evaluated=%t err=%v", evaluated, err)
	}
	loaded, err = state.Load(cfg.StateDir)
	if err != nil {
		t.Fatalf("load verified receipt: %v", err)
	}
	if loaded.OutcomeRecovery.Status != outcome.RecoveryStatusVerified || loaded.OutcomeRecovery.VerifiedAt.IsZero() {
		t.Fatalf("verified receipt=%+v", loaded.OutcomeRecovery)
	}
	if runCalls != 1 {
		t.Fatalf("healthy verification replayed recovery: %d", runCalls)
	}
}

func TestEvaluateOutcomeRecoveryOnce_ExecutingLeaseNeverReplays(t *testing.T) {
	cfg := recoveryTestConfig(t)
	t0 := time.Date(2026, 7, 18, 9, 0, 0, 0, time.UTC)
	st := state.NewState()
	st.OutcomeRecovery = &outcome.RecoveryState{
		AttemptID: "outcome-existing", Status: outcome.RecoveryStatusExecuting,
		Attempts: 1, StartedAt: t0.Add(-30 * time.Second), UpdatedAt: t0.Add(-30 * time.Second),
	}
	if err := state.Save(cfg.StateDir, st); err != nil {
		t.Fatalf("seed state: %v", err)
	}

	originalCheck, originalRun := checkOutcomeForRecovery, runOutcomeRecovery
	t.Cleanup(func() { checkOutcomeForRecovery, runOutcomeRecovery = originalCheck, originalRun })
	checkOutcomeForRecovery = func(_ context.Context, _ outcome.Brief) outcome.HealthCheckResult {
		return outcome.HealthCheckResult{CheckedAt: t0, Signal: "healthcheck_command", State: outcome.HealthFailing}
	}
	runCalls := 0
	runOutcomeRecovery = func(_ context.Context, _ outcome.Brief) outcome.RecoveryExecution {
		runCalls++
		return outcome.RecoveryExecution{}
	}

	if _, err := EvaluateOutcomeRecoveryOnce(cfg, t0); err != nil {
		t.Fatalf("evaluate existing lease: %v", err)
	}
	if runCalls != 0 {
		t.Fatalf("existing executing lease replayed command %d time(s)", runCalls)
	}

	checkOutcomeForRecovery = func(_ context.Context, _ outcome.Brief) outcome.HealthCheckResult {
		return outcome.HealthCheckResult{CheckedAt: t0.Add(4 * time.Minute), Signal: "healthcheck_command", State: outcome.HealthFailing}
	}
	if _, err := EvaluateOutcomeRecoveryOnce(cfg, t0.Add(4*time.Minute)); err != nil {
		t.Fatalf("evaluate stale lease: %v", err)
	}
	loaded, _ := state.Load(cfg.StateDir)
	if loaded.OutcomeRecovery.Status != outcome.RecoveryStatusUncertain || runCalls != 0 {
		t.Fatalf("stale lease=%+v runCalls=%d, want uncertain/no replay", loaded.OutcomeRecovery, runCalls)
	}

	checkOutcomeForRecovery = func(_ context.Context, _ outcome.Brief) outcome.HealthCheckResult {
		return outcome.HealthCheckResult{CheckedAt: t0.Add(8 * time.Minute), Signal: "healthcheck_command", State: outcome.HealthFailing}
	}
	if _, err := EvaluateOutcomeRecoveryOnce(cfg, t0.Add(8*time.Minute)); err != nil {
		t.Fatalf("evaluate unchanged uncertain failure: %v", err)
	}
	loaded, _ = state.Load(cfg.StateDir)
	if loaded.OutcomeRecovery.Status != outcome.RecoveryStatusUncertain || runCalls != 0 {
		t.Fatalf("uncertain lease replayed on a later cycle: receipt=%+v runCalls=%d", loaded.OutcomeRecovery, runCalls)
	}

	checkOutcomeForRecovery = func(_ context.Context, _ outcome.Brief) outcome.HealthCheckResult {
		return outcome.HealthCheckResult{CheckedAt: t0.Add(10 * time.Minute), Signal: "healthcheck_command", State: outcome.HealthHealthy}
	}
	if _, err := EvaluateOutcomeRecoveryOnce(cfg, t0.Add(10*time.Minute)); err != nil {
		t.Fatalf("evaluate authoritative healthy result: %v", err)
	}
	loaded, _ = state.Load(cfg.StateDir)
	if loaded.OutcomeRecovery.Status != outcome.RecoveryStatusVerified || runCalls != 0 {
		t.Fatalf("healthy check did not clear uncertain fence: receipt=%+v runCalls=%d", loaded.OutcomeRecovery, runCalls)
	}

	checkOutcomeForRecovery = func(_ context.Context, _ outcome.Brief) outcome.HealthCheckResult {
		return outcome.HealthCheckResult{CheckedAt: t0.Add(12 * time.Minute), Signal: "healthcheck_command", State: outcome.HealthFailing}
	}
	runOutcomeRecovery = func(_ context.Context, _ outcome.Brief) outcome.RecoveryExecution {
		runCalls++
		return outcome.RecoveryExecution{StartedAt: t0.Add(12 * time.Minute), FinishedAt: t0.Add(12*time.Minute + time.Second), ExitCode: 7}
	}
	if _, err := EvaluateOutcomeRecoveryOnce(cfg, t0.Add(12*time.Minute)); err != nil {
		t.Fatalf("evaluate new post-healthy failure: %v", err)
	}
	if runCalls != 1 {
		t.Fatalf("verified receipt blocked a later failure: runCalls=%d, want 1", runCalls)
	}
}

func TestEvaluateOutcomeRecoveryOnce_FailedCommandHonorsCooldown(t *testing.T) {
	cfg := recoveryTestConfig(t)
	t0 := time.Date(2026, 7, 18, 10, 0, 0, 0, time.UTC)
	if err := state.Save(cfg.StateDir, state.NewState()); err != nil {
		t.Fatalf("seed state: %v", err)
	}

	originalCheck, originalRun := checkOutcomeForRecovery, runOutcomeRecovery
	t.Cleanup(func() { checkOutcomeForRecovery, runOutcomeRecovery = originalCheck, originalRun })
	checkOutcomeForRecovery = func(_ context.Context, _ outcome.Brief) outcome.HealthCheckResult {
		return outcome.HealthCheckResult{CheckedAt: t0, Signal: "healthcheck_command", State: outcome.HealthFailing}
	}
	runCalls := 0
	runOutcomeRecovery = func(_ context.Context, _ outcome.Brief) outcome.RecoveryExecution {
		runCalls++
		return outcome.RecoveryExecution{StartedAt: t0, FinishedAt: t0.Add(time.Second), ExitCode: 7}
	}
	if _, err := EvaluateOutcomeRecoveryOnce(cfg, t0); err != nil {
		t.Fatalf("failed command evaluation: %v", err)
	}
	loaded, _ := state.Load(cfg.StateDir)
	if loaded.OutcomeRecovery.Status != outcome.RecoveryStatusFailed || loaded.OutcomeRecovery.ExitCode == nil || *loaded.OutcomeRecovery.ExitCode != 7 {
		t.Fatalf("failed receipt=%+v", loaded.OutcomeRecovery)
	}
	if !loaded.OutcomeRecovery.NextEligibleAt.Equal(t0.Add(time.Second).Add(20 * time.Minute)) {
		t.Fatalf("next eligible=%s", loaded.OutcomeRecovery.NextEligibleAt)
	}
	if runCalls != 1 {
		t.Fatalf("runCalls=%d, want 1", runCalls)
	}
	if loaded.OutcomeRecovery.ConsecutiveFailures != 1 {
		t.Fatalf("post-attempt failing verification count = %d, want 1", loaded.OutcomeRecovery.ConsecutiveFailures)
	}
}

func TestEvaluateOutcomeRecoveryOnce_NonZeroCommandStillUsesPostAttemptHealth(t *testing.T) {
	cfg := recoveryTestConfig(t)
	cfg.Outcome.RecoveryMaxFutileAttempts = 1
	t0 := time.Date(2026, 7, 18, 11, 0, 0, 0, time.UTC)
	if err := state.Save(cfg.StateDir, state.NewState()); err != nil {
		t.Fatalf("seed state: %v", err)
	}

	originalCheck, originalRun := checkOutcomeForRecovery, runOutcomeRecovery
	t.Cleanup(func() { checkOutcomeForRecovery, runOutcomeRecovery = originalCheck, originalRun })
	checkCalls := 0
	checkOutcomeForRecovery = func(_ context.Context, _ outcome.Brief) outcome.HealthCheckResult {
		checkCalls++
		if checkCalls == 1 {
			return outcome.HealthCheckResult{
				CheckedAt: t0,
				Signal:    "healthcheck_command",
				State:     outcome.HealthFailing,
				Checks:    []outcome.HealthCheckItem{{Name: "candidate-delivery", Blocking: true, Status: "fail"}},
			}
		}
		return outcome.HealthCheckResult{
			CheckedAt: t0.Add(2 * time.Second),
			Signal:    "healthcheck_command",
			State:     outcome.HealthHealthy,
			Checks:    []outcome.HealthCheckItem{{Name: "candidate-delivery", Blocking: true, Status: "pass"}},
		}
	}
	runOutcomeRecovery = func(_ context.Context, _ outcome.Brief) outcome.RecoveryExecution {
		return outcome.RecoveryExecution{StartedAt: t0, FinishedAt: t0.Add(time.Second), ExitCode: 7}
	}

	if _, err := EvaluateOutcomeRecoveryOnce(cfg, t0); err != nil {
		t.Fatalf("evaluate recovery: %v", err)
	}
	loaded, err := state.Load(cfg.StateDir)
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	if checkCalls != 2 {
		t.Fatalf("health checks = %d, want trigger + post-attempt verification", checkCalls)
	}
	if loaded.OutcomeRecovery == nil || loaded.OutcomeRecovery.Status != outcome.RecoveryStatusVerified {
		t.Fatalf("non-zero command ignored healthy post-attempt verification: %+v", loaded.OutcomeRecovery)
	}
	if loaded.OutcomeRecovery.ConsecutiveFailures != 0 || !loaded.OutcomeRecovery.CappedAt.IsZero() {
		t.Fatalf("healthy post-attempt verification counted as futile: %+v", loaded.OutcomeRecovery)
	}
}

func TestEvaluateOutcomeRecoveryOnce_BoundsFutileAttemptsAndStopsLeasing(t *testing.T) {
	cfg := recoveryTestConfig(t)
	cfg.Outcome.RecoveryMaxFutileAttempts = 3
	t0 := time.Date(2026, 7, 19, 6, 0, 0, 0, time.UTC)
	if err := state.Save(cfg.StateDir, state.NewState()); err != nil {
		t.Fatalf("seed state: %v", err)
	}

	originalCheck, originalRun := checkOutcomeForRecovery, runOutcomeRecovery
	t.Cleanup(func() { checkOutcomeForRecovery, runOutcomeRecovery = originalCheck, originalRun })
	checkCalls := 0
	checkOutcomeForRecovery = func(_ context.Context, _ outcome.Brief) outcome.HealthCheckResult {
		checkCalls++
		return outcome.HealthCheckResult{
			CheckedAt: t0.Add(time.Duration(checkCalls) * time.Minute),
			Signal:    "healthcheck_command",
			State:     outcome.HealthFailing,
			Summary:   "healthcheck_command: linux-candidate-delivery reported fail",
			Checks: []outcome.HealthCheckItem{{
				Name: "linux-candidate-delivery", Blocking: true, Status: "fail",
			}},
		}
	}
	runCalls := 0
	runOutcomeRecovery = func(_ context.Context, _ outcome.Brief) outcome.RecoveryExecution {
		runCalls++
		finished := t0.Add(time.Duration(runCalls) * time.Minute)
		return outcome.RecoveryExecution{StartedAt: finished.Add(-time.Second), FinishedAt: finished, ExitCode: 0}
	}

	for cycle := 0; cycle < 5; cycle++ {
		if _, err := EvaluateOutcomeRecoveryOnce(cfg, t0.Add(time.Duration(cycle)*25*time.Minute)); err != nil {
			t.Fatalf("cycle %d: %v", cycle, err)
		}
	}
	if runCalls != 3 {
		t.Fatalf("recovery command ran %d times, want cap K=3", runCalls)
	}
	loaded, err := state.Load(cfg.StateDir)
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	recovery := loaded.OutcomeRecovery
	if recovery == nil || recovery.Status != outcome.RecoveryStatusCapped {
		t.Fatalf("recovery = %+v, want capped", recovery)
	}
	if recovery.ConsecutiveFailures != 3 || recovery.FailureFingerprint == "" || recovery.CappedAt.IsZero() {
		t.Fatalf("cap-hit state incomplete: %+v", recovery)
	}
}

func TestEvaluateOutcomeRecoveryOnce_ChangedFailureFingerprintRearmsImmediately(t *testing.T) {
	cfg := recoveryTestConfig(t)
	cfg.Outcome.RecoveryMaxFutileAttempts = 2
	t0 := time.Date(2026, 7, 19, 9, 0, 0, 0, time.UTC)
	if err := state.Save(cfg.StateDir, state.NewState()); err != nil {
		t.Fatalf("seed state: %v", err)
	}

	originalCheck, originalRun := checkOutcomeForRecovery, runOutcomeRecovery
	t.Cleanup(func() { checkOutcomeForRecovery, runOutcomeRecovery = originalCheck, originalRun })
	gate := "gate-a"
	checkCalls := 0
	checkOutcomeForRecovery = func(_ context.Context, _ outcome.Brief) outcome.HealthCheckResult {
		checkCalls++
		return outcome.HealthCheckResult{
			CheckedAt: t0.Add(time.Duration(checkCalls) * time.Minute),
			Signal:    "healthcheck_command",
			State:     outcome.HealthFailing,
			Checks:    []outcome.HealthCheckItem{{Name: gate, Blocking: true, Status: "fail"}},
		}
	}
	runCalls := 0
	runOutcomeRecovery = func(_ context.Context, _ outcome.Brief) outcome.RecoveryExecution {
		runCalls++
		finished := t0.Add(time.Duration(runCalls) * time.Minute)
		return outcome.RecoveryExecution{FinishedAt: finished, ExitCode: 0}
	}

	for cycle := 0; cycle < 3; cycle++ {
		if _, err := EvaluateOutcomeRecoveryOnce(cfg, t0.Add(time.Duration(cycle)*25*time.Minute)); err != nil {
			t.Fatalf("gate-a cycle %d: %v", cycle, err)
		}
	}
	if runCalls != 2 {
		t.Fatalf("gate-a recovery ran %d times, want 2", runCalls)
	}

	gate = "gate-b"
	if _, err := EvaluateOutcomeRecoveryOnce(cfg, t0.Add(75*time.Minute)); err != nil {
		t.Fatalf("gate-b cycle: %v", err)
	}
	if runCalls != 3 {
		t.Fatalf("changed fingerprint did not re-arm immediately: runCalls=%d", runCalls)
	}
	loaded, err := state.Load(cfg.StateDir)
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	if loaded.OutcomeRecovery.Status == outcome.RecoveryStatusCapped || loaded.OutcomeRecovery.ConsecutiveFailures != 1 {
		t.Fatalf("changed fingerprint did not start a fresh streak: %+v", loaded.OutcomeRecovery)
	}
}

func recoveryTestConfig(t *testing.T) *config.Config {
	t.Helper()
	return &config.Config{
		StateDir: t.TempDir(),
		Outcome: outcome.Brief{
			DesiredOutcome:          "candidate feed follows main",
			HealthcheckCommand:      "./check-outcome.sh",
			RecoveryMode:            outcome.RecoveryModeAutomatic,
			RecoveryCommand:         "./recover-outcome.sh",
			RecoveryIntervalSeconds: 60,
			RecoveryCooldownMinutes: 20,
			RecoveryTimeoutSeconds:  120,
		},
	}
}
