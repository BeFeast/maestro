package orchestrator

import (
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/github"
	"github.com/befeast/maestro/internal/state"
	"github.com/befeast/maestro/internal/worker"
)

func TestFreshDispatchClaim_RepairApprovalCannotRevokeInFlightStartup(t *testing.T) {
	dir := t.TempDir()
	cfg := cfgWithBackends("claude", "claude")
	cfg.StateDir = dir
	cfg.WorktreeBase = filepath.Join(dir, "worktrees")
	cfg.SessionPrefix = "txc"
	issue := makeIssue(1, "single canonical worker", "maestro-ready")

	seed := state.NewState()
	seed.NextSlot = 2
	seed.Sessions["txc-1"] = &state.Session{
		IssueNumber: 1,
		IssueTitle:  issue.Title,
		Status:      state.StatusDead,
		Backend:     "claude",
	}
	if err := state.Save(dir, seed); err != nil {
		t.Fatal(err)
	}

	first, _, _ := newStartWorkersOrchestrator(cfg, []github.Issue{issue})
	second, _, _ := newStartWorkersOrchestrator(cfg, []github.Issue{issue})
	first.workerStartFn = nil
	second.workerStartFn = nil

	claimed := make(chan struct{})
	release := make(chan struct{})
	var starts atomic.Int32
	first.workerStartClaimedFn = func(_ *config.Config, s *state.State, _ string, got github.Issue, _ string, backend, slot string) (string, error) {
		starts.Add(1)
		close(claimed)
		<-release
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
		// Production StartReserved persists the launched session before it
		// returns to the orchestrator's claim-completion step.
		if err := state.Save(dir, s); err != nil {
			return "", err
		}
		return slot, nil
	}
	second.workerStartClaimedFn = func(_ *config.Config, _ *state.State, _ string, _ github.Issue, _ string, _ string, _ string) (string, error) {
		starts.Add(1)
		return "", nil
	}

	firstState, err := state.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	firstDone := make(chan struct{})
	go func() {
		defer close(firstDone)
		first.startNewWorkers(firstState, 1)
	}()
	<-claimed

	approvedAt := time.Now().UTC()
	if err := state.Update(dir, func(latest *state.State) error {
		approval := repairApproval("repair-1", 1, 0, state.ApprovalStatusAwaitingDispatch, approvedAt)
		approval.Target.Session = "txc-1"
		latest.Approvals = append(latest.Approvals, approval)
		return nil
	}); err != nil {
		t.Fatalf("persist competing repair approval: %v", err)
	}

	secondState, err := state.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	var repairs atomic.Int32
	second.respawnWorkerFn = func(_ *config.Config, _ string, _ *state.Session, _ string, _ github.Issue, _ string, _ string) error {
		repairs.Add(1)
		return nil
	}
	second.startNewWorkers(secondState, 1)
	if err := state.Save(dir, secondState); err != nil {
		t.Fatalf("persist competing poll: %v", err)
	}
	if got := repairs.Load(); got != 0 {
		t.Fatalf("repair started while fresh startup held the durable claim: got %d", got)
	}
	if got := approvalStatus(t, secondState, "repair-1"); got != state.ApprovalStatusStale {
		t.Fatalf("competing repair approval = %q, want stale", got)
	}
	claim, active := secondState.FreshDispatchClaimFor(1)
	if !active || claim.Slot != "txc-2" {
		t.Fatalf("fresh claim after competing poll = %+v active=%t, want txc-2 retained", claim, active)
	}

	close(release)
	<-firstDone

	reloaded, err := state.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := starts.Load(); got != 1 {
		t.Fatalf("fresh starts = %d, want one canonical startup", got)
	}
	if got := repairs.Load(); got != 0 {
		t.Fatalf("repair starts = %d, want none after fresh reservation won", got)
	}
	if reloaded.Sessions["txc-2"] == nil || reloaded.Sessions["txc-2"].Status != state.StatusRunning {
		t.Fatalf("canonical fresh session = %+v", reloaded.Sessions["txc-2"])
	}
	if reloaded.Sessions["txc-1"] == nil || reloaded.Sessions["txc-1"].Status != state.StatusDead {
		t.Fatalf("original repair target changed = %+v", reloaded.Sessions["txc-1"])
	}
	if reloaded.NextSlot != 3 {
		t.Fatalf("next_slot = %d, want 3 (no duplicate allocation)", reloaded.NextSlot)
	}
	evidence := reloaded.FreshDispatchClaims[1]
	if evidence == nil || evidence.Status != state.FreshDispatchClaimStatusCompleted || evidence.Slot != "txc-2" {
		t.Fatalf("completed fresh claim = %+v", evidence)
	}

	second.startNewWorkers(reloaded, 1)
	if got := starts.Load(); got != 1 {
		t.Fatalf("reload/retry started another worker: starts=%d", got)
	}
	if got := repairs.Load(); got != 0 {
		t.Fatalf("reload/retry dispatched stale repair: repairs=%d", got)
	}
}

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

