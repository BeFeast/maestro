package forgejo

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/befeast/maestro/internal/forge"
)

// recordedRequest captures one request the fixture server saw.
type recordedRequest struct {
	Method string
	Path   string
	Auth   string
	Body   []byte
}

// fixtureServer answers every request with the given status and body and
// records what it saw. Fixtures mirror the live-verified Forgejo 16.0.1
// payload shapes.
func fixtureServer(t *testing.T, status int, body string) (*Client, *[]recordedRequest) {
	t.Helper()
	var seen []recordedRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		payload, _ := io.ReadAll(r.Body)
		seen = append(seen, recordedRequest{
			Method: r.Method,
			Path:   r.URL.Path,
			Auth:   r.Header.Get("Authorization"),
			Body:   payload,
		})
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return New(srv.URL, "sekrit"), &seen
}

func lastRequest(t *testing.T, seen *[]recordedRequest) recordedRequest {
	t.Helper()
	if len(*seen) == 0 {
		t.Fatal("no request reached the fixture server")
	}
	return (*seen)[len(*seen)-1]
}

func TestGetPR(t *testing.T) {
	c, seen := fixtureServer(t, 200,
		`{"number":3,"title":"canary","head":{"ref":"canary","sha":" fjsha1 "},"base":{"ref":"main"}}`)
	pr, err := c.GetPR(context.Background(), "owner/repo", 3)
	if err != nil {
		t.Fatalf("GetPR: %v", err)
	}
	want := forge.PR{Number: 3, Title: "canary", HeadSHA: "fjsha1", BaseRef: "main"}
	if pr != want {
		t.Fatalf("GetPR = %+v, want %+v", pr, want)
	}
	req := lastRequest(t, seen)
	if req.Method != http.MethodGet || req.Path != "/repos/owner/repo/pulls/3" {
		t.Fatalf("request = %s %s", req.Method, req.Path)
	}
	if req.Auth != "token sekrit" {
		t.Fatalf("auth header = %q, want the token scheme", req.Auth)
	}
}

func TestGetPR_EmptyHeadSHA(t *testing.T) {
	c, _ := fixtureServer(t, 200, `{"number":3,"title":"t","head":{"sha":""},"base":{"ref":"main"}}`)
	if _, err := c.GetPR(context.Background(), "owner/repo", 3); err == nil {
		t.Fatal("an empty head sha must be rejected — every downstream op anchors to it")
	}
}

func TestGetPR_HTTPError(t *testing.T) {
	c, _ := fixtureServer(t, 404, `{"message":"Not Found"}`)
	if _, err := c.GetPR(context.Background(), "owner/repo", 3); err == nil {
		t.Fatal("a non-2xx response must surface as an error")
	}
}

func TestGetPRDiff(t *testing.T) {
	diff := "diff --git a/x.kt b/x.kt\n+not json\n"
	c, seen := fixtureServer(t, 200, diff)
	out, err := c.GetPRDiff(context.Background(), "owner/repo", 3)
	if err != nil {
		t.Fatalf("GetPRDiff: %v", err)
	}
	if string(out) != diff {
		t.Fatalf("diff = %q, want raw passthrough", out)
	}
	req := lastRequest(t, seen)
	if req.Path != "/repos/owner/repo/pulls/3.diff" {
		t.Fatalf("path = %s, want the .diff media path", req.Path)
	}
}

