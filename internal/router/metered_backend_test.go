package router

import (
	"testing"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/github"
)

// TestResolveBackend_AutoMode_MeteredRouterModel_Refused pins #838 for the
// router: when routing.mode=auto and router_model is metered without
// routing.allow_metered_backend, the router skips the LLM call and falls back
// to model.default with ReasonMeteredRefused. The injected DecisionFn records
// whether the model call was dispatched — it must not be.
func TestResolveBackend_AutoMode_MeteredRouterModel_Refused(t *testing.T) {
	cfg := &config.Config{
		Model: config.ModelConfig{
			Default: "claude",
			Backends: map[string]config.BackendDef{
				"claude":    {Cmd: "claude"},
				"fireworks": {Cmd: "fw", PricingClass: config.PricingClassMetered},
			},
		},
		Routing: config.RoutingConfig{Mode: "auto", RouterModel: "fireworks"},
	}
	r := New(cfg)
	called := false
	r.DecisionFn = func(github.Issue) (Decision, error) {
		called = true
		return Decision{Backend: "codex"}, nil
	}

	name, reason := r.ResolveBackend(makeIssue(70, "route me", "enhancement"))
	if called {
		t.Fatal("router LLM (DecisionFn) must not be called for a metered router_model without opt-in")
	}
	if name != "claude" {
		t.Errorf("backend = %q, want claude (deterministic fallback)", name)
	}
	if reason != ReasonMeteredRefused {
		t.Errorf("reason = %q, want %q", reason, ReasonMeteredRefused)
	}
}

// TestResolveBackend_AutoMode_MeteredRouterModel_OptIn confirms the opt-in
// restores the auto path: routing.allow_metered_backend lets the router LLM run.
func TestResolveBackend_AutoMode_MeteredRouterModel_OptIn(t *testing.T) {
	cfg := &config.Config{
		Model: config.ModelConfig{
			Default: "claude",
			Backends: map[string]config.BackendDef{
				"claude":    {Cmd: "claude"},
				"codex":     {Cmd: "codex"},
				"fireworks": {Cmd: "fw", PricingClass: config.PricingClassMetered},
			},
		},
		Routing: config.RoutingConfig{Mode: "auto", RouterModel: "fireworks", AllowMeteredBackend: true},
	}
	r := New(cfg)
	called := false
	r.DecisionFn = func(github.Issue) (Decision, error) {
		called = true
		return Decision{Backend: "codex", Reason: "picked"}, nil
	}

	name, reason := r.ResolveBackend(makeIssue(71, "route me", "enhancement"))
	if !called {
		t.Fatal("opt-in must let the router LLM run")
	}
	if name != "codex" {
		t.Errorf("backend = %q, want codex (router pick)", name)
	}
	if reason != ReasonAuto {
		t.Errorf("reason = %q, want %q", reason, ReasonAuto)
	}
}
