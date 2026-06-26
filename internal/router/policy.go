package router

import (
	"fmt"
	"log"
	"path/filepath"
	"strings"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/github"
)

// resolvePolicyDecision implements the deterministic signal→tier policy
// (RFC §2.4–2.6). It assumes routing.mode == "policy" and the label override
// already missed. escalationSteps climbs that many tiers above the
// signal-derived starting tier (capped at the policy max_tier).
//
// In shadow mode (policy.shadow) the dispatched backend is left at
// model.default and the would-pick tier is attached as ShadowTier so a wave can
// be validated before the policy is enabled (RFC §2.8).
func (r *Router) resolvePolicyDecision(issue github.Issue, escalationSteps int) BackendDecision {
	pol := r.cfg.Routing.Policy
	if pol == nil {
		// mode: policy without a policy block is rejected at config load, but
		// stay safe at runtime and fall through to the default backend.
		return BackendDecision{Backend: r.cfg.Model.Default, Reason: ReasonDefault}
	}

	startTier, signal, matched := r.matchPolicyTier(issue)
	if !matched {
		startTier = strings.TrimSpace(pol.DefaultTier)
		signal = "default_tier"
	}

	tierName := startTier
	if escalationSteps > 0 {
		tierName = r.climbTier(startTier, escalationSteps)
		signal = fmt.Sprintf("escalation+%d from %s=%s", escalationSteps, startTier, signal)
	}

	tier, ok := r.cfg.Routing.Tiers[tierName]
	if !ok {
		log.Printf("[router] issue #%d: policy tier %q not declared — using default %q with reason=%s",
			issue.Number, tierName, r.cfg.Model.Default, ReasonPolicyError)
		return BackendDecision{Backend: r.cfg.Model.Default, Reason: ReasonPolicyError, Tier: tierName}
	}
	backend, valid := ValidateBackend(strings.TrimSpace(tier.Backend), r.cfg)
	if !valid {
		log.Printf("[router] issue #%d: policy tier %q backend %q not declared — using default %q with reason=%s",
			issue.Number, tierName, tier.Backend, r.cfg.Model.Default, ReasonPolicyError)
		return BackendDecision{Backend: backend, Reason: ReasonPolicyError, Tier: tierName}
	}

	reason := fmt.Sprintf("policy:%s (%s)", tierName, signal)

	// Shadow mode: keep dispatch on model.default, but record the would-pick.
	if pol.Shadow {
		log.Printf("[router] issue #%d: policy SHADOW would pick tier %q (backend=%s, %s) — dispatching default %q unchanged",
			issue.Number, tierName, backend, signal, r.cfg.Model.Default)
		return BackendDecision{
			Backend:      r.cfg.Model.Default,
			Reason:       ReasonDefault,
			ShadowTier:   tierName,
			ShadowReason: reason,
		}
	}

	log.Printf("[router] issue #%d → %s (%s, effort=%q model=%q)", issue.Number, backend, reason, tier.Effort, tier.Model)
	return BackendDecision{
		Backend: backend,
		Reason:  reason,
		Tier:    tierName,
		Effort:  strings.TrimSpace(tier.Effort),
		Model:   strings.TrimSpace(tier.Model),
	}
}

// DecisionForTier builds a decision for an explicit tier name, used by the
// budget cap to downgrade an over-budget strong dispatch to the default tier
// without re-running the rule list. ok is false when the tier or its backend is
// not usable.
func (r *Router) DecisionForTier(tierName, reason string) (BackendDecision, bool) {
	tier, ok := r.cfg.Routing.Tiers[tierName]
	if !ok {
		return BackendDecision{}, false
	}
	backend, valid := ValidateBackend(strings.TrimSpace(tier.Backend), r.cfg)
	if !valid {
		return BackendDecision{}, false
	}
	return BackendDecision{
		Backend: backend,
		Reason:  reason,
		Tier:    tierName,
		Effort:  strings.TrimSpace(tier.Effort),
		Model:   strings.TrimSpace(tier.Model),
	}, true
}

// matchPolicyTier runs the first-match rule list and returns the resolved tier
// plus a short description of the deciding signal (e.g. "labels=migration").
// A rule that resolves to the passthrough sentinel is treated as no match, so
// resolution falls through to DefaultTier — this lets an operator document that
// a signal (e.g. a model:* label) bypasses the policy without effect.
func (r *Router) matchPolicyTier(issue github.Issue) (tier, signal string, matched bool) {
	pol := r.cfg.Routing.Policy
	if pol == nil {
		return "", "", false
	}
	for _, rule := range pol.Rules {
		desc, ok := matchSignal(issue, rule.When)
		if !ok {
			continue
		}
		if strings.TrimSpace(rule.Tier) == config.PolicyPassthroughTier {
			return "", "", false
		}
		return strings.TrimSpace(rule.Tier), desc, true
	}
	return "", "", false
}

