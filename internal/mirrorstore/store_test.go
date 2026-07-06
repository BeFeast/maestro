package mirrorstore

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "maestro.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func mustTime(t *testing.T, s string) time.Time {
	t.Helper()
	tm, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("parse time %q: %v", s, err)
	}
	return tm
}

// TestOpenIdempotent covers AC 5: the schema is added idempotently, so opening
// the same maestro.db twice (an existing fleet upgrading) is a no-op that neither
// errors nor drops data.
func TestOpenIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "maestro.db")
	ctx := context.Background()

	s1, err := Open(path)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	if _, err := s1.UpsertIssue(ctx, Issue{Repo: "o/r", Number: 1, Title: "t", State: "open", LastSeenAt: mustTime(t, "2026-07-01T00:00:00Z"), Source: SourceWebhook}); err != nil {
		t.Fatalf("seed issue: %v", err)
	}
	if err := s1.Close(); err != nil {
		t.Fatalf("close s1: %v", err)
	}

	// Re-open: Init runs again against the populated DB and must not error or wipe.
	s2, err := Open(path)
	if err != nil {
		t.Fatalf("second Open: %v", err)
	}
	defer s2.Close()
	if err := s2.Init(ctx); err != nil {
		t.Fatalf("re-Init: %v", err)
	}
	row, ok, err := s2.GetIssue(ctx, "o/r", 1)
	if err != nil {
		t.Fatalf("GetIssue: %v", err)
	}
	if !ok || row.Title != "t" {
		t.Fatalf("row not preserved across re-open: ok=%v row=%+v", ok, row)
	}
}

// TestUpsertOrderingSafe covers AC 2: a newer event wins, a strictly-older event
// is rejected, and a duplicate does not regress the row.
func TestUpsertOrderingSafe(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	t1 := mustTime(t, "2026-07-01T10:00:00Z")
	t2 := mustTime(t, "2026-07-01T12:00:00Z")

	// First write.
	applied, err := s.UpsertIssue(ctx, Issue{Repo: "o/r", Number: 5, Title: "v1", State: "open", LastSeenAt: t1, Source: SourceWebhook})
	if err != nil || !applied {
		t.Fatalf("first upsert applied=%v err=%v", applied, err)
	}

	// Newer event applies.
	applied, err = s.UpsertIssue(ctx, Issue{Repo: "o/r", Number: 5, Title: "v2", State: "closed", LastSeenAt: t2, Source: SourceWebhook})
	if err != nil || !applied {
		t.Fatalf("newer upsert applied=%v err=%v", applied, err)
	}

	// Strictly-older event is rejected and does not regress the row.
	applied, err = s.UpsertIssue(ctx, Issue{Repo: "o/r", Number: 5, Title: "STALE", State: "open", LastSeenAt: t1, Source: SourceWebhook})
	if err != nil {
		t.Fatalf("older upsert err=%v", err)
	}
	if applied {
		t.Fatalf("older upsert must not apply")
	}
	row, _, err := s.GetIssue(ctx, "o/r", 5)
	if err != nil {
		t.Fatalf("GetIssue: %v", err)
	}
	if row.Title != "v2" || row.State != "closed" {
		t.Fatalf("row regressed by stale event: %+v", row)
	}

	// A duplicate of the current newest (equal timestamp) is idempotent: it re-
	// applies the same values (applied=true) but the row content is unchanged.
	applied, err = s.UpsertIssue(ctx, Issue{Repo: "o/r", Number: 5, Title: "v2", State: "closed", LastSeenAt: t2, Source: SourceWebhook})
	if err != nil {
		t.Fatalf("duplicate upsert err=%v", err)
	}
	if !applied {
		t.Fatalf("equal-timestamp upsert should re-apply (>=)")
	}
	row, _, _ = s.GetIssue(ctx, "o/r", 5)
	if row.Title != "v2" {
		t.Fatalf("duplicate changed row: %+v", row)
	}
}

