package router

import (
	"log"
	"strings"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/github"
)

// Role constants for the planner → implementer → validator pipeline.
const (
	RolePlanner     = "planner"
	RoleImplementer = "implementer"
	RoleValidator   = "validator"
)

// BackendDecision is the resolved backend plus visibility metadata that callers
// persist on sessions.
type BackendDecision struct {
	Backend  string
	Reason   string
	TaskType string
}

// Selection reason canonical values written to Session.BackendSelection.SelectionReason
// when the orchestrator records why a backend was chosen for an issue.
const (
	ReasonLabel       = "label"
	ReasonRole        = "role"
	ReasonAuto        = "auto"
	ReasonDefault     = "default"
	ReasonRouterError = "router_error"
	ReasonUnknownPin  = "unknown_label_backend"
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
// structured task type when routing.mode=auto produced one.
func (r *Router) ResolveBackendDecision(issue github.Issue) BackendDecision {
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

	// 2. Auto-routing via LLM (if enabled)
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

	// 3. Default backend
	return BackendDecision{Backend: r.cfg.Model.Default, Reason: ReasonDefault}
}

// roleBackend returns the configured backend name for a given role, or empty string if not set.
func roleBackend(cfg *config.Config, role string) string {
	switch role {
	case RolePlanner:
		return cfg.Routing.PlannerBackend
	case RoleImplementer:
		return cfg.Routing.ImplementationBackend
	case RoleValidator:
		return cfg.Routing.ValidatorBackend
	default:
		return ""
	}
}

// ResolveBackendForRole determines the backend for an issue and a specific pipeline role.
// Priority:
//  1. model:<backend> label on the issue (highest priority, same as issue-level)
//  2. Role-specific backend from config (e.g. routing.planner_backend)
//  3. Falls back to issue-level ResolveBackend (auto-routing or default)
func (r *Router) ResolveBackendForRole(issue github.Issue, role string) (backendName, reason string) {
	// 1. Label override takes precedence over everything (consistent with ResolveBackend)
	if name := BackendFromLabels(issue); name != "" {
		validated, ok := ValidateBackend(name, r.cfg)
		if !ok {
			log.Printf("[router] issue #%d role=%s: label specifies unknown backend %q, falling back to default %q",
				issue.Number, role, name, r.cfg.Model.Default)
			return validated, ReasonUnknownPin
		}
		log.Printf("[router] issue #%d role=%s → %s (label override)", issue.Number, role, validated)
		return validated, ReasonLabel
	}

	// 2. Role-specific backend from config
	if rb := roleBackend(r.cfg, role); rb != "" {
		validated, ok := ValidateBackend(rb, r.cfg)
		if !ok {
			log.Printf("[router] issue #%d role=%s: configured role backend %q not found, falling back to issue-level routing",
				issue.Number, role, rb)
		} else {
			log.Printf("[router] issue #%d role=%s → %s (role config)", issue.Number, role, validated)
			return validated, ReasonRole
		}
	}

	// 3. Fall back to issue-level resolution (auto-routing or default)
	return r.ResolveBackend(issue)
}