// climbTier returns the tier `steps` ranks above start in the ordered tier
// list, capped at the policy max_tier (or the top tier when unset). This is the
// loop-safety bound that, with the per-issue retry budget, keeps escalation from
// climbing forever (RFC §2.6).
func (r *Router) climbTier(start string, steps int) string {
	order := r.cfg.Routing.OrderedTierNames()
	idx := indexOfString(order, start)
	if idx < 0 || len(order) == 0 {
		return start
	}
	maxIdx := len(order) - 1
	if pol := r.cfg.Routing.Policy; pol != nil {
		if mt := strings.TrimSpace(pol.Escalation.MaxTier); mt != "" {
			if mi := indexOfString(order, mt); mi >= 0 && mi < maxIdx {
				maxIdx = mi
			}
		}
	}
	target := idx + steps
	if target > maxIdx {
		target = maxIdx
	}
	if target < idx {
		target = idx
	}
	return order[target]
}

// matchSignal reports whether the issue satisfies the predicate. Every field
// the predicate sets must match (logical AND); within a field the match is an
// OR (the issue need satisfy only one listed label / keyword). It returns a
// short descriptor of the deciding match for the audit reason.
func matchSignal(issue github.Issue, when config.RoutingSignalMatch) (desc string, ok bool) {
	if len(when.Labels) > 0 {
		lbl, hit := matchAnyLabel(issue, when.Labels)
		if !hit {
			return "", false
		}
		desc = "labels=" + lbl
	}
	if len(when.RiskKeywords) > 0 {
		kw, hit := matchAnyKeyword(issue, when.RiskKeywords)
		if !hit {
			return "", false
		}
		if desc == "" {
			desc = "risk_keywords=" + kw
		}
	}
	if want := strings.ToLower(strings.TrimSpace(when.Size)); want != "" {
		if issueSize(issue) != want {
			return "", false
		}
		if desc == "" {
			desc = "size=" + want
		}
	}
	if want := strings.ToLower(strings.TrimSpace(when.Dependency)); want != "" {
		if issueDependency(issue) != want {
			return "", false
		}
		if desc == "" {
			desc = "dependency=" + want
		}
	}
	if desc == "" {
		// Predicate set no fields — config validation rejects this, but never
		// match-everything at runtime.
		return "", false
	}
	return desc, true
}

func matchAnyLabel(issue github.Issue, globs []string) (matched string, ok bool) {
	for _, label := range issue.Labels {
		name := strings.TrimSpace(label.Name)
		for _, glob := range globs {
			if ok, _ := filepath.Match(strings.TrimSpace(glob), name); ok {
				return name, true
			}
		}
	}
	return "", false
}

func matchAnyKeyword(issue github.Issue, keywords []string) (matched string, ok bool) {
	hay := strings.ToLower(issue.Title + "\n" + issue.Body)
	for _, kw := range keywords {
		k := strings.ToLower(strings.TrimSpace(kw))
		if k != "" && strings.Contains(hay, k) {
			return k, true
		}
	}
	return "", false
}

// issueSize derives the size signal from a size:<v> (or size/<v>) issue label.
func issueSize(issue github.Issue) string {
	return labelValue(issue, "size")
}

// issueDependency derives the dependency signal from a dependency:<v> (or
// dependency/<v>) issue label; also accepts the bare leaf/foundation labels.
func issueDependency(issue github.Issue) string {
	if v := labelValue(issue, "dependency"); v != "" {
		return v
	}
	for _, label := range issue.Labels {
		switch strings.ToLower(strings.TrimSpace(label.Name)) {
		case config.DependencyLeaf:
			return config.DependencyLeaf
		case config.DependencyFoundation:
			return config.DependencyFoundation
		}
	}
	return ""
}

// labelValue returns the lowercased value of the first key:<v> or key/<v> label.
func labelValue(issue github.Issue, key string) string {
	for _, label := range issue.Labels {
		name := strings.TrimSpace(label.Name)
		for _, sep := range []string{":", "/"} {
			prefix := key + sep
			if strings.HasPrefix(strings.ToLower(name), prefix) {
				return strings.ToLower(strings.TrimSpace(name[len(prefix):]))
			}
		}
	}
	return ""
}

func indexOfString(values []string, want string) int {
	for i, v := range values {
		if v == want {
			return i
		}
	}
	return -1
}
