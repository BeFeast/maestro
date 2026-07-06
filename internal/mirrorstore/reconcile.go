package mirrorstore

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/befeast/maestro/internal/github"
)

// SourceReconcile marks a mirror row last written by the reconciliation loop's
// authoritative GitHub snapshot (#827, epic #811 phase E), distinct from a pushed
// webhook delivery (SourceWebhook) or an on-demand hydration on a read miss
// (SourceAPI). A reader can tell state that arrived by push from drift a full
// snapshot had to repair — the latter is the signal that a delivery was missed.
const SourceReconcile = "reconcile"

// ReconcileReader is the slice of *github.Client the reconciler needs to take an
// authoritative snapshot of a repo's open issues and pull requests. *github.Client
// satisfies it; tests supply a fake so the drift logic is exercised without a real
// gh binary.
//
// ListAllOpenIssues / ListAllOpenPRs are the PAGINATED list calls (they follow
// gh's --paginate, fetching every page). The reconciler treats a mirrored-open
// row absent from these lists as a missed close and stamps it closed, so it must
// see GitHub's COMPLETE open set — the single-page ListOpenIssues/ListOpenPRs
// (first 100 only) would wrongly close every still-open entity past the first
// page on a repo with more than 100 open issues or PRs (#827).
//
// Every read still flows through the package's conditional (ETag / If-None-Match)
// request layer (internal/github, #797): a repo whose open-issue / open-PR lists
// have not changed since the last pass answers 304 Not Modified and the whole
// reconcile costs near-zero core quota — the "cheap when nothing changed"
// property (#827 AC 2). IsPRMerged is consulted only for a PR that has dropped out
// of the open set, so it is a rare per-drift call, not a per-cycle one.
type ReconcileReader interface {
	ListAllOpenIssues(labels []string) ([]github.Issue, error)
	ListAllOpenPRs() ([]github.PR, error)
	IsPRMerged(prNumber int) (bool, error)
}

// Reconciler compares the authoritative GitHub open-issue / open-PR sets for one
// repo against the local mirror and repairs any drift: an entity GitHub reports as
// open that the mirror is missing or has recorded differently is refreshed, and an
// entity the mirror still records as open that GitHub no longer lists is marked
// closed (its close/merge delivery was missed). The number of rows actually
// repaired is counted, so a webhook outage that the loop healed is visible in
// diagnostics without operator action (#827 AC 1).
//
// The loop is a SAFETY NET over webhook ingestion, not a replacement for it: it
// runs at a low cadence (config: github_mirror.reconcile_seconds) and only writes
// when it detects a divergence, so a fully-caught-up mirror does no mirror writes
// and — thanks to the ETag layer — issues no billed API calls.
type Reconciler struct {
	store *Store
	api   ReconcileReader
	repo  string
	now   func() time.Time
}

// NewReconciler builds a reconciler over store for repo, taking its authoritative
// GitHub snapshot through api. repo is the "owner/name" the mirror rows are keyed
// on.
func NewReconciler(store *Store, api ReconcileReader, repo string) *Reconciler {
	return &Reconciler{
		store: store,
		api:   api,
		repo:  strings.TrimSpace(repo),
		now:   func() time.Time { return time.Now().UTC() },
	}
}

// ReconcileResult reports what one reconcile pass observed and repaired.
type ReconcileResult struct {
	Repo           string    `json:"repo"`
	At             time.Time `json:"at"`
	IssuesChecked  int       `json:"issues_checked"`
	PRsChecked     int       `json:"prs_checked"`
	IssuesRepaired int       `json:"issues_repaired"`
	PRsRepaired    int       `json:"prs_repaired"`
}

// Repaired is the total number of mirror rows this pass had to correct — the
// drift the loop healed.
func (r ReconcileResult) Repaired() int { return r.IssuesRepaired + r.PRsRepaired }

