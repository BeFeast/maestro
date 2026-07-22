package state

import (
	"strings"
	"testing"
	"time"
)

func TestRecordDispatchCycleAlertsOnceAfterTwoConsecutiveCycles(t *testing.T) {
	now := time.Date(2026, 7, 21, 18, 0, 0, 0, time.UTC)
	s := NewState()
	hold := DispatchHold{
		Active:      true,
		ReasonClass: DispatchHoldBlockingOutcomeCheck,
		Detail:      "blocking outcome check smoke is failing",
	}

	if s.RecordDispatchCycle(hold, 0, 1, now) {
		t.Fatal("first idle cycle must not alert")
	}
	if !s.RecordDispatchCycle(hold, 0, 1, now.Add(time.Minute)) {
		t.Fatal("second consecutive idle cycle must alert")
	}
	if got := s.DispatchHold.Since; !got.Equal(now) {
		t.Fatalf("hold since = %s, want %s", got, now)
	}
	s.MarkIdleStallNotified(DispatchHoldBlockingOutcomeCheck)
	if s.RecordDispatchCycle(hold, 0, 1, now.Add(2*time.Minute)) {
		t.Fatal("notified unchanged stall must stay deduped")
	}

	if s.RecordDispatchCycle(DispatchHold{}, 1, 1, now.Add(3*time.Minute)) {
		t.Fatal("live worker must clear the stall")
	}
	if s.IdleStall.ConsecutiveCycles != 0 || s.DispatchHold.Active {
		t.Fatalf("cleared state = hold %+v idle %+v", s.DispatchHold, s.IdleStall)
	}
}

func TestRecordDispatchCycleAlertsOnEmptyReadyQueue(t *testing.T) {
	// F8 companion: empty ready + 0 live must still notify after two cycles.
	now := time.Date(2026, 7, 22, 14, 0, 0, 0, time.UTC)
	s := NewState()
	hold := DispatchHold{
		Active:      true,
		ReasonClass: DispatchHoldQueueEmpty,
		Detail:      "no live workers and no eligible ready issues to dispatch",
	}
	if s.RecordDispatchCycle(hold, 0, 0, now) {
		t.Fatal("first empty-ready idle cycle must not alert")
	}
	if !s.RecordDispatchCycle(hold, 0, 0, now.Add(time.Minute)) {
		t.Fatal("second empty-ready idle cycle must alert")
	}
	if s.IdleStall.ReasonClass != DispatchHoldQueueEmpty {
		t.Fatalf("reason = %q, want %q", s.IdleStall.ReasonClass, DispatchHoldQueueEmpty)
	}
}

func TestRecordDispatchCycleBoundsPublicHoldText(t *testing.T) {
	s := NewState()
	now := time.Date(2026, 7, 21, 18, 0, 0, 0, time.UTC)
	s.RecordDispatchCycle(DispatchHold{
		Active:      true,
		ReasonClass: strings.Repeat("r", maxDispatchHoldReasonRunes+20),
		Detail:      strings.Repeat("д", maxDispatchHoldDetailRunes+20),
	}, 0, 1, now)
	if got := len([]rune(s.DispatchHold.ReasonClass)); got > maxDispatchHoldReasonRunes {
		t.Fatalf("reason class runes = %d, want bounded", got)
	}
	if got := len([]rune(s.DispatchHold.Detail)); got > maxDispatchHoldDetailRunes {
		t.Fatalf("detail runes = %d, want bounded", got)
	}
}

func TestMergeStateSnapshotsPreservesNewestDispatchVisibility(t *testing.T) {
	base := NewState()
	current := cloneState(base)
	ours := cloneState(base)
	now := time.Date(2026, 7, 21, 18, 0, 0, 0, time.UTC)

	current.RecordDispatchCycle(DispatchHold{Active: true, ReasonClass: DispatchHoldPaused, Detail: "paused"}, 0, 0, now)
	ours.RecordDispatchCycle(DispatchHold{Active: true, ReasonClass: DispatchHoldBackendsCoolingDown, Detail: "cooling"}, 0, 1, now.Add(time.Minute))

	merged, err := mergeStateSnapshots(base, current, ours)
	if err != nil {
		t.Fatalf("mergeStateSnapshots: %v", err)
	}
	if got := merged.DispatchHold.ReasonClass; got != DispatchHoldBackendsCoolingDown {
		t.Fatalf("reason class = %q, want newest %q", got, DispatchHoldBackendsCoolingDown)
	}
	if merged.IdleStall.ConsecutiveCycles != 1 {
		t.Fatalf("idle cycles = %d, want 1", merged.IdleStall.ConsecutiveCycles)
	}
}
