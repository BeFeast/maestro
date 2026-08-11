package forgejo

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

// writeReq captures one request the writes fixture server saw, including the
// DECODED JSON payload — every write test asserts method + path + exact
// payload fields, so a stray or missing wire field fails the test, not the
// live forge.
type writeReq struct {
	Method string
	Path   string
	Query  url.Values
	// JSON is the decoded request body; nil when the request carried none.
	JSON map[string]any
}

func newWritesClient(t *testing.T, fn func(r *http.Request) (int, string)) (*Client, *[]writeReq) {
	t.Helper()
	var seen []writeReq
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
		seen = append(seen, writeReq{
			Method: r.Method,
			Path:   r.URL.EscapedPath(),
			Query:  r.URL.Query(),
			JSON:   decoded,
		})
		status, body := fn(r)
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return New(srv.URL, "sekrit"), &seen
}

// staticWrites is newWritesClient with one fixed response.
func staticWrites(t *testing.T, status int, body string) (*Client, *[]writeReq) {
	t.Helper()
	return newWritesClient(t, func(*http.Request) (int, string) { return status, body })
}

// assertOneWrite asserts the fixture saw exactly one request with the given
// method, path, and exact decoded payload (nil wantJSON = no body at all).
func assertOneWrite(t *testing.T, seen *[]writeReq, method, path string, wantJSON map[string]any) writeReq {
	t.Helper()
	if len(*seen) != 1 {
		t.Fatalf("requests = %d, want 1: %+v", len(*seen), *seen)
	}
	req := (*seen)[0]
	if req.Method != method || req.Path != path {
		t.Fatalf("request = %s %s, want %s %s", req.Method, req.Path, method, path)
	}
	if !reflect.DeepEqual(req.JSON, wantJSON) {
		t.Fatalf("payload = %#v, want %#v", req.JSON, wantJSON)
	}
	return req
}

func TestCreatePull(t *testing.T) {
	// The number comes from the response JSON `number`, never from html_url —
	// Forgejo web URLs are /pulls/N where gh scrapes /pull/N; the decoy URL
	// carries a different number to catch any scraping regression.
	c, seen := staticWrites(t, 201,
		`{"id":99,"number":7,"html_url":"https://forge/o/r/pulls/1","state":"open","draft":false}`)
	n, err := c.CreatePull(context.Background(), "owner/repo", "feat: title", "the body", "main", "feat/x")
	if err != nil {
		t.Fatalf("CreatePull: %v", err)
	}
	if n != 7 {
		t.Fatalf("number = %d, want 7 (from response JSON, not the html_url decoy)", n)
	}
	assertOneWrite(t, seen, http.MethodPost, "/repos/owner/repo/pulls", map[string]any{
		"title": "feat: title",
		"body":  "the body",
		"base":  "main",
		"head":  "feat/x",
	})
}

func TestCreatePull_MissingNumberFailsLoud(t *testing.T) {
	c, _ := staticWrites(t, 201, `{"id":99}`)
	if _, err := c.CreatePull(context.Background(), "owner/repo", "t", "b", "main", "h"); err == nil {
		t.Fatal("a created pull without a number must error — downstream keys off it")
	}
}

func TestCreatePull_HTTPError(t *testing.T) {
	c, _ := staticWrites(t, 422, `{"message":"head branch does not exist","url":""}`)
	_, err := c.CreatePull(context.Background(), "owner/repo", "t", "b", "main", "gone")
	if err == nil {
		t.Fatal("a 422 must surface as an error")
	}
	if !strings.Contains(err.Error(), "HTTP 422") || !strings.Contains(err.Error(), "head branch does not exist") {
		t.Fatalf("error must carry status and body, got: %v", err)
	}
}

func TestEditPull_Body(t *testing.T) {
	c, seen := staticWrites(t, 201, `{}`)
	body := "replaced"
	if err := c.EditPull(context.Background(), "owner/repo", 5, Edit{Body: &body}); err != nil {
		t.Fatalf("EditPull: %v", err)
	}
	// Full replace of body ONLY — a stray state field would close/reopen.
	assertOneWrite(t, seen, http.MethodPatch, "/repos/owner/repo/pulls/5", map[string]any{"body": "replaced"})
}

