package state

import (
	"testing"
	"time"

	"github.com/befeast/maestro/internal/progress"
)

var mpBase = time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)

func procGitSet(pid, head string) progress.SignalSet {
	return progress.SignalSet{
		{Kind: progress.SignalProcessTmux, Fingerprint: progress.Fingerprint(pid)},
		{Kind: progress.SignalWorktreeGit, Fingerprint: progress.Fingerprint(head)},
	}
}

// RecordMaterialProgress persists the watermark and returns the recovery
// decision; a quiet-but-active worker (git advancing) never trips the watchdog.
func TestRecordMaterialProgress_QuietButActive(t *testing.T) {
	s := NewState()
	budget := 20 * time.Minute
	now := mpBase
	for i := 0; i < 6; i++ {
		now = now.Add(30 * time.Minute) // past the budget every tick
		dec := s.RecordMaterialProgress(procGitSet("pid-1", "head-"+time.Duration(i).String()), progress.PhasePreDelivery, budget, time.Minute, now)
		if dec.Acted() {
			t.Fatalf("tick %d acted on an actively-editing worker: %q", i, dec.Action)
		}
	}
	if s.MaterialProgress == nil {
		t.Fatalf("watermark not persisted")
	}
	if s.MaterialProgress.LastRecovery != nil {
		t.Fatalf("recovery recorded for a healthy worker")
	}
}

func TestRecordMaterialProgress_StallRecordsRecovery(t *testing.T) {
	s := NewState()
	budget := 20 * time.Minute
	frozen := procGitSet("pid-1", "head-1")
	s.RecordMaterialProgress(frozen, progress.PhasePreDelivery, budget, time.Minute, mpBase)

	dec := s.RecordMaterialProgress(frozen, progress.PhasePreDelivery, budget, time.Minute, mpBase.Add(budget+time.Minute))
	if dec.Action != progress.ActionStopAndRetry {
		t.Fatalf("action = %q, want stop_and_retry", dec.Action)
	}
	if s.MaterialProgress.LastRecovery == nil {
		t.Fatalf("recovery decision not persisted")
	}
	if s.MaterialProgress.LastRecovery.Action != progress.ActionStopAndRetry {
		t.Fatalf("LastRecovery.Action = %q", s.MaterialProgress.LastRecovery.Action)
	}
	// The reported deadline matches Watermark.At + budget.
	if got, want := s.MaterialProgress.Deadline(), mpBase.Add(budget); !got.Equal(want) {
		t.Fatalf("Deadline = %s, want %s", got, want)
	}
}

// Requirement: survive daemon restart without resetting or duplicating the
// deadline. Save then Load the state and confirm the watermark + derived
// deadline are unchanged.
func TestMaterialProgress_SurvivesSaveLoad(t *testing.T) {
	dir := t.TempDir()
	s := NewState()
	budget := 20 * time.Minute
	frozen := procGitSet("pid-1", "head-1")
	s.RecordMaterialProgress(frozen, progress.PhasePreDelivery, budget, time.Minute, mpBase)
	deadlineBefore := s.MaterialProgress.Deadline()

	if err := Save(dir, s); err != nil {
		t.Fatalf("Save: %v", err)
	}
	reloaded, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if reloaded.MaterialProgress == nil {
		t.Fatalf("watermark lost across Save/Load")
	}
	if got := reloaded.MaterialProgress.Watermark.At; !got.Equal(mpBase) {
		t.Fatalf("watermark At = %s, want %s", got, mpBase)
	}
	if got := reloaded.MaterialProgress.Deadline(); !got.Equal(deadlineBefore) {
		t.Fatalf("deadline after restart = %s, want %s (must not reset)", got, deadlineBefore)
	}

	// Re-evaluating the frozen signals after "restart" keeps the same deadline.
	dec := reloaded.RecordMaterialProgress(frozen, progress.PhasePreDelivery, budget, time.Minute, mpBase.Add(5*time.Minute))
	if dec.Action != progress.ActionWaiting {
		t.Fatalf("post-restart action = %q, want waiting", dec.Action)
	}
	if !dec.Deadline.Equal(deadlineBefore) {
		t.Fatalf("post-restart deadline = %s, want %s", dec.Deadline, deadlineBefore)
	}
}

