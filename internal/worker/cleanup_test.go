package worker

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/state"
)

func neverAlive(int) bool    { return false }
func alwaysAlive(int) bool   { return true }
func neverTmux(string) bool  { return false }
func alwaysTmux(string) bool { return true }

// TestValidateCleanupLease_AllowsUnchangedTerminalSession is the baseline: a
// terminal session with a dead PID that has not been re-leased is safe to clean.
func TestValidateCleanupLease_AllowsUnchangedTerminalSession(t *testing.T) {
	started := time.Now().Add(-2 * time.Hour)
	sess := &state.Session{
		IssueNumber:      331,
		PRNumber:         398,
		PID:              111,
		Worktree:         "/wt/ok-player-296",
		Branch:           "feat/x",
		StartedAt:        started,
		WorkerGeneration: 4,
		Status:           state.StatusRetryExhausted,
	}
	lease := CaptureCleanupLease("ok-player-296", sess)
	if err := ValidateCleanupLease(lease, sess, CleanupProbes{PIDAlive: neverAlive, TmuxAlive: neverTmux}, CleanupPolicy{RequireTerminal: true}); err != nil {
		t.Fatalf("unchanged terminal session with dead PID should be cleanable, got: %v", err)
	}
}

func TestValidateCleanupLease_AbortsOnEachReleaseSignal(t *testing.T) {
	started := time.Now().Add(-2 * time.Hour)
	base := func() *state.Session {
		return &state.Session{
			IssueNumber:      331,
			PRNumber:         398,
			PID:              111,
			Worktree:         "/wt/ok-player-296",
			Branch:           "feat/x",
			StartedAt:        started,
			WorkerGeneration: 4,
			Status:           state.StatusRetryExhausted,
		}
	}
	lease := CaptureCleanupLease("ok-player-296", base())

	cases := []struct {
		name    string
		current *state.Session
		probes  CleanupProbes
	}{
		{"slot vanished", nil, CleanupProbes{PIDAlive: neverAlive, TmuxAlive: neverTmux}},
		{"issue recycled", func() *state.Session { s := base(); s.IssueNumber = 999; return s }(), CleanupProbes{PIDAlive: neverAlive, TmuxAlive: neverTmux}},
		{"pr changed", func() *state.Session { s := base(); s.PRNumber = 399; return s }(), CleanupProbes{PIDAlive: neverAlive, TmuxAlive: neverTmux}},
		{"generation changed", func() *state.Session { s := base(); s.WorkerGeneration++; return s }(), CleanupProbes{PIDAlive: neverAlive, TmuxAlive: neverTmux}},
		{"pid changed", func() *state.Session { s := base(); s.PID = 758258; return s }(), CleanupProbes{PIDAlive: neverAlive, TmuxAlive: neverTmux}},
		{"tmux changed", func() *state.Session { s := base(); s.TmuxSession = "maestro-new"; return s }(), CleanupProbes{PIDAlive: neverAlive, TmuxAlive: neverTmux}},
		{"started changed", func() *state.Session { s := base(); s.StartedAt = time.Now(); return s }(), CleanupProbes{PIDAlive: neverAlive, TmuxAlive: neverTmux}},
		{"no longer terminal", func() *state.Session { s := base(); s.Status = state.StatusRunning; return s }(), CleanupProbes{PIDAlive: neverAlive, TmuxAlive: neverTmux}},
		{"pid alive", base(), CleanupProbes{PIDAlive: alwaysAlive, TmuxAlive: neverTmux}},
		{"tmux alive", base(), CleanupProbes{PIDAlive: neverAlive, TmuxAlive: alwaysTmux}},
		{"worktree re-pointed", func() *state.Session { s := base(); s.Worktree = "/wt/other"; return s }(), CleanupProbes{PIDAlive: neverAlive, TmuxAlive: neverTmux}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateCleanupLease(lease, tc.current, tc.probes, CleanupPolicy{RequireTerminal: true})
			if err == nil {
				t.Fatalf("expected cleanup to abort for %q, got nil", tc.name)
			}
			if !errors.Is(err, ErrCleanupLeaseChanged) {
				t.Fatalf("expected ErrCleanupLeaseChanged for %q, got: %v", tc.name, err)
			}
		})
	}
}

