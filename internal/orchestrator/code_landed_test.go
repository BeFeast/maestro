package orchestrator

import (
	"fmt"
	"testing"
	"time"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/github"
	"github.com/befeast/maestro/internal/notify"
	"github.com/befeast/maestro/internal/outcome"
	"github.com/befeast/maestro/internal/state"
)

func codeLandedTestOrchestrator(labels []string, changed []string, changedErr error) *Orchestrator {
	cfg := &config.Config{Repo: "owner/repo"}
	ghLabels := make([]struct {
		Name string `json:"name"`
	}, 0, len(labels))
	for _, l := range labels {
		ghLabels = append(ghLabels, struct {
			Name string `json:"name"`
		}{Name: l})
	}
	return &Orchestrator{
		cfg:      cfg,
		notifier: &notify.Notifier{},
		getIssueFn: func(number int) (github.Issue, error) {
			return github.Issue{Number: number, Labels: ghLabels}, nil
		},
		ghPRChangedFilesFn: func(prNumber int) ([]string, error) {
			return changed, changedErr
		},
	}
}

func TestClassifyMergedCodeLanded_DocsOnlyBugReleased(t *testing.T) {
	o := codeLandedTestOrchestrator([]string{"bug"}, []string{"docs/qa/ok-player-486.md"}, nil)
	s := state.NewState()
	sess := &state.Session{IssueNumber: 486, Status: state.StatusCodeLanded, PRNumber: 900}
	s.Sessions["slot-a"] = sess

	if got := o.classifyMergedCodeLandedDelivery(s, sess); got != codeLandedRecordOnly {
		t.Fatalf("classify = %v, want codeLandedRecordOnly", got)
	}
	if !sess.ReleasedForRedispatch {
		t.Fatalf("docs-only bug session must be released for redispatch")
	}
	if sess.WorkerOutcome != state.WorkerOutcomeRecordOnlyDelivery {
		t.Fatalf("WorkerOutcome = %q, want %q", sess.WorkerOutcome, state.WorkerOutcomeRecordOnlyDelivery)
	}
	if s.IssueInProgress(486) {
		t.Fatalf("released docs-only session must not keep issue #486 in progress")
	}
}

func TestClassifyMergedCodeLanded_FunctionalBugSettles(t *testing.T) {
	o := codeLandedTestOrchestrator([]string{"bug"}, []string{"docs/note.md", "internal/state/state.go"}, nil)
	s := state.NewState()
	sess := &state.Session{IssueNumber: 487, Status: state.StatusCodeLanded, PRNumber: 901}
	s.Sessions["slot-b"] = sess

	if got := o.classifyMergedCodeLandedDelivery(s, sess); got != codeLandedSettle {
		t.Fatalf("classify = %v, want codeLandedSettle", got)
	}
	if sess.ReleasedForRedispatch {
		t.Fatalf("a real code fix must settle, not release")
	}
	if !s.IssueInProgress(487) {
		t.Fatalf("a settling code_landed session must keep its issue claim")
	}
}

func TestClassifyMergedCodeLanded_NonBugDocsSettles(t *testing.T) {
	o := codeLandedTestOrchestrator([]string{"enhancement"}, []string{"docs/guide.md"}, nil)
	s := state.NewState()
	sess := &state.Session{IssueNumber: 488, Status: state.StatusCodeLanded, PRNumber: 902}
	s.Sessions["slot-c"] = sess

	if got := o.classifyMergedCodeLandedDelivery(s, sess); got != codeLandedSettle {
		t.Fatalf("classify = %v, want codeLandedSettle (only bug issues are guarded)", got)
	}
	if sess.ReleasedForRedispatch {
		t.Fatalf("a non-bug docs delivery must settle as today")
	}
}

func TestClassifyMergedCodeLanded_ChangedFilesErrorSettles(t *testing.T) {
	// A changed-files read failure must fall through to the legacy settle path,
	// not hold — otherwise a transient forge blip would strand every normal PR.
	o := codeLandedTestOrchestrator([]string{"bug"}, nil, fmt.Errorf("boom"))
	s := state.NewState()
	sess := &state.Session{IssueNumber: 489, Status: state.StatusCodeLanded, PRNumber: 903}
	s.Sessions["slot-d"] = sess

	if got := o.classifyMergedCodeLandedDelivery(s, sess); got != codeLandedSettle {
		t.Fatalf("classify = %v, want codeLandedSettle on changed-files read error", got)
	}
	if sess.ReleasedForRedispatch {
		t.Fatalf("a changed-files read error must not release the session")
	}
}

