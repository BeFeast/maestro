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

func TestAutoMergePRs_HumanGateFailureHoldsDraftPROpenWithoutRetry(t *testing.T) {
	pr := github.PR{Number: 7, HeadRefName: "feat/android", IsDraft: true}
	cfg := &config.Config{
		Repo:               "owner/repo",
		MaxRetriesPerIssue: 3,
		Supervisor: config.SupervisorConfig{
			OperatorGate: config.SupervisorOperatorGateConfig{
				CheckNames:     []string{"Android SDK license acceptance gate"},
				RequiredAction: "Record Android SDK license acceptance, then rerun CI.",
			},
		},
	}
	o, _ := newMergeTestOrchestrator(cfg, []github.PR{pr})
	o.ghPRCheckRollupFn = func(int) (github.PRCheckRollup, error) {
		return github.PRCheckRollup{
			HeadSHA:     strings.Repeat("a", 40),
			Verdict:     "failure",
			Fingerprint: strings.Repeat("1", 16),
			Complete:    true,
			Signals: []github.PRCheckSignal{{
				Source: "check_run", Name: "Android SDK license acceptance gate", Status: "completed", Conclusion: "failure",
			}},
		}, nil
	}
	o.getIssueFn = func(number int) (github.Issue, error) { return makeIssue(number, "android gate"), nil }
	o.ghPRLabelsFn = func(int) ([]string, error) { return nil, nil }
	o.ghClosePRFn = func(int, string) error {
		t.Fatal("operator-held PR must not be closed")
		return nil
	}
	o.workerStopFn = func(*config.Config, string, *state.Session) error {
		t.Fatal("operator-held PR must not stop/delete worker state for retry")
		return nil
	}

	st := makeTestState([]github.PR{pr})
	sess := st.Sessions["slot-0"]
	sess.Worktree = "/tmp/worktree"

	o.autoMergePRs(st)
	o.autoMergePRs(st)

	if sess.Status != state.StatusPROpen {
		t.Fatalf("status = %q, want pr_open", sess.Status)
	}
	if sess.RetryCount != 0 || sess.NextRetryAt != nil || sess.LastClosedPRNumber != 0 || sess.PRNumber != pr.Number {
		t.Fatalf("retry state mutated: retry=%d next=%v lastClosed=%d pr=%d", sess.RetryCount, sess.NextRetryAt, sess.LastClosedPRNumber, sess.PRNumber)
	}
	if sess.Worktree != "/tmp/worktree" {
		t.Fatalf("worktree = %q, want preserved", sess.Worktree)
	}
	if sess.OperatorGateName != "check:Android SDK license acceptance gate" {
		t.Fatalf("operator gate = %q", sess.OperatorGateName)
	}
	attention := state.SessionAttentionFor(sess, nil)
	if !attention.NeedsAttention || !strings.Contains(attention.Reason, "Android SDK license acceptance gate") || !strings.Contains(attention.NextAction, "retry budget was not consumed") {
		t.Fatalf("attention = %+v, want named operator gate and retry-budget note", attention)
	}
}

