package orchestrator

import (
	"os/exec"
	"testing"
	"time"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/github"
	"github.com/befeast/maestro/internal/notify"
	"github.com/befeast/maestro/internal/state"
	"github.com/befeast/maestro/internal/worker"
)

// A process that disappears without a PR used to become Dead without a
// NextRetryAt. Because dead sessions are not material-progress watchdog
// targets, the session then waited for a later supervisor repair cycle and the
// live OK Player incident exceeded the ten-minute recovery SLA. The 60-second
// orchestrator pass must reserve and execute one bounded retry in the same
// canonical slot/worktree instead.
func TestUnexpectedWorkerExitSchedulesImmediateCanonicalRetry(t *testing.T) {
	worktree := t.TempDir()
	if out, err := exec.Command("git", "init", "-b", "main", worktree).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}

	respawned := 0
	o := &Orchestrator{
		cfg: &config.Config{
			Repo:     "owner/repo",
			StateDir: t.TempDir(),
			// One allowed retry must still recover the first unexpected exit; the
			// current running attempt must not be counted twice.
			MaxRetriesPerIssue: 1,
		},
		repo:                 "owner/repo",
		notifier:             &notify.Notifier{},
		promptBase:           "test prompt",
		pidAliveFn:           func(int) bool { return false },
		tmuxSessionExistsFn:  func(string) bool { return false },
		listOpenPRsFn:        func() ([]github.PR, error) { return nil, nil },
		remoteBranchExistsFn: func(string) (bool, error) { return false, nil },
		isIssueClosedFn:      func(int) (bool, error) { return false, nil },
		getIssueFn:           func(number int) (github.Issue, error) { return makeIssue(number, "recover me"), nil },
		respawnInPlaceFn: func(_ *config.Config, slot string, sess *state.Session, _ string, issue github.Issue, _, _ string) error {
			respawned++
			if slot != "ok-player-302" || issue.Number != 406 {
				t.Fatalf("respawn identity = %s/#%d, want ok-player-302/#406", slot, issue.Number)
			}
			if sess.Worktree != worktree || sess.Branch != "feat/ok-player-302-406-recover" {
				t.Fatalf("canonical identity changed: worktree=%q branch=%q", sess.Worktree, sess.Branch)
			}
			sess.Status = state.StatusRunning
			sess.PID = 5555
			sess.TmuxSession = "maestro-ok-player-302"
			return nil
		},
	}

	s := state.NewState()
	s.Sessions["ok-player-302"] = &state.Session{
		IssueNumber: 406,
		IssueTitle:  "recover me",
		Status:      state.StatusRunning,
		PID:         4242,
		TmuxSession: "maestro-ok-player-302",
		Branch:      "feat/ok-player-302-406-recover",
		Worktree:    worktree,
		Backend:     "sol",
	}

	if !o.reconcileRunningSessions(s) {
		t.Fatal("dead process was not reconciled")
	}
	sess := s.Sessions["ok-player-302"]
	if sess.Status != state.StatusDead || sess.NextRetryAt == nil {
		t.Fatalf("reconciled state = %q retry=%v, want dead with immediate retry", sess.Status, sess.NextRetryAt)
	}
	if sess.RetryCount != 1 || sess.RetryReason != state.RetryReasonStalledProgress {
		t.Fatalf("retry metadata = count %d reason %q", sess.RetryCount, sess.RetryReason)
	}
	if sess.UnexpectedExitRetries != 1 {
		t.Fatalf("unexpected-exit retries = %d, want 1", sess.UnexpectedExitRetries)
	}
	if sess.NextRetryAt.After(time.Now().UTC()) {
		t.Fatalf("retry scheduled in the future: %s", sess.NextRetryAt)
	}
	if !s.IssueInProgress(406) {
		t.Fatal("scheduled canonical retry must retain the issue dispatch lease")
	}

	// This is Step 2b of the same orchestrator cycle.
	o.respawnDueRetries(s, 1)
	if respawned != 1 {
		t.Fatalf("in-place respawn count = %d, want 1", respawned)
	}
	if sess.Status != state.StatusRunning || sess.PID != 5555 {
		t.Fatalf("resumed state = status %q pid %d", sess.Status, sess.PID)
	}
}

