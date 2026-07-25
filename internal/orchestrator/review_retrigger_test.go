package orchestrator

import (
	"testing"
	"time"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/github"
)

// newRetriggerTestOrchestrator wires a merge-flow orchestrator whose greptile
// stream reports pending (the #691 webhook-miss wedge: CI green, zero review
// signal on head) and records every "@greptile review" comment posted.
func newRetriggerTestOrchestrator(cfg *config.Config, prs []github.PR, headSHA string) (*Orchestrator, *[]string) {
	o, _ := newMergeTestOrchestrator(cfg, prs)
	o.ghPRGreptileApprovedFn = func(prNumber int) (bool, bool, error) {
		return false, true, nil // greptile=pending
	}
	o.ghPRHeadSHAFn = func(prNumber int) (string, error) {
		return headSHA, nil
	}
	comments := make([]string, 0)
	o.ghCommentPRFn = func(prNumber int, body string) error {
		comments = append(comments, body)
		return nil
	}
	return o, &comments
}

func retriggerTestConfig() *config.Config {
	return &config.Config{Repo: "owner/repo", ReviewGate: "greptile"}
}

func backdated(d time.Duration) *time.Time {
	t := time.Now().UTC().Add(-d)
	return &t
}

func TestReviewRetrigger_FirstPendingObservationStartsClock(t *testing.T) {
	prs := []github.PR{{Number: 10, HeadRefName: "feat/a"}}
	o, comments := newRetriggerTestOrchestrator(retriggerTestConfig(), prs, "abc123def456789")
	s := makeTestState(prs)

	o.autoMergePRs(s)

	if len(*comments) != 0 {
		t.Fatalf("comments = %v, want none on first pending observation", *comments)
	}
	sess := s.Sessions["slot-0"]
	if sess.ReviewPendingHeadSHA != "abc123def456789" {
		t.Errorf("ReviewPendingHeadSHA = %q, want head SHA recorded", sess.ReviewPendingHeadSHA)
	}
	if sess.ReviewPendingSince == nil {
		t.Errorf("ReviewPendingSince = nil, want pending clock started")
	}
}

func TestReviewRetrigger_PostsAfterPendingThreshold(t *testing.T) {
	prs := []github.PR{{Number: 10, HeadRefName: "feat/a"}}
	o, comments := newRetriggerTestOrchestrator(retriggerTestConfig(), prs, "abc123def456789")
	s := makeTestState(prs)
	sess := s.Sessions["slot-0"]
	sess.ReviewPendingHeadSHA = "abc123def456789"
	sess.ReviewPendingSince = backdated(11 * time.Minute) // default threshold is 10m

	o.autoMergePRs(s)

	if len(*comments) != 1 || (*comments)[0] != "@greptile review" {
		t.Fatalf("comments = %v, want one %q", *comments, "@greptile review")
	}
	if sess.ReviewRetriggerAt == nil {
		t.Fatalf("ReviewRetriggerAt = nil, want cooldown anchor stamped")
	}

	// The very next cycle must not re-post: still pending, cooldown active.
	o.autoMergePRs(s)
	if len(*comments) != 1 {
		t.Fatalf("comments = %v, want exactly one re-trigger within cooldown", *comments)
	}
}

func TestReviewRetrigger_UnderThresholdDoesNotPost(t *testing.T) {
	prs := []github.PR{{Number: 10, HeadRefName: "feat/a"}}
	o, comments := newRetriggerTestOrchestrator(retriggerTestConfig(), prs, "abc123def456789")
	s := makeTestState(prs)
	sess := s.Sessions["slot-0"]
	sess.ReviewPendingHeadSHA = "abc123def456789"
	sess.ReviewPendingSince = backdated(5 * time.Minute)

	o.autoMergePRs(s)

	if len(*comments) != 0 {
		t.Fatalf("comments = %v, want none under the pending threshold", *comments)
	}
}

func TestReviewRetrigger_CooldownElapsedRepostsOnce(t *testing.T) {
	prs := []github.PR{{Number: 10, HeadRefName: "feat/a"}}
	o, comments := newRetriggerTestOrchestrator(retriggerTestConfig(), prs, "abc123def456789")
	s := makeTestState(prs)
	sess := s.Sessions["slot-0"]
	sess.ReviewPendingHeadSHA = "abc123def456789"
	sess.ReviewPendingSince = backdated(45 * time.Minute)
	sess.ReviewRetriggerAt = backdated(31 * time.Minute) // default cooldown is 30m

	o.autoMergePRs(s)

	if len(*comments) != 1 {
		t.Fatalf("comments = %v, want one re-post after cooldown elapsed", *comments)
	}
}

