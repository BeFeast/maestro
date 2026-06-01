package orchestrator

import (
	"context"
	"testing"
	"time"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/github"
	"github.com/befeast/maestro/internal/notify"
	"github.com/befeast/maestro/internal/outcome"
	"github.com/befeast/maestro/internal/state"
)

// healthyOutcome returns an outcome verifier that always reports healthy.
func healthyOutcome() func(context.Context, outcome.Brief) outcome.HealthCheckResult {
	return func(_ context.Context, _ outcome.Brief) outcome.HealthCheckResult {
		return outcome.HealthCheckResult{
			CheckedAt: time.Now().UTC(),
			State:     outcome.HealthHealthy,
			Signal:    "healthcheck_command",
			Summary:   "live verifier passed",
		}
	}
}

// TestReconcile_CompletionGatesHoldDesignIssueOnHealthzPass exercises the
// Scribe redesign incident path (#443): runtime health passes, but the
// issue body / labels require live visual verification, so the supervisor
// must not close the issue or mark the session Done.
func TestReconcile_CompletionGatesHoldDesignIssueOnHealthzPass(t *testing.T) {
	cfg := &config.Config{
		Repo: "owner/repo",
		Outcome: outcome.Brief{
			DesiredOutcome:      "Live app works",
			VerifierCommand:     "check-live",
			PassRequiredForDone: boolPtr(true),
		},
		Supervisor: config.SupervisorConfig{
			SafeActions: []string{config.SupervisorActionCloseIssue},
			CompletionGates: config.SupervisorCompletionGatesConfig{
				RequiredLabels: []string{"design-handoff"},
				BodyMarkers:    []string{"live visual"},
			},
		},
	}
	closeCalled := 0
	o := &Orchestrator{
		cfg:            cfg,
		notifier:       &notify.Notifier{},
		outcomeCheckFn: healthyOutcome(),
		isPRMergedFn: func(prNumber int) (bool, error) {
			return prNumber == 10, nil
		},
		isIssueClosedFn: func(issueNumber int) (bool, error) {
			return false, nil
		},
		ghCloseIssueFn: func(number int, comment string) error {
			closeCalled++
			return nil
		},
		getIssueFn: func(number int) (github.Issue, error) {
			return makeIssue(number, "Redesign nav", "design-handoff"), nil
		},
	}

	s := state.NewState()
	s.Sessions["slot-0"] = &state.Session{
		IssueNumber: 168,
		IssueTitle:  "Redesign nav",
		Status:      state.StatusCodeLanded,
		PRNumber:    10,
	}

	o.reconcileCodeLandedSessions(s)

	if got := s.Sessions["slot-0"].Status; got != state.StatusCodeLanded {
		t.Fatalf("status = %q, want code_landed (gates must hold close)", got)
	}
	if closeCalled != 0 {
		t.Fatalf("CloseIssue called %d times; gates must block close on healthz alone", closeCalled)
	}
}

// TestReconcile_CompletionGatesIgnoredWhenIssueDoesNotMatch verifies that
// the gates only fire when the issue actually matches the configured
// labels/markers. A vanilla bug fix still closes on healthz.
func TestReconcile_CompletionGatesIgnoredWhenIssueDoesNotMatch(t *testing.T) {
	cfg := &config.Config{
		Repo: "owner/repo",
		Supervisor: config.SupervisorConfig{
			SafeActions: []string{config.SupervisorActionCloseIssue},
			CompletionGates: config.SupervisorCompletionGatesConfig{
				RequiredLabels: []string{"design-handoff"},
			},
		},
	}
	closeCalled := 0
	o := &Orchestrator{
		cfg: cfg,
		isPRMergedFn: func(prNumber int) (bool, error) {
			return prNumber == 10, nil
		},
		isIssueClosedFn: func(issueNumber int) (bool, error) {
			return false, nil
		},
		ghCloseIssueFn: func(number int, comment string) error {
			closeCalled++
			return nil
		},
		getIssueFn: func(number int) (github.Issue, error) {
			return makeIssue(number, "Plain bug", "bug"), nil
		},
	}
	s := state.NewState()
	s.Sessions["slot-0"] = &state.Session{
		IssueNumber: 42,
		Status:      state.StatusCodeLanded,
		PRNumber:    10,
	}

	o.reconcileCodeLandedSessions(s)

	if got := s.Sessions["slot-0"].Status; got != state.StatusDone {
		t.Fatalf("status = %q, want done (issue does not match gates)", got)
	}
	if closeCalled != 1 {
		t.Fatalf("CloseIssue called %d times, want 1", closeCalled)
	}
}

