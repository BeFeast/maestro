package approver

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"time"

	"github.com/befeast/maestro/internal/approvalstore"
	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/state"
)

var (
	ErrDeliveryReconcileRunnerGoneRequired = approvalstore.ErrDeliveryReconcileRunnerGoneRequired
	ErrDeliveryReconcileTargetSafeRequired = approvalstore.ErrDeliveryReconcileTargetSafeRequired
	ErrDeliveryReconcileRevisionRequired   = errors.New("verified reconciliation requires the exact observed full revision")
	ErrDeliveryReconcileRevisionMismatch   = errors.New("observed target revision differs from the approved revision")
	ErrDeliveryReconcileMode               = errors.New("delivery reconciliation requires approval_required mode")
)

// DeliveryReconcileRequest is the complete explicit operator assertion set.
// There is deliberately no unknown outcome and no free-text note field.
type DeliveryReconcileRequest struct {
	ID               string
	Outcome          string
	ObservedRevision string
	RunnerGone       bool
	TargetSafe       bool
}

// DeliveryReconcileResult contains only the authoritative terminal row and a
// fixed, safe status summary. Verifier output is never returned or persisted.
type DeliveryReconcileResult struct {
	Approval *state.Approval
	Status   state.ApprovalStatus
	Summary  string
}

// DeliveryReconciler is the explicit recovery path for an interrupted
// executing delivery. It never runs the deployment entrypoint. A verified
// outcome materializes a fresh exact-SHA checkout and runs only the committed
// verifier; negative outcomes require the operator's target-safe assertion.
type DeliveryReconciler struct {
	Store       *approvalstore.Store
	StateDir    string
	Repo        string
	Delivery    config.DeliveryConfig
	Runner      CommandRunner
	Checkout    CheckoutPreparer
	Now         func() time.Time
	OutputLimit int
}

// ParseDeliveryReconcileOutcome maps the CLI spelling onto the closed durable
// vocabulary. Keeping this parser strict means a typo can never accidentally
// clear an executing lease.
func ParseDeliveryReconcileOutcome(raw string) (string, error) {
	switch strings.TrimSpace(raw) {
	case state.DeliveryReconcileOutcomeVerified:
		return state.DeliveryReconcileOutcomeVerified, nil
	case "not-applied":
		return state.DeliveryReconcileOutcomeNotApplied, nil
	case "remediated-failed":
		return state.DeliveryReconcileOutcomeRemediatedFailed, nil
	default:
		return "", approvalstore.ErrDeliveryReconcileOutcome
	}
}

func (r *DeliveryReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now().UTC()
	}
	return time.Now().UTC()
}

func (r *DeliveryReconciler) executor() *DeliveryExecutor {
	return &DeliveryExecutor{
		Repo:        r.Repo,
		Delivery:    r.Delivery,
		Runner:      r.Runner,
		Checkout:    r.Checkout,
		OutputLimit: r.OutputLimit,
	}
}

