// Package emergencystore persists the fleet-wide EMERGENCY STOP switch (#840) in
// the SAME shared maestro.db that internal/approvalstore, internal/statestore,
// internal/webhookstore and internal/mirrorstore already use.
//
// Unlike the per-project `maestro pause`/`drain` flags (which live in each
// project's state.json and must be toggled once per project), the emergency
// stop is ONE fleet-wide row: a single `maestro emergency stop-llm` writes it
// and every flow honors it. Storing it in the unified DB (not in-memory) is a
// hard requirement — it must survive a `systemctl restart maestro.service` and
// be writable by the CLI directly (`maestro emergency stop-llm --db
// ~/.maestro/maestro.db`) even when the web/daemon is down, with the running
// daemon picking it up on its next poll (#840).
//
// The table holds a single logical row keyed by a constant scope ('fleet'), so
// a set is an idempotent upsert and a read is a single primary-key lookup.
package emergencystore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// Schema creates the single emergency_stop table. IF NOT EXISTS so Init is
// idempotent and safe to run against the same maestro.db the other stores
// initialise (each package owns disjoint tables in one file).
const Schema = `
CREATE TABLE IF NOT EXISTS emergency_stop (
	scope      TEXT PRIMARY KEY,
	level      TEXT NOT NULL DEFAULT 'none',
	since      TEXT NOT NULL DEFAULT '',
	actor      TEXT NOT NULL DEFAULT '',
	reason     TEXT NOT NULL DEFAULT '',
	updated_at TEXT NOT NULL DEFAULT ''
);
`

// fleetScope is the constant primary key of the single fleet-wide row. The
// switch is deliberately fleet-wide (not per-project), so one row is all the
// schema needs.
const fleetScope = "fleet"

// Level is the emergency-stop level. Both non-none levels halt every LLM call
// and every new worker spawn fleet-wide; they differ only in how in-flight work
// and the daemon lifecycle are treated (see the CLI verbs in cmd/maestro).
type Level string

const (
	// LevelNone is the normal, unstopped state.
	LevelNone Level = "none"
	// LevelLLMStopped halts all LLM spend (supervisor LLM path → deterministic
	// only, router calls stopped, no new worker spawns) while the daemon keeps
	// running: dashboards, state, GitHub reads and watchdogs stay alive so the
	// operator can watch while nothing spends. In-flight tmux workers are left
	// running unless the operator also passed --kill-workers.
	LevelLLMStopped Level = "llm_stopped"
	// LevelAllStopped is the whole-fleet stop: everything LevelLLMStopped does,
	// recorded as an emergency state that survives a restart — the daemon comes
	// back up in stopped mode until an explicit `maestro emergency resume`.
	LevelAllStopped Level = "all_stopped"
)

// ParseLevel canonicalises a level string. Empty is treated as LevelNone.
func ParseLevel(s string) (Level, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", string(LevelNone), "resume":
		return LevelNone, nil
	case string(LevelLLMStopped), "llm", "stop-llm":
		return LevelLLMStopped, nil
	case string(LevelAllStopped), "all", "stop-all":
		return LevelAllStopped, nil
	default:
		return "", fmt.Errorf("unknown emergency level %q (want none|llm_stopped|all_stopped)", s)
	}
}

// State is the current fleet-wide emergency switch, as read from the store.
type State struct {
	Level  Level
	Since  time.Time // when the current level was set (UTC); zero when LevelNone
	Actor  string    // who set it
	Reason string    // why
}

// Active reports whether any emergency stop is in effect.
func (s State) Active() bool {
	return s.Level == LevelLLMStopped || s.Level == LevelAllStopped
}

// HaltsLLM reports whether LLM calls must be suppressed (supervisor
// deterministic-only, router calls stopped). Both non-none levels do.
func (s State) HaltsLLM() bool { return s.Active() }

// HaltsSpawns reports whether the orchestrator spawn gate must be closed. Both
// non-none levels do.
func (s State) HaltsSpawns() bool { return s.Active() }

// ActivationMessage renders the notify_red-grade alert sent when the switch is
// engaged (#840 requirement 5/9). It is shared by the CLI verb and the dashboard
// endpoint so the wording is identical whichever surface pressed the button.
func (s State) ActivationMessage() string {
	scope := "all LLM calls & new worker spawns halted"
	if s.Level == LevelAllStopped {
		scope = "whole fleet stopped (survives restart until resume)"
	}
	msg := "🛑 maestro: EMERGENCY STOP activated — " + scope
	if a := strings.TrimSpace(s.Actor); a != "" {
		msg += "; by " + a
	}
	if r := strings.TrimSpace(s.Reason); r != "" {
		msg += "; reason: " + r
	}
	msg += ". Run `maestro emergency resume` to restore normal operation."
	return msg
}