func TestEditPull_EmptyBodyReplaceIsSent(t *testing.T) {
	// Body is *string: non-nil empty means "replace with empty", and the
	// field must go on the wire.
	c, seen := staticWrites(t, 201, `{}`)
	body := ""
	if err := c.EditPull(context.Background(), "owner/repo", 5, Edit{Body: &body}); err != nil {
		t.Fatalf("EditPull: %v", err)
	}
	assertOneWrite(t, seen, http.MethodPatch, "/repos/owner/repo/pulls/5", map[string]any{"body": ""})
}

func TestEditPull_Close(t *testing.T) {
	c, seen := staticWrites(t, 201, `{}`)
	if err := c.EditPull(context.Background(), "owner/repo", 5, Edit{State: "closed"}); err != nil {
		t.Fatalf("EditPull: %v", err)
	}
	assertOneWrite(t, seen, http.MethodPatch, "/repos/owner/repo/pulls/5", map[string]any{"state": "closed"})
}

func TestEditPull_EmptyEditIsError(t *testing.T) {
	c, seen := staticWrites(t, 201, `{}`)
	if err := c.EditPull(context.Background(), "owner/repo", 5, Edit{}); err == nil {
		t.Fatal("a field-less PATCH must be rejected — it would be a silent green no-op")
	}
	if len(*seen) != 0 {
		t.Fatalf("an empty edit must not reach the API, saw %+v", *seen)
	}
}

func TestEditIssue_BodyAndClose(t *testing.T) {
	c, seen := staticWrites(t, 201, `{}`)
	body := "new body"
	if err := c.EditIssue(context.Background(), "owner/repo", 36, Edit{Body: &body}); err != nil {
		t.Fatalf("EditIssue body: %v", err)
	}
	if err := c.EditIssue(context.Background(), "owner/repo", 36, Edit{State: "closed"}); err != nil {
		t.Fatalf("EditIssue close: %v", err)
	}
	if len(*seen) != 2 {
		t.Fatalf("requests = %d, want 2", len(*seen))
	}
	for i, want := range []map[string]any{{"body": "new body"}, {"state": "closed"}} {
		req := (*seen)[i]
		if req.Method != http.MethodPatch || req.Path != "/repos/owner/repo/issues/36" {
			t.Fatalf("request %d = %s %s", i, req.Method, req.Path)
		}
		if !reflect.DeepEqual(req.JSON, want) {
			t.Fatalf("payload %d = %#v, want %#v", i, req.JSON, want)
		}
	}
}

func TestEditIssue_EmptyEditIsError(t *testing.T) {
	c, seen := staticWrites(t, 201, `{}`)
	if err := c.EditIssue(context.Background(), "owner/repo", 36, Edit{}); err == nil {
		t.Fatal("a field-less PATCH must be rejected")
	}
	if len(*seen) != 0 {
		t.Fatalf("an empty edit must not reach the API, saw %+v", *seen)
	}
}

func TestMergePull(t *testing.T) {
	c, seen := staticWrites(t, 200, ``)
	err := c.MergePull(context.Background(), "owner/repo", 9, MergeOptions{
		Do:                     "squash",
		HeadCommitID:           "59e99c49c27d3e2f73bae1657f07cd2f9a15f926",
		DeleteBranchAfterMerge: true,
	})
	if err != nil {
		t.Fatalf("MergePull: %v", err)
	}
	// The exact mixed wire casing of MergePullRequestOption: Go-cased `Do`,
	// snake `head_commit_id`/`delete_branch_after_merge`.
	assertOneWrite(t, seen, http.MethodPost, "/repos/owner/repo/pulls/9/merge", map[string]any{
		"Do":                        "squash",
		"head_commit_id":            "59e99c49c27d3e2f73bae1657f07cd2f9a15f926",
		"delete_branch_after_merge": true,
	})
}

func TestMergePull_OmitsEmptyHeadCommitID(t *testing.T) {
	c, seen := staticWrites(t, 200, ``)
	if err := c.MergePull(context.Background(), "owner/repo", 9, MergeOptions{Do: "merge"}); err != nil {
		t.Fatalf("MergePull: %v", err)
	}
	assertOneWrite(t, seen, http.MethodPost, "/repos/owner/repo/pulls/9/merge", map[string]any{
		"Do":                        "merge",
		"delete_branch_after_merge": false,
	})
}

