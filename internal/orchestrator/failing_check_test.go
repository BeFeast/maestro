package orchestrator

import (
	"strings"
	"testing"
	"time"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/github"
	"github.com/befeast/maestro/internal/notify"
	"github.com/befeast/maestro/internal/state"
)

func TestFormatFailingCheckContext_CombinesAndDegrades(t *testing.T) {
	checks := []github.FailingCheck{
		{Name: "agent-lint", Conclusion: "failure", Excerpt: "agent-lint: possible secret detected (ghp) in added diff lines.\nagent-lint: 1 check(s) failed"},
		{Name: "flaky-check", Conclusion: "timed_out"}, // no excerpt -> degrade
	}
	got := formatFailingCheckContext(checks, failingCheckExcerptCapBytes)

	if !strings.Contains(got, "agent-lint failed (conclusion: failure):") {
		t.Errorf("named failing check with excerpt missing header: %q", got)
	}
	if !strings.Contains(got, "possible secret detected (ghp)") {
		t.Errorf("excerpt error line missing: %q", got)
	}
	if !strings.Contains(got, "flaky-check failed (conclusion: timed_out); no log excerpt available.") {
		t.Errorf("graceful degradation line missing for check without excerpt: %q", got)
	}
}

func TestFormatFailingCheckContext_EmptyWhenNoFailingChecks(t *testing.T) {
	if got := formatFailingCheckContext(nil, failingCheckExcerptCapBytes); got != "" {
		t.Errorf("no failing checks should yield empty context, got %q", got)
	}
	if got := formatFailingCheckContext([]github.FailingCheck{}, failingCheckExcerptCapBytes); got != "" {
		t.Errorf("empty failing-check slice should yield empty context, got %q", got)
	}
}

func TestFormatFailingCheckContext_RedactsSecrets(t *testing.T) {
	// A raw secret that slipped into a check log must never reach the prompt.
	checks := []github.FailingCheck{
		{Name: "agent-lint", Conclusion: "failure", Excerpt: "leaked TOKEN=ghp_abcdefghijklmnopqrstuvwxyz0123456789"},
	}
	got := formatFailingCheckContext(checks, failingCheckExcerptCapBytes)
	if strings.Contains(got, "ghp_abcdefghijklmnopqrstuvwxyz0123456789") {
		t.Errorf("raw github token leaked into failing-check context: %q", got)
	}
	if !strings.Contains(got, "REDACTED") {
		t.Errorf("expected a redaction marker, got %q", got)
	}
}

func TestCapFailingCheckExcerpt_Enforced(t *testing.T) {
	long := strings.Repeat("agent-lint error line that is fairly long\n", 500)
	capped := capFailingCheckExcerpt(long, failingCheckExcerptCapBytes)
	if len(capped) > failingCheckExcerptCapBytes {
		t.Fatalf("capped excerpt = %d bytes, exceeds cap %d", len(capped), failingCheckExcerptCapBytes)
	}
	if !strings.Contains(capped, "truncated") {
		t.Errorf("expected truncation marker, got tail %q", capped[max(0, len(capped)-80):])
	}
}

func TestCapFailingCheckExcerpt_ShortInputUnchanged(t *testing.T) {
	in := "agent-lint: 1 check failed"
	if got := capFailingCheckExcerpt(in, failingCheckExcerptCapBytes); got != in {
		t.Errorf("short input should pass through unchanged, got %q", got)
	}
}

func TestFormatFailingCheckContext_CapEnforcedOnAssembled(t *testing.T) {
	checks := []github.FailingCheck{
		{Name: "agent-lint", Conclusion: "failure", Excerpt: strings.Repeat("error: boom\n", 1000)},
	}
	got := formatFailingCheckContext(checks, failingCheckExcerptCapBytes)
	if len(got) > failingCheckExcerptCapBytes {
		t.Fatalf("assembled context = %d bytes, exceeds cap %d", len(got), failingCheckExcerptCapBytes)
	}
}

func TestAppendFailingCheckContext_AddsSection(t *testing.T) {
	base := "You are a coding agent."
	excerpt := "- agent-lint failed (conclusion: failure):\n    agent-lint: possible secret detected (pem)"
	result := appendFailingCheckContext(base, excerpt)

	if !strings.Contains(result, "You are a coding agent.") {
		t.Error("result should retain the original prompt base")
	}
	if !strings.Contains(result, "Failing Check on the Current PR Head") {
		t.Error("result should contain the failing-check header")
	}
	if !strings.Contains(result, "agent-lint: possible secret detected (pem)") {
		t.Error("result should contain the excerpt")
	}
	if !strings.Contains(result, "hard constraint on the new diff") {
		t.Error("result should frame the check as a constraint on the new diff")
	}
}