func TestUnexpectedWorkerExitAtTokenBudgetTerminalizesWithoutRetry(t *testing.T) {
	worktree := t.TempDir()
	if out, err := exec.Command("git", "init", "-b", "main", worktree).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}

	o := &Orchestrator{
		cfg: &config.Config{
			Repo:               "owner/repo",
			WorkerMaxTokens:    100,
			MaxRetriesPerIssue: 0, // unlimited must not override a deterministic budget stop
		},
		repo:                 "owner/repo",
		notifier:             &notify.Notifier{},
		pidAliveFn:           func(int) bool { return false },
		tmuxSessionExistsFn:  func(string) bool { return false },
		listOpenPRsFn:        func() ([]github.PR, error) { return nil, nil },
		remoteBranchExistsFn: func(string) (bool, error) { return false, nil },
		isIssueClosedFn:      func(int) (bool, error) { return false, nil },
	}

	s := state.NewState()
	s.Sessions["ok-player-302"] = &state.Session{
		IssueNumber:       406,
		IssueTitle:        "over budget before marker reconciliation",
		Status:            state.StatusRunning,
		PID:               4242,
		TmuxSession:       "maestro-ok-player-302",
		Branch:            "feat/ok-player-302-406-over-budget",
		Worktree:          worktree,
		TokensUsedAttempt: 100,
	}

	if !o.reconcileRunningSessions(s) {
		t.Fatal("dead over-budget process was not reconciled")
	}
	sess := s.Sessions["ok-player-302"]
	if sess.Status != state.StatusFailed || sess.WorkerOutcome != worker.TokenBudgetExceededOutcome {
		t.Fatalf("terminal state = %q/%q, want failed/%q", sess.Status, sess.WorkerOutcome, worker.TokenBudgetExceededOutcome)
	}
	if sess.NextRetryAt != nil || sess.UnexpectedExitRetries != 0 {
		t.Fatalf("budget stop scheduled retry: next=%v unexpected=%d", sess.NextRetryAt, sess.UnexpectedExitRetries)
	}
}

func TestUnexpectedWorkerExitBelowUncachedBudgetStillGetsOneRecovery(t *testing.T) {
	worktree := t.TempDir()
	if out, err := exec.Command("git", "init", "-b", "main", worktree).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}

	o := &Orchestrator{
		cfg: &config.Config{
			Repo:               "owner/repo",
			StateDir:           t.TempDir(),
			WorkerMaxTokens:    160_000,
			MaxRetriesPerIssue: 1,
		},
		repo:                 "owner/repo",
		notifier:             &notify.Notifier{},
		pidAliveFn:           func(int) bool { return false },
		tmuxSessionExistsFn:  func(string) bool { return false },
		listOpenPRsFn:        func() ([]github.PR, error) { return nil, nil },
		remoteBranchExistsFn: func(string) (bool, error) { return false, nil },
		isIssueClosedFn:      func(int) (bool, error) { return false, nil },
	}

	s := state.NewState()
	s.Sessions["fin-26"] = &state.Session{
		IssueNumber:              409,
		IssueTitle:               "cache-heavy healthy attempt",
		Status:                   state.StatusRunning,
		PID:                      4242,
		TmuxSession:              "maestro-fin-26",
		Branch:                   "feat/fin-26-409-cache-heavy",
		Worktree:                 worktree,
		Backend:                  "claude",
		TokensUsedAttempt:        200_506,
		TokenBudgetTokensAttempt: 77_030,
		TokenBudgetMeasure:       worker.TokenBudgetMeasureUncached,
	}

	if !o.reconcileRunningSessions(s) {
		t.Fatal("cache-heavy dead process was not reconciled")
	}
	sess := s.Sessions["fin-26"]
	if sess.Status != state.StatusDead || sess.NextRetryAt == nil {
		t.Fatalf("reconciled state = %q retry=%v, want one canonical recovery", sess.Status, sess.NextRetryAt)
	}
	if sess.WorkerOutcome != "" || sess.UnexpectedExitRetries != 1 {
		t.Fatalf("cache-heavy exit outcome=%q unexpected=%d, want non-terminal first recovery", sess.WorkerOutcome, sess.UnexpectedExitRetries)
	}
}

