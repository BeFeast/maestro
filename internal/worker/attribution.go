package worker

import (
	"time"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/state"
)

// beginSessionAttempt resets fields whose meaning is scoped to the currently
// executing backend process while preserving issue/worktree identity and the
// cumulative attribution/token history. Recovery and phase transitions reuse
// the same state.Session, so leaving a previous attempt's terminal timestamp or
// self-reported model behind makes a live replacement look finished (and can
// label a Sol worker as Fable in Mission Control).
func beginSessionAttempt(cfg *config.Config, sess *state.Session, backendName, reason, previousEndReason string, now time.Time) {
	if sess == nil {
		return
	}
	now = now.UTC()
	sess.WorkerGeneration++
	sess.StartedAt = now
	sess.FinishedAt = nil
	sess.WorkerEndedAt = nil
	sess.Status = state.StatusRunning
	sess.Backend = backendName
	sess.Model = ""
	sess.CostUSDBackend = 0
	sess.UsageTokensWatermark = 0
	sess.TokensUsedAttempt = 0
	sess.WorkerOutcome = ""
	// A scheduled retry owns NextRetryAt only until a replacement process has
	// actually started. Keeping the elapsed timestamp on a Running attempt makes
	// Fleet report contradictory queued/running state and lets a later terminal
	// transition accidentally inherit an already-due retry.
	sess.NextRetryAt = nil
	recordBackendAttribution(cfg, sess, backendName, reason, previousEndReason, now)
}

// AdoptLiveRuntime repairs the persisted runtime projection after a worker
// process was started successfully but the state write that followed lost a
// concurrent compare-and-merge race. The caller must first prove the exact
// tmux session, pane PID, and worktree identity; this function deliberately
// performs no process discovery of its own.
//
// Adoption starts a new observable attempt at the time Maestro recovered
// ownership. That is an honest lower bound for worker_runtime and avoids
// reusing terminal timestamps/token watermarks from the attempt whose state
// was stranded. Issue, worktree, branch, and PR identity stay unchanged.
func AdoptLiveRuntime(cfg *config.Config, sess *state.Session, pid int, tmuxName string, observedAt time.Time) {
	if sess == nil || pid <= 0 || tmuxName == "" {
		return
	}
	beginSessionAttempt(cfg, sess, sess.Backend, "runtime_adoption", "runtime_state_lost", observedAt)
	sess.PID = pid
	sess.TmuxSession = tmuxName
	sess.NextRetryAt = nil
	sess.RestartCheckpointAt = nil
	sess.LastNotifiedStatus = ""
	sess.LastOutputHash = ""
	sess.LastOutputChangedAt = time.Time{}
}

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
