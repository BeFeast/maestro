package mirrorstore

import (
	"context"
	"database/sql"
	"time"
)

// This file holds the LIST reads the mirror-first source (source.go) serves for
// the supervisor/orchestrator poll loops — the reads that dominate the fleet's
// per-cycle REST consumption. Point reads live in store.go (GetIssue,
// GetPullRequest, …); these return whole open-issue / open-PR sets so a warm
// mirror answers "what is open right now?" with zero GitHub traffic (#826).
//
// A list is fundamentally different from a point read: a point miss is a clean
// "hydrate this one entity" signal, but a list has no per-row miss — an empty
// or lagging result could mean "nothing is open" or "the mirror never saw the
// webhooks". The source therefore gates list serving on warmth (NewestIssueAt /
// NewestPullRequestAt against a freshness horizon) and falls back to the API
// when the mirror is cold, so an unpopulated mirror degrades to today's
// API-direct behavior rather than silently under-reporting.

// ListOpenIssues returns every mirrored issue for repo whose state is "open",
// each with its mirrored label set attached, ordered by issue number. When
// labels is non-empty the result is filtered to issues carrying at least one of
// them (OR semantics — the same filter github.Client.ListOpenIssues applies).
//
// The label set is read from mirror_labels so a caller gets the same
// {Number,Title,Body,State,Labels} shape the REST list returns; body and title
// are mirrored on the issue row, so the reconstruction is loss-free.
func (s *Store) ListOpenIssues(ctx context.Context, repo string, labels []string) ([]Issue, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT repo, number, title, state, body, last_seen_at, source
FROM mirror_issues
WHERE repo = ? AND state = 'open'
ORDER BY number`, repo)
	if err != nil {
		return nil, err
	}
	issues, err := scanIssues(rows)
	if err != nil {
		return nil, err
	}
	want := labelFilter(labels)
	out := make([]Issue, 0, len(issues))
	for _, iss := range issues {
		names, err := s.Labels(ctx, repo, SubjectIssue, iss.Number)
		if err != nil {
			return nil, err
		}
		if want != nil && !anyLabelMatches(names, want) {
			continue
		}
		iss.Labels = names
		out = append(out, iss)
	}
	return out, nil
}

// ListOpenPullRequests returns every mirrored pull request for repo whose state
// is "open", ordered by number. head_ref and body are mirrored on the row so the
// source can reconstruct the fields ListOpenPRs consumers rely on (branch
// matching, draft-marker detection) without an API call.
func (s *Store) ListOpenPullRequests(ctx context.Context, repo string) ([]PullRequest, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT repo, number, title, state, draft, merged, head_sha, head_ref, base_ref, body, last_seen_at, source
FROM mirror_pull_requests
WHERE repo = ? AND state = 'open'
ORDER BY number`, repo)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PullRequest
	for rows.Next() {
		var (
			r             PullRequest
			draft, merged int
			seen          string
		)
		if err := rows.Scan(&r.Repo, &r.Number, &r.Title, &r.State, &draft, &merged,
			&r.HeadSHA, &r.HeadRef, &r.BaseRef, &r.Body, &seen, &r.Source); err != nil {
			return nil, err
		}
		r.Draft = draft != 0
		r.Merged = merged != 0
		r.LastSeenAt, _ = parseTS(seen)
		out = append(out, r)
	}
	return out, rows.Err()
}

func scanIssues(rows *sql.Rows) ([]Issue, error) {
	defer rows.Close()
	var out []Issue
	for rows.Next() {
		var (
			r    Issue
			seen string
		)
		if err := rows.Scan(&r.Repo, &r.Number, &r.Title, &r.State, &r.Body, &seen, &r.Source); err != nil {
			return nil, err
		}
		r.LastSeenAt, _ = parseTS(seen)
		out = append(out, r)
	}
	return out, rows.Err()
}

// ---- Warmth ----------------------------------------------------------------
//
// A list can only be trusted when the mirror is warm for that entity: the
// newest row is within the freshness horizon, i.e. webhooks are flowing. The
// source consults these before serving a list; a cold mirror (no rows, or the
// newest row is stale) means "we cannot vouch for completeness" → API.

// NewestIssueAt returns the most recent last_seen_at across mirrored issues for
// repo and ok=false when the mirror holds no issue for repo.
func (s *Store) NewestIssueAt(ctx context.Context, repo string) (time.Time, bool, error) {
	return s.newestSeen(ctx, "mirror_issues", repo)
}

// NewestPullRequestAt returns the most recent last_seen_at across mirrored pull
// requests for repo and ok=false when the mirror holds no PR for repo.
func (s *Store) NewestPullRequestAt(ctx context.Context, repo string) (time.Time, bool, error) {
	return s.newestSeen(ctx, "mirror_pull_requests", repo)
}

func (s *Store) newestSeen(ctx context.Context, table, repo string) (time.Time, bool, error) {
	var newest sql.NullString
	// MAX over a fixed-width, byte-order == time-order timestamp column yields the
	// chronologically-latest row (see tsLayout).
	err := s.db.QueryRowContext(ctx,
		"SELECT MAX(last_seen_at) FROM "+table+" WHERE repo = ?", repo).Scan(&newest)
	if err != nil {
		return time.Time{}, false, err
	}
	if !newest.Valid || newest.String == "" {
		return time.Time{}, false, nil
	}
	t, err := parseTS(newest.String)
	if err != nil {
		return time.Time{}, false, err
	}
	return t, true, nil
}

// ---- label helpers ---------------------------------------------------------

// labelFilter normalises the requested labels into a lookup set, returning nil
// when no filter was requested (every open issue matches).
func labelFilter(labels []string) map[string]struct{} {
	if len(labels) == 0 {
		return nil
	}
	want := make(map[string]struct{}, len(labels))
	for _, l := range labels {
		if l != "" {
			want[l] = struct{}{}
		}
	}
	if len(want) == 0 {
		return nil
	}
	return want
}

func anyLabelMatches(have []string, want map[string]struct{}) bool {
	for _, name := range have {
		if _, ok := want[name]; ok {
			return true
		}
	}
	return false
}
