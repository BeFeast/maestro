package orchestrator

import (
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/github"
	"github.com/befeast/maestro/internal/router"
	"github.com/befeast/maestro/internal/state"
)

// applyTierOverride returns a config whose chosen backend def carries the
// resolved tier's per-tier effort/model override (#783, RFC §2.4 step 5). When
// the decision has no override the input config is returned unchanged. The
// override is applied to a clone (deep-copying the Backends map) so the shared
// base config is never mutated; worker.Start/Respawn then read the override off
// the backend def exactly as it reads a backend's own effort/model.
func applyTierOverride(cfg *config.Config, backendName string, decision router.BackendDecision) *config.Config {
	effort := strings.TrimSpace(decision.Effort)
	model := strings.TrimSpace(decision.Model)
	if effort == "" && model == "" {
		return cfg
	}
	def, ok := cfg.Model.Backends[backendName]
	if !ok {
		return cfg
	}
	clone := *cfg
	backends := make(map[string]config.BackendDef, len(cfg.Model.Backends))
	for k, v := range cfg.Model.Backends {
		backends[k] = v
	}
	if effort != "" {
		def.Effort = effort
	}
	if model != "" {
		def.Model = model
	}
	backends[backendName] = def
	clone.Model.Backends = backends
	return &clone
}

// policyBackendSelection builds the audit record for a policy decision (RFC
// §2.7): the deciding signal+tier reason, the tier and its overrides, and
// real, tier-rank-derived candidate scores (replacing the constant
// placeholders for the policy path).
func (o *Orchestrator) policyBackendSelection(decision router.BackendDecision) *state.BackendSelection {
	sel := &state.BackendSelection{
		SelectedBackend: decision.Backend,
		SelectionReason: decision.Reason,
		TaskType:        decision.TaskType,
		Tier:            decision.Tier,
		Effort:          decision.Effort,
		Model:           decision.Model,
		ShadowTier:      decision.ShadowTier,
	}
	if decision.Tier != "" {
		sel.CandidateScores = o.policyCandidateScores(decision.Tier)
	}
	return sel
}

// policyCandidateScores derives a candidate score per declared tier from the
// tier ranking and which tier was selected, so the fleet view shows real
// signal-derived scores instead of the 0.5/0.8/0.9 placeholders for a policy
// decision (RFC §2.7).
func (o *Orchestrator) policyCandidateScores(selectedTier string) []state.BackendCandidate {
	order := o.cfg.Routing.OrderedTierNames()
	if len(order) == 0 {
		return nil
	}
	maxRank := float64(len(order) - 1)
	if maxRank <= 0 {
		maxRank = 1
	}
	scores := make([]state.BackendCandidate, 0, len(order))
	for i, name := range order {
		tier := o.cfg.Routing.Tiers[name]
		def, known := o.cfg.Model.Backends[tier.Backend]
		policyScore := float64(i) / maxRank
		fit := 0.5
		if name == selectedTier {
			fit = 1.0
		}
		c := state.BackendCandidate{
			Backend:   tier.Backend,
			Fit:       fit,
			Policy:    policyScore,
			Final:     (fit + policyScore) / 2,
			Available: known && def.IsEnabled(),
		}
		if !c.Available {
			if !known {
				c.BlockedBy = state.BackendBlockUnknown
			} else {
				c.BlockedBy = state.BackendBlockDisabled
			}
		}
		scores = append(scores, c)
	}
	return scores
}

// applyPolicyBudget enforces routing.policy.budget.max_strong_per_wave (RFC
// §2.5): when a fresh dispatch resolves to the top (strong) tier and the wave
// already has that many active strong-tier sessions, it is downgraded
// ("excess large tasks queue at standard"). A zero/absent cap is unlimited and
// escalated retries are not subject to the cap.
func (o *Orchestrator) applyPolicyBudget(s *state.State, decision router.BackendDecision) router.BackendDecision {
	pol := o.cfg.Routing.Policy
	if pol == nil || pol.Budget.MaxStrongPerWave <= 0 || decision.Tier == "" {
		return decision
	}
	top := o.policyTopTier()
	if top == "" || decision.Tier != top {
		return decision
	}
	if o.countActiveTierSessions(s, top) < pol.Budget.MaxStrongPerWave {
		return decision
	}
	// Pick where the over-budget dispatch lands. Normally the default tier, but a
	// config can set default_tier == top (everything defaults to strong); then a
	// no-op downgrade-to-self would silently bypass the cap, so fall back to the
	// tier one rank below top. "" means no lower tier exists — leave it unchanged.
	target := o.policyBudgetDowngradeTier(top)
	if target == "" || target == top {
		return decision
	}
	reason := fmt.Sprintf("policy:%s (budget downgrade from %s, max_strong_per_wave=%d)",
		target, top, pol.Budget.MaxStrongPerWave)
	if downgraded, ok := o.router.DecisionForTier(target, reason); ok {
		return downgraded
	}
	return decision
}