// Reconcile runs one full pass and records it in the process-global reconcile
// stats (ReconcileStatsSnapshot). The pass is snapshot-consistent to a single
// instant captured before the GitHub reads: repaired rows are stamped with that
// instant so a genuinely-newer webhook that arrives concurrently (its resource
// updated_at is later) still wins the ordering guard and is not clobbered by the
// snapshot.
func (r *Reconciler) Reconcile(ctx context.Context) (ReconcileResult, error) {
	snap := r.now()
	res := ReconcileResult{Repo: r.repo, At: snap}
	if r.store == nil || r.api == nil {
		err := errors.New("mirror reconciler not fully configured (nil store or client)")
		noteReconcile(res, err)
		return res, err
	}

	// ---- Issues -------------------------------------------------------------
	ghIssues, err := r.api.ListAllOpenIssues(nil)
	if err != nil {
		err = fmt.Errorf("reconcile %s: list open issues: %w", r.repo, err)
		noteReconcile(res, err)
		return res, err
	}
	res.IssuesChecked = len(ghIssues)
	ghOpenIssue := make(map[int]bool, len(ghIssues))
	for _, iss := range ghIssues {
		ghOpenIssue[iss.Number] = true
		repaired, err := r.reconcileOpenIssue(ctx, iss, snap)
		if err != nil {
			err = fmt.Errorf("reconcile %s: issue #%d: %w", r.repo, iss.Number, err)
			noteReconcile(res, err)
			return res, err
		}
		if repaired {
			res.IssuesRepaired++
		}
	}
	closedIssues, err := r.closeMissedIssues(ctx, ghOpenIssue, snap)
	res.IssuesRepaired += closedIssues
	if err != nil {
		err = fmt.Errorf("reconcile %s: close missed issues: %w", r.repo, err)
		noteReconcile(res, err)
		return res, err
	}

	// ---- Pull requests ------------------------------------------------------
	ghPRs, err := r.api.ListAllOpenPRs()
	if err != nil {
		err = fmt.Errorf("reconcile %s: list open PRs: %w", r.repo, err)
		noteReconcile(res, err)
		return res, err
	}
	res.PRsChecked = len(ghPRs)
	ghOpenPR := make(map[int]bool, len(ghPRs))
	for _, pr := range ghPRs {
		ghOpenPR[pr.Number] = true
		repaired, err := r.reconcileOpenPR(ctx, pr, snap)
		if err != nil {
			err = fmt.Errorf("reconcile %s: PR #%d: %w", r.repo, pr.Number, err)
			noteReconcile(res, err)
			return res, err
		}
		if repaired {
			res.PRsRepaired++
		}
	}
	closedPRs, err := r.closeMissedPRs(ctx, ghOpenPR, snap)
	res.PRsRepaired += closedPRs
	if err != nil {
		err = fmt.Errorf("reconcile %s: close missed PRs: %w", r.repo, err)
		noteReconcile(res, err)
		return res, err
	}

	noteReconcile(res, nil)
	return res, nil
}

// reconcileOpenIssue refreshes the mirror for one GitHub-open issue when the
// mirror is missing it or has recorded it differently. It writes nothing — and
// returns repaired=false — when the mirror row already matches GitHub, so an
// up-to-date mirror does no work and keeps its webhook-sourced last_seen_at (a
// needless reconcile write would mask staleness). repaired=true means the guarded
// upsert actually applied.
func (r *Reconciler) reconcileOpenIssue(ctx context.Context, gh github.Issue, snap time.Time) (bool, error) {
	row, ok, err := r.store.GetIssue(ctx, r.repo, gh.Number)
	if err != nil {
		return false, err
	}
	want := sortedIssueLabels(gh)
	if ok {
		have, err := r.store.Labels(ctx, r.repo, SubjectIssue, gh.Number)
		if err != nil {
			return false, err
		}
		if issueRowMatches(row, gh, have) {
			return false, nil
		}
	}
	return r.store.UpsertIssueWithLabels(ctx, Issue{
		Repo:       r.repo,
		Number:     gh.Number,
		Title:      gh.Title,
		State:      normalizeState(gh.State, "open"),
		Body:       gh.Body,
		LastSeenAt: snap,
		Source:     SourceReconcile,
	}, want)
}

// closeMissedIssues marks closed every mirror issue still recorded as open that
// GitHub's authoritative open set no longer contains — the missed-close-delivery
// case. GitHub was asked for state=open (paginated, so the set is complete), so
// absence is unambiguous: the issue is no longer open. Returns the number of rows
// the guarded upsert actually closed.
func (r *Reconciler) closeMissedIssues(ctx context.Context, ghOpen map[int]bool, snap time.Time) (int, error) {
	mirrorOpen, err := r.store.ListOpenIssues(ctx, r.repo, nil)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, m := range mirrorOpen {
		if ghOpen[m.Number] {
			continue
		}
		applied, err := r.store.UpsertIssue(ctx, Issue{
			Repo:       r.repo,
			Number:     m.Number,
			Title:      m.Title,
			State:      "closed",
			Body:       m.Body,
			LastSeenAt: snap,
			Source:     SourceReconcile,
		})
		if err != nil {
			return n, err
		}
		if applied {
			n++
		}
	}
	return n, nil
}

