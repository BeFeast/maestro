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
// scope the second project's write would collide with the first project's row
// and an approve/reject would consume or block the wrong project's claim.
package approvalstore

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
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
	record_hash   TEXT NOT NULL DEFAULT '',
	created_at    TEXT NOT NULL,
	updated_at    TEXT NOT NULL,
	approval_json TEXT NOT NULL,
	-- Scope the row identity by state_dir: ids are content-addressed on
	-- (action, target) only, so two projects can mint the same id. Without
	-- (state_dir, id) the second project's write collides with the first
	-- project's row and a claim consumes the wrong project's approval.
	PRIMARY KEY (state_dir, id)
);`

// approvalsColumns is the canonical column order of the approvals table. It is
// the carry-over list when the legacy single-column-PK table is rebuilt under
// the new schema (migrateApprovalsSchema copies the intersection of these with
// whatever the legacy table actually has).
var approvalsColumns = []string{
	"id", "decision_id", "project", "repo", "state_dir", "action",
	"status", "payload_hash", "record_hash", "created_at", "updated_at", "approval_json",
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

// ErrDeliveryConfigMismatch means the live executor spec no longer matches
// the digest durably approved by the operator. The authoritative row is made
// stale in the same transaction, so rolling config back cannot resurrect it.
var ErrDeliveryConfigMismatch = errors.New("delivery config differs from approved digest")

// ErrDeliveryIntegrity means approval_json, its embedded payload hash, and the
// denormalized SQLite hash do not agree with the canonical immutable delivery
// payload. Callers fail closed and run no side effect.
var ErrDeliveryIntegrity = errors.New("delivery approval integrity check failed")

// ErrDeliveryInFlight means another deploy_project approval for the same
// project state directory already owns the execution lease. The newer
// approval remains approved and may be claimed after the in-flight delivery
// reaches a terminal state; it is never run concurrently with the older
// generation.
var ErrDeliveryInFlight = errors.New("another project delivery is already executing")

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
	if err := s.migrateApprovalsSchema(ctx); err != nil {
		return err
	}
	if err := s.ensureDeliveryRecordHashColumn(ctx); err != nil {
		return err
	}
	if err := s.migrateCanonicalStateDirs(ctx); err != nil {
		return err
	}
	return nil
}

// ensureDeliveryRecordHashColumn upgrades an already-scoped approvals table
// created before delivery result records gained their independent integrity
// digest. Existing rows deliberately remain blank: approval-gated delivery is
// not yet live, so an old deploy_project row has no trusted terminal result to
// grandfather and will fail closed on read.
func (s *Store) ensureDeliveryRecordHashColumn(ctx context.Context) error {
	_, cols, err := approvalsPKScopedByStateDir(ctx, s.db)
	if err != nil {
		return err
	}
	for _, col := range cols {
		if col == "record_hash" {
			return nil
		}
	}
	_, err = s.db.ExecContext(ctx, `ALTER TABLE approvals ADD COLUMN record_hash TEXT NOT NULL DEFAULT ''`)
	return err
}

// migrateCanonicalStateDirs folds legacy lexical/symlink aliases into the one
// canonical state_dir namespace now used by config loading. This migration is
// intentionally fail-closed on an ID collision: two independently claimable
// rows for the same physical state directory may represent a historical
// duplicate-delivery hazard, and guessing which status wins could replay a
// side effect. Collision-free aliases are updated atomically together with
// their audit rows before any new mint/claim can occur.
func (s *Store) migrateCanonicalStateDirs(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `
SELECT DISTINCT state_dir FROM (
  SELECT state_dir FROM approvals
  UNION ALL
  SELECT state_dir FROM approval_audit
)
WHERE state_dir <> ''`)
	if err != nil {
		return err
	}
	var rawDirs []string
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			rows.Close()
			return err
		}
		rawDirs = append(rawDirs, raw)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, raw := range rawDirs {
		if !filepath.IsAbs(raw) {
			return fmt.Errorf("legacy approval state_dir %q is relative; refusing cwd-dependent automatic migration", raw)
		}
		canonical, err := canonicalStateDir(raw)
		if err != nil {
			return fmt.Errorf("canonicalize legacy approval state_dir %q: %w", raw, err)
		}
		if canonical == raw {
			continue
		}
		var collisionID string
		err = tx.QueryRowContext(ctx, `
SELECT old.id
FROM approvals old
JOIN approvals current ON current.state_dir = ? AND current.id = old.id
WHERE old.state_dir = ?
LIMIT 1`, canonical, raw).Scan(&collisionID)
		if err == nil {
			return fmt.Errorf("legacy approval state_dir alias collision for id %s: %q and %q; refusing automatic migration", collisionID, raw, canonical)
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		// record_hash binds state_dir, so a trusted alias migration must verify
		// the old record first and then rebind its digest to the canonical path
		// inside this same transaction. Blindly changing state_dir would make
		// every valid delivery row look tampered on the next read.
		type deliveryRebind struct {
			id, blob, project, repo, action, status, payloadHash, recordHash string
		}
		var rebinds []deliveryRebind
		deliveryRows, err := tx.QueryContext(ctx, `
