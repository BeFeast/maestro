// Package approvalstore persists supervisor Approvals (and their audit
// trail) in SQLite with a transactional claim-once primitive, replacing the
// per-project JSON read-merge-write path for the approve/reject transition.
//
// The motivating bug (write-path premortem 2026-05-30, failure modes #2/#3):
// two parallel `approve` of the same id read the same pending JSON snapshot,
// each flips it to approved, and the 3-way merge keeps both — so the
// downstream executor fires the side effect twice (double merge_pr, double
// close_issue). The fix is an atomic claim:
//
//	UPDATE approvals SET status='approved' WHERE id=? AND status='pending'
//
// Exactly one writer observes rows-affected==1 and proceeds; every other
// caller sees 0 and is told the approval was already processed. WAL + a
// single pooled connection (mirroring internal/configstore) make the claim
// safe both in-process and across the CLI/daemon process boundary.
//
// Each row carries project / repo / state_dir columns so a single shared
// maestro.db can hold every project's approvals without cross-project
// mutation (premortem #2/#3 — the executor's repo guard re-checks the same
// binding at execute time). Approval ids are content-addressed on
// (action, target) only (state.approvalID), so two DIFFERENT projects that
// gate the same action on the same target mint the SAME id. The row identity
// and every claim predicate are therefore scoped by state_dir (the JSON state
// directory, which is 1:1 with a project — repo can be shared by two configs,
// the project name can be aliased, but the state dir is unique). Without that
// scope the second project's INSERT OR IGNORE would be dropped and an
// approve/reject would consume or block the first project's claim.
package approvalstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/befeast/maestro/internal/state"
	_ "modernc.org/sqlite"
)

// approvalsTableDDL is the approvals table on its own. It is reused as the
// rebuild target for the legacy-schema migration (migrateApprovalsSchema),
// which recreates the table under the composite (state_dir, id) primary key.
const approvalsTableDDL = `
CREATE TABLE IF NOT EXISTS approvals (
	id            TEXT NOT NULL,
	decision_id   TEXT NOT NULL DEFAULT '',
	project       TEXT NOT NULL DEFAULT '',
	repo          TEXT NOT NULL DEFAULT '',
	state_dir     TEXT NOT NULL DEFAULT '',
	action        TEXT NOT NULL DEFAULT '',
	status        TEXT NOT NULL,
	payload_hash  TEXT NOT NULL DEFAULT '',
	created_at    TEXT NOT NULL,
	updated_at    TEXT NOT NULL,
	approval_json TEXT NOT NULL,
	-- Scope the row identity by state_dir: ids are content-addressed on
	-- (action, target) only, so two projects can mint the same id. Without
	-- (state_dir, id) the second project's INSERT OR IGNORE is dropped and a
	-- claim consumes the wrong project's approval.
	PRIMARY KEY (state_dir, id)
);`

// approvalsColumns is the canonical column order of the approvals table. It is
// the carry-over list when the legacy single-column-PK table is rebuilt under
// the new schema (migrateApprovalsSchema copies the intersection of these with
// whatever the legacy table actually has).
var approvalsColumns = []string{
	"id", "decision_id", "project", "repo", "state_dir", "action",
	"status", "payload_hash", "created_at", "updated_at", "approval_json",
}

// Schema creates the approvals + approval_audit tables. Both are
// IF NOT EXISTS so Init is idempotent and safe to run against an existing
// maestro.db. The denormalized project/repo/state_dir columns bind every
// row to its originating project context (premortem #2/#3).
const Schema = approvalsTableDDL + `

CREATE TABLE IF NOT EXISTS approval_audit (
	seq               INTEGER PRIMARY KEY AUTOINCREMENT,
	approval_id       TEXT NOT NULL,
	project           TEXT NOT NULL DEFAULT '',
	repo              TEXT NOT NULL DEFAULT '',
	state_dir         TEXT NOT NULL DEFAULT '',
	at                TEXT NOT NULL,
	event             TEXT NOT NULL,
	actor             TEXT NOT NULL DEFAULT '',
	reason            TEXT NOT NULL DEFAULT '',
	payload_hash      TEXT NOT NULL DEFAULT '',
	target_state_hash TEXT NOT NULL DEFAULT ''
);

CREATE INDEX IF NOT EXISTS idx_approval_audit_approval_id ON approval_audit(approval_id);
`

