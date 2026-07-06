package mirrorstore

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/befeast/maestro/internal/github"
)

// fakeReconcileReader is an in-memory ReconcileReader: it returns the configured
// open-issue / open-PR snapshots and answers IsPRMerged from a lookup table,
// counting the calls so a test can prove the merged read is only made for a
// dropped PR (never per-cycle).
type fakeReconcileReader struct {
	issues     []github.Issue
	prs        []github.PR
	merged     map[int]bool
	issuesErr  error
	prsErr     error
	mergedErr  error
	mergedCall int
}

func (f *fakeReconcileReader) ListAllOpenIssues(labels []string) ([]github.Issue, error) {
	return f.issues, f.issuesErr
}
func (f *fakeReconcileReader) ListAllOpenPRs() ([]github.PR, error) { return f.prs, f.prsErr }
func (f *fakeReconcileReader) IsPRMerged(prNumber int) (bool, error) {
	f.mergedCall++
	if f.mergedErr != nil {
		return false, f.mergedErr
	}
	return f.merged[prNumber], nil
}

func ghIssue(number int, title, body string, labels ...string) github.Issue {
	iss := github.Issue{Number: number, Title: title, Body: body, State: "open"}
	for _, l := range labels {
		iss.Labels = append(iss.Labels, struct {
			Name string `json:"name"`
		}{Name: l})
	}
	return iss
}

// TestReconcileAddsMissingIssue: an issue GitHub reports open that the mirror
// never saw is added by reconcile and counted as a repair.
func TestReconcileAddsMissingIssue(t *testing.T) {
	resetReconcileStatsForTest()
	store := openTestStore(t)
	ctx := context.Background()
	api := &fakeReconcileReader{issues: []github.Issue{ghIssue(7, "hello", "body", "bug")}}

	res, err := NewReconciler(store, api, "o/r").Reconcile(ctx)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if res.IssuesRepaired != 1 || res.Repaired() != 1 {
		t.Fatalf("expected 1 issue repaired, got %+v", res)
	}
	row, ok, err := store.GetIssue(ctx, "o/r", 7)
	if err != nil || !ok {
		t.Fatalf("issue not mirrored: ok=%v err=%v", ok, err)
	}
	if row.Source != SourceReconcile || row.State != "open" || row.Title != "hello" {
		t.Fatalf("unexpected mirrored issue: %+v", row)
	}
	labels, _ := store.Labels(ctx, "o/r", SubjectIssue, 7)
	if len(labels) != 1 || labels[0] != "bug" {
		t.Fatalf("labels not mirrored: %v", labels)
	}
}

// TestReconcileSkipsInSyncIssue: when the mirror already matches GitHub, reconcile
// writes nothing (no repair, and the webhook-sourced last_seen_at is preserved).
func TestReconcileSkipsInSyncIssue(t *testing.T) {
	resetReconcileStatsForTest()
	store := openTestStore(t)
	ctx := context.Background()
	seen := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	if _, err := store.UpsertIssueWithLabels(ctx, Issue{
		Repo: "o/r", Number: 7, Title: "hello", State: "open", Body: "body",
		LastSeenAt: seen, Source: SourceWebhook,
	}, []string{"bug"}); err != nil {
		t.Fatal(err)
	}
	api := &fakeReconcileReader{issues: []github.Issue{ghIssue(7, "hello", "body", "bug")}}

	res, err := NewReconciler(store, api, "o/r").Reconcile(ctx)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if res.Repaired() != 0 {
		t.Fatalf("expected no repair for in-sync issue, got %+v", res)
	}
	row, _, _ := store.GetIssue(ctx, "o/r", 7)
	if row.Source != SourceWebhook || !row.LastSeenAt.Equal(seen) {
		t.Fatalf("in-sync row should keep its webhook source/timestamp, got %+v", row)
	}
}

// TestReconcileRepairsChangedIssue: a title/label edit the mirror missed is
// repaired and re-stamped with the reconcile source.
func TestReconcileRepairsChangedIssue(t *testing.T) {
	resetReconcileStatsForTest()
	store := openTestStore(t)
	ctx := context.Background()
	old := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	if _, err := store.UpsertIssueWithLabels(ctx, Issue{
		Repo: "o/r", Number: 7, Title: "old title", State: "open", Body: "body",
		LastSeenAt: old, Source: SourceWebhook,
	}, []string{"bug"}); err != nil {
		t.Fatal(err)
	}
	api := &fakeReconcileReader{issues: []github.Issue{ghIssue(7, "new title", "body", "bug", "ready")}}

	res, err := NewReconciler(store, api, "o/r").Reconcile(ctx)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if res.IssuesRepaired != 1 {
		t.Fatalf("expected 1 repair, got %+v", res)
	}
	row, _, _ := store.GetIssue(ctx, "o/r", 7)
	if row.Title != "new title" || row.Source != SourceReconcile {
		t.Fatalf("issue not repaired: %+v", row)
	}
	labels, _ := store.Labels(ctx, "o/r", SubjectIssue, 7)
	if len(labels) != 2 {
		t.Fatalf("labels not repaired: %v", labels)
	}
}

