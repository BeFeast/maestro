package state

import (
	"strings"
	"testing"
	"time"
)

func deliveryPayload(sha string) DeliveryPayload {
	return DeliveryPayload{
		Project:        "owner/app",
		Repo:           "owner/app",
		PR:             12,
		MergedSHA:      sha,
		LocalPath:      "/srv/app",
		Target:         "prod web",
		CommandPreview: "./deploy.sh",
		Rollback:       "helm rollback app",
		TimeoutMinutes: 15,
	}
}

// A fresh delivery mints one pending deploy_project approval carrying the exact
// merged revision and operator-safe context.
func TestRecordDeliveryApproval_MintsPending(t *testing.T) {
	s := NewState()
	now := time.Now().UTC()
	a := s.RecordDeliveryApproval(deliveryPayload("abc123def456abc123def456abc123def456abcd"), now)
	if a == nil {
		t.Fatal("RecordDeliveryApproval returned nil")
	}
	if a.Status != ApprovalStatusPending {
		t.Fatalf("status = %q, want pending", a.Status)
	}
	if a.Action != ApprovalActionDeployProject {
		t.Fatalf("action = %q, want deploy_project", a.Action)
	}
	if a.Delivery == nil || a.Delivery.MergedSHA != "abc123def456abc123def456abc123def456abcd" {
		t.Fatalf("delivery payload missing merged SHA: %+v", a.Delivery)
	}
	if a.Delivery.Target != "prod web" || a.Delivery.Rollback != "helm rollback app" {
		t.Fatalf("operator-safe context missing: %+v", a.Delivery)
	}
	if a.PayloadHash == "" || a.PayloadHash != a.ComputePayloadHash() {
		t.Fatalf("payload hash not self-consistent")
	}
	if len(s.Approvals) != 1 {
		t.Fatalf("approvals = %d, want 1", len(s.Approvals))
	}
}

// Re-processing the same merge (daemon restart replaying handleMergedPR) is
// idempotent — the same pending approval is returned, not a duplicate.
func TestRecordDeliveryApproval_IdempotentSameSHA(t *testing.T) {
	s := NewState()
	now := time.Now().UTC()
	p := deliveryPayload("sha-A")
	a1 := s.RecordDeliveryApproval(p, now)
	a2 := s.RecordDeliveryApproval(p, now.Add(time.Minute))
	if a1.ID != a2.ID {
		t.Fatalf("ids differ on replay: %q vs %q", a1.ID, a2.ID)
	}
	if len(s.Approvals) != 1 {
		t.Fatalf("approvals = %d, want 1 (idempotent)", len(s.Approvals))
	}
}

// A newer merge supersedes the stale pending so the old revision can never be
// approved into a deploy, with a visible audit entry.
func TestRecordDeliveryApproval_SupersedesStalePending(t *testing.T) {
	s := NewState()
	now := time.Now().UTC()
	old := s.RecordDeliveryApproval(deliveryPayload("sha-OLD"), now)
	oldID := old.ID
	fresh := s.RecordDeliveryApproval(deliveryPayload("sha-NEW"), now.Add(time.Minute))
	if fresh.ID == oldID {
		t.Fatal("new merge should mint a distinct approval id")
	}
	staled, ok := s.FindApproval(oldID)
	if !ok {
		t.Fatal("old approval vanished")
	}
	if staled.Status != ApprovalStatusSuperseded {
		t.Fatalf("old status = %q, want superseded", staled.Status)
	}
	last := staled.Audit[len(staled.Audit)-1]
	if last.Event != ApprovalAuditSuperseded || !strings.Contains(last.Reason, "newer merge") {
		t.Fatalf("audit = %+v, want a superseded-by-newer-merge entry", last)
	}
	if fresh.Status != ApprovalStatusPending {
		t.Fatalf("fresh status = %q, want pending", fresh.Status)
	}
}

// An already-approved delivery for an older revision is also superseded by a
// newer merge (require a fresh approval for the latest revision).
func TestRecordDeliveryApproval_SupersedesApproved(t *testing.T) {
	s := NewState()
	now := time.Now().UTC()
	old := s.RecordDeliveryApproval(deliveryPayload("sha-OLD"), now)
	if _, err := s.ApproveApproval(old.ID, now, "op", "go"); err != nil {
		t.Fatalf("approve: %v", err)
	}
	s.RecordDeliveryApproval(deliveryPayload("sha-NEW"), now.Add(time.Minute))
	staled, _ := s.FindApproval(old.ID)
	if staled.Status != ApprovalStatusSuperseded {
		t.Fatalf("approved-old status = %q, want superseded", staled.Status)
	}
}

// A completed delivery is never resurrected to pending by a replay of the same
// revision.
func TestRecordDeliveryApproval_TerminalNotResurrected(t *testing.T) {
	s := NewState()
	now := time.Now().UTC()
	a := s.RecordDeliveryApproval(deliveryPayload("sha-A"), now)
	a.Status = ApprovalStatusExecuted
	again := s.RecordDeliveryApproval(deliveryPayload("sha-A"), now.Add(time.Minute))
	if again.Status != ApprovalStatusExecuted {
		t.Fatalf("status = %q, want executed (not resurrected)", again.Status)
	}
	if len(s.Approvals) != 1 {
		t.Fatalf("approvals = %d, want 1", len(s.Approvals))
	}
}

