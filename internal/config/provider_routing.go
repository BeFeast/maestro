package config

import (
	"fmt"
	"strings"
)

const (
	ModelRouteExplicitBackendChain = "explicit_backend_chain"
	ModelRouteProviderLanes        = "provider_lanes"
	ModelRouteDefaultOnly          = "model_default_only"
)

// ProviderLane is one provider-local routing lane. Default is tried first,
// followed by FallbackBackends, before selection advances to the next lane.
type ProviderLane struct {
	Provider         string   `yaml:"provider" json:"provider"`
	Default          string   `yaml:"default" json:"default"`
	FallbackBackends []string `yaml:"fallback_backends,omitempty" json:"fallback_backends,omitempty"`
}

// ModelRoute is the exact effective worker route and why it was selected.
type ModelRoute struct {
	Lanes           []ProviderLane
	Backends        []string
	SelectionReason string
}

// EffectiveDefault returns the backend used when no label or routing policy
// chooses another backend. A legacy explicit fallback chain remains anchored
// to model.default; otherwise the first provider lane supplies the default.
func (m ModelConfig) EffectiveDefault() string {
	if len(m.FallbackBackends) == 0 && len(m.ProviderLanes) > 0 {
		if name := strings.TrimSpace(m.ProviderLanes[0].Default); name != "" {
			return name
		}
	}
	return strings.TrimSpace(m.Default)
}

// ResolvedRoute returns the deterministic worker route. Legacy explicit
// fallback_backends wins as a project-local override. No configured route ever
// falls through to backend-map iteration order.
func (m ModelConfig) ResolvedRoute() ModelRoute {
	if len(m.FallbackBackends) > 0 {
		backends := appendUniqueBackend(nil, m.Default)
		for _, name := range m.FallbackBackends {
			backends = appendUniqueBackend(backends, name)
		}
		return ModelRoute{
			Lanes:           lanesForBackendRoute(backends, m.Backends),
			Backends:        backends,
			SelectionReason: ModelRouteExplicitBackendChain,
		}
	}

	if len(m.ProviderLanes) > 0 {
		lanes := make([]ProviderLane, 0, len(m.ProviderLanes))
		var backends []string
		for _, configured := range m.ProviderLanes {
			lane := ProviderLane{
				Provider: strings.TrimSpace(configured.Provider),
				Default:  strings.TrimSpace(configured.Default),
			}
			backends = appendUniqueBackend(backends, lane.Default)
			for _, name := range configured.FallbackBackends {
				name = strings.TrimSpace(name)
				lane.FallbackBackends = append(lane.FallbackBackends, name)
				backends = appendUniqueBackend(backends, name)
			}
			lanes = append(lanes, lane)
		}
		return ModelRoute{Lanes: lanes, Backends: backends, SelectionReason: ModelRouteProviderLanes}
	}

	backend := strings.TrimSpace(m.Default)
	return ModelRoute{
		Lanes:           lanesForBackendRoute([]string{backend}, m.Backends),
		Backends:        appendUniqueBackend(nil, backend),
		SelectionReason: ModelRouteDefaultOnly,
	}
}

// FallbackCandidates returns route entries after current. If current is an
// explicit label/policy backend outside the route, the whole route is eligible.
func (m ModelConfig) FallbackCandidates(current string) []string {
	if len(m.FallbackBackends) > 0 {
		current = strings.TrimSpace(current)
		out := make([]string, 0, len(m.FallbackBackends))
		for _, name := range m.FallbackBackends {
			name = strings.TrimSpace(name)
			if name != "" && name != current {
				out = appendUniqueBackend(out, name)
			}
		}
		return out
	}
	route := m.ResolvedRoute().Backends
	current = strings.TrimSpace(current)
	for i, name := range route {
		if name == current {
			return append([]string(nil), route[i+1:]...)
		}
	}
	out := make([]string, 0, len(route))
	for _, name := range route {
		if name != current {
			out = append(out, name)
		}
	}
	return out
}

