package mirrorstore

import (
	"context"
	"testing"
	"time"

	"github.com/befeast/maestro/internal/github"
)

// fakeAPI is an apiReader that records every call, so a test can assert a warm
// mirror performed ZERO API reads (AC 6) and a cold/stale mirror fell back.
type fakeAPI struct {
	issues    []github.Issue
	prs       []github.PR
	closed    map[int]bool
	merged    map[int]bool
	getIssues map[int]github.Issue

	listIssuesCalls int
	listPRsCalls    int
	isClosedCalls   int
	isMergedCalls   int
	getIssueCalls   int
}

func (f *fakeAPI) ListOpenIssues(labels []string) ([]github.Issue, error) {
	f.listIssuesCalls++
	return f.issues, nil
}
func (f *fakeAPI) ListOpenPRs() ([]github.PR, error) {
	f.listPRsCalls++
	return f.prs, nil
}
func (f *fakeAPI) IsIssueClosed(number int) (bool, error) {
	f.isClosedCalls++
	return f.closed[number], nil
}
func (f *fakeAPI) IsPRMerged(prNumber int) (bool, error) {
	f.isMergedCalls++
	return f.merged[prNumber], nil
}
func (f *fakeAPI) GetIssue(number int) (github.Issue, error) {
	f.getIssueCalls++
	return f.getIssues[number], nil
}

func (f *fakeAPI) totalCalls() int {
	return f.listIssuesCalls + f.listPRsCalls + f.isClosedCalls + f.isMergedCalls + f.getIssueCalls
}

// newTestSource wires a Source over a real mirror store and a fake API, with a
// fixed clock so freshness is deterministic.
func newTestSource(t *testing.T, s *Store, api *fakeAPI, now time.Time, horizon time.Duration, apiDirect func() bool) *Source {
	t.Helper()
	resetReadStatsForTest()
	src := NewSource(nil, s, "o/r", SourceOptions{
		Horizon:   horizon,
		APIDirect: apiDirect,
		Now:       func() time.Time { return now },
	})
	src.api = api
	return src
}

func seedIssue(t *testing.T, s *Store, number int, title, state, body string, seen time.Time, labels ...string) {
	t.Helper()
	if _, err := s.UpsertIssueWithLabels(context.Background(), Issue{
		Repo: "o/r", Number: number, Title: title, State: state, Body: body,
		LastSeenAt: seen, Source: SourceWebhook,
	}, labels); err != nil {
		t.Fatalf("seed issue %d: %v", number, err)
	}
}

func seedPR(t *testing.T, s *Store, number int, title, state string, draft, merged bool, headRef, body string, seen time.Time) {
	t.Helper()
	if _, err := s.UpsertPullRequest(context.Background(), PullRequest{
		Repo: "o/r", Number: number, Title: title, State: state, Draft: draft, Merged: merged,
		HeadSHA: "sha", HeadRef: headRef, BaseRef: "main", Body: body,
		LastSeenAt: seen, Source: SourceWebhook,
	}); err != nil {
		t.Fatalf("seed pr %d: %v", number, err)
	}
}

// TestWarmMirrorZeroAPIReads is AC 6: with a warm mirror, the list + point reads
// covered by the mirror perform ZERO GitHub API reads.
func TestWarmMirrorZeroAPIReads(t *testing.T) {
	s := openTestStore(t)
	now := mustTime(t, "2026-07-06T12:00:00Z")
	seedIssue(t, s, 10, "issue ten", "open", "body", now, "maestro-ready")
	seedIssue(t, s, 11, "issue eleven", "closed", "body", now)
	seedPR(t, s, 20, "pr twenty", "open", false, false, "feat/x", "pr body", now)

	api := &fakeAPI{}
	src := newTestSource(t, s, api, now, time.Hour, nil)

	issues, err := src.ListOpenIssues(nil)
	if err != nil {
		t.Fatalf("ListOpenIssues: %v", err)
	}
	if len(issues) != 1 || issues[0].Number != 10 {
		t.Fatalf("open issues = %+v, want just #10", issues)
	}
	if len(issues[0].Labels) != 1 || issues[0].Labels[0].Name != "maestro-ready" {
		t.Fatalf("issue #10 labels = %+v", issues[0].Labels)
	}

	prs, err := src.ListOpenPRs()
	if err != nil {
		t.Fatalf("ListOpenPRs: %v", err)
	}
	if len(prs) != 1 || prs[0].HeadRefName != "feat/x" || prs[0].Body != "pr body" || prs[0].State != "OPEN" {
		t.Fatalf("open prs = %+v", prs)
	}

	closed, err := src.IsIssueClosed(11)
	if err != nil || !closed {
		t.Fatalf("IsIssueClosed(11) = %v, %v; want true", closed, err)
	}
	merged, err := src.IsPRMerged(20)
	if err != nil || merged {
		t.Fatalf("IsPRMerged(20) = %v, %v; want false", merged, err)
	}
	got, err := src.GetIssue(10)
	if err != nil || got.Number != 10 {
		t.Fatalf("GetIssue(10) = %+v, %v", got, err)
	}

	if api.totalCalls() != 0 {
		t.Fatalf("warm mirror hit the API %d time(s); want 0 (%+v)", api.totalCalls(), api)
	}
	stats := ReadStatsSnapshot()
	if stats.APIFallbacks != 0 {
		t.Fatalf("APIFallbacks = %d; want 0", stats.APIFallbacks)
	}
	if stats.MirrorHits != 5 {
		t.Fatalf("MirrorHits = %d; want 5", stats.MirrorHits)
	}
}

