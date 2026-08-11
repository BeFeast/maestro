// Core read methods for the #1172 transport switch (M2). These back the
// internal/github funnel dispatch on forgejo-mode rows; the forge.Client six
// in forgejo.go stay untouched as the producer surface.
//
// Contract notes baked in here (all live-verified against Forgejo 16.0.1 on
// 2026-08-11):
//
//   - the server clamps list pages at 50 items (settings/api
//     max_response_items), so the pagination loop uses 50 as its own page
//     size — asking for more silently returns 50 and would break the
//     "short page means done" stop condition;
//   - an unknown label name in the `labels` filter is silently DISCARDED and
//     the UNFILTERED list comes back, so ListIssues re-filters client-side;
//   - issues and pulls share one index space: issue-shaped payloads carry a
//     `pull_request` key on BOTH kinds, null on real issues — callers must
//     test the pointer, never key presence;
//   - the pulls list default order is index-desc, NOT updated-desc; newest-
//     updated-first requires an explicit sort=recentupdate;
//   - issue comments and issue labels have NO page/limit params and return
//     everything in one response — they must not go through the pager.
//
// M4 additions (#1172, contract pinned by the stage-A live probe):
//
//   - the combined-status aggregate lives in the top-level `state` field while
//     each per-status state lives in `.status` (never `.state`); a SHA with
//     zero statuses answers 200 {"state":"","total_count":0,"statuses":null} —
//     EMPTY-STRING aggregate plus null statuses is the no-signal shape;
//   - review-comment reads report the new-file line number in `position`
//     (x-go-name LineNum) — there is NO `line` and NO `new_position` on the
//     read side — and the per-review comments endpoint has no page/limit
//     params, so it must not go through the pager.
package forgejo

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// pageSize is the live-verified server clamp (GET /api/v1/settings/api →
// max_response_items: 50). Requesting more still returns at most 50, so the
// pager must use the clamp itself or every page would look "short" and the
// loop would stop after page one.
const pageSize = 50

// maxListPages bounds the pagination loop: 100 pages × 50 items = 5000
// entries, far beyond homelab scale. Hitting the cap is an explicit error —
// never a silent truncation that downstream logic would read as "the full
// set".
const maxListPages = 100

// boundedWindow is the item budget of a bounded (allPages=false) read: 100,
// matching the gh transport's single-page per_page=100 calls. The server
// clamps pages at 50 (pageSize), so a first-page-only bounded read would see
// HALF the window the gh path sees — a consumer scanning the bounded open-PR
// list (e.g. HasOpenPRForIssue on supervisor dispatch) would miss a linked PR
// sitting at position 51 and re-dispatch the issue. The pager keeps fetching
// clamped pages until the window is filled or the set ends.
const boundedWindow = 100

// Label is one issue/PR label. Only the name matters to callers.
type Label struct {
	Name string `json:"name"`
}

// PullMeta is the pull-request marker on issue-shaped payloads. The key is
// present on real issues too, with value null — so only the pointer test
// (Issue.PullRequest != nil) identifies a pull.
type PullMeta struct {
	Merged   bool    `json:"merged"`
	MergedAt *string `json:"merged_at"`
	Draft    bool    `json:"draft"`
}

// Issue mirrors the live-verified Forgejo issue payload. State is
// "open"|"closed" verbatim (lowercase).
type Issue struct {
	Number int     `json:"number"`
	Title  string  `json:"title"`
	Body   string  `json:"body"`
	State  string  `json:"state"`
	Labels []Label `json:"labels"`
	// PullRequest is non-nil when the entry is actually a pull request.
	// Issues and pulls share one index space, so GetIssue on a pull index
	// returns an issue-shaped payload with this set — issue paths must keep
	// the != nil belt.
	PullRequest *PullMeta `json:"pull_request"`
}

// IssueComment is one issue/PR comment, flattened to what the gh layer maps
// into its own IssueComment (id/body/user.login/created_at).
type IssueComment struct {
	ID        int64
	Body      string
	Author    string
	CreatedAt string
}

