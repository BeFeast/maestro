package server

import (
	"math"
	"testing"
	"time"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/state"
)

// TestBuildFleetCostObservability_BucketsByWindow exercises the three
// rollup windows (today / 7d / lifetime) and confirms tokens get
// bucketed against `now` correctly when sessions span the boundaries.
func TestBuildFleetCostObservability_BucketsByWindow(t *testing.T) {
	now := time.Date(2026, 6, 2, 12, 0, 0, 0, time.UTC)
	dayStart := time.Date(2026, 6, 2, 0, 0, 0, 0, time.UTC)
	cfg := &config.Config{
		Model: config.ModelConfig{
			Backends: map[string]config.BackendDef{
				"claude": {
					Pricing: config.BackendPricing{InputUSDPerMtok: 10, OutputUSDPerMtok: 30},
				},
				"codex": {},
			},
		},
	}
	st := &state.State{Sessions: map[string]*state.Session{
		// Today: 100k tokens on claude — priced
		"a-1": {
			IssueNumber:     1,
			IssueTitle:      "today claude",
			Backend:         "claude",
			Status:          state.StatusDone,
			StartedAt:       dayStart.Add(time.Hour),
			FinishedAt:      timePtr(dayStart.Add(2 * time.Hour)),
			TokensUsedTotal: 100_000,
		},
		// 7d but not today: 200k tokens on codex — unpriced
		"a-2": {
			IssueNumber:     2,
			IssueTitle:      "week codex",
			Backend:         "codex",
			Status:          state.StatusDone,
			StartedAt:       now.Add(-3 * 24 * time.Hour),
			FinishedAt:      timePtr(now.Add(-3 * 24 * time.Hour)),
			TokensUsedTotal: 200_000,
		},
		// Older than 7d: lifetime only, 50k tokens on claude — priced
		"a-3": {
			IssueNumber:     3,
			IssueTitle:      "old claude",
			Backend:         "claude",
			Status:          state.StatusDone,
			StartedAt:       now.Add(-30 * 24 * time.Hour),
			FinishedAt:      timePtr(now.Add(-30 * 24 * time.Hour)),
			TokensUsedTotal: 50_000,
		},
	}}

	got := buildFleetCostObservability(cfg, st, now)

	if got.WindowToday.Tokens != 100_000 {
		t.Errorf("today tokens = %d, want 100000", got.WindowToday.Tokens)
	}
	if got.WindowToday.Sessions != 1 {
		t.Errorf("today sessions = %d, want 1", got.WindowToday.Sessions)
	}
	if got.WindowToday.PricedTokens != 100_000 {
		t.Errorf("today priced tokens = %d, want 100000", got.WindowToday.PricedTokens)
	}
	if got.WindowToday.UnpricedTokens != 0 {
		t.Errorf("today unpriced tokens = %d, want 0", got.WindowToday.UnpricedTokens)
	}

	// 7d bucket includes today's claude session + the codex one (3d old).
	if got.Window7D.Tokens != 300_000 {
		t.Errorf("7d tokens = %d, want 300000", got.Window7D.Tokens)
	}
	if got.Window7D.PricedTokens != 100_000 {
		t.Errorf("7d priced tokens = %d, want 100000", got.Window7D.PricedTokens)
	}
	if got.Window7D.UnpricedTokens != 200_000 {
		t.Errorf("7d unpriced tokens = %d, want 200000", got.Window7D.UnpricedTokens)
	}

	// Lifetime sums every session.
	if got.Lifetime.Tokens != 350_000 {
		t.Errorf("lifetime tokens = %d, want 350000", got.Lifetime.Tokens)
	}
	if got.Lifetime.Sessions != 3 {
		t.Errorf("lifetime sessions = %d, want 3", got.Lifetime.Sessions)
	}

	// USD: today 100k * ((10+30)/2)/1M = $2.00
	if math.Abs(got.WindowToday.USD-2.0) > 1e-6 {
		t.Errorf("today USD = %f, want ~2.00", got.WindowToday.USD)
	}
	// Lifetime claude: 100k + 50k = 150k * 20/1M = $3.00. Codex unpriced contributes 0.
	if math.Abs(got.Lifetime.USD-3.0) > 1e-6 {
		t.Errorf("lifetime USD = %f, want ~3.00", got.Lifetime.USD)
	}
}