// TestColdMirrorFallsBackToAPI is AC 7: an empty mirror degrades to API-direct.
func TestColdMirrorFallsBackToAPI(t *testing.T) {
	s := openTestStore(t)
	now := mustTime(t, "2026-07-06T12:00:00Z")
	api := &fakeAPI{
		issues: []github.Issue{{Number: 1, Title: "from api"}},
		prs:    []github.PR{{Number: 2}},
	}
	src := newTestSource(t, s, api, now, time.Hour, nil)

	issues, err := src.ListOpenIssues(nil)
	if err != nil || len(issues) != 1 || issues[0].Title != "from api" {
		t.Fatalf("cold ListOpenIssues = %+v, %v", issues, err)
	}
	if _, err := src.ListOpenPRs(); err != nil {
		t.Fatalf("cold ListOpenPRs: %v", err)
	}
	if api.listIssuesCalls != 1 || api.listPRsCalls != 1 {
		t.Fatalf("cold mirror did not fall back: %+v", api)
	}
	if got := ReadStatsSnapshot().APIFallbacks; got != 2 {
		t.Fatalf("APIFallbacks = %d; want 2", got)
	}
}

// TestStaleRowFallsBackToAPI: a present-but-stale list triggers a fallback.
func TestStaleRowFallsBackToAPI(t *testing.T) {
	s := openTestStore(t)
	seeded := mustTime(t, "2026-07-01T00:00:00Z")
	now := mustTime(t, "2026-07-06T12:00:00Z") // > 24h later
	seedIssue(t, s, 10, "old", "open", "body", seeded)

	api := &fakeAPI{issues: []github.Issue{{Number: 99, Title: "fresh from api"}}}
	src := newTestSource(t, s, api, now, 24*time.Hour, nil)

	issues, err := src.ListOpenIssues(nil)
	if err != nil {
		t.Fatalf("ListOpenIssues: %v", err)
	}
	if len(issues) != 1 || issues[0].Number != 99 {
		t.Fatalf("stale mirror should have fallen back to API: %+v", issues)
	}
	if api.listIssuesCalls != 1 {
		t.Fatalf("expected 1 API fallback, got %d", api.listIssuesCalls)
	}
}

