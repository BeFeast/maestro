package supervisor

import (
	"context"
	"testing"
	"time"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/outcome"
	"github.com/befeast/maestro/internal/state"
)

func streakTestConfig(t *testing.T) *config.Config {
	t.Helper()
	return &config.Config{
		Repo:     "BeFeast/example",
		StateDir: t.TempDir(),
		Outcome: outcome.Brief{
			DesiredOutcome:          "candidate feed follows main",
			HealthcheckCommand:      "./check-outcome.sh",
			HealthcheckURL:          "https://runtime/health",
			RecoveryIntervalSeconds: 60,
		},
	}
}

type streakNotification struct {
	gate  string
	count int
	link  string
}

// installStreakDispatchStubs replaces the notify/intake side effects with
// in-memory recorders and returns pointers to the captured events.
func installStreakDispatchStubs(t *testing.T) (*[]streakNotification, *[]string, *int) {
	t.Helper()
	originalNotify, originalIntake := gateFailStreakNotify, gateFailStreakIntake
	t.Cleanup(func() { gateFailStreakNotify, gateFailStreakIntake = originalNotify, originalIntake })

	notifications := &[]streakNotification{}
	intakeFingerprints := &[]string{}
	issueSeq := new(int)
	*issueSeq = 900

	gateFailStreakNotify = func(_ *config.Config, event outcome.GateStreakEvent) error {
		*notifications = append(*notifications, streakNotification{gate: event.Gate, count: event.ConsecutiveFailures, link: event.RunLink})
		return nil
	}
	gateFailStreakIntake = func(_ *config.Config, event outcome.GateStreakEvent) (int, error) {
		*intakeFingerprints = append(*intakeFingerprints, event.Fingerprint)
		*issueSeq++
		return *issueSeq, nil
	}
	return notifications, intakeFingerprints, issueSeq
}

func stubScheduledCheck(t *testing.T, result *outcome.HealthCheckResult) {
	t.Helper()
	original := checkOutcomeForRecovery
	t.Cleanup(func() { checkOutcomeForRecovery = original })
	checkOutcomeForRecovery = func(_ context.Context, _ outcome.Brief) outcome.HealthCheckResult {
		return *result
	}
}

func failingScheduledResult(gate string, at time.Time) outcome.HealthCheckResult {
	return outcome.HealthCheckResult{
		CheckedAt: at,
		Signal:    "healthcheck_command",
		State:     outcome.HealthFailing,
		Checks:    []outcome.HealthCheckItem{{Name: gate, Blocking: true, Status: "fail"}},
	}
}

func TestEvaluateGateFailureStreaksOnce_ReplayEmitsOnceAndDedupes(t *testing.T) {
	cfg := streakTestConfig(t)
	if err := state.Save(cfg.StateDir, state.NewState()); err != nil {
		t.Fatalf("seed state: %v", err)
	}
	notifications, intakes, _ := installStreakDispatchStubs(t)

	t0 := time.Date(2026, 7, 21, 8, 0, 0, 0, time.UTC)
	current := failingScheduledResult("deb-idle-return-smoke", t0)
	stubScheduledCheck(t, &current)

	// Scheduled run #1: first failure, below threshold — no escalation.
	if _, err := EvaluateGateFailureStreaksOnce(cfg, t0); err != nil {
		t.Fatalf("run 1: %v", err)
	}
	if len(*notifications) != 0 || len(*intakes) != 0 {
		t.Fatalf("first failure escalated early: notifications=%v intakes=%v", *notifications, *intakes)
	}

	// Scheduled run #2: second consecutive same-gate failure crosses N=2.
	current = failingScheduledResult("deb-idle-return-smoke", t0.Add(time.Minute))
	if _, err := EvaluateGateFailureStreaksOnce(cfg, t0.Add(time.Minute)); err != nil {
		t.Fatalf("run 2: %v", err)
	}
	if len(*notifications) != 1 {
		t.Fatalf("want exactly one notification, got %d: %v", len(*notifications), *notifications)
	}
	if got := (*notifications)[0]; got.gate != "deb-idle-return-smoke" || got.count != 2 || got.link != "https://runtime/health" {
		t.Fatalf("notification = %+v, want gate+count+run link", got)
	}
	if len(*intakes) != 1 {
		t.Fatalf("want exactly one deduped repair issue, got %d", len(*intakes))
	}

	// Scheduled run #3: identical failure — no duplicate notification or issue.
	current = failingScheduledResult("deb-idle-return-smoke", t0.Add(2*time.Minute))
	if _, err := EvaluateGateFailureStreaksOnce(cfg, t0.Add(2*time.Minute)); err != nil {
		t.Fatalf("run 3: %v", err)
	}
	if len(*notifications) != 1 || len(*intakes) != 1 {
		t.Fatalf("third identical failure duplicated escalation: notifications=%d intakes=%d", len(*notifications), len(*intakes))
	}

	loaded, err := state.Load(cfg.StateDir)
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	if len(loaded.OutcomeGateStreaks) != 1 {
		t.Fatalf("want one tracked gate streak, got %+v", loaded.OutcomeGateStreaks)
	}
	streak := loaded.OutcomeGateStreaks[0]
	if streak.ConsecutiveFailures != 3 {
		t.Fatalf("streak count = %d, want 3", streak.ConsecutiveFailures)
	}
	if streak.IntakeIssue != 901 {
		t.Fatalf("intake issue = %d, want the single filed repair issue 901", streak.IntakeIssue)
	}
	if streak.LastTransitionAt.IsZero() {
		t.Fatalf("last transition should be visible in state")
	}
}

