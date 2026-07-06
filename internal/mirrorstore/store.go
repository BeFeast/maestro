// Package mirrorstore is the GitHub read-model mirror in the SAME shared
// maestro.db that internal/webhookstore, internal/approvalstore and
// internal/statestore already use (epic #811, phase C — issue #825).
//
// Phase B (#824) lands raw webhook deliveries verbatim. This phase projects
// those deliveries into normalised, queryable tables — issues, pull requests,
// labels, comments, reviews, check/CI status and Projects v2 items — so the
// supervisor/orchestrator can eventually read GitHub state locally instead of
// polling the API on every cycle. It does NOT switch any read path over yet;
// that is phase D. This phase only builds and populates the model.
//
// Three properties make the mirror trustworthy:
//
//   - Ordering-safety. Every row carries last_seen_at, the source resource's own
//     timestamp (issue.updated_at, review.submitted_at, check_run.completed_at,
//     …). An upsert applies only when the incoming last_seen_at is >= the stored
//     one, so an out-of-order or duplicate redelivery never regresses a row to an
//     older state. GitHub retries and operator replays are therefore idempotent.
//
//   - Explicit staleness. Rows do not expire, but a reader can classify any row
//     as fresh / stale / missing against a configurable horizon (Classify), and
//     diagnostics expose per-entity stale counts (StaleCounts) so an operator can
//     see how much of the mirror is lagging.
//
//   - Hydration. On a mirror miss a reader can fetch the entity via the existing
//     internal/github client, record it (source = "api"), and return — so the
//     mirror converges to full coverage without a bulk backfill (see hydrate.go).
//
// Each package owns disjoint tables in the one maestro.db file; the schema here
// only ever CREATE ... IF NOT EXISTS, so Init is idempotent and an existing
// fleet upgrades with no manual migration step (#825 AC 5).
package mirrorstore

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

// Source records how a mirror row was last written: from an ingested webhook
// delivery, or from an on-demand API hydration on a mirror miss. Readers can use
// it to tell "pushed to us" from "we pulled it".
const (
	SourceWebhook = "webhook"
	SourceAPI     = "api"
)

// Subject kinds for rows that hang off an issue or a pull request (labels,
// comments). GitHub models a PR as an issue for comments, so a comment can carry
// either subject type.
const (
	SubjectIssue       = "issue"
	SubjectPullRequest = "pull_request"
)

// Check kinds distinguish a Checks-API check_run from a legacy commit-status
// context; both land in mirror_checks keyed by (repo, head_sha, name).
const (
	CheckKindRun    = "check_run"
	CheckKindStatus = "status"
)

// tsLayout is a FIXED-WIDTH, UTC, nanosecond-precision timestamp layout, the
// same shape internal/webhookstore uses. last_seen_at is stored as TEXT and both
// the ordering-safety guard (excluded.last_seen_at >= mirror.last_seen_at) and
// the staleness query (last_seen_at < cutoff) compare it lexically, so its byte
// order must equal its chronological order. Padding to a constant nine
// fractional digits and always emitting "Z" keeps the field constant-width, so
// byte-order == time-order regardless of a source timestamp's own precision.
const tsLayout = "2006-01-02T15:04:05.000000000Z07:00"

func formatTS(t time.Time) string { return t.UTC().Format(tsLayout) }

func parseTS(s string) (time.Time, error) {
	return time.Parse(time.RFC3339Nano, strings.TrimSpace(s))
}

