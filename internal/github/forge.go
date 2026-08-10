package github

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/befeast/maestro/internal/forge"
)

// Forge implements forge.Client for GitHub on this package's hardened gh
// transport (#1162 S2): rate-limit classification with backoff, the primary
// -limit fail-fast gate, conditional-request caching on the eligible reads,
// App-auth injection, and the bounded subprocess fence all apply unchanged.
// It is stateless — the repo travels per call, so one value serves every
// project flow.
//
// Context handling matches the rest of this package: ctx is checked before
// dispatch, and the call itself is bounded by the transport's own deadline
// (ghTimeout per attempt) rather than by mid-flight ctx propagation.
type Forge struct{}

// NewForge returns the GitHub forge client.
func NewForge() *Forge { return &Forge{} }

var _ forge.Client = (*Forge)(nil)

// GetPR returns the PR metadata the review producer keys on. An empty head
// SHA is rejected here — every downstream op (statuses, inline comments)
// anchors to it, so serving one would only defer the failure.
func (f *Forge) GetPR(ctx context.Context, repo string, index int) (forge.PR, error) {
	if err := ctx.Err(); err != nil {
		return forge.PR{}, err
	}
	out, err := ghAPI(fmt.Sprintf("repos/%s/pulls/%d", repo, index))
	if err != nil {
		return forge.PR{}, fmt.Errorf("get PR %s#%d: %w", repo, index, err)
	}
	var pull restPull
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

// GetPRDiff returns the PR's unified diff. The Accept override makes gh api
// return the diff media type; its presence in the args also keeps the call off
// the conditional-request path, so the raw (non-JSON) body passes through the
// transport untouched.
func (f *Forge) GetPRDiff(ctx context.Context, repo string, index int) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	out, err := ghAPIWithArgs(fmt.Sprintf("repos/%s/pulls/%d", repo, index),
		"-H", "Accept: application/vnd.github.diff")
	if err != nil {
		return nil, fmt.Errorf("get PR %s#%d diff: %w", repo, index, err)
	}
	return out, nil
}

// restCommitStatus is the per-status element of the combined-status payload,
// carrying the fields combinedStatusResponse drops (created_at for the
// producer's stale-pending self-heal, description and target_url for
// round-tripping).
type restCommitStatus struct {
	Context     string `json:"context"`
	State       string `json:"state"`
	Description string `json:"description"`
	TargetURL   string `json:"target_url"`
	CreatedAt   string `json:"created_at"`
}

// CommitStatuses returns the statuses on one SHA. The combined-status endpoint
// reports the latest status per context, so the result needs no dedup. A
// created_at that is absent or does not parse maps to a zero CreatedAt — the
// forge.Status contract for "no usable age".
func (f *Forge) CommitStatuses(ctx context.Context, repo, sha string) ([]forge.Status, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	out, err := ghAPI(fmt.Sprintf("repos/%s/commits/%s/status", repo, sha))
	if err != nil {
		return nil, fmt.Errorf("commit statuses for %s@%s: %w", repo, sha, err)
	}
	var combined struct {
		Statuses []restCommitStatus `json:"statuses"`
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
			State:       forge.StatusState(strings.ToLower(strings.TrimSpace(st.State))),
			Description: st.Description,
			TargetURL:   st.TargetURL,
			CreatedAt:   createdAt,
		})
	}
	return statuses, nil
}

// CreateCommitStatus posts one commit status. Errors must reach the caller
// unswallowed: the producer's pending-first protocol aborts a run whose
// pending post failed, and a transient error here reading as success is
// exactly the silent no-op the bash glue was hardened against.
func (f *Forge) CreateCommitStatus(ctx context.Context, repo, sha string, status forge.Status) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if status.Context == "" || status.State == "" {
		return fmt.Errorf("create status on %s@%s: context and state are required", repo, sha)
	}
	args := []string{
		"-f", "state=" + string(status.State),
		"-f", "context=" + status.Context,
		"-f", "description=" + status.Description,
	}
	if status.TargetURL != "" {
		args = append(args, "-f", "target_url="+status.TargetURL)
	}
	if _, err := ghAPIWithArgs(fmt.Sprintf("repos/%s/statuses/%s", repo, sha), args...); err != nil {
		return fmt.Errorf("create status %s=%s on %s@%s: %w", status.Context, status.State, repo, sha, err)
	}
	return nil
}

// CreateReviewComment posts an inline comment on a new-file line of the diff
// at the given head. A rejected anchor (line outside the diff, renamed file)
// returns an error; the caller owns the fall-back to CreateComment.
func (f *Forge) CreateReviewComment(ctx context.Context, repo string, index int, sha, path string, line int, body string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	_, err := ghAPIWithArgs(fmt.Sprintf("repos/%s/pulls/%d/comments", repo, index),
		"-f", "body="+body,
		"-f", "commit_id="+sha,
		"-f", "path="+path,
		"-F", fmt.Sprintf("line=%d", line),
		"-f", "side=RIGHT")
	if err != nil {
		return fmt.Errorf("create review comment on %s#%d %s:%d: %w", repo, index, path, line, err)
	}
	return nil
}

// CreateComment posts a plain PR comment via the issues endpoint (PRs share
// the issue number space).
func (f *Forge) CreateComment(ctx context.Context, repo string, index int, body string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	_, err := ghAPIWithArgs(fmt.Sprintf("repos/%s/issues/%d/comments", repo, index),
		"-f", "body="+body)
	if err != nil {
		return fmt.Errorf("create comment on %s#%d: %w", repo, index, err)
	}
	return nil
}
