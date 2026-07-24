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
