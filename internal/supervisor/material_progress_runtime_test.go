package supervisor

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/state"
)

func TestEvaluateMaterialProgressOnce_InactiveLegacyProjectDoesNotWriteState(t *testing.T) {
	cfg := &config.Config{StateDir: t.TempDir()}
	evaluated, err := EvaluateMaterialProgressOnce(cfg, time.Now().UTC())
	if err != nil || evaluated {
		t.Fatalf("inactive evaluation: evaluated=%t err=%v", evaluated, err)
	}
	if _, err := os.Stat(state.StatePath(cfg.StateDir)); !os.IsNotExist(err) {
		t.Fatalf("inactive legacy project wrote state: err=%v", err)
	}
}

func TestEvaluateMaterialProgressOnce_CadenceAndConfigTransitions(t *testing.T) {
	enabled := true
	cfg := &config.Config{StateDir: t.TempDir()}
	cfg.StalledProgressWatchdog.Enabled = &enabled
	cfg.StalledProgressWatchdog.MaxSilenceMinutes = 20
	cfg.StalledProgressWatchdog.EvalIntervalSecs = 60
	t0 := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)

	st := state.NewState()
	st.Sessions["slot-1"] = &state.Session{
		IssueNumber: 7,
		Status:      state.StatusRunning,
		PID:         1234,
		StartedAt:   t0.Add(-time.Minute),
	}
	if err := state.Save(cfg.StateDir, st); err != nil {
		t.Fatalf("seed state: %v", err)
	}

	if evaluated, err := EvaluateMaterialProgressOnce(cfg, t0); err != nil || !evaluated {
		t.Fatalf("first evaluation: evaluated=%t err=%v", evaluated, err)
	}
	loaded, err := state.Load(cfg.StateDir)
	if err != nil {
		t.Fatalf("load first evaluation: %v", err)
	}
	if loaded.MaterialProgress == nil || loaded.MaterialProgress.BudgetSeconds != 1200 {
		t.Fatalf("first evaluation did not persist active budget: %+v", loaded.MaterialProgress)
	}
	if len(loaded.MaterialProgress.Targets) != 1 {
		t.Fatalf("active targets = %d, want 1", len(loaded.MaterialProgress.Targets))
	}
	firstAt := loaded.MaterialProgress.LastEvaluatedAt

	if evaluated, err := EvaluateMaterialProgressOnce(cfg, t0.Add(10*time.Second)); err != nil || evaluated {
		t.Fatalf("inside cadence: evaluated=%t err=%v", evaluated, err)
	}
	// A cadence-only edit is persisted immediately so Fleet reports the actual
	// runtime schedule, but it must not reset the target's silence watermark.
	cfg.StalledProgressWatchdog.EvalIntervalSecs = 300
	if evaluated, err := EvaluateMaterialProgressOnce(cfg, t0.Add(10*time.Second)); err != nil || !evaluated {
		t.Fatalf("cadence transition: evaluated=%t err=%v", evaluated, err)
	}
	loaded, err = state.Load(cfg.StateDir)
	if err != nil {
		t.Fatalf("load cadence transition: %v", err)
	}
	if loaded.MaterialProgress.EvalIntervalSeconds != 300 {
		t.Fatalf("persisted cadence = %d, want 300", loaded.MaterialProgress.EvalIntervalSeconds)
	}
	for key, target := range loaded.MaterialProgress.Targets {
		if !target.Watermark.At.Equal(t0) {
			t.Fatalf("cadence-only edit reset target %s watermark to %s", key, target.Watermark.At)
		}
	}

	// Disable is a config transition and must bypass the configured cadence, persist
	// budget=0, and retire the old target immediately.
	disabled := false
	cfg.StalledProgressWatchdog.Enabled = &disabled
	if evaluated, err := EvaluateMaterialProgressOnce(cfg, t0.Add(11*time.Second)); err != nil || !evaluated {
		t.Fatalf("disable transition: evaluated=%t err=%v", evaluated, err)
	}
	loaded, err = state.Load(cfg.StateDir)
	if err != nil {
		t.Fatalf("load disabled evaluation: %v", err)
	}
	if loaded.MaterialProgress.BudgetSeconds != 0 {
		t.Fatalf("disabled budget = %d, want 0", loaded.MaterialProgress.BudgetSeconds)
	}
	for key, target := range loaded.MaterialProgress.Targets {
		if target.Active {
			t.Fatalf("target %s stayed active after disable", key)
		}
	}

	// Re-enable with the same 20m budget is another immediate transition. The
	// target is re-baselined at the new evaluation time; the pre-disable
	// watermark/deadline cannot be resurrected.
	cfg.StalledProgressWatchdog.Enabled = &enabled
	if evaluated, err := EvaluateMaterialProgressOnce(cfg, t0.Add(12*time.Second)); err != nil || !evaluated {
		t.Fatalf("re-enable transition: evaluated=%t err=%v", evaluated, err)
	}
	loaded, err = state.Load(cfg.StateDir)
	if err != nil {
		t.Fatalf("load re-enabled evaluation: %v", err)
	}
	if !loaded.MaterialProgress.LastEvaluatedAt.After(firstAt) {
		t.Fatalf("re-enable did not advance evaluation time: first=%s now=%s", firstAt, loaded.MaterialProgress.LastEvaluatedAt)
	}
	for key, target := range loaded.MaterialProgress.Targets {
		if !target.Active {
			t.Fatalf("target %s did not reactivate", key)
		}
		if !target.Watermark.At.Equal(t0.Add(12 * time.Second)) {
			t.Fatalf("target %s resurrected stale watermark at %s", key, target.Watermark.At)
		}
	}
}

func TestRunMaterialProgressEvaluator_OwnsIndependentCadence(t *testing.T) {
	enabled := true
	cfg := &config.Config{StateDir: t.TempDir()}
	cfg.StalledProgressWatchdog.Enabled = &enabled
	cfg.StalledProgressWatchdog.MaxSilenceMinutes = 20
	cfg.StalledProgressWatchdog.EvalIntervalSecs = 1

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		RunMaterialProgressEvaluator(ctx, "runtime-test", func() *config.Config { return cfg })
	}()
	defer func() {
		cancel()
		<-done
	}()

	first := waitForMaterialEvaluation(t, cfg.StateDir, time.Time{}, time.Second)
	second := waitForMaterialEvaluation(t, cfg.StateDir, first, 2*time.Second)
	if !second.After(first) {
		t.Fatalf("independent cadence did not advance: first=%s second=%s", first, second)
	}
}

func waitForMaterialEvaluation(t *testing.T, stateDir string, after time.Time, timeout time.Duration) time.Time {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		st, err := state.Load(stateDir)
		if err == nil && st.MaterialProgress != nil && st.MaterialProgress.LastEvaluatedAt.After(after) {
			return st.MaterialProgress.LastEvaluatedAt
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("material-progress evaluation did not advance after %s within %s", after, timeout)
	return time.Time{}
}