// Pull is the widened pull view for the transport switch (forge.PR stays
// minimal for the producer surface). State is "open"|"closed" verbatim
// (lowercase — a merged pull is state=closed with MergedAt set). Mergeable is
// a plain bool on the wire (no tri-state), kept as *bool so an absent field
// is distinguishable from false, and carried VERBATIM: the server forces it
// false on draft/WIP pulls and during the async conflict re-check
// (live-verified, 41/41 draft pulls report false), so the gh layer — not this
// transport — owns mapping that contamination out (fjPullToREST). MergedAt is
// nil when unmerged, RFC3339 when merged; MergeCommitSHA is "" when unmerged,
// 40-hex when merged.
type Pull struct {
	Number         int
	Title          string
	Body           string
	State          string
	Draft          bool
	Mergeable      *bool
	MergedAt       *string
	MergeCommitSHA string
	HeadRef        string
	HeadSHA        string
	BaseRef        string
	// MergeBase is the merge-base SHA between head and base (wire
	// `merge_base`, 40-hex; live-verified present on both open and merged
	// pulls). On an up-to-date open pull it equals the base branch head; after
	// a merge base.sha moves on while merge_base stays — so "behind" synthesis
	// must compare it against BranchHeadSHA(BaseRef), never against base.sha
	// from the same payload. Carried verbatim; empty when the wire omits it.
	MergeBase string
}

// wirePull is the nested wire shape of GET /repos/{repo}/pulls[/{index}].
type wirePull struct {
	Number         int     `json:"number"`
	Title          string  `json:"title"`
	Body           string  `json:"body"`
	State          string  `json:"state"`
	Draft          bool    `json:"draft"`
	Mergeable      *bool   `json:"mergeable"`
	MergedAt       *string `json:"merged_at"`
	MergeCommitSHA string  `json:"merge_commit_sha"`
	MergeBase      string  `json:"merge_base"`
	Head           struct {
		Ref string `json:"ref"`
		SHA string `json:"sha"`
	} `json:"head"`
	Base struct {
		Ref string `json:"ref"`
	} `json:"base"`
}

func (wp wirePull) pull() Pull {
	return Pull{
		Number:         wp.Number,
		Title:          wp.Title,
		Body:           wp.Body,
		State:          wp.State,
		Draft:          wp.Draft,
		Mergeable:      wp.Mergeable,
		MergedAt:       wp.MergedAt,
		MergeCommitSHA: wp.MergeCommitSHA,
		MergeBase:      wp.MergeBase,
		HeadRef:        wp.Head.Ref,
		HeadSHA:        wp.Head.SHA,
		BaseRef:        wp.Base.Ref,
	}
}

