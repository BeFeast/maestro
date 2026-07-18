package orchestrator

import (
	"testing"
	"time"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/github"
	"github.com/befeast/maestro/internal/state"
)

func TestReconcileRunningSessionsAdoptsLiveRepairHiddenBehindPROpen(t *testing.T) {
	now := time.Date(2026, 7, 18, 2, 40, 0, 0, time.UTC)
	worktree := t.TempDir()
	s := state.NewState()
	s.Sessions["ok-player-302"] = &state.Session{
		IssueNumber: 406,
		Worktree:    worktree,
		Branch:      "feat/ok-player-302-406-repair",
		Status:      state.StatusPROpen,
		PRNumber:    388,
		Backend:     "sol",
		FinishedAt:  ptrTime(now.Add(-time.Hour)),
		Attribution: []state.BackendAttribution{{Backend: "sol", StartedAt: now.Add(-2 * time.Hour)}},
	}

	o := &Orchestrator{
		cfg: &config.Config{Model: config.ModelConfig{Backends: map[string]config.BackendDef{
			"sol": {Model: "gpt-5.6-sol"},
		}}},
		listOpenPRsFn:       func() ([]github.PR, error) { return nil, nil },
		tmuxSessionExistsFn: func(name string) bool { return name == "maestro-ok-player-302" },
		tmuxPaneIdentityFn: func(name string) (int, string, error) {
			return 4242, worktree, nil
		},
		pidAliveFn: func(pid int) bool { return pid == 4242 },
	}

	if !o.reconcileRunningSessions(s) {
		t.Fatal("expected live hidden repair to be reconciled")
	}
	sess := s.Sessions["ok-player-302"]
	if sess.Status != state.StatusRunning || sess.PID != 4242 || sess.TmuxSession != "maestro-ok-player-302" {
		t.Fatalf("adopted runtime = status=%s pid=%d tmux=%q", sess.Status, sess.PID, sess.TmuxSession)
	}
	if sess.FinishedAt != nil || sess.WorkerEndedAt != nil {
		t.Fatalf("adopted live runtime retained terminal timestamps: finished=%v ended=%v", sess.FinishedAt, sess.WorkerEndedAt)
	}
	if len(sess.Attribution) != 2 || sess.Attribution[1].Reason != "runtime_adoption" {
		t.Fatalf("attribution = %#v, want appended runtime_adoption segment", sess.Attribution)
	}
}

func TestReconcileRunningSessionsRefusesForeignLiveTmux(t *testing.T) {
	worktree := t.TempDir()
	s := state.NewState()
	s.Sessions["slot-1"] = &state.Session{
		IssueNumber: 1,
		Worktree:    worktree,
		Branch:      "feat/slot-1",
		Status:      state.StatusPROpen,
	}
	o := &Orchestrator{
		cfg:                 &config.Config{},
		listOpenPRsFn:       func() ([]github.PR, error) { return nil, nil },
		tmuxSessionExistsFn: func(string) bool { return true },
		tmuxPaneIdentityFn:  func(string) (int, string, error) { return 4242, t.TempDir(), nil },
		pidAliveFn:          func(int) bool { return true },
	}

	if o.reconcileRunningSessions(s) {
		t.Fatal("foreign tmux must not be adopted")
	}
	if got := s.Sessions["slot-1"].Status; got != state.StatusPROpen {
		t.Fatalf("status = %s, want pr_open", got)
	}
}

func TestEnsureAttributionTrailerDefersWhileExactTmuxIsLive(t *testing.T) {
	worktree := t.TempDir()
	called := false
	o := &Orchestrator{
		cfg:                 &config.Config{},
		tmuxSessionExistsFn: func(name string) bool { return name == "maestro-slot-1" },
		tmuxPaneIdentityFn:  func(string) (int, string, error) { return 5151, worktree, nil },
		amendHeadFn: func(string, string, []state.BackendAttribution, time.Time) error {
			called = true
			return nil
		},
	}
	sess := &state.Session{
		Worktree: worktree,
		Branch:   "feat/slot-1",
		Status:   state.StatusPROpen, // persisted status is deliberately stale
		Attribution: []state.BackendAttribution{{
			Backend: "sol", StartedAt: time.Now().UTC(),
		}},
	}

	if !o.ensureAttributionTrailerOnBranch("slot-1", sess) {
		t.Fatal("live worker ownership must defer attribution amend")
	}
	if called {
		t.Fatal("attribution amend ran underneath a live worker")
	}
}

func TestSaveStatePreservingLiveRuntimeRecoversSameSessionConflict(t *testing.T) {
	stateDir := t.TempDir()
	worktreeBase := t.TempDir()
	worktree := worktreeBase + "/slot-1"
	now := time.Date(2026, 7, 18, 2, 45, 0, 0, time.UTC)
	initial := state.NewState()
	initial.Sessions["slot-1"] = &state.Session{
		IssueNumber: 42,
		Worktree:    worktree,
		Branch:      "feat/slot-1-42",
		Status:      state.StatusPROpen,
		PRNumber:    99,
		Backend:     "sol",
	}
	if err := state.Save(stateDir, initial); err != nil {
		t.Fatal(err)
	}

	runSnapshot, err := state.Load(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	concurrent, err := state.Load(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	run := runSnapshot.Sessions["slot-1"]
	run.Status = state.StatusRunning
	run.PID = 6262
	run.TmuxSession = "maestro-slot-1"
	run.StartedAt = now
	concurrent.Sessions["slot-1"].LastNotifiedStatus = "concurrent-supervisor-observation"
	concurrent.Sessions["slot-1"].MaintenanceRetryCount = 7
	concurrent.LastRunOnceAt = now.Add(time.Minute)
	if err := state.Save(stateDir, concurrent); err != nil {
		t.Fatal(err)
	}

	o := &Orchestrator{
		cfg:                 &config.Config{StateDir: stateDir, WorktreeBase: worktreeBase},
		tmuxSessionExistsFn: func(name string) bool { return name == "maestro-slot-1" },
		tmuxPaneIdentityFn:  func(string) (int, string, error) { return 6262, worktree, nil },
		pidAliveFn:          func(pid int) bool { return pid == 6262 },
	}
	if err := o.saveStatePreservingLiveRuntime(runSnapshot); err != nil {
		t.Fatalf("saveStatePreservingLiveRuntime: %v", err)
	}

	loaded, err := state.Load(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if got := loaded.Sessions["slot-1"]; got.Status != state.StatusRunning || got.PID != 6262 {
		t.Fatalf("persisted runtime = %#v", got)
	}
	if got := loaded.Sessions["slot-1"].MaintenanceRetryCount; got != 7 {
		t.Fatalf("concurrent non-runtime session field lost: maintenance_retry_count=%d", got)
	}
	if !loaded.LastRunOnceAt.Equal(now.Add(time.Minute)) {
		t.Fatalf("concurrent heartbeat lost: %s", loaded.LastRunOnceAt)
	}
}

func ptrTime(v time.Time) *time.Time { return &v }
