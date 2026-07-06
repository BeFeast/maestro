package mirrorstore

import (
	"context"
	"strings"
	"sync/atomic"
	"time"

	"github.com/befeast/maestro/internal/github"
)

// Source is the mirror-first read source the supervisor and orchestrator poll
// loops read GitHub through (#826, epic #811 phase D). It serves the high-volume
// per-cycle reads — the open-issue and open-PR lists and the issue/PR state
// point reads — from the local SQLite mirror when the mirror is warm, and falls
// back to the GitHub API only on a miss, a stale row, a cold mirror, or the
// escape hatch. Every fallback is counted so the journal digest can show the
// reduction (#826 AC 5).
//
// It EMBEDS *github.Client, so a Source is a drop-in wherever the concrete
// client was used: every method the Source does not override — all writes
// (labels, comments, merges), and the reads the mirror cannot faithfully serve
// (mergeable state, review-gate verdicts, CI status, project board) — passes
// straight through to GitHub, which stays authoritative. Only the reads listed
// below are intercepted:
//
//   - ListOpenIssues  — mirror_issues (+ mirror_labels), loss-free
//   - ListOpenPRs     — mirror_pull_requests (head_ref/body mirrored in phase D)
//   - IsIssueClosed   — mirror_issues.state
//   - IsPRMerged      — mirror_pull_requests.merged
//   - GetIssue        — mirror hit, else hydrate (fetch → store → serve)
//
// Merge-gating reads (PRCIStatus, PRMergeStatus, PRGreptileApproved, review
// verdicts) are deliberately NOT mirror-served: the mirror cannot prove check
// completeness, and a stale "green" would be a merge-safety regression. They
// stay on the authoritative API — the conservative choice AC 4 (no
// decision-quality regression) demands.
type Source struct {
	*github.Client // pass-through: writes + reads the mirror does not serve

	store *Store
	repo  string
	// api is the fallback read client for the overridden methods, injectable so
	// tests can drive the fallback path without a real gh binary. In production it
	// is the same *github.Client that is embedded.
	api apiReader
	// horizon is the freshness window: a mirror row (or list) older than this is
	// stale and triggers an API fallback. <= 0 means "never stale" — every present
	// row is served locally.
	horizon time.Duration
	// apiDirect is the escape hatch, consulted per read so a live config-store
	// edit flips the whole fleet back to API-direct without a redeploy (#826 AC
	// 3). nil means "always mirror-first".
	apiDirect func() bool
	now       func() time.Time
}

// apiReader is the slice of *github.Client the mirror-first reads fall back to.
// *github.Client satisfies it; tests supply a fake.
type apiReader interface {
	ListOpenIssues(labels []string) ([]github.Issue, error)
	ListOpenPRs() ([]github.PR, error)
	IsIssueClosed(number int) (bool, error)
	IsPRMerged(prNumber int) (bool, error)
	GetIssue(number int) (github.Issue, error)
}

// SourceOptions tunes a Source. The zero value is valid: no staleness horizon
// (every present row served), always mirror-first, wall-clock now.
type SourceOptions struct {
	// Horizon is the staleness window; <= 0 means never stale.
	Horizon time.Duration
	// APIDirect, when non-nil and returning true, forces every read to the API —
	// the fleet-wide escape hatch. Evaluated per read for hot-reload.
	APIDirect func() bool
	// Now overrides the clock (tests). Defaults to time.Now().UTC().
	Now func() time.Time
}

// NewSource builds a mirror-first Source over store for repo, backed by client
// for API fallback and pass-through. A nil store or nil client yields a Source
// that behaves API-direct (it can only pass through), so wiring is fail-safe.
func NewSource(client *github.Client, store *Store, repo string, opts SourceOptions) *Source {
	now := opts.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &Source{
		Client:    client,
		store:     store,
		repo:      strings.TrimSpace(repo),
		api:       client,
		horizon:   opts.Horizon,
		apiDirect: opts.APIDirect,
		now:       now,
	}
}

func (s *Source) ctx() context.Context { return context.Background() }

// apiDirectNow reports whether the escape hatch is engaged, or the source has no
// usable mirror to read from.
func (s *Source) apiDirectNow() bool {
	if s.store == nil {
		return true
	}
	return s.apiDirect != nil && s.apiDirect()
}

// fresh reports whether a row last seen at lastSeen is within the horizon.
func (s *Source) fresh(lastSeen time.Time) bool {
	return Classify(lastSeen, s.now(), s.horizon) == Fresh
}