func TestMergePull_EmptyDoIsError(t *testing.T) {
	c, seen := staticWrites(t, 200, ``)
	if err := c.MergePull(context.Background(), "owner/repo", 9, MergeOptions{}); err == nil {
		t.Fatal("Do is swagger-required — an empty strategy must fail pre-flight")
	}
	if len(*seen) != 0 {
		t.Fatalf("a pre-flight failure must not reach the API, saw %+v", *seen)
	}
}

func TestMergePull_HeadMismatch409IsClassified(t *testing.T) {
	// The live Forgejo head_commit_id-mismatch refusal: 409 with an APIError
	// body whose text is the out-of-date family.
	c, _ := staticWrites(t, 409, `{"message":"head commit is out of date","url":""}`)
	err := c.MergePull(context.Background(), "owner/repo", 9, MergeOptions{Do: "squash", HeadCommitID: "abc"})
	if err == nil {
		t.Fatal("a 409 refusal must surface as an error")
	}
	if !errors.Is(err, ErrMergeOutOfDate) {
		t.Fatalf("a head-mismatch 409 must wrap ErrMergeOutOfDate, got: %v", err)
	}
	// The raw refusal must stay in the chain — classification adds, never
	// replaces.
	if !strings.Contains(err.Error(), "HTTP 409") || !strings.Contains(err.Error(), "head commit is out of date") {
		t.Fatalf("error must keep the raw status and body, got: %v", err)
	}
	var se *StatusError
	if !errors.As(err, &se) || se.StatusCode != 409 {
		t.Fatalf("StatusError must survive the wrap chain, got: %v", err)
	}
}

func TestMergePull_405OutOfDateBodyIsClassified(t *testing.T) {
	c, _ := staticWrites(t, 405, `{"message":"Please update your branch: it is not up to date with the base branch","url":""}`)
	err := c.MergePull(context.Background(), "owner/repo", 9, MergeOptions{Do: "squash"})
	if !errors.Is(err, ErrMergeOutOfDate) {
		t.Fatalf("a 405 with out-of-date body text must wrap ErrMergeOutOfDate, got: %v", err)
	}
}

func TestMergePull_Bodyless405StaysRaw(t *testing.T) {
	// The swagger documents 405 with an EMPTY body ("APIEmpty") — with no
	// text to inspect, classification must NOT fire (never classify blindly):
	// the caller gets the loud raw error, not the AutoRebase sentinel.
	c, _ := staticWrites(t, 405, ``)
	err := c.MergePull(context.Background(), "owner/repo", 9, MergeOptions{Do: "squash"})
	if err == nil {
		t.Fatal("a 405 refusal must surface as an error")
	}
	if errors.Is(err, ErrMergeOutOfDate) {
		t.Fatalf("a bodyless 405 must stay unclassified, got: %v", err)
	}
}

func TestMergePull_Conflict409StaysRaw(t *testing.T) {
	// A 409 that is NOT the out-of-date family (e.g. a merge conflict) must
	// not trigger AutoRebase semantics.
	c, _ := staticWrites(t, 409, `{"message":"merge conflict between base and head","url":""}`)
	err := c.MergePull(context.Background(), "owner/repo", 9, MergeOptions{Do: "squash"})
	if err == nil {
		t.Fatal("a 409 refusal must surface as an error")
	}
	if errors.Is(err, ErrMergeOutOfDate) {
		t.Fatalf("an unrelated 409 body must stay unclassified, got: %v", err)
	}
}

func TestMergePull_OutOfDateOnNon405409StaysRaw(t *testing.T) {
	// Classification is gated on the two documented refusal codes: matching
	// body text on any other status (here a 500 echoing the phrase) must not
	// mint the sentinel.
	c, _ := staticWrites(t, 500, `{"message":"internal: out of date","url":""}`)
	err := c.MergePull(context.Background(), "owner/repo", 9, MergeOptions{Do: "squash"})
	if err == nil {
		t.Fatal("a 500 must surface as an error")
	}
	if errors.Is(err, ErrMergeOutOfDate) {
		t.Fatalf("a non-405/409 status must stay unclassified, got: %v", err)
	}
}

