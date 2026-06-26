package router

import (
	"log"
	"strings"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/github"
)

// BackendDecision is the resolved backend plus visibility metadata that callers
// persist on sessions.
type BackendDecision struct {
	Backend  string
	Reason   string
	TaskType string

	// Task-aware policy routing (#783). Tier is the strength tier the policy
	// resolved to (empty for non-policy decisions). Effort/Model are the tier's
	// optional per-tier overrides threaded into the worker argv so one backend
	// can serve multiple tiers. ShadowTier/ShadowReason carry the tier the
	// policy *would* have picked when policy.shadow is on — selection is
	// unchanged but the would-pick is logged/recorded for validation (RFC §2.8).
	Tier         string
	Effort       string
	Model        string
	ShadowTier   string
	ShadowReason string
}

// Selection reason canonical values written to Session.BackendSelection.SelectionReason
// when the orchestrator records why a backend was chosen for an issue.
const (
	ReasonLabel       = "label"
	ReasonAuto        = "auto"
	ReasonDefault     = "default"
	ReasonRouterError = "router_error"
	ReasonUnknownPin  = "unknown_label_backend"
	// ReasonPolicyError marks a policy decision that could not resolve to a
	// usable tier backend (e.g. tier points at a now-missing backend) and fell
	// back to model.default — the policy analogue of ReasonRouterError.
	ReasonPolicyError = "policy_error"
)

// BackendFromLabels extracts a backend name from issue labels with the "model:" prefix.
// Returns the backend name if found, empty string otherwise.
// If multiple model: labels exist, the first one wins.
func BackendFromLabels(issue github.Issue) string {
	for _, label := range issue.Labels {
		if strings.HasPrefix(label.Name, "model:") {
			if name := strings.TrimPrefix(label.Name, "model:"); name != "" {
				return name
			}
		}
	}
	return ""
}

// ValidateBackend checks that a backend name exists in the config's backend map.
// Returns the validated name and true if valid, or the default backend name and
// false if the requested backend is unknown.
func ValidateBackend(name string, cfg *config.Config) (string, bool) {
	if _, ok := cfg.Model.Backends[name]; ok {
		return name, true
	}
	return cfg.Model.Default, false
}

// ResolveBackend determines the backend for an issue using 3-tier priority:
//  1. model:<backend> label on the issue (highest priority)
//  2. Auto-routing via LLM (if routing.mode == "auto")
//  3. Default backend from config
//
// The returned reason is one of the canonical Reason* constants. When auto-routing
// is configured but its execution fails (network / parse / unknown backend), the
// reason is ReasonRouterError instead of ReasonDefault so operators can tell
// auto-routing failed silently rather than not being configured at all (#427).
func (r *Router) ResolveBackend(issue github.Issue) (backendName, reason string) {
	decision := r.ResolveBackendDecision(issue)
	return decision.Backend, decision.Reason
}

// ResolveBackendDecision determines the backend for an issue and includes the
// structured task type when routing.mode=auto produced one. It is the
// fresh-dispatch entry point (escalation level 0); the escalation ladder uses
// ResolveBackendDecisionForAttempt.
func (r *Router) ResolveBackendDecision(issue github.Issue) BackendDecision {
	return r.ResolveBackendDecisionForAttempt(issue, 0)
}

// ResolveBackendDecisionForAttempt resolves the backend for an issue at a given
// escalation level (RFC §2.6). escalationSteps > 0 climbs that many strength
// tiers above the signal-derived starting tier (capped at the policy max_tier);
// it only applies under routing.mode == "policy" and is ignored otherwise. The
// label override and model.default precedence is identical at every level.
func (r *Router) ResolveBackendDecisionForAttempt(issue github.Issue, escalationSteps int) BackendDecision {
	// 1. Check for model: label (highest priority)
	if name := BackendFromLabels(issue); name != "" {
		validated, ok := ValidateBackend(name, r.cfg)
		if !ok {
			log.Printf("[router] issue #%d: label specifies unknown backend %q, falling back to default %q",
				issue.Number, name, r.cfg.Model.Default)
			return BackendDecision{Backend: validated, Reason: ReasonUnknownPin}
		}
		log.Printf("[router] issue #%d → %s (label override)", issue.Number, validated)
		return BackendDecision{Backend: validated, Reason: ReasonLabel}
	}

	// 2. Task-aware policy routing (#783) — between the label override and
	// auto/default, evaluated only when routing.mode == "policy". Inert
	// otherwise, so manual/auto selection is byte-for-byte unchanged.
	if r.cfg.Routing.IsPolicyMode() {
		return r.resolvePolicyDecision(issue, escalationSteps)
	}

	// 3. Auto-routing via LLM (if enabled)
	if r.cfg.Routing.Mode == "auto" {
		routeDecisionFn := r.RouteDecision
		if r.DecisionFn != nil {
			routeDecisionFn = r.DecisionFn
		} else if r.RouteFn != nil {
			routeDecisionFn = func(issue github.Issue) (Decision, error) {
				backend, reason, err := r.RouteFn(issue)
				return Decision{Backend: backend, Reason: reason}, err
			}
		}
		routeDecision, err := routeDecisionFn(issue)
		if err != nil {
			log.Printf("[router] issue #%d: auto-routing failed (%v) — using default %q with reason=%s",
				issue.Number, err, r.cfg.Model.Default, ReasonRouterError)
			return BackendDecision{Backend: r.cfg.Model.Default, Reason: ReasonRouterError, TaskType: routeDecision.TaskType}
		}
		taskType := normalizeTaskType(routeDecision.TaskType)
		if taskType != "" {
			if mappedBackend := strings.TrimSpace(r.cfg.Routing.TaskTypeBackends[taskType]); mappedBackend != "" {
				validated, ok := ValidateBackend(mappedBackend, r.cfg)
				if !ok {
					log.Printf("[router] issue #%d: task_type=%s maps to unknown backend %q — using default %q with reason=%s",
						issue.Number, taskType, mappedBackend, r.cfg.Model.Default, ReasonRouterError)
					return BackendDecision{Backend: validated, Reason: ReasonRouterError, TaskType: taskType}
				}
				log.Printf("[router] issue #%d → %s (auto task_type=%s mapped from router backend=%s: %s)",
					issue.Number, validated, taskType, routeDecision.Backend, routeDecision.Reason)
				return BackendDecision{Backend: validated, Reason: ReasonAuto, TaskType: taskType}
			}
		}
		if routeDecision.Backend != "" {
			log.Printf("[router] issue #%d → %s (auto task_type=%s: %s)", issue.Number, routeDecision.Backend, taskType, routeDecision.Reason)
			return BackendDecision{Backend: routeDecision.Backend, Reason: ReasonAuto, TaskType: taskType}
		}
		// Auto-routing returned empty without an error — treat as router_error too
		// so the dashboard can distinguish silent fallback from manual mode.
		log.Printf("[router] issue #%d: auto-routing returned empty backend — using default %q with reason=%s",
			issue.Number, r.cfg.Model.Default, ReasonRouterError)
		return BackendDecision{Backend: r.cfg.Model.Default, Reason: ReasonRouterError, TaskType: taskType}
	}

	// 4. Default backend
	return BackendDecision{Backend: r.cfg.Model.Default, Reason: ReasonDefault}
}
