package github

// End-to-end forgejo-mode tests for the ported WRITE funnels (#1172 M3).
// Same construction as forgejo_port_test.go — config.ForgeConfig → github.New
// → internal/forgejo.Client → httptest fixtures, gh transport armed to fail on
// any leak — plus request-body capture: every write asserts METHOD + PATH +
// the exact decoded JSON payload, so a stray or missing wire field fails the
// test here instead of on the live forge (writes are NEVER exercised live).

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/forgejo"
)

// fjWriteReq is one request the write fixture server observed, including the
// DECODED JSON payload (nil when the request carried no body).
type fjWriteReq struct {
	Method string
	Path   string
	Query  url.Values
	JSON   map[string]any
}

// fjWriteResp is one canned fixture response.
type fjWriteResp struct {
	Status int
	Body   string
}

// newForgejoWriteClient mirrors newForgejoPortClient with payload capture.
func newForgejoWriteClient(t *testing.T, fn func(r *http.Request) fjWriteResp) (*Client, *[]fjWriteReq, *atomic.Int64) {
	t.Helper()
	ghCalls := stubGHNeverCalled(t)
	var seen []fjWriteReq
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.EscapedPath(), "/api/v1/") {
			t.Errorf("request outside the API root: %s", r.URL.EscapedPath())
			w.WriteHeader(http.StatusNotFound)
			return
		}
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read %s %s body: %v", r.Method, r.URL.Path, err)
		}
		var decoded map[string]any
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &decoded); err != nil {
				t.Errorf("decode %s %s payload: %v (raw: %s)", r.Method, r.URL.Path, err, raw)
			}
		}
		seen = append(seen, fjWriteReq{
			Method: r.Method,
			Path:   strings.TrimPrefix(r.URL.EscapedPath(), "/api/v1"),
			Query:  r.URL.Query(),
			JSON:   decoded,
		})
		resp := fn(r)
		w.WriteHeader(resp.Status)
		if resp.Body != "" {
			_, _ = w.Write([]byte(resp.Body))
		}
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

// fjWriteRoute serves canned responses keyed by "METHOD /path" (path relative
// to the API root, query stripped). Unrouted requests fail the test.
func fjWriteRoute(t *testing.T, routes map[string]fjWriteResp) func(r *http.Request) fjWriteResp {
	t.Helper()
	return func(r *http.Request) fjWriteResp {
		key := r.Method + " " + strings.TrimPrefix(r.URL.EscapedPath(), "/api/v1")
		if resp, ok := routes[key]; ok {
			return resp
		}
		t.Errorf("unrouted fixture request: %s", key)
		return fjWriteResp{Status: 404, Body: `{"message":"no fixture"}`}
	}
}

// assertWrite asserts request i has the given method, path, and exact decoded
// payload (nil wantJSON = no body at all).
func assertWrite(t *testing.T, seen *[]fjWriteReq, i int, method, path string, wantJSON map[string]any) fjWriteReq {
	t.Helper()
	if len(*seen) <= i {
		t.Fatalf("requests = %d, want at least %d: %+v", len(*seen), i+1, *seen)
	}
	req := (*seen)[i]
	if req.Method != method || req.Path != path {
		t.Fatalf("request %d = %s %s, want %s %s", i, req.Method, req.Path, method, path)
	}
	if !reflect.DeepEqual(req.JSON, wantJSON) {
		t.Fatalf("request %d payload = %#v, want %#v", i, req.JSON, wantJSON)
	}
	return req
}

// --- PRs -----------------------------------------------------------------------