func TestRepeatedUnexpectedWorkerExitTerminalizesWithUnlimitedRetries(t *testing.T) {
	worktree := t.TempDir()
	if out, err := exec.Command("git", "init", "-b", "main", worktree).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}

	respawned := 0
	o := &Orchestrator{
		cfg: &config.Config{
			Repo:               "owner/repo",
			MaxRetriesPerIssue: 0, // the zombie cap is independent of the general retry policy
		},
		repo:                 "owner/repo",
		notifier:             &notify.Notifier{},
		promptBase:           "test prompt",
		pidAliveFn:           func(int) bool { return false },
		tmuxSessionExistsFn:  func(string) bool { return false },
		listOpenPRsFn:        func() ([]github.PR, error) { return nil, nil },
		remoteBranchExistsFn: func(string) (bool, error) { return false, nil },
		isIssueClosedFn:      func(int) (bool, error) { return false, nil },
		getIssueFn:           func(number int) (github.Issue, error) { return makeIssue(number, "zombie loop"), nil },
		respawnInPlaceFn: func(_ *config.Config, _ string, sess *state.Session, _ string, _ github.Issue, _, _ string) error {
			respawned++
			sess.Status = state.StatusRunning
			sess.PID = 5555
			sess.TmuxSession = "maestro-ok-player-302"
			return nil
		},
	}

	s := state.NewState()
	sess := &state.Session{
		IssueNumber: 406,
		IssueTitle:  "zombie loop",
		Status:      state.StatusRunning,
		PID:         4242,
		TmuxSession: "maestro-ok-player-302",
		Branch:      "feat/ok-player-302-406-zombie",
		Worktree:    worktree,
		Backend:     "sol",
	}
	s.Sessions["ok-player-302"] = sess

	if !o.reconcileRunningSessions(s) {
		t.Fatal("first unexpected exit was not reconciled")
	}
	o.respawnDueRetries(s, 1)
	if respawned != 1 || sess.Status != state.StatusRunning {
		t.Fatalf("first recovery = respawned %d status %q, want one running replacement", respawned, sess.Status)
	}

	if !o.reconcileRunningSessions(s) {
		t.Fatal("replacement exit was not reconciled")
	}
	if sess.Status != state.StatusFailed || sess.WorkerOutcome != state.WorkerOutcomeRepeatedUnexpectedExit {
		t.Fatalf("replacement terminal state = %q/%q, want failed/%q", sess.Status, sess.WorkerOutcome, state.WorkerOutcomeRepeatedUnexpectedExit)
	}
	if sess.NextRetryAt != nil || sess.RetryCount != 1 || sess.UnexpectedExitRetries != 1 {
		t.Fatalf("replacement retry metadata = next %v count %d unexpected %d", sess.NextRetryAt, sess.RetryCount, sess.UnexpectedExitRetries)
	}

	o.respawnDueRetries(s, 1)
	if respawned != 1 {
		t.Fatalf("terminal zombie respawned %d times, want exactly one recovery", respawned)
	}
	claim, ok := s.IssueClaimFor(406)
	if !ok || claim.Kind != state.IssueClaimTerminalFailure {
		t.Fatalf("terminal zombie claim = %+v, %v", claim, ok)
	}
}

func TestRespawnDueRetries_OverBudgetTerminalizesWithoutAvailableSlot(t *testing.T) {
	respawned := false
	o := &Orchestrator{
		cfg:      &config.Config{Repo: "owner/repo", WorkerMaxTokens: 100, MaxRetriesPerIssue: 0},
		notifier: &notify.Notifier{},
		respawnWorkerFn: func(*config.Config, string, *state.Session, string, github.Issue, string, string) error {
			respawned = true
			return nil
		},
	}
	past := time.Now().UTC().Add(-time.Second)
	s := state.NewState()
	sess := &state.Session{
		IssueNumber:       407,
		IssueTitle:        "queued over-budget retry",
		Status:            state.StatusDead,
		NextRetryAt:       &past,
		TokensUsedAttempt: 125,
	}
	s.Sessions["ok-player-303"] = sess

	o.respawnDueRetries(s, 0)
	if respawned {
		t.Fatal("over-budget retry must not respawn")
	}
	if sess.Status != state.StatusFailed || sess.WorkerOutcome != worker.TokenBudgetExceededOutcome || sess.NextRetryAt != nil {
		t.Fatalf("terminal budget retry = status %q outcome %q next %v", sess.Status, sess.WorkerOutcome, sess.NextRetryAt)
	}
	if claim, ok := s.IssueClaimFor(407); !ok || claim.Kind != state.IssueClaimTerminalFailure {
		t.Fatalf("terminal budget claim = %+v, %v", claim, ok)
	}
}

func TestRespawnDueRetries_OperatorRestartBypassesOldAttemptBudgetLatch(t *testing.T) {
	respawned := false
	o := &Orchestrator{
		cfg:      &config.Config{Repo: "owner/repo", WorkerMaxTokens: 100, MaxRetriesPerIssue: 0},
		notifier: &notify.Notifier{},
		getIssueFn: func(number int) (github.Issue, error) {
			return makeIssue(number, "intentional restart"), nil
		},
		isIssueClosedFn: func(int) (bool, error) { return false, nil },
		respawnWorkerFn: func(*config.Config, string, *state.Session, string, github.Issue, string, string) error {
			respawned = true
			return nil
		},
	}
	past := time.Now().UTC().Add(-time.Second)
	s := state.NewState()
	sess := &state.Session{
		IssueNumber:       408,
		IssueTitle:        "intentional restart",
		Status:            state.StatusDead,
		NextRetryAt:       &past,
		RetryReason:       state.RetryReasonOperatorRestart,
		TokensUsedAttempt: 125, // stale accounting from the terminal old attempt
	}
	s.Sessions["ok-player-304"] = sess

	o.respawnDueRetries(s, 1)
	if !respawned {
		t.Fatal("approved operator restart was blocked by stale attempt accounting")
	}
	if sess.WorkerOutcome == worker.TokenBudgetExceededOutcome {
		t.Fatal("old attempt budget latch was restored during intentional restart")
	}
}

