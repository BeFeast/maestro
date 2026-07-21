package server

import (
	"sort"
	"time"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/state"
)

// fleetCostObservability is the per-project token + USD spend rollup
// surfaced on the fleet snapshot for issue #619. Operators use it to
// answer "how much did each backend / each issue burn today, this week"
// before turning up max_parallel or adding another project.
//
// The aggregation reads the per-session counters and backend attribution
// already persisted on state.Session. Usage-unreliable sessions remain in the
// rollup even when their token lower bound is zero, so the API never describes
// missing telemetry as "no activity".
type fleetCostObservability struct {
	// WindowToday is the local-day rollup (UTC midnight floor).
	WindowToday fleetCostWindow `json:"window_today"`
	// Window7D is the rolling 7-day rollup (now - 7d .. now).
	Window7D fleetCostWindow `json:"window_7d"`
	// Lifetime sums every session in state.Sessions regardless of age.
	Lifetime fleetCostWindow `json:"lifetime"`
	// PerBackend lists backend roll-ups for today / 7d together, sorted
	// by descending 7-day tokens so the heaviest backend is first. Every
	// backend known to the project config is included even when its
	// tokens are zero so the panel renders a stable row order.
	PerBackend []fleetCostBackend `json:"per_backend"`
	// PerIssue lists per-issue roll-ups (collapsing every session for a
	// given issue_number across the lifetime of the project) sorted by
	// descending tokens. Capped to top 25 so the snapshot stays small.
	PerIssue []fleetCostIssue `json:"per_issue,omitempty"`
}

// fleetCostWindow carries token + USD totals for one time window. USD
// is the estimate computed via BackendPricing.EstimateCostUSD; tokens are
// exact unless UsageUnreliableSessions is non-zero, in which case they are a
// lower bound. PricedTokens / UnpricedTokens split the same tokens by
// whether the backend has pricing configured — this lets the SPA tell
// the operator "today: 1.2M tokens, $14.30 priced + 200K from
// backends without rates".
type fleetCostWindow struct {
	Tokens                  int     `json:"tokens"`
	PricedTokens            int     `json:"priced_tokens"`
	UnpricedTokens          int     `json:"unpriced_tokens"`
	USD                     float64 `json:"usd"`
	Sessions                int     `json:"sessions"`
	UsageUnreliableSessions int     `json:"usage_unreliable_sessions,omitempty"`
}

// fleetCostBackend is one backend row in the per-backend rollup.
// PriceConfigured drives the SPA's "no $ available" presentation. When
// a backend is configured (pricing set) the SPA renders USD; otherwise
// only tokens are shown.
type fleetCostBackend struct {
	Backend          string          `json:"backend"`
	PriceConfigured  bool            `json:"price_configured"`
	InputUSDPerMtok  float64         `json:"input_usd_per_mtok,omitempty"`
	OutputUSDPerMtok float64         `json:"output_usd_per_mtok,omitempty"`
	Today            fleetCostWindow `json:"today"`
	Week             fleetCostWindow `json:"week"`
	Lifetime         fleetCostWindow `json:"lifetime"`
}

// fleetCostIssue is one issue row in the per-issue rollup. Tokens / USD
// roll up every session that ran for the issue across its lifetime so
// retries are visible. The SPA renders this on the worker / issue
// drawer next to the current attempt's counters.
type fleetCostIssue struct {
	IssueNumber             int      `json:"issue_number"`
	IssueTitle              string   `json:"issue_title,omitempty"`
	Tokens                  int      `json:"tokens"`
	USD                     float64  `json:"usd"`
	Sessions                int      `json:"sessions"`
	UsageUnreliableSessions int      `json:"usage_unreliable_sessions,omitempty"`
	Backends                []string `json:"backends,omitempty"`
}

// maxFleetCostIssues bounds the per-issue table size. Picked to keep
// the API response under a few KB extra while still surfacing the
// heaviest spenders. Operators who want the full list can read the
// per-session counters directly.
const maxFleetCostIssues = 25

