package approvalstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/befeast/maestro/internal/state"
)

// testStateDir is the scope every seedPending row is bound to; tests pass it
// as the claim scope so the (state_dir, id) row key resolves.
const testStateDir = "/tmp/sd"

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
// hash, so the currency guard does not stale it) bound to testStateDir and
// returns it.
func seedPending(t *testing.T, s *Store, id, action string, target *state.SupervisorTarget) *state.Approval {
	t.Helper()
	return seedPendingScoped(t, s, RowBinding{Project: "owner/repo", Repo: "owner/repo", StateDir: testStateDir}, id, action, target)
}

// seedPendingScoped is seedPending with an explicit project binding, used by
// the cross-project isolation test to seed the SAME id under two state dirs.
func seedPendingScoped(t *testing.T, s *Store, b RowBinding, id, action string, target *state.SupervisorTarget) *state.Approval {
	t.Helper()
	a := makeApproval(id, action, target, b)
	inserted, err := s.Put(context.Background(), a, b)
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	if !inserted {
		t.Fatalf("expected fresh insert for %s under %s", id, b.StateDir)
	}
	return a
}

// makeApproval builds a self-consistent pending approval (payload hash matches
// so the currency guard does not stale it) without writing it anywhere.
func makeApproval(id, action string, target *state.SupervisorTarget, b RowBinding) *state.Approval {
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
		Repo:      b.Repo,
		Project:   b.Project,
		Audit:     []state.ApprovalAudit{{At: now, Event: state.ApprovalAuditCreated}},
	}
	a.PayloadHash = a.ComputePayloadHash()
	return a
}

func TestApprove_HappyPath(t *testing.T) {
	s := openTestStore(t)
	seedPending(t, s, "a1", "merge_pr", &state.SupervisorTarget{PR: 7})

	got, err := s.Approve(context.Background(), testStateDir, "a1", time.Now().UTC(), "oleg", "green")
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

	got, err := s.Reject(context.Background(), testStateDir, "r1", time.Now().UTC(), "oleg", "no")
	if err != nil {
		t.Fatalf("reject: %v", err)
	}
	if got.Status != state.ApprovalStatusRejected {
		t.Fatalf("status = %q, want rejected", got.Status)
	}
}

func TestApprove_NotFound(t *testing.T) {
	s := openTestStore(t)
	if _, err := s.Approve(context.Background(), testStateDir, "missing", time.Now().UTC(), "x", ""); !errors.Is(err, state.ErrApprovalNotFound) {
		t.Fatalf("err = %v, want ErrApprovalNotFound", err)
	}
}

func TestApprove_SecondAttemptAlreadyProcessed(t *testing.T) {
	s := openTestStore(t)
	seedPending(t, s, "a2", "merge_pr", &state.SupervisorTarget{PR: 1})

	if _, err := s.Approve(context.Background(), testStateDir, "a2", time.Now().UTC(), "x", ""); err != nil {
		t.Fatalf("first approve: %v", err)
	}
	// Sequential second approve of an already-approved id → not pending.
	if _, err := s.Approve(context.Background(), testStateDir, "a2", time.Now().UTC(), "y", ""); !errors.Is(err, state.ErrApprovalNotPending) {
		t.Fatalf("second approve err = %v, want ErrApprovalNotPending", err)
	}
}

// TestApprove_CrossProjectSameID_Isolated guards the P1 fix: approval ids are
// content-addressed on (action, target) only, so two DIFFERENT projects that
// both gate merge_pr on PR #1 mint the SAME id ("dup"). With a shared
// maestro.db they must remain independent rows keyed by (state_dir, id):
// approving project A's row must neither consume nor block project B's, and B
// must still be able to approve (and reject) its own.
func TestApprove_CrossProjectSameID_Isolated(t *testing.T) {
	s := openTestStore(t)
	const id = "dup"
	bindA := RowBinding{Project: "owner/a", Repo: "owner/a", StateDir: "/tmp/a"}
	bindB := RowBinding{Project: "owner/b", Repo: "owner/b", StateDir: "/tmp/b"}
	seedPendingScoped(t, s, bindA, id, "merge_pr", &state.SupervisorTarget{PR: 1})
	seedPendingScoped(t, s, bindB, id, "merge_pr", &state.SupervisorTarget{PR: 1})

	// Approve A's copy — B's must stay untouched (still pending).
	gotA, err := s.Approve(context.Background(), bindA.StateDir, id, time.Now().UTC(), "x", "")
	if err != nil {
		t.Fatalf("approve A: %v", err)
	}
	if gotA.Status != state.ApprovalStatusApproved {
		t.Fatalf("A status = %q, want approved", gotA.Status)
	}
	gotB, err := s.Get(context.Background(), bindB.StateDir, id)
	if err != nil {
		t.Fatalf("get B: %v", err)
	}
	if gotB.Status != state.ApprovalStatusPending {
		t.Fatalf("B status = %q after A approve, want pending (cross-project leak)", gotB.Status)
	}
	if gotB.Project != "owner/b" {
		t.Fatalf("B project = %q, want owner/b (A's row leaked into B's scope)", gotB.Project)
	}

	// B reject its own copy independently — A stays approved.
	if _, err := s.Reject(context.Background(), bindB.StateDir, id, time.Now().UTC(), "y", "no"); err != nil {
		t.Fatalf("reject B: %v", err)
	}
	finalA, err := s.Get(context.Background(), bindA.StateDir, id)
	if err != nil {
		t.Fatalf("get A: %v", err)
	}
	if finalA.Status != state.ApprovalStatusApproved {
		t.Fatalf("A status = %q after B reject, want still approved", finalA.Status)
	}
}

