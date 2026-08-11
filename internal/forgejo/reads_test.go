package forgejo

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
)

// readReq captures one request the reads fixture server saw. Path is the
// ESCAPED path (RawPath when set) so percent-encoding assertions — the
// slash-in-branch-name case — see what actually went on the wire.
type readReq struct {
	Method string
	Path   string
	Query  url.Values
}

// newReadsClient builds a client against a fixture server whose responses are
// computed per request — pagination tests need page-dependent bodies.
func newReadsClient(t *testing.T, fn func(r *http.Request) (int, string)) (*Client, *[]readReq) {
	t.Helper()
	var seen []readReq
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, readReq{
			Method: r.Method,
			Path:   r.URL.EscapedPath(),
			Query:  r.URL.Query(),
		})
		status, body := fn(r)
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return New(srv.URL, "sekrit"), &seen
}

// staticReads is newReadsClient with one fixed response.
func staticReads(t *testing.T, status int, body string) (*Client, *[]readReq) {
	t.Helper()
	return newReadsClient(t, func(*http.Request) (int, string) { return status, body })
}

// issuesPage builds a JSON array of n issues numbered from `from`, each
// carrying the given label names and the contract's ever-present
// "pull_request": null key of a real issue.
func issuesPage(t *testing.T, from, n int, labels ...string) string {
	t.Helper()
	items := make([]map[string]any, 0, n)
	for i := 0; i < n; i++ {
		ls := make([]map[string]any, 0, len(labels))
		for _, name := range labels {
			ls = append(ls, map[string]any{"id": 1, "name": name, "color": "00aabb"})
		}
		items = append(items, map[string]any{
			"number":       from + i,
			"title":        fmt.Sprintf("issue %d", from+i),
			"body":         "b",
			"state":        "open",
			"labels":       ls,
			"pull_request": nil,
		})
	}
	out, err := json.Marshal(items)
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}
	return string(out)
}

func TestListIssues(t *testing.T) {
	c, seen := staticReads(t, 200, issuesPage(t, 7, 2, "maestro-ready"))
	issues, err := c.ListIssues(context.Background(), "owner/repo", "open", "maestro-ready", true)
	if err != nil {
		t.Fatalf("ListIssues: %v", err)
	}
	if len(issues) != 2 || issues[0].Number != 7 || issues[1].Number != 8 {
		t.Fatalf("issues = %+v", issues)
	}
	if issues[0].Title != "issue 7" || issues[0].Body != "b" || issues[0].State != "open" {
		t.Fatalf("first issue = %+v", issues[0])
	}
	if len(issues[0].Labels) != 1 || issues[0].Labels[0].Name != "maestro-ready" {
		t.Fatalf("labels = %+v", issues[0].Labels)
	}
	if len(*seen) != 1 {
		t.Fatalf("requests = %d, want 1 (a short page ends the loop)", len(*seen))
	}
	req := (*seen)[0]
	if req.Method != http.MethodGet || req.Path != "/repos/owner/repo/issues" {
		t.Fatalf("request = %s %s", req.Method, req.Path)
	}
	for key, want := range map[string]string{
		"type":   "issues",
		"state":  "open",
		"labels": "maestro-ready",
		"limit":  strconv.Itoa(pageSize),
		"page":   "1",
	} {
		if got := req.Query.Get(key); got != want {
			t.Fatalf("query %s = %q, want %q (full query: %v)", key, got, want, req.Query)
		}
	}
}

func TestListIssues_Pagination(t *testing.T) {
	// Page 1 full (== the server clamp), page 2 short → exactly two requests,
	// results concatenated in order.
	c, seen := newReadsClient(t, func(r *http.Request) (int, string) {
		switch r.URL.Query().Get("page") {
		case "1":
			return 200, issuesPage(t, 1, pageSize, "maestro-ready")
		case "2":
			return 200, issuesPage(t, 1+pageSize, 3, "maestro-ready")
		default:
			return 500, `{"message":"unexpected page"}`
		}
	})
	issues, err := c.ListIssues(context.Background(), "owner/repo", "open", "maestro-ready", true)
	if err != nil {
		t.Fatalf("ListIssues: %v", err)
	}
	if len(issues) != pageSize+3 {
		t.Fatalf("issues = %d, want %d", len(issues), pageSize+3)
	}
	if issues[0].Number != 1 || issues[pageSize+2].Number != pageSize+3 {
		t.Fatalf("order broken: first=%d last=%d", issues[0].Number, issues[pageSize+2].Number)
	}
	if len(*seen) != 2 {
		t.Fatalf("requests = %d, want 2", len(*seen))
	}
	if (*seen)[0].Query.Get("page") != "1" || (*seen)[1].Query.Get("page") != "2" {
		t.Fatalf("pages requested: %v then %v", (*seen)[0].Query, (*seen)[1].Query)
	}
}