// TestBuildFleetCostObservability_PerBackend confirms the per-backend
// rows include every configured backend (zero-row stability) and that
// price_configured is faithful.
func TestBuildFleetCostObservability_PerBackend(t *testing.T) {
	now := time.Date(2026, 6, 2, 12, 0, 0, 0, time.UTC)
	cfg := &config.Config{
		Model: config.ModelConfig{
			Backends: map[string]config.BackendDef{
				"claude":   {Pricing: config.BackendPricing{InputUSDPerMtok: 10, OutputUSDPerMtok: 30}},
				"codex":    {},
				"opencode": {},
			},
		},
	}
	st := &state.State{Sessions: map[string]*state.Session{
		"a-1": {
			IssueNumber:     7,
			Backend:         "claude",
			Status:          state.StatusDone,
			StartedAt:       now.Add(-2 * time.Hour),
			FinishedAt:      timePtr(now.Add(-time.Hour)),
			TokensUsedTotal: 1_000_000,
		},
		"a-2": {
			IssueNumber:     8,
			Backend:         "codex",
			Status:          state.StatusRunning,
			StartedAt:       now.Add(-15 * time.Minute),
			TokensUsedTotal: 200_000,
		},
	}}
	got := buildFleetCostObservability(cfg, st, now)

	rowFor := func(name string) (fleetCostBackend, bool) {
		for _, r := range got.PerBackend {
			if r.Backend == name {
				return r, true
			}
		}
		return fleetCostBackend{}, false
	}

	claude, ok := rowFor("claude")
	if !ok {
		t.Fatalf("claude row missing")
	}
	if !claude.PriceConfigured {
		t.Errorf("claude price_configured = false; pricing was set in config")
	}
	if claude.Today.Tokens != 1_000_000 {
		t.Errorf("claude today tokens = %d, want 1000000", claude.Today.Tokens)
	}
	if math.Abs(claude.Today.USD-20.0) > 1e-6 {
		t.Errorf("claude today USD = %f, want ~20.00 (1M tokens * 20$/Mtok blend)", claude.Today.USD)
	}

	codex, ok := rowFor("codex")
	if !ok {
		t.Fatalf("codex row missing")
	}
	if codex.PriceConfigured {
		t.Errorf("codex price_configured = true; codex has no rates set")
	}
	if codex.Today.Tokens != 200_000 {
		t.Errorf("codex today tokens = %d, want 200000", codex.Today.Tokens)
	}
	if codex.Today.USD != 0 {
		t.Errorf("codex today USD = %f, want 0 (no rate configured)", codex.Today.USD)
	}

	if _, ok := rowFor("opencode"); !ok {
		t.Errorf("opencode row missing even though no tokens — every configured backend must appear")
	}
}

