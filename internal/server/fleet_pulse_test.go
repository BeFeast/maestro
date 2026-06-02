package server

import (
	"testing"
	"time"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/state"
)

// TestBuildFleetSupervisorPulseExposesLastRunOnceAndCadence verifies that the
// header verdict card (issue #531) sees the LastRunOnceAt timestamp, the
// configured poll interval, the policy mode, and the most recent
// recommended_action verbs from the decision log. Without these, the SPA
// falls back to passive «Action required» text instead of a positive
// liveness signal + countdown + decision sparkline.
func TestBuildFleetSupervisorPulseExposesLastRunOnceAndCadence(t *testing.T) {
	cfg := &config.Config{
		PollIntervalSeconds: 120,
	}
	cfg.Supervisor.Mode = "cautious"

	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	st := state.NewState()
	st.LastRunOnceAt = now.Add(-90 * time.Second)
	st.SupervisorDecisions = []state.SupervisorDecision{
		{RecommendedAction: "monitor_open_pr", CreatedAt: now.Add(-300 * time.Second)},
		{RecommendedAction: "monitor_open_pr", CreatedAt: now.Add(-180 * time.Second)},
		{RecommendedAction: "spawn_worker", CreatedAt: now.Add(-90 * time.Second)},
	}

	pulse := buildFleetSupervisorPulse(cfg, st, now)

	if pulse.PollIntervalSeconds != 120 {
		t.Errorf("PollIntervalSeconds = %d, want 120", pulse.PollIntervalSeconds)
	}
	if pulse.Mode != "cautious" {
		t.Errorf("Mode = %q, want %q", pulse.Mode, "cautious")
	}
	if pulse.LastRunOnceAt == "" {
		t.Errorf("LastRunOnceAt empty, want non-empty timestamp")
	}
	if pulse.LastRunOnceAgeSeconds < 89 || pulse.LastRunOnceAgeSeconds > 91 {
		t.Errorf("LastRunOnceAgeSeconds = %d, want ~90", pulse.LastRunOnceAgeSeconds)
	}
	wantVerbs := []string{"monitor_open_pr", "monitor_open_pr", "spawn_worker"}
	if len(pulse.RecentActions) != len(wantVerbs) {
		t.Fatalf("RecentActions len = %d (%v), want %d", len(pulse.RecentActions), pulse.RecentActions, len(wantVerbs))
	}
	for i, want := range wantVerbs {
		if pulse.RecentActions[i] != want {
			t.Errorf("RecentActions[%d] = %q, want %q", i, pulse.RecentActions[i], want)
		}
	}
}

// TestBuildFleetSupervisorPulseTrimsToLastTenVerbs pins that the sparkline
// never grows past ten entries even when the decision log is at the state
// limit. The SPA header has fixed width, so leaking the full 20-decision
// state slice would either overflow or hide the freshest verbs.
func TestBuildFleetSupervisorPulseTrimsToLastTenVerbs(t *testing.T) {
	cfg := &config.Config{}
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	st := state.NewState()
	st.LastRunOnceAt = now.Add(-10 * time.Second)
	for i := 0; i < state.DefaultSupervisorDecisionLimit; i++ {
		st.SupervisorDecisions = append(st.SupervisorDecisions, state.SupervisorDecision{
			RecommendedAction: "monitor_open_pr",
			CreatedAt:         now.Add(time.Duration(-i) * time.Minute),
		})
	}
	// Stamp the freshest verb so we can confirm it's preserved at the tail.
	st.SupervisorDecisions[len(st.SupervisorDecisions)-1].RecommendedAction = "spawn_worker"

	pulse := buildFleetSupervisorPulse(cfg, st, now)

	if got, want := len(pulse.RecentActions), fleetSupervisorPulseRecentLimit; got != want {
		t.Fatalf("RecentActions len = %d, want %d", got, want)
	}
	if tail := pulse.RecentActions[len(pulse.RecentActions)-1]; tail != "spawn_worker" {
		t.Errorf("tail verb = %q, want %q (freshest decision must survive truncation)", tail, "spawn_worker")
	}
}

// TestBuildFleetSupervisorPulseHandlesEmptyStateGracefully pins that a
// freshly-loaded project (no run_once stamped yet, no decisions recorded)
// produces a zero pulse rather than panicking. The SPA renders that as
// "no cycles yet — supervisor warming up" instead of a wedged hero.
func TestBuildFleetSupervisorPulseHandlesEmptyStateGracefully(t *testing.T) {
	cfg := &config.Config{PollIntervalSeconds: 60}
	cfg.Supervisor.Mode = "read_only"

	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	pulse := buildFleetSupervisorPulse(cfg, state.NewState(), now)

	if pulse.LastRunOnceAt != "" {
		t.Errorf("LastRunOnceAt = %q, want empty when no run_once stamped", pulse.LastRunOnceAt)
	}
	if pulse.LastRunOnceAgeSeconds != 0 {
		t.Errorf("LastRunOnceAgeSeconds = %d, want 0 when no run_once stamped", pulse.LastRunOnceAgeSeconds)
	}
	if pulse.PollIntervalSeconds != 60 {
		t.Errorf("PollIntervalSeconds = %d, want 60", pulse.PollIntervalSeconds)
	}
	if pulse.Mode != "read_only" {
		t.Errorf("Mode = %q, want %q", pulse.Mode, "read_only")
	}
	if len(pulse.RecentActions) != 0 {
		t.Errorf("RecentActions = %v, want empty slice", pulse.RecentActions)
	}
}

