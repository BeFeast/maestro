package mirrorstore

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// TestMigrateAddsPRColumns proves a mirror created before #826 — whose
// mirror_pull_requests table has neither head_ref nor body — is upgraded in
// place by Init, so ListOpenPRs can serve the head branch and body without a
// manual migration step. CREATE TABLE IF NOT EXISTS alone never alters an
// existing table, so this exercises the ALTER path in migrate().
func TestMigrateAddsPRColumns(t *testing.T) {
	path := filepath.Join(t.TempDir(), "maestro.db")

	// Seed a pre-#826 schema: the old column set, no head_ref / body.
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	if _, err := raw.Exec(`CREATE TABLE mirror_pull_requests (
		repo TEXT NOT NULL, number INTEGER NOT NULL, title TEXT, state TEXT,
		draft INTEGER, merged INTEGER, head_sha TEXT, base_ref TEXT,
		last_seen_at TEXT, source TEXT, PRIMARY KEY (repo, number))`); err != nil {
		t.Fatalf("create old table: %v", err)
	}
	raw.Close()

	// Open via mirrorstore: Init runs the schema then migrate(), adding the
	// missing columns to the existing table.
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	ctx := context.Background()
	seen := mustTime(t, "2026-07-06T12:00:00Z")
	if _, err := s.UpsertPullRequest(ctx, PullRequest{
		Repo: "o/r", Number: 1, Title: "feat", State: "open",
		HeadSHA: "sha", HeadRef: "feat/x", BaseRef: "main", Body: "desc",
		LastSeenAt: seen, Source: SourceWebhook,
	}); err != nil {
		t.Fatalf("upsert after migrate: %v", err)
	}
	pr, ok, err := s.GetPullRequest(ctx, "o/r", 1)
	if err != nil || !ok {
		t.Fatalf("GetPullRequest ok=%v err=%v", ok, err)
	}
	if pr.HeadRef != "feat/x" || pr.Body != "desc" {
		t.Fatalf("migrated columns not persisted: head_ref=%q body=%q", pr.HeadRef, pr.Body)
	}

	// Re-opening an already-migrated DB is a no-op (idempotent).
	s2, err := Open(path)
	if err != nil {
		t.Fatalf("re-Open after migrate: %v", err)
	}
	s2.Close()
}

// TestProjectPullRequestCapturesHeadRefAndBody proves the webhook projection now
// records the head branch and PR body — the fields ListOpenPRs consumers rely on.
func TestProjectPullRequestCapturesHeadRefAndBody(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	payload := []byte(`{
		"action": "opened",
		"number": 42,
		"pull_request": {
			"number": 42, "title": "feat", "state": "open",
			"draft": false, "merged": false,
			"body": "the description",
			"updated_at": "2026-07-02T09:15:00Z",
			"head": {"sha": "abc123", "ref": "feat/head-branch"},
			"base": {"ref": "main"}
		},
		"repository": {"full_name": "o/r"}
	}`)
	if err := s.ProjectWebhook(ctx, "pull_request", payload, time.Time{}); err != nil {
		t.Fatalf("ProjectWebhook: %v", err)
	}
	pr, ok, err := s.GetPullRequest(ctx, "o/r", 42)
	if err != nil || !ok {
		t.Fatalf("GetPullRequest ok=%v err=%v", ok, err)
	}
	if pr.HeadRef != "feat/head-branch" {
		t.Fatalf("head_ref = %q, want feat/head-branch", pr.HeadRef)
	}
	if pr.Body != "the description" {
		t.Fatalf("body = %q, want the description", pr.Body)
	}
}

// TestListOpenPullRequestsFiltersState confirms the list query returns only
// open PRs, in number order — a merged/closed PR is excluded.
func TestListOpenPullRequestsFiltersState(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	seen := mustTime(t, "2026-07-06T12:00:00Z")
	for _, pr := range []PullRequest{
		{Repo: "o/r", Number: 3, State: "open", HeadRef: "c", LastSeenAt: seen, Source: SourceWebhook},
		{Repo: "o/r", Number: 1, State: "open", HeadRef: "a", LastSeenAt: seen, Source: SourceWebhook},
		{Repo: "o/r", Number: 2, State: "closed", Merged: true, HeadRef: "b", LastSeenAt: seen, Source: SourceWebhook},
	} {
		if _, err := s.UpsertPullRequest(ctx, pr); err != nil {
			t.Fatalf("upsert %d: %v", pr.Number, err)
		}
	}
	got, err := s.ListOpenPullRequests(ctx, "o/r")
	if err != nil {
		t.Fatalf("ListOpenPullRequests: %v", err)
	}
	if len(got) != 2 || got[0].Number != 1 || got[1].Number != 3 {
		t.Fatalf("open PRs = %+v; want #1,#3 in order", got)
	}
}
