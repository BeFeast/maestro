// Package statestore persists the orchestration State slices that Phase 4 left
// in per-project JSON — Sessions, SupervisorDecisions, BackendHealth and
// Missions — in the SAME shared maestro.db that internal/approvalstore already
// uses. The target model (epic #754) is "one DB per daemon": every project's
// state lives in unified tables keyed by project/repo/state_dir, so a
// cross-project question ("every running session across the fleet") is one SQL
// query instead of N state.Load calls.
//
// During the transition JSON remains the authoritative mint source: the daemon
// imports each project's state.json into these tables on startup (idempotent,
// one-shot) and write-through-mirrors every subsequent Save. Because the JSON
// path still owns the flock+3-way-merge write, the SQLite copy is a faithful
// read-optimised mirror — ImportState replaces a project's rows wholesale, so
// re-running it (startup re-import or a write-through after a no-op Save) always
// converges to the current snapshot.
//
// Every row carries project / repo / state_dir columns so one shared maestro.db
// holds every project's state without cross-project bleed. state_dir is the
// per-project identity (1:1 with a project; repo can be shared by two configs,
// the project name can be aliased, but the state dir is unique), mirroring the
// scoping internal/approvalstore uses for its (state_dir, id) row key.
package statestore

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
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

// Schema creates the sessions / decisions / backend_health / missions tables.
// All are IF NOT EXISTS so Init is idempotent and safe to run against the same
// maestro.db internal/approvalstore initialises (the two packages own disjoint
// tables in one file). The denormalised project/repo/state_dir columns bind
// every row to its originating project.
const Schema = `
CREATE TABLE IF NOT EXISTS sessions (
	state_dir    TEXT NOT NULL,
	project      TEXT NOT NULL DEFAULT '',
	repo         TEXT NOT NULL DEFAULT '',
	slot         TEXT NOT NULL,
	issue_number INTEGER NOT NULL DEFAULT 0,
	pr_number    INTEGER NOT NULL DEFAULT 0,
	status       TEXT NOT NULL DEFAULT '',
	backend      TEXT NOT NULL DEFAULT '',
	started_at   TEXT NOT NULL DEFAULT '',
	updated_at   TEXT NOT NULL DEFAULT '',
	session_json TEXT NOT NULL,
	PRIMARY KEY (state_dir, slot)
);

CREATE INDEX IF NOT EXISTS idx_sessions_status ON sessions(status);

CREATE TABLE IF NOT EXISTS decisions (
	state_dir     TEXT NOT NULL,
	project       TEXT NOT NULL DEFAULT '',
	repo          TEXT NOT NULL DEFAULT '',
	id            TEXT NOT NULL,
	created_at    TEXT NOT NULL DEFAULT '',
	decision_json TEXT NOT NULL,
	PRIMARY KEY (state_dir, id)
);

CREATE INDEX IF NOT EXISTS idx_decisions_created ON decisions(state_dir, created_at);

CREATE TABLE IF NOT EXISTS backend_health (
	state_dir    TEXT NOT NULL,
	project      TEXT NOT NULL DEFAULT '',
	repo         TEXT NOT NULL DEFAULT '',
	backend      TEXT NOT NULL,
	health_state TEXT NOT NULL DEFAULT '',
	updated_at   TEXT NOT NULL DEFAULT '',
	health_json  TEXT NOT NULL,
	PRIMARY KEY (state_dir, backend)
);

CREATE TABLE IF NOT EXISTS missions (
	state_dir    TEXT NOT NULL,
	project      TEXT NOT NULL DEFAULT '',
	repo         TEXT NOT NULL DEFAULT '',
	parent_issue INTEGER NOT NULL,
	status       TEXT NOT NULL DEFAULT '',
	mission_json TEXT NOT NULL,
	PRIMARY KEY (state_dir, parent_issue)
);
`

// RowBinding is the per-project context stamped onto every row so one shared
// maestro.db can hold every project's state. It mirrors
// approvalstore.RowBinding.
type RowBinding struct {
	Project  string
	Repo     string
	StateDir string
}

// Store is a SQLite-backed store for the non-approval orchestration state.
type Store struct {
	db *sql.DB
}

