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
		if st.OutcomeRecovery != nil && st.OutcomeRecovery.Status == outcome.RecoveryStatusCapped {
			dispatchFutileOutcomeRecovery(cfg, now)
		}
		return false, nil
	}

	health := checkOutcomeForRecovery(context.Background(), brief)
	if health.State == outcome.HealthHealthy {
		if err := state.Update(cfg.StateDir, func(latest *state.State) error {
			if setOutcomeHealthIfNewer(latest, health) {
				markOutcomeRecoveryVerified(latest, now)
			}
			return nil
		}); err != nil {
			return false, fmt.Errorf("outcome recovery: save healthy check: %w", err)
		}
		return true, nil
	}
	if health.State != outcome.HealthFailing {
		if err := state.Update(cfg.StateDir, func(latest *state.State) error {
			setOutcomeHealthIfNewer(latest, health)
			return nil
		}); err != nil {
			return false, fmt.Errorf("outcome recovery: save non-failing check: %w", err)
		}
		return true, nil
	}

	maxFutile := brief.EffectiveRecoveryMaxFutileAttempts()
	claimed := false
	attemptID := ""
	if err := state.Update(cfg.StateDir, func(latest *state.State) error {
		setOutcomeHealthIfNewer(latest, health)
		if recovery := latest.OutcomeRecovery; recovery != nil {
			if recovery.Status == outcome.RecoveryStatusCapped {
				fingerprint := state.OutcomeFailureFingerprint(health)
				if fingerprint != "" && fingerprint == recovery.FailureFingerprint {
					return nil
				}
				// A different failing signal is new work for the recovery actuator.
				// Re-arm it immediately rather than carrying the old cap/cooldown
				// across fingerprints.
				recovery.Status = outcome.RecoveryStatusFailed
				recovery.ConsecutiveFailures = 0
				recovery.FailureFingerprint = ""
				recovery.CappedAt = time.Time{}
				recovery.NextEligibleAt = time.Time{}
				recovery.UpdatedAt = now
			}
			if recovery.Status == outcome.RecoveryStatusUncertain {
				// The prior process ended without a durable receipt, so the same
				// failing signal cannot prove whether its external side effect landed.
				// Only an authoritative healthy check may clear this no-replay fence.
				return nil
			}
			if recovery.Status == outcome.RecoveryStatusExecuting {
				// The process identity is deliberately not replayable. Once its bounded
				// timeout elapsed, surface an uncertain receipt and wait for the health
				// signal to prove whether the side effect landed.
				if !recovery.StartedAt.IsZero() && now.After(recovery.StartedAt.Add(brief.EffectiveRecoveryTimeout()+time.Minute)) {
					recovery.Status = outcome.RecoveryStatusUncertain
					recovery.UpdatedAt = now
					recovery.Summary = "outcome recovery process ended without a durable receipt; health verification remains authoritative"
				}
				return nil
			}
			if !recovery.NextEligibleAt.IsZero() && now.Before(recovery.NextEligibleAt) {
				return nil
			}
		}

		attempts := 1
		consecutive := 0
		fingerprint := ""
		repairIssue := 0
		repairFingerprint := ""
		notifiedFingerprint := ""
		if previous := latest.OutcomeRecovery; previous != nil {
			attempts = previous.Attempts + 1
			consecutive = previous.ConsecutiveFailures
			fingerprint = previous.FailureFingerprint
			repairIssue = previous.RepairIssue
			repairFingerprint = previous.RepairFingerprint
			notifiedFingerprint = previous.NotifiedFingerprint
		}
		attemptID = outcomeRecoveryAttemptID(cfg.StateDir, health.CheckedAt, attempts)
		latest.OutcomeRecovery = &outcome.RecoveryState{
			AttemptID:           attemptID,
			Status:              outcome.RecoveryStatusExecuting,
			Attempts:            attempts,
			TriggerCheckedAt:    health.CheckedAt,
			StartedAt:           now,
			UpdatedAt:           now,
			Summary:             "outcome recovery execution leased",
			ConsecutiveFailures: consecutive,
			FailureFingerprint:  fingerprint,
			RepairIssue:         repairIssue,
			RepairFingerprint:   repairFingerprint,
			NotifiedFingerprint: notifiedFingerprint,
		}
		claimed = true
		return nil
	}); err != nil {
		return false, fmt.Errorf("outcome recovery: persist execution lease: %w", err)
	}
	if !claimed {
		dispatchFutileOutcomeRecovery(cfg, now)
		return true, nil
	}

	log.Printf("[outcome/recovery] leased automatic recovery attempt %s after failing %s", attemptID, health.Signal)
	execution := runOutcomeRecovery(context.Background(), brief)
	finishedAt := execution.FinishedAt.UTC()
	if finishedAt.IsZero() {
		finishedAt = time.Now().UTC()
	}
	leaseLost := false
	if err := state.Update(cfg.StateDir, func(latest *state.State) error {
		if latest.OutcomeRecovery == nil || latest.OutcomeRecovery.AttemptID != attemptID {
			leaseLost = true
			return nil
		}
		code := execution.ExitCode
		recovery := latest.OutcomeRecovery
		recovery.ExitCode = &code
		recovery.FinishedAt = finishedAt
		recovery.UpdatedAt = finishedAt
		recovery.NextEligibleAt = finishedAt.Add(brief.EffectiveRecoveryCooldown())
		if execution.ExitCode != 0 {
			recovery.Status = outcome.RecoveryStatusFailed
			if execution.TimedOut {
				recovery.Summary = "outcome recovery command timed out; retry held until cooldown"
			} else {
				recovery.Summary = "outcome recovery command failed; retry held until cooldown"
			}
			return nil
		}
		recovery.Status = outcome.RecoveryStatusVerificationPending
		recovery.Summary = "outcome recovery command completed; awaiting authoritative health verification"
		return nil
	}); err != nil {
		return true, fmt.Errorf("outcome recovery: persist execution receipt: %w", err)
	}
	if leaseLost {
		return true, fmt.Errorf("outcome recovery: execution lease %s lost before receipt", attemptID)
	}
	// Verification remains authoritative even when the recovery command exits
	// non-zero: bounded futility counts only post-attempt checks that are still
	// failing, never the pre-attempt trigger or a command exit by itself.
	verification := checkOutcomeForRecovery(context.Background(), brief)
	leaseLost = false
	capHit := false
	if err := state.Update(cfg.StateDir, func(latest *state.State) error {
		if latest.OutcomeRecovery == nil || latest.OutcomeRecovery.AttemptID != attemptID {
			leaseLost = true
			return nil
		}
		accepted := setOutcomeHealthIfNewer(latest, verification)
		if accepted && verification.State == outcome.HealthHealthy {
			markOutcomeRecoveryVerified(latest, verification.CheckedAt)
		} else if accepted && verification.State == outcome.HealthFailing {
			capHit = recordOutcomeRecoveryFutility(latest.OutcomeRecovery, verification, maxFutile, verification.CheckedAt)
		}
		return nil
	}); err != nil {
		return true, fmt.Errorf("outcome recovery: save execution receipt: %w", err)
	}
	if leaseLost {
		return true, fmt.Errorf("outcome recovery: execution lease %s lost before verification receipt", attemptID)
	}
	log.Printf("[outcome/recovery] attempt %s completed exit=%d verification=%s next_eligible_at=%s", attemptID, execution.ExitCode, verification.State, finishedAt.Add(brief.EffectiveRecoveryCooldown()).Format(time.RFC3339))
	if capHit {
		log.Printf("[outcome/recovery] capped after %d consecutive failing verifications for %s", maxFutile, outcome.FailingGateName(verification))
		dispatchFutileOutcomeRecovery(cfg, verification.CheckedAt)
	}
	return true, nil
}

