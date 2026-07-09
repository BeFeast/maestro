package config

import "testing"

// TestBackendDef_IsMetered pins the #838 classification: only an explicit
// pricing_class: metered qualifies. A backend with a pricing: table but no
// class stays flat so subscription backends configured for cost observability
// (#619) are never gated.
func TestBackendDef_IsMetered(t *testing.T) {
	cases := []struct {
		name string
		def  BackendDef
		want bool
	}{
		{"explicit metered", BackendDef{PricingClass: "metered"}, true},
		{"metered case-insensitive", BackendDef{PricingClass: "Metered"}, true},
		{"metered padded", BackendDef{PricingClass: " metered "}, true},
		{"explicit flat", BackendDef{PricingClass: "flat"}, false},
		{"explicit subscription", BackendDef{PricingClass: "subscription"}, false},
		{"unset class", BackendDef{}, false},
		// Backward compat: a priced backend with no class is NOT metered.
		{"priced but no class", BackendDef{Pricing: BackendPricing{InputUSDPerMtok: 3, OutputUSDPerMtok: 9}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.def.IsMetered(); got != tc.want {
				t.Errorf("IsMetered() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestParse_PricingClass_RoundTripsAndValidates covers YAML parse of the field
// and the parse-time rejection of a mistyped class (which would otherwise
// silently disable the guard).
func TestParse_PricingClass_RoundTripsAndValidates(t *testing.T) {
	yaml := `
repo: owner/repo
model:
  default: claude
  backends:
    claude:
      cmd: claude
      pricing_class: subscription
    fireworks:
      cmd: fw
      pricing_class: metered
`
	cfg, err := parse([]byte(yaml))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if cfg.Model.Backends["claude"].IsMetered() {
		t.Error("claude (subscription) must not be metered")
	}
	if !cfg.Model.Backends["fireworks"].IsMetered() {
		t.Error("fireworks (metered) must be metered")
	}

	bad := `
repo: owner/repo
model:
  default: claude
  backends:
    claude:
      cmd: claude
      pricing_class: per_token
`
	if _, err := parse([]byte(bad)); err == nil {
		t.Fatal("parse must reject an unknown pricing_class")
	}
}

// TestSupervisorMeteredRefusal covers the supervisor guard truth table (#838):
// metered+no-opt-in refuses; metered+opt-in and flat/subscription/unset run.
func TestSupervisorMeteredRefusal(t *testing.T) {
	metered := func() *Config {
		return &Config{
			Model: ModelConfig{
				Default: "claude",
				Backends: map[string]BackendDef{
					"claude":    {Cmd: "claude"},
					"fireworks": {Cmd: "fw", PricingClass: PricingClassMetered},
				},
			},
			Supervisor: SupervisorConfig{Backend: "fireworks"},
		}
	}

	t.Run("metered + no opt-in refuses", func(t *testing.T) {
		backend, refused := metered().SupervisorMeteredRefusal()
		if !refused || backend != "fireworks" {
			t.Fatalf("SupervisorMeteredRefusal() = (%q, %v), want (fireworks, true)", backend, refused)
		}
	})

	t.Run("metered + opt-in runs", func(t *testing.T) {
		cfg := metered()
		cfg.Supervisor.AllowMeteredBackend = true
		if _, refused := cfg.SupervisorMeteredRefusal(); refused {
			t.Fatal("opt-in must clear the refusal")
		}
	})

	t.Run("flat backend runs", func(t *testing.T) {
		cfg := metered()
		cfg.Supervisor.Backend = "claude"
		if _, refused := cfg.SupervisorMeteredRefusal(); refused {
			t.Fatal("flat/unset backend must not be refused")
		}
	})

	t.Run("supervisor.backend unset falls back to model.default", func(t *testing.T) {
		cfg := metered()
		cfg.Supervisor.Backend = ""
		cfg.Model.Default = "fireworks"
		backend, refused := cfg.SupervisorMeteredRefusal()
		if !refused || backend != "fireworks" {
			t.Fatalf("fallback resolution = (%q, %v), want (fireworks, true)", backend, refused)
		}
	})
}

// TestRouterMeteredRefusal covers the router analog (#838): router_model metered
// without routing.allow_metered_backend refuses; opt-in and flat run.
func TestRouterMeteredRefusal(t *testing.T) {
	base := func() *Config {
		return &Config{
			Model: ModelConfig{
				Default: "claude",
				Backends: map[string]BackendDef{
					"claude":    {Cmd: "claude"},
					"fireworks": {Cmd: "fw", PricingClass: PricingClassMetered},
				},
			},
			Routing: RoutingConfig{RouterModel: "fireworks"},
		}
	}

	backend, refused := base().RouterMeteredRefusal()
	if !refused || backend != "fireworks" {
		t.Fatalf("RouterMeteredRefusal() = (%q, %v), want (fireworks, true)", backend, refused)
	}

	optIn := base()
	optIn.Routing.AllowMeteredBackend = true
	if _, refused := optIn.RouterMeteredRefusal(); refused {
		t.Fatal("routing.allow_metered_backend must clear the refusal")
	}

	flat := base()
	flat.Routing.RouterModel = "claude"
	if _, refused := flat.RouterMeteredRefusal(); refused {
		t.Fatal("flat router_model must not be refused")
	}
}

// TestMeteredRefusal_UnsetPricingZeroTableIsFlat is the backward-compat
// acceptance: a backend with no pricing_class and a zero pricing table is
// treated as flat, so nothing is gated for pre-#838 configs.
func TestMeteredRefusal_UnsetPricingZeroTableIsFlat(t *testing.T) {
	cfg := &Config{
		Model: ModelConfig{
			Default:  "claude",
			Backends: map[string]BackendDef{"claude": {Cmd: "claude"}},
		},
		Supervisor: SupervisorConfig{Backend: "claude"},
		Routing:    RoutingConfig{RouterModel: "claude"},
	}
	if _, refused := cfg.SupervisorMeteredRefusal(); refused {
		t.Error("unset pricing_class + zero pricing table must not refuse the supervisor")
	}
	if _, refused := cfg.RouterMeteredRefusal(); refused {
		t.Error("unset pricing_class + zero pricing table must not refuse the router")
	}
}
