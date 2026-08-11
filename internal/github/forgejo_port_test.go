package github

// End-to-end forgejo-mode tests for the ported core-read funnels (#1172 M2).
// Each test constructs the REAL stack — config.ForgeConfig → github.New →
// internal/forgejo.Client → httptest server — with contract-verified Forgejo
// JSON fixtures (live-verified against Forgejo 16.0.1 on 2026-08-11), and
// arms the gh transport so any leak to the gh CLI fails the test: the repo is
// mirrored on github.com, so a leaked read would consult the GitHub mirror.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/befeast/maestro/internal/config"
)

const fjTestRepo = "BeFeast/apertune"

// fjSeenReq is one request the fixture server observed. Path is the ESCAPED
// path so percent-encoding assertions (slash-in-branch-name) see the wire.
type fjSeenReq struct {
	Method string
	Path   string
	Query  url.Values
	Auth   string
}

// newForgejoPortClient builds a forgejo-mode github.Client whose transport
// points at an httptest fixture server, plus the request log. The gh seam is
// armed via stubGHNeverCalled so every test doubles as a no-gh-leak check.
// The M1 config contract puts the API under BaseURL + "/api/v1", so handler
// paths in fn arrive WITHOUT that prefix (it is stripped here after being
// asserted).
func newForgejoPortClient(t *testing.T, fn func(r *http.Request) (int, string)) (*Client, *[]fjSeenReq, *atomic.Int64) {
	t.Helper()
	ghCalls := stubGHNeverCalled(t)
	var seen []fjSeenReq
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.EscapedPath(), "/api/v1/") {
			t.Errorf("request outside the API root: %s", r.URL.EscapedPath())
			w.WriteHeader(http.StatusNotFound)
			return
		}
		seen = append(seen, fjSeenReq{
			Method: r.Method,
			Path:   strings.TrimPrefix(r.URL.EscapedPath(), "/api/v1"),
			Query:  r.URL.Query(),
			Auth:   r.Header.Get("Authorization"),
		})
		status, body := fn(r)
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)

	t.Setenv("TEST_FORGEJO_TOKEN", "test-token")
	c := New(fjTestRepo, config.ForgeConfig{
		Kind:     "forgejo",
		BaseURL:  srv.URL,
		TokenEnv: "TEST_FORGEJO_TOKEN",
	})
	if c.fj == nil || c.forgeErr != nil {
		t.Fatalf("client not in forgejo transport state: fj=%v forgeErr=%v", c.fj, c.forgeErr)
	}
	return c, &seen, ghCalls
}

// fjRoute serves fixed bodies keyed by the request path relative to the API
// root ("/repos/..."). Unrouted paths fail the test.
func fjRoute(t *testing.T, routes map[string]string) func(r *http.Request) (int, string) {
	t.Helper()
	return func(r *http.Request) (int, string) {
		path := strings.TrimPrefix(r.URL.EscapedPath(), "/api/v1")
		if body, ok := routes[path]; ok {
			return 200, body
		}
		t.Errorf("unrouted fixture path: %s", path)
		return 404, `{"message":"no fixture"}`
	}
}

// --- issues ------------------------------------------------------------------

func TestForgejoPortListOpenIssues(t *testing.T) {
	issuesPath := "/repos/" + fjTestRepo + "/issues"
	c, seen, ghCalls := newForgejoPortClient(t, fjRoute(t, map[string]string{
		// Contract shape: real issues carry "pull_request": null (the KEY is
		// always present); pull entries carry a populated object. The entry
		// without the requested label simulates the P1 footgun — an unknown/
		// mismatched label name makes the server return the UNFILTERED list.
		issuesPath: `[
			{"number":36,"title":"ready one","body":"b36","state":"open",
			 "labels":[{"id":1,"name":"maestro-ready","color":"00aabb"}],"pull_request":null},
			{"number":34,"title":"a pull in the shared index space","body":"","state":"open",
			 "labels":[{"id":1,"name":"maestro-ready","color":"00aabb"}],
			 "pull_request":{"merged":false,"merged_at":null,"draft":true,"html_url":"x"}},
			{"number":17,"title":"unlabeled leak","body":"","state":"open",
			 "labels":[{"id":2,"name":"enhancement","color":"cc0000"}],"pull_request":null},
			{"number":13,"title":"ready two","body":null,"state":"open",
			 "labels":[{"id":1,"name":"maestro-ready","color":"00aabb"}],"pull_request":null}
		]`,
	}))

	issues, err := c.ListOpenIssues([]string{"maestro-ready"})
	if err != nil {
		t.Fatalf("ListOpenIssues: %v", err)
	}
	if len(issues) != 2 || issues[0].Number != 36 || issues[1].Number != 13 {
		t.Fatalf("issues = %+v, want the pull entry AND the unlabeled leak dropped, order preserved", issues)
	}
	if issues[0].Title != "ready one" || issues[0].Body != "b36" || issues[0].State != "open" {
		t.Fatalf("issue 36 mapped wrong: %+v", issues[0])
	}
	if issues[1].Body != "" {
		t.Fatalf("null body must map to empty, got %q", issues[1].Body)
	}
	if len(issues[0].Labels) != 1 || issues[0].Labels[0].Name != "maestro-ready" {
		t.Fatalf("labels = %+v", issues[0].Labels)
	}

	if len(*seen) != 1 {
		t.Fatalf("requests = %d, want 1", len(*seen))
	}
	req := (*seen)[0]
	if req.Method != http.MethodGet || req.Path != issuesPath {
		t.Fatalf("request = %s %s", req.Method, req.Path)
	}
	for key, want := range map[string]string{
		"type":   "issues",
		"state":  "open",
		"labels": "maestro-ready",
	} {
		if got := req.Query.Get(key); got != want {
			t.Fatalf("query %s = %q, want %q (full: %v)", key, got, want, req.Query)
		}
	}
	if req.Auth != "token test-token" {
		t.Fatalf("Authorization = %q, want the Forgejo token scheme", req.Auth)
	}
	if n := ghCalls.Load(); n != 0 {
		t.Fatalf("gh runner invoked %d time(s), want 0", n)
	}
}