func TestApprove_PayloadMismatchStales(t *testing.T) {
	s := openTestStore(t)
	a := seedPending(t, s, "a3", "merge_pr", &state.SupervisorTarget{PR: 5})
	// Corrupt the stored payload hash so ComputePayloadHash() no longer matches.
	a.PayloadHash = "stale-hash"
	if err := s.forceWriteJSON(testStateDir, a); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	got, err := s.Approve(context.Background(), testStateDir, "a3", time.Now().UTC(), "x", "")
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
			_, err := s.Approve(context.Background(), testStateDir, "race", time.Now().UTC(), "racer", "")
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
			_, err = s.Approve(context.Background(), testStateDir, "xproc", time.Now().UTC(), "racer", "")
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
	if _, err := s.Approve(context.Background(), testStateDir, "m1", time.Now().UTC(), "x", ""); err != nil {
		t.Fatalf("approve: %v", err)
	}
	got, err := s.MarkExecuted(context.Background(), testStateDir, "m1", time.Now().UTC(), "x", "merged PR #3")
	if err != nil {
		t.Fatalf("mark executed: %v", err)
	}
	if got.Status != state.ApprovalStatusExecuted {
		t.Fatalf("status = %q, want executed", got.Status)
	}
	// Second finalize is a no-op error (not in status=approved).
	if _, err := s.MarkExecuted(context.Background(), testStateDir, "m1", time.Now().UTC(), "x", "again"); !errors.Is(err, state.ErrApprovalNotApproved) {
		t.Fatalf("second mark err = %v, want ErrApprovalNotApproved", err)
	}
}

func TestPut_DoesNotResetStatus(t *testing.T) {
	s := openTestStore(t)
	a := seedPending(t, s, "p1", "merge_pr", &state.SupervisorTarget{PR: 8})
	if _, err := s.Approve(context.Background(), testStateDir, "p1", time.Now().UTC(), "x", ""); err != nil {
		t.Fatalf("approve: %v", err)
	}
	// A second seed of the same (still pending in JSON) approval must NOT
	// reset the SQLite row back to pending — that would re-open the claim.
	a.Status = state.ApprovalStatusPending
	inserted, err := s.Put(context.Background(), a, RowBinding{StateDir: testStateDir})
	if err != nil {
		t.Fatalf("re-put: %v", err)
	}
	if inserted {
		t.Fatalf("re-put reported inserted; INSERT OR IGNORE must keep the existing row")
	}
	got, err := s.Get(context.Background(), testStateDir, "p1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status != state.ApprovalStatusApproved {
		t.Fatalf("status = %q after re-put, want approved (claim must survive)", got.Status)
	}
}

// legacyApprovalsDDL is the pre-scope approvals table: keyed on `id` alone, so
// a shared maestro.db cannot hold two projects' same content-addressed id. It
// reproduces a database an operator created before the (state_dir, id) key.
const legacyApprovalsDDL = `
CREATE TABLE approvals (
	id            TEXT PRIMARY KEY,
	decision_id   TEXT NOT NULL DEFAULT '',
	project       TEXT NOT NULL DEFAULT '',
	repo          TEXT NOT NULL DEFAULT '',
	state_dir     TEXT NOT NULL DEFAULT '',
	action        TEXT NOT NULL DEFAULT '',
	status        TEXT NOT NULL,
	payload_hash  TEXT NOT NULL DEFAULT '',
	created_at    TEXT NOT NULL,
	updated_at    TEXT NOT NULL,
	approval_json TEXT NOT NULL
);`