// DispatchCandidates returns the same route ordering but wraps after the
// current backend. Fresh dispatch substitution may recover from a blocked
// label/policy pin by returning to the route default after trying later entries;
// outage fallback does not wrap because a live session must only move forward.
func (m ModelConfig) DispatchCandidates(current string) []string {
	if len(m.FallbackBackends) > 0 {
		current = strings.TrimSpace(current)
		var out []string
		if strings.TrimSpace(m.Default) != current {
			out = appendUniqueBackend(out, m.Default)
		}
		for _, name := range m.FallbackBackends {
			if strings.TrimSpace(name) != current {
				out = appendUniqueBackend(out, name)
			}
		}
		return out
	}
	route := m.ResolvedRoute().Backends
	current = strings.TrimSpace(current)
	for i, name := range route {
		if name != current {
			continue
		}
		out := append([]string(nil), route[i+1:]...)
		out = append(out, route[:i]...)
		return out
	}
	return m.FallbackCandidates(current)
}

func appendUniqueBackend(backends []string, name string) []string {
	name = strings.TrimSpace(name)
	if name == "" {
		return backends
	}
	for _, existing := range backends {
		if existing == name {
			return backends
		}
	}
	return append(backends, name)
}

func lanesForBackendRoute(backends []string, defs map[string]BackendDef) []ProviderLane {
	var lanes []ProviderLane
	for _, name := range backends {
		provider := strings.TrimSpace(defs[name].Provider)
		if provider == "" {
			provider = "unspecified"
		}
		if len(lanes) == 0 || !strings.EqualFold(lanes[len(lanes)-1].Provider, provider) {
			lanes = append(lanes, ProviderLane{Provider: provider, Default: name})
			continue
		}
		lanes[len(lanes)-1].FallbackBackends = append(lanes[len(lanes)-1].FallbackBackends, name)
	}
	return lanes
}

func validateProviderLanes(cfg *Config) error {
	// An explicit legacy fallback chain is a project-local route override. Fleet
	// provider lanes may still be present after settings-layer injection, but
	// they are inactive and must not require their backends in this project.
	if cfg == nil || len(cfg.Model.FallbackBackends) > 0 || len(cfg.Model.ProviderLanes) == 0 {
		return nil
	}
	providers := make(map[string]bool, len(cfg.Model.ProviderLanes))
	backends := make(map[string]bool)
	for i := range cfg.Model.ProviderLanes {
		lane := &cfg.Model.ProviderLanes[i]
		lane.Provider = strings.ToLower(strings.TrimSpace(lane.Provider))
		lane.Default = strings.TrimSpace(lane.Default)
		if lane.Provider == "" {
			return fmt.Errorf("config: model.provider_lanes[%d].provider is required", i)
		}
		if providers[lane.Provider] {
			return fmt.Errorf("config: model.provider_lanes repeats provider %q; each provider must have one ordered lane", lane.Provider)
		}
		providers[lane.Provider] = true
		if lane.Default == "" {
			return fmt.Errorf("config: model.provider_lanes[%d].default is required", i)
		}
		laneBackends := append([]string{lane.Default}, lane.FallbackBackends...)
		for j, raw := range laneBackends {
			name := strings.TrimSpace(raw)
			if j > 0 {
				lane.FallbackBackends[j-1] = name
			}
			if name == "" {
				return fmt.Errorf("config: model.provider_lanes[%d] contains an empty backend", i)
			}
			if backends[name] {
				return fmt.Errorf("config: model.provider_lanes backend %q appears more than once; route entries must be unique", name)
			}
			backends[name] = true
			def, ok := cfg.Model.Backends[name]
			if !ok {
				return fmt.Errorf("config: model.provider_lanes references %q which is not defined in model.backends", name)
			}
			if def.NonAgentic {
				return fmt.Errorf("config: model.provider_lanes includes %q which is marked non_agentic; provider lanes may only route workers", name)
			}
			if provider := strings.TrimSpace(def.Provider); provider != "" && !strings.EqualFold(provider, lane.Provider) {
				return fmt.Errorf("config: model.provider_lanes provider %q contains backend %q declared with provider %q", lane.Provider, name, provider)
			}
		}
	}
	return nil
}