func TestForgejoWriteCreatePR(t *testing.T) {
	c, seen, ghCalls := newForgejoWriteClient(t, fjWriteRoute(t, map[string]fjWriteResp{
		// html_url is a decoy with a DIFFERENT number: the number must come
		// from the response JSON, never from URL scraping (Forgejo web URLs
		// are /pulls/N where the gh path scrapes /pull/N).
		"POST /repos/" + fjTestRepo + "/pulls": {201, `{"id":99,"number":7,"html_url":"https://forge/BeFeast/apertune/pulls/1","state":"open","draft":false}`},
	}))
	n, err := c.CreatePR("feat: title", "the body", "main", "feat/x")
	if err != nil {
		t.Fatalf("CreatePR: %v", err)
	}
	if n != 7 {
		t.Fatalf("number = %d, want 7 (from response JSON, not the html_url decoy)", n)
	}
	assertWrite(t, seen, 0, http.MethodPost, "/repos/"+fjTestRepo+"/pulls", map[string]any{
		"title": "feat: title",
		"body":  "the body",
		"base":  "main",
		"head":  "feat/x",
	})
	if len(*seen) != 1 || ghCalls.Load() != 0 {
		t.Fatalf("requests = %d, gh calls = %d; want 1 and 0", len(*seen), ghCalls.Load())
	}
}

func TestForgejoWriteUpdatePRBody(t *testing.T) {
	c, seen, _ := newForgejoWriteClient(t, fjWriteRoute(t, map[string]fjWriteResp{
		"PATCH /repos/" + fjTestRepo + "/pulls/5": {201, `{}`},
	}))
	if err := c.UpdatePRBody(5, "replacement body"); err != nil {
		t.Fatalf("UpdatePRBody: %v", err)
	}
	assertWrite(t, seen, 0, http.MethodPatch, "/repos/"+fjTestRepo+"/pulls/5", map[string]any{
		"body": "replacement body",
	})
}

func TestForgejoWriteClosePRCommentThenClose(t *testing.T) {
	c, seen, _ := newForgejoWriteClient(t, fjWriteRoute(t, map[string]fjWriteResp{
		"POST /repos/" + fjTestRepo + "/issues/9/comments": {201, `{"id":1}`},
		"PATCH /repos/" + fjTestRepo + "/pulls/9":          {201, `{}`},
	}))
	if err := c.ClosePR(9, "superseded by #10"); err != nil {
		t.Fatalf("ClosePR: %v", err)
	}
	// ORDER is the contract: the explanation lands before the state flips.
	assertWrite(t, seen, 0, http.MethodPost, "/repos/"+fjTestRepo+"/issues/9/comments", map[string]any{
		"body": "superseded by #10",
	})
	assertWrite(t, seen, 1, http.MethodPatch, "/repos/"+fjTestRepo+"/pulls/9", map[string]any{
		"state": "closed",
	})
	if len(*seen) != 2 {
		t.Fatalf("requests = %d, want exactly comment then close", len(*seen))
	}
}

func TestForgejoWriteClosePREmptyCommentSkips(t *testing.T) {
	c, seen, _ := newForgejoWriteClient(t, fjWriteRoute(t, map[string]fjWriteResp{
		"PATCH /repos/" + fjTestRepo + "/pulls/9": {201, `{}`},
	}))
	if err := c.ClosePR(9, ""); err != nil {
		t.Fatalf("ClosePR: %v", err)
	}
	assertWrite(t, seen, 0, http.MethodPatch, "/repos/"+fjTestRepo+"/pulls/9", map[string]any{
		"state": "closed",
	})
	if len(*seen) != 1 {
		t.Fatalf("requests = %d, want the comment step skipped entirely", len(*seen))
	}
}

func TestForgejoWriteClosePRAbortsWhenCommentFails(t *testing.T) {
	c, seen, _ := newForgejoWriteClient(t, fjWriteRoute(t, map[string]fjWriteResp{
		"POST /repos/" + fjTestRepo + "/issues/9/comments": {500, `{"message":"boom"}`},
	}))
	err := c.ClosePR(9, "explanation that must not be lost")
	if err == nil {
		t.Fatal("a failed comment must abort the close")
	}
	if len(*seen) != 1 {
		t.Fatalf("requests = %+v, want NO close PATCH after the failed comment (PR stays open)", *seen)
	}
}

