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
//     always "" here; PRMergeStatus instead SYNTHESIZES its vocabulary in
//     fjPRMergeStatus (#1172 M4) — "behind" / "dirty" / "" only, never
//     "clean" / "unstable";
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

// ---------------------------------------------------------------------------
// Writes (#1172 M3). Same dispatch shape as the reads above: the exported
// github.go method branches into the fj* sibling, REST specifics live in
// internal/forgejo. The semantic contracts preserved here — comment-then-close
// abort, number-from-response-JSON, label-name resolution, EnsureLabel upsert
// — are pinned by the forgejo-mode write tests. MarkPRReady is the one write
// with NO forgejo sibling: EditPullRequestOption has no draft toggle on
// 16.0.1, so it keeps the fail-loud guard in github.go.
// ---------------------------------------------------------------------------

// fjCreatePR returns the new PR number from the response JSON `number` —
// never scraped from a URL: gh output carries /pull/N where Forgejo web URLs
// are /pulls/N, so URL scraping would break on exactly this forge.
func (c *Client) fjCreatePR(title, body, base, head string) (int, error) {
	fj, err := c.fjTransport()
	if err != nil {
		return 0, err
	}
	ctx, cancel := forgejoCallContext()
	defer cancel()
	n, err := fj.CreatePull(ctx, c.Repo, title, body, base, head)
	if err != nil {
		return 0, fmt.Errorf("create PR: %w", err)
	}
	return n, nil
}

func (c *Client) fjUpdatePRBody(prNumber int, body string) error {
	fj, err := c.fjTransport()
	if err != nil {
		return err
	}
	ctx, cancel := forgejoCallContext()
	defer cancel()
	if err := fj.EditPull(ctx, c.Repo, prNumber, forgejo.Edit{Body: &body}); err != nil {
		return fmt.Errorf("update PR %d body: %w", prNumber, err)
	}
	return nil
}

// fjClosePR preserves the gh path's comment-then-close contract: a non-empty
// comment is posted FIRST and its failure aborts the close (the PR stays open
// so the explanation is never lost); an empty comment skips the comment step
// entirely. Pulls share the issue number space, so the comment goes through
// the issue-comments route.
func (c *Client) fjClosePR(prNumber int, comment string) error {
	fj, err := c.fjTransport()
	if err != nil {
		return err
	}
	ctx, cancel := forgejoCallContext()
	defer cancel()
	if comment != "" {
		if err := fj.CreateComment(ctx, c.Repo, prNumber, comment); err != nil {
			return fmt.Errorf("comment PR %d: %w", prNumber, err)
		}
	}
	if err := fj.EditPull(ctx, c.Repo, prNumber, forgejo.Edit{State: "closed"}); err != nil {
		return fmt.Errorf("close PR %d: %w", prNumber, err)
	}
	return nil
}

// fjMergePRAtHead is the squash + delete-branch + head-bound merge —
// `gh pr merge --squash --delete-branch --match-head-commit` parity via
// MergePullRequestOption. The empty-SHA pre-flight already ran in
// MergePRAtHead (shared with the gh path). A refusal the transport classified
// as out-of-date/head-mismatch (forgejo.ErrMergeOutOfDate) is re-wrapped with
// the package sentinel so the orchestrator's AutoRebase branch matches it via
// errors.Is; every other failure surfaces raw and loud.
func (c *Client) fjMergePRAtHead(prNumber int, expectedHeadSHA string) error {
	fj, err := c.fjTransport()
	if err != nil {
		return err
	}
	ctx, cancel := forgejoCallContext()
	defer cancel()
	err = fj.MergePull(ctx, c.Repo, prNumber, forgejo.MergeOptions{
		Do:                     "squash",
		HeadCommitID:           expectedHeadSHA,
		DeleteBranchAfterMerge: true,
	})
	if err != nil {
		if errors.Is(err, forgejo.ErrMergeOutOfDate) {
			return fmt.Errorf("merge PR %d at head %s: %w: %w", prNumber, expectedHeadSHA, ErrMergeNotUpToDate, err)
		}
		return fmt.Errorf("merge PR %d at head %s: %w", prNumber, expectedHeadSHA, err)
	}
	return nil
}

