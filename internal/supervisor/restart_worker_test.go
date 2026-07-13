package supervisor

import (
	"testing"

	"github.com/befeast/maestro/internal/state"
)

// #877 reproduction: a self-deploy `systemctl restart maestro.service` used to
// kill in-flight tmux workers before the in-process drain completed, because the
// worker tmux server shared the unit's control group. The fix launches worker
// tmux servers in a transient scope outside that cgroup, so a worker spans the
// restart. This test models that survival at the supervisor layer: two in-flight
// workers whose recorded pane PIDs read dead after the restart but whose tmux
// sessions are still live must NOT raise a "dead_running_pid" blocked finding —
// which would nudge an operator to reconcile a genuinely-live worker to dead.
func TestSupervisor_SurvivingTmuxWorkersNotFlaggedDeadAfterRestart(t *testing.T) {
	cfg := testConfig(t)
	st := state.NewState()
	for _, slot := range []string{"sup-310", "sup-311"} {
		st.Sessions[slot] = &state.Session{
			IssueNumber: 310,
			IssueTitle:  "in-flight dogfood worker",
			Status:      state.StatusRunning,
			PID:         424242, // stale recorded pid after the restart
			TmuxSession: "maestro-" + slot,
			Worktree:    "/wt/" + slot, // dirty worktree must survive
			StartedAt:   testEngineNow(),
		}
	}

	eng := testEngine(cfg, &fakeReader{})
	eng.pidAlive = func(pid int) bool { return false }     // recorded pids are stale
	eng.tmuxAlive = func(name string) bool { return true } // tmux servers survived the restart

	decision, err := eng.Decide(st)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	for _, stuck := range decision.StuckStates {
		if stuck.Code == "dead_running_pid" {
			t.Fatalf("surviving worker flagged dead_running_pid across a restart (%s) — false running->dead (#877)", stuck.Summary)
		}
	}
	// The dirty worktrees and running status must be intact for the resume.
	if got := st.RunningSessionCount(); got != 2 {
		t.Fatalf("RunningSessionCount = %d, want 2 (surviving workers stay in-flight so drain waits)", got)
	}
}

// The survival guard must not blanket-suppress the finding: a worker whose PID
// AND tmux session are both gone is genuinely dead and must still be flagged.
func TestSupervisor_TrulyDeadWorkerStillFlagged(t *testing.T) {
	cfg := testConfig(t)
	st := state.NewState()
	st.Sessions["sup-312"] = &state.Session{
		IssueNumber: 312,
		IssueTitle:  "genuinely dead worker",
		Status:      state.StatusRunning,
		PID:         424242,
		TmuxSession: "maestro-sup-312",
		StartedAt:   testEngineNow(),
	}

	eng := testEngine(cfg, &fakeReader{})
	eng.pidAlive = func(pid int) bool { return false }
	eng.tmuxAlive = func(name string) bool { return false } // tmux session also gone

	decision, err := eng.Decide(st)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if _, found := findStuckState(decision, "dead_running_pid"); !found {
		t.Fatal("a worker with both PID and tmux session gone must still be flagged dead_running_pid")
	}
}

func findStuckState(decision state.SupervisorDecision, code string) (state.SupervisorStuckState, bool) {
	for _, stuck := range decision.StuckStates {
		if stuck.Code == code {
			return stuck, true
		}
	}
	return state.SupervisorStuckState{}, false
}

// #874: after a restart tears down the worktree, the session's stale
// worktree/PR pointers must be cleared so respawnDueRetries fresh-respawns
// instead of choosing RespawnInPlace against a directory that no longer exists.
func TestClearWorktreeAfterRestart(t *testing.T) {
	sess := &state.Session{
		IssueNumber: 42,
		Status:      state.StatusDead,
		Worktree:    "/wt/slot-1",
		PRNumber:    867,
	}
	clearWorktreeAfterRestart(sess)
	if sess.Worktree != "" {
		t.Fatalf("Worktree = %q, want cleared", sess.Worktree)
	}
	if sess.PRNumber != 0 {
		t.Fatalf("PRNumber = %d, want cleared", sess.PRNumber)
	}
	// Nil-safe.
	clearWorktreeAfterRestart(nil)
}
