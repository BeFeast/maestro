package worker

import (
	"time"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/state"
)

// recordBackendAttribution appends a new BackendAttribution segment to
// sess.Attribution and closes the previous one's EndedAt + EndReason.
// Called from every place that sets sess.Backend (Start, Respawn,
// RespawnInPlace) so the timeline records who actually produced the
// commits across the session lifecycle.
//
// The metadata snapshot (provider/model/variant/effort) comes from the
// matching config.BackendDef; absent fields are kept empty and omit
// from JSON output, so a backend that doesn't declare metadata keeps
// working unchanged.
//
// reason is one of "initial_spawn", "fallover", "in_place_respawn",
// "phase_transition" — names the cause of this segment becoming active.
// previousEndReason is the symmetric label for the segment being closed
// ("provider_limit" for fallover triggered by rate limit, "completed"
// when the worker finished cleanly, "killed" for forced respawn, etc).
//
// If sess is nil or backendName is empty the call is a no-op — defensive
// against the test harness path where sess.Backend is set without going
// through any of the three real entry points.
func recordBackendAttribution(cfg *config.Config, sess *state.Session, backendName, reason, previousEndReason string, now time.Time) {
	if sess == nil || backendName == "" {
		return
	}
	now = now.UTC()

	// Close the previous open segment (if any).
	if n := len(sess.Attribution); n > 0 && sess.Attribution[n-1].EndedAt == nil {
		ts := now
		sess.Attribution[n-1].EndedAt = &ts
		if previousEndReason != "" {
			sess.Attribution[n-1].EndReason = previousEndReason
		}
	}

	seg := state.BackendAttribution{
		Backend:   backendName,
		StartedAt: now,
		Reason:    reason,
	}
	if cfg != nil {
		if def, ok := cfg.Model.Backends[backendName]; ok {
			seg.Provider = def.Provider
			seg.Model = def.Model
			seg.Variant = def.Variant
			seg.Effort = def.Effort
		}
	}
	sess.Attribution = append(sess.Attribution, seg)
}
