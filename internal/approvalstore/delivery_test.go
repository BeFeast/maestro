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

func pendingDelivery(id, sha string, pr int, mergedAt, created time.Time) *state.Approval {
	a := &state.Approval{
		ID:        id,
		CreatedAt: created,
		UpdatedAt: created,
		Action:    state.ApprovalActionDeployProject,
		Status:    state.ApprovalStatusPending,
		Repo:      "owner/app",
		Project:   "owner/app",
		Delivery: &state.DeliveryPayload{
			Project: "owner/app", Repo: "owner/app", PR: pr,
			MergedSHA: sha, MergedAt: mergedAt,
			ConfigDigest: "sha256:spec",
		},
	}
	a.PayloadHash = a.ComputePayloadHash()
	return a
}

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
		Delivery:  &state.DeliveryPayload{Project: "owner/app", Repo: "owner/app", MergedSHA: sha},
	}
	a.PayloadHash = a.ComputePayloadHash()
	b := RowBinding{Project: "owner/app", Repo: "owner/app", StateDir: testStateDir}
	if _, err := s.Put(context.Background(), a, b); err != nil {
		t.Fatalf("put: %v", err)
	}
	return a
}

func successfulDeliveryResult(p *state.DeliveryPayload, started, finished time.Time) *state.DeliveryPayload {
	result := p.Clone()
	zero := 0
	result.StartedAt = started
	result.FinishedAt = finished
	result.ExecutedRevision = result.MergedSHA
	result.DeployExitCode = &zero
	result.VerifyExitCode = &zero
	result.Verified = true
	return result
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

func TestReleaseDeliveryExecuting_AllowsTransientPreSideEffectRetry(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	a := seedApprovedDelivery(t, s, "approval-delivery-release", strings.Repeat("a", 40))
	if _, err := s.ClaimExecuting(ctx, testStateDir, a.ID, now, "daemon", "claim"); err != nil {
		t.Fatal(err)
	}
	released, err := s.ReleaseDeliveryExecuting(ctx, testStateDir, a.ID, now.Add(time.Second), "daemon")
	if err != nil {
		t.Fatal(err)
	}
	if released.Status != state.ApprovalStatusApproved || released.Audit[len(released.Audit)-1].Event != state.ApprovalAuditExecutionReleased ||
		released.Audit[len(released.Audit)-1].Reason != "delivery claim released before side effect" {
		t.Fatalf("released approval = %+v", released)
	}
	if _, err := s.ReleaseDeliveryExecuting(ctx, testStateDir, a.ID, now.Add(2*time.Second), "daemon"); !errors.Is(err, state.ErrApprovalNotExecuting) {
		t.Fatalf("second release err = %v, want ErrApprovalNotExecuting", err)
	}
	if _, err := s.ClaimExecuting(ctx, testStateDir, a.ID, now.Add(3*time.Second), "daemon", "retry"); err != nil {
		t.Fatalf("retry claim: %v", err)
	}
}

func TestClaimDeliveryExecuting_ProjectLeaseSerializesGenerations(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	a := seedApprovedDelivery(t, s, "approval-generation-a", "sha-A")
	if _, err := s.ClaimExecuting(ctx, testStateDir, a.ID, now, "daemon-a", "claim A"); err != nil {
		t.Fatalf("claim A: %v", err)
	}

	// A newer generation may be minted and approved while A is still running,
	// but it must not start until A releases the project execution lease.
	b := pendingDelivery("approval-generation-b", "sha-B", 22, now.Add(time.Second), now.Add(time.Second))
	binding := RowBinding{Project: "owner/app", Repo: "owner/app", StateDir: testStateDir}
	if _, err := s.PutDelivery(ctx, b, binding, now.Add(time.Second)); err != nil {
		t.Fatalf("put B: %v", err)
	}
	if _, err := s.Approve(ctx, testStateDir, b.ID, now.Add(2*time.Second), "operator", "approve B"); err != nil {
		t.Fatalf("approve B: %v", err)
	}
	if _, err := s.ClaimExecuting(ctx, testStateDir, b.ID, now.Add(3*time.Second), "daemon-b", "claim B"); !errors.Is(err, ErrDeliveryInFlight) {
		t.Fatalf("claim B while A executes err = %v, want ErrDeliveryInFlight", err)
	}
	stillApproved, err := s.Get(ctx, testStateDir, b.ID)
	if err != nil {
		t.Fatalf("get B: %v", err)
	}
	if stillApproved.Status != state.ApprovalStatusApproved {
		t.Fatalf("B status = %q, want approved while lease is held", stillApproved.Status)
	}

	result := successfulDeliveryResult(a.Delivery, now.Add(time.Second), now.Add(4*time.Second))
	if _, err := s.FinishDelivery(ctx, testStateDir, a.ID, true, result, now.Add(4*time.Second), "daemon-a", "A done"); err != nil {
		t.Fatalf("finish A: %v", err)
	}
	if _, err := s.ClaimExecuting(ctx, testStateDir, b.ID, now.Add(5*time.Second), "daemon-b", "claim B"); err != nil {
		t.Fatalf("claim B after A finished: %v", err)
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
	finishAt := time.Now().UTC()
	res := successfulDeliveryResult(claimed.Delivery, finishAt.Add(-time.Second), finishAt)
	done, err := s.FinishDelivery(ctx, testStateDir, "approval-deploy-4", true, res, time.Now().UTC(), "daemon", "delivered")
	if err != nil {
		t.Fatalf("finish: %v", err)
	}
	if done.Status != state.ApprovalStatusExecuted {
		t.Fatalf("status = %q, want executed", done.Status)
	}
	if done.Delivery == nil || !done.Delivery.Verified || done.Delivery.DeployExitCode == nil || *done.Delivery.DeployExitCode != 0 {
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
	failure := &state.DeliveryPayload{
		FinishedAt:   time.Now().UTC(),
		FailureStage: state.DeliveryFailureStagePrecondition,
	}
	done, err := s.FinishDelivery(ctx, testStateDir, "approval-deploy-5", false, failure, time.Now().UTC(), "daemon", "verifier failed")
	if err != nil {
		t.Fatalf("finish: %v", err)
	}
	if done.Status != state.ApprovalStatusExecutionFailed {
		t.Fatalf("status = %q, want execution_failed", done.Status)
	}
}

func TestPutDeliveryCanonicalizesFreeTextAndPreservesAuthenticatedActor(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	secret := "opaque-sensitive-value"
	a := pendingDelivery("delivery-canonical", strings.Repeat("a", 40), 12, now, now)
	a.Summary = "summary " + secret
	a.Risk = "risk " + secret
	a.Evidence = []string{"evidence " + secret}
	a.Target = &state.SupervisorTarget{PR: 12, Body: "body " + secret}
	a.Audit = []state.ApprovalAudit{{
		At: now, Event: state.ApprovalAuditCreated, Actor: "maestro", Reason: "reason " + secret,
	}}
	a.PayloadHash = a.ComputePayloadHash()
	if _, err := s.PutDelivery(ctx, a, RowBinding{Project: "owner/app", Repo: "owner/app", StateDir: testStateDir}, now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Approve(ctx, testStateDir, a.ID, now.Add(time.Second), "oleg", "approve "+secret); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get(ctx, testStateDir, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(got)
	if strings.Contains(string(encoded), secret) {
		t.Fatalf("free text survived canonical delivery write: %s", encoded)
	}
	var actor, reason string
	if err := s.db.QueryRowContext(ctx, `SELECT actor, reason FROM approval_audit WHERE state_dir = ? AND approval_id = ? AND event = ?`, testStateDir, a.ID, state.ApprovalAuditApproved).Scan(&actor, &reason); err != nil {
		t.Fatal(err)
	}
	if actor != "oleg" || reason != "delivery approved" {
		t.Fatalf("authenticated actor or canonical reason lost: actor=%q reason=%q", actor, reason)
	}
}

// ClaimExecuting on a still-pending (never approved) delivery is refused —
// approval-required mode runs zero delivery before approval.
func TestClaimExecuting_PendingRefused(t *testing.T) {
	s := openTestStore(t)
	now := time.Now().UTC()
	a := pendingDelivery("approval-deploy-6", strings.Repeat("a", 40), 6, now, now)
	if _, err := s.PutDelivery(context.Background(), a, RowBinding{Project: "owner/app", Repo: "owner/app", StateDir: testStateDir}, now); err != nil {
		t.Fatal(err)
	}
	_, err := s.ClaimExecuting(context.Background(), testStateDir, "approval-deploy-6", time.Now().UTC(), "daemon", "claim")
	if err != state.ErrApprovalNotApproved {
		t.Fatalf("claim on pending err = %v, want ErrApprovalNotApproved", err)
	}
}

func TestPutDelivery_OlderLateEnsureCannotSupersedeNewer(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	b := RowBinding{Project: "owner/app", Repo: "owner/app", StateDir: testStateDir}
	now := time.Now().UTC()
	newer := pendingDelivery("delivery-new", "sha-NEW", 22, now, now)
	older := pendingDelivery("delivery-old", "sha-OLD", 21, now.Add(-time.Hour), now.Add(time.Minute))
	if _, err := s.PutDelivery(ctx, newer, b, now); err != nil {
		t.Fatalf("put newer: %v", err)
	}
	if _, err := s.PutDelivery(ctx, older, b, now.Add(time.Minute)); err != nil {
		t.Fatalf("put late older: %v", err)
	}
	gotNew, _ := s.Get(ctx, testStateDir, newer.ID)
	gotOld, _ := s.Get(ctx, testStateDir, older.ID)
	if gotNew.Status != state.ApprovalStatusPending || gotOld.Status != state.ApprovalStatusSuperseded {
		t.Fatalf("statuses newer/older = %q/%q, want pending/superseded", gotNew.Status, gotOld.Status)
	}
	// A late JSON→SQLite ensure of the already-present old id is a strict no-op.
	older.Status = state.ApprovalStatusPending
	if inserted, err := s.PutDelivery(ctx, older, b, now.Add(2*time.Minute)); err != nil || inserted {
		t.Fatalf("late ensure inserted=%v err=%v, want no-op", inserted, err)
	}
	gotNew, _ = s.Get(ctx, testStateDir, newer.ID)
	if gotNew.Status != state.ApprovalStatusPending {
		t.Fatalf("newer status after late ensure = %q, want pending", gotNew.Status)
	}
}

func TestPutDelivery_SameTimestampDifferentSHAsRemainActionableForTopologyFence(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	binding := RowBinding{Project: "owner/app", Repo: "owner/app", StateDir: testStateDir}
	now := time.Now().UTC()
	first := pendingDelivery("delivery-same-second-99", strings.Repeat("d", 40), 99, now, now)
	second := pendingDelivery("delivery-same-second-1", strings.Repeat("e", 40), 1, now, now.Add(time.Second))
	if _, err := s.PutDelivery(ctx, first, binding, now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutDelivery(ctx, second, binding, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	gotFirst, err := s.Get(ctx, testStateDir, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	gotSecond, err := s.Get(ctx, testStateDir, second.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotFirst.Status != state.ApprovalStatusPending || gotSecond.Status != state.ApprovalStatusPending {
		t.Fatalf("same-timestamp statuses = %q/%q, want both pending until ancestry proof", gotFirst.Status, gotSecond.Status)
	}
}

func TestPutDelivery_OlderLateMintBlockedByTerminalNewer(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	b := RowBinding{Project: "owner/app", Repo: "owner/app", StateDir: testStateDir}
	now := time.Now().UTC()
	newer := pendingDelivery("delivery-new-terminal", "sha-NEW", 22, now, now)
	newer.Status = state.ApprovalStatusExecuted
	newer.Delivery = successfulDeliveryResult(newer.Delivery, now.Add(-time.Minute), now)
	older := pendingDelivery("delivery-old-late", "sha-OLD", 21, now.Add(-time.Hour), now.Add(time.Minute))
	if _, err := s.PutDelivery(ctx, newer, b, now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutDelivery(ctx, older, b, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	gotOld, _ := s.Get(ctx, testStateDir, older.ID)
	if gotOld.Status != state.ApprovalStatusSuperseded {
		t.Fatalf("late older status = %q, want superseded", gotOld.Status)
	}
}

func TestPutDelivery_NewerAtomicallySupersedesApprovedOlder(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	b := RowBinding{Project: "owner/app", Repo: "owner/app", StateDir: testStateDir}
	now := time.Now().UTC()
	older := pendingDelivery("delivery-old", "sha-OLD", 21, now.Add(-time.Hour), now.Add(-time.Hour))
	newer := pendingDelivery("delivery-new", "sha-NEW", 22, now, now)
	if _, err := s.PutDelivery(ctx, older, b, now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Approve(ctx, testStateDir, older.ID, now.Add(-time.Minute), "op", "go"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutDelivery(ctx, newer, b, now); err != nil {
		t.Fatal(err)
	}
	gotOld, _ := s.Get(ctx, testStateDir, older.ID)
	gotNew, _ := s.Get(ctx, testStateDir, newer.ID)
	if gotOld.Status != state.ApprovalStatusSuperseded || gotNew.Status != state.ApprovalStatusPending {
		t.Fatalf("statuses old/new = %q/%q", gotOld.Status, gotNew.Status)
	}
	if _, err := s.ClaimExecuting(ctx, testStateDir, older.ID, now, "daemon", "claim"); !errors.Is(err, state.ErrApprovalNotApproved) {
		t.Fatalf("superseded old claim err = %v", err)
	}
}

func TestClaimDeliveryExecuting_ConfigMismatchDurablyStales(t *testing.T) {
	s := openTestStore(t)
	a := seedApprovedDelivery(t, s, "delivery-drift", "sha-A")
	a.Delivery.ConfigDigest = "sha256:approved"
	a.PayloadHash = a.ComputePayloadHash()
	if err := s.forceWriteJSON(testStateDir, a); err != nil {
		t.Fatal(err)
	}
	_, err := s.ClaimDeliveryExecuting(context.Background(), testStateDir, a.ID, "sha256:current", time.Now().UTC(), "daemon", "claim")
	if !errors.Is(err, ErrDeliveryConfigMismatch) {
		t.Fatalf("claim err = %v, want config mismatch", err)
	}
	got, _ := s.Get(context.Background(), testStateDir, a.ID)
	if got.Status != state.ApprovalStatusStale {
		t.Fatalf("status = %q, want stale", got.Status)
	}
}

func TestDeliveryIntegrityTamperedRevisionFailsClosed(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	a := seedApprovedDelivery(t, s, "delivery-tampered", strings.Repeat("a", 40))
	canonicalDir, err := canonicalStateDir(testStateDir)
	if err != nil {
		t.Fatal(err)
	}
	var blob string
	if err := s.db.QueryRowContext(ctx, `SELECT approval_json FROM approvals WHERE state_dir = ? AND id = ?`, canonicalDir, a.ID).Scan(&blob); err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal([]byte(blob), &raw); err != nil {
		t.Fatal(err)
	}
	delivery := raw["delivery"].(map[string]any)
	delivery["merged_sha"] = strings.Repeat("b", 40)
	tampered, _ := json.Marshal(raw)
	if _, err := s.db.ExecContext(ctx, `UPDATE approvals SET approval_json = ? WHERE state_dir = ? AND id = ?`, string(tampered), canonicalDir, a.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(ctx, testStateDir, a.ID); !errors.Is(err, ErrDeliveryIntegrity) {
		t.Fatalf("Get tampered err = %v, want ErrDeliveryIntegrity", err)
	}
	if _, err := s.ClaimExecuting(ctx, testStateDir, a.ID, time.Now().UTC(), "daemon", "claim"); !errors.Is(err, ErrDeliveryIntegrity) {
		t.Fatalf("Claim tampered err = %v, want ErrDeliveryIntegrity", err)
	}
	var status string
	if err := s.db.QueryRowContext(ctx, `SELECT status FROM approvals WHERE state_dir = ? AND id = ?`, canonicalDir, a.ID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != string(state.ApprovalStatusApproved) {
		t.Fatalf("tampered approval transitioned to %q", status)
	}
}

func TestDeliveryIntegritySQLBindingTamperFailsClosed(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	a := seedApprovedDelivery(t, s, "delivery-binding-tampered", strings.Repeat("a", 40))
	canonicalDir, err := canonicalStateDir(testStateDir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE approvals SET repo = ? WHERE state_dir = ? AND id = ?`, "other/repo", canonicalDir, a.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(ctx, testStateDir, a.ID); !errors.Is(err, ErrDeliveryIntegrity) {
		t.Fatalf("Get binding-tampered err = %v, want ErrDeliveryIntegrity", err)
	}
	if _, err := s.ClaimExecuting(ctx, testStateDir, a.ID, time.Now().UTC(), "daemon", "claim"); !errors.Is(err, ErrDeliveryIntegrity) {
		t.Fatalf("Claim binding-tampered err = %v, want ErrDeliveryIntegrity", err)
	}
}

func TestDeliveryIntegritySQLStatusAndTerminalResultTamperFailClosed(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	a := seedApprovedDelivery(t, s, "delivery-result-tampered", strings.Repeat("a", 40))
	canonicalDir, err := canonicalStateDir(testStateDir)
	if err != nil {
		t.Fatal(err)
	}

	// The denormalized SQL status is an independent transition fence. Changing
	// only approval_json from approved to a plausible-looking executed result
	// must not let completion consumers trust it.
	var blob string
	if err := s.db.QueryRowContext(ctx, `SELECT approval_json FROM approvals WHERE state_dir = ? AND id = ?`, canonicalDir, a.ID).Scan(&blob); err != nil {
		t.Fatal(err)
	}
	var raw state.Approval
	if err := json.Unmarshal([]byte(blob), &raw); err != nil {
		t.Fatal(err)
	}
	raw.Status = state.ApprovalStatusExecuted
	raw.Delivery = successfulDeliveryResult(raw.Delivery, now.Add(-time.Second), now)
	tampered, err := json.Marshal(&raw)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE approvals SET approval_json = ? WHERE state_dir = ? AND id = ?`, string(tampered), canonicalDir, a.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(ctx, testStateDir, a.ID); !errors.Is(err, ErrDeliveryIntegrity) {
		t.Fatalf("JSON status/result tamper err = %v, want ErrDeliveryIntegrity", err)
	}

	// Restore the valid JSON, then tamper the SQL status alone. The reverse
	// mismatch is rejected by the same read/claim gate.
	valid, err := json.Marshal(state.CanonicalDeliveryApprovalForWrite(a))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE approvals SET approval_json = ?, status = ? WHERE state_dir = ? AND id = ?`,
		string(valid), string(state.ApprovalStatusExecuted), canonicalDir, a.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(ctx, testStateDir, a.ID); !errors.Is(err, ErrDeliveryIntegrity) {
		t.Fatalf("SQL status tamper err = %v, want ErrDeliveryIntegrity", err)
	}
}

func TestDeliveryIntegrityRecordHashCoversStructuredResultAndIsMandatory(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	a := seedApprovedDelivery(t, s, "delivery-record-hash", strings.Repeat("b", 40))
	if _, err := s.ClaimExecuting(ctx, testStateDir, a.ID, now, "daemon", "claim"); err != nil {
		t.Fatal(err)
	}
	result := successfulDeliveryResult(a.Delivery, now, now.Add(time.Second))
	if _, err := s.FinishDelivery(ctx, testStateDir, a.ID, true, result, now.Add(time.Second), "daemon", "done"); err != nil {
		t.Fatal(err)
	}
	canonicalDir, err := canonicalStateDir(testStateDir)
	if err != nil {
		t.Fatal(err)
	}
	var blob string
	if err := s.db.QueryRowContext(ctx, `SELECT approval_json FROM approvals WHERE state_dir = ? AND id = ?`, canonicalDir, a.ID).Scan(&blob); err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal([]byte(blob), &raw); err != nil {
		t.Fatal(err)
	}
	delivery := raw["delivery"].(map[string]any)
	delivery["verified"] = false
	tampered, _ := json.Marshal(raw)
	if _, err := s.db.ExecContext(ctx, `UPDATE approvals SET approval_json = ? WHERE state_dir = ? AND id = ?`, string(tampered), canonicalDir, a.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(ctx, testStateDir, a.ID); !errors.Is(err, ErrDeliveryIntegrity) {
		t.Fatalf("structured-result tamper err = %v, want ErrDeliveryIntegrity", err)
	}

	// A blank record_hash is never grandfathered for delivery rows.
	if _, err := s.db.ExecContext(ctx, `UPDATE approvals SET record_hash = '' WHERE state_dir = ? AND id = ?`, canonicalDir, a.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(ctx, testStateDir, a.ID); !errors.Is(err, ErrDeliveryIntegrity) {
		t.Fatalf("missing record_hash err = %v, want ErrDeliveryIntegrity", err)
	}
}

func TestClaimExecuting_ExpiredDurablyStales(t *testing.T) {
	s := openTestStore(t)
	a := seedApprovedDelivery(t, s, "delivery-expired", "sha-A")
	a.Delivery.ExpiresAt = time.Now().UTC().Add(-time.Minute)
	a.PayloadHash = a.ComputePayloadHash()
	if err := s.forceWriteJSON(testStateDir, a); err != nil {
		t.Fatal(err)
	}
	_, err := s.ClaimExecuting(context.Background(), testStateDir, a.ID, time.Now().UTC(), "daemon", "claim")
	if !errors.Is(err, state.ErrApprovalStale) {
		t.Fatalf("claim err = %v, want stale", err)
	}
	got, _ := s.Get(context.Background(), testStateDir, a.ID)
	if got.Status != state.ApprovalStatusStale {
		t.Fatalf("status = %q, want stale", got.Status)
	}
}
