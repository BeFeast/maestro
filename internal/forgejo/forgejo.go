// Package forgejo implements forge.Client against the Forgejo REST API
// (#1162 S3). The endpoint contract below was verified live against Forgejo
// 16.0.1; the two gotchas that differ from GitHub are baked in here so callers
// never see them:
//
//   - the combined-status read reports each status's state in a `.status`
//     field, not `.state` — normalized to forge.StatusState on read;
//   - there is no per-line inline-comment endpoint — an inline comment is a
//     one-comment review (POST /pulls/{index}/reviews, event COMMENT, the
//     line as new_position).
package forgejo

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/befeast/maestro/internal/forge"
)

// requestTimeout bounds one REST round trip when the caller's context carries
// no earlier deadline, mirroring the gh transport's per-call bound so a hung
// forge cannot wedge a producer cycle.
const requestTimeout = 30 * time.Second

// maxResponseBytes bounds one response body. Exceeding it is an explicit
// error, never a silent cut: a truncated diff with a nil error would let the
// producer post a full-diff verdict over a partial diff with no truncation
// note — the exact evidence-weakening the bash glue's TRUNC_NOTE surfaces.
const maxResponseBytes = 4 << 20

// Client is the Forgejo forge. Zero value is not usable; construct with New.
type Client struct {
	baseURL string
	token   string
	httpc   *http.Client
}

var _ forge.Client = (*Client)(nil)

// New returns a Forgejo client for one API root, e.g. "https://host/api/v1".
// token is the PAT sent as `Authorization: token <PAT>`; the caller resolves
// it (config *_env indirection) — it is never read from the environment here.
func New(baseURL, token string) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		token:   token,
		httpc: &http.Client{
			Timeout: requestTimeout,
			// Never follow redirects: Go rewrites a redirected POST into a
			// body-less GET, and Forgejo's write paths have 200-answering GET
			// twins (list statuses/comments) — a misconfigured base URL behind
			// an http→https 301 would turn every write into a silent green
			// no-op. Returning the 3xx as-is makes do() fail loudly instead.
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

// WithHTTPClient overrides the underlying HTTP client (tests, custom TLS).
// The caller then owns the redirect policy New installs by default.
func (c *Client) WithHTTPClient(httpc *http.Client) *Client {
	c.httpc = httpc
	return c
}

// do runs one request and returns the response body. Any non-2xx status is an
// error carrying a bounded excerpt of the response body — Forgejo's error
// JSON is short and is exactly what distinguishes "line outside the diff"
// from "bad token" for the caller's fallback decision.
func (c *Client) do(ctx context.Context, method, path string, payload any) ([]byte, error) {
	var body io.Reader
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			return nil, fmt.Errorf("encode %s %s: %w", method, path, err)
		}
		body = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
	if err != nil {
		return nil, fmt.Errorf("build %s %s: %w", method, path, err)
	}
	if c.token != "" {
		req.Header.Set("Authorization", "token "+c.token)
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.httpc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	// limit+1 so an at-limit body is distinguishable from an over-limit one
	// (the appauth.go idiom) — oversize fails loudly instead of truncating.
	out, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("%s %s: read response: %w", method, path, err)
	}
	if len(out) > maxResponseBytes {
		return nil, fmt.Errorf("%s %s: response exceeds %d bytes", method, path, maxResponseBytes)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		excerpt := strings.TrimSpace(string(out))
		if len(excerpt) > 512 {
			excerpt = excerpt[:512]
		}
		return nil, fmt.Errorf("%s %s: HTTP %d: %s", method, path, resp.StatusCode, excerpt)
	}
	return out, nil
}

// GetPR returns the PR metadata. An empty head SHA is rejected per the
// forge.Client contract — every downstream op anchors to it.
func (c *Client) GetPR(ctx context.Context, repo string, index int) (forge.PR, error) {
	out, err := c.do(ctx, http.MethodGet, fmt.Sprintf("/repos/%s/pulls/%d", repo, index), nil)
	if err != nil {
		return forge.PR{}, fmt.Errorf("get PR %s#%d: %w", repo, index, err)
	}
	var pull struct {
		Number int    `json:"number"`
		Title  string `json:"title"`
		Head   struct {
			SHA string `json:"sha"`
		} `json:"head"`
		Base struct {
			Ref string `json:"ref"`
		} `json:"base"`
	}
	if err := json.Unmarshal(out, &pull); err != nil {
		return forge.PR{}, fmt.Errorf("parse PR %s#%d: %w", repo, index, err)
	}
	sha := strings.TrimSpace(pull.Head.SHA)
	if sha == "" {
		return forge.PR{}, fmt.Errorf("PR %s#%d has an empty head sha", repo, index)
	}
	return forge.PR{
		Number:  pull.Number,
		Title:   pull.Title,
		HeadSHA: sha,
		BaseRef: pull.Base.Ref,
	}, nil
}

