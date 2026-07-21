package outcome

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRecoveryRunnerTimeoutKillsDescendantProcessGroup(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "descendant-survived")
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	execution := (RecoveryRunner{}).Run(ctx, Brief{
		RecoveryCommand: fmt.Sprintf("(sleep 1; touch %q) & wait", marker),
	})
	if !execution.TimedOut {
		t.Fatalf("TimedOut = false, receipt=%+v", execution)
	}

	time.Sleep(1100 * time.Millisecond)
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("recovery descendant survived timeout and produced its marker")
	} else if !os.IsNotExist(err) {
		t.Fatalf("stat marker: %v", err)
	}
}

func TestRecoveryStateOwnsActiveFailure(t *testing.T) {
	now := time.Date(2026, 7, 18, 8, 42, 0, 0, time.UTC)
	cases := []struct {
		name  string
		state *RecoveryState
		want  bool
	}{
		{name: "nil", state: nil, want: false},
		{name: "executing", state: &RecoveryState{Status: RecoveryStatusExecuting}, want: true},
		{name: "verification_pending", state: &RecoveryState{Status: RecoveryStatusVerificationPending}, want: true},
		{name: "verified", state: &RecoveryState{Status: RecoveryStatusVerified}, want: false},
		{name: "uncertain", state: &RecoveryState{Status: RecoveryStatusUncertain}, want: false},
		{name: "failed_no_cooldown", state: &RecoveryState{Status: RecoveryStatusFailed}, want: false},
		{
			name:  "failed_cooldown_bounded",
			state: &RecoveryState{Status: RecoveryStatusFailed, NextEligibleAt: now.Add(2 * time.Minute)},
			want:  true,
		},
		{
			name:  "failed_cooldown_elapsed",
			state: &RecoveryState{Status: RecoveryStatusFailed, NextEligibleAt: now.Add(-2 * time.Minute)},
			want:  false,
		},
		{
			name:  "failed_cooldown_boundary_not_owned",
			state: &RecoveryState{Status: RecoveryStatusFailed, NextEligibleAt: now},
			want:  false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.state.OwnsActiveFailure(now); got != tc.want {
				t.Fatalf("OwnsActiveFailure = %v, want %v", got, tc.want)
			}
		})
	}
}
