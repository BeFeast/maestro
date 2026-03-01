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
		routedBackend, routeReason, err := r.Route(issue)
		if err != nil {
			log.Printf("[router] issue #%d: error %v — using default", issue.Number, err)
		} else {
			log.Printf("[router] issue #%d → %s (%s)", issue.Number, routedBackend, routeReason)
		}
		// Route already falls back to default on error
		return routedBackend, routeReason
	}

	// 3. Default backend
	return r.cfg.Model.Default, "default"
}