// ListOpenIssues serves the open-issue list from the mirror when it is warm,
// falling back to the API otherwise. Warmth (the newest mirrored issue is within
// the horizon) is what lets an empty/cold mirror degrade to today's API-direct
// correctness instead of silently under-reporting an unpopulated list (AC 7).
func (s *Source) ListOpenIssues(labels []string) ([]github.Issue, error) {
	if s.apiDirectNow() {
		return s.apiListOpenIssues(labels)
	}
	newest, ok, err := s.store.NewestIssueAt(s.ctx(), s.repo)
	if err != nil || !ok || !s.fresh(newest) {
		return s.apiListOpenIssues(labels)
	}
	rows, err := s.store.ListOpenIssues(s.ctx(), s.repo, labels)
	if err != nil {
		return s.apiListOpenIssues(labels)
	}
	// The newest-row warmth check only proves webhooks are flowing for the repo as
	// a whole; it does not vouch for every individual row. A single open row older
	// than the horizon may have had its close/unlabel delivery missed, so it could
	// be a no-longer-open (or mis-labelled) issue the mirror is still exposing —
	// which would leak into supervisor/orchestrator decisions. Reject the whole
	// list to the authoritative API if any returned row is itself stale (#826).
	for i := range rows {
		if !s.fresh(rows[i].LastSeenAt) {
			return s.apiListOpenIssues(labels)
		}
	}
	noteMirrorHit()
	out := make([]github.Issue, 0, len(rows))
	for _, r := range rows {
		out = append(out, issueToGitHub(r))
	}
	return out, nil
}

// ListOpenPRs serves the open-PR list from the mirror when warm, else the API.
func (s *Source) ListOpenPRs() ([]github.PR, error) {
	if s.apiDirectNow() {
		return s.apiListOpenPRs()
	}
	newest, ok, err := s.store.NewestPullRequestAt(s.ctx(), s.repo)
	if err != nil || !ok || !s.fresh(newest) {
		return s.apiListOpenPRs()
	}
	rows, err := s.store.ListOpenPullRequests(s.ctx(), s.repo)
	if err != nil {
		return s.apiListOpenPRs()
	}
	// As in ListOpenIssues, reject the list to the API if any returned open PR is
	// itself stale (a missed close delivery could keep a merged/closed PR "open").
	// ALSO reject it if any row is missing its head branch: a mirror upgraded before
	// the head_ref column existed backfills head_ref = '' on every already-open PR
	// (store.go migrate), and those rows keep their old — possibly still fresh —
	// last_seen_at, so the staleness check alone would not catch them. The
	// supervisor/orchestrator match running sessions to open PRs BY HEAD BRANCH, so
	// serving a PR with an empty HeadRefName would mis-match or duplicate work; fall
	// back to GitHub until a webhook repopulates the branch (#826).
	for i := range rows {
		if !s.fresh(rows[i].LastSeenAt) || strings.TrimSpace(rows[i].HeadRef) == "" {
			return s.apiListOpenPRs()
		}
	}
	noteMirrorHit()
	out := make([]github.PR, 0, len(rows))
	for _, r := range rows {
		out = append(out, prToGitHub(r))
	}
	return out, nil
}

// IsIssueClosed answers from a fresh mirror issue row, else the API.
func (s *Source) IsIssueClosed(number int) (bool, error) {
	if s.apiDirectNow() {
		return s.apiIsIssueClosed(number)
	}
	row, ok, err := s.store.GetIssue(s.ctx(), s.repo, number)
	if err != nil || !ok || !s.fresh(row.LastSeenAt) {
		return s.apiIsIssueClosed(number)
	}
	noteMirrorHit()
	return strings.EqualFold(strings.TrimSpace(row.State), "closed"), nil
}

// IsPRMerged answers from a fresh mirror PR row, else the API. "merged" is a
// terminal, monotonic state, so a lagging mirror can at worst report a
// not-yet-merged PR (which re-polls) — it never fabricates a merge.
func (s *Source) IsPRMerged(prNumber int) (bool, error) {
	if s.apiDirectNow() {
		return s.apiIsPRMerged(prNumber)
	}
	row, ok, err := s.store.GetPullRequest(s.ctx(), s.repo, prNumber)
	if err != nil || !ok || !s.fresh(row.LastSeenAt) {
		return s.apiIsPRMerged(prNumber)
	}
	noteMirrorHit()
	return row.Merged, nil
}

// GetIssue serves a fresh mirror issue, otherwise HYDRATES: it fetches the issue
// from the API, records it (source = "api"), and returns it — so the next read
// is local. This is the miss-degrades-to-API-direct path (AC 7): an empty mirror
// converges to full coverage one requested entity at a time, with no bulk
// backfill.
func (s *Source) GetIssue(number int) (github.Issue, error) {
	if s.apiDirectNow() {
		return s.apiGetIssue(number)
	}
	row, ok, err := s.store.GetIssue(s.ctx(), s.repo, number)
	if err == nil && ok && s.fresh(row.LastSeenAt) {
		labels, lerr := s.store.Labels(s.ctx(), s.repo, SubjectIssue, number)
		if lerr == nil {
			row.Labels = labels
			noteMirrorHit()
			return issueToGitHub(row), nil
		}
	}
	// Miss / stale / mirror error → hydrate from the API and persist.
	gh, err := s.apiGetIssue(number)
	if err != nil {
		return github.Issue{}, err
	}
	s.hydrateIssue(gh)
	return gh, nil
}