// fjUpdateBranch merges base into the PR head server-side, style=merge — the
// gh `pr update-branch` default. The #547 contract carries over: the head SHA
// moves, so callers must not merge in the same pass.
func (c *Client) fjUpdateBranch(prNumber int) error {
	fj, err := c.fjTransport()
	if err != nil {
		return err
	}
	ctx, cancel := forgejoCallContext()
	defer cancel()
	if err := fj.UpdatePullBranch(ctx, c.Repo, prNumber); err != nil {
		return fmt.Errorf("update branch of PR %d: %w", prNumber, err)
	}
	return nil
}

// fjCloseIssue mirrors fjClosePR's comment-then-close contract on the issue
// PATCH route.
func (c *Client) fjCloseIssue(number int, comment string) error {
	fj, err := c.fjTransport()
	if err != nil {
		return err
	}
	ctx, cancel := forgejoCallContext()
	defer cancel()
	if comment != "" {
		if err := fj.CreateComment(ctx, c.Repo, number, comment); err != nil {
			return fmt.Errorf("comment issue %d: %w", number, err)
		}
	}
	if err := fj.EditIssue(ctx, c.Repo, number, forgejo.Edit{State: "closed"}); err != nil {
		return fmt.Errorf("close issue %d: %w", number, err)
	}
	return nil
}

// fjCreateIssue returns the new issue number from the response JSON. Label
// names are resolved to ids inside the transport (CreateIssueOption.labels is
// []int64); an unknown label name aborts loudly BEFORE the issue is created —
// gh parity, and outcome_repair calls EnsureLabel first so a miss here is a
// genuinely unknown label.
func (c *Client) fjCreateIssue(title, body string, labels []string) (int, error) {
	fj, err := c.fjTransport()
	if err != nil {
		return 0, err
	}
	ctx, cancel := forgejoCallContext()
	defer cancel()
	return fj.CreateIssue(ctx, c.Repo, title, body, labels)
}

func (c *Client) fjEditIssueBody(number int, body string) error {
	fj, err := c.fjTransport()
	if err != nil {
		return err
	}
	ctx, cancel := forgejoCallContext()
	defer cancel()
	if err := fj.EditIssue(ctx, c.Repo, number, forgejo.Edit{Body: &body}); err != nil {
		return fmt.Errorf("edit issue %d body: %w", number, err)
	}
	return nil
}

// fjAddIssueLabel passes the NAME verbatim — IssueLabelsOption accepts label
// names on this instance (contract §6), so no id resolution happens; adding
// an already-present label is a server-side no-op (idempotent-safe). Known
// gap flagged as a #1172 follow-up: the server silently drops unknown names
// (see forgejo.AddIssueLabels).
func (c *Client) fjAddIssueLabel(issueNumber int, label string) error {
	fj, err := c.fjTransport()
	if err != nil {
		return err
	}
	ctx, cancel := forgejoCallContext()
	defer cancel()
	return fj.AddIssueLabels(ctx, c.Repo, issueNumber, []string{label})
}

// fjRemoveIssueLabel removes by NAME (the DELETE path param is name-or-id).
// gh-parity split, riding on the server's own status codes: a label the issue
// does not carry answers 204 (no-op success); a label missing from the repo
// entirely answers non-2xx and stays a loud error.
func (c *Client) fjRemoveIssueLabel(issueNumber int, label string) error {
	fj, err := c.fjTransport()
	if err != nil {
		return err
	}
	ctx, cancel := forgejoCallContext()
	defer cancel()
	return fj.RemoveIssueLabel(ctx, c.Repo, issueNumber, label)
}

// fjEnsureLabelDefaultColor is sent when the caller provided no color for a
// label that must be CREATED — CreateLabelOption requires one (the gh path
// can omit --color because gh picks a color itself). Forgejo accepts both
// "#rrggbb" and bare "rrggbb" and stores bare.
const fjEnsureLabelDefaultColor = "#ededed"