// RowBinding is the per-project context stamped onto every approval and
// audit row so one shared maestro.db can hold every project's approvals.
type RowBinding struct {
	Project  string
	Repo     string
	StateDir string
}

// Store is a SQLite-backed approvals store.
type Store struct {
	db *sql.DB
}

// DefaultDBPath is the unified fleet database (~/.maestro/maestro.db). A single
// file holds config, approvals, state, and the other fleet tables; rows are
// tagged by project/repo/state_dir where required.
func DefaultDBPath() string {
	return filepath.Join(os.Getenv("HOME"), ".maestro", "maestro.db")
}

// Open opens (creating the parent dir if needed) the approvals database and
// ensures the schema. The pragmas mirror internal/configstore: busy_timeout
// lets a connection wait for a lock instead of erroring "database is locked"
// (the CLI `supervise approve` writes from a separate process than the
// daemon), WAL lets a reader and a writer proceed concurrently across
// processes, and MaxOpenConns(1) removes the SQLITE_BUSY that modernc/sqlite
// can still report between two pooled connections of the same process.
func Open(path string) (*Store, error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return nil, fmt.Errorf("create approvals db dir %s: %w", dir, err)
		}
	}
	dsn := path
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	// _txlock=immediate is essential for the claim-once UPDATE: every
	// transaction acquires the write lock at BEGIN rather than upgrading from
	// a read lock mid-transaction. Two concurrent claims that both started a
	// deferred (read) transaction and then tried to UPDATE would dead-lock on
	// the write-lock upgrade and one would get SQLITE_BUSY *immediately*
	// (busy_timeout does not cover an upgrade deadlock). With IMMEDIATE the
	// second claimant simply waits up to busy_timeout for the first to commit,
	// then observes status!='pending' and loses the claim cleanly. This is the
	// cross-process (CLI vs daemon) safety that the WHERE status='pending'
	// guard relies on.
	dsn += sep + "_txlock=immediate&_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	s := &Store{db: db}
	if err := s.Init(context.Background()); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Close releases the underlying database handle.
func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

// Init creates the schema and upgrades any legacy single-column-PK approvals
// table to the (state_dir, id) composite key. Idempotent.
func (s *Store) Init(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, Schema); err != nil {
		return err
	}
	return s.migrateApprovalsSchema(ctx)
}

// migrateApprovalsSchema upgrades a maestro.db created before approval rows
// were scoped by state_dir. The original table used `id TEXT PRIMARY KEY`, so
// CREATE TABLE IF NOT EXISTS above is a no-op against it and the composite
// PRIMARY KEY (state_dir, id) is never installed. In that unscoped table two
// projects that mint the same content-addressed id collide: the second
// project's INSERT OR IGNORE is dropped and a scoped claim cannot find — or is
// blocked by — the wrong project's row. This detects the legacy primary key
// and rebuilds the table in place (rename → recreate → copy → drop), so the
// fix reaches already-created databases, not only fresh ones. No-op once the
// table already carries the composite key.
func (s *Store) migrateApprovalsSchema(ctx context.Context) error {
	scoped, cols, err := approvalsPKScopedByStateDir(ctx, s.db)
	if err != nil {
		return err
	}
	// scoped==true: fresh DB or already migrated. len(cols)==0: the table does
	// not exist (Schema would have created it, so this only guards a racing
	// drop) — nothing to rebuild either way.
	if scoped || len(cols) == 0 {
		return nil
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// Carry over only columns present in BOTH the legacy table and the new
	// schema, so a partial legacy layout still migrates without a missing-column
	// error; any new-only column falls back to its schema default.
	carry := intersectColumns(cols, approvalsColumns)
	colList := strings.Join(carry, ", ")
	if _, err := tx.ExecContext(ctx, `ALTER TABLE approvals RENAME TO approvals_legacy`); err != nil {
		return fmt.Errorf("rename legacy approvals table: %w", err)
	}
	if _, err := tx.ExecContext(ctx, approvalsTableDDL); err != nil {
		return fmt.Errorf("recreate approvals table: %w", err)
	}
	// Legacy `id` was globally unique, so (state_dir, id) pairs are unique too —
	// a plain INSERT cannot conflict and surfaces any unexpected duplicate.
	if _, err := tx.ExecContext(ctx, fmt.Sprintf(
		`INSERT INTO approvals(%s) SELECT %s FROM approvals_legacy`, colList, colList)); err != nil {
		return fmt.Errorf("copy legacy approvals rows: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DROP TABLE approvals_legacy`); err != nil {
		return fmt.Errorf("drop legacy approvals table: %w", err)
	}
	return tx.Commit()
}

// approvalsPKScopedByStateDir reports whether the approvals table's primary key
// includes state_dir (the new scoped schema) and returns the table's column
// names. A table with no rows from PRAGMA table_info (the table does not exist)
// returns scoped=false, cols=nil.
func approvalsPKScopedByStateDir(ctx context.Context, db *sql.DB) (scoped bool, cols []string, err error) {
	rows, err := db.QueryContext(ctx, `PRAGMA table_info(approvals)`)
	if err != nil {
		return false, nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			cid     int
			name    string
			ctype   string
			notnull int
			dflt    sql.NullString
			pk      int
		)
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return false, nil, err
		}
		cols = append(cols, name)
		// pk is the 1-based position within the primary key (0 = not part of
		// it). The new schema puts state_dir first; the legacy schema keys on
		// id alone, leaving state_dir at pk=0.
		if name == "state_dir" && pk > 0 {
			scoped = true
		}
	}
	return scoped, cols, rows.Err()
}