// reconcileOpenPR refreshes the mirror for one GitHub-open PR. As with issues it
// writes only on a divergence. It preserves head_sha and base_ref from the
// existing row — the open-PR list read does not carry them — so a reconcile that
// repairs the title/head-branch does not blank a SHA a webhook recorded. This is
// also where a PR mirrored before the head_ref column existed (empty head_ref,
// possibly still "fresh") gets its branch backfilled, since the empty branch no
// longer matches GitHub's headRefName.
func (r *Reconciler) reconcileOpenPR(ctx context.Context, gh github.PR, snap time.Time) (bool, error) {
	row, ok, err := r.store.GetPullRequest(ctx, r.repo, gh.Number)
	if err != nil {
		return false, err
	}
	if ok && prRowMatchesOpen(row, gh) {
		return false, nil
	}
	headSHA, baseRef := "", ""
	if ok {
		headSHA, baseRef = row.HeadSHA, row.BaseRef
	}
	return r.store.UpsertPullRequest(ctx, PullRequest{
		Repo:       r.repo,
		Number:     gh.Number,
		Title:      gh.Title,
		State:      "open",
		Draft:      gh.IsDraft,
		Merged:     false,
		HeadSHA:    headSHA,
		HeadRef:    strings.TrimSpace(gh.HeadRefName),
		BaseRef:    baseRef,
		Body:       gh.Body,
		LastSeenAt: snap,
		Source:     SourceReconcile,
	})
}

// closeMissedPRs marks closed every mirror PR still recorded as open that GitHub
// no longer lists as open. Because a fabricated "not merged" for a merged PR would
// be a merge-detection regression, the authoritative merged flag is fetched for
// each dropped PR and stored alongside state=closed. A PR whose merged state
// cannot be determined this pass is NOT closed with a guessed flag; instead the
// lookup error is recorded and returned so the pass is marked failed (visible as
// reconcile[].failures / last_error in diagnostics) rather than silently skipped.
// A persistent merge-status error (e.g. a 404) therefore surfaces as a stuck,
// non-converging PR an operator can see — the missed row is not left to look like
// a healthy no-op — while a transient error simply clears on the next pass. The
// other dropped PRs in the same pass are still settled; one flaky lookup does not
// block them.
func (r *Reconciler) closeMissedPRs(ctx context.Context, ghOpen map[int]bool, snap time.Time) (int, error) {
	mirrorOpen, err := r.store.ListOpenPullRequests(ctx, r.repo)
	if err != nil {
		return 0, err
	}
	n := 0
	var mergeErrs []error
	for _, m := range mirrorOpen {
		if ghOpen[m.Number] {
			continue
		}
		merged, err := r.api.IsPRMerged(m.Number)
		if err != nil {
			// Cannot settle merged vs plain-closed right now. Record the failure
			// (rather than silently skip) so a PERSISTENT merge-status error does
			// not leave the row open every pass with no signal. Keep going: the
			// remaining dropped PRs are still settled this pass.
			mergeErrs = append(mergeErrs, fmt.Errorf("PR #%d merge status: %w", m.Number, err))
			continue
		}
		applied, err := r.store.UpsertPullRequest(ctx, PullRequest{
			Repo:       r.repo,
			Number:     m.Number,
			Title:      m.Title,
			State:      "closed",
			Draft:      m.Draft,
			Merged:     merged,
			HeadSHA:    m.HeadSHA,
			HeadRef:    m.HeadRef,
			BaseRef:    m.BaseRef,
			Body:       m.Body,
			LastSeenAt: snap,
			Source:     SourceReconcile,
		})
		if err != nil {
			return n, errors.Join(append(mergeErrs, err)...)
		}
		if applied {
			n++
		}
	}
	if len(mergeErrs) > 0 {
		return n, errors.Join(mergeErrs...)
	}
	return n, nil
}

// ---- comparison helpers ----------------------------------------------------

// issueRowMatches reports whether the mirror issue row (with its label set) is
// already in sync with GitHub's open snapshot, so reconcile can skip the write.
func issueRowMatches(row Issue, gh github.Issue, haveLabels []string) bool {
	if !strings.EqualFold(strings.TrimSpace(row.State), normalizeState(gh.State, "open")) {
		return false
	}
	if row.Title != gh.Title || row.Body != gh.Body {
		return false
	}
	return sameStrings(sortedStrings(haveLabels), sortedIssueLabels(gh))
}

