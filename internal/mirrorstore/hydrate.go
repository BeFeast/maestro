package mirrorstore

import (
	"context"
	"fmt"
	"time"

	"github.com/befeast/maestro/internal/github"
)

// GitHubClient is the narrow slice of *github.Client the hydrator needs to fill a
// mirror miss. *github.Client satisfies it; tests supply a fake so hydration is
// exercised without touching the network.
//
// Only the reads a mirror miss triggers are listed here — the interface exists to
// keep hydration testable and to document exactly which API calls the mirror can
// still make. It deliberately does not grow into a full GitHub surface: the point
// of the mirror is to stop calling the API, not to wrap it.
type GitHubClient interface {
	GetIssue(number int) (github.Issue, error)
}

// Hydrator serves reads from the mirror, falling back to the GitHub API on a
// miss and recording the result so the next read is local (#825 AC 3). It is the
// bridge that lets the mirror converge to full coverage without a bulk backfill:
// every entity a reader actually asks for is fetched once and then mirrored.
//
// Hydration stamps source = "api" and last_seen_at = the fetch time (now). Unlike
// a webhook, an API fetch has no server-side resource timestamp on
// github.Issue, so it uses wall-clock arrival as the ordering key. A genuinely
// newer webhook that arrives afterwards still wins (its resource updated_at is
// later); a webhook that predates the fetch is correctly rejected as stale.
type Hydrator struct {
	store  *Store
	client GitHubClient
	repo   string
	now    func() time.Time
}

// NewHydrator builds a hydrator over store for repo, backed by client for
// misses. repo is the "owner/name" the mirror rows are keyed on.
func NewHydrator(store *Store, client GitHubClient, repo string) *Hydrator {
	return &Hydrator{
		store:  store,
		client: client,
		repo:   repo,
		now:    func() time.Time { return time.Now().UTC() },
	}
}

// Issue returns the issue for number: from the mirror when present, otherwise
// fetched from the API, stored (source = "api"), and returned. After a hydrating
// call the row is local, so the immediately following call is served from the
// mirror with no API traffic — the "mirror miss → hydrate → served locally"
// acceptance.
//
// A miss with no client wired is reported as an error rather than a silent empty
// issue, so a caller does not mistake "not configured" for "does not exist".
func (h *Hydrator) Issue(ctx context.Context, number int) (Issue, error) {
	row, ok, err := h.store.GetIssue(ctx, h.repo, number)
	if err != nil {
		return Issue{}, err
	}
	if ok {
		return row, nil
	}
	if h.client == nil {
		return Issue{}, fmt.Errorf("mirror miss for issue %d and no GitHub client to hydrate from", number)
	}
	gh, err := h.client.GetIssue(number)
	if err != nil {
		return Issue{}, fmt.Errorf("hydrate issue %d: %w", number, err)
	}
	now := h.now()
	row = Issue{
		Repo:       h.repo,
		Number:     gh.Number,
		Title:      gh.Title,
		State:      issueStateFromGitHub(gh),
		Body:       gh.Body,
		LastSeenAt: now,
		Source:     SourceAPI,
	}
	if _, err := h.store.UpsertIssue(ctx, row); err != nil {
		return Issue{}, fmt.Errorf("store hydrated issue %d: %w", number, err)
	}
	if err := h.store.ReplaceLabels(ctx, h.repo, SubjectIssue, gh.Number, issueLabelNames(gh), now, SourceAPI); err != nil {
		return Issue{}, fmt.Errorf("store hydrated issue %d labels: %w", number, err)
	}
	// Re-read so the returned row reflects exactly what a subsequent local read
	// would see (e.g. if a fresher webhook row was already present, the guarded
	// upsert kept it and this read returns that, not the API snapshot).
	stored, _, err := h.store.GetIssue(ctx, h.repo, number)
	if err != nil {
		return Issue{}, err
	}
	return stored, nil
}

// issueStateFromGitHub reports the issue state. github.Issue does not carry an
// explicit state field (the REST fetch drops it), so a hydrated issue is recorded
// as "open" — the mirror only hydrates issues a reader is actively working, which
// are open by construction. A later close arrives as an "issues"/"closed" webhook
// and updates the row.
func issueStateFromGitHub(github.Issue) string {
	return "open"
}

func issueLabelNames(gh github.Issue) []string {
	out := make([]string, 0, len(gh.Labels))
	for _, l := range gh.Labels {
		out = append(out, l.Name)
	}
	return out
}