func TestForgejoPortListAllOpenIssuesPaginates(t *testing.T) {
	page := func(from, n int) string {
		items := make([]map[string]any, 0, n)
		for i := 0; i < n; i++ {
			items = append(items, map[string]any{
				"number": from + i, "title": fmt.Sprintf("i%d", from+i), "body": "b",
				"state": "open", "labels": []any{}, "pull_request": nil,
			})
		}
		out, err := json.Marshal(items)
		if err != nil {
			t.Fatalf("marshal fixture: %v", err)
		}
		return string(out)
	}
	c, seen, _ := newForgejoPortClient(t, func(r *http.Request) (int, string) {
		switch r.URL.Query().Get("page") {
		case "1":
			return 200, page(1, 50) // the live-verified server clamp
		case "2":
			return 200, page(51, 3)
		default:
			t.Errorf("unexpected page %q", r.URL.Query().Get("page"))
			return 200, "[]"
		}
	})

	issues, err := c.ListAllOpenIssues(nil)
	if err != nil {
		t.Fatalf("ListAllOpenIssues: %v", err)
	}
	if len(issues) != 53 {
		t.Fatalf("issues = %d, want 53 across two pages", len(issues))
	}
	if len(*seen) != 2 {
		t.Fatalf("requests = %d, want 2 (page 2 is short and ends the loop)", len(*seen))
	}
	// The bounded variant assembles the gh-parity 100-item window from
	// clamped pages: page 1 is full so page 2 is fetched, the short page ends
	// the read below the window, and nothing beyond is requested.
	*seen = nil
	bounded, err := c.ListOpenIssues(nil)
	if err != nil {
		t.Fatalf("ListOpenIssues: %v", err)
	}
	if len(bounded) != 53 {
		t.Fatalf("bounded issues = %d, want all 53 (set smaller than the 100-item window)", len(bounded))
	}
	if len(*seen) != 2 {
		t.Fatalf("bounded read made %d requests, want 2 (full page then short page)", len(*seen))
	}
}

func TestForgejoPortGetIssueAndDerived(t *testing.T) {
	issuePath := "/repos/" + fjTestRepo + "/issues/7"
	c, _, ghCalls := newForgejoPortClient(t, fjRoute(t, map[string]string{
		issuePath: `{"number":7,"title":"closed one","body":"the body","state":"closed",
			"labels":[{"id":3,"name":"bug","color":"ee0000"}],"pull_request":null}`,
	}))

	issue, err := c.GetIssue(7)
	if err != nil {
		t.Fatalf("GetIssue: %v", err)
	}
	if issue.Number != 7 || issue.Title != "closed one" || issue.Body != "the body" || issue.State != "closed" {
		t.Fatalf("issue = %+v", issue)
	}
	if len(issue.Labels) != 1 || issue.Labels[0].Name != "bug" {
		t.Fatalf("labels = %+v", issue.Labels)
	}

	body, err := c.IssueBody(7)
	if err != nil || body != "the body" {
		t.Fatalf("IssueBody = %q, %v", body, err)
	}
	closed, err := c.IsIssueClosed(7)
	if err != nil || !closed {
		t.Fatalf("IsIssueClosed = %v, %v; want true", closed, err)
	}
	if n := ghCalls.Load(); n != 0 {
		t.Fatalf("gh runner invoked %d time(s), want 0", n)
	}
}

// --- pulls ---------------------------------------------------------------------

