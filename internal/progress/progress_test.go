package progress

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

var base = time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)

// sig is a tiny helper to build a signal with a hashed fingerprint.
func sig(kind SignalKind, raw string) Signal {
	return Signal{Kind: kind, Fingerprint: Fingerprint(raw)}
}

func TestFingerprint_NonReversibleAndStable(t *testing.T) {
	// Absent when every part is empty — an absent signal, not a stall.
	if got := Fingerprint("", "  "); got != "" {
		t.Fatalf("Fingerprint(empty) = %q, want empty", got)
	}
	// Stable and non-reversible: same input → same short digest, and the raw
	// input never appears verbatim in the digest.
	raw := "/home/god/worktrees/secret-path@abc123"
	a := Fingerprint(raw)
	b := Fingerprint(raw)
	if a == "" || a != b {
		t.Fatalf("Fingerprint not stable: %q vs %q", a, b)
	}
	if len(a) != 16 {
		t.Fatalf("Fingerprint len = %d, want 16", len(a))
	}
	if got := Fingerprint(raw); got == raw {
		t.Fatalf("fingerprint leaked raw input")
	}
	if Fingerprint("a", "b") == Fingerprint("ab") {
		t.Fatalf("part boundaries not separated in digest")
	}
}

func TestCombined_OrderIndependentAndSensitive(t *testing.T) {
	s1 := SignalSet{sig(SignalWorktreeGit, "head-1"), sig(SignalProcessTmux, "pid-1")}
	s2 := SignalSet{sig(SignalProcessTmux, "pid-1"), sig(SignalWorktreeGit, "head-1")}
	if s1.Combined() != s2.Combined() {
		t.Fatalf("combined identity depends on insertion order")
	}
	// Any signal advancing changes the identity.
	s3 := SignalSet{sig(SignalProcessTmux, "pid-1"), sig(SignalWorktreeGit, "head-2")}
	if s1.Combined() == s3.Combined() {
		t.Fatalf("advancing a signal did not change combined identity")
	}
	// Empty fingerprints are dropped (absent signal), not counted.
	s4 := SignalSet{sig(SignalWorktreeGit, "head-1"), sig(SignalProcessTmux, "pid-1"), {Kind: SignalPRReview, Fingerprint: ""}}
	if s4.Combined() != s1.Combined() {
		t.Fatalf("absent signal changed combined identity")
	}
	if (SignalSet{}).Combined() != "" {
		t.Fatalf("empty set should yield empty identity")
	}
}

// Requirement: do not flag or kill a quiet worker while bounded filesystem/git
// evidence shows continued material progress — even if the terminal signal is
// frozen and the budget elapses many times over.
func TestEvaluate_QuietButActive_NoFalsePositive(t *testing.T) {
	budget := 20 * time.Minute
	// Terminal output is frozen the entire run; git HEAD advances each tick.
	frozenTerminal := sig(SignalTerminalCheckpoint, "same-output-hash")

	var wm Watermark
	now := base
	for tick := 0; tick < 10; tick++ {
		// 30 minutes between ticks: well past a 20-minute budget every time.
		now = now.Add(30 * time.Minute)
		git := sig(SignalWorktreeGit, "commit-tree-"+time.Duration(tick).String())
		observed := SignalSet{frozenTerminal, sig(SignalProcessTmux, "pid-1"), git}
		var dec Decision
		wm, dec = Evaluate(wm, observed, PhasePreDelivery, budget, now)
		if dec.Action != ActionNone {
			t.Fatalf("tick %d: action = %q, want none (worker still making git progress)", tick, dec.Action)
		}
		if dec.Acted() {
			t.Fatalf("tick %d: watchdog acted on an actively-editing worker", tick)
		}
	}
}