func TestCommitStatuses_ReadsStatusFieldNotState(t *testing.T) {
	// The live-verified 16.0.1 gotcha: the per-status state lives in
	// `.status`. The fixture plants a contradictory `.state` decoy so a
	// regression to the GitHub field name fails loudly.
	c, seen := fixtureServer(t, 200, `{"state":"failure","statuses":[
		{"context":"llm-review-opus","status":"success","state":"failure","description":"ok","target_url":"https://x","created_at":"2026-08-10T12:00:00Z"},
		{"context":"llm-review-cursor","status":"PENDING","state":"failure","created_at":"not-a-time"}]}`)
	statuses, err := c.CommitStatuses(context.Background(), "owner/repo", "fjsha1")
	if err != nil {
		t.Fatalf("CommitStatuses: %v", err)
	}
	if len(statuses) != 2 {
		t.Fatalf("statuses = %d, want 2", len(statuses))
	}
	first := statuses[0]
	if first.State != forge.StatusSuccess {
		t.Fatalf("first state = %q — the impl must read Forgejo's .status, never .state", first.State)
	}
	if first.Context != "llm-review-opus" || first.Description != "ok" || first.TargetURL != "https://x" {
		t.Fatalf("first status = %+v", first)
	}
	if want := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC); !first.CreatedAt.Equal(want) {
		t.Fatalf("first CreatedAt = %v, want %v", first.CreatedAt, want)
	}
	second := statuses[1]
	if second.State != forge.StatusPending {
		t.Fatalf("second state = %q, want pending (normalized lowercase)", second.State)
	}
	if !second.CreatedAt.IsZero() {
		t.Fatalf("an unparseable created_at must map to zero, got %v", second.CreatedAt)
	}
	req := lastRequest(t, seen)
	if req.Path != "/repos/owner/repo/commits/fjsha1/status" {
		t.Fatalf("path = %s", req.Path)
	}
}

func TestCreateCommitStatus(t *testing.T) {
	c, seen := fixtureServer(t, 201, `{}`)
	err := c.CreateCommitStatus(context.Background(), "owner/repo", "fjsha1", forge.Status{
		Context:     "llm-review-opus",
		State:       forge.StatusPending,
		Description: "review in progress",
	})
	if err != nil {
		t.Fatalf("CreateCommitStatus: %v", err)
	}
	req := lastRequest(t, seen)
	if req.Method != http.MethodPost || req.Path != "/repos/owner/repo/statuses/fjsha1" {
		t.Fatalf("request = %s %s", req.Method, req.Path)
	}
	var payload map[string]string
	if err := json.Unmarshal(req.Body, &payload); err != nil {
		t.Fatalf("payload: %v", err)
	}
	want := map[string]string{"state": "pending", "context": "llm-review-opus", "description": "review in progress"}
	if len(payload) != len(want) {
		t.Fatalf("payload = %v, want exactly %v (no empty target_url)", payload, want)
	}
	for k, v := range want {
		if payload[k] != v {
			t.Fatalf("payload[%s] = %q, want %q", k, payload[k], v)
		}
	}
}

func TestCreateCommitStatus_TargetURL(t *testing.T) {
	c, seen := fixtureServer(t, 201, `{}`)
	err := c.CreateCommitStatus(context.Background(), "owner/repo", "fjsha1", forge.Status{
		Context:   "llm-review-opus",
		State:     forge.StatusSuccess,
		TargetURL: "https://example.test/run/1",
	})
	if err != nil {
		t.Fatalf("CreateCommitStatus: %v", err)
	}
	var payload map[string]string
	if err := json.Unmarshal(lastRequest(t, seen).Body, &payload); err != nil {
		t.Fatalf("payload: %v", err)
	}
	if payload["target_url"] != "https://example.test/run/1" {
		t.Fatalf("target_url = %q", payload["target_url"])
	}
}

func TestCreateCommitStatus_ErrorPropagates(t *testing.T) {
	c, _ := fixtureServer(t, 502, `bad gateway`)
	err := c.CreateCommitStatus(context.Background(), "owner/repo", "fjsha1", forge.Status{
		Context: "llm-review-opus",
		State:   forge.StatusPending,
	})
	if err == nil {
		t.Fatal("a failed status post must surface — the pending-first protocol depends on it")
	}
}