func setOutcomeHealthIfNewer(st *state.State, health outcome.HealthCheckResult) bool {
	if st == nil {
		return false
	}
	if st.OutcomeHealth != nil && !health.CheckedAt.IsZero() && health.CheckedAt.Before(st.OutcomeHealth.CheckedAt) {
		return false
	}
	copy := health
	st.OutcomeHealth = &copy
	return true
}

func markOutcomeRecoveryVerified(st *state.State, at time.Time) {
	if st == nil || st.OutcomeRecovery == nil || st.OutcomeRecovery.Status == outcome.RecoveryStatusVerified {
		return
	}
	st.OutcomeRecovery.Status = outcome.RecoveryStatusVerified
	st.OutcomeRecovery.VerifiedAt = at.UTC()
	st.OutcomeRecovery.UpdatedAt = at.UTC()
	st.OutcomeRecovery.Summary = "authoritative outcome health verification passed"
	st.OutcomeRecovery.ConsecutiveFailures = 0
	st.OutcomeRecovery.FailureFingerprint = ""
	st.OutcomeRecovery.CappedAt = time.Time{}
}

// recordOutcomeRecoveryFutility advances the same-fingerprint post-attempt
// failure count and marks the cap hit. It returns true only for the transition
// into capped, which keeps the journal edge-triggered.
func recordOutcomeRecoveryFutility(recovery *outcome.RecoveryState, health outcome.HealthCheckResult, maxAttempts int, at time.Time) bool {
	if recovery == nil || health.State != outcome.HealthFailing {
		return false
	}
	fingerprint := state.OutcomeFailureFingerprint(health)
	if fingerprint == "" {
		return false
	}
	if fingerprint != recovery.FailureFingerprint {
		recovery.FailureFingerprint = fingerprint
		recovery.ConsecutiveFailures = 1
	} else {
		recovery.ConsecutiveFailures++
	}
	recovery.UpdatedAt = at.UTC()
	if maxAttempts <= 0 || recovery.ConsecutiveFailures < maxAttempts {
		return false
	}
	alreadyCapped := recovery.Status == outcome.RecoveryStatusCapped
	recovery.Status = outcome.RecoveryStatusCapped
	recovery.CappedAt = at.UTC()
	recovery.Summary = fmt.Sprintf("outcome recovery capped after %d consecutive failing verifications; awaiting repair intake", recovery.ConsecutiveFailures)
	return !alreadyCapped
}

func outcomeRecoveryAttemptID(stateDir string, checkedAt time.Time, attempts int) string {
	digest := sha256.Sum256([]byte(fmt.Sprintf("%s|%s|%d", stateDir, checkedAt.UTC().Format(time.RFC3339Nano), attempts)))
	return fmt.Sprintf("outcome-%x", digest[:8])
}