// DefaultDBPath is the shared state database (~/.maestro/maestro.db), the same
// file internal/approvalstore uses. A single file holds every project's state;
// rows are tagged by project/repo/state_dir.
func DefaultDBPath() string {
	return filepath.Join(os.Getenv("HOME"), ".maestro", "maestro.db")
}

// Open opens (creating the parent dir if needed) the state database and ensures
// the schema. The pragmas mirror internal/configstore and internal/approvalstore:
// busy_timeout lets a connection wait for a lock instead of erroring "database
// is locked" (CLI verbs may write from a separate process than the daemon), WAL
// lets a reader and a writer proceed concurrently across processes, and
// MaxOpenConns(1) removes the SQLITE_BUSY that modernc/sqlite can still report
// between two pooled connections of the same process.
func Open(path string) (*Store, error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return nil, fmt.Errorf("create state db dir %s: %w", dir, err)
		}
	}
	dsn := path
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
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

// Init creates the schema. Idempotent.
func (s *Store) Init(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, Schema)
	return err
}

// ImportState replaces every row bound to b.StateDir with the slices carried by
// st, in one transaction. It is the one-shot startup import AND the
// write-through mirror: both want the SQLite copy to equal the JSON snapshot for
// one project, so a wholesale per-project delete+insert is the simplest faithful
// (and idempotent) sync — a session, decision, health entry or mission removed
// from JSON disappears from the mirror on the next import, and re-importing an
// unchanged state is a no-op observable result.
//
// st==nil clears the project's rows (an empty snapshot), so a freshly-deleted
// project does not strand stale rows in a cross-project query.
func (s *Store) ImportState(ctx context.Context, b RowBinding, st *state.State) error {
	if strings.TrimSpace(b.StateDir) == "" {
		return errors.New("state_dir is required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	for _, table := range []string{"sessions", "decisions", "backend_health", "missions"} {
		if _, err := tx.ExecContext(ctx, `DELETE FROM `+table+` WHERE state_dir = ?`, b.StateDir); err != nil {
			return fmt.Errorf("clear %s for %s: %w", table, b.StateDir, err)
		}
	}
	if st == nil {
		return tx.Commit()
	}

	now := time.Now().UTC()
	if err := insertSessionsTx(ctx, tx, b, st.Sessions, now); err != nil {
		return err
	}
	if err := insertDecisionsTx(ctx, tx, b, st.SupervisorDecisions); err != nil {
		return err
	}
	if err := insertBackendHealthTx(ctx, tx, b, st.BackendHealth, now); err != nil {
		return err
	}
	if err := insertMissionsTx(ctx, tx, b, st.Missions); err != nil {
		return err
	}
	return tx.Commit()
}

// ClearStateDir removes every row bound to stateDir (all four tables) — the
// removal counterpart used when a project is dropped from the fleet at runtime
// (#760), so its sessions/health stop surfacing in cross-project queries the
// moment it leaves. It is ImportState(stateDir, nil) under a clearer name.
func (s *Store) ClearStateDir(ctx context.Context, stateDir string) error {
	return s.ImportState(ctx, RowBinding{StateDir: stateDir}, nil)
}

// RetainStateDirs deletes every row whose state_dir is NOT in keep, across all
// four tables, in one transaction. It is the startup reconcile for projects that
// vanished from the config store while the daemon was stopped (#760): the import
// loop refreshes the rows of the CURRENT fleet, and this drops the stale rows of
// any project no longer present, so a cross-project query never returns sessions
// or health for a project that is not running. An empty keep set clears every row
// (no projects → no state). Blank/duplicate dirs in keep are ignored.
func (s *Store) RetainStateDirs(ctx context.Context, keep []string) error {
	seen := make(map[string]bool, len(keep))
	placeholders := make([]string, 0, len(keep))
	args := make([]any, 0, len(keep))
	for _, dir := range keep {
		dir = strings.TrimSpace(dir)
		if dir == "" || seen[dir] {
			continue
		}
		seen[dir] = true
		placeholders = append(placeholders, "?")
		args = append(args, dir)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, table := range []string{"sessions", "decisions", "backend_health", "missions"} {
		q := `DELETE FROM ` + table
		if len(placeholders) > 0 {
			q += ` WHERE state_dir NOT IN (` + strings.Join(placeholders, ", ") + `)`
		}
		if _, err := tx.ExecContext(ctx, q, args...); err != nil {
			return fmt.Errorf("prune %s: %w", table, err)
		}
	}
	return tx.Commit()
}

// ProjectSession is one session row with its originating project binding,
// returned by the cross-project queries so a fleet caller knows which project a
// session belongs to without a second lookup.
type ProjectSession struct {
	RowBinding
	Slot    string
	Session *state.Session
}

// RunningSessions returns every session across all projects whose status is
// StatusRunning, in one query — the canonical cross-project example from #760
// (all running sessions across the fleet without N state.Load calls).
func (s *Store) RunningSessions(ctx context.Context) ([]ProjectSession, error) {
	return s.SessionsByStatus(ctx, string(state.StatusRunning))
}

// SessionsByStatus returns every session across all projects whose status is in
// the given set, ordered by project then slot, in one query. An empty set
// returns every session.
func (s *Store) SessionsByStatus(ctx context.Context, statuses ...string) ([]ProjectSession, error) {
	q := `SELECT project, repo, state_dir, slot, session_json FROM sessions`
	args := make([]any, 0, len(statuses))
	if len(statuses) > 0 {
		placeholders := make([]string, len(statuses))
		for i, st := range statuses {
			placeholders[i] = "?"
			args = append(args, st)
		}
		q += ` WHERE status IN (` + strings.Join(placeholders, ", ") + `)`
	}
	q += ` ORDER BY project, state_dir, slot`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]ProjectSession, 0)
	for rows.Next() {
		var project, repo, stateDir, slot, blob string
		if err := rows.Scan(&project, &repo, &stateDir, &slot, &blob); err != nil {
			return nil, err
		}
		var sess state.Session
		if err := json.Unmarshal([]byte(blob), &sess); err != nil {
			return nil, fmt.Errorf("unmarshal session %s/%s: %w", stateDir, slot, err)
		}
		out = append(out, ProjectSession{
			RowBinding: RowBinding{Project: project, Repo: repo, StateDir: stateDir},
			Slot:       slot,
			Session:    &sess,
		})
	}
	return out, rows.Err()
}

