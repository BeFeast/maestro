// Package digest aggregates a morning operator report across all configured
// fleet projects: what needs a decision today, which issues look promotable,
// and a per-project fleet-health one-liner (#703).
//
// The collection layer is deliberately decoupled from the CLI so a Mission
// Control panel can reuse Collect/CollectProject later: inputs are plain
// state snapshots plus a narrow read-only GitHub interface, and the output is
// a serializable Report.
package digest

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/github"
	"github.com/befeast/maestro/internal/mirrorstore"
	"github.com/befeast/maestro/internal/state"
)

// GitHubReader is the read-only slice of the GitHub client the digest needs.
// *github.Client satisfies it; tests inject fakes.
type GitHubReader interface {
	ListOpenIssues(labels []string) ([]github.Issue, error)
	ListOpenPRs() ([]github.PR, error)
	IsIssueClosed(number int) (bool, error)
	HasMergedPRForIssue(issueNumber int) (bool, error)
}

// Project is one fleet member's collection input.
type Project struct {
	Name  string
	Repo  string
	State *state.State
	GH    GitHubReader

	ReadyLabel      string
	BlockedLabel    string
	ExcludedLabels  []string // labels that are never promotable (e.g. epic, meta)
	BlockerPatterns []string // regexes for "blocked by #N" references in issue bodies
}

// ProjectFromConfig builds a Project input from a loaded maestro config.
func ProjectFromConfig(name string, cfg *config.Config, st *state.State, gh GitHubReader) Project {
	if strings.TrimSpace(name) == "" && cfg != nil {
		parts := strings.Split(strings.TrimSpace(cfg.Repo), "/")
		name = parts[len(parts)-1]
	}
	p := Project{Name: strings.TrimSpace(name), State: st, GH: gh}
	if cfg != nil {
		p.Repo = cfg.Repo
		p.ReadyLabel = cfg.Supervisor.ReadyLabel
		p.BlockedLabel = cfg.Supervisor.BlockedLabel
		p.ExcludedLabels = cfg.Supervisor.ExcludedLabels
		p.BlockerPatterns = cfg.BlockerPatterns
	}
	return p
}

// Options tunes collection thresholds.
type Options struct {
	Now time.Time
	// StaleReviewAge is the minimum age of unresolved review findings before
	// the PR is surfaced as a decide-today item. Default 24h.
	StaleReviewAge time.Duration
	// HealthWindow is the look-back window for the fleet-health line. Default 24h.
	HealthWindow time.Duration
	// MaxPromotable caps the promotable list per project. Default 10.
	MaxPromotable int
}

func (o Options) withDefaults() Options {
	if o.Now.IsZero() {
		o.Now = time.Now()
	}
	if o.StaleReviewAge <= 0 {
		o.StaleReviewAge = 24 * time.Hour
	}
	if o.HealthWindow <= 0 {
		o.HealthWindow = 24 * time.Hour
	}
	if o.MaxPromotable <= 0 {
		o.MaxPromotable = 10
	}
	return o
}

// ItemKind classifies a digest item for ranking and counting.
type ItemKind string

const (
	KindPendingApproval  ItemKind = "pending_approval"
	KindRetryExhaustedPR ItemKind = "retry_exhausted_pr"
	KindUnblockedIssue   ItemKind = "unblocked_issue"
	KindStaleReviewPR    ItemKind = "stale_review_pr"
	KindPromotable       ItemKind = "promotable"
)

// Item is one ranked entry in a report section.
type Item struct {
	Project string   `json:"project"`
	Kind    ItemKind `json:"kind"`
	Title   string   `json:"title"`
	URL     string   `json:"url,omitempty"`
	Detail  string   `json:"detail,omitempty"`
	Score   float64  `json:"score"`
	// AgeHours is how long the item has been waiting, when known.
	AgeHours float64 `json:"age_hours,omitempty"`
}