// Schema creates the mirror tables. IF NOT EXISTS so Init is idempotent and safe
// to run against the same maestro.db the other stores initialise. Timestamps are
// TEXT in tsLayout; booleans are INTEGER 0/1.
const Schema = `
CREATE TABLE IF NOT EXISTS mirror_issues (
	repo         TEXT    NOT NULL,
	number       INTEGER NOT NULL,
	title        TEXT    NOT NULL DEFAULT '',
	state        TEXT    NOT NULL DEFAULT '',
	body         TEXT    NOT NULL DEFAULT '',
	last_seen_at TEXT    NOT NULL DEFAULT '',
	source       TEXT    NOT NULL DEFAULT '',
	PRIMARY KEY (repo, number)
);

CREATE TABLE IF NOT EXISTS mirror_pull_requests (
	repo         TEXT    NOT NULL,
	number       INTEGER NOT NULL,
	title        TEXT    NOT NULL DEFAULT '',
	state        TEXT    NOT NULL DEFAULT '',
	draft        INTEGER NOT NULL DEFAULT 0,
	merged       INTEGER NOT NULL DEFAULT 0,
	head_sha     TEXT    NOT NULL DEFAULT '',
	base_ref     TEXT    NOT NULL DEFAULT '',
	last_seen_at TEXT    NOT NULL DEFAULT '',
	source       TEXT    NOT NULL DEFAULT '',
	PRIMARY KEY (repo, number)
);

CREATE TABLE IF NOT EXISTS mirror_labels (
	repo           TEXT    NOT NULL,
	subject_type   TEXT    NOT NULL,
	subject_number INTEGER NOT NULL,
	name           TEXT    NOT NULL,
	last_seen_at   TEXT    NOT NULL DEFAULT '',
	source         TEXT    NOT NULL DEFAULT '',
	PRIMARY KEY (repo, subject_type, subject_number, name)
);

CREATE TABLE IF NOT EXISTS mirror_comments (
	repo           TEXT    NOT NULL,
	comment_id     INTEGER NOT NULL,
	subject_type   TEXT    NOT NULL DEFAULT '',
	subject_number INTEGER NOT NULL DEFAULT 0,
	author         TEXT    NOT NULL DEFAULT '',
	body           TEXT    NOT NULL DEFAULT '',
	last_seen_at   TEXT    NOT NULL DEFAULT '',
	source         TEXT    NOT NULL DEFAULT '',
	PRIMARY KEY (repo, comment_id)
);

CREATE TABLE IF NOT EXISTS mirror_reviews (
	repo         TEXT    NOT NULL,
	review_id    INTEGER NOT NULL,
	pr_number    INTEGER NOT NULL DEFAULT 0,
	author       TEXT    NOT NULL DEFAULT '',
	state        TEXT    NOT NULL DEFAULT '',
	body         TEXT    NOT NULL DEFAULT '',
	last_seen_at TEXT    NOT NULL DEFAULT '',
	source       TEXT    NOT NULL DEFAULT '',
	PRIMARY KEY (repo, review_id)
);

CREATE TABLE IF NOT EXISTS mirror_checks (
	repo         TEXT    NOT NULL,
	head_sha     TEXT    NOT NULL,
	name         TEXT    NOT NULL,
	kind         TEXT    NOT NULL DEFAULT '',
	status       TEXT    NOT NULL DEFAULT '',
	conclusion   TEXT    NOT NULL DEFAULT '',
	last_seen_at TEXT    NOT NULL DEFAULT '',
	source       TEXT    NOT NULL DEFAULT '',
	PRIMARY KEY (repo, head_sha, name)
);

CREATE TABLE IF NOT EXISTS mirror_project_items (
	repo           TEXT    NOT NULL,
	item_id        TEXT    NOT NULL,
	content_type   TEXT    NOT NULL DEFAULT '',
	content_number INTEGER NOT NULL DEFAULT 0,
	status         TEXT    NOT NULL DEFAULT '',
	last_seen_at   TEXT    NOT NULL DEFAULT '',
	source         TEXT    NOT NULL DEFAULT '',
	PRIMARY KEY (repo, item_id)
);

CREATE INDEX IF NOT EXISTS idx_mirror_checks_sha ON mirror_checks(repo, head_sha);
CREATE INDEX IF NOT EXISTS idx_mirror_labels_subject ON mirror_labels(repo, subject_type, subject_number);
CREATE INDEX IF NOT EXISTS idx_mirror_comments_subject ON mirror_comments(repo, subject_type, subject_number);
CREATE INDEX IF NOT EXISTS idx_mirror_reviews_pr ON mirror_reviews(repo, pr_number);
`