// fjEnsureLabel is the upsert (`gh label create --force` parity). The name is
// already trimmed/non-empty (shared pre-flight in EnsureLabel). Existing label
// (exact name match first, then case-insensitive — same order as the
// transport's findLabelID, because Forgejo does not enforce name uniqueness
// under case folding): PATCH only the provided fields, skipping the call when
// there is nothing to update. Missing: POST with the default color when the
// caller omitted one. The caller's color goes on the wire verbatim, leading
// '#' intact, where the gh path strips it (gh wants bare rrggbb) — Forgejo
// accepts both forms and stores bare, so only the wire differs, not the
// outcome. Its only consumer (outcome_repair) tolerates failure, so errors
// stay informative rather than swallowed.
func (c *Client) fjEnsureLabel(name, color, description string) error {
	fj, err := c.fjTransport()
	if err != nil {
		return err
	}
	ctx, cancel := forgejoCallContext()
	defer cancel()
	color = strings.TrimSpace(color)
	description = strings.TrimSpace(description)
	labels, err := fj.ListRepoLabels(ctx, c.Repo)
	if err != nil {
		return fmt.Errorf("ensure label %q: %w", name, err)
	}
	if l, ok := fjFindRepoLabel(labels, name); ok {
		if color == "" && description == "" {
			return nil // exists, nothing to update — a field-less PATCH is banned
		}
		if err := fj.EditLabel(ctx, c.Repo, l.ID, color, description); err != nil {
			return fmt.Errorf("ensure label %q: %w", name, err)
		}
		return nil
	}
	if color == "" {
		color = fjEnsureLabelDefaultColor
	}
	if err := fj.CreateLabel(ctx, c.Repo, name, color, description); err != nil {
		return fmt.Errorf("ensure label %q: %w", name, err)
	}
	return nil
}

// fjFindRepoLabel returns the repo label matching name; an exact match wins
// over a case-insensitive one (mirrors the transport's findLabelID), so a repo
// carrying both "Bug" and "bug" gets the exact one PATCHed.
func fjFindRepoLabel(labels []forgejo.RepoLabel, name string) (forgejo.RepoLabel, bool) {
	for _, l := range labels {
		if l.Name == name {
			return l, true
		}
	}
	for _, l := range labels {
		if strings.EqualFold(l.Name, name) {
			return l, true
		}
	}
	return forgejo.RepoLabel{}, false
}

// fjComment serves CommentIssue AND CommentPR — pulls share the issue number
// space, so one comments route covers both (contract §9).
func (c *Client) fjComment(number int, body string) error {
	fj, err := c.fjTransport()
	if err != nil {
		return err
	}
	ctx, cancel := forgejoCallContext()
	defer cancel()
	return fj.CreateComment(ctx, c.Repo, number, body)
}

// fjCreateRelease anchors the release to the already-pushed tag. Divergence
// from the gh path, documented: Forgejo has no --generate-notes equivalent
// anywhere in its swagger, so the release body stays empty.
func (c *Client) fjCreateRelease(tag, title string) error {
	fj, err := c.fjTransport()
	if err != nil {
		return err
	}
	ctx, cancel := forgejoCallContext()
	defer cancel()
	return fj.CreateRelease(ctx, c.Repo, tag, title)
}

// ---------------------------------------------------------------------------
// CI/status rollup + review-gate reads + merge-state synthesis (#1172 M4).
// Forgejo has NO check-runs API, NO mergeable_state, and NO review-thread
// resolution API — the siblings below map what DOES exist (commit statuses,
// reviews + review comments, merge_base) onto the existing internal shapes so
// the shared aggregation/decision helpers (ciStatusFromREST,
// formatChecksOverview, namedStatusDecision, filterStreamFindings, ...) run
// unchanged on both forges.
// ---------------------------------------------------------------------------

// fjCombinedStatusForSHA maps forgejo's combined commit status onto the
// gh-wire combinedStatusResponse. Two live-pinned gotchas are already
// normalized by the transport: the per-status state arrives in the wire
// `.status` field (NOT `.state`), and the no-signal shape is
// {"state":"","total_count":0,"statuses":null} — which maps to State "" plus
// nil Statuses here, exactly the no-signal shape ciStatusFromREST treats as
// carrying no signal. CreatedAt is threaded through (RFC3339 verbatim) so the
// rollup fingerprint advances on a status re-run — forgejo has no check-run
// IDs to do that job.
func (c *Client) fjCombinedStatusForSHA(sha string) (combinedStatusResponse, error) {
	fj, err := c.fjTransport()
	if err != nil {
		return combinedStatusResponse{}, err
	}
	ctx, cancel := forgejoCallContext()
	defer cancel()
	combined, err := fj.CombinedStatus(ctx, c.Repo, sha)
	if err != nil {
		return combinedStatusResponse{}, err
	}
	out := combinedStatusResponse{State: combined.State}
	for _, st := range combined.Statuses {
		out.Statuses = append(out.Statuses, combinedStatusEntry{
			Context:     st.Context,
			State:       st.State,
			Description: st.Description,
			TargetURL:   st.TargetURL,
			CreatedAt:   st.CreatedAt,
		})
	}
	return out, nil
}

