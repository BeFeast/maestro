package approvalstore

import (
	"bytes"
	"context"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/befeast/maestro/internal/state"
)

// remint builds a fresh mint of the SAME decision identity: the id is
// content-addressed on (action, target) so it repeats, but the record is a new
// instance with a strictly newer CreatedAt and a fresh pending status. This is
// what the JSON state produces once the previous instance of the decision is
// gone from state.json and the supervisor re-evaluates the same gate.
func remint(id, action string, target *state.SupervisorTarget, b RowBinding, mintedAt time.Time) *state.Approval {
	a := makeApproval(id, action, target, b)
	a.CreatedAt = mintedAt
	a.UpdatedAt = mintedAt
	a.Audit = []state.ApprovalAudit{{At: mintedAt, Event: state.ApprovalAuditCreated}}
	a.PayloadHash = a.ComputePayloadHash()
	return a
}

// An approval whose CreatedAt was never set must NOT count as a newer mint.
// normalize() maps a zero time to time.Now(), so without an explicit guard such
// a record always looks strictly newer and resets an already-claimed row back
// to pending — re-opening the claim-once gate and allowing a second merge_pr or
// close_issue. Approval.CreatedAt is a plain json field, so a legacy or
// hand-edited state.json decodes it to zero, and Put is exported.
func TestPut_ZeroMintNeverResetsClaimedRow(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	rb := RowBinding{Project: "owner/repo", Repo: "owner/repo", StateDir: testStateDir}
	target := &state.SupervisorTarget{PR: 91}

	seedPending(t, s, "zc1", "merge_pr", target)
	if _, err := s.Approve(ctx, testStateDir, "zc1", time.Now().UTC(), "x", "green"); err != nil {
		t.Fatalf("approve first instance: %v", err)
	}

	zero := makeApproval("zc1", "merge_pr", target, rb)
	zero.CreatedAt = time.Time{}
	zero.UpdatedAt = time.Time{}
	zero.Status = state.ApprovalStatusPending
	zero.PayloadHash = zero.ComputePayloadHash()

	if _, err := s.Put(ctx, zero, rb); err != nil {
		t.Fatalf("put zero-mint: %v", err)
	}

	got, err := s.Get(ctx, testStateDir, "zc1")
	if err != nil {
		t.Fatalf("get after zero-mint: %v", err)
	}
	if got.Status != state.ApprovalStatusApproved {
		t.Fatalf("status = %s, want approved: a zero CreatedAt reset a claimed row and re-opened the gate", got.Status)
	}
}

// A re-minted approval must replace the row, not be swallowed. With the old
// `INSERT OR IGNORE` the row kept the FIRST instance's status forever: the
// mirror reported `approved` for an approval the JSON state had already
// closed, and the fresh pending record could never be claimed.
func TestPut_ReMintReplacesStoredRow(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	rb := RowBinding{Project: "owner/repo", Repo: "owner/repo", StateDir: testStateDir}
	target := &state.SupervisorTarget{PR: 42}

	seedPending(t, s, "rm1", "merge_pr", target)
	if _, err := s.Approve(ctx, testStateDir, "rm1", time.Now().UTC(), "x", "green"); err != nil {
		t.Fatalf("approve first instance: %v", err)
	}

	fresh := remint("rm1", "merge_pr", target, rb, time.Now().UTC().Add(time.Minute))
	written, err := s.Put(ctx, fresh, rb)
	if err != nil {
		t.Fatalf("put re-mint: %v", err)
	}
	if !written {
		t.Fatalf("re-mint reported written=false: the write was dropped")
	}

	got, err := s.Get(ctx, testStateDir, "rm1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status != state.ApprovalStatusPending {
		t.Fatalf("status = %q after re-mint, want pending (row kept the previous instance's status)", got.Status)
	}
	if !got.CreatedAt.Equal(fresh.CreatedAt) {
		t.Fatalf("created_at = %s after re-mint, want %s", got.CreatedAt, fresh.CreatedAt)
	}
	// The re-minted instance carries its own audit trail into the mirror.
	if n := auditCount(t, s, "rm1"); n != 3 {
		t.Fatalf("audit rows = %d, want 3 (created + approved + re-mint created)", n)
	}
	// The point of the fix: the fresh instance is claimable again. Against a
	// dropped write the row is still `approved` and this returns
	// ErrApprovalNotPending.
	claimed, err := s.Approve(ctx, testStateDir, "rm1", time.Now().UTC(), "oleg", "second cycle")
	if err != nil {
		t.Fatalf("approve re-minted instance: %v", err)
	}
	if claimed.Status != state.ApprovalStatusApproved {
		t.Fatalf("claimed status = %q, want approved", claimed.Status)
	}
}