// Store is the SQLite-backed GitHub mirror read model.
type Store struct {
	db *sql.DB
}

// DefaultDBPath is the shared database (~/.maestro/maestro.db) — the same file
// approvalstore, statestore and webhookstore use. The mirror lives in its own
// tables within that one file.
func DefaultDBPath() string {
	return filepath.Join(os.Getenv("HOME"), ".maestro", "maestro.db")
}

// Open opens (creating the parent dir if needed) the database and ensures the
// schema. The pragmas mirror internal/webhookstore: busy_timeout waits for a
// lock instead of erroring, WAL lets a reader and writer proceed concurrently,
// and MaxOpenConns(1) avoids the SQLITE_BUSY modernc/sqlite can report between
// two pooled connections of the same process.
func Open(path string) (*Store, error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return nil, fmt.Errorf("create mirror db dir %s: %w", dir, err)
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

// Init creates the schema. Idempotent — safe to call on every daemon start, and
// safe to run against a maestro.db that already has the other stores' tables.
func (s *Store) Init(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, Schema)
	return err
}

// ---- Row types -------------------------------------------------------------

// Issue is one mirrored GitHub issue.
type Issue struct {
	Repo       string
	Number     int
	Title      string
	State      string
	Body       string
	LastSeenAt time.Time
	Source     string
}

// PullRequest is one mirrored GitHub pull request.
type PullRequest struct {
	Repo       string
	Number     int
	Title      string
	State      string
	Draft      bool
	Merged     bool
	HeadSHA    string
	BaseRef    string
	LastSeenAt time.Time
	Source     string
}

// Comment is one mirrored issue or review comment.
type Comment struct {
	Repo          string
	CommentID     int64
	SubjectType   string
	SubjectNumber int
	Author        string
	Body          string
	LastSeenAt    time.Time
	Source        string
}

// Review is one mirrored pull request review.
type Review struct {
	Repo       string
	ReviewID   int64
	PRNumber   int
	Author     string
	State      string
	Body       string
	LastSeenAt time.Time
	Source     string
}

// Check is one mirrored check_run or commit-status row for a head SHA.
type Check struct {
	Repo       string
	HeadSHA    string
	Name       string
	Kind       string
	Status     string
	Conclusion string
	LastSeenAt time.Time
	Source     string
}

// ProjectItem is one mirrored Projects v2 item.
type ProjectItem struct {
	Repo          string
	ItemID        string
	ContentType   string
	ContentNumber int
	Status        string
	LastSeenAt    time.Time
	Source        string
}

// ---- Ordering-safe upserts -------------------------------------------------
//
// Every upsert applies only when the incoming last_seen_at is >= the stored one
// (the WHERE on the DO UPDATE). RowsAffected then tells the caller whether the
// write landed: 1 for a fresh insert or an applied update, 0 for a stale event
// whose guard rejected it. Callers that must reconcile a dependent set (labels
// for an issue) gate that work on the returned applied flag so a stale parent
// event does not clobber a fresher child set.

// execer is the subset of *sql.DB / *sql.Tx the guarded upserts need, so the same
// upsert body can run either standalone (against the pool) or inside a
// transaction that also reconciles a dependent set (labels).
type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// UpsertIssue writes or refreshes an issue row, guarded by last_seen_at. Returns
// applied=false when a strictly-older event was rejected.
func (s *Store) UpsertIssue(ctx context.Context, r Issue) (applied bool, err error) {
	return upsertIssue(ctx, s.db, r)
}

