package approvalstore

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/befeast/maestro/internal/state"
)

const reconcileTestDigest = "sha256:reconcile-spec"

func seedExecutingReconcileDelivery(t *testing.T, s *Store, id string) (*state.Approval, time.Time) {
	t.Helper()
	sha := strings.Repeat("a", 40)
	a := seedApprovedDelivery(t, s, id, sha)
	a.Delivery.ConfigDigest = reconcileTestDigest
	a.PayloadHash = a.ComputePayloadHash()
	if err := s.forceWriteJSON(testStateDir, a); err != nil {
		t.Fatalf("bind reconcile config digest: %v", err)
	}
	now := time.Now().UTC()
	claimed, err := s.ClaimDeliveryExecuting(context.Background(), testStateDir, a.ID, reconcileTestDigest, now, "daemon", "claim")
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	return claimed, now
}

func TestReconcileDeliveryVerifiedWritesClosedStructuredResult(t *testing.T) {
	s := openTestStore(t)
	claimed, now := seedExecutingReconcileDelivery(t, s, "reconcile-verified")
	zero := 0
	done, err := s.ReconcileDelivery(context.Background(), testStateDir, claimed.ID,
		state.DeliveryReconcileOutcomeVerified,
		&state.DeliveryPayload{
			StartedAt:        now,
			FinishedAt:       now.Add(time.Second),
			ExecutedRevision: claimed.Delivery.MergedSHA,
			DeployExitCode:   &zero, // caller input is deliberately discarded
			FailureStage:     state.DeliveryFailureStageDeploy,
		}, DeliveryReconcileAssertions{RunnerGone: true, ExpectedConfigDigest: reconcileTestDigest}, now.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if done.Status != state.ApprovalStatusExecuted || done.Delivery == nil || !done.Delivery.Verified ||
		done.Delivery.CompletionSource != state.DeliveryCompletionSourceOperatorReconcile ||
		done.Delivery.ReconcileOutcome != state.DeliveryReconcileOutcomeVerified ||
		done.Delivery.DeployExitCode != nil || done.Delivery.VerifyExitCode == nil || *done.Delivery.VerifyExitCode != 0 ||
		done.Delivery.FailureStage != "" {
		t.Fatalf("verified reconciliation = %+v", done)
	}
	last := done.Audit[len(done.Audit)-1]
	if last.Event != state.ApprovalAuditDeliveryReconciled || last.Actor != DeliveryReconcileActor ||
		last.Reason != "delivery reconciled by operator" {
		t.Fatalf("reconcile audit = %+v", last)
	}
	blob, err := json.Marshal(done)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"command", "output", "stderr", "stdout", "error"} {
		if strings.Contains(strings.ToLower(string(blob)), forbidden) {
			t.Fatalf("forbidden free-text key %q in persisted reconciliation: %s", forbidden, blob)
		}
	}
}

func TestReconcileDeliveryNegativeOutcomesAreTerminalFailures(t *testing.T) {
	for _, outcome := range []string{
		state.DeliveryReconcileOutcomeNotApplied,
		state.DeliveryReconcileOutcomeRemediatedFailed,
	} {
		t.Run(outcome, func(t *testing.T) {
			s := openTestStore(t)
			claimed, now := seedExecutingReconcileDelivery(t, s, "reconcile-"+outcome)
			done, err := s.ReconcileDelivery(context.Background(), testStateDir, claimed.ID, outcome,
				&state.DeliveryPayload{StartedAt: now, FinishedAt: now.Add(time.Second)},
				DeliveryReconcileAssertions{RunnerGone: true, TargetSafe: true}, now.Add(time.Second))
			if err != nil {
				t.Fatal(err)
			}
			if done.Status != state.ApprovalStatusExecutionFailed || done.Delivery == nil || done.Delivery.Verified ||
				done.Delivery.CompletionSource != state.DeliveryCompletionSourceOperatorReconcile ||
				done.Delivery.ReconcileOutcome != outcome || done.Delivery.DeployExitCode != nil || done.Delivery.VerifyExitCode != nil {
				t.Fatalf("negative reconciliation = %+v", done)
			}
		})
	}
}

