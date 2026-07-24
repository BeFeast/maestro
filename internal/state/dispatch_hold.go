package state

import (
	"strings"
	"time"
)

const (
	maxDispatchHoldReasonRunes = 96
	maxDispatchHoldDetailRunes = 512
)

const (
	DispatchHoldBlockingOutcomeCheck = "blocking_outcome_check"
	DispatchHoldBackendsCoolingDown  = "all_backends_cooling_down"
	// DispatchHoldOnCooldown: model.hold_on_cooldown is deliberately waiting
	// out the routed backend's quota-cooldown reset instead of cascading work
	// onto the remaining fallback rungs (which may be healthy).
	DispatchHoldOnCooldown       = "hold_on_cooldown"
	DispatchHoldPRGateCapacity   = "capacity_blocked_by_pr_gates"
	DispatchHoldPaused           = "paused"
	DispatchHoldAwaitingDispatch = "awaiting_dispatch"
	DispatchHoldQueueEmpty       = "queue_empty"
)

// DispatchHold is the bounded, command/output-free explanation for why fresh
// issue dispatch is suspended. EvaluatedAt is persistence/merge metadata and
// is deliberately omitted from the Fleet projection, whose public contract is
// only active, reason_class, detail, and since.
type DispatchHold struct {
	Active      bool      `json:"active"`
	ReasonClass string    `json:"reason_class,omitempty"`
	Detail      string    `json:"detail,omitempty"`
	Since       time.Time `json:"since,omitempty"`
	EvaluatedAt time.Time `json:"evaluated_at,omitempty"`
}

// IdleStallState is the durable two-cycle debounce for idle_stall alerts.
// It is internal control-plane state; Fleet exposes DispatchHold, not this
// counter or its notification receipt.
type IdleStallState struct {
	ConsecutiveCycles int       `json:"consecutive_cycles,omitempty"`
	ReasonClass       string    `json:"reason_class,omitempty"`
	Notified          bool      `json:"notified,omitempty"`
	LastEvaluatedAt   time.Time `json:"last_evaluated_at,omitempty"`
}

// RecordDispatchCycle updates the durable hold snapshot and returns true when
// the project has met the idle-stall condition for two consecutive cycles and
// its current reason class has not been notified yet. The caller marks the
// receipt only after the notify layer succeeds.
//
// Idle means 0 live workers for two cycles — including the empty-ready case
// (queue_empty). Requiring readyIssues>0 previously silenced the boom/bust
// failure mode where the ready queue drained under outcome hold and the fleet
// sat at 0 with no alert (2026-07-22).
func (s *State) RecordDispatchCycle(hold DispatchHold, liveWorkers, readyIssues int, now time.Time) bool {
	if s == nil {
		return false
	}
	now = normalizedTime(now)
	hold = normalizeDispatchHold(hold, s.DispatchHold, now)
	s.DispatchHold = hold

	if liveWorkers != 0 {
		s.IdleStall = IdleStallState{LastEvaluatedAt: now}
		return false
	}

	reasonClass := strings.TrimSpace(hold.ReasonClass)
	if reasonClass == "" {
		if readyIssues <= 0 {
			reasonClass = DispatchHoldQueueEmpty
		} else {
			reasonClass = DispatchHoldAwaitingDispatch
		}
	}
	if s.IdleStall.ReasonClass == reasonClass && s.IdleStall.ConsecutiveCycles > 0 {
		s.IdleStall.ConsecutiveCycles++
	} else {
		s.IdleStall = IdleStallState{
			ConsecutiveCycles: 1,
			ReasonClass:       reasonClass,
		}
	}
	s.IdleStall.LastEvaluatedAt = now
	return s.IdleStall.ConsecutiveCycles >= 2 && !s.IdleStall.Notified
}

// MarkIdleStallNotified records the successful alert side effect for the
// current hold class. A changed class or a cleared stall starts a fresh debounce.
func (s *State) MarkIdleStallNotified(reasonClass string) {
	if s == nil || strings.TrimSpace(reasonClass) == "" || s.IdleStall.ReasonClass != strings.TrimSpace(reasonClass) {
		return
	}
	s.IdleStall.Notified = true
}

func normalizeDispatchHold(next, previous DispatchHold, now time.Time) DispatchHold {
	next.ReasonClass = boundedDispatchHoldText(next.ReasonClass, maxDispatchHoldReasonRunes)
	next.Detail = boundedDispatchHoldText(next.Detail, maxDispatchHoldDetailRunes)
	next.EvaluatedAt = now
	if !next.Active {
		return DispatchHold{Active: false, EvaluatedAt: now}
	}
	if previous.Active && previous.ReasonClass == next.ReasonClass && !previous.Since.IsZero() {
		next.Since = previous.Since.UTC()
	} else if next.Since.IsZero() {
		next.Since = now
	} else {
		next.Since = next.Since.UTC()
	}
	return next
}

func boundedDispatchHoldText(value string, limit int) string {
	value = strings.TrimSpace(value)
	if value == "" || limit <= 0 {
		return ""
	}
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	if limit == 1 {
		return "…"
	}
	return strings.TrimSpace(string(runes[:limit-1])) + "…"
}

// mergeDispatchVisibility keeps the snapshot written by the newest completed
// orchestrator cycle when the supervisor saves the same state concurrently.
func mergeDispatchVisibility(current, ours *State) (DispatchHold, IdleStallState) {
	winner := current
	if winner == nil || (ours != nil && dispatchVisibilityUpdatedAt(ours).After(dispatchVisibilityUpdatedAt(winner))) {
		winner = ours
	}
	if winner == nil {
		return DispatchHold{}, IdleStallState{}
	}
	return winner.DispatchHold, winner.IdleStall
}

func dispatchVisibilityUpdatedAt(s *State) time.Time {
	if s == nil {
		return time.Time{}
	}
	if s.IdleStall.LastEvaluatedAt.After(s.DispatchHold.EvaluatedAt) {
		return s.IdleStall.LastEvaluatedAt
	}
	return s.DispatchHold.EvaluatedAt
}
