package orchestrator

import (
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/github"
	"github.com/befeast/maestro/internal/notify"
	"github.com/befeast/maestro/internal/router"
	"github.com/befeast/maestro/internal/state"
	"github.com/befeast/maestro/internal/supervisor"
)

// #1238: an operator pause (#683) must hold back every new native launch, not
// only fresh issue selection. These tests pin each launch path that used to
// run while paused, plus the resume contract: nothing deferred is consumed,
// and the first cycle after `maestro resume` launches it exactly once.

func TestRespawnDueRetries_PausedDefersElapsedRetryUntilResume(t *testing.T) {
	cfg := &config.Config{Repo: "owner/repo", MaxRetryBackoffMs: 300000, MaxRuntimeMinutes: 999}
	respawns := 0
	o := &Orchestrator{
		cfg:             cfg,
		notifier:        &notify.Notifier{},
		promptBase:      "test prompt",
		isIssueClosedFn: func(int) (bool, error) { return false, nil },
		getIssueFn: func(number int) (github.Issue, error) {
			return makeIssue(number, "retry after CI failure"), nil
		},
		respawnWorkerFn: func(cfg *config.Config, slotName string, sess *state.Session, repo string, issue github.Issue, promptBase, backend string) error {
			respawns++
			if !strings.Contains(promptBase, "ci failure excerpt") {
				t.Fatalf("resumed retry lost its CI context; prompt = %q", promptBase)
			}
			sess.Status = state.StatusRunning
			sess.PID = 5555
			return nil
		},
	}

	due := time.Now().UTC().Add(-time.Second)
	s := state.NewState()
	s.Sessions["mae-12"] = &state.Session{
		IssueNumber:     112,
		IssueTitle:      "retry after CI failure",
		Status:          state.StatusDead,
		RetryCount:      1,
		NextRetryAt:     &due,
		Branch:          "feat/mae-12-112-test",
		CIFailureOutput: "ci failure excerpt",
	}
	s.SetPaused(time.Now().UTC())

	o.respawnDueRetries(s, 10)

	sess := s.Sessions["mae-12"]
	if respawns != 0 {
		t.Fatalf("respawned %d time(s) while paused, want 0", respawns)
	}
	if sess.Status != state.StatusDead || sess.NextRetryAt == nil || !sess.NextRetryAt.Equal(due) {
		t.Fatalf("paused retry = status %q next_retry_at %v, want dead and still due at %v", sess.Status, sess.NextRetryAt, due)
	}
	if sess.RetryCount != 1 || sess.CIFailureOutput != "ci failure excerpt" {
		t.Fatalf("paused retry consumed state: retry_count=%d ci_output=%q", sess.RetryCount, sess.CIFailureOutput)
	}
	if got := len(o.pauseDeferredLaunches); got != 1 {
		t.Fatalf("deferred launches = %d, want 1", got)
	}

	s.ClearPaused(time.Now().UTC())
	o.respawnDueRetries(s, 10)
	o.respawnDueRetries(s, 10)

	if respawns != 1 {
		t.Fatalf("respawns after resume = %d, want exactly 1", respawns)
	}
	if sess.Status != state.StatusRunning || sess.NextRetryAt != nil {
		t.Fatalf("resumed retry = status %q next_retry_at %v, want running with no pending retry", sess.Status, sess.NextRetryAt)
	}
}