// listPages fetches a JSON-array list endpoint page by page. path carries the
// caller's query already encoded (or none); the pager appends page/limit.
// allPages=false bounds the read to a boundedWindow (100) item window —
// clamped pages are fetched until the window fills or a short page ends the
// set — so bounded reads see the same window as the gh transport's
// single-page per_page=100 calls. More than maxListPages full pages is an
// explicit error — silent truncation would let e.g. a merged-PR scan miss the
// PR it was looking for and re-dispatch work.
//
// The x-total-count header (live-verified present and filter-consistent on
// every paged endpoint here) is a truncation belt: pageSize hardcodes THIS
// instance's max_response_items clamp, and on a server clamped LOWER every
// page would come back short, the short-page stop would fire after page one,
// and an authoritative read (e.g. the #827 reconcile open-issue set) would
// silently truncate — downstream would stamp still-open issues closed. A
// short final page that leaves the accumulated count below the server's own
// total is therefore an explicit error. Reaching the bounded window with more
// entries outstanding on the server is NOT truncation — it is the window's
// contract — so the bounded stop returns before the belt is consulted; the
// belt still fires when a short page strands a bounded read below BOTH the
// window and the server total. The total is re-read on every page so a set
// that legitimately shrinks mid-scan does not false-alarm, and reaching the
// total on a full page ends the loop without demanding one empty page (which
// at exactly maxListPages×pageSize entries would trip the cap error).
func listPages[T any](ctx context.Context, c *Client, path string, allPages bool) ([]T, error) {
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	var all []T
	total := -1 // -1: header absent, belt disabled
	for page := 1; ; page++ {
		if page > maxListPages {
			return nil, fmt.Errorf("GET %s: more than %d pages (%d entries so far); refusing to truncate silently", path, maxListPages, len(all))
		}
		paged := fmt.Sprintf("%s%slimit=%d&page=%d", path, sep, pageSize, page)
		out, header, err := c.doHeader(ctx, http.MethodGet, paged, nil)
		if err != nil {
			return nil, err
		}
		if raw := strings.TrimSpace(header.Get("X-Total-Count")); raw != "" {
			if parsed, parseErr := strconv.Atoi(raw); parseErr == nil && parsed >= 0 {
				total = parsed
			}
		}
		var items []T
		if err := json.Unmarshal(out, &items); err != nil {
			return nil, fmt.Errorf("parse GET %s: %w", paged, err)
		}
		all = append(all, items...)
		if !allPages && len(all) >= boundedWindow {
			// Intentional bounded stop: the window is full, anything beyond it
			// is out of contract (gh parity), so the truncation belt below
			// must not see this as a short set.
			return all[:boundedWindow], nil
		}
		if len(items) < pageSize {
			if total >= 0 && len(all) < total {
				return nil, fmt.Errorf("GET %s: page %d came back short at %d items with only %d of %d total entries accumulated (server page clamp below %d?); refusing to truncate silently", path, page, len(items), len(all), total, pageSize)
			}
			return all, nil
		}
		if total >= 0 && len(all) >= total {
			return all, nil
		}
	}
}

// hasLabel reports whether labels contains name (case-folded — label filters
// are name-based and the belt must not drop a hit over casing).
func hasLabel(labels []Label, name string) bool {
	for _, l := range labels {
		if strings.EqualFold(l.Name, name) {
			return true
		}
	}
	return false
}

// ListIssues lists issues — never pulls — in one state ("open", "closed",
// "all"; empty means server default "open"), optionally filtered to a single
// label (the gh layer ORs multiple labels client-side by N calls; that
// contract is kept here). Two client-side belts on top of the server filter:
//
//   - pull entries are dropped even though type=issues already excludes them
//     (shared index space, zero tolerance for a pull leaking into issue
//     dispatch);
//   - the label is re-checked by name, because the server silently DISCARDS
//     an unknown label name and returns the unfiltered list — trusting it
//     would turn a typo'd label into "dispatch everything".
func (c *Client) ListIssues(ctx context.Context, repo, state, label string, allPages bool) ([]Issue, error) {
	q := url.Values{}
	q.Set("type", "issues")
	if state != "" {
		q.Set("state", state)
	}
	if label != "" {
		q.Set("labels", label)
	}
	path := fmt.Sprintf("/repos/%s/issues?%s", repo, q.Encode())
	issues, err := listPages[Issue](ctx, c, path, allPages)
	if err != nil {
		return nil, fmt.Errorf("list issues %s: %w", repo, err)
	}
	kept := make([]Issue, 0, len(issues))
	for _, is := range issues {
		if is.PullRequest != nil {
			continue
		}
		if label != "" && !hasLabel(is.Labels, label) {
			continue
		}
		kept = append(kept, is)
	}
	return kept, nil
}

// GetIssue returns one issue by index, verbatim — including a populated
// PullRequest when the index belongs to a pull (shared number space); the
// caller owns the != nil belt.
func (c *Client) GetIssue(ctx context.Context, repo string, index int) (Issue, error) {
	out, err := c.do(ctx, http.MethodGet, fmt.Sprintf("/repos/%s/issues/%d", repo, index), nil)
	if err != nil {
		return Issue{}, fmt.Errorf("get issue %s#%d: %w", repo, index, err)
	}
	var is Issue
	if err := json.Unmarshal(out, &is); err != nil {
		return Issue{}, fmt.Errorf("parse issue %s#%d: %w", repo, index, err)
	}
	return is, nil
}