func TestReplaceLabels(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	ts := mustTime(t, "2026-07-01T10:00:00Z")

	if err := s.ReplaceLabels(ctx, "o/r", SubjectIssue, 7, []string{"b", "a"}, ts, SourceWebhook); err != nil {
		t.Fatalf("ReplaceLabels: %v", err)
	}
	got, err := s.Labels(ctx, "o/r", SubjectIssue, 7)
	if err != nil {
		t.Fatalf("Labels: %v", err)
	}
	if len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("labels = %v, want sorted [a b]", got)
	}

	// A whole-set replace removes labels no longer present.
	if err := s.ReplaceLabels(ctx, "o/r", SubjectIssue, 7, []string{"a"}, ts, SourceWebhook); err != nil {
		t.Fatalf("ReplaceLabels 2: %v", err)
	}
	got, _ = s.Labels(ctx, "o/r", SubjectIssue, 7)
	if len(got) != 1 || got[0] != "a" {
		t.Fatalf("labels = %v, want [a]", got)
	}

	// An empty set clears all labels.
	if err := s.ReplaceLabels(ctx, "o/r", SubjectIssue, 7, nil, ts, SourceWebhook); err != nil {
		t.Fatalf("ReplaceLabels 3: %v", err)
	}
	got, _ = s.Labels(ctx, "o/r", SubjectIssue, 7)
	if len(got) != 0 {
		t.Fatalf("labels = %v, want empty", got)
	}
}