// TestWarmListWithStaleMemberFallsBack: a list whose NEWEST row is fresh (so the
// mirror is warm) but which also contains an individually-stale open row must
// fall back to the API for the whole list rather than serve the stale member. A
// missed close/unlabel delivery would otherwise leak a no-longer-open issue/PR
// into supervisor/orchestrator decisions (review comment 2).
func TestWarmListWithStaleMemberFallsBack(t *testing.T) {
	s := openTestStore(t)
	now := mustTime(t, "2026-07-06T12:00:00Z")
	stale := mustTime(t, "2026-07-01T00:00:00Z") // > 24h before now
	// Newest row is fresh → warmth check passes; #11 is an individually-stale member.
	seedIssue(t, s, 10, "fresh", "open", "body", now)
	seedIssue(t, s, 11, "stale", "open", "body", stale)
	seedPR(t, s, 20, "fresh pr", "open", false, false, "feat/a", "b", now)
	seedPR(t, s, 21, "stale pr", "open", false, false, "feat/b", "b", stale)

	api := &fakeAPI{
		issues: []github.Issue{{Number: 99, Title: "from api"}},
		prs:    []github.PR{{Number: 98, HeadRefName: "from-api"}},
	}
	src := newTestSource(t, s, api, now, 24*time.Hour, nil)

	issues, err := src.ListOpenIssues(nil)
	if err != nil {
		t.Fatalf("ListOpenIssues: %v", err)
	}
	if len(issues) != 1 || issues[0].Number != 99 {
		t.Fatalf("stale list member should force API fallback, got %+v", issues)
	}
	prs, err := src.ListOpenPRs()
	if err != nil {
		t.Fatalf("ListOpenPRs: %v", err)
	}
	if len(prs) != 1 || prs[0].Number != 98 {
		t.Fatalf("stale PR list member should force API fallback, got %+v", prs)
	}
	if api.listIssuesCalls != 1 || api.listPRsCalls != 1 {
		t.Fatalf("expected one API fallback each: %+v", api)
	}
	if got := ReadStatsSnapshot().MirrorHits; got != 0 {
		t.Fatalf("MirrorHits = %d; want 0 (both lists fell back)", got)
	}
}

// TestListOpenPRsMissingHeadRefFallsBack: after an in-place upgrade the migration
// adds head_ref with an empty default on already-open PR rows, which keep their
// (still fresh) last_seen_at. Serving such a row would hand the orchestrator an
// empty HeadRefName and break branch matching, so the source must fall back to the
// API for the list even though the row is fresh (review comment 1).
func TestListOpenPRsMissingHeadRefFallsBack(t *testing.T) {
	s := openTestStore(t)
	now := mustTime(t, "2026-07-06T12:00:00Z")
	// Fresh row, but empty head_ref — exactly the shape a pre-#826 row has after the
	// ALTER TABLE adds the column with its empty default.
	seedPR(t, s, 20, "migrated pr", "open", false, false, "", "body", now)

	api := &fakeAPI{prs: []github.PR{{Number: 20, HeadRefName: "feat/real"}}}
	src := newTestSource(t, s, api, now, 24*time.Hour, nil)

	prs, err := src.ListOpenPRs()
	if err != nil {
		t.Fatalf("ListOpenPRs: %v", err)
	}
	if len(prs) != 1 || prs[0].HeadRefName != "feat/real" {
		t.Fatalf("empty-head_ref row should force API fallback, got %+v", prs)
	}
	if api.listPRsCalls != 1 {
		t.Fatalf("expected 1 API fallback, got %d", api.listPRsCalls)
	}
	if got := ReadStatsSnapshot().MirrorHits; got != 0 {
		t.Fatalf("MirrorHits = %d; want 0 (missing head_ref fell back)", got)
	}
}

// TestGetIssueHydrates covers the hydration path: a miss fetches from the API,
// stores the row, and the NEXT GetIssue is served locally (zero further API).
func TestGetIssueHydrates(t *testing.T) {
	s := openTestStore(t)
	now := mustTime(t, "2026-07-06T12:00:00Z")
	api := &fakeAPI{getIssues: map[int]github.Issue{
		7: {Number: 7, Title: "hydrated", State: "open", Body: "b"},
	}}
	src := newTestSource(t, s, api, now, time.Hour, nil)

	got, err := src.GetIssue(7)
	if err != nil || got.Title != "hydrated" {
		t.Fatalf("first GetIssue = %+v, %v", got, err)
	}
	if api.getIssueCalls != 1 {
		t.Fatalf("expected 1 hydration fetch, got %d", api.getIssueCalls)
	}
	// Row is now local — the next read hits the mirror, no further API.
	got2, err := src.GetIssue(7)
	if err != nil || got2.Number != 7 {
		t.Fatalf("second GetIssue = %+v, %v", got2, err)
	}
	if api.getIssueCalls != 1 {
		t.Fatalf("second GetIssue hit the API; hydration did not persist (%d calls)", api.getIssueCalls)
	}
	if got2.State != "open" {
		t.Fatalf("hydrated issue state = %q, want open", got2.State)
	}
}