// Sessions returns every session bound to stateDir, keyed by slot — the SQLite
// equivalent of state.Load(stateDir).Sessions.
func (s *Store) Sessions(ctx context.Context, stateDir string) (map[string]*state.Session, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT slot, session_json FROM sessions WHERE state_dir = ? ORDER BY slot`, stateDir)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]*state.Session)
	for rows.Next() {
		var slot, blob string
		if err := rows.Scan(&slot, &blob); err != nil {
			return nil, err
		}
		var sess state.Session
		if err := json.Unmarshal([]byte(blob), &sess); err != nil {
			return nil, fmt.Errorf("unmarshal session %s/%s: %w", stateDir, slot, err)
		}
		out[slot] = &sess
	}
	return out, rows.Err()
}

// Decisions returns every supervisor decision bound to stateDir, ordered by
// created_at (oldest first), matching the JSON slice order.
func (s *Store) Decisions(ctx context.Context, stateDir string) ([]state.SupervisorDecision, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT decision_json FROM decisions WHERE state_dir = ? ORDER BY created_at, id`, stateDir)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]state.SupervisorDecision, 0)
	for rows.Next() {
		var blob string
		if err := rows.Scan(&blob); err != nil {
			return nil, err
		}
		var d state.SupervisorDecision
		if err := json.Unmarshal([]byte(blob), &d); err != nil {
			return nil, fmt.Errorf("unmarshal decision: %w", err)
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// BackendHealth returns the backend-health map bound to stateDir, keyed by
// backend name.
func (s *Store) BackendHealth(ctx context.Context, stateDir string) (map[string]state.BackendHealth, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT backend, health_json FROM backend_health WHERE state_dir = ? ORDER BY backend`, stateDir)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]state.BackendHealth)
	for rows.Next() {
		var backend, blob string
		if err := rows.Scan(&backend, &blob); err != nil {
			return nil, err
		}
		var h state.BackendHealth
		if err := json.Unmarshal([]byte(blob), &h); err != nil {
			return nil, fmt.Errorf("unmarshal backend health %s/%s: %w", stateDir, backend, err)
		}
		out[backend] = h
	}
	return out, rows.Err()
}

// Missions returns the missions map bound to stateDir, keyed by parent issue.
func (s *Store) Missions(ctx context.Context, stateDir string) (map[int]*state.Mission, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT parent_issue, mission_json FROM missions WHERE state_dir = ? ORDER BY parent_issue`, stateDir)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[int]*state.Mission)
	for rows.Next() {
		var parent int
		var blob string
		if err := rows.Scan(&parent, &blob); err != nil {
			return nil, err
		}
		var m state.Mission
		if err := json.Unmarshal([]byte(blob), &m); err != nil {
			return nil, fmt.Errorf("unmarshal mission %s/%d: %w", stateDir, parent, err)
		}
		out[parent] = &m
	}
	return out, rows.Err()
}