func upsertIssue(ctx context.Context, ex execer, r Issue) (bool, error) {
	res, err := ex.ExecContext(ctx, `
INSERT INTO mirror_issues (repo, number, title, state, body, last_seen_at, source)
VALUES (?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(repo, number) DO UPDATE SET
	title = excluded.title,
	state = excluded.state,
	body = excluded.body,
	last_seen_at = excluded.last_seen_at,
	source = excluded.source
WHERE excluded.last_seen_at >= mirror_issues.last_seen_at`,
		r.Repo, r.Number, r.Title, r.State, r.Body, formatTS(r.LastSeenAt), r.Source)
	return rowsApplied(res, err)
}

// UpsertPullRequest writes or refreshes a PR row, guarded by last_seen_at.
func (s *Store) UpsertPullRequest(ctx context.Context, r PullRequest) (applied bool, err error) {
	return upsertPullRequest(ctx, s.db, r)
}

func upsertPullRequest(ctx context.Context, ex execer, r PullRequest) (bool, error) {
	res, err := ex.ExecContext(ctx, `
INSERT INTO mirror_pull_requests (repo, number, title, state, draft, merged, head_sha, base_ref, last_seen_at, source)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(repo, number) DO UPDATE SET
	title = excluded.title,
	state = excluded.state,
	draft = excluded.draft,
	merged = excluded.merged,
	head_sha = excluded.head_sha,
	base_ref = excluded.base_ref,
	last_seen_at = excluded.last_seen_at,
	source = excluded.source
WHERE excluded.last_seen_at >= mirror_pull_requests.last_seen_at`,
		r.Repo, r.Number, r.Title, r.State, boolToInt(r.Draft), boolToInt(r.Merged),
		r.HeadSHA, r.BaseRef, formatTS(r.LastSeenAt), r.Source)
	return rowsApplied(res, err)
}

// UpsertIssueWithLabels writes the issue row and, only when that write applied
// (i.e. the event was not stale), reconciles the full label set — both inside ONE
// transaction. The shared handle is single-connection, so the transaction holds it
// for the whole logical update; a concurrent newer delivery cannot interleave its
// own upsert+labels between this upsert and its label replace and then have this
// (older) goroutine delete the fresher labels. The label set is thus reconciled
// under the SAME timestamp guard as the parent row, closing the race a separate
// UpsertIssue-then-ReplaceLabels sequence left open (#825 AC 2).
func (s *Store) UpsertIssueWithLabels(ctx context.Context, r Issue, labels []string) (applied bool, err error) {
	return s.upsertWithLabels(ctx, SubjectIssue, r.Repo, r.Number, r.LastSeenAt, r.Source, labels,
		func(ex execer) (bool, error) { return upsertIssue(ctx, ex, r) })
}

// UpsertPullRequestWithLabels is UpsertIssueWithLabels for a pull request: the PR
// row and its labels are reconciled atomically under one timestamp guard.
func (s *Store) UpsertPullRequestWithLabels(ctx context.Context, r PullRequest, labels []string) (applied bool, err error) {
	return s.upsertWithLabels(ctx, SubjectPullRequest, r.Repo, r.Number, r.LastSeenAt, r.Source, labels,
		func(ex execer) (bool, error) { return upsertPullRequest(ctx, ex, r) })
}

// upsertWithLabels runs upsert inside a transaction and, when it applied, replaces
// the subject's labels in the same transaction so both share one timestamp guard.
func (s *Store) upsertWithLabels(ctx context.Context, subjectType, repo string, subjectNumber int, ts time.Time, source string, labels []string, upsert func(execer) (bool, error)) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	applied, err := upsert(tx)
	if err != nil {
		return false, err
	}
	if applied {
		if err := replaceLabelsTx(ctx, tx, repo, subjectType, subjectNumber, labels, ts, source); err != nil {
			return false, err
		}
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return applied, nil
}