// TestBuildFleetCostObservability_PerIssue confirms retries are
// collapsed into a single per-issue row with the union of backends.
func TestBuildFleetCostObservability_PerIssue(t *testing.T) {
	now := time.Date(2026, 6, 2, 12, 0, 0, 0, time.UTC)
	cfg := &config.Config{
		Model: config.ModelConfig{
			Backends: map[string]config.BackendDef{
				"claude": {Pricing: config.BackendPricing{InputUSDPerMtok: 10, OutputUSDPerMtok: 30}},
				"codex":  {Pricing: config.BackendPricing{InputUSDPerMtok: 5, OutputUSDPerMtok: 15}},
			},
		},
	}
	st := &state.State{Sessions: map[string]*state.Session{
		"a-1": {
			IssueNumber:     42,
			IssueTitle:      "flaky integration",
			Backend:         "claude",
			Status:          state.StatusFailed,
			StartedAt:       now.Add(-3 * time.Hour),
			FinishedAt:      timePtr(now.Add(-2 * time.Hour)),
			TokensUsedTotal: 100_000,
		},
		"a-2": {
			IssueNumber:     42,
			IssueTitle:      "flaky integration",
			Backend:         "codex",
			Status:          state.StatusDone,
			StartedAt:       now.Add(-time.Hour),
			FinishedAt:      timePtr(now.Add(-30 * time.Minute)),
			TokensUsedTotal: 50_000,
		},
		"a-3": {
			IssueNumber:     99,
			IssueTitle:      "single",
			Backend:         "claude",
			Status:          state.StatusDone,
			StartedAt:       now.Add(-time.Hour),
			FinishedAt:      timePtr(now.Add(-30 * time.Minute)),
			TokensUsedTotal: 25_000,
		},
	}}
	got := buildFleetCostObservability(cfg, st, now)

	var row42 *fleetCostIssue
	for i := range got.PerIssue {
		if got.PerIssue[i].IssueNumber == 42 {
			row42 = &got.PerIssue[i]
		}
	}
	if row42 == nil {
		t.Fatalf("issue 42 row missing; got %+v", got.PerIssue)
	}
	if row42.Tokens != 150_000 {
		t.Errorf("issue 42 tokens = %d, want 150000 (both attempts summed)", row42.Tokens)
	}
	if row42.Sessions != 2 {
		t.Errorf("issue 42 sessions = %d, want 2", row42.Sessions)
	}
	// claude blend 20/Mtok * 100k = $2.00; codex blend 10/Mtok * 50k = $0.50; total $2.50.
	if math.Abs(row42.USD-2.5) > 1e-6 {
		t.Errorf("issue 42 USD = %f, want ~2.50", row42.USD)
	}
	if len(row42.Backends) != 2 {
		t.Errorf("issue 42 backends = %v, want both claude and codex", row42.Backends)
	}
}

// TestBuildFleetCostObservability_UnconfiguredBackend ensures backends
// without pricing degrade to tokens-only without crashing the rollup.
// Mirrors the acceptance criterion "Backends with no configured price
// degrade gracefully to token-only" from issue #619.
func TestBuildFleetCostObservability_UnconfiguredBackend(t *testing.T) {
	now := time.Date(2026, 6, 2, 12, 0, 0, 0, time.UTC)
	cfg := &config.Config{
		Model: config.ModelConfig{
			Backends: map[string]config.BackendDef{
				"localllm": {},
			},
		},
	}
	st := &state.State{Sessions: map[string]*state.Session{
		"a-1": {
			IssueNumber:     1,
			Backend:         "localllm",
			Status:          state.StatusDone,
			StartedAt:       now.Add(-time.Hour),
			FinishedAt:      timePtr(now.Add(-30 * time.Minute)),
			TokensUsedTotal: 500_000,
		},
	}}
	got := buildFleetCostObservability(cfg, st, now)
	if got.WindowToday.Tokens != 500_000 {
		t.Errorf("today tokens = %d, want 500000", got.WindowToday.Tokens)
	}
	if got.WindowToday.USD != 0 {
		t.Errorf("today USD = %f, want 0 (no pricing configured)", got.WindowToday.USD)
	}
	if got.WindowToday.UnpricedTokens != 500_000 {
		t.Errorf("today unpriced tokens = %d, want 500000", got.WindowToday.UnpricedTokens)
	}
}

