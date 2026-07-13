// Package progress models a durable, multi-signal material-progress watermark
// and the stalled-progress watchdog that evaluates it (#887).
//
// The legacy `worker_silent_timeout_minutes` watched terminal output only, so
// it could kill a worker that was actively editing files without emitting
// output, and when disabled (0/absent) it could not recover a genuinely
// hung-but-alive worker. This package replaces that single-signal heuristic
// with one durable per-project watermark derived from whichever
// phase-appropriate signals are present:
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
// advances whenever any signal advances (or the observed set changes). Only
// when *no* signal has advanced for the whole silence budget does the watchdog
// act, and even then the recovery boundary depends on the lifecycle phase —
// a safe pre-delivery stall may stop+retry the single worker exactly once,
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
// signal before the watchdog acts.
const DefaultMaxSilence = 20 * time.Minute

// ContractVersion is the fingerprint-bound capability contract name emitted by
// genesis/lifecycle templates only after runtime canary evidence proves the
// multi-signal watchdog. Until then the capability stays a visible promotion
// blocker (#887).
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
	// SignalDelivery covers delivery approval generation, execution lease, and
	// terminal receipt.
	SignalDelivery SignalKind = "delivery"
)

// signalOrder is the stable iteration order for signals so a combined identity
// digest is deterministic regardless of the caller's insertion order.
var signalOrder = map[SignalKind]int{
	SignalIssueSession:       0,
	SignalProcessTmux:        1,
	SignalTerminalCheckpoint: 2,
	SignalWorktreeGit:        3,
	SignalPRReview:           4,
	SignalDelivery:           5,
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
// tick, which is not itself evidence of a stall.
type Signal struct {
	Kind        SignalKind `json:"kind"`
	Fingerprint string     `json:"fingerprint,omitempty"`
	ObservedAt  time.Time  `json:"observed_at,omitempty"`
}

// SignalSet is the phase-appropriate set of signals observed in one evaluation.
type SignalSet []Signal

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

// Phase is the lifecycle phase used to pick the recovery boundary when a stall
// is proven. The boundary is deliberately asymmetric: pre-delivery work may be
// safely retried, but a durable delivery lease must never be replayed on an
// uncertain result.
type Phase string

const (
	// PhaseUnknown is the zero phase; treated as pre-delivery for recovery.
	PhaseUnknown Phase = ""
	// PhasePreDelivery covers issue → worker → PR/CI/review → merge/release:
	// a proven stall here may stop the single worker and retry once.
	PhasePreDelivery Phase = "pre_delivery"
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

// Action is the watchdog's recovery verdict for one tick.
type Action string

const (
	// ActionNone means material progress was observed and the watermark
	// advanced; there is nothing to recover.
	ActionNone Action = "none"
	// ActionWaiting means no new progress yet, but the silence budget has not
	// been exhausted, so the watchdog waits.
	ActionWaiting Action = "waiting"
	// ActionStopAndRetry means a proven safe pre-delivery stall: stop the
	// single stale worker and retry/resume exactly once under the existing
	// retry budget. The caller enforces the "exactly once" via that budget;
	// Evaluate is idempotent and keeps returning this until progress advances.
	ActionStopAndRetry Action = "stop_and_retry"
	// ActionSurfaceReconciliation means the stall crossed the durable delivery
	// lease / replay boundary or occurred in a non-retryable phase: hand it to
	// operator reconciliation and never replay automatically.
	ActionSurfaceReconciliation Action = "surface_reconciliation"
	// ActionDisabled means the watchdog is off (budget <= 0). Kept distinct
	// from ActionWaiting so Fleet can report "disabled" truthfully rather than
	// implying a live-but-quiet deadline.
	ActionDisabled Action = "disabled"
)

// Decision is the durable, secret-free recovery record for one watchdog tick.
// It captures the observed signal set (by kind only), the last material
// watermark, the derived deadline, the phase, the idempotency/replay boundary,
// and the action plus its no-op reason — never a secret, raw private path, or
// command output (#887).
type Decision struct {
	EvaluatedAt     time.Time    `json:"evaluated_at"`
	Action          Action       `json:"action"`
	Reason          string       `json:"reason"`
	Phase           Phase        `json:"phase,omitempty"`
	Identity        string       `json:"identity,omitempty"`     // watermark identity at decision time
	WatermarkAt     time.Time    `json:"watermark_at,omitempty"` // last material progress time
	Deadline        time.Time    `json:"deadline,omitempty"`     // next stall deadline (derived)
	BudgetSeconds   int          `json:"budget_seconds,omitempty"`
	ObservedSignals []SignalKind `json:"observed_signals,omitempty"`
	// ReplayBoundary is true when a durable delivery lease bars automatic
	// replay: the decision defers to operator reconciliation.
	ReplayBoundary bool `json:"replay_boundary,omitempty"`
}

// Acted reports whether the decision took (or asks the caller to take) a
// recovery action, as opposed to observing progress or waiting.
func (d Decision) Acted() bool {
	return d.Action == ActionStopAndRetry || d.Action == ActionSurfaceReconciliation
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
	now = now.UTC()
	id := observed.Combined()
	dec := Decision{
		EvaluatedAt:     now,
		Phase:           phase,
		ObservedSignals: observed.Kinds(),
	}
	if budget > 0 {
		dec.BudgetSeconds = int(budget.Round(time.Second).Seconds())
	}

	// Disabled watchdog: record the identity for reporting but never act.
	if budget <= 0 {
		if prev.IsZero() && id != "" {
			prev = Watermark{Identity: id, At: now, Phase: phase, Signals: observed.Present()}
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
	if prev.IsZero() || id != prev.Identity {
		next := Watermark{Identity: id, At: now, Phase: phase, Signals: observed.Present()}
		dec.Action = ActionNone
		dec.Reason = "material progress observed; watermark advanced"
		dec.Identity = next.Identity
		dec.WatermarkAt = next.At
		dec.Deadline = next.Deadline(budget)
		return next, dec
	}

	// No material progress since prev.At. The deadline is derived from the
	// durable watermark, so it is identical after a daemon restart.
	deadline := prev.Deadline(budget)
	dec.Identity = prev.Identity
	dec.WatermarkAt = prev.At
	dec.Deadline = deadline

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
	case phase.RequiresReconciliation():
		dec.Action = ActionSurfaceReconciliation
		dec.ReplayBoundary = true
		dec.Reason = "delivery lease is executing/uncertain; recovery authority ends before the durable delivery lease — surface operator reconciliation, never replay"
	case phase.AllowsWorkerRetry():
		dec.Action = ActionStopAndRetry
		dec.Reason = "proven pre-delivery stall past the silence budget; stop the single stale worker and retry once under the existing retry budget"
	default:
		dec.Action = ActionSurfaceReconciliation
		dec.ReplayBoundary = true
		dec.Reason = "stall past the silence budget in a non-retryable phase; surface operator reconciliation"
	}
	return prev, dec
}
