package state

import (
	"strings"
	"testing"
	"time"
)

func TestRecordPRGateTransition_ExactSemanticGenerations(t *testing.T) {
	st := NewState()
	t0 := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
	headA := strings.Repeat("a", 40)
	headB := strings.Repeat("b", 40)
	base := PRGateTransition{
		Project:                "owner/repo",
		IssueNumber:            887,
		PRNumber:               7,
		HeadSHA:                headA,
		CIObserved:             true,
		CIRollupVerdict:        PRGateCIPending,
		CIEffectiveVerdict:     PRGateCIPending,
		CheckRollupObserved:    true,
		CheckRollupFingerprint: strings.Repeat("1", 16),
	}
	pending, changed, err := st.RecordPRGateTransition(base, t0)
	if err != nil || !changed {
		t.Fatalf("record pending: snapshot=%+v changed=%t err=%v", pending, changed, err)
	}
	if pending.Generation != 1 || pending.Key() == "" || len(st.PRGateSnapshots) != 1 {
		t.Fatalf("pending exact generation = %+v map=%+v", pending, st.PRGateSnapshots)
	}

	repeated, changed, err := st.RecordPRGateTransition(base, t0.Add(time.Minute))
	if err != nil || changed || repeated.Generation != pending.Generation || !repeated.UpdatedAt.Equal(pending.UpdatedAt) {
		t.Fatalf("unchanged poll fabricated progress: before=%+v after=%+v changed=%t err=%v", pending, repeated, changed, err)
	}

	greenTransition := base
	greenTransition.CIRollupVerdict = PRGateCISuccess
	greenTransition.CIEffectiveVerdict = PRGateCISuccess
	greenTransition.CheckRollupFingerprint = strings.Repeat("2", 16)
	green, changed, err := st.RecordPRGateTransition(greenTransition, t0.Add(2*time.Minute))
	if err != nil || !changed || green.Generation != 2 || green.Key() == pending.Key() {
		t.Fatalf("pending->green did not mint exact generation: %+v changed=%t err=%v", green, changed, err)
	}

	newHeadTransition := base
	newHeadTransition.HeadSHA = headB
	newHeadTransition.CheckRollupFingerprint = strings.Repeat("3", 16)
	newHead, changed, err := st.RecordPRGateTransition(newHeadTransition, t0.Add(3*time.Minute))
	if err != nil || !changed || newHead.Generation != 3 || newHead.HeadSHA != headB || newHead.ReviewDecision != PRGateReviewUnknown {
		t.Fatalf("head transition = %+v changed=%t err=%v", newHead, changed, err)
	}

	lateFeedback := PRGateTransition{
		Project:                       "owner/repo",
		IssueNumber:                   887,
		PRNumber:                      7,
		HeadSHA:                       headB,
		ReviewObserved:                true,
		ReviewDecision:                PRGateReviewBlocked,
		ReviewVerdictFingerprint:      strings.Repeat("4", 16),
		ActionableFindingsFingerprint: strings.Repeat("5", 16),
		ActionableFindingsCount:       1,
	}
	blocked, changed, err := st.RecordPRGateTransition(lateFeedback, t0.Add(4*time.Minute))
	if err != nil || !changed || blocked.Generation != 4 || blocked.FeedbackGeneration != 1 {
		t.Fatalf("late feedback = %+v changed=%t err=%v", blocked, changed, err)
	}
	lateFeedback.ActionableFindingsFingerprint = strings.Repeat("6", 16)
	newFeedback, changed, err := st.RecordPRGateTransition(lateFeedback, t0.Add(5*time.Minute))
	if err != nil || !changed || newFeedback.Generation != 5 || newFeedback.FeedbackGeneration != 2 {
		t.Fatalf("new late-feedback generation = %+v changed=%t err=%v", newFeedback, changed, err)
	}

	mergeTransition := PRGateTransition{
		Project:        "owner/repo",
		IssueNumber:    887,
		PRNumber:       7,
		HeadSHA:        headB,
		MergeObserved:  true,
		MergeCommitSHA: strings.Repeat("f", 40),
		MergedAt:       t0.Add(6 * time.Minute),
	}
	merged, changed, err := st.RecordPRGateTransition(mergeTransition, t0.Add(6*time.Minute))
	if err != nil || !changed || merged.Generation != 6 || merged.MergeCommitSHA == "" || len(st.PRGateSnapshots) != 1 {
		t.Fatalf("merge transition = %+v changed=%t err=%v", merged, changed, err)
	}
	latest, ok := st.LatestPRGateSnapshot("owner/repo", 887, 7)
	if !ok || latest.Key() != merged.Key() {
		t.Fatalf("latest = %+v ok=%t, want merged %+v", latest, ok, merged)
	}
}

func TestRecordPRGateTransition_RejectsRawFingerprintMaterial(t *testing.T) {
	st := NewState()
	_, _, err := st.RecordPRGateTransition(PRGateTransition{
		Project:                "owner/repo",
		IssueNumber:            1,
		PRNumber:               2,
		HeadSHA:                strings.Repeat("a", 40),
		CheckRollupObserved:    true,
		CheckRollupFingerprint: "/home/god/private/check-output",
	}, time.Now())
	if err == nil {
		t.Fatal("raw path was accepted as a persisted check-rollup fingerprint")
	}
	if len(st.PRGateSnapshots) != 0 {
		t.Fatalf("invalid transition mutated state: %+v", st.PRGateSnapshots)
	}
}

func TestMergePRGateSnapshots_PrefersNewestGeneration(t *testing.T) {
	t0 := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
	old := PRGateSnapshot{
		Project: "owner/repo", IssueNumber: 1, PRNumber: 2, HeadSHA: strings.Repeat("a", 40),
		Generation: 1, CIRollupVerdict: PRGateCIPending, CIEffectiveVerdict: PRGateCIPending,
		ReviewDecision: PRGateReviewUnknown, UpdatedAt: t0,
	}
	newer := old
	newer.Generation = 2
	newer.CIRollupVerdict = PRGateCISuccess
	newer.CIEffectiveVerdict = PRGateCISuccess
	newer.UpdatedAt = t0.Add(time.Minute)
	merged := mergePRGateSnapshots(
		map[string]PRGateSnapshot{old.Key(): old},
		map[string]PRGateSnapshot{newer.Key(): newer},
	)
	if len(merged) != 1 || merged[newer.Key()].Generation != 2 {
		t.Fatalf("merged snapshots = %+v, want only newest generation", merged)
	}
}
