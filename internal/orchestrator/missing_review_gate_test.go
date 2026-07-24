package orchestrator

import (
	"testing"
	"time"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/github"
	"github.com/befeast/maestro/internal/notify"
	"github.com/befeast/maestro/internal/state"
)

func missingReviewOrchestrator(missingAfterMinutes int) *Orchestrator {
	return &Orchestrator{
		cfg: &config.Config{
			Repo: "owner/repo",
			ReviewRetrigger: config.ReviewRetriggerConfig{
				MissingAfterMinutes: missingAfterMinutes,
			},
		},
		notifier: &notify.Notifier{},
		repo:     "owner/repo",
	}
}

func pendingSince(d time.Duration, now time.Time) *state.Session {
	since := now.Add(-d)
	return &state.Session{ReviewPendingSince: &since, ReviewPendingHeadSHA: "deadbeef"}
}

// An unobserved (silent) gate becomes non-blocking once the grace elapses.
func TestMissingReviewGateElapsed_SilentGatePastGrace(t *testing.T) {
	now := time.Now().UTC()
	o := missingReviewOrchestrator(60)
	verdict := github.ReviewGateVerdict{Pending: true, Observed: false}

	silentFor, missing := o.missingReviewGateElapsed(pendingSince(90*time.Minute, now), verdict, now)
	if !missing {
		t.Fatal("silent gate past the grace should be treated as missing")
	}
	if silentFor < 89*time.Minute {
		t.Fatalf("silentFor = %v, want ~90m", silentFor)
	}
}

// Inside the grace the PR keeps waiting.
func TestMissingReviewGateElapsed_WithinGraceStillWaits(t *testing.T) {
	now := time.Now().UTC()
	o := missingReviewOrchestrator(60)
	verdict := github.ReviewGateVerdict{Pending: true, Observed: false}

	if _, missing := o.missingReviewGateElapsed(pendingSince(10*time.Minute, now), verdict, now); missing {
		t.Fatal("gate should still be waiting inside the grace window")
	}
}

// The safety property: a reviewer that DID produce a signal keeps blocking no
// matter how long it takes. Only true silence is bounded.
func TestMissingReviewGateElapsed_ObservedGateNeverExpires(t *testing.T) {
	now := time.Now().UTC()
	o := missingReviewOrchestrator(60)
	verdict := github.ReviewGateVerdict{Pending: true, Observed: true}

	if _, missing := o.missingReviewGateElapsed(pendingSince(48*time.Hour, now), verdict, now); missing {
		t.Fatal("an observed (working) reviewer must keep blocking regardless of elapsed time")
	}
}

// Opt-in: 0 preserves today's block-forever behavior.
func TestMissingReviewGateElapsed_DisabledByDefault(t *testing.T) {
	now := time.Now().UTC()
	o := missingReviewOrchestrator(0)
	verdict := github.ReviewGateVerdict{Pending: true, Observed: false}

	if _, missing := o.missingReviewGateElapsed(pendingSince(48*time.Hour, now), verdict, now); missing {
		t.Fatal("missing-review policy must be off unless the operator opts in")
	}
}

// No pending clock yet (first observation) — nothing to expire.
func TestMissingReviewGateElapsed_NoClockYet(t *testing.T) {
	now := time.Now().UTC()
	o := missingReviewOrchestrator(60)
	verdict := github.ReviewGateVerdict{Pending: true, Observed: false}

	if _, missing := o.missingReviewGateElapsed(&state.Session{}, verdict, now); missing {
		t.Fatal("a session with no pending clock cannot have exceeded the grace")
	}
}

// A settled gate (not pending) never routes through the missing-review path.
func TestMissingReviewGateElapsed_SettledGateIgnored(t *testing.T) {
	now := time.Now().UTC()
	o := missingReviewOrchestrator(60)
	verdict := github.ReviewGateVerdict{Pending: false, Observed: false, Passed: true}

	if _, missing := o.missingReviewGateElapsed(pendingSince(90*time.Minute, now), verdict, now); missing {
		t.Fatal("a settled gate must not be treated as missing")
	}
}