// hydrateIssue stores an API-fetched issue into the mirror so the next GetIssue
// is served locally. A store failure is swallowed: hydration is best-effort
// convergence, and the correct answer was already returned to the caller.
func (s *Source) hydrateIssue(gh github.Issue) {
	if s.store == nil {
		return
	}
	labels := make([]string, 0, len(gh.Labels))
	for _, l := range gh.Labels {
		labels = append(labels, l.Name)
	}
	state := strings.ToLower(strings.TrimSpace(gh.State))
	if state == "" {
		state = "open"
	}
	_, _ = s.store.UpsertIssueWithLabels(s.ctx(), Issue{
		Repo:       s.repo,
		Number:     gh.Number,
		Title:      gh.Title,
		State:      state,
		Body:       gh.Body,
		LastSeenAt: s.now(),
		Source:     SourceAPI,
	}, labels)
}

// ---- API fallbacks (each records the fallback) -----------------------------

func (s *Source) apiListOpenIssues(labels []string) ([]github.Issue, error) {
	noteAPIFallback()
	return s.api.ListOpenIssues(labels)
}

func (s *Source) apiListOpenPRs() ([]github.PR, error) {
	noteAPIFallback()
	return s.api.ListOpenPRs()
}

func (s *Source) apiIsIssueClosed(number int) (bool, error) {
	noteAPIFallback()
	return s.api.IsIssueClosed(number)
}

func (s *Source) apiIsPRMerged(prNumber int) (bool, error) {
	noteAPIFallback()
	return s.api.IsPRMerged(prNumber)
}

func (s *Source) apiGetIssue(number int) (github.Issue, error) {
	noteAPIFallback()
	return s.api.GetIssue(number)
}

// ---- conversions -----------------------------------------------------------

// ghLabel is the exact anonymous label type github.Issue.Labels holds, so a
// reconstructed issue reports labels in the same shape a REST fetch would.
type ghLabel = struct {
	Name string `json:"name"`
}

func issueToGitHub(r Issue) github.Issue {
	gh := github.Issue{
		Number: r.Number,
		Title:  r.Title,
		Body:   r.Body,
		State:  r.State,
	}
	for _, name := range r.Labels {
		gh.Labels = append(gh.Labels, ghLabel{Name: name})
	}
	return gh
}

func prToGitHub(r PullRequest) github.PR {
	// State is upper-cased to match github.Client's REST mapping (restPull.pr()).
	// Mergeable and MergedAt are left empty on purpose: the REST list endpoint
	// returns them empty for open PRs too (mergeable is only computed by the
	// single-PR endpoint), so a mirror-served list matches in every field a
	// ListOpenPRs consumer actually reads.
	return github.PR{
		Number:      r.Number,
		HeadRefName: r.HeadRef,
		State:       strings.ToUpper(strings.TrimSpace(r.State)),
		Title:       r.Title,
		Body:        r.Body,
		IsDraft:     r.Draft,
	}
}

// ---- process-global read metrics -------------------------------------------
//
// Counted process-wide (like github.APIUsage and the primary-limit gate) so the
// journal digest can read them without threading a Source handle through every
// call site. MirrorHits are reads served locally; APIFallbacks are reads that
// reached the GitHub API through the source (miss, stale, cold mirror, or the
// escape hatch). Together they quantify how much the mirror saved (#826 AC 5).

var (
	mirrorHitsTotal   atomic.Int64
	apiFallbacksTotal atomic.Int64
)

func noteMirrorHit()   { mirrorHitsTotal.Add(1) }
func noteAPIFallback() { apiFallbacksTotal.Add(1) }

// ReadStats is a snapshot of the mirror-first read counters.
type ReadStats struct {
	MirrorHits   int64 `json:"mirror_hits"`
	APIFallbacks int64 `json:"api_fallbacks"`
}

// Total is the number of source reads observed.
func (r ReadStats) Total() int64 { return r.MirrorHits + r.APIFallbacks }

// HitRate is the fraction of source reads served from the mirror (0 when none).
func (r ReadStats) HitRate() float64 {
	total := r.Total()
	if total == 0 {
		return 0
	}
	return float64(r.MirrorHits) / float64(total)
}

// ReadStats returns the process-lifetime mirror-first read counters.
func ReadStatsSnapshot() ReadStats {
	return ReadStats{
		MirrorHits:   mirrorHitsTotal.Load(),
		APIFallbacks: apiFallbacksTotal.Load(),
	}
}

// resetReadStatsForTest zeroes the counters so a test starts from a known base.
func resetReadStatsForTest() {
	mirrorHitsTotal.Store(0)
	apiFallbacksTotal.Store(0)
}