func TestForgejoPortListOpenPRsMapping(t *testing.T) {
	pullsPath := "/repos/" + fjTestRepo + "/pulls"
	c, seen, _ := newForgejoPortClient(t, fjRoute(t, map[string]string{
		pullsPath: `[
			{"number":5,"title":"draft pr","body":"wip","state":"open","draft":true,
			 "mergeable":true,"merged":false,"merged_at":null,"merge_commit_sha":null,
			 "head":{"ref":"feat/one","sha":"59e99c49c27d3e2f73bae1657f07cd2f9a15f926"},
			 "base":{"ref":"main"}},
			{"number":4,"title":"ready pr","body":null,"state":"open","draft":false,
			 "mergeable":true,"merged":false,"merged_at":null,"merge_commit_sha":null,
			 "head":{"ref":"feat/two","sha":"dfc9446cb6a16c60075286299cd07d2cb655769a"},
			 "base":{"ref":"main"}}
		]`,
	}))

	prs, err := c.ListOpenPRs()
	if err != nil {
		t.Fatalf("ListOpenPRs: %v", err)
	}
	if len(prs) != 2 {
		t.Fatalf("prs = %+v", prs)
	}
	// restPull.pr() upper-cases the wire's lowercase state.
	if prs[0].State != "OPEN" {
		t.Fatalf("state = %q, want OPEN (pr() upper-cases the lowercase wire state)", prs[0].State)
	}
	if !prs[0].IsDraft || prs[1].IsDraft {
		t.Fatalf("draft mapping wrong: %+v", prs)
	}
	if prs[0].MergedAt != "" {
		t.Fatalf("unmerged MergedAt = %q, want empty", prs[0].MergedAt)
	}
	if prs[0].HeadRefName != "feat/one" || prs[1].Body != "" {
		t.Fatalf("mapping wrong: %+v", prs)
	}

	req := (*seen)[0]
	if req.Query.Get("state") != "open" || req.Query.Get("sort") != "recentupdate" {
		t.Fatalf("query = %v, want state=open plus the explicit recentupdate sort", req.Query)
	}
}

func TestForgejoPortMergedPRNumberForIssue(t *testing.T) {
	pullsPath := "/repos/" + fjTestRepo + "/pulls"
	c, seen, _ := newForgejoPortClient(t, fjRoute(t, map[string]string{
		// Newest-updated-first server order: an unmerged closed PR, then the
		// merged one that closes #12.
		pullsPath: `[
			{"number":9,"title":"abandoned","body":"Closes #12","state":"closed","draft":false,
			 "mergeable":true,"merged":false,"merged_at":null,"merge_commit_sha":null,
			 "head":{"ref":"feat/old","sha":"59e99c49c27d3e2f73bae1657f07cd2f9a15f926"},
			 "base":{"ref":"main"}},
			{"number":8,"title":"landed","body":"Closes #12","state":"closed","draft":false,
			 "mergeable":true,"merged":true,"merged_at":"2026-07-31T21:55:11Z",
			 "merge_commit_sha":"dfc9446cb6a16c60075286299cd07d2cb655769a",
			 "head":{"ref":"feat/landed","sha":"aaaa446cb6a16c60075286299cd07d2cb655769a"},
			 "base":{"ref":"main"}}
		]`,
	}))

	n, err := c.MergedPRNumberForIssue(12)
	if err != nil {
		t.Fatalf("MergedPRNumberForIssue: %v", err)
	}
	if n != 8 {
		t.Fatalf("merged PR = %d, want 8 (the unmerged closed PR must not count)", n)
	}
	req := (*seen)[0]
	if req.Query.Get("state") != "closed" || req.Query.Get("sort") != "recentupdate" {
		t.Fatalf("query = %v, want state=closed plus recentupdate", req.Query)
	}
}