// pausedRepairFixture is the #1238 incident shape: a retained pr_open session
// whose current-head CI fails and a deterministic supervisor recommendation
// (no approval required) to repair it in place.
func pausedRepairFixture(t *testing.T) (*Orchestrator, *state.State, *int) {
	t.Helper()
	cfg := cfgWithBackends("codex", "codex")
	issues := []github.Issue{makeIssue(517, "repair failing PR")}
	o, started, _ := newStartWorkersOrchestrator(cfg, issues)
	authorizeCurrentFailedRepairGate(o, 520)
	o.hasOpenPRForIssueFn = func(issueNumber int) (bool, error) { return issueNumber == 517, nil }
	respawns := 0
	o.respawnInPlaceFn = func(cfg *config.Config, slotName string, sess *state.Session, repo string, issue github.Issue, promptBase, backend string) error {
		respawns++
		if slotName != "rep-1" {
			t.Fatalf("respawned slot %q, want reserved session rep-1", slotName)
		}
		sess.Status = state.StatusRunning
		sess.PID = 5555
		return nil
	}
	t.Cleanup(func() {
		if len(*started) != 0 {
			t.Errorf("fresh starts = %v, want none for a same-session repair", *started)
		}
	})

	s := state.NewState()
	s.Sessions["rep-1"] = &state.Session{
		IssueNumber: 517,
		IssueTitle:  "repair failing PR",
		Status:      state.StatusPROpen,
		PRNumber:    520,
		Branch:      "feat/rep-1-517-repair",
		Worktree:    "/work/rep-1",
		Backend:     "codex",
	}
	return o, s, &respawns
}

func TestStartNewWorkers_PausedHoldsDeterministicRepairRecommendationUntilResume(t *testing.T) {
	o, s, respawns := pausedRepairFixture(t)
	s.RecordSupervisorDecision(state.SupervisorDecision{
		ID:                "sup-repair-517",
		CreatedAt:         time.Now().UTC(),
		RecommendedAction: supervisor.ActionSpawnRepairWorker,
		Risk:              supervisor.RiskMutating,
		RequiresApproval:  false,
		Target:            &state.SupervisorTarget{Issue: 517, PR: 520, Session: "rep-1"},
	}, state.DefaultSupervisorDecisionLimit)
	s.SetPaused(time.Now().UTC())

	o.startNewWorkers(s, 1)

	if *respawns != 0 {
		t.Fatalf("RespawnInPlace called %d time(s) while paused, want 0", *respawns)
	}
	if got := s.Sessions["rep-1"].Status; got != state.StatusPROpen {
		t.Fatalf("paused repair target status = %q, want pr_open untouched", got)
	}
	if d := s.LatestSupervisorDecision(); d == nil || d.Disposition != nil {
		t.Fatalf("paused recommendation disposition = %+v, want unmaterialized", d)
	}
	o.notePausedLaunchIntents(s, time.Now().UTC())
	if got := len(o.pauseDeferredLaunches); got != 1 {
		t.Fatalf("deferred launches = %d, want 1 (the selected repair)", got)
	}
	o.pauseDeferredLaunches = nil

	s.ClearPaused(time.Now().UTC())
	o.startNewWorkers(s, 1)
	o.startNewWorkers(s, 1)

	if *respawns != 1 {
		t.Fatalf("RespawnInPlace after resume = %d, want exactly 1", *respawns)
	}
	if d := s.LatestSupervisorDecision(); d == nil || d.Disposition == nil ||
		d.Disposition.Status != state.RecommendationDispositionMaterialized ||
		d.Disposition.Reason != state.RecommendationDispositionWorkerStarted {
		t.Fatalf("resumed recommendation disposition = %+v, want materialized/worker_started", d)
	}
}