func TestCleanupLeasedWorktree_RestoresAfterPartialRemoval(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	worktreeBase := filepath.Join(root, "worktrees")
	stateDir := filepath.Join(root, "state")
	slot := "sup-963"
	branch := "feat/sup-963"
	worktree := filepath.Join(worktreeBase, slot)

	runCleanupGit(t, root, "init", repo)
	runCleanupGit(t, repo, "config", "user.email", "maestro@example.invalid")
	runCleanupGit(t, repo, "config", "user.name", "Maestro Test")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runCleanupGit(t, repo, "add", "README.md")
	runCleanupGit(t, repo, "commit", "-m", "base")
	runCleanupGit(t, repo, "branch", branch)
	if err := os.MkdirAll(worktreeBase, 0o755); err != nil {
		t.Fatal(err)
	}
	runCleanupGit(t, repo, "worktree", "add", worktree, branch)

	finished := time.Now().UTC().Add(-2 * time.Hour)
	s := state.NewState()
	s.Sessions[slot] = &state.Session{
		IssueNumber:      963,
		PRNumber:         1001,
		Worktree:         worktree,
		Branch:           branch,
		StartedAt:        finished.Add(-time.Hour),
		FinishedAt:       &finished,
		Status:           state.StatusRetryExhausted,
		WorkerGeneration: 3,
	}
	s.Sessions["sup-other"] = &state.Session{
		IssueNumber:      964,
		Status:           state.StatusRunning,
		StartedAt:        time.Now().UTC(),
		WorkerGeneration: 9,
	}
	otherSession := s.Sessions["sup-other"]
	if err := state.Save(stateDir, s); err != nil {
		t.Fatal(err)
	}

	lease := CaptureCleanupLease(slot, s.Sessions[slot])
	cfg := &config.Config{LocalPath: repo, WorktreeBase: worktreeBase, StateDir: stateDir}
	err := CleanupLeasedWorktree(
		cfg,
		s,
		lease,
		CleanupProbes{PIDAlive: neverAlive, TmuxAlive: neverTmux},
		CleanupPolicy{RequireTerminal: true, RequireClean: true},
		CleanupHooks{Remove: func(localPath, worktreePath string) error {
			if err := RemoveWorktree(localPath, worktreePath); err != nil {
				return err
			}
			return errors.New("simulated failure after git metadata removal")
		}},
	)
	if !errors.Is(err, ErrCleanupConsistencyViolation) {
		t.Fatalf("cleanup err = %v, want ErrCleanupConsistencyViolation", err)
	}
	if out, gitErr := exec.Command("git", "-C", worktree, "rev-parse", "--is-inside-work-tree").CombinedOutput(); gitErr != nil || string(out) != "true\n" {
		t.Fatalf("worktree was not automatically restored: err=%v out=%q", gitErr, out)
	}
	canonical, loadErr := state.Load(stateDir)
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if got := canonical.Sessions[slot].Worktree; got != worktree {
		t.Fatalf("canonical worktree claim = %q, want %q", got, worktree)
	}
	if s.Sessions["sup-other"] != otherSession {
		t.Fatal("cleanup compensation replaced the caller's sessions map")
	}
}

