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