// A different merged SHA changes the payload hash so the two are genuinely
// distinct payloads (not a hash collision that dedup would coalesce).
func TestDeliveryPayloadHash_ChangesWithSHA(t *testing.T) {
	a := Approval{Action: ApprovalActionDeployProject}
	pa := deliveryPayload("sha-A")
	pb := deliveryPayload("sha-B")
	a.Delivery = &pa
	ha := a.ComputePayloadHash()
	a.Delivery = &pb
	hb := a.ComputePayloadHash()
	if ha == hb {
		t.Fatal("payload hash must change with the merged SHA")
	}
}

// Recording the execution result (mutable fields) must NOT change the payload
// hash — otherwise the currency guard would stale the approval mid-flight.
func TestDeliveryPayloadHash_StableAcrossResult(t *testing.T) {
	p := deliveryPayload("sha-A")
	a := Approval{Action: ApprovalActionDeployProject, Delivery: &p}
	before := a.ComputePayloadHash()
	p.Output = "deploy log"
	p.Verified = true
	p.FinishedAt = time.Now().UTC()
	after := a.ComputePayloadHash()
	if before != after {
		t.Fatal("recording a run must not drift the payload hash")
	}
}

func TestMarkApprovalExecuting_ClaimsApproved(t *testing.T) {
	s := NewState()
	now := time.Now().UTC()
	a := s.RecordDeliveryApproval(deliveryPayload("sha-A"), now)
	if _, err := s.ApproveApproval(a.ID, now, "op", "go"); err != nil {
		t.Fatalf("approve: %v", err)
	}
	claimed, err := s.MarkApprovalExecuting(a.ID, now, "daemon", "claim")
	if err != nil {
		t.Fatalf("MarkApprovalExecuting: %v", err)
	}
	if claimed.Status != ApprovalStatusExecuting {
		t.Fatalf("status = %q, want executing", claimed.Status)
	}
	// Second claim (restart) is refused — non-replay.
	if _, err := s.MarkApprovalExecuting(a.ID, now, "daemon", "claim"); err != ErrApprovalNotApproved {
		t.Fatalf("second claim err = %v, want ErrApprovalNotApproved", err)
	}
}

func TestRecordDeliveryResult_ExecutingToTerminal(t *testing.T) {
	s := NewState()
	now := time.Now().UTC()
	a := s.RecordDeliveryApproval(deliveryPayload("sha-A"), now)
	_, _ = s.ApproveApproval(a.ID, now, "op", "go")
	_, _ = s.MarkApprovalExecuting(a.ID, now, "daemon", "claim")

	res := a.Delivery.Clone()
	res.Verified = true
	res.Output = "ok"
	done, err := s.RecordDeliveryResult(a.ID, true, res, now, "daemon", "delivered")
	if err != nil {
		t.Fatalf("RecordDeliveryResult: %v", err)
	}
	if done.Status != ApprovalStatusExecuted {
		t.Fatalf("status = %q, want executed", done.Status)
	}
	if !done.Delivery.Verified || done.Delivery.Output != "ok" {
		t.Fatalf("result not folded: %+v", done.Delivery)
	}
	// Idempotent: a second record on a non-executing row is refused.
	if _, err := s.RecordDeliveryResult(a.ID, true, res, now, "daemon", "x"); err != ErrApprovalNotExecuting {
		t.Fatalf("second record err = %v, want ErrApprovalNotExecuting", err)
	}
}

func TestListExecutingDeliveries(t *testing.T) {
	s := NewState()
	now := time.Now().UTC()
	a := s.RecordDeliveryApproval(deliveryPayload("sha-A"), now)
	_, _ = s.ApproveApproval(a.ID, now, "op", "go")
	if got := s.ListExecutingDeliveries(); len(got) != 0 {
		t.Fatalf("executing = %d, want 0 before claim", len(got))
	}
	_, _ = s.MarkApprovalExecuting(a.ID, now, "daemon", "claim")
	got := s.ListExecutingDeliveries()
	if len(got) != 1 || got[0].ID != a.ID {
		t.Fatalf("executing = %v, want [%s]", got, a.ID)
	}
}

func TestSanitizeDeliveryOutput_RedactsSecrets(t *testing.T) {
	cases := []struct {
		raw    string
		absent string
	}{
		{"token=ghp_abcdefghijklmnopqrstuvwxyz0123456789", "ghp_abcdefghijklmnopqrstuvwxyz0123456789"},
		{"API_KEY: sk-secret-value-xyz", "sk-secret-value-xyz"},
		{"Authorization: Bearer aaaaaaaaaaaaaaaaaaaa", "aaaaaaaaaaaaaaaaaaaa"},
		{"password = hunter2hunter2", "hunter2hunter2"},
		{"AKIAIOSFODNN7EXAMPLE stray", "AKIAIOSFODNN7EXAMPLE"},
	}
	for _, tc := range cases {
		got := SanitizeDeliveryOutput(tc.raw, 0)
		if strings.Contains(got, tc.absent) {
			t.Fatalf("SanitizeDeliveryOutput(%q) = %q, still contains secret", tc.raw, got)
		}
		if !strings.Contains(got, "[REDACTED]") {
			t.Fatalf("SanitizeDeliveryOutput(%q) = %q, want a redaction marker", tc.raw, got)
		}
	}
}

func TestSanitizeDeliveryOutput_Bounds(t *testing.T) {
	raw := strings.Repeat("x", 100)
	got := SanitizeDeliveryOutput(raw, 20)
	if !strings.Contains(got, "truncated") {
		t.Fatalf("expected truncation marker, got %q", got)
	}
	if len(got) > 20+40 {
		t.Fatalf("bounded output too long: %d", len(got))
	}
}