func TestStartNewWorkers_PausedKeepsRepairApprovalAwaitingDispatchUntilResume(t *testing.T) {
	o, s, respawns := pausedRepairFixture(t)
	now := time.Now().UTC().Add(-time.Minute)
	s.Approvals = []state.Approval{repairApproval("repair-517", 517, 520, state.ApprovalStatusAwaitingDispatch, now)}
	s.Approvals[0].Target.Session = "rep-1"
	before := len(s.Approvals[0].Audit)
	s.SetPaused(time.Now().UTC())

	o.startNewWorkers(s, 1)

	if *respawns != 0 {
		t.Fatalf("RespawnInPlace called %d time(s) while paused, want 0", *respawns)
	}
	if got := approvalStatus(t, s, "repair-517"); got != state.ApprovalStatusAwaitingDispatch {
		t.Fatalf("paused approval = %q, want awaiting_dispatch", got)
	}
	if got := len(s.Approvals[0].Audit); got != before {
		t.Fatalf("paused approval audit grew %d -> %d, want untouched", before, got)
	}
	o.notePausedLaunchIntents(s, time.Now().UTC())
	if got := len(o.pauseDeferredLaunches); got != 1 {
		t.Fatalf("deferred launches = %d, want 1 (the approved repair)", got)
	}
	o.pauseDeferredLaunches = nil

	s.ClearPaused(time.Now().UTC())
	o.startNewWorkers(s, 1)
	o.startNewWorkers(s, 1)

	if *respawns != 1 {
		t.Fatalf("RespawnInPlace after resume = %d, want exactly 1", *respawns)
	}
	if got := approvalStatus(t, s, "repair-517"); got != state.ApprovalStatusSuperseded {
		t.Fatalf("resumed approval = %q, want consumed by the dispatch", got)
	}
}

// Emergency stop (#840) keeps its own precedence: it returns before the pause
// gate, so it neither dispatches the repair nor is reported as a pause
// deferral.
func TestStartNewWorkers_EmergencyStopStillPrecedesPause(t *testing.T) {
	o, s, respawns := pausedRepairFixture(t)
	s.RecordSupervisorDecision(state.SupervisorDecision{
		ID:                "sup-repair-517",
		CreatedAt:         time.Now().UTC(),
		RecommendedAction: supervisor.ActionSpawnRepairWorker,
		Risk:              supervisor.RiskMutating,
		Target:            &state.SupervisorTarget{Issue: 517, PR: 520, Session: "rep-1"},
	}, state.DefaultSupervisorDecisionLimit)
	s.SetPaused(time.Now().UTC())
	o.emergencyHaltFn = func() bool { return true }

	var logs strings.Builder
	previous := log.Writer()
	log.SetOutput(&logs)
	defer log.SetOutput(previous)

	o.startNewWorkers(s, 1)

	if *respawns != 0 || len(o.pauseDeferredLaunches) != 0 {
		t.Fatalf("respawns=%d deferred=%d, want emergency stop to return first", *respawns, len(o.pauseDeferredLaunches))
	}
	if !strings.Contains(logs.String(), "EMERGENCY STOP active") {
		t.Fatalf("logs = %q, want the emergency stop line", logs.String())
	}
}