// TestMigrate_LegacyIDPrimaryKey_Upgrades guards the review P1: an operator who
// already created maestro.db with the legacy `approvals(id PRIMARY KEY)` table
// would, with only CREATE TABLE IF NOT EXISTS, keep the unscoped key forever —
// the second project's same-id approval is dropped by INSERT OR IGNORE. Opening
// the store must rebuild the table under (state_dir, id), preserving existing
// rows and unblocking per-project isolation.
func TestMigrate_LegacyIDPrimaryKey_Upgrades(t *testing.T) {
	path := filepath.Join(t.TempDir(), "maestro.db")
	bindA := RowBinding{Project: "owner/a", Repo: "owner/a", StateDir: "/tmp/a"}
	legacy := makeApproval("legacy", "merge_pr", &state.SupervisorTarget{PR: 1}, bindA)
	writeLegacyApprovalsDB(t, path, bindA, legacy)

	// Open runs Init → migrateApprovalsSchema, which must detect and rebuild it.
	s, err := Open(path)
	if err != nil {
		t.Fatalf("open legacy store: %v", err)
	}
	defer s.Close()

	if scoped, _, err := approvalsPKScopedByStateDir(context.Background(), s.db); err != nil {
		t.Fatalf("inspect pk: %v", err)
	} else if !scoped {
		t.Fatalf("approvals PK still unscoped after migration")
	}

	// The legacy row survived the rebuild.
	got, err := s.Get(context.Background(), bindA.StateDir, "legacy")
	if err != nil {
		t.Fatalf("get migrated row: %v", err)
	}
	if got.Status != state.ApprovalStatusPending || got.Project != "owner/a" {
		t.Fatalf("migrated row = {status:%q project:%q}, want pending owner/a", got.Status, got.Project)
	}

	// The bug is fixed: a second project can now seed the SAME id under its own
	// state_dir (previously dropped by INSERT OR IGNORE), and approving B's copy
	// must leave A's untouched.
	bindB := RowBinding{Project: "owner/b", Repo: "owner/b", StateDir: "/tmp/b"}
	seedPendingScoped(t, s, bindB, "legacy", "merge_pr", &state.SupervisorTarget{PR: 1})
	if _, err := s.Approve(context.Background(), bindB.StateDir, "legacy", time.Now().UTC(), "x", ""); err != nil {
		t.Fatalf("approve B: %v", err)
	}
	a, err := s.Get(context.Background(), bindA.StateDir, "legacy")
	if err != nil {
		t.Fatalf("get A after B approve: %v", err)
	}
	if a.Status != state.ApprovalStatusPending {
		t.Fatalf("A status = %q after B approve, want pending (legacy row cross-claimed)", a.Status)
	}
}

// TestMigrate_NoOpOnFreshDB asserts the migration leaves an already-scoped
// (freshly created) database untouched and does not error on repeated Init.
func TestMigrate_NoOpOnFreshDB(t *testing.T) {
	s := openTestStore(t)
	seedPending(t, s, "fresh", "merge_pr", &state.SupervisorTarget{PR: 1})
	// Re-running Init (idempotent) must not disturb the row or the schema.
	if err := s.Init(context.Background()); err != nil {
		t.Fatalf("re-init: %v", err)
	}
	if _, err := s.Get(context.Background(), testStateDir, "fresh"); err != nil {
		t.Fatalf("get after re-init: %v", err)
	}
}

// writeLegacyApprovalsDB creates a maestro.db with the legacy approvals schema
// and one row, then closes it — the on-disk state an upgrade has to migrate.
func writeLegacyApprovalsDB(t *testing.T, path string, b RowBinding, a *state.Approval) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open legacy db: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(legacyApprovalsDDL); err != nil {
		t.Fatalf("create legacy schema: %v", err)
	}
	blob, err := json.Marshal(a)
	if err != nil {
		t.Fatalf("marshal approval: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO approvals(id, decision_id, project, repo, state_dir, action, status, payload_hash, created_at, updated_at, approval_json)
		 VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		a.ID, a.DecisionID, b.Project, b.Repo, b.StateDir, a.Action, string(a.Status), a.PayloadHash,
		a.CreatedAt.Format(time.RFC3339Nano), a.UpdatedAt.Format(time.RFC3339Nano), string(blob)); err != nil {
		t.Fatalf("insert legacy row: %v", err)
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
func (s *Store) forceWriteJSON(stateDir string, a *state.Approval) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := writeApprovalJSONTx(context.Background(), tx, stateDir, a); err != nil {
		return err
	}
	return tx.Commit()
}
