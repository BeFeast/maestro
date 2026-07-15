package config

import (
	"reflect"
	"strings"
	"testing"
)

func TestProviderLanesParseAndResolve(t *testing.T) {
	cfg, err := Parse([]byte(`
repo: owner/repo
model:
  default: claude
  provider_lanes:
    - provider: anthropic
      default: claude
    - provider: openai
      default: sol
      fallback_backends: [gpt55]
  backends:
    claude: {cmd: claude, provider: anthropic, model: fable-5, effort: high}
    sol: {cmd: codex, provider: openai, model: gpt-5.6-sol, effort: high}
    gpt55: {cmd: codex, provider: openai, model: gpt-5.5, effort: high}
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	route := cfg.Model.ResolvedRoute()
	if route.SelectionReason != ModelRouteProviderLanes {
		t.Fatalf("selection reason = %q, want %q", route.SelectionReason, ModelRouteProviderLanes)
	}
	if want := []string{"claude", "sol", "gpt55"}; !reflect.DeepEqual(route.Backends, want) {
		t.Fatalf("route = %v, want %v", route.Backends, want)
	}
	if cfg.Model.EffectiveDefault() != "claude" {
		t.Fatalf("effective default = %q, want claude", cfg.Model.EffectiveDefault())
	}
	if got := cfg.Model.Backends["sol"]; got.Model != "gpt-5.6-sol" || got.Effort != "high" {
		t.Fatalf("SOL backend = %+v", got)
	}
}

func TestExplicitFallbackChainOverridesProviderLanes(t *testing.T) {
	m := ModelConfig{
		Default:          "claude",
		FallbackBackends: []string{"sol", "gpt55"},
		ProviderLanes: []ProviderLane{
			{Provider: "anthropic", Default: "claude"},
			{Provider: "openai", Default: "sol", FallbackBackends: []string{"gpt55"}},
		},
		Backends: map[string]BackendDef{
			"claude": {Provider: "anthropic"},
			"sol":    {Provider: "openai"},
			"gpt55":  {Provider: "openai"},
		},
	}
	route := m.ResolvedRoute()
	if route.SelectionReason != ModelRouteExplicitBackendChain || !reflect.DeepEqual(route.Backends, []string{"claude", "sol", "gpt55"}) {
		t.Fatalf("route = %+v, want legacy explicit chain", route)
	}
	if got := m.FallbackCandidates("gpt55"); !reflect.DeepEqual(got, []string{"sol"}) {
		t.Fatalf("fallback candidates from gpt55 = %v, want legacy scan [sol]", got)
	}
}

func TestModelRouteWithoutFallbackDoesNotUseBackendMapOrder(t *testing.T) {
	m := ModelConfig{
		Default: "claude",
		Backends: map[string]BackendDef{
			"claude": {Provider: "anthropic"},
			"aaa":    {Provider: "openai"},
			"zzz":    {Provider: "openai"},
		},
	}
	if got := m.FallbackCandidates("claude"); len(got) != 0 {
		t.Fatalf("implicit fallback candidates = %v, want none", got)
	}
}

func TestProviderLaneValidationRejectsProviderMismatch(t *testing.T) {
	_, err := Parse([]byte(`
repo: owner/repo
model:
  provider_lanes:
    - provider: openai
      default: claude
  backends:
    claude: {provider: anthropic}
`))
	if err == nil || !strings.Contains(err.Error(), "declared with provider") {
		t.Fatalf("error = %v, want provider mismatch", err)
	}
}

func TestProviderLaneValidationRejectsUndeclaredDerivedDefault(t *testing.T) {
	_, err := Parse([]byte(`
repo: owner/repo
model:
  provider_lanes:
    - provider: openai
      default: sol
  backends: {}
`))
	if err == nil || !strings.Contains(err.Error(), `references "sol" which is not defined`) {
		t.Fatalf("error = %v, want undeclared lane default", err)
	}
}