func TestListIssues_PageCapIsExplicitError(t *testing.T) {
	// Every page comes back full → the pager must stop at maxListPages with a
	// loud error, never return a silently truncated set.
	c, seen := newReadsClient(t, func(*http.Request) (int, string) {
		return 200, issuesPage(t, 1, pageSize)
	})
	_, err := c.ListIssues(context.Background(), "owner/repo", "open", "", true)
	if err == nil {
		t.Fatal("an over-cap listing must error — silent truncation reads as \"the full set\"")
	}
	if !strings.Contains(err.Error(), "refusing to truncate") {
		t.Fatalf("error should name the cap, got: %v", err)
	}
	if len(*seen) != maxListPages {
		t.Fatalf("requests = %d, want exactly %d", len(*seen), maxListPages)
	}
}

// newReadsClientWithTotal is newReadsClient plus an x-total-count response
// header, for the pager's truncation belt.
func newReadsClientWithTotal(t *testing.T, fn func(r *http.Request) (total int, status int, body string)) (*Client, *[]readReq) {
	t.Helper()
	var seen []readReq
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, readReq{
			Method: r.Method,
			Path:   r.URL.EscapedPath(),
			Query:  r.URL.Query(),
		})
		total, status, body := fn(r)
		w.Header().Set("X-Total-Count", strconv.Itoa(total))
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return New(srv.URL, "sekrit"), &seen
}

// pageSize hardcodes THIS instance's max_response_items clamp. On a server
// clamped LOWER, every page comes back short, so without the x-total-count
// belt the short-page stop would end the loop after page one and silently
// truncate an authoritative read (#827 reconcile would stamp still-open
// issues closed).
func TestListIssues_LowerServerClampFailsLoud(t *testing.T) {
	c, seen := newReadsClientWithTotal(t, func(*http.Request) (int, int, string) {
		return 120, 200, issuesPage(t, 1, 25) // clamp 25 < pageSize, 120 total
	})
	_, err := c.ListIssues(context.Background(), "owner/repo", "open", "", true)
	if err == nil {
		t.Fatal("a short page with more entries outstanding must error, not truncate silently")
	}
	if !strings.Contains(err.Error(), "refusing to truncate") {
		t.Fatalf("error should name the truncation, got: %v", err)
	}
	if len(*seen) != 1 {
		t.Fatalf("requests = %d, want 1 (the mismatch is detectable on the first short page)", len(*seen))
	}
}

// A short final page whose accumulated count MATCHES the server total is the
// normal end of a listing — the belt must not false-alarm; and a total that
// shrank mid-scan (an issue closed between pages) must also pass, which is
// why the pager re-reads the header on every page.
func TestListIssues_TotalMatchingShortPagePasses(t *testing.T) {
	c, _ := newReadsClientWithTotal(t, func(r *http.Request) (int, int, string) {
		switch r.URL.Query().Get("page") {
		case "1":
			return pageSize + 3, 200, issuesPage(t, 1, pageSize)
		case "2":
			return pageSize + 3, 200, issuesPage(t, 1+pageSize, 3)
		default:
			return 0, 500, `{"message":"unexpected page"}`
		}
	})
	issues, err := c.ListIssues(context.Background(), "owner/repo", "open", "", true)
	if err != nil {
		t.Fatalf("ListIssues: %v", err)
	}
	if len(issues) != pageSize+3 {
		t.Fatalf("issues = %d, want %d", len(issues), pageSize+3)
	}
}