// ListIssueComments returns all comments on one issue/PR. The endpoint has NO
// page/limit params (swagger: only since/before) and returns everything in
// one response — do not route it through the pager.
func (c *Client) ListIssueComments(ctx context.Context, repo string, index int) ([]IssueComment, error) {
	out, err := c.do(ctx, http.MethodGet, fmt.Sprintf("/repos/%s/issues/%d/comments", repo, index), nil)
	if err != nil {
		return nil, fmt.Errorf("list comments %s#%d: %w", repo, index, err)
	}
	var wire []struct {
		ID   int64  `json:"id"`
		Body string `json:"body"`
		User struct {
			Login string `json:"login"`
		} `json:"user"`
		CreatedAt string `json:"created_at"`
	}
	if err := json.Unmarshal(out, &wire); err != nil {
		return nil, fmt.Errorf("parse comments %s#%d: %w", repo, index, err)
	}
	comments := make([]IssueComment, 0, len(wire))
	for _, wc := range wire {
		comments = append(comments, IssueComment{
			ID:        wc.ID,
			Body:      wc.Body,
			Author:    wc.User.Login,
			CreatedAt: wc.CreatedAt,
		})
	}
	return comments, nil
}

// IssueLabels returns the label names on one issue/PR. Like comments, the
// endpoint has no page/limit params and returns everything in one response.
func (c *Client) IssueLabels(ctx context.Context, repo string, index int) ([]string, error) {
	out, err := c.do(ctx, http.MethodGet, fmt.Sprintf("/repos/%s/issues/%d/labels", repo, index), nil)
	if err != nil {
		return nil, fmt.Errorf("list labels %s#%d: %w", repo, index, err)
	}
	var labels []Label
	if err := json.Unmarshal(out, &labels); err != nil {
		return nil, fmt.Errorf("parse labels %s#%d: %w", repo, index, err)
	}
	names := make([]string, 0, len(labels))
	for _, l := range labels {
		names = append(names, l.Name)
	}
	return names, nil
}

// ListPulls lists pull requests in one state ("open", "closed", "all"; empty
// means server default "open"), newest-updated-first. The sort is explicit:
// the server's default order is index-desc, NOT updated-desc, and the gh
// layer's closed-PR scans depend on updated-desc semantics.
func (c *Client) ListPulls(ctx context.Context, repo, state string, allPages bool) ([]Pull, error) {
	q := url.Values{}
	q.Set("sort", "recentupdate")
	if state != "" {
		q.Set("state", state)
	}
	path := fmt.Sprintf("/repos/%s/pulls?%s", repo, q.Encode())
	wires, err := listPages[wirePull](ctx, c, path, allPages)
	if err != nil {
		return nil, fmt.Errorf("list pulls %s: %w", repo, err)
	}
	pulls := make([]Pull, 0, len(wires))
	for _, wp := range wires {
		pulls = append(pulls, wp.pull())
	}
	return pulls, nil
}

// GetPull returns one pull request by index, widened for the transport
// switch. forge.Client's GetPR stays as-is for the producer surface.
func (c *Client) GetPull(ctx context.Context, repo string, index int) (Pull, error) {
	out, err := c.do(ctx, http.MethodGet, fmt.Sprintf("/repos/%s/pulls/%d", repo, index), nil)
	if err != nil {
		return Pull{}, fmt.Errorf("get pull %s#%d: %w", repo, index, err)
	}
	var wp wirePull
	if err := json.Unmarshal(out, &wp); err != nil {
		return Pull{}, fmt.Errorf("parse pull %s#%d: %w", repo, index, err)
	}
	return wp.pull(), nil
}

// PullCommit is one commit on a pull request: the sha plus the full commit
// message verbatim (the wire message carries a trailing newline; headline
// extraction is the caller's job, mirroring the gh layer's parsePRCommits).
type PullCommit struct {
	SHA     string
	Message string
}