func TestUpdatePullBranch(t *testing.T) {
	c, seen := staticWrites(t, 200, ``)
	if err := c.UpdatePullBranch(context.Background(), "owner/repo", 8); err != nil {
		t.Fatalf("UpdatePullBranch: %v", err)
	}
	req := assertOneWrite(t, seen, http.MethodPost, "/repos/owner/repo/pulls/8/update", nil)
	// style=merge is explicit — gh-default parity, never rebase.
	if got := req.Query.Get("style"); got != "merge" {
		t.Fatalf("style = %q, want merge (full query: %v)", got, req.Query)
	}
}

func TestUpdatePullBranch_HTTPError(t *testing.T) {
	c, _ := staticWrites(t, 409, `{"message":"cannot update: conflict","url":""}`)
	if err := c.UpdatePullBranch(context.Background(), "owner/repo", 8); err == nil {
		t.Fatal("a 409 must surface as an error")
	}
}

func TestCreateIssue_ResolvesLabelNamesToIDs(t *testing.T) {
	// CreateIssueOption.labels is []int64 ("list of label ids") — names go
	// through the repo label list first, order preserved.
	c, seen := newWritesClient(t, func(r *http.Request) (int, string) {
		if r.Method == http.MethodGet {
			return 200, `[{"id":23,"name":"blocked","color":"d93f0b"},{"id":5,"name":"maestro-ready","color":"00aabb"}]`
		}
		return 201, `{"number":42}`
	})
	n, err := c.CreateIssue(context.Background(), "owner/repo", "issue title", "issue body",
		[]string{"maestro-ready", "blocked"})
	if err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}
	if n != 42 {
		t.Fatalf("number = %d, want 42", n)
	}
	if len(*seen) != 2 {
		t.Fatalf("requests = %d, want 2 (label list, then create): %+v", len(*seen), *seen)
	}
	list := (*seen)[0]
	if list.Method != http.MethodGet || list.Path != "/repos/owner/repo/labels" {
		t.Fatalf("first request = %s %s, want the repo label list", list.Method, list.Path)
	}
	if list.Query.Get("limit") != strconv.Itoa(pageSize) || list.Query.Get("page") != "1" {
		t.Fatalf("label list must be paginated, query = %v", list.Query)
	}
	create := (*seen)[1]
	if create.Method != http.MethodPost || create.Path != "/repos/owner/repo/issues" {
		t.Fatalf("second request = %s %s", create.Method, create.Path)
	}
	want := map[string]any{
		"title":  "issue title",
		"body":   "issue body",
		"labels": []any{float64(5), float64(23)}, // ids in the caller's name order
	}
	if !reflect.DeepEqual(create.JSON, want) {
		t.Fatalf("payload = %#v, want %#v", create.JSON, want)
	}
}

func TestCreateIssue_NoLabelsSkipsResolution(t *testing.T) {
	c, seen := staticWrites(t, 201, `{"number":43}`)
	n, err := c.CreateIssue(context.Background(), "owner/repo", "t", "b", nil)
	if err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}
	if n != 43 {
		t.Fatalf("number = %d, want 43", n)
	}
	// One request only — no label-list read — and no labels key at all.
	assertOneWrite(t, seen, http.MethodPost, "/repos/owner/repo/issues", map[string]any{
		"title": "t",
		"body":  "b",
	})
}

func TestCreateIssue_UnknownLabelFailsLoudBeforeCreate(t *testing.T) {
	// gh parity: an unknown label fails the create, naming the label; the
	// issue must NOT be created without it.
	c, seen := newWritesClient(t, func(r *http.Request) (int, string) {
		if r.Method == http.MethodGet {
			return 200, `[{"id":23,"name":"blocked","color":"d93f0b"}]`
		}
		return 201, `{"number":44}`
	})
	_, err := c.CreateIssue(context.Background(), "owner/repo", "t", "b", []string{"maestro-ready"})
	if err == nil {
		t.Fatal("an unknown label must abort the create")
	}
	if !strings.Contains(err.Error(), `"maestro-ready"`) {
		t.Fatalf("error must name the unknown label, got: %v", err)
	}
	if len(*seen) != 1 || (*seen)[0].Method != http.MethodGet {
		t.Fatalf("only the label list may be read — no create call, saw %+v", *seen)
	}
}

