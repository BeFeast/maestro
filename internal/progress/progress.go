// Package progress models a durable, multi-signal material-progress watermark
// and the stalled-progress watchdog that evaluates it (#887).
//
// The legacy `worker_silent_timeout_minutes` watched terminal output only, so
// it could kill a worker that was actively editing files without emitting
// output, and when disabled (0/absent) it could not recover a genuinely
// hung-but-alive worker. This package replaces that single-signal heuristic
// with one durable watermark per exact live worker, PR gate, post-merge
// verification, or delivery lease,
// derived from whichever phase-appropriate signals are present:
//
//   - issue/session state and lease identity;
//   - process and exact tmux/session identity;
//   - terminal output or checkpoint advancement;
//   - bounded worktree evidence (Git HEAD/index/status/diff identity),
//     excluding known volatile/generated paths;
//   - PR head, CI/check/review state, merge/release identity;
//   - delivery approval generation, execution lease, and terminal receipt.
//
// A single missing or stale signal is never proof of a stall: the watermark
// advances whenever any fingerprint or lifecycle phase advances. A signal
// disappearing is not progress and cannot re-arm the deadline. Only
// when *no* signal has advanced for the whole silence budget does the watchdog
// recommend recovery, and even then the boundary depends on lifecycle phase —
// a safe pre-delivery stall may stop+retry that exact worker exactly once,
// while an uncertain delivery lease is handed to operator reconciliation and
// never replayed automatically.
//
// It is a pure leaf package (standard library only) so both the durable state
// layer and the supervisor/orchestrator loops can share one evaluation without
// an import cycle. Fingerprints are non-reversible digests, so no raw path,
// secret, or command output is ever persisted through this package.
package progress

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"
)

// DefaultMaxSilence is the default maximum silence budget for a new hands-off
// project (#887): 20 minutes with no material progress across any lifecycle
// signal before the watchdog recommends an action.
const DefaultMaxSilence = 20 * time.Minute

// ContractVersion names the fingerprint-bound capability that may be published
// only after runtime canary evidence proves the multi-signal watchdog. Maestro's
// own genesis does not auto-publish it; until then the capability stays a visible
// promotion blocker (#887).
const ContractVersion = "multi-signal-progress-v1"

// SignalKind identifies one phase-appropriate progress signal. The watchdog
// derives one combined watermark from whichever kinds are present in a tick;
// it never treats a single stale kind as proof of a stall.
type SignalKind string

const (
	// SignalIssueSession covers issue/session state and lease identity.
	SignalIssueSession SignalKind = "issue_session"
	// SignalProcessTmux covers the process and exact tmux/session identity.
	SignalProcessTmux SignalKind = "process_tmux"
	// SignalTerminalCheckpoint covers terminal output or checkpoint advancement.
	SignalTerminalCheckpoint SignalKind = "terminal_checkpoint"
	// SignalWorktreeGit covers bounded worktree evidence: Git HEAD/index/
	// status/diff identity and relevant source-file changes, excluding known
	// volatile/generated paths.
	SignalWorktreeGit SignalKind = "worktree_git"
	// SignalPRReview covers PR head, CI/check/review state, and merge/release
	// identity.
	SignalPRReview SignalKind = "pr_review"
	// SignalOutcomeVerification covers durable post-merge state and semantic
	// runtime/outcome verification changes. Poll timestamps alone are excluded
	// so an unchanged failing check cannot mask a stalled live-verification gate.
	SignalOutcomeVerification SignalKind = "outcome_verification"
	// SignalDelivery covers delivery approval generation, execution lease, and
	// terminal receipt.
	SignalDelivery SignalKind = "delivery"
)

// signalOrder is the stable iteration order for signals so a combined identity
// digest is deterministic regardless of the caller's insertion order.
var signalOrder = map[SignalKind]int{
	SignalIssueSession:        0,
	SignalProcessTmux:         1,
	SignalTerminalCheckpoint:  2,
	SignalWorktreeGit:         3,
	SignalPRReview:            4,
	SignalOutcomeVerification: 5,
	SignalDelivery:            6,
}

