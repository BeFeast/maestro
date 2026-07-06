package github

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// withGHRunner installs a runner that records every invocation's args and
// answers via fn. It also neutralizes the sleeper and jitter, and restores
// everything on cleanup.
func withGHRunner(t *testing.T, fn func(args []string) ([]byte, error)) *[][]string {
	t.Helper()
	origRunner := ghAPIRunner
	origSleep := ghAPISleep
	origJitter := ghAPIJitterFrac
	// The primary-limit pause gate (#812) is process-wide; clear it before and
	// after so a gate armed by another test cannot short-circuit this runner.
	resetPrimaryLimitForTest()
	var calls [][]string
	ghAPIRunner = func(args ...string) ([]byte, error) {
		calls = append(calls, args)
		return fn(args)
	}
	ghAPISleep = func(time.Duration) {}
	ghAPIJitterFrac = func() float64 { return 0 }
	t.Cleanup(func() {
		ghAPIRunner = origRunner
		ghAPISleep = origSleep
		ghAPIJitterFrac = origJitter
		resetPrimaryLimitForTest()
	})
	return &calls
}

func clearETagCache(t *testing.T) {
	t.Helper()
	reset := func() {
		etagMu.Lock()
		etagCache = map[string]*etagEntry{}
		etagMu.Unlock()
	}
	reset()
	t.Cleanup(reset)
}

// includeResponse builds the combined output `gh api --include` produces: a
// status line, headers, a blank line, then the body.
func includeResponse(statusLine, etag, body string) []byte {
	var b strings.Builder
	b.WriteString("HTTP/2.0 " + statusLine + "\r\n")
	b.WriteString("Content-Type: application/json; charset=utf-8\r\n")
	if etag != "" {
		b.WriteString("Etag: " + etag + "\r\n")
	}
	b.WriteString("X-Ratelimit-Remaining: 4000\r\n")
	b.WriteString("\r\n")
	b.WriteString(body)
	return []byte(b.String())
}

func argWithPrefix(args []string, prefix string) string {
	for _, a := range args {
		if strings.HasPrefix(a, prefix) {
			return a
		}
	}
	return ""
}

func hasArg(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}

func TestGHAPI_ConditionalServes304FromCache(t *testing.T) {
	clearETagCache(t)
	endpoint := "repos/owner/repo/pulls?state=open&per_page=100"
	etag := `W/"abc123"`
	bodyJSON := `[{"number":839}]`
	calls := withGHRunner(t, func(args []string) ([]byte, error) {
		if argWithPrefix(args, "If-None-Match:") != "" {
			// Conditional replay of unchanged content: bodyless 304 headers on
			// stdout plus gh's non-2xx error text, exit status 1.
			out := append(includeResponse("304 Not Modified", "", ""),
				[]byte("gh: HTTP 304 (https://api.github.com/"+endpoint+")")...)
			return out, errors.New("exit status 1")
		}
		return includeResponse("200 OK", etag, bodyJSON), nil
	})

	before := APIUsage()

	out, err := ghAPIWithArgs(endpoint)
	if err != nil {
		t.Fatalf("first fetch error = %v", err)
	}
	if string(out) != bodyJSON {
		t.Fatalf("first fetch body = %q, want %q", out, bodyJSON)
	}
	if !hasArg((*calls)[0], "--include") {
		t.Fatalf("first call args = %v, want --include so the ETag is captured", (*calls)[0])
	}
	if argWithPrefix((*calls)[0], "If-None-Match:") != "" {
		t.Fatalf("first call must not send If-None-Match (nothing cached yet); args = %v", (*calls)[0])
	}

	out2, err := ghAPIWithArgs(endpoint)
	if err != nil {
		t.Fatalf("conditional refetch error = %v, want the 304 served from cache", err)
	}
	if string(out2) != bodyJSON {
		t.Fatalf("304 body = %q, want the cached copy %q", out2, bodyJSON)
	}
	if got := argWithPrefix((*calls)[1], "If-None-Match:"); got != "If-None-Match: "+etag {
		t.Fatalf("second call conditional header = %q, want the cached ETag", got)
	}

	delta := APIUsage()
	if delta.Requests-before.Requests != 2 {
		t.Fatalf("requests delta = %d, want 2", delta.Requests-before.Requests)
	}
	if delta.NotModified-before.NotModified != 1 {
		t.Fatalf("not-modified delta = %d, want 1 (the free 304)", delta.NotModified-before.NotModified)
	}
}

