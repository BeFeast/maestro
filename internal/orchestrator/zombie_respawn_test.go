package orchestrator

// #800: a session's saved retry/review-feedback state must not outlive the
// work it was scheduled for. Merge of the session's PR or closing of its
// issue invalidates the pending retry — the pre-respawn staleness check
// settles the session instead of respawning a zombie worker, and the
// reconcile auto-create path skips branches whose content already landed.

import (
	"fmt"
	"testing"
	"time"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/github"
	"github.com/befeast/maestro/internal/notify"
	"github.com/befeast/maestro/internal/state"
)

func zombieRetryOrchestrator(t *testing.T, respawned *bool) *Orchestrator {
	t.Helper()
	return &Orchestrator{
		cfg: &config.Config{
			Repo:              "owner/repo",
			MaxRetryBackoffMs: 300000,
			MaxRuntimeMinutes: 999,
		},
		notifier:   &notify.Notifier{},
		promptBase: "test prompt",
		getIssueFn: func(number int) (github.Issue, error) {
			return makeIssue(number, "test issue"), nil
		},
		respawnWorkerFn: func(cfg *config.Config, slotName string, sess *state.Session, repo string, issue github.Issue, promptBase string, backend string) error {
			*respawned = true
			sess.Status = state.StatusRunning
			sess.PID = 5555
			return nil
		},
		respawnInPlaceFn: func(cfg *config.Config, slotName string, sess *state.Session, repo string, issue github.Issue, promptBase string, backend string) error {
			*respawned = true
			sess.Status = state.StatusRunning
			sess.PID = 5555
			return nil
		},
	}
}

func TestRespawnDueRetries_IssueClosed_RetiresInsteadOfRespawning(t *testing.T) {
	respawned := false
	o := zombieRetryOrchestrator(t, &respawned)
	o.isIssueClosedFn = func(issueNumber int) (bool, error) {
		return issueNumber == 133, nil
	}

	pastTime := time.Now().UTC().Add(-1 * time.Second)
	s := state.NewState()
	s.Sessions["ok-player-1"] = &state.Session{
		IssueNumber: 133,
		IssueTitle:  "closed while backoff ran",
		Status:      state.StatusDead,
		RetryCount:  1,
		NextRetryAt: &pastTime,
		Branch:      "feat/ok-player-1-133-test",
	}

	o.respawnDueRetries(s, 10)

	if respawned {
		t.Fatal("worker must NOT respawn for a closed issue")
	}
	sess := s.Sessions["ok-player-1"]
	if sess.Status != state.StatusDone {
		t.Fatalf("status = %q, want %q", sess.Status, state.StatusDone)
	}
	if sess.NextRetryAt != nil {
		t.Fatal("NextRetryAt must be cleared when the retry is retired")
	}
	if sess.FinishedAt == nil {
		t.Fatal("FinishedAt should be set when the retry is retired")
	}
}

func TestRespawnDueRetries_SessionPRMerged_RetiresInsteadOfRespawning(t *testing.T) {
	respawned := false
	o := zombieRetryOrchestrator(t, &respawned)
	o.isPRMergedFn = func(prNumber int) (bool, error) {
		return prNumber == 135, nil
	}
	o.isIssueClosedFn = func(issueNumber int) (bool, error) {
		return false, nil // issue still open — the merged PR alone must invalidate the retry
	}

	pastTime := time.Now().UTC().Add(-1 * time.Second)
	s := state.NewState()
	s.Sessions["ok-player-1"] = &state.Session{
		IssueNumber: 133,
		IssueTitle:  "review retry with merged PR",
		Status:      state.StatusDead,
		RetryCount:  1,
		NextRetryAt: &pastTime,
		Branch:      "feat/ok-player-1-133-test",
		Worktree:    "/tmp/maestro-ok-player-1",
		PRNumber:    135,
	}

	o.respawnDueRetries(s, 10)

	if respawned {
		t.Fatal("worker must NOT respawn when the session's PR is already merged")
	}
	sess := s.Sessions["ok-player-1"]
	if sess.Status != state.StatusCodeLanded {
		t.Fatalf("status = %q, want %q", sess.Status, state.StatusCodeLanded)
	}
	if sess.NextRetryAt != nil {
		t.Fatal("NextRetryAt must be cleared when the retry is retired")
	}
}