func TestCreateIssue_MissingNumberFailsLoud(t *testing.T) {
	c, _ := staticWrites(t, 201, `{}`)
	if _, err := c.CreateIssue(context.Background(), "owner/repo", "t", "b", nil); err == nil {
		t.Fatal("a created issue without a number must error")
	}
}

func TestResolveLabelIDs_ExactMatchWinsOverFold(t *testing.T) {
	c, _ := staticWrites(t, 200, `[{"id":1,"name":"Bug","color":"ededed"},{"id":2,"name":"bug","color":"ededed"}]`)
	ids, err := c.ResolveLabelIDs(context.Background(), "owner/repo", []string{"bug"})
	if err != nil {
		t.Fatalf("ResolveLabelIDs: %v", err)
	}
	if len(ids) != 1 || ids[0] != 2 {
		t.Fatalf("ids = %v, want [2] (exact match preferred over case fold)", ids)
	}
}

func TestResolveLabelIDs_CaseInsensitiveFallback(t *testing.T) {
	c, _ := staticWrites(t, 200, `[{"id":7,"name":"Maestro-Ready","color":"00aabb"}]`)
	ids, err := c.ResolveLabelIDs(context.Background(), "owner/repo", []string{"maestro-ready"})
	if err != nil {
		t.Fatalf("ResolveLabelIDs: %v", err)
	}
	if len(ids) != 1 || ids[0] != 7 {
		t.Fatalf("ids = %v, want [7] (case-folded match, mirroring the read-side hasLabel belt)", ids)
	}
}

func TestResolveLabelIDs_EmptyNamesSkipsFetch(t *testing.T) {
	c, seen := staticWrites(t, 200, `[]`)
	ids, err := c.ResolveLabelIDs(context.Background(), "owner/repo", nil)
	if err != nil || ids != nil {
		t.Fatalf("ids = %v err = %v, want nil/nil", ids, err)
	}
	if len(*seen) != 0 {
		t.Fatalf("no names must mean no fetch, saw %+v", *seen)
	}
}

func TestAddIssueLabels_PassesNamesVerbatim(t *testing.T) {
	// IssueLabelsOption accepts label NAMES on this instance — no id
	// resolution call may happen.
	c, seen := staticWrites(t, 200, `[{"id":23,"name":"blocked","color":"d93f0b"}]`)
	if err := c.AddIssueLabels(context.Background(), "owner/repo", 3, []string{"needs-info", "blocked"}); err != nil {
		t.Fatalf("AddIssueLabels: %v", err)
	}
	assertOneWrite(t, seen, http.MethodPost, "/repos/owner/repo/issues/3/labels", map[string]any{
		"labels": []any{"needs-info", "blocked"},
	})
}

func TestAddIssueLabels_EmptyIsError(t *testing.T) {
	c, seen := staticWrites(t, 200, `[]`)
	if err := c.AddIssueLabels(context.Background(), "owner/repo", 3, nil); err == nil {
		t.Fatal("an empty label add must fail pre-flight")
	}
	if len(*seen) != 0 {
		t.Fatalf("an empty add must not reach the API, saw %+v", *seen)
	}
}

func TestRemoveIssueLabel_ByNamePathEscaped(t *testing.T) {
	// The DELETE identifier is name-accepting; names carry spaces, so the
	// path segment must be percent-escaped. A 204 — including the zero-rows
	// case of a label the issue does not carry — is a no-op SUCCESS
	// (gh parity).
	c, seen := staticWrites(t, 204, ``)
	if err := c.RemoveIssueLabel(context.Background(), "owner/repo", 3, "in progress"); err != nil {
		t.Fatalf("RemoveIssueLabel: %v", err)
	}
	assertOneWrite(t, seen, http.MethodDelete, "/repos/owner/repo/issues/3/labels/in%20progress", nil)
}