// Exactly maxListPages×pageSize entries: the last allowed page comes back
// full, but the total says the set is complete — the pager must return it
// instead of demanding one empty page beyond the cap and erroring.
func TestListIssues_TotalReachedOnFullFinalPagePasses(t *testing.T) {
	const exact = maxListPages * pageSize
	c, seen := newReadsClientWithTotal(t, func(r *http.Request) (int, int, string) {
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		if page < 1 || page > maxListPages {
			return 0, 500, `{"message":"page out of range"}`
		}
		return exact, 200, issuesPage(t, (page-1)*pageSize+1, pageSize)
	})
	issues, err := c.ListIssues(context.Background(), "owner/repo", "open", "", true)
	if err != nil {
		t.Fatalf("ListIssues at exactly the cap boundary: %v", err)
	}
	if len(issues) != exact {
		t.Fatalf("issues = %d, want %d", len(issues), exact)
	}
	if len(*seen) != maxListPages {
		t.Fatalf("requests = %d, want exactly %d (no empty page beyond the cap)", len(*seen), maxListPages)
	}
}

// A bounded read must see the same 100-item window as the gh transport's
// per_page=100 single-page calls: with 120 entries on the server it assembles
// two clamped pages, stops at exactly boundedWindow, and the truncation belt
// must NOT fire even though x-total-count says more entries exist — stopping
// at the window is the contract, not truncation.
func TestListIssues_BoundedWindowStopsAtHundred(t *testing.T) {
	c, seen := newReadsClientWithTotal(t, func(r *http.Request) (int, int, string) {
		switch r.URL.Query().Get("page") {
		case "1":
			return 120, 200, issuesPage(t, 1, pageSize)
		case "2":
			return 120, 200, issuesPage(t, 1+pageSize, pageSize)
		default:
			return 0, 500, `{"message":"page beyond the bounded window"}`
		}
	})
	issues, err := c.ListIssues(context.Background(), "owner/repo", "open", "", false)
	if err != nil {
		t.Fatalf("ListIssues: %v", err)
	}
	if len(issues) != boundedWindow {
		t.Fatalf("issues = %d, want the %d-item bounded window", len(issues), boundedWindow)
	}
	if issues[0].Number != 1 || issues[boundedWindow-1].Number != boundedWindow {
		t.Fatalf("window broken: first=%d last=%d", issues[0].Number, issues[boundedWindow-1].Number)
	}
	if len(*seen) != 2 {
		t.Fatalf("requests = %d, want 2 (two clamped pages fill the window)", len(*seen))
	}
}

// A bounded read over a set smaller than the window returns the whole set —
// the short final page ends the loop, and the total-matching count keeps the
// truncation belt quiet.
func TestListIssues_BoundedReturnsAllWhenSetSmallerThanWindow(t *testing.T) {
	c, seen := newReadsClientWithTotal(t, func(r *http.Request) (int, int, string) {
		switch r.URL.Query().Get("page") {
		case "1":
			return 80, 200, issuesPage(t, 1, pageSize)
		case "2":
			return 80, 200, issuesPage(t, 1+pageSize, 80-pageSize)
		default:
			return 0, 500, `{"message":"unexpected page"}`
		}
	})
	issues, err := c.ListIssues(context.Background(), "owner/repo", "open", "", false)
	if err != nil {
		t.Fatalf("ListIssues: %v", err)
	}
	if len(issues) != 80 {
		t.Fatalf("issues = %d, want all 80 (set smaller than the window)", len(issues))
	}
	if len(*seen) != 2 {
		t.Fatalf("requests = %d, want 2", len(*seen))
	}
}

// Genuine truncation must still fail loud in bounded mode: a server clamped
// below pageSize answers a short first page, leaving the read below BOTH the
// window and the server's own total — returning that silently would hand a
// consumer (HasOpenPRForIssue et al.) a fraction of the window it believes it
// scanned.
func TestListIssues_BoundedLowerServerClampFailsLoud(t *testing.T) {
	c, seen := newReadsClientWithTotal(t, func(*http.Request) (int, int, string) {
		return 120, 200, issuesPage(t, 1, 25) // clamp 25 < pageSize, 120 total
	})
	_, err := c.ListIssues(context.Background(), "owner/repo", "open", "", false)
	if err == nil {
		t.Fatal("a short page below both window and total must error, not truncate silently")
	}
	if !strings.Contains(err.Error(), "refusing to truncate") {
		t.Fatalf("error should name the truncation, got: %v", err)
	}
	if len(*seen) != 1 {
		t.Fatalf("requests = %d, want 1", len(*seen))
	}
}

