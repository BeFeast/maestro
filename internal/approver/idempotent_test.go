package approver

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/state"
)

// fakeSessionLookup is a minimal in-memory SessionLookup implementation.
type fakeSessionLookup map[string]*state.Session

func (f fakeSessionLookup) LookupSession(slot string) (*state.Session, bool) {
	s, ok := f[slot]
	return s, ok
}

// blockingGH wraps fakeGH so we can hold MergePR mid-flight from a
// test, exposing whether the OTHER goroutine called MergePR while we
// held the per-approval lock.
type blockingGH struct {
	mu       sync.Mutex
	gate     chan struct{} // closed when test is ready to release MergePR
	released chan struct{} // closed when MergePR returns
	calls    int32
}

func (b *blockingGH) MergePR(pr int) error {
	atomic.AddInt32(&b.calls, 1)
	if b.gate != nil {
		<-b.gate
	}
	if b.released != nil {
		close(b.released)
	}
	return nil
}
func (b *blockingGH) CloseIssue(issue int, comment string) error {
	atomic.AddInt32(&b.calls, 1)
	return nil
}

// PRMergeStatus returns the zero verdict so executeMergePR falls straight
// through to MergePR — this test only exercises the per-approval lock, not
// the #547 behind-branch path.
func (b *blockingGH) PRMergeStatus(pr int) (string, string, error) {
	return "", "", nil
}
func (b *blockingGH) UpdateBranch(pr int) error {
	atomic.AddInt32(&b.calls, 1)
	return nil
}

// --- #488: per-approval-ID lock (concurrent Execute) -----------------------

func TestExecute_ConcurrentSameApproval_OnlyOneSideEffect(t *testing.T) {
	gate := make(chan struct{})
	released := make(chan struct{})
	gh := &blockingGH{gate: gate, released: released}
	ex := &Executor{GH: gh, Cfg: newCfg()}

	a := mkApproval(config.SupervisorActionMergePR, &state.SupervisorTarget{PR: 7}, "merge", "")

	results := make(chan Result, 2)
	go func() { results <- ex.Execute(a) }()
	// Tiny pause to make sure the first goroutine has acquired the lock
	// before the second one races in. Using gh.calls as the rendezvous
	// signal keeps the test independent of clock jitter.
	deadline := time.Now().Add(2 * time.Second)
	for atomic.LoadInt32(&gh.calls) == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if atomic.LoadInt32(&gh.calls) == 0 {
		t.Fatal("first goroutine never reached MergePR — deadlock or test setup bug")
	}
	go func() { results <- ex.Execute(a) }()

	// The second goroutine returns immediately on TryLock failure.
	first := <-results
	if first.Status != state.ApprovalStatusExecutionSkipped {
		t.Fatalf("expected first-completing goroutine to be skipped (TryLock loser); got %+v", first)
	}

	// Now release the held MergePR so the executing goroutine finishes.
	close(gate)
	<-released
	second := <-results
	if second.Status != state.ApprovalStatusExecuted {
		t.Fatalf("expected the held goroutine to land executed; got %+v", second)
	}

	if got := atomic.LoadInt32(&gh.calls); got != 1 {
		t.Fatalf("expected MergePR called exactly once, got %d", got)
	}
}

// --- #488: slot-reuse fence on delete_worktree ----------------------------

func TestExecute_DeleteWorktree_SlotReuseRefused(t *testing.T) {
	wt := &fakeWT{}
	ex := &Executor{
		Worktrees: wt,
		Cfg:       newCfg(),
		Sessions: fakeSessionLookup{
			"sup-77": &state.Session{IssueNumber: 999}, // recycled to a different issue
		},
	}
	a := mkApproval(config.SupervisorActionDeleteWorktree, &state.SupervisorTarget{Session: "sup-77", Issue: 812}, "stale", "")

	res := ex.Execute(a)
	if res.Status != state.ApprovalStatusExecutionFailed {
		t.Fatalf("res = %+v, want execution_failed", res)
	}
	if len(wt.calls) != 0 {
		t.Fatalf("RemoveWorktree was called %d times — fence failed", len(wt.calls))
	}
	if res.Err == nil {
		t.Fatalf("expected non-nil Err, got nil")
	}
}

func TestExecute_DeleteWorktree_SlotMatchesProceeds(t *testing.T) {
	wt := &fakeWT{}
	ex := &Executor{
		Worktrees: wt,
		Cfg:       newCfg(),
		Sessions: fakeSessionLookup{
			"sup-77": &state.Session{IssueNumber: 812}, // same issue — fine
		},
	}
	a := mkApproval(config.SupervisorActionDeleteWorktree, &state.SupervisorTarget{Session: "sup-77", Issue: 812}, "stale", "")

	res := ex.Execute(a)
	if res.Status != state.ApprovalStatusExecuted {
		t.Fatalf("res = %+v, want executed", res)
	}
	if len(wt.calls) != 1 {
		t.Fatalf("expected 1 RemoveWorktree call, got %d", len(wt.calls))
	}
}

func TestExecute_DeleteWorktree_NoSessionsLookupSkipsFence(t *testing.T) {
	// Backward compat: callers that don't wire Sessions get the previous
	// behaviour (no fence).
	wt := &fakeWT{}
	ex := &Executor{Worktrees: wt, Cfg: newCfg()} // Sessions: nil
	a := mkApproval(config.SupervisorActionDeleteWorktree, &state.SupervisorTarget{Session: "sup-77", Issue: 812}, "stale", "")

	res := ex.Execute(a)
	if res.Status != state.ApprovalStatusExecuted {
		t.Fatalf("res = %+v, want executed", res)
	}
}

func TestExecute_DeleteWorktree_NoLiveSessionProceeds(t *testing.T) {
	// Slot is empty / not bound — no race possible, fence is a no-op.
	wt := &fakeWT{}
	ex := &Executor{
		Worktrees: wt,
		Cfg:       newCfg(),
		Sessions:  fakeSessionLookup{}, // empty
	}
	a := mkApproval(config.SupervisorActionDeleteWorktree, &state.SupervisorTarget{Session: "sup-77", Issue: 812}, "stale", "")

	res := ex.Execute(a)
	if res.Status != state.ApprovalStatusExecuted {
		t.Fatalf("res = %+v, want executed", res)
	}
}

func TestExecute_DeleteWorktree_NoTargetIssueSkipsFence(t *testing.T) {
	// Older approvals may not carry Target.Issue. Don't trip the fence —
	// fall through to the previous behaviour. (When #489 lands and
	// stamps Issue on every approval, this case will be obsolete.)
	wt := &fakeWT{}
	ex := &Executor{
		Worktrees: wt,
		Cfg:       newCfg(),
		Sessions: fakeSessionLookup{
			"sup-77": &state.Session{IssueNumber: 999},
		},
	}
	a := mkApproval(config.SupervisorActionDeleteWorktree, &state.SupervisorTarget{Session: "sup-77" /* Issue absent */}, "x", "")

	res := ex.Execute(a)
	if res.Status != state.ApprovalStatusExecuted {
		t.Fatalf("res = %+v, want executed (fence must skip when approval lacks target issue)", res)
	}
}