// Failover respawns (provider limit, backend auth failure) replace a worker
// whose process already ended. While paused they park as a due, budget-neutral
// retry; after resume the retry queue respawns once on the healthy fallback.
func TestPausedFailoverRespawnParksAsDueRetryUntilResume(t *testing.T) {
	cases := []struct {
		name      string
		reconcile bool
		alive     bool
		rateLimit bool
		authFail  bool
		tmuxOut   string
	}{
		{name: "reconcile provider limit", reconcile: true, rateLimit: true},
		{name: "reconcile backend auth failure", reconcile: true, authFail: true},
		{name: "dead worker provider limit", rateLimit: true},
		{name: "dead worker backend auth failure", authFail: true},
		{name: "live worker provider limit", alive: true, tmuxOut: "rate limit exceeded. Try again at January 1, 2027 12:00 PM."},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.Config{
				Repo:              "owner/repo",
				MaxRuntimeMinutes: 999,
				Model: config.ModelConfig{
					Default:          "claude",
					FallbackBackends: []string{"codex"},
					Backends: map[string]config.BackendDef{
						"claude": {Cmd: "claude"},
						"codex":  {Cmd: "codex"},
					},
				},
			}
			o, _ := newCheckSessionsOrchestrator(cfg, tc.tmuxOut)
			o.router = router.New(cfg)
			o.promptBase = "test prompt"
			o.pidAliveFn = func(int) bool { return tc.alive }
			if tc.reconcile {
				o.tmuxSessionExistsFn = func(string) bool { return false }
			}
			o.isRateLimitedFn = func(string) bool { return tc.rateLimit }
			if tc.rateLimit {
				o.rateLimitResetFromLogFn = mockRateLimitReset
			}
			o.authFailureFromLogFn = func(string) (bool, string) {
				if tc.authFail {
					return true, "failed_to_authenticate"
				}
				return false, ""
			}
			o.getIssueFn = func(number int) (github.Issue, error) {
				return makeIssue(number, "failover target"), nil
			}
			var respawned []string
			o.respawnWorkerFn = func(cfg *config.Config, slotName string, sess *state.Session, repo string, issue github.Issue, promptBase, backend string) error {
				respawned = append(respawned, backend)
				sess.Status = state.StatusRunning
				sess.PID = 9001
				sess.Backend = backend
				sess.StartedAt = time.Now().UTC()
				sess.FinishedAt = nil
				return nil
			}

			s := state.NewState()
			s.Sessions["fo-1"] = &state.Session{
				IssueNumber: 301,
				IssueTitle:  "failover target",
				Status:      state.StatusRunning,
				PID:         987001,
				TmuxSession: "maestro-fo-1",
				Branch:      "feat/fo-1-301-failover",
				Backend:     "claude",
				StartedAt:   time.Now().UTC().Add(-2 * time.Minute),
				LogFile:     filepath.Join(t.TempDir(), "worker.log"),
			}
			s.SetPaused(time.Now().UTC())

			if tc.reconcile {
				if !o.reconcileRunningSessions(s) {
					t.Fatal("expected reconcile to report the parked session")
				}
			} else {
				o.checkSessions(s)
			}

			sess := s.Sessions["fo-1"]
			if len(respawned) != 0 {
				t.Fatalf("failover respawned on %v while paused, want none", respawned)
			}
			if sess.Status != state.StatusDead || sess.NextRetryAt == nil || sess.NextRetryAt.After(time.Now().UTC()) {
				t.Fatalf("paused failover = status %q next_retry_at %v, want dead with a due retry", sess.Status, sess.NextRetryAt)
			}
			if sess.RetryCount != 0 || sess.UnexpectedExitRetries != 0 {
				t.Fatalf("paused failover burned budget: retry_count=%d unexpected=%d", sess.RetryCount, sess.UnexpectedExitRetries)
			}
			if got := len(o.pauseDeferredLaunches); got != 1 {
				t.Fatalf("deferred launches = %d, want 1", got)
			}
			if _, gated := s.BackendHealth["claude"]; !gated {
				t.Fatal("the failed backend must still be gated while paused")
			}

			s.ClearPaused(time.Now().UTC())
			o.respawnDueRetries(s, 1)
			o.respawnDueRetries(s, 1)

			if len(respawned) != 1 || respawned[0] != "codex" {
				t.Fatalf("respawns after resume = %v, want exactly [codex]", respawned)
			}
		})
	}
}