// Requirement: on a proven safe pre-delivery stall, ask to stop the single
// worker and retry once.
func TestEvaluate_ProvenStall_StopAndRetry(t *testing.T) {
	budget := 20 * time.Minute
	frozen := SignalSet{sig(SignalProcessTmux, "pid-1"), sig(SignalWorktreeGit, "head-1"), sig(SignalTerminalCheckpoint, "hash-1")}

	// First tick establishes the watermark.
	wm, dec := Evaluate(Watermark{}, frozen, PhasePreDelivery, budget, base)
	if dec.Action != ActionNone {
		t.Fatalf("first tick action = %q, want none", dec.Action)
	}
	wantDeadline := base.Add(budget)
	if !dec.Deadline.Equal(wantDeadline) {
		t.Fatalf("deadline = %s, want %s", dec.Deadline, wantDeadline)
	}

	// Just inside the budget: still waiting, no action.
	wm2, dec := Evaluate(wm, frozen, PhasePreDelivery, budget, base.Add(budget-time.Second))
	if dec.Action != ActionWaiting {
		t.Fatalf("inside budget action = %q, want waiting", dec.Action)
	}
	if !wm2.At.Equal(wm.At) {
		t.Fatalf("waiting tick reset the watermark time")
	}

	// One second past the deadline: proven stall.
	_, dec = Evaluate(wm, frozen, PhasePreDelivery, budget, base.Add(budget+time.Second))
	if dec.Action != ActionStopAndRetry {
		t.Fatalf("past deadline action = %q, want stop_and_retry", dec.Action)
	}
	if dec.ReplayBoundary {
		t.Fatalf("pre-delivery stall must not set replay boundary")
	}
	if len(dec.ObservedSignals) != 3 {
		t.Fatalf("decision recorded %d signals, want 3", len(dec.ObservedSignals))
	}
}

// Requirement: Evaluate is idempotent past the deadline (keeps flagging the
// stall) so the caller's retry budget is the single "retry exactly once"
// authority; the deadline never resets while the worker stays frozen.
func TestEvaluate_StallIdempotent_DeadlineDoesNotDrift(t *testing.T) {
	budget := 20 * time.Minute
	frozen := SignalSet{sig(SignalProcessTmux, "pid-1"), sig(SignalWorktreeGit, "head-1")}
	wm, _ := Evaluate(Watermark{}, frozen, PhasePreDelivery, budget, base)

	deadline := base.Add(budget)
	for i := 1; i <= 5; i++ {
		next, dec := Evaluate(wm, frozen, PhasePreDelivery, budget, base.Add(budget+time.Duration(i)*time.Minute))
		if dec.Action != ActionStopAndRetry {
			t.Fatalf("iter %d action = %q, want stop_and_retry", i, dec.Action)
		}
		if !dec.Deadline.Equal(deadline) {
			t.Fatalf("iter %d deadline drifted to %s, want %s", i, dec.Deadline, deadline)
		}
		if !next.At.Equal(wm.At) {
			t.Fatalf("iter %d watermark time drifted", i)
		}
	}
}

// Requirement: after recovery, a respawned worker presents fresh signals
// (new pid/tmux/session) so the watermark advances and the deadline resets.
func TestEvaluate_RecoveryAdvancesWatermark(t *testing.T) {
	budget := 20 * time.Minute
	stale := SignalSet{sig(SignalProcessTmux, "pid-1"), sig(SignalWorktreeGit, "head-1")}
	wm, _ := Evaluate(Watermark{}, stale, PhasePreDelivery, budget, base)

	// Stall.
	_, dec := Evaluate(wm, stale, PhasePreDelivery, budget, base.Add(budget+time.Minute))
	if dec.Action != ActionStopAndRetry {
		t.Fatalf("expected stall, got %q", dec.Action)
	}

	// Respawn: new process identity.
	respawnAt := base.Add(budget + 2*time.Minute)
	fresh := SignalSet{sig(SignalProcessTmux, "pid-2"), sig(SignalWorktreeGit, "head-1")}
	wm2, dec := Evaluate(wm, fresh, PhasePreDelivery, budget, respawnAt)
	if dec.Action != ActionNone {
		t.Fatalf("post-respawn action = %q, want none", dec.Action)
	}
	if !wm2.At.Equal(respawnAt) {
		t.Fatalf("watermark did not advance to respawn time")
	}
	if !dec.Deadline.Equal(respawnAt.Add(budget)) {
		t.Fatalf("deadline did not reset after recovery")
	}
}

