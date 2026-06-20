package config

import (
	"math"
	"testing"
)

// TestBackendPricing_Configured covers the priced / unpriced split the
// fleet rollup uses to decide whether a backend row shows $ or
// tokens-only. Issue #619 acceptance: "Backends with no configured
// price degrade gracefully to token-only".
func TestBackendPricing_Configured(t *testing.T) {
	if (BackendPricing{}).Configured() {
		t.Errorf("empty pricing should not be configured")
	}
	if !(BackendPricing{InputUSDPerMtok: 1}).Configured() {
		t.Errorf("input-only pricing should be configured")
	}
	if !(BackendPricing{OutputUSDPerMtok: 1}).Configured() {
		t.Errorf("output-only pricing should be configured")
	}
}

// TestBackendPricing_EstimateCostUSD covers the 50/50 input/output
// blend used because the orchestrator only stamps a single combined
// token counter.
func TestBackendPricing_EstimateCostUSD(t *testing.T) {
	cases := []struct {
		name    string
		price   BackendPricing
		tokens  int
		wantUSD float64
	}{
		{"unpriced returns 0", BackendPricing{}, 1_000_000, 0},
		{"zero tokens returns 0", BackendPricing{InputUSDPerMtok: 10, OutputUSDPerMtok: 30}, 0, 0},
		{"negative tokens returns 0", BackendPricing{InputUSDPerMtok: 10, OutputUSDPerMtok: 30}, -100, 0},
		{"input-only 1M tokens", BackendPricing{InputUSDPerMtok: 10}, 1_000_000, 5},                        // (10+0)/2 = 5/Mtok
		{"output-only 1M tokens", BackendPricing{OutputUSDPerMtok: 30}, 1_000_000, 15},                     // (0+30)/2 = 15/Mtok
		{"both rates 500k tokens", BackendPricing{InputUSDPerMtok: 10, OutputUSDPerMtok: 30}, 500_000, 10}, // 500k * 20/Mtok
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.price.EstimateCostUSD(tc.tokens)
			if math.Abs(got-tc.wantUSD) > 1e-6 {
				t.Errorf("EstimateCostUSD(%d) = %f, want %f", tc.tokens, got, tc.wantUSD)
			}
		})
	}
}

// TestBackendPricing_CacheRates covers the effective cache-read / cache-write
// rates: the configured value when set, otherwise the Anthropic-default
// multiple of the input rate (#739).
func TestBackendPricing_CacheRates(t *testing.T) {
	// Defaults derive from the input rate: read = 0.1×, write = 1.25×.
	base := BackendPricing{InputUSDPerMtok: 15, OutputUSDPerMtok: 75}
	if got := base.CacheReadRate(); math.Abs(got-1.5) > 1e-9 {
		t.Errorf("default cache-read rate = %f, want 1.5 (0.1 × 15)", got)
	}
	if got := base.CacheWriteRate(); math.Abs(got-18.75) > 1e-9 {
		t.Errorf("default cache-write rate = %f, want 18.75 (1.25 × 15)", got)
	}

	// Explicit overrides win over the input-derived default.
	override := BackendPricing{InputUSDPerMtok: 15, CacheReadUSDPerMtok: 2, CacheWriteUSDPerMtok: 20}
	if got := override.CacheReadRate(); got != 2 {
		t.Errorf("override cache-read rate = %f, want 2", got)
	}
	if got := override.CacheWriteRate(); got != 20 {
		t.Errorf("override cache-write rate = %f, want 20", got)
	}
}

