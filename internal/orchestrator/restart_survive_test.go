package orchestrator

import (
	"testing"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/github"
	"github.com/befeast/maestro/internal/state"
)

// #877: a self-deploy restart of maestro.service must not kill in-flight tmux
// workers. maestro launches worker tmux servers in a transient `systemd-run
// --user --scope` cgroup that is a sibling of the unit, so a worker (and its
// dirty worktree) spans the restart and the next daemon re-attaches to the
// still-live pane. The recorded pane PID is normally still alive across the
// restart; but if it went stale while the tmux session survived, the reconcile
// must adopt the surviving pane instead of a false running->dead + duplicate
// dispatch.

// TestReconcileRunningSessions_AdoptsSurvivingTmuxAcrossRestart drives the whole
// reconcile: a running session whose recorded PID reads dead but whose tmux
// session survives with a live pane must stay running (PID refreshed), not be
// marked dead. A false dead here strands the dirty worktree and re-dispatches a
// duplicate worker.
func TestReconcileRunningSessions_AdoptsSurvivingTmuxAcrossRestart(t *testing.T) {
	const stalePID, livePanePID = 4242, 9999

	o := &Orchestrator{
		cfg:        &config.Config{Repo: "owner/repo"},
		pidAliveFn: func(pid int) bool { return pid == livePanePID },
		tmuxSessionExistsFn: func(name string) bool {
			return name == "maestro-slot-1" // the worker's tmux server survived the restart
		},
		tmuxPanePIDFn: func(name string) int {
			if name == "maestro-slot-1" {
				return livePanePID
			}
			return 0
		},
		listOpenPRsFn: func() ([]github.PR, error) { return []github.PR{}, nil },
	}

	s := state.NewState()
	s.Sessions["slot-1"] = &state.Session{
		IssueNumber: 310,
		IssueTitle:  "in-flight dogfood worker",
		Status:      state.StatusRunning,
		PID:         stalePID,
		TmuxSession: "maestro-slot-1",
		Branch:      "feat/slot-1-310-x",
		Worktree:    "/wt/slot-1", // dirty worktree must survive
	}

	o.reconcileRunningSessions(s)

	sess := s.Sessions["slot-1"]
	if sess.Status != state.StatusRunning {
		t.Fatalf("status = %q, want running (surviving worker must not be marked dead) (#877)", sess.Status)
	}
	if sess.PID != livePanePID {
		t.Fatalf("PID = %d, want %d (adopt the surviving pane's live pid)", sess.PID, livePanePID)
	}
	if sess.Worktree != "/wt/slot-1" {
		t.Fatalf("Worktree = %q, want preserved", sess.Worktree)
	}
	if sess.FinishedAt != nil {
		t.Fatalf("FinishedAt = %v, want nil (worker still running)", sess.FinishedAt)
	}
	if sess.NextRetryAt != nil {
		t.Fatalf("NextRetryAt = %v, want nil (no retry — the worker never died)", sess.NextRetryAt)
	}
}

func TestAdoptSurvivingTmux(t *testing.T) {
	newOrch := func(sessionExists bool, panePID int, live func(int) bool) *Orchestrator {
		return &Orchestrator{
			cfg:                 &config.Config{Repo: "owner/repo"},
			pidAliveFn:          live,
			tmuxSessionExistsFn: func(string) bool { return sessionExists },
			tmuxPanePIDFn:       func(string) int { return panePID },
		}
	}

	t.Run("adopts a live surviving session and refreshes the pid", func(t *testing.T) {
		o := newOrch(true, 9999, func(pid int) bool { return pid == 9999 })
		sess := &state.Session{Status: state.StatusRunning, PID: 4242, TmuxSession: "maestro-slot-1"}
		if !o.adoptSurvivingTmux("slot-1", sess) {
			t.Fatal("expected adoption of a surviving tmux session")
		}
		if sess.PID != 9999 {
			t.Fatalf("PID = %d, want 9999 (refreshed to the live pane)", sess.PID)
		}
	})

	t.Run("does not adopt when the tmux session is gone", func(t *testing.T) {
		o := newOrch(false, 0, func(int) bool { return false })
		sess := &state.Session{Status: state.StatusRunning, PID: 4242, TmuxSession: "maestro-slot-1"}
		if o.adoptSurvivingTmux("slot-1", sess) {
			t.Fatal("must not adopt when the session is truly gone — that is a real death")
		}
		if sess.PID != 4242 {
			t.Fatalf("PID = %d, want unchanged 4242", sess.PID)
		}
	})

	t.Run("does not adopt a session whose pane pid is dead", func(t *testing.T) {
		o := newOrch(true, 9999, func(int) bool { return false }) // pane pid not alive
		sess := &state.Session{Status: state.StatusRunning, PID: 4242, TmuxSession: "maestro-slot-1"}
		if o.adoptSurvivingTmux("slot-1", sess) {
			t.Fatal("must not adopt when the surviving session's pane is dead")
		}
	})
}
