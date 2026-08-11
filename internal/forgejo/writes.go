// Write methods for the #1172 transport switch (M3). These back the
// internal/github write funnels on forgejo-mode rows; comment-then-close
// orchestration (abort on failed comment, skip on empty comment) lives in the
// github layer — this file is the REST surface only.
//
// Contract notes pinned from swagger.v1.json (Forgejo 16.0.1+gitea-1.22.0,
// fetched 2026-08-11; writes are NEVER exercised against the live instance):
//
//   - every create endpoint answers 201, merge/update answer 200 empty, the
//     label delete answers 204 — all inside do()'s 2xx window;
//   - CreatePull/CreateIssue return the number from the RESPONSE JSON —
//     never scraped from a web URL (gh scrapes /pull/N; Forgejo web URLs are
//     /pulls/N, so URL scraping would be wrong on exactly this forge);
//   - CreateIssueOption.labels is []int64 ("list of label ids") — names must
//     be resolved against the repo label list first; IssueLabelsOption.labels
//     and the DELETE {identifier} path param accept NAMES directly ("Labels
//     can be a list of integers ... or a list of strings", "name or id of the
//     label to remove") — no resolution on the add/remove path;
//   - MergePullRequestOption's wire casing is mixed and pinned verbatim:
//     `Do` and Go-cased friends vs `head_commit_id`/`delete_branch_after_merge`;
//   - the merge endpoint documents 405 with an EMPTY body and 409 with an
//     APIError body, with NO per-code out-of-date semantics — classification
//     of "head/base moved" refusals is therefore body-text-based and
//     conservative (see mergeRefusalOutOfDate); a bodyless refusal stays a
//     raw loud error, never the sentinel;
//   - CreateLabelOption REQUIRES color; the default for a caller that did not
//     provide one is the github layer's business ("#ededed" per the gh
//     parity contract), not this transport's — an empty color here is a loud
//     pre-flight error;
//   - CreateReleaseOption has no notes-generation equivalent (gh
//     --generate-notes divergence): the release body stays empty, the tag
//     must already exist (pushed by CommitAndTag) and target_commitish is
//     deliberately omitted so the release anchors to it.
package forgejo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// CreatePull opens a pull request and returns its number, taken from the
// response JSON `number` field. A response without a positive number is a
// loud error: every downstream op (labels, gates, merge) keys off it, so a
// zero would poison state instead of failing the create.
func (c *Client) CreatePull(ctx context.Context, repo, title, body, base, head string) (int, error) {
	payload := map[string]any{
		"title": title,
		"body":  body,
		"base":  base,
		"head":  head,
	}
	out, err := c.do(ctx, http.MethodPost, fmt.Sprintf("/repos/%s/pulls", repo), payload)
	if err != nil {
		return 0, fmt.Errorf("create pull on %s: %w", repo, err)
	}
	var created struct {
		Number int `json:"number"`
	}
	if err := json.Unmarshal(out, &created); err != nil {
		return 0, fmt.Errorf("parse created pull on %s: %w", repo, err)
	}
	if created.Number <= 0 {
		return 0, fmt.Errorf("create pull on %s: response carries no pull number", repo)
	}
	return created.Number, nil
}

// Edit is the body/state subset of EditPullRequestOption/EditIssueOption the
// port needs. A nil Body leaves the body unchanged; a non-nil Body is a FULL
// replace (empty string allowed). An empty State leaves the state unchanged;
// otherwise it goes on the wire verbatim — the swagger types state as a free
// string, "open"/"closed" are the live values.
type Edit struct {
	Body  *string
	State string
}

func (e Edit) payload() map[string]any {
	p := map[string]any{}
	if e.Body != nil {
		p["body"] = *e.Body
	}
	if e.State != "" {
		p["state"] = e.State
	}
	return p
}

// EditPull PATCHes one pull request. An empty edit is a loud error — a
// field-less PATCH would be a silent green no-op.
func (c *Client) EditPull(ctx context.Context, repo string, index int, edit Edit) error {
	payload := edit.payload()
	if len(payload) == 0 {
		return fmt.Errorf("edit pull %s#%d: empty edit", repo, index)
	}
	if _, err := c.do(ctx, http.MethodPatch, fmt.Sprintf("/repos/%s/pulls/%d", repo, index), payload); err != nil {
		return fmt.Errorf("edit pull %s#%d: %w", repo, index, err)
	}
	return nil
}