// A concurrent stall record must not clobber a fresh progress advance: the
// merge keeps the snapshot with the newer material-progress time.
func TestMergeMaterialProgress_ProgressBeatsStall(t *testing.T) {
	budget := 20 * time.Minute

	// current: a stale snapshot still on the old watermark.
	current := NewState()
	current.RecordMaterialProgress(procGitSet("pid-1", "head-1"), progress.PhasePreDelivery, budget, time.Minute, mpBase)

	// ours: a fresh snapshot that observed new git progress later.
	ours := NewState()
	ours.RecordMaterialProgress(procGitSet("pid-1", "head-2"), progress.PhasePreDelivery, budget, time.Minute, mpBase.Add(10*time.Minute))

	merged := NewState()
	mergeMaterialProgress(merged, current, ours)
	if merged.MaterialProgress == nil {
		t.Fatalf("merge dropped the watermark")
	}
	if got := merged.MaterialProgress.Watermark.At; !got.Equal(mpBase.Add(10 * time.Minute)) {
		t.Fatalf("merge kept the stale watermark At=%s, want the fresher progress", got)
	}

	// Order-independent: swapping current/ours keeps the same winner.
	merged2 := NewState()
	mergeMaterialProgress(merged2, ours, current)
	if got := merged2.MaterialProgress.Watermark.At; !got.Equal(mpBase.Add(10 * time.Minute)) {
		t.Fatalf("merge not order-independent: At=%s", got)
	}
}

func TestMergeMaterialProgress_NilHandling(t *testing.T) {
	populated := NewState()
	populated.RecordMaterialProgress(procGitSet("pid-1", "head-1"), progress.PhasePreDelivery, time.Minute, time.Minute, mpBase)

	merged := NewState()
	mergeMaterialProgress(merged, NewState(), populated)
	if merged.MaterialProgress == nil {
		t.Fatalf("merge dropped the only populated watermark (ours)")
	}
	merged2 := NewState()
	mergeMaterialProgress(merged2, populated, NewState())
	if merged2.MaterialProgress == nil {
		t.Fatalf("merge dropped the only populated watermark (current)")
	}
}

// A concurrent stall record must not be lost when the other writer's progress
// advance wins the merge base: the newer LastRecovery/LastDecision is preserved
// independently of the watermark winner (#887 review).
func TestMergeMaterialProgress_PreservesRecoveryUnderConcurrentAdvance(t *testing.T) {
	budget := 20 * time.Minute
	frozen := procGitSet("pid-1", "head-1")

	// Writer A: observes a frozen worker past the budget and records a recovery.
	a := NewState()
	a.RecordMaterialProgress(frozen, progress.PhasePreDelivery, budget, time.Minute, mpBase)
	stallAt := mpBase.Add(budget + time.Minute)
	decA := a.RecordMaterialProgress(frozen, progress.PhasePreDelivery, budget, time.Minute, stallAt)
	if decA.Action != progress.ActionStopAndRetry {
		t.Fatalf("setup: writer A action = %q, want stop_and_retry", decA.Action)
	}
	if a.MaterialProgress.LastRecovery == nil {
		t.Fatalf("setup: writer A recorded no recovery")
	}

	// Writer B: a fresher progress advance (head-2) even later, no recovery.
	b := NewState()
	b.RecordMaterialProgress(procGitSet("pid-1", "head-2"), progress.PhasePreDelivery, budget, time.Minute, stallAt.Add(time.Minute))
	if b.MaterialProgress.LastRecovery != nil {
		t.Fatalf("setup: writer B unexpectedly recorded a recovery")
	}

	// Merge: B's fresher watermark wins the base, but A's recovery must survive.
	merged := NewState()
	mergeMaterialProgress(merged, a, b)
	if merged.MaterialProgress == nil {
		t.Fatalf("merge dropped the watermark")
	}
	if got := merged.MaterialProgress.Watermark.At; !got.Equal(stallAt.Add(time.Minute)) {
		t.Fatalf("merge kept a stale watermark At=%s, want the fresher progress", got)
	}
	if merged.MaterialProgress.LastRecovery == nil {
		t.Fatalf("merge dropped the concurrently-recorded recovery")
	}
	if merged.MaterialProgress.LastRecovery.Action != progress.ActionStopAndRetry {
		t.Fatalf("LastRecovery.Action = %q, want stop_and_retry", merged.MaterialProgress.LastRecovery.Action)
	}

	// Order-independent.
	merged2 := NewState()
	mergeMaterialProgress(merged2, b, a)
	if merged2.MaterialProgress.LastRecovery == nil || merged2.MaterialProgress.LastRecovery.Action != progress.ActionStopAndRetry {
		t.Fatalf("merge not order-independent for recovery preservation")
	}
}