func TestReconcile_PausedDefersRestartResumeUntilResume(t *testing.T) {
	resumeCount := 0
	o := restartResumeOrchestrator(t, &resumeCount)
	o.isIssueClosedFn = func(int) (bool, error) { return false, nil }
	o.workerStopFn = func(*config.Config, string, *state.Session) error {
		t.Fatal("a paused restart resume must not stop anything")
		return nil
	}

	worktree := t.TempDir()
	stamp := time.Now().UTC().Add(-30 * time.Second)
	s := state.NewState()
	s.Sessions["sup-320"] = &state.Session{
		IssueNumber:         320,
		IssueTitle:          "in-flight issue",
		Status:              state.StatusRunning,
		PID:                 987002,
		TmuxSession:         "maestro-sup-320",
		Branch:              "feat/sup-320-320-in-flight",
		Worktree:            worktree,
		Backend:             "claude",
		RestartCheckpointAt: &stamp,
	}
	s.SetPaused(time.Now().UTC())

	o.reconcileRunningSessions(s)
	o.checkSessions(s)
	o.respawnDueRetries(s, 1)

	sess := s.Sessions["sup-320"]
	if resumeCount != 0 {
		t.Fatalf("restart resume fired %d time(s) while paused, want 0", resumeCount)
	}
	if sess.RestartCheckpointAt == nil || sess.Status != state.StatusDead {
		t.Fatalf("paused restart = status %q marker %v, want dead with the marker preserved", sess.Status, sess.RestartCheckpointAt)
	}
	if sess.NextRetryAt != nil || sess.RetryCount != 0 || sess.UnexpectedExitRetries != 0 {
		t.Fatalf("paused restart became a budgeted retry: next=%v retry_count=%d unexpected=%d", sess.NextRetryAt, sess.RetryCount, sess.UnexpectedExitRetries)
	}

	s.ClearPaused(time.Now().UTC())
	o.reconcileRunningSessions(s)
	o.reconcileRunningSessions(s)

	if resumeCount != 1 {
		t.Fatalf("restart resume after unpause fired %d time(s), want exactly 1", resumeCount)
	}
	if sess.Status != state.StatusRunning || sess.RestartCheckpointAt != nil || sess.Worktree != worktree {
		t.Fatalf("resumed session = status %q marker %v worktree %q, want running in the same worktree", sess.Status, sess.RestartCheckpointAt, sess.Worktree)
	}
}

func TestCheckSessions_PausedLeavesSoftTokenWorkerRunning(t *testing.T) {
	softThreshold := 0.8
	cfg := &config.Config{
		Repo:                     "owner/repo",
		WorkerMaxTokens:          100000,
		WorkerSoftTokenThreshold: &softThreshold,
		MaxRuntimeMinutes:        999,
	}
	o, stopped := newCheckSessionsOrchestrator(cfg, "tokens 85000 (in 25000 / out 60000)")
	checkpoints := 0
	respawns := 0
	o.saveCheckpointFn = func(*state.Session) (string, error) {
		checkpoints++
		return "CHECKPOINT.md", nil
	}
	o.getIssueFn = func(number int) (github.Issue, error) { return makeIssue(number, "long task"), nil }
	o.respawnInPlaceFn = func(*config.Config, string, *state.Session, string, github.Issue, string, string) error {
		respawns++
		return nil
	}

	s := state.NewState()
	s.Sessions["mae-1"] = &state.Session{
		IssueNumber: 42,
		Status:      state.StatusRunning,
		PID:         987003,
		TmuxSession: "maestro-mae-1",
		Branch:      "feat/mae-1-42-test",
		StartedAt:   time.Now().UTC().Add(-30 * time.Minute),
		Backend:     "claude",
	}
	s.SetPaused(time.Now().UTC())

	o.checkSessions(s)

	sess := s.Sessions["mae-1"]
	if checkpoints != 0 || respawns != 0 || len(*stopped) != 0 {
		t.Fatalf("paused soft threshold: checkpoints=%d respawns=%d stopped=%v, want none", checkpoints, respawns, *stopped)
	}
	if sess.Status != state.StatusRunning || sess.CheckpointFile != "" {
		t.Fatalf("in-flight worker = status %q checkpoint %q, want running and untouched", sess.Status, sess.CheckpointFile)
	}
	if got := len(o.pauseDeferredLaunches); got != 1 {
		t.Fatalf("deferred launches = %d, want 1", got)
	}

	s.ClearPaused(time.Now().UTC())
	o.checkSessions(s)

	if checkpoints != 1 || respawns != 1 {
		t.Fatalf("after resume: checkpoints=%d respawns=%d, want 1 and 1", checkpoints, respawns)
	}
}