func TestListIssues_RefiltersUnknownLabelClientSide(t *testing.T) {
	// P1 footgun (live-verified): a label name the repo does not have is
	// silently DISCARDED by the server, which then returns the UNFILTERED
	// list. The client-side belt must drop everything that does not carry the
	// requested label by name.
	unfiltered := `[` +
		strings.TrimSuffix(strings.TrimPrefix(issuesPage(t, 1, 1, "Maestro-Ready"), "["), "]") + `,` +
		strings.TrimSuffix(strings.TrimPrefix(issuesPage(t, 2, 1, "enhancement"), "["), "]") + `,` +
		strings.TrimSuffix(strings.TrimPrefix(issuesPage(t, 3, 1), "["), "]") + `]`
	c, _ := staticReads(t, 200, unfiltered)
	issues, err := c.ListIssues(context.Background(), "owner/repo", "open", "maestro-ready", true)
	if err != nil {
		t.Fatalf("ListIssues: %v", err)
	}
	if len(issues) != 1 || issues[0].Number != 1 {
		t.Fatalf("issues = %+v, want only #1 (case-folded label match)", issues)
	}
}

func TestListIssues_DropsPullEntries(t *testing.T) {
	// Real issues carry "pull_request": null; pulls carry a populated object.
	// Only the pointer test tells them apart — key presence never does.
	c, _ := staticReads(t, 200, `[
		{"number":1,"title":"real issue","state":"open","labels":[],"pull_request":null},
		{"number":2,"title":"a pull","state":"open","labels":[],
		 "pull_request":{"merged":false,"merged_at":null,"draft":false,"html_url":"https://x/pulls/2"}}]`)
	issues, err := c.ListIssues(context.Background(), "owner/repo", "open", "", true)
	if err != nil {
		t.Fatalf("ListIssues: %v", err)
	}
	if len(issues) != 1 || issues[0].Number != 1 {
		t.Fatalf("issues = %+v, want the pull entry dropped", issues)
	}
}

func TestListIssues_HTTPError(t *testing.T) {
	c, _ := staticReads(t, 500, `{"message":"boom"}`)
	if _, err := c.ListIssues(context.Background(), "owner/repo", "open", "", true); err == nil {
		t.Fatal("a non-2xx listing must surface as an error, not an empty set")
	}
}

func TestGetIssue(t *testing.T) {
	c, seen := staticReads(t, 200,
		`{"number":7,"title":"t","body":"full body","state":"closed",
		  "labels":[{"id":1,"name":"maestro-ready","color":"00aabb"}],"pull_request":null}`)
	is, err := c.GetIssue(context.Background(), "owner/repo", 7)
	if err != nil {
		t.Fatalf("GetIssue: %v", err)
	}
	if is.Number != 7 || is.Body != "full body" || is.State != "closed" {
		t.Fatalf("issue = %+v", is)
	}
	if is.PullRequest != nil {
		t.Fatalf("a real issue's null pull_request must decode to nil, got %+v", is.PullRequest)
	}
	if len(is.Labels) != 1 || is.Labels[0].Name != "maestro-ready" {
		t.Fatalf("labels = %+v", is.Labels)
	}
	req := (*seen)[0]
	if req.Method != http.MethodGet || req.Path != "/repos/owner/repo/issues/7" {
		t.Fatalf("request = %s %s", req.Method, req.Path)
	}
}

func TestGetIssue_PullIndexCarriesMarker(t *testing.T) {
	// Shared number space: GET /issues/{n} on a pull index answers 200 with
	// an issue shape whose pull_request is populated. The marker must survive
	// so callers can keep their != nil belt.
	c, _ := staticReads(t, 200,
		`{"number":1,"title":"pr","state":"closed","labels":[],
		  "pull_request":{"merged":true,"merged_at":"2026-07-31T21:55:11Z","draft":false}}`)
	is, err := c.GetIssue(context.Background(), "owner/repo", 1)
	if err != nil {
		t.Fatalf("GetIssue: %v", err)
	}
	if is.PullRequest == nil {
		t.Fatal("a pull index must surface a non-nil PullRequest marker")
	}
	if !is.PullRequest.Merged || is.PullRequest.MergedAt == nil {
		t.Fatalf("marker = %+v", is.PullRequest)
	}
}

