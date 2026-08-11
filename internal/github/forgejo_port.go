// Forgejo-side implementations of the ported core-read funnels (#1172 M2).
//
// Dispatch happens at the FUNNELS: each fj* method here maps Forgejo wire
// types (internal/forgejo) into the existing internal structs — restIssue,
// restPull, IssueComment — so restIssue.issue() / restPull.pr() and every
// derived method (PRDetails, PRHeadSHA, PRMergeInfo, IsPRMerged, PRMergeable,
// MergedPRNumberForIssue, HasOpenPRForIssue, ...) run unchanged on both
// forges.
//
// Mapping contract (live-verified against Forgejo 16.0.1, 2026-08-11):
//
//   - state is "open"|"closed" lowercase verbatim on issues AND pulls — same
//     as GitHub REST, so restPull.pr()'s upper-casing and
//     restIssueStateClosed() work unchanged; a merged pull is state=closed
//     with merged_at set;
//   - merged_at is null when unmerged / RFC3339 when merged, and
//     merge_commit_sha is null-or-40-hex under the same rule, so
//     PRMergeInfo's 40-hex assert holds;
//   - mergeable is a plain bool on the wire (true observed even on merged
//     pulls — NOT GitHub's tri-state), carried as *bool so
//     mergeableFromRESTPull takes the bool branch when present; it is the
//     server's Mergeable() predicate, forced false on drafts and during the
//     async conflict re-check — see fjPullToREST for the parity mapping;
//   - mergeable_state has NO Forgejo equivalent: restPull.MergeableState is
//     always "" here, which is why PRMergeStatus keeps an explicit
//     NOT-ported guard (an empty raw state reads as "don't know, proceed");
//   - draft is a plain bool.
//
// Methods that stay UNPORTED keep failing loud with ErrForgejoNotSupported
// through the receiver chokes; the handful that would otherwise half-work
// after this port (their first transport touch is now a ported read, or they
// swallow downstream errors by design) carry explicit early guards in
// github.go — see the "NOT ported in M2" list in the M2 spec.
package github

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/befeast/maestro/internal/forgejo"
)

// fjTransport returns the Forgejo REST client, or the construction-time
// fault. An empty token env surfaces here on EVERY ported call — naming the
// env var to export — as a plain loud error, deliberately NOT wrapped in
// ErrForgejoNotSupported: the operation IS supported, the configuration is
// broken, and a caller branching on the sentinel to mean "feature absent on
// this forge" must not confuse the two.
func (c *Client) fjTransport() (*forgejo.Client, error) {
	if c.forgeErr != nil {
		return nil, fmt.Errorf("repo %s: %w", c.Repo, c.forgeErr)
	}
	if c.fj == nil {
		return nil, fmt.Errorf("repo %s: forgejo transport not initialized", c.Repo)
	}
	return c.fj, nil
}

// forgejoCallContext is the context for fj* adapter calls made from ctx-less
// legacy Client methods. It derives from a context canceled the moment the gh
// transport's shutdown fail-fast fires (BeginShutdown, #797), giving the
// forgejo path the same drain semantics the gh path already has: once
// shutdown begins, an in-flight or newly issued Forgejo HTTP request fails
// promptly instead of riding out the transport's 30s timeout. Full caller-ctx
// propagation through the ~80 ctx-less Client method signatures is deferred
// to the signature modernization; methods that already take a ctx
// (RepositoryDefaultBranch, LatestMergedPRGenerations) pass the caller's ctx
// straight through and do not use this.
//
// The caller must defer cancel() — it releases the shutdown watcher.
func forgejoCallContext() (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	shutdown := ghShutdownChan()
	select {
	case <-shutdown:
		// Shutdown already begun: hand out an already-canceled context so the
		// transport fails fast without even dialing.
		cancel()
		return ctx, cancel
	default:
	}
	go func() {
		select {
		case <-shutdown:
			cancel()
		case <-ctx.Done():
		}
	}()
	return ctx, cancel
}

// fjIssueToREST maps a Forgejo issue into the gh-wire restIssue, preserving
// the pull_request marker (issues and pulls share one index space on both
// forges, so the parseRESTIssues belt semantics carry over).
func fjIssueToREST(is forgejo.Issue) restIssue {
	body := is.Body
	ri := restIssue{
		Number: is.Number,
		Title:  is.Title,
		Body:   &body,
		State:  is.State,
	}
	for _, l := range is.Labels {
		ri.Labels = append(ri.Labels, struct {
			Name string `json:"name"`
		}{Name: l.Name})
	}
	if is.PullRequest != nil {
		ri.PullRequest = &struct{}{}
	}
	return ri
}