// buildFleetCostObservability rolls per-session counters into the
// today / 7d / lifetime windows for the panel. Today is bucketed at
// UTC-midnight against `now` so the rollup is stable across calls.
func buildFleetCostObservability(cfg *config.Config, st *state.State, now time.Time) fleetCostObservability {
	if st == nil {
		return fleetCostObservability{}
	}
	dayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	weekStart := now.Add(-7 * 24 * time.Hour)

	pricing := backendPricingMap(cfg)
	priceConfigured := func(backend string) bool {
		p, ok := pricing[backend]
		return ok && p.Configured()
	}

	type backendRow struct {
		today    fleetCostWindow
		week     fleetCostWindow
		lifetime fleetCostWindow
	}
	type issueRow struct {
		issue                   int
		title                   string
		tokens                  int
		usd                     float64
		sessions                int
		usageUnreliableSessions int
		backends                map[string]struct{}
	}

	backends := make(map[string]*backendRow)
	issues := make(map[int]*issueRow)
	var out fleetCostObservability

	for _, sess := range st.Sessions {
		if sess == nil {
			continue
		}
		tokens := sess.TokensUsedTotal
		usd := sessionCostUSD(sess, pricing)
		usageUnreliable := state.SessionUsageAccountingUnreliable(sess)
		if tokens <= 0 && usd <= 0 && !usageUnreliable {
			continue
		}
		stamp := sessionCostTimestamp(sess)
		backend := sess.Backend
		priced := priceConfigured(backend)

		row, ok := backends[backend]
		if !ok {
			row = &backendRow{}
			backends[backend] = row
		}
		applyCostWindow(&row.lifetime, tokens, usd, priced, 1, usageUnreliable)
		applyCostWindow(&out.Lifetime, tokens, usd, priced, 1, usageUnreliable)
		if !stamp.IsZero() {
			if !stamp.Before(dayStart) {
				applyCostWindow(&row.today, tokens, usd, priced, 1, usageUnreliable)
				applyCostWindow(&out.WindowToday, tokens, usd, priced, 1, usageUnreliable)
			}
			if !stamp.Before(weekStart) {
				applyCostWindow(&row.week, tokens, usd, priced, 1, usageUnreliable)
				applyCostWindow(&out.Window7D, tokens, usd, priced, 1, usageUnreliable)
			}
		}

		if sess.IssueNumber > 0 {
			ir, ok := issues[sess.IssueNumber]
			if !ok {
				ir = &issueRow{issue: sess.IssueNumber, backends: make(map[string]struct{})}
				issues[sess.IssueNumber] = ir
			}
			if ir.title == "" {
				ir.title = sess.IssueTitle
			}
			ir.tokens += tokens
			ir.usd += usd
			ir.sessions++
			if usageUnreliable {
				ir.usageUnreliableSessions++
			}
			if backend != "" {
				ir.backends[backend] = struct{}{}
			}
		}
	}

	// Always surface a row for every backend the config knows about so
	// the panel stays stable when the day's traffic skips a backend.
	if cfg != nil {
		for name := range cfg.Model.Backends {
			if _, ok := backends[name]; !ok {
				backends[name] = &backendRow{}
			}
		}
	}

	out.PerBackend = make([]fleetCostBackend, 0, len(backends))
	for name, row := range backends {
		p := pricing[name]
		entry := fleetCostBackend{
			Backend:          name,
			PriceConfigured:  p.Configured(),
			InputUSDPerMtok:  p.InputUSDPerMtok,
			OutputUSDPerMtok: p.OutputUSDPerMtok,
			Today:            row.today,
			Week:             row.week,
			Lifetime:         row.lifetime,
		}
		out.PerBackend = append(out.PerBackend, entry)
	}
	sort.Slice(out.PerBackend, func(i, j int) bool {
		if out.PerBackend[i].Week.Tokens != out.PerBackend[j].Week.Tokens {
			return out.PerBackend[i].Week.Tokens > out.PerBackend[j].Week.Tokens
		}
		if out.PerBackend[i].Lifetime.Tokens != out.PerBackend[j].Lifetime.Tokens {
			return out.PerBackend[i].Lifetime.Tokens > out.PerBackend[j].Lifetime.Tokens
		}
		return out.PerBackend[i].Backend < out.PerBackend[j].Backend
	})

	out.PerIssue = make([]fleetCostIssue, 0, len(issues))
	for _, ir := range issues {
		backendList := make([]string, 0, len(ir.backends))
		for name := range ir.backends {
			backendList = append(backendList, name)
		}
		sort.Strings(backendList)
		out.PerIssue = append(out.PerIssue, fleetCostIssue{
			IssueNumber:             ir.issue,
			IssueTitle:              ir.title,
			Tokens:                  ir.tokens,
			USD:                     ir.usd,
			Sessions:                ir.sessions,
			UsageUnreliableSessions: ir.usageUnreliableSessions,
			Backends:                backendList,
		})
	}
	sort.Slice(out.PerIssue, func(i, j int) bool {
		if out.PerIssue[i].Tokens != out.PerIssue[j].Tokens {
			return out.PerIssue[i].Tokens > out.PerIssue[j].Tokens
		}
		return out.PerIssue[i].IssueNumber < out.PerIssue[j].IssueNumber
	})
	if len(out.PerIssue) > maxFleetCostIssues {
		out.PerIssue = out.PerIssue[:maxFleetCostIssues]
	}
	return out
}