func TestReconcileDeliveryWrongStatusOutcomeAndRevisionLeaveExecutingLease(t *testing.T) {
	s := openTestStore(t)
	approved := seedApprovedDelivery(t, s, "reconcile-wrong-status", strings.Repeat("a", 40))
	approved.Delivery.ConfigDigest = reconcileTestDigest
	approved.PayloadHash = approved.ComputePayloadHash()
	if err := s.forceWriteJSON(testStateDir, approved); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	result := &state.DeliveryPayload{StartedAt: now, FinishedAt: now.Add(time.Second), ExecutedRevision: approved.Delivery.MergedSHA}
	verifiedAssertions := DeliveryReconcileAssertions{RunnerGone: true, ExpectedConfigDigest: reconcileTestDigest}
	if _, err := s.ReconcileDelivery(context.Background(), testStateDir, approved.ID, state.DeliveryReconcileOutcomeVerified, result, verifiedAssertions, now); !errors.Is(err, state.ErrApprovalNotExecuting) {
		t.Fatalf("approved-row reconcile err = %v", err)
	}

	claimed, _ := s.ClaimDeliveryExecuting(context.Background(), testStateDir, approved.ID, reconcileTestDigest, now, "daemon", "claim")
	if _, err := s.ReconcileDelivery(context.Background(), testStateDir, approved.ID, "unknown", result, verifiedAssertions, now); !errors.Is(err, ErrDeliveryReconcileOutcome) {
		t.Fatalf("unknown outcome err = %v", err)
	}
	wrongRevision := result.Clone()
	wrongRevision.ExecutedRevision = strings.Repeat("b", 40)
	if _, err := s.ReconcileDelivery(context.Background(), testStateDir, approved.ID, state.DeliveryReconcileOutcomeVerified, wrongRevision, verifiedAssertions, now); !errors.Is(err, ErrDeliveryIntegrity) {
		t.Fatalf("wrong revision err = %v", err)
	}
	got, err := s.Get(context.Background(), testStateDir, claimed.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != state.ApprovalStatusExecuting || got.Delivery.CompletionSource != "" {
		t.Fatalf("invalid reconcile changed lease: %+v", got)
	}
}

func TestReconcileDeliveryStoreEnforcesAssertionsAndCurrentDigest(t *testing.T) {
	s := openTestStore(t)
	claimed, now := seedExecutingReconcileDelivery(t, s, "reconcile-store-fences")
	verified := &state.DeliveryPayload{
		StartedAt:        now,
		FinishedAt:       now.Add(time.Second),
		ExecutedRevision: claimed.Delivery.MergedSHA,
	}
	if _, err := s.ReconcileDelivery(context.Background(), testStateDir, claimed.ID,
		state.DeliveryReconcileOutcomeVerified, verified,
		DeliveryReconcileAssertions{ExpectedConfigDigest: reconcileTestDigest}, now); !errors.Is(err, ErrDeliveryReconcileRunnerGoneRequired) {
		t.Fatalf("missing runner assertion err = %v", err)
	}
	if _, err := s.ReconcileDelivery(context.Background(), testStateDir, claimed.ID,
		state.DeliveryReconcileOutcomeNotApplied,
		&state.DeliveryPayload{StartedAt: now, FinishedAt: now.Add(time.Second)},
		DeliveryReconcileAssertions{RunnerGone: true}, now); !errors.Is(err, ErrDeliveryReconcileTargetSafeRequired) {
		t.Fatalf("missing target-safe assertion err = %v", err)
	}
	if _, err := s.ReconcileDelivery(context.Background(), testStateDir, claimed.ID,
		state.DeliveryReconcileOutcomeVerified, verified,
		DeliveryReconcileAssertions{RunnerGone: true, ExpectedConfigDigest: "sha256:drifted"}, now); !errors.Is(err, ErrDeliveryConfigMismatch) {
		t.Fatalf("config drift err = %v", err)
	}
	got, err := s.Get(context.Background(), testStateDir, claimed.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != state.ApprovalStatusExecuting || got.Delivery.CompletionSource != "" {
		t.Fatalf("store fence changed executing lease: %+v", got)
	}
}

func TestReconcileDeliveryConcurrentTransitionExactlyOnce(t *testing.T) {
	s := openTestStore(t)
	claimed, now := seedExecutingReconcileDelivery(t, s, "reconcile-contention")
	result := &state.DeliveryPayload{
		StartedAt:        now,
		FinishedAt:       now.Add(time.Second),
		ExecutedRevision: claimed.Delivery.MergedSHA,
	}
	const contenders = 12
	var wins atomic.Int64
	var expectedLosses atomic.Int64
	var unexpected atomic.Int64
	var wg sync.WaitGroup
	for range contenders {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := s.ReconcileDelivery(context.Background(), testStateDir, claimed.ID,
				state.DeliveryReconcileOutcomeVerified, result,
				DeliveryReconcileAssertions{RunnerGone: true, ExpectedConfigDigest: reconcileTestDigest}, now.Add(2*time.Second))
			switch {
			case err == nil:
				wins.Add(1)
			case errors.Is(err, state.ErrApprovalNotExecuting):
				expectedLosses.Add(1)
			default:
				unexpected.Add(1)
			}
		}()
	}
	wg.Wait()
	if wins.Load() != 1 || expectedLosses.Load() != contenders-1 || unexpected.Load() != 0 {
		t.Fatalf("wins=%d expected_losses=%d unexpected=%d", wins.Load(), expectedLosses.Load(), unexpected.Load())
	}
	var auditCount int
	if err := s.db.QueryRowContext(context.Background(), `
SELECT count(*) FROM approval_audit
WHERE state_dir = ? AND approval_id = ? AND event = ?`, testStateDir, claimed.ID, state.ApprovalAuditDeliveryReconciled).Scan(&auditCount); err != nil {
		t.Fatal(err)
	}
	if auditCount != 1 {
		t.Fatalf("reconcile audit rows = %d, want 1", auditCount)
	}
}

func TestFinishDeliveryCannotForgeOperatorReconciliation(t *testing.T) {
	tests := []struct {
		name    string
		success bool
		outcome string
	}{
		{name: "verified", success: true, outcome: state.DeliveryReconcileOutcomeVerified},
		{name: "not-applied", outcome: state.DeliveryReconcileOutcomeNotApplied},
		{name: "remediated-failed", outcome: state.DeliveryReconcileOutcomeRemediatedFailed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := openTestStore(t)
			claimed, now := seedExecutingReconcileDelivery(t, s, "forged-reconcile-"+tt.name)
			result := &state.DeliveryPayload{
				StartedAt:        now,
				FinishedAt:       now.Add(time.Second),
				CompletionSource: state.DeliveryCompletionSourceOperatorReconcile,
				ReconcileOutcome: tt.outcome,
			}
			if tt.success {
				zero := 0
				result.ExecutedRevision = claimed.Delivery.MergedSHA
				result.VerifyExitCode = &zero
				result.Verified = true
			}
			if _, err := s.FinishDelivery(context.Background(), testStateDir, claimed.ID, tt.success,
				result, now.Add(2*time.Second), "internal-caller", "forged"); !errors.Is(err, ErrDeliveryIntegrity) {
				t.Fatalf("forged reconciliation err = %v, want ErrDeliveryIntegrity", err)
			}
			got, err := s.Get(context.Background(), testStateDir, claimed.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got.Status != state.ApprovalStatusExecuting || got.Delivery.CompletionSource != "" {
				t.Fatalf("forged reconciliation changed lease: %+v", got)
			}
		})
	}
}