// fjPullToREST maps a Forgejo pull into the gh-wire restPull. MergeableState
// is always "" — Forgejo has no equivalent field (see the package comment).
//
// Mergeable parity (live-verified on a Forgejo 16.0 instance, 41/41 draft
// pulls): Forgejo's wire mergeable is the server-side Mergeable() predicate,
// which is false while the async conflict re-check runs AND on every
// draft/WIP pull regardless of conflicts. GitHub's bool answers only "can a
// merge commit be created" (null while computing, true on clean drafts), so a
// draft's false carries zero conflict information here — mapping it through
// would turn EVERY draft PR into "CONFLICTING" and e.g. openPRNeedsRepair
// would authorize repair on it each cycle. Drop the draft-contaminated false
// to nil so mergeableFromRESTPull answers "UNKNOWN", exactly GitHub's
// not-computed state. The non-draft checking-window false (seconds after a
// push, indistinguishable from a real conflict on the wire) remains an
// accepted transient until the M4 merge-semantics slice.
func fjPullToREST(p forgejo.Pull) restPull {
	body := p.Body
	mergeable := p.Mergeable
	if p.Draft && mergeable != nil && !*mergeable {
		mergeable = nil
	}
	rp := restPull{
		Number:         p.Number,
		Title:          p.Title,
		Body:           &body,
		State:          p.State,
		Draft:          p.Draft,
		Mergeable:      mergeable,
		MergeableState: "",
		MergedAt:       p.MergedAt,
		MergeCommitSHA: p.MergeCommitSHA,
	}
	rp.Head.Ref = p.HeadRef
	rp.Head.SHA = p.HeadSHA
	rp.Base.Ref = p.BaseRef
	return rp
}

// fjListOpenIssuesByLabel backs listOpenIssuesByLabel (allPages=false, the
// cheap working-set read) and listAllOpenIssuesByLabel (allPages=true, the
// authoritative reconcile read). forgejo.ListIssues already drops pull
// entries and re-filters the label client-side (the server silently discards
// unknown label names); the marker belt here mirrors parseRESTIssues so both
// transports enforce it at the same layer.
func (c *Client) fjListOpenIssuesByLabel(label string, allPages bool) ([]Issue, error) {
	fj, err := c.fjTransport()
	if err != nil {
		return nil, err
	}
	ctx, cancel := forgejoCallContext()
	defer cancel()
	fjIssues, err := fj.ListIssues(ctx, c.Repo, "open", label, allPages)
	if err != nil {
		return nil, fmt.Errorf("list open issues: %w", err)
	}
	issues := make([]Issue, 0, len(fjIssues))
	for _, is := range fjIssues {
		ri := fjIssueToREST(is)
		if ri.PullRequest != nil {
			continue
		}
		issues = append(issues, ri.issue())
	}
	return issues, nil
}

func (c *Client) fjGetIssue(number int) (Issue, error) {
	fj, err := c.fjTransport()
	if err != nil {
		return Issue{}, err
	}
	ctx, cancel := forgejoCallContext()
	defer cancel()
	is, err := fj.GetIssue(ctx, c.Repo, number)
	if err != nil {
		return Issue{}, fmt.Errorf("get issue %d: %w", number, err)
	}
	// Verbatim like parseRESTIssue: a pull index returns its issue shape
	// (shared number space); callers keep their own pull_request belt.
	return fjIssueToREST(is).issue(), nil
}

func (c *Client) fjIsIssueClosed(number int) (bool, error) {
	fj, err := c.fjTransport()
	if err != nil {
		return false, err
	}
	ctx, cancel := forgejoCallContext()
	defer cancel()
	is, err := fj.GetIssue(ctx, c.Repo, number)
	if err != nil {
		return false, fmt.Errorf("get issue %d: %w", number, err)
	}
	return restIssueStateClosed(is.State), nil
}

// fjListPulls backs ListOpenPRs/listClosedPRs (allPages=false — a bounded
// 100-item window assembled from clamped 50-item pages, matching the gh
// single-page per_page=100 reads item for item) and ListAllOpenPRs
// (allPages=true).
// forgejo.ListPulls always sends sort=recentupdate, giving the same
// newest-updated-first order as the gh path's sort=updated&direction=desc —
// listClosedPRs/MergedPRNumberForIssue semantics depend on it.
func (c *Client) fjListPulls(state string, allPages bool) ([]PR, error) {
	fj, err := c.fjTransport()
	if err != nil {
		return nil, err
	}
	ctx, cancel := forgejoCallContext()
	defer cancel()
	pulls, err := fj.ListPulls(ctx, c.Repo, state, allPages)
	if err != nil {
		return nil, fmt.Errorf("list %s PRs: %w", state, err)
	}
	prs := make([]PR, 0, len(pulls))
	for _, p := range pulls {
		prs = append(prs, fjPullToREST(p).pr())
	}
	return prs, nil
}

func (c *Client) fjGetRESTPull(prNumber int) (restPull, error) {
	fj, err := c.fjTransport()
	if err != nil {
		return restPull{}, err
	}
	ctx, cancel := forgejoCallContext()
	defer cancel()
	p, err := fj.GetPull(ctx, c.Repo, prNumber)
	if err != nil {
		return restPull{}, err
	}
	return fjPullToREST(p), nil
}