func TestForgejoPortGetRESTPullDerivedMerged(t *testing.T) {
	pullPath := "/repos/" + fjTestRepo + "/pulls/1"
	const mergeSHA = "dfc9446cb6a16c60075286299cd07d2cb655769a"
	const headSHA = "59e99c49c27d3e2f73bae1657f07cd2f9a15f926"
	c, _, ghCalls := newForgejoPortClient(t, fjRoute(t, map[string]string{
		// The live merged-PR shape: state closed (lowercase), mergeable STILL
		// true (Forgejo's bool is not GitHub's tri-state), slash ref verbatim.
		pullPath: `{"number":1,"title":"merged pr","body":"done","state":"closed","draft":false,
			"mergeable":true,"merged":true,"merged_at":"2026-07-31T21:55:11Z",
			"merge_commit_sha":"` + mergeSHA + `",
			"head":{"ref":"canary/llm-review-e2e","sha":"` + headSHA + `"},
			"base":{"ref":"main"}}`,
	}))

	pr, err := c.PRDetails(1)
	if err != nil {
		t.Fatalf("PRDetails: %v", err)
	}
	if pr.State != "CLOSED" || pr.HeadRefName != "canary/llm-review-e2e" || pr.MergedAt != "2026-07-31T21:55:11Z" {
		t.Fatalf("PRDetails = %+v", pr)
	}

	sha, err := c.PRHeadSHA(1)
	if err != nil || sha != headSHA {
		t.Fatalf("PRHeadSHA = %q, %v", sha, err)
	}

	merged, err := c.IsPRMerged(1)
	if err != nil || !merged {
		t.Fatalf("IsPRMerged = %v, %v; want true", merged, err)
	}

	mergeable, err := c.PRMergeable(1)
	if err != nil || mergeable != "MERGEABLE" {
		t.Fatalf("PRMergeable = %q, %v (the wire bool feeds mergeableFromRESTPull)", mergeable, err)
	}

	info, err := c.PRMergeInfo(1)
	if err != nil {
		t.Fatalf("PRMergeInfo: %v", err)
	}
	if info.SHA != mergeSHA || info.HeadSHA != headSHA {
		t.Fatalf("PRMergeInfo = %+v", info)
	}
	if info.MergedAt.UTC().Format("2006-01-02T15:04:05Z") != "2026-07-31T21:55:11Z" {
		t.Fatalf("MergedAt = %v", info.MergedAt)
	}
	if got, err := c.PRMergeCommitSHA(1); err != nil || got != mergeSHA {
		t.Fatalf("PRMergeCommitSHA = %q, %v", got, err)
	}
	if n := ghCalls.Load(); n != 0 {
		t.Fatalf("gh runner invoked %d time(s), want 0", n)
	}
}

func TestForgejoPortGetRESTPullDerivedUnmerged(t *testing.T) {
	pullPath := "/repos/" + fjTestRepo + "/pulls/2"
	c, _, _ := newForgejoPortClient(t, fjRoute(t, map[string]string{
		pullPath: `{"number":2,"title":"open pr","body":null,"state":"open","draft":false,
			"mergeable":false,"merged":false,"merged_at":null,"merge_commit_sha":null,
			"head":{"ref":"feat/x","sha":"59e99c49c27d3e2f73bae1657f07cd2f9a15f926"},
			"base":{"ref":"main"}}`,
	}))

	merged, err := c.IsPRMerged(2)
	if err != nil || merged {
		t.Fatalf("IsPRMerged = %v, %v; want false", merged, err)
	}
	if _, err := c.PRMergeInfo(2); err == nil || !strings.Contains(err.Error(), "not merged") {
		t.Fatalf("PRMergeInfo on an unmerged PR = %v, want the not-merged error", err)
	}
	mergeable, err := c.PRMergeable(2)
	if err != nil || mergeable != "CONFLICTING" {
		t.Fatalf("PRMergeable = %q, %v; want CONFLICTING from mergeable=false", mergeable, err)
	}
	pr, err := c.PRDetails(2)
	if err != nil || pr.MergedAt != "" || pr.Body != "" {
		t.Fatalf("PRDetails = %+v, %v (null body and null merged_at must map to empty)", pr, err)
	}
}

// Forgejo's wire mergeable is the server-side Mergeable() predicate, forced
// false on EVERY draft/WIP pull regardless of conflicts (live-verified: 41/41
// draft pulls on a Forgejo 16.0 instance report false). GitHub's bool stays
// true on a clean draft, so mapping the draft false through would flip every
// draft PR to "CONFLICTING" and openPRNeedsRepair would authorize repair on it
// each cycle. The port drops the contaminated false to nil → "UNKNOWN".
func TestForgejoPortDraftMergeableFalseIsUnknown(t *testing.T) {
	pullPath := "/repos/" + fjTestRepo + "/pulls/3"
	c, _, ghCalls := newForgejoPortClient(t, fjRoute(t, map[string]string{
		pullPath: `{"number":3,"title":"draft pr","body":"wip","state":"open","draft":true,
			"mergeable":false,"merged":false,"merged_at":null,"merge_commit_sha":null,
			"head":{"ref":"feat/wip","sha":"59e99c49c27d3e2f73bae1657f07cd2f9a15f926"},
			"base":{"ref":"main"}}`,
	}))

	mergeable, err := c.PRMergeable(3)
	if err != nil || mergeable != "UNKNOWN" {
		t.Fatalf("PRMergeable on a draft = %q, %v; want UNKNOWN (draft false carries no conflict information)", mergeable, err)
	}
	pr, err := c.PRDetails(3)
	if err != nil || !pr.IsDraft {
		t.Fatalf("PRDetails = %+v, %v; draft flag must survive the mapping", pr, err)
	}
	if n := ghCalls.Load(); n != 0 {
		t.Fatalf("gh runner invoked %d time(s), want 0", n)
	}
}

