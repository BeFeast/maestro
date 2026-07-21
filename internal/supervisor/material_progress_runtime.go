package supervisor

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/state"
)

// materialProgressConfigRefreshCap bounds how long a live config edit can take
// to retime or disable the evaluator. The evaluator still records only at the
// configured cadence: calls inside that cadence are rejected by the durable
// LastEvaluatedAt gate in recordMaterialProgress and perform no Save.
const materialProgressConfigRefreshCap = 15 * time.Second

// EvaluateMaterialProgressOnce performs one local, GitHub/LLM-free watchdog
// evaluation and durably saves it when it was due. It is intentionally separate
// from Engine.Decide and supervisor.RunOnce: a broken GitHub read, unavailable
// model backend, or wedged decision cycle must not stop the liveness clock.
//
// The returned bool is true only when an evaluation was actually recorded. A
// call inside the configured cadence is a cheap load/no-op and does not churn
// state.json or the SQLite SaveHook mirror.
func EvaluateMaterialProgressOnce(cfg *config.Config, now time.Time) (bool, error) {
	if cfg == nil {
		return false, fmt.Errorf("material-progress evaluator: nil config")
	}
	if strings.TrimSpace(cfg.StateDir) == "" {
		return false, fmt.Errorf("material-progress evaluator: empty state_dir")
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}

	st, err := state.Load(cfg.StateDir)
	if err != nil {
		return false, fmt.Errorf("material-progress evaluator: load state: %w", err)
	}
	budget := cfg.StalledProgressWatchdog.EffectiveMaxSilence()
	interval := cfg.StalledProgressWatchdog.EffectiveEvalInterval()
	// A legacy project with no v1 state and no explicit opt-in needs no write at
	// all. Once state exists, disabled evaluation remains mandatory so a config
	// transition persists budget=0 and retires old targets.
	if st.MaterialProgress == nil && budget <= 0 {
		return false, nil
	}
	evaluated := false
	if st.MaterialProgress == nil || st.MaterialProgress.EvaluationDue(budget, interval, now) {
		if _, err := recordMaterialProgress(cfg, st, now); err != nil {
			return false, fmt.Errorf("material-progress evaluator: evaluate: %w", err)
		}
		if err := state.Save(cfg.StateDir, st); err != nil {
			return false, fmt.Errorf("material-progress evaluator: save state: %w", err)
		}
		evaluated = true
	}
	// Recovery reconciliation runs even inside the evaluation cadence. A prior
	// owner may have crashed after claiming/stopping but before scheduling the
	// existing orchestrator retry; the durable lease expiry, not a fresh
	// watermark evaluation, controls that takeover.
	if err := reconcileMaterialProgressRecoveries(cfg, now, defaultMaterialProgressRecoveryRuntime); err != nil {
		return evaluated, fmt.Errorf("material-progress evaluator: reconcile recovery: %w", err)
	}
	return evaluated, nil
}

// RunMaterialProgressEvaluator runs the durable per-project evaluation clock.
// It owns its timer and consults the current config on every wake-up, independent
// of the orchestrator and supervisor decision loops. The short refresh cap lets
// config-store edits retime/disable it promptly without evaluating more often
// than the persisted configured cadence.
func RunMaterialProgressEvaluator(ctx context.Context, name string, getCfg func() *config.Config) {
	if getCfg == nil {
		return
	}
	logPrefix := "material-progress/watchdog"
	if name = strings.TrimSpace(name); name != "" {
		logPrefix = name + " material-progress/watchdog"
	}

	for {
		cfg := getCfg()
		wait := materialProgressConfigRefreshCap
		if cfg != nil {
			interval := cfg.StalledProgressWatchdog.EffectiveEvalInterval()
			if interval < wait {
				wait = interval
			}
			// Evaluate disabled config too. State.EvaluationDue treats an
			// enabled↔disabled/budget transition as immediately due and retires
			// active targets with budget=0, so re-enable cannot resurrect an old
			// deadline.
			if _, err := EvaluateMaterialProgressOnce(cfg, time.Now().UTC()); err != nil {
				log.Printf("[%s] evaluation failed (will retry): %v", logPrefix, err)
			}
			if _, err := EvaluateOutcomeRecoveryOnce(cfg, time.Now().UTC()); err != nil {
				log.Printf("[%s] outcome recovery evaluation failed (will retry): %v", logPrefix, err)
			}
			if _, err := EvaluateGateFailureStreaksOnce(cfg, time.Now().UTC()); err != nil {
				log.Printf("[%s] gate-fail-streak evaluation failed (will retry): %v", logPrefix, err)
			}
		}
		if wait <= 0 {
			wait = time.Second
		}

		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}