// TestRollupGlobalCost confirms that fleet-level aggregation sums
// per-project windows and merges per-backend rows by name.
func TestRollupGlobalCost(t *testing.T) {
	projA := fleetProjectState{
		Name: "alpha",
		Repo: "owner/alpha",
		CostObservability: fleetCostObservability{
			WindowToday: fleetCostWindow{Tokens: 1000, PricedTokens: 1000, USD: 0.5, Sessions: 1},
			Window7D:    fleetCostWindow{Tokens: 5000, PricedTokens: 5000, USD: 2.5, Sessions: 3},
			Lifetime:    fleetCostWindow{Tokens: 12_000, PricedTokens: 12_000, USD: 6.0, Sessions: 7},
			PerBackend: []fleetCostBackend{
				{
					Backend:          "claude",
					PriceConfigured:  true,
					InputUSDPerMtok:  10,
					OutputUSDPerMtok: 30,
					Today:            fleetCostWindow{Tokens: 1000, PricedTokens: 1000, USD: 0.5, Sessions: 1},
					Week:             fleetCostWindow{Tokens: 5000, PricedTokens: 5000, USD: 2.5, Sessions: 3},
					Lifetime:         fleetCostWindow{Tokens: 12_000, PricedTokens: 12_000, USD: 6.0, Sessions: 7},
				},
			},
		},
	}
	projB := fleetProjectState{
		Name: "beta",
		Repo: "owner/beta",
		CostObservability: fleetCostObservability{
			WindowToday: fleetCostWindow{Tokens: 2000, UnpricedTokens: 2000, Sessions: 2},
			Window7D:    fleetCostWindow{Tokens: 9000, UnpricedTokens: 9000, Sessions: 4},
			Lifetime:    fleetCostWindow{Tokens: 30_000, UnpricedTokens: 30_000, Sessions: 9},
			PerBackend: []fleetCostBackend{
				{
					Backend:  "claude",
					Today:    fleetCostWindow{Tokens: 2000, UnpricedTokens: 2000, Sessions: 2},
					Week:     fleetCostWindow{Tokens: 9000, UnpricedTokens: 9000, Sessions: 4},
					Lifetime: fleetCostWindow{Tokens: 30_000, UnpricedTokens: 30_000, Sessions: 9},
				},
			},
		},
	}

	got := rollupGlobalCost([]fleetProjectState{projA, projB})

	if got.WindowToday.Tokens != 3000 {
		t.Errorf("today tokens = %d, want 3000", got.WindowToday.Tokens)
	}
	if got.Window7D.Tokens != 14_000 {
		t.Errorf("7d tokens = %d, want 14000", got.Window7D.Tokens)
	}
	if got.Lifetime.Tokens != 42_000 {
		t.Errorf("lifetime tokens = %d, want 42000", got.Lifetime.Tokens)
	}
	if len(got.PerProject) != 2 {
		t.Fatalf("per_project rows = %d, want 2", len(got.PerProject))
	}
	if got.PerProject[0].Project != "beta" {
		// beta has more 7d tokens than alpha, so it should sort first.
		t.Errorf("per_project[0] = %q, want beta (higher 7d tokens)", got.PerProject[0].Project)
	}

	if len(got.PerBackend) != 1 {
		t.Fatalf("per_backend rows = %d, want 1 (claude merged)", len(got.PerBackend))
	}
	merged := got.PerBackend[0]
	if merged.Backend != "claude" {
		t.Fatalf("merged backend = %q, want claude", merged.Backend)
	}
	if merged.Today.Tokens != 3000 {
		t.Errorf("merged claude today tokens = %d, want 3000", merged.Today.Tokens)
	}
	if !merged.PriceConfigured {
		t.Errorf("merged claude price_configured = false; alpha had pricing set — OR merge should win")
	}
	if merged.InputUSDPerMtok != 10 || merged.OutputUSDPerMtok != 30 {
		t.Errorf("merged claude rates = %f / %f, want 10 / 30", merged.InputUSDPerMtok, merged.OutputUSDPerMtok)
	}
}

// TestApplySessionCostEstimate covers the per-session cost helper used
// to populate sessionInfo.CostUSDEstimate and the fleet drawer field.
func TestApplySessionCostEstimate(t *testing.T) {
	pricing := map[string]config.BackendPricing{
		"claude": {InputUSDPerMtok: 10, OutputUSDPerMtok: 30},
		"codex":  {},
	}
	if got := applySessionCostEstimate("claude", 1_000_000, pricing); math.Abs(got-20.0) > 1e-6 {
		t.Errorf("claude 1M tokens = %f, want ~20.00", got)
	}
	if got := applySessionCostEstimate("codex", 500_000, pricing); got != 0 {
		t.Errorf("codex unpriced 500k tokens = %f, want 0", got)
	}
	if got := applySessionCostEstimate("unknown", 100_000, pricing); got != 0 {
		t.Errorf("unknown backend = %f, want 0", got)
	}
	if got := applySessionCostEstimate("claude", 0, pricing); got != 0 {
		t.Errorf("zero tokens = %f, want 0", got)
	}
}