// Health is the per-project fleet-health rollup over Options.HealthWindow.
type Health struct {
	Sessions int            `json:"sessions"`
	Merged   int            `json:"merged"`
	Failed   int            `json:"failed"`
	Backends map[string]int `json:"backends,omitempty"`
}

// Line renders the health rollup as a single human line.
func (h Health) Line() string {
	parts := fmt.Sprintf("%d session(s), %d merged, %d failed", h.Sessions, h.Merged, h.Failed)
	if len(h.Backends) == 0 {
		return parts
	}
	names := make([]string, 0, len(h.Backends))
	for name := range h.Backends {
		names = append(names, name)
	}
	sort.Strings(names)
	dist := make([]string, 0, len(names))
	for _, name := range names {
		dist = append(dist, fmt.Sprintf("%s ×%d", name, h.Backends[name]))
	}
	return parts + " · backends: " + strings.Join(dist, ", ")
}

// ProjectReport is one project's digest sections.
type ProjectReport struct {
	Name        string   `json:"name"`
	Repo        string   `json:"repo"`
	DecideToday []Item   `json:"decide_today,omitempty"`
	Promotable  []Item   `json:"promotable,omitempty"`
	Health      Health   `json:"health"`
	Errors      []string `json:"errors,omitempty"`
}

// AuthSummary captures which GitHub auth mode and rate-limit bucket the daemon
// is using, surfaced in the digest so an operator can confirm at a glance
// whether the fleet is on the shared PAT or a GitHub App installation (#823).
type AuthSummary struct {
	Mode           string    `json:"mode"`                      // "app" or "pat"
	Bucket         string    `json:"bucket"`                    // "installation" or "shared-pat"
	InstallationID int64     `json:"installation_id,omitempty"` // App installation, when mode=app
	TokenExpiry    time.Time `json:"token_expiry,omitempty"`    // installation token expiry, when mode=app
	FallbackActive bool      `json:"fallback_active,omitempty"` // App configured but currently on PAT fallback
	FallbackReason string    `json:"fallback_reason,omitempty"` // why the fallback is active
}

// Line renders the auth summary as a single human line for the digest header.
func (a AuthSummary) Line() string {
	if a.FallbackActive {
		reason := a.FallbackReason
		if reason == "" {
			reason = "app token unavailable"
		}
		return fmt.Sprintf("PAT/`gh` (App fallback active — bucket %s; reason: %s)", a.Bucket, reason)
	}
	if a.Mode == string(github.AuthModeApp) {
		exp := "unknown"
		if !a.TokenExpiry.IsZero() {
			exp = a.TokenExpiry.UTC().Format("2006-01-02 15:04 MST")
		}
		return fmt.Sprintf("GitHub App installation %d · bucket `%s` · token expires %s",
			a.InstallationID, a.Bucket, exp)
	}
	return "PAT/`gh` · bucket `shared-pat`"
}

// GitHubUsage summarizes the process-lifetime GitHub read traffic so the digest
// shows the quota picture at a glance (#826 AC 5/10): how many REST exchanges the
// gh wrapper made, how many were billed against the core quota vs served free by
// a 304, and — once mirror-first reads are enabled — how many reads the local
// mirror served vs how many fell back to the API. A high MirrorHits with a low
// APIFallbacks is the phase-D quota win, made visible.
//
// The counters are process-lifetime for whichever process built the report; in
// the long-running daemon they accumulate the fleet's real traffic, and the gh
// wrapper's hourly journal line reports the same numbers per window.
type GitHubUsage struct {
	Requests     int64 `json:"requests"`      // total gh REST exchanges
	Billed       int64 `json:"billed"`        // exchanges billed against the core quota (requests - not_modified)
	NotModified  int64 `json:"not_modified"`  // served free by a 304 conditional response
	RateLimited  int64 `json:"rate_limited"`  // exchanges that hit a rate limit
	MirrorHits   int64 `json:"mirror_hits"`   // reads served from the local mirror (zero API traffic)
	APIFallbacks int64 `json:"api_fallbacks"` // reads that fell back to the GitHub API
}