func TestGHAPI_Conditional304ErrorTextWithoutHeadersServesCache(t *testing.T) {
	clearETagCache(t)
	endpoint := "repos/owner/repo/pulls/136"
	cached := []byte(`{"number":136}`)
	etagStore(endpoint, `W/"pr136"`, cached)
	withGHRunner(t, func(args []string) ([]byte, error) {
		// Defensive path: gh swallowed the header block, only its error text
		// survives. The cached copy must still be served.
		return []byte("gh: HTTP 304 (https://api.github.com/" + endpoint + ")"), errors.New("exit status 1")
	})

	out, err := ghAPIWithArgs(endpoint)
	if err != nil {
		t.Fatalf("error = %v, want cached body on a header-less 304", err)
	}
	if string(out) != string(cached) {
		t.Fatalf("body = %q, want cached %q", out, cached)
	}
}

func TestGHAPI_ConditionalRefreshesCacheOnChange(t *testing.T) {
	clearETagCache(t)
	endpoint := "repos/owner/repo/commits/main"
	etagStore(endpoint, `W/"old"`, []byte(`{"sha":"old"}`))
	newBody := `{"sha":"new"}`
	withGHRunner(t, func(args []string) ([]byte, error) {
		return includeResponse("200 OK", `W/"new"`, newBody), nil
	})

	out, err := ghAPIWithArgs(endpoint)
	if err != nil {
		t.Fatalf("error = %v", err)
	}
	if string(out) != newBody {
		t.Fatalf("body = %q, want the fresh %q", out, newBody)
	}
	gotETag, gotBody := etagLookup(endpoint)
	if gotETag != `W/"new"` || string(gotBody) != newBody {
		t.Fatalf("cache = (%q, %q), want the refreshed entry", gotETag, gotBody)
	}
}

func TestGHAPI_PaginatedMultiPageConcatenatesAndSkipsCache(t *testing.T) {
	clearETagCache(t)
	endpoint := "repos/owner/repo/commits/abc/check-runs?per_page=100"
	page1 := includeResponse("200 OK", `W/"p1"`, "{\"check_runs\":[\n")
	page2 := includeResponse("200 OK", `W/"p2"`, "]}")
	calls := withGHRunner(t, func(args []string) ([]byte, error) {
		return append(append([]byte{}, page1...), page2...), nil
	})

	out, err := ghAPIWithArgs(endpoint, "--paginate")
	if err != nil {
		t.Fatalf("error = %v", err)
	}
	if string(out) != "{\"check_runs\":[\n]}" {
		t.Fatalf("body = %q, want the pages' bodies concatenated", out)
	}
	if e, b := etagLookup(etagCacheKey(endpoint, true)); e != "" || b != nil {
		t.Fatalf("cache = (%q, %q), want empty — a multi-page response must not be cached", e, b)
	}
	if _, err := ghAPIWithArgs(endpoint, "--paginate"); err != nil {
		t.Fatalf("second fetch error = %v", err)
	}
	if got := argWithPrefix((*calls)[1], "If-None-Match:"); got != "" {
		t.Fatalf("second call sent %q, want no conditional header after a multi-page response", got)
	}
}

func TestGHAPI_PaginatedFullPageNotCached(t *testing.T) {
	clearETagCache(t)
	// Exactly per_page items: the collection can grow onto page 2 while page 1
	// stays byte-identical, so a later 304 would hide the new items. The page
	// must never enter the cache.
	endpoint := "repos/owner/repo/issues/5/comments?per_page=2"
	fullPage := `[{"id":1},{"id":2}]`
	calls := withGHRunner(t, func(args []string) ([]byte, error) {
		return includeResponse("200 OK", `W/"full"`, fullPage), nil
	})

	out, err := ghAPIWithArgs(endpoint, "--paginate")
	if err != nil {
		t.Fatalf("error = %v", err)
	}
	if string(out) != fullPage {
		t.Fatalf("body = %q, want %q", out, fullPage)
	}
	if e, b := etagLookup(etagCacheKey(endpoint, true)); e != "" || b != nil {
		t.Fatalf("cache = (%q, %q), want empty — a full page can overflow to page 2 without changing", e, b)
	}
	if _, err := ghAPIWithArgs(endpoint, "--paginate"); err != nil {
		t.Fatalf("second fetch error = %v", err)
	}
	if got := argWithPrefix((*calls)[1], "If-None-Match:"); got != "" {
		t.Fatalf("second call sent %q, want no conditional header for an uncacheable full page", got)
	}
}