// --- tx insert helpers ------------------------------------------------------

func insertSessionsTx(ctx context.Context, tx *sql.Tx, b RowBinding, sessions map[string]*state.Session, now time.Time) error {
	for slot, sess := range sessions {
		if sess == nil {
			continue
		}
		blob, err := json.Marshal(sess)
		if err != nil {
			return fmt.Errorf("marshal session %s: %w", slot, err)
		}
		if _, err := tx.ExecContext(ctx, `
INSERT INTO sessions(state_dir, project, repo, slot, issue_number, pr_number, status, backend, started_at, updated_at, session_json)
VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			b.StateDir, b.Project, b.Repo, slot, sess.IssueNumber, sess.PRNumber, string(sess.Status), sess.Backend,
			formatTime(sess.StartedAt), formatTime(now), string(blob)); err != nil {
			return fmt.Errorf("insert session %s: %w", slot, err)
		}
	}
	return nil
}

func insertDecisionsTx(ctx context.Context, tx *sql.Tx, b RowBinding, decisions []state.SupervisorDecision) error {
	for i := range decisions {
		d := decisions[i]
		blob, err := json.Marshal(d)
		if err != nil {
			return fmt.Errorf("marshal decision: %w", err)
		}
		// A decision normally carries a stable ID; fall back to a content hash
		// so an id-less legacy decision still gets a unique (state_dir, id) key
		// and is not collapsed with a sibling by the primary key.
		id := decisionRowID(d, blob)
		if _, err := tx.ExecContext(ctx, `
INSERT OR REPLACE INTO decisions(state_dir, project, repo, id, created_at, decision_json)
VALUES(?, ?, ?, ?, ?, ?)`,
			b.StateDir, b.Project, b.Repo, id, formatTime(d.CreatedAt), string(blob)); err != nil {
			return fmt.Errorf("insert decision %s: %w", id, err)
		}
	}
	return nil
}

func insertBackendHealthTx(ctx context.Context, tx *sql.Tx, b RowBinding, health map[string]state.BackendHealth, now time.Time) error {
	for backend, h := range health {
		blob, err := json.Marshal(h)
		if err != nil {
			return fmt.Errorf("marshal backend health %s: %w", backend, err)
		}
		if _, err := tx.ExecContext(ctx, `
INSERT INTO backend_health(state_dir, project, repo, backend, health_state, updated_at, health_json)
VALUES(?, ?, ?, ?, ?, ?, ?)`,
			b.StateDir, b.Project, b.Repo, backend, h.State, formatTime(now), string(blob)); err != nil {
			return fmt.Errorf("insert backend health %s: %w", backend, err)
		}
	}
	return nil
}

func insertMissionsTx(ctx context.Context, tx *sql.Tx, b RowBinding, missions map[int]*state.Mission) error {
	for parent, m := range missions {
		if m == nil {
			continue
		}
		blob, err := json.Marshal(m)
		if err != nil {
			return fmt.Errorf("marshal mission %d: %w", parent, err)
		}
		if _, err := tx.ExecContext(ctx, `
INSERT INTO missions(state_dir, project, repo, parent_issue, status, mission_json)
VALUES(?, ?, ?, ?, ?, ?)`,
			b.StateDir, b.Project, b.Repo, parent, m.Status, string(blob)); err != nil {
			return fmt.Errorf("insert mission %d: %w", parent, err)
		}
	}
	return nil
}

// decisionRowID mirrors state.decisionKey: the decision's own ID when set, else
// a content hash of its JSON so an id-less decision still keys uniquely.
func decisionRowID(d state.SupervisorDecision, blob []byte) string {
	if strings.TrimSpace(d.ID) != "" {
		return d.ID
	}
	sum := sha256.Sum256(blob)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func formatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}
