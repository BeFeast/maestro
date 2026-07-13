package state

import (
	"time"

	"github.com/befeast/maestro/internal/progress"
)

// MaterialProgress is the durable per-project stalled-progress watermark plus
// the watchdog's cadence and last decisions (#887).
//
// The Watermark carries the last material-progress identity/time; the deadline
// is never stored — it is always derived from Watermark.At + the silence budget
// so a daemon restart re-reads the same watermark and computes the identical
// deadline (no reset, no duplication). LastDecision is the most recent watchdog
// verdict (progress observed / waiting / stall action); LastRecovery is the
// most recent verdict that actually asked for a recovery action, kept separate
// so Fleet can report "last recovery" without it being overwritten by a routine
// progress tick. All fields are secret-free: fingerprints are non-reversible
// digests and no raw path, secret, or command output is ever persisted.
type MaterialProgress struct {
	Watermark           progress.Watermark `json:"watermark"`
	BudgetSeconds       int                `json:"budget_seconds,omitempty"`        // silence budget in effect at last evaluation (0 = watchdog disabled)
	EvalIntervalSeconds int                `json:"eval_interval_seconds,omitempty"` // watchdog evaluation cadence, distinct from the poll/supervisor cadences
	LastEvaluatedAt     time.Time          `json:"last_evaluated_at,omitempty"`
	LastDecision        *progress.Decision `json:"last_decision,omitempty"`
	LastRecovery        *progress.Decision `json:"last_recovery,omitempty"`
}

// Deadline returns the next stall deadline, or the zero time when the watchdog
// is disabled or no progress has been recorded yet.
func (m *MaterialProgress) Deadline() time.Time {
	if m == nil || m.BudgetSeconds <= 0 {
		return time.Time{}
	}
	return m.Watermark.Deadline(time.Duration(m.BudgetSeconds) * time.Second)
}

// RecordMaterialProgress evaluates the stalled-progress watchdog against the
// observed phase-appropriate signal set and persists the advanced watermark and
// decision onto the project state. It returns the recovery decision for the
// caller to act on under the existing retry budget.
//
// It is safe to call every watchdog tick: progress.Evaluate is idempotent while
// the worker stays frozen (the derived deadline never drifts), so the caller's
// retry budget stays the single "retry exactly once" authority. budget <= 0
// records the watermark for reporting but never asks for a recovery action.
func (s *State) RecordMaterialProgress(observed progress.SignalSet, phase progress.Phase, budget, evalInterval time.Duration, now time.Time) progress.Decision {
	var prev progress.Watermark
	if s.MaterialProgress != nil {
		prev = s.MaterialProgress.Watermark
	}
	wm, dec := progress.Evaluate(prev, observed, phase, budget, now)

	mp := s.MaterialProgress
	if mp == nil {
		mp = &MaterialProgress{}
		s.MaterialProgress = mp
	}
	mp.Watermark = wm
	mp.LastEvaluatedAt = now.UTC()
	if budget > 0 {
		mp.BudgetSeconds = int(budget.Round(time.Second) / time.Second)
	} else {
		mp.BudgetSeconds = 0
	}
	if evalInterval > 0 {
		mp.EvalIntervalSeconds = int(evalInterval.Round(time.Second) / time.Second)
	}
	d := dec
	mp.LastDecision = &d
	if dec.Acted() {
		r := dec
		mp.LastRecovery = &r
	}
	return dec
}

// mergeMaterialProgress resolves concurrent writers of the per-project
// watermark (#887). The watermark time (last material progress) advances only
// forward, so the snapshot with the newer Watermark.At wins — this guarantees a
// concurrent stall record can never clobber a fresh progress advance and reset
// the deadline. Ties break on the later evaluation time. Mirrors mergePaused /
// mergeSpawnDrain's latest-write-wins discipline.
func mergeMaterialProgress(merged, current, ours *State) {
	a, b := current.MaterialProgress, ours.MaterialProgress
	switch {
	case a == nil:
		merged.MaterialProgress = b
	case b == nil:
		merged.MaterialProgress = a
	case b.Watermark.At.After(a.Watermark.At):
		merged.MaterialProgress = b
	case a.Watermark.At.After(b.Watermark.At):
		merged.MaterialProgress = a
	case b.LastEvaluatedAt.After(a.LastEvaluatedAt):
		merged.MaterialProgress = b
	default:
		merged.MaterialProgress = a
	}
}
