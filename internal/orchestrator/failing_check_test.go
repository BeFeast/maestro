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
		{Name: "agent-lint", Conclusion: "failure", Excerpt: "agent-lint: forbidden artifact committed\nagent-lint: 1 check(s) failed"},
		{Name: "flaky-check", Conclusion: "timed_out"}, // no excerpt -> degrade
	}
	got := formatFailingCheckContext(checks, failingCheckExcerptCapBytes)

	if !strings.Contains(got, "agent-lint failed (conclusion: failure):") {
		t.Errorf("named failing check with excerpt missing header: %q", got)
	}
	if !strings.Contains(got, "forbidden artifact committed") {
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
	// Build the github-token-shaped literal at RUNTIME so this source file never
	// contains a contiguous secret pattern of its own — tripping the secret
	// scanner with a fixture literal is the exact failure class #857 exists to
	// stop (the PR #850 lint blindness).
	secret := "ghp_" + strings.Repeat("A", 36)
	checks := []github.FailingCheck{
		{Name: "agent-lint", Conclusion: "failure", Excerpt: "leaked TOKEN=" + secret},
	}
	got := formatFailingCheckContext(checks, failingCheckExcerptCapBytes)
	if strings.Contains(got, secret) {
		t.Error("raw github token leaked into failing-check context")
	}
	if !strings.Contains(got, "REDACTED") {
		t.Error("expected a redaction marker in the redacted excerpt")
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
	excerpt := "- agent-lint failed (conclusion: failure):\n    agent-lint: forbidden artifact committed"
	result := appendFailingCheckContext(base, excerpt)

	if !strings.Contains(result, "You are a coding agent.") {
		t.Error("result should retain the original prompt base")
	}
	if !strings.Contains(result, "Failing Check on the Current PR Head") {
		t.Error("result should contain the failing-check header")
	}
	if !strings.Contains(result, "agent-lint: forbidden artifact committed") {
		t.Error("result should contain the excerpt")
	}
	if !strings.Contains(result, "hard constraint on the new diff") {
		t.Error("result should frame the check as a constraint on the new diff")
	}
}

// failingCheckOrchestrator builds an orchestrator whose respawn hooks record the
// assembled prompt so an end-to-end respawn can be asserted against.
func failingCheckOrchestrator(captured *string) *Orchestrator {
	cfg := &config.Config{
		Repo:               "owner/repo",
		MaxRetriesPerIssue: 3,
		MaxRetryBackoffMs:  300000,
		Model: config.ModelConfig{
			Default:  "claude",
			Backends: map[string]config.BackendDef{"claude": {Cmd: "claude"}},
		},
	}
	record := func(cfg *config.Config, slotName string, sess *state.Session, repo string, issue github.Issue, promptBase string, backendName string) error {
		*captured = promptBase
		sess.Status = state.StatusRunning
		return nil
	}
	return &Orchestrator{
		cfg:        cfg,
		notifier:   &notify.Notifier{},
		promptBase: "base prompt {{ISSUE_NUMBER}}",
		getIssueFn: func(number int) (github.Issue, error) {
			return github.Issue{Number: 42, Title: "test issue", Body: "fix this"}, nil
		},
		// Never treat the test PR/issue as merged/closed so the retry proceeds.
		isPRMergedFn:     func(int) (bool, error) { return false, nil },
		isIssueClosedFn:  func(int) (bool, error) { return false, nil },
		respawnInPlaceFn: record,
		respawnWorkerFn:  record,
	}
}

// End-to-end through the respawn loop: a review-feedback retry whose head also
// has a failing check carries BOTH the review-feedback section and a
// failing-check section naming the check, and the excerpt is consumed.
func TestRespawnDueRetries_ReviewFeedbackWithFailingCheck(t *testing.T) {
	respawnedPrompt := ""
	o := failingCheckOrchestrator(&respawnedPrompt)

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
		FailingCheckContext:         "- agent-lint failed (conclusion: failure):\n    agent-lint: forbidden artifact committed",
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
	if !strings.Contains(respawnedPrompt, "agent-lint: forbidden artifact committed") {
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
	respawnedPrompt := ""
	o := failingCheckOrchestrator(&respawnedPrompt)

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

// handleReviewFeedbackRetry (the #424-unstable / non-required-check path)
// captures a bounded failing-check excerpt when the PR head has one, and
// captures nothing when all checks pass.
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

// handleCIFailureRetry is the path a red REQUIRED check (e.g. agent-lint) really
// takes — its failure conclusion makes the aggregate CI verdict "failure". This
// is the case the PR #850 lint blindness hit, so the concrete failing-check
// excerpt must be captured here too, not only in the review-feedback path
// (#857, addressing the "success-only branch" review concern).
func TestHandleCIFailureRetry_CapturesFailingCheck(t *testing.T) {
	captureFor := func(failing []github.FailingCheck) *state.Session {
		o := &Orchestrator{
			cfg:                         &config.Config{Repo: "owner/repo", MaxRetriesPerIssue: 3, MaxRetryBackoffMs: 300000},
			notifier:                    &notify.Notifier{},
			ghPRChecksOutputFn:          func(int) (string, error) { return "agent-lint\tfailure\nbuild\tsuccess\n", nil },
			ghCollectPRReviewFeedbackFn: func(int) (string, error) { return "", nil },
			ghPRFailingChecksFn:         func(int) ([]github.FailingCheck, error) { return failing, nil },
			ghClosePRFn:                 func(int, string) error { return nil },
			workerStopFn:                func(*config.Config, string, *state.Session) error { return nil },
		}
		s := state.NewState()
		sess := &state.Session{IssueNumber: 42, IssueTitle: "t", Status: state.StatusPROpen, PRNumber: 10, Worktree: "/tmp/wt"}
		s.Sessions["slot-1"] = sess
		o.handleCIFailureRetry(s, "slot-1", sess, github.PR{Number: 10})
		return sess
	}

	t.Run("red check excerpt captured alongside CI overview", func(t *testing.T) {
		sess := captureFor([]github.FailingCheck{
			{Name: "agent-lint", Conclusion: "failure", Excerpt: "agent-lint: forbidden artifact committed"},
		})
		if sess.CIFailureOutput == "" {
			t.Error("existing CI-failure overview must still be captured (path unchanged)")
		}
		if !strings.Contains(sess.FailingCheckContext, "agent-lint failed (conclusion: failure)") {
			t.Errorf("concrete failing-check excerpt not captured on CI-failure path, got %q", sess.FailingCheckContext)
		}
		if !strings.Contains(sess.FailingCheckContext, "forbidden artifact committed") {
			t.Errorf("failing-check excerpt missing concrete detail, got %q", sess.FailingCheckContext)
		}
	})

	t.Run("no failing check leaves excerpt empty", func(t *testing.T) {
		sess := captureFor(nil)
		if sess.FailingCheckContext != "" {
			t.Errorf("no failing check — FailingCheckContext must stay empty, got %q", sess.FailingCheckContext)
		}
	})
}

var errTestFetch = errTest("boom")

type errTest string

func (e errTest) Error() string { return string(e) }