func TestClassifyMergedCodeLanded_DocsOnlyIssueReadErrorHolds(t *testing.T) {
	// The diff is confirmed non-functional, but the issue read (needed to check
	// the bug label) failed: hold and retry rather than settle a possible
	// docs-only bug delivery on incomplete evidence.
	o := codeLandedTestOrchestrator([]string{"bug"}, []string{"docs/qa/rec.md"}, nil)
	o.getIssueFn = func(number int) (github.Issue, error) {
		return github.Issue{}, fmt.Errorf("issue read boom")
	}
	s := state.NewState()
	sess := &state.Session{IssueNumber: 489, Status: state.StatusCodeLanded, PRNumber: 903}
	s.Sessions["slot-d"] = sess

	if got := o.classifyMergedCodeLandedDelivery(s, sess); got != codeLandedHold {
		t.Fatalf("classify = %v, want codeLandedHold when a docs-only PR's issue read fails", got)
	}
	if sess.ReleasedForRedispatch {
		t.Fatalf("an issue read error must not release the session")
	}
}

func TestReconcileIneffectiveCodeLanded_ArmsThenConvicts(t *testing.T) {
	o := codeLandedTestOrchestrator([]string{"bug"}, []string{"internal/state/state.go"}, nil)
	s := state.NewState()
	sess := &state.Session{IssueNumber: 490, Status: state.StatusCodeLanded, PRNumber: 904}
	s.Sessions["slot-e"] = sess

	failing := outcome.HealthCheckResult{
		State:  outcome.HealthFailing,
		Signal: "healthcheck_command",
		Checks: []outcome.HealthCheckItem{{Name: "ok-player-boots", Blocking: true, Status: outcome.HealthFailing}},
	}
	t0 := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)

	// First observation arms the deadline; must not convict yet.
	if o.reconcileIneffectiveCodeLanded(s, sess, failing, t0) {
		t.Fatalf("first failing observation must arm, not convict")
	}
	if sess.CodeLandedVerifyDeadline == nil || sess.OutcomeFailureFingerprint == "" {
		t.Fatalf("deadline and fingerprint must be armed on first observation")
	}
	if sess.ReleasedForRedispatch {
		t.Fatalf("must not release before the deadline elapses")
	}

	// Same fingerprint, still before deadline: keep holding.
	if o.reconcileIneffectiveCodeLanded(s, sess, failing, t0.Add(time.Minute)) {
		t.Fatalf("must not convict before the verification deadline")
	}

	// Same fingerprint, past deadline: ineffective -> released.
	past := t0.Add(o.codeLandedVerifyGrace() + time.Minute)
	if !o.reconcileIneffectiveCodeLanded(s, sess, failing, past) {
		t.Fatalf("same failing fingerprint past the deadline must convict")
	}
	if !sess.ReleasedForRedispatch || sess.WorkerOutcome != state.WorkerOutcomeCodeLandedIneffective {
		t.Fatalf("convicted session must be released as code_landed_ineffective, got released=%v outcome=%q",
			sess.ReleasedForRedispatch, sess.WorkerOutcome)
	}
	if s.IssueInProgress(490) {
		t.Fatalf("released ineffective session must not keep issue #490 in progress")
	}
}

func TestReconcileIneffectiveCodeLanded_ChangedFingerprintReArms(t *testing.T) {
	o := codeLandedTestOrchestrator([]string{"bug"}, nil, nil)
	s := state.NewState()
	sess := &state.Session{IssueNumber: 491, Status: state.StatusCodeLanded, PRNumber: 905}
	s.Sessions["slot-f"] = sess

	first := outcome.HealthCheckResult{
		State:  outcome.HealthFailing,
		Checks: []outcome.HealthCheckItem{{Name: "check-a", Blocking: true, Status: outcome.HealthFailing}},
	}
	second := outcome.HealthCheckResult{
		State:  outcome.HealthFailing,
		Checks: []outcome.HealthCheckItem{{Name: "check-b", Blocking: true, Status: outcome.HealthFailing}},
	}
	t0 := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)

	o.reconcileIneffectiveCodeLanded(s, sess, first, t0)
	firstDeadline := *sess.CodeLandedVerifyDeadline

	// A different failure past the first deadline must RE-ARM, not convict:
	// a new red check is not evidence the merged fix was ineffective.
	past := t0.Add(o.codeLandedVerifyGrace() + time.Minute)
	if o.reconcileIneffectiveCodeLanded(s, sess, second, past) {
		t.Fatalf("a changed fingerprint must re-arm, not convict")
	}
	if sess.ReleasedForRedispatch {
		t.Fatalf("changed fingerprint must not release the session")
	}
	if !sess.CodeLandedVerifyDeadline.After(firstDeadline) {
		t.Fatalf("changed fingerprint must push the deadline forward")
	}
}