func TestForgejoPortLatestMergedPRGenerationsBaseFilter(t *testing.T) {
	repoPath := "/repos/" + fjTestRepo
	pullsPath := "/repos/" + fjTestRepo + "/pulls"
	c, seen, _ := newForgejoPortClient(t, fjRoute(t, map[string]string{
		repoPath: `{"name":"apertune","default_branch":"main"}`,
		pullsPath: `[
			{"number":30,"title":"tie a","body":"","state":"closed","draft":false,
			 "mergeable":true,"merged":true,"merged_at":"2026-08-02T10:00:00Z",
			 "merge_commit_sha":"aaaa446cb6a16c60075286299cd07d2cb655769a",
			 "head":{"ref":"feat/a","sha":"59e99c49c27d3e2f73bae1657f07cd2f9a15f926"},"base":{"ref":"main"}},
			{"number":31,"title":"tie b","body":"","state":"closed","draft":false,
			 "mergeable":true,"merged":true,"merged_at":"2026-08-02T10:00:00Z",
			 "merge_commit_sha":"bbbb446cb6a16c60075286299cd07d2cb655769a",
			 "head":{"ref":"feat/b","sha":"59e99c49c27d3e2f73bae1657f07cd2f9a15f926"},"base":{"ref":"main"}},
			{"number":32,"title":"newer but wrong base","body":"","state":"closed","draft":false,
			 "mergeable":true,"merged":true,"merged_at":"2026-08-03T10:00:00Z",
			 "merge_commit_sha":"cccc446cb6a16c60075286299cd07d2cb655769a",
			 "head":{"ref":"feat/c","sha":"59e99c49c27d3e2f73bae1657f07cd2f9a15f926"},"base":{"ref":"release/v1"}},
			{"number":33,"title":"older","body":"","state":"closed","draft":false,
			 "mergeable":true,"merged":true,"merged_at":"2026-08-01T10:00:00Z",
			 "merge_commit_sha":"dddd446cb6a16c60075286299cd07d2cb655769a",
			 "head":{"ref":"feat/d","sha":"59e99c49c27d3e2f73bae1657f07cd2f9a15f926"},"base":{"ref":"main"}},
			{"number":34,"title":"closed unmerged","body":"","state":"closed","draft":false,
			 "mergeable":true,"merged":false,"merged_at":null,"merge_commit_sha":null,
			 "head":{"ref":"feat/e","sha":"59e99c49c27d3e2f73bae1657f07cd2f9a15f926"},"base":{"ref":"main"}}
		]`,
	}))

	infos, err := c.LatestMergedPRGenerations(t.Context())
	if err != nil {
		t.Fatalf("LatestMergedPRGenerations: %v", err)
	}
	if len(infos) != 2 {
		t.Fatalf("generations = %+v, want exactly the two main-based PRs tied at the latest second", infos)
	}
	got := map[string]bool{infos[0].SHA: true, infos[1].SHA: true}
	if !got["aaaa446cb6a16c60075286299cd07d2cb655769a"] || !got["bbbb446cb6a16c60075286299cd07d2cb655769a"] {
		t.Fatalf("wrong tie set: %+v (release/v1 must be filtered client-side on Base.Ref)", infos)
	}
	// Two reads: repo (default branch) then the closed-pulls page.
	if len(*seen) != 2 || (*seen)[0].Path != repoPath || (*seen)[1].Path != pullsPath {
		t.Fatalf("requests = %+v", *seen)
	}
}

// --- sub-resources ------------------------------------------------------------

func TestForgejoPortListIssueComments(t *testing.T) {
	commentsPath := "/repos/" + fjTestRepo + "/issues/42/comments"
	c, seen, _ := newForgejoPortClient(t, fjRoute(t, map[string]string{
		commentsPath: `[
			{"id":101,"body":"first","user":{"login":"oleg"},"created_at":"2026-08-01T10:00:00Z","updated_at":"2026-08-01T10:00:00Z"},
			{"id":102,"body":"@maestro groom","user":{"login":"smoke-bot"},"created_at":"2026-08-02T11:30:00Z","updated_at":"2026-08-02T11:30:00Z"}
		]`,
	}))

	comments, err := c.ListIssueComments(42)
	if err != nil {
		t.Fatalf("ListIssueComments: %v", err)
	}
	if len(comments) != 2 {
		t.Fatalf("comments = %+v", comments)
	}
	want := IssueComment{ID: 102, Body: "@maestro groom", Author: "smoke-bot", CreatedAt: "2026-08-02T11:30:00Z"}
	if comments[1] != want {
		t.Fatalf("comment = %+v, want %+v", comments[1], want)
	}
	// The endpoint has NO page/limit params on Forgejo — one bare request.
	req := (*seen)[0]
	if len(*seen) != 1 || req.Query.Get("limit") != "" || req.Query.Get("page") != "" {
		t.Fatalf("requests = %+v, want one unpaged read", *seen)
	}
}

