package approvalstore

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/befeast/maestro/internal/state"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "maestro.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// seedPending records a pending approval (with a self-consistent payload
// hash, so the currency guard does not stale it) and returns its id.
func seedPending(t *testing.T, s *Store, id, action string, target *state.SupervisorTarget) *state.Approval {
	t.Helper()
	now := time.Now().UTC()
	a := &state.Approval{
		ID:        id,
		CreatedAt: now,
		UpdatedAt: now,
		Action:    action,
		Target:    target,
		Summary:   "test " + action,
		Risk:      "high",
		Status:    state.ApprovalStatusPending,
		Repo:      "owner/repo",
		Project:   "owner/repo",
		Audit:     []state.ApprovalAudit{{At: now, Event: state.ApprovalAuditCreated}},
	}
	a.PayloadHash = a.ComputePayloadHash()
	inserted, err := s.Put(context.Background(), a, RowBinding{Project: "owner/repo", Repo: "owner/repo", StateDir: "/tmp/sd"})
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	if !inserted {
		t.Fatalf("expected fresh insert for %s", id)
	}
	return a
}

func TestApprove_HappyPath(t *testing.T) {
	s := openTestStore(t)
	seedPending(t, s, "a1", "merge_pr", &state.SupervisorTarget{PR: 7})

	got, err := s.Approve(context.Background(), "a1", time.Now().UTC(), "oleg", "green")
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	if got.Status != state.ApprovalStatusApproved {
		t.Fatalf("status = %q, want approved", got.Status)
	}
	// Audit trail: created + approved mirrored into the table.
	if n := auditCount(t, s, "a1"); n != 2 {
		t.Fatalf("audit rows = %d, want 2", n)
	}
}

func TestReject_HappyPath(t *testing.T) {
	s := openTestStore(t)
	seedPending(t, s, "r1", "close_issue", &state.SupervisorTarget{Issue: 9})

	got, err := s.Reject(context.Background(), "r1", time.Now().UTC(), "oleg", "no")
	if err != nil {
		t.Fatalf("reject: %v", err)
	}
	if got.Status != state.ApprovalStatusRejected {
		t.Fatalf("status = %q, want rejected", got.Status)
	}
}

func TestApprove_NotFound(t *testing.T) {
	s := openTestStore(t)
	if _, err := s.Approve(context.Background(), "missing", time.Now().UTC(), "x", ""); !errors.Is(err, state.ErrApprovalNotFound) {
		t.Fatalf("err = %v, want ErrApprovalNotFound", err)
	}
}

func TestApprove_SecondAttemptAlreadyProcessed(t *testing.T) {
	s := openTestStore(t)
	seedPending(t, s, "a2", "merge_pr", &state.SupervisorTarget{PR: 1})

	if _, err := s.Approve(context.Background(), "a2", time.Now().UTC(), "x", ""); err != nil {
		t.Fatalf("first approve: %v", err)
	}
	// Sequential second approve of an already-approved id → not pending.
	if _, err := s.Approve(context.Background(), "a2", time.Now().UTC(), "y", ""); !errors.Is(err, state.ErrApprovalNotPending) {
		t.Fatalf("second approve err = %v, want ErrApprovalNotPending", err)
	}
}

func TestApprove_PayloadMismatchStales(t *testing.T) {
	s := openTestStore(t)
	a := seedPending(t, s, "a3", "merge_pr", &state.SupervisorTarget{PR: 5})
	// Corrupt the stored payload hash so ComputePayloadHash() no longer matches.
	a.PayloadHash = "stale-hash"
	if err := s.forceWriteJSON(a); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	got, err := s.Approve(context.Background(), "a3", time.Now().UTC(), "x", "")
	if !errors.Is(err, state.ErrApprovalPayloadMismatch) {
		t.Fatalf("err = %v, want ErrApprovalPayloadMismatch", err)
	}
	if got.Status != state.ApprovalStatusStale {
		t.Fatalf("status = %q, want stale", got.Status)
	}
}

// TestApprove_ConcurrentClaimOnce reproduces the write-path premortem
// double-execute scenario: two parallel approves of the SAME id must result
// in exactly ONE winning claim. Every loser is told the approval was already
// processed (ErrApprovalNotPending), so a downstream executor fires once.
func TestApprove_ConcurrentClaimOnce(t *testing.T) {
	s := openTestStore(t)
	seedPending(t, s, "race", "merge_pr", &state.SupervisorTarget{PR: 42})

	const racers = 8
	var (
		wins      int32
		notending int32
		executed  int32 // proxy for the real side effect
		start     = make(chan struct{})
		wg        sync.WaitGroup
	)
	wg.Add(racers)
	for i := 0; i < racers; i++ {
		go func() {
			defer wg.Done()
			<-start
			_, err := s.Approve(context.Background(), "race", time.Now().UTC(), "racer", "")
			switch {
			case err == nil:
				atomic.AddInt32(&wins, 1)
				atomic.AddInt32(&executed, 1) // only the winner "executes"
			case errors.Is(err, state.ErrApprovalNotPending):
				atomic.AddInt32(&notending, 1)
			default:
				t.Errorf("unexpected error: %v", err)
			}
		}()
	}
	close(start)
	wg.Wait()

	if wins != 1 {
		t.Fatalf("wins = %d, want exactly 1 (claim-once violated)", wins)
	}
	if executed != 1 {
		t.Fatalf("executed = %d, want exactly 1 side effect", executed)
	}
	if notending != racers-1 {
		t.Fatalf("already-processed losers = %d, want %d", notending, racers-1)
	}
}