// TestSessionCostEstimate_CodexVirtual covers the #738 virtual-cost contract:
// codex never self-reports USD (backendCost is always 0), so the dollar figure
// must come from the configured pricing block. With pricing the estimate is
// non-zero; without it the session degrades to tokens-only ($0). A claude
// session that DID self-report a cost still prefers that over the estimate.
// Ported to the #739 split-token signature: codex carries no split tokens, so
// the call falls through to the blended estimate (same result as pre-#739).
func TestSessionCostEstimate_CodexVirtual(t *testing.T) {
	pricing := map[string]config.BackendPricing{
		"codex":  {InputUSDPerMtok: 1.25, OutputUSDPerMtok: 10},
		"claude": {InputUSDPerMtok: 10, OutputUSDPerMtok: 30},
	}
	// codex: backendCost is always 0 and no split tokens, so the
	// (1.25+10)/2 = 5.625 $/Mtok blend applies → 1M tokens ≈ $5.625.
	if got := sessionCostEstimate("codex", 1_000_000, 0, 0, 0, 0, pricing, 0); math.Abs(got-5.625) > 1e-6 {
		t.Errorf("codex virtual cost = %f, want ~5.625", got)
	}
	// codex with no pricing → tokens-only ($0), never a self-reported cost.
	if got := sessionCostEstimate("codex", 1_000_000, 0, 0, 0, 0, map[string]config.BackendPricing{"codex": {}}, 0); got != 0 {
		t.Errorf("codex unpriced cost = %f, want 0", got)
	}
	// A self-reported cost (e.g. claude total_cost_usd) still wins over pricing.
	if got := sessionCostEstimate("claude", 1_000_000, 0, 0, 0, 0, pricing, 0.04); math.Abs(got-0.04) > 1e-9 {
		t.Errorf("self-reported cost = %f, want 0.04 (prefer backend cost)", got)
	}
}

// TestBuildFleetCostObservability_SplitCosting proves the rollup prices a
// session carrying cache-aware split tokens (#739) with EstimateCostSplit —
// the cache-read discount makes the panel's USD a small fraction of the naive
// blended figure on a cache-heavy run.
func TestBuildFleetCostObservability_SplitCosting(t *testing.T) {
	now := time.Date(2026, 6, 2, 12, 0, 0, 0, time.UTC)
	dayStart := time.Date(2026, 6, 2, 0, 0, 0, 0, time.UTC)
	cfg := &config.Config{
		Model: config.ModelConfig{
			Backends: map[string]config.BackendDef{
				"claude": {Pricing: config.BackendPricing{InputUSDPerMtok: 15, OutputUSDPerMtok: 75}},
			},
		},
	}
	st := &state.State{Sessions: map[string]*state.Session{
		// Cache-heavy split tokens, no self-reported cost → split estimate.
		"split-1": {
			IssueNumber:      1,
			IssueTitle:       "cache-heavy run",
			Backend:          "claude",
			Status:           state.StatusDone,
			StartedAt:        dayStart.Add(time.Hour),
			FinishedAt:       timePtr(dayStart.Add(2 * time.Hour)),
			TokensUsedTotal:  1_065_000,
			TokensInput:      10_000,
			TokensOutput:     5_000,
			TokensCacheRead:  1_000_000,
			TokensCacheWrite: 50_000,
		},
	}}

	got := buildFleetCostObservability(cfg, st, now)

	// Cache-aware split: $2.9625, NOT the blended $47.925.
	if math.Abs(got.WindowToday.USD-2.9625) > 1e-6 {
		t.Errorf("today USD = %f, want ~2.9625 (cache-aware split, not blended $47.925)", got.WindowToday.USD)
	}
	if math.Abs(got.Lifetime.USD-2.9625) > 1e-6 {
		t.Errorf("lifetime USD = %f, want ~2.9625", got.Lifetime.USD)
	}
	// Tokens still report the full combined total for the panel.
	if got.WindowToday.Tokens != 1_065_000 {
		t.Errorf("today tokens = %d, want 1065000 (combined total preserved)", got.WindowToday.Tokens)
	}
	// The per-issue rollup carries the same split-priced figure.
	if len(got.PerIssue) != 1 {
		t.Fatalf("per_issue rows = %d, want 1", len(got.PerIssue))
	}
	if math.Abs(got.PerIssue[0].USD-2.9625) > 1e-6 {
		t.Errorf("issue USD = %f, want ~2.9625", got.PerIssue[0].USD)
	}
}