func TestGHAPI_PaginatedPartialPageServes304FromCache(t *testing.T) {
	clearETagCache(t)
	// Fewer items than per_page: any growth changes page 1's bytes, so a 304
	// proves the whole collection unchanged and the cache is safe to serve.
	endpoint := "repos/owner/repo/pulls/7/comments?per_page=100"
	body := `[{"id":1}]`
	etag := `W/"partial"`
	calls := withGHRunner(t, func(args []string) ([]byte, error) {
		if argWithPrefix(args, "If-None-Match:") != "" {
			out := append(includeResponse("304 Not Modified", "", ""),
				[]byte("gh: HTTP 304 (https://api.github.com/"+endpoint+")")...)
			return out, errors.New("exit status 1")
		}
		return includeResponse("200 OK", etag, body), nil
	})

	if _, err := ghAPIWithArgs(endpoint, "--paginate"); err != nil {
		t.Fatalf("first fetch error = %v", err)
	}
	out, err := ghAPIWithArgs(endpoint, "--paginate")
	if err != nil {
		t.Fatalf("conditional refetch error = %v", err)
	}
	if string(out) != body {
		t.Fatalf("304 body = %q, want the cached copy %q", out, body)
	}
	if got := argWithPrefix((*calls)[1], "If-None-Match:"); got != "If-None-Match: "+etag {
		t.Fatalf("second call conditional header = %q, want the cached ETag", got)
	}
}

func TestGHAPI_PaginatedTotalCountBodyCached(t *testing.T) {
	clearETagCache(t)
	// check-runs shape: growth anywhere changes total_count on page 1, so even
	// a full page is safe to cache.
	endpoint := "repos/owner/repo/commits/abc/check-runs?per_page=100"
	body := `{"total_count":1,"check_runs":[{"id":9}]}`
	withGHRunner(t, func(args []string) ([]byte, error) {
		return includeResponse("200 OK", `W/"cr"`, body), nil
	})

	if _, err := ghAPIWithArgs(endpoint, "--paginate"); err != nil {
		t.Fatalf("error = %v", err)
	}
	if e, b := etagLookup(etagCacheKey(endpoint, true)); e != `W/"cr"` || string(b) != body {
		t.Fatalf("cache = (%q, %q), want the total_count body cached", e, b)
	}
}

