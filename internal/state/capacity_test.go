package state

import (
	"testing"
	"time"
)

func stateWith(statuses ...SessionStatus) *State {
	s := NewState()
	for i, st := range statuses {
		s.Sessions[slotName(i)] = &Session{Status: st}
	}
	return s
}

func slotName(i int) string {
	return "slot-" + string(rune('a'+i))
}

// #814: legacy accounting (max_live_workers unset) counts running + pr_open
// against max_parallel, and reports the live-worker slots that pr_open sessions
// withhold.
func TestCapacity_LegacyCountsPROpenAgainstParallel(t *testing.T) {
	s := stateWith(StatusRunning, StatusRunning, StatusPROpen, StatusPROpen)
	c := s.Capacity(CapacityInput{MaxParallel: 4})

	if c.LiveWorkers != 2 || c.PRGates != 2 {
		t.Fatalf("LiveWorkers=%d PRGates=%d, want 2/2", c.LiveWorkers, c.PRGates)
	}
	if c.AvailableSlots != 0 {
		t.Errorf("AvailableSlots = %d, want 0 (pr_open fills max_parallel)", c.AvailableSlots)
	}
	if c.CapacityUsed != 4 {
		t.Errorf("CapacityUsed = %d, want 4", c.CapacityUsed)
	}
	if c.BlockedByGates != 2 {
		t.Errorf("BlockedByGates = %d, want 2 (both gates withhold a live slot)", c.BlockedByGates)
	}
	if c.Separated {
		t.Errorf("Separated = true, want false in legacy mode")
	}
}

// The recurring misleading state: every slot is a PR gate, so a naive
// "workers_running" reads 0 while the queue is not idle.
func TestCapacity_LegacyGateBoundWhenAllPROpen(t *testing.T) {
	s := stateWith(StatusPROpen, StatusPROpen, StatusPROpen, StatusPROpen)
	c := s.Capacity(CapacityInput{MaxParallel: 4})

	if c.AvailableSlots != 0 {
		t.Errorf("AvailableSlots = %d, want 0", c.AvailableSlots)
	}
	if c.BlockedByGates != 4 {
		t.Errorf("BlockedByGates = %d, want 4", c.BlockedByGates)
	}
	if !c.GateBound() {
		t.Errorf("GateBound() = false, want true (0 live workers, gates hold all capacity)")
	}
}

// The core fix: with max_live_workers>0, pr_open sessions no longer consume
// live-worker capacity, so eligible work keeps dispatching.
func TestCapacity_SeparatedIgnoresPROpen(t *testing.T) {
	s := stateWith(StatusRunning, StatusPROpen, StatusPROpen, StatusPROpen, StatusPROpen)
	c := s.Capacity(CapacityInput{MaxParallel: 4, MaxLiveWorkers: 3})

	if !c.Separated {
		t.Fatalf("Separated = false, want true")
	}
	if c.PRGates != 4 {
		t.Fatalf("PRGates = %d, want 4", c.PRGates)
	}
	if c.AvailableSlots != 2 {
		t.Errorf("AvailableSlots = %d, want 2 (3 live limit - 1 running, gates ignored)", c.AvailableSlots)
	}
	if c.CapacityUsed != 1 {
		t.Errorf("CapacityUsed = %d, want 1 (live workers only)", c.CapacityUsed)
	}
	if c.BlockedByGates != 0 {
		t.Errorf("BlockedByGates = %d, want 0 (gates separated out)", c.BlockedByGates)
	}
	if c.GateBound() {
		t.Errorf("GateBound() = true, want false (slots available)")
	}
}

func TestCapacity_SeparatedRespectsLiveLimit(t *testing.T) {
	s := stateWith(StatusRunning, StatusRunning, StatusRunning, StatusPROpen)
	c := s.Capacity(CapacityInput{MaxParallel: 10, MaxLiveWorkers: 3})
	if c.AvailableSlots != 0 {
		t.Errorf("AvailableSlots = %d, want 0 (at live limit)", c.AvailableSlots)
	}
}

