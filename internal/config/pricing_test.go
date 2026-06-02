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

// TestParse_BackendPricingFromYAML confirms the pricing block parses
// cleanly off the yaml config and is reachable through cfg.Model.Backends.
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
    codex:
      cmd: codex
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
	codex := cfg.Model.Backends["codex"]
	if codex.Pricing.Configured() {
		t.Errorf("codex pricing should not be configured")
	}
}