// End-to-end through the respawn loop: a review-feedback retry whose head also
// has a failing check carries BOTH the review-feedback section and a
// failing-check section naming the check, and the excerpt is consumed.
func TestRespawnDueRetries_ReviewFeedbackWithFailingCheck(t *testing.T) {
	cfg := &config.Config{
		Repo:               "owner/repo",
		MaxRetriesPerIssue: 3,
		MaxRetryBackoffMs:  300000,
		Model: config.ModelConfig{
			Default:  "claude",
			Backends: map[string]config.BackendDef{"claude": {Cmd: "claude"}},
		},
	}

	respawnedPrompt := ""
	o := &Orchestrator{
		cfg:        cfg,
		notifier:   &notify.Notifier{},
		promptBase: "base prompt {{ISSUE_NUMBER}}",
		getIssueFn: func(number int) (github.Issue, error) {
			return github.Issue{Number: 42, Title: "test issue", Body: "fix this"}, nil
		},
		respawnInPlaceFn: func(cfg *config.Config, slotName string, sess *state.Session, repo string, issue github.Issue, promptBase string, backendName string) error {
			respawnedPrompt = promptBase
			return nil
		},
	}

	s := state.NewState()
	retryAt := time.Now().UTC().Add(-1 * time.Minute)
	s.Sessions["slot-1"] = &state.Session{
		IssueNumber:                 42,
		IssueTitle:                  "test issue",
		Status:                      state.StatusDead,
		RetryCount:                  1,
		NextRetryAt:                 &retryAt,
		Backend:                     "claude",
		PRNumber:                    10,
		Worktree:                    "/tmp/wt",
		PreviousAttemptFeedback:     "Confidence 3/5\nP2: enabled flag inverted in bridge.rs",
		PreviousAttemptFeedbackKind: state.RetryReasonReviewFeedback,
		FailingCheckContext:         "- agent-lint failed (conclusion: failure):\n    agent-lint: possible secret detected (ghp) in added diff lines.",
	}

	o.respawnDueRetries(s, 10)

	if respawnedPrompt == "" {
		t.Fatal("respawnInPlaceFn should have been called")
	}
	if !strings.Contains(respawnedPrompt, "Code Review Findings") {
		t.Error("prompt should contain the review feedback section")
	}
	if !strings.Contains(respawnedPrompt, "enabled flag inverted") {
		t.Error("prompt should contain the review feedback content")
	}
	if !strings.Contains(respawnedPrompt, "Failing Check on the Current PR Head") {
		t.Error("prompt should contain the failing-check section")
	}
	if !strings.Contains(respawnedPrompt, "agent-lint: possible secret detected (ghp)") {
		t.Error("prompt should name the failing check and its excerpt")
	}
	// A review retry carries no CI-failure trigger section.
	if strings.Contains(respawnedPrompt, "Previous CI Failure") {
		t.Error("pure review retry should not include a CI-failure trigger section")
	}

	sess := s.Sessions["slot-1"]
	if sess.FailingCheckContext != "" {
		t.Errorf("FailingCheckContext should be consumed, got %q", sess.FailingCheckContext)
	}
}

// No behavior change when the head has no failing check: the review retry omits
// the failing-check section entirely.
func TestRespawnDueRetries_ReviewFeedbackNoFailingCheck(t *testing.T) {
	cfg := &config.Config{
		Repo:               "owner/repo",
		MaxRetriesPerIssue: 3,
		MaxRetryBackoffMs:  300000,
		Model: config.ModelConfig{
			Default:  "claude",
			Backends: map[string]config.BackendDef{"claude": {Cmd: "claude"}},
		},
	}

	respawnedPrompt := ""
	o := &Orchestrator{
		cfg:        cfg,
		notifier:   &notify.Notifier{},
		promptBase: "base prompt {{ISSUE_NUMBER}}",
		getIssueFn: func(number int) (github.Issue, error) {
			return github.Issue{Number: 42, Title: "test issue", Body: "fix this"}, nil
		},
		respawnInPlaceFn: func(cfg *config.Config, slotName string, sess *state.Session, repo string, issue github.Issue, promptBase string, backendName string) error {
			respawnedPrompt = promptBase
			return nil
		},
	}

	s := state.NewState()
	retryAt := time.Now().UTC().Add(-1 * time.Minute)
	s.Sessions["slot-1"] = &state.Session{
		IssueNumber:                 42,
		IssueTitle:                  "test issue",
		Status:                      state.StatusDead,
		RetryCount:                  1,
		NextRetryAt:                 &retryAt,
		Backend:                     "claude",
		PRNumber:                    10,
		Worktree:                    "/tmp/wt",
		PreviousAttemptFeedback:     "P2: rename helper",
		PreviousAttemptFeedbackKind: state.RetryReasonReviewFeedback,
		// FailingCheckContext intentionally empty.
	}

	o.respawnDueRetries(s, 10)

	if respawnedPrompt == "" {
		t.Fatal("respawnInPlaceFn should have been called")
	}
	if !strings.Contains(respawnedPrompt, "Code Review Findings") {
		t.Error("prompt should still contain the review feedback section")
	}
	if strings.Contains(respawnedPrompt, "Failing Check on the Current PR Head") {
		t.Error("no failing check present — the failing-check section must be omitted")
	}
}