// TestStaleCountsAndClassify covers AC 4: staleness is queryable and readers can
// distinguish fresh/stale/missing.
func TestStaleCountsAndClassify(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	now := mustTime(t, "2026-07-03T00:00:00Z")
	horizon := 24 * time.Hour

	fresh := now.Add(-1 * time.Hour)  // within horizon
	stale := now.Add(-48 * time.Hour) // older than horizon
	if _, err := s.UpsertIssue(ctx, Issue{Repo: "o/r", Number: 1, LastSeenAt: fresh, Source: SourceWebhook}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpsertIssue(ctx, Issue{Repo: "o/r", Number: 2, LastSeenAt: stale, Source: SourceWebhook}); err != nil {
		t.Fatal(err)
	}

	counts, err := s.Counts(ctx)
	if err != nil {
		t.Fatalf("Counts: %v", err)
	}
	if counts.Issues != 2 {
		t.Fatalf("Counts.Issues = %d, want 2", counts.Issues)
	}

	staleCounts, err := s.StaleCounts(ctx, now, horizon)
	if err != nil {
		t.Fatalf("StaleCounts: %v", err)
	}
	if staleCounts.Issues != 1 {
		t.Fatalf("StaleCounts.Issues = %d, want 1", staleCounts.Issues)
	}

	// Classify: fresh row, stale row, and a missing row (caller supplies Missing).
	if got := Classify(fresh, now, horizon); got != Fresh {
		t.Fatalf("Classify(fresh) = %v, want fresh", got)
	}
	if got := Classify(stale, now, horizon); got != Stale {
		t.Fatalf("Classify(stale) = %v, want stale", got)
	}
	if _, ok, _ := s.GetIssue(ctx, "o/r", 999); ok {
		t.Fatalf("expected miss for absent issue")
	}
	// A non-positive horizon means never stale.
	if got := Classify(stale, now, 0); got != Fresh {
		t.Fatalf("Classify with zero horizon = %v, want fresh", got)
	}
	if sc, _ := s.StaleCounts(ctx, now, 0); sc.Total() != 0 {
		t.Fatalf("StaleCounts with zero horizon = %+v, want all zero", sc)
	}
}

// TestUpsertIssueWithLabelsStaleKeepsLabels covers the P2 fix: reconciling an
// issue's row and its labels atomically under one timestamp guard. A strictly
// older event must not replace (and so must not delete) a fresher label set, even
// though a bare UpsertIssue would report applied=false only after the fact.
func TestUpsertIssueWithLabelsStaleKeepsLabels(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	newer := mustTime(t, "2026-07-02T12:00:00Z")
	older := mustTime(t, "2026-07-02T09:00:00Z")

	// A fresh event set the row and labels.
	applied, err := s.UpsertIssueWithLabels(ctx, Issue{
		Repo: "o/r", Number: 1, Title: "NEW", State: "closed", LastSeenAt: newer, Source: SourceWebhook,
	}, []string{"keep"})
	if err != nil || !applied {
		t.Fatalf("seed applied=%v err=%v", applied, err)
	}

	// A strictly older event arrives: its upsert is rejected and, crucially, it does
	// NOT run the label replace — so "keep" survives.
	applied, err = s.UpsertIssueWithLabels(ctx, Issue{
		Repo: "o/r", Number: 1, Title: "OLD", State: "open", LastSeenAt: older, Source: SourceWebhook,
	}, []string{"stale-label"})
	if err != nil {
		t.Fatalf("stale upsert err=%v", err)
	}
	if applied {
		t.Fatalf("stale event reported applied=true")
	}
	row, _, _ := s.GetIssue(ctx, "o/r", 1)
	if row.Title != "NEW" || row.State != "closed" {
		t.Fatalf("stale event regressed row: %+v", row)
	}
	labels, _ := s.Labels(ctx, "o/r", SubjectIssue, 1)
	if len(labels) != 1 || labels[0] != "keep" {
		t.Fatalf("stale event regressed labels: %v", labels)
	}
}

// TestUpsertIssueWithLabelsAppliedReplaces confirms that when the row DOES move
// forward, its labels are reconciled to the new set in the same transaction.
func TestUpsertIssueWithLabelsAppliedReplaces(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	t0 := mustTime(t, "2026-07-02T09:00:00Z")
	t1 := mustTime(t, "2026-07-02T12:00:00Z")

	if _, err := s.UpsertIssueWithLabels(ctx, Issue{
		Repo: "o/r", Number: 1, Title: "v0", State: "open", LastSeenAt: t0, Source: SourceWebhook,
	}, []string{"a"}); err != nil {
		t.Fatal(err)
	}
	applied, err := s.UpsertIssueWithLabels(ctx, Issue{
		Repo: "o/r", Number: 1, Title: "v1", State: "open", LastSeenAt: t1, Source: SourceWebhook,
	}, []string{"b", "c"})
	if err != nil || !applied {
		t.Fatalf("newer applied=%v err=%v", applied, err)
	}
	labels, _ := s.Labels(ctx, "o/r", SubjectIssue, 1)
	if !equalStrings(labels, []string{"b", "c"}) {
		t.Fatalf("labels after applied upsert = %v, want [b c]", labels)
	}
}

// TestUpsertPullRequestWithLabels confirms the PR variant reconciles row + labels
// atomically the same way.
func TestUpsertPullRequestWithLabels(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	t0 := mustTime(t, "2026-07-02T09:00:00Z")
	t1 := mustTime(t, "2026-07-02T12:00:00Z")

	if _, err := s.UpsertPullRequestWithLabels(ctx, PullRequest{
		Repo: "o/r", Number: 5, Title: "pr", State: "open", LastSeenAt: t1, Source: SourceWebhook,
	}, []string{"ready"}); err != nil {
		t.Fatal(err)
	}
	// Older event: rejected, labels preserved.
	applied, err := s.UpsertPullRequestWithLabels(ctx, PullRequest{
		Repo: "o/r", Number: 5, Title: "pr-old", State: "closed", LastSeenAt: t0, Source: SourceWebhook,
	}, []string{"stale"})
	if err != nil {
		t.Fatal(err)
	}
	if applied {
		t.Fatalf("older PR event reported applied=true")
	}
	labels, _ := s.Labels(ctx, "o/r", SubjectPullRequest, 5)
	if !equalStrings(labels, []string{"ready"}) {
		t.Fatalf("older PR event regressed labels: %v", labels)
	}
}
