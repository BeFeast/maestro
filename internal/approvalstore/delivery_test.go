package approvalstore

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/befeast/maestro/internal/state"
)

// seedApprovedDelivery seeds an approved deploy_project approval bound to
// testStateDir and returns it.
func seedApprovedDelivery(t *testing.T, s *Store, id, sha string) *state.Approval {
	t.Helper()
	now := time.Now().UTC()
	a := &state.Approval{
		ID:        id,
		CreatedAt: now,
		UpdatedAt: now,
		Action:    state.ApprovalActionDeployProject,
		Status:    state.ApprovalStatusApproved,
		Repo:      "owner/app",
		Project:   "owner/app",
		Delivery:  &state.DeliveryPayload{Project: "owner/app", Repo: "owner/app", MergedSHA: sha, LocalPath: "/srv/app"},
	}
	a.PayloadHash = a.ComputePayloadHash()
	b := RowBinding{Project: "owner/app", Repo: "owner/app", StateDir: testStateDir}
	if _, err := s.Put(context.Background(), a, b); err != nil {
		t.Fatalf("put: %v", err)
	}
	return a
}

// The approved→executing claim admits exactly one caller; the loser sees
// ErrApprovalNotApproved and no second claim happens.
func TestClaimExecuting_OnlyOneWinner(t *testing.T) {
	s := openTestStore(t)
	seedApprovedDelivery(t, s, "approval-deploy-1", "sha-A")

	if _, err := s.ClaimExecuting(context.Background(), testStateDir, "approval-deploy-1", time.Now().UTC(), "daemon", "claim"); err != nil {
		t.Fatalf("first claim: %v", err)
	}
	_, err := s.ClaimExecuting(context.Background(), testStateDir, "approval-deploy-1", time.Now().UTC(), "cli", "claim")
	if err != state.ErrApprovalNotApproved {
		t.Fatalf("second claim err = %v, want ErrApprovalNotApproved", err)
	}
}

// Cross-executor contention: N concurrent claimants, exactly one wins.
func TestClaimExecuting_ConcurrentContention(t *testing.T) {
	s := openTestStore(t)
	seedApprovedDelivery(t, s, "approval-deploy-2", "sha-A")

	const n = 8
	var winners int64
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			if _, err := s.ClaimExecuting(context.Background(), testStateDir, "approval-deploy-2", time.Now().UTC(), "c", "claim"); err == nil {
				atomic.AddInt64(&winners, 1)
			}
		}()
	}
	wg.Wait()
	if winners != 1 {
		t.Fatalf("winners = %d, want exactly 1", winners)
	}
}

// A restart observing an executing row must NOT be able to re-claim it (no
// automatic replay of an in-flight delivery).
func TestClaimExecuting_ExecutingNotReplayed(t *testing.T) {
	s := openTestStore(t)
	seedApprovedDelivery(t, s, "approval-deploy-3", "sha-A")
	if _, err := s.ClaimExecuting(context.Background(), testStateDir, "approval-deploy-3", time.Now().UTC(), "daemon", "claim"); err != nil {
		t.Fatalf("claim: %v", err)
	}
	// Simulated restart: a fresh store handle over the same DB still refuses.
	if _, err := s.ClaimExecuting(context.Background(), testStateDir, "approval-deploy-3", time.Now().UTC(), "daemon-2", "claim"); err != state.ErrApprovalNotApproved {
		t.Fatalf("re-claim err = %v, want ErrApprovalNotApproved (no replay)", err)
	}
	got, err := s.Get(context.Background(), testStateDir, "approval-deploy-3")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status != state.ApprovalStatusExecuting {
		t.Fatalf("status = %q, want executing (still awaiting operator reconcile)", got.Status)
	}
}

func TestFinishDelivery_RecordsResult(t *testing.T) {
	s := openTestStore(t)
	seedApprovedDelivery(t, s, "approval-deploy-4", "sha-A")
	ctx := context.Background()
	claimed, err := s.ClaimExecuting(ctx, testStateDir, "approval-deploy-4", time.Now().UTC(), "daemon", "claim")
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	res := claimed.Delivery.Clone()
	res.Verified = true
	res.Output = "deployed"
	res.ExecutedRevision = "sha-A"
	done, err := s.FinishDelivery(ctx, testStateDir, "approval-deploy-4", true, res, time.Now().UTC(), "daemon", "delivered")
	if err != nil {
		t.Fatalf("finish: %v", err)
	}
	if done.Status != state.ApprovalStatusExecuted {
		t.Fatalf("status = %q, want executed", done.Status)
	}
	if done.Delivery == nil || !done.Delivery.Verified || done.Delivery.Output != "deployed" {
		t.Fatalf("result not persisted: %+v", done.Delivery)
	}
	// Idempotent: second finish on a non-executing row is refused.
	if _, err := s.FinishDelivery(ctx, testStateDir, "approval-deploy-4", true, res, time.Now().UTC(), "daemon", "x"); err != state.ErrApprovalNotExecuting {
		t.Fatalf("second finish err = %v, want ErrApprovalNotExecuting", err)
	}
}

func TestFinishDelivery_Failure(t *testing.T) {
	s := openTestStore(t)
	seedApprovedDelivery(t, s, "approval-deploy-5", "sha-A")
	ctx := context.Background()
	if _, err := s.ClaimExecuting(ctx, testStateDir, "approval-deploy-5", time.Now().UTC(), "daemon", "claim"); err != nil {
		t.Fatalf("claim: %v", err)
	}
	done, err := s.FinishDelivery(ctx, testStateDir, "approval-deploy-5", false, nil, time.Now().UTC(), "daemon", "verifier failed")
	if err != nil {
		t.Fatalf("finish: %v", err)
	}
	if done.Status != state.ApprovalStatusExecutionFailed {
		t.Fatalf("status = %q, want execution_failed", done.Status)
	}
}

// ClaimExecuting on a still-pending (never approved) delivery is refused —
// approval-required mode runs zero delivery before approval.
func TestClaimExecuting_PendingRefused(t *testing.T) {
	s := openTestStore(t)
	seedPending(t, s, "approval-deploy-6", state.ApprovalActionDeployProject, nil)
	_, err := s.ClaimExecuting(context.Background(), testStateDir, "approval-deploy-6", time.Now().UTC(), "daemon", "claim")
	if err != state.ErrApprovalNotApproved {
		t.Fatalf("claim on pending err = %v, want ErrApprovalNotApproved", err)
	}
}