// Regression for the BeFeast/ok-player 2026-07-02 incident (#800): CI fails on
// the PR, the CI-retry path closes it and schedules a backoff retry; an
// operator then pushes a fix, reopens the PR, and merges it. When the backoff
// elapses the orchestrator must NOT respawn — the closed-then-merged PR is
// tracked via LastClosedPRNumber and invalidates the retry even though the
// issue itself is still open (maestro PRs carry no auto-closing keywords).
func TestRespawnDueRetries_PRClosedByCIRetryThenReopenedAndMerged_NoRespawn(t *testing.T) {
	respawned := false
	o := zombieRetryOrchestrator(t, &respawned)
	o.ghPRChecksOutputFn = func(prNumber int) (string, error) { return "build failed", nil }
	o.ghCollectPRReviewFeedbackFn = func(prNumber int) (string, error) { return "", nil }
	o.ghClosePRFn = func(prNumber int, comment string) error { return nil }
	o.workerStopFn = func(cfg *config.Config, slotName string, sess *state.Session) error { return nil }

	s := state.NewState()
	sess := &state.Session{
		IssueNumber: 133,
		IssueTitle:  "zombie respawn incident",
		Status:      state.StatusPROpen,
		PRNumber:    135,
		Branch:      "feat/ok-player-1-133-test",
		Worktree:    "/tmp/maestro-ok-player-1",
	}
	s.Sessions["ok-player-1"] = sess

	// Step 1: CI fails — maestro closes PR #135 and schedules a retry.
	o.handleCIFailureRetry(s, "ok-player-1", sess, github.PR{Number: 135, HeadRefName: sess.Branch})

	if sess.Status != state.StatusDead || sess.NextRetryAt == nil {
		t.Fatalf("after CI-retry: status = %q nextRetryAt = %v, want dead with a scheduled retry", sess.Status, sess.NextRetryAt)
	}
	if sess.PRNumber != 0 {
		t.Fatalf("after CI-retry: PRNumber = %d, want 0 (PR closed)", sess.PRNumber)
	}
	if sess.LastClosedPRNumber != 135 {
		t.Fatalf("after CI-retry: LastClosedPRNumber = %d, want 135", sess.LastClosedPRNumber)
	}

	// Step 2: operator pushes a fix, reopens PR #135, and merges it. The
	// issue stays open (no auto-closing keywords on maestro PRs).
	o.isPRMergedFn = func(prNumber int) (bool, error) {
		return prNumber == 135, nil
	}
	o.isIssueClosedFn = func(issueNumber int) (bool, error) {
		return false, nil
	}

	// Step 3: backoff elapses.
	pastTime := time.Now().UTC().Add(-1 * time.Second)
	sess.NextRetryAt = &pastTime

	o.respawnDueRetries(s, 10)

	if respawned {
		t.Fatal("worker must NOT respawn after the CI-closed PR was reopened and merged")
	}
	if sess.Status != state.StatusCodeLanded {
		t.Fatalf("status = %q, want %q", sess.Status, state.StatusCodeLanded)
	}
	if sess.PRNumber != 135 {
		t.Fatalf("PRNumber = %d, want 135 (restored from the merged PR)", sess.PRNumber)
	}
	if sess.NextRetryAt != nil {
		t.Fatal("NextRetryAt must be cleared when the retry is retired")
	}
}