func TestGetIssue_HTTPError(t *testing.T) {
	c, _ := staticReads(t, 404, `{"message":"not found"}`)
	if _, err := c.GetIssue(context.Background(), "owner/repo", 999); err == nil {
		t.Fatal("a 404 must surface as an error")
	}
}

func TestListIssueComments(t *testing.T) {
	c, seen := staticReads(t, 200, `[
		{"id":101,"body":"first","user":{"login":"alice"},"created_at":"2026-08-01T10:00:00Z","updated_at":"2026-08-01T10:00:00Z"},
		{"id":102,"body":"second","user":{"login":"bob"},"created_at":"2026-08-02T10:00:00Z","updated_at":"2026-08-02T10:00:00Z"}]`)
	comments, err := c.ListIssueComments(context.Background(), "owner/repo", 7)
	if err != nil {
		t.Fatalf("ListIssueComments: %v", err)
	}
	if len(comments) != 2 {
		t.Fatalf("comments = %d, want 2", len(comments))
	}
	want := IssueComment{ID: 101, Body: "first", Author: "alice", CreatedAt: "2026-08-01T10:00:00Z"}
	if comments[0] != want {
		t.Fatalf("comments[0] = %+v, want %+v", comments[0], want)
	}
	req := (*seen)[0]
	if req.Path != "/repos/owner/repo/issues/7/comments" {
		t.Fatalf("path = %s", req.Path)
	}
	// The endpoint has no page/limit params — sending them would be dead
	// weight at best and a behavior change on a future server at worst.
	if req.Query.Get("page") != "" || req.Query.Get("limit") != "" {
		t.Fatalf("comments must not be paginated, query = %v", req.Query)
	}
	if len(*seen) != 1 {
		t.Fatalf("requests = %d, want 1 (single-response endpoint)", len(*seen))
	}
}

func TestIssueLabels(t *testing.T) {
	c, seen := staticReads(t, 200,
		`[{"id":1,"name":"maestro-ready","color":"00aabb"},{"id":2,"name":"enhancement","color":"84b6eb"}]`)
	names, err := c.IssueLabels(context.Background(), "owner/repo", 36)
	if err != nil {
		t.Fatalf("IssueLabels: %v", err)
	}
	if len(names) != 2 || names[0] != "maestro-ready" || names[1] != "enhancement" {
		t.Fatalf("names = %v", names)
	}
	req := (*seen)[0]
	if req.Path != "/repos/owner/repo/issues/36/labels" {
		t.Fatalf("path = %s", req.Path)
	}
	if req.Query.Get("page") != "" || req.Query.Get("limit") != "" {
		t.Fatalf("labels must not be paginated, query = %v", req.Query)
	}
}

func TestListPulls(t *testing.T) {
	// Server order must be preserved verbatim: sort=recentupdate delivers
	// updated-desc and the client must not re-sort.
	c, seen := staticReads(t, 200, `[
		{"number":33,"title":"newer","body":"b","state":"open","draft":true,"mergeable":true,
		 "merged_at":null,"merge_commit_sha":null,
		 "head":{"ref":"feat/a","sha":"59e99c49c27d3e2f73bae1657f07cd2f9a15f926"},"base":{"ref":"main"}},
		{"number":34,"title":"older","body":"","state":"open","draft":false,"mergeable":true,
		 "merged_at":null,"merge_commit_sha":null,
		 "head":{"ref":"feat/b","sha":"dfc9446cb6a16c60075286299cd07d2cb655769a"},"base":{"ref":"main"}}]`)
	pulls, err := c.ListPulls(context.Background(), "owner/repo", "all", true)
	if err != nil {
		t.Fatalf("ListPulls: %v", err)
	}
	if len(pulls) != 2 || pulls[0].Number != 33 || pulls[1].Number != 34 {
		t.Fatalf("pulls = %+v, want server order preserved", pulls)
	}
	first := pulls[0]
	if first.Title != "newer" || !first.Draft || first.HeadRef != "feat/a" || first.BaseRef != "main" {
		t.Fatalf("first pull = %+v", first)
	}
	if first.HeadSHA != "59e99c49c27d3e2f73bae1657f07cd2f9a15f926" {
		t.Fatalf("head sha = %q", first.HeadSHA)
	}
	if first.MergedAt != nil || first.MergeCommitSHA != "" {
		t.Fatalf("unmerged pull must carry nil MergedAt and empty MergeCommitSHA, got %+v", first)
	}
	req := (*seen)[0]
	if req.Method != http.MethodGet || req.Path != "/repos/owner/repo/pulls" {
		t.Fatalf("request = %s %s", req.Method, req.Path)
	}
	for key, want := range map[string]string{
		"sort":  "recentupdate", // default server order is index-desc, NOT updated-desc
		"state": "all",
		"limit": strconv.Itoa(pageSize),
		"page":  "1",
	} {
		if got := req.Query.Get(key); got != want {
			t.Fatalf("query %s = %q, want %q (full query: %v)", key, got, want, req.Query)
		}
	}
}