// fjLatestMergedPRGenerations pages the FULL closed set and filters on
// Base.Ref client-side inside latestMergedPRGenerations — the shared helper
// the gh path uses, so tie-at-same-second and 40-hex semantics stay
// identical. The opaque error strings mirror the gh path on purpose: these
// messages flow into delivery-executor state.
func (c *Client) fjLatestMergedPRGenerations(ctx context.Context) ([]PRMergeInfo, error) {
	fj, err := c.fjTransport()
	if err != nil {
		return nil, err
	}
	deliveryBase, err := c.RepositoryDefaultBranch(ctx)
	if err != nil {
		return nil, err
	}
	pulls, err := fj.ListPulls(ctx, c.Repo, "closed", true)
	if err != nil {
		return nil, errors.New("list merged delivery generations failed")
	}
	rest := make([]restPull, 0, len(pulls))
	for _, p := range pulls {
		rest = append(rest, fjPullToREST(p))
	}
	return latestMergedPRGenerations(rest, deliveryBase)
}

// fjRepositoryDefaultBranch keeps the gh path's validation (a branch name
// with query/fragment metacharacters must not reach URL construction) and
// its opaque read-failure message; only the transport fault (empty token
// env) surfaces verbatim, because that one names the fix.
func (c *Client) fjRepositoryDefaultBranch(ctx context.Context) (string, error) {
	fj, err := c.fjTransport()
	if err != nil {
		return "", err
	}
	branch, err := fj.DefaultBranch(ctx, c.Repo)
	if err != nil {
		return "", errors.New("read repository delivery branch failed")
	}
	if strings.ContainsAny(branch, "\r\n?&#") {
		return "", errors.New("repository delivery branch is invalid")
	}
	return branch, nil
}

func (c *Client) fjBranchHeadSHA(branch string) (string, error) {
	fj, err := c.fjTransport()
	if err != nil {
		return "", err
	}
	ctx, cancel := forgejoCallContext()
	defer cancel()
	return fj.BranchHeadSHA(ctx, c.Repo, branch)
}

func (c *Client) fjListIssueComments(issueNumber int) ([]IssueComment, error) {
	fj, err := c.fjTransport()
	if err != nil {
		return nil, err
	}
	ctx, cancel := forgejoCallContext()
	defer cancel()
	fjComments, err := fj.ListIssueComments(ctx, c.Repo, issueNumber)
	if err != nil {
		return nil, fmt.Errorf("list issue comments for #%d: %w", issueNumber, err)
	}
	comments := make([]IssueComment, 0, len(fjComments))
	for _, fc := range fjComments {
		comments = append(comments, IssueComment{
			ID:        fc.ID,
			Body:      fc.Body,
			Author:    fc.Author,
			CreatedAt: fc.CreatedAt,
		})
	}
	return comments, nil
}

func (c *Client) fjPRLabels(prNumber int) ([]string, error) {
	fj, err := c.fjTransport()
	if err != nil {
		return nil, err
	}
	ctx, cancel := forgejoCallContext()
	defer cancel()
	names, err := fj.IssueLabels(ctx, c.Repo, prNumber)
	if err != nil {
		return nil, fmt.Errorf("list PR %d labels: %w", prNumber, err)
	}
	return names, nil
}

// fjPRCommits applies the same headline extraction as parsePRCommits: trim,
// skip empty, first line only (Forgejo messages carry a trailing newline on
// the wire).
func (c *Client) fjPRCommits(prNumber int) ([]string, error) {
	fj, err := c.fjTransport()
	if err != nil {
		return nil, err
	}
	ctx, cancel := forgejoCallContext()
	defer cancel()
	commits, err := fj.ListPullCommits(ctx, c.Repo, prNumber, true)
	if err != nil {
		return nil, fmt.Errorf("list PR %d commits: %w", prNumber, err)
	}
	msgs := make([]string, 0, len(commits))
	for _, commit := range commits {
		message := strings.TrimSpace(commit.Message)
		if message == "" {
			continue
		}
		msgs = append(msgs, strings.SplitN(message, "\n", 2)[0])
	}
	return msgs, nil
}

// fjPRChangedFiles mirrors parsePRChangedFiles: trimmed, empties dropped.
func (c *Client) fjPRChangedFiles(prNumber int) ([]string, error) {
	fj, err := c.fjTransport()
	if err != nil {
		return nil, err
	}
	ctx, cancel := forgejoCallContext()
	defer cancel()
	names, err := fj.ListPullFiles(ctx, c.Repo, prNumber, true)
	if err != nil {
		return nil, fmt.Errorf("list PR %d files: %w", prNumber, err)
	}
	files := make([]string, 0, len(names))
	for _, entry := range names {
		if name := strings.TrimSpace(entry); name != "" {
			files = append(files, name)
		}
	}
	return files, nil
}
