package orchestrator

import (
	"testing"
	"time"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/github"
)

// llmReviewTestConfig is a project on the #1148 llm-review gate (opus + terra
// pair) with the bounded absent-reviewer escape configured.
func llmReviewTestConfig() *config.Config {
	cfg := &config.Config{
		Repo:              "owner/repo",
		ReviewGate:        "llm-review",
		ReviewGateStreams: []string{"llm-review-opus", "llm-review-terra"},
	}
	return cfg
}

func llmPendingVerdict(observed bool) github.ReviewGateVerdict {
	return github.ReviewGateVerdict{
		Passed:   false,
		Pending:  true,
		Observed: observed,
		Streams: []github.ReviewStreamVerdict{
			{Name: "llm-review-opus", Pending: true, Observed: observed},
			{Name: "llm-review-terra", Pending: true, Observed: observed},
		},
	}
}

// The "@greptile review" re-trigger comment is greptile-specific: a pending
// llm-review stream must never post it — the generic bounded escape
// (review_retrigger.missing_after_minutes, #1115) owns the absent-reviewer
// case for these streams.
func TestReviewRetrigger_LLMReviewPendingStreamDoesNotPostGreptileComment(t *testing.T) {
	prs := []github.PR{{Number: 10, HeadRefName: "feat/a"}}
	o, comments := newRetriggerTestOrchestrator(llmReviewTestConfig(), prs, "abc123def456789")
	o.ghPRReviewGateVerdictFn = func(int, []string) (github.ReviewGateVerdict, error) {
		return llmPendingVerdict(false), nil
	}
	s := makeTestState(prs)
	sess := s.Sessions["slot-0"]
	sess.ReviewPendingHeadSHA = "abc123def456789"
	sess.ReviewPendingSince = backdated(45 * time.Minute) // far past the 10m default threshold

	o.autoMergePRs(s)

	if len(*comments) != 0 {
		t.Fatalf("comments = %v, want none — the greptile nudge must not fire for llm-review streams", *comments)
	}
}

// End-to-end #1148 acceptance: an llm-review gate that has produced no signal
// at all on the head for longer than missing_after_minutes yields the
// non-blocking escape and the green PR merges.
func TestAutoMergePRs_LLMReviewAbsentPastGraceMerges(t *testing.T) {
	prs := []github.PR{{Number: 10, HeadRefName: "feat/a"}}
	cfg := llmReviewTestConfig()
	cfg.ReviewRetrigger.MissingAfterMinutes = 60
	o, merged := newMergeTestOrchestrator(cfg, prs)
	o.ghPRReviewGateVerdictFn = func(int, []string) (github.ReviewGateVerdict, error) {
		return llmPendingVerdict(false), nil // silent: no status, no check, no comment
	}
	o.ghPRHeadSHAFn = func(int) (string, error) { return "abc123def456789", nil }

	s := makeTestState(prs)
	sess := s.Sessions["slot-0"]
	sess.ReviewPendingHeadSHA = "abc123def456789"
	sess.ReviewPendingSince = backdated(90 * time.Minute)

	o.autoMergePRs(s)

	if len(*merged) != 1 || (*merged)[0] != 10 {
		t.Fatalf("merged = %v, want PR #10 merged past the absent llm-review gate", *merged)
	}
}