func TestAutoMergePRs_HoldLabelsPreventCIRetryForIssueAndPR(t *testing.T) {
	cases := []struct {
		name         string
		blockedLabel string
		issue        github.Issue
		prLabels     []string
		wantGate     string
	}{
		{
			name:     "issue blocked",
			issue:    makeIssue(100, "held issue", "blocked"),
			wantGate: "label:blocked",
		},
		{
			name:         "literal blocked with custom supervisor label",
			blockedLabel: "supervisor-blocked",
			issue:        makeIssue(100, "held issue", "blocked"),
			wantGate:     "label:blocked",
		},
		{
			name:     "pr operator decision",
			issue:    makeIssue(100, "held issue"),
			prLabels: []string{"operator-decision"},
			wantGate: "label:operator-decision",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pr := github.PR{Number: 7, HeadRefName: "feat/held", IsDraft: true}
			blockedLabel := tc.blockedLabel
			if blockedLabel == "" {
				blockedLabel = "blocked"
			}
			cfg := &config.Config{Repo: "owner/repo", MaxRetriesPerIssue: 3, Supervisor: config.SupervisorConfig{BlockedLabel: blockedLabel}}
			o, _ := newMergeTestOrchestrator(cfg, []github.PR{pr})
			o.ghPRCheckRollupFn = func(int) (github.PRCheckRollup, error) {
				return github.PRCheckRollup{
					HeadSHA: strings.Repeat("b", 40), Verdict: "failure", Fingerprint: strings.Repeat("2", 16), Complete: true,
					Signals: []github.PRCheckSignal{{Source: "check_run", Name: "ordinary CI", Status: "completed", Conclusion: "failure"}},
				}, nil
			}
			o.getIssueFn = func(number int) (github.Issue, error) { return tc.issue, nil }
			o.ghPRLabelsFn = func(int) ([]string, error) { return tc.prLabels, nil }
			o.ghClosePRFn = func(int, string) error {
				t.Fatal("label-held PR must not be closed")
				return nil
			}

			st := makeTestState([]github.PR{pr})
			sess := st.Sessions["slot-0"]
			o.autoMergePRs(st)

			if sess.Status != state.StatusPROpen || sess.RetryCount != 0 || sess.LastClosedPRNumber != 0 {
				t.Fatalf("held session mutated into retry: status=%q retry=%d lastClosed=%d", sess.Status, sess.RetryCount, sess.LastClosedPRNumber)
			}
			if sess.OperatorGateName != tc.wantGate {
				t.Fatalf("operator gate = %q, want %q", sess.OperatorGateName, tc.wantGate)
			}
		})
	}
}

func TestAutoMergePRs_HoldLabelPreventsGreenPRMerge(t *testing.T) {
	pr := github.PR{Number: 7, HeadRefName: "feat/held", IsDraft: false}
	cfg := &config.Config{Repo: "owner/repo", MaxRetriesPerIssue: 3}
	o, _ := newMergeTestOrchestrator(cfg, []github.PR{pr})
	o.ghPRCheckRollupFn = func(int) (github.PRCheckRollup, error) {
		return github.PRCheckRollup{
			HeadSHA:     strings.Repeat("e", 40),
			Verdict:     "success",
			Fingerprint: strings.Repeat("5", 16),
			Complete:    true,
		}, nil
	}
	o.getIssueFn = func(number int) (github.Issue, error) { return makeIssue(number, "held issue"), nil }
	o.ghPRLabelsFn = func(int) ([]string, error) { return []string{"operator-decision"}, nil }
	merged := 0
	o.ghMergePRFn = func(int) error {
		merged++
		return nil
	}

	st := makeTestState([]github.PR{pr})
	sess := st.Sessions["slot-0"]
	o.autoMergePRs(st)

	if merged != 0 {
		t.Fatalf("merged %d held PRs, want 0", merged)
	}
	if sess.Status != state.StatusPROpen || sess.RetryCount != 0 || sess.NextRetryAt != nil {
		t.Fatalf("held session state = status=%q retry=%d next=%v", sess.Status, sess.RetryCount, sess.NextRetryAt)
	}
	if sess.OperatorGateName != "label:operator-decision" {
		t.Fatalf("operator gate = %q", sess.OperatorGateName)
	}
}