// Ids are content-addressed on (action, target) only, so two DIFFERENT
// projects gating the same action on the same target mint the SAME id. The
// row identity is scoped by state_dir; a re-mint in one project must not read,
// update, or resurrect the other project's row.
func TestPut_ReMintKeepsCrossProjectRowsIndependent(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	const id = "dup-remint"
	target := &state.SupervisorTarget{PR: 5}
	bindA := RowBinding{Project: "owner/a", Repo: "owner/a", StateDir: "/tmp/remint-a"}
	bindB := RowBinding{Project: "owner/b", Repo: "owner/b", StateDir: "/tmp/remint-b"}

	seedPendingScoped(t, s, bindA, id, "merge_pr", target)
	origB := seedPendingScoped(t, s, bindB, id, "merge_pr", target)
	if _, err := s.Reject(ctx, bindB.StateDir, id, time.Now().UTC(), "y", "no"); err != nil {
		t.Fatalf("reject B: %v", err)
	}

	fresh := remint(id, "merge_pr", target, bindA, time.Now().UTC().Add(time.Minute))
	if _, err := s.Put(ctx, fresh, bindA); err != nil {
		t.Fatalf("put re-mint under A: %v", err)
	}

	gotA, err := s.Get(ctx, bindA.StateDir, id)
	if err != nil {
		t.Fatalf("get A: %v", err)
	}
	if gotA.Status != state.ApprovalStatusPending || gotA.Project != "owner/a" {
		t.Fatalf("A after re-mint = (%q, %q), want (pending, owner/a)", gotA.Status, gotA.Project)
	}

	gotB, err := s.Get(ctx, bindB.StateDir, id)
	if err != nil {
		t.Fatalf("get B: %v", err)
	}
	if gotB.Status != state.ApprovalStatusRejected {
		t.Fatalf("B status = %q after A re-mint, want rejected (cross-project leak)", gotB.Status)
	}
	if gotB.Project != "owner/b" {
		t.Fatalf("B project = %q, want owner/b (A's row leaked into B's scope)", gotB.Project)
	}
	if !gotB.CreatedAt.Equal(origB.CreatedAt) {
		t.Fatalf("B created_at = %s, want %s (A's re-mint rewrote B's row)", gotB.CreatedAt, origB.CreatedAt)
	}
}

// The one case where NOT writing is correct — a re-seed of the mint the row
// already holds, e.g. gate.Put right before a claim — must be an explicit,
// logged branch rather than a silent side effect of the conflict clause.
func TestPut_SameMintKeepsAdvancedRowAndLogs(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	rb := RowBinding{Project: "owner/repo", Repo: "owner/repo", StateDir: testStateDir}

	a := seedPending(t, s, "keep1", "close_issue", &state.SupervisorTarget{Issue: 3})
	if _, err := s.Approve(ctx, testStateDir, "keep1", time.Now().UTC(), "x", ""); err != nil {
		t.Fatalf("approve: %v", err)
	}

	var buf bytes.Buffer
	prevOut, prevFlags := log.Writer(), log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(prevOut)
		log.SetFlags(prevFlags)
	})

	// Same mint (CreatedAt unchanged), still pending in JSON.
	a.Status = state.ApprovalStatusPending
	written, err := s.Put(ctx, a, rb)
	if err != nil {
		t.Fatalf("re-seed: %v", err)
	}
	if written {
		t.Fatalf("re-seed of the same mint reported written=true; the claim must survive")
	}
	got, err := s.Get(ctx, testStateDir, "keep1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status != state.ApprovalStatusApproved {
		t.Fatalf("status = %q after re-seed, want approved (claim must survive)", got.Status)
	}
	logged := buf.String()
	if !strings.Contains(logged, "keeping approval keep1") || !strings.Contains(logged, "stored status=approved") {
		t.Fatalf("keep branch did not log the dropped write; log = %q", logged)
	}
}
