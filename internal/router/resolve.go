package router

import (
	"log"
	"strings"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/github"
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

// Role constants for role-specific backend routing.
const (
	RolePlanner     = "planner"
	RoleImplementer = "implementer"
	RoleValidator   = "validator"
)

// resolveFromLabel checks for a model:<backend> label override on the issue.
// Returns (backend, reason, true) if a label was found, or ("", "", false) otherwise.
func (r *Router) resolveFromLabel(issue github.Issue) (string, string, bool) {
	name := BackendFromLabels(issue)
	if name == "" {
		return "", "", false
	}
	validated, ok := ValidateBackend(name, r.cfg)
	if !ok {
		log.Printf("[router] issue #%d: label specifies unknown backend %q, falling back to default %q",
			issue.Number, name, r.cfg.Model.Default)
		return validated, "unknown label backend", true
	}
	return validated, "label", true
}

// ResolveBackendForRole determines the backend for a specific role within the
// planner → implementer → validator pipeline. Priority:
//  1. model:<backend> label on the issue (highest, overrides everything)
//  2. Role-specific backend from routing config (planner_backend, etc.)
//  3. Issue-level routing (auto or default)
//
// If the role-specific backend is not configured or references an unknown
// backend, falls back to issue-level resolution.
func (r *Router) ResolveBackendForRole(issue github.Issue, role string) (backendName, reason string) {
	// 1. Label override always wins
	if backend, reason, found := r.resolveFromLabel(issue); found {
		if reason == "label" {
			log.Printf("[router] issue #%d [%s] → %s (label override)", issue.Number, role, backend)
		}
		return backend, reason
	}

	// 2. Role-specific backend from config
	var roleBackend string
	switch role {
	case RolePlanner:
		roleBackend = r.cfg.Routing.PlannerBackend
	case RoleImplementer:
		roleBackend = r.cfg.Routing.ImplementationBackend
	case RoleValidator:
		roleBackend = r.cfg.Routing.ValidatorBackend
	}
	if roleBackend != "" {
		validated, ok := ValidateBackend(roleBackend, r.cfg)
		if ok {
			log.Printf("[router] issue #%d [%s] → %s (role config)", issue.Number, role, validated)
			return validated, "role:" + role
		}
		log.Printf("[router] issue #%d [%s]: configured backend %q unknown, falling back to issue-level routing",
			issue.Number, role, roleBackend)
	}

	// 3. Fall back to issue-level resolution (inline to avoid redundant label check)
	return r.resolveIssueLevel(issue)
}

// ResolveBackend determines the backend for an issue using 3-tier priority:
//  1. model:<backend> label on the issue (highest priority)
//  2. Auto-routing via LLM (if routing.mode == "auto")
//  3. Default backend from config
func (r *Router) ResolveBackend(issue github.Issue) (backendName, reason string) {
	// 1. Check for model: label (highest priority)
	if backend, reason, found := r.resolveFromLabel(issue); found {
		if reason == "label" {
			log.Printf("[router] issue #%d → %s (label override)", issue.Number, backend)
		}
		return backend, reason
	}

	// 2. Auto-routing / default
	return r.resolveIssueLevel(issue)
}

// resolveIssueLevel handles auto-routing and default backend resolution.
// Extracted so ResolveBackendForRole can skip the redundant label check
// when falling back to issue-level routing.
func (r *Router) resolveIssueLevel(issue github.Issue) (backendName, reason string) {
	// Auto-routing via LLM (if enabled)
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

	// Default backend
	return r.cfg.Model.Default, "default"
}
