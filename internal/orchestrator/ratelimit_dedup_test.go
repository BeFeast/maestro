package orchestrator

import (
	"errors"
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

// TestRunOnce_AutoCreatedPRNotOrphaned is the regression guard for the #794
// review finding: the per-cycle open-PR cache is populated before
// reconcileRunningSessions auto-creates a PR, so a stale cache made autoMergePRs
// miss the freshly created PR for the now-pr_open session, take the "no open
// PR — assuming merged/closed" branch, and mark the session done — orphaning a
// live PR in the same RunOnce. createPR must invalidate the cache so a later
// cycle step re-fetches and finds the PR.
func TestRunOnce_AutoCreatedPRNotOrphaned(t *testing.T) {
	cfg := &config.Config{
		Repo:              "owner/repo",
		StateDir:          t.TempDir(),
		MaxParallel:       1,
		MaxRuntimeMinutes: 60,
	}

	// A running session whose worker process + tmux are gone but whose branch
	// was already pushed, with no PR recorded. reconcileRunningSessions takes the
	// recovery path and auto-creates the PR via reconcilePushedBranch.
	s := state.NewState()
	s.Sessions["slot-0"] = &state.Session{
		IssueNumber: 200,
		IssueTitle:  "issue 200",
		Status:      state.StatusRunning,
		Branch:      "feat/b",
		PRNumber:    0,
		PID:         5555,
		TmuxSession: "maestro-slot-0",
		StartedAt:   time.Now().UTC(),
	}
	if err := state.Save(cfg.StateDir, s); err != nil {
		t.Fatalf("save state: %v", err)
	}

	const newPR = 42
	prCreated := false
	listCalls := 0
	ciChecked := false
	o := &Orchestrator{
		cfg:      cfg,
		repo:     cfg.Repo,
		notifier: &notify.Notifier{},
		// The open-PR list reflects the live world: empty until the PR is
		// created, then containing it. Before the review fix the empty list was
		// cached for the whole cycle, so autoMergePRs never saw PR #42.
		listOpenPRsFn: func() ([]github.PR, error) {
			listCalls++
			if !prCreated {
				return nil, nil
			}
			return []github.PR{{Number: newPR, HeadRefName: "feat/b", State: "OPEN"}}, nil
		},
		pidAliveFn:           func(pid int) bool { return false },
		tmuxSessionExistsFn:  func(name string) bool { return false },
		remoteBranchExistsFn: func(branch string) (bool, error) { return true, nil },
		createPRFn: func(title, body, base, head string) (int, error) {
			prCreated = true
			return newPR, nil
		},
		// autoMergePRs must find PR #42 and inspect its CI. "pending" keeps the
		// session in the merge flow without merging it, so it stays pr_open and
		// the orphaning regression (status flipping to done) is observable.
		ghPRCIStatusFn: func(prNumber int) (string, error) {
			ciChecked = true
			if prNumber != newPR {
				t.Errorf("CI checked for PR #%d, want #%d", prNumber, newPR)
			}
			return "pending", nil
		},
		// Non-MERGEABLE keeps the #424 pending→success override from firing and
		// keeps rebaseConflicts a no-op, so nothing merges the PR.
		ghPRMergeStatusFn: func(prNumber int) (string, string, error) {
			return "UNKNOWN", "unknown", nil
		},
		isIssueClosedFn:  func(issueNumber int) (bool, error) { return false, nil },
		captureTmuxFn:    func(session string) (string, error) { return "", nil },
		listOpenIssuesFn: func(labels []string) ([]github.Issue, error) { return nil, nil },
	}

	if err := o.RunOnce(); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	final, err := state.Load(cfg.StateDir)
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	sess := final.Sessions["slot-0"]
	if sess == nil {
		t.Fatal("session slot-0 vanished")
	}
	if sess.Status != state.StatusPROpen {
		t.Fatalf("session status = %q, want pr_open — the auto-created PR #%d was orphaned", sess.Status, newPR)
	}
	if sess.PRNumber != newPR {
		t.Fatalf("session PRNumber = %d, want %d", sess.PRNumber, newPR)
	}
	if !ciChecked {
		t.Fatal("autoMergePRs never inspected the auto-created PR — it was not found in the (stale) cached list")
	}
	// One fetch before creation + one after the cache was invalidated.
	if listCalls < 2 {
		t.Fatalf("ListOpenPRs fired %d times; want ≥2 (a re-fetch after the PR was created)", listCalls)
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

	// Invalidating the cache mid-cycle (as a PR create/merge/close does) forces
	// the next call to re-fetch, then re-memoize.
	o.invalidateCyclePRs()
	for i := 0; i < 3; i++ {
		if _, err := o.listOpenPRsForCycle(); err != nil {
			t.Fatalf("listOpenPRsForCycle (post-invalidate): %v", err)
		}
	}
	if calls != 5 {
		t.Fatalf("post-invalidate calls fetched %d times total, want 5 (one re-fetch then memoized)", calls)
	}
	o.endCycle()
}

// TestListOpenPRsForCycle_SkipsWhilePrimaryRateLimited proves requirement #2 /
// acceptance #2 at the orchestrator layer: when the shared core-REST bucket is
// paused on a primary rate-limit exhaustion (#812), a cycle skips its dominant
// open-PR poll entirely — never issuing the doomed fetch — and surfaces the
// pause instant so the four cycle steps degrade to a no-op until reset.
func TestListOpenPRsForCycle_SkipsWhilePrimaryRateLimited(t *testing.T) {
	reset := time.Now().Add(30 * time.Minute)
	calls := 0
	o := &Orchestrator{
		repo: "owner/repo",
		listOpenPRsFn: func() ([]github.PR, error) {
			calls++
			return nil, nil
		},
		primaryRESTPausedFn: func() (bool, time.Time) { return true, reset },
	}
	o.beginCycle()
	defer o.endCycle()

	prs, err := o.listOpenPRsForCycle()
	if err == nil {
		t.Fatal("listOpenPRsForCycle err = nil, want a primary-rate-limit skip error")
	}
	var primaryErr *github.PrimaryRateLimitError
	if !errors.As(err, &primaryErr) {
		t.Fatalf("err = %v, want a *github.PrimaryRateLimitError", err)
	}
	if !primaryErr.ResetAt.Equal(reset) {
		t.Fatalf("skip error ResetAt = %s, want %s", primaryErr.ResetAt, reset)
	}
	if prs != nil {
		t.Fatalf("prs = %v, want nil while paused", prs)
	}
	if calls != 0 {
		t.Fatalf("ListOpenPRs fired %d times while paused, want 0 (the doomed call must be skipped)", calls)
	}

	// The skip verdict is cached for the cycle: a second consumer reuses it
	// without a fetch (still zero calls).
	if _, err := o.listOpenPRsForCycle(); err == nil {
		t.Fatal("second consumer err = nil, want the cached skip error")
	}
	if calls != 0 {
		t.Fatalf("ListOpenPRs fired %d times, want 0 (skip verdict cached per cycle)", calls)
	}
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