// ListPullCommits returns the commits on one pull request in server order.
// Unlike the comments/labels endpoints this one IS paginated (page/limit
// accepted, x-total-count + Link observed live), so it goes through the pager.
func (c *Client) ListPullCommits(ctx context.Context, repo string, index int, allPages bool) ([]PullCommit, error) {
	type wireCommit struct {
		SHA    string `json:"sha"`
		Commit struct {
			Message string `json:"message"`
		} `json:"commit"`
	}
	wires, err := listPages[wireCommit](ctx, c, fmt.Sprintf("/repos/%s/pulls/%d/commits", repo, index), allPages)
	if err != nil {
		return nil, fmt.Errorf("list pull commits %s#%d: %w", repo, index, err)
	}
	commits := make([]PullCommit, 0, len(wires))
	for _, w := range wires {
		commits = append(commits, PullCommit{SHA: w.SHA, Message: w.Commit.Message})
	}
	return commits, nil
}

// ListPullFiles returns the repo-relative paths changed by one pull request.
// Paginated like the commits list.
func (c *Client) ListPullFiles(ctx context.Context, repo string, index int, allPages bool) ([]string, error) {
	type wireFile struct {
		Filename string `json:"filename"`
	}
	wires, err := listPages[wireFile](ctx, c, fmt.Sprintf("/repos/%s/pulls/%d/files", repo, index), allPages)
	if err != nil {
		return nil, fmt.Errorf("list pull files %s#%d: %w", repo, index, err)
	}
	files := make([]string, 0, len(wires))
	for _, w := range wires {
		files = append(files, w.Filename)
	}
	return files, nil
}

// DefaultBranch returns the repository's default branch. Empty is an error —
// every caller anchors base-branch logic to it.
func (c *Client) DefaultBranch(ctx context.Context, repo string) (string, error) {
	out, err := c.do(ctx, http.MethodGet, fmt.Sprintf("/repos/%s", repo), nil)
	if err != nil {
		return "", fmt.Errorf("get repo %s: %w", repo, err)
	}
	var meta struct {
		DefaultBranch string `json:"default_branch"`
	}
	if err := json.Unmarshal(out, &meta); err != nil {
		return "", fmt.Errorf("parse repo %s: %w", repo, err)
	}
	branch := strings.TrimSpace(meta.DefaultBranch)
	if branch == "" {
		return "", fmt.Errorf("repo %s reports no default branch", repo)
	}
	return branch, nil
}

// BranchHeadSHA returns the head commit SHA of one branch. The branch name is
// path-escaped so slash-containing feature branches route correctly
// (/branches/{name} answers 200 for both escaped and raw slashes live, but
// escaping is the documented, unambiguous form). The head SHA lives in
// commit.id (Branch.commit is a PayloadCommit, not the top-level sha shape).
func (c *Client) BranchHeadSHA(ctx context.Context, repo, branch string) (string, error) {
	out, err := c.do(ctx, http.MethodGet, fmt.Sprintf("/repos/%s/branches/%s", repo, url.PathEscape(branch)), nil)
	if err != nil {
		return "", fmt.Errorf("get branch %s@%s: %w", repo, branch, err)
	}
	var br struct {
		Commit struct {
			ID string `json:"id"`
		} `json:"commit"`
	}
	if err := json.Unmarshal(out, &br); err != nil {
		return "", fmt.Errorf("parse branch %s@%s: %w", repo, branch, err)
	}
	sha := strings.TrimSpace(br.Commit.ID)
	if sha == "" {
		return "", fmt.Errorf("branch %s@%s has an empty head sha", repo, branch)
	}
	return sha, nil
}

// CommitStatus is one per-context row of the combined-status read. State is
// decoded from Forgejo's per-status `.status` wire field — NOT `.state`, which
// does not exist per status here (the live-verified 16.0.1 gotcha the
// forge.Client CommitStatuses already normalizes). The vocabulary (swagger
// CommitStatusState) is pending|success|error|failure|warning|skipped, carried
// lowercased but otherwise verbatim: "warning" and "skipped" are real values
// (skipped live-verified) and must reach the gh layer undamaged so its failure
// mapping never miscounts them. CreatedAt is the RFC3339 wire string
// byte-for-byte — the gh layer mixes it into the CI rollup fingerprint
// (`status:<context>:<state>:<created_at>`), so it must not round-trip through
// time.Time.
type CommitStatus struct {
	Context     string
	State       string
	Description string
	TargetURL   string
	CreatedAt   string
}

