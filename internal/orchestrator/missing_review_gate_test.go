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
	o.trackReviewGateHead(sess, "deadbeef", github.ReviewGateVerdict{Pending: true, Observed: true}, now)
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
func TestTrackReviewGateHead_NewHeadResets(t *testing.T) {
	now := time.Now().UTC()
	o := missingReviewOrchestrator(60)
	sess := pendingSince(90*time.Minute, now)
	sess.ReviewGateObserved = true
	sess.ReviewRetriggerCount = 3

	o.trackReviewGateHead(sess, "cafebabe", github.ReviewGateVerdict{Pending: true, Observed: false}, now)

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
func TestTrackReviewGateHead_StartsForNonGreptileStream(t *testing.T) {
	now := time.Now().UTC()
	o := missingReviewOrchestrator(60)
	sess := &state.Session{}

	o.trackReviewGateHead(sess, "deadbeef", github.ReviewGateVerdict{
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
	o.trackReviewGateHead(sess, "deadbeef", verdict, now.Add(time.Minute))
	if _, missing := o.missingReviewGateElapsed(sess, verdict, now.Add(time.Minute)); !missing {
		t.Fatal("a deferred candidate lost its elapsed grace — repeated deferrals would reset the window forever")
	}
}

// Codex review catch (P1): a SETTLED verdict is proof the reviewer is alive
// too. Recording observations only for pending verdicts meant a rejection
// followed by failing check-runs reads could look like "never showed up" and
// merge a PR the reviewer had rejected.
func TestTrackReviewGateHead_RecordsSettledRejection(t *testing.T) {
	now := time.Now().UTC()
	o := missingReviewOrchestrator(60)
	sess := &state.Session{}

	// The reviewer rejected this head: settled, not pending, but observed.
	o.trackReviewGateHead(sess, "deadbeef", github.ReviewGateVerdict{Pending: false, Passed: false, Observed: true}, now)
	if !sess.ReviewGateObserved {
		t.Fatal("a settled rejection was not recorded as an observation")
	}
	if sess.ReviewPendingSince != nil {
		t.Fatal("a settled verdict must not start the pending clock")
	}

	// Later reads fail over and look unobserved for longer than the grace.
	since := now.Add(-90 * time.Minute)
	sess.ReviewPendingSince = &since
	blip := github.ReviewGateVerdict{Pending: true, Observed: false}
	if _, missing := o.missingReviewGateElapsed(sess, blip, now); missing {
		t.Fatal("a PR whose reviewer already rejected it was treated as unreviewed")
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

// Codex review catch (P1): a degraded read must never be read as silence — a
// check-runs API outage would otherwise merge PRs unreviewed.
func TestMissingReviewGateElapsed_LookupFailureIsNotSilence(t *testing.T) {
	now := time.Now().UTC()
	o := missingReviewOrchestrator(60)
	verdict := github.ReviewGateVerdict{Pending: true, Observed: false, LookupFailed: true}

	if _, missing := o.missingReviewGateElapsed(pendingSince(48*time.Hour, now), verdict, now); missing {
		t.Fatal("an API failure was treated as proof the reviewer is silent")
	}
}

// Codex review catch (P2): a check that settles and goes pending again on the
// SAME head must not reset the re-trigger cap, or flapping lets a PR collect
// unlimited review-nag comments.
func TestTrackReviewGateHead_SettledThenPendingKeepsRetryCount(t *testing.T) {
	now := time.Now().UTC()
	o := missingReviewOrchestrator(60)
	sess := &state.Session{}

	o.trackReviewGateHead(sess, "deadbeef", github.ReviewGateVerdict{Pending: true, Observed: true}, now)
	sess.ReviewRetriggerCount = 3

	// The gate settles: the clock stops, the head anchor stays.
	clearReviewPendingTracking(sess)
	if sess.ReviewPendingHeadSHA != "deadbeef" {
		t.Fatal("the head anchor was dropped when the gate settled")
	}

	// The same head goes pending again (check re-run).
	o.trackReviewGateHead(sess, "deadbeef", github.ReviewGateVerdict{Pending: true, Observed: false}, now.Add(time.Minute))
	if sess.ReviewRetriggerCount != 3 {
		t.Fatalf("retrigger count = %d, want 3 — a settled/pending flap is not a new head", sess.ReviewRetriggerCount)
	}
	if !sess.ReviewGateObserved {
		t.Fatal("observation memory was lost on a settled/pending flap")
	}
}

// Workflow review catch: end-to-end wiring of the missing-review policy through
// autoMergePRs — a silent gate past the grace must actually merge the PR, and a
// silent gate inside the grace must not.
func TestAutoMergePRs_MissingReviewMergesPastGrace(t *testing.T) {
	prs := []github.PR{{Number: 10, HeadRefName: "feat/a"}}
	cfg := &config.Config{Repo: "owner/repo", ReviewGate: "greptile"}
	cfg.ReviewRetrigger.MissingAfterMinutes = 60
	cfg.ReviewRetrigger.Enabled = new(bool) // re-triggers off: the clock must still run
	o, merged := newMergeTestOrchestrator(cfg, prs)
	o.ghPRGreptileApprovedFn = func(int) (bool, bool, error) { return false, true, nil } // pending, unobserved
	o.ghPRHeadSHAFn = func(int) (string, error) { return "abc123def456789", nil }

	s := makeTestState(prs)
	sess := s.Sessions["slot-0"]
	sess.ReviewPendingHeadSHA = "abc123def456789"
	sess.ReviewPendingSince = backdated(90 * time.Minute)

	o.autoMergePRs(s)

	if len(*merged) != 1 || (*merged)[0] != 10 {
		t.Fatalf("merged = %v, want PR #10 merged past the silent gate", *merged)
	}
}

func TestAutoMergePRs_MissingReviewWaitsInsideGrace(t *testing.T) {
	prs := []github.PR{{Number: 10, HeadRefName: "feat/a"}}
	cfg := &config.Config{Repo: "owner/repo", ReviewGate: "greptile"}
	cfg.ReviewRetrigger.MissingAfterMinutes = 60
	o, merged := newMergeTestOrchestrator(cfg, prs)
	o.ghPRGreptileApprovedFn = func(int) (bool, bool, error) { return false, true, nil }
	o.ghPRHeadSHAFn = func(int) (string, error) { return "abc123def456789", nil }

	s := makeTestState(prs)
	sess := s.Sessions["slot-0"]
	sess.ReviewPendingHeadSHA = "abc123def456789"
	sess.ReviewPendingSince = backdated(10 * time.Minute)

	o.autoMergePRs(s)

	if len(*merged) != 0 {
		t.Fatalf("merged = %v, want none inside the grace window", *merged)
	}
}

// The policy stays off unless the operator opts in, end to end.
func TestAutoMergePRs_MissingReviewDisabledByDefault(t *testing.T) {
	prs := []github.PR{{Number: 10, HeadRefName: "feat/a"}}
	cfg := &config.Config{Repo: "owner/repo", ReviewGate: "greptile"}
	o, merged := newMergeTestOrchestrator(cfg, prs)
	o.ghPRGreptileApprovedFn = func(int) (bool, bool, error) { return false, true, nil }
	o.ghPRHeadSHAFn = func(int) (string, error) { return "abc123def456789", nil }

	s := makeTestState(prs)
	sess := s.Sessions["slot-0"]
	sess.ReviewPendingHeadSHA = "abc123def456789"
	sess.ReviewPendingSince = backdated(48 * time.Hour)

	o.autoMergePRs(s)

	if len(*merged) != 0 {
		t.Fatalf("merged = %v, want none — missing_after_minutes defaults to off", *merged)
	}
}
