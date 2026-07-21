package orchestrator

import (
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/github"
	"github.com/befeast/maestro/internal/notify"
	"github.com/befeast/maestro/internal/state"
)

// TestStaleCleanupRejectedAfterApprovedInPlaceRepair is the deterministic
// #963 regression and links the unattended recovery contract in #940:
//
//   - cycle A selects an old terminal session with an open PR identity;
//   - cycle B persists approval and respawns an in-place repair in the exact
//     canonical slot while A is paused;
//   - A resumes and loses the exact session-generation lease before removal;
//   - the live PID, valid Git worktree, and uncommitted repair file survive;
//   - canonical state contains one coherent slot/PID/generation.
func TestStaleCleanupRejectedAfterApprovedInPlaceRepair(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	worktreeBase := filepath.Join(root, "worktrees")
	stateDir := filepath.Join(root, "state")
	slot := "ok-player-296"
	branch := "feat/ok-player-repair"
	worktree := filepath.Join(worktreeBase, slot)

	runGitTest(t, root, "init", repo)
	runGitTest(t, repo, "config", "user.email", "maestro@example.invalid")
	runGitTest(t, repo, "config", "user.name", "Maestro Test")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitTest(t, repo, "add", "README.md")
	runGitTest(t, repo, "commit", "-m", "base")
	runGitTest(t, repo, "branch", branch)
	if err := os.MkdirAll(worktreeBase, 0o755); err != nil {
		t.Fatal(err)
	}
	runGitTest(t, repo, "worktree", "add", worktree, branch)

	finished := time.Now().UTC().Add(-2 * time.Hour)
	initial := state.NewState()
	initial.Sessions[slot] = &state.Session{
		IssueNumber:      331,
		IssueTitle:       "approved canonical repair",
		PRNumber:         398,
		PID:              500100,
		Worktree:         worktree,
		Branch:           branch,
		StartedAt:        finished.Add(-time.Hour),
		FinishedAt:       &finished,
		Status:           state.StatusRetryExhausted,
		WorkerGeneration: 7,
	}
	if err := state.Save(stateDir, initial); err != nil {
		t.Fatalf("save initial state: %v", err)
	}
	cycleA, err := state.Load(stateDir)
	if err != nil {
		t.Fatal(err)
	}

	selected := make(chan struct{})
	resumeA := make(chan struct{})
	var removeCalls atomic.Int32
	var livePID atomic.Int64
	var tmuxLive atomic.Bool
	cfg := &config.Config{
		Repo:         "owner/ok-player",
		LocalPath:    repo,
		WorktreeBase: worktreeBase,
		StateDir:     stateDir,
	}
	cycleAOrchestrator := &Orchestrator{
		cfg:      cfg,
		notifier: &notify.Notifier{},
		listOpenPRsFn: func() ([]github.PR, error) {
			return nil, nil
		},
		isIssueClosedFn: func(int) (bool, error) { return false, nil },
		isPRMergedFn:    func(int) (bool, error) { return false, nil },
		pidAliveFn: func(pid int) bool {
			return int64(pid) == livePID.Load()
		},
		tmuxSessionExistsFn: func(string) bool { return tmuxLive.Load() },
		beforeWorktreeCleanupFn: func(string) {
			close(selected)
			<-resumeA
		},
		removeWorktreeFn: func(string, string) error {
			removeCalls.Add(1)
			return nil
		},
	}

	doneA := make(chan struct{})
	go func() {
		defer close(doneA)
		cycleAOrchestrator.checkSessions(cycleA)
	}()
	<-selected

	cycleB, err := state.Load(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	cycleB.Approvals = append(cycleB.Approvals, state.Approval{
		ID:     "approval-repair-331",
		Action: "spawn_repair_worker",
		Target: &state.SupervisorTarget{Issue: 331, PR: 398, Session: slot},
		Status: state.ApprovalStatusAwaitingDispatch,
	})
	if err := state.Save(stateDir, cycleB); err != nil {
		t.Fatalf("persist approved repair: %v", err)
	}

	repairPID := 758258
	cycleBOrchestrator := &Orchestrator{
		cfg: cfg,
		respawnInPlaceFn: func(_ *config.Config, gotSlot string, sess *state.Session, _ string, _ github.Issue, _ string, _ string) error {
			if gotSlot != slot {
				t.Fatalf("respawn slot = %q, want %q", gotSlot, slot)
			}
			dirty := filepath.Join(worktree, "WORK_IN_PROGRESS.txt")
			if err := os.WriteFile(dirty, []byte("uncommitted repair work\n"), 0o644); err != nil {
				return err
			}
			sess.WorkerGeneration++
			sess.PID = repairPID
			sess.TmuxSession = "maestro-" + slot
			sess.StartedAt = time.Now().UTC()
			sess.FinishedAt = nil
			sess.Status = state.StatusRunning
			livePID.Store(int64(repairPID))
			tmuxLive.Store(true)
			return state.Save(stateDir, cycleB)
		},
	}
	if err := cycleBOrchestrator.respawnInPlaceWithConfig(cfg, slot, cycleB.Sessions[slot], github.Issue{Number: 331}, "repair", "codex"); err != nil {
		t.Fatalf("cycle B respawn: %v", err)
	}
	cycleB.ResolveDispatchedSpawnRepairApproval("approval-repair-331", time.Now().UTC(), "repair worker dispatched")
	if err := state.Save(stateDir, cycleB); err != nil {
		t.Fatalf("persist resolved repair approval: %v", err)
	}

	close(resumeA)
	<-doneA

	if got := removeCalls.Load(); got != 0 {
		t.Fatalf("cleanup reached filesystem mutation %d time(s), want 0", got)
	}
	if livePID.Load() != int64(repairPID) {
		t.Fatalf("repair PID was not preserved: got %d want %d", livePID.Load(), repairPID)
	}
	if out, err := exec.Command("git", "-C", worktree, "rev-parse", "--is-inside-work-tree").CombinedOutput(); err != nil || string(out) != "true\n" {
		t.Fatalf("canonical worktree invalid after rejected cleanup: err=%v out=%q", err, out)
	}
	dirtyPath := filepath.Join(worktree, "WORK_IN_PROGRESS.txt")
	if data, err := os.ReadFile(dirtyPath); err != nil || string(data) != "uncommitted repair work\n" {
		t.Fatalf("dirty repair work was not preserved: err=%v data=%q", err, data)
	}

	canonical, err := state.Load(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(canonical.Sessions) != 1 {
		t.Fatalf("canonical sessions = %d, want 1", len(canonical.Sessions))
	}
	got := canonical.Sessions[slot]
	if got == nil || got.Status != state.StatusRunning || got.PID != repairPID || got.WorkerGeneration != 8 || got.Worktree != worktree {
		t.Fatalf("canonical slot incoherent: %+v", got)
	}
}

func runGitTest(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}