// The per-state "running" limit still caps live-worker spawning under
// separated accounting — new workers always enter StatusRunning.
func TestCapacity_SeparatedStillHonorsRunningPerStateLimit(t *testing.T) {
	s := stateWith(StatusRunning, StatusPROpen)
	c := s.Capacity(CapacityInput{
		MaxParallel:          10,
		MaxLiveWorkers:       5,
		MaxConcurrentByState: map[string]int{"running": 2},
	})
	if c.AvailableSlots != 1 {
		t.Errorf("AvailableSlots = %d, want 1 (running per-state limit 2 - 1 live)", c.AvailableSlots)
	}
}

func TestCapacity_NilStateSafe(t *testing.T) {
	var s *State
	c := s.Capacity(CapacityInput{MaxParallel: 3})
	if c.AvailableSlots != 3 {
		t.Errorf("AvailableSlots = %d, want 3 on nil state", c.AvailableSlots)
	}
}

func TestClassifyActivity(t *testing.T) {
	tests := []struct {
		name string
		in   ActivityInput
		want ProjectActivity
	}{
		{
			name: "live workers implementing",
			in:   ActivityInput{Capacity: Capacity{LiveWorkers: 2, PRGates: 1}, EligibleIssues: 3},
			want: ActivityImplementing,
		},
		{
			name: "paused wins over idle",
			in:   ActivityInput{Capacity: Capacity{AvailableSlots: 2}, EligibleIssues: 3, Paused: true},
			want: ActivityPaused,
		},
		{
			name: "model limits block dispatch",
			in:   ActivityInput{Capacity: Capacity{AvailableSlots: 2}, EligibleIssues: 3, BackendsBlocked: true},
			want: ActivityBlockedByModelLimits,
		},
		{
			name: "pending approvals block dispatch",
			in:   ActivityInput{Capacity: Capacity{AvailableSlots: 2}, EligibleIssues: 3, PendingApprovals: 1},
			want: ActivityBlockedByApprovals,
		},
		{
			name: "gate-bound with eligible work is the intervention loop",
			in:   ActivityInput{Capacity: Capacity{PRGates: 3, AvailableSlots: 0}, EligibleIssues: 5},
			want: ActivityBlockedByGates,
		},
		{
			name: "gate-bound with no eligible work is just waiting",
			in:   ActivityInput{Capacity: Capacity{PRGates: 3, AvailableSlots: 0}, EligibleIssues: 0},
			want: ActivityWaitingOnGates,
		},
		{
			name: "empty queue",
			in:   ActivityInput{Capacity: Capacity{AvailableSlots: 4}, EligibleIssues: 0},
			want: ActivityQueueEmpty,
		},
		{
			name: "idle with free capacity and eligible work",
			in:   ActivityInput{Capacity: Capacity{AvailableSlots: 4}, EligibleIssues: 2},
			want: ActivityIdle,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, reason := ClassifyActivity(tc.in)
			if got != tc.want {
				t.Errorf("ClassifyActivity() = %q, want %q", got, tc.want)
			}
			if reason == "" {
				t.Errorf("ClassifyActivity() reason is empty for %q", tc.want)
			}
		})
	}
}

// #814 requirement 3/6: a pr_open PR-gate session must not produce stale-PID
// attention — the worker process is intentionally gone after the PR opens.
func TestPROpenSessionProducesNoStalePIDAttention(t *testing.T) {
	// PID cleared at the running→pr_open transition; PR number recorded.
	sess := &Session{Status: StatusPROpen, PRNumber: 42, PID: 0}

	// Even if a caller passes a dead-liveness hint, pr_open never flags on PID.
	dead := false
	att := SessionAttentionFor(sess, &dead)
	if att.NeedsAttention {
		t.Errorf("pr_open session NeedsAttention = true, want false (reason: %q)", att.Reason)
	}

	// And stale reconciliation never reclassifies a pr_open session, even long
	// idle with a missing worktree.
	now := time.Now().UTC()
	sess.StartedAt = now.Add(-72 * time.Hour)
	sess.Worktree = "/tmp/does-not-exist-814"
	policy := StaleSessionPolicy{
		Enabled:                true,
		IdleAfter:              time.Hour,
		RequireWorktreeMissing: true,
	}
	if _, stale := SessionStale(sess, now, policy, func(string) bool { return false }); stale {
		t.Errorf("SessionStale() = true for pr_open, want false")
	}
}
