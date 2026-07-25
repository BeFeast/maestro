package state

import (
	"strings"
	"testing"
	"time"
)

func TestMergeControl_HoldInvalidatesClaimAndRefusesNextClaim(t *testing.T) {
	s := NewState()
	now := time.Now().UTC()
	if _, refusal := s.TryClaimMerge(42, 99, "claim-a", strings.Repeat("a", 40), "orchestrator", now); refusal != "" {
		t.Fatalf("first claim refusal = %q", refusal)
	}
	control := s.HoldMerge(42, 99, "operator", "NO-SHIP", now.Add(time.Second))
	if !control.Held || control.ClaimID != "" {
		t.Fatalf("held control = %+v", control)
	}
	if _, refusal := s.TryClaimMerge(42, 99, "claim-b", strings.Repeat("a", 40), "supervisor", now.Add(2*time.Second)); !strings.Contains(refusal, "NO-SHIP") {
		t.Fatalf("refusal = %q", refusal)
	}
}

func TestMergeMergeControls_PreservesLateHoldAgainstStaleWriter(t *testing.T) {
	now := time.Now().UTC()
	current := map[int]MergeControl{7: {PRNumber: 7, Held: true, HoldReason: "late hold", UpdatedAt: now.Add(time.Second)}}
	ours := map[int]MergeControl{7: {PRNumber: 7, ClaimID: "stale", UpdatedAt: now}}
	merged := mergeMergeControls(current, ours)
	if !merged[7].Held || merged[7].HoldReason != "late hold" {
		t.Fatalf("merged control = %+v", merged[7])
	}
}
