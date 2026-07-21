package state

import (
	"testing"
	"time"

	"github.com/befeast/maestro/internal/outcome"
)

func failingOutcome(checkName string) outcome.HealthCheckResult {
	return outcome.HealthCheckResult{
		State:  outcome.HealthFailing,
		Signal: "healthcheck_command",
		Checks: []outcome.HealthCheckItem{
			{Name: checkName, Blocking: true, Status: outcome.HealthFailing},
		},
	}
}

func TestOutcomeFailureFingerprint(t *testing.T) {
	// Non-failing results have no fingerprint.
	for _, st := range []string{outcome.HealthHealthy, outcome.HealthPending, outcome.HealthUnknown} {
		if fp := OutcomeFailureFingerprint(outcome.HealthCheckResult{State: st}); fp != "" {
			t.Fatalf("state %q: expected empty fingerprint, got %q", st, fp)
		}
	}

	// Same failing blocking check => same fingerprint regardless of summary noise.
	a := failingOutcome("ok-player-boots")
	a.Summary = "run at 2026-07-20T01:02:03Z on host-a"
	b := failingOutcome("ok-player-boots")
	b.Summary = "run at 2026-07-21T09:09:09Z on host-b"
	if OutcomeFailureFingerprint(a) == "" {
		t.Fatalf("expected non-empty fingerprint for failing check")
	}
	if OutcomeFailureFingerprint(a) != OutcomeFailureFingerprint(b) {
		t.Fatalf("same blocking check should fingerprint identically: %q vs %q",
			OutcomeFailureFingerprint(a), OutcomeFailureFingerprint(b))
	}

	// A different failing check yields a different fingerprint.
	if OutcomeFailureFingerprint(a) == OutcomeFailureFingerprint(failingOutcome("some-other-check")) {
		t.Fatalf("different blocking checks must not share a fingerprint")
	}

	// Falls back to signal+exit code when no structured blocking checks exist.
	sig := outcome.HealthCheckResult{State: outcome.HealthFailing, Signal: "healthcheck_url", ExitCode: 7}
	if got := OutcomeFailureFingerprint(sig); got != "signal:healthcheck_url:exit:7" {
		t.Fatalf("signal fallback fingerprint = %q", got)
	}
}

func TestCodeLandedIneffective(t *testing.T) {
	now := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	past := now.Add(-time.Minute)
	future := now.Add(time.Minute)
	failing := failingOutcome("ok-player-boots")
	fp := OutcomeFailureFingerprint(failing)

	base := func() *Session {
		return &Session{
			Status:                    StatusCodeLanded,
			CodeLandedVerifyDeadline:  &past,
			OutcomeFailureFingerprint: fp,
		}
	}

	t.Run("deadline passed with matching fingerprint is ineffective", func(t *testing.T) {
		if !CodeLandedIneffective(base(), failing, now) {
			t.Fatalf("expected ineffective when deadline passed and fingerprint matches")
		}
	})

	t.Run("deadline in future holds", func(t *testing.T) {
		sess := base()
		sess.CodeLandedVerifyDeadline = &future
		if CodeLandedIneffective(sess, failing, now) {
			t.Fatalf("must not convict before the verification deadline")
		}
	})

	t.Run("unarmed deadline holds", func(t *testing.T) {
		sess := base()
		sess.CodeLandedVerifyDeadline = nil
		if CodeLandedIneffective(sess, failing, now) {
			t.Fatalf("must not convict before a deadline is armed")
		}
	})

	t.Run("changed fingerprint holds", func(t *testing.T) {
		if CodeLandedIneffective(base(), failingOutcome("a-new-different-failure"), now) {
			t.Fatalf("a different failure fingerprint must re-arm, not convict")
		}
	})

	t.Run("healthy outcome holds", func(t *testing.T) {
		if CodeLandedIneffective(base(), outcome.HealthCheckResult{State: outcome.HealthHealthy}, now) {
			t.Fatalf("a recovered outcome must not be judged ineffective")
		}
	})

	t.Run("already released does not re-trigger", func(t *testing.T) {
		sess := base()
		sess.ReleasedForRedispatch = true
		if CodeLandedIneffective(sess, failing, now) {
			t.Fatalf("an already-released session must not re-trigger")
		}
	})

	t.Run("non code_landed status holds", func(t *testing.T) {
		sess := base()
		sess.Status = StatusDone
		if CodeLandedIneffective(sess, failing, now) {
			t.Fatalf("only code_landed sessions are subject to the guard")
		}
	})
}

func TestCodeLandedReleasedDropsIssueClaim(t *testing.T) {
	s := NewState()
	s.Sessions["slot-a"] = &Session{
		IssueNumber: 486,
		Status:      StatusCodeLanded,
		PRNumber:    900,
	}

	if !s.IssueInProgress(486) {
		t.Fatalf("a fresh code_landed session must claim its issue")
	}

	// Release it as a record-only delivery (docs-only merge).
	s.Sessions["slot-a"].ReleasedForRedispatch = true
	s.Sessions["slot-a"].WorkerOutcome = WorkerOutcomeRecordOnlyDelivery

	if s.IssueInProgress(486) {
		t.Fatalf("a released code_landed session must not keep the issue claimed")
	}
	if s.IssueDone(486) {
		t.Fatalf("a released code_landed session must not report the issue done")
	}
}
