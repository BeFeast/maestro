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
func (r *Router) ResolveBackend(issue github.Issue) (backendName, reason string) {
	// 1. Check for model: label (highest priority)
	if name := BackendFromLabels(issue); name != "" {
		validated, ok := ValidateBackend(name, r.cfg)
		if !ok {
			log.Printf("[router] issue #%d: label specifies unknown backend %q, falling back to default %q",
				issue.Number, name, r.cfg.Model.Default)
			return validated, "unknown label backend"
		}
		log.Printf("[router] issue #%d → %s (label override)", issue.Number, validated)
		return validated, "label"
	}

	// 2. Auto-routing via LLM (if enabled)
	if r.cfg.Routing.Mode == "auto" {
		routeFn := r.Route
		if r.RouteFn != nil {
			routeFn = r.RouteFn
		}
		routedBackend, routeReason, err := routeFn(issue)
		if err != nil {
			log.Printf("[router] issue #%d: error %v — using default", issue.Number, err)
		} else if routedBackend != "" {
			log.Printf("[router] issue #%d → %s (%s)", issue.Number, routedBackend, routeReason)
			return routedBackend, routeReason
		}
		// Fall through to default on error or empty backend
	}

	// 3. Default backend
	return r.cfg.Model.Default, "default"
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
			return validated, "unknown label backend"
		}
		log.Printf("[router] issue #%d role=%s → %s (label override)", issue.Number, role, validated)
		return validated, "label"
	}

	// 2. Role-specific backend from config
	if rb := roleBackend(r.cfg, role); rb != "" {
		validated, ok := ValidateBackend(rb, r.cfg)
		if !ok {
			log.Printf("[router] issue #%d role=%s: configured role backend %q not found, falling back to issue-level routing",
				issue.Number, role, rb)
		} else {
			log.Printf("[router] issue #%d role=%s → %s (role config)", issue.Number, role, validated)
			return validated, "role"
		}
	}

	// 3. Fall back to issue-level resolution (auto-routing or default)
	return r.ResolveBackend(issue)
}