// EditIssue PATCHes one issue (shared number space with pulls — but the
// /issues PATCH is the issue-semantics endpoint; pulls go through EditPull).
func (c *Client) EditIssue(ctx context.Context, repo string, index int, edit Edit) error {
	payload := edit.payload()
	if len(payload) == 0 {
		return fmt.Errorf("edit issue %s#%d: empty edit", repo, index)
	}
	if _, err := c.do(ctx, http.MethodPatch, fmt.Sprintf("/repos/%s/issues/%d", repo, index), payload); err != nil {
		return fmt.Errorf("edit issue %s#%d: %w", repo, index, err)
	}
	return nil
}

// ErrMergeOutOfDate marks a merge refusal whose response body says the
// head/base moved since the caller's read (head_commit_id mismatch, branch
// out of date). The github layer maps it onto its own AutoRebase sentinel via
// errors.Is; the message deliberately contains the legacy "not up to date"
// needle as a belt for any string-matching consumer.
var ErrMergeOutOfDate = errors.New("pull head/base not up to date for merge")

// mergeRefusalOutOfDate reports whether a 405/409 merge-refusal body belongs
// to the out-of-date/head-mismatch family. The needles are the known Forgejo
// texts ("head commit is out of date" on a head_commit_id mismatch) plus the
// field name itself and the gh-style phrasing; anything else — including an
// EMPTY 405 body, which the swagger documents — stays unclassified and
// surfaces as the raw loud error. Never classify blindly.
func mergeRefusalOutOfDate(body string) bool {
	b := strings.ToLower(body)
	for _, needle := range []string{"out of date", "not up to date", "head_commit_id"} {
		if strings.Contains(b, needle) {
			return true
		}
	}
	return false
}

// MergeOptions selects the merge behavior for MergePull. The wire casing of
// MergePullRequestOption is mixed and pinned verbatim in the payload: `Do`
// is Go-cased, `head_commit_id`/`delete_branch_after_merge` are snake.
type MergeOptions struct {
	// Do is the merge strategy (swagger enum: merge, rebase, rebase-merge,
	// squash, fast-forward-only, manually-merged). Required by the swagger.
	Do string
	// HeadCommitID, when set, makes the server refuse the merge unless the
	// pull head still matches — the force-push race belt
	// (gh --match-head-commit parity). Whether it may be empty is the
	// caller's contract; this transport only omits the field when it is.
	HeadCommitID string
	// DeleteBranchAfterMerge deletes the head branch on success
	// (gh --delete-branch parity).
	DeleteBranchAfterMerge bool
}

// MergePull merges one pull request. A 405/409 refusal whose body indicates
// the out-of-date/head-mismatch family comes back wrapped with
// ErrMergeOutOfDate (the full HTTP status and body text stay in the chain);
// every other refusal — including a bodyless 405 — is the raw loud error.
func (c *Client) MergePull(ctx context.Context, repo string, index int, opts MergeOptions) error {
	if strings.TrimSpace(opts.Do) == "" {
		return fmt.Errorf("merge pull %s#%d: merge strategy (Do) is required", repo, index)
	}
	payload := map[string]any{
		"Do":                        opts.Do,
		"delete_branch_after_merge": opts.DeleteBranchAfterMerge,
	}
	if opts.HeadCommitID != "" {
		payload["head_commit_id"] = opts.HeadCommitID
	}
	if _, err := c.do(ctx, http.MethodPost, fmt.Sprintf("/repos/%s/pulls/%d/merge", repo, index), payload); err != nil {
		var se *StatusError
		if errors.As(err, &se) &&
			(se.StatusCode == http.StatusMethodNotAllowed || se.StatusCode == http.StatusConflict) &&
			mergeRefusalOutOfDate(se.Body) {
			return fmt.Errorf("merge pull %s#%d: %w: %w", repo, index, ErrMergeOutOfDate, err)
		}
		return fmt.Errorf("merge pull %s#%d: %w", repo, index, err)
	}
	return nil
}

// UpdatePullBranch merges the base branch into the pull's head branch
// (POST /update). style=merge is explicit — parity with the gh default; the
// #547 contract (the head SHA moves, stale approvals must not merge in the
// same pass) is the caller's to honor.
func (c *Client) UpdatePullBranch(ctx context.Context, repo string, index int) error {
	if _, err := c.do(ctx, http.MethodPost, fmt.Sprintf("/repos/%s/pulls/%d/update?style=merge", repo, index), nil); err != nil {
		return fmt.Errorf("update pull branch %s#%d: %w", repo, index, err)
	}
	return nil
}