// One RunOnce while paused writes a single journal line for every deferred
// launch, launches nothing, and leaves the retry due and the recommendation
// unmaterialized on disk; the first cycle after resume launches both.
func TestRunOnce_PausedJournalsOneDeferralLinePerCycle(t *testing.T) {
	cfg := cfgWithBackends("codex", "codex")
	cfg.StateDir = t.TempDir()
	cfg.MaxParallel = 4
	cfg.MaxRuntimeMinutes = 999
	issues := []github.Issue{makeIssue(517, "repair failing PR"), makeIssue(518, "retry target")}
	o, started, _ := newStartWorkersOrchestrator(cfg, issues)
	o.repo = cfg.Repo
	o.listOpenPRsFn = func() ([]github.PR, error) {
		return []github.PR{{Number: 520, HeadRefName: "feat/rep-1-517-repair", State: "OPEN"}}, nil
	}
	o.pidAliveFn = func(int) bool { return false }
	o.tmuxSessionExistsFn = func(string) bool { return false }
	authorizeCurrentFailedRepairGate(o, 520)
	o.hasOpenPRForIssueFn = func(issueNumber int) (bool, error) { return issueNumber == 517, nil }
	repairs := 0
	o.respawnInPlaceFn = func(cfg *config.Config, slotName string, sess *state.Session, repo string, issue github.Issue, promptBase, backend string) error {
		repairs++
		sess.Status = state.StatusRunning
		sess.PID = 5555
		return nil
	}
	retries := 0
	o.respawnWorkerFn = func(cfg *config.Config, slotName string, sess *state.Session, repo string, issue github.Issue, promptBase, backend string) error {
		retries++
		sess.Status = state.StatusRunning
		sess.PID = 5556
		return nil
	}

	due := time.Now().UTC().Add(-time.Second)
	s := state.NewState()
	s.Sessions["rep-1"] = &state.Session{
		IssueNumber: 517,
		IssueTitle:  "repair failing PR",
		Status:      state.StatusPROpen,
		PRNumber:    520,
		Branch:      "feat/rep-1-517-repair",
		Worktree:    "/work/rep-1",
		Backend:     "codex",
		// The red head was already reported; the supervisor-selected repair,
		// not a fresh CI-failure retry, is the pending launch here.
		LastNotifiedStatus: "ci_failure",
	}
	s.Sessions["rep-2"] = &state.Session{
		IssueNumber: 518,
		IssueTitle:  "retry target",
		Status:      state.StatusDead,
		RetryCount:  1,
		NextRetryAt: &due,
		Branch:      "feat/rep-2-518-retry",
		Backend:     "codex",
	}
	s.RecordSupervisorDecision(state.SupervisorDecision{
		ID:                "sup-repair-517",
		CreatedAt:         time.Now().UTC(),
		RecommendedAction: supervisor.ActionSpawnRepairWorker,
		Risk:              supervisor.RiskMutating,
		Target:            &state.SupervisorTarget{Issue: 517, PR: 520, Session: "rep-1"},
	}, state.DefaultSupervisorDecisionLimit)
	s.SetPaused(time.Now().UTC())
	if err := state.Save(cfg.StateDir, s); err != nil {
		t.Fatalf("save paused state: %v", err)
	}

	var logs strings.Builder
	previous := log.Writer()
	log.SetOutput(&logs)
	defer log.SetOutput(previous)

	if err := o.RunOnce(); err != nil {
		t.Fatalf("paused RunOnce: %v", err)
	}

	if repairs != 0 || retries != 0 || len(*started) != 0 {
		t.Fatalf("paused cycle launched: repairs=%d retries=%d fresh=%v", repairs, retries, *started)
	}
	var pauseLines []string
	for _, line := range strings.Split(logs.String(), "\n") {
		if strings.Contains(line, "project paused") {
			pauseLines = append(pauseLines, line)
		}
	}
	if len(pauseLines) != 1 || !strings.Contains(pauseLines[0], "project paused: deferring 2 native launches") {
		t.Fatalf("pause journal lines = %q, want exactly one 'project paused: deferring 2 native launches'", pauseLines)
	}
	loaded, err := state.Load(cfg.StateDir)
	if err != nil {
		t.Fatalf("load paused state: %v", err)
	}
	if retry := loaded.Sessions["rep-2"]; retry.Status != state.StatusDead || retry.NextRetryAt == nil {
		t.Fatalf("persisted retry = status %q next %v, want still due", retry.Status, retry.NextRetryAt)
	}
	if d := loaded.LatestSupervisorDecision(); d == nil || d.Disposition != nil {
		t.Fatalf("persisted recommendation = %+v, want unmaterialized", d)
	}

	loaded.ClearPaused(time.Now().UTC())
	if err := state.Save(cfg.StateDir, loaded); err != nil {
		t.Fatalf("save resumed state: %v", err)
	}
	logs.Reset()
	if err := o.RunOnce(); err != nil {
		t.Fatalf("resumed RunOnce: %v", err)
	}

	if repairs != 1 || retries != 1 || len(*started) != 0 {
		t.Fatalf("after resume: repairs=%d retries=%d fresh=%v, want one repair and one retry\n%s", repairs, retries, *started, logs.String())
	}
	if strings.Contains(logs.String(), "project paused") {
		t.Fatalf("resumed cycles still journal a pause: %q", logs.String())
	}
}

