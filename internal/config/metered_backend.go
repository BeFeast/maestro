package config

import "strings"

// Metered-backend guard (#838).
//
// Nothing used to stop an operator (or a config-store edit) from pointing a
// high-frequency internal loop — supervisor cycles, the auto-router — at a
// per-token metered model. On 2026-07-09 the shared codex backend row was
// re-pointed at a per-token Fireworks model in response to a 429-storm;
// supervise cycles then burned ~$4/hour making "do nothing" decisions across
// nine idle projects. The guard below lets the supervisor and router refuse a
// backend declared `pricing_class: metered` unless the project opts in
// explicitly, capping the blast radius of a re-point.
//
// These helpers are pure config reads so both the deterministic supervisor
// decision and the router can share one source of truth, and so the whole
// guard is unit-testable without a real backend.

// ResolvedSupervisorBackend returns the backend name the supervisor LLM loop
// dispatches to: supervisor.backend when set, otherwise model.default. parse()
// already defaults supervisor.backend to model.default, but the fallback here
// keeps the helper correct for configs built in-process (tests, hot-reload).
func (c *Config) ResolvedSupervisorBackend() string {
	if c == nil {
		return ""
	}
	name := strings.TrimSpace(c.Supervisor.Backend)
	if name == "" {
		name = strings.TrimSpace(c.Model.Default)
	}
	return name
}

// ResolvedRouterBackend returns the backend name the auto-router dispatches to:
// routing.router_model. parse() defaults it to "claude".
func (c *Config) ResolvedRouterBackend() string {
	if c == nil {
		return ""
	}
	return strings.TrimSpace(c.Routing.RouterModel)
}

// backendIsMetered reports whether the named backend is declared metered
// (pricing_class: metered). An unknown name is not metered — the caller's
// existing "backend not found" error path handles that case.
func (c *Config) backendIsMetered(name string) bool {
	if c == nil {
		return false
	}
	def, ok := c.Model.Backends[strings.TrimSpace(name)]
	return ok && def.IsMetered()
}

// SupervisorMeteredRefusal reports whether the supervisor LLM path must be
// refused because its resolved backend is metered and the project has not set
// supervisor.allow_metered_backend (#838). The returned backend name feeds the
// red stuck state / log line. refused=false means the LLM path may run — either
// the backend is flat/subscription/unset, or the operator opted in.
func (c *Config) SupervisorMeteredRefusal() (backend string, refused bool) {
	if c == nil || c.Supervisor.AllowMeteredBackend {
		return "", false
	}
	name := c.ResolvedSupervisorBackend()
	if c.backendIsMetered(name) {
		return name, true
	}
	return "", false
}

// RouterMeteredRefusal is the router analog of SupervisorMeteredRefusal: it
// reports whether routing.router_model is metered and routing.allow_metered_backend
// is not set (#838).
func (c *Config) RouterMeteredRefusal() (backend string, refused bool) {
	if c == nil || c.Routing.AllowMeteredBackend {
		return "", false
	}
	name := c.ResolvedRouterBackend()
	if c.backendIsMetered(name) {
		return name, true
	}
	return "", false
}