// TestBackendPricing_EstimateCostSplit covers the cache-aware per-dimension
// cost (#739) and proves the cache-read discount makes a cache-heavy run's
// split cost a small fraction of the naive blended figure (acceptance:
// "blended vs split diverge as expected on a cache-heavy run").
func TestBackendPricing_EstimateCostSplit(t *testing.T) {
	price := BackendPricing{InputUSDPerMtok: 15, OutputUSDPerMtok: 75}

	// Unpriced backend → 0 regardless of tokens.
	if got := (BackendPricing{}).EstimateCostSplit(1000, 1000, 1000, 1000); got != 0 {
		t.Errorf("unpriced split = %f, want 0", got)
	}

	// Cache-heavy agentic turn: the full context (1M tokens) is re-sent as
	// cache_read, with a little fresh input/output/cache_write.
	input, output, cacheRead, cacheWrite := 10_000, 5_000, 1_000_000, 50_000
	// input 10k*15 + output 5k*75 + cacheRead 1M*1.5 + cacheWrite 50k*18.75
	//   = (150_000 + 375_000 + 1_500_000 + 937_500) / 1e6 = 2.9625
	wantSplit := 2.9625
	gotSplit := price.EstimateCostSplit(input, output, cacheRead, cacheWrite)
	if math.Abs(gotSplit-wantSplit) > 1e-6 {
		t.Errorf("split cost = %f, want %f", gotSplit, wantSplit)
	}

	// The legacy blend prices the same tokens (1.065M total) at the 45/Mtok
	// 50/50 blend → $47.925, ~16× the cache-aware figure.
	total := input + output + cacheRead + cacheWrite
	gotBlend := price.EstimateCostUSD(total)
	if math.Abs(gotBlend-47.925) > 1e-6 {
		t.Errorf("blended cost = %f, want 47.925", gotBlend)
	}
	if gotSplit >= gotBlend {
		t.Errorf("split %f should be far below blended %f on a cache-heavy run", gotSplit, gotBlend)
	}
	if gotBlend/gotSplit < 5 {
		t.Errorf("blended/split = %f, want a large divergence (>5×)", gotBlend/gotSplit)
	}

	// Negative counters are clamped to zero rather than subtracting cost.
	if got := price.EstimateCostSplit(-100, -100, -100, -100); got != 0 {
		t.Errorf("all-negative split = %f, want 0", got)
	}
}

// TestParse_BackendPricingFromYAML confirms the pricing block parses
// cleanly off the yaml config and is reachable through cfg.Model.Backends.
// It also covers the #739 cache rates: round-tripped when set, defaulted
// from the input rate when omitted.
func TestParse_BackendPricingFromYAML(t *testing.T) {
	yaml := `
repo: owner/repo
model:
  default: claude
  backends:
    claude:
      cmd: claude
      pricing:
        input_usd_per_mtok: 15
        output_usd_per_mtok: 75
        cache_read_usd_per_mtok: 2
        cache_write_usd_per_mtok: 20
    codex:
      cmd: codex
      pricing:
        input_usd_per_mtok: 10
        output_usd_per_mtok: 30
    gemini:
      cmd: gemini
`
	cfg, err := parse([]byte(yaml))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	claude := cfg.Model.Backends["claude"]
	if !claude.Pricing.Configured() {
		t.Fatalf("claude pricing not configured; got %+v", claude.Pricing)
	}
	if claude.Pricing.InputUSDPerMtok != 15 {
		t.Errorf("claude input rate = %f, want 15", claude.Pricing.InputUSDPerMtok)
	}
	if claude.Pricing.OutputUSDPerMtok != 75 {
		t.Errorf("claude output rate = %f, want 75", claude.Pricing.OutputUSDPerMtok)
	}
	// Explicit cache rates round-trip from YAML.
	if claude.Pricing.CacheReadUSDPerMtok != 2 {
		t.Errorf("claude cache-read rate = %f, want 2", claude.Pricing.CacheReadUSDPerMtok)
	}
	if claude.Pricing.CacheWriteUSDPerMtok != 20 {
		t.Errorf("claude cache-write rate = %f, want 20", claude.Pricing.CacheWriteUSDPerMtok)
	}

	// codex omits cache rates → effective rates default off its input rate.
	codex := cfg.Model.Backends["codex"]
	if codex.Pricing.CacheReadUSDPerMtok != 0 {
		t.Errorf("codex cache-read rate should be unset (0), got %f", codex.Pricing.CacheReadUSDPerMtok)
	}
	if got := codex.Pricing.CacheReadRate(); math.Abs(got-1.0) > 1e-9 {
		t.Errorf("codex effective cache-read rate = %f, want 1.0 (0.1 × 10)", got)
	}
	if got := codex.Pricing.CacheWriteRate(); math.Abs(got-12.5) > 1e-9 {
		t.Errorf("codex effective cache-write rate = %f, want 12.5 (1.25 × 10)", got)
	}

	gemini := cfg.Model.Backends["gemini"]
	if gemini.Pricing.Configured() {
		t.Errorf("gemini pricing should not be configured")
	}
}