// Line renders the usage summary as a single human line for the digest header.
func (u GitHubUsage) Line() string {
	base := fmt.Sprintf("%d REST exchange(s) (~%d billed against core quota, %d served free by 304, %d rate-limited)",
		u.Requests, u.Billed, u.NotModified, u.RateLimited)
	if u.MirrorHits == 0 && u.APIFallbacks == 0 {
		return base + " · mirror-first reads: not enabled"
	}
	total := u.MirrorHits + u.APIFallbacks
	pct := 0.0
	if total > 0 {
		pct = 100 * float64(u.MirrorHits) / float64(total)
	}
	return fmt.Sprintf("%s · mirror reads: %d served locally, %d fell back to API (%.0f%% hit)",
		base, u.MirrorHits, u.APIFallbacks, pct)
}

// ReconcileSummary is the fleet-wide rollup of the mirror reconciliation loop
// (#827, phase E): how many repos the loop covers, how many passes ran, how many
// failed, the cumulative drift-repair count, and the most recent successful
// reconcile across repos. Surfacing drift repairs and the last-reconcile time in
// the digest is one of phase E's observability requirements — a nonzero repair
// count is the visible sign a missed webhook was healed without operator action.
type ReconcileSummary struct {
	Repos         int       `json:"repos"`
	Runs          int64     `json:"runs"`
	Failures      int64     `json:"failures"`
	DriftRepairs  int64     `json:"drift_repairs"`
	LastReconcile time.Time `json:"last_reconcile,omitempty"`
}

// Line renders the reconcile rollup as a single human line for the digest header.
func (r ReconcileSummary) Line() string {
	if r.Repos == 0 && r.Runs == 0 {
		return "reconciliation loop not enabled"
	}
	last := "never"
	if !r.LastReconcile.IsZero() {
		last = r.LastReconcile.UTC().Format("2006-01-02 15:04 MST")
	}
	return fmt.Sprintf("%d repo(s), %d pass(es), %d failure(s), %d drift repair(s) · last reconcile %s",
		r.Repos, r.Runs, r.Failures, r.DriftRepairs, last)
}

// Report is the full fleet digest.
type Report struct {
	GeneratedAt time.Time        `json:"generated_at"`
	Auth        AuthSummary      `json:"auth"`
	GitHub      GitHubUsage      `json:"github_usage"`
	Reconcile   ReconcileSummary `json:"reconcile"`
	Projects    []ProjectReport  `json:"projects"`
}

// DecideTodayCount is the number of action items that need an operator
// decision; the notifier fires when it is > 0.
func (r *Report) DecideTodayCount() int {
	n := 0
	for _, p := range r.Projects {
		n += len(p.DecideToday)
	}
	return n
}

// PromotableCount is the fleet-wide number of promotable issue candidates.
func (r *Report) PromotableCount() int {
	n := 0
	for _, p := range r.Projects {
		n += len(p.Promotable)
	}
	return n
}

// Collect builds the fleet digest across all projects.
func Collect(projects []Project, opts Options) *Report {
	opts = opts.withDefaults()
	rep := &Report{GeneratedAt: opts.Now, Auth: authSummary(), GitHub: githubUsage(), Reconcile: reconcileSummary()}
	for _, p := range projects {
		rep.Projects = append(rep.Projects, CollectProject(p, opts))
	}
	return rep
}

// reconcileSummary snapshots the process-wide mirror reconciliation counters
// (#827) into the serializable digest field. In the long-running daemon these
// accumulate the fleet's real reconcile activity; in the one-shot `maestro digest`
// process no loop has run, so the summary reports "not enabled".
func reconcileSummary() ReconcileSummary {
	t := mirrorstore.ReconcileTotalsSnapshot()
	return ReconcileSummary{
		Repos:         t.Repos,
		Runs:          t.Runs,
		Failures:      t.Failures,
		DriftRepairs:  t.Repairs,
		LastReconcile: t.LastSuccessAt,
	}
}