func TestAutoMergePRs_AmbiguousFailureRetriesAfterGateOpens(t *testing.T) {
	pr := github.PR{Number: 7, HeadRefName: "feat/android"}
	cfg := &config.Config{
		Repo:               "owner/repo",
		MaxRetriesPerIssue: 3,
		MaxRetryBackoffMs:  1,
		Supervisor: config.SupervisorConfig{OperatorGate: config.SupervisorOperatorGateConfig{
			CheckNames: []string{"Android SDK license acceptance gate"},
		}},
	}
	o, _ := newMergeTestOrchestrator(cfg, []github.PR{pr})
	gated := true
	o.ghPRCheckRollupFn = func(int) (github.PRCheckRollup, error) {
		name := "ordinary CI"
		if gated {
			name = "Android SDK license acceptance gate"
		}
		return github.PRCheckRollup{
			HeadSHA: strings.Repeat("c", 40), Verdict: "failure", Fingerprint: strings.Repeat("3", 16), Complete: true,
			Signals: []github.PRCheckSignal{{Source: "check_run", Name: name, Status: "completed", Conclusion: "failure"}},
		}, nil
	}
	o.ghPRHeadSHAFn = func(int) (string, error) { return strings.Repeat("c", 40), nil }
	o.getIssueFn = func(number int) (github.Issue, error) { return makeIssue(number, "android gate"), nil }
	o.ghPRLabelsFn = func(int) ([]string, error) { return nil, nil }
	o.ghPRChecksOutputFn = func(int) (string, error) { return "ordinary CI\tfailure\n", nil }
	o.ghCollectPRReviewFeedbackFn = func(int) (string, error) { return "", nil }
	o.ghPRFailingChecksFn = func(int) ([]github.FailingCheck, error) { return nil, nil }
	closed := 0
	o.ghClosePRFn = func(int, string) error {
		closed++
		return nil
	}

	st := makeTestState([]github.PR{pr})
	sess := st.Sessions["slot-0"]
	sess.Worktree = "/tmp/worktree"

	o.autoMergePRs(st)
	if closed != 0 || sess.RetryCount != 0 || sess.Status != state.StatusPROpen {
		t.Fatalf("gated poll mutated retry: closed=%d retry=%d status=%q", closed, sess.RetryCount, sess.Status)
	}

	gated = false
	o.autoMergePRs(st)
	if sess.Status != state.StatusDead || sess.RetryCount != 1 || sess.PRNumber != pr.Number {
		t.Fatalf("retry state = status=%q retry=%d pr=%d lastClosed=%d", sess.Status, sess.RetryCount, sess.PRNumber, sess.LastClosedPRNumber)
	}
	if sess.OperatorGateName != "" || sess.LastNotifiedStatus == operatorGateStatus {
		t.Fatalf("operator gate not cleared: gate=%q status=%q", sess.OperatorGateName, sess.LastNotifiedStatus)
	}
}

func TestRespawnDueRetries_HoldLabelStopsScheduledRetryWithoutBudgetBurn(t *testing.T) {
	cfg := &config.Config{
		Repo:               "owner/repo",
		MaxRetriesPerIssue: 3,
		Supervisor: config.SupervisorConfig{OperatorGate: config.SupervisorOperatorGateConfig{
			Labels: []string{"blocked"},
		}},
	}
	o := &Orchestrator{
		cfg:          cfg,
		notifier:     &notify.Notifier{},
		getIssueFn:   func(number int) (github.Issue, error) { return makeIssue(number, "held retry", "blocked"), nil },
		isPRMergedFn: func(int) (bool, error) { return false, nil },
		isIssueClosedFn: func(int) (bool, error) {
			return false, nil
		},
		respawnInPlaceFn: func(*config.Config, string, *state.Session, string, github.Issue, string, string) error {
			t.Fatal("operator-held scheduled retry must not respawn in place")
			return nil
		},
		respawnWorkerFn: func(*config.Config, string, *state.Session, string, github.Issue, string, string) error {
			t.Fatal("operator-held scheduled retry must not spawn a fresh worker")
			return nil
		},
	}

	st := state.NewState()
	retryAt := time.Now().UTC().Add(-time.Minute)
	st.Sessions["slot-1"] = &state.Session{
		IssueNumber: 100,
		IssueTitle:  "held retry",
		Status:      state.StatusDead,
		PRNumber:    7,
		Worktree:    "/tmp/worktree",
		RetryCount:  1,
		NextRetryAt: &retryAt,
	}

	o.respawnDueRetries(st, 1)
	o.respawnDueRetries(st, 1)

	sess := st.Sessions["slot-1"]
	if sess.Status != state.StatusPROpen {
		t.Fatalf("status = %q, want pr_open stable hold", sess.Status)
	}
	if sess.RetryCount != 1 || sess.NextRetryAt != nil || sess.PRNumber != 7 {
		t.Fatalf("retry state mutated: retry=%d next=%v pr=%d", sess.RetryCount, sess.NextRetryAt, sess.PRNumber)
	}
	if sess.OperatorGateName != "label:blocked" {
		t.Fatalf("operator gate = %q, want label:blocked", sess.OperatorGateName)
	}
	attention := state.SessionAttentionFor(sess, nil)
	if !attention.NeedsAttention || !strings.Contains(attention.NextAction, "retry budget was not consumed") {
		t.Fatalf("attention = %+v, want stable operator hold without budget burn", attention)
	}
}

