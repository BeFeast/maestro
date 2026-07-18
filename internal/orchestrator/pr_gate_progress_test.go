package orchestrator

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/github"
	"github.com/befeast/maestro/internal/state"
)

func TestAutoMergePRs_PersistsAuthoritativePRGateTransitions(t *testing.T) {
	pr := github.PR{Number: 7, HeadRefName: "feat/watchdog"}
	cfg := &config.Config{
		Repo:              "owner/repo",
		MergeStrategy:     "parallel",
		ReviewGate:        "greptile",
		ReviewGateStreams: []string{"greptile", "simplicity"},
	}
	o, merged := newMergeTestOrchestrator(cfg, []github.PR{pr})
	headA := strings.Repeat("a", 40)
	headB := strings.Repeat("b", 40)
	rollup := github.PRCheckRollup{
		HeadSHA: headA, Verdict: "pending", Fingerprint: strings.Repeat("1", 16), Complete: true,
	}
	o.ghPRCheckRollupFn = func(int) (github.PRCheckRollup, error) { return rollup, nil }
	o.ghPRHeadSHAFn = func(int) (string, error) { return rollup.HeadSHA, nil }
	reviewVerdict := github.ReviewGateVerdict{
		Pending: true,
		Streams: []github.ReviewStreamVerdict{
			{Name: "greptile", Pending: true},
			{Name: "simplicity", Pending: true},
		},
	}
	o.ghPRReviewGateVerdictFn = func(int, []string) (github.ReviewGateVerdict, error) { return reviewVerdict, nil }
	st := makeTestState([]github.PR{pr})

	o.autoMergePRs(st)
	pending := mustLatestPRGateSnapshot(t, st, 100, 7)
	if pending.Generation != 1 || pending.HeadSHA != headA || pending.CIEffectiveVerdict != state.PRGateCIPending || pending.ReviewDecision != state.PRGateReviewUnknown {
		t.Fatalf("pending snapshot = %+v", pending)
	}

	rollup.Verdict = "success"
	rollup.Fingerprint = strings.Repeat("2", 16)
	o.autoMergePRs(st)
	green := mustLatestPRGateSnapshot(t, st, 100, 7)
	if green.Generation <= pending.Generation || green.CIEffectiveVerdict != state.PRGateCISuccess || green.ReviewDecision != state.PRGateReviewPending {
		t.Fatalf("green snapshot = %+v, pending=%+v", green, pending)
	}

	rollup.HeadSHA = headB
	rollup.Verdict = "pending"
	rollup.Fingerprint = strings.Repeat("3", 16)
	o.autoMergePRs(st)
	newHead := mustLatestPRGateSnapshot(t, st, 100, 7)
	if newHead.Generation <= green.Generation || newHead.HeadSHA != headB || newHead.ReviewDecision != state.PRGateReviewUnknown {
		t.Fatalf("new-head snapshot = %+v, green=%+v", newHead, green)
	}

	rollup.Verdict = "success"
	rollup.Fingerprint = strings.Repeat("4", 16)
	reviewVerdict = github.ReviewGateVerdict{
		Passed: false,
		Streams: []github.ReviewStreamVerdict{
			{Name: "greptile", Passed: true},
			{Name: "simplicity", Passed: false, Findings: []github.ReviewComment{{
				Path: "/home/god/private/internal.go", Line: 42,
				Body: "late actionable feedback with private detail", User: "review-bot",
			}}},
		},
	}
	o.autoMergePRs(st)
	blocked := mustLatestPRGateSnapshot(t, st, 100, 7)
	if blocked.Generation <= newHead.Generation || blocked.ReviewDecision != state.PRGateReviewBlocked || blocked.ActionableFindingsCount != 1 || blocked.FeedbackGeneration != 1 {
		t.Fatalf("late-feedback snapshot = %+v, newHead=%+v", blocked, newHead)
	}
	encoded, err := json.Marshal(st.PRGateSnapshots)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"/home/god", "internal.go", "late actionable feedback", "review-bot"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("durable snapshot leaked raw review material %q: %s", forbidden, encoded)
		}
	}
	if len(*merged) != 0 {
		t.Fatalf("pending/blocked review unexpectedly merged: %v", *merged)
	}
}