// Codex review catch (P1): a reviewer seen on this head keeps blocking even if
// a later read comes back unobserved — a transient check-runs error must not
// demote a working reviewer to "absent".
func TestMissingReviewGateElapsed_StickyObservationBlocksExpiry(t *testing.T) {
	now := time.Now().UTC()
	o := missingReviewOrchestrator(60)
	sess := pendingSince(90*time.Minute, now)

	// The reviewer was seen earlier on this head.
	o.trackReviewPendingClock(sess, "deadbeef", github.ReviewGateVerdict{Pending: true, Observed: true}, now)
	if !sess.ReviewGateObserved {
		t.Fatal("observation was not recorded")
	}

	// This cycle's read failed over to the comment path and looks unobserved.
	blip := github.ReviewGateVerdict{Pending: true, Observed: false}
	if _, missing := o.missingReviewGateElapsed(sess, blip, now); missing {
		t.Fatal("a single unobserved read expired a gate that was observed earlier on the same head")
	}
}

// A new head resets both the clock and the observation memory.
func TestTrackReviewPendingClock_NewHeadResets(t *testing.T) {
	now := time.Now().UTC()
	o := missingReviewOrchestrator(60)
	sess := pendingSince(90*time.Minute, now)
	sess.ReviewGateObserved = true
	sess.ReviewRetriggerCount = 3

	o.trackReviewPendingClock(sess, "cafebabe", github.ReviewGateVerdict{Pending: true, Observed: false}, now)

	if sess.ReviewPendingHeadSHA != "cafebabe" {
		t.Fatalf("head = %q, want cafebabe", sess.ReviewPendingHeadSHA)
	}
	if sess.ReviewGateObserved {
		t.Fatal("observation memory survived a head change")
	}
	if sess.ReviewRetriggerCount != 0 {
		t.Fatalf("retrigger count = %d, want 0 after a head change", sess.ReviewRetriggerCount)
	}
	if sess.ReviewPendingSince == nil || !sess.ReviewPendingSince.Equal(now) {
		t.Fatal("clock was not restarted on the new head")
	}
}

// Codex review catch (P2): the clock must start for a pending gate regardless
// of which stream is pending — the Greptile-specific retrigger is not the
// owner of the clock.
func TestTrackReviewPendingClock_StartsForNonGreptileStream(t *testing.T) {
	now := time.Now().UTC()
	o := missingReviewOrchestrator(60)
	sess := &state.Session{}

	o.trackReviewPendingClock(sess, "deadbeef", github.ReviewGateVerdict{
		Pending:  true,
		Observed: false,
		Streams:  []github.ReviewStreamVerdict{{Name: "simplicity", Pending: true}},
	}, now)

	if sess.ReviewPendingSince == nil {
		t.Fatal("clock did not start for a non-greptile pending stream")
	}
}

// Codex review catch: the elapsed grace must survive a deferred merge. A
// candidate that is queued but not merged this cycle keeps its per-head clock,
// otherwise repeated deferrals restart the window forever and the PR never
// merges.
func TestMissingReviewGateElapsed_SurvivesDeferredMerge(t *testing.T) {
	now := time.Now().UTC()
	o := missingReviewOrchestrator(60)
	sess := pendingSince(90*time.Minute, now)
	verdict := github.ReviewGateVerdict{Pending: true, Observed: false}

	if _, missing := o.missingReviewGateElapsed(sess, verdict, now); !missing {
		t.Fatal("precondition: the gate should be expired")
	}

	// The merge was deferred this cycle; the clock must NOT have been reset.
	o.trackReviewPendingClock(sess, "deadbeef", verdict, now.Add(time.Minute))
	if _, missing := o.missingReviewGateElapsed(sess, verdict, now.Add(time.Minute)); !missing {
		t.Fatal("a deferred candidate lost its elapsed grace — repeated deferrals would reset the window forever")
	}
}

// The merge-past-absent-gate alert fires once per PR.
func TestNotifyMissingReviewGate_OncePerPR(t *testing.T) {
	o := missingReviewOrchestrator(60)
	o.notifyMissingReviewGate(42, time.Hour)
	if !o.missingReviewNotified[42] {
		t.Fatal("first notification was not recorded")
	}
	o.notifyMissingReviewGate(42, 2*time.Hour)
	if len(o.missingReviewNotified) != 1 {
		t.Fatalf("notified map = %v, want a single entry", o.missingReviewNotified)
	}
}
