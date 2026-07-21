package supervisor

import (
	"testing"
	"time"

	"github.com/befeast/maestro/internal/github"
	"github.com/befeast/maestro/internal/state"
)

func TestDetectWorkerStuckStatesSuppressesPredecessorsAndSurfacesCurrentRegression(t *testing.T) {
	cfg := testConfig(t)
	reader := &fakeReader{}
	eng := testEngine(cfg, reader)
	base := time.Date(2026, 7, 21, 15, 0, 0, 0, time.UTC)
	st := state.NewState()
	st.Sessions["attempt-a"] = &state.Session{
		IssueNumber: 976, Status: state.StatusFailed, StartedAt: base.Add(100 * time.Millisecond),
		PreviousAttemptFeedbackKind: state.RetryReasonReviewFeedback,
	}
	st.Sessions["attempt-b"] = &state.Session{
		IssueNumber: 976, Status: state.StatusDead, StartedAt: base.Add(200 * time.Millisecond),
		PreviousAttemptFeedbackKind: state.RetryReasonReviewFeedback,
	}
	current := &state.Session{
		IssueNumber: 976, Status: state.StatusDone, PRNumber: 1033, StartedAt: base.Add(300 * time.Millisecond),
	}
	st.Sessions["current"] = current

	findings := eng.detectWorkerStuckStates(st, base.Add(time.Hour), newResolutionCache(eng.reader))
	for _, finding := range findings {
		if finding.Code == "stale_review_feedback" {
			t.Fatalf("predecessor review finding leaked after later done session: %#v", finding)
		}
	}

	current.Status = state.StatusFailed
	current.PreviousAttemptFeedbackKind = state.RetryReasonReviewFeedback
	findings = eng.detectWorkerStuckStates(st, base.Add(time.Hour), newResolutionCache(eng.reader))
	var reviewFindings []state.SupervisorStuckState
	for _, finding := range findings {
		if finding.Code == "stale_review_feedback" {
			reviewFindings = append(reviewFindings, finding)
		}
	}
	if len(reviewFindings) != 1 || reviewFindings[0].Target == nil || reviewFindings[0].Target.Session != "current" {
		t.Fatalf("review findings = %#v, want only current regression", reviewFindings)
	}
}

func TestDetectPRStuckStatesSuppressesPredecessorPRsAndSurfacesCurrentRegression(t *testing.T) {
	cfg := testConfig(t)
	reader := &fakeReader{ciStatuses: map[int]string{1033: "failure"}}
	eng := testEngine(cfg, reader)
	base := time.Date(2026, 7, 21, 16, 0, 0, 0, time.UTC)
	st := state.NewState()
	st.Sessions["attempt-a"] = &state.Session{
		IssueNumber: 976, Status: state.StatusFailed, PRNumber: 1031, Branch: "feat/attempt-a", StartedAt: base.Add(100 * time.Millisecond),
	}
	st.Sessions["attempt-b"] = &state.Session{
		IssueNumber: 976, Status: state.StatusDead, PRNumber: 1032, Branch: "feat/attempt-b", StartedAt: base.Add(200 * time.Millisecond),
	}
	current := &state.Session{
		IssueNumber: 976, Status: state.StatusDone, PRNumber: 1033, Branch: "feat/current", StartedAt: base.Add(300 * time.Millisecond),
	}
	st.Sessions["current"] = current

	findings := eng.detectPRStuckStates(st, nil, newResolutionCache(eng.reader))
	if len(findings) != 0 {
		t.Fatalf("historical predecessor PRs produced stuck findings: %#v", findings)
	}

	current.Status = state.StatusFailed
	reader.prs = []github.PR{{Number: 1033, HeadRefName: "feat/current", State: "OPEN", Mergeable: "MERGEABLE"}}
	findings = eng.detectPRStuckStates(st, reader.prs, newResolutionCache(eng.reader))
	foundCurrentFailure := false
	for _, finding := range findings {
		if finding.Target != nil && (finding.Target.Session == "attempt-a" || finding.Target.Session == "attempt-b") {
			t.Fatalf("predecessor consumed PR stuck attention: %#v", finding)
		}
		if finding.Code == "failing_checks" && finding.Target != nil && finding.Target.Session == "current" {
			foundCurrentFailure = true
		}
	}
	if !foundCurrentFailure {
		t.Fatalf("current regressed PR was not surfaced: %#v", findings)
	}
}

func TestDetectWorkerStuckStatesUsesActiveSharedPRContinuation(t *testing.T) {
	cfg := testConfig(t)
	reader := &fakeReader{}
	eng := testEngine(cfg, reader)
	base := time.Date(2026, 7, 21, 17, 0, 0, 0, time.UTC)
	st := state.NewState()
	st.Sessions["source"] = &state.Session{
		IssueNumber: 900, Status: state.StatusDead, PRNumber: 1033, StartedAt: base,
		PreviousAttemptFeedbackKind: state.RetryReasonReviewFeedback,
	}
	continuation := &state.Session{
		IssueNumber: 976, Status: state.StatusPROpen, PRNumber: 1033, StartedAt: base.Add(time.Second),
	}
	st.Sessions["continuation"] = continuation

	findings := eng.detectWorkerStuckStates(st, base.Add(time.Hour), newResolutionCache(eng.reader))
	for _, finding := range findings {
		if finding.Target != nil && finding.Target.Session == "source" {
			t.Fatalf("declared continuation left source actionable: %#v", finding)
		}
	}

	continuation.Status = state.StatusFailed
	continuation.PreviousAttemptFeedbackKind = state.RetryReasonReviewFeedback
	findings = eng.detectWorkerStuckStates(st, base.Add(time.Hour), newResolutionCache(eng.reader))
	var reviewTargets []string
	for _, finding := range findings {
		if finding.Code == "stale_review_feedback" && finding.Target != nil {
			reviewTargets = append(reviewTargets, finding.Target.Session)
		}
	}
	if len(reviewTargets) != 1 || reviewTargets[0] != "continuation" {
		t.Fatalf("review targets = %v, want only continuation", reviewTargets)
	}
}