// A pause routinely outlasts the 1h terminal worktree GC grace. A dead session
// whose retry or restart resume the pause is holding still owns its clean
// worktree: removing it would make the relaunch after resume fall back to
// worker.Respawn (branch deleted, fresh branch from the default branch)
// instead of the in-place relaunch on the existing branch.
func TestCheckSessions_PausedHeldRelaunchKeepsWorktreeBeyondCleanupGrace(t *testing.T) {
	cases := []struct {
		name          string
		restartMarker bool
	}{
		{name: "due retry"},
		{name: "restart resume marker", restartMarker: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			repo := filepath.Join(root, "repo")
			worktreeBase := filepath.Join(root, "worktrees")
			slot := "mae-7"
			branch := "feat/mae-7-707-repair"
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

			cfg := &config.Config{
				Repo:              "owner/repo",
				LocalPath:         repo,
				WorktreeBase:      worktreeBase,
				MaxRuntimeMinutes: 999,
			}
			o, _ := newCheckSessionsOrchestrator(cfg, "")
			o.repo = cfg.Repo
			o.promptBase = "test prompt"
			inPlaceLaunched := false
			// Only the relaunched worker is alive.
			o.pidAliveFn = func(pid int) bool { return pid == 5555 }
			o.tmuxSessionExistsFn = func(name string) bool { return inPlaceLaunched && name == "maestro-"+slot }
			o.isPRMergedFn = func(int) (bool, error) { return false, nil }
			o.getIssueFn = func(number int) (github.Issue, error) { return makeIssue(number, "held relaunch"), nil }
			removed := 0
			o.removeWorktreeFn = func(string, string) error {
				removed++
				return nil
			}
			inPlace := 0
			o.respawnInPlaceFn = func(_ *config.Config, gotSlot string, sess *state.Session, _ string, _ github.Issue, _ string, _ string) error {
				inPlace++
				if gotSlot != slot || sess.Worktree != worktree || sess.Branch != branch {
					t.Fatalf("in-place relaunch slot=%q worktree=%q branch=%q, want the retained %q/%q/%q", gotSlot, sess.Worktree, sess.Branch, slot, worktree, branch)
				}
				inPlaceLaunched = true
				sess.Status = state.StatusRunning
				sess.PID = 5555
				sess.TmuxSession = "maestro-" + slot
				sess.RestartCheckpointAt = nil
				sess.FinishedAt = nil
				return nil
			}
			fresh := 0
			o.respawnWorkerFn = func(*config.Config, string, *state.Session, string, github.Issue, string, string) error {
				fresh++
				return nil
			}

			finished := time.Now().UTC().Add(-2 * time.Hour)
			sess := &state.Session{
				IssueNumber: 707,
				IssueTitle:  "held relaunch",
				Status:      state.StatusDead,
				PRNumber:    708,
				Branch:      branch,
				Worktree:    worktree,
				Backend:     "claude",
				StartedAt:   finished.Add(-time.Hour),
				FinishedAt:  &finished,
				RetryCount:  1,
			}
			if tc.restartMarker {
				stamp := finished.Add(-time.Minute)
				sess.RestartCheckpointAt = &stamp
			} else {
				due := finished.Add(time.Minute)
				sess.NextRetryAt = &due
			}
			s := state.NewState()
			s.Sessions[slot] = sess
			s.SetPaused(finished.Add(-time.Hour))

			o.reconcileRunningSessions(s)
			o.checkSessions(s)
			o.respawnDueRetries(s, 1)

			if removed != 0 || sess.Worktree != worktree {
				t.Fatalf("paused GC removed=%d worktree=%q, want the held relaunch to keep %q", removed, sess.Worktree, worktree)
			}
			if _, err := os.Stat(worktree); err != nil {
				t.Fatalf("held relaunch worktree gone: %v", err)
			}
			if inPlace != 0 || fresh != 0 {
				t.Fatalf("paused cycle launched: in_place=%d fresh=%d", inPlace, fresh)
			}

			s.ClearPaused(time.Now().UTC())
			for i := 0; i < 2; i++ {
				o.reconcileRunningSessions(s)
				o.respawnDueRetries(s, 1)
			}

			if inPlace != 1 || fresh != 0 {
				t.Fatalf("after resume: in_place=%d fresh=%d, want exactly one in-place relaunch and no fresh respawn", inPlace, fresh)
			}
		})
	}
}