// UpsertComment writes or refreshes a comment row, guarded by last_seen_at.
func (s *Store) UpsertComment(ctx context.Context, r Comment) (applied bool, err error) {
	res, err := s.db.ExecContext(ctx, `
INSERT INTO mirror_comments (repo, comment_id, subject_type, subject_number, author, body, last_seen_at, source)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(repo, comment_id) DO UPDATE SET
	subject_type = excluded.subject_type,
	subject_number = excluded.subject_number,
	author = excluded.author,
	body = excluded.body,
	last_seen_at = excluded.last_seen_at,
	source = excluded.source
WHERE excluded.last_seen_at >= mirror_comments.last_seen_at`,
		r.Repo, r.CommentID, r.SubjectType, r.SubjectNumber, r.Author, r.Body,
		formatTS(r.LastSeenAt), r.Source)
	return rowsApplied(res, err)
}

// DeleteComment removes the mirror comment row for (repo, commentID). It is called
// when a deletion webhook arrives (issue_comment or pull_request_review_comment
// with action "deleted") so the read model stops exposing — and stops staleness
// diagnostics from tracking — a comment that no longer exists on GitHub. A comment
// id is never reused, so a straight delete keeps the mirror matching the direct
// API snapshot. Idempotent: deleting an absent row is a no-op.
func (s *Store) DeleteComment(ctx context.Context, repo string, commentID int64) error {
	_, err := s.db.ExecContext(ctx, `
DELETE FROM mirror_comments WHERE repo = ? AND comment_id = ?`, repo, commentID)
	return err
}

// UpsertReview writes or refreshes a review row, guarded by last_seen_at.
func (s *Store) UpsertReview(ctx context.Context, r Review) (applied bool, err error) {
	res, err := s.db.ExecContext(ctx, `
INSERT INTO mirror_reviews (repo, review_id, pr_number, author, state, body, last_seen_at, source)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(repo, review_id) DO UPDATE SET
	pr_number = excluded.pr_number,
	author = excluded.author,
	state = excluded.state,
	body = excluded.body,
	last_seen_at = excluded.last_seen_at,
	source = excluded.source
WHERE excluded.last_seen_at >= mirror_reviews.last_seen_at`,
		r.Repo, r.ReviewID, r.PRNumber, r.Author, r.State, r.Body,
		formatTS(r.LastSeenAt), r.Source)
	return rowsApplied(res, err)
}

// UpsertCheck writes or refreshes a check/status row, guarded by last_seen_at.
func (s *Store) UpsertCheck(ctx context.Context, r Check) (applied bool, err error) {
	res, err := s.db.ExecContext(ctx, `
INSERT INTO mirror_checks (repo, head_sha, name, kind, status, conclusion, last_seen_at, source)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(repo, head_sha, name) DO UPDATE SET
	kind = excluded.kind,
	status = excluded.status,
	conclusion = excluded.conclusion,
	last_seen_at = excluded.last_seen_at,
	source = excluded.source
WHERE excluded.last_seen_at >= mirror_checks.last_seen_at`,
		r.Repo, r.HeadSHA, r.Name, r.Kind, r.Status, r.Conclusion,
		formatTS(r.LastSeenAt), r.Source)
	return rowsApplied(res, err)
}

// UpsertProjectItem writes or refreshes a Projects v2 item row, guarded by
// last_seen_at.
func (s *Store) UpsertProjectItem(ctx context.Context, r ProjectItem) (applied bool, err error) {
	res, err := s.db.ExecContext(ctx, `
INSERT INTO mirror_project_items (repo, item_id, content_type, content_number, status, last_seen_at, source)
VALUES (?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(repo, item_id) DO UPDATE SET
	content_type = excluded.content_type,
	content_number = excluded.content_number,
	status = excluded.status,
	last_seen_at = excluded.last_seen_at,
	source = excluded.source
WHERE excluded.last_seen_at >= mirror_project_items.last_seen_at`,
		r.Repo, r.ItemID, r.ContentType, r.ContentNumber, r.Status,
		formatTS(r.LastSeenAt), r.Source)
	return rowsApplied(res, err)
}