func TestForgejoPortPRLabels(t *testing.T) {
	labelsPath := "/repos/" + fjTestRepo + "/issues/36/labels"
	c, _, _ := newForgejoPortClient(t, fjRoute(t, map[string]string{
		labelsPath: `[{"id":1,"name":"maestro-ready","color":"00aabb"},{"id":2,"name":"enhancement","color":"cc0000"}]`,
	}))
	names, err := c.PRLabels(36)
	if err != nil {
		t.Fatalf("PRLabels: %v", err)
	}
	if len(names) != 2 || names[0] != "maestro-ready" || names[1] != "enhancement" {
		t.Fatalf("labels = %v", names)
	}
}

func TestForgejoPortPRCommitsHeadlines(t *testing.T) {
	commitsPath := "/repos/" + fjTestRepo + "/pulls/1/commits"
	c, _, _ := newForgejoPortClient(t, fjRoute(t, map[string]string{
		// Wire messages carry a trailing "\n" (live-verified); one entry is
		// multi-line, one is whitespace-only and must be dropped.
		commitsPath: `[
			{"sha":"59e99c49c27d3e2f73bae1657f07cd2f9a15f926","commit":{"message":"feat: headline\n\nlong body\n"}},
			{"sha":"dfc9446cb6a16c60075286299cd07d2cb655769a","commit":{"message":"  \n"}},
			{"sha":"aaaa446cb6a16c60075286299cd07d2cb655769a","commit":{"message":"fix: second\n"}}
		]`,
	}))
	msgs, err := c.PRCommits(1)
	if err != nil {
		t.Fatalf("PRCommits: %v", err)
	}
	if len(msgs) != 2 || msgs[0] != "feat: headline" || msgs[1] != "fix: second" {
		t.Fatalf("headlines = %v (headline extraction must match parsePRCommits)", msgs)
	}
}

func TestForgejoPortPRChangedFiles(t *testing.T) {
	filesPath := "/repos/" + fjTestRepo + "/pulls/1/files"
	c, _, _ := newForgejoPortClient(t, fjRoute(t, map[string]string{
		filesPath: `[{"filename":"web/app.css","status":"modified","additions":3,"deletions":1},
			{"filename":"  ","status":"modified"},
			{"filename":"cmd/main.go","status":"added","additions":9,"deletions":0}]`,
	}))
	files, err := c.PRChangedFiles(1)
	if err != nil {
		t.Fatalf("PRChangedFiles: %v", err)
	}
	if len(files) != 2 || files[0] != "web/app.css" || files[1] != "cmd/main.go" {
		t.Fatalf("files = %v", files)
	}
}

func TestForgejoPortPRVisualEvidenceAttached(t *testing.T) {
	pullPath := "/repos/" + fjTestRepo + "/pulls/3"
	commentsPath := "/repos/" + fjTestRepo + "/issues/3/comments"
	pullNoImage := `{"number":3,"title":"ui","body":"plain text","state":"open","draft":false,
		"mergeable":true,"merged":false,"merged_at":null,"merge_commit_sha":null,
		"head":{"ref":"feat/ui","sha":"59e99c49c27d3e2f73bae1657f07cd2f9a15f926"},"base":{"ref":"main"}}`

	t.Run("image in body short-circuits", func(t *testing.T) {
		c, seen, _ := newForgejoPortClient(t, fjRoute(t, map[string]string{
			pullPath: strings.Replace(pullNoImage, "plain text", "![shot](https://h/x.png)", 1),
		}))
		ok, err := c.PRVisualEvidenceAttached(3)
		if err != nil || !ok {
			t.Fatalf("PRVisualEvidenceAttached = %v, %v; want true from the body", ok, err)
		}
		if len(*seen) != 1 {
			t.Fatalf("requests = %+v, want the comments read skipped", *seen)
		}
	})
	t.Run("image in a comment", func(t *testing.T) {
		c, _, _ := newForgejoPortClient(t, fjRoute(t, map[string]string{
			pullPath:     pullNoImage,
			commentsPath: `[{"id":1,"body":"<img src=\"https://h/s.png\">","user":{"login":"worker"},"created_at":"2026-08-01T10:00:00Z"}]`,
		}))
		ok, err := c.PRVisualEvidenceAttached(3)
		if err != nil || !ok {
			t.Fatalf("PRVisualEvidenceAttached = %v, %v; want true from the comment", ok, err)
		}
	})
	t.Run("no image anywhere", func(t *testing.T) {
		c, _, _ := newForgejoPortClient(t, fjRoute(t, map[string]string{
			pullPath:     pullNoImage,
			commentsPath: `[{"id":1,"body":"looks good","user":{"login":"worker"},"created_at":"2026-08-01T10:00:00Z"}]`,
		}))
		ok, err := c.PRVisualEvidenceAttached(3)
		if err != nil || ok {
			t.Fatalf("PRVisualEvidenceAttached = %v, %v; want false", ok, err)
		}
	})
}