// The journal count is one per held launch intent. A failover parked as a due
// retry during reconcile is the same launch the retry queue defers later in
// the cycle, and a held repair approval is counted even when no slot is free
// (RunOnce skips startNewWorkers then).
func TestJournalPauseDeferrals_CountsEachHeldLaunchOnce(t *testing.T) {
	o, s, respawns := pausedRepairFixture(t)
	now := time.Now().UTC().Add(-time.Minute)
	s.Approvals = []state.Approval{repairApproval("repair-517", 517, 520, state.ApprovalStatusAwaitingDispatch, now)}
	s.Approvals[0].Target.Session = "rep-1"
	s.Sessions["fo-1"] = &state.Session{
		IssueNumber: 301,
		IssueTitle:  "failover target",
		Status:      state.StatusDead,
		Branch:      "feat/fo-1-301-failover",
		Backend:     "codex",
	}
	s.SetPaused(time.Now().UTC())

	// The reconcile failover gate parks fo-1; the retry queue then defers the
	// same now-due retry in the same cycle.
	o.pauseDefersNativeLaunch(s, "fo-1", 301)
	parkEndedWorkerForPause(s.Sessions["fo-1"], time.Now().UTC())
	o.respawnDueRetries(s, 1)

	var logs strings.Builder
	previous := log.Writer()
	log.SetOutput(&logs)
	defer log.SetOutput(previous)

	// No startNewWorkers call: a cycle with no free slot still counts the
	// held repair approval.
	o.journalPauseDeferrals(s)

	if *respawns != 0 {
		t.Fatalf("RespawnInPlace called %d time(s) while paused, want 0", *respawns)
	}
	if !strings.Contains(logs.String(), "project paused: deferring 2 native launches") {
		t.Fatalf("journal = %q, want 'project paused: deferring 2 native launches' (one failover retry, one repair)", logs.String())
	}
	if got := approvalStatus(t, s, "repair-517"); got != state.ApprovalStatusAwaitingDispatch {
		t.Fatalf("paused approval = %q, want awaiting_dispatch", got)
	}
	if o.pauseDeferredLaunches != nil {
		t.Fatalf("journal did not reset the per-cycle record: %v", o.pauseDeferredLaunches)
	}
}