func TestRemoveIssueLabel_UnknownRepoLabelFailsLoud(t *testing.T) {
	// A label that does not exist on the repo at all answers 404/422 — that
	// must stay a loud error (gh parity), never be converted into a no-op.
	c, _ := staticWrites(t, 422, `{"message":"label does not exist","url":""}`)
	err := c.RemoveIssueLabel(context.Background(), "owner/repo", 3, "ghost")
	if err == nil {
		t.Fatal("removing a label missing from the repo must error")
	}
	if !strings.Contains(err.Error(), "label does not exist") {
		t.Fatalf("error must carry the server body, got: %v", err)
	}
}

func TestRemoveIssueLabel_EmptyNameIsError(t *testing.T) {
	c, seen := staticWrites(t, 204, ``)
	if err := c.RemoveIssueLabel(context.Background(), "owner/repo", 3, "  "); err == nil {
		t.Fatal("an empty label name must fail pre-flight")
	}
	if len(*seen) != 0 {
		t.Fatalf("an empty name must not reach the API, saw %+v", *seen)
	}
}

func TestListRepoLabels(t *testing.T) {
	// Wire color is bare rrggbb (no leading '#') — carried verbatim.
	c, seen := staticWrites(t, 200,
		`[{"id":23,"name":"blocked","exclusive":false,"is_archived":false,"color":"d93f0b","description":"waiting","url":"https://forge/api/v1/repos/o/r/labels/23"}]`)
	labels, err := c.ListRepoLabels(context.Background(), "owner/repo")
	if err != nil {
		t.Fatalf("ListRepoLabels: %v", err)
	}
	want := RepoLabel{ID: 23, Name: "blocked", Color: "d93f0b", Description: "waiting"}
	if len(labels) != 1 || labels[0] != want {
		t.Fatalf("labels = %+v, want [%+v]", labels, want)
	}
	req := (*seen)[0]
	if req.Method != http.MethodGet || req.Path != "/repos/owner/repo/labels" {
		t.Fatalf("request = %s %s", req.Method, req.Path)
	}
	// The endpoint is paginated (live-verified x-total-count) — the pager
	// must drive it.
	if req.Query.Get("limit") != strconv.Itoa(pageSize) || req.Query.Get("page") != "1" {
		t.Fatalf("repo labels must be paginated, query = %v", req.Query)
	}
}

func TestListRepoLabels_Pagination(t *testing.T) {
	labelsPage := func(from, n int) string {
		items := make([]string, 0, n)
		for i := 0; i < n; i++ {
			items = append(items, `{"id":`+strconv.Itoa(from+i)+`,"name":"l`+strconv.Itoa(from+i)+`","color":"ededed"}`)
		}
		return "[" + strings.Join(items, ",") + "]"
	}
	c, seen := newWritesClient(t, func(r *http.Request) (int, string) {
		switch r.URL.Query().Get("page") {
		case "1":
			return 200, labelsPage(1, pageSize)
		case "2":
			return 200, labelsPage(1+pageSize, 2)
		default:
			return 500, `{"message":"unexpected page"}`
		}
	})
	labels, err := c.ListRepoLabels(context.Background(), "owner/repo")
	if err != nil {
		t.Fatalf("ListRepoLabels: %v", err)
	}
	if len(labels) != pageSize+2 {
		t.Fatalf("labels = %d, want %d (resolution must see the full set)", len(labels), pageSize+2)
	}
	if len(*seen) != 2 {
		t.Fatalf("requests = %d, want 2", len(*seen))
	}
}

func TestCreateLabel(t *testing.T) {
	c, seen := staticWrites(t, 201, `{"id":30,"name":"maestro-ready","color":"ededed"}`)
	if err := c.CreateLabel(context.Background(), "owner/repo", "maestro-ready", "#ededed", "dispatch gate"); err != nil {
		t.Fatalf("CreateLabel: %v", err)
	}
	assertOneWrite(t, seen, http.MethodPost, "/repos/owner/repo/labels", map[string]any{
		"name":        "maestro-ready",
		"color":       "#ededed",
		"description": "dispatch gate",
	})
}

func TestCreateLabel_OmitsEmptyDescription(t *testing.T) {
	c, seen := staticWrites(t, 201, `{"id":31,"name":"x","color":"ededed"}`)
	if err := c.CreateLabel(context.Background(), "owner/repo", "x", "ededed", ""); err != nil {
		t.Fatalf("CreateLabel: %v", err)
	}
	assertOneWrite(t, seen, http.MethodPost, "/repos/owner/repo/labels", map[string]any{
		"name":  "x",
		"color": "ededed",
	})
}