// fjPRCheckRollup is the statuses-only rollup: check-runs do not exist on
// forgejo, so Verdict is ciStatusFromREST(nil, combined) and
// PendingCheckRuns stays false (it means "a check-RUN is live"; a pending
// commit status surfaces as Verdict "pending" and as a pending commit_status
// signal instead — which is exactly why the supervisor's #425 escape must be
// disabled on forgejo rows: here statuses ARE the CI).
//
// NO-SIGNAL PARITY — DOCUMENTED LOUDLY, READ BEFORE RELYING ON IT: a head
// with ZERO commit statuses rolls up as Verdict "success", byte-identical to
// the GitHub no-signal behavior (a repo with no CI at all must not deadlock
// every PR). On the canary forgejo row the producer posts pending-first
// statuses (the llm-review pair) so a real head is never signal-free for
// long; CI statuses proper arrive with the Actions runner (M4-ops). Until
// then a PR whose producer never posts anything would read green — the
// review gate (default llm-review pair, pending until observed) is what
// still blocks its merge.
func (c *Client) fjPRCheckRollup(prNumber int) (PRCheckRollup, error) {
	sha, err := c.pullHeadSHA(prNumber)
	if err != nil {
		return PRCheckRollup{Verdict: "unknown"}, fmt.Errorf("get pull %d head sha: %w", prNumber, err)
	}
	combined, err := c.fjCombinedStatusForSHA(sha)
	if err != nil {
		// Identity-wrapped (%w), unlike the gh path's joint %v formatting:
		// there is only one CI source here and its transport faults (token
		// env, *forgejo.StatusError) must stay errors.Is/As-matchable.
		return PRCheckRollup{HeadSHA: sha, Verdict: "unknown"}, fmt.Errorf("get statuses for PR %d: %w", prNumber, err)
	}
	rollup := PRCheckRollup{
		HeadSHA:          sha,
		Verdict:          ciStatusFromREST(nil, combined),
		Complete:         true,
		PendingCheckRuns: false,
	}
	rollup.Fingerprint = ciCheckRollupFingerprint(nil, combined)
	rollup.Signals = ciCheckRollupSignals(nil, combined)
	return rollup, nil
}

// fjPRChecksOutput renders the statuses-only overview through the shared
// formatter (name, state, description columns; "no checks\n" when empty).
func (c *Client) fjPRChecksOutput(prNumber int) (string, error) {
	sha, err := c.pullHeadSHA(prNumber)
	if err != nil {
		return "", fmt.Errorf("get pull %d head sha: %w", prNumber, err)
	}
	combined, err := c.fjCombinedStatusForSHA(sha)
	if err != nil {
		return "", fmt.Errorf("get statuses for PR %d: %w", prNumber, err)
	}
	return formatChecksOverview(nil, combined), nil
}

// fjFailingStatusState reports whether a commit-status state is a hard CI
// failure. ONLY "failure" and "error" qualify — "warning" and "skipped" are
// real Forgejo per-status states (live-verified) and must never be classified
// as failing, mirroring how ciStatusFromREST's aggregate arm only hardens on
// failure/error.
func fjFailingStatusState(state string) bool {
	switch strings.ToLower(strings.TrimSpace(state)) {
	case "failure", "error":
		return true
	}
	return false
}

// fjPRFailingChecks maps failing commit statuses onto FailingCheck. The
// status Description is the only actionable text a forgejo status carries
// (no check-run output, no annotations), so it is threaded through as the
// excerpt — empty descriptions degrade to name + conclusion, same contract
// as the gh path (#857).
func (c *Client) fjPRFailingChecks(prNumber int) ([]FailingCheck, error) {
	sha, err := c.pullHeadSHA(prNumber)
	if err != nil {
		return nil, fmt.Errorf("get pull %d head sha: %w", prNumber, err)
	}
	combined, err := c.fjCombinedStatusForSHA(sha)
	if err != nil {
		return nil, fmt.Errorf("get statuses for PR %d: %w", prNumber, err)
	}
	var failing []FailingCheck
	for _, st := range combined.Statuses {
		if !fjFailingStatusState(st.State) {
			continue
		}
		failing = append(failing, FailingCheck{
			Name:       st.Context,
			Conclusion: st.State,
			Excerpt:    strings.TrimSpace(st.Description),
		})
	}
	return failing, nil
}