// intersectColumns returns the members of want present in have, preserving
// want's order.
func intersectColumns(have, want []string) []string {
	set := make(map[string]bool, len(have))
	for _, c := range have {
		set[c] = true
	}
	out := make([]string, 0, len(want))
	for _, c := range want {
		if set[c] {
			out = append(out, c)
		}
	}
	return out
}

// Put seeds an approval into the store WITHOUT overwriting an existing row
// (INSERT OR IGNORE). Returning inserted=false means a row for this id was
// already present — crucially, a concurrent claim that already advanced the
// status MUST NOT be reset back to pending, so Put never updates an existing
// row. When the row is newly inserted, the approval's existing audit entries
// are mirrored into approval_audit so the table is a faithful trail.
//
// Put is the write-through seed used during the JSON→SQLite transition: the
// JSON state remains the mint source; Put copies a pending approval into
// SQLite just before the atomic claim.
func (s *Store) Put(ctx context.Context, a *state.Approval, b RowBinding) (inserted bool, err error) {
	if a == nil || strings.TrimSpace(a.ID) == "" {
		return false, errors.New("approval id is required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	ins, err := putApprovalTx(ctx, tx, a, b)
	if err != nil {
		return false, err
	}
	if ins {
		for i := range a.Audit {
			if err := insertAuditTx(ctx, tx, a.ID, b, a.Audit[i]); err != nil {
				return false, err
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return ins, nil
}

// Get returns the full approval (audit included, via approval_json) for id
// within the given state_dir scope, or state.ErrApprovalNotFound.
func (s *Store) Get(ctx context.Context, stateDir, id string) (*state.Approval, error) {
	row := s.db.QueryRowContext(ctx, `SELECT approval_json FROM approvals WHERE state_dir = ? AND id = ?`, stateDir, id)
	return scanApproval(row)
}

// List returns every approval bound to the given state_dir, ordered by
// created_at. An empty stateDir returns all approvals.
func (s *Store) List(ctx context.Context, stateDir string) ([]*state.Approval, error) {
	var (
		rows *sql.Rows
		err  error
	)
	if strings.TrimSpace(stateDir) == "" {
		rows, err = s.db.QueryContext(ctx, `SELECT approval_json FROM approvals ORDER BY created_at`)
	} else {
		rows, err = s.db.QueryContext(ctx, `SELECT approval_json FROM approvals WHERE state_dir = ? ORDER BY created_at`, stateDir)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]*state.Approval, 0)
	for rows.Next() {
		a, err := scanApproval(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// Approve atomically claims a pending approval (pending → approved). It is
// the transactional counterpart of state.ApproveApproval and returns the
// same error sentinels:
//
//   - state.ErrApprovalNotFound       — no such id
//   - state.ErrApprovalStale          — already stale
//   - state.ErrApprovalSuperseded     — already superseded
//   - state.ErrApprovalNotPending     — already approved/rejected/executed,
//     OR lost the claim race to a concurrent caller (the "already
//     processed" outcome)
//   - state.ErrApprovalPayloadMismatch — payload drifted from its hash
//
// id must be the resolved Approval.ID (the row key) — callers that accept a
// decision-id alias resolve it before claiming. stateDir scopes the claim to
// one project so a shared maestro.db cannot cross-claim.
//
// Exactly one concurrent caller observes rows-affected==1 and returns nil;
// every other caller returns ErrApprovalNotPending without mutating state.
func (s *Store) Approve(ctx context.Context, stateDir, id string, now time.Time, actor, reason string) (*state.Approval, error) {
	return s.claim(ctx, stateDir, id, state.ApprovalStatusApproved, state.ApprovalAuditApproved, now, actor, reason)
}

// Reject atomically claims a pending approval (pending → rejected), with the
// same claim-once semantics and error sentinels as Approve.
func (s *Store) Reject(ctx context.Context, stateDir, id string, now time.Time, actor, reason string) (*state.Approval, error) {
	return s.claim(ctx, stateDir, id, state.ApprovalStatusRejected, state.ApprovalAuditRejected, now, actor, reason)
}

// claim implements the pending → {approved,rejected} transition with the
// atomic UPDATE ... WHERE status='pending' guard, scoped to one state_dir.
func (s *Store) claim(ctx context.Context, stateDir, id string, target state.ApprovalStatus, event string, now time.Time, actor, reason string) (*state.Approval, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	a, b, err := loadApprovalTx(ctx, tx, stateDir, id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, state.ErrApprovalNotFound
	}
	if err != nil {
		return nil, err
	}

	switch a.Status {
	case state.ApprovalStatusStale:
		return a, state.ErrApprovalStale
	case state.ApprovalStatusSuperseded:
		return a, state.ErrApprovalSuperseded
	case state.ApprovalStatusPending:
		// claimable — fall through
	default:
		return a, state.ErrApprovalNotPending
	}

	// Internal-consistency guard, mirroring state.ensureApprovalCurrent: a
	// pending approval whose payload drifted from its recorded hash is staled
	// rather than approved.
	if a.PayloadHash != "" && a.ComputePayloadHash() != a.PayloadHash {
		if err := markStaleTx(ctx, tx, a, b, now, "approval payload changed"); err != nil {
			return a, err
		}
		if err := tx.Commit(); err != nil {
			return a, err
		}
		return a, state.ErrApprovalPayloadMismatch
	}

	now = normalize(now)
	res, err := tx.ExecContext(ctx,
		`UPDATE approvals SET status = ?, updated_at = ? WHERE state_dir = ? AND id = ? AND status = ?`,
		string(target), formatTime(now), stateDir, id, string(state.ApprovalStatusPending))
	if err != nil {
		return a, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return a, err
	}
	if n != 1 {
		// Lost the claim to a concurrent writer between our read and UPDATE.
		// Reload so the caller sees the winner's status, and report
		// "already processed".
		reloaded, _, lerr := loadApprovalTx(ctx, tx, stateDir, id)
		if lerr == nil {
			a = reloaded
		}
		return a, state.ErrApprovalNotPending
	}

	a.Status = target
	a.UpdatedAt = now
	audit := state.ApprovalAudit{
		At:              now,
		Event:           event,
		Actor:           actor,
		Reason:          reason,
		PayloadHash:     a.PayloadHash,
		TargetStateHash: a.TargetStateHash,
	}
	a.Audit = append(a.Audit, audit)
	if err := writeApprovalJSONTx(ctx, tx, b.StateDir, a); err != nil {
		return a, err
	}
	if err := insertAuditTx(ctx, tx, id, b, audit); err != nil {
		return a, err
	}
	if err := tx.Commit(); err != nil {
		return a, err
	}
	return a, nil
}

// MarkExecuted transitions approved → executed, mirroring
// state.MarkApprovalExecuted. Idempotent at the caller boundary: an approval
// not in status=approved returns state.ErrApprovalNotApproved unchanged, so a
// concurrent executor cannot double-record.
func (s *Store) MarkExecuted(ctx context.Context, stateDir, id string, now time.Time, actor, summary string) (*state.Approval, error) {
	return s.markTerminal(ctx, stateDir, id, state.ApprovalStatusExecuted, state.ApprovalAuditExecuted, now, actor, summary)
}

// MarkExecutionFailed transitions approved → execution_failed.
func (s *Store) MarkExecutionFailed(ctx context.Context, stateDir, id string, now time.Time, actor, errMsg string) (*state.Approval, error) {
	return s.markTerminal(ctx, stateDir, id, state.ApprovalStatusExecutionFailed, state.ApprovalAuditExecutionFailed, now, actor, errMsg)
}

// MarkExecutionSkipped transitions approved → execution_skipped.
func (s *Store) MarkExecutionSkipped(ctx context.Context, stateDir, id string, now time.Time, actor, reason string) (*state.Approval, error) {
	return s.markTerminal(ctx, stateDir, id, state.ApprovalStatusExecutionSkipped, state.ApprovalAuditExecutionSkipped, now, actor, reason)
}

// MarkAwaitingDispatch transitions approved → awaiting_dispatch.
func (s *Store) MarkAwaitingDispatch(ctx context.Context, stateDir, id string, now time.Time, actor, reason string) (*state.Approval, error) {
	return s.markTerminal(ctx, stateDir, id, state.ApprovalStatusAwaitingDispatch, state.ApprovalAuditAwaitingDispatch, now, actor, reason)
}

// markTerminal implements the approved → terminal transition with the atomic
// UPDATE ... WHERE status='approved' idempotency guard, scoped to one state_dir.
func (s *Store) markTerminal(ctx context.Context, stateDir, id string, target state.ApprovalStatus, event string, now time.Time, actor, reason string) (*state.Approval, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	a, b, err := loadApprovalTx(ctx, tx, stateDir, id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, state.ErrApprovalNotFound
	}
	if err != nil {
		return nil, err
	}
	if a.Status != state.ApprovalStatusApproved {
		return a, state.ErrApprovalNotApproved
	}

	now = normalize(now)
	res, err := tx.ExecContext(ctx,
		`UPDATE approvals SET status = ?, updated_at = ? WHERE state_dir = ? AND id = ? AND status = ?`,
		string(target), formatTime(now), stateDir, id, string(state.ApprovalStatusApproved))
	if err != nil {
		return a, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return a, err
	}
	if n != 1 {
		reloaded, _, lerr := loadApprovalTx(ctx, tx, stateDir, id)
		if lerr == nil {
			a = reloaded
		}
		return a, state.ErrApprovalNotApproved
	}

	a.Status = target
	a.UpdatedAt = now
	audit := state.ApprovalAudit{
		At:              now,
		Event:           event,
		Actor:           actor,
		Reason:          reason,
		PayloadHash:     a.PayloadHash,
		TargetStateHash: a.TargetStateHash,
	}
	a.Audit = append(a.Audit, audit)
	if err := writeApprovalJSONTx(ctx, tx, b.StateDir, a); err != nil {
		return a, err
	}
	if err := insertAuditTx(ctx, tx, id, b, audit); err != nil {
		return a, err
	}
	if err := tx.Commit(); err != nil {
		return a, err
	}
	return a, nil
}

// MarkStale idempotently reconciles a moot approval to the terminal `stale`
// status in SQLite, mirroring state.markApprovalStale. It is the SQLite half of
// the #866 fix: when the orchestrator stales a spawn_repair_worker approval in
// JSON because its target issue reached a terminal state, the same transition
// must reach the unified approval store so a later approve/reject claim cannot
// act on a row the JSON state already retired. Only an active row (pending /
// approved / awaiting_dispatch) is transitioned; a row already stale — or in any
// other terminal status — is returned unchanged with a nil error, so repeated
// reconciles, daemon restarts, and concurrent saves converge without churn or a
// duplicate audit entry. A missing row returns state.ErrApprovalNotFound for the
// caller (approvalstore.ReconcileMoot) to tolerate.
func (s *Store) MarkStale(ctx context.Context, stateDir, id string, now time.Time, reason string) (*state.Approval, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	a, b, err := loadApprovalTx(ctx, tx, stateDir, id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, state.ErrApprovalNotFound
	}
	if err != nil {
		return nil, err
	}
	switch a.Status {
	case state.ApprovalStatusPending, state.ApprovalStatusApproved, state.ApprovalStatusAwaitingDispatch:
		// Active — fall through and stale it.
	default:
		// Already stale / superseded / executed / rejected — idempotent no-op.
		return a, nil
	}
	if err := markStaleTx(ctx, tx, a, b, now, reason); err != nil {
		return a, err
	}
	if err := tx.Commit(); err != nil {
		return a, err
	}
	return a, nil
}

// --- tx helpers -------------------------------------------------------------

func putApprovalTx(ctx context.Context, tx *sql.Tx, a *state.Approval, b RowBinding) (bool, error) {
	blob, err := json.Marshal(a)
	if err != nil {
		return false, fmt.Errorf("marshal approval %s: %w", a.ID, err)
	}
	res, err := tx.ExecContext(ctx, `
INSERT OR IGNORE INTO approvals(id, decision_id, project, repo, state_dir, action, status, payload_hash, created_at, updated_at, approval_json)
VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		a.ID, a.DecisionID, b.Project, b.Repo, b.StateDir, a.Action, string(a.Status), a.PayloadHash,
		formatTime(normalize(a.CreatedAt)), formatTime(normalize(a.UpdatedAt)), string(blob))
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n == 1, nil
}

func writeApprovalJSONTx(ctx context.Context, tx *sql.Tx, stateDir string, a *state.Approval) error {
	blob, err := json.Marshal(a)
	if err != nil {
		return fmt.Errorf("marshal approval %s: %w", a.ID, err)
	}
	_, err = tx.ExecContext(ctx,
		`UPDATE approvals SET payload_hash = ?, approval_json = ? WHERE state_dir = ? AND id = ?`,
		a.PayloadHash, string(blob), stateDir, a.ID)
	return err
}

func insertAuditTx(ctx context.Context, tx *sql.Tx, approvalID string, b RowBinding, e state.ApprovalAudit) error {
	_, err := tx.ExecContext(ctx, `
INSERT INTO approval_audit(approval_id, project, repo, state_dir, at, event, actor, reason, payload_hash, target_state_hash)
VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		approvalID, b.Project, b.Repo, b.StateDir, formatTime(normalize(e.At)), e.Event, e.Actor, e.Reason, e.PayloadHash, e.TargetStateHash)
	return err
}

func markStaleTx(ctx context.Context, tx *sql.Tx, a *state.Approval, b RowBinding, now time.Time, reason string) error {
	now = normalize(now)
	a.Status = state.ApprovalStatusStale
	a.UpdatedAt = now
	audit := state.ApprovalAudit{
		At:              now,
		Event:           state.ApprovalAuditStale,
		Reason:          reason,
		PayloadHash:     a.PayloadHash,
		TargetStateHash: a.TargetStateHash,
	}
	a.Audit = append(a.Audit, audit)
	if _, err := tx.ExecContext(ctx,
		`UPDATE approvals SET status = ?, updated_at = ? WHERE state_dir = ? AND id = ?`,
		string(state.ApprovalStatusStale), formatTime(now), b.StateDir, a.ID); err != nil {
		return err
	}
	if err := writeApprovalJSONTx(ctx, tx, b.StateDir, a); err != nil {
		return err
	}
	return insertAuditTx(ctx, tx, a.ID, b, audit)
}

// loadApprovalTx returns the full approval plus its stored project/repo/
// state_dir binding so audit rows written by a later transition carry the
// same context that Put seeded.
func loadApprovalTx(ctx context.Context, tx *sql.Tx, scopeDir, id string) (*state.Approval, RowBinding, error) {
	var blob, project, repo, stateDir string
	err := tx.QueryRowContext(ctx,
		`SELECT approval_json, project, repo, state_dir FROM approvals WHERE state_dir = ? AND id = ?`, scopeDir, id).
		Scan(&blob, &project, &repo, &stateDir)
	if err != nil {
		return nil, RowBinding{}, err
	}
	var a state.Approval
	if err := json.Unmarshal([]byte(blob), &a); err != nil {
		return nil, RowBinding{}, fmt.Errorf("unmarshal approval %s: %w", id, err)
	}
	return &a, RowBinding{Project: project, Repo: repo, StateDir: stateDir}, nil
}

// rowScanner is satisfied by both *sql.Row and *sql.Rows.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanApproval(row rowScanner) (*state.Approval, error) {
	var blob string
	if err := row.Scan(&blob); err != nil {
		return nil, err
	}
	var a state.Approval
	if err := json.Unmarshal([]byte(blob), &a); err != nil {
		return nil, fmt.Errorf("unmarshal approval: %w", err)
	}
	return &a, nil
}

func normalize(t time.Time) time.Time {
	if t.IsZero() {
		return time.Now().UTC()
	}
	return t.UTC()
}

func formatTime(t time.Time) string {
	return t.Format(time.RFC3339Nano)
}