func TestMergeReadyPR_PersistsMergeIdentityTransition(t *testing.T) {
	pr := github.PR{Number: 7, HeadRefName: "feat/watchdog"}
	cfg := &config.Config{Repo: "owner/repo", MergeStrategy: "parallel", ReviewGate: "none"}
	o, _ := newMergeTestOrchestrator(cfg, []github.PR{pr})
	st := makeTestState([]github.PR{pr})
	sess := st.Sessions["slot-0"]
	head := strings.Repeat("a", 40)
	mergeSHA := strings.Repeat("f", 40)
	mergedAt := time.Date(2026, 7, 13, 12, 6, 0, 0, time.UTC)
	if _, _, err := st.RecordPRGateTransition(state.PRGateTransition{
		Project: "owner/repo", IssueNumber: sess.IssueNumber, PRNumber: pr.Number, HeadSHA: head,
		CIObserved: true, CIRollupVerdict: state.PRGateCISuccess, CIEffectiveVerdict: state.PRGateCISuccess,
	}, mergedAt.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	o.ghPRMergeInfoFn = func(int) (github.PRMergeInfo, error) {
		return github.PRMergeInfo{SHA: mergeSHA, HeadSHA: head, MergedAt: mergedAt}, nil
	}

	if !o.mergeReadyPR(st, "slot-0", sess, pr) {
		t.Fatal("mergeReadyPR returned false")
	}
	snapshot := mustLatestPRGateSnapshot(t, st, sess.IssueNumber, pr.Number)
	if snapshot.Generation != 2 || snapshot.MergeCommitSHA != mergeSHA || !snapshot.MergedAt.Equal(mergedAt) {
		t.Fatalf("merge snapshot = %+v", snapshot)
	}
}

func TestAutoMergePRs_HeadChangeDuringReviewDefersExactSnapshotAndMerge(t *testing.T) {
	pr := github.PR{Number: 9, HeadRefName: "feat/race"}
	cfg := &config.Config{
		Repo: "owner/repo", MergeStrategy: "parallel", ReviewGate: "greptile",
		ReviewGateStreams: []string{"greptile", "simplicity"},
	}
	o, merged := newMergeTestOrchestrator(cfg, []github.PR{pr})
	headA := strings.Repeat("a", 40)
	headB := strings.Repeat("b", 40)
	o.ghPRCheckRollupFn = func(int) (github.PRCheckRollup, error) {
		return github.PRCheckRollup{HeadSHA: headA, Verdict: "success", Fingerprint: strings.Repeat("1", 16), Complete: true}, nil
	}
	o.ghPRReviewGateVerdictFn = func(int, []string) (github.ReviewGateVerdict, error) {
		return github.ReviewGateVerdict{Passed: true, Streams: []github.ReviewStreamVerdict{{Name: "greptile", Passed: true}, {Name: "simplicity", Passed: true}}}, nil
	}
	o.ghPRHeadSHAFn = func(int) (string, error) { return headB, nil }
	st := makeTestState([]github.PR{pr})

	o.autoMergePRs(st)
	snapshot := mustLatestPRGateSnapshot(t, st, 100, pr.Number)
	if snapshot.HeadSHA != headA || snapshot.CIEffectiveVerdict != state.PRGateCISuccess || snapshot.ReviewDecision != state.PRGateReviewUnknown {
		t.Fatalf("mixed-head review was persisted: %+v", snapshot)
	}
	if len(*merged) != 0 {
		t.Fatalf("head changed during review but PR merged: %v", *merged)
	}
}

func TestAutoMergePRs_HeadChangeAfterFailedRollupDoesNotScheduleStaleRetry(t *testing.T) {
	pr := github.PR{Number: 388, HeadRefName: "feat/current-head-race"}
	cfg := &config.Config{
		Repo: "owner/repo", MergeStrategy: "parallel", ReviewGate: "none",
		MaxRetriesPerIssue: 3, MaxRetryBackoffMs: 300000,
	}
	o, merged := newMergeTestOrchestrator(cfg, []github.PR{pr})
	oldHead := strings.Repeat("a", 40)
	newHead := strings.Repeat("b", 40)
	o.ghPRCheckRollupFn = func(int) (github.PRCheckRollup, error) {
		return github.PRCheckRollup{
			HeadSHA: oldHead, Verdict: "failure", Fingerprint: strings.Repeat("9", 16), Complete: true,
		}, nil
	}
	o.ghPRHeadSHAFn = func(int) (string, error) { return newHead, nil }
	o.ghPRChecksOutputFn = func(int) (string, error) {
		t.Fatal("stale failed head must not be consumed into a repair prompt")
		return "", nil
	}
	o.ghCollectPRReviewFeedbackFn = func(int) (string, error) {
		t.Fatal("stale failed head must not read review feedback for a repair")
		return "", nil
	}
	o.ghPRFailingChecksFn = func(int) ([]github.FailingCheck, error) {
		t.Fatal("stale failed head must not read failing-check logs for a repair")
		return nil, nil
	}
	st := makeTestState([]github.PR{pr})
	sess := st.Sessions["slot-0"]

	o.autoMergePRs(st)

	if sess.Status != state.StatusPROpen || sess.RetryCount != 0 || sess.NextRetryAt != nil {
		t.Fatalf("stale failed head mutated session: status=%q retry=%d next=%v", sess.Status, sess.RetryCount, sess.NextRetryAt)
	}
	if len(*merged) != 0 {
		t.Fatalf("head changed during failed-rollup observation but PR merged: %v", *merged)
	}
	snapshot := mustLatestPRGateSnapshot(t, st, sess.IssueNumber, pr.Number)
	if snapshot.HeadSHA != oldHead || snapshot.CIEffectiveVerdict != state.PRGateCIFailure {
		t.Fatalf("failed-head observation was not retained as read-only history: %+v", snapshot)
	}
}

func TestAutoMergePRs_HeadChangeWhileCollectingFailureContextDoesNotScheduleStaleRetry(t *testing.T) {
	pr := github.PR{Number: 389, HeadRefName: "feat/late-head-race"}
	cfg := &config.Config{
		Repo: "owner/repo", MergeStrategy: "parallel", ReviewGate: "none",
		MaxRetriesPerIssue: 3, MaxRetryBackoffMs: 300000,
	}
	o, merged := newMergeTestOrchestrator(cfg, []github.PR{pr})
	oldHead := strings.Repeat("c", 40)
	newHead := strings.Repeat("d", 40)
	o.ghPRCheckRollupFn = func(int) (github.PRCheckRollup, error) {
		return github.PRCheckRollup{
			HeadSHA: oldHead, Verdict: "failure", Fingerprint: strings.Repeat("8", 16), Complete: true,
		}, nil
	}
	headReads := 0
	o.ghPRHeadSHAFn = func(int) (string, error) {
		headReads++
		if headReads == 1 {
			return oldHead, nil
		}
		return newHead, nil
	}
	o.ghPRChecksOutputFn = func(int) (string, error) { return "old head failed", nil }
	o.ghCollectPRReviewFeedbackFn = func(int) (string, error) { return "old head review", nil }
	o.ghPRFailingChecksFn = func(int) ([]github.FailingCheck, error) {
		return []github.FailingCheck{{Name: "old-check", Conclusion: "failure", Excerpt: "old head"}}, nil
	}
	st := makeTestState([]github.PR{pr})
	sess := st.Sessions["slot-0"]

	o.autoMergePRs(st)

	if headReads != 2 {
		t.Fatalf("head reads = %d, want pre-context and pre-mutation checks", headReads)
	}
	if sess.Status != state.StatusPROpen || sess.RetryCount != 0 || sess.NextRetryAt != nil || sess.NotifiedCIFail || sess.CIFailureOutput != "" || sess.FailingCheckContext != "" || sess.PreviousAttemptFeedback != "" {
		t.Fatalf("late head move persisted stale retry context: %+v", sess)
	}
	if len(*merged) != 0 {
		t.Fatalf("head changed while collecting failed context but PR merged: %v", *merged)
	}
}

func TestAutoMergePRs_PendingGateDoesNotInvokeAttributionAmend(t *testing.T) {
	pr := github.PR{Number: 13, HeadRefName: "feat/deferred-attribution"}
	cfg := &config.Config{Repo: "owner/repo", MergeStrategy: "parallel", ReviewGate: "none"}
	o, merged := newMergeTestOrchestrator(cfg, []github.PR{pr})
	currentHead := strings.Repeat("c", 40)
	o.ghPRCheckRollupFn = func(int) (github.PRCheckRollup, error) {
		return github.PRCheckRollup{
			HeadSHA: currentHead, Verdict: "pending", Fingerprint: strings.Repeat("7", 16), Complete: true,
		}, nil
	}
	amended := false
	o.amendHeadFn = func(string, string, []state.BackendAttribution, time.Time) error {
		amended = true
		return errAmendDiverged
	}
	st := makeTestState([]github.PR{pr})
	sess := st.Sessions["slot-0"]
	sess.Attribution = []state.BackendAttribution{{Backend: "sol", StartedAt: time.Now().UTC()}}

	o.autoMergePRs(st)

	snapshot := mustLatestPRGateSnapshot(t, st, sess.IssueNumber, pr.Number)
	if snapshot.HeadSHA != currentHead || snapshot.CIEffectiveVerdict != state.PRGateCIPending || snapshot.CheckRollupFingerprint != strings.Repeat("7", 16) || snapshot.ReviewDecision != state.PRGateReviewUnknown {
		t.Fatalf("deferred-attribution snapshot = %+v", snapshot)
	}
	if len(*merged) != 0 {
		t.Fatalf("pending PR unexpectedly merged: %v", *merged)
	}
	if amended {
		t.Fatal("PR gate observation must not rewrite the product branch for attribution")
	}
}

func TestAutoMergePRs_PassingGateDoesNotInvokeAttributionAmend(t *testing.T) {
	pr := github.PR{Number: 14, HeadRefName: "feat/deferred-review"}
	cfg := &config.Config{Repo: "owner/repo", MergeStrategy: "parallel", ReviewGate: "greptile", ReviewGateStreams: []string{"greptile"}}
	o, merged := newMergeTestOrchestrator(cfg, []github.PR{pr})
	currentHead := strings.Repeat("d", 40)
	o.ghPRCheckRollupFn = func(int) (github.PRCheckRollup, error) {
		return github.PRCheckRollup{HeadSHA: currentHead, Verdict: "success", Fingerprint: strings.Repeat("8", 16), Complete: true}, nil
	}
	o.ghPRHeadSHAFn = func(int) (string, error) { return currentHead, nil }
	o.ghPRReviewGateVerdictFn = func(int, []string) (github.ReviewGateVerdict, error) {
		return github.ReviewGateVerdict{Passed: true, Streams: []github.ReviewStreamVerdict{{Name: "greptile", Passed: true}}}, nil
	}
	amended := false
	o.amendHeadFn = func(string, string, []state.BackendAttribution, time.Time) error {
		amended = true
		return errAmendDiverged
	}
	st := makeTestState([]github.PR{pr})
	sess := st.Sessions["slot-0"]
	sess.Attribution = []state.BackendAttribution{{Backend: "sol", StartedAt: time.Now().UTC()}}

	o.autoMergePRs(st)

	snapshot := mustLatestPRGateSnapshot(t, st, sess.IssueNumber, pr.Number)
	if snapshot.HeadSHA != currentHead || snapshot.CIEffectiveVerdict != state.PRGateCISuccess || snapshot.ReviewDecision != state.PRGateReviewPassed {
		t.Fatalf("deferred passing-review snapshot = %+v", snapshot)
	}
	if len(*merged) != 1 || (*merged)[0] != pr.Number {
		t.Fatalf("passing PR was not merged normally: %v", *merged)
	}
	if amended {
		t.Fatal("passing merge gate must not rewrite the product branch for attribution")
	}
}

func mustLatestPRGateSnapshot(t *testing.T, st *state.State, issueNumber, prNumber int) state.PRGateSnapshot {
	t.Helper()
	snapshot, ok := st.LatestPRGateSnapshot("owner/repo", issueNumber, prNumber)
	if !ok {
		t.Fatalf("missing PR-gate snapshot for issue=%d pr=%d: %+v", issueNumber, prNumber, st.PRGateSnapshots)
	}
	return snapshot
}