// fjCIFailureSummary mirrors the gh CIFailureSummary shape — overview, then a
// section per failed status — with the status description standing in for the
// check-run output (nothing else exists on forgejo) and the same 8000-byte
// cap. Downstream errors are swallowed exactly like the gh path: the summary
// degrades to the overview, and the overview itself degrades to the error
// text, because this string feeds a retry-worker prompt and must never abort
// the retry.
func (c *Client) fjCIFailureSummary(prNumber int) (string, error) {
	overview, err := c.fjPRChecksOutput(prNumber)
	if err != nil {
		overview = err.Error()
	}
	sha, err := c.pullHeadSHA(prNumber)
	if err != nil || sha == "" {
		return overview, nil
	}
	combined, err := c.fjCombinedStatusForSHA(sha)
	if err != nil {
		return overview, nil
	}
	var failed []combinedStatusEntry
	for _, st := range combined.Statuses {
		if fjFailingStatusState(st.State) {
			failed = append(failed, st)
		}
	}
	if len(failed) == 0 {
		return overview, nil
	}

	var result strings.Builder
	result.WriteString("CI Check Overview:\n")
	result.WriteString(overview)
	result.WriteString("\n\n")
	for _, st := range failed {
		result.WriteString(fmt.Sprintf("=== Failed check: %s ===\n", st.Context))
		if desc := strings.TrimSpace(st.Description); desc != "" {
			result.WriteString(desc)
			result.WriteString("\n")
		}
		if st.TargetURL != "" {
			result.WriteString("Details: ")
			result.WriteString(st.TargetURL)
			result.WriteString("\n")
		}
		result.WriteString("\n")
	}
	s := result.String()
	if len(s) > 8000 {
		s = s[:8000] + "\n... (truncated)"
	}
	return s, nil
}

// fjPRMergeStatus synthesizes the mergeable_state vocabulary forgejo does not
// have (#1172 M4 D3). The mergeable verdict is the shared
// mergeableFromRESTPull over fjPullToREST (draft-contaminated false → nil →
// "UNKNOWN" rule included). The synthesized mergeStateStatus is:
//
//   - "dirty"  — non-draft wire mergeable == false (a real conflict, or the
//     seconds-long async re-check window — same accepted transient as
//     PRMergeable's CONFLICTING). Checked FIRST: GitHub reports a conflicting
//     PR as dirty even when it is also behind, and the conflict routing
//     (approver execution_failed on CONFLICTING) is the stronger signal.
//   - "behind" — the pull's merge_base differs from the CURRENT base-branch
//     head (BranchHeadSHA(BaseRef), a second read — never base.sha from the
//     same payload, which moves after merges and would mask "behind").
//     This revives the approver's behind→UpdateBranch routing on forgejo.
//   - ""       — everything else: callers read it as "not computed, proceed".
//
// "clean" and "unstable" are DELIBERATELY never synthesized. Both unlock
// GitHub-semantics escapes that assert "every required check passed", which
// has no forgejo equivalent until branch protection + Actions land:
//
//   - the #424 pending→success promotion (orchestrator
//     pr_gate_progress.go observePRGateCI) requires "clean" — permanently
//     inert on forgejo rows;
//   - mergeStateAllowsMerge (supervisor #425 escape and review_repair's
//     CI-pending arm) requires "clean"/"unstable" — both arms permanently
//     inert on forgejo rows (M7 observation item; the supervisor
//     additionally hard-gates the #425 escape on !IsForgejo so the
//     inertness is double-guarded).
//
// An error from the base-branch head read fails the whole call (identity-
// wrapped): callers treat a PRMergeStatus error as "no signal" and stay on
// their conservative paths.
func (c *Client) fjPRMergeStatus(prNumber int) (mergeable string, mergeStateStatus string, err error) {
	fj, err := c.fjTransport()
	if err != nil {
		return "", "", err
	}
	ctx, cancel := forgejoCallContext()
	defer cancel()
	p, err := fj.GetPull(ctx, c.Repo, prNumber)
	if err != nil {
		return "", "", fmt.Errorf("get pull %d: %w", prNumber, err)
	}
	mergeable = mergeableFromRESTPull(fjPullToREST(p))
	if !p.Draft && p.Mergeable != nil && !*p.Mergeable {
		return mergeable, "dirty", nil
	}
	mergeBase := strings.TrimSpace(p.MergeBase)
	baseRef := strings.TrimSpace(p.BaseRef)
	if mergeBase == "" || baseRef == "" {
		// merge_base absent on the wire: "behind" cannot be determined, and
		// "" is the honest "not computed" answer — never a guess.
		return mergeable, "", nil
	}
	baseHead, err := fj.BranchHeadSHA(ctx, c.Repo, baseRef)
	if err != nil {
		return "", "", fmt.Errorf("get base branch %q head for PR %d: %w", baseRef, prNumber, err)
	}
	if !strings.EqualFold(mergeBase, strings.TrimSpace(baseHead)) {
		return mergeable, "behind", nil
	}
	return mergeable, "", nil
}

