package orchestrator

import (
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/befeast/maestro/internal/notify"
	"github.com/befeast/maestro/internal/outcome"
	"github.com/befeast/maestro/internal/state"
	"github.com/befeast/maestro/internal/supervisor"
)

// reconcileDispatchVisibility records the top-level reason fresh issue work
// did not produce a live worker this cycle and emits the durable two-cycle
// idle_stall alert. The reason is synthesized only from closed enums, counts,
// timestamps, and outcome.HealthCheckItem's allow-listed name/status fields;
// raw verifier commands, summaries, details, stdout, and stderr never enter the
// dispatch_hold payload.
func (o *Orchestrator) reconcileDispatchVisibility(s *state.State, now time.Time) {
	if o == nil || o.cfg == nil || s == nil {
		return
	}
	now = now.UTC()
	capacity := s.Capacity(capacityInput(o.cfg))
	readyIssues := dispatchReadyIssueCount(s)
	hold := o.dispatchHoldForCycle(s, capacity, readyIssues, now)
	if !s.RecordDispatchCycle(hold, capacity.LiveWorkers, readyIssues, now) {
		return
	}

	reasonClass := strings.TrimSpace(s.DispatchHold.ReasonClass)
	if reasonClass == "" {
		reasonClass = state.DispatchHoldAwaitingDispatch
	}
	body := fmt.Sprintf(
		"0 live workers with %d ready issue(s) for 2 consecutive cycles; dispatch_hold.reason_class=%s",
		readyIssues,
		reasonClass,
	)
	if detail := strings.TrimSpace(s.DispatchHold.Detail); detail != "" {
		body += "; " + detail
	}
	if o.notifier == nil {
		return
	}
	project := strings.TrimSpace(o.repo)
	if project == "" {
		project = strings.TrimSpace(o.cfg.Repo)
	}
	// A stall whose cause is backend cooldown routes to the dedicated
	// backend_cooldown_exhausted class ("battery"), so the operator can tell a
	// quota wait (self-clears at the stated reset) from a generic idle stall.
	alertClass := notify.AlertIdleStall
	title := "maestro idle stall"
	if reasonClass == state.DispatchHoldBackendsCoolingDown || reasonClass == state.DispatchHoldOnCooldown {
		alertClass = notify.AlertBackendCooldownExhausted
		title = "maestro backend cooldown"
	}
	if project != "" {
		title += ": " + project
	}
	if err := o.notifier.Alert(
		alertClass,
		project+":"+reasonClass,
		title,
		body,
	); err != nil {
		log.Printf("[orch] %s notification failed for %s: %v", alertClass, project, err)
		return
	}
	s.MarkIdleStallNotified(reasonClass)
}

func dispatchReadyIssueCount(s *state.State) int {
	if s == nil {
		return 0
	}
	decision := s.LatestSupervisorDecision()
	if decision == nil || decision.QueueAnalysis == nil {
		return 0
	}
	ready := decision.QueueAnalysis.EligibleCandidates
	if ready < 0 {
		return 0
	}
	return ready
}

func (o *Orchestrator) dispatchHoldForCycle(s *state.State, capacity state.Capacity, readyIssues int, now time.Time) state.DispatchHold {
	if s == nil {
		return state.DispatchHold{}
	}
	if s.PauseActive() {
		return state.DispatchHold{
			Active:      true,
			ReasonClass: state.DispatchHoldPaused,
			Detail:      "project dispatch is paused by an operator",
			Since:       s.PausedAt,
		}
	}
	if capacity.LiveWorkers > 0 {
		return state.DispatchHold{}
	}
	if hold, ok := blockingOutcomeDispatchHold(s); ok {
		return hold
	}
	if readyIssues > 0 {
		if blocked, retryAt := o.allDispatchBackendsCoolingDown(s, now); blocked {
			detail := "all dispatch backends are cooling down"
			if retryAt != nil {
				detail += "; earliest retry at " + retryAt.UTC().Format(time.RFC3339)
			}
			return state.DispatchHold{
				Active:      true,
				ReasonClass: state.DispatchHoldBackendsCoolingDown,
				Detail:      detail,
			}
		}
		if holdUntil, backend, held := o.dispatchHoldOnCooldown(s, now); held {
			return state.DispatchHold{
				Active:      true,
				ReasonClass: state.DispatchHoldOnCooldown,
				Detail: fmt.Sprintf(
					"hold_on_cooldown: routed backend %s cooling down; holding for reset at %s instead of cascading",
					backend,
					holdUntil.UTC().Format(time.RFC3339),
				),
			}
		}
		if capacity.GateBound() {
			return state.DispatchHold{
				Active:      true,
				ReasonClass: state.DispatchHoldPRGateCapacity,
				Detail: fmt.Sprintf(
					"%d PR gate(s) hold all worker capacity while %d ready issue(s) wait",
					capacity.PRGates,
					readyIssues,
				),
			}
		}
		if capacity.AvailableSlots > 0 {
			return state.DispatchHold{
				Active:      true,
				ReasonClass: state.DispatchHoldAwaitingDispatch,
				Detail:      "ready work exists and worker capacity is free, but no worker started this cycle",
			}
		}
	}
	// Empty ready with 0 live workers is still an idle stall — not a healthy
	// quiet fleet. Surface it so operators get notified instead of discovering
	// 0 workers by hand (F8 companion, 2026-07-22).
	return state.DispatchHold{
		Active:      true,
		ReasonClass: state.DispatchHoldQueueEmpty,
		Detail:      "no live workers and no eligible ready issues to dispatch",
	}
}

