package orchestrator

import (
	"fmt"
	"log"
	"time"

	"github.com/befeast/maestro/internal/state"
)

// reconcileBackendQuota maintains the quota-pressure gate (#704). It
// accrues the cycle's session token deltas into the per-backend rolling
// quota windows, then for every backend with quota config compares
// estimated usage against the dispatch threshold:
//
//   - above threshold → a BackendHealth cooldown entry with reason
//     quota_pressure and RetryAfter at the window reset. The existing
//     dispatch gate (#695) and fallback selector then steer fresh
//     dispatches to the next healthy backend with no further wiring,
//     and the supervisor surfaces a backend_quota_pressure finding.
//   - back below threshold (window rolled, or usage recalibrated) → the
//     quota_pressure entry is removed, ending the episode.
//
// A cooldown owned by another failure class (auth_failure,
// provider_limit) is never overwritten or cleared here: those gates
// signal a malfunction and carry their own recovery semantics, while
// quota pressure is merely predictive. Returns true when state changed.
func (o *Orchestrator) reconcileBackendQuota(s *state.State, now time.Time) bool {
	if s == nil || o.cfg == nil {
		return false
	}
	now = now.UTC()
	changed := state.AccrueBackendQuotaUsage(s, now)
	for name, def := range o.cfg.Model.Backends {
		if !def.Quota.Configured() {
			continue
		}
		status := state.EvaluateBackendQuota(s.BackendQuotaUsage[name],
			def.Quota.WindowTokens, def.Quota.WeeklyTokens,
			def.Quota.EffectiveDispatchThreshold(), now)
		existing, exists := s.BackendHealth[name]
		if exists && existing.Reason != state.BackendBlockQuotaPressure {
			continue
		}
		if !status.Pressured {
			if exists {
				delete(s.BackendHealth, name)
				changed = true
				log.Printf("[orch] backend %s quota pressure cleared (window %.0f%%, week %.0f%%) — default dispatch order restored",
					name, status.WindowPercent, status.WeekPercent)
			}
			continue
		}
		health := state.BackendHealth{
			State:      state.BackendHealthCooldown,
			Reason:     state.BackendBlockQuotaPressure,
			Pattern:    backendQuotaPattern(status),
			Since:      now,
			RetryAfter: status.PressureResetAt,
		}
		if exists {
			// Keep the episode anchor stable so the supervisor finding
			// stays "once per pressure episode" while usage fluctuates.
			health.Since = existing.Since
		} else {
			log.Printf("[orch] backend %s quota pressure: %s — fresh dispatches prefer fallback backends%s",
				name, health.Pattern, retryAfterHint(health.RetryAfter))
		}
		if s.BackendHealth == nil {
			s.BackendHealth = make(map[string]state.BackendHealth)
		}
		if !exists || backendQuotaHealthDiffers(existing, health) {
			s.BackendHealth[name] = health
			changed = true
		}
	}
	return changed
}

// backendQuotaPattern renders the usage position into the cooldown's
// Pattern field so the MC pill and supervisor evidence show why the
// backend is gated, e.g. "window 91% / week 34% (threshold 85%)".
func backendQuotaPattern(status state.BackendQuotaStatus) string {
	window := "window n/a"
	if status.WindowCapTokens > 0 {
		window = fmt.Sprintf("window %.0f%%", status.WindowPercent)
	}
	week := "week n/a"
	if status.WeeklyCapTokens > 0 {
		week = fmt.Sprintf("week %.0f%%", status.WeekPercent)
	}
	return fmt.Sprintf("%s / %s (threshold %.0f%%)", window, week, status.Threshold*100)
}

func backendQuotaHealthDiffers(a, b state.BackendHealth) bool {
	if a.Pattern != b.Pattern || !a.Since.Equal(b.Since) {
		return true
	}
	switch {
	case a.RetryAfter == nil && b.RetryAfter == nil:
		return false
	case a.RetryAfter == nil || b.RetryAfter == nil:
		return true
	default:
		return !a.RetryAfter.Equal(*b.RetryAfter)
	}
}
