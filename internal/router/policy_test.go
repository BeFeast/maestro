package router

import (
	"strings"
	"testing"

	"github.com/befeast/maestro/internal/config"
)

func policyConfig() *config.Config {
	return &config.Config{
		Model: config.ModelConfig{
			Default: "codex",
			Backends: map[string]config.BackendDef{
				"gemini": {Cmd: "gemini"},
				"codex":  {Cmd: "codex"},
				"claude": {Cmd: "claude"},
			},
		},
		Routing: config.RoutingConfig{
			Mode: "policy",
			Tiers: map[string]config.RoutingTier{
				"cheap":    {Backend: "gemini", Rank: 0},
				"standard": {Backend: "codex", Effort: "medium", Rank: 1},
				"strong":   {Backend: "claude", Effort: "high", Rank: 2},
			},
			Policy: &config.RoutingPolicy{
				DefaultTier: "standard",
				Rules: []config.RoutingPolicyRule{
					{When: config.RoutingSignalMatch{Labels: []string{"model:*"}}, Tier: config.PolicyPassthroughTier},
					{When: config.RoutingSignalMatch{Labels: []string{"migration", "security"}}, Tier: "strong"},
					{When: config.RoutingSignalMatch{RiskKeywords: []string{"schema", "auth"}}, Tier: "strong"},
					{When: config.RoutingSignalMatch{Size: "large"}, Tier: "strong"},
					{When: config.RoutingSignalMatch{Size: "small", Dependency: "leaf"}, Tier: "cheap"},
				},
				Escalation: config.RoutingEscalation{Enabled: true, On: []string{"ci_failure", "retry"}, MaxTier: "strong"},
			},
		},
	}
}

func TestPolicy_LabelMatchSelectsStrong(t *testing.T) {
	r := New(policyConfig())
	d := r.ResolveBackendDecision(makeIssue(1, "Add table", "migration"))
	if d.Backend != "claude" || d.Tier != "strong" {
		t.Fatalf("decision = %+v, want claude/strong", d)
	}
	if d.Effort != "high" {
		t.Fatalf("effort = %q, want high", d.Effort)
	}
	if !strings.Contains(d.Reason, "policy:strong") || !strings.Contains(d.Reason, "labels=migration") {
		t.Fatalf("reason = %q, want policy:strong (labels=migration)", d.Reason)
	}
}

func TestPolicy_RiskKeywordMatch(t *testing.T) {
	r := New(policyConfig())
	issue := makeIssue(2, "Rework the auth flow")
	d := r.ResolveBackendDecision(issue)
	if d.Tier != "strong" || !strings.Contains(d.Reason, "risk_keywords=auth") {
		t.Fatalf("decision = %+v, want strong via risk_keywords=auth", d)
	}
}

// TestPolicy_RiskKeywordWordBoundary is the #792 P3 regression guard: risk
// keywords must match whole words only. The old strings.Contains over title+body
// false-routed to the expensive tier — "auth" matched "author"/"oauth", "infra"
// matched "infrastructure".
func TestPolicy_RiskKeywordWordBoundary(t *testing.T) {
	r := New(policyConfig())
	// Standalone keyword still routes to strong.
	if d := r.ResolveBackendDecision(makeIssue(20, "Rework the auth flow")); d.Tier != "strong" {
		t.Fatalf("standalone 'auth' tier = %q, want strong", d.Tier)
	}
	// Substrings must NOT match.
	if d := r.ResolveBackendDecision(makeIssue(21, "Credit the author in README")); d.Tier != "standard" {
		t.Fatalf("'author' falsely matched 'auth': tier = %q, want standard", d.Tier)
	}
	if d := r.ResolveBackendDecision(makeIssue(22, "Add oauth login button")); d.Tier != "standard" {
		t.Fatalf("'oauth' falsely matched 'auth': tier = %q, want standard", d.Tier)
	}
}

func TestPolicy_SizeDependencyMatch(t *testing.T) {
	r := New(policyConfig())
	d := r.ResolveBackendDecision(makeIssue(3, "Tiny fix", "size:small", "dependency:leaf"))
	if d.Tier != "cheap" || d.Backend != "gemini" {
		t.Fatalf("decision = %+v, want cheap/gemini", d)
	}
}

func TestPolicy_NoMatchUsesDefaultTier(t *testing.T) {
	r := New(policyConfig())
	d := r.ResolveBackendDecision(makeIssue(4, "Ordinary change"))
	if d.Tier != "standard" || d.Backend != "codex" {
		t.Fatalf("decision = %+v, want standard/codex", d)
	}
	if !strings.Contains(d.Reason, "default_tier") {
		t.Fatalf("reason = %q, want default_tier", d.Reason)
	}
}