// --- repo / branch -------------------------------------------------------------

func TestForgejoPortRepositoryDefaultBranch(t *testing.T) {
	repoPath := "/repos/" + fjTestRepo
	c, _, _ := newForgejoPortClient(t, fjRoute(t, map[string]string{
		repoPath: `{"name":"apertune","default_branch":"main"}`,
	}))
	branch, err := c.RepositoryDefaultBranch(t.Context())
	if err != nil || branch != "main" {
		t.Fatalf("RepositoryDefaultBranch = %q, %v", branch, err)
	}
}

func TestForgejoPortRepositoryDefaultBranchRejectsMetachars(t *testing.T) {
	repoPath := "/repos/" + fjTestRepo
	c, _, _ := newForgejoPortClient(t, fjRoute(t, map[string]string{
		repoPath: `{"name":"apertune","default_branch":"main?x=1"}`,
	}))
	if _, err := c.RepositoryDefaultBranch(t.Context()); err == nil {
		t.Fatal("a metacharacter branch must be rejected — gh-path validation parity")
	}
}

func TestForgejoPortBranchHeadSHAEscapesSlash(t *testing.T) {
	c, seen, _ := newForgejoPortClient(t, func(r *http.Request) (int, string) {
		return 200, `{"name":"canary/llm-review-e2e","sha":"decoy",
			"commit":{"id":"59e99c49c27d3e2f73bae1657f07cd2f9a15f926","message":"m"}}`
	})
	sha, err := c.BranchHeadSHA("canary/llm-review-e2e")
	if err != nil {
		t.Fatalf("BranchHeadSHA: %v", err)
	}
	if sha != "59e99c49c27d3e2f73bae1657f07cd2f9a15f926" {
		t.Fatalf("sha = %q (must come from commit.id, not the top-level decoy)", sha)
	}
	req := (*seen)[0]
	if req.Path != "/repos/"+fjTestRepo+"/branches/canary%2Fllm-review-e2e" {
		t.Fatalf("path = %s, want the slash percent-escaped", req.Path)
	}
}

// --- fail-loud edges -------------------------------------------------------------

func TestForgejoPortHTTPErrorSurfaces(t *testing.T) {
	c, _, _ := newForgejoPortClient(t, func(*http.Request) (int, string) {
		return 500, `{"message":"boom"}`
	})
	_, err := c.GetIssue(1)
	if err == nil {
		t.Fatal("a 500 must surface as an error")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Fatalf("err = %v, want the HTTP status visible", err)
	}
	if errors.Is(err, ErrForgejoNotSupported) {
		t.Fatalf("err = %v: a transport failure on a PORTED read must not claim not-supported", err)
	}
}

// TestForgejoPortGuardsMakeNoRequests pins the explicit NOT-ported guards: the
// methods that would half-work now that their underlying funnels are ported
// must fail with the sentinel BEFORE any Forgejo request — a half-executed
// read would waste quota and, worse, look like partial support.
func TestForgejoPortGuardsMakeNoRequests(t *testing.T) {
	c, seen, ghCalls := newForgejoPortClient(t, func(*http.Request) (int, string) {
		return 200, `{}`
	})
	guards := []struct {
		name string
		call func() error
	}{
		{"PRMergeStatus", func() error { _, _, err := c.PRMergeStatus(1); return err }},
		{"PRCheckRollup", func() error { _, err := c.PRCheckRollup(1); return err }},
		{"PRCIStatus", func() error { _, err := c.PRCIStatus(1); return err }},
		{"PRChecksOutput", func() error { _, err := c.PRChecksOutput(1); return err }},
		{"CIFailureSummary", func() error { _, err := c.CIFailureSummary(1); return err }},
		{"CollectPRReviewFeedback", func() error { _, err := c.CollectPRReviewFeedback(1, nil); return err }},
	}
	for _, g := range guards {
		err := g.call()
		if err == nil {
			t.Errorf("%s: err = nil, want ErrForgejoNotSupported", g.name)
			continue
		}
		if !errors.Is(err, ErrForgejoNotSupported) {
			t.Errorf("%s: err = %v, not errors.Is-matchable against the sentinel", g.name, err)
		}
	}
	if len(*seen) != 0 {
		t.Fatalf("guarded methods reached the Forgejo API: %+v", *seen)
	}
	if n := ghCalls.Load(); n != 0 {
		t.Fatalf("gh runner invoked %d time(s), want 0", n)
	}
}