func TestRespawnDueRetries_GitHubReadErrorFailsOpen(t *testing.T) {
	respawned := false
	o := zombieRetryOrchestrator(t, &respawned)
	o.isPRMergedFn = func(prNumber int) (bool, error) {
		return false, fmt.Errorf("transient API error")
	}
	o.isIssueClosedFn = func(issueNumber int) (bool, error) {
		return false, fmt.Errorf("transient API error")
	}

	pastTime := time.Now().UTC().Add(-1 * time.Second)
	s := state.NewState()
	s.Sessions["mae-1"] = &state.Session{
		IssueNumber:        200,
		IssueTitle:         "retry survives GitHub hiccup",
		Status:             state.StatusDead,
		RetryCount:         1,
		NextRetryAt:        &pastTime,
		Branch:             "feat/mae-1-200-test",
		LastClosedPRNumber: 42,
	}

	o.respawnDueRetries(s, 10)

	if !respawned {
		t.Fatal("a transient GitHub read error must not strand a legitimate retry")
	}
	if s.Sessions["mae-1"].Status != state.StatusRunning {
		t.Fatalf("status = %q, want %q", s.Sessions["mae-1"].Status, state.StatusRunning)
	}
}

func TestRespawnDueRetries_IssueOpenPRNotMerged_StillRespawns(t *testing.T) {
	respawned := false
	o := zombieRetryOrchestrator(t, &respawned)
	o.isPRMergedFn = func(prNumber int) (bool, error) { return false, nil }
	o.isIssueClosedFn = func(issueNumber int) (bool, error) { return false, nil }

	pastTime := time.Now().UTC().Add(-1 * time.Second)
	s := state.NewState()
	s.Sessions["mae-2"] = &state.Session{
		IssueNumber:        201,
		IssueTitle:         "legit retry",
		Status:             state.StatusDead,
		RetryCount:         1,
		NextRetryAt:        &pastTime,
		Branch:             "feat/mae-2-201-test",
		LastClosedPRNumber: 43,
	}

	o.respawnDueRetries(s, 10)

	if !respawned {
		t.Fatal("a retry whose issue is open and whose PR is unmerged must respawn")
	}
}

// A branch-only merge (operator merged the PR; maestro PRs carry no
// auto-closing keywords, so the issue stays open) must settle the session as
// code_landed, not dead: a dead session without NextRetryAt drops out of
// IssueInProgress, and HasMergedPRForIssue cannot see a merge that closed no
// issue — the next dispatch cycle would spawn a fresh worker for
// already-landed work (#800 review).
func TestReconcilePushedBranch_BranchAlreadyMerged_SettlesCodeLanded(t *testing.T) {
	created := false
	o := &Orchestrator{
		cfg:                  &config.Config{Repo: "owner/repo"},
		pidAliveFn:           func(pid int) bool { return false },
		tmuxSessionExistsFn:  func(name string) bool { return false },
		listOpenPRsFn:        func() ([]github.PR, error) { return []github.PR{}, nil },
		remoteBranchExistsFn: func(branch string) (bool, error) { return true, nil },
		mergedPRForBranchFn: func(branch string) (int, error) {
			if branch == "feat/ok-player-1-133-test" {
				return 135, nil
			}
			return 0, nil
		},
		isIssueClosedFn: func(issueNumber int) (bool, error) { return false, nil },
		createPRFn: func(title, body, base, head string) (int, error) {
			created = true
			return 137, nil
		},
	}

	s := state.NewState()
	s.Sessions["ok-player-1"] = &state.Session{
		IssueNumber: 133,
		IssueTitle:  "stale squash-merged branch",
		Status:      state.StatusRunning,
		PID:         4242,
		TmuxSession: "maestro-ok-player-1",
		Branch:      "feat/ok-player-1-133-test",
	}

	o.reconcileRunningSessions(s)

	if created {
		t.Fatal("reconcile must NOT auto-create a PR from a branch that already merged via an earlier PR")
	}
	sess := s.Sessions["ok-player-1"]
	if sess.Status != state.StatusCodeLanded {
		t.Fatalf("status = %q, want %q (settled, not dead — dead would re-open the zombie-respawn window)", sess.Status, state.StatusCodeLanded)
	}
	if sess.PRNumber != 135 {
		t.Fatalf("PRNumber = %d, want 135 (the merged PR for the branch)", sess.PRNumber)
	}
	if sess.PID != 0 || sess.TmuxSession != "" {
		t.Fatalf("PID = %d TmuxSession = %q, want cleared", sess.PID, sess.TmuxSession)
	}
	if !s.IssueInProgress(133) {
		t.Fatal("settled session must keep issue #133 in progress so dispatch cannot spawn a duplicate worker")
	}
}