func TestDueRetryDefersWhenIssueGainsExcludedLabel(t *testing.T) {
	respawned := false
	past := time.Now().UTC().Add(-time.Second)
	o := &Orchestrator{
		cfg:             &config.Config{Repo: "owner/repo", ExcludeLabels: []string{"blocked"}},
		repo:            "owner/repo",
		notifier:        &notify.Notifier{},
		promptBase:      "test prompt",
		isIssueClosedFn: func(int) (bool, error) { return false, nil },
		getIssueFn: func(number int) (github.Issue, error) {
			return makeIssue(number, "blocked after exit", "blocked"), nil
		},
		respawnWorkerFn: func(*config.Config, string, *state.Session, string, github.Issue, string, string) error {
			respawned = true
			return nil
		},
	}
	s := state.NewState()
	sess := &state.Session{
		IssueNumber: 407,
		IssueTitle:  "blocked after exit",
		Status:      state.StatusDead,
		RetryCount:  1,
		NextRetryAt: &past,
	}
	s.Sessions["ok-player-303"] = sess

	o.respawnDueRetries(s, 1)
	if respawned {
		t.Fatal("excluded issue must not consume delayed retry authority")
	}
	if sess.Status != state.StatusDead || sess.NextRetryAt == nil || !sess.NextRetryAt.After(time.Now().UTC()) {
		t.Fatalf("blocked retry state = status %q retry=%v, want preserved deferred retry", sess.Status, sess.NextRetryAt)
	}
	if sess.RetryHoldReason == "" {
		t.Fatal("blocked retry must persist its issue-guard hold reason")
	}
	if got := state.SessionDisplayStatusFor(sess, nil); got != string(state.DisplayWaitingForIssueGuard) {
		t.Fatalf("blocked retry display = %q, want %q", got, state.DisplayWaitingForIssueGuard)
	}
	if attention := state.SessionAttentionFor(sess, nil); attention.NeedsAttention {
		t.Fatalf("intentional issue-guard hold must not require attention: %+v", attention)
	}
	if !s.IssueInProgress(407) {
		t.Fatal("issue-guard hold must retain the canonical issue lease")
	}
}

func TestDueRetryResumesCanonicalSessionAfterExcludedLabelClears(t *testing.T) {
	blocked := true
	respawned := 0
	past := time.Now().UTC().Add(-time.Second)
	o := &Orchestrator{
		cfg:             &config.Config{Repo: "owner/repo", ExcludeLabels: []string{"blocked"}},
		repo:            "owner/repo",
		notifier:        &notify.Notifier{},
		promptBase:      "test prompt",
		isIssueClosedFn: func(int) (bool, error) { return false, nil },
		getIssueFn: func(number int) (github.Issue, error) {
			if blocked {
				return makeIssue(number, "guarded retry", "blocked"), nil
			}
			return makeIssue(number, "guarded retry"), nil
		},
		respawnInPlaceFn: func(_ *config.Config, slot string, sess *state.Session, _ string, issue github.Issue, _, _ string) error {
			respawned++
			if slot != "ok-player-302" || issue.Number != 406 || sess.Worktree != "/worktrees/ok-player-302" {
				t.Fatalf("canonical retry identity drifted: slot=%q issue=%d worktree=%q", slot, issue.Number, sess.Worktree)
			}
			sess.Status = state.StatusRunning
			sess.PID = 5555
			return nil
		},
	}
	s := state.NewState()
	sess := &state.Session{
		IssueNumber: 406,
		IssueTitle:  "guarded retry",
		Status:      state.StatusDead,
		RetryCount:  1,
		NextRetryAt: &past,
		Worktree:    "/worktrees/ok-player-302",
		Branch:      "feat/ok-player-302-406-recover",
		PRNumber:    388,
	}
	s.Sessions["ok-player-302"] = sess

	o.respawnDueRetries(s, 1)
	if respawned != 0 || sess.RetryHoldReason == "" {
		t.Fatalf("guarded cycle = respawned %d hold %q, want held canonical retry", respawned, sess.RetryHoldReason)
	}

	blocked = false
	sess.NextRetryAt = &past
	o.respawnDueRetries(s, 1)
	if respawned != 1 || sess.Status != state.StatusRunning || sess.PID != 5555 {
		t.Fatalf("cleared cycle = respawned %d status %q pid %d, want one in-place resume", respawned, sess.Status, sess.PID)
	}
	if sess.RetryHoldReason != "" {
		t.Fatalf("retry hold survived guard removal: %q", sess.RetryHoldReason)
	}
}