func TestCreateCommitStatus_RequiresContextAndState(t *testing.T) {
	c, seen := fixtureServer(t, 201, `{}`)
	if err := c.CreateCommitStatus(context.Background(), "owner/repo", "s", forge.Status{State: forge.StatusPending}); err == nil {
		t.Fatal("a status without a context must be rejected")
	}
	if err := c.CreateCommitStatus(context.Background(), "owner/repo", "s", forge.Status{Context: "c"}); err == nil {
		t.Fatal("a status without a state must be rejected")
	}
	if len(*seen) != 0 {
		t.Fatalf("validation failures must not reach the API, saw %v", *seen)
	}
}

func TestCreateReviewComment(t *testing.T) {
	c, seen := fixtureServer(t, 200, `{}`)
	err := c.CreateReviewComment(context.Background(), "owner/repo", 3, "fjsha1", "a.kt", 42, "[P1] boom")
	if err != nil {
		t.Fatalf("CreateReviewComment: %v", err)
	}
	req := lastRequest(t, seen)
	if req.Method != http.MethodPost || req.Path != "/repos/owner/repo/pulls/3/reviews" {
		t.Fatalf("request = %s %s — inline comments go through the reviews endpoint only", req.Method, req.Path)
	}
	var payload struct {
		CommitID string `json:"commit_id"`
		Event    string `json:"event"`
		Comments []struct {
			Path        string `json:"path"`
			NewPosition int    `json:"new_position"`
			Body        string `json:"body"`
		} `json:"comments"`
	}
	if err := json.Unmarshal(req.Body, &payload); err != nil {
		t.Fatalf("payload: %v", err)
	}
	if payload.CommitID != "fjsha1" || payload.Event != "COMMENT" {
		t.Fatalf("payload = %+v", payload)
	}
	if len(payload.Comments) != 1 {
		t.Fatalf("comments = %d, want exactly 1 (one finding per review)", len(payload.Comments))
	}
	cm := payload.Comments[0]
	if cm.Path != "a.kt" || cm.NewPosition != 42 || cm.Body != "[P1] boom" {
		t.Fatalf("comment = %+v", cm)
	}
}

func TestCreateReviewComment_RejectedAnchorPropagates(t *testing.T) {
	c, _ := fixtureServer(t, 422, `{"message":"new_position is not in the diff"}`)
	err := c.CreateReviewComment(context.Background(), "owner/repo", 3, "fjsha1", "a.kt", 9999, "[P1] boom")
	if err == nil {
		t.Fatal("a rejected inline anchor must surface as an error — it is the caller's CreateComment fallback trigger")
	}
}

func TestCreateComment(t *testing.T) {
	c, seen := fixtureServer(t, 201, `{}`)
	if err := c.CreateComment(context.Background(), "owner/repo", 3, "[P1] boom (at `a.kt:9999`)"); err != nil {
		t.Fatalf("CreateComment: %v", err)
	}
	req := lastRequest(t, seen)
	if req.Method != http.MethodPost || req.Path != "/repos/owner/repo/issues/3/comments" {
		t.Fatalf("request = %s %s", req.Method, req.Path)
	}
	var payload map[string]string
	if err := json.Unmarshal(req.Body, &payload); err != nil {
		t.Fatalf("payload: %v", err)
	}
	if payload["body"] != "[P1] boom (at `a.kt:9999`)" {
		t.Fatalf("body = %q", payload["body"])
	}
}

func TestCreateComment_ErrorPropagates(t *testing.T) {
	c, _ := fixtureServer(t, 500, `oops`)
	if err := c.CreateComment(context.Background(), "owner/repo", 3, "b"); err == nil {
		t.Fatal("a failed fallback comment must not read as posted")
	}
}

func TestContextCanceled(t *testing.T) {
	c, seen := fixtureServer(t, 200, `{}`)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.GetPR(ctx, "owner/repo", 3); err == nil {
		t.Fatal("GetPR must fail on a canceled context")
	}
	if err := c.CreateComment(ctx, "owner/repo", 3, "b"); err == nil {
		t.Fatal("CreateComment must fail on a canceled context")
	}
	if len(*seen) != 0 {
		t.Fatalf("a canceled context must not reach the API, saw %v", *seen)
	}
}
