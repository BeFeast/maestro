package orchestrator

import (
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/github"
	"github.com/befeast/maestro/internal/state"
	"github.com/befeast/maestro/internal/worker"
)

func TestFreshDispatchClaim_PreventsConcurrentDuplicateBeforeSessionRegistration(t *testing.T) {
	dir := t.TempDir()
	cfg := cfgWithBackends("claude", "claude")
	cfg.StateDir = dir
	cfg.WorktreeBase = filepath.Join(dir, "worktrees")
	cfg.SessionPrefix = "ok-player"
	if err := state.Save(dir, state.NewState()); err != nil {
		t.Fatal(err)
	}
	issue := makeIssue(394, "atomic fresh dispatch", "maestro-ready")
	first, _, _ := newStartWorkersOrchestrator(cfg, []github.Issue{issue})
	second, _, _ := newStartWorkersOrchestrator(cfg, []github.Issue{issue})
	first.workerStartFn = nil
	second.workerStartFn = nil

	claimed := make(chan struct{})
	release := make(chan struct{})
	var starts atomic.Int32
	start := func(_ *config.Config, s *state.State, _ string, got github.Issue, _ string, backend, slot string) (string, error) {
		if starts.Add(1) == 1 {
			close(claimed)
			<-release
		}
		startedAt := time.Now().UTC()
		s.Sessions[slot] = &state.Session{
			IssueNumber: got.Number,
			IssueTitle:  got.Title,
			Worktree:    filepath.Join(cfg.WorktreeBase, slot),
			Branch:      worker.BranchName(slot, got),
			PID:         4242,
			TmuxSession: worker.TmuxSessionName(slot),
			StartedAt:   startedAt,
			Status:      state.StatusRunning,
			Backend:     backend,
		}
		return slot, nil
	}
	first.workerStartClaimedFn = start
	second.workerStartClaimedFn = start

	firstState, _ := state.Load(dir)
	secondState, _ := state.Load(dir) // deliberately stale before the first claim
	done := make(chan struct{})
	go func() {
		defer close(done)
		first.startNewWorkers(firstState, 1)
	}()
	<-claimed

	second.startNewWorkers(secondState, 1)
	if got := starts.Load(); got != 1 {
		t.Fatalf("worker starts while first claim in flight = %d, want 1", got)
	}
	contended, _ := state.Load(dir)
	claim := contended.FreshDispatchClaims[394]
	if claim == nil || claim.Status != state.FreshDispatchClaimStatusClaimed || claim.ContentionCount != 1 || claim.Slot != "ok-player-1" {
		t.Fatalf("durable contended claim = %+v", claim)
	}

	close(release)
	<-done
	// RunOnce performs an ordinary save after startNewWorkers returns. That stale
	// pre-claim snapshot must merge cleanly with the atomic claim/session writes
	// instead of resurrecting the lease or reporting a session conflict.
	if err := state.Save(dir, firstState); err != nil {
		t.Fatalf("post-dispatch cycle save: %v", err)
	}
	loaded, _ := state.Load(dir)
	claim = loaded.FreshDispatchClaims[394]
	if claim == nil || claim.Status != state.FreshDispatchClaimStatusCompleted || claim.ContentionCount != 1 || claim.TerminalReason != "session_committed" {
		t.Fatalf("completed claim evidence = %+v", claim)
	}
	if len(loaded.Sessions) != 1 || loaded.Sessions["ok-player-1"] == nil || loaded.Sessions["ok-player-1"].IssueNumber != 394 {
		t.Fatalf("sessions = %+v, want exactly canonical ok-player-1", loaded.Sessions)
	}
	if loaded.NextSlot != 2 {
		t.Fatalf("next_slot = %d, want 2 (no duplicate allocation)", loaded.NextSlot)
	}
}