func TestPaginatedCacheable(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		perPage int
		want    bool
	}{
		{"array below page size", `[{"id":1}]`, 100, true},
		{"array at page size", `[{"id":1},{"id":2}]`, 2, false},
		{"empty array", `[]`, 100, true},
		{"object with total_count", `{"total_count":3,"check_runs":[]}`, 100, true},
		{"object without total_count", `{"check_runs":[]}`, 100, false},
		{"invalid json", `[{"id":`, 100, false},
		{"empty body", "", 100, false},
	}
	for _, tc := range cases {
		if got := paginatedCacheable([]byte(tc.body), tc.perPage); got != tc.want {
			t.Fatalf("%s: paginatedCacheable = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestEndpointPerPage(t *testing.T) {
	cases := []struct {
		endpoint string
		want     int
	}{
		{"repos/o/r/issues/5/comments?per_page=100", 100},
		{"repos/o/r/issues/5/comments?direction=asc&per_page=7", 7},
		{"repos/o/r/issues/5/comments", 30},
	}
	for _, tc := range cases {
		if got := endpointPerPage(tc.endpoint); got != tc.want {
			t.Fatalf("endpointPerPage(%q) = %d, want %d", tc.endpoint, got, tc.want)
		}
	}
}

func TestGHAPI_ShutdownSkipsRateLimitRetries(t *testing.T) {
	resetShutdownForTest()
	t.Cleanup(resetShutdownForTest)
	BeginShutdown()

	limit := []byte("gh: API rate limit exceeded for user ID 51094. (HTTP 403)")
	calls := withGHRunner(t, func(args []string) ([]byte, error) {
		return limit, errors.New("exit status 1")
	})

	_, err := ghAPIWithArgs("repos/owner/repo/pulls/839")
	if err == nil {
		t.Fatal("error = nil, want the rate-limit failure surfaced immediately")
	}
	if !strings.Contains(err.Error(), "rate limit") {
		t.Fatalf("error = %q, want the real rate-limit text", err)
	}
	if len(*calls) != 1 {
		t.Fatalf("runner calls = %d, want 1 — no retries once shutdown has begun", len(*calls))
	}
}

func TestGHAPI_ShutdownDuringBackoffStopsRetrying(t *testing.T) {
	resetShutdownForTest()
	t.Cleanup(resetShutdownForTest)

	limit := []byte("gh: You have exceeded a secondary rate limit. (HTTP 403)")
	calls := withGHRunner(t, func(args []string) ([]byte, error) {
		return limit, errors.New("exit status 1")
	})
	sleeps := 0
	ghAPISleep = func(time.Duration) {
		sleeps++
		BeginShutdown() // shutdown begins while the backoff is sleeping
	}

	_, err := ghAPIWithArgs("repos/owner/repo/pulls/838")
	if err == nil {
		t.Fatal("error = nil, want failure once shutdown interrupts the backoff")
	}
	if len(*calls) != 1 {
		t.Fatalf("runner calls = %d, want 1 — no further attempt after an interrupted backoff", len(*calls))
	}
	if sleeps != 1 {
		t.Fatalf("sleeps = %d, want 1", sleeps)
	}
}

func TestParseGHConditionalResponse(t *testing.T) {
	if _, ok := parseGHConditionalResponse([]byte(`{"ok":true}`)); ok {
		t.Fatal("plain JSON must not parse as a header block")
	}
	if _, ok := parseGHConditionalResponse([]byte("gh: HTTP 304 (https://api.github.com/x)")); ok {
		t.Fatal("gh error text must not parse as a header block")
	}

	resp, ok := parseGHConditionalResponse(includeResponse("200 OK", `W/"x"`, `{"a":1}`))
	if !ok {
		t.Fatal("single 200 block did not parse")
	}
	if resp.status != 200 || resp.etag != `W/"x"` || string(resp.body) != `{"a":1}` || resp.blocks != 1 || !resp.allOK {
		t.Fatalf("parsed = %+v body=%q, want status 200 / etag / body / 1 block / allOK", resp, resp.body)
	}

	resp304, ok := parseGHConditionalResponse(includeResponse("304 Not Modified", "", ""))
	if !ok {
		t.Fatal("304 block did not parse")
	}
	if resp304.status != 304 || resp304.allOK {
		t.Fatalf("parsed 304 = %+v, want status 304 and not allOK", resp304)
	}
}

func TestGHConditionalEligible(t *testing.T) {
	cases := []struct {
		endpoint string
		args     []string
		want     bool
	}{
		{"repos/o/r/pulls?state=open", nil, true},
		{"repos/o/r/commits/sha/check-runs?per_page=100", []string{"--paginate"}, true},
		{"rate_limit", nil, false},
		{"repos/o/r/pulls", []string{"-X", "POST"}, false},
	}
	for _, tc := range cases {
		if got := ghConditionalEligible(tc.endpoint, tc.args); got != tc.want {
			t.Fatalf("ghConditionalEligible(%q, %v) = %v, want %v", tc.endpoint, tc.args, got, tc.want)
		}
	}
}

func TestAPIUsage_CountsAndRollsHourlyWindow(t *testing.T) {
	base := time.Date(2026, 7, 2, 21, 0, 0, 0, time.UTC)
	now := base
	origNow := apiUsageNow
	apiUsageNow = func() time.Time { return now }
	t.Cleanup(func() { apiUsageNow = origNow })
	apiStatsMu.Lock()
	apiStatsWindow = APIStats{}
	apiWindowStart = time.Time{}
	apiStatsMu.Unlock()

	before := APIUsage()
	noteAPIRequest(false, false)
	noteAPIRequest(true, false)
	noteAPIRequest(false, true)
	got := APIUsage()
	if d := got.Requests - before.Requests; d != 3 {
		t.Fatalf("requests delta = %d, want 3", d)
	}
	if d := got.NotModified - before.NotModified; d != 1 {
		t.Fatalf("not-modified delta = %d, want 1", d)
	}
	if d := got.RateLimited - before.RateLimited; d != 1 {
		t.Fatalf("rate-limited delta = %d, want 1", d)
	}

	// Crossing the window boundary emits the digest and resets the window; the
	// request that crossed it opens the new window rather than vanishing.
	now = base.Add(2 * time.Hour)
	noteAPIRequest(false, false)
	apiStatsMu.Lock()
	windowReqs := apiStatsWindow.Requests
	windowStart := apiWindowStart
	apiStatsMu.Unlock()
	if windowReqs != 1 {
		t.Fatalf("window requests after rollover = %d, want 1 (the boundary-crossing request)", windowReqs)
	}
	if !windowStart.Equal(now) {
		t.Fatalf("window start = %s, want reset to %s", windowStart, now)
	}
}

// TestListAllOpenIssues_FlattensPaginatedPages proves the #827 review fix: a
// repo with more than one page of open issues answers as several back-to-back
// `[...]` documents (the wrapper concatenates page bodies), and ListAllOpenIssues
// must flatten every page rather than fail the first multi-page reconcile.
func TestListAllOpenIssues_FlattensPaginatedPages(t *testing.T) {
	clearETagCache(t)
	c := &Client{Repo: "owner/repo"}
	// page1's body ends in a newline, exactly as gh prints between --include
	// blocks, so the second HTTP status line is recognised at line start.
	page1 := includeResponse("200 OK", `W/"p1"`, "[{\"number\":1,\"title\":\"one\"},{\"number\":2,\"title\":\"two\"}]\n")
	page2 := includeResponse("200 OK", `W/"p2"`, `[{"number":3,"title":"three"}]`)
	calls := withGHRunner(t, func(args []string) ([]byte, error) {
		if !hasArg(args, "--paginate") {
			t.Errorf("expected --paginate for the authoritative open set; args = %v", args)
		}
		return append(append([]byte{}, page1...), page2...), nil
	})

	issues, err := c.ListAllOpenIssues(nil)
	if err != nil {
		t.Fatalf("ListAllOpenIssues error = %v", err)
	}
	if len(issues) != 3 {
		t.Fatalf("got %d issues across two pages, want 3: %#v", len(issues), issues)
	}
	if issues[0].Number != 1 || issues[1].Number != 2 || issues[2].Number != 3 {
		t.Fatalf("flattened order = %d,%d,%d, want 1,2,3", issues[0].Number, issues[1].Number, issues[2].Number)
	}
	// A multi-page response is never cached, so the ETag of page one must not be
	// stored and used to serve a truncated 304 next pass.
	if e, b := etagLookup(etagCacheKey("repos/owner/repo/issues?state=open&per_page=100", true)); e != "" || b != nil {
		t.Fatalf("cache = (%q, %q), want empty for a multi-page open set", e, b)
	}
	if len(*calls) != 1 {
		t.Fatalf("runner calls = %d, want 1", len(*calls))
	}
}

// TestListAllOpenPRs_FlattensPaginatedPages is the ListAllOpenPRs counterpart.
func TestListAllOpenPRs_FlattensPaginatedPages(t *testing.T) {
	clearETagCache(t)
	c := &Client{Repo: "owner/repo"}
	page1 := includeResponse("200 OK", `W/"p1"`, "[{\"number\":10,\"head\":{\"ref\":\"a\"}},{\"number\":11,\"head\":{\"ref\":\"b\"}}]\n")
	page2 := includeResponse("200 OK", `W/"p2"`, `[{"number":12,"head":{"ref":"c"}}]`)
	withGHRunner(t, func(args []string) ([]byte, error) {
		return append(append([]byte{}, page1...), page2...), nil
	})

	prs, err := c.ListAllOpenPRs()
	if err != nil {
		t.Fatalf("ListAllOpenPRs error = %v", err)
	}
	if len(prs) != 3 {
		t.Fatalf("got %d PRs across two pages, want 3: %#v", len(prs), prs)
	}
	if prs[0].Number != 10 || prs[1].Number != 11 || prs[2].Number != 12 {
		t.Fatalf("flattened order = %d,%d,%d, want 10,11,12", prs[0].Number, prs[1].Number, prs[2].Number)
	}
}

// TestListAllOpenIssues_UnchangedRepoServes304 guards acceptance criterion #2:
// an unchanged repo whose open set fits one partial page keeps answering 304
// from the ETag cache. The flatten fix must not disturb the conditional path
// (this is why the reconciler stays on --paginate rather than --slurp, which
// would disqualify the call from conditional eligibility).
func TestListAllOpenIssues_UnchangedRepoServes304(t *testing.T) {
	clearETagCache(t)
	c := &Client{Repo: "owner/repo"}
	endpoint := "repos/owner/repo/issues?state=open&per_page=100"
	body := `[{"number":1,"title":"one"}]`
	etag := `W/"single"`
	calls := withGHRunner(t, func(args []string) ([]byte, error) {
		if argWithPrefix(args, "If-None-Match:") != "" {
			out := append(includeResponse("304 Not Modified", "", ""),
				[]byte("gh: HTTP 304 (https://api.github.com/"+endpoint+")")...)
			return out, errors.New("exit status 1")
		}
		return includeResponse("200 OK", etag, body), nil
	})

	before := APIUsage()
	first, err := c.ListAllOpenIssues(nil)
	if err != nil {
		t.Fatalf("first ListAllOpenIssues error = %v", err)
	}
	second, err := c.ListAllOpenIssues(nil)
	if err != nil {
		t.Fatalf("second ListAllOpenIssues error = %v", err)
	}
	if len(first) != 1 || len(second) != 1 || first[0].Number != second[0].Number {
		t.Fatalf("first=%#v second=%#v, want the same single issue both passes", first, second)
	}
	if got := argWithPrefix((*calls)[1], "If-None-Match:"); got != "If-None-Match: "+etag {
		t.Fatalf("second call conditional header = %q, want the cached ETag replayed", got)
	}
	if d := APIUsage().NotModified - before.NotModified; d != 1 {
		t.Fatalf("not-modified delta = %d, want 1 (the free 304 on the unchanged repo)", d)
	}
}

// TestPaginatedReadDoesNotReuseNonPaginatedCache proves the #827 review fix for
// the paginated cache collision. A single-page ListOpenIssues caches page one
// under the endpoint key; the paginated ListAllOpenIssues reconcile read hits
// the SAME endpoint string. If the paginated read reused that cache entry it
// could replay the page-one ETag, receive a 304, and serve the first page as
// the entire open set — wrongly treating still-open issues past page one as
// missing and stamping them closed. The namespaced cache key must keep the two
// reads from sharing a body, so the paginated read always fetches every page.
func TestPaginatedReadDoesNotReuseNonPaginatedCache(t *testing.T) {
	clearETagCache(t)
	c := &Client{Repo: "owner/repo"}
	// Two concatenated --include blocks: the reconciler's authoritative open set
	// spans two pages (issues #1 and #2).
	full := append(
		includeResponse("200 OK", `W/"p1"`, "[{\"number\":1,\"title\":\"one\"}]\n"),
		includeResponse("200 OK", `W/"p2"`, `[{"number":2,"title":"two"}]`)...,
	)
	calls := withGHRunner(t, func(args []string) ([]byte, error) {
		paginated := hasArg(args, "--paginate")
		if paginated && argWithPrefix(args, "If-None-Match:") != "" {
			// The bug: the paginated reconcile read replaying the non-paginated
			// page-one ETag. GitHub would answer 304 and hide page two.
			t.Errorf("paginated read replayed a non-paginated ETag (%q) — cache collision",
				argWithPrefix(args, "If-None-Match:"))
		}
		if paginated {
			return full, nil
		}
		// Non-paginated ListOpenIssues sees only page one and caches it.
		return includeResponse("200 OK", `W/"page1"`, `[{"number":1,"title":"one"}]`), nil
	})

	// Prime the non-paginated cache entry.
	if _, err := c.ListOpenIssues(nil); err != nil {
		t.Fatalf("ListOpenIssues error = %v", err)
	}
	// The reconciler's authoritative read must fetch every page fresh, not serve
	// the truncated page-one body cached by the non-paginated read.
	issues, err := c.ListAllOpenIssues(nil)
	if err != nil {
		t.Fatalf("ListAllOpenIssues error = %v", err)
	}
	if len(issues) != 2 || issues[0].Number != 1 || issues[1].Number != 2 {
		t.Fatalf("open set = %#v, want both pages (issues #1 and #2)", issues)
	}
	if len(*calls) != 2 {
		t.Fatalf("runner calls = %d, want 2 (one non-paginated, one paginated)", len(*calls))
	}
}