func TestEvaluateGateFailureStreaksOnce_PassResetsAndDifferentGateReEscalates(t *testing.T) {
	cfg := streakTestConfig(t)
	if err := state.Save(cfg.StateDir, state.NewState()); err != nil {
		t.Fatalf("seed state: %v", err)
	}
	notifications, intakes, _ := installStreakDispatchStubs(t)

	t0 := time.Date(2026, 7, 21, 8, 0, 0, 0, time.UTC)
	var current outcome.HealthCheckResult
	stubScheduledCheck(t, &current)

	current = failingScheduledResult("gate-a", t0)
	if _, err := EvaluateGateFailureStreaksOnce(cfg, t0); err != nil {
		t.Fatalf("run 1: %v", err)
	}
	current = failingScheduledResult("gate-a", t0.Add(time.Minute))
	if _, err := EvaluateGateFailureStreaksOnce(cfg, t0.Add(time.Minute)); err != nil {
		t.Fatalf("run 2: %v", err)
	}
	if len(*notifications) != 1 {
		t.Fatalf("want one escalation for gate-a, got %d", len(*notifications))
	}

	// A pass resets the streak.
	current = outcome.HealthCheckResult{
		CheckedAt: t0.Add(2 * time.Minute),
		State:     outcome.HealthHealthy,
		Checks:    []outcome.HealthCheckItem{{Name: "gate-a", Blocking: true, Status: "pass"}},
	}
	if _, err := EvaluateGateFailureStreaksOnce(cfg, t0.Add(2*time.Minute)); err != nil {
		t.Fatalf("pass run: %v", err)
	}
	if len(*notifications) != 1 {
		t.Fatalf("pass should not notify: %d", len(*notifications))
	}

	// A different failing gate is a new fingerprint and escalates on its own.
	current = failingScheduledResult("gate-b", t0.Add(3*time.Minute))
	if _, err := EvaluateGateFailureStreaksOnce(cfg, t0.Add(3*time.Minute)); err != nil {
		t.Fatalf("gate-b run 1: %v", err)
	}
	current = failingScheduledResult("gate-b", t0.Add(4*time.Minute))
	if _, err := EvaluateGateFailureStreaksOnce(cfg, t0.Add(4*time.Minute)); err != nil {
		t.Fatalf("gate-b run 2: %v", err)
	}
	if len(*notifications) != 2 || len(*intakes) != 2 {
		t.Fatalf("different failing gate did not re-escalate: notifications=%d intakes=%d", len(*notifications), len(*intakes))
	}
	if (*notifications)[1].gate != "gate-b" {
		t.Fatalf("second escalation gate = %q, want gate-b", (*notifications)[1].gate)
	}
}

func TestEvaluateGateFailureStreaksOnce_DisabledWithoutHealthSignal(t *testing.T) {
	cfg := &config.Config{
		Repo:     "BeFeast/example",
		StateDir: t.TempDir(),
		Outcome:  outcome.Brief{DesiredOutcome: "candidate feed follows main"},
	}
	if err := state.Save(cfg.StateDir, state.NewState()); err != nil {
		t.Fatalf("seed state: %v", err)
	}
	evaluated, err := EvaluateGateFailureStreaksOnce(cfg, time.Now().UTC())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if evaluated {
		t.Fatalf("gate-streak evaluator should be a no-op without a health signal")
	}
}