// ReplaceLabels reconciles the FULL label set for a subject atomically: it clears
// the subject's existing labels and inserts the given names, all stamped at ts.
// GitHub webhook payloads carry the complete current label array on the issue /
// PR object, so a whole-set replace is both simpler and more correct than
// per-label add/remove — and ordering-safety is inherited from the caller, which
// only replaces labels when the parent issue/PR upsert applied (i.e. this event
// was not stale). An empty names slice clears the subject's labels.
func (s *Store) ReplaceLabels(ctx context.Context, repo, subjectType string, subjectNumber int, names []string, ts time.Time, source string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := replaceLabelsTx(ctx, tx, repo, subjectType, subjectNumber, names, ts, source); err != nil {
		return err
	}
	return tx.Commit()
}

// replaceLabelsTx is the transaction body of ReplaceLabels, factored out so the
// same delete-then-insert can run inside a caller's transaction (upsertWithLabels)
// and share that transaction's timestamp guard rather than racing it.
func replaceLabelsTx(ctx context.Context, tx *sql.Tx, repo, subjectType string, subjectNumber int, names []string, ts time.Time, source string) error {
	if _, err := tx.ExecContext(ctx, `
DELETE FROM mirror_labels WHERE repo = ? AND subject_type = ? AND subject_number = ?`,
		repo, subjectType, subjectNumber); err != nil {
		return err
	}
	stamp := formatTS(ts)
	for _, name := range names {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if _, err := tx.ExecContext(ctx, `
INSERT OR REPLACE INTO mirror_labels (repo, subject_type, subject_number, name, last_seen_at, source)
VALUES (?, ?, ?, ?, ?, ?)`, repo, subjectType, subjectNumber, name, stamp, source); err != nil {
			return err
		}
	}
	return nil
}

// ---- Reads -----------------------------------------------------------------

// GetIssue returns the mirror issue row for (repo, number), or ok=false on a
// miss. A miss is the signal for a reader to hydrate (see Hydrator).
func (s *Store) GetIssue(ctx context.Context, repo string, number int) (Issue, bool, error) {
	var (
		r    Issue
		seen string
	)
	err := s.db.QueryRowContext(ctx, `
SELECT repo, number, title, state, body, last_seen_at, source
FROM mirror_issues WHERE repo = ? AND number = ?`, repo, number).
		Scan(&r.Repo, &r.Number, &r.Title, &r.State, &r.Body, &seen, &r.Source)
	if err == sql.ErrNoRows {
		return Issue{}, false, nil
	}
	if err != nil {
		return Issue{}, false, err
	}
	r.LastSeenAt, _ = parseTS(seen)
	return r, true, nil
}

// GetPullRequest returns the mirror PR row for (repo, number), or ok=false on a
// miss.
func (s *Store) GetPullRequest(ctx context.Context, repo string, number int) (PullRequest, bool, error) {
	var (
		r             PullRequest
		draft, merged int
		seen          string
	)
	err := s.db.QueryRowContext(ctx, `
SELECT repo, number, title, state, draft, merged, head_sha, base_ref, last_seen_at, source
FROM mirror_pull_requests WHERE repo = ? AND number = ?`, repo, number).
		Scan(&r.Repo, &r.Number, &r.Title, &r.State, &draft, &merged, &r.HeadSHA, &r.BaseRef, &seen, &r.Source)
	if err == sql.ErrNoRows {
		return PullRequest{}, false, nil
	}
	if err != nil {
		return PullRequest{}, false, err
	}
	r.Draft = draft != 0
	r.Merged = merged != 0
	r.LastSeenAt, _ = parseTS(seen)
	return r, true, nil
}