// ResumeMessage renders the notify_red-grade alert sent when the switch is
// cleared. prev is the level being lifted (for context); cur carries the
// resuming actor.
func ResumeMessage(prev, cur State) string {
	msg := "✅ maestro: emergency stop resumed — normal operation restored"
	if a := strings.TrimSpace(cur.Actor); a != "" {
		msg += "; by " + a
	}
	if prev.Active() {
		msg += " (was " + string(prev.Level) + ")"
	}
	return msg
}

// Store is a SQLite-backed store for the fleet-wide emergency switch.
type Store struct {
	db *sql.DB
}

// DefaultDBPath is the shared database (~/.maestro/maestro.db) — the same file
// the approvals/state/webhook/mirror stores use. A single file holds every
// project's state; the emergency switch lives in its own table.
func DefaultDBPath() string {
	return filepath.Join(os.Getenv("HOME"), ".maestro", "maestro.db")
}

// Open opens (creating the parent dir if needed) the database and ensures the
// schema. The pragmas mirror internal/webhookstore: busy_timeout lets a
// connection wait for a lock instead of erroring "database is locked" (the CLI
// verb writes from a separate process than the daemon), WAL lets a reader and a
// writer proceed concurrently across processes (and is what makes the switch
// durable across a daemon restart — #840 acceptance), and MaxOpenConns(1)
// removes the SQLITE_BUSY modernc/sqlite can report between two pooled
// connections of the same process.
func Open(path string) (*Store, error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return nil, fmt.Errorf("create emergency db dir %s: %w", dir, err)
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

// Get returns the current fleet-wide emergency state. A missing row (no
// emergency ever set) reads back as LevelNone with a zero timestamp, so callers
// never special-case "table empty".
func (s *Store) Get(ctx context.Context) (State, error) {
	var level, since, actor, reason string
	err := s.db.QueryRowContext(ctx,
		`SELECT level, since, actor, reason FROM emergency_stop WHERE scope = ?`, fleetScope).
		Scan(&level, &since, &actor, &reason)
	if errors.Is(err, sql.ErrNoRows) {
		return State{Level: LevelNone}, nil
	}
	if err != nil {
		return State{}, err
	}
	parsed, perr := ParseLevel(level)
	if perr != nil {
		// A row with a corrupt level is safer read as an active stop than as
		// "normal": fail toward not spending. Surface it via all-stopped.
		parsed = LevelAllStopped
	}
	st := State{Level: parsed, Actor: actor, Reason: reason}
	if parsed == LevelNone {
		return st, nil
	}
	if t, terr := time.Parse(time.RFC3339, since); terr == nil {
		st.Since = t.UTC()
	}
	return st, nil
}

// Set writes the fleet-wide emergency level. actor/reason are recorded verbatim;
// `at` is stamped as the RFC3339 "since" timestamp when the level is non-none.
// Setting LevelNone is a resume (Resume is the named alias). The write is an
// idempotent upsert on the single 'fleet' row.
func (s *Store) Set(ctx context.Context, level Level, actor, reason string, at time.Time) error {
	if _, err := ParseLevel(string(level)); err != nil {
		return err
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	since := ""
	if level != LevelNone {
		since = at.UTC().Format(time.RFC3339)
	}
	_, err := s.db.ExecContext(ctx, `
INSERT INTO emergency_stop(scope, level, since, actor, reason, updated_at)
VALUES(?, ?, ?, ?, ?, ?)
ON CONFLICT(scope) DO UPDATE SET level = excluded.level, since = excluded.since, actor = excluded.actor, reason = excluded.reason, updated_at = excluded.updated_at`,
		fleetScope, string(level), since, strings.TrimSpace(actor), strings.TrimSpace(reason), now)
	return err
}

// Resume clears any emergency stop (sets LevelNone), recording who resumed. It
// is Set(LevelNone, …) under a clearer name for the `maestro emergency resume`
// verb.
func (s *Store) Resume(ctx context.Context, actor string, at time.Time) error {
	return s.Set(ctx, LevelNone, actor, "", at)
}

// Fingerprint returns the row's updated_at timestamp, used by the daemon's
// emergency-watch poll loop to detect a flip without decoding the full state on
// every tick. A missing row returns the zero time.
func (s *Store) Fingerprint(ctx context.Context) (time.Time, error) {
	var updatedAt string
	err := s.db.QueryRowContext(ctx,
		`SELECT updated_at FROM emergency_stop WHERE scope = ?`, fleetScope).Scan(&updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, err
	}
	if updatedAt == "" {
		return time.Time{}, nil
	}
	t, err := time.Parse(time.RFC3339Nano, updatedAt)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse emergency updated_at: %w", err)
	}
	return t, nil
}
