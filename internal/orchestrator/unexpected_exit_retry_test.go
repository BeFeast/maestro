package orchestrator

import (
	"os/exec"
	"testing"
	"time"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/github"
	"github.com/befeast/maestro/internal/notify"
	"github.com/befeast/maestro/internal/state"
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
}
