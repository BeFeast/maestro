package state

import (
	"sync"
	"testing"
	"time"

	"github.com/befeast/maestro/internal/progress"
)

func TestMaterialProgressRecommendationJournalDue(t *testing.T) {
	target := &MaterialProgressTarget{}
	base := time.Date(2026, 7, 21, 10, 0, 0, 0, time.UTC)

	due, rollUp, count := target.RecommendationJournalDue("watchdog:a", time.Hour, base)
	if !due || rollUp || count != 1 {
		t.Fatalf("first observation = due:%v rollup:%v count:%d, want transition", due, rollUp, count)
	}
	for i := 1; i <= 5; i++ {
		due, rollUp, count = target.RecommendationJournalDue("watchdog:a", time.Hour, base.Add(time.Duration(i)*10*time.Minute))
		if due || !rollUp || count != i+1 {
			t.Fatalf("repeat %d = due:%v rollup:%v count:%d, want suppressed observation", i, due, rollUp, count)
		}
	}
	due, rollUp, count = target.RecommendationJournalDue("watchdog:a", time.Hour, base.Add(time.Hour))
	if !due || !rollUp || count != 7 {
		t.Fatalf("window boundary = due:%v rollup:%v count:%d, want roll-up count 7", due, rollUp, count)
	}
	if !target.RecommendationFirstSeenAt.Equal(base) || !target.RecommendationLastSeenAt.Equal(base.Add(time.Hour)) {
		t.Fatalf("first/last seen = %s/%s", target.RecommendationFirstSeenAt, target.RecommendationLastSeenAt)
	}

	due, rollUp, count = target.RecommendationJournalDue("watchdog:b", time.Hour, base.Add(time.Hour+time.Minute))
	if !due || rollUp || count != 1 {
		t.Fatalf("changed recommendation = due:%v rollup:%v count:%d, want transition", due, rollUp, count)
	}
}

var mpBase = time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)

func workerObservation(issue int, slot string, pid int, lease, head string) progress.Observation {
	return progress.Observation{
		Target: progress.Target{
			Kind:        progress.TargetWorker,
			IssueNumber: issue,
			Slot:        slot,
			SessionID:   "started-" + lease,
			TmuxSession: "tmux-" + slot,
			ProcessID:   pid,
			LeaseID:     lease,
		},
		Signals: progress.SignalSet{
			{Kind: progress.SignalProcessTmux, Fingerprint: progress.Fingerprint(slot, lease)},
			{Kind: progress.SignalWorktreeGit, Fingerprint: progress.Fingerprint(head)},
		},
		Phase: progress.PhasePreDelivery,
	}
}

func recordOne(t *testing.T, s *State, observation progress.Observation, budget time.Duration, now time.Time) progress.Decision {
	t.Helper()
	decisions, err := s.RecordMaterialProgress([]progress.Observation{observation}, budget, time.Minute, now)
	if err != nil {
		t.Fatalf("RecordMaterialProgress: %v", err)
	}
	if len(decisions) != 1 {
		t.Fatalf("decisions = %d, want 1", len(decisions))
	}
	return decisions[0]
}

func targetRecord(t *testing.T, s *State, observation progress.Observation) *MaterialProgressTarget {
	t.Helper()
	if s.MaterialProgress == nil {
		t.Fatal("material progress container is nil")
	}
	record := s.MaterialProgress.Targets[observation.Target.Key()]
	if record == nil {
		t.Fatalf("target %q not persisted", observation.Target.Key())
	}
	return record
}