// TestReconcileClosesMissedIssue: an issue the mirror still records open that
// GitHub no longer lists (a missed close delivery) is marked closed. This is the
// convergence-after-a-webhook-outage acceptance (#827 AC 1).
func TestReconcileClosesMissedIssue(t *testing.T) {
	resetReconcileStatsForTest()
	store := openTestStore(t)
	ctx := context.Background()
	if _, err := store.UpsertIssue(ctx, Issue{
		Repo: "o/r", Number: 7, Title: "hello", State: "open", Body: "body",
		LastSeenAt: time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC), Source: SourceWebhook,
	}); err != nil {
		t.Fatal(err)
	}
	// GitHub now reports NO open issues.
	api := &fakeReconcileReader{}

	res, err := NewReconciler(store, api, "o/r").Reconcile(ctx)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if res.IssuesRepaired != 1 {
		t.Fatalf("expected the missed-close to be repaired, got %+v", res)
	}
	row, _, _ := store.GetIssue(ctx, "o/r", 7)
	if row.State != "closed" || row.Source != SourceReconcile {
		t.Fatalf("issue not closed by reconcile: %+v", row)
	}
}

// TestReconcileBackfillsPRHeadRef: a PR mirrored before the head_ref column
// existed (empty branch) is repaired to match GitHub's headRefName.
func TestReconcileBackfillsPRHeadRef(t *testing.T) {
	resetReconcileStatsForTest()
	store := openTestStore(t)
	ctx := context.Background()
	if _, err := store.UpsertPullRequest(ctx, PullRequest{
		Repo: "o/r", Number: 12, Title: "feat", State: "open", HeadRef: "", HeadSHA: "abc",
		Body: "b", LastSeenAt: time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC), Source: SourceWebhook,
	}); err != nil {
		t.Fatal(err)
	}
	api := &fakeReconcileReader{prs: []github.PR{{Number: 12, Title: "feat", HeadRefName: "feat/x", Body: "b", State: "OPEN"}}}

	res, err := NewReconciler(store, api, "o/r").Reconcile(ctx)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if res.PRsRepaired != 1 {
		t.Fatalf("expected PR head_ref backfill, got %+v", res)
	}
	row, _, _ := store.GetPullRequest(ctx, "o/r", 12)
	if row.HeadRef != "feat/x" || row.HeadSHA != "abc" || row.Source != SourceReconcile {
		t.Fatalf("PR not repaired (head_sha must be preserved): %+v", row)
	}
}

// TestReconcileClosesMergedPR: a PR the mirror records open that GitHub no longer
// lists is settled closed with the authoritative merged flag, and IsPRMerged is
// consulted exactly once (only for the dropped PR).
func TestReconcileClosesMergedPR(t *testing.T) {
	resetReconcileStatsForTest()
	store := openTestStore(t)
	ctx := context.Background()
	if _, err := store.UpsertPullRequest(ctx, PullRequest{
		Repo: "o/r", Number: 12, Title: "feat", State: "open", HeadRef: "feat/x",
		Body: "b", LastSeenAt: time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC), Source: SourceWebhook,
	}); err != nil {
		t.Fatal(err)
	}
	api := &fakeReconcileReader{merged: map[int]bool{12: true}} // GitHub lists no open PRs

	res, err := NewReconciler(store, api, "o/r").Reconcile(ctx)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if res.PRsRepaired != 1 {
		t.Fatalf("expected the dropped PR to be repaired, got %+v", res)
	}
	if api.mergedCall != 1 {
		t.Fatalf("IsPRMerged should be called once for the dropped PR, got %d", api.mergedCall)
	}
	row, _, _ := store.GetPullRequest(ctx, "o/r", 12)
	if row.State != "closed" || !row.Merged {
		t.Fatalf("PR not settled merged/closed: %+v", row)
	}
}