SELECT id, approval_json, project, repo, action, status, payload_hash, record_hash
FROM approvals WHERE state_dir = ? AND (action = ? OR record_hash <> '')`, raw, state.ApprovalActionDeployProject)
		if err != nil {
			return err
		}
		for deliveryRows.Next() {
			var row deliveryRebind
			if err := deliveryRows.Scan(&row.id, &row.blob, &row.project, &row.repo, &row.action, &row.status, &row.payloadHash, &row.recordHash); err != nil {
				deliveryRows.Close()
				return err
			}
			rebinds = append(rebinds, row)
		}
		if err := deliveryRows.Close(); err != nil {
			return err
		}
		for _, row := range rebinds {
			var approval state.Approval
			if err := json.Unmarshal([]byte(row.blob), &approval); err != nil {
				return fmt.Errorf("unmarshal delivery approval %s during state_dir migration: %w", row.id, err)
			}
			verified, err := verifyDeliveryIntegrity(&approval, row.payloadHash, row.recordHash, row.id, row.action, row.status,
				RowBinding{Project: row.project, Repo: row.repo, StateDir: raw})
			if err != nil {
				return fmt.Errorf("rebind delivery approval %s: %w", row.id, err)
			}
			newHash, err := computeDeliveryRecordHash(verified, RowBinding{Project: row.project, Repo: row.repo, StateDir: canonical})
			if err != nil {
				return fmt.Errorf("hash rebound delivery approval %s: %w", row.id, err)
			}
			if _, err := tx.ExecContext(ctx, `UPDATE approvals SET record_hash = ? WHERE state_dir = ? AND id = ?`, newHash, raw, row.id); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `UPDATE approvals SET state_dir = ? WHERE state_dir = ?`, canonical, raw); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE approval_audit SET state_dir = ? WHERE state_dir = ?`, canonical, raw); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func canonicalStateDir(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", errors.New("state_dir is required")
	}
	abs, err := filepath.Abs(filepath.Clean(path))
	if err != nil {
		return "", err
	}
	probe := abs
	var suffix []string
	for {
		resolved, evalErr := filepath.EvalSymlinks(probe)
		if evalErr == nil {
			for i := len(suffix) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, suffix[i])
			}
			return filepath.Clean(resolved), nil
		}
		if !os.IsNotExist(evalErr) {
			return "", evalErr
		}
		parent := filepath.Dir(probe)
		if parent == probe {
			return abs, nil
		}
		suffix = append(suffix, filepath.Base(probe))
		probe = parent
	}
}

// migrateApprovalsSchema upgrades a maestro.db created before approval rows
// were scoped by state_dir. The original table used `id TEXT PRIMARY KEY`, so
// CREATE TABLE IF NOT EXISTS above is a no-op against it and the composite
// PRIMARY KEY (state_dir, id) is never installed. In that unscoped table two
// projects that mint the same content-addressed id collide: the write path
// resolves the conflict on (state_dir, id), which an unscoped `id PRIMARY KEY`
// table does not have, so the statement ERRORS at runtime instead of writing —
// and a scoped claim cannot find, or is blocked by, the wrong project's row. This detects the legacy primary key
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