// githubUsage snapshots the process-wide GitHub read counters (#826): the gh
// wrapper's REST exchange tallies and the mirror-first read hit/fallback counts.
func githubUsage() GitHubUsage {
	api := github.APIUsage()
	mirror := mirrorstore.ReadStatsSnapshot()
	return GitHubUsage{
		Requests:     api.Requests,
		Billed:       api.Requests - api.NotModified,
		NotModified:  api.NotModified,
		RateLimited:  api.RateLimited,
		MirrorHits:   mirror.MirrorHits,
		APIFallbacks: mirror.APIFallbacks,
	}
}

// authSummary snapshots the process-wide GitHub auth mode into a serializable
// digest field (#823). In the daemon this reflects the live installation-token
// state; the standalone `maestro digest` command configures App auth from its
// loaded configs before Collect so the report matches the daemon.
func authSummary() AuthSummary {
	info := github.GetAuthInfo()
	s := AuthSummary{
		Mode:           string(info.Mode),
		InstallationID: info.InstallationID,
		TokenExpiry:    info.TokenExpiry,
		FallbackActive: info.FallbackActive,
		FallbackReason: info.LastError,
	}
	if info.Mode == github.AuthModeApp {
		s.Bucket = "installation"
	} else {
		s.Bucket = "shared-pat"
	}
	return s
}

// CollectProject builds one project's digest sections. GitHub failures
// degrade gracefully: the affected check is skipped or over-reports, and the
// error is recorded on the project report.
func CollectProject(p Project, opts Options) ProjectReport {
	opts = opts.withDefaults()
	rep := ProjectReport{Name: p.Name, Repo: p.Repo}
	if p.State == nil {
		p.State = state.NewState()
	}

	var issues []github.Issue
	var openPRs map[int]bool // nil means "unknown" (fetch failed or no reader)
	if p.GH != nil {
		var err error
		issues, err = p.GH.ListOpenIssues(nil)
		if err != nil {
			rep.Errors = append(rep.Errors, fmt.Sprintf("list open issues: %v", err))
			issues = nil
		}
		prs, err := p.GH.ListOpenPRs()
		if err != nil {
			rep.Errors = append(rep.Errors, fmt.Sprintf("list open PRs: %v", err))
		} else {
			openPRs = make(map[int]bool, len(prs))
			for _, pr := range prs {
				openPRs[pr.Number] = true
			}
		}
	}

	res := newResolver(p.GH)
	rep.DecideToday = collectDecideToday(p, issues, openPRs, res, opts)
	rep.Promotable = collectPromotable(p, issues, res, opts)
	rep.Health = collectHealth(p.State, opts)
	rep.Errors = append(rep.Errors, res.errors()...)
	return rep
}

// --- Decide today ---

func collectDecideToday(p Project, issues []github.Issue, openPRs map[int]bool, res *resolver, opts Options) []Item {
	var items []Item
	items = append(items, pendingApprovalItems(p, opts)...)
	items = append(items, retryExhaustedItems(p, openPRs, opts)...)
	items = append(items, unblockedIssueItems(p, issues, res, opts)...)
	items = append(items, staleReviewItems(p, openPRs, opts)...)
	rankItems(items)
	return items
}