// TestReconcile_CompletionGatesInactiveLegacyPath confirms that projects
// without any completion-gates config get the pre-#443 close-on-healthz
// behaviour unchanged.
func TestReconcile_CompletionGatesInactiveLegacyPath(t *testing.T) {
	cfg := &config.Config{
		Repo: "owner/repo",
		Supervisor: config.SupervisorConfig{
			SafeActions: []string{config.SupervisorActionCloseIssue},
		},
	}
	getIssueCalled := false
	closeCalled := 0
	o := &Orchestrator{
		cfg: cfg,
		isPRMergedFn: func(prNumber int) (bool, error) {
			return prNumber == 10, nil
		},
		isIssueClosedFn: func(issueNumber int) (bool, error) {
			return false, nil
		},
		ghCloseIssueFn: func(number int, comment string) error {
			closeCalled++
			return nil
		},
		getIssueFn: func(number int) (github.Issue, error) {
			getIssueCalled = true
			return makeIssue(number, "Plain bug", "design-handoff"), nil
		},
	}
	s := state.NewState()
	s.Sessions["slot-0"] = &state.Session{
		IssueNumber: 42,
		Status:      state.StatusCodeLanded,
		PRNumber:    10,
	}

	o.reconcileCodeLandedSessions(s)

	if getIssueCalled {
		t.Fatalf("GetIssue must not fire when CompletionGates is inactive (legacy projects unchanged)")
	}
	if got := s.Sessions["slot-0"].Status; got != state.StatusDone {
		t.Fatalf("status = %q, want done (gates inactive)", got)
	}
	if closeCalled != 1 {
		t.Fatalf("CloseIssue called %d times, want 1", closeCalled)
	}
}

// TestMarkDoneAfterOutcomePass_BodyMarkerHoldsClose covers the case where
// the issue carries no special label but its body contains a configured
// marker (e.g. "live visual command:").
func TestMarkDoneAfterOutcomePass_BodyMarkerHoldsClose(t *testing.T) {
	cfg := &config.Config{
		Repo: "owner/repo",
		Supervisor: config.SupervisorConfig{
			SafeActions: []string{config.SupervisorActionCloseIssue},
			CompletionGates: config.SupervisorCompletionGatesConfig{
				BodyMarkers: []string{"live visual command"},
			},
		},
	}
	closeCalled := 0
	o := &Orchestrator{
		cfg:      cfg,
		notifier: &notify.Notifier{},
		ghCloseIssueFn: func(number int, comment string) error {
			closeCalled++
			return nil
		},
		isIssueClosedFn: func(issueNumber int) (bool, error) {
			return false, nil
		},
		getIssueFn: func(number int) (github.Issue, error) {
			issue := makeIssue(number, "redesign card")
			issue.Body = "## Acceptance\n- live visual command: bun run e2e/visual.spec\n"
			return issue, nil
		},
	}
	sess := &state.Session{
		IssueNumber: 200,
		Status:      state.StatusCodeLanded,
		PRNumber:    11,
	}

	o.markDoneAfterOutcomePass(sess, 11)

	if sess.Status != state.StatusCodeLanded {
		t.Fatalf("status = %q, want code_landed", sess.Status)
	}
	if closeCalled != 0 {
		t.Fatalf("CloseIssue called %d times; body marker must hold close", closeCalled)
	}
}

// TestReconcile_LiveVerificationNotifiesOnce verifies #570: holding a
// code_landed session for live verification fires the board sync + operator
// notification only once, even though reconcileCodeLandedSessions re-enters
// the hold path on every tick while the gate holds.
func TestReconcile_LiveVerificationNotifiesOnce(t *testing.T) {
	cfg := &config.Config{
		Repo:           "owner/repo",
		GitHubProjects: config.GitHubProjectsConfig{Enabled: true, ProjectNumber: 5},
		Outcome: outcome.Brief{
			DesiredOutcome:      "Live app works",
			VerifierCommand:     "check-live",
			PassRequiredForDone: boolPtr(true),
		},
		Supervisor: config.SupervisorConfig{
			SafeActions: []string{config.SupervisorActionCloseIssue},
			CompletionGates: config.SupervisorCompletionGatesConfig{
				RequiredLabels: []string{"design-handoff"},
				BodyMarkers:    []string{"live visual"},
			},
		},
	}
	syncCalls := 0
	o := &Orchestrator{
		cfg:            cfg,
		notifier:       &notify.Notifier{},
		outcomeCheckFn: healthyOutcome(),
		rateLimitFn: func() (github.RateLimitStatus, error) {
			return github.RateLimitStatus{GraphQL: github.RateLimitBucket{Limit: 5000, Remaining: 5000}}, nil
		},
		syncProjectFn: func(_ int, status github.ProjectStatus) bool {
			if status == github.ProjectStatusLiveVerify {
				syncCalls++
			}
			return true
		},
		isPRMergedFn: func(prNumber int) (bool, error) {
			return prNumber == 10, nil
		},
		isIssueClosedFn: func(issueNumber int) (bool, error) {
			return false, nil
		},
		getIssueFn: func(number int) (github.Issue, error) {
			return makeIssue(number, "Redesign nav", "design-handoff"), nil
		},
	}

	s := state.NewState()
	s.Sessions["slot-0"] = &state.Session{
		IssueNumber: 168,
		IssueTitle:  "Redesign nav",
		Status:      state.StatusCodeLanded,
		PRNumber:    10,
	}

	for i := 0; i < 3; i++ {
		o.reconcileCodeLandedSessions(s)
	}

	if syncCalls != 1 {
		t.Fatalf("live-verification board sync fired %d times across 3 reconciles, want 1 (#570 one-shot)", syncCalls)
	}
	if !s.Sessions["slot-0"].LiveVerificationNotified {
		t.Fatalf("LiveVerificationNotified flag not set after hold")
	}
	if got := s.Sessions["slot-0"].Status; got != state.StatusCodeLanded {
		t.Fatalf("status = %q, want code_landed", got)
	}
}
