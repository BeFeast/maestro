package supervisor

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/outcome"
	"github.com/befeast/maestro/internal/progress"
	"github.com/befeast/maestro/internal/state"
)

func TestCollectMaterialProgressObservations_ExactWorkersCannotMaskEachOther(t *testing.T) {
	st := state.NewState()
	t0 := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
	st.Sessions["slot-a"] = &state.Session{
		IssueNumber: 11, Status: state.StatusRunning, PID: 101,
		StartedAt: t0.Add(-time.Hour), LastOutputHash: "frozen-a",
	}
	st.Sessions["slot-b"] = &state.Session{
		IssueNumber: 12, Status: state.StatusRunning, PID: 202,
		StartedAt: t0.Add(-30 * time.Minute), LastOutputHash: "output-b-1",
	}

	first := collectMaterialProgressObservations(st, t0)
	if len(first) != 2 {
		t.Fatalf("observations = %d, want two exact workers", len(first))
	}
	if first[0].Target.Key() == first[1].Target.Key() {
		t.Fatalf("distinct workers collapsed to one target: %+v %+v", first[0].Target, first[1].Target)
	}
	for _, observation := range first {
		if observation.Target.Kind != progress.TargetWorker || observation.Phase != progress.PhasePreDelivery {
			t.Fatalf("worker observation = %+v, phase=%q", observation.Target, observation.Phase)
		}
		if observation.Target.ProcessID <= 0 || observation.Target.SessionID == "" || observation.Target.LeaseID == "" {
			t.Fatalf("worker target is not exact: %+v", observation.Target)
		}
	}

	budget := 20 * time.Minute
	if _, err := st.RecordMaterialProgress(first, budget, time.Minute, t0); err != nil {
		t.Fatal(err)
	}
	// Only worker B progresses after the shared silence deadline. Its progress
	// must not reset worker A's independently persisted watermark.
	st.Sessions["slot-b"].LastOutputHash = "output-b-2"
	later := t0.Add(budget + time.Minute)
	decisions, err := st.RecordMaterialProgress(collectMaterialProgressObservations(st, later), budget, time.Minute, later)
	if err != nil {
		t.Fatal(err)
	}
	bySlot := decisionsBySlot(decisions)
	if got := bySlot["slot-a"].Action; got != progress.ActionStopAndRetry {
		t.Fatalf("hung worker A action = %q, want stop_and_retry", got)
	}
	if got := bySlot["slot-b"].Action; got != progress.ActionNone {
		t.Fatalf("progressing worker B action = %q, want none", got)
	}
}

