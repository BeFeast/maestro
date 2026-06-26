package config

import (
	"fmt"
	"path/filepath"
	"strings"
)

// routingTierExists reports whether name is a usable tier reference in a policy
// rule or escalation cap: a declared tier, or the reserved passthrough sentinel
// that documents "this signal bypasses the policy".
func (c *Config) routingTierExists(name string) bool {
	if name == PolicyPassthroughTier {
		return true
	}
	_, ok := c.Routing.Tiers[name]
	return ok
}

// validateRoutingPolicy validates the task-aware routing tiers/policy (#783).
// It is a no-op for configs that set none of the new fields and do not request
// mode: policy, so existing configs validate byte-for-byte as before (RFC §2.9).
func validateRoutingPolicy(cfg *Config) error {
	r := cfg.Routing
	policyMode := r.IsPolicyMode()
	hasNew := len(r.Tiers) > 0 || r.Policy != nil

	if policyMode && len(r.Tiers) == 0 {
		return fmt.Errorf("config: routing.mode: policy requires routing.tiers to be configured")
	}
	if policyMode && r.Policy == nil {
		return fmt.Errorf("config: routing.mode: policy requires routing.policy to be configured")
	}
	if !hasNew {
		return nil
	}

	// Tiers must reference real, agentic, enabled backends so pricing/quota/
	// enabled keep gating selection (RFC §2.5).
	for name, tier := range r.Tiers {
		backend := strings.TrimSpace(tier.Backend)
		if backend == "" {
			return fmt.Errorf("config: routing.tiers.%s.backend is required", name)
		}
		def, ok := cfg.Model.Backends[backend]
		if !ok {
			return fmt.Errorf("config: routing.tiers.%s.backend = %q is not declared in model.backends", name, backend)
		}
		if def.NonAgentic {
			return fmt.Errorf("config: routing.tiers.%s.backend = %q is marked non_agentic; a tier drives workers and a non-agentic backend would emit instructions as text instead of executing them", name, backend)
		}
		if !def.IsEnabled() {
			return fmt.Errorf("config: routing.tiers.%s.backend = %q is disabled; enable it or point the tier at an enabled backend", name, backend)
		}
	}

	if r.Policy == nil {
		return nil
	}
	pol := r.Policy

	dt := strings.TrimSpace(pol.DefaultTier)
	if dt == "" {
		return fmt.Errorf("config: routing.policy.default_tier is required when routing.policy is configured")
	}
	if _, ok := r.Tiers[dt]; !ok {
		return fmt.Errorf("config: routing.policy.default_tier = %q is not declared in routing.tiers", dt)
	}

	for i, rule := range pol.Rules {
		tier := strings.TrimSpace(rule.Tier)
		if tier == "" {
			return fmt.Errorf("config: routing.policy.rules[%d].tier is required", i)
		}
		if !cfg.routingTierExists(tier) {
			return fmt.Errorf("config: routing.policy.rules[%d].tier = %q is not declared in routing.tiers", i, tier)
		}
		if rule.When.isEmpty() {
			return fmt.Errorf("config: routing.policy.rules[%d].when must set at least one of labels, risk_keywords, size, dependency", i)
		}
		if err := rule.When.validate(i); err != nil {
			return err
		}
	}

	for _, on := range pol.Escalation.On {
		switch strings.ToLower(strings.TrimSpace(on)) {
		case EscalationOnCIFailure, EscalationOnReviewRejection, EscalationOnRetry:
		default:
			return fmt.Errorf("config: routing.policy.escalation.on has unknown trigger %q (valid: ci_failure, review_rejection, retry)", on)
		}
	}
	if mt := strings.TrimSpace(pol.Escalation.MaxTier); mt != "" {
		if _, ok := r.Tiers[mt]; !ok {
			return fmt.Errorf("config: routing.policy.escalation.max_tier = %q is not declared in routing.tiers", mt)
		}
		// #792 P3: max_tier caps the escalation climb at its rank, so a climb is a
		// silent no-op only when NO possible starting tier ranks below max_tier. The
		// starting tiers are default_tier (unmatched issues) and each rule's tier
		// (matched issues). A high default_tier is fine as long as some lower-tier
		// rule can still climb up to max_tier — e.g. default_tier: strong, a
		// small/leaf -> cheap rule, and max_tier: standard lets cheap tasks climb to
		// standard while unmatched tasks start strong. Reject only when default_tier
		// sits above max_tier AND no rule selects a lower tier to climb from.
		order := r.OrderedTierNames()
		mtIdx, dtIdx := tierRankIndex(order, mt), tierRankIndex(order, dt)
		if mtIdx >= 0 && dtIdx >= 0 && mtIdx < dtIdx && !anyRuleTierBelow(order, mtIdx, pol.Rules) {
			return fmt.Errorf("config: routing.policy.escalation.max_tier = %q ranks below default_tier = %q and no rule selects a lower-rank tier, so the escalation ladder could never climb above the start tier; raise max_tier, lower default_tier, or add a lower-tier rule", mt, dt)
		}
	}
	if pol.Budget.MaxStrongPerWave < 0 {
		return fmt.Errorf("config: routing.policy.budget.max_strong_per_wave must be >= 0")
	}
	return nil
}

// tierRankIndex returns the position of name in the rank-ordered tier list (the
// same order the escalation ladder climbs), or -1 when absent.
func tierRankIndex(order []string, name string) int {
	for i, n := range order {
		if n == name {
			return i
		}
	}
	return -1
}

// anyRuleTierBelow reports whether some non-passthrough policy rule selects a
// starting tier ranked strictly below rank. Such a rule gives the escalation
// ladder a tier to climb up from even when default_tier sits at/above max_tier,
// so max_tier ranked below default_tier is not necessarily a no-op (#792 P3).
func anyRuleTierBelow(order []string, rank int, rules []RoutingPolicyRule) bool {
	for _, rule := range rules {
		t := strings.TrimSpace(rule.Tier)
		if t == "" || t == PolicyPassthroughTier {
			continue
		}
		if idx := tierRankIndex(order, t); idx >= 0 && idx < rank {
			return true
		}
	}
	return false
}

// isEmpty reports whether the predicate sets no signal, which would make a rule
// match every issue by accident — rejected at validation time.
func (m RoutingSignalMatch) isEmpty() bool {
	return len(m.Labels) == 0 && len(m.RiskKeywords) == 0 &&
		strings.TrimSpace(m.Size) == "" && strings.TrimSpace(m.Dependency) == ""
}

func (m RoutingSignalMatch) validate(idx int) error {
	if s := strings.ToLower(strings.TrimSpace(m.Size)); s != "" && s != SizeSmall && s != SizeLarge {
		return fmt.Errorf("config: routing.policy.rules[%d].when.size = %q (valid: small, large)", idx, m.Size)
	}
	if d := strings.ToLower(strings.TrimSpace(m.Dependency)); d != "" && d != DependencyLeaf && d != DependencyFoundation {
		return fmt.Errorf("config: routing.policy.rules[%d].when.dependency = %q (valid: leaf, foundation)", idx, m.Dependency)
	}
	for _, lbl := range m.Labels {
		if _, err := filepath.Match(lbl, ""); err != nil {
			return fmt.Errorf("config: routing.policy.rules[%d].when.labels has invalid glob %q: %w", idx, lbl, err)
		}
	}
	return nil
}

// Canonical size / dependency signal values (RFC §2.2).
const (
	SizeSmall            = "small"
	SizeLarge            = "large"
	DependencyLeaf       = "leaf"
	DependencyFoundation = "foundation"
)
