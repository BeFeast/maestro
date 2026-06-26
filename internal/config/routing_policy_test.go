package config

import (
	"strings"
	"testing"
)

const policyBackendsYAML = `
repo: owner/repo
model:
  default: codex
  backends:
    gemini:
      cmd: gemini
    codex:
      cmd: codex
    claude:
      cmd: claude
`

func TestRoutingPolicy_ParseAndValidate(t *testing.T) {
	yaml := policyBackendsYAML + `
routing:
  mode: policy
  tiers:
    cheap:
      backend: gemini
      rank: 0
    standard:
      backend: codex
      effort: medium
      rank: 1
    strong:
      backend: claude
      effort: high
      rank: 2
  policy:
    default_tier: standard
    rules:
      - when: { labels: ["migration", "security"] }
        tier: strong
      - when: { size: small, dependency: leaf }
        tier: cheap
    escalation:
      enabled: true
      on: [ci_failure, retry]
      max_tier: strong
    budget:
      max_strong_per_wave: 3
`
	cfg, err := parse([]byte(yaml))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !cfg.Routing.IsPolicyMode() {
		t.Fatalf("IsPolicyMode() = false, want true")
	}
	if got := cfg.Routing.Tiers["strong"].Backend; got != "claude" {
		t.Fatalf("strong tier backend = %q, want claude", got)
	}
	if got := cfg.Routing.Tiers["standard"].Effort; got != "medium" {
		t.Fatalf("standard tier effort = %q, want medium", got)
	}
	if cfg.Routing.Policy == nil || cfg.Routing.Policy.DefaultTier != "standard" {
		t.Fatalf("default_tier not parsed: %+v", cfg.Routing.Policy)
	}
	if got := cfg.Routing.OrderedTierNames(); strings.Join(got, ",") != "cheap,standard,strong" {
		t.Fatalf("OrderedTierNames = %v, want [cheap standard strong]", got)
	}
}

func TestRoutingPolicy_InertWhenModeManual(t *testing.T) {
	// Tiers/policy present but mode != policy must parse without error and be
	// inert (IsPolicyMode false) — selection stays today's behavior.
	yaml := policyBackendsYAML + `
routing:
  mode: manual
  tiers:
    strong:
      backend: claude
  policy:
    default_tier: strong
`
	cfg, err := parse([]byte(yaml))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if cfg.Routing.IsPolicyMode() {
		t.Fatalf("IsPolicyMode() = true for mode: manual")
	}
}

func TestRoutingPolicy_ExistingConfigUnaffected(t *testing.T) {
	// A config with no tiers/policy parses and validates identically, and the
	// new helpers degrade to empty.
	cfg, err := parse([]byte(policyBackendsYAML))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if cfg.Routing.IsPolicyMode() {
		t.Fatalf("IsPolicyMode() = true for default config")
	}
	if len(cfg.Routing.OrderedTierNames()) != 0 {
		t.Fatalf("OrderedTierNames non-empty for default config")
	}
}

func TestRoutingPolicy_ValidationErrors(t *testing.T) {
	cases := []struct {
		name     string
		routing  string
		wantSnip string
	}{
		{
			name: "policy mode without tiers",
			routing: `
routing:
  mode: policy
`,
			wantSnip: "requires routing.tiers",
		},
		{
			name: "policy mode without policy block",
			routing: `
routing:
  mode: policy
  tiers:
    strong:
      backend: claude
`,
			wantSnip: "requires routing.policy",
		},
		{
			name: "tier unknown backend",
			routing: `
routing:
  mode: policy
  tiers:
    strong:
      backend: nope
  policy:
    default_tier: strong
`,
			wantSnip: "not declared in model.backends",
		},
		{
			name: "default_tier missing",
			routing: `
routing:
  tiers:
    strong:
      backend: claude
  policy:
    default_tier: ghost
`,
			wantSnip: "default_tier = \"ghost\"",
		},
		{
			name: "rule tier missing",
			routing: `
routing:
  tiers:
    strong:
      backend: claude
  policy:
    default_tier: strong
    rules:
      - when: { labels: ["x"] }
        tier: ghost
`,
			wantSnip: "rules[0].tier = \"ghost\"",
		},
		{
			name: "empty rule predicate",
			routing: `
routing:
  tiers:
    strong:
      backend: claude
  policy:
    default_tier: strong
    rules:
      - when: {}
        tier: strong
`,
			wantSnip: "must set at least one",
		},
		{
			name: "unknown escalation trigger",
			routing: `
routing:
  tiers:
    strong:
      backend: claude
  policy:
    default_tier: strong
    escalation:
      on: [bogus]
`,
			wantSnip: "unknown trigger",
		},
		{
			name: "invalid size",
			routing: `
routing:
  tiers:
    strong:
      backend: claude
  policy:
    default_tier: strong
    rules:
      - when: { size: huge }
        tier: strong
`,
			wantSnip: "when.size",
		},
		{
			name: "max_tier unknown",
			routing: `
routing:
  tiers:
    strong:
      backend: claude
  policy:
    default_tier: strong
    escalation:
      max_tier: titan
`,
			wantSnip: "max_tier = \"titan\"",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parse([]byte(policyBackendsYAML + tc.routing))
			if err == nil {
				t.Fatalf("parse: expected error containing %q, got nil", tc.wantSnip)
			}
			if !strings.Contains(err.Error(), tc.wantSnip) {
				t.Fatalf("parse error = %q, want substring %q", err.Error(), tc.wantSnip)
			}
		})
	}
}

func TestRoutingPolicy_NonAgenticTierRejected(t *testing.T) {
	yaml := `
repo: owner/repo
model:
  default: codex
  backends:
    codex:
      cmd: codex
    helper:
      cmd: helper
      non_agentic: true
routing:
  mode: policy
  tiers:
    cheap:
      backend: helper
  policy:
    default_tier: cheap
`
	_, err := parse([]byte(yaml))
	if err == nil || !strings.Contains(err.Error(), "non_agentic") {
		t.Fatalf("parse error = %v, want non_agentic rejection", err)
	}
}
