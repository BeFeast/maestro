package supervisor

import (
	"context"
	"crypto/sha256"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/outcome"
	"github.com/befeast/maestro/internal/state"
)

var (
	checkOutcomeForRecovery = func(ctx context.Context, brief outcome.Brief) outcome.HealthCheckResult {
		return outcome.Checker{}.Check(ctx, brief)
	}
	runOutcomeRecovery = func(ctx context.Context, brief outcome.Brief) outcome.RecoveryExecution {
		return outcome.RecoveryRunner{}.Run(ctx, brief)
	}
)

// EvaluateOutcomeRecoveryOnce owns the project-scoped 60-second health and
// automatic-recovery clock. It persists an executing lease before launching
// the project-owned idempotent command, so overlapping daemon loops or a
// restart cannot replay an uncertain side effect.
func EvaluateOutcomeRecoveryOnce(cfg *config.Config, now time.Time) (bool, error) {
	if cfg == nil {
		return false, fmt.Errorf("outcome recovery: nil config")
	}
	brief := cfg.Outcome.Normalized()
	if !brief.AutomaticRecoveryEnabled() {
		return false, nil
	}
	if strings.TrimSpace(cfg.StateDir) == "" {
		return false, fmt.Errorf("outcome recovery: empty state_dir")
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}

	st, err := state.Load(cfg.StateDir)
	if err != nil {
		return false, fmt.Errorf("outcome recovery: load state: %w", err)
	}
	if st.OutcomeHealth != nil && !st.OutcomeHealth.CheckedAt.IsZero() &&
		now.Sub(st.OutcomeHealth.CheckedAt) < brief.EffectiveRecoveryInterval() {
		return false, nil
	}

	health := checkOutcomeForRecovery(context.Background(), brief)
	st.OutcomeHealth = &health
	if health.State == outcome.HealthHealthy {
		markOutcomeRecoveryVerified(st, now)
		if err := state.Save(cfg.StateDir, st); err != nil {
			return false, fmt.Errorf("outcome recovery: save healthy check: %w", err)
		}
		return true, nil
	}

	if recovery := st.OutcomeRecovery; recovery != nil {
		if recovery.Status == outcome.RecoveryStatusExecuting {
			// The process identity is deliberately not replayable. Once its bounded
			// timeout elapsed, surface an uncertain receipt and wait for the health
			// signal to prove whether the side effect landed.
			if !recovery.StartedAt.IsZero() && now.After(recovery.StartedAt.Add(brief.EffectiveRecoveryTimeout()+time.Minute)) {
				recovery.Status = outcome.RecoveryStatusUncertain
				recovery.UpdatedAt = now
				recovery.Summary = "outcome recovery process ended without a durable receipt; health verification remains authoritative"
			}
			if err := state.Save(cfg.StateDir, st); err != nil {
				return false, fmt.Errorf("outcome recovery: save executing check: %w", err)
			}
			return true, nil
		}
		if !recovery.NextEligibleAt.IsZero() && now.Before(recovery.NextEligibleAt) {
			if err := state.Save(cfg.StateDir, st); err != nil {
				return false, fmt.Errorf("outcome recovery: save cooldown check: %w", err)
			}
			return true, nil
		}
	}

	attempts := 1
	if st.OutcomeRecovery != nil {
		attempts = st.OutcomeRecovery.Attempts + 1
	}
	attemptID := outcomeRecoveryAttemptID(cfg.StateDir, health.CheckedAt, attempts)
	st.OutcomeRecovery = &outcome.RecoveryState{
		AttemptID:        attemptID,
		Status:           outcome.RecoveryStatusExecuting,
		Attempts:         attempts,
		TriggerCheckedAt: health.CheckedAt,
		StartedAt:        now,
		UpdatedAt:        now,
		Summary:          "outcome recovery execution leased",
	}
	if err := state.Save(cfg.StateDir, st); err != nil {
		return false, fmt.Errorf("outcome recovery: persist execution lease: %w", err)
	}

	log.Printf("[outcome/recovery] leased automatic recovery attempt %s after failing %s", attemptID, health.Signal)
	execution := runOutcomeRecovery(context.Background(), brief)
	latest, err := state.Load(cfg.StateDir)
	if err != nil {
		return true, fmt.Errorf("outcome recovery: reload after execution: %w", err)
	}
	if latest.OutcomeRecovery == nil || latest.OutcomeRecovery.AttemptID != attemptID {
		return true, fmt.Errorf("outcome recovery: execution lease %s lost before receipt", attemptID)
	}
	code := execution.ExitCode
	recovery := latest.OutcomeRecovery
	recovery.ExitCode = &code
	recovery.FinishedAt = execution.FinishedAt
	recovery.UpdatedAt = execution.FinishedAt
	recovery.NextEligibleAt = execution.FinishedAt.Add(brief.EffectiveRecoveryCooldown())
	if execution.ExitCode != 0 {
		recovery.Status = outcome.RecoveryStatusFailed
		if execution.TimedOut {
			recovery.Summary = "outcome recovery command timed out; retry held until cooldown"
		} else {
			recovery.Summary = "outcome recovery command failed; retry held until cooldown"
		}
		if err := state.Save(cfg.StateDir, latest); err != nil {
			return true, fmt.Errorf("outcome recovery: save failed receipt: %w", err)
		}
		log.Printf("[outcome/recovery] attempt %s failed exit=%d; retry_at=%s", attemptID, execution.ExitCode, recovery.NextEligibleAt.Format(time.RFC3339))
		return true, nil
	}

	recovery.Status = outcome.RecoveryStatusVerificationPending
	recovery.Summary = "outcome recovery command completed; awaiting authoritative health verification"
	verification := checkOutcomeForRecovery(context.Background(), brief)
	latest.OutcomeHealth = &verification
	if verification.State == outcome.HealthHealthy {
		markOutcomeRecoveryVerified(latest, verification.CheckedAt)
	}
	if err := state.Save(cfg.StateDir, latest); err != nil {
		return true, fmt.Errorf("outcome recovery: save execution receipt: %w", err)
	}
	log.Printf("[outcome/recovery] attempt %s completed exit=0 verification=%s next_eligible_at=%s", attemptID, verification.State, recovery.NextEligibleAt.Format(time.RFC3339))
	return true, nil
}

func markOutcomeRecoveryVerified(st *state.State, at time.Time) {
	if st == nil || st.OutcomeRecovery == nil || st.OutcomeRecovery.Status == outcome.RecoveryStatusVerified {
		return
	}
	st.OutcomeRecovery.Status = outcome.RecoveryStatusVerified
	st.OutcomeRecovery.VerifiedAt = at.UTC()
	st.OutcomeRecovery.UpdatedAt = at.UTC()
	st.OutcomeRecovery.Summary = "authoritative outcome health verification passed"
}

func outcomeRecoveryAttemptID(stateDir string, checkedAt time.Time, attempts int) string {
	digest := sha256.Sum256([]byte(fmt.Sprintf("%s|%s|%d", stateDir, checkedAt.UTC().Format(time.RFC3339Nano), attempts)))
	return fmt.Sprintf("outcome-%x", digest[:8])
}