// Put seeds an approval into the store. A row that already exists under
// (state_dir, id) is either REPLACED by a strictly newer mint or explicitly
// kept — never dropped silently (#1142). Ids are content-addressed on
// (action, target), so the same id legitimately recurs once the earlier
// instance of that decision is gone from the JSON state; the old
// `INSERT OR IGNORE` swallowed the write and the row kept the first
// instance's status forever while JSON moved on.
//
// Returning inserted=false means the pre-existing row was kept: a re-seed of
// the SAME mint (equal CreatedAt) must not reset a status that a concurrent
// claim already advanced. Returning true means the row now holds this
// approval — a fresh insert or an accepted re-mint — and the approval's audit
// entries are mirrored into approval_audit so the table is a faithful trail.
//
// Put is the write-through seed used during the JSON→SQLite transition: the
// JSON state remains the mint source; Put copies a pending approval into
// SQLite just before the atomic claim.
func (s *Store) Put(ctx context.Context, a *state.Approval, b RowBinding) (inserted bool, err error) {
	if a == nil || strings.TrimSpace(a.ID) == "" {
		return false, errors.New("approval id is required")
	}
	if a.Action == state.ApprovalActionDeployProject && a.Delivery == nil {
		return false, errors.New("deploy_project approval requires delivery payload")
	}
	b.StateDir, err = canonicalStateDir(b.StateDir)
	if err != nil {
		return false, fmt.Errorf("canonicalize approval state_dir: %w", err)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	persisted := a
	if a.Action == state.ApprovalActionDeployProject {
		persisted = state.CanonicalDeliveryApprovalForWrite(a)
	}
	outcome, err := putApprovalTx(ctx, tx, persisted, b)
	if err != nil {
		return false, err
	}
	ins := outcome != approvalPutKept
	if ins {
		for i := range persisted.Audit {
			if err := insertAuditTx(ctx, tx, persisted.ID, b, persisted.Audit[i]); err != nil {
				return false, err
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return ins, nil
}

// PutDelivery atomically seeds a deploy_project approval and supersedes every
// other still-actionable delivery for the same project in the same state_dir.
// This is intentionally stronger than generic Put: an old approved SHA/spec
// must lose the same SQLite race that arbitrates approved→executing, otherwise
// JSON could say "superseded" while a daemon still executes the old DB row.
func (s *Store) PutDelivery(ctx context.Context, a *state.Approval, b RowBinding, now time.Time) (inserted bool, err error) {
	if a == nil || strings.TrimSpace(a.ID) == "" {
		return false, errors.New("approval id is required")
	}
	if a.Action != state.ApprovalActionDeployProject || a.Delivery == nil {
		return false, errors.New("PutDelivery requires a deploy_project approval with delivery payload")
	}
	b.StateDir, err = canonicalStateDir(b.StateDir)
	if err != nil {
		return false, fmt.Errorf("canonicalize delivery state_dir: %w", err)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	// Ensure-row calls from an executor or restart must be side-effect free.
	// In particular, a late ensure of old A after newer B was minted must not
	// supersede B. Only a genuinely fresh generation reaches reconciliation.
	if _, _, loadErr := loadApprovalTx(ctx, tx, b.StateDir, a.ID); loadErr == nil {
		return false, nil
	} else if !errors.Is(loadErr, sql.ErrNoRows) {
		return false, loadErr
	}

	rows, err := tx.QueryContext(ctx, `
SELECT id FROM approvals
WHERE state_dir = ? AND action = ? AND id <> ?`,
		b.StateDir, string(state.ApprovalActionDeployProject), a.ID)
	if err != nil {
		return false, err
	}
	var siblingIDs []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return false, err
		}
		siblingIDs = append(siblingIDs, id)
	}
	if err := rows.Close(); err != nil {
		return false, err
	}

	now = normalize(now)
	candidate := *state.CanonicalDeliveryApprovalForWrite(a)
	var staleIDs []string
	if candidate.Status == state.ApprovalStatusPending || candidate.Status == state.ApprovalStatusApproved {
		newerExists := false
		for _, id := range siblingIDs {
			old, _, loadErr := loadApprovalTx(ctx, tx, b.StateDir, id)
			if loadErr != nil {
				return false, loadErr
			}
			if state.DeliveryGenerationsAmbiguous(old.Delivery, candidate.Delivery) {
				// GitHub timestamps are second-resolution. Leave tied revisions
				// actionable so the execution-time isolated-remote topology fence
				// can stale the ancestor and admit the descendant.
				continue
			} else if state.CompareDeliveryGeneration(old.Delivery, old.CreatedAt, candidate.Delivery, candidate.CreatedAt) > 0 {
				newerExists = true
			}
			if old.Status == state.ApprovalStatusPending || old.Status == state.ApprovalStatusApproved {
				staleIDs = append(staleIDs, id)
			}
		}
		if newerExists {
			candidate.Status = state.ApprovalStatusSuperseded
			candidate.UpdatedAt = now
			candidate.Audit = append(candidate.Audit, state.ApprovalAudit{
				At:          now,
				Event:       state.ApprovalAuditSuperseded,
				Reason:      "delivery generation is not safely orderable",
				PayloadHash: candidate.PayloadHash,
			})
			candidate = *state.CanonicalDeliveryApprovalForWrite(&candidate)
		}
		if newerExists {
			// A known newer generation remains authoritative; do not mutate it or
			// its siblings merely because an old standing reconcile arrived late.
			staleIDs = nil
		}
	}

	for _, id := range staleIDs {
		old, oldBinding, loadErr := loadApprovalTx(ctx, tx, b.StateDir, id)
		if loadErr != nil {
			return false, loadErr
		}
		old.Status = state.ApprovalStatusSuperseded
		old.UpdatedAt = now
		reason := fmt.Sprintf("superseded by delivery approval %s for revision %s", candidate.ID, shortDeliverySHA(candidate.Delivery.MergedSHA))
		audit := state.ApprovalAudit{
			At:              now,
			Event:           state.ApprovalAuditSuperseded,
			Reason:          reason,
			PayloadHash:     old.PayloadHash,
			TargetStateHash: old.TargetStateHash,
		}
		old.Audit = append(old.Audit, audit)
		old = state.CanonicalDeliveryApprovalForWrite(old)
		audit = old.Audit[len(old.Audit)-1]
		res, updateErr := tx.ExecContext(ctx,
			`UPDATE approvals SET status = ?, updated_at = ? WHERE state_dir = ? AND id = ? AND status IN (?, ?)`,
			string(state.ApprovalStatusSuperseded), formatTime(now), b.StateDir, id,
			string(state.ApprovalStatusPending), string(state.ApprovalStatusApproved))
		if updateErr != nil {
			return false, updateErr
		}
		if n, rowsErr := res.RowsAffected(); rowsErr != nil {
			return false, rowsErr
		} else if n != 1 {
			return false, fmt.Errorf("delivery approval %s lost supersede race", id)
		}
		if err := writeApprovalJSONTx(ctx, tx, oldBinding, old); err != nil {
			return false, err
		}
		if err := insertAuditTx(ctx, tx, id, oldBinding, audit); err != nil {
			return false, err
		}
	}

	candidate = *state.CanonicalDeliveryApprovalForWrite(&candidate)
	outcome, err := putApprovalTx(ctx, tx, &candidate, b)
	if err != nil {
		return false, err
	}
	inserted = outcome != approvalPutKept
	if inserted {
		for i := range candidate.Audit {
			if err := insertAuditTx(ctx, tx, candidate.ID, b, candidate.Audit[i]); err != nil {
				return false, err
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return inserted, nil
}

func shortDeliverySHA(sha string) string {
	sha = strings.TrimSpace(sha)
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

// Get returns the full approval (audit included, via approval_json) for id
// within the given state_dir scope, or state.ErrApprovalNotFound.
func (s *Store) Get(ctx context.Context, stateDir, id string) (*state.Approval, error) {
	canonical, err := canonicalStateDir(stateDir)
	if err != nil {
		return nil, fmt.Errorf("canonicalize approval state_dir: %w", err)
	}
	stateDir = canonical
	row := s.db.QueryRowContext(ctx, `SELECT approval_json, payload_hash, record_hash, id, project, repo, state_dir, action, status FROM approvals WHERE state_dir = ? AND id = ?`, stateDir, id)
	a, err := scanApproval(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, state.ErrApprovalNotFound
	}
	return a, err
}

// List returns every approval bound to the given state_dir, ordered by
// created_at. An empty stateDir returns all approvals.
func (s *Store) List(ctx context.Context, stateDir string) ([]*state.Approval, error) {
	var (
		rows *sql.Rows
		err  error
	)
	if strings.TrimSpace(stateDir) == "" {
		rows, err = s.db.QueryContext(ctx, `SELECT approval_json, payload_hash, record_hash, id, project, repo, state_dir, action, status FROM approvals ORDER BY created_at`)
	} else {
		stateDir, err = canonicalStateDir(stateDir)
		if err != nil {
			return nil, fmt.Errorf("canonicalize approval state_dir: %w", err)
		}
		rows, err = s.db.QueryContext(ctx, `SELECT approval_json, payload_hash, record_hash, id, project, repo, state_dir, action, status FROM approvals WHERE state_dir = ? ORDER BY created_at`, stateDir)
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
	canonical, err := canonicalStateDir(stateDir)
	if err != nil {
		return nil, fmt.Errorf("canonicalize approval state_dir: %w", err)
	}
	stateDir = canonical
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
	if a.Action == state.ApprovalActionDeployProject && a.Delivery.DeliveryExpired(now) {
		a.Delivery.StaleCause = state.DeliveryStaleCauseExpired
		if err := markStaleTx(ctx, tx, a, b, now, "delivery approval expired before execution"); err != nil {
			return a, err
		}
		if err := tx.Commit(); err != nil {
			return a, err
		}
		return a, state.ErrApprovalStale
	}
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
	if a.Action == state.ApprovalActionDeployProject {
		a = state.CanonicalDeliveryApprovalForWrite(a)
		audit = a.Audit[len(a.Audit)-1]
	}
	if err := writeApprovalJSONTx(ctx, tx, b, a); err != nil {
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
	canonical, err := canonicalStateDir(stateDir)
	if err != nil {
		return nil, fmt.Errorf("canonicalize approval state_dir: %w", err)
	}
	stateDir = canonical
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
	if a.Action == state.ApprovalActionDeployProject {
		a = state.CanonicalDeliveryApprovalForWrite(a)
		audit = a.Audit[len(a.Audit)-1]
	}
	if err := writeApprovalJSONTx(ctx, tx, b, a); err != nil {
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

// ClaimExecuting takes the durable approved → executing claim on a delivery
// approval (#872 safety addendum). It is the transactional primitive the daemon
// and CLI delivery executors contend on BEFORE any external side effect: with
// the same claim-once UPDATE ... WHERE status='approved' guard, exactly one
// caller observes rows-affected==1 and proceeds to run the delivery; every
// other caller — including this process after a crash-and-restart — sees a
// non-approved row and returns state.ErrApprovalNotApproved, so an in-flight or
// completed delivery is never replayed automatically. scoped to one state_dir.
func (s *Store) ClaimExecuting(ctx context.Context, stateDir, id string, now time.Time, actor, reason string) (*state.Approval, error) {
	return s.claimDeliveryExecuting(ctx, stateDir, id, "", now, actor, reason)
}

// ClaimDeliveryExecuting is ClaimExecuting with an atomic execution-spec
// fence. expectedConfigDigest is recomputed from the live config by the
// executor; mismatch durably stales the approval before returning.
func (s *Store) ClaimDeliveryExecuting(ctx context.Context, stateDir, id, expectedConfigDigest string, now time.Time, actor, reason string) (*state.Approval, error) {
	if strings.TrimSpace(expectedConfigDigest) == "" {
		return nil, errors.New("expected delivery config digest is required")
	}
	return s.claimDeliveryExecuting(ctx, stateDir, id, strings.TrimSpace(expectedConfigDigest), now, actor, reason)
}

func (s *Store) claimDeliveryExecuting(ctx context.Context, stateDir, id, expectedConfigDigest string, now time.Time, actor, reason string) (*state.Approval, error) {
	canonical, err := canonicalStateDir(stateDir)
	if err != nil {
		return nil, fmt.Errorf("canonicalize delivery state_dir: %w", err)
	}
	stateDir = canonical
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
	if expectedConfigDigest != "" && (a.Delivery == nil || strings.TrimSpace(a.Delivery.ConfigDigest) != expectedConfigDigest) {
		if a.Delivery != nil {
			a.Delivery.StaleCause = state.DeliveryStaleCauseConfigDrift
		}
		if err := markStaleTx(ctx, tx, a, b, now, "delivery config changed after approval; fresh approval required"); err != nil {
			return a, err
		}
		if err := tx.Commit(); err != nil {
			return a, err
		}
		return a, ErrDeliveryConfigMismatch
	}
	// Expiry must contend in the same transaction as approved→executing.
	// Checking it before this claim leaves a TOCTOU where the deadline passes
	// between the check and UPDATE, and returning stale without persisting it
	// leaves an approved row claimable by another process.
	if a.Action == state.ApprovalActionDeployProject && a.Delivery.DeliveryExpired(now) {
		a.Delivery.StaleCause = state.DeliveryStaleCauseExpired
		if err := markStaleTx(ctx, tx, a, b, now, "delivery approval expired before execution"); err != nil {
			return a, err
		}
		if err := tx.Commit(); err != nil {
			return a, err
		}
		return a, state.ErrApprovalStale
	}

	now = normalize(now)
	// The NOT EXISTS predicate is the project-scoped delivery execution lease.
	// It deliberately lives in the same write statement as approved→executing:
	// SQLite serializes competing writers, so two different approval IDs cannot
	// both observe the lease as free and start side effects concurrently.
	res, err := tx.ExecContext(ctx, `
UPDATE approvals
SET status = ?, updated_at = ?
WHERE state_dir = ? AND id = ? AND status = ?
  AND NOT EXISTS (
    SELECT 1 FROM approvals
    WHERE state_dir = ? AND action = ? AND status = ? AND id <> ?
  )`,
		string(state.ApprovalStatusExecuting), formatTime(now), stateDir, id, string(state.ApprovalStatusApproved),
		stateDir, string(state.ApprovalActionDeployProject), string(state.ApprovalStatusExecuting), id)
	if err != nil {
		return a, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return a, err
	}
	if n != 1 {
		reloaded, _, loadErr := loadApprovalTx(ctx, tx, stateDir, id)
		if loadErr == nil {
			a = reloaded
		}
		if a.Status == state.ApprovalStatusApproved {
			var executingID string
			leaseErr := tx.QueryRowContext(ctx, `
SELECT id FROM approvals
WHERE state_dir = ? AND action = ? AND status = ? AND id <> ?
LIMIT 1`, stateDir, string(state.ApprovalActionDeployProject), string(state.ApprovalStatusExecuting), id).Scan(&executingID)
			if leaseErr == nil {
				return a, ErrDeliveryInFlight
			}
			if !errors.Is(leaseErr, sql.ErrNoRows) {
				return a, leaseErr
			}
		}
		return a, state.ErrApprovalNotApproved
	}

	a.Status = state.ApprovalStatusExecuting
	a.UpdatedAt = now
	audit := state.ApprovalAudit{
		At:              now,
		Event:           state.ApprovalAuditExecuting,
		Actor:           actor,
		Reason:          reason,
		PayloadHash:     a.PayloadHash,
		TargetStateHash: a.TargetStateHash,
	}
	a.Audit = append(a.Audit, audit)
	if a.Action == state.ApprovalActionDeployProject {
		a = state.CanonicalDeliveryApprovalForWrite(a)
		audit = a.Audit[len(a.Audit)-1]
	}
	if err := writeApprovalJSONTx(ctx, tx, b, a); err != nil {
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

// FinishDelivery records the terminal result of a claimed delivery, moving it
// executing → executed (success) or execution_failed. It persists the
// caller-provided result payload (sanitized output, timings, verified flag)
// into the stored approval_json so the Fleet API/history reflect the run. The
// atomic UPDATE ... WHERE status='executing' guard keeps the result record
// idempotent: a second caller sees state.ErrApprovalNotExecuting and does not
// double-record.
func (s *Store) FinishDelivery(ctx context.Context, stateDir, id string, success bool, result *state.DeliveryPayload, now time.Time, actor, summary string) (*state.Approval, error) {
	target := state.ApprovalStatusExecuted
	event := state.ApprovalAuditExecuted
	if !success {
		target = state.ApprovalStatusExecutionFailed
		event = state.ApprovalAuditExecutionFailed
	}
	mutate := func(a *state.Approval) {
		a.Delivery = state.MergeDeliveryResult(a.Delivery, result)
	}
	return s.markFrom(ctx, stateDir, id, state.ApprovalStatusExecuting, target, event, state.ErrApprovalNotExecuting, now, actor, summary, mutate)
}

// ReleaseDeliveryExecuting atomically returns an executing delivery to
// approved only when the executor has not materialized a checkout or run any
// side effect. It is the retry path for a transient post-claim freshness check;
// interrupted/unknown executions must never call it and remain executing for
// explicit reconciliation.
func (s *Store) ReleaseDeliveryExecuting(ctx context.Context, stateDir, id string, now time.Time, actor string) (*state.Approval, error) {
	return s.markFrom(ctx, stateDir, id,
		state.ApprovalStatusExecuting, state.ApprovalStatusApproved,
		state.ApprovalAuditExecutionReleased, state.ErrApprovalNotExecuting,
		now, actor, "delivery claim released before side effect", nil)
}

// markFrom is the generalized single-source claim/transition: it atomically
// moves id from `from` to `target` behind UPDATE ... WHERE status=from, appends
// an audit entry, applies the optional mutate to the loaded approval before
// persisting its JSON, and returns notReady (unchanged) when the row is not in
// `from`. It generalizes markTerminal (which is from=approved) so the delivery
// executing claim/finish transitions reuse the same claim-once machinery.
func (s *Store) markFrom(ctx context.Context, stateDir, id string, from, target state.ApprovalStatus, event string, notReady error, now time.Time, actor, reason string, mutate func(*state.Approval)) (*state.Approval, error) {
	canonical, err := canonicalStateDir(stateDir)
	if err != nil {
		return nil, fmt.Errorf("canonicalize approval state_dir: %w", err)
	}
	stateDir = canonical
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
	if a.Status != from {
		return a, notReady
	}

	now = normalize(now)
	res, err := tx.ExecContext(ctx,
		`UPDATE approvals SET status = ?, updated_at = ? WHERE state_dir = ? AND id = ? AND status = ?`,
		string(target), formatTime(now), stateDir, id, string(from))
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
		return a, notReady
	}

	if mutate != nil {
		mutate(a)
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
	if a.Action == state.ApprovalActionDeployProject {
		a = state.CanonicalDeliveryApprovalForWrite(a)
		audit = a.Audit[len(a.Audit)-1]
	}
	if err := writeApprovalJSONTx(ctx, tx, b, a); err != nil {
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
	canonical, err := canonicalStateDir(stateDir)
	if err != nil {
		return nil, fmt.Errorf("canonicalize approval state_dir: %w", err)
	}
	stateDir = canonical
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

// approvalPutOutcome says what putApprovalTx did with the row. It exists so
// the drop case is a named, logged branch instead of an invisible side effect
// of a conflict clause (#1142).
type approvalPutOutcome int

const (
	// approvalPutInserted: no row existed for (state_dir, id).
	approvalPutInserted approvalPutOutcome = iota
	// approvalPutReminted: a strictly newer mint replaced the stored row.
	approvalPutReminted
	// approvalPutKept: the stored row was deliberately preserved because the
	// incoming record is a re-seed of the same (or an older) mint.
	approvalPutKept
)

// putApprovalTx writes an approval row, resolving a (state_dir, id) collision
// by mint time rather than by dropping the write.
//
// Ids are content-addressed on (action, target) and the row identity is scoped
// by state_dir (see the package comment: two DIFFERENT projects mint the SAME
// id, and the state_dir scope is what keeps their rows independent). Nothing
// here widens that scope — every statement stays keyed on (state_dir, id), so
// cross-project isolation is unchanged.
//
// Within one project the same id recurs when an earlier instance of the same
// decision has left the JSON state and the decision is minted again. CreatedAt
// separates the two cases, and it is the only signal that does so safely:
//
//   - incoming CreatedAt strictly after the stored one → a genuinely new mint;
//     replace the row (ON CONFLICT DO UPDATE). Without this the row keeps the
//     first instance's status forever — the drift this fixes.
//   - otherwise → a re-seed of the mint the row already holds (Put is called
//     on every claim from the same JSON record). Keep the row: resetting it
//     would re-open a claim that already advanced past pending.
//
// Statuses are deliberately NOT the predicate: `approved` and
// `awaiting_dispatch` are legitimately open, so a status-based rule would
// either reset a live claim or leave exactly the stale-approved rows this
// fixes. CreatedAt is compared as time.Time, never as SQL text: RFC3339Nano
// trims trailing zeros, so string ordering is wrong.
func putApprovalTx(ctx context.Context, tx *sql.Tx, a *state.Approval, b RowBinding) (approvalPutOutcome, error) {
	persisted := a
	if a.Action == state.ApprovalActionDeployProject {
		persisted = state.CanonicalDeliveryApprovalForWrite(a)
	}
	blob, err := json.Marshal(persisted)
	if err != nil {
		return approvalPutKept, fmt.Errorf("marshal approval %s: %w", a.ID, err)
	}
	recordHash := ""
	if persisted.Action == state.ApprovalActionDeployProject {
		recordHash, err = computeDeliveryRecordHash(persisted, b)
		if err != nil {
			return approvalPutKept, err
		}
	}

	// Read-then-write is atomic here: the DSN opens every transaction with
	// _txlock=immediate, so this tx already holds the write lock and no other
	// connection can insert the row between the SELECT and the INSERT.
	existingStatus, existingCreated, found, err := lookupApprovalMintTx(ctx, tx, b.StateDir, persisted.ID)
	if err != nil {
		return approvalPutKept, err
	}
	// A zero CreatedAt is NOT a newer mint. normalize() maps zero to time.Now(),
	// so without this an approval whose CreatedAt never got set - a legacy or
	// hand-edited state.json decodes the plain `created_at` field to zero, and
	// Put is exported - would always look strictly newer and reset an already
	// claimed row back to pending, re-opening the claim-once gate this package
	// exists to protect. Unknown provenance keeps the row.
	mintKnown := !persisted.CreatedAt.IsZero()
	mintedAt := normalize(persisted.CreatedAt)
	if found && (!mintKnown || !mintedAt.After(existingCreated)) {
		// Explicit keep branch — the one case where not writing is correct.
		if string(persisted.Status) != existingStatus {
			log.Printf("[approvalstore] keeping approval %s (state_dir=%s): stored status=%s, re-seed says %s; same mint %s, an advanced row is never reset",
				persisted.ID, b.StateDir, existingStatus, persisted.Status, formatTime(existingCreated))
		}
		return approvalPutKept, nil
	}

	if _, err := tx.ExecContext(ctx, `
INSERT INTO approvals(id, decision_id, project, repo, state_dir, action, status, payload_hash, record_hash, created_at, updated_at, approval_json)
VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(state_dir, id) DO UPDATE SET
	decision_id   = excluded.decision_id,
	project       = excluded.project,
	repo          = excluded.repo,
	action        = excluded.action,
	status        = excluded.status,
	payload_hash  = excluded.payload_hash,
	record_hash   = excluded.record_hash,
	created_at    = excluded.created_at,
	updated_at    = excluded.updated_at,
	approval_json = excluded.approval_json`,
		persisted.ID, persisted.DecisionID, b.Project, b.Repo, b.StateDir, persisted.Action, string(persisted.Status), persisted.PayloadHash, recordHash,
		formatTime(mintedAt), formatTime(normalize(persisted.UpdatedAt)), string(blob)); err != nil {
		return approvalPutKept, err
	}
	if !found {
		return approvalPutInserted, nil
	}
	log.Printf("[approvalstore] re-minted approval %s (state_dir=%s): status %s -> %s, mint %s -> %s",
		persisted.ID, b.StateDir, existingStatus, persisted.Status, formatTime(existingCreated), formatTime(mintedAt))
	return approvalPutReminted, nil
}

// lookupApprovalMintTx returns the stored status and mint time of the row
// under (state_dir, id). It reads the columns directly rather than going
// through loadApprovalTx so a delivery-integrity failure on an unrelated
// pre-existing row cannot block seeding a new approval.
func lookupApprovalMintTx(ctx context.Context, tx *sql.Tx, stateDir, id string) (string, time.Time, bool, error) {
	var status, createdAt string
	err := tx.QueryRowContext(ctx,
		`SELECT status, created_at FROM approvals WHERE state_dir = ? AND id = ?`, stateDir, id).
		Scan(&status, &createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		return "", time.Time{}, false, nil
	}
	if err != nil {
		return "", time.Time{}, false, err
	}
	created, perr := time.Parse(time.RFC3339Nano, createdAt)
	if perr != nil {
		return "", time.Time{}, false, fmt.Errorf("approval %s (state_dir=%s) has unparsable created_at %q: %w", id, stateDir, createdAt, perr)
	}
	return status, created.UTC(), true, nil
}

func writeApprovalJSONTx(ctx context.Context, tx *sql.Tx, b RowBinding, a *state.Approval) error {
	persisted := a
	if a.Action == state.ApprovalActionDeployProject {
		persisted = state.CanonicalDeliveryApprovalForWrite(a)
	}
	blob, err := json.Marshal(persisted)
	if err != nil {
		return fmt.Errorf("marshal approval %s: %w", a.ID, err)
	}
	recordHash := ""
	if persisted.Action == state.ApprovalActionDeployProject {
		recordHash, err = computeDeliveryRecordHash(persisted, b)
		if err != nil {
			return err
		}
	}
	_, err = tx.ExecContext(ctx,
		`UPDATE approvals SET payload_hash = ?, record_hash = ?, approval_json = ? WHERE state_dir = ? AND id = ?`,
		persisted.PayloadHash, recordHash, string(blob), b.StateDir, persisted.ID)
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
	if a.Action == state.ApprovalActionDeployProject && a.Delivery != nil && a.Delivery.StaleCause == "" {
		a.Delivery.StaleCause = state.DeliveryStaleCauseOther
	}
	audit := state.ApprovalAudit{
		At:              now,
		Event:           state.ApprovalAuditStale,
		Reason:          reason,
		PayloadHash:     a.PayloadHash,
		TargetStateHash: a.TargetStateHash,
	}
	a.Audit = append(a.Audit, audit)
	if a.Action == state.ApprovalActionDeployProject {
		a = state.CanonicalDeliveryApprovalForWrite(a)
		audit = a.Audit[len(a.Audit)-1]
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE approvals SET status = ?, updated_at = ? WHERE state_dir = ? AND id = ?`,
		string(state.ApprovalStatusStale), formatTime(now), b.StateDir, a.ID); err != nil {
		return err
	}
	if err := writeApprovalJSONTx(ctx, tx, b, a); err != nil {
		return err
	}
	return insertAuditTx(ctx, tx, a.ID, b, audit)
}

// loadApprovalTx returns the full approval plus its stored project/repo/
// state_dir binding so audit rows written by a later transition carry the
// same context that Put seeded.
func loadApprovalTx(ctx context.Context, tx *sql.Tx, scopeDir, id string) (*state.Approval, RowBinding, error) {
	var blob, project, repo, stateDir, sqlPayloadHash, sqlRecordHash, sqlID, sqlAction, sqlStatus string
	err := tx.QueryRowContext(ctx,
		`SELECT approval_json, project, repo, state_dir, payload_hash, record_hash, id, action, status FROM approvals WHERE state_dir = ? AND id = ?`, scopeDir, id).
		Scan(&blob, &project, &repo, &stateDir, &sqlPayloadHash, &sqlRecordHash, &sqlID, &sqlAction, &sqlStatus)
	if err != nil {
		return nil, RowBinding{}, err
	}
	var a state.Approval
	if err := json.Unmarshal([]byte(blob), &a); err != nil {
		return nil, RowBinding{}, fmt.Errorf("unmarshal approval %s: %w", id, err)
	}
	if a.Action == state.ApprovalActionDeployProject || sqlAction == state.ApprovalActionDeployProject || sqlRecordHash != "" {
		canonical, integrityErr := verifyDeliveryIntegrity(&a, sqlPayloadHash, sqlRecordHash, sqlID, sqlAction, sqlStatus, RowBinding{Project: project, Repo: repo, StateDir: stateDir})
		return canonical, RowBinding{Project: project, Repo: repo, StateDir: stateDir}, integrityErr
	}
	return &a, RowBinding{Project: project, Repo: repo, StateDir: stateDir}, nil
}

// rowScanner is satisfied by both *sql.Row and *sql.Rows.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanApproval(row rowScanner) (*state.Approval, error) {
	var blob, sqlPayloadHash, sqlRecordHash, sqlID, project, repo, stateDir, sqlAction, sqlStatus string
	if err := row.Scan(&blob, &sqlPayloadHash, &sqlRecordHash, &sqlID, &project, &repo, &stateDir, &sqlAction, &sqlStatus); err != nil {
		return nil, err
	}
	var a state.Approval
	if err := json.Unmarshal([]byte(blob), &a); err != nil {
		return nil, fmt.Errorf("unmarshal approval: %w", err)
	}
	if a.Action == state.ApprovalActionDeployProject || sqlAction == state.ApprovalActionDeployProject || sqlRecordHash != "" {
		return verifyDeliveryIntegrity(&a, sqlPayloadHash, sqlRecordHash, sqlID, sqlAction, sqlStatus, RowBinding{Project: project, Repo: repo, StateDir: stateDir})
	}
	return &a, nil
}

func verifyDeliveryIntegrity(raw *state.Approval, sqlPayloadHash, sqlRecordHash, sqlID, sqlAction, sqlStatus string, b RowBinding) (*state.Approval, error) {
	canonical := state.CanonicalDeliveryApproval(raw)
	if canonical == nil || raw.Action != state.ApprovalActionDeployProject || sqlAction != state.ApprovalActionDeployProject ||
		canonical.Delivery == nil || strings.TrimSpace(raw.PayloadHash) == "" || strings.TrimSpace(sqlRecordHash) == "" ||
		raw.PayloadHash != strings.TrimSpace(sqlPayloadHash) || canonical.ComputePayloadHash() != raw.PayloadHash ||
		canonical.ID != sqlID || string(canonical.Status) != sqlStatus ||
		!strings.EqualFold(strings.TrimSpace(canonical.Repo), strings.TrimSpace(b.Repo)) ||
		!strings.EqualFold(strings.TrimSpace(canonical.Delivery.Repo), strings.TrimSpace(b.Repo)) ||
		strings.TrimSpace(canonical.Project) != strings.TrimSpace(b.Project) ||
		strings.TrimSpace(canonical.Delivery.Project) != strings.TrimSpace(b.Project) {
		return canonical, ErrDeliveryIntegrity
	}
	recordHash, err := computeDeliveryRecordHash(canonical, b)
	if err != nil || recordHash != strings.TrimSpace(sqlRecordHash) {
		return canonical, ErrDeliveryIntegrity
	}
	return canonical, nil
}

// computeDeliveryRecordHash binds the complete strict delivery record — row
// scope, status, immutable approval payload, structured result, and canonical
// audit — independently from PayloadHash (which intentionally covers only the
// immutable authorization). Every transition updates this digest in the same
// SQLite transaction as status + approval_json.
func computeDeliveryRecordHash(a *state.Approval, b RowBinding) (string, error) {
	canonical := state.CanonicalDeliveryApproval(a)
	if canonical == nil || canonical.Delivery == nil || canonical.Action != state.ApprovalActionDeployProject {
		return "", fmt.Errorf("%w: delivery payload is required", ErrDeliveryIntegrity)
	}
	b.Project = strings.TrimSpace(b.Project)
	b.Repo = strings.TrimSpace(b.Repo)
	b.StateDir = filepath.Clean(strings.TrimSpace(b.StateDir))
	if b.Project == "" || b.Repo == "" || b.StateDir == "" || b.StateDir == "." ||
		strings.TrimSpace(canonical.Project) != b.Project ||
		strings.TrimSpace(canonical.Delivery.Project) != b.Project ||
		!strings.EqualFold(strings.TrimSpace(canonical.Repo), b.Repo) ||
		!strings.EqualFold(strings.TrimSpace(canonical.Delivery.Repo), b.Repo) {
		return "", fmt.Errorf("%w: delivery row binding mismatch", ErrDeliveryIntegrity)
	}
	if err := validateDeliveryTerminalResult(canonical); err != nil {
		return "", err
	}
	record := struct {
		Version  int             `json:"version"`
		Binding  RowBinding      `json:"binding"`
		Approval *state.Approval `json:"approval"`
	}{Version: 1, Binding: b, Approval: canonical}
	blob, err := json.Marshal(record)
	if err != nil {
		return "", fmt.Errorf("marshal delivery record %s: %w", canonical.ID, err)
	}
	sum := sha256.Sum256(blob)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

func validateDeliveryTerminalResult(a *state.Approval) error {
	if a == nil || a.Delivery == nil {
		return fmt.Errorf("%w: delivery payload is required", ErrDeliveryIntegrity)
	}
	p := a.Delivery
	successTimes := func() bool {
		return !p.StartedAt.IsZero() && !p.FinishedAt.IsZero() && !p.FinishedAt.Before(p.StartedAt)
	}
	zero := func(code *int) bool { return code != nil && *code == 0 }
	switch a.Status {
	case state.ApprovalStatusExecuted:
		if !successTimes() || !p.Verified || p.FailureStage != "" || p.TimedOut ||
			strings.TrimSpace(p.ExecutedRevision) == "" ||
			!strings.EqualFold(strings.TrimSpace(p.ExecutedRevision), strings.TrimSpace(p.MergedSHA)) ||
			!zero(p.VerifyExitCode) {
			return fmt.Errorf("%w: invalid verified delivery result", ErrDeliveryIntegrity)
		}
		switch p.CompletionSource {
		case "":
			if p.ReconcileOutcome != "" || !zero(p.DeployExitCode) {
				return fmt.Errorf("%w: invalid executor delivery result", ErrDeliveryIntegrity)
			}
		case state.DeliveryCompletionSourceOperatorReconcile:
			if p.ReconcileOutcome != state.DeliveryReconcileOutcomeVerified ||
				(p.DeployExitCode != nil && *p.DeployExitCode != 0) ||
				!hasOperatorReconcileAudit(a) {
				return fmt.Errorf("%w: invalid verified reconciliation result", ErrDeliveryIntegrity)
			}
		default:
			return fmt.Errorf("%w: unknown delivery completion source", ErrDeliveryIntegrity)
		}
	case state.ApprovalStatusExecutionFailed:
		if p.Verified || p.FinishedAt.IsZero() {
			return fmt.Errorf("%w: invalid failed delivery result", ErrDeliveryIntegrity)
		}
		switch p.CompletionSource {
		case "":
			if p.ReconcileOutcome != "" || p.FailureStage == "" {
				return fmt.Errorf("%w: invalid executor failure result", ErrDeliveryIntegrity)
			}
		case state.DeliveryCompletionSourceOperatorReconcile:
			if p.ReconcileOutcome != state.DeliveryReconcileOutcomeNotApplied &&
				p.ReconcileOutcome != state.DeliveryReconcileOutcomeRemediatedFailed {
				return fmt.Errorf("%w: invalid failed reconciliation result", ErrDeliveryIntegrity)
			}
			if !successTimes() || !hasOperatorReconcileAudit(a) {
				return fmt.Errorf("%w: invalid failed reconciliation result", ErrDeliveryIntegrity)
			}
		default:
			return fmt.Errorf("%w: unknown delivery completion source", ErrDeliveryIntegrity)
		}
	default:
		if p.CompletionSource != "" || p.ReconcileOutcome != "" {
			return fmt.Errorf("%w: reconciliation metadata on non-terminal delivery", ErrDeliveryIntegrity)
		}
	}
	return nil
}

// hasOperatorReconcileAudit binds recovery-only result metadata to the sole
// store transition that enforces the explicit operator assertions. Without
// this link, a future/internal caller could pass CompletionSource through the
// general FinishDelivery API and bypass RunnerGone/TargetSafe/config fences.
func hasOperatorReconcileAudit(a *state.Approval) bool {
	if a == nil || len(a.Audit) == 0 {
		return false
	}
	last := a.Audit[len(a.Audit)-1]
	return last.Event == state.ApprovalAuditDeliveryReconciled && last.Actor == DeliveryReconcileActor
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