func TestRespawnDueRetries_HumanGateCheckStopsScheduledRetry(t *testing.T) {
	cfg := &config.Config{
		Repo:               "owner/repo",
		MaxRetriesPerIssue: 3,
		Supervisor: config.SupervisorConfig{OperatorGate: config.SupervisorOperatorGateConfig{
			CheckNames: []string{"Android SDK license acceptance gate"},
		}},
	}
	o := &Orchestrator{
		cfg:          cfg,
		notifier:     &notify.Notifier{},
		getIssueFn:   func(number int) (github.Issue, error) { return makeIssue(number, "held retry"), nil },
		ghPRLabelsFn: func(int) ([]string, error) { return nil, nil },
		isPRMergedFn: func(int) (bool, error) { return false, nil },
		isIssueClosedFn: func(int) (bool, error) {
			return false, nil
		},
		ghPRCheckRollupFn: func(int) (github.PRCheckRollup, error) {
			return github.PRCheckRollup{
				HeadSHA:     strings.Repeat("d", 40),
				Verdict:     "pending",
				Fingerprint: strings.Repeat("4", 16),
				Complete:    true,
				Signals: []github.PRCheckSignal{{
					Source: "check_run", Name: "Android SDK license acceptance gate", Status: "queued",
				}},
			}, nil
		},
		respawnInPlaceFn: func(*config.Config, string, *state.Session, string, github.Issue, string, string) error {
			t.Fatal("operator-gated check must not respawn")
			return nil
		},
	}

	st := state.NewState()
	retryAt := time.Now().UTC().Add(-time.Minute)
	st.Sessions["slot-1"] = &state.Session{
		IssueNumber: 100,
		IssueTitle:  "held retry",
		Status:      state.StatusDead,
		PRNumber:    7,
		Worktree:    "/tmp/worktree",
		RetryCount:  1,
		NextRetryAt: &retryAt,
	}

	o.respawnDueRetries(st, 1)

	sess := st.Sessions["slot-1"]
	if sess.Status != state.StatusPROpen || sess.RetryCount != 1 || sess.NextRetryAt != nil {
		t.Fatalf("retry state = status=%q retry=%d next=%v, want held without budget burn", sess.Status, sess.RetryCount, sess.NextRetryAt)
	}
	if sess.OperatorGateName != "check:Android SDK license acceptance gate" {
		t.Fatalf("operator gate = %q", sess.OperatorGateName)
	}
}

func TestStartNewWorkers_NoReadyIssueDoesNotSpawnDuringHeldPRCanary(t *testing.T) {
	cfg := &config.Config{Repo: "owner/repo", IssueLabels: []string{"maestro-ready"}, MaxParallel: 1}
	o := &Orchestrator{
		cfg:      cfg,
		notifier: &notify.Notifier{},
		listOpenIssuesFn: func(labels []string) ([]github.Issue, error) {
			if len(labels) != 1 || labels[0] != "maestro-ready" {
				t.Fatalf("labels = %v, want maestro-ready filter", labels)
			}
			return nil, nil
		},
		workerStartFn: func(*config.Config, *state.State, string, github.Issue, string, string) (string, error) {
			t.Fatal("worker spawned without a ready issue")
			return "", nil
		},
	}
	st := state.NewState()

	o.startNewWorkers(st, 1)

	if len(st.Sessions) != 0 {
		t.Fatalf("sessions = %d, want none", len(st.Sessions))
	}
}

func TestStartNewWorkers_OperatorDecisionReadyIssueDoesNotSpawn(t *testing.T) {
	cfg := &config.Config{Repo: "owner/repo", IssueLabels: []string{"maestro-ready"}, MaxParallel: 1}
	o := &Orchestrator{
		cfg:      cfg,
		notifier: &notify.Notifier{},
		listOpenIssuesFn: func(labels []string) ([]github.Issue, error) {
			return []github.Issue{makeIssue(100, "held issue", "maestro-ready", "operator-decision")}, nil
		},
		isIssueClosedFn: func(int) (bool, error) { return false, nil },
		hasMergedPRForIssueFn: func(int) (bool, error) {
			return false, nil
		},
		workerStartFn: func(*config.Config, *state.State, string, github.Issue, string, string) (string, error) {
			t.Fatal("worker spawned for an operator-decision issue")
			return "", nil
		},
	}
	st := state.NewState()

	o.startNewWorkers(st, 1)

	if len(st.Sessions) != 0 {
		t.Fatalf("sessions = %d, want none", len(st.Sessions))
	}
}