func pendingApprovalItems(p Project, opts Options) []Item {
	var items []Item
	for _, a := range p.State.Approvals {
		if a.Status != state.ApprovalStatusPending {
			continue
		}
		age := opts.Now.Sub(a.CreatedAt)
		score := 100.0
		switch strings.ToLower(a.Risk) {
		case "danger":
			score += 15
		case "caution":
			score += 5
		}
		url := ""
		detail := fmt.Sprintf("approval %s · risk: %s", a.ID, a.Risk)
		if a.Target != nil {
			switch {
			case a.Target.PR > 0:
				url = prURL(p.Repo, a.Target.PR)
			case a.Target.Issue > 0:
				url = issueURL(p.Repo, a.Target.Issue)
			}
		}
		items = append(items, Item{
			Project:  p.Name,
			Kind:     KindPendingApproval,
			Title:    "Approve or reject: " + a.Summary,
			URL:      url,
			Detail:   detail,
			Score:    score,
			AgeHours: hours(age),
		})
	}
	return items
}

func retryExhaustedItems(p Project, openPRs map[int]bool, opts Options) []Item {
	var items []Item
	seenPR := make(map[int]bool)
	for _, sess := range p.State.Sessions {
		if sess == nil || sess.Status != state.StatusRetryExhausted || sess.PRNumber <= 0 {
			continue
		}
		// When the open-PR list is unavailable, over-report rather than hide.
		if openPRs != nil && !openPRs[sess.PRNumber] {
			continue
		}
		if seenPR[sess.PRNumber] {
			continue
		}
		seenPR[sess.PRNumber] = true
		var age time.Duration
		if sess.FinishedAt != nil {
			age = opts.Now.Sub(*sess.FinishedAt)
		}
		items = append(items, Item{
			Project:  p.Name,
			Kind:     KindRetryExhaustedPR,
			Title:    fmt.Sprintf("Retry-exhausted session with open PR #%d (issue #%d: %s)", sess.PRNumber, sess.IssueNumber, sess.IssueTitle),
			URL:      prURL(p.Repo, sess.PRNumber),
			Detail:   fmt.Sprintf("retries exhausted after %d attempt(s); decide: merge, repair, or close", sess.RetryCount+1),
			Score:    90,
			AgeHours: hours(age),
		})
	}
	return items
}

func unblockedIssueItems(p Project, issues []github.Issue, res *resolver, opts Options) []Item {
	if strings.TrimSpace(p.BlockedLabel) == "" {
		return nil
	}
	var items []Item
	for _, issue := range issues {
		if !github.HasLabel(issue, []string{p.BlockedLabel}) {
			continue
		}
		deps := mergeRefs(github.FindBlockers(issue.Body, p.BlockerPatterns), github.FindDependencies(issue.Body))
		if len(deps) == 0 {
			// No parseable dependencies: operator-gated, never auto-surfaced
			// (same semantics as the supervisor dependency-unblock controller).
			continue
		}
		resolved, allResolved := res.allResolved(deps)
		if !allResolved {
			continue
		}
		items = append(items, Item{
			Project: p.Name,
			Kind:    KindUnblockedIssue,
			Title:   fmt.Sprintf("Issue #%d is still labeled `%s` but every blocker is resolved: %s", issue.Number, p.BlockedLabel, issue.Title),
			URL:     issueURL(p.Repo, issue.Number),
			Detail:  "resolved dependencies: " + refList(resolved),
			Score:   80,
		})
	}
	return items
}

func staleReviewItems(p Project, openPRs map[int]bool, opts Options) []Item {
	var items []Item
	seenPR := make(map[int]bool)
	for _, track := range p.State.ReviewRepairTracks {
		if !track.Exhausted || track.PRNumber <= 0 || seenPR[track.PRNumber] {
			continue
		}
		since := track.ExhaustedAt
		if since.IsZero() {
			since = track.LastDecisionAt
		}
		age := opts.Now.Sub(since)
		if !since.IsZero() && age < opts.StaleReviewAge {
			continue
		}
		if openPRs != nil && !openPRs[track.PRNumber] {
			continue
		}
		seenPR[track.PRNumber] = true
		detail := fmt.Sprintf("repair budget exhausted after %d attempt(s)", track.Attempts)
		if s := strings.TrimSpace(track.UnresolvedSummary); s != "" {
			detail += " · " + s
		}
		items = append(items, Item{
			Project:  p.Name,
			Kind:     KindStaleReviewPR,
			Title:    fmt.Sprintf("PR #%d has unresolved review findings older than %s", track.PRNumber, formatDuration(opts.StaleReviewAge)),
			URL:      prURL(p.Repo, track.PRNumber),
			Detail:   detail,
			Score:    70 + min(hours(age)/24, 10),
			AgeHours: hours(age),
		})
	}
	return items
}

