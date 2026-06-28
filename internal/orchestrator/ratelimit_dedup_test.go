package orchestrator

import (
	"testing"
	"time"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/github"
	"github.com/befeast/maestro/internal/notify"
	"github.com/befeast/maestro/internal/state"
)

// TestRunOnce_ListOpenPRsFiresOnce proves acceptance criterion #3 / requirement
// #20: ListOpenPRs is the dominant (~78%) forge REST read and was issued 4× per
// RunOnce; after the dedup it must fire at most once per cycle.
func TestRunOnce_ListOpenPRsFiresOnce(t *testing.T) {
	cfg := &config.Config{
		Repo:              "owner/repo",
		StateDir:          t.TempDir(),
		MaxParallel:       1,
		MaxRuntimeMinutes: 60,
	}

	// A running session with a live process/tmux + an open PR on its branch
	// exercises every cycle step that consumes the open-PR list (reconcile,
	// check, auto-merge, rebase — each calls listOpenPRsForCycle at its top), so
	// a regression that re-fetches in any of them is caught. Running (not
	// pr_open) keeps the one slot occupied so startNewWorkers is skipped.
	s := state.NewState()
	s.Sessions["slot-0"] = &state.Session{
		IssueNumber: 100,
		IssueTitle:  "issue 100",
		Status:      state.StatusRunning,
		Branch:      "feat/a",
		PRNumber:    10,
		PID:         4321,
		TmuxSession: "maestro-slot-0",
		StartedAt:   time.Now().UTC(),
	}
	if err := state.Save(cfg.StateDir, s); err != nil {
		t.Fatalf("save state: %v", err)
	}

	listCalls := 0
	o := &Orchestrator{
		cfg:      cfg,
		repo:     cfg.Repo,
		notifier: &notify.Notifier{},
		listOpenPRsFn: func() ([]github.PR, error) {
			listCalls++
			return []github.PR{{Number: 10, HeadRefName: "feat/a", State: "OPEN"}}, nil
		},
		pidAliveFn:          func(pid int) bool { return true },
		tmuxSessionExistsFn: func(name string) bool { return true },
		isIssueClosedFn:     func(issueNumber int) (bool, error) { return false, nil },
		captureTmuxFn:       func(session string) (string, error) { return "", nil },
	}

	if err := o.RunOnce(); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	if listCalls != 1 {
		t.Fatalf("ListOpenPRs fired %d times in one RunOnce, want exactly 1 (was 4× before #794)", listCalls)
	}

	// A second cycle must re-fetch — the cache is per-RunOnce, not persistent.
	if err := o.RunOnce(); err != nil {
		t.Fatalf("RunOnce second pass: %v", err)
	}
	if listCalls != 2 {
		t.Fatalf("ListOpenPRs fired %d times across two cycles, want 2 (one per cycle)", listCalls)
	}
}

// TestListOpenPRsForCycle_NoMemoizationOutsideCycle guards the behavior that
// keeps single-step callers/tests correct: outside an active cycle each call
// fetches fresh.
func TestListOpenPRsForCycle_NoMemoizationOutsideCycle(t *testing.T) {
	calls := 0
	o := &Orchestrator{
		listOpenPRsFn: func() ([]github.PR, error) {
			calls++
			return nil, nil
		},
	}

	for i := 0; i < 3; i++ {
		if _, err := o.listOpenPRsForCycle(); err != nil {
			t.Fatalf("listOpenPRsForCycle: %v", err)
		}
	}
	if calls != 3 {
		t.Fatalf("out-of-cycle calls fetched %d times, want 3 (no memoization)", calls)
	}

	// Inside a cycle the fetch is memoized.
	o.beginCycle()
	for i := 0; i < 3; i++ {
		if _, err := o.listOpenPRsForCycle(); err != nil {
			t.Fatalf("listOpenPRsForCycle (in cycle): %v", err)
		}
	}
	if calls != 4 {
		t.Fatalf("in-cycle calls fetched %d times total, want 4 (one shared fetch)", calls)
	}
	o.endCycle()
}

func TestComputeStartupJitter(t *testing.T) {
	if d := computeStartupJitter(0, 0.5); d != 0 {
		t.Fatalf("non-positive interval jitter = %s, want 0", d)
	}
	if d := computeStartupJitter(-time.Second, 0.5); d != 0 {
		t.Fatalf("negative interval jitter = %s, want 0", d)
	}
	// Within the cap: full span available.
	if d := computeStartupJitter(40*time.Second, 0); d != 0 {
		t.Fatalf("frac 0 jitter = %s, want 0", d)
	}
	if d := computeStartupJitter(40*time.Second, 0.5); d != 20*time.Second {
		t.Fatalf("frac 0.5 of 40s = %s, want 20s", d)
	}
	// Above the cap: span is clamped to startupJitterCap. rand.Float64 yields
	// [0,1), so the largest realizable offset stays strictly under the cap.
	if d := computeStartupJitter(10*time.Minute, 0.999); d >= startupJitterCap {
		t.Fatalf("jitter %s, want < cap %s for a long interval", d, startupJitterCap)
	}
	if d := computeStartupJitter(10*time.Minute, 0.5); d != startupJitterCap/2 {
		t.Fatalf("frac 0.5 above cap = %s, want %s", d, startupJitterCap/2)
	}
}