// Inside the grace the PR keeps waiting — the escape is bounded, not a bypass.
func TestAutoMergePRs_LLMReviewAbsentInsideGraceWaits(t *testing.T) {
	prs := []github.PR{{Number: 10, HeadRefName: "feat/a"}}
	cfg := llmReviewTestConfig()
	cfg.ReviewRetrigger.MissingAfterMinutes = 60
	o, merged := newMergeTestOrchestrator(cfg, prs)
	o.ghPRReviewGateVerdictFn = func(int, []string) (github.ReviewGateVerdict, error) {
		return llmPendingVerdict(false), nil
	}
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

// An llm-review stream that actually spoke (pending status observed on the
// head) never expires: only true silence is bounded.
func TestAutoMergePRs_LLMReviewObservedPendingNeverExpires(t *testing.T) {
	prs := []github.PR{{Number: 10, HeadRefName: "feat/a"}}
	cfg := llmReviewTestConfig()
	cfg.ReviewRetrigger.MissingAfterMinutes = 60
	o, merged := newMergeTestOrchestrator(cfg, prs)
	o.ghPRReviewGateVerdictFn = func(int, []string) (github.ReviewGateVerdict, error) {
		return llmPendingVerdict(true), nil // the reviewer is alive and working
	}
	o.ghPRHeadSHAFn = func(int) (string, error) { return "abc123def456789", nil }

	s := makeTestState(prs)
	sess := s.Sessions["slot-0"]
	sess.ReviewPendingHeadSHA = "abc123def456789"
	sess.ReviewPendingSince = backdated(48 * time.Hour)

	o.autoMergePRs(s)

	if len(*merged) != 0 {
		t.Fatalf("merged = %v, want none — an observed reviewer must keep blocking", *merged)
	}
}

// A settled llm-review rejection blocks the merge outright.
func TestAutoMergePRs_LLMReviewRejectionBlocks(t *testing.T) {
	prs := []github.PR{{Number: 10, HeadRefName: "feat/a"}}
	cfg := llmReviewTestConfig()
	o, merged := newMergeTestOrchestrator(cfg, prs)
	o.ghPRReviewGateVerdictFn = func(int, []string) (github.ReviewGateVerdict, error) {
		return github.ReviewGateVerdict{
			Passed:   false,
			Pending:  false,
			Observed: true,
			Streams: []github.ReviewStreamVerdict{
				{Name: "llm-review-opus", Passed: true, Observed: true},
				{Name: "llm-review-terra", Passed: false, Observed: true,
					Findings: []github.ReviewComment{{Path: "a.go", Line: 3, Body: "[P0] boom", User: "okbot"}}},
			},
		}, nil
	}
	o.ghPRHeadSHAFn = func(int) (string, error) { return "abc123def456789", nil }

	s := makeTestState(prs)

	o.autoMergePRs(s)

	if len(*merged) != 0 {
		t.Fatalf("merged = %v, want none — a P0 rejection must block", *merged)
	}
}

// llmHalfObservedVerdict is the #1148 review round 1 P1-2 shape: opus settled
// green, terra never reported anything. Aggregate: Pending (terra) + Observed
// (opus) — the state that used to wedge forever.
func llmHalfObservedVerdict() github.ReviewGateVerdict {
	return github.ReviewGateVerdict{
		Passed:   false,
		Pending:  true,
		Observed: true,
		Streams: []github.ReviewStreamVerdict{
			{Name: "llm-review-opus", Passed: true, Observed: true},
			{Name: "llm-review-terra", Pending: true, Observed: false},
		},
	}
}

// P1-2 regression (unit): one stream settled green, the other silent past the
// grace — the silent stream must expire instead of the aggregate Observed bit
// blocking the escape forever.
func TestMissingReviewGateElapsed_HalfObservedPairExpires(t *testing.T) {
	now := time.Now().UTC()
	o := missingReviewOrchestrator(60)
	sess := pendingSince(90*time.Minute, now)
	verdict := llmHalfObservedVerdict()

	// Realistic flow: the tracker has already recorded the observed opus
	// stream (aggregate sticky bit AND per-stream memory) on this head.
	o.trackReviewGateHead(sess, "deadbeef", verdict, now)
	if !sess.ReviewGateObserved || !sess.ReviewGateStreamObserved["llm-review-opus"] {
		t.Fatal("precondition: the opus observation was not recorded")
	}

	silentFor, missing := o.missingReviewGateElapsed(sess, verdict, now)
	if !missing {
		t.Fatal("half-observed pair: the never-reporting stream must stop blocking past the grace")
	}
	if silentFor < 89*time.Minute {
		t.Fatalf("silentFor = %v, want ~90m", silentFor)
	}
}

// An OBSERVED pending stream still hard-blocks at any duration — only true
// per-stream silence is bounded.
func TestMissingReviewGateElapsed_ObservedPendingStreamNeverExpires(t *testing.T) {
	now := time.Now().UTC()
	o := missingReviewOrchestrator(60)
	sess := pendingSince(48*time.Hour, now)
	verdict := github.ReviewGateVerdict{
		Passed:   false,
		Pending:  true,
		Observed: true,
		Streams: []github.ReviewStreamVerdict{
			{Name: "llm-review-opus", Passed: true, Observed: true},
			{Name: "llm-review-terra", Pending: true, Observed: true}, // pending status posted
		},
	}
	o.trackReviewGateHead(sess, "deadbeef", verdict, now)

	if _, missing := o.missingReviewGateElapsed(sess, verdict, now); missing {
		t.Fatal("a stream that posted a pending signal must keep blocking regardless of elapsed time")
	}
}

// Sticky per-stream memory: a stream seen once on this head keeps blocking
// even when a later read comes back unobserved for it.
func TestMissingReviewGateElapsed_StickyStreamObservationBlocksExpiry(t *testing.T) {
	now := time.Now().UTC()
	o := missingReviewOrchestrator(60)
	sess := pendingSince(90*time.Minute, now)

	// terra was genuinely observed pending earlier on this head.
	o.trackReviewGateHead(sess, "deadbeef", github.ReviewGateVerdict{
		Pending: true, Observed: true,
		Streams: []github.ReviewStreamVerdict{
			{Name: "llm-review-opus", Passed: true, Observed: true},
			{Name: "llm-review-terra", Pending: true, Observed: true},
		},
	}, now)

	// This cycle's read degrades terra to unobserved (e.g. the statuses read
	// fell through) — the sticky memory must keep blocking.
	if _, missing := o.missingReviewGateElapsed(sess, llmHalfObservedVerdict(), now); missing {
		t.Fatal("a single unobserved read expired a stream that was observed earlier on the same head")
	}
}

// A settled REJECTION from any stream disables the escape outright: merging
// past an absent reviewer must never also merge past a live red verdict.
func TestMissingReviewGateElapsed_SettledFailureBlocksEscape(t *testing.T) {
	now := time.Now().UTC()
	o := missingReviewOrchestrator(60)
	sess := pendingSince(90*time.Minute, now)
	verdict := github.ReviewGateVerdict{
		Passed:   false,
		Pending:  true,
		Observed: true,
		Streams: []github.ReviewStreamVerdict{
			{Name: "llm-review-opus", Passed: false, Observed: true}, // settled red
			{Name: "llm-review-terra", Pending: true, Observed: false},
		},
	}
	o.trackReviewGateHead(sess, "deadbeef", verdict, now)

	if _, missing := o.missingReviewGateElapsed(sess, verdict, now); missing {
		t.Fatal("the absent-stream escape must not merge past a settled failing stream")
	}
}

// State written by a pre-#1148 binary: aggregate sticky bit set, no per-stream
// attribution. Must stay conservative and keep blocking.
func TestMissingReviewGateElapsed_LegacyAggregateStickyBlocks(t *testing.T) {
	now := time.Now().UTC()
	o := missingReviewOrchestrator(60)
	sess := pendingSince(90*time.Minute, now)
	sess.ReviewGateObserved = true // old binary recorded this without a map

	if _, missing := o.missingReviewGateElapsed(sess, llmHalfObservedVerdict(), now); missing {
		t.Fatal("aggregate-only sticky state must be treated as observed, not expired")
	}
}

// A head change resets the per-stream memory together with the aggregate bit.
func TestTrackReviewGateHead_NewHeadResetsStreamMemory(t *testing.T) {
	now := time.Now().UTC()
	o := missingReviewOrchestrator(60)
	sess := pendingSince(90*time.Minute, now)
	sess.ReviewGateStreamObserved = map[string]bool{"llm-review-opus": true}

	o.trackReviewGateHead(sess, "cafebabe", github.ReviewGateVerdict{Pending: true}, now)

	if len(sess.ReviewGateStreamObserved) != 0 {
		t.Fatalf("stream memory = %v, want empty after a head change", sess.ReviewGateStreamObserved)
	}
}

// P1-2 regression (end to end): the half-observed pair merges past the grace
// through autoMergePRs, and waits inside it.
func TestAutoMergePRs_HalfObservedPairMergesPastGrace(t *testing.T) {
	prs := []github.PR{{Number: 10, HeadRefName: "feat/a"}}
	cfg := llmReviewTestConfig()
	cfg.ReviewRetrigger.MissingAfterMinutes = 60
	o, merged := newMergeTestOrchestrator(cfg, prs)
	o.ghPRReviewGateVerdictFn = func(int, []string) (github.ReviewGateVerdict, error) {
		return llmHalfObservedVerdict(), nil
	}
	o.ghPRHeadSHAFn = func(int) (string, error) { return "abc123def456789", nil }

	s := makeTestState(prs)
	sess := s.Sessions["slot-0"]
	sess.ReviewPendingHeadSHA = "abc123def456789"
	sess.ReviewPendingSince = backdated(90 * time.Minute)

	o.autoMergePRs(s)

	if len(*merged) != 1 || (*merged)[0] != 10 {
		t.Fatalf("merged = %v, want PR #10 — a stream that never reported must not wedge the PR forever", *merged)
	}
}

func TestAutoMergePRs_HalfObservedPairWaitsInsideGrace(t *testing.T) {
	prs := []github.PR{{Number: 10, HeadRefName: "feat/a"}}
	cfg := llmReviewTestConfig()
	cfg.ReviewRetrigger.MissingAfterMinutes = 60
	o, merged := newMergeTestOrchestrator(cfg, prs)
	o.ghPRReviewGateVerdictFn = func(int, []string) (github.ReviewGateVerdict, error) {
		return llmHalfObservedVerdict(), nil
	}
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

// Both streams green — the pair gate passes and the PR merges.
func TestAutoMergePRs_LLMReviewPairGreenMerges(t *testing.T) {
	prs := []github.PR{{Number: 10, HeadRefName: "feat/a"}}
	cfg := llmReviewTestConfig()
	o, merged := newMergeTestOrchestrator(cfg, prs)
	o.ghPRReviewGateVerdictFn = func(int, []string) (github.ReviewGateVerdict, error) {
		return github.ReviewGateVerdict{
			Passed:   true,
			Observed: true,
			Streams: []github.ReviewStreamVerdict{
				{Name: "llm-review-opus", Passed: true, Observed: true},
				{Name: "llm-review-terra", Passed: true, Observed: true},
			},
		}, nil
	}
	o.ghPRHeadSHAFn = func(int) (string, error) { return "abc123def456789", nil }

	s := makeTestState(prs)

	o.autoMergePRs(s)

	if len(*merged) != 1 || (*merged)[0] != 10 {
		t.Fatalf("merged = %v, want PR #10", *merged)
	}
}