// --- Promotable ---

func collectPromotable(p Project, issues []github.Issue, res *resolver, opts Options) []Item {
	var items []Item
	for _, issue := range issues {
		if p.ReadyLabel != "" && github.HasLabel(issue, []string{p.ReadyLabel}) {
			continue
		}
		if p.BlockedLabel != "" && github.HasLabel(issue, []string{p.BlockedLabel}) {
			continue
		}
		if len(p.ExcludedLabels) > 0 && github.HasLabel(issue, p.ExcludedLabels) {
			continue
		}
		if p.State.IssueInProgress(issue.Number) || p.State.IssueDone(issue.Number) {
			continue
		}
		deps := mergeRefs(github.FindBlockers(issue.Body, p.BlockerPatterns), github.FindDependencies(issue.Body))
		if len(deps) > 0 {
			if _, allResolved := res.allResolved(deps); !allResolved {
				continue
			}
		}
		score, notes := promotableScore(issue)
		items = append(items, Item{
			Project: p.Name,
			Kind:    KindPromotable,
			Title:   fmt.Sprintf("#%d %s", issue.Number, issue.Title),
			URL:     issueURL(p.Repo, issue.Number),
			Detail:  strings.Join(notes, ", "),
			Score:   score,
		})
	}
	rankItems(items)
	if len(items) > opts.MaxPromotable {
		items = items[:opts.MaxPromotable]
	}
	return items
}

// promotableScore estimates how mergeable/self-contained an issue looks from
// its title and body alone. Higher is better; the scale is 0–100.
func promotableScore(issue github.Issue) (float64, []string) {
	score := 50.0
	var notes []string
	body := strings.TrimSpace(issue.Body)
	lower := strings.ToLower(issue.Title + "\n" + body)

	switch n := len(body); {
	case n == 0:
		score -= 20
		notes = append(notes, "no description")
	case n < 200:
		score -= 5
		notes = append(notes, "thin description")
	case n <= 4000:
		score += 10
		notes = append(notes, "well-scoped description")
	default:
		score -= 5
		notes = append(notes, "very long description")
	}

	if strings.Contains(lower, "acceptance criteria") {
		score += 20
		notes = append(notes, "has acceptance criteria")
	}
	if strings.Contains(lower, "affected surfaces") {
		score += 5
		notes = append(notes, "names affected surfaces")
	}
	for _, kw := range []string{"investigate", "research", "spike", "rfc", "discussion"} {
		if strings.Contains(strings.ToLower(issue.Title), kw) {
			score -= 15
			notes = append(notes, "exploratory ("+kw+")")
			break
		}
	}
	for _, kw := range []string{"epic", "umbrella", "tracking", "meta"} {
		if strings.Contains(strings.ToLower(issue.Title), kw) {
			score -= 25
			notes = append(notes, "looks like an epic/tracking issue")
			break
		}
	}

	if score < 0 {
		score = 0
	}
	if score > 100 {
		score = 100
	}
	return score, notes
}

// --- Health ---