// CombinedStatus is the aggregate view of one SHA's commit statuses: the
// server-computed rollup over the latest status per context, plus the rows.
//
// NO-SIGNAL SHAPE (live-verified): a SHA with zero statuses answers HTTP 200
// {"state":"","total_count":0,"statuses":null} — State is the EMPTY STRING and
// Statuses is nil (JSON null, not []). Both together mean "no signal"; neither
// is an error and neither may be read as an unknown state.
type CombinedStatus struct {
	State    string
	Statuses []CommitStatus
}

// CombinedStatus returns one SHA's combined status. forge.Client's
// CommitStatuses stays untouched as the producer surface (it drops the
// aggregate); this is the transport-switch sibling exposing BOTH the top-level
// aggregate and the per-context rows.
//
// The statuses array is paginated (swagger page/limit; not exercisable live —
// no probe SHA carries >1 page), so the loop cannot go through listPages (the
// payload is an object, not a bare array) and cannot lean on x-total-count
// semantics that were never pinned for this endpoint. It uses the short-page
// stop plus the maxListPages cap: on a server that ignored the page param, a
// >=pageSize set would keep answering full pages and hit the cap as an
// explicit error — never a silently duplicated or truncated set. The
// aggregate is taken from page 1.
func (c *Client) CombinedStatus(ctx context.Context, repo, sha string) (CombinedStatus, error) {
	var combined CombinedStatus
	for page := 1; ; page++ {
		if page > maxListPages {
			return CombinedStatus{}, fmt.Errorf("combined status for %s@%s: more than %d pages (%d statuses so far); refusing to truncate silently", repo, sha, maxListPages, len(combined.Statuses))
		}
		path := fmt.Sprintf("/repos/%s/commits/%s/status?limit=%d&page=%d", repo, sha, pageSize, page)
		out, err := c.do(ctx, http.MethodGet, path, nil)
		if err != nil {
			return CombinedStatus{}, fmt.Errorf("combined status for %s@%s: %w", repo, sha, err)
		}
		var wire struct {
			State    string `json:"state"`
			Statuses []struct {
				Context     string `json:"context"`
				Status      string `json:"status"`
				Description string `json:"description"`
				TargetURL   string `json:"target_url"`
				CreatedAt   string `json:"created_at"`
			} `json:"statuses"`
		}
		if err := json.Unmarshal(out, &wire); err != nil {
			return CombinedStatus{}, fmt.Errorf("parse combined status for %s@%s: %w", repo, sha, err)
		}
		if page == 1 {
			combined.State = strings.ToLower(strings.TrimSpace(wire.State))
		}
		for _, st := range wire.Statuses {
			combined.Statuses = append(combined.Statuses, CommitStatus{
				Context:     st.Context,
				State:       strings.ToLower(strings.TrimSpace(st.Status)),
				Description: st.Description,
				TargetURL:   st.TargetURL,
				CreatedAt:   strings.TrimSpace(st.CreatedAt),
			})
		}
		if len(wire.Statuses) < pageSize {
			return combined, nil
		}
	}
}

// PullReview is one review on a pull request, flattened to what the gh layer
// maps. State is the ReviewStateType VERBATIM — uppercase "APPROVED" /
// "COMMENT" / "REQUEST_CHANGES" / "PENDING" / "REQUEST_REVIEW" — vocabulary
// mapping is the gh layer's job, and the full set is not enumerated in
// swagger, so nothing is normalized or filtered here. CommitID can
// legitimately be "" (live-proven on GitHub-migrated reviews); the per-comment
// commit_id IS populated in that case, so head-anchoring callers must fall
// back review CommitID -> comment CommitID.
type PullReview struct {
	ID          int64
	User        string // user.login; "" when the wire user is null
	State       string
	Body        string
	CommitID    string
	SubmittedAt string // RFC3339 verbatim
	// CommentsCount is the wire comments_count (x-go-name CodeCommentsCount):
	// the number of inline code comments in this review — lets callers skip
	// the per-review comments round trip for body-only reviews.
	CommentsCount int64
}