func TestFreshDispatchClaim_StartFailureSupersedesLease(t *testing.T) {
	dir := t.TempDir()
	cfg := cfgWithBackends("claude", "claude")
	cfg.StateDir = dir
	cfg.WorktreeBase = filepath.Join(dir, "worktrees")
	cfg.SessionPrefix = "ok-player"
	if err := state.Save(dir, state.NewState()); err != nil {
		t.Fatal(err)
	}
	issue := makeIssue(394, "release failed startup", "maestro-ready")
	orch, _, _ := newStartWorkersOrchestrator(cfg, []github.Issue{issue})
	orch.workerStartFn = nil
	var attemptedSlots []string
	orch.workerStartClaimedFn = func(_ *config.Config, _ *state.State, _ string, _ github.Issue, _ string, _ string, slot string) (string, error) {
		attemptedSlots = append(attemptedSlots, slot)
		return "", errors.New("base checkout blocked")
	}

	cycle, err := state.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	orch.startNewWorkers(cycle, 1)
	if _, active := cycle.FreshDispatchClaimFor(394); active {
		t.Fatal("failed startup remained active in cycle state")
	}
	if err := state.Save(dir, cycle); err != nil {
		t.Fatalf("post-failure cycle save: %v", err)
	}

	loaded, err := state.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	claim := loaded.FreshDispatchClaims[394]
	if claim == nil || claim.Status != state.FreshDispatchClaimStatusSuperseded || claim.TerminalReason != "start_failed" {
		t.Fatalf("failed startup claim = %+v", claim)
	}
	if !claim.LeaseExpiresAt.IsZero() || claim.CompletedAt.IsZero() {
		t.Fatalf("failed startup claim retained lease = %+v", claim)
	}
	if len(loaded.Sessions) != 0 {
		t.Fatalf("sessions after pre-launch failure = %+v, want none", loaded.Sessions)
	}
	if loaded.NextSlot != 2 {
		t.Fatalf("next_slot = %d, want 2", loaded.NextSlot)
	}

	firstBranch := claim.Branch
	firstWorktree := claim.Worktree
	orch.startNewWorkers(loaded, 1)
	if err := state.Save(dir, loaded); err != nil {
		t.Fatalf("post-retry cycle save: %v", err)
	}
	retried, err := state.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	claim = retried.FreshDispatchClaims[394]
	if len(attemptedSlots) != 2 || attemptedSlots[0] != "ok-player-1" || attemptedSlots[1] != attemptedSlots[0] {
		t.Fatalf("failed startup slots = %v, want the reserved slot reused", attemptedSlots)
	}
	if claim == nil || claim.Status != state.FreshDispatchClaimStatusSuperseded || claim.TerminalReason != "start_failed" || claim.LeaseGeneration != 2 {
		t.Fatalf("retried failed startup claim = %+v", claim)
	}
	if claim.Branch != firstBranch || claim.Worktree != firstWorktree || retried.NextSlot != 2 {
		t.Fatalf("retry changed reserved identity: claim=%+v next_slot=%d", claim, retried.NextSlot)
	}
}
