package daemon

import (
	"sync/atomic"

	"github.com/befeast/maestro/internal/config"
)

// configHolder is the synchronized current-config cell for one project flow
// (#768). The flow's reload pump stores a freshly loaded config on every
// config-store edit; the supervisor loop reads it once per cycle and the
// FleetProject dashboard snapshot is rebuilt from it, so a live edit reaches
// every reader race-free without restarting the flow.
//
// It is backed by an atomic pointer: a reader always observes a complete config
// because the pump swaps a whole pointer, never a half-written field. This is
// the safe alternative to the in-place field mutation #767 had to give the
// orchestrator a private copy to avoid — the supervisor and dashboard share the
// holder's pointer, but nothing mutates the pointed-at config in place
// (supervisor.RunOnce only reads it; the orchestrator still mutates its own
// private shallow copy).
type configHolder struct {
	ptr atomic.Pointer[config.Config]
}

// newConfigHolder seeds the holder with the flow's startup config.
func newConfigHolder(cfg *config.Config) *configHolder {
	h := &configHolder{}
	h.ptr.Store(cfg)
	return h
}

// Load returns the flow's current config. Safe for concurrent use.
func (h *configHolder) Load() *config.Config {
	if h == nil {
		return nil
	}
	return h.ptr.Load()
}

// Store swaps in a newly loaded config. A nil config is ignored so a failed
// reload can never blank out the holder. Safe for concurrent use.
func (h *configHolder) Store(cfg *config.Config) {
	if h == nil || cfg == nil {
		return
	}
	h.ptr.Store(cfg)
}