// policyBudgetDowngradeTier resolves the tier an over-budget top-tier dispatch
// is downgraded to. It is the default tier when that is below the top tier
// ("excess large tasks queue at standard"); when default_tier is itself the top
// tier (or unset) the cap would be unenforceable, so it falls back to the tier
// one rank below top. Returns "" when no lower tier exists (a single-tier config
// cannot enforce the cap).
func (o *Orchestrator) policyBudgetDowngradeTier(top string) string {
	pol := o.cfg.Routing.Policy
	if pol == nil {
		return ""
	}
	if def := strings.TrimSpace(pol.DefaultTier); def != "" && def != top {
		return def
	}
	order := o.cfg.Routing.OrderedTierNames()
	if len(order) >= 2 {
		return order[len(order)-2]
	}
	return ""
}

func (o *Orchestrator) policyTopTier() string {
	order := o.cfg.Routing.OrderedTierNames()
	if len(order) == 0 {
		return ""
	}
	return order[len(order)-1]
}

func (o *Orchestrator) countActiveTierSessions(s *state.State, tier string) int {
	n := 0
	for _, sess := range s.ActiveSessions() {
		if sess != nil && sess.BackendSelection != nil && sess.BackendSelection.Tier == tier {
			n++
		}
	}
	return n
}

// escalateRetryBackend re-resolves the routing tier for a retry under
// routing.mode: policy and climbs the escalation ladder when the retry's
// trigger is enabled in escalation.on (RFC §2.6). It returns the re-resolved
// decision and ok=true whenever policy routing is active, so the retry
// dispatches on the (possibly escalated) tier — and any newly-applied model:
// label — instead of blindly reusing sess.Backend. ok=false leaves the retry on
// today's behavior (reuse sess.Backend).
//
// Must be called before the retry consumes sess.CIFailureOutput /
// PreviousAttemptFeedback, since the trigger is derived from them.
func (o *Orchestrator) escalateRetryBackend(s *state.State, sess *state.Session, issue github.Issue) (router.BackendDecision, bool) {
	if !o.cfg.Routing.IsPolicyMode() || o.cfg.Routing.Policy == nil {
		return router.BackendDecision{}, false
	}
	// Leave the planner/validator pipeline phases on their own per-role backend
	// (pipeline.{planner,validator}.backend); policy escalation governs the
	// normal single-phase / implement path only, so the two mechanisms stay
	// orthogonal (RFC §1.4 / §2.6).
	if sess.Phase == state.PhasePlan || sess.Phase == state.PhaseValidate {
		return router.BackendDecision{}, false
	}
	steps := escalationSteps(o.cfg.Routing.Policy, sess)
	decision := o.router.ResolveBackendDecisionForAttempt(issue, steps)
	if decision.Backend == "" {
		return router.BackendDecision{}, false
	}
	// Reuse BackendHealth gating (RFC §2.6): never escalate onto a backend that
	// is disabled or cooling down after an auth failure / provider limit — leave
	// the retry on its current backend instead of spawning a doomed worker.
	if blockedBy, _ := o.dispatchBackendBlock(s, decision.Backend, time.Now().UTC()); blockedBy != "" {
		log.Printf("[orch] issue #%d retry: escalation target %s blocked (%s) — keeping backend %s",
			sess.IssueNumber, decision.Backend, blockedBy, sess.Backend)
		return router.BackendDecision{}, false
	}
	return decision, true
}

// escalationSteps returns how many tiers a retry climbs: 0 when escalation is
// disabled or this retry's trigger is not in escalation.on, else the live
// session's own retry count (each retry of this session climbs one tier). It
// deliberately uses sess.RetryCount rather than FailedAttemptsForIssue: the
// latter counts every dead / failed no-PR session for the issue, so a stale
// abandoned attempt would inflate the climb and skip tiers the current attempt
// history does not warrant. FailedAttemptsForIssue + RetryCount remains the
// per-issue *retry budget* (canRetryIssue) that, with max_tier, bounds the
// ladder so it cannot loop (RFC §2.6).
func escalationSteps(pol *config.RoutingPolicy, sess *state.Session) int {
	if pol == nil || !pol.Escalation.Enabled {
		return 0
	}
	if !escalationTriggerEnabled(pol.Escalation.On, retryTrigger(sess)) {
		return 0
	}
	steps := sess.RetryCount
	if steps < 1 {
		steps = 1
	}
	return steps
}

// retryTrigger classifies why a retry is happening so escalation.on can opt in
// per trigger class. CI-failure context takes precedence over review feedback,
// which takes precedence over a plain retry.
func retryTrigger(sess *state.Session) string {
	switch {
	case strings.TrimSpace(sess.CIFailureOutput) != "":
		return config.EscalationOnCIFailure
	case sess.RetryReason == state.RetryReasonReviewFeedback ||
		sess.PreviousAttemptFeedbackKind == state.RetryReasonReviewFeedback:
		return config.EscalationOnReviewRejection
	default:
		return config.EscalationOnRetry
	}
}

func escalationTriggerEnabled(on []string, trigger string) bool {
	for _, t := range on {
		if strings.EqualFold(strings.TrimSpace(t), trigger) {
			return true
		}
	}
	return false
}