func TestPolicy_LabelOverrideBeatsPolicy(t *testing.T) {
	// model:<name> is evaluated before the policy and wins (RFC §2.3/2.9).
	r := New(policyConfig())
	d := r.ResolveBackendDecision(makeIssue(5, "Big migration", "migration", "model:gemini"))
	if d.Backend != "gemini" || d.Reason != ReasonLabel {
		t.Fatalf("decision = %+v, want gemini/label (label beats policy)", d)
	}
	if d.Tier != "" {
		t.Fatalf("tier = %q, want empty (label path bypasses policy)", d.Tier)
	}
}

func TestPolicy_PassthroughFallsToDefaultTier(t *testing.T) {
	// A passthrough rule is treated as no policy match. (A model:* label would
	// normally be caught by the label override first; this exercises the
	// passthrough path directly via a non-backend label glob.)
	cfg := policyConfig()
	cfg.Routing.Policy.Rules = []config.RoutingPolicyRule{
		{When: config.RoutingSignalMatch{Labels: []string{"wip"}}, Tier: config.PolicyPassthroughTier},
		{When: config.RoutingSignalMatch{Labels: []string{"migration"}}, Tier: "strong"},
	}
	r := New(cfg)
	d := r.ResolveBackendDecision(makeIssue(6, "WIP", "wip"))
	if d.Tier != "standard" {
		t.Fatalf("decision = %+v, want default standard (passthrough = no match)", d)
	}
}

func TestPolicy_InertWhenModeManual(t *testing.T) {
	// mode != policy: tiers/policy present but ignored; model.default reached
	// exactly as today.
	cfg := policyConfig()
	cfg.Routing.Mode = "manual"
	r := New(cfg)
	d := r.ResolveBackendDecision(makeIssue(7, "Add table", "migration"))
	if d.Backend != "codex" || d.Reason != ReasonDefault {
		t.Fatalf("decision = %+v, want codex/default in manual mode", d)
	}
	if d.Tier != "" {
		t.Fatalf("tier = %q, want empty in manual mode", d.Tier)
	}
}

func TestPolicy_EscalationClimbsOneTierPerStep(t *testing.T) {
	r := New(policyConfig())
	issue := makeIssue(8, "Ordinary change") // starts at default standard
	// step 1 → strong (standard rank 1 → rank 2)
	d1 := r.ResolveBackendDecisionForAttempt(issue, 1)
	if d1.Tier != "strong" {
		t.Fatalf("escalation step 1 tier = %q, want strong", d1.Tier)
	}
	if !strings.Contains(d1.Reason, "escalation+1") {
		t.Fatalf("reason = %q, want escalation+1", d1.Reason)
	}
	// step 2 → capped at strong (top tier / max_tier)
	d2 := r.ResolveBackendDecisionForAttempt(issue, 2)
	if d2.Tier != "strong" {
		t.Fatalf("escalation step 2 tier = %q, want strong (capped)", d2.Tier)
	}
}

func TestPolicy_EscalationCappedByMaxTier(t *testing.T) {
	cfg := policyConfig()
	cfg.Routing.Policy.Escalation.MaxTier = "standard" // cap below strong
	r := New(cfg)
	issue := makeIssue(9, "Tiny", "size:small", "dependency:leaf") // starts cheap
	d := r.ResolveBackendDecisionForAttempt(issue, 5)
	if d.Tier != "standard" {
		t.Fatalf("escalation tier = %q, want standard (max_tier cap)", d.Tier)
	}
}

func TestPolicy_ShadowKeepsDispatchUnchanged(t *testing.T) {
	cfg := policyConfig()
	cfg.Routing.Policy.Shadow = true
	r := New(cfg)
	d := r.ResolveBackendDecision(makeIssue(10, "Add table", "migration"))
	if d.Backend != "codex" || d.Reason != ReasonDefault {
		t.Fatalf("shadow decision = %+v, want model.default codex/default", d)
	}
	if d.ShadowTier != "strong" {
		t.Fatalf("shadow tier = %q, want strong", d.ShadowTier)
	}
	if d.Tier != "" || d.Effort != "" {
		t.Fatalf("shadow must not apply tier override: %+v", d)
	}
}

func TestPolicy_DecisionForTier(t *testing.T) {
	r := New(policyConfig())
	d, ok := r.DecisionForTier("strong", "budget downgrade")
	if !ok || d.Backend != "claude" || d.Effort != "high" {
		t.Fatalf("DecisionForTier = %+v ok=%v, want claude/high", d, ok)
	}
	if _, ok := r.DecisionForTier("ghost", "x"); ok {
		t.Fatalf("DecisionForTier(ghost) ok=true, want false")
	}
}

func TestPolicy_UnknownTierBackendFallsToDefault(t *testing.T) {
	cfg := policyConfig()
	cfg.Routing.Tiers["standard"] = config.RoutingTier{Backend: "missing", Rank: 1}
	r := New(cfg)
	d := r.ResolveBackendDecision(makeIssue(11, "Ordinary"))
	if d.Backend != "codex" || d.Reason != ReasonPolicyError {
		t.Fatalf("decision = %+v, want model.default/policy_error", d)
	}
}