func TestCleanupLeasedWorktree_PreservesConcurrentCanonicalUpdatesWithoutReplacingCycleMap(t *testing.T) {
	stateDir := t.TempDir()
	finished := time.Now().UTC().Add(-2 * time.Hour)
	initial := state.NewState()
	initial.Sessions["sup-cleanup"] = &state.Session{
		IssueNumber:      963,
		PRNumber:         1044,
		Worktree:         "/worktrees/sup-cleanup",
		Branch:           "feat/cleanup",
		StartedAt:        finished.Add(-time.Hour),
		FinishedAt:       &finished,
		Status:           state.StatusRetryExhausted,
		WorkerGeneration: 3,
	}
	initial.Sessions["sup-concurrent"] = &state.Session{
		IssueNumber:      1000,
		IssueTitle:       "original",
		Status:           state.StatusRunning,
		StartedAt:        time.Now().UTC(),
		WorkerGeneration: 1,
	}
	initial.Sessions["sup-later"] = &state.Session{
		IssueNumber:      1001,
		IssueTitle:       "original",
		Status:           state.StatusRunning,
		StartedAt:        time.Now().UTC(),
		WorkerGeneration: 1,
	}
	if err := state.Save(stateDir, initial); err != nil {
		t.Fatal(err)
	}

	cycle, err := state.Load(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	laterSession := cycle.Sessions["sup-later"]
	lease := CaptureCleanupLease("sup-cleanup", cycle.Sessions["sup-cleanup"])

	if err := state.Update(stateDir, func(latest *state.State) error {
		latest.Sessions["sup-concurrent"].PID = 4242
		latest.Sessions["sup-concurrent"].WorkerGeneration = 2
		latest.Approvals = append(latest.Approvals, state.Approval{
			ID:     "approval-unrelated",
			Action: "merge_pr",
			Target: &state.SupervisorTarget{PR: 2000},
			Status: state.ApprovalStatusPending,
		})
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	var removeCalls int
	err = CleanupLeasedWorktree(
		&config.Config{StateDir: stateDir},
		cycle,
		lease,
		CleanupProbes{PIDAlive: neverAlive, TmuxAlive: neverTmux},
		CleanupPolicy{RequireTerminal: true},
		CleanupHooks{Remove: func(string, string) error {
			removeCalls++
			return nil
		}},
	)
	if err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	if removeCalls != 1 {
		t.Fatalf("remove calls = %d, want 1", removeCalls)
	}
	if cycle.Sessions["sup-later"] != laterSession {
		t.Fatal("cleanup replaced the caller's sessions map during iteration")
	}

	// Simulate a later mutation by the same checkSessions range. The final
	// three-way merge must retain both that local change and the independently
	// persisted canonical update.
	laterSession.IssueTitle = "cycle update"
	if err := state.Save(stateDir, cycle); err != nil {
		t.Fatalf("save cycle state: %v", err)
	}
	canonical, err := state.Load(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if got := canonical.Sessions["sup-cleanup"].Worktree; got != "" {
		t.Fatalf("cleanup worktree claim = %q, want empty", got)
	}
	concurrent := canonical.Sessions["sup-concurrent"]
	if concurrent.PID != 4242 || concurrent.WorkerGeneration != 2 {
		t.Fatalf("concurrent session update lost: %+v", concurrent)
	}
	later := canonical.Sessions["sup-later"]
	if later.IssueTitle != "cycle update" {
		t.Fatalf("later cycle mutation lost: %+v", later)
	}
	if _, ok := canonical.FindApproval("approval-unrelated"); !ok {
		t.Fatal("unrelated approval was discarded by cleanup transition")
	}
}

func TestCleanupLeasedWorktree_BeforeRemoveFailureIsNonFatal(t *testing.T) {
	stateDir := t.TempDir()
	finished := time.Now().UTC().Add(-2 * time.Hour)
	s := state.NewState()
	s.Sessions["sup-963"] = &state.Session{
		IssueNumber:      963,
		PRNumber:         1044,
		Worktree:         "/worktrees/sup-963",
		Branch:           "feat/sup-963",
		StartedAt:        finished.Add(-time.Hour),
		FinishedAt:       &finished,
		Status:           state.StatusRetryExhausted,
		WorkerGeneration: 3,
	}
	if err := state.Save(stateDir, s); err != nil {
		t.Fatal(err)
	}
	lease := CaptureCleanupLease("sup-963", s.Sessions["sup-963"])

	var removeCalls int
	err := CleanupLeasedWorktree(
		&config.Config{StateDir: stateDir},
		s,
		lease,
		CleanupProbes{PIDAlive: neverAlive, TmuxAlive: neverTmux},
		CleanupPolicy{RequireTerminal: true},
		CleanupHooks{
			BeforeRemove: func() error { return errors.New("hook failed") },
			Remove: func(string, string) error {
				removeCalls++
				return nil
			},
		},
	)
	if err != nil {
		t.Fatalf("cleanup should continue after before_remove failure: %v", err)
	}
	if removeCalls != 1 {
		t.Fatalf("remove calls = %d, want 1", removeCalls)
	}
	canonical, err := state.Load(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if got := canonical.Sessions["sup-963"].Worktree; got != "" {
		t.Fatalf("worktree claim = %q, want empty", got)
	}
}

func TestCleanupLeasedWorktree_ApprovalDuringHookRestoresClaimBeforeRemove(t *testing.T) {
	stateDir := t.TempDir()
	finished := time.Now().UTC().Add(-2 * time.Hour)
	s := state.NewState()
	s.Sessions["sup-963"] = &state.Session{
		IssueNumber:      963,
		PRNumber:         1044,
		Worktree:         "/worktrees/sup-963",
		Branch:           "feat/sup-963",
		StartedAt:        finished.Add(-time.Hour),
		FinishedAt:       &finished,
		Status:           state.StatusRetryExhausted,
		WorkerGeneration: 3,
	}
	if err := state.Save(stateDir, s); err != nil {
		t.Fatal(err)
	}
	lease := CaptureCleanupLease("sup-963", s.Sessions["sup-963"])

	var removeCalls int
	err := CleanupLeasedWorktree(
		&config.Config{StateDir: stateDir},
		s,
		lease,
		CleanupProbes{PIDAlive: neverAlive, TmuxAlive: neverTmux},
		CleanupPolicy{RequireTerminal: true},
		CleanupHooks{
			BeforeRemove: func() error {
				return state.Update(stateDir, func(latest *state.State) error {
					latest.Approvals = append(latest.Approvals, state.Approval{
						ID:     "approval-repair-963",
						Action: "spawn_repair_worker",
						Target: &state.SupervisorTarget{Session: "sup-963", Issue: 963, PR: 1044},
						Status: state.ApprovalStatusAwaitingDispatch,
					})
					return nil
				})
			},
			Remove: func(string, string) error {
				removeCalls++
				return nil
			},
		},
	)
	if !errors.Is(err, ErrCleanupLeaseChanged) {
		t.Fatalf("cleanup err = %v, want ErrCleanupLeaseChanged", err)
	}
	if removeCalls != 0 {
		t.Fatalf("remove calls = %d, want 0", removeCalls)
	}
	canonical, loadErr := state.Load(stateDir)
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if got := canonical.Sessions["sup-963"].Worktree; got != "/worktrees/sup-963" {
		t.Fatalf("worktree claim = %q, want restored path", got)
	}
	if _, ok := canonical.FindApproval("approval-repair-963"); !ok {
		t.Fatal("repair approval was lost during cleanup compensation")
	}
}

func runCleanupGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func TestCleanupWorktrees_RemovesTerminalSessionWorktrees(t *testing.T) {
	// Create fake worktree directories
	tmpDir := t.TempDir()
	wt1 := filepath.Join(tmpDir, "wt1")
	wt2 := filepath.Join(tmpDir, "wt2")
	os.MkdirAll(wt1, 0755)
	os.MkdirAll(wt2, 0755)

	cfg := &config.Config{
		Repo:      "owner/repo",
		LocalPath: tmpDir,
	}

	s := state.NewState()
	s.Sessions["slot-1"] = &state.Session{
		IssueNumber: 100,
		Status:      state.StatusDone,
		Worktree:    wt1,
	}
	s.Sessions["slot-2"] = &state.Session{
		IssueNumber: 101,
		Status:      state.StatusDead,
		Worktree:    wt2,
	}
	// Running session should be skipped
	s.Sessions["slot-3"] = &state.Session{
		IssueNumber: 102,
		Status:      state.StatusRunning,
		Worktree:    filepath.Join(tmpDir, "wt3"),
	}

	results := CleanupWorktrees(cfg, s)

	// Note: actual git worktree remove will fail since these aren't real worktrees,
	// but we verify that the function attempts cleanup on the right sessions.
	if len(results) != 2 {
		t.Fatalf("expected 2 cleanup results, got %d", len(results))
	}

	// Running session should not be touched
	runSess := s.Sessions["slot-3"]
	if runSess.Worktree == "" {
		t.Error("running session worktree should not be cleared")
	}
}

func TestCleanupWorktrees_SkipsAlreadyCleanedSessions(t *testing.T) {
	cfg := &config.Config{
		Repo:      "owner/repo",
		LocalPath: "/tmp",
	}

	s := state.NewState()
	s.Sessions["slot-1"] = &state.Session{
		IssueNumber: 100,
		Status:      state.StatusDone,
		Worktree:    "", // Already cleaned
	}

	results := CleanupWorktrees(cfg, s)

	if len(results) != 0 {
		t.Fatalf("expected 0 cleanup results for already-cleaned sessions, got %d", len(results))
	}
}

func TestCleanupWorktrees_ClearsWorktreeFieldForMissingDirs(t *testing.T) {
	cfg := &config.Config{
		Repo:      "owner/repo",
		LocalPath: "/tmp",
	}

	s := state.NewState()
	s.Sessions["slot-1"] = &state.Session{
		IssueNumber: 100,
		Status:      state.StatusDone,
		Worktree:    "/nonexistent/path/that/does/not/exist",
	}

	results := CleanupWorktrees(cfg, s)

	// Directory doesn't exist, so it should silently clear the field
	if len(results) != 0 {
		t.Fatalf("expected 0 cleanup results for missing dirs, got %d", len(results))
	}
	if s.Sessions["slot-1"].Worktree != "" {
		t.Errorf("Worktree should be cleared for nonexistent directory, got %q", s.Sessions["slot-1"].Worktree)
	}
}

func TestCleanupWorktrees_HandlesAllTerminalStatuses(t *testing.T) {
	tmpDir := t.TempDir()

	terminalStatuses := []state.SessionStatus{
		state.StatusDone,
		state.StatusFailed,
		state.StatusConflictFailed,
		state.StatusDead,
	}

	cfg := &config.Config{
		Repo:      "owner/repo",
		LocalPath: tmpDir,
	}

	s := state.NewState()
	for i, status := range terminalStatuses {
		wt := filepath.Join(tmpDir, fmt.Sprintf("wt-%d", i))
		os.MkdirAll(wt, 0755)
		s.Sessions[fmt.Sprintf("slot-%d", i)] = &state.Session{
			IssueNumber: 100 + i,
			Status:      status,
			Worktree:    wt,
		}
	}

	results := CleanupWorktrees(cfg, s)

	// All 4 terminal sessions should be attempted
	if len(results) != 4 {
		t.Fatalf("expected 4 cleanup results, got %d", len(results))
	}
}