// prRowMatchesOpen reports whether the mirror PR row already matches GitHub's
// open snapshot. head_sha and base_ref are not compared: the open-PR list read
// does not carry them, so they are preserved, not validated, here.
func prRowMatchesOpen(row PullRequest, gh github.PR) bool {
	return strings.EqualFold(strings.TrimSpace(row.State), "open") &&
		!row.Merged &&
		row.Title == gh.Title &&
		row.Body == gh.Body &&
		row.Draft == gh.IsDraft &&
		strings.TrimSpace(row.HeadRef) == strings.TrimSpace(gh.HeadRefName)
}

// normalizeState lower-cases GitHub's state, falling back to fallback when empty
// so the two write paths (webhook projection, reconcile) store the same value.
func normalizeState(state, fallback string) string {
	s := strings.ToLower(strings.TrimSpace(state))
	if s == "" {
		return fallback
	}
	return s
}

func sortedIssueLabels(gh github.Issue) []string {
	out := make([]string, 0, len(gh.Labels))
	for _, l := range gh.Labels {
		if n := strings.TrimSpace(l.Name); n != "" {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}

func sortedStrings(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// ---- process-global reconcile metrics --------------------------------------
//
// Counted process-wide, like the mirror-first read counters (ReadStats) and the
// gh wrapper's APIUsage, so the fleet diagnostics block and the journal digest can
// read them without threading a Reconciler handle through the daemon. Keyed by
// repo so "last successful reconcile per project" and the per-project drift-repair
// count are both surfaced (#827 observability).

// ReconcileStat is the observability snapshot for one repo's reconcile loop.
type ReconcileStat struct {
	Repo          string    `json:"repo"`
	LastRunAt     time.Time `json:"last_run_at"`
	LastSuccessAt time.Time `json:"last_success_at"`
	Runs          int64     `json:"runs"`
	Failures      int64     `json:"failures"`
	Repairs       int64     `json:"repairs"`      // cumulative rows repaired across all passes
	LastRepairs   int       `json:"last_repairs"` // rows repaired in the most recent pass
	LastError     string    `json:"last_error,omitempty"`
}

var (
	reconcileMu    sync.Mutex
	reconcileStats = map[string]*ReconcileStat{}
)

// noteReconcile records one reconcile pass (success or failure) into the
// process-global registry. Rows the pass repaired before it returned are always
// counted — the writes have already landed, so the drift-repair total must
// reflect them even when a later step (e.g. a merge-status lookup) failed.
func noteReconcile(res ReconcileResult, err error) {
	reconcileMu.Lock()
	defer reconcileMu.Unlock()
	st := reconcileStats[res.Repo]
	if st == nil {
		st = &ReconcileStat{Repo: res.Repo}
		reconcileStats[res.Repo] = st
	}
	st.Runs++
	st.LastRunAt = res.At
	st.LastRepairs = res.Repaired()
	st.Repairs += int64(res.Repaired())
	if err != nil {
		st.Failures++
		st.LastError = err.Error()
		return
	}
	st.LastError = ""
	st.LastSuccessAt = res.At
}

// ReconcileStatsSnapshot returns the per-repo reconcile counters, sorted by repo
// for a stable diagnostics render.
func ReconcileStatsSnapshot() []ReconcileStat {
	reconcileMu.Lock()
	defer reconcileMu.Unlock()
	out := make([]ReconcileStat, 0, len(reconcileStats))
	for _, st := range reconcileStats {
		out = append(out, *st)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Repo < out[j].Repo })
	return out
}

// ReconcileTotals is the fleet-wide reconcile rollup for the journal digest.
type ReconcileTotals struct {
	Repos         int       `json:"repos"`
	Runs          int64     `json:"runs"`
	Failures      int64     `json:"failures"`
	Repairs       int64     `json:"repairs"`
	LastSuccessAt time.Time `json:"last_success_at"`
}

// ReconcileTotalsSnapshot aggregates the per-repo reconcile counters into a
// fleet-wide rollup, taking the most recent successful reconcile across repos.
func ReconcileTotalsSnapshot() ReconcileTotals {
	reconcileMu.Lock()
	defer reconcileMu.Unlock()
	var t ReconcileTotals
	t.Repos = len(reconcileStats)
	for _, st := range reconcileStats {
		t.Runs += st.Runs
		t.Failures += st.Failures
		t.Repairs += st.Repairs
		if st.LastSuccessAt.After(t.LastSuccessAt) {
			t.LastSuccessAt = st.LastSuccessAt
		}
	}
	return t
}

// resetReconcileStatsForTest clears the registry so a test starts from a known
// base.
func resetReconcileStatsForTest() {
	reconcileMu.Lock()
	defer reconcileMu.Unlock()
	reconcileStats = map[string]*ReconcileStat{}
}