func TestReviewRetrigger_CooldownActiveSuppressesRepost(t *testing.T) {
	prs := []github.PR{{Number: 10, HeadRefName: "feat/a"}}
	o, comments := newRetriggerTestOrchestrator(retriggerTestConfig(), prs, "abc123def456789")
	s := makeTestState(prs)
	sess := s.Sessions["slot-0"]
	sess.ReviewPendingHeadSHA = "abc123def456789"
	sess.ReviewPendingSince = backdated(45 * time.Minute)
	sess.ReviewRetriggerAt = backdated(5 * time.Minute)

	o.autoMergePRs(s)

	if len(*comments) != 0 {
		t.Fatalf("comments = %v, want none while cooldown is active", *comments)
	}
}

func TestReviewRetrigger_HeadChangeRestartsClock(t *testing.T) {
	// After a push or server-side update-branch the head SHA changes and
	// greptile gets a fresh chance to deliver its webhook — the pending
	// clock must restart instead of firing on stale elapsed time.
	prs := []github.PR{{Number: 10, HeadRefName: "feat/a"}}
	o, comments := newRetriggerTestOrchestrator(retriggerTestConfig(), prs, "newhead9876543")
	s := makeTestState(prs)
	sess := s.Sessions["slot-0"]
	sess.ReviewPendingHeadSHA = "oldhead1234567"
	sess.ReviewPendingSince = backdated(45 * time.Minute)

	o.autoMergePRs(s)

	if len(*comments) != 0 {
		t.Fatalf("comments = %v, want none right after head change", *comments)
	}
	if sess.ReviewPendingHeadSHA != "newhead9876543" {
		t.Errorf("ReviewPendingHeadSHA = %q, want new head recorded", sess.ReviewPendingHeadSHA)
	}
	if sess.ReviewPendingSince == nil || time.Since(*sess.ReviewPendingSince) > time.Minute {
		t.Errorf("ReviewPendingSince = %v, want clock restarted", sess.ReviewPendingSince)
	}
}

func TestReviewRetrigger_DisabledDoesNothing(t *testing.T) {
	off := false
	cfg := retriggerTestConfig()
	cfg.ReviewRetrigger.Enabled = &off
	prs := []github.PR{{Number: 10, HeadRefName: "feat/a"}}
	o, comments := newRetriggerTestOrchestrator(cfg, prs, "abc123def456789")
	s := makeTestState(prs)
	sess := s.Sessions["slot-0"]
	sess.ReviewPendingHeadSHA = "abc123def456789"
	sess.ReviewPendingSince = backdated(45 * time.Minute)

	o.autoMergePRs(s)

	if len(*comments) != 0 {
		t.Fatalf("comments = %v, want none when review_retrigger is disabled", *comments)
	}
}

func TestReviewRetrigger_ConfigurableThresholdAndCooldown(t *testing.T) {
	cfg := retriggerTestConfig()
	cfg.ReviewRetrigger.PendingMinutes = 20
	cfg.ReviewRetrigger.CooldownMinutes = 60
	prs := []github.PR{{Number: 10, HeadRefName: "feat/a"}}
	o, comments := newRetriggerTestOrchestrator(cfg, prs, "abc123def456789")
	s := makeTestState(prs)
	sess := s.Sessions["slot-0"]
	sess.ReviewPendingHeadSHA = "abc123def456789"
	sess.ReviewPendingSince = backdated(15 * time.Minute) // < 20m custom threshold

	o.autoMergePRs(s)
	if len(*comments) != 0 {
		t.Fatalf("comments = %v, want none under custom 20m threshold", *comments)
	}

	sess.ReviewPendingSince = backdated(25 * time.Minute)
	sess.ReviewRetriggerAt = backdated(45 * time.Minute) // < 60m custom cooldown
	o.autoMergePRs(s)
	if len(*comments) != 0 {
		t.Fatalf("comments = %v, want none within custom 60m cooldown", *comments)
	}

	sess.ReviewRetriggerAt = backdated(61 * time.Minute)
	o.autoMergePRs(s)
	if len(*comments) != 1 {
		t.Fatalf("comments = %v, want one re-trigger past custom threshold+cooldown", *comments)
	}
}