// Requirement: record the last material progress identity/time across the full
// lifecycle and survive daemon restart without resetting or duplicating the
// deadline.
func TestEvaluate_RestartDurability(t *testing.T) {
	budget := 20 * time.Minute
	frozen := SignalSet{sig(SignalProcessTmux, "pid-1"), sig(SignalWorktreeGit, "head-1")}
	wm, dec := Evaluate(Watermark{}, frozen, PhasePreDelivery, budget, base)
	deadlineBefore := dec.Deadline

	// Simulate a daemon restart: the watermark round-trips through state.json.
	blob, err := json.Marshal(wm)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var reloaded Watermark
	if err := json.Unmarshal(blob, &reloaded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if reloaded.Identity != wm.Identity || !reloaded.At.Equal(wm.At) {
		t.Fatalf("watermark did not survive round-trip: %+v vs %+v", reloaded, wm)
	}

	// After restart, evaluating the same frozen signals yields the identical
	// deadline — no reset, no duplication.
	_, dec2 := Evaluate(reloaded, frozen, PhasePreDelivery, budget, base.Add(5*time.Minute))
	if !dec2.Deadline.Equal(deadlineBefore) {
		t.Fatalf("deadline after restart = %s, want %s (must not reset)", dec2.Deadline, deadlineBefore)
	}
	if dec2.Action != ActionWaiting {
		t.Fatalf("action after restart = %q, want waiting (still inside budget)", dec2.Action)
	}
}

// Requirement: preserve late review feedback — a new PR/review signal after a
// stall counts as fresh material progress and re-arms the watchdog.
func TestEvaluate_LateFeedbackReArms(t *testing.T) {
	budget := 20 * time.Minute
	before := SignalSet{sig(SignalPRReview, "pr#5@sha1;ci=pending")}
	wm, _ := Evaluate(Watermark{}, before, PhasePreDelivery, budget, base)

	// Stall on the same review state.
	_, dec := Evaluate(wm, before, PhasePreDelivery, budget, base.Add(budget+time.Minute))
	if dec.Action != ActionStopAndRetry {
		t.Fatalf("expected stall before feedback, got %q", dec.Action)
	}

	// Late review feedback lands: the PR/review fingerprint changes.
	feedbackAt := base.Add(budget + 2*time.Minute)
	after := SignalSet{sig(SignalPRReview, "pr#5@sha1;ci=failure;review=changes_requested")}
	wm2, dec := Evaluate(wm, after, PhasePreDelivery, budget, feedbackAt)
	if dec.Action != ActionNone {
		t.Fatalf("late feedback action = %q, want none (fresh progress)", dec.Action)
	}
	if !wm2.At.Equal(feedbackAt) {
		t.Fatalf("late feedback did not advance the watermark")
	}
}

// Requirement: if an approval-gated delivery is executing/uncertain, surface
// operator reconciliation and never replay it automatically.
func TestEvaluate_UncertainDelivery_NeverReplay(t *testing.T) {
	budget := 20 * time.Minute
	frozen := SignalSet{sig(SignalDelivery, "lease-abc;state=executing")}
	wm, _ := Evaluate(Watermark{}, frozen, PhaseDeliveryExecuting, budget, base)

	_, dec := Evaluate(wm, frozen, PhaseDeliveryExecuting, budget, base.Add(budget+time.Minute))
	if dec.Action != ActionSurfaceReconciliation {
		t.Fatalf("executing-delivery stall action = %q, want surface_reconciliation", dec.Action)
	}
	if !dec.ReplayBoundary {
		t.Fatalf("executing-delivery stall must mark the replay boundary")
	}
	if dec.Action == ActionStopAndRetry {
		t.Fatalf("must never auto-retry an executing delivery")
	}
}

// Requirement: disabled timeout (0/absent) cannot recover — Evaluate reports
// ActionDisabled and never kills, distinct from a live-but-quiet deadline.
func TestEvaluate_DisabledBudget(t *testing.T) {
	frozen := SignalSet{sig(SignalProcessTmux, "pid-1")}
	wm, dec := Evaluate(Watermark{}, frozen, PhasePreDelivery, 0, base)
	if dec.Action != ActionDisabled {
		t.Fatalf("action = %q, want disabled", dec.Action)
	}
	// Even far in the future the disabled watchdog never acts.
	_, dec = Evaluate(wm, frozen, PhasePreDelivery, 0, base.Add(240*time.Hour))
	if dec.Acted() {
		t.Fatalf("disabled watchdog acted")
	}
	if !dec.Deadline.IsZero() {
		t.Fatalf("disabled watchdog reported a deadline")
	}
}

// Requirement: a single missing/stale signal is not proof of a stall — losing
// one kind while another advances still reads as progress.
func TestEvaluate_SingleMissingSignalNotAStall(t *testing.T) {
	budget := 20 * time.Minute
	full := SignalSet{sig(SignalProcessTmux, "pid-1"), sig(SignalTerminalCheckpoint, "hash-1"), sig(SignalWorktreeGit, "head-1")}
	wm, _ := Evaluate(Watermark{}, full, PhasePreDelivery, budget, base)

	// Terminal signal drops out but git advances; still progress.
	partial := SignalSet{sig(SignalProcessTmux, "pid-1"), sig(SignalWorktreeGit, "head-2")}
	_, dec := Evaluate(wm, partial, PhasePreDelivery, budget, base.Add(budget+time.Minute))
	if dec.Action != ActionNone {
		t.Fatalf("action = %q, want none (git still advancing)", dec.Action)
	}
}

// Concurrent watchdog cycles: two evaluations reading the same durable
// watermark and clock must produce identical, side-effect-free decisions.
func TestEvaluate_ConcurrentCyclesDeterministic(t *testing.T) {
	budget := 20 * time.Minute
	frozen := SignalSet{sig(SignalProcessTmux, "pid-1"), sig(SignalWorktreeGit, "head-1")}
	wm, _ := Evaluate(Watermark{}, frozen, PhasePreDelivery, budget, base)
	at := base.Add(budget + time.Minute)

	w1, d1 := Evaluate(wm, frozen, PhasePreDelivery, budget, at)
	w2, d2 := Evaluate(wm, frozen, PhasePreDelivery, budget, at)
	if d1.Action != d2.Action || !d1.Deadline.Equal(d2.Deadline) {
		t.Fatalf("concurrent cycles diverged: %+v vs %+v", d1, d2)
	}
	if w1.Identity != w2.Identity || !w1.At.Equal(w2.At) {
		t.Fatalf("concurrent cycles produced different watermarks")
	}
	// prev is never mutated.
	if !wm.At.Equal(base) {
		t.Fatalf("Evaluate mutated the input watermark")
	}
}

func TestDecision_NoSecretsInObservedSignals(t *testing.T) {
	// Kinds are safe to log; fingerprints are digests, never raw material.
	observed := SignalSet{sig(SignalWorktreeGit, "/home/god/secret/path"), sig(SignalDelivery, "ghp_realtokenlike")}
	_, dec := Evaluate(Watermark{}, observed, PhasePreDelivery, time.Minute, base)
	blob, _ := json.Marshal(dec)
	for _, leak := range []string{"/home/god", "secret", "ghp_realtokenlike"} {
		if strings.Contains(string(blob), leak) {
			t.Fatalf("decision leaked %q: %s", leak, blob)
		}
	}
}

// A signal that stops advancing while another keeps advancing must show its own
// last-changed age, not the evaluation time (#887 review: signal ages must not
// collapse to a single fresh value). The frozen signal's ObservedAt is carried
// forward while the moving signal's ObservedAt tracks each advance.
func TestEvaluate_PerSignalObservedAtTracksLastChange(t *testing.T) {
	budget := 20 * time.Minute
	// t0: both signals observed together.
	t0 := base
	obs0 := SignalSet{
		{Kind: SignalProcessTmux, Fingerprint: Fingerprint("pid-1"), ObservedAt: t0},
		{Kind: SignalWorktreeGit, Fingerprint: Fingerprint("head-1"), ObservedAt: t0},
	}
	wm, _ := Evaluate(Watermark{}, obs0, PhasePreDelivery, budget, t0)

	// t1: git advances (head-2), process/tmux fingerprint unchanged.
	t1 := t0.Add(5 * time.Minute)
	obs1 := SignalSet{
		{Kind: SignalProcessTmux, Fingerprint: Fingerprint("pid-1"), ObservedAt: t1},
		{Kind: SignalWorktreeGit, Fingerprint: Fingerprint("head-2"), ObservedAt: t1},
	}
	wm, dec := Evaluate(wm, obs1, PhasePreDelivery, budget, t1)
	if dec.Action != ActionNone {
		t.Fatalf("git advance should be progress, got %q", dec.Action)
	}

	byKind := map[SignalKind]time.Time{}
	for _, s := range wm.Signals {
		byKind[s.Kind] = s.ObservedAt
	}
	// The moving git signal ages from t1; the frozen process signal still ages
	// from t0 — the two are no longer identical.
	if got := byKind[SignalWorktreeGit]; !got.Equal(t1) {
		t.Errorf("worktree_git ObservedAt = %s, want %s (advanced at t1)", got, t1)
	}
	if got := byKind[SignalProcessTmux]; !got.Equal(t0) {
		t.Errorf("process_tmux ObservedAt = %s, want %s (carried forward, not eval time)", got, t0)
	}
	if byKind[SignalWorktreeGit].Equal(byKind[SignalProcessTmux]) {
		t.Fatalf("frozen and moving signals collapsed to one age")
	}
}

func TestEvaluate_PerSignalObservedAtIsStampedOnFingerprintChange(t *testing.T) {
	budget := 20 * time.Minute
	// Collector timestamps are observation times, not change times. The
	// evaluator must ignore even a plausible-looking synthetic timestamp.
	collectorTime := base.Add(-24 * time.Hour)
	observed := SignalSet{{
		Kind: SignalWorktreeGit, Fingerprint: Fingerprint("head-1"), ObservedAt: collectorTime,
	}}
	wm, _ := Evaluate(Watermark{}, observed, PhasePreDelivery, budget, base)
	if got := wm.Signals[0].ObservedAt; !got.Equal(base) {
		t.Fatalf("initial signal timestamp = %s, want evaluator change time %s", got, base)
	}

	changedAt := base.Add(5 * time.Minute)
	observed[0].Fingerprint = Fingerprint("head-2")
	observed[0].ObservedAt = changedAt.Add(10 * time.Hour)
	wm, _ = Evaluate(wm, observed, PhasePreDelivery, budget, changedAt)
	if got := wm.Signals[0].ObservedAt; !got.Equal(changedAt) {
		t.Fatalf("changed signal timestamp = %s, want evaluator change time %s", got, changedAt)
	}
}

func TestEvaluate_SignalDisappearanceAndSameReappearanceDoNotRearm(t *testing.T) {
	budget := 20 * time.Minute
	process := sig(SignalProcessTmux, "pid-1")
	git := sig(SignalWorktreeGit, "head-1")
	wm, _ := Evaluate(Watermark{}, SignalSet{process, git}, PhasePreDelivery, budget, base)
	identity, watermarkAt := wm.Identity, wm.At

	// Losing git observability is not progress.
	var dec Decision
	wm, dec = Evaluate(wm, SignalSet{process}, PhasePreDelivery, budget, base.Add(5*time.Minute))
	if dec.Action != ActionWaiting || wm.Identity != identity || !wm.At.Equal(watermarkAt) {
		t.Fatalf("disappearance re-armed watermark: wm=%+v dec=%+v", wm, dec)
	}
	// The same fingerprint reappearing is also not progress.
	wm, dec = Evaluate(wm, SignalSet{process, git}, PhasePreDelivery, budget, base.Add(10*time.Minute))
	if dec.Action != ActionWaiting || wm.Identity != identity || !wm.At.Equal(watermarkAt) {
		t.Fatalf("same reappearance re-armed watermark: wm=%+v dec=%+v", wm, dec)
	}
	// A changed fingerprint after absence is genuine progress.
	wm, dec = Evaluate(wm, SignalSet{process, sig(SignalWorktreeGit, "head-2")}, PhasePreDelivery, budget, base.Add(15*time.Minute))
	if dec.Action != ActionNone || !wm.At.Equal(base.Add(15*time.Minute)) {
		t.Fatalf("changed reappearance did not advance: wm=%+v dec=%+v", wm, dec)
	}
}

func TestEvaluateTarget_RecommendationIsExactAndStable(t *testing.T) {
	target := Target{
		Kind: TargetWorker, IssueNumber: 42, Slot: "3", SessionID: "started-at",
		TmuxSession: "maestro-3", ProcessID: 1234, LeaseID: "lease-42-3",
	}
	if err := target.Validate(); err != nil {
		t.Fatal(err)
	}
	observed := SignalSet{sig(SignalProcessTmux, "pid-1234")}
	wm, _ := EvaluateTarget(target, Watermark{}, observed, PhasePreDelivery, time.Minute, base)
	_, first := EvaluateTarget(target, wm, observed, PhasePreDelivery, time.Minute, base.Add(2*time.Minute))
	_, repeated := EvaluateTarget(target, wm, observed, PhasePreDelivery, time.Minute, base.Add(3*time.Minute))
	if first.Target.Key() != target.Key() || first.RecommendationID == "" {
		t.Fatalf("recommendation not bound to exact target: %+v", first)
	}
	if repeated.RecommendationID != first.RecommendationID || !repeated.RecommendedAt.Equal(first.RecommendedAt) {
		t.Fatalf("repeated recommendation was refreshed: first=%+v repeated=%+v", first, repeated)
	}
}

func TestEvaluateTarget_DeliveredReceiptNeverRecommendsRecovery(t *testing.T) {
	target := Target{Kind: TargetDelivery, LeaseID: "approval-1:gen-2"}
	observed := SignalSet{sig(SignalDelivery, "executed-receipt")}
	wm, first := EvaluateTarget(target, Watermark{}, observed, PhaseDelivered, time.Minute, base)
	if first.RecommendsRecovery() || !first.Deadline.IsZero() {
		t.Fatalf("terminal receipt armed recovery: %+v", first)
	}
	_, overdue := EvaluateTarget(target, wm, observed, PhaseDelivered, time.Minute, base.Add(time.Hour))
	if overdue.RecommendsRecovery() || !overdue.Deadline.IsZero() {
		t.Fatalf("old terminal receipt armed recovery: %+v", overdue)
	}
}

func TestTargetValidate_PRGateRejectsLiveProcessIdentity(t *testing.T) {
	target := Target{
		Kind: TargetPRGate, IssueNumber: 42, Slot: "3", SessionID: "started-at",
		LeaseID: "pr-7:head-abc", ProcessID: 1234,
	}
	if err := target.Validate(); err == nil {
		t.Fatal("PR-gate target with live process identity was accepted")
	}
}