// RepoLabel is one repository label. Color is the wire form: bare rrggbb
// WITHOUT a leading '#' (live-verified) — compare accordingly.
type RepoLabel struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	Color       string `json:"color"`
	Description string `json:"description"`
}

// ListRepoLabels returns every label defined on the repo. The endpoint is
// paginated (live-verified: limit honored, x-total-count present), so it goes
// through the pager, all pages — label resolution must see the full set.
func (c *Client) ListRepoLabels(ctx context.Context, repo string) ([]RepoLabel, error) {
	labels, err := listPages[RepoLabel](ctx, c, fmt.Sprintf("/repos/%s/labels", repo), true)
	if err != nil {
		return nil, fmt.Errorf("list repo labels %s: %w", repo, err)
	}
	return labels, nil
}

// ResolveLabelIDs maps label names to repo label ids, preserving order —
// CreateIssueOption.labels demands ids ("list of label ids"). An exact name
// match wins over a case-insensitive one (the fold mirrors the read side's
// hasLabel belt); a name that resolves to nothing is a loud error naming the
// label, gh parity — outcome_repair calls EnsureLabel first, so a miss here
// means a genuinely unknown label, not a race it should paper over.
func (c *Client) ResolveLabelIDs(ctx context.Context, repo string, names []string) ([]int64, error) {
	if len(names) == 0 {
		return nil, nil
	}
	labels, err := c.ListRepoLabels(ctx, repo)
	if err != nil {
		return nil, err
	}
	ids := make([]int64, 0, len(names))
	for _, name := range names {
		id, ok := findLabelID(labels, name)
		if !ok {
			return nil, fmt.Errorf("label %q does not exist on %s", name, repo)
		}
		ids = append(ids, id)
	}
	return ids, nil
}

func findLabelID(labels []RepoLabel, name string) (int64, bool) {
	for _, l := range labels {
		if l.Name == name {
			return l.ID, true
		}
	}
	for _, l := range labels {
		if strings.EqualFold(l.Name, name) {
			return l.ID, true
		}
	}
	return 0, false
}

// CreateIssue opens an issue and returns its number from the response JSON.
// Label names are resolved to ids first (order preserved) and applied at
// create time; an unknown name aborts before anything is created.
func (c *Client) CreateIssue(ctx context.Context, repo, title, body string, labels []string) (int, error) {
	ids, err := c.ResolveLabelIDs(ctx, repo, labels)
	if err != nil {
		return 0, fmt.Errorf("create issue on %s: %w", repo, err)
	}
	payload := map[string]any{
		"title": title,
		"body":  body,
	}
	if len(ids) > 0 {
		payload["labels"] = ids
	}
	out, err := c.do(ctx, http.MethodPost, fmt.Sprintf("/repos/%s/issues", repo), payload)
	if err != nil {
		return 0, fmt.Errorf("create issue on %s: %w", repo, err)
	}
	var created struct {
		Number int `json:"number"`
	}
	if err := json.Unmarshal(out, &created); err != nil {
		return 0, fmt.Errorf("parse created issue on %s: %w", repo, err)
	}
	if created.Number <= 0 {
		return 0, fmt.Errorf("create issue on %s: response carries no issue number", repo)
	}
	return created.Number, nil
}

// AddIssueLabels adds labels to an issue/PR by NAME — IssueLabelsOption
// accepts "a list of strings representing label names" on this instance, so
// no id resolution happens here (spec-pinned for #1172 M3). Adding an
// already-present label is a server-side no-op (the response is the resulting
// label list, discarded).
//
// KNOWN GAP (flagged #1172 follow-up, not fixable without leaving the spec's
// pass-the-name contract): the Gitea 1.22 lineage resolves names server-side
// and silently DROPS names that do not resolve, answering 200 with the
// unchanged list — so adding a renamed/deleted label reports success without
// applying anything, where the gh path fails loud on the same input. Client-
// side ResolveLabelIDs would restore loud parity at the cost of a read per
// add; carried as a follow-up decision, not smuggled into M3.
func (c *Client) AddIssueLabels(ctx context.Context, repo string, index int, names []string) error {
	if len(names) == 0 {
		return fmt.Errorf("add labels on %s#%d: no label names given", repo, index)
	}
	payload := map[string]any{"labels": names}
	if _, err := c.do(ctx, http.MethodPost, fmt.Sprintf("/repos/%s/issues/%d/labels", repo, index), payload); err != nil {
		return fmt.Errorf("add labels %s on %s#%d: %w", strings.Join(names, ","), repo, index, err)
	}
	return nil
}

