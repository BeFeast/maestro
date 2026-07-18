package state

import (
	"testing"
	"time"

	"github.com/befeast/maestro/internal/outcome"
)

func TestMergeStateSnapshotsPreservesNewestOutcomeRecoveryLease(t *testing.T) {
	t0 := time.Date(2026, 7, 18, 8, 0, 0, 0, time.UTC)
	base := NewState()
	current := cloneState(base)
	ours := cloneState(base)
	current.OutcomeRecovery = &outcome.RecoveryState{
		AttemptID: "leased", Status: outcome.RecoveryStatusExecuting, Attempts: 1, UpdatedAt: t0.Add(time.Minute),
	}
	ours.OutcomeRecovery = &outcome.RecoveryState{
		AttemptID: "stale", Status: outcome.RecoveryStatusFailed, Attempts: 1, UpdatedAt: t0,
	}

	merged, err := mergeStateSnapshots(base, current, ours)
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	if merged.OutcomeRecovery == nil || merged.OutcomeRecovery.AttemptID != "leased" || merged.OutcomeRecovery.Status != outcome.RecoveryStatusExecuting {
		t.Fatalf("newest execution lease lost: %+v", merged.OutcomeRecovery)
	}
}