// Reconcile performs one explicit recovery attempt. All validation failures,
// checkout failures, and verifier failures leave the authoritative row in
// status=executing: uncertainty never releases or replays the old delivery.
func (r *DeliveryReconciler) Reconcile(ctx context.Context, req DeliveryReconcileRequest) (DeliveryReconcileResult, error) {
	var result DeliveryReconcileResult
	if r == nil || r.Store == nil {
		return result, errors.New("delivery reconciliation has no authoritative store")
	}
	outcome, err := ParseDeliveryReconcileOutcome(req.Outcome)
	if err != nil {
		return result, err
	}
	if !req.RunnerGone {
		return result, ErrDeliveryReconcileRunnerGoneRequired
	}
	if outcome != state.DeliveryReconcileOutcomeVerified && !req.TargetSafe {
		return result, ErrDeliveryReconcileTargetSafeRequired
	}

	a, err := r.Store.Get(ctx, r.StateDir, strings.TrimSpace(req.ID))
	if err != nil {
		return result, err
	}
	result.Approval = a
	result.Status = a.Status
	if a.Action != state.ApprovalActionDeployProject || a.Delivery == nil {
		return result, ErrNotDeliverable
	}
	if err := r.executor().repoGuard(a); err != nil {
		return result, err
	}
	if a.Status != state.ApprovalStatusExecuting {
		return result, state.ErrApprovalNotExecuting
	}

	started := r.now()
	if outcome != state.DeliveryReconcileOutcomeVerified {
		terminal, err := r.Store.ReconcileDelivery(ctx, r.StateDir, a.ID, outcome, &state.DeliveryPayload{
			StartedAt:  started,
			FinishedAt: r.now(),
		}, approvalstore.DeliveryReconcileAssertions{
			RunnerGone: req.RunnerGone,
			TargetSafe: req.TargetSafe,
		}, r.now())
		result.Approval = terminal
		result.Status = statusOf(terminal)
		result.Summary = "delivery reconciled as target-safe failure"
		return result, err
	}

	if r.Delivery.Mode != config.DeliveryModeApprovalRequired {
		return result, ErrDeliveryReconcileMode
	}
	if strings.TrimSpace(a.Delivery.ConfigDigest) == "" || a.Delivery.ConfigDigest != r.Delivery.ApprovalDigest() {
		return result, ErrDeliveryConfigMismatch
	}
	observed := strings.ToLower(strings.TrimSpace(req.ObservedRevision))
	if !validFullRevision(observed) {
		return result, ErrDeliveryReconcileRevisionRequired
	}
	if !revisionMatches(observed, a.Delivery.MergedSHA) {
		return result, ErrDeliveryReconcileRevisionMismatch
	}
	verify := strings.TrimSpace(r.Delivery.VerifyCommand)
	if verify == "" {
		return result, ErrDeliveryVerifierRequired
	}

	// Use the same hardened materializer and direct argument-free entrypoint
	// runner as normal delivery, but create only one pristine checkout because
	// no deployment entrypoint is permitted on this recovery path.
	ex := r.executor()
	timeout := r.Delivery.EffectiveTimeout()
	checkout, _, err := ex.prepareDeliveryCheckout(ctx, timeout, a.Delivery.MergedSHA)
	if err != nil {
		return result, ErrDeliveryCheckoutFailed
	}
	cleaned := false
	cleanup := func() bool {
		if cleaned {
			return false
		}
		cleaned = true
		if checkout == nil || checkout.Cleanup == nil {
			return true
		}
		return checkout.Cleanup() != nil
	}
	defer cleanup()
	if filepath.Clean(checkout.Dir) == "." || !revisionMatches(checkout.Revision, a.Delivery.MergedSHA) {
		cleanup()
		return result, ErrDeliveryCheckoutFailed
	}
	if _, err := secureDeliveryEntrypoint(checkout.Dir, verify); err != nil {
		cleanup()
		return result, ErrDeliveryEntrypointUnsafe
	}

	verifyCtx, cancel := context.WithTimeout(ctx, timeout)
	_, verifyErr := ex.runner().Run(verifyCtx, checkout.Dir, verify)
	timedOut := errors.Is(verifyCtx.Err(), context.DeadlineExceeded) || errors.Is(verifyErr, context.DeadlineExceeded)
	cancel()
	if verifyErr != nil || timedOut {
		cleanup()
		return result, ErrDeliveryVerificationFailed
	}
	cleanupFailed := cleanup()
	finished := r.now()
	terminal, err := r.Store.ReconcileDelivery(ctx, r.StateDir, a.ID, outcome, &state.DeliveryPayload{
		StartedAt:        started,
		FinishedAt:       finished,
		ExecutedRevision: observed,
		CleanupFailed:    cleanupFailed,
	}, approvalstore.DeliveryReconcileAssertions{
		RunnerGone:           req.RunnerGone,
		ExpectedConfigDigest: r.Delivery.ApprovalDigest(),
	}, r.now())
	result.Approval = terminal
	result.Status = statusOf(terminal)
	result.Summary = "interrupted delivery verified and reconciled"
	return result, err
}
