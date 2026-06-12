package server

import (
	"sort"
	"time"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/state"
)

// fleetBackendQuota is the per-backend subscription quota position
// surfaced on the fleet snapshot (#704): how full each calibrated
// window is, when it resets, and whether dispatch is currently being
// steered away (pressured). Only backends with quota config emit a row;
// the MC gauge hides itself when the list is empty (legacy servers /
// unconfigured projects).
type fleetBackendQuota struct {
	Backend          string     `json:"backend"`
	WindowCapTokens  int        `json:"window_cap_tokens,omitempty"`
	WindowUsedTokens int        `json:"window_used_tokens"`
	WindowPercent    float64    `json:"window_percent"`
	WindowResetAt    *time.Time `json:"window_reset_at,omitempty"`
	WeeklyCapTokens  int        `json:"weekly_cap_tokens,omitempty"`
	WeekUsedTokens   int        `json:"week_used_tokens"`
	WeekPercent      float64    `json:"week_percent"`
	WeekResetAt      *time.Time `json:"week_reset_at,omitempty"`
	// DispatchThreshold is the used fraction (0..1) above which fresh
	// dispatch prefers fallback backends.
	DispatchThreshold float64 `json:"dispatch_threshold"`
	Pressured         bool    `json:"pressured"`
}

// buildFleetBackendQuota evaluates the quota position of every backend
// with quota config. Evaluation is read-only and window expiry is
// applied on the fly, so the rows stay fresh between orchestrator
// cycles without mutating the loaded state.
func buildFleetBackendQuota(cfg *config.Config, st *state.State, now time.Time) []fleetBackendQuota {
	if cfg == nil {
		return nil
	}
	var out []fleetBackendQuota
	for name, def := range cfg.Model.Backends {
		if !def.Quota.Configured() {
			continue
		}
		var usage *state.BackendQuotaUsage
		if st != nil {
			usage = st.BackendQuotaUsage[name]
		}
		status := state.EvaluateBackendQuota(usage,
			def.Quota.WindowTokens, def.Quota.WeeklyTokens,
			def.Quota.EffectiveDispatchThreshold(), now)
		out = append(out, fleetBackendQuota{
			Backend:           name,
			WindowCapTokens:   status.WindowCapTokens,
			WindowUsedTokens:  status.WindowUsedTokens,
			WindowPercent:     status.WindowPercent,
			WindowResetAt:     status.WindowResetAt,
			WeeklyCapTokens:   status.WeeklyCapTokens,
			WeekUsedTokens:    status.WeekUsedTokens,
			WeekPercent:       status.WeekPercent,
			WeekResetAt:       status.WeekResetAt,
			DispatchThreshold: status.Threshold,
			Pressured:         status.Pressured,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Backend < out[j].Backend })
	return out
}