// TestEscapeHatchForcesAPI is AC 3/8: with the escape hatch engaged, every read
// goes to the API even when the mirror is warm.
func TestEscapeHatchForcesAPI(t *testing.T) {
	s := openTestStore(t)
	now := mustTime(t, "2026-07-06T12:00:00Z")
	seedIssue(t, s, 10, "issue", "open", "body", now)

	apiDirect := true
	api := &fakeAPI{issues: []github.Issue{{Number: 10, Title: "api"}}}
	src := newTestSource(t, s, api, now, time.Hour, func() bool { return apiDirect })

	if _, err := src.ListOpenIssues(nil); err != nil {
		t.Fatalf("ListOpenIssues: %v", err)
	}
	if api.listIssuesCalls != 1 {
		t.Fatalf("escape hatch did not force API: %d calls", api.listIssuesCalls)
	}
	// Flipping the hatch off restores mirror-first with no rebuild (hot-reload).
	apiDirect = false
	if _, err := src.ListOpenIssues(nil); err != nil {
		t.Fatalf("ListOpenIssues after hatch off: %v", err)
	}
	if api.listIssuesCalls != 1 {
		t.Fatalf("hatch-off should serve from mirror, but API was called again: %d", api.listIssuesCalls)
	}
	if ReadStatsSnapshot().MirrorHits != 1 {
		t.Fatalf("MirrorHits = %d; want 1 after hatch off", ReadStatsSnapshot().MirrorHits)
	}
}

// TestLabelFilter: ListOpenIssues honors the OR label filter against the mirror.
func TestLabelFilter(t *testing.T) {
	s := openTestStore(t)
	now := mustTime(t, "2026-07-06T12:00:00Z")
	seedIssue(t, s, 1, "ready", "open", "b", now, "maestro-ready")
	seedIssue(t, s, 2, "blocked", "open", "b", now, "blocked")
	seedIssue(t, s, 3, "both", "open", "b", now, "maestro-ready", "extra")

	api := &fakeAPI{}
	src := newTestSource(t, s, api, now, time.Hour, nil)

	got, err := src.ListOpenIssues([]string{"maestro-ready"})
	if err != nil {
		t.Fatalf("ListOpenIssues: %v", err)
	}
	nums := map[int]bool{}
	for _, iss := range got {
		nums[iss.Number] = true
	}
	if len(got) != 2 || !nums[1] || !nums[3] {
		t.Fatalf("label filter returned %+v; want #1 and #3", got)
	}
	if api.totalCalls() != 0 {
		t.Fatalf("label-filtered mirror read hit the API %d time(s)", api.totalCalls())
	}
}

// TestLabelFilterCaseInsensitive: a requested label matches a mirrored label
// that differs only in case, mirroring GitHub's own case-insensitive labels=
// filter, so label-gated work is not silently skipped over a case variant.
func TestLabelFilterCaseInsensitive(t *testing.T) {
	s := openTestStore(t)
	now := mustTime(t, "2026-07-06T12:00:00Z")
	// Mirrored label is capitalised; caller asks for the lower-case form.
	seedIssue(t, s, 1, "ready", "open", "b", now, "Maestro-Ready")

	api := &fakeAPI{}
	src := newTestSource(t, s, api, now, time.Hour, nil)

	got, err := src.ListOpenIssues([]string{"maestro-ready"})
	if err != nil {
		t.Fatalf("ListOpenIssues: %v", err)
	}
	if len(got) != 1 || got[0].Number != 1 {
		t.Fatalf("case-variant label filter returned %+v; want #1", got)
	}
	if api.totalCalls() != 0 {
		t.Fatalf("case-variant mirror read hit the API %d time(s)", api.totalCalls())
	}
}

// TestNilStoreIsAPIDirect: a Source with no mirror store is fail-safe API-direct.
func TestNilStoreIsAPIDirect(t *testing.T) {
	now := mustTime(t, "2026-07-06T12:00:00Z")
	api := &fakeAPI{issues: []github.Issue{{Number: 1}}}
	src := NewSource(nil, nil, "o/r", SourceOptions{Now: func() time.Time { return now }})
	src.api = api
	resetReadStatsForTest()
	if _, err := src.ListOpenIssues(nil); err != nil {
		t.Fatalf("ListOpenIssues: %v", err)
	}
	if api.listIssuesCalls != 1 {
		t.Fatalf("nil-store source should be API-direct: %d calls", api.listIssuesCalls)
	}
}