func TestCreateLabel_EmptyColorIsError(t *testing.T) {
	// CreateLabelOption REQUIRES color; the "#ededed" default belongs to the
	// CALLER (github-layer EnsureLabel), so the transport fails loud instead
	// of inventing one.
	c, seen := staticWrites(t, 201, `{}`)
	if err := c.CreateLabel(context.Background(), "owner/repo", "x", " ", "d"); err == nil {
		t.Fatal("an empty color must fail pre-flight — the swagger requires it")
	}
	if err := c.CreateLabel(context.Background(), "owner/repo", "", "ededed", ""); err == nil {
		t.Fatal("an empty name must fail pre-flight")
	}
	if len(*seen) != 0 {
		t.Fatalf("pre-flight failures must not reach the API, saw %+v", *seen)
	}
}

func TestEditLabel_SendsOnlyProvidedFields(t *testing.T) {
	c, seen := staticWrites(t, 200, `{"id":23,"name":"blocked","color":"00aabb"}`)
	if err := c.EditLabel(context.Background(), "owner/repo", 23, "00aabb", ""); err != nil {
		t.Fatalf("EditLabel: %v", err)
	}
	// EnsureLabel's update arm: only the provided fields go on the wire —
	// an absent description must NOT be cleared.
	assertOneWrite(t, seen, http.MethodPatch, "/repos/owner/repo/labels/23", map[string]any{
		"color": "00aabb",
	})
}

func TestEditLabel_EmptyEditIsError(t *testing.T) {
	c, seen := staticWrites(t, 200, `{}`)
	if err := c.EditLabel(context.Background(), "owner/repo", 23, "", ""); err == nil {
		t.Fatal("a field-less label PATCH must be rejected")
	}
	if len(*seen) != 0 {
		t.Fatalf("an empty edit must not reach the API, saw %+v", *seen)
	}
}

func TestCreateRelease(t *testing.T) {
	c, seen := staticWrites(t, 201, `{"id":12,"tag_name":"v1.2.3"}`)
	if err := c.CreateRelease(context.Background(), "owner/repo", "v1.2.3", "v1.2.3 — cadence"); err != nil {
		t.Fatalf("CreateRelease: %v", err)
	}
	// Exactly tag_name + name: no body (no --generate-notes equivalent in
	// the swagger) and no target_commitish (the tag already exists — the
	// release must anchor to it, not to a branch head).
	assertOneWrite(t, seen, http.MethodPost, "/repos/owner/repo/releases", map[string]any{
		"tag_name": "v1.2.3",
		"name":     "v1.2.3 — cadence",
	})
}

func TestCreateRelease_EmptyTagIsError(t *testing.T) {
	c, seen := staticWrites(t, 201, `{}`)
	if err := c.CreateRelease(context.Background(), "owner/repo", " ", "t"); err == nil {
		t.Fatal("tag_name is swagger-required — an empty tag must fail pre-flight")
	}
	if len(*seen) != 0 {
		t.Fatalf("a pre-flight failure must not reach the API, saw %+v", *seen)
	}
}

func TestCreateRelease_ExistingReleaseConflictPropagates(t *testing.T) {
	c, _ := staticWrites(t, 409, `{"message":"release tag already exist","url":""}`)
	err := c.CreateRelease(context.Background(), "owner/repo", "v1.2.3", "t")
	if err == nil {
		t.Fatal("a 409 (release exists) must surface as an error")
	}
	if !strings.Contains(err.Error(), "HTTP 409") {
		t.Fatalf("error must carry the status, got: %v", err)
	}
}

func TestWrites_ContextCanceled(t *testing.T) {
	c, seen := staticWrites(t, 200, ``)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.CreatePull(ctx, "owner/repo", "t", "b", "main", "h"); err == nil {
		t.Fatal("CreatePull must fail on a canceled context")
	}
	if err := c.MergePull(ctx, "owner/repo", 1, MergeOptions{Do: "squash"}); err == nil {
		t.Fatal("MergePull must fail on a canceled context")
	}
	if len(*seen) != 0 {
		t.Fatalf("a canceled context must not reach the API, saw %+v", *seen)
	}
}
