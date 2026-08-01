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