// TestRecentSupervisorActionsSkipsEmptyVerbs guards the sparkline from
// rendering "-" placeholders for legacy decisions that were stored without
// a recommended_action set.
func TestRecentSupervisorActionsSkipsEmptyVerbs(t *testing.T) {
	decisions := []state.SupervisorDecision{
		{RecommendedAction: "monitor_open_pr"},
		{RecommendedAction: ""},
		{RecommendedAction: "  "},
		{RecommendedAction: "spawn_worker"},
	}
	got := recentSupervisorActions(decisions, 10)
	want := []string{"monitor_open_pr", "spawn_worker"}
	if len(got) != len(want) {
		t.Fatalf("got %d verbs (%v), want %d", len(got), got, len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestFleetNextActionCTAForApprovalNamesTheEffect pins that the header CTA
// reads "Approve PR #N" / "Close issue #N" — the verb-shaped label the
// operator confirms — rather than a generic "Action required" (#531 gap 14).
func TestFleetNextActionCTAForApprovalNamesTheEffect(t *testing.T) {
	cases := []struct {
		name     string
		approval fleetApprovalState
		want     string
	}{
		{
			name:     "merge_pr with PR number",
			approval: fleetApprovalState{Action: "merge_pr", PRNumber: 123},
			want:     "Approve PR #123",
		},
		{
			name:     "merge_pr without PR number falls back",
			approval: fleetApprovalState{Action: "merge_pr"},
			want:     "Approve merge",
		},
		{
			name:     "close_issue with issue number",
			approval: fleetApprovalState{Action: "close_issue", IssueNumber: 42},
			want:     "Close issue #42",
		},
		{
			name:     "spawn_worker with issue number",
			approval: fleetApprovalState{Action: "spawn_worker", IssueNumber: 7},
			want:     "Start worker on #7",
		},
		{
			name:     "delete_worktree",
			approval: fleetApprovalState{Action: "delete_worktree"},
			want:     "Delete worktree",
		},
		{
			name:     "unknown action with PR number",
			approval: fleetApprovalState{Action: "something_new", PRNumber: 9},
			want:     "Review PR #9",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := fleetNextActionCTAForApproval(&tc.approval)
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// TestFleetNextActionCTAForProjectNamesTheStuckPath pins that project-level
// kinds (dispatch_failure, stale_worker, …) render a verb-shaped CTA so the
// header reads "Resolve stuck dispatch" / "Resolve stuck session" instead of
// the passive «Action required».
func TestFleetNextActionCTAForProjectNamesTheStuckPath(t *testing.T) {
	cases := []struct {
		name string
		kind string
		op   fleetOperatorState
		want string
	}{
		{name: "dispatch_failure", kind: "dispatch_failure", want: "Resolve stuck dispatch"},
		{name: "stale_worker with PR", kind: "stale_worker", op: fleetOperatorState{PRNumber: 9, Session: "wk-3"}, want: "Open worker log for PR #9"},
		{name: "stale_worker with session", kind: "stale_worker", op: fleetOperatorState{Session: "wk-3"}, want: "Open worker log for wk-3"},
		{name: "stale_worker without session", kind: "stale_worker", want: "Open worker log"},
		{name: "outcome_drift", kind: "outcome_drift", want: "Reconcile outcome drift"},
		{name: "outcome_missing", kind: "outcome_missing", want: "Configure outcome"},
		{name: "no_eligible_issues", kind: "no_eligible_issues", want: "Queue more work"},
		{name: "queue_blocked", kind: "queue_blocked", want: "Unblock queue"},
		{name: "stale", kind: "stale", want: "Refresh stale snapshot"},
		{name: "attention with PR", kind: "attention", op: fleetOperatorState{PRNumber: 5}, want: "Review PR #5"},
		{name: "attention with PR + failing checks", kind: "attention", op: fleetOperatorState{PRNumber: 5, Summary: "CI checks failed on PR"}, want: "Fix failing checks on PR #5"},
		{name: "attention with PR + conflict", kind: "attention", op: fleetOperatorState{PRNumber: 5, Summary: "rebase conflict on branch"}, want: "Resolve conflict on PR #5"},
		{name: "auto_merging is calm", kind: "auto_merging", op: fleetOperatorState{PRNumber: 7}, want: ""},
		{name: "unknown kind", kind: "unknown", want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := fleetNextActionCTAForProject(tc.kind, tc.op)
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}