// applyCostWindow accumulates a session into a window bucket.
func applyCostWindow(w *fleetCostWindow, tokens int, usd float64, priced bool, sessions int, usageUnreliable bool) {
	w.Tokens += tokens
	if priced {
		w.PricedTokens += tokens
	} else {
		w.UnpricedTokens += tokens
	}
	w.USD += usd
	w.Sessions += sessions
	if usageUnreliable {
		w.UsageUnreliableSessions += sessions
	}
}

// backendPricingMap returns the per-backend pricing table from a config
// keyed by backend name. The returned map is never nil so callers can
// look up unknown backends without a nil check.
func backendPricingMap(cfg *config.Config) map[string]config.BackendPricing {
	out := make(map[string]config.BackendPricing)
	if cfg == nil {
		return out
	}
	for name, def := range cfg.Model.Backends {
		out[name] = def.Pricing
	}
	return out
}

// sessionCostTimestamp picks the most representative timestamp for
// bucketing a session into a day/week window. Prefers FinishedAt
// (terminal sessions) but falls back to StartedAt for running ones so
// the day's burn includes work in flight.
func sessionCostTimestamp(sess *state.Session) time.Time {
	if sess == nil {
		return time.Time{}
	}
	if sess.FinishedAt != nil && !sess.FinishedAt.IsZero() {
		return sess.FinishedAt.UTC()
	}
	if !sess.StartedAt.IsZero() {
		return sess.StartedAt.UTC()
	}
	return time.Time{}
}

// fleetGlobalCost is the cross-project rollup of cost observability.
// Aggregates per-project per-backend rows by backend name; per-project
// totals stay as a flat list keyed by project. The SPA uses the
// per-backend / per-project rows for the fleet "Cost & usage" hero
// panel and the per-issue list for drill-down.
type fleetGlobalCost struct {
	WindowToday fleetCostWindow    `json:"window_today"`
	Window7D    fleetCostWindow    `json:"window_7d"`
	Lifetime    fleetCostWindow    `json:"lifetime"`
	PerBackend  []fleetCostBackend `json:"per_backend"`
	PerProject  []fleetCostProject `json:"per_project"`
}

// fleetCostProject is one project row in the fleet-level rollup.
type fleetCostProject struct {
	Project  string          `json:"project"`
	Repo     string          `json:"repo,omitempty"`
	Today    fleetCostWindow `json:"today"`
	Week     fleetCostWindow `json:"week"`
	Lifetime fleetCostWindow `json:"lifetime"`
}