func TestListPulls_HTTPError(t *testing.T) {
	c, _ := staticReads(t, 502, `bad gateway`)
	if _, err := c.ListPulls(context.Background(), "owner/repo", "open", true); err == nil {
		t.Fatal("a non-2xx listing must surface as an error")
	}
}

func TestGetPull_Merged(t *testing.T) {
	// The live-verified merged shape: state stays lowercase "closed",
	// merged_at is RFC3339, merge_commit_sha is 40-hex.
	c, seen := staticReads(t, 200,
		`{"number":2,"title":"gate","body":"pr body","state":"closed","draft":false,
		  "mergeable":true,"merged":true,"merged_at":"2026-07-31T21:55:11Z",
		  "merge_commit_sha":"dfc9446cb6a16c60075286299cd07d2cb655769a",
		  "head":{"ref":"canary/llm-review-e2e","sha":"59e99c49c27d3e2f73bae1657f07cd2f9a15f926"},
		  "base":{"ref":"main"}}`)
	p, err := c.GetPull(context.Background(), "owner/repo", 2)
	if err != nil {
		t.Fatalf("GetPull: %v", err)
	}
	if p.Number != 2 || p.Title != "gate" || p.Body != "pr body" {
		t.Fatalf("pull = %+v", p)
	}
	if p.State != "closed" {
		t.Fatalf("state = %q, want lowercase verbatim (the gh layer upper-cases)", p.State)
	}
	if p.Mergeable == nil || !*p.Mergeable {
		t.Fatalf("mergeable = %v", p.Mergeable)
	}
	if p.MergedAt == nil || *p.MergedAt != "2026-07-31T21:55:11Z" {
		t.Fatalf("merged_at = %v", p.MergedAt)
	}
	if len(p.MergeCommitSHA) != 40 {
		t.Fatalf("merge_commit_sha = %q, want 40-hex (PRMergeInfo asserts on it)", p.MergeCommitSHA)
	}
	if p.HeadRef != "canary/llm-review-e2e" || p.BaseRef != "main" {
		t.Fatalf("refs = %q / %q — slashes must survive verbatim", p.HeadRef, p.BaseRef)
	}
	req := (*seen)[0]
	if req.Path != "/repos/owner/repo/pulls/2" {
		t.Fatalf("path = %s", req.Path)
	}
}

func TestGetPull_Unmerged(t *testing.T) {
	c, _ := staticReads(t, 200,
		`{"number":1,"title":"open pr","body":null,"state":"open","draft":false,
		  "mergeable":true,"merged":false,"merged_at":null,"merge_commit_sha":null,
		  "head":{"ref":"feat/x","sha":"59e99c49c27d3e2f73bae1657f07cd2f9a15f926"},"base":{"ref":"main"}}`)
	p, err := c.GetPull(context.Background(), "owner/repo", 1)
	if err != nil {
		t.Fatalf("GetPull: %v", err)
	}
	if p.MergedAt != nil {
		t.Fatalf("merged_at = %v, want nil on an unmerged pull", p.MergedAt)
	}
	if p.MergeCommitSHA != "" {
		t.Fatalf("merge_commit_sha = %q, want empty on an unmerged pull", p.MergeCommitSHA)
	}
	if p.Body != "" {
		t.Fatalf("a null body must decode to empty, got %q", p.Body)
	}
}

func TestGetPull_HTTPError(t *testing.T) {
	// A real-issue index answers 404 on /pulls/{n} (shared number space).
	c, _ := staticReads(t, 404, `{"message":"not found"}`)
	if _, err := c.GetPull(context.Background(), "owner/repo", 36); err == nil {
		t.Fatal("a 404 must surface as an error")
	}
}