func TestForgejoWriteCloseIssue(t *testing.T) {
	t.Run("comment then close, in order", func(t *testing.T) {
		c, seen, _ := newForgejoWriteClient(t, fjWriteRoute(t, map[string]fjWriteResp{
			"POST /repos/" + fjTestRepo + "/issues/4/comments": {201, `{"id":1}`},
			"PATCH /repos/" + fjTestRepo + "/issues/4":         {201, `{}`},
		}))
		if err := c.CloseIssue(4, "fixed in #5"); err != nil {
			t.Fatalf("CloseIssue: %v", err)
		}
		assertWrite(t, seen, 0, http.MethodPost, "/repos/"+fjTestRepo+"/issues/4/comments", map[string]any{
			"body": "fixed in #5",
		})
		assertWrite(t, seen, 1, http.MethodPatch, "/repos/"+fjTestRepo+"/issues/4", map[string]any{
			"state": "closed",
		})
		if len(*seen) != 2 {
			t.Fatalf("requests = %d, want exactly comment then close", len(*seen))
		}
	})
	t.Run("empty comment skips the comment step", func(t *testing.T) {
		c, seen, _ := newForgejoWriteClient(t, fjWriteRoute(t, map[string]fjWriteResp{
			"PATCH /repos/" + fjTestRepo + "/issues/4": {201, `{}`},
		}))
		if err := c.CloseIssue(4, ""); err != nil {
			t.Fatalf("CloseIssue: %v", err)
		}
		if len(*seen) != 1 {
			t.Fatalf("requests = %+v, want the close PATCH only", *seen)
		}
	})
	t.Run("failed comment aborts the close", func(t *testing.T) {
		c, seen, _ := newForgejoWriteClient(t, fjWriteRoute(t, map[string]fjWriteResp{
			"POST /repos/" + fjTestRepo + "/issues/4/comments": {500, `{"message":"boom"}`},
		}))
		if err := c.CloseIssue(4, "why"); err == nil {
			t.Fatal("a failed comment must abort the close")
		}
		if len(*seen) != 1 {
			t.Fatalf("requests = %+v, want NO close PATCH after the failed comment", *seen)
		}
	})
}

// --- merge ---------------------------------------------------------------------

const fjWriteHeadSHA = "59e99c49c27d3e2f73bae1657f07cd2f9a15f926"

func TestForgejoWriteMergePRAtHeadPayload(t *testing.T) {
	c, seen, ghCalls := newForgejoWriteClient(t, fjWriteRoute(t, map[string]fjWriteResp{
		"POST /repos/" + fjTestRepo + "/pulls/12/merge": {200, ``},
	}))
	if err := c.MergePRAtHead(12, fjWriteHeadSHA); err != nil {
		t.Fatalf("MergePRAtHead: %v", err)
	}
	// gh parity: --squash --delete-branch --match-head-commit, with the
	// swagger's mixed wire casing pinned exactly.
	assertWrite(t, seen, 0, http.MethodPost, "/repos/"+fjTestRepo+"/pulls/12/merge", map[string]any{
		"Do":                        "squash",
		"head_commit_id":            fjWriteHeadSHA,
		"delete_branch_after_merge": true,
	})
	if ghCalls.Load() != 0 {
		t.Fatalf("gh runner invoked %d time(s), want 0", ghCalls.Load())
	}
}

func TestForgejoWriteMergePRAtHeadEmptySHAPreflight(t *testing.T) {
	c, seen, _ := newForgejoWriteClient(t, fjWriteRoute(t, map[string]fjWriteResp{}))
	err := c.MergePRAtHead(12, "   ")
	if err == nil || !strings.Contains(err.Error(), "merge PR 12: expected head SHA is required") {
		t.Fatalf("err = %v, want the shared gh-parity pre-flight message", err)
	}
	if len(*seen) != 0 {
		t.Fatalf("requests = %+v, want none — the pre-flight must run before any call", *seen)
	}
}