// Fingerprint reduces raw identity material to a short, stable, non-reversible
// digest. Callers pass raw identity (a Git HEAD SHA, a tmux session name, a PR
// head + check conclusion, a delivery lease id, …) and persist only the digest,
// so no raw path, secret, or command output survives into state. When every
// part is empty the fingerprint is empty, marking the signal absent.
func Fingerprint(parts ...string) string {
	present := false
	for _, p := range parts {
		if strings.TrimSpace(p) != "" {
			present = true
			break
		}
	}
	if !present {
		return ""
	}
	h := sha256.New()
	for _, p := range parts {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// Signal is one observed progress signal. Fingerprint is an opaque digest (see
// Fingerprint); an empty Fingerprint means the signal was not observable this
// tick, which is not itself evidence of a stall. Collectors may leave
// ObservedAt zero: Evaluate stamps the durable value only when the fingerprint
// actually changes and ignores synthetic per-poll timestamps.
type Signal struct {
	Kind        SignalKind `json:"kind"`
	Fingerprint string     `json:"fingerprint,omitempty"`
	ObservedAt  time.Time  `json:"observed_at,omitempty"`
}

// SignalSet is the phase-appropriate set of signals observed in one evaluation.
type SignalSet []Signal

// reconcileSignals updates the durable last-known fingerprint per signal kind.
// ObservedAt is stamped by the evaluator only when that fingerprint genuinely
// changes; caller-provided timestamps are deliberately ignored because a
// collector commonly observes every signal at `now`, which would otherwise
// make stale evidence look fresh on every tick.
//
// A temporarily missing signal remains in the last-known set. Disappearance is
// loss of observability, not material progress, and repeated disappear/reappear
// cycles with the same fingerprint therefore cannot keep re-arming a deadline.
// A signal that reappears with a different fingerprint does advance normally.
func reconcileSignals(prev, present []Signal, now time.Time) ([]Signal, bool) {
	known := make(map[SignalKind]Signal, len(prev)+len(present))
	for _, sig := range prev {
		if strings.TrimSpace(sig.Fingerprint) != "" {
			known[sig.Kind] = sig
		}
	}
	changed := false
	for _, sig := range present {
		prior, ok := known[sig.Kind]
		if ok && prior.Fingerprint == sig.Fingerprint {
			continue
		}
		sig.ObservedAt = now
		known[sig.Kind] = sig
		changed = true
	}
	out := make([]Signal, 0, len(known))
	for _, sig := range known {
		out = append(out, sig)
	}
	sort.Slice(out, func(i, j int) bool {
		oi, oj := signalOrder[out[i].Kind], signalOrder[out[j].Kind]
		if oi != oj {
			return oi < oj
		}
		return out[i].Kind < out[j].Kind
	})
	return out, changed
}

// Present returns the signals with a non-empty fingerprint, de-duplicated by
// kind (last write wins) and sorted by the stable signal order.
func (s SignalSet) Present() []Signal {
	byKind := make(map[SignalKind]Signal, len(s))
	for _, sig := range s {
		if strings.TrimSpace(sig.Fingerprint) == "" {
			continue
		}
		byKind[sig.Kind] = sig
	}
	out := make([]Signal, 0, len(byKind))
	for _, sig := range byKind {
		out = append(out, sig)
	}
	sort.Slice(out, func(i, j int) bool {
		oi, oj := signalOrder[out[i].Kind], signalOrder[out[j].Kind]
		if oi != oj {
			return oi < oj
		}
		return out[i].Kind < out[j].Kind
	})
	return out
}

// Kinds returns the present signal kinds in stable order. Safe to record in a
// decision log: it names which evidence was available without any fingerprint.
func (s SignalSet) Kinds() []SignalKind {
	present := s.Present()
	kinds := make([]SignalKind, len(present))
	for i, sig := range present {
		kinds[i] = sig.Kind
	}
	return kinds
}

// Combined reduces the present signals to one stable identity digest. Two ticks
// with the same present (kind,fingerprint) pairs yield the same identity; any
// signal advancing — or the observed set gaining/losing a kind — changes it.
// An empty set (no observable signal at all) yields the empty identity.
func (s SignalSet) Combined() string {
	present := s.Present()
	if len(present) == 0 {
		return ""
	}
	h := sha256.New()
	for _, sig := range present {
		fmt.Fprintf(h, "%s=%s\x00", sig.Kind, sig.Fingerprint)
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// TargetKind identifies the exact independently-evaluated lifecycle target.
// A live worker, a PR gate, a post-merge verification, and a delivery lease
// never share a watermark: work by one target therefore cannot hide a stall in
// another target (#887).
type TargetKind string

const (
	TargetWorker    TargetKind = "worker"
	TargetPRGate    TargetKind = "pr_gate"
	TargetPostMerge TargetKind = "post_merge"
	TargetDelivery  TargetKind = "delivery"
)

// Target is the durable, exact recovery boundary for one watchdog watermark.
// Worker targets bind issue + slot + session + tmux/process + lease identity;
// PR gates and post-merge verification targets intentionally carry no process
// identity; delivery targets bind the durable approval id/generation as
// LeaseID. No command, output, path, or secret belongs in a Target.
type Target struct {
	Kind        TargetKind `json:"kind"`
	IssueNumber int        `json:"issue_number,omitempty"`
	Slot        string     `json:"slot,omitempty"`
	SessionID   string     `json:"session_id,omitempty"`
	TmuxSession string     `json:"tmux_session,omitempty"`
	ProcessID   int        `json:"process_id,omitempty"`
	LeaseID     string     `json:"lease_id"`
}

// Validate rejects ambiguous recovery targets. A worker must be addressable by
// issue, slot, session, process, and durable lease. Tmux is optional because a
// configured worker may run without tmux; when present it remains part of the
// exact identity. PR gates have no live process. Delivery targets are keyed only
// by their durable approval/generation lease.
func (t Target) Validate() error {
	t.Slot = strings.TrimSpace(t.Slot)
	t.SessionID = strings.TrimSpace(t.SessionID)
	t.TmuxSession = strings.TrimSpace(t.TmuxSession)
	t.LeaseID = strings.TrimSpace(t.LeaseID)
	switch t.Kind {
	case TargetWorker:
		if t.IssueNumber <= 0 || t.Slot == "" || t.SessionID == "" || t.ProcessID <= 0 || t.LeaseID == "" {
			return fmt.Errorf("worker target requires issue, slot, session, process, and lease identity")
		}
	case TargetPRGate:
		if t.IssueNumber <= 0 || t.Slot == "" || t.SessionID == "" || t.LeaseID == "" {
			return fmt.Errorf("PR-gate target requires issue, slot, session, and lease identity")
		}
		if t.ProcessID != 0 || t.TmuxSession != "" {
			return fmt.Errorf("PR-gate target must not carry process or tmux identity")
		}
	case TargetPostMerge:
		if t.IssueNumber <= 0 || t.Slot == "" || t.SessionID == "" || t.LeaseID == "" {
			return fmt.Errorf("post-merge target requires issue, slot, session, and lease identity")
		}
		if t.ProcessID != 0 || t.TmuxSession != "" {
			return fmt.Errorf("post-merge target must not carry process or tmux identity")
		}
	case TargetDelivery:
		if t.LeaseID == "" {
			return fmt.Errorf("delivery target requires a durable lease identity")
		}
		if t.ProcessID != 0 || t.TmuxSession != "" {
			return fmt.Errorf("delivery target must not carry process or tmux identity")
		}
	default:
		return fmt.Errorf("unknown progress target kind %q", t.Kind)
	}
	return nil
}

// Key returns a stable, non-reversible map key for this exact target, or an
// empty string when the target is invalid. The full exact Target is stored next
// to the record so a later actuator does not have to reverse the digest.
func (t Target) Key() string {
	if err := t.Validate(); err != nil {
		return ""
	}
	return string(t.Kind) + ":" + Fingerprint(
		fmt.Sprintf("%d", t.IssueNumber), strings.TrimSpace(t.Slot),
		strings.TrimSpace(t.SessionID), strings.TrimSpace(t.TmuxSession),
		fmt.Sprintf("%d", t.ProcessID), strings.TrimSpace(t.LeaseID),
	)
}

// Observation is one exact target plus only that target's phase-appropriate
// signals. Collectors return one Observation per live worker/PR gate/post-merge
// verification/delivery lease and no observation for an idle project.
type Observation struct {
	Target             Target       `json:"target"`
	Signals            SignalSet    `json:"signals,omitempty"`
	Phase              Phase        `json:"phase,omitempty"`
	Incomplete         bool         `json:"incomplete,omitempty"`
	UnavailableSignals []SignalKind `json:"unavailable_signals,omitempty"`
}

// Phase is the lifecycle phase used to pick the recovery boundary when a stall
// is proven. The boundary is deliberately asymmetric: pre-delivery work may be
// safely retried, but a durable delivery lease must never be replayed on an
// uncertain result.
type Phase string

const (
	// PhaseUnknown is the zero phase; treated as pre-delivery for recovery.
	PhaseUnknown Phase = ""
	// PhasePreDelivery covers an exact live implementation worker:
	// a proven stall here may stop the exact live worker and retry once.
	PhasePreDelivery Phase = "pre_delivery"
	// PhasePRGate covers an open PR waiting on CI/review/merge. There is no live
	// worker process to stop, so an overdue gate is surfaced for reconciliation.
	PhasePRGate Phase = "pr_gate"
	// PhasePostMergeVerification covers code that is durably landed but has not
	// yet reached the configured runtime/live-verification outcome. The merge
	// cannot be replayed and there is no worker process to retry.
	PhasePostMergeVerification Phase = "post_merge_verification"
	// PhaseDeliveryPending is an approval-gated delivery not yet executing:
	// there is no live worker to retry, so a stall is an operator wait.
	PhaseDeliveryPending Phase = "delivery_pending"
	// PhaseDeliveryExecuting is an in-flight delivery lease whose result is
	// uncertain. Recovery authority ends before this boundary: never replay,
	// surface operator reconciliation (#872).
	PhaseDeliveryExecuting Phase = "delivery_executing"
	// PhaseDelivered is a terminal receipt: nothing to recover.
	PhaseDelivered Phase = "delivered"
)

// AllowsWorkerRetry reports whether a proven stall in this phase may stop and
// retry the single worker. Only pre-delivery phases do; every delivery phase
// hands off to operator reconciliation instead of replaying a runner.
func (p Phase) AllowsWorkerRetry() bool {
	switch p {
	case PhasePreDelivery, PhaseUnknown:
		return true
	default:
		return false
	}
}

// RequiresReconciliation reports whether a proven stall in this phase must be
// surfaced to an operator rather than acted on automatically, because the
// result crosses the durable delivery lease / replay boundary.
func (p Phase) RequiresReconciliation() bool {
	return p == PhaseDeliveryExecuting
}

// Watermark is the durable last-material-progress record. It survives daemon
// restart in state.json; the deadline is always derived (At + budget) so a
// reload never resets or duplicates it.
type Watermark struct {
	Identity string    `json:"identity,omitempty"` // combined signal digest
	At       time.Time `json:"at,omitempty"`       // when Identity was first observed
	Phase    Phase     `json:"phase,omitempty"`
	Signals  []Signal  `json:"signals,omitempty"` // the present set that produced Identity (fingerprints only)
}

// IsZero reports whether the watermark has never advanced.
func (w Watermark) IsZero() bool {
	return w.Identity == "" && w.At.IsZero()
}

// Deadline returns the next stall deadline for the given silence budget, or the
// zero time when the watchdog is disabled (budget <= 0) or no progress has been
// recorded yet. Deriving the deadline instead of storing it is what lets the
// watermark survive a restart without the deadline resetting or duplicating.
func (w Watermark) Deadline(budget time.Duration) time.Time {
	if budget <= 0 || w.At.IsZero() {
		return time.Time{}
	}
	return w.At.Add(budget)
}

// Action is the evaluator's recommendation/no-op for one tick. It never claims
// an actual recovery attempt; attempts are persisted separately as Recovery.
type Action string

const (
	// ActionNone means material progress was observed and the watermark
	// advanced; there is nothing to recover.
	ActionNone Action = "none"
	// ActionWaiting means no new progress yet, but the silence budget has not
	// been exhausted, so the watchdog waits.
	ActionWaiting Action = "waiting"
	// ActionStopAndRetry means a proven safe pre-delivery stall: stop the
	// exact stale worker and retry/resume exactly once under the existing
	// retry budget. The caller enforces the "exactly once" via that budget;
	// Evaluate is idempotent and keeps returning this until progress advances.
	ActionStopAndRetry Action = "stop_and_retry"
	// ActionSurfaceGateRepair means an open PR gate is overdue. There is no
	// worker process or delivery lease to replay: surface a gate repair/no-op
	// recommendation without claiming the durable-delivery replay boundary.
	ActionSurfaceGateRepair Action = "surface_gate_repair"
	// ActionSurfaceOutcomeRepair means code is already merged but the
	// post-merge/live-verification gate stopped advancing. It surfaces repair
	// without claiming that an uncertain delivery lease was crossed.
	ActionSurfaceOutcomeRepair Action = "surface_outcome_repair"
	// ActionSurfaceDeliveryWait means a delivery approval exists but execution
	// has not begun. The approval is a control-plane wait, not an uncertain
	// execution result and therefore not a replay boundary.
	ActionSurfaceDeliveryWait Action = "surface_delivery_wait"
	// ActionSurfaceReconciliation means the stall crossed the durable delivery
	// lease / replay boundary or occurred in a non-retryable phase: hand it to
	// operator reconciliation and never replay automatically.
	ActionSurfaceReconciliation Action = "surface_reconciliation"
	// ActionEvidenceUnavailable means one or more required bounded probes failed
	// or overflowed. The old deadline remains armed, but destructive recovery is
	// suppressed until a complete observation is available.
	ActionEvidenceUnavailable Action = "evidence_unavailable"
	// ActionDisabled means the watchdog is off (budget <= 0). Kept distinct
	// from ActionWaiting so Fleet can report "disabled" truthfully rather than
	// implying a live-but-quiet deadline.
	ActionDisabled Action = "disabled"
)

// Decision is the durable, secret-free evaluation record for one watchdog tick.
// It captures the observed signal set (by kind only), the last material
// watermark, the derived deadline, the phase, the idempotency/replay boundary,
// and the action plus its no-op reason — never a secret, raw private path, or
// command output (#887).
type Decision struct {
	EvaluatedAt           time.Time    `json:"evaluated_at"`
	Target                Target       `json:"target"`
	Action                Action       `json:"action"`
	Reason                string       `json:"reason"`
	Phase                 Phase        `json:"phase,omitempty"`
	Identity              string       `json:"identity,omitempty"`     // watermark identity at decision time
	WatermarkAt           time.Time    `json:"watermark_at,omitempty"` // last material progress time
	Deadline              time.Time    `json:"deadline,omitempty"`     // next stall deadline (derived)
	BudgetSeconds         int          `json:"budget_seconds,omitempty"`
	ObservedSignals       []SignalKind `json:"observed_signals,omitempty"`
	ObservationIncomplete bool         `json:"observation_incomplete,omitempty"`
	UnavailableSignals    []SignalKind `json:"unavailable_signals,omitempty"`
	// ReplayBoundary is true only when an executing/uncertain durable delivery
	// lease bars automatic replay. A pending approval, PR gate, or post-merge
	// verification wait is not represented as an uncertain execution result.
	ReplayBoundary bool `json:"replay_boundary,omitempty"`
	// RecommendationID is a deterministic idempotency key for one overdue
	// target/watermark/action episode. Repeated evaluations retain the same id;
	// an actuator records at most one recovery attempt for it.
	RecommendationID string    `json:"recommendation_id,omitempty"`
	RecommendedAt    time.Time `json:"recommended_at,omitempty"`
}

// Acted is kept for source compatibility. It reports a recommendation, not an
// actual attempt. New code should call RecommendsRecovery.
// Deprecated: use RecommendsRecovery.
func (d Decision) Acted() bool {
	return d.RecommendsRecovery()
}

// RecommendsRecovery reports that evaluation recommends an action. It does not
// claim the action was attempted; actual attempts are separate Recovery values.
func (d Decision) RecommendsRecovery() bool {
	return d.Action == ActionStopAndRetry || d.Action == ActionSurfaceGateRepair ||
		d.Action == ActionSurfaceOutcomeRepair || d.Action == ActionSurfaceDeliveryWait ||
		d.Action == ActionSurfaceReconciliation
}

// RecoveryOutcome is the durable result of an actual recovery attempt. A
// recommendation alone never creates one of these records.
type RecoveryOutcome string

const (
	RecoveryAttempted RecoveryOutcome = "attempted"
	RecoverySucceeded RecoveryOutcome = "succeeded"
	RecoveryFailed    RecoveryOutcome = "failed"
)

// Recovery records actual actuation separately from the evaluator's verdict
// and recommendation. RecommendationID is the idempotency key: one overdue
// episode may have at most one attempt, later completed with success/failure.
type Recovery struct {
	RecommendationID string          `json:"recommendation_id"`
	Target           Target          `json:"target"`
	Action           Action          `json:"action"`
	Outcome          RecoveryOutcome `json:"outcome"`
	AttemptedAt      time.Time       `json:"attempted_at"`
	CompletedAt      time.Time       `json:"completed_at,omitempty"`
}

// Evaluate advances the watermark and returns the recovery decision for one
// watchdog tick.
//
//   - prev is the durable watermark loaded from state (zero on the first tick).
//   - observed is the phase-appropriate signal set collected this tick.
//   - phase is the current lifecycle phase (picks the recovery boundary).
//   - budget is the configured silence budget; budget <= 0 disables the
//     watchdog (ActionDisabled) so no quiet worker is ever killed by a misread.
//   - now is the injected clock, so callers can drive deterministic tests.
//
// Evaluate never mutates prev. The returned Watermark is the new durable value
// to persist: it equals prev whenever no progress was observed, so re-persisting
// it across restarts never resets or duplicates the deadline.
func Evaluate(prev Watermark, observed SignalSet, phase Phase, budget time.Duration, now time.Time) (Watermark, Decision) {
	return evaluate(Target{}, prev, observed, phase, budget, now, false, nil)
}

// EvaluateTarget is Evaluate bound to one exact worker/PR-gate/delivery target.
// The target is copied into the decision and binds any recovery recommendation
// to an exact, independently-watermarked recovery boundary.
func EvaluateTarget(target Target, prev Watermark, observed SignalSet, phase Phase, budget time.Duration, now time.Time) (Watermark, Decision) {
	return evaluate(target, prev, observed, phase, budget, now, false, nil)
}

// EvaluateObservation evaluates one collector observation, including whether a
// bounded evidence probe was incomplete. Incomplete evidence can still carry
// genuine progress from another signal, but can never authorize a destructive
// recommendation at the deadline.
func EvaluateObservation(prev Watermark, observation Observation, budget time.Duration, now time.Time) (Watermark, Decision) {
	return evaluate(observation.Target, prev, observation.Signals, observation.Phase, budget, now,
		observation.Incomplete, observation.UnavailableSignals)
}

func evaluate(target Target, prev Watermark, observed SignalSet, phase Phase, budget time.Duration, now time.Time, incomplete bool, unavailable []SignalKind) (Watermark, Decision) {
	now = now.UTC()
	// Reconcile against durable last-known fingerprints. The evaluator stamps
	// actual detected changes; signal disappearance alone is not progress.
	present, signalChanged := reconcileSignals(prev.Signals, observed.Present(), now)
	id := SignalSet(present).Combined()
	dec := Decision{
		EvaluatedAt:           now,
		Target:                target,
		Phase:                 phase,
		ObservedSignals:       observed.Kinds(),
		ObservationIncomplete: incomplete,
		UnavailableSignals:    stableSignalKinds(unavailable),
	}
	if budget > 0 {
		dec.BudgetSeconds = int(budget.Round(time.Second).Seconds())
	}

	// Disabled watchdog: record the identity for reporting but never act.
	if budget <= 0 {
		if prev.IsZero() && id != "" {
			prev = Watermark{Identity: id, At: now, Phase: phase, Signals: present}
		}
		dec.Action = ActionDisabled
		dec.Reason = "stalled-progress watchdog disabled (no silence budget)"
		dec.Identity = prev.Identity
		dec.WatermarkAt = prev.At
		return prev, dec
	}

	// First observation, or material progress since the last watermark: the
	// combined identity changed because a signal advanced or the observed set
	// gained/lost a kind. A single stale signal cannot land here while any
	// other signal is still advancing.
	if prev.IsZero() || signalChanged || id != prev.Identity || phase != prev.Phase {
		next := Watermark{Identity: id, At: now, Phase: phase, Signals: present}
		dec.Action = ActionNone
		dec.Reason = "material progress observed; watermark advanced"
		dec.Identity = next.Identity
		dec.WatermarkAt = next.At
		if phase == PhaseDelivered {
			dec.Reason = "delivery reached a terminal receipt; no recovery is required"
		} else {
			dec.Deadline = next.Deadline(budget)
		}
		return next, dec
	}

	// No material progress since prev.At. The deadline is derived from the
	// durable watermark, so it is identical after a daemon restart.
	deadline := prev.Deadline(budget)
	dec.Identity = prev.Identity
	dec.WatermarkAt = prev.At
	dec.Deadline = deadline
	if phase == PhaseDelivered {
		dec.Action = ActionNone
		dec.Reason = "delivery reached a terminal receipt; no recovery is required"
		dec.Deadline = time.Time{}
		return prev, dec
	}
	if incomplete {
		dec.Action = ActionEvidenceUnavailable
		dec.Reason = "required material-progress evidence is unavailable; preserve the existing watermark/deadline and suppress recovery until a complete observation"
		return prev, dec
	}

	if now.Before(deadline) {
		dec.Action = ActionWaiting
		dec.Reason = "no new material progress, still inside the silence budget"
		return prev, dec
	}

	// Silence budget exhausted → proven stall. The recovery boundary depends on
	// the phase. prev is returned unchanged (the deadline does NOT reset), so
	// Evaluate stays idempotent and the caller's retry budget is the single
	// authority for "retry exactly once".
	switch {
	case phase == PhasePostMergeVerification:
		dec.Action = ActionSurfaceOutcomeRepair
		dec.Reason = "code is already merged but runtime/live verification has not advanced; surface outcome repair without replaying the worker or claiming an uncertain delivery result"
	case phase.RequiresReconciliation():
		dec.Action = ActionSurfaceReconciliation
		dec.ReplayBoundary = true
		dec.Reason = "delivery lease is executing/uncertain; recovery authority ends before the durable delivery lease — surface operator reconciliation, never replay"
	case phase == PhaseDeliveryPending:
		dec.Action = ActionSurfaceDeliveryWait
		dec.Reason = "delivery approval is pending and execution has not begun; surface the control-plane wait without claiming an uncertain delivery result"
	case phase.AllowsWorkerRetry():
		dec.Action = ActionStopAndRetry
		dec.Reason = "proven pre-delivery stall past the silence budget; stop the single stale worker and retry once under the existing retry budget"
	case phase == PhasePRGate:
		dec.Action = ActionSurfaceGateRepair
		dec.Reason = "PR gate is overdue; surface an idempotent gate repair/no-op recommendation without replaying a worker or delivery"
	default:
		dec.Action = ActionSurfaceReconciliation
		dec.Reason = "stall past the silence budget in a non-retryable phase; surface operator reconciliation"
	}
	if dec.RecommendsRecovery() {
		dec.RecommendedAt = deadline
		dec.RecommendationID = recommendationID(target, dec)
	}
	return prev, dec
}

func stableSignalKinds(kinds []SignalKind) []SignalKind {
	if len(kinds) == 0 {
		return nil
	}
	seen := make(map[SignalKind]struct{}, len(kinds))
	out := make([]SignalKind, 0, len(kinds))
	for _, kind := range kinds {
		if _, ok := seen[kind]; ok {
			continue
		}
		seen[kind] = struct{}{}
		out = append(out, kind)
	}
	sort.Slice(out, func(i, j int) bool {
		oi, oj := signalOrder[out[i]], signalOrder[out[j]]
		if oi != oj {
			return oi < oj
		}
		return out[i] < out[j]
	})
	return out
}

func recommendationID(target Target, dec Decision) string {
	return "watchdog:" + Fingerprint(
		target.Key(), dec.Identity, string(dec.Phase), string(dec.Action),
		dec.Deadline.UTC().Format(time.RFC3339Nano),
	)
}