func TestReviewRetrigger_NonGreptilePendingStreamIgnored(t *testing.T) {
	cfg := retriggerTestConfig()
	cfg.ReviewGateStreams = []string{"greptile", "simplicity"}
	prs := []github.PR{{Number: 10, HeadRefName: "feat/a"}}
	o, comments := newRetriggerTestOrchestrator(cfg, prs, "abc123def456789")
	o.ghPRReviewGateVerdictFn = func(prNumber int, streams []string) (github.ReviewGateVerdict, error) {
		return github.ReviewGateVerdict{
			Passed:  false,
			Pending: true,
			Streams: []github.ReviewStreamVerdict{
				{Name: "greptile", Passed: true},
				{Name: "simplicity", Pending: true},
			},
		}, nil
	}
	s := makeTestState(prs)
	sess := s.Sessions["slot-0"]
	sess.ReviewPendingHeadSHA = "abc123def456789"
	sess.ReviewPendingSince = backdated(45 * time.Minute)

	o.autoMergePRs(s)

	if len(*comments) != 0 {
		t.Fatalf("comments = %v, want none when only a non-greptile stream is pending", *comments)
	}
}

func TestReviewRetrigger_GateResolutionClearsTracking(t *testing.T) {
	prs := []github.PR{{Number: 10, HeadRefName: "feat/a"}}
	o, _ := newRetriggerTestOrchestrator(retriggerTestConfig(), prs, "abc123def456789")
	o.ghPRGreptileApprovedFn = func(prNumber int) (bool, bool, error) {
		return false, false, nil // resolved: blocked by findings, not pending
	}
	s := makeTestState(prs)
	sess := s.Sessions["slot-0"]
	sess.ReviewPendingHeadSHA = "abc123def456789"
	sess.ReviewPendingSince = backdated(45 * time.Minute)

	o.autoMergePRs(s)

	// The clock stops when the gate resolves, but the head anchor stays: a
	// check that settles and goes pending again on the same commit must not
	// look like a new head, or the per-head re-trigger cap would reset on
	// every such flap.
	if sess.ReviewPendingSince != nil {
		t.Fatalf("pending clock = %v, want stopped once the gate resolves", sess.ReviewPendingSince)
	}
	if sess.ReviewPendingHeadSHA != "abc123def456789" {
		t.Fatalf("head anchor = %q, want it retained until the head actually changes", sess.ReviewPendingHeadSHA)
	}
}

// Workflow review catch: the per-head cap must actually suppress comments.
func TestReviewRetrigger_MaxAttemptsCapSuppressesComment(t *testing.T) {
	prs := []github.PR{{Number: 10, HeadRefName: "feat/a"}}
	cfg := retriggerTestConfig()
	cfg.ReviewRetrigger.MaxAttempts = 2
	o, comments := newRetriggerTestOrchestrator(cfg, prs, "abc123def456789")
	s := makeTestState(prs)
	sess := s.Sessions["slot-0"]
	sess.ReviewPendingHeadSHA = "abc123def456789"
	sess.ReviewPendingSince = backdated(11 * time.Minute)
	sess.ReviewRetriggerCount = 2 // cap already reached

	o.autoMergePRs(s)

	if len(*comments) != 0 {
		t.Fatalf("comments = %v, want none once the per-head cap is reached", *comments)
	}
	if sess.ReviewRetriggerCount != 2 {
		t.Fatalf("count = %d, want it to stay at the cap", sess.ReviewRetriggerCount)
	}
}

// Below the cap the comment is posted and the counter advances.
func TestReviewRetrigger_CountsAttemptsPerHead(t *testing.T) {
	prs := []github.PR{{Number: 10, HeadRefName: "feat/a"}}
	cfg := retriggerTestConfig()
	cfg.ReviewRetrigger.MaxAttempts = 2
	o, comments := newRetriggerTestOrchestrator(cfg, prs, "abc123def456789")
	s := makeTestState(prs)
	sess := s.Sessions["slot-0"]
	sess.ReviewPendingHeadSHA = "abc123def456789"
	sess.ReviewPendingSince = backdated(11 * time.Minute)

	o.autoMergePRs(s)

	if len(*comments) != 1 {
		t.Fatalf("comments = %v, want one below the cap", *comments)
	}
	if sess.ReviewRetriggerCount != 1 {
		t.Fatalf("count = %d, want 1 after the first re-trigger", sess.ReviewRetriggerCount)
	}
}
