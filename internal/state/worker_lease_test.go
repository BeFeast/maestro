package state

import (
	"strings"
	"testing"
	"time"
)

func TestWorkerLeaseMetadataSurvivesSaveLoad(t *testing.T) {
	dir := t.TempDir()
	st := NewState()
	unit := "maestro-worker-0123456789abcdef0123456789abcdef-g1.service"
	st.Sessions["sup-1"] = &Session{
		IssueNumber: 927, Status: StatusRunning, StartedAt: time.Now().UTC(),
		ProcessLeaseUnit: unit, ProcessLeaseManager: "system",
		WorkerLeaseID: "mw-lease", WorkerLeaseUnit: unit,
		WorkerLeaseScope: "system", WorkerScratchDir: "/var/tmp/maestro-workers/lease",
		WorkerLeaseManifest: "/var/tmp/maestro-workers/lease/lease.json",
	}
	if err := Save(dir, st); err != nil {
		t.Fatal(err)
	}
	got, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	sess := got.Sessions["sup-1"]
	if sess.WorkerLeaseID != "mw-lease" || sess.WorkerLeaseUnit != sess.ProcessLeaseUnit ||
		sess.WorkerLeaseScope != sess.ProcessLeaseManager || sess.WorkerLeaseManifest == "" {
		t.Fatalf("lease metadata lost: %+v", sess)
	}
}

func TestUnreleasedWorkerScratchReceiptPreventsPruning(t *testing.T) {
	st := NewState()
	finished := time.Now().UTC().Add(-48 * time.Hour)
	st.Sessions["sup-1"] = &Session{
		IssueNumber: 927, Status: StatusDone, StartedAt: finished.Add(-time.Hour), FinishedAt: &finished,
		WorkerLeaseID: "mw-retained",
	}
	if removed := st.PruneOldSessions(time.Hour); removed != 0 || st.Sessions["sup-1"] == nil {
		t.Fatalf("unreleased scratch receipt was pruned: removed=%d sessions=%v", removed, st.Sessions)
	}
}

func TestWorkerLeaseAttentionIsOperatorVisible(t *testing.T) {
	sess := &Session{
		Status: StatusFailed, StartedAt: time.Now().UTC().Add(-2 * FleetAttentionTTL),
		WorkerLeaseAttention: "invalid ownership manifest",
	}
	attention := SessionAttentionFor(sess, nil)
	if !attention.NeedsAttention || !strings.Contains(attention.Reason, "invalid ownership manifest") ||
		!strings.Contains(attention.NextAction, "do not delete") {
		t.Fatalf("attention = %+v", attention)
	}
	if !SessionAttentionActionableAt(sess, time.Now().UTC()) {
		t.Fatal("unresolved worker lease ownership must not age out of operator attention")
	}
}

func TestWorkerLeaseReconciliationAttentionSurvivesSaveLoad(t *testing.T) {
	dir := t.TempDir()
	st := NewState()
	reconciledAt := time.Now().UTC()
	st.WorkerLeaseReconciledAt = reconciledAt
	st.WorkerLeaseAttention = []WorkerLeaseAttention{{
		Identity: "ambiguous-a1b2c3d4e5f6", Slot: "sup-1",
		Reason: "invalid ownership manifest", NextAction: "inspect exact lease",
		DetectedAt: reconciledAt.Add(-time.Minute),
	}}
	if err := Save(dir, st); err != nil {
		t.Fatal(err)
	}
	got, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !got.WorkerLeaseReconciledAt.Equal(reconciledAt) || len(got.WorkerLeaseAttention) != 1 ||
		got.WorkerLeaseAttention[0].Identity != "ambiguous-a1b2c3d4e5f6" {
		t.Fatalf("worker lease reconciliation state lost: %+v", got.WorkerLeaseAttention)
	}
}