// ListPullReviews returns the reviews on one pull request in server order
// (ascending by id/submitted_at, live-verified). Paginated (page/limit +
// x-total-count live-verified; past-the-end pages answer 200 []), so it goes
// through the pager.
func (c *Client) ListPullReviews(ctx context.Context, repo string, index int, allPages bool) ([]PullReview, error) {
	type wireReview struct {
		ID   int64 `json:"id"`
		User struct {
			Login string `json:"login"`
		} `json:"user"`
		State         string `json:"state"`
		Body          string `json:"body"`
		CommitID      string `json:"commit_id"`
		SubmittedAt   string `json:"submitted_at"`
		CommentsCount int64  `json:"comments_count"`
	}
	wires, err := listPages[wireReview](ctx, c, fmt.Sprintf("/repos/%s/pulls/%d/reviews", repo, index), allPages)
	if err != nil {
		return nil, fmt.Errorf("list reviews %s#%d: %w", repo, index, err)
	}
	reviews := make([]PullReview, 0, len(wires))
	for _, w := range wires {
		reviews = append(reviews, PullReview{
			ID:            w.ID,
			User:          w.User.Login,
			State:         w.State,
			Body:          w.Body,
			CommitID:      w.CommitID,
			SubmittedAt:   w.SubmittedAt,
			CommentsCount: w.CommentsCount,
		})
	}
	return reviews, nil
}

// PullReviewComment is one inline comment of a review. THE LINE FIELD
// (live-pinned, the one most likely to be mis-mapped): the read side has NO
// `line` and NO `new_position` — the write-side new_position comes back as
// `position` (x-go-name LineNum), the NEW-file LINE NUMBER, not a diff
// offset; `original_position` (OldLineNum) carries the old-file line, 0 when
// the comment targets the new side. Line/OldLine map them 1:1. CommitID is
// populated even when the parent review's commit_id is "" — it is the
// reliable head anchor.
type PullReviewComment struct {
	ID               int64
	Body             string
	Author           string // user.login
	Path             string
	CommitID         string
	OriginalCommitID string
	Line             int64  // wire `position`: new-file line number
	OldLine          int64  // wire `original_position`: old-file line number
	CreatedAt        string // RFC3339 verbatim
}

// ListPullReviewComments returns all inline comments of one review. The
// endpoint has NO page/limit params (swagger: only owner/repo/index/id) and
// returns everything in one response — it must not go through the pager.
func (c *Client) ListPullReviewComments(ctx context.Context, repo string, index int, reviewID int64) ([]PullReviewComment, error) {
	out, err := c.do(ctx, http.MethodGet, fmt.Sprintf("/repos/%s/pulls/%d/reviews/%d/comments", repo, index, reviewID), nil)
	if err != nil {
		return nil, fmt.Errorf("list review comments %s#%d review %d: %w", repo, index, reviewID, err)
	}
	var wire []struct {
		ID   int64  `json:"id"`
		Body string `json:"body"`
		User struct {
			Login string `json:"login"`
		} `json:"user"`
		Path             string `json:"path"`
		CommitID         string `json:"commit_id"`
		OriginalCommitID string `json:"original_commit_id"`
		Position         int64  `json:"position"`
		OriginalPosition int64  `json:"original_position"`
		CreatedAt        string `json:"created_at"`
	}
	if err := json.Unmarshal(out, &wire); err != nil {
		return nil, fmt.Errorf("parse review comments %s#%d review %d: %w", repo, index, reviewID, err)
	}
	comments := make([]PullReviewComment, 0, len(wire))
	for _, wc := range wire {
		comments = append(comments, PullReviewComment{
			ID:               wc.ID,
			Body:             wc.Body,
			Author:           wc.User.Login,
			Path:             wc.Path,
			CommitID:         wc.CommitID,
			OriginalCommitID: wc.OriginalCommitID,
			Line:             wc.Position,
			OldLine:          wc.OriginalPosition,
			CreatedAt:        wc.CreatedAt,
		})
	}
	return comments, nil
}