func collectHealth(st *state.State, opts Options) Health {
	h := Health{Backends: make(map[string]int)}
	cutoff := opts.Now.Add(-opts.HealthWindow)
	for _, sess := range st.Sessions {
		if sess == nil {
			continue
		}
		ran := sess.StartedAt.After(cutoff) || (sess.FinishedAt != nil && sess.FinishedAt.After(cutoff))
		if !ran {
			continue
		}
		h.Sessions++
		backend := strings.TrimSpace(sess.Backend)
		if backend == "" {
			backend = "unknown"
		}
		h.Backends[backend]++
		finishedInWindow := sess.FinishedAt != nil && sess.FinishedAt.After(cutoff)
		switch sess.Status {
		case state.StatusDone, state.StatusCodeLanded:
			if finishedInWindow {
				h.Merged++
			}
		case state.StatusFailed, state.StatusConflictFailed, state.StatusDead, state.StatusRetryExhausted:
			if finishedInWindow || sess.FinishedAt == nil {
				h.Failed++
			}
		}
	}
	if len(h.Backends) == 0 {
		h.Backends = nil
	}
	return h
}

// --- ranking and helpers ---

// rankItems sorts by score descending, then by age descending (older waits
// rank higher), then by title for a deterministic order.
func rankItems(items []Item) {
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].Score != items[j].Score {
			return items[i].Score > items[j].Score
		}
		if items[i].AgeHours != items[j].AgeHours {
			return items[i].AgeHours > items[j].AgeHours
		}
		return items[i].Title < items[j].Title
	})
}

// resolver memoizes per-dependency GitHub lookups so a blocker shared by many
// issues is fetched once per project. Lookup errors count as "not resolved".
type resolver struct {
	gh     GitHubReader
	memo   map[int]bool
	errs   []string
	errSet map[string]bool
}

func newResolver(gh GitHubReader) *resolver {
	return &resolver{gh: gh, memo: make(map[int]bool), errSet: make(map[string]bool)}
}

func (r *resolver) resolved(dep int) bool {
	if done, ok := r.memo[dep]; ok {
		return done
	}
	done := false
	if r.gh != nil {
		closed, err := r.gh.IsIssueClosed(dep)
		if err != nil {
			r.recordErr(fmt.Sprintf("check issue #%d closed: %v", dep, err))
		} else if closed {
			done = true
		}
		if !done {
			merged, err := r.gh.HasMergedPRForIssue(dep)
			if err != nil {
				r.recordErr(fmt.Sprintf("check merged PR for #%d: %v", dep, err))
			} else if merged {
				done = true
			}
		}
	}
	r.memo[dep] = done
	return done
}

func (r *resolver) allResolved(deps []int) (resolved []int, all bool) {
	all = true
	for _, dep := range deps {
		if r.resolved(dep) {
			resolved = append(resolved, dep)
		} else {
			all = false
		}
	}
	return resolved, all
}

func (r *resolver) recordErr(msg string) {
	if r.errSet[msg] {
		return
	}
	r.errSet[msg] = true
	r.errs = append(r.errs, msg)
}

func (r *resolver) errors() []string { return r.errs }

func mergeRefs(groups ...[]int) []int {
	seen := make(map[int]bool)
	var refs []int
	for _, group := range groups {
		for _, n := range group {
			if n > 0 && !seen[n] {
				seen[n] = true
				refs = append(refs, n)
			}
		}
	}
	return refs
}

func refList(refs []int) string {
	parts := make([]string, len(refs))
	for i, n := range refs {
		parts[i] = fmt.Sprintf("#%d", n)
	}
	return strings.Join(parts, ", ")
}

func issueURL(repo string, n int) string {
	if repo == "" || n <= 0 {
		return ""
	}
	return fmt.Sprintf("https://github.com/%s/issues/%d", repo, n)
}

func prURL(repo string, n int) string {
	if repo == "" || n <= 0 {
		return ""
	}
	return fmt.Sprintf("https://github.com/%s/pull/%d", repo, n)
}

func hours(d time.Duration) float64 {
	if d <= 0 {
		return 0
	}
	return d.Hours()
}

func formatDuration(d time.Duration) string {
	if d%(24*time.Hour) == 0 {
		return fmt.Sprintf("%dd", int(d/(24*time.Hour)))
	}
	return fmt.Sprintf("%dh", int(d/time.Hour))
}