// Labels returns the mirrored label names for a subject, sorted, or an empty
// slice when the subject has none.
func (s *Store) Labels(ctx context.Context, repo, subjectType string, subjectNumber int) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT name FROM mirror_labels
WHERE repo = ? AND subject_type = ? AND subject_number = ?
ORDER BY name`, repo, subjectType, subjectNumber)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		out = append(out, name)
	}
	return out, rows.Err()
}

// GetComment returns the mirror comment row for (repo, commentID), or ok=false.
func (s *Store) GetComment(ctx context.Context, repo string, commentID int64) (Comment, bool, error) {
	var (
		r    Comment
		seen string
	)
	err := s.db.QueryRowContext(ctx, `
SELECT repo, comment_id, subject_type, subject_number, author, body, last_seen_at, source
FROM mirror_comments WHERE repo = ? AND comment_id = ?`, repo, commentID).
		Scan(&r.Repo, &r.CommentID, &r.SubjectType, &r.SubjectNumber, &r.Author, &r.Body, &seen, &r.Source)
	if err == sql.ErrNoRows {
		return Comment{}, false, nil
	}
	if err != nil {
		return Comment{}, false, err
	}
	r.LastSeenAt, _ = parseTS(seen)
	return r, true, nil
}

// GetReview returns the mirror review row for (repo, reviewID), or ok=false.
func (s *Store) GetReview(ctx context.Context, repo string, reviewID int64) (Review, bool, error) {
	var (
		r    Review
		seen string
	)
	err := s.db.QueryRowContext(ctx, `
SELECT repo, review_id, pr_number, author, state, body, last_seen_at, source
FROM mirror_reviews WHERE repo = ? AND review_id = ?`, repo, reviewID).
		Scan(&r.Repo, &r.ReviewID, &r.PRNumber, &r.Author, &r.State, &r.Body, &seen, &r.Source)
	if err == sql.ErrNoRows {
		return Review{}, false, nil
	}
	if err != nil {
		return Review{}, false, err
	}
	r.LastSeenAt, _ = parseTS(seen)
	return r, true, nil
}

// Checks returns the mirrored check/status rows for a head SHA, sorted by name.
func (s *Store) Checks(ctx context.Context, repo, headSHA string) ([]Check, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT repo, head_sha, name, kind, status, conclusion, last_seen_at, source
FROM mirror_checks WHERE repo = ? AND head_sha = ?
ORDER BY name`, repo, headSHA)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Check
	for rows.Next() {
		var (
			r    Check
			seen string
		)
		if err := rows.Scan(&r.Repo, &r.HeadSHA, &r.Name, &r.Kind, &r.Status, &r.Conclusion, &seen, &r.Source); err != nil {
			return nil, err
		}
		r.LastSeenAt, _ = parseTS(seen)
		out = append(out, r)
	}
	return out, rows.Err()
}

// GetProjectItem returns the mirror Projects v2 item row for (repo, itemID), or
// ok=false.
func (s *Store) GetProjectItem(ctx context.Context, repo, itemID string) (ProjectItem, bool, error) {
	var (
		r    ProjectItem
		seen string
	)
	err := s.db.QueryRowContext(ctx, `
SELECT repo, item_id, content_type, content_number, status, last_seen_at, source
FROM mirror_project_items WHERE repo = ? AND item_id = ?`, repo, itemID).
		Scan(&r.Repo, &r.ItemID, &r.ContentType, &r.ContentNumber, &r.Status, &seen, &r.Source)
	if err == sql.ErrNoRows {
		return ProjectItem{}, false, nil
	}
	if err != nil {
		return ProjectItem{}, false, err
	}
	r.LastSeenAt, _ = parseTS(seen)
	return r, true, nil
}

// ---- Staleness -------------------------------------------------------------

// DefaultStaleHorizon is the fallback staleness horizon: a mirror row not
// refreshed by a webhook or hydration within this window is reported stale in
// diagnostics. Chosen conservatively — the mirror is refreshed by pushes, so a
// row older than a day almost certainly means deliveries stopped arriving for
// that repo, which is exactly what an operator wants surfaced. Callers pass their
// own horizon to Classify / StaleCounts; this is only the wiring default.
const DefaultStaleHorizon = 24 * time.Hour