// RemoveIssueLabel removes one label from an issue/PR by NAME — the DELETE
// {identifier} path param is "name or id of the label to remove" (string), so
// no id resolution happens here either. The name is path-escaped (label names
// carry spaces and slashes).
//
// Semantics pinned for gh parity: removing a label the issue does not carry
// answers 204 (the server deletes zero rows) and stays a no-op SUCCESS;
// removing a label that does not exist on the repo at all answers 404/422
// with an APIError body and stays a loud error. Nothing non-2xx is ever
// converted into a no-op here — the split rides on the server's own status
// codes, never on this client guessing.
func (c *Client) RemoveIssueLabel(ctx context.Context, repo string, index int, name string) error {
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("remove label on %s#%d: empty name", repo, index)
	}
	path := fmt.Sprintf("/repos/%s/issues/%d/labels/%s", repo, index, url.PathEscape(name))
	if _, err := c.do(ctx, http.MethodDelete, path, nil); err != nil {
		return fmt.Errorf("remove label %q on %s#%d: %w", name, repo, index, err)
	}
	return nil
}

// CreateLabel creates one repo label. Color is REQUIRED by CreateLabelOption
// — the default for an omitted color belongs to the CALLER (the github
// layer's EnsureLabel sends "#ededed"), so an empty color here fails loud
// instead of inventing one. The server accepts both "#rrggbb" and bare
// "rrggbb" and stores bare.
func (c *Client) CreateLabel(ctx context.Context, repo, name, color, description string) error {
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("create label on %s: empty name", repo)
	}
	if strings.TrimSpace(color) == "" {
		return fmt.Errorf("create label %q on %s: color is required", name, repo)
	}
	payload := map[string]any{
		"name":  name,
		"color": color,
	}
	if description != "" {
		payload["description"] = description
	}
	if _, err := c.do(ctx, http.MethodPost, fmt.Sprintf("/repos/%s/labels", repo), payload); err != nil {
		return fmt.Errorf("create label %q on %s: %w", name, repo, err)
	}
	return nil
}

// EditLabel updates one repo label by id, sending ONLY the provided fields
// (EditLabelOption is all-optional) — the upsert-update arm of EnsureLabel:
// empty means "not provided, leave unchanged", never "clear". A field-less
// PATCH would be a silent green no-op, so an empty edit fails loud; the
// caller skips the call when it has nothing to update.
func (c *Client) EditLabel(ctx context.Context, repo string, id int64, color, description string) error {
	payload := map[string]any{}
	if color != "" {
		payload["color"] = color
	}
	if description != "" {
		payload["description"] = description
	}
	if len(payload) == 0 {
		return fmt.Errorf("edit label %d on %s: no fields to update", id, repo)
	}
	if _, err := c.do(ctx, http.MethodPatch, fmt.Sprintf("/repos/%s/labels/%d", repo, id), payload); err != nil {
		return fmt.Errorf("edit label %d on %s: %w", id, repo, err)
	}
	return nil
}

// CreateRelease creates a release anchored to an EXISTING tag (pushed by
// CommitAndTag before this runs) — target_commitish is deliberately omitted
// so Forgejo anchors to that tag. There is no notes-generation equivalent in
// the swagger (gh --generate-notes divergence, documented in the port), so
// the release body stays empty. A release already existing for the tag
// answers 409 and surfaces loud.
func (c *Client) CreateRelease(ctx context.Context, repo, tag, title string) error {
	if strings.TrimSpace(tag) == "" {
		return fmt.Errorf("create release on %s: tag is required", repo)
	}
	payload := map[string]any{
		"tag_name": tag,
		"name":     title,
	}
	if _, err := c.do(ctx, http.MethodPost, fmt.Sprintf("/repos/%s/releases", repo), payload); err != nil {
		return fmt.Errorf("create release %q on %s: %w", tag, repo, err)
	}
	return nil
}