// GetPRDiff returns the PR's unified diff via the `.diff` media path (the
// files listing endpoint is NOT equivalent — it paginates and re-encodes).
func (c *Client) GetPRDiff(ctx context.Context, repo string, index int) ([]byte, error) {
	out, err := c.do(ctx, http.MethodGet, fmt.Sprintf("/repos/%s/pulls/%d.diff", repo, index), nil)
	if err != nil {
		return nil, fmt.Errorf("get PR %s#%d diff: %w", repo, index, err)
	}
	return out, nil
}

// CommitStatuses returns the statuses on one SHA from the combined-status
// endpoint (latest per context, so no dedup). Forgejo reports the per-status
// state in `.status` — NOT `.state`, which unlike GitHub is absent here; that
// read-side divergence is normalized to forge.StatusState at this boundary.
func (c *Client) CommitStatuses(ctx context.Context, repo, sha string) ([]forge.Status, error) {
	out, err := c.do(ctx, http.MethodGet, fmt.Sprintf("/repos/%s/commits/%s/status", repo, sha), nil)
	if err != nil {
		return nil, fmt.Errorf("commit statuses for %s@%s: %w", repo, sha, err)
	}
	var combined struct {
		Statuses []struct {
			Context     string `json:"context"`
			Status      string `json:"status"`
			Description string `json:"description"`
			TargetURL   string `json:"target_url"`
			CreatedAt   string `json:"created_at"`
		} `json:"statuses"`
	}
	if err := json.Unmarshal(out, &combined); err != nil {
		return nil, fmt.Errorf("parse commit statuses for %s@%s: %w", repo, sha, err)
	}
	statuses := make([]forge.Status, 0, len(combined.Statuses))
	for _, st := range combined.Statuses {
		createdAt, err := time.Parse(time.RFC3339, strings.TrimSpace(st.CreatedAt))
		if err != nil {
			createdAt = time.Time{}
		}
		statuses = append(statuses, forge.Status{
			Context:     st.Context,
			State:       forge.StatusState(strings.ToLower(strings.TrimSpace(st.Status))),
			Description: st.Description,
			TargetURL:   st.TargetURL,
			CreatedAt:   createdAt,
		})
	}
	return statuses, nil
}

// CreateCommitStatus posts one commit status. The write vocabulary is the
// shared pending|success|error|failure set. Errors surface unswallowed — the
// producer's pending-first protocol depends on a failed post being visible.
func (c *Client) CreateCommitStatus(ctx context.Context, repo, sha string, status forge.Status) error {
	if status.Context == "" || status.State == "" {
		return fmt.Errorf("create status on %s@%s: context and state are required", repo, sha)
	}
	payload := map[string]string{
		"state":       string(status.State),
		"context":     status.Context,
		"description": status.Description,
	}
	if status.TargetURL != "" {
		payload["target_url"] = status.TargetURL
	}
	if _, err := c.do(ctx, http.MethodPost, fmt.Sprintf("/repos/%s/statuses/%s", repo, sha), payload); err != nil {
		return fmt.Errorf("create status %s=%s on %s@%s: %w", status.Context, status.State, repo, sha, err)
	}
	return nil
}

// CreateReviewComment posts one inline finding as a single-comment COMMENT
// review — Forgejo has no per-line comment endpoint. new_position is the
// new-file line in the diff; an anchor Forgejo rejects surfaces as an error
// so the caller falls back to CreateComment instead of losing the finding.
func (c *Client) CreateReviewComment(ctx context.Context, repo string, index int, sha, path string, line int, body string) error {
	payload := map[string]any{
		"commit_id": sha,
		"event":     "COMMENT",
		"comments": []map[string]any{{
			"path":         path,
			"new_position": line,
			"body":         body,
		}},
	}
	if _, err := c.do(ctx, http.MethodPost, fmt.Sprintf("/repos/%s/pulls/%d/reviews", repo, index), payload); err != nil {
		return fmt.Errorf("create review comment on %s#%d %s:%d: %w", repo, index, path, line, err)
	}
	return nil
}

// CreateComment posts a plain PR comment (PRs share the issue number space).
func (c *Client) CreateComment(ctx context.Context, repo string, index int, body string) error {
	payload := map[string]string{"body": body}
	if _, err := c.do(ctx, http.MethodPost, fmt.Sprintf("/repos/%s/issues/%d/comments", repo, index), payload); err != nil {
		return fmt.Errorf("create comment on %s#%d: %w", repo, index, err)
	}
	return nil
}