func blockingOutcomeDispatchHold(s *state.State) (state.DispatchHold, bool) {
	if s == nil || s.OutcomeHealth == nil || s.OutcomeHealth.State != outcome.HealthFailing {
		return state.DispatchHold{}, false
	}
	decision := s.LatestSupervisorDecision()
	heldByDecision := decision != nil && !decision.RecommendationDropped() && decision.RecommendedAction == supervisor.ActionCheckOutcomeHealth
	heldByCodeLanded := false
	for _, sess := range s.Sessions {
		if sess != nil && sess.Status == state.StatusCodeLanded && !sess.ReleasedForRedispatch {
			heldByCodeLanded = true
			break
		}
	}
	if !heldByDecision && !heldByCodeLanded {
		return state.DispatchHold{}, false
	}

	detail := blockingOutcomeCheckDetail(s.OutcomeHealth.Checks)
	return state.DispatchHold{
		Active:      true,
		ReasonClass: state.DispatchHoldBlockingOutcomeCheck,
		Detail:      detail,
		Since:       s.OutcomeHealth.CheckedAt,
	}, true
}

func blockingOutcomeCheckDetail(checks []outcome.HealthCheckItem) string {
	for _, statuses := range [][]string{{"fail", "failing", "error"}, {"pending", "in_progress", "queued", "unknown", "warning"}} {
		for _, check := range checks {
			if !check.Blocking || !containsFold(statuses, check.Status) {
				continue
			}
			name := strings.TrimSpace(check.Name)
			status := strings.TrimSpace(check.Status)
			if name == "" {
				name = "structured-check"
			}
			if status == "" {
				status = outcome.HealthFailing
			}
			return fmt.Sprintf("blocking outcome check %s is %s", name, status)
		}
	}
	return "blocking outcome verifier is failing"
}

func containsFold(values []string, want string) bool {
	want = strings.TrimSpace(want)
	for _, value := range values {
		if strings.EqualFold(value, want) {
			return true
		}
	}
	return false
}

// dispatchHoldOnCooldown mirrors resolveDispatchBackend's hold gate for the
// route default so the dashboard names the deliberate wait: the routed backend
// is in a holdable quota cooldown whose reset lands inside the
// model.hold_on_cooldown window, so fresh dispatch waits instead of cascading.
// Per-issue label pins can route elsewhere; the route default is the fleet's
// dispatch surface and is what the hold visibly parks.
func (o *Orchestrator) dispatchHoldOnCooldown(s *state.State, now time.Time) (*time.Time, string, bool) {
	if o == nil || o.cfg == nil || !o.cfg.Model.HoldOnCooldown.Enabled {
		return nil, "", false
	}
	backend := o.cfg.Model.EffectiveDefault()
	if backend == "" {
		return nil, "", false
	}
	blockedBy, blockedRetry := o.dispatchBackendBlock(s, backend, "", now)
	if blockedBy == "" {
		return nil, "", false
	}
	holdUntil, held := o.holdOnCooldownUntil(blockedBy, blockedRetry, now)
	if !held {
		return nil, "", false
	}
	return holdUntil, backend, true
}

// allDispatchBackendsCoolingDown mirrors the selector's real candidate set and
// dispatchBackendBlock checks. Disabled/unknown configuration is deliberately
// not labeled as cooldown; that falls through to the generic awaiting_dispatch
// stall instead of presenting a false recovery clock.
func (o *Orchestrator) allDispatchBackendsCoolingDown(s *state.State, now time.Time) (bool, *time.Time) {
	if o == nil || o.cfg == nil {
		return false, nil
	}
	candidates := o.dispatchBackendCandidates("")
	if len(candidates) == 0 {
		return false, nil
	}
	var earliest *time.Time
	for _, backend := range candidates {
		blockedBy, retryAt := o.dispatchBackendBlock(s, backend, "", now)
		if blockedBy == "" {
			return false, nil
		}
		if blockedBy == state.BackendBlockUnknown || blockedBy == state.BackendBlockDisabled {
			return false, nil
		}
		if retryAt != nil && (earliest == nil || retryAt.Before(*earliest)) {
			at := retryAt.UTC()
			earliest = &at
		}
	}
	return true, earliest
}