// rollupGlobalCost folds per-project cost observability into a
// fleet-level snapshot. Per-backend rows are merged by backend name;
// pricing flags follow the OR rule so a backend marked priced in any
// project shows priced at the fleet level (mirroring the behaviour the
// operator would expect when only one project has rates wired).
func rollupGlobalCost(projects []fleetProjectState) fleetGlobalCost {
	var out fleetGlobalCost
	backendIdx := make(map[string]*fleetCostBackend)
	projectList := make([]fleetCostProject, 0, len(projects))
	for _, item := range projects {
		co := item.CostObservability
		// Sum the project's own totals into the fleet rollup. Each
		// project window is treated as authoritative for that project so
		// fleet-level totals match what the per-project tiles show.
		mergeCostWindow(&out.WindowToday, co.WindowToday)
		mergeCostWindow(&out.Window7D, co.Window7D)
		mergeCostWindow(&out.Lifetime, co.Lifetime)
		projectList = append(projectList, fleetCostProject{
			Project:  item.Name,
			Repo:     item.Repo,
			Today:    co.WindowToday,
			Week:     co.Window7D,
			Lifetime: co.Lifetime,
		})
		for _, row := range co.PerBackend {
			existing, ok := backendIdx[row.Backend]
			if !ok {
				clone := row
				backendIdx[row.Backend] = &clone
				continue
			}
			mergeCostWindow(&existing.Today, row.Today)
			mergeCostWindow(&existing.Week, row.Week)
			mergeCostWindow(&existing.Lifetime, row.Lifetime)
			if row.PriceConfigured {
				existing.PriceConfigured = true
				if existing.InputUSDPerMtok == 0 {
					existing.InputUSDPerMtok = row.InputUSDPerMtok
				}
				if existing.OutputUSDPerMtok == 0 {
					existing.OutputUSDPerMtok = row.OutputUSDPerMtok
				}
			}
		}
	}
	out.PerBackend = make([]fleetCostBackend, 0, len(backendIdx))
	for _, row := range backendIdx {
		out.PerBackend = append(out.PerBackend, *row)
	}
	sort.Slice(out.PerBackend, func(i, j int) bool {
		if out.PerBackend[i].Week.Tokens != out.PerBackend[j].Week.Tokens {
			return out.PerBackend[i].Week.Tokens > out.PerBackend[j].Week.Tokens
		}
		return out.PerBackend[i].Backend < out.PerBackend[j].Backend
	})
	sort.Slice(projectList, func(i, j int) bool {
		if projectList[i].Week.Tokens != projectList[j].Week.Tokens {
			return projectList[i].Week.Tokens > projectList[j].Week.Tokens
		}
		return projectList[i].Project < projectList[j].Project
	})
	out.PerProject = projectList
	return out
}

func mergeCostWindow(dst *fleetCostWindow, src fleetCostWindow) {
	dst.Tokens += src.Tokens
	dst.PricedTokens += src.PricedTokens
	dst.UnpricedTokens += src.UnpricedTokens
	dst.USD += src.USD
	dst.Sessions += src.Sessions
	dst.UsageUnreliableSessions += src.UsageUnreliableSessions
}

// applySessionCostEstimate returns the USD estimate for a single
// session's combined token count under the configured per-backend
// pricing. Returns 0 when the backend is unknown to the config or has
// no pricing rates set — the SPA reads this as "tokens only".
func applySessionCostEstimate(backend string, tokens int, pricing map[string]config.BackendPricing) float64 {
	if tokens <= 0 || pricing == nil {
		return 0
	}
	p, ok := pricing[backend]
	if !ok {
		return 0
	}
	return p.EstimateCostUSD(tokens)
}

// sessionCostEstimate returns the USD cost to surface for a session under
// the cache-aware precedence:
//
//  1. the backend's self-reported cost (Pi --mode json cost.total / claude
//     total_cost_usd, #730) always wins;
//  2. otherwise a cache-aware split estimate when the session carries split
//     tokens (#739) — this prices the cache-read discount so an agentic run's
//     cost reflects reality rather than the over-stated blend;
//  3. otherwise the legacy blended estimate over the combined total (#619).
//
// Zero when the backend is unpriced/unknown and nothing was reported.
func sessionCostEstimate(backend string, tokens, input, output, cacheRead, cacheWrite int, pricing map[string]config.BackendPricing, backendCost float64) float64 {
	if backendCost > 0 {
		return backendCost
	}
	if input > 0 || output > 0 || cacheRead > 0 || cacheWrite > 0 {
		p, ok := pricing[backend]
		if !ok {
			return 0
		}
		return p.EstimateCostSplit(input, output, cacheRead, cacheWrite)
	}
	return applySessionCostEstimate(backend, tokens, pricing)
}

// sessionCostUSD applies sessionCostEstimate to a state.Session, sourcing the
// split tokens / self-reported cost directly off the session record. Used by
// the fleet rollup and the per-worker drawer so the cost panel reflects split
// costing for claude/codex/Pi sessions that carry split tokens (#739).
func sessionCostUSD(sess *state.Session, pricing map[string]config.BackendPricing) float64 {
	if sess == nil {
		return 0
	}
	return sessionCostEstimate(sess.Backend, sess.TokensUsedTotal,
		sess.TokensInput, sess.TokensOutput, sess.TokensCacheRead, sess.TokensCacheWrite,
		pricing, sess.CostUSDBackend)
}

// SessionCostEstimate is the exported form used by cmd/maestro (history
// --json) so the CLI does not need to rebuild the pricing map logic. It
// applies the same self-reported > split > blended precedence over the
// session's persisted counters.
func SessionCostEstimate(cfg *config.Config, sess *state.Session) float64 {
	return sessionCostUSD(sess, backendPricingMap(cfg))
}