func TestRecordMaterialProgress_IndependentTargets(t *testing.T) {
	s := NewState()
	budget := 20 * time.Minute
	a := workerObservation(10, "1", 101, "lease-a", "head-a1")
	b := workerObservation(11, "2", 202, "lease-b", "head-b1")
	if _, err := s.RecordMaterialProgress([]progress.Observation{a, b}, budget, time.Minute, mpBase); err != nil {
		t.Fatal(err)
	}

	// Worker A advances while worker B remains frozen past its own deadline.
	a.Signals[1].Fingerprint = progress.Fingerprint("head-a2")
	decisions, err := s.RecordMaterialProgress([]progress.Observation{a, b}, budget, time.Minute, mpBase.Add(budget+time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	byTarget := map[string]progress.Decision{}
	for _, decision := range decisions {
		byTarget[decision.Target.Key()] = decision
	}
	if got := byTarget[a.Target.Key()].Action; got != progress.ActionNone {
		t.Fatalf("active worker action = %q, want none", got)
	}
	if got := byTarget[b.Target.Key()].Action; got != progress.ActionStopAndRetry {
		t.Fatalf("hung worker action = %q, want stop_and_retry", got)
	}
	if targetRecord(t, s, a).Watermark.At.Equal(targetRecord(t, s, b).Watermark.At) {
		t.Fatal("independent worker watermarks collapsed together")
	}
}

func TestRecordMaterialProgress_RecommendationIsNotRecoveryOrRefreshed(t *testing.T) {
	s := NewState()
	budget := 20 * time.Minute
	worker := workerObservation(10, "1", 101, "lease-a", "head-a1")
	recordOne(t, s, worker, budget, mpBase)
	firstAt := mpBase.Add(budget + time.Minute)
	first := recordOne(t, s, worker, budget, firstAt)
	if !first.RecommendsRecovery() || first.RecommendationID == "" {
		t.Fatalf("first overdue decision = %+v, want recommendation", first)
	}
	record := targetRecord(t, s, worker)
	if record.LastRecommendation == nil {
		t.Fatal("recommendation not persisted")
	}
	if got := record.LastRecovery(); got != nil {
		t.Fatalf("evaluation claimed actual recovery: %+v", got)
	}

	second := recordOne(t, s, worker, budget, firstAt.Add(10*time.Minute))
	if second.RecommendationID != first.RecommendationID {
		t.Fatalf("repeated overdue recommendation id changed: %q != %q", second.RecommendationID, first.RecommendationID)
	}
	if got := targetRecord(t, s, worker).LastRecommendation.EvaluatedAt; !got.Equal(firstAt) {
		t.Fatalf("repeated overdue evaluation refreshed recommendation: %s, want %s", got, firstAt)
	}
	if got := targetRecord(t, s, worker).LastDecision.EvaluatedAt; !got.Equal(firstAt.Add(10 * time.Minute)) {
		t.Fatalf("latest verdict was not refreshed: %s", got)
	}
}

func TestRecordMaterialRecovery_ExplicitExactlyOnceAttemptAndResult(t *testing.T) {
	s := NewState()
	worker := workerObservation(10, "1", 101, "lease-a", "head-a1")
	recordOne(t, s, worker, time.Minute, mpBase)
	decision := recordOne(t, s, worker, time.Minute, mpBase.Add(2*time.Minute))
	key := worker.Target.Key()
	attemptAt := mpBase.Add(2*time.Minute + time.Second)
	if err := s.RecordMaterialRecovery(key, decision.RecommendationID, progress.RecoveryAttempted, attemptAt); err != nil {
		t.Fatal(err)
	}
	// Idempotent replay does not append a second attempt.
	if err := s.RecordMaterialRecovery(key, decision.RecommendationID, progress.RecoveryAttempted, attemptAt.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordMaterialRecovery(key, decision.RecommendationID, progress.RecoverySucceeded, attemptAt.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	record := targetRecord(t, s, worker)
	if len(record.Recoveries) != 1 {
		t.Fatalf("recoveries = %d, want exactly 1", len(record.Recoveries))
	}
	if got := record.LastRecovery(); got == nil || got.Outcome != progress.RecoverySucceeded {
		t.Fatalf("LastRecovery = %+v, want succeeded", got)
	}
	if err := s.RecordMaterialRecovery(key, decision.RecommendationID, progress.RecoveryFailed, attemptAt.Add(3*time.Second)); err == nil {
		t.Fatal("conflicting second terminal result was accepted")
	}
}

func TestMaterialProgressRecoveryLease_ExpiresAndRejectsStaleCompletion(t *testing.T) {
	s := NewState()
	worker := workerObservation(10, "1", 101, "lease-a", "head-a1")
	recordOne(t, s, worker, time.Minute, mpBase)
	decision := recordOne(t, s, worker, time.Minute, mpBase.Add(2*time.Minute))
	key := worker.Target.Key()
	claimed, err := s.ClaimMaterialRecovery(key, decision.RecommendationID, "lease-1", time.Minute, mpBase.Add(2*time.Minute))
	if err != nil || !claimed {
		t.Fatalf("first claim: claimed=%t err=%v", claimed, err)
	}
	claimed, err = s.ClaimMaterialRecovery(key, decision.RecommendationID, "lease-2", time.Minute, mpBase.Add(2*time.Minute+30*time.Second))
	if err != nil || claimed {
		t.Fatalf("live lease takeover: claimed=%t err=%v", claimed, err)
	}
	claimed, err = s.ClaimMaterialRecovery(key, decision.RecommendationID, "lease-2", time.Minute, mpBase.Add(4*time.Minute))
	if err != nil || !claimed {
		t.Fatalf("expired lease takeover: claimed=%t err=%v", claimed, err)
	}
	if err := s.CompleteMaterialRecovery(key, decision.RecommendationID, "lease-1", progress.RecoverySucceeded,
		progress.RecoveryStageRetryScheduled, progress.RecoveryReasonRetryScheduled, mpBase.Add(4*time.Minute)); err == nil {
		t.Fatal("stale lease completed a newer recovery claim")
	}
	if err := s.CompleteMaterialRecovery(key, decision.RecommendationID, "lease-2", progress.RecoverySucceeded,
		progress.RecoveryStageRetryScheduled, progress.RecoveryReasonRetryScheduled, mpBase.Add(4*time.Minute)); err != nil {
		t.Fatal(err)
	}
	recovery := targetRecord(t, s, worker).LastRecovery()
	if recovery == nil || recovery.LeaseGeneration != 2 || recovery.Outcome != progress.RecoverySucceeded {
		t.Fatalf("recovery = %+v", recovery)
	}
}

func TestMaterialProgressRecoveryLease_RejectsTakeoverAfterProgress(t *testing.T) {
	s := NewState()
	worker := workerObservation(10, "1", 101, "lease-a", "head-a1")
	recordOne(t, s, worker, time.Minute, mpBase)
	decision := recordOne(t, s, worker, time.Minute, mpBase.Add(2*time.Minute))
	key := worker.Target.Key()
	claimed, err := s.ClaimMaterialRecovery(key, decision.RecommendationID, "lease-1", time.Minute, mpBase.Add(2*time.Minute))
	if err != nil || !claimed {
		t.Fatalf("first claim: claimed=%t err=%v", claimed, err)
	}

	advanced := workerObservation(10, "1", 101, "lease-a", "head-a2")
	advancedDecision := recordOne(t, s, advanced, time.Minute, mpBase.Add(3*time.Minute))
	if advancedDecision.Action != progress.ActionNone {
		t.Fatalf("advanced decision = %+v", advancedDecision)
	}
	claimed, err = s.ClaimMaterialRecovery(key, decision.RecommendationID, "lease-2", time.Minute, mpBase.Add(4*time.Minute))
	if err != nil || claimed {
		t.Fatalf("stale recovery takeover: claimed=%t err=%v", claimed, err)
	}
	recovery := targetRecord(t, s, advanced).LastRecovery()
	if recovery == nil || recovery.LeaseGeneration != 1 || recovery.LeaseID != "lease-1" {
		t.Fatalf("stale recovery claim changed = %+v", recovery)
	}
}

func TestRecordMaterialProgress_DisabledToEnabledGetsFreshBaseline(t *testing.T) {
	s := NewState()
	worker := workerObservation(10, "1", 101, "lease-a", "head-a1")
	if got := recordOne(t, s, worker, 0, mpBase).Action; got != progress.ActionDisabled {
		t.Fatalf("disabled action = %q", got)
	}
	// Hours of disabled silence must not become an immediate stall on enable.
	recordOne(t, s, worker, 0, mpBase.Add(3*time.Hour))
	enabledAt := mpBase.Add(4 * time.Hour)
	decision := recordOne(t, s, worker, 20*time.Minute, enabledAt)
	if decision.Action != progress.ActionNone {
		t.Fatalf("enable action = %q, want fresh baseline", decision.Action)
	}
	if !decision.WatermarkAt.Equal(enabledAt) {
		t.Fatalf("enable baseline = %s, want %s", decision.WatermarkAt, enabledAt)
	}
}

func TestRecordMaterialProgress_BudgetChangeGetsFreshBaseline(t *testing.T) {
	s := NewState()
	worker := workerObservation(10, "1", 101, "lease-a", "head-a1")
	recordOne(t, s, worker, 20*time.Minute, mpBase)
	changedAt := mpBase.Add(30 * time.Minute)
	decision := recordOne(t, s, worker, 60*time.Minute, changedAt)
	if decision.Action != progress.ActionNone {
		t.Fatalf("budget-change action = %q, want fresh baseline", decision.Action)
	}
	if want := changedAt.Add(60 * time.Minute); !decision.Deadline.Equal(want) {
		t.Fatalf("budget-change deadline = %s, want %s", decision.Deadline, want)
	}
}

func TestRecordMaterialProgress_RetiresAbsentTargets(t *testing.T) {
	s := NewState()
	worker := workerObservation(10, "1", 101, "lease-a", "head-a1")
	recordOne(t, s, worker, 20*time.Minute, mpBase)
	decisions, err := s.RecordMaterialProgress(nil, 20*time.Minute, time.Minute, mpBase.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(decisions) != 0 {
		t.Fatalf("idle snapshot produced %d decisions", len(decisions))
	}
	record := targetRecord(t, s, worker)
	if record.Active || !record.Deadline(1200).IsZero() {
		t.Fatalf("retired target still armed: %+v", record)
	}
	// Reactivation is a fresh baseline, never an inherited overdue deadline.
	reactivated := recordOne(t, s, worker, 20*time.Minute, mpBase.Add(time.Hour))
	if reactivated.Action != progress.ActionNone || !reactivated.WatermarkAt.Equal(mpBase.Add(time.Hour)) {
		t.Fatalf("reactivated decision = %+v", reactivated)
	}
}

func TestMaterialProgress_ConcurrentSavePreservesRecoveryAndProgress(t *testing.T) {
	dir := t.TempDir()
	worker := workerObservation(10, "1", 101, "lease-a", "head-a1")
	seed := NewState()
	recordOne(t, seed, worker, 20*time.Minute, mpBase)
	if err := Save(dir, seed); err != nil {
		t.Fatal(err)
	}

	stalled, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	advancing, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	stallAt := mpBase.Add(21 * time.Minute)
	recommendation := recordOne(t, stalled, worker, 20*time.Minute, stallAt)
	if err := stalled.RecordMaterialRecovery(worker.Target.Key(), recommendation.RecommendationID, progress.RecoveryAttempted, stallAt.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	advancedWorker := worker
	advancedWorker.Signals = append(progress.SignalSet(nil), worker.Signals...)
	advancedWorker.Signals[1].Fingerprint = progress.Fingerprint("head-a2")
	advanceAt := stallAt.Add(time.Minute)
	recordOne(t, advancing, advancedWorker, 20*time.Minute, advanceAt)

	// Exercise both lock orders repeatedly: Save performs a three-way merge
	// against the snapshot each writer originally loaded.
	var wg sync.WaitGroup
	start := make(chan struct{})
	errs := make(chan error, 2)
	for _, snapshot := range []*State{stalled, advancing} {
		wg.Add(1)
		go func(snapshot *State) {
			defer wg.Done()
			<-start
			errs <- Save(dir, snapshot)
		}(snapshot)
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}

	reloaded, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	record := targetRecord(t, reloaded, worker)
	if !record.Watermark.At.Equal(advanceAt) {
		t.Fatalf("concurrent Save lost progress: watermark=%s want=%s", record.Watermark.At, advanceAt)
	}
	if recovery := record.LastRecovery(); recovery == nil || recovery.RecommendationID != recommendation.RecommendationID {
		t.Fatalf("concurrent Save lost actual recovery history: %+v", recovery)
	}
}

func TestMaterialProgress_ConcurrentStaleRecoveryCannotReactivateRetiredTarget(t *testing.T) {
	dir := t.TempDir()
	worker := workerObservation(10, "1", 101, "lease-a", "head-a1")
	seed := NewState()
	recordOne(t, seed, worker, time.Minute, mpBase)
	if err := Save(dir, seed); err != nil {
		t.Fatal(err)
	}

	recoveryWriter, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	retireWriter, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	recommendation := recordOne(t, recoveryWriter, worker, time.Minute, mpBase.Add(2*time.Minute))
	if err := recoveryWriter.RecordMaterialRecovery(worker.Target.Key(), recommendation.RecommendationID, progress.RecoveryAttempted, mpBase.Add(4*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := retireWriter.RecordMaterialProgress(nil, time.Minute, time.Minute, mpBase.Add(3*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := Save(dir, retireWriter); err != nil {
		t.Fatal(err)
	}
	if err := Save(dir, recoveryWriter); err != nil {
		t.Fatal(err)
	}

	reloaded, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	record := targetRecord(t, reloaded, worker)
	if record.Active {
		t.Fatal("stale recovery writer reactivated a target retired by a newer complete snapshot")
	}
	if recovery := record.LastRecovery(); recovery == nil || recovery.RecommendationID != recommendation.RecommendationID {
		t.Fatalf("retirement merge dropped recovery history: %+v", recovery)
	}
}

func TestMaterialProgress_EvaluationDueIncludesConfigTransitions(t *testing.T) {
	var nilMP *MaterialProgress
	if !nilMP.EvaluationDue(20*time.Minute, time.Minute, mpBase) {
		t.Fatal("nil state should be due")
	}
	mp := &MaterialProgress{BudgetSeconds: 1200, EvalIntervalSeconds: 60, LastEvaluatedAt: mpBase}
	if mp.EvaluationDue(20*time.Minute, time.Minute, mpBase.Add(30*time.Second)) {
		t.Fatal("evaluation inside cadence reported due")
	}
	if !mp.EvaluationDue(60*time.Minute, time.Minute, mpBase.Add(30*time.Second)) {
		t.Fatal("budget change did not bypass cadence")
	}
	if !mp.EvaluationDue(20*time.Minute, 5*time.Minute, mpBase.Add(30*time.Second)) {
		t.Fatal("evaluation cadence change did not bypass cadence")
	}
	if !mp.EvaluationDue(20*time.Minute, time.Minute, mpBase.Add(time.Minute)) {
		t.Fatal("elapsed cadence did not report due")
	}
}