// Freshness classifies a mirror row against a staleness horizon. Missing is the
// zero value so a caller that forgot to check "ok" first still reads a safe
// "we don't have it" rather than a false "fresh".
type Freshness int

const (
	// Missing: the mirror has no row for the entity (the caller's GetX returned
	// ok=false). Hydrate to fill it.
	Missing Freshness = iota
	// Stale: the row exists but its last_seen_at is older than the horizon.
	Stale
	// Fresh: the row exists and is within the horizon.
	Fresh
)

func (f Freshness) String() string {
	switch f {
	case Fresh:
		return "fresh"
	case Stale:
		return "stale"
	default:
		return "missing"
	}
}

// Classify reports whether a row last seen at lastSeen is Fresh or Stale at now
// under horizon. A non-positive horizon means "never stale" (every present row
// is Fresh). Callers pass Missing themselves when the row is absent; Classify
// only ever returns Fresh or Stale.
func Classify(lastSeen, now time.Time, horizon time.Duration) Freshness {
	if horizon <= 0 {
		return Fresh
	}
	if now.Sub(lastSeen) > horizon {
		return Stale
	}
	return Fresh
}

// ---- Diagnostics -----------------------------------------------------------

// Counts is the per-entity row tally exposed in diagnostics.
type Counts struct {
	Issues       int `json:"issues"`
	PullRequests int `json:"pull_requests"`
	Labels       int `json:"labels"`
	Comments     int `json:"comments"`
	Reviews      int `json:"reviews"`
	Checks       int `json:"checks"`
	ProjectItems int `json:"project_items"`
}

// Total is the sum across every entity type.
func (c Counts) Total() int {
	return c.Issues + c.PullRequests + c.Labels + c.Comments + c.Reviews + c.Checks + c.ProjectItems
}

// Counts returns the total mirrored row count per entity type.
func (s *Store) Counts(ctx context.Context) (Counts, error) {
	return s.countRows(ctx, "")
}

// StaleCounts returns the per-entity count of rows whose last_seen_at is older
// than horizon at now — the queryable staleness diagnostic (#825 AC 4). A
// non-positive horizon returns an all-zero Counts (nothing is ever stale).
func (s *Store) StaleCounts(ctx context.Context, now time.Time, horizon time.Duration) (Counts, error) {
	if horizon <= 0 {
		return Counts{}, nil
	}
	cutoff := formatTS(now.Add(-horizon))
	return s.countRows(ctx, cutoff)
}

// countRows tallies each mirror table. When cutoff is non-empty it counts only
// rows with last_seen_at < cutoff (the stale set); otherwise it counts all rows.
func (s *Store) countRows(ctx context.Context, cutoff string) (Counts, error) {
	var c Counts
	specs := []struct {
		table string
		dst   *int
	}{
		{"mirror_issues", &c.Issues},
		{"mirror_pull_requests", &c.PullRequests},
		{"mirror_labels", &c.Labels},
		{"mirror_comments", &c.Comments},
		{"mirror_reviews", &c.Reviews},
		{"mirror_checks", &c.Checks},
		{"mirror_project_items", &c.ProjectItems},
	}
	for _, spec := range specs {
		query := "SELECT COUNT(*) FROM " + spec.table
		var row *sql.Row
		if cutoff != "" {
			query += " WHERE last_seen_at < ?"
			row = s.db.QueryRowContext(ctx, query, cutoff)
		} else {
			row = s.db.QueryRowContext(ctx, query)
		}
		if err := row.Scan(spec.dst); err != nil {
			return Counts{}, fmt.Errorf("count %s: %w", spec.table, err)
		}
	}
	return c, nil
}

// ---- helpers ---------------------------------------------------------------

func rowsApplied(res sql.Result, err error) (bool, error) {
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