// --- github-mode regression -------------------------------------------------------

// TestGitHubModeCoreReadsUnchanged pins that a zero-ForgeConfig client keeps
// issuing the exact historical gh reads for the funnels this port touched —
// same endpoints, same pagination flags — and parses them as before.
func TestGitHubModeCoreReadsUnchanged(t *testing.T) {
	resetPrimaryLimitForTest()
	var endpoints []string
	orig := ghAPIRunner
	ghAPIRunner = func(args ...string) ([]byte, error) {
		joined := strings.Join(args, " ")
		endpoints = append(endpoints, joined)
		switch {
		case strings.Contains(joined, "/issues/9/comments"):
			return []byte(`[{"id":1,"body":"c","user":{"login":"u"},"created_at":"2026-08-01T10:00:00Z"}]`), nil
		case strings.Contains(joined, "/issues/7"):
			return []byte(`{"number":7,"title":"t","body":"b","state":"open","labels":[]}`), nil
		case strings.Contains(joined, "/pulls?state=open"):
			return []byte(`[{"number":4,"title":"pr","body":"","state":"open","draft":false,
				"head":{"ref":"feat/x","sha":"59e99c49c27d3e2f73bae1657f07cd2f9a15f926"},
				"base":{"ref":"main"},"merged_at":null}]`), nil
		}
		return nil, fmt.Errorf("unexpected gh call: %s", joined)
	}
	t.Cleanup(func() {
		ghAPIRunner = orig
		resetPrimaryLimitForTest()
	})

	c := New("owner/repo", config.ForgeConfig{})

	issue, err := c.GetIssue(7)
	if err != nil || issue.Number != 7 {
		t.Fatalf("GetIssue = %+v, %v", issue, err)
	}
	prs, err := c.ListOpenPRs()
	if err != nil || len(prs) != 1 || prs[0].State != "OPEN" {
		t.Fatalf("ListOpenPRs = %+v, %v", prs, err)
	}
	comments, err := c.ListIssueComments(9)
	if err != nil || len(comments) != 1 || comments[0].Author != "u" {
		t.Fatalf("ListIssueComments = %+v, %v", comments, err)
	}

	wantSubstrings := []string{
		"repos/owner/repo/issues/7",
		"repos/owner/repo/pulls?state=open&per_page=100",
		"repos/owner/repo/issues/9/comments?per_page=100 --paginate",
	}
	if len(endpoints) != len(wantSubstrings) {
		t.Fatalf("gh calls = %v", endpoints)
	}
	for i, want := range wantSubstrings {
		if !strings.Contains(endpoints[i], want) {
			t.Fatalf("gh call %d = %q, want it to contain %q", i, endpoints[i], want)
		}
	}
}

// --- shutdown-aware call contexts (#797 parity for ctx-less fj adapters) -----

// forgejoCallContext must mirror the gh path's drain semantics: alive before
// BeginShutdown, canceled by it mid-flight, and born canceled after it.
func TestForgejoCallContextFollowsShutdown(t *testing.T) {
	resetShutdownForTest()
	t.Cleanup(resetShutdownForTest)

	ctx, cancel := forgejoCallContext()
	defer cancel()
	select {
	case <-ctx.Done():
		t.Fatal("call context done before shutdown began")
	default:
	}

	BeginShutdown()
	select {
	case <-ctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("BeginShutdown did not cancel an already-issued forgejo call context")
	}

	post, postCancel := forgejoCallContext()
	defer postCancel()
	select {
	case <-post.Done():
	default:
		t.Fatal("a call context minted after BeginShutdown must start canceled")
	}
}

// A ctx-less legacy method (GetIssue) whose Forgejo read is in flight against
// a hanging server must return promptly with a context error once shutdown
// begins — not ride out the transport's 30s timeout (the pre-fix behavior:
// context.Background() made daemon drain wait on every hung read).
func TestForgejoPortShutdownCancelsInFlightRead(t *testing.T) {
	resetShutdownForTest()
	t.Cleanup(resetShutdownForTest)

	reached := make(chan struct{})
	c, _, _ := newForgejoPortClient(t, func(r *http.Request) (int, string) {
		close(reached)
		<-r.Context().Done() // hang until the canceled client tears the request down
		return 500, `{"message":"too late"}`
	})

	errCh := make(chan error, 1)
	go func() {
		_, err := c.GetIssue(41)
		errCh <- err
	}()

	select {
	case <-reached:
	case <-time.After(5 * time.Second):
		t.Fatal("fixture server never saw the read")
	}
	BeginShutdown()

	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("read must fail once shutdown cancels its context")
		}
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v, want context.Canceled through the adapter chain", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("read still in flight 5s after BeginShutdown — forgejo calls are not shutdown-aware")
	}
}
