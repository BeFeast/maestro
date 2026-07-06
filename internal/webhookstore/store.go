// Package webhookstore persists inbound GitHub webhook deliveries in the SAME
// shared maestro.db that internal/approvalstore and internal/statestore already
// use (epic #811, phase B — issue #824). It is the durable landing zone for the
// fleet daemon's webhook ingestion endpoint: instead of polling GitHub for
// issue / PR / check / review / label state, the daemon accepts signed webhook
// deliveries and lands the raw payload plus a parsed envelope here, idempotently.
//
// Idempotency is keyed on the delivery's X-GitHub-Delivery UUID, which GitHub
// guarantees is stable across redeliveries of the same event. The row is the
// primary key, so a redelivery (GitHub retries on any non-2xx, and an operator
// can replay from the repo's webhook settings) is a no-op INSERT rather than a
// duplicate row — Insert reports stored=false for it so the caller can 200 the
// redelivery without side effects.
//
// This is ingestion only (phase B). No read-path consumer joins these rows into
// the orchestration state yet; that is phase C/D. The table therefore carries
// the raw payload verbatim so a later phase can re-parse whatever it needs
// without a schema migration, plus a small set of denormalised envelope columns
// (event type, action, repo, sender) so observability and routing questions are
// one indexed query.
package webhookstore

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// Schema creates the webhook_deliveries table. IF NOT EXISTS so Init is
// idempotent and safe to run against the same maestro.db approvalstore /
// statestore initialise (each package owns disjoint tables in one file). The
// delivery_id primary key is what makes a redelivery a no-op.
const Schema = `
CREATE TABLE IF NOT EXISTS webhook_deliveries (
	delivery_id TEXT PRIMARY KEY,
	event_type  TEXT NOT NULL DEFAULT '',
	action      TEXT NOT NULL DEFAULT '',
	repo        TEXT NOT NULL DEFAULT '',
	sender      TEXT NOT NULL DEFAULT '',
	hook_id     TEXT NOT NULL DEFAULT '',
	received_at TEXT NOT NULL DEFAULT '',
	payload     TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_webhook_deliveries_event ON webhook_deliveries(event_type);
CREATE INDEX IF NOT EXISTS idx_webhook_deliveries_received ON webhook_deliveries(received_at);
`

// Delivery is one ingested GitHub webhook delivery. Payload is the raw request
// body verbatim (the signed bytes), so a later read-path phase can re-parse it
// without depending on the denormalised columns.
type Delivery struct {
	DeliveryID string    // X-GitHub-Delivery UUID (idempotency key)
	EventType  string    // X-GitHub-Event (issues, pull_request, check_run, …)
	Action     string    // parsed envelope "action" (opened, labeled, completed, …)
	Repo       string    // parsed repository.full_name
	Sender     string    // parsed sender.login
	HookID     string    // X-GitHub-Hook-ID (the webhook that delivered it)
	ReceivedAt time.Time // when the daemon accepted it
	Payload    []byte    // raw request body
}

// Store is a SQLite-backed store for inbound webhook deliveries.
type Store struct {
	db *sql.DB
}

// DefaultDBPath is the shared database (~/.maestro/maestro.db) — the same file
// approvalstore and statestore use. A single file holds every project's state;
// webhook deliveries live in their own table.
func DefaultDBPath() string {
	return filepath.Join(os.Getenv("HOME"), ".maestro", "maestro.db")
}

// Open opens (creating the parent dir if needed) the database and ensures the
// schema. The pragmas mirror internal/statestore: busy_timeout lets a
// connection wait for a lock instead of erroring "database is locked", WAL lets
// a reader and a writer proceed concurrently (and is what makes an acknowledged
// delivery durable across a daemon restart — #824 acceptance), and
// MaxOpenConns(1) removes the SQLITE_BUSY modernc/sqlite can report between two
// pooled connections of the same process.
func Open(path string) (*Store, error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return nil, fmt.Errorf("create webhook db dir %s: %w", dir, err)
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

// Insert stores a delivery idempotently and reports whether the row was newly
// written. A delivery whose DeliveryID already exists is a no-op — INSERT OR
// IGNORE leaves the original row untouched and returns stored=false, so the
// caller can acknowledge a GitHub redelivery with 200 without any duplicate
// side effect (#824 acceptance: "replaying the same X-GitHub-Delivery does not
// duplicate").
//
// DeliveryID is required; GitHub always sets X-GitHub-Delivery, and a blank one
// would collapse every unkeyed delivery onto one row.
func (s *Store) Insert(ctx context.Context, d Delivery) (stored bool, err error) {
	if strings.TrimSpace(d.DeliveryID) == "" {
		return false, fmt.Errorf("delivery_id is required")
	}
	received := d.ReceivedAt
	if received.IsZero() {
		received = time.Now().UTC()
	}
	res, err := s.db.ExecContext(ctx, `
INSERT OR IGNORE INTO webhook_deliveries(delivery_id, event_type, action, repo, sender, hook_id, received_at, payload)
VALUES(?, ?, ?, ?, ?, ?, ?, ?)`,
		d.DeliveryID, d.EventType, d.Action, d.Repo, d.Sender, d.HookID,
		received.UTC().Format(time.RFC3339Nano), string(d.Payload))
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// Count returns the total number of stored deliveries.
func (s *Store) Count(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM webhook_deliveries`).Scan(&n)
	return n, err
}

// CountsByEventType returns the per-event-type delivery counts, keyed by
// X-GitHub-Event. Empty map when nothing has been ingested.
func (s *Store) CountsByEventType(ctx context.Context) (map[string]int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT event_type, COUNT(*) FROM webhook_deliveries GROUP BY event_type`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]int)
	for rows.Next() {
		var event string
		var n int
		if err := rows.Scan(&event, &n); err != nil {
			return nil, err
		}
		out[event] = n
	}
	return out, rows.Err()
}

// LastDeliveryAt returns the most recent received_at across all stored
// deliveries, or the zero time when the store is empty.
func (s *Store) LastDeliveryAt(ctx context.Context) (time.Time, error) {
	var raw sql.NullString
	if err := s.db.QueryRowContext(ctx, `SELECT MAX(received_at) FROM webhook_deliveries`).Scan(&raw); err != nil {
		return time.Time{}, err
	}
	if !raw.Valid || strings.TrimSpace(raw.String) == "" {
		return time.Time{}, nil
	}
	t, err := time.Parse(time.RFC3339Nano, raw.String)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse received_at %q: %w", raw.String, err)
	}
	return t, nil
}

// Get returns the stored delivery for id, or ok=false when none exists. Used by
// tests to assert the raw payload and envelope landed intact.
func (s *Store) Get(ctx context.Context, id string) (Delivery, bool, error) {
	var (
		d       Delivery
		payload string
		recv    string
	)
	err := s.db.QueryRowContext(ctx, `
SELECT delivery_id, event_type, action, repo, sender, hook_id, received_at, payload
FROM webhook_deliveries WHERE delivery_id = ?`, id).
		Scan(&d.DeliveryID, &d.EventType, &d.Action, &d.Repo, &d.Sender, &d.HookID, &recv, &payload)
	if err == sql.ErrNoRows {
		return Delivery{}, false, nil
	}
	if err != nil {
		return Delivery{}, false, err
	}
	d.Payload = []byte(payload)
	if strings.TrimSpace(recv) != "" {
		if t, perr := time.Parse(time.RFC3339Nano, recv); perr == nil {
			d.ReceivedAt = t
		}
	}
	return d, true, nil
}