func TestReconcilePushedBranch_IssueClosed_SettlesDone(t *testing.T) {
	created := false
	o := &Orchestrator{
		cfg:                  &config.Config{Repo: "owner/repo"},
		pidAliveFn:           func(pid int) bool { return false },
		tmuxSessionExistsFn:  func(name string) bool { return false },
		listOpenPRsFn:        func() ([]github.PR, error) { return []github.PR{}, nil },
		remoteBranchExistsFn: func(branch string) (bool, error) { return true, nil },
		mergedPRForBranchFn:  func(branch string) (int, error) { return 0, nil },
		isIssueClosedFn:      func(issueNumber int) (bool, error) { return issueNumber == 133, nil },
		createPRFn: func(title, body, base, head string) (int, error) {
			created = true
			return 137, nil
		},
	}

	s := state.NewState()
	s.Sessions["ok-player-1"] = &state.Session{
		IssueNumber: 133,
		IssueTitle:  "issue closed before rescue",
		Status:      state.StatusRunning,
		PID:         4242,
		TmuxSession: "maestro-ok-player-1",
		Branch:      "feat/ok-player-1-133-test",
	}

	o.reconcileRunningSessions(s)

	if created {
		t.Fatal("reconcile must NOT auto-create a PR for a closed issue")
	}
	sess := s.Sessions["ok-player-1"]
	if sess.Status != state.StatusDone {
		t.Fatalf("status = %q, want %q", sess.Status, state.StatusDone)
	}
	if sess.FinishedAt == nil {
		t.Fatal("FinishedAt should be set when the session is settled")
	}
}

func TestReconcilePushedBranch_MergedCheckErrorFailsOpen(t *testing.T) {
	created := false
	o := &Orchestrator{
		cfg:                  &config.Config{Repo: "owner/repo"},
		pidAliveFn:           func(pid int) bool { return false },
		tmuxSessionExistsFn:  func(name string) bool { return false },
		listOpenPRsFn:        func() ([]github.PR, error) { return []github.PR{}, nil },
		remoteBranchExistsFn: func(branch string) (bool, error) { return true, nil },
		mergedPRForBranchFn:  func(branch string) (int, error) { return 0, fmt.Errorf("transient API error") },
		isIssueClosedFn:      func(issueNumber int) (bool, error) { return false, nil },
		createPRFn: func(title, body, base, head string) (int, error) {
			created = true
			return 137, nil
		},
	}

	s := state.NewState()
	s.Sessions["ok-player-1"] = &state.Session{
		IssueNumber: 133,
		IssueTitle:  "rescue survives GitHub hiccup",
		Status:      state.StatusRunning,
		PID:         4242,
		TmuxSession: "maestro-ok-player-1",
		Branch:      "feat/ok-player-1-133-test",
	}

	o.reconcileRunningSessions(s)

	if !created {
		t.Fatal("a transient GitHub read error on the merged-branch check must not disable the auto-create rescue")
	}
	sess := s.Sessions["ok-player-1"]
	if sess.Status != state.StatusPROpen {
		t.Fatalf("status = %q, want %q", sess.Status, state.StatusPROpen)
	}
}