// TestApprove_ConcurrentClaimOnce_SeparateConnections simulates the
// cross-process CLI race: two `maestro supervise approve` invocations each
// open their OWN store handle (separate connection) against the same db file.
// The IMMEDIATE-locked claim plus busy_timeout must still yield exactly one
// winner with no SQLITE_BUSY leaking out.
func TestApprove_ConcurrentClaimOnce_SeparateConnections(t *testing.T) {
	path := filepath.Join(t.TempDir(), "maestro.db")
	seeder, err := Open(path)
	if err != nil {
		t.Fatalf("open seeder: %v", err)
	}
	defer seeder.Close()
	seedPending(t, seeder, "xproc", "merge_pr", &state.SupervisorTarget{PR: 99})

	const procs = 6
	var (
		wins     int32
		notpend  int32
		start    = make(chan struct{})
		wg       sync.WaitGroup
		otherErr = make(chan error, procs)
	)
	wg.Add(procs)
	for i := 0; i < procs; i++ {
		go func() {
			defer wg.Done()
			s, err := Open(path) // its own connection, like a separate process
			if err != nil {
				otherErr <- err
				return
			}
			defer s.Close()
			<-start
			_, err = s.Approve(context.Background(), "xproc", time.Now().UTC(), "racer", "")
			switch {
			case err == nil:
				atomic.AddInt32(&wins, 1)
			case errors.Is(err, state.ErrApprovalNotPending):
				atomic.AddInt32(&notpend, 1)
			default:
				otherErr <- err
			}
		}()
	}
	close(start)
	wg.Wait()
	close(otherErr)
	for e := range otherErr {
		t.Errorf("unexpected error from cross-connection claim: %v", e)
	}
	if wins != 1 {
		t.Fatalf("wins = %d, want exactly 1", wins)
	}
	if notpend != procs-1 {
		t.Fatalf("already-processed = %d, want %d", notpend, procs-1)
	}
}

func TestMarkExecuted_Idempotent(t *testing.T) {
	s := openTestStore(t)
	seedPending(t, s, "m1", "merge_pr", &state.SupervisorTarget{PR: 3})
	if _, err := s.Approve(context.Background(), "m1", time.Now().UTC(), "x", ""); err != nil {
		t.Fatalf("approve: %v", err)
	}
	got, err := s.MarkExecuted(context.Background(), "m1", time.Now().UTC(), "x", "merged PR #3")
	if err != nil {
		t.Fatalf("mark executed: %v", err)
	}
	if got.Status != state.ApprovalStatusExecuted {
		t.Fatalf("status = %q, want executed", got.Status)
	}
	// Second finalize is a no-op error (not in status=approved).
	if _, err := s.MarkExecuted(context.Background(), "m1", time.Now().UTC(), "x", "again"); !errors.Is(err, state.ErrApprovalNotApproved) {
		t.Fatalf("second mark err = %v, want ErrApprovalNotApproved", err)
	}
}

func TestPut_DoesNotResetStatus(t *testing.T) {
	s := openTestStore(t)
	a := seedPending(t, s, "p1", "merge_pr", &state.SupervisorTarget{PR: 8})
	if _, err := s.Approve(context.Background(), "p1", time.Now().UTC(), "x", ""); err != nil {
		t.Fatalf("approve: %v", err)
	}
	// A second seed of the same (still pending in JSON) approval must NOT
	// reset the SQLite row back to pending — that would re-open the claim.
	a.Status = state.ApprovalStatusPending
	inserted, err := s.Put(context.Background(), a, RowBinding{})
	if err != nil {
		t.Fatalf("re-put: %v", err)
	}
	if inserted {
		t.Fatalf("re-put reported inserted; INSERT OR IGNORE must keep the existing row")
	}
	got, err := s.Get(context.Background(), "p1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status != state.ApprovalStatusApproved {
		t.Fatalf("status = %q after re-put, want approved (claim must survive)", got.Status)
	}
}

func auditCount(t *testing.T, s *Store, id string) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM approval_audit WHERE approval_id = ?`, id).Scan(&n); err != nil {
		t.Fatalf("count audit: %v", err)
	}
	return n
}

// forceWriteJSON rewrites the stored approval_json + payload_hash for tests
// that need to simulate payload drift.
func (s *Store) forceWriteJSON(a *state.Approval) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := writeApprovalJSONTx(context.Background(), tx, a); err != nil {
		return err
	}
	return tx.Commit()
}