// handleReviewFeedbackRetry captures a bounded failing-check excerpt naming the
// red check when the PR head has one, and captures nothing when all checks pass.
func TestHandleReviewFeedbackRetry_CapturesFailingCheck(t *testing.T) {
	newOrch := func(failing []github.FailingCheck) *Orchestrator {
		return &Orchestrator{
			cfg:      &config.Config{Repo: "owner/repo", MaxRetryBackoffMs: 300000},
			notifier: &notify.Notifier{},
			ghPRFailingChecksFn: func(prNumber int) ([]github.FailingCheck, error) {
				return failing, nil
			},
		}
	}

	t.Run("failing check captured", func(t *testing.T) {
		o := newOrch([]github.FailingCheck{
			{Name: "agent-lint", Conclusion: "failure", Excerpt: "agent-lint: forbidden artifact committed"},
		})
		s := state.NewState()
		sess := &state.Session{IssueNumber: 42, IssueTitle: "t", Worktree: "/tmp/wt"}
		s.Sessions["slot-1"] = sess

		o.handleReviewFeedbackRetry(s, "slot-1", sess, github.PR{Number: 10}, "P2: fix it")

		if sess.PreviousAttemptFeedback != "P2: fix it" {
			t.Errorf("review feedback not stored, got %q", sess.PreviousAttemptFeedback)
		}
		if !strings.Contains(sess.FailingCheckContext, "agent-lint failed (conclusion: failure)") {
			t.Errorf("failing-check excerpt not captured, got %q", sess.FailingCheckContext)
		}
		if !strings.Contains(sess.FailingCheckContext, "forbidden artifact committed") {
			t.Errorf("failing-check excerpt missing detail, got %q", sess.FailingCheckContext)
		}
	})

	t.Run("all checks green captures nothing", func(t *testing.T) {
		o := newOrch(nil)
		s := state.NewState()
		sess := &state.Session{IssueNumber: 42, IssueTitle: "t", Worktree: "/tmp/wt"}
		s.Sessions["slot-1"] = sess

		o.handleReviewFeedbackRetry(s, "slot-1", sess, github.PR{Number: 10}, "P2: fix it")

		if sess.FailingCheckContext != "" {
			t.Errorf("no failing check — FailingCheckContext must stay empty, got %q", sess.FailingCheckContext)
		}
	})

	t.Run("fetch error degrades to no excerpt", func(t *testing.T) {
		o := &Orchestrator{
			cfg:      &config.Config{Repo: "owner/repo", MaxRetryBackoffMs: 300000},
			notifier: &notify.Notifier{},
			ghPRFailingChecksFn: func(prNumber int) ([]github.FailingCheck, error) {
				return nil, errTestFetch
			},
		}
		s := state.NewState()
		sess := &state.Session{IssueNumber: 42, IssueTitle: "t", Worktree: "/tmp/wt"}
		s.Sessions["slot-1"] = sess

		o.handleReviewFeedbackRetry(s, "slot-1", sess, github.PR{Number: 10}, "P2: fix it")

		if sess.FailingCheckContext != "" {
			t.Errorf("fetch error should degrade to empty excerpt, got %q", sess.FailingCheckContext)
		}
		if sess.PreviousAttemptFeedback != "P2: fix it" {
			t.Errorf("review retry should still proceed on fetch error, got %q", sess.PreviousAttemptFeedback)
		}
	})
}

var errTestFetch = errTest("boom")

type errTest string

func (e errTest) Error() string { return string(e) }