func TestMaterialProgressWatchdog_UsageUnreliableDoesNotKillHealthyWorker(t *testing.T) {
	st := state.NewState()
	t0 := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	st.Sessions["slot-946"] = &state.Session{
		IssueNumber:    946,
		Status:         state.StatusRunning,
		PID:            946,
		StartedAt:      t0.Add(-time.Hour),
		LastOutputHash: "healthy-output-1",
		Attribution: []state.BackendAttribution{{
			Backend:               "grok",
			UsageUnreliable:       true,
			UsageUnreliableReason: "live_assistant_zero_input_or_output",
			UsageUnreliableScope:  state.UsageUnreliableScopeLiveBudget,
		}},
	}

	budget := 20 * time.Minute
	if _, err := st.RecordMaterialProgress(collectMaterialProgressObservations(st, t0), budget, time.Minute, t0); err != nil {
		t.Fatal(err)
	}
	// Token telemetry remains unavailable, but independently-observed terminal
	// output advances. The watchdog must treat the worker as healthy.
	st.Sessions["slot-946"].LastOutputHash = "healthy-output-2"
	decisions, err := st.RecordMaterialProgress(
		collectMaterialProgressObservations(st, t0.Add(budget+time.Minute)),
		budget,
		time.Minute,
		t0.Add(budget+time.Minute),
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(decisions) != 1 || decisions[0].Action != progress.ActionNone || decisions[0].RecommendsRecovery() {
		t.Fatalf("usage-unreliable healthy worker decision = %+v, want progress/no recovery", decisions)
	}
}

func TestCollectMaterialProgressObservations_WorkerReplacementHasNewLease(t *testing.T) {
	st := state.NewState()
	t0 := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
	sess := &state.Session{
		IssueNumber:      9,
		Status:           state.StatusRunning,
		PID:              100,
		StartedAt:        t0,
		ProcessLeaseUnit: "maestro-worker-0123456789abcdef0123456789abcdef-g1.scope",
	}
	st.Sessions["slot-9"] = sess
	first := collectMaterialProgressObservations(st, t0)

	sess.PID = 200
	sess.StartedAt = t0.Add(10 * time.Minute)
	sess.ProcessLeaseUnit = "maestro-worker-0123456789abcdef0123456789abcdef-g2.scope"
	second := collectMaterialProgressObservations(st, t0.Add(10*time.Minute))
	if len(first) != 1 || len(second) != 1 {
		t.Fatalf("observations first=%d second=%d, want one each", len(first), len(second))
	}
	if first[0].Target.Key() == second[0].Target.Key() || first[0].Target.LeaseID == second[0].Target.LeaseID {
		t.Fatalf("respawn reused old exact lease: before=%+v after=%+v", first[0].Target, second[0].Target)
	}
	if first[0].Target.LeaseID != "maestro-worker-0123456789abcdef0123456789abcdef-g1.scope" || second[0].Target.LeaseID != "maestro-worker-0123456789abcdef0123456789abcdef-g2.scope" {
		t.Fatalf("material progress lost OS process lease identity: before=%+v after=%+v", first[0].Target, second[0].Target)
	}
}

func TestCollectMaterialProgressObservations_PROpenNeverCarriesProcessIdentity(t *testing.T) {
	st := state.NewState()
	now := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
	// PID/tmux are deliberately stale legacy fields. pr_open is a gate, not a
	// running worker, so the collector must not attach either to its target.
	st.Sessions["slot-pr"] = &state.Session{
		IssueNumber: 44, Status: state.StatusPROpen, PRNumber: 893,
		PID: 999, TmuxSession: "stale-tmux", StartedAt: now.Add(-time.Hour),
		ReviewPendingHeadSHA: "abc123", LastNotifiedStatus: "ci_pending",
	}
	recordTestPRGateSnapshot(t, st, 44, 893, strings.Repeat("a", 40), now)
	observations := collectMaterialProgressObservations(st, now)
	if len(observations) != 1 {
		t.Fatalf("observations = %d, want one PR gate", len(observations))
	}
	got := observations[0]
	if got.Target.Kind != progress.TargetPRGate || got.Phase != progress.PhasePRGate {
		t.Fatalf("target=%+v phase=%q, want pr_gate", got.Target, got.Phase)
	}
	if got.Target.ProcessID != 0 || got.Target.TmuxSession != "" {
		t.Fatalf("pr_open leaked process identity: %+v", got.Target)
	}
	if signalKindPresent(got.Signals, progress.SignalProcessTmux) || signalKindPresent(got.Signals, progress.SignalWorktreeGit) {
		t.Fatalf("pr_open collected live-worker signals: %+v", got.Signals)
	}

	if _, err := st.RecordMaterialProgress(observations, 20*time.Minute, time.Minute, now); err != nil {
		t.Fatal(err)
	}
	later := now.Add(21 * time.Minute)
	decisions, err := st.RecordMaterialProgress(collectMaterialProgressObservations(st, later), 20*time.Minute, time.Minute, later)
	if err != nil {
		t.Fatal(err)
	}
	if len(decisions) != 1 || decisions[0].Action != progress.ActionSurfaceGateRepair {
		t.Fatalf("overdue pr_gate decisions = %+v, want surface_gate_repair", decisions)
	}
	if decisions[0].ReplayBoundary {
		t.Fatalf("PR gate recommendation incorrectly claimed delivery replay boundary: %+v", decisions[0])
	}
}

func TestCollectMaterialProgressObservations_QueuedRemainsAnActivePRGate(t *testing.T) {
	st := state.NewState()
	now := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
	st.Sessions["slot-pr"] = &state.Session{
		IssueNumber: 44, Status: state.StatusQueued, PRNumber: 893,
		PID: 999, TmuxSession: "stale-tmux", StartedAt: now.Add(-time.Hour),
		ReviewPendingHeadSHA: "abc123", LastNotifiedStatus: "merge_queued",
	}
	recordTestPRGateSnapshot(t, st, 44, 893, strings.Repeat("a", 40), now)
	observations := collectMaterialProgressObservations(st, now)
	if len(observations) != 1 {
		t.Fatalf("observations = %d, want one queued PR gate", len(observations))
	}
	got := observations[0]
	if got.Target.Kind != progress.TargetPRGate || got.Phase != progress.PhasePRGate {
		t.Fatalf("queued target=%+v phase=%q, want pr_gate", got.Target, got.Phase)
	}
	if got.Target.ProcessID != 0 || got.Target.TmuxSession != "" {
		t.Fatalf("queued PR gate leaked stale worker identity: %+v", got.Target)
	}
}

func TestCollectMaterialProgressObservations_LiveWorkerOwnsExactPRGate(t *testing.T) {
	st := state.NewState()
	now := time.Date(2026, 7, 18, 3, 0, 0, 0, time.UTC)
	st.Sessions["old-gate"] = &state.Session{
		IssueNumber: 345, Status: state.StatusPROpen, PRNumber: 388, StartedAt: now.Add(-time.Hour),
	}
	st.Sessions["continuation"] = &state.Session{
		IssueNumber: 406, Status: state.StatusRunning, PRNumber: 388, PID: 1234, StartedAt: now.Add(-time.Minute),
	}
	recordTestPRGateSnapshot(t, st, 345, 388, strings.Repeat("a", 40), now.Add(-time.Hour))

	observations := collectMaterialProgressObservationsForProject(st, "owner/repo", now)
	if len(observations) != 1 {
		t.Fatalf("observations = %+v, want only the live continuation", observations)
	}
	if observations[0].Target.Kind != progress.TargetWorker || observations[0].Target.IssueNumber != 406 || observations[0].Target.ProcessID != 1234 {
		t.Fatalf("active target = %+v, want live continuation worker", observations[0].Target)
	}
}

func TestCollectMaterialProgressObservations_NewestSessionOwnsSharedPRGate(t *testing.T) {
	st := state.NewState()
	now := time.Date(2026, 7, 18, 3, 15, 0, 0, time.UTC)
	st.Sessions["old-gate"] = &state.Session{
		IssueNumber: 345, Status: state.StatusPROpen, PRNumber: 388, StartedAt: now.Add(-time.Hour),
	}
	st.Sessions["continuation"] = &state.Session{
		IssueNumber: 406, Status: state.StatusPROpen, PRNumber: 388, StartedAt: now.Add(-time.Minute),
	}
	recordTestPRGateSnapshot(t, st, 345, 388, strings.Repeat("a", 40), now.Add(-time.Hour))
	recordTestPRGateSnapshot(t, st, 406, 388, strings.Repeat("a", 40), now.Add(-time.Minute))

	observations := collectMaterialProgressObservationsForProject(st, "owner/repo", now)
	if len(observations) != 1 {
		t.Fatalf("observations = %+v, want one canonical PR gate", observations)
	}
	if observations[0].Target.Kind != progress.TargetPRGate || observations[0].Target.IssueNumber != 406 || observations[0].Target.Slot != "continuation" {
		t.Fatalf("canonical PR target = %+v, want newest continuation", observations[0].Target)
	}
}

func TestCollectMaterialProgressObservations_PartialOrWrongProjectPRSnapshotIsIncomplete(t *testing.T) {
	st := state.NewState()
	now := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
	st.Sessions["slot-pr"] = &state.Session{
		IssueNumber: 44, Status: state.StatusPROpen, PRNumber: 893, StartedAt: now.Add(-time.Hour),
	}
	if _, _, err := st.RecordPRGateTransition(state.PRGateTransition{
		Project: "owner/repo", IssueNumber: 44, PRNumber: 893, HeadSHA: strings.Repeat("a", 40),
	}, now); err != nil {
		t.Fatal(err)
	}
	partial := collectMaterialProgressObservationsForProject(st, "owner/repo", now)
	if len(partial) != 1 || !partial[0].Incomplete || !signalKindListed(partial[0].UnavailableSignals, progress.SignalPRReview) {
		t.Fatalf("head-only PR snapshot was not explicit incomplete evidence: %+v", partial)
	}
	wrongProject := collectMaterialProgressObservationsForProject(st, "other/repo", now)
	if len(wrongProject) != 1 || !wrongProject[0].Incomplete || signalKindPresent(wrongProject[0].Signals, progress.SignalPRReview) {
		t.Fatalf("different-project PR snapshot was accepted: %+v", wrongProject)
	}
}

func TestCollectMaterialProgressObservations_CodeLandedHasPostMergeOutcomeTarget(t *testing.T) {
	st := state.NewState()
	now := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
	landedAt := now.Add(-time.Hour)
	st.Sessions["slot-pr"] = &state.Session{
		IssueNumber: 44, Status: state.StatusCodeLanded, PRNumber: 893,
		PID: 999, TmuxSession: "stale-tmux", StartedAt: now.Add(-2 * time.Hour), FinishedAt: &landedAt,
	}
	if _, _, err := st.RecordPRGateTransition(state.PRGateTransition{
		Project: "owner/repo", IssueNumber: 44, PRNumber: 893, HeadSHA: strings.Repeat("a", 40),
		MergeObserved: true, MergeCommitSHA: strings.Repeat("f", 40), MergedAt: landedAt,
	}, landedAt); err != nil {
		t.Fatal(err)
	}
	st.OutcomeHealth = &outcome.HealthCheckResult{State: outcome.HealthFailing, Signal: "healthcheck", ExitCode: 1, CheckedAt: now}
	observations := collectMaterialProgressObservations(st, now)
	if len(observations) != 1 {
		t.Fatalf("observations = %d, want one post-merge verification target", len(observations))
	}
	got := observations[0]
	if got.Target.Kind != progress.TargetPostMerge || got.Phase != progress.PhasePostMergeVerification {
		t.Fatalf("code_landed target=%+v phase=%q, want post_merge_verification", got.Target, got.Phase)
	}
	if got.Target.ProcessID != 0 || got.Target.TmuxSession != "" {
		t.Fatalf("post-merge target leaked stale worker identity: %+v", got.Target)
	}
	if !signalKindPresent(got.Signals, progress.SignalOutcomeVerification) {
		t.Fatalf("post-merge target lacks outcome evidence: %+v", got.Signals)
	}

	budget := 20 * time.Minute
	if _, err := st.RecordMaterialProgress(observations, budget, time.Minute, now); err != nil {
		t.Fatal(err)
	}
	later := now.Add(budget + time.Minute)
	decisions, err := st.RecordMaterialProgress(collectMaterialProgressObservations(st, later), budget, time.Minute, later)
	if err != nil {
		t.Fatal(err)
	}
	if len(decisions) != 1 || decisions[0].Action != progress.ActionSurfaceOutcomeRepair || decisions[0].ReplayBoundary {
		t.Fatalf("overdue post-merge decision = %+v, want no-replay outcome repair", decisions)
	}

	// Repeating the same failing check at a new poll timestamp is not progress.
	st.OutcomeHealth.CheckedAt = later
	laterAgain := later.Add(time.Minute)
	decisions, err = st.RecordMaterialProgress(collectMaterialProgressObservations(st, laterAgain), budget, time.Minute, laterAgain)
	if err != nil {
		t.Fatal(err)
	}
	if len(decisions) != 1 || decisions[0].Action != progress.ActionSurfaceOutcomeRepair {
		t.Fatalf("unchanged failing outcome poll masked stall: %+v", decisions)
	}

	// A semantic outcome transition is material progress and re-arms the gate.
	st.OutcomeHealth.State = outcome.HealthHealthy
	st.OutcomeHealth.ExitCode = 0
	progressAt := laterAgain.Add(time.Minute)
	decisions, err = st.RecordMaterialProgress(collectMaterialProgressObservations(st, progressAt), budget, time.Minute, progressAt)
	if err != nil {
		t.Fatal(err)
	}
	if len(decisions) != 1 || decisions[0].Action != progress.ActionNone {
		t.Fatalf("semantic outcome transition did not advance post-merge target: %+v", decisions)
	}
}

func TestCollectMaterialProgressObservations_AuthoritativePRGateTransitionsAdvanceWatermark(t *testing.T) {
	st := state.NewState()
	t0 := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
	headA := strings.Repeat("a", 40)
	headB := strings.Repeat("b", 40)
	st.Sessions["slot-pr"] = &state.Session{
		IssueNumber: 887, Status: state.StatusPROpen, PRNumber: 7, StartedAt: t0.Add(-time.Hour),
		LastNotifiedStatus: "ci_pending", ReviewPendingHeadSHA: "legacy-timer-head",
	}
	pendingTransition := state.PRGateTransition{
		Project: "owner/repo", IssueNumber: 887, PRNumber: 7, HeadSHA: headA,
		CIObserved: true, CIRollupVerdict: state.PRGateCIPending, CIEffectiveVerdict: state.PRGateCIPending,
		CheckRollupObserved: true, CheckRollupFingerprint: strings.Repeat("1", 16),
	}
	if _, _, err := st.RecordPRGateTransition(pendingTransition, t0); err != nil {
		t.Fatal(err)
	}
	pending := onlyPRGateObservation(t, st, t0)
	if pending.Incomplete {
		t.Fatalf("authoritative pending snapshot was incomplete: %+v", pending)
	}
	if _, err := st.RecordMaterialProgress([]progress.Observation{pending}, 20*time.Minute, time.Minute, t0); err != nil {
		t.Fatal(err)
	}
	targetKey := pending.Target.Key()
	lastAt := materialProgressWatermarkAt(t, st, targetKey)

	// Notification dedup and the Greptile pending/retrigger clock are control
	// bookkeeping, not authoritative forge progress. Churning only those fields
	// must leave both the exact signal and durable watermark unchanged.
	st.Sessions["slot-pr"].LastNotifiedStatus = "merge_failed"
	st.Sessions["slot-pr"].ReviewPendingHeadSHA = "another-timer-head"
	timerNow := t0.Add(time.Minute)
	timerOnly := onlyPRGateObservation(t, st, timerNow)
	if timerOnly.Signals.Combined() != pending.Signals.Combined() {
		t.Fatalf("notification/timer bookkeeping changed PR signal: before=%+v after=%+v", pending.Signals, timerOnly.Signals)
	}
	if _, err := st.RecordMaterialProgress([]progress.Observation{timerOnly}, 20*time.Minute, time.Minute, timerNow); err != nil {
		t.Fatal(err)
	}
	if got := materialProgressWatermarkAt(t, st, targetKey); !got.Equal(lastAt) {
		t.Fatalf("notification/timer bookkeeping advanced watermark: before=%s after=%s", lastAt, got)
	}

	greenTransition := pendingTransition
	greenTransition.CIRollupVerdict = state.PRGateCISuccess
	greenTransition.CIEffectiveVerdict = state.PRGateCISuccess
	greenTransition.CheckRollupFingerprint = strings.Repeat("2", 16)
	greenTransition.ReviewObserved = true
	greenTransition.ReviewDecision = state.PRGateReviewDisabled
	greenTransition.ReviewVerdictFingerprint = strings.Repeat("6", 16)
	greenAt := t0.Add(2 * time.Minute)
	if _, _, err := st.RecordPRGateTransition(greenTransition, greenAt); err != nil {
		t.Fatal(err)
	}
	green := onlyPRGateObservation(t, st, greenAt)
	if green.Target.Key() != targetKey || green.Signals.Combined() == timerOnly.Signals.Combined() {
		t.Fatalf("pending->green changed lease or missed progress: pending=%+v green=%+v", pending, green)
	}
	if _, err := st.RecordMaterialProgress([]progress.Observation{green}, 20*time.Minute, time.Minute, greenAt); err != nil {
		t.Fatal(err)
	}
	lastAt = assertMaterialProgressWatermarkAt(t, st, targetKey, greenAt)

	newHeadTransition := pendingTransition
	newHeadTransition.HeadSHA = headB
	newHeadTransition.CheckRollupFingerprint = strings.Repeat("3", 16)
	headAt := t0.Add(3 * time.Minute)
	if _, _, err := st.RecordPRGateTransition(newHeadTransition, headAt); err != nil {
		t.Fatal(err)
	}
	newHead := onlyPRGateObservation(t, st, headAt)
	if newHead.Target.Key() != targetKey || newHead.Signals.Combined() == green.Signals.Combined() {
		t.Fatalf("head transition changed stable PR lease or missed progress: green=%+v head=%+v", green, newHead)
	}
	if _, err := st.RecordMaterialProgress([]progress.Observation{newHead}, 20*time.Minute, time.Minute, headAt); err != nil {
		t.Fatal(err)
	}
	lastAt = assertMaterialProgressWatermarkAt(t, st, targetKey, headAt)

	feedbackAt := t0.Add(4 * time.Minute)
	if _, _, err := st.RecordPRGateTransition(state.PRGateTransition{
		Project: "owner/repo", IssueNumber: 887, PRNumber: 7, HeadSHA: headB,
		ReviewObserved: true, ReviewDecision: state.PRGateReviewBlocked,
		ReviewVerdictFingerprint:      strings.Repeat("4", 16),
		ActionableFindingsFingerprint: strings.Repeat("5", 16), ActionableFindingsCount: 1,
	}, feedbackAt); err != nil {
		t.Fatal(err)
	}
	feedback := onlyPRGateObservation(t, st, feedbackAt)
	if feedback.Signals.Combined() == newHead.Signals.Combined() {
		t.Fatal("actionable late-review feedback did not change PR signal")
	}
	if _, err := st.RecordMaterialProgress([]progress.Observation{feedback}, 20*time.Minute, time.Minute, feedbackAt); err != nil {
		t.Fatal(err)
	}
	lastAt = assertMaterialProgressWatermarkAt(t, st, targetKey, feedbackAt)

	mergeAt := t0.Add(5 * time.Minute)
	if _, _, err := st.RecordPRGateTransition(state.PRGateTransition{
		Project: "owner/repo", IssueNumber: 887, PRNumber: 7, HeadSHA: headB,
		MergeObserved: true, MergeCommitSHA: strings.Repeat("f", 40), MergedAt: mergeAt,
	}, mergeAt); err != nil {
		t.Fatal(err)
	}
	merged := onlyPRGateObservation(t, st, mergeAt)
	if merged.Signals.Combined() == feedback.Signals.Combined() {
		t.Fatal("merge identity did not change PR signal")
	}
	if _, err := st.RecordMaterialProgress([]progress.Observation{merged}, 20*time.Minute, time.Minute, mergeAt); err != nil {
		t.Fatal(err)
	}
	if got := assertMaterialProgressWatermarkAt(t, st, targetKey, mergeAt); !got.After(lastAt) {
		t.Fatalf("merge transition did not advance watermark: before=%s after=%s", lastAt, got)
	}

	// Production immediately moves the session to code_landed. The distinct
	// post-merge target must carry the same authoritative merge snapshot rather
	// than inventing a second PR-gate target.
	st.Sessions["slot-pr"].Status = state.StatusCodeLanded
	postMerge := collectMaterialProgressObservations(st, mergeAt.Add(time.Minute))
	if len(postMerge) != 1 || postMerge[0].Target.Kind != progress.TargetPostMerge || !signalKindPresent(postMerge[0].Signals, progress.SignalPRReview) {
		t.Fatalf("post-merge target did not inherit merge identity: %+v", postMerge)
	}
}

func TestCollectMaterialProgressObservations_IdleProjectHasNoTarget(t *testing.T) {
	st := state.NewState()
	st.Sessions["done"] = &state.Session{IssueNumber: 1, Status: state.StatusDone, PID: 123}
	st.Sessions["failed"] = &state.Session{IssueNumber: 2, Status: state.StatusFailed}
	if got := collectMaterialProgressObservations(st, time.Now().UTC()); len(got) != 0 {
		t.Fatalf("idle observations = %+v, want none", got)
	}
}

func TestCollectMaterialProgressObservations_DeliveryLifecycleOneStableLease(t *testing.T) {
	st := state.NewState()
	now := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
	approval := state.Approval{
		ID: "delivery-lease", Action: state.ApprovalActionDeployProject,
		Status: state.ApprovalStatusPending, CreatedAt: now, UpdatedAt: now,
		Delivery: &state.DeliveryPayload{
			Issue: 887, PR: 893, MergedSHA: "merge-a", ConfigDigest: "cfg-a", ApprovalGeneration: 2,
		},
	}
	st.Approvals = append(st.Approvals, approval)

	pending := onlyDeliveryObservation(t, st, now)
	key := pending.Target.Key()
	if pending.Phase != progress.PhaseDeliveryPending || pending.Target.ProcessID != 0 || pending.Target.TmuxSession != "" {
		t.Fatalf("pending delivery observation = %+v phase=%q", pending.Target, pending.Phase)
	}
	pendingID := pending.Signals.Combined()

	st.Approvals[0].Status = state.ApprovalStatusApproved
	approved := onlyDeliveryObservation(t, st, now.Add(time.Minute))
	if approved.Target.Key() != key || approved.Signals.Combined() == pendingID {
		t.Fatalf("approval transition changed lease or not signal: pending=%+v approved=%+v", pending, approved)
	}

	st.Approvals[0].Status = state.ApprovalStatusExecuting
	st.Approvals[0].Delivery.StartedAt = now.Add(2 * time.Minute)
	executing := onlyDeliveryObservation(t, st, now.Add(2*time.Minute))
	if executing.Target.Key() != key || executing.Phase != progress.PhaseDeliveryExecuting {
		t.Fatalf("executing delivery changed target/phase: %+v", executing)
	}

	zero := 0
	st.Approvals[0].Status = state.ApprovalStatusExecuted
	st.Approvals[0].Delivery.FinishedAt = now.Add(3 * time.Minute)
	st.Approvals[0].Delivery.DeployExitCode = &zero
	st.Approvals[0].Delivery.VerifyExitCode = &zero
	st.Approvals[0].Delivery.Verified = true
	st.Approvals[0].Delivery.ExecutedRevision = "merge-a"
	delivered := onlyDeliveryObservation(t, st, now.Add(3*time.Minute))
	if delivered.Target.Key() != key || delivered.Phase != progress.PhaseDelivered || delivered.Signals.Combined() == executing.Signals.Combined() {
		t.Fatalf("terminal receipt missing or changed lease: %+v", delivered)
	}
	if _, err := st.RecordMaterialProgress([]progress.Observation{delivered}, 20*time.Minute, time.Minute, now.Add(3*time.Minute)); err != nil {
		t.Fatal(err)
	}
	// Once the exact terminal receipt is durable, the next complete snapshot
	// retires it instead of arming/evaluating historical deliveries forever.
	if got := collectMaterialProgressObservations(st, now.Add(4*time.Minute)); len(got) != 0 {
		t.Fatalf("already-recorded terminal delivery remained active: %+v", got)
	}
}

func TestCollectMaterialProgressObservations_IgnoresNonActionableDeliveryRows(t *testing.T) {
	for _, status := range []state.ApprovalStatus{
		state.ApprovalStatusRejected, state.ApprovalStatusStale, state.ApprovalStatusSuperseded,
	} {
		st := state.NewState()
		st.Approvals = append(st.Approvals, state.Approval{
			ID: "delivery-" + string(status), Action: state.ApprovalActionDeployProject, Status: status,
			Delivery: &state.DeliveryPayload{MergedSHA: "a", ConfigDigest: "b"},
		})
		if got := collectMaterialProgressObservations(st, time.Now().UTC()); len(got) != 0 {
			t.Fatalf("status %q produced watchdog target: %+v", status, got)
		}
	}
}

func TestRecordMaterialProgress_DisableTransitionRetiresTargets(t *testing.T) {
	st := state.NewState()
	now := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
	st.Sessions["slot"] = &state.Session{IssueNumber: 1, Status: state.StatusRunning, PID: 1, StartedAt: now}
	enabled := true
	cfg := &config.Config{StalledProgressWatchdog: config.StalledProgressWatchdogConfig{Enabled: &enabled}}
	if _, err := recordMaterialProgress(cfg, st, now); err != nil {
		t.Fatal(err)
	}
	if len(st.MaterialProgress.Targets) != 1 {
		t.Fatalf("enabled targets = %d, want 1", len(st.MaterialProgress.Targets))
	}

	disabled := false
	cfg.StalledProgressWatchdog.Enabled = &disabled
	if _, err := recordMaterialProgress(cfg, st, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	for key, target := range st.MaterialProgress.Targets {
		if target.Active {
			t.Fatalf("disabled watchdog left target %s active: %+v", key, target)
		}
		if deadline := target.Deadline(st.MaterialProgress.BudgetSeconds); !deadline.IsZero() {
			t.Fatalf("disabled target deadline = %s, want zero", deadline)
		}
	}
}

func TestWorktreeProgressFingerprint_MaterialGitIdentityNotMtime(t *testing.T) {
	wt := initMaterialProgressGitRepo(t)
	src := filepath.Join(wt, "main.go")
	base := worktreeProgressFingerprint(wt)
	if base == "" {
		t.Fatal("clean Git worktree produced no fingerprint")
	}

	// A timestamp-only touch is not material progress.
	touched := time.Now().UTC().Add(2 * time.Hour)
	if err := os.Chtimes(src, touched, touched); err != nil {
		t.Fatal(err)
	}
	if got := worktreeProgressFingerprint(wt); got != base {
		t.Fatalf("mtime-only touch advanced fingerprint: got %q, want %q", got, base)
	}

	// Actual working-tree content and then the staged index are distinct
	// material states even though HEAD has not changed.
	if err := os.WriteFile(src, []byte("package main\n\nfunc main() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	edited := worktreeProgressFingerprint(wt)
	if edited == "" || edited == base {
		t.Fatalf("source edit did not advance fingerprint: base=%q edited=%q", base, edited)
	}
	runMaterialProgressGit(t, wt, "add", "main.go")
	staged := worktreeProgressFingerprint(wt)
	if staged == "" || staged == edited {
		t.Fatalf("index transition did not advance fingerprint: edited=%q staged=%q", edited, staged)
	}

	// A commit moves the resolved HEAD even if it leaves a clean tree.
	runMaterialProgressGit(t, wt, "-c", "user.name=Maestro Test", "-c", "user.email=maestro@example.invalid", "commit", "-q", "-m", "edit")
	committed := worktreeProgressFingerprint(wt)
	if committed == "" || committed == staged || committed == base {
		t.Fatalf("HEAD commit did not produce a distinct identity: base=%q staged=%q committed=%q", base, staged, committed)
	}
}

func TestWorktreeProgressFingerprint_ExcludesGeneratedAndVolatileChurn(t *testing.T) {
	wt := initMaterialProgressGitRepo(t)
	base := worktreeProgressFingerprint(wt)

	volatile := []struct {
		path string
		body string
	}{
		{"build/cache.bin", "one"},
		{"node_modules/pkg/index.js", "one"},
		{"generated/schema.pb.go", "package generated"},
		{"coverage.out", "mode: set"},
	}
	for _, file := range volatile {
		path := filepath.Join(wt, filepath.FromSlash(file.path))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(file.body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if got := worktreeProgressFingerprint(wt); got != base {
		t.Fatalf("generated/volatile churn advanced fingerprint: got %q, want %q", got, base)
	}

	if err := os.WriteFile(filepath.Join(wt, "README.md"), []byte("material docs\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := worktreeProgressFingerprint(wt); got == "" || got == base {
		t.Fatalf("relevant untracked source/docs did not advance fingerprint: got %q, base %q", got, base)
	}
}

func TestCollectMaterialProgressObservations_SourceEditsAdvanceButGeneratedChurnDoesNot(t *testing.T) {
	wt := initMaterialProgressGitRepo(t)
	st := state.NewState()
	t0 := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
	st.Sessions["slot"] = &state.Session{
		IssueNumber: 7, Status: state.StatusRunning, PID: 77,
		StartedAt: t0.Add(-time.Hour), LastOutputHash: "frozen", Worktree: wt,
	}
	budget := 20 * time.Minute
	if _, err := st.RecordMaterialProgress(collectMaterialProgressObservations(st, t0), budget, time.Minute, t0); err != nil {
		t.Fatal(err)
	}

	// Quiet terminal, real source edit after the old deadline: the exact
	// worktree signal advances and prevents a false recovery recommendation.
	if err := os.WriteFile(filepath.Join(wt, "main.go"), []byte("package main\n\nfunc main() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t1 := t0.Add(21 * time.Minute)
	decisions, err := st.RecordMaterialProgress(collectMaterialProgressObservations(st, t1), budget, time.Minute, t1)
	if err != nil {
		t.Fatal(err)
	}
	if len(decisions) != 1 || decisions[0].Action != progress.ActionNone {
		t.Fatalf("source edit decisions = %+v, want material progress", decisions)
	}

	// Churn only generated output for another full budget. It is intentionally
	// excluded, so it cannot keep a genuinely idle worker alive forever.
	generated := filepath.Join(wt, "build", "heartbeat.log")
	if err := os.MkdirAll(filepath.Dir(generated), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(generated, []byte("churn"), 0o644); err != nil {
		t.Fatal(err)
	}
	t2 := t1.Add(21 * time.Minute)
	decisions, err = st.RecordMaterialProgress(collectMaterialProgressObservations(st, t2), budget, time.Minute, t2)
	if err != nil {
		t.Fatal(err)
	}
	if len(decisions) != 1 || decisions[0].Action != progress.ActionStopAndRetry {
		t.Fatalf("generated-only churn decisions = %+v, want stop_and_retry", decisions)
	}
}

func TestCollectMaterialProgressObservations_LiveExactTmuxAdvancesWithFrozenGitAndPersistedHash(t *testing.T) {
	bin := t.TempDir()
	outputPath := filepath.Join(bin, "output")
	script := filepath.Join(bin, "tmux")
	if err := os.WriteFile(script, []byte(`#!/bin/sh
if [ "$1" != "capture-pane" ] || [ "$2" != "-p" ] || [ "$3" != "-t" ] || [ "$4" != "=mae-exact:" ]; then
  exit 91
fi
if [ "${TMUX_FAKE_FAIL:-}" = "1" ]; then
  exit 92
fi
cat "$TMUX_FAKE_OUTPUT"
`), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(outputPath, []byte("first pane\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("TMUX_FAKE_OUTPUT", outputPath)

	wt := initMaterialProgressGitRepo(t)
	checkpointPath := filepath.Join(bin, "CHECKPOINT.md")
	if err := os.WriteFile(checkpointPath, []byte("checkpoint frozen\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	st := state.NewState()
	t0 := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
	st.Sessions["slot"] = &state.Session{
		IssueNumber: 7, Status: state.StatusRunning, PID: 77, TmuxSession: "mae-exact",
		StartedAt: t0.Add(-time.Hour), LastOutputHash: "persisted-frozen", Worktree: wt, CheckpointFile: checkpointPath,
	}
	first := collectMaterialProgressObservations(st, t0)
	if len(first) != 1 || first[0].Incomplete {
		t.Fatalf("first exact-tmux observation = %+v, want complete", first)
	}
	budget := 20 * time.Minute
	if _, err := st.RecordMaterialProgress(first, budget, time.Minute, t0); err != nil {
		t.Fatal(err)
	}

	// Neither Git nor the persisted legacy LastOutputHash changes. Fresh bounded
	// output from the exact tmux session is independently material progress.
	if err := os.WriteFile(outputPath, []byte("second pane\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t1 := t0.Add(budget + time.Minute)
	decisions, err := st.RecordMaterialProgress(collectMaterialProgressObservations(st, t1), budget, time.Minute, t1)
	if err != nil {
		t.Fatal(err)
	}
	if len(decisions) != 1 || decisions[0].Action != progress.ActionNone {
		t.Fatalf("live tmux change with frozen Git/hash = %+v, want material progress", decisions)
	}

	// A later tmux probe failure is explicit incomplete evidence and suppresses
	// recovery at the unchanged deadline instead of silently omitting the signal.
	t.Setenv("TMUX_FAKE_FAIL", "1")
	t2 := t1.Add(budget + time.Minute)
	observations := collectMaterialProgressObservations(st, t2)
	if len(observations) != 1 || !observations[0].Incomplete || !signalKindListed(observations[0].UnavailableSignals, progress.SignalTerminalCheckpoint) {
		t.Fatalf("tmux failure was not explicit incomplete evidence: %+v", observations)
	}
	decisions, err = st.RecordMaterialProgress(observations, budget, time.Minute, t2)
	if err != nil {
		t.Fatal(err)
	}
	if len(decisions) != 1 || decisions[0].Action != progress.ActionEvidenceUnavailable || decisions[0].RecommendsRecovery() {
		t.Fatalf("tmux failure authorized recovery: %+v", decisions)
	}
}

func TestTerminalCheckpointProgress_LiveTmuxIsCompleteWhenConsumedCheckpointIsMissing(t *testing.T) {
	bin := t.TempDir()
	outputPath := filepath.Join(bin, "output")
	script := filepath.Join(bin, "tmux")
	if err := os.WriteFile(script, []byte(`#!/bin/sh
if [ "$1" = "has-session" ]; then
  exit 1
fi
if [ "$1" != "capture-pane" ] || [ "$2" != "-p" ] || [ "$3" != "-t" ] || [ "$4" != "=mae-exact:" ]; then
  exit 91
fi
cat "$TMUX_FAKE_OUTPUT"
`), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(outputPath, []byte("live terminal progress\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("TMUX_FAKE_OUTPUT", outputPath)

	sess := &state.Session{
		TmuxSession:    "mae-exact",
		CheckpointFile: filepath.Join(bin, "consumed-CHECKPOINT.md"),
	}
	got, complete := terminalCheckpointProgress(sess)
	if got == "" || !complete {
		t.Fatalf("live tmux with consumed checkpoint = (%q,%t), want non-empty complete evidence", got, complete)
	}
}

func TestTmuxProgressFingerprint_TimeoutAndOverflowAreIncomplete(t *testing.T) {
	bin := t.TempDir()
	script := filepath.Join(bin, "tmux")
	if err := os.WriteFile(script, []byte(`#!/bin/sh
if [ "${TMUX_FAKE_SLEEP:-}" = "1" ]; then
  exec sleep 2
fi
cat "$TMUX_FAKE_OUTPUT"
`), 0o755); err != nil {
		t.Fatal(err)
	}
	outputPath := filepath.Join(bin, "output")
	if err := os.WriteFile(outputPath, make([]byte, tmuxProgressMaxOutputBytes+1), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("TMUX_FAKE_OUTPUT", outputPath)
	if got, complete := tmuxProgressFingerprint("mae-exact"); got != "" || complete {
		t.Fatalf("overflow probe = (%q,%t), want explicit incomplete", got, complete)
	}

	oldTimeout := tmuxProgressTimeout
	tmuxProgressTimeout = 20 * time.Millisecond
	t.Cleanup(func() { tmuxProgressTimeout = oldTimeout })
	t.Setenv("TMUX_FAKE_SLEEP", "1")
	if got, complete := tmuxProgressFingerprint("mae-exact"); got != "" || complete {
		t.Fatalf("timeout probe = (%q,%t), want explicit incomplete", got, complete)
	}
}

func TestWorktreeProgressFingerprint_NonRepositoryIsAbsent(t *testing.T) {
	dir := t.TempDir()
	if got := worktreeProgressFingerprint(dir); got != "" {
		t.Fatalf("non-Git directory fingerprint = %q, want absent", got)
	}
	if got, complete := worktreeProgressProbe(dir); got != "" || complete {
		t.Fatalf("non-Git configured probe = (%q,%t), want explicit incomplete", got, complete)
	}
}

func decisionsBySlot(decisions []progress.Decision) map[string]progress.Decision {
	out := make(map[string]progress.Decision, len(decisions))
	for _, decision := range decisions {
		out[decision.Target.Slot] = decision
	}
	return out
}

func signalKindPresent(signals progress.SignalSet, kind progress.SignalKind) bool {
	for _, signal := range signals.Present() {
		if signal.Kind == kind {
			return true
		}
	}
	return false
}

func signalKindListed(signals []progress.SignalKind, kind progress.SignalKind) bool {
	for _, signal := range signals {
		if signal == kind {
			return true
		}
	}
	return false
}

func onlyDeliveryObservation(t *testing.T, st *state.State, now time.Time) progress.Observation {
	t.Helper()
	observations := collectMaterialProgressObservations(st, now)
	if len(observations) != 1 || observations[0].Target.Kind != progress.TargetDelivery {
		t.Fatalf("delivery observations = %+v, want exactly one delivery", observations)
	}
	return observations[0]
}

func onlyPRGateObservation(t *testing.T, st *state.State, now time.Time) progress.Observation {
	t.Helper()
	observations := collectMaterialProgressObservations(st, now)
	if len(observations) != 1 || observations[0].Target.Kind != progress.TargetPRGate {
		t.Fatalf("PR-gate observations = %+v, want exactly one PR gate", observations)
	}
	return observations[0]
}

func recordTestPRGateSnapshot(t *testing.T, st *state.State, issueNumber, prNumber int, headSHA string, now time.Time) state.PRGateSnapshot {
	t.Helper()
	snapshot, _, err := st.RecordPRGateTransition(state.PRGateTransition{
		Project: "owner/repo", IssueNumber: issueNumber, PRNumber: prNumber, HeadSHA: headSHA,
		CIObserved: true, CIRollupVerdict: state.PRGateCIPending, CIEffectiveVerdict: state.PRGateCIPending,
		CheckRollupObserved: true, CheckRollupFingerprint: strings.Repeat("1", 16),
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func materialProgressWatermarkAt(t *testing.T, st *state.State, key string) time.Time {
	t.Helper()
	if st.MaterialProgress == nil || st.MaterialProgress.Targets[key] == nil {
		t.Fatalf("missing material-progress target %q: %+v", key, st.MaterialProgress)
	}
	return st.MaterialProgress.Targets[key].Watermark.At
}

func assertMaterialProgressWatermarkAt(t *testing.T, st *state.State, key string, want time.Time) time.Time {
	t.Helper()
	got := materialProgressWatermarkAt(t, st, key)
	if !got.Equal(want) {
		t.Fatalf("watermark at = %s, want %s", got, want)
	}
	return got
}

func initMaterialProgressGitRepo(t *testing.T) string {
	t.Helper()
	wt := t.TempDir()
	runMaterialProgressGit(t, wt, "init", "-q")
	if err := os.WriteFile(filepath.Join(wt, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runMaterialProgressGit(t, wt, "add", "main.go")
	runMaterialProgressGit(t, wt, "-c", "user.name=Maestro Test", "-c", "user.email=maestro@example.invalid", "commit", "-q", "-m", "initial")
	return wt
}

func runMaterialProgressGit(t *testing.T, worktree string, args ...string) {
	t.Helper()
	cmdArgs := append([]string{"-C", worktree}, args...)
	if out, err := exec.Command("git", cmdArgs...).CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}