func TestDefaultBranch(t *testing.T) {
	c, seen := staticReads(t, 200, `{"name":"apertune","default_branch":"main"}`)
	branch, err := c.DefaultBranch(context.Background(), "owner/repo")
	if err != nil {
		t.Fatalf("DefaultBranch: %v", err)
	}
	if branch != "main" {
		t.Fatalf("branch = %q", branch)
	}
	req := (*seen)[0]
	if req.Method != http.MethodGet || req.Path != "/repos/owner/repo" {
		t.Fatalf("request = %s %s", req.Method, req.Path)
	}
}

func TestDefaultBranch_EmptyIsError(t *testing.T) {
	c, _ := staticReads(t, 200, `{"name":"empty-repo"}`)
	if _, err := c.DefaultBranch(context.Background(), "owner/repo"); err == nil {
		t.Fatal("a missing default branch must error — base-branch logic anchors to it")
	}
}

func TestBranchHeadSHA(t *testing.T) {
	// The head SHA lives in commit.id (Branch.commit is a PayloadCommit); a
	// top-level "sha" decoy guards against regressing to the commits-list
	// shape.
	c, seen := staticReads(t, 200,
		`{"name":"canary/llm-review-e2e","sha":"decoy",
		  "commit":{"id":"59e99c49c27d3e2f73bae1657f07cd2f9a15f926","message":"m"}}`)
	sha, err := c.BranchHeadSHA(context.Background(), "owner/repo", "canary/llm-review-e2e")
	if err != nil {
		t.Fatalf("BranchHeadSHA: %v", err)
	}
	if sha != "59e99c49c27d3e2f73bae1657f07cd2f9a15f926" {
		t.Fatalf("sha = %q", sha)
	}
	req := (*seen)[0]
	if req.Path != "/repos/owner/repo/branches/canary%2Fllm-review-e2e" {
		t.Fatalf("path = %s, want the slash percent-escaped", req.Path)
	}
}

func TestBranchHeadSHA_EmptySHAIsError(t *testing.T) {
	c, _ := staticReads(t, 200, `{"name":"b","commit":{"id":"  "}}`)
	if _, err := c.BranchHeadSHA(context.Background(), "owner/repo", "b"); err == nil {
		t.Fatal("an empty head sha must be rejected")
	}
}

func TestBranchHeadSHA_HTTPError(t *testing.T) {
	c, _ := staticReads(t, 404, `{"message":"branch does not exist"}`)
	if _, err := c.BranchHeadSHA(context.Background(), "owner/repo", "gone"); err == nil {
		t.Fatal("a missing branch must surface as an error")
	}
}

func TestListPullCommits(t *testing.T) {
	// commit.message carries a trailing "\n" on the wire (live-verified);
	// it must survive verbatim — headline extraction is the gh layer's job.
	c, seen := staticReads(t, 200,
		`[{"sha":"59e99c49c27d3e2f73bae1657f07cd2f9a15f926","commit":{"message":"feat: first\n\nbody\n"}},
		  {"sha":"dfc9446cb6a16c60075286299cd07d2cb655769a","commit":{"message":"fix: second\n"}}]`)
	commits, err := c.ListPullCommits(context.Background(), "owner/repo", 1, true)
	if err != nil {
		t.Fatalf("ListPullCommits: %v", err)
	}
	if len(commits) != 2 {
		t.Fatalf("commits = %+v", commits)
	}
	if commits[0].SHA != "59e99c49c27d3e2f73bae1657f07cd2f9a15f926" || commits[0].Message != "feat: first\n\nbody\n" {
		t.Fatalf("first commit = %+v, want the message verbatim incl. trailing newline", commits[0])
	}
	req := (*seen)[0]
	if req.Path != "/repos/owner/repo/pulls/1/commits" {
		t.Fatalf("path = %s", req.Path)
	}
	if req.Query.Get("limit") != strconv.Itoa(pageSize) || req.Query.Get("page") != "1" {
		t.Fatalf("commits list must be paginated, query = %v", req.Query)
	}
}