// TestBuildFleetCostObservability_SelfReportedCostWins proves the backend's
// self-reported cost (#730) takes precedence over both the split and blended
// estimates in the rollup (acceptance: "Self-reported cost still takes
// precedence over the estimate").
func TestBuildFleetCostObservability_SelfReportedCostWins(t *testing.T) {
	now := time.Date(2026, 6, 2, 12, 0, 0, 0, time.UTC)
	dayStart := time.Date(2026, 6, 2, 0, 0, 0, 0, time.UTC)
	cfg := &config.Config{
		Model: config.ModelConfig{
			Backends: map[string]config.BackendDef{
				"claude": {Pricing: config.BackendPricing{InputUSDPerMtok: 15, OutputUSDPerMtok: 75}},
			},
		},
	}
	st := &state.State{Sessions: map[string]*state.Session{
		"reported-1": {
			IssueNumber:      1,
			Backend:          "claude",
			Status:           state.StatusDone,
			StartedAt:        dayStart.Add(time.Hour),
			FinishedAt:       timePtr(dayStart.Add(2 * time.Hour)),
			TokensUsedTotal:  1_065_000,
			TokensInput:      10_000,
			TokensOutput:     5_000,
			TokensCacheRead:  1_000_000,
			TokensCacheWrite: 50_000,
			CostUSDBackend:   1.23, // claude total_cost_usd
		},
	}}

	got := buildFleetCostObservability(cfg, st, now)
	if math.Abs(got.WindowToday.USD-1.23) > 1e-6 {
		t.Errorf("today USD = %f, want 1.23 (self-reported wins over split/blended)", got.WindowToday.USD)
	}
}

// TestSessionCostEstimate_Precedence covers the per-session cost helper's
// self-reported > split > blended precedence (#739).
func TestSessionCostEstimate_Precedence(t *testing.T) {
	pricing := map[string]config.BackendPricing{
		"claude": {InputUSDPerMtok: 15, OutputUSDPerMtok: 75},
		"codex":  {},
	}
	const (
		total      = 1_065_000
		input      = 10_000
		output     = 5_000
		cacheRead  = 1_000_000
		cacheWrite = 50_000
	)

	// Self-reported cost wins over everything else.
	if got := sessionCostEstimate("claude", total, input, output, cacheRead, cacheWrite, pricing, 0.99); got != 0.99 {
		t.Errorf("with self-reported cost = %f, want 0.99", got)
	}
	// Split tokens present, no self-reported cost → cache-aware split.
	if got := sessionCostEstimate("claude", total, input, output, cacheRead, cacheWrite, pricing, 0); math.Abs(got-2.9625) > 1e-6 {
		t.Errorf("split estimate = %f, want ~2.9625", got)
	}
	// No split tokens → legacy blended estimate over the combined total.
	if got := sessionCostEstimate("claude", 1_000_000, 0, 0, 0, 0, pricing, 0); math.Abs(got-45.0) > 1e-6 {
		t.Errorf("blended estimate = %f, want 45.00", got)
	}
	// Unpriced backend with split tokens → 0.
	if got := sessionCostEstimate("codex", total, input, output, cacheRead, cacheWrite, pricing, 0); got != 0 {
		t.Errorf("unpriced split = %f, want 0", got)
	}
	// Unknown backend with split tokens → 0.
	if got := sessionCostEstimate("unknown", total, input, output, cacheRead, cacheWrite, pricing, 0); got != 0 {
		t.Errorf("unknown backend split = %f, want 0", got)
	}
}

func timePtr(t time.Time) *time.Time { return &t }