// TestForgejoWriteMergeRefusalClassification pins the sentinel contract end to
// end: a 405/409 refusal whose body indicates the out-of-date/head-mismatch
// family maps onto github.ErrMergeNotUpToDate (and keeps the legacy
// "not up to date" needle in its text for string-matching consumers); every
// other refusal — bodyless 405 included — stays a raw loud error.
func TestForgejoWriteMergeRefusalClassification(t *testing.T) {
	cases := []struct {
		name         string
		status       int
		body         string
		wantSentinel bool
	}{
		{"409 out-of-date body", 409, `{"message":"Please update your branch: it is not up to date with the base branch","url":""}`, true},
		{"405 head_commit_id mismatch body", 405, `{"message":"head_commit_id is out of date","url":""}`, true},
		{"bodyless 405 stays raw", 405, ``, false},
		{"409 unrelated conflict body stays raw", 409, `{"message":"merge conflict detected in web/app.css","url":""}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, _, _ := newForgejoWriteClient(t, fjWriteRoute(t, map[string]fjWriteResp{
				"POST /repos/" + fjTestRepo + "/pulls/12/merge": {tc.status, tc.body},
			}))
			err := c.MergePRAtHead(12, fjWriteHeadSHA)
			if err == nil {
				t.Fatal("a merge refusal must surface as an error")
			}
			if got := errors.Is(err, ErrMergeNotUpToDate); got != tc.wantSentinel {
				t.Fatalf("errors.Is(err, ErrMergeNotUpToDate) = %v, want %v (err = %v)", got, tc.wantSentinel, err)
			}
			if got := errors.Is(err, forgejo.ErrMergeOutOfDate); got != tc.wantSentinel {
				t.Fatalf("errors.Is(err, forgejo.ErrMergeOutOfDate) = %v, want %v (err = %v)", got, tc.wantSentinel, err)
			}
			if tc.wantSentinel && !strings.Contains(err.Error(), "not up to date") {
				t.Fatalf("classified error must keep the legacy needle for string-matching consumers, got: %v", err)
			}
			var se *forgejo.StatusError
			if !errors.As(err, &se) || se.StatusCode != tc.status {
				t.Fatalf("the raw *forgejo.StatusError must survive the wrap chain, got: %v", err)
			}
		})
	}
}

func TestForgejoWriteUpdateBranch(t *testing.T) {
	c, seen, _ := newForgejoWriteClient(t, fjWriteRoute(t, map[string]fjWriteResp{
		"POST /repos/" + fjTestRepo + "/pulls/3/update": {200, ``},
	}))
	if err := c.UpdateBranch(3); err != nil {
		t.Fatalf("UpdateBranch: %v", err)
	}
	req := assertWrite(t, seen, 0, http.MethodPost, "/repos/"+fjTestRepo+"/pulls/3/update", nil)
	if got := req.Query.Get("style"); got != "merge" {
		t.Fatalf("style = %q, want the explicit merge style (gh default parity, not rebase)", got)
	}
}

// --- issues ---------------------------------------------------------------------

func TestForgejoWriteCreateIssueResolvesLabelNames(t *testing.T) {
	labelsList := `[{"id":5,"name":"maestro-ready","exclusive":false,"is_archived":false,"color":"00aabb","description":""},
		{"id":9,"name":"bug","exclusive":false,"is_archived":false,"color":"ee0000","description":""}]`
	c, seen, _ := newForgejoWriteClient(t, fjWriteRoute(t, map[string]fjWriteResp{
		"GET /repos/" + fjTestRepo + "/labels":  {200, labelsList},
		"POST /repos/" + fjTestRepo + "/issues": {201, `{"id":77,"number":31,"html_url":"https://forge/BeFeast/apertune/issues/2"}`},
	}))
	n, err := c.CreateIssue("issue title", "issue body", []string{"bug", "maestro-ready"})
	if err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}
	if n != 31 {
		t.Fatalf("number = %d, want 31 from the response JSON", n)
	}
	if len(*seen) != 2 {
		t.Fatalf("requests = %+v, want label resolution then create", *seen)
	}
	if (*seen)[0].Method != http.MethodGet || (*seen)[0].Path != "/repos/"+fjTestRepo+"/labels" {
		t.Fatalf("request 0 = %s %s, want the repo labels read", (*seen)[0].Method, (*seen)[0].Path)
	}
	// CreateIssueOption.labels is []int64 — ids in CALLER order, not repo order.
	assertWrite(t, seen, 1, http.MethodPost, "/repos/"+fjTestRepo+"/issues", map[string]any{
		"title":  "issue title",
		"body":   "issue body",
		"labels": []any{float64(9), float64(5)},
	})
}

func TestForgejoWriteCreateIssueUnknownLabelAbortsBeforeCreate(t *testing.T) {
	c, seen, _ := newForgejoWriteClient(t, fjWriteRoute(t, map[string]fjWriteResp{
		"GET /repos/" + fjTestRepo + "/labels": {200, `[{"id":5,"name":"bug","color":"ee0000","description":""}]`},
	}))
	_, err := c.CreateIssue("t", "b", []string{"bug", "no-such-label"})
	if err == nil || !strings.Contains(err.Error(), `label "no-such-label" does not exist`) {
		t.Fatalf("err = %v, want the unknown label named loudly", err)
	}
	if len(*seen) != 1 {
		t.Fatalf("requests = %+v, want NO issue created after the failed resolution", *seen)
	}
}

func TestForgejoWriteCreateIssueNoLabelsSkipsResolution(t *testing.T) {
	c, seen, _ := newForgejoWriteClient(t, fjWriteRoute(t, map[string]fjWriteResp{
		"POST /repos/" + fjTestRepo + "/issues": {201, `{"number":32}`},
	}))
	n, err := c.CreateIssue("t", "b", nil)
	if err != nil || n != 32 {
		t.Fatalf("CreateIssue = %d, %v", n, err)
	}
	// No labels key at all when none were requested, and no labels read.
	assertWrite(t, seen, 0, http.MethodPost, "/repos/"+fjTestRepo+"/issues", map[string]any{
		"title": "t",
		"body":  "b",
	})
	if len(*seen) != 1 {
		t.Fatalf("requests = %+v, want the create only", *seen)
	}
}

func TestForgejoWriteEditIssueBody(t *testing.T) {
	c, seen, _ := newForgejoWriteClient(t, fjWriteRoute(t, map[string]fjWriteResp{
		"PATCH /repos/" + fjTestRepo + "/issues/4": {201, `{}`},
	}))
	if err := c.EditIssueBody(4, "replaced body"); err != nil {
		t.Fatalf("EditIssueBody: %v", err)
	}
	assertWrite(t, seen, 0, http.MethodPatch, "/repos/"+fjTestRepo+"/issues/4", map[string]any{
		"body": "replaced body",
	})
}

// --- labels ---------------------------------------------------------------------

func TestForgejoWriteAddIssueLabelByName(t *testing.T) {
	c, seen, _ := newForgejoWriteClient(t, fjWriteRoute(t, map[string]fjWriteResp{
		"POST /repos/" + fjTestRepo + "/issues/6/labels": {200, `[]`},
	}))
	if err := c.AddIssueLabel(6, "blocked"); err != nil {
		t.Fatalf("AddIssueLabel: %v", err)
	}
	// IssueLabelsOption accepts NAMES on this instance — verbatim, no id
	// resolution round trip.
	assertWrite(t, seen, 0, http.MethodPost, "/repos/"+fjTestRepo+"/issues/6/labels", map[string]any{
		"labels": []any{"blocked"},
	})
	if len(*seen) != 1 {
		t.Fatalf("requests = %+v, want the label add only (no resolution read)", *seen)
	}
}

func TestForgejoWriteRemoveIssueLabel(t *testing.T) {
	t.Run("by name, path-escaped, 204 is success", func(t *testing.T) {
		c, seen, _ := newForgejoWriteClient(t, fjWriteRoute(t, map[string]fjWriteResp{
			"DELETE /repos/" + fjTestRepo + "/issues/6/labels/needs%20info": {204, ``},
		}))
		if err := c.RemoveIssueLabel(6, "needs info"); err != nil {
			t.Fatalf("RemoveIssueLabel: %v", err)
		}
		// A label the issue does not carry also answers 204 (zero rows
		// deleted) — the same green path, gh's no-op parity.
		assertWrite(t, seen, 0, http.MethodDelete, "/repos/"+fjTestRepo+"/issues/6/labels/needs%20info", nil)
	})
	t.Run("label missing from the repo fails loud", func(t *testing.T) {
		c, _, _ := newForgejoWriteClient(t, fjWriteRoute(t, map[string]fjWriteResp{
			"DELETE /repos/" + fjTestRepo + "/issues/6/labels/ghost": {404, `{"message":"label does not exist","url":""}`},
		}))
		err := c.RemoveIssueLabel(6, "ghost")
		if err == nil || !strings.Contains(err.Error(), "404") {
			t.Fatalf("err = %v, want the repo-missing label loud (gh parity)", err)
		}
	})
}

func TestForgejoWriteEnsureLabelUpsert(t *testing.T) {
	existing := `[{"id":23,"name":"Blocked","exclusive":false,"is_archived":false,"color":"cccccc","description":"old"}]`
	t.Run("existing label is PATCHed with only the provided fields", func(t *testing.T) {
		c, seen, _ := newForgejoWriteClient(t, fjWriteRoute(t, map[string]fjWriteResp{
			"GET /repos/" + fjTestRepo + "/labels":      {200, existing},
			"PATCH /repos/" + fjTestRepo + "/labels/23": {200, `{}`},
		}))
		// Case-insensitive name match: "blocked" finds "Blocked" (id 23).
		if err := c.EnsureLabel("blocked", "d93f0b", "now blocking"); err != nil {
			t.Fatalf("EnsureLabel: %v", err)
		}
		assertWrite(t, seen, 1, http.MethodPatch, "/repos/"+fjTestRepo+"/labels/23", map[string]any{
			"color":       "d93f0b",
			"description": "now blocking",
		})
	})
	t.Run("exact name match wins over a case-insensitive one", func(t *testing.T) {
		// Forgejo does not enforce name uniqueness under case folding: with
		// "Blocked" (id 7) listed BEFORE "blocked" (id 23), a fold-first
		// matcher would PATCH id 7 — the exact match must win.
		both := `[{"id":7,"name":"Blocked","color":"cccccc","description":""},{"id":23,"name":"blocked","color":"cccccc","description":""}]`
		c, seen, _ := newForgejoWriteClient(t, fjWriteRoute(t, map[string]fjWriteResp{
			"GET /repos/" + fjTestRepo + "/labels":      {200, both},
			"PATCH /repos/" + fjTestRepo + "/labels/23": {200, `{}`},
		}))
		if err := c.EnsureLabel("blocked", "d93f0b", ""); err != nil {
			t.Fatalf("EnsureLabel: %v", err)
		}
		assertWrite(t, seen, 1, http.MethodPatch, "/repos/"+fjTestRepo+"/labels/23", map[string]any{
			"color": "d93f0b",
		})
	})
	t.Run("existing label with nothing to update makes no write", func(t *testing.T) {
		c, seen, _ := newForgejoWriteClient(t, fjWriteRoute(t, map[string]fjWriteResp{
			"GET /repos/" + fjTestRepo + "/labels": {200, existing},
		}))
		if err := c.EnsureLabel("Blocked", "", ""); err != nil {
			t.Fatalf("EnsureLabel: %v", err)
		}
		if len(*seen) != 1 {
			t.Fatalf("requests = %+v, want the list read only (a field-less PATCH is banned)", *seen)
		}
	})
	t.Run("missing label is created with the caller color", func(t *testing.T) {
		c, seen, _ := newForgejoWriteClient(t, fjWriteRoute(t, map[string]fjWriteResp{
			"GET /repos/" + fjTestRepo + "/labels":  {200, `[]`},
			"POST /repos/" + fjTestRepo + "/labels": {201, `{}`},
		}))
		if err := c.EnsureLabel("maestro-repair", "D93F0B", "repair work"); err != nil {
			t.Fatalf("EnsureLabel: %v", err)
		}
		assertWrite(t, seen, 1, http.MethodPost, "/repos/"+fjTestRepo+"/labels", map[string]any{
			"name":        "maestro-repair",
			"color":       "D93F0B",
			"description": "repair work",
		})
	})
	t.Run("missing label with no caller color gets the documented default", func(t *testing.T) {
		c, seen, _ := newForgejoWriteClient(t, fjWriteRoute(t, map[string]fjWriteResp{
			"GET /repos/" + fjTestRepo + "/labels":  {200, `[]`},
			"POST /repos/" + fjTestRepo + "/labels": {201, `{}`},
		}))
		// CreateLabelOption REQUIRES color; gh picks one itself, so the
		// forgejo arm supplies the fixed "#ededed" default. Description is
		// omitted when empty.
		if err := c.EnsureLabel("bare-label", "", ""); err != nil {
			t.Fatalf("EnsureLabel: %v", err)
		}
		assertWrite(t, seen, 1, http.MethodPost, "/repos/"+fjTestRepo+"/labels", map[string]any{
			"name":  "bare-label",
			"color": "#ededed",
		})
	})
	t.Run("empty name is a pre-flight error, no request", func(t *testing.T) {
		c, seen, _ := newForgejoWriteClient(t, fjWriteRoute(t, map[string]fjWriteResp{}))
		err := c.EnsureLabel("   ", "d93f0b", "x")
		if err == nil || !strings.Contains(err.Error(), "ensure label: empty name") {
			t.Fatalf("err = %v, want the shared empty-name pre-flight", err)
		}
		if len(*seen) != 0 {
			t.Fatalf("requests = %+v, want none", *seen)
		}
	})
}

// --- comments / releases ----------------------------------------------------------

func TestForgejoWriteCommentIssueAndPR(t *testing.T) {
	// One shared route serves both: pulls live in the issue number space.
	c, seen, _ := newForgejoWriteClient(t, fjWriteRoute(t, map[string]fjWriteResp{
		"POST /repos/" + fjTestRepo + "/issues/9/comments":  {201, `{"id":1}`},
		"POST /repos/" + fjTestRepo + "/issues/11/comments": {201, `{"id":2}`},
	}))
	if err := c.CommentIssue(9, "issue note"); err != nil {
		t.Fatalf("CommentIssue: %v", err)
	}
	if err := c.CommentPR(11, "pr note"); err != nil {
		t.Fatalf("CommentPR: %v", err)
	}
	assertWrite(t, seen, 0, http.MethodPost, "/repos/"+fjTestRepo+"/issues/9/comments", map[string]any{
		"body": "issue note",
	})
	assertWrite(t, seen, 1, http.MethodPost, "/repos/"+fjTestRepo+"/issues/11/comments", map[string]any{
		"body": "pr note",
	})
}

func TestForgejoWriteCreateRelease(t *testing.T) {
	c, seen, _ := newForgejoWriteClient(t, fjWriteRoute(t, map[string]fjWriteResp{
		"POST /repos/" + fjTestRepo + "/releases": {201, `{"id":3}`},
	}))
	if err := c.CreateRelease("v1.2.3", "Release v1.2.3"); err != nil {
		t.Fatalf("CreateRelease: %v", err)
	}
	// Exactly tag_name + name: no body (no --generate-notes equivalent — the
	// documented divergence) and no target_commitish (the release anchors to
	// the already-pushed tag).
	assertWrite(t, seen, 0, http.MethodPost, "/repos/"+fjTestRepo+"/releases", map[string]any{
		"tag_name": "v1.2.3",
		"name":     "Release v1.2.3",
	})
}

// --- the one write that stays guarded ---------------------------------------------

func TestForgejoWriteMarkPRReadyStaysGuarded(t *testing.T) {
	c, seen, ghCalls := newForgejoWriteClient(t, fjWriteRoute(t, map[string]fjWriteResp{}))
	err := c.MarkPRReady(1)
	if err == nil {
		t.Fatal("MarkPRReady must fail loud on forgejo — EditPullRequestOption has no draft toggle on 16.0.1")
	}
	if !errors.Is(err, ErrForgejoNotSupported) {
		t.Fatalf("err = %v, want ErrForgejoNotSupported (draft semantics land with M7)", err)
	}
	if len(*seen) != 0 {
		t.Fatalf("requests = %+v, want none — the guard must fire before any transport touch", *seen)
	}
	if ghCalls.Load() != 0 {
		t.Fatalf("gh runner invoked %d time(s), want 0", ghCalls.Load())
	}
}