func TestListPullCommits_Pagination(t *testing.T) {
	commit := `{"sha":"%040d","commit":{"message":"c %d\n"}}`
	page := func(from, n int) string {
		items := make([]string, 0, n)
		for i := 0; i < n; i++ {
			items = append(items, fmt.Sprintf(commit, from+i, from+i))
		}
		return "[" + strings.Join(items, ",") + "]"
	}
	c, seen := newReadsClient(t, func(r *http.Request) (int, string) {
		switch r.URL.Query().Get("page") {
		case "1":
			return 200, page(1, pageSize)
		case "2":
			return 200, page(1+pageSize, 2)
		default:
			return 200, "[]"
		}
	})
	commits, err := c.ListPullCommits(context.Background(), "owner/repo", 1, true)
	if err != nil {
		t.Fatalf("ListPullCommits: %v", err)
	}
	if len(commits) != pageSize+2 {
		t.Fatalf("commits = %d, want %d", len(commits), pageSize+2)
	}
	if len(*seen) != 2 {
		t.Fatalf("requests = %d, want 2", len(*seen))
	}
}

func TestListPullCommits_HTTPError(t *testing.T) {
	c, _ := staticReads(t, 500, `{"message":"boom"}`)
	if _, err := c.ListPullCommits(context.Background(), "owner/repo", 1, true); err == nil {
		t.Fatal("a 500 must surface as an error")
	}
}

func TestListPullFiles(t *testing.T) {
	c, seen := staticReads(t, 200,
		`[{"filename":"cmd/main.go","status":"modified","additions":1,"deletions":0},
		  {"filename":"web/app.css","status":"added","additions":9,"deletions":0}]`)
	files, err := c.ListPullFiles(context.Background(), "owner/repo", 1, true)
	if err != nil {
		t.Fatalf("ListPullFiles: %v", err)
	}
	if len(files) != 2 || files[0] != "cmd/main.go" || files[1] != "web/app.css" {
		t.Fatalf("files = %v", files)
	}
	req := (*seen)[0]
	if req.Path != "/repos/owner/repo/pulls/1/files" {
		t.Fatalf("path = %s", req.Path)
	}
	if req.Query.Get("limit") != strconv.Itoa(pageSize) || req.Query.Get("page") != "1" {
		t.Fatalf("files list must be paginated, query = %v", req.Query)
	}
}

// The bounded window applies to every paged list read: two full clamped pages
// fill it, a third is never requested.
func TestListPullFiles_BoundedWindowWhenAllPagesFalse(t *testing.T) {
	filesPage := func(from, n int) string {
		items := make([]string, 0, n)
		for i := 0; i < n; i++ {
			items = append(items, fmt.Sprintf(`{"filename":"f%d.go"}`, from+i))
		}
		return "[" + strings.Join(items, ",") + "]"
	}
	c, seen := newReadsClient(t, func(r *http.Request) (int, string) {
		switch r.URL.Query().Get("page") {
		case "1":
			return 200, filesPage(0, pageSize)
		case "2":
			return 200, filesPage(pageSize, pageSize)
		default:
			return 500, `{"message":"page beyond the bounded window"}`
		}
	})
	files, err := c.ListPullFiles(context.Background(), "owner/repo", 1, false)
	if err != nil {
		t.Fatalf("ListPullFiles: %v", err)
	}
	if len(files) != boundedWindow || len(*seen) != 2 {
		t.Fatalf("files = %d requests = %d, want the %d-item window from exactly 2 pages", len(files), len(*seen), boundedWindow)
	}
}

func TestListPullFiles_HTTPError(t *testing.T) {
	c, _ := staticReads(t, 404, `{"message":"not found"}`)
	if _, err := c.ListPullFiles(context.Background(), "owner/repo", 9, true); err == nil {
		t.Fatal("a 404 must surface as an error")
	}
}

func TestReads_ContextCanceled(t *testing.T) {
	c, seen := staticReads(t, 200, `[]`)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.ListIssues(ctx, "owner/repo", "open", "", true); err == nil {
		t.Fatal("ListIssues must fail on a canceled context")
	}
	if _, err := c.GetPull(ctx, "owner/repo", 1); err == nil {
		t.Fatal("GetPull must fail on a canceled context")
	}
	if len(*seen) != 0 {
		t.Fatalf("a canceled context must not reach the API, saw %v", *seen)
	}
}