// TestReconcileMergeStatusErrorRecordsFailure: when a dropped PR's IsPRMerged
// lookup errors, the row is NOT closed with a guessed flag and the pass is
// recorded as a FAILURE (surfaced via last_error) rather than silently skipped —
// so a persistently non-converging PR is visible in diagnostics. A sibling
// dropped PR whose lookup succeeds is still settled in the same pass.
func TestReconcileMergeStatusErrorRecordsFailure(t *testing.T) {
	resetReconcileStatsForTest()
	store := openTestStore(t)
	ctx := context.Background()
	if _, err := store.UpsertPullRequest(ctx, PullRequest{
		Repo: "o/r", Number: 12, Title: "feat", State: "open", HeadRef: "feat/x",
		Body: "b", LastSeenAt: time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC), Source: SourceWebhook,
	}); err != nil {
		t.Fatal(err)
	}
	// GitHub lists no open PRs, and the merge-status lookup persistently fails.
	api := &fakeReconcileReader{mergedErr: errors.New("404 not found")}

	res, err := NewReconciler(store, api, "o/r").Reconcile(ctx)
	if err == nil {
		t.Fatal("expected reconcile to return the merge-status error")
	}
	if res.PRsRepaired != 0 {
		t.Fatalf("a PR whose merged state is unknown must not be counted repaired, got %+v", res)
	}
	// The row is left open for a later pass — never closed with a guessed flag.
	row, _, _ := store.GetPullRequest(ctx, "o/r", 12)
	if row.State != "open" || row.Merged {
		t.Fatalf("PR must stay open when merged state is unknown, got %+v", row)
	}
	// The failure is visible, not silent.
	stats := ReconcileStatsSnapshot()
	if len(stats) != 1 || stats[0].Failures != 1 || stats[0].LastError == "" {
		t.Fatalf("merge-status failure not recorded: %+v", stats)
	}
}

// TestReconcileUnchangedRepoIsCheap: a fully caught-up mirror does zero repairs
// and does not consult IsPRMerged — the "cheap when nothing changed" property.
func TestReconcileUnchangedRepoIsCheap(t *testing.T) {
	resetReconcileStatsForTest()
	store := openTestStore(t)
	ctx := context.Background()
	seen := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	if _, err := store.UpsertIssueWithLabels(ctx, Issue{
		Repo: "o/r", Number: 7, Title: "hello", State: "open", Body: "body",
		LastSeenAt: seen, Source: SourceWebhook,
	}, []string{"bug"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpsertPullRequest(ctx, PullRequest{
		Repo: "o/r", Number: 12, Title: "feat", State: "open", HeadRef: "feat/x",
		Body: "b", LastSeenAt: seen, Source: SourceWebhook,
	}); err != nil {
		t.Fatal(err)
	}
	api := &fakeReconcileReader{
		issues: []github.Issue{ghIssue(7, "hello", "body", "bug")},
		prs:    []github.PR{{Number: 12, Title: "feat", HeadRefName: "feat/x", Body: "b", State: "OPEN"}},
	}

	res, err := NewReconciler(store, api, "o/r").Reconcile(ctx)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if res.Repaired() != 0 {
		t.Fatalf("expected zero repairs on an unchanged repo, got %+v", res)
	}
	if api.mergedCall != 0 {
		t.Fatalf("IsPRMerged must not be called when nothing dropped, got %d", api.mergedCall)
	}
}

// TestReconcileStatsRecorded: the process-global registry records runs, the last
// success, and cumulative + last-pass repair counts.
func TestReconcileStatsRecorded(t *testing.T) {
	resetReconcileStatsForTest()
	store := openTestStore(t)
	ctx := context.Background()
	api := &fakeReconcileReader{issues: []github.Issue{ghIssue(7, "hello", "body")}}
	rec := NewReconciler(store, api, "o/r")

	if _, err := rec.Reconcile(ctx); err != nil { // repairs 1 (adds)
		t.Fatal(err)
	}
	if _, err := rec.Reconcile(ctx); err != nil { // repairs 0 (in sync)
		t.Fatal(err)
	}

	stats := ReconcileStatsSnapshot()
	if len(stats) != 1 || stats[0].Repo != "o/r" {
		t.Fatalf("unexpected stats: %+v", stats)
	}
	st := stats[0]
	if st.Runs != 2 || st.Failures != 0 {
		t.Fatalf("runs/failures = %d/%d, want 2/0", st.Runs, st.Failures)
	}
	if st.Repairs != 1 || st.LastRepairs != 0 {
		t.Fatalf("cumulative/last repairs = %d/%d, want 1/0", st.Repairs, st.LastRepairs)
	}
	if st.LastSuccessAt.IsZero() {
		t.Fatal("last success time not recorded")
	}

	totals := ReconcileTotalsSnapshot()
	if totals.Repos != 1 || totals.Runs != 2 || totals.Repairs != 1 {
		t.Fatalf("unexpected totals: %+v", totals)
	}
}

// TestReconcileRecordsFailure: a GitHub read error is recorded as a failed pass
// and surfaced as LastError, not a silent no-op.
func TestReconcileRecordsFailure(t *testing.T) {
	resetReconcileStatsForTest()
	store := openTestStore(t)
	ctx := context.Background()
	api := &fakeReconcileReader{issuesErr: errors.New("boom")}

	if _, err := NewReconciler(store, api, "o/r").Reconcile(ctx); err == nil {
		t.Fatal("expected reconcile to return the list error")
	}
	stats := ReconcileStatsSnapshot()
	if len(stats) != 1 || stats[0].Failures != 1 || stats[0].LastError == "" {
		t.Fatalf("failure not recorded: %+v", stats)
	}
	if !stats[0].LastSuccessAt.IsZero() {
		t.Fatalf("a failed pass must not set last success time: %+v", stats[0])
	}
}
