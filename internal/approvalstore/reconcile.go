package approvalstore

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/befeast/maestro/internal/state"
)

// DeliveryReconcileActor is the closed audit identity for the local,
// operator-only reconciliation command. Reconciliation deliberately does not
// accept caller-supplied audit text: neither actor nor reason can become a
// durable free-text channel.
const DeliveryReconcileActor = "operator_cli"

// ErrDeliveryReconcileOutcome is returned for every outcome outside the
// closed operator reconciliation vocabulary. In particular there is no
// "unknown" outcome: uncertainty leaves the durable execution lease in
// status=executing for later investigation.
var ErrDeliveryReconcileOutcome = errors.New("unknown delivery reconciliation outcome")

var (
	ErrDeliveryReconcileRunnerGoneRequired = errors.New("reconciliation requires an explicit assertion that the prior runner is gone")
	ErrDeliveryReconcileTargetSafeRequired = errors.New("negative reconciliation requires an explicit assertion that the target is safe")
)

// DeliveryReconcileAssertions carries the non-text operator confirmations and
// live config digest that must hold before an executing lease may close. The
// store checks these too, so future internal callers cannot bypass the CLI
// recovery contract.
type DeliveryReconcileAssertions struct {
	RunnerGone           bool
	TargetSafe           bool
	ExpectedConfigDigest string
}

// ReconcileDelivery atomically closes an interrupted approval-gated delivery.
// It is intentionally callable only by an explicit operator surface; no
// daemon/timer/replay path invokes it. The WHERE status='executing' transition
// admits one terminal writer under contention and leaves every other status
// untouched.
//
// verified records executing -> executed after the caller ran the pristine
// pinned verifier. not_applied and remediated_failed record executing ->
// execution_failed after the caller established that the target is safe. The
// persisted result is rebuilt from a strict structured allow-list, so command,
// output, error, and arbitrary note text cannot enter the ledger.
func (s *Store) ReconcileDelivery(ctx context.Context, stateDir, id, outcome string, result *state.DeliveryPayload, assertions DeliveryReconcileAssertions, now time.Time) (*state.Approval, error) {
	outcome = strings.TrimSpace(outcome)
	if !assertions.RunnerGone {
		return nil, ErrDeliveryReconcileRunnerGoneRequired
	}
	if result == nil || result.StartedAt.IsZero() || result.FinishedAt.IsZero() || result.FinishedAt.Before(result.StartedAt) {
		return nil, ErrDeliveryIntegrity
	}

	safe := &state.DeliveryPayload{
		StartedAt:        result.StartedAt,
		FinishedAt:       result.FinishedAt,
		CompletionSource: state.DeliveryCompletionSourceOperatorReconcile,
		ReconcileOutcome: outcome,
	}
	target := state.ApprovalStatusExecutionFailed
	switch outcome {
	case state.DeliveryReconcileOutcomeVerified:
		expectedDigest := strings.TrimSpace(assertions.ExpectedConfigDigest)
		if expectedDigest == "" {
			return nil, ErrDeliveryConfigMismatch
		}
		revision := strings.ToLower(strings.TrimSpace(result.ExecutedRevision))
		if !fullHexRevision(revision) {
			return nil, ErrDeliveryIntegrity
		}
		zero := 0
		target = state.ApprovalStatusExecuted
		safe.ExecutedRevision = revision
		safe.VerifyExitCode = &zero
		safe.Verified = true
		safe.CleanupFailed = result.CleanupFailed
	case state.DeliveryReconcileOutcomeNotApplied, state.DeliveryReconcileOutcomeRemediatedFailed:
		if !assertions.TargetSafe {
			return nil, ErrDeliveryReconcileTargetSafeRequired
		}
		// No execution or verifier claim is made for a negative reconciliation.
		// The explicit CLI assertions are the authorization; the ledger carries
		// only the closed outcome and observation timestamps.
	default:
		return nil, ErrDeliveryReconcileOutcome
	}

	// ConfigDigest is immutable and record-hash protected. Re-read it from the
	// authoritative ledger immediately before the atomic executing transition;
	// a caller cannot claim it matched by supplying result metadata.
	if outcome == state.DeliveryReconcileOutcomeVerified {
		current, err := s.Get(ctx, stateDir, id)
		if err != nil {
			return current, err
		}
		if current.Status != state.ApprovalStatusExecuting {
			return current, state.ErrApprovalNotExecuting
		}
		if current.Delivery == nil || strings.TrimSpace(current.Delivery.ConfigDigest) != strings.TrimSpace(assertions.ExpectedConfigDigest) {
			return current, ErrDeliveryConfigMismatch
		}
	}

	mutate := func(a *state.Approval) {
		a.Delivery = state.MergeDeliveryResult(a.Delivery, safe)
	}
	return s.markFrom(ctx, stateDir, id,
		state.ApprovalStatusExecuting, target,
		state.ApprovalAuditDeliveryReconciled, state.ErrApprovalNotExecuting,
		now, DeliveryReconcileActor, "delivery reconciled by operator", mutate)
}

func fullHexRevision(revision string) bool {
	if len(revision) != 40 && len(revision) != 64 {
		return false
	}
	for _, ch := range revision {
		if !((ch >= '0' && ch <= '9') || (ch >= 'a' && ch <= 'f')) {
			return false
		}
	}
	return true
}