// fjReviewComments is the forgejo arm of greptileReviewComments (the generic
// inline-comment reader): reviews list + per-review comments, flattened into
// the gh review-comment shape.
//
// CommitID head-anchoring contract (D4): the REVIEW's commit_id is the
// primary anchor — the llm-review producer writes one-comment COMMENT reviews
// anchored to the head it reviewed, and without that anchor
// reviewCommentTargetsHead degenerates to "matches every head" and historical
// findings leak across pushes. A review-level commit_id can legitimately be
// "" (live-proven on migrated reviews), so the per-comment commit_id — always
// populated — is the fallback. OriginalCommitID is deliberately left empty:
// reviewCommentTargetsHead consults it FIRST when non-empty, and forgejo's
// original_commit_id semantics across force-pushes are unpinned — the
// review-anchored CommitID is the reliable head test.
//
// Body-only reviews (CommentsCount == 0) skip the per-review round trip.
func (c *Client) fjReviewComments(prNumber int) ([]greptileReviewComment, error) {
	fj, err := c.fjTransport()
	if err != nil {
		return nil, err
	}
	ctx, cancel := forgejoCallContext()
	defer cancel()
	reviews, err := fj.ListPullReviews(ctx, c.Repo, prNumber, true)
	if err != nil {
		return nil, fmt.Errorf("list PR %d reviews: %w", prNumber, err)
	}
	var comments []greptileReviewComment
	for _, review := range reviews {
		if review.CommentsCount == 0 {
			continue
		}
		rcs, err := fj.ListPullReviewComments(ctx, c.Repo, prNumber, review.ID)
		if err != nil {
			return nil, fmt.Errorf("list PR %d review %d comments: %w", prNumber, review.ID, err)
		}
		for _, rc := range rcs {
			commitID := strings.TrimSpace(review.CommitID)
			if commitID == "" {
				commitID = strings.TrimSpace(rc.CommitID)
			}
			cm := greptileReviewComment{
				Body: rc.Body,
				Path: rc.Path,
				// Line ← the wire `position` field (transport PullReviewComment
				// .Line), which on the forgejo READ side is the NEW-file line
				// number (x-go-name LineNum) — NOT a diff offset; there is no
				// `line`/`new_position` on reads (stage-A live pin).
				Line:     int(rc.Line),
				CommitID: commitID,
			}
			cm.User.Login = rc.Author
			comments = append(comments, cm)
		}
	}
	return comments, nil
}

// fjUnresolvedReviewThreadsOnHead is the D5 mergegate shim: forgejo has no
// unresolved-conversations API, so it answers "no unresolved threads" while
// still providing the boundary's second independent head read from the ported
// pull read. An empty head refuses loudly — the same refusal the gh GraphQL
// parse enforces — so the compare/claim boundary never anchors to "".
func (c *Client) fjUnresolvedReviewThreadsOnHead(prNumber int) (string, []ReviewThread, error) {
	rp, err := c.fjGetRESTPull(prNumber)
	if err != nil {
		return "", nil, fmt.Errorf("read review threads for PR %d: %w", prNumber, err)
	}
	head := strings.TrimSpace(rp.Head.SHA)
	if head == "" {
		return "", nil, fmt.Errorf("read review threads for PR %d: pull request head SHA is empty", prNumber)
	}
	return head, nil, nil
}
