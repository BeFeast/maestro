package orchestrator

// #877: a self-deploy/operator restart must not turn an in-flight worker into a
// false running->dead transition that strands a dirty worktree and forces a
// manual resume. The daemon deliberately checkpoints each still-running session
// on shutdown (state.Session.RestartCheckpointAt); this reconcile must then
// resume the SAME logical session in place EXACTLY ONCE, preserving the
// worktree, and never dispatch a duplicate.

import (
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/github"
	"github.com/befeast/maestro/internal/notify"
	"github.com/befeast/maestro/internal/state"
)

// restartResumeOrchestrator builds an orchestrator whose worker is genuinely
// gone (dead pid, missing tmux) with no open PR / pushed branch — the exact
// production shape after KillMode reaps the worker cgroup on restart. The
// respawnInPlace stub records how many times it fired and mimics a real resume
// (fresh running pid + tmux), so a second reconcile sees a live worker.
func restartResumeOrchestrator(t *testing.T, resumeCount *int) *Orchestrator {
	t.Helper()
	const resumedPID = 5555
	const resumedTmux = "maestro-resumed"
	return &Orchestrator{
		cfg:      &config.Config{Repo: "owner/repo", StateDir: t.TempDir()},
		repo:     "owner/repo",
		notifier: &notify.Notifier{},
		// No PR, no pushed branch — the worker was mid-flight when the restart hit.
		listOpenPRsFn:        func() ([]github.PR, error) { return nil, nil },
		remoteBranchExistsFn: func(string) (bool, error) { return false, nil },
		// A resumed worker (pid 5555 / tmux "maestro-resumed") is alive; the
		// original pid is dead and its tmux is gone.
		pidAliveFn:          func(pid int) bool { return pid == resumedPID },
		tmuxSessionExistsFn: func(name string) bool { return name == resumedTmux },
		getIssueFn:          func(number int) (github.Issue, error) { return makeIssue(number, "in-flight issue"), nil },
		respawnInPlaceFn: func(cfg *config.Config, slotName string, sess *state.Session, repo string, issue github.Issue, promptBase string, backend string) error {
			*resumeCount++
			// Preserve identity; only runtime fields change, exactly as
			// worker.RespawnInPlace does.
			sess.Status = state.StatusRunning
			sess.PID = resumedPID
			sess.TmuxSession = resumedTmux
			sess.RestartCheckpointAt = nil
			return nil
		},
	}
}

func TestReconcile_RestartCheckpoint_ResumesInPlaceExactlyOnce(t *testing.T) {
	resumeCount := 0
	o := restartResumeOrchestrator(t, &resumeCount)

	worktree := t.TempDir() // dirty worktree survived the restart
	stamp := time.Now().UTC().Add(-30 * time.Second)
	s := state.NewState()
	s.Sessions["sup-310"] = &state.Session{
		IssueNumber:         310,
		IssueTitle:          "in-flight issue",
		Status:              state.StatusRunning,
		PID:                 4242, // dead after the restart
		TmuxSession:         "maestro-sup-310",
		Branch:              "feat/sup-310-310-in-flight",
		Worktree:            worktree,
		Backend:             "claude",
		RestartCheckpointAt: &stamp,
	}

	// First reconcile after the restart: resume in place.
	if !o.reconcileRunningSessions(s) {
		t.Fatal("expected reconcile to report a change (in-place resume)")
	}
	sess := s.Sessions["sup-310"]
	if resumeCount != 1 {
		t.Fatalf("respawnInPlace fired %d times, want exactly 1", resumeCount)
	}
	if sess.Status != state.StatusRunning {
		t.Fatalf("status = %q, want %q — a checkpointed session must resume, never go dead", sess.Status, state.StatusRunning)
	}
	if sess.RestartCheckpointAt != nil {
		t.Fatal("RestartCheckpointAt must be consumed on resume (exactly-once marker)")
	}
	// Same logical session: identity fields untouched, worktree preserved.
	if sess.IssueNumber != 310 || sess.Branch != "feat/sup-310-310-in-flight" || sess.Worktree != worktree {
		t.Fatalf("session identity changed on resume: issue=%d branch=%q worktree=%q", sess.IssueNumber, sess.Branch, sess.Worktree)
	}

	// Second reconcile: the resumed worker is alive (pid 5555 / tmux present) and
	// carries no marker, so nothing fires — no duplicate dispatch.
	o.reconcileRunningSessions(s)
	if resumeCount != 1 {
		t.Fatalf("respawnInPlace fired %d times across two reconciles, want exactly 1 (no duplicate execution)", resumeCount)
	}
	if s.Sessions["sup-310"].Status != state.StatusRunning {
		t.Fatalf("status after second reconcile = %q, want running", s.Sessions["sup-310"].Status)
	}
}

// #967: a worker can exit after drain begins but before the daemon shutdown
// checkpoint pass. The flow that observes the exit must persist a marker on
// the dead session, hold it while the old daemon is draining, and let only the
// replacement daemon resume the same slot/worktree.
func TestReconcile_DrainDeathResumesDeadCheckpointAfterRestart(t *testing.T) {
	resumeCount := 0
	o := restartResumeOrchestrator(t, &resumeCount)

	stateDir := t.TempDir()
	worktree := t.TempDir()
	s := state.NewState()
	s.SetShutdownDrain(time.Now().UTC().Add(-time.Minute))
	s.Sessions["sup-313"] = &state.Session{
		IssueNumber: 313,
		IssueTitle:  "dies during drain",
		Status:      state.StatusRunning,
		PID:         4245,
		TmuxSession: "maestro-sup-313",
		Branch:      "feat/sup-313-313-drain",
		Worktree:    worktree,
		Backend:     "claude",
	}

	// Old daemon observes the worker exit while drain is active. It records a
	// durable dead checkpoint but must not respawn during this process.
	if !o.reconcileRunningSessions(s) {
		t.Fatal("expected drain-time death to change session state")
	}
	sess := s.Sessions["sup-313"]
	if sess.Status != state.StatusDead || sess.RestartCheckpointAt == nil {
		t.Fatalf("drain-time state = status %q marker %v, want dead with restart marker", sess.Status, sess.RestartCheckpointAt)
	}
	if sess.NextRetryAt != nil {
		t.Fatalf("drain-time death must wait for replacement-daemon resume, got ordinary retry %v", sess.NextRetryAt)
	}
	if resumeCount != 0 {
		t.Fatalf("old draining daemon resumed %d workers, want 0", resumeCount)
	}
	if err := state.Save(stateDir, s); err != nil {
		t.Fatalf("persist drain-time death: %v", err)
	}

	// Cross the actual daemon restart boundary. The dead marker and canonical
	// slot identity must survive persistence, and the retained-worktree claim
	// must suppress a fresh ready-queue dispatch before restart reconcile runs.
	post, err := state.Load(stateDir)
	if err != nil {
		t.Fatalf("reload drain-time death: %v", err)
	}
	sess = post.Sessions["sup-313"]
	if sess.Status != state.StatusDead || sess.RestartCheckpointAt == nil {
		t.Fatalf("persisted drain-time state = status %q marker %v, want dead with restart marker", sess.Status, sess.RestartCheckpointAt)
	}
	if sess.IssueNumber != 313 || sess.Branch != "feat/sup-313-313-drain" || sess.Worktree != worktree {
		t.Fatalf("persisted session identity changed: issue=%d branch=%q worktree=%q", sess.IssueNumber, sess.Branch, sess.Worktree)
	}
	if !post.IssueInProgress(313) {
		t.Fatal("persisted retained worktree must keep the issue claimed against duplicate dispatch")
	}

	o.reconcileRunningSessions(post)
	if resumeCount != 0 {
		t.Fatalf("old draining daemon replayed dead checkpoint %d times, want 0", resumeCount)
	}

	// New daemon clears the one-shot drain flag before its first cycle and
	// consumes the dead marker exactly once on the same logical session. Even if
	// fresh dispatch were evaluated first, the retained claim must win.
	post.ClearSpawnDrain(time.Now().UTC())
	freshDispatches := 0
	o.listOpenIssuesFn = func([]string) ([]github.Issue, error) {
		return []github.Issue{makeIssue(313, "dies during drain")}, nil
	}
	o.workerStartFn = func(*config.Config, *state.State, string, github.Issue, string, string) (string, error) {
		freshDispatches++
		return "duplicate", nil
	}
	o.startNewWorkers(post, 1)
	if freshDispatches != 0 {
		t.Fatalf("fresh dispatch count = %d, want 0 while canonical dead checkpoint owns issue", freshDispatches)
	}
	if !o.reconcileRunningSessions(post) {
		t.Fatal("replacement daemon did not resume dead checkpoint")
	}
	if resumeCount != 1 {
		t.Fatalf("replacement daemon resume count = %d, want 1", resumeCount)
	}
	if sess.Status != state.StatusRunning || sess.RestartCheckpointAt != nil {
		t.Fatalf("resumed state = status %q marker %v, want running with consumed marker", sess.Status, sess.RestartCheckpointAt)
	}
	if sess.IssueNumber != 313 || sess.Branch != "feat/sup-313-313-drain" || sess.Worktree != worktree {
		t.Fatalf("session identity changed: issue=%d branch=%q worktree=%q", sess.IssueNumber, sess.Branch, sess.Worktree)
	}

	// Persist marker consumption too. A later daemon/cycle sees the live resumed
	// runtime and cannot replay the checkpoint or dispatch another worker.
	if err := state.Save(stateDir, post); err != nil {
		t.Fatalf("persist resumed session: %v", err)
	}
	next, err := state.Load(stateDir)
	if err != nil {
		t.Fatalf("reload resumed session: %v", err)
	}
	o.reconcileRunningSessions(next)
	if resumeCount != 1 {
		t.Fatalf("dead checkpoint resumed %d times, want exactly once", resumeCount)
	}
}

// A terminal failure recorded during the drain window is not proof that
// shutdown caused it. In particular, a dead session with an ordinary retry
// remains governed by its backoff; recovery must never infer restart intent
// later from FinishedAt relative to SpawnDrainAt.
func TestCheckpointDrainDeathForRestart_DoesNotInferOrdinaryTerminal(t *testing.T) {
	worktree := t.TempDir()
	drainStart := time.Now().UTC().Add(-time.Minute)
	finished := drainStart.Add(30 * time.Second)
	retryAt := finished.Add(5 * time.Minute)

	s := state.NewState()
	s.SetSpawnDrain(drainStart)
	sess := &state.Session{
		IssueNumber: 313,
		Status:      state.StatusDead,
		Worktree:    worktree,
		FinishedAt:  &finished,
		NextRetryAt: &retryAt,
	}

	if checkpointDrainDeathForRestart(s, sess, finished) {
		t.Fatal("already-terminal failure must not be inferred to be restart-resumable")
	}
	if sess.RestartCheckpointAt != nil {
		t.Fatalf("ordinary terminal failure received restart marker %v", sess.RestartCheckpointAt)
	}
	if sess.NextRetryAt == nil || !sess.NextRetryAt.Equal(retryAt) {
		t.Fatalf("ordinary retry policy changed: next_retry_at=%v, want %v", sess.NextRetryAt, retryAt)
	}
}

// A standalone operator drain only stops fresh dispatch; it does not prove an
// unrelated process loss was caused by a daemon restart. The ordinary crash
// must therefore remain dead and must not be replayed after a later restart.
func TestReconcile_GenericDrainCrashDoesNotBecomeRestartCheckpoint(t *testing.T) {
	resumeCount := 0
	o := restartResumeOrchestrator(t, &resumeCount)
	o.cfg.MaxRetriesPerIssue = 1
	stateDir := t.TempDir()
	worktree := t.TempDir()

	s := state.NewState()
	s.SetSpawnDrain(time.Now().UTC().Add(-time.Minute))
	// Exhaust the ordinary retry budget so this process loss is terminal. The
	// shutdown-resume marker must not silently override that policy.
	s.Sessions["sup-776"] = &state.Session{
		IssueNumber: 777,
		Status:      state.StatusDead,
	}
	s.Sessions["sup-777"] = &state.Session{
		IssueNumber: 777,
		IssueTitle:  "ordinary crash during operator drain",
		Status:      state.StatusRunning,
		PID:         424242,
		TmuxSession: "sup-777",
		Branch:      "feat/sup-777-ordinary-crash",
		Worktree:    worktree,
	}

	if !o.reconcileRunningSessions(s) {
		t.Fatal("expected ordinary process loss to be reconciled")
	}
	sess := s.Sessions["sup-777"]
	if sess.Status != state.StatusDead {
		t.Fatalf("status = %q, want dead", sess.Status)
	}
	if sess.RestartCheckpointAt != nil {
		t.Fatalf("generic drain crash received restart marker %v", sess.RestartCheckpointAt)
	}
	if resumeCount != 0 {
		t.Fatalf("old daemon resumed %d workers, want 0", resumeCount)
	}
	if sess.NextRetryAt != nil {
		t.Fatalf("retry-exhausted crash unexpectedly scheduled retry %v", sess.NextRetryAt)
	}
	if err := state.Save(stateDir, s); err != nil {
		t.Fatalf("persist ordinary terminal crash: %v", err)
	}

	post, err := state.Load(stateDir)
	if err != nil {
		t.Fatalf("reload ordinary terminal crash: %v", err)
	}
	sess = post.Sessions["sup-777"]
	if sess.Status != state.StatusDead || sess.RestartCheckpointAt != nil || sess.NextRetryAt != nil {
		t.Fatalf("persisted ordinary crash = status %q marker %v retry %v, want terminal dead without restart policy", sess.Status, sess.RestartCheckpointAt, sess.NextRetryAt)
	}

	post.ClearSpawnDrain(time.Now().UTC())
	o.reconcileRunningSessions(post)
	if resumeCount != 0 {
		t.Fatalf("ordinary crash resumed after restart %d times, want 0", resumeCount)
	}
}

// TestReconcile_RestartCheckpoint_SurvivesRestartBoundary is the end-to-end
// reproduction the #877 review asked for: an in-flight worker checkpointed at
// shutdown, persisted to the state file, and read back by the NEXT daemon
// process must resume the SAME logical session in place exactly once — no false
// running->dead, no duplicate execution. The Save/Load models the process
// restart boundary the marker has to survive.
func TestReconcile_RestartCheckpoint_SurvivesRestartBoundary(t *testing.T) {
	resumeCount := 0
	o := restartResumeOrchestrator(t, &resumeCount)

	stateDir := t.TempDir()
	worktree := t.TempDir()
	stamp := time.Now().UTC().Add(-time.Minute)

	// Old daemon: in-flight worker stamped for restart-resume, then process exits.
	pre := state.NewState()
	pre.Sessions["sup-310"] = &state.Session{
		IssueNumber:         310,
		IssueTitle:          "in-flight issue",
		Status:              state.StatusRunning,
		PID:                 4242,
		TmuxSession:         "maestro-sup-310",
		Branch:              "feat/sup-310-310-in-flight",
		Worktree:            worktree,
		Backend:             "claude",
		RestartCheckpointAt: &stamp,
	}
	if err := state.Save(stateDir, pre); err != nil {
		t.Fatalf("save: %v", err)
	}

	// New daemon: read the persisted state and run its first reconcile cycle.
	post, err := state.Load(stateDir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if post.Sessions["sup-310"].RestartCheckpointAt == nil {
		t.Fatal("marker did not survive the restart boundary (save/load)")
	}

	o.reconcileRunningSessions(post)
	if resumeCount != 1 {
		t.Fatalf("resume fired %d times across the restart, want exactly 1", resumeCount)
	}
	sess := post.Sessions["sup-310"]
	if sess.Status == state.StatusDead {
		t.Fatal("session was falsely marked dead across the restart")
	}
	if sess.IssueNumber != 310 || sess.Branch != "feat/sup-310-310-in-flight" || sess.Worktree != worktree {
		t.Fatalf("logical identity changed across the restart: issue=%d branch=%q worktree=%q", sess.IssueNumber, sess.Branch, sess.Worktree)
	}

	// A subsequent reconcile in the same process must not re-dispatch it.
	o.reconcileRunningSessions(post)
	if resumeCount != 1 {
		t.Fatalf("resume fired %d times, want exactly 1 (no duplicate execution)", resumeCount)
	}
}

// Without the deliberate restart checkpoint marker, a genuinely dead running
// worker (no PR, no pushed branch) still follows the existing terminal path and
// is NOT resumed in place — the #877 change is scoped strictly to restart-
// interrupted sessions and does not auto-resume every worker crash.
func TestReconcile_NoRestartCheckpoint_DeadWorkerStillGoesDead(t *testing.T) {
	resumeCount := 0
	o := restartResumeOrchestrator(t, &resumeCount)

	s := state.NewState()
	s.Sessions["sup-311"] = &state.Session{
		IssueNumber: 311,
		IssueTitle:  "crashed worker",
		Status:      state.StatusRunning,
		PID:         4243,
		TmuxSession: "maestro-sup-311",
		Branch:      "feat/sup-311-311-crash",
		Worktree:    t.TempDir(),
		Backend:     "claude",
		// RestartCheckpointAt intentionally nil.
	}

	if !o.reconcileRunningSessions(s) {
		t.Fatal("expected reconcile to settle the dead worker")
	}
	if resumeCount != 0 {
		t.Fatalf("respawnInPlace fired %d times, want 0 — only restart-checkpointed sessions resume in place", resumeCount)
	}
	if got := s.Sessions["sup-311"].Status; got != state.StatusDead {
		t.Fatalf("status = %q, want %q (unmarked dead worker keeps existing behavior)", got, state.StatusDead)
	}
}

// #877 review comment 3: if a prior restart-resume's new runtime identity did
// not fully persist (e.g. the post-resume state save failed, so the next daemon
// reloads the old marker + dead pid and re-enters this branch), the replacement
// worker started by that first resume is ALREADY running under the slot's
// deterministic tmux session. Re-entry must ADOPT it — refresh the recorded pid
// from the live pane and consume the marker — never call RespawnInPlace, which
// would kill that live replacement and lose the work it did since it started.
func TestReconcile_RestartCheckpoint_AdoptsLiveReplacement(t *testing.T) {
	resumeCount := 0
	o := restartResumeOrchestrator(t, &resumeCount)

	const adoptedPID = 7777
	const slotTmux = "maestro-sup-310"
	// The slot's own tmux session is alive (a replacement from a prior resume),
	// and its pane pid is the live worker. The old recorded pid is dead.
	o.tmuxSessionExistsFn = func(name string) bool { return name == slotTmux }
	o.pidAliveFn = func(pid int) bool { return pid == adoptedPID }
	worktree := t.TempDir()
	o.tmuxPaneIdentityFn = func(session string) (int, string, error) {
		if session == slotTmux {
			return adoptedPID, worktree, nil
		}
		return 0, "", fmt.Errorf("no such session %q", session)
	}

	stamp := time.Now().UTC().Add(-30 * time.Second)
	s := state.NewState()
	s.Sessions["sup-310"] = &state.Session{
		IssueNumber:         310,
		IssueTitle:          "in-flight issue",
		Status:              state.StatusRunning,
		PID:                 4242, // dead after the restart
		TmuxSession:         slotTmux,
		Branch:              "feat/sup-310-310-in-flight",
		Worktree:            worktree,
		Backend:             "claude",
		RestartCheckpointAt: &stamp,
	}

	if !o.reconcileRunningSessions(s) {
		t.Fatal("expected reconcile to report a change (adoption)")
	}
	sess := s.Sessions["sup-310"]
	if resumeCount != 0 {
		t.Fatalf("respawnInPlace fired %d times, want 0 — a live replacement must be adopted, never respawned over (that would kill it)", resumeCount)
	}
	if sess.PID != adoptedPID {
		t.Fatalf("pid = %d, want the live pane pid %d (adopted, so subsequent reconciles see it alive)", sess.PID, adoptedPID)
	}
	if sess.RestartCheckpointAt != nil {
		t.Fatal("marker must be consumed on a successful adoption")
	}
	if sess.Status != state.StatusRunning {
		t.Fatalf("status = %q, want running — an adopted live replacement must never go dead", sess.Status)
	}

	// A subsequent reconcile sees a live pid + live tmux and no marker → nothing
	// fires: no respawn, no duplicate, no false dead.
	o.reconcileRunningSessions(s)
	if resumeCount != 0 {
		t.Fatalf("respawnInPlace fired %d times across two reconciles, want 0", resumeCount)
	}
	if s.Sessions["sup-310"].Status != state.StatusRunning {
		t.Fatalf("status after second reconcile = %q, want running", s.Sessions["sup-310"].Status)
	}
}

// #966: workers launched in the isolated worker scope survive maestro.service
// itself. The replacement daemon must consume the restart marker by adopting the
// same PID/worktree, without respawning or starting a duplicate attempt.
func TestReconcile_RestartCheckpoint_ConsumesMarkerForSameSurvivingWorker(t *testing.T) {
	resumeCount := 0
	o := restartResumeOrchestrator(t, &resumeCount)

	const survivingPID = 7888
	const slotTmux = "maestro-sup-313"
	worktree := t.TempDir()
	o.pidAliveFn = func(pid int) bool { return pid == survivingPID }
	o.tmuxSessionExistsFn = func(name string) bool { return name == slotTmux }
	o.tmuxPaneIdentityFn = func(session string) (int, string, error) {
		return survivingPID, worktree, nil
	}

	stamp := time.Now().UTC()
	s := state.NewState()
	s.Sessions["sup-313"] = &state.Session{
		IssueNumber:         313,
		IssueTitle:          "surviving worker",
		Status:              state.StatusRunning,
		PID:                 survivingPID,
		TmuxSession:         slotTmux,
		Worktree:            worktree,
		RestartCheckpointAt: &stamp,
	}

	if !o.reconcileRunningSessions(s) {
		t.Fatal("expected surviving worker marker adoption to report a state change")
	}
	sess := s.Sessions["sup-313"]
	if resumeCount != 0 {
		t.Fatalf("respawnInPlace fired %d times, want 0 for the same surviving worker", resumeCount)
	}
	if sess.PID != survivingPID || sess.Worktree != worktree || sess.Status != state.StatusRunning {
		t.Fatalf("surviving runtime changed: status=%q pid=%d worktree=%q", sess.Status, sess.PID, sess.Worktree)
	}
	if sess.RestartCheckpointAt != nil {
		t.Fatal("restart marker was not consumed after exact surviving-worker adoption")
	}

	if o.reconcileRunningSessions(s) {
		t.Fatal("second reconcile changed an already-adopted live worker")
	}
	if resumeCount != 0 {
		t.Fatalf("respawnInPlace fired %d times across two reconciles, want 0", resumeCount)
	}
}

// If a replacement's tmux is alive but its live pane pid can't be read, the
// marker must be PRESERVED (not consumed) so the next cycle retries the
// non-destructive adoption — the session must never be respawned over or falsely
// marked dead while a live replacement is running.
func TestReconcile_RestartCheckpoint_LiveReplacementUnreadablePID_RetriesNextCycle(t *testing.T) {
	resumeCount := 0
	o := restartResumeOrchestrator(t, &resumeCount)

	const slotTmux = "maestro-sup-310"
	o.tmuxSessionExistsFn = func(name string) bool { return name == slotTmux }
	o.tmuxPaneIdentityFn = func(session string) (int, string, error) {
		return 0, "", fmt.Errorf("pane gone")
	}

	worktree := t.TempDir()
	stamp := time.Now().UTC()
	s := state.NewState()
	s.Sessions["sup-310"] = &state.Session{
		IssueNumber:         310,
		Status:              state.StatusRunning,
		PID:                 4242, // dead
		TmuxSession:         slotTmux,
		Branch:              "feat/sup-310-310-in-flight",
		Worktree:            worktree,
		Backend:             "claude",
		RestartCheckpointAt: &stamp,
	}

	o.reconcileRunningSessions(s)
	sess := s.Sessions["sup-310"]
	if resumeCount != 0 {
		t.Fatalf("respawnInPlace fired %d times, want 0 — must not respawn over a live replacement", resumeCount)
	}
	if sess.RestartCheckpointAt == nil {
		t.Fatal("marker must be PRESERVED when the live pane pid is unreadable, so the next cycle retries adoption")
	}
	if sess.Status != state.StatusRunning {
		t.Fatalf("status = %q, want running — never false-dead over a live replacement", sess.Status)
	}
}

// A deterministic tmux name is not sufficient identity. If a failed resume
// leaves (or encounters) a same-name pane in another worktree, recovery must
// neither adopt nor kill it, and must not retry into a destructive loop.
func TestReconcile_RestartCheckpoint_ForeignLiveReplacementFailsClosed(t *testing.T) {
	resumeCount := 0
	o := restartResumeOrchestrator(t, &resumeCount)

	const slotTmux = "maestro-sup-310"
	o.tmuxSessionExistsFn = func(name string) bool { return name == slotTmux }
	o.tmuxPaneIdentityFn = func(session string) (int, string, error) {
		return 7777, filepath.Join(t.TempDir(), "foreign"), nil
	}

	worktree := t.TempDir()
	stamp := time.Now().UTC()
	s := state.NewState()
	s.Sessions["sup-310"] = &state.Session{
		IssueNumber:         310,
		Status:              state.StatusRunning,
		PID:                 4242,
		TmuxSession:         slotTmux,
		Branch:              "feat/sup-310-310-in-flight",
		Worktree:            worktree,
		Backend:             "claude",
		RestartCheckpointAt: &stamp,
	}

	if !o.reconcileRunningSessions(s) {
		t.Fatal("expected foreign pane identity to reconcile fail-closed state")
	}
	sess := s.Sessions["sup-310"]
	if resumeCount != 0 {
		t.Fatalf("respawnInPlace fired %d times, want 0", resumeCount)
	}
	if sess.Status != state.StatusDead || sess.PID != 0 || sess.TmuxSession != "" {
		t.Fatalf("session = status %q pid %d tmux %q, want dead/0/empty", sess.Status, sess.PID, sess.TmuxSession)
	}
	if sess.RestartCheckpointAt != nil {
		t.Fatal("marker must be consumed after a foreign-pane identity mismatch")
	}
	if sess.Worktree != worktree {
		t.Fatalf("retained worktree = %q, want preserved %q", sess.Worktree, worktree)
	}
	if sess.LastNotifiedStatus != "restart_resume_identity_mismatch" {
		t.Fatalf("last notified status = %q, want mismatch blocker", sess.LastNotifiedStatus)
	}
}

// A restart-checkpointed session whose worktree did NOT survive must not be
// resumed in place (RespawnInPlace would fail against a missing directory). The
// marker is consumed and the session falls through to the normal terminal path,
// so the retry ladder can fresh-respawn it instead of looping.
func TestReconcile_RestartCheckpoint_MissingWorktree_FallsThrough(t *testing.T) {
	resumeCount := 0
	o := restartResumeOrchestrator(t, &resumeCount)

	stamp := time.Now().UTC()
	s := state.NewState()
	s.Sessions["sup-312"] = &state.Session{
		IssueNumber:         312,
		IssueTitle:          "worktree gone",
		Status:              state.StatusRunning,
		PID:                 4244,
		TmuxSession:         "maestro-sup-312",
		Branch:              "feat/sup-312-312-gone",
		Worktree:            "/nonexistent/maestro-sup-312",
		Backend:             "claude",
		RestartCheckpointAt: &stamp,
	}

	o.reconcileRunningSessions(s)
	sess := s.Sessions["sup-312"]
	if resumeCount != 0 {
		t.Fatalf("respawnInPlace fired %d times, want 0 — a missing worktree cannot be resumed in place", resumeCount)
	}
	if sess.RestartCheckpointAt != nil {
		t.Fatal("marker must be consumed even when the worktree is gone, so it never loops")
	}
	if sess.Status != state.StatusDead {
		t.Fatalf("status = %q, want %q (fell through to terminal handling)", sess.Status, state.StatusDead)
	}
}

func TestReconcile_PaneExitedTerminatesPersistedProcessLease(t *testing.T) {
	resumeCount := 0
	o := restartResumeOrchestrator(t, &resumeCount)
	stopCalls := 0
	o.workerStopProcessFn = func(_ string, sess *state.Session) error {
		stopCalls++
		sess.ProcessLeaseUnit = ""
		sess.ProcessLeaseManager = ""
		return nil
	}

	s := state.NewState()
	s.Sessions["sup-920"] = &state.Session{
		IssueNumber:         920,
		IssueTitle:          "durable process lease",
		Status:              state.StatusRunning,
		PID:                 4242,
		TmuxSession:         "maestro-sup-920",
		Branch:              "feat/sup-920",
		ProcessLeaseUnit:    "maestro-worker-0123456789abcdef0123456789abcdef-g1.scope",
		ProcessLeaseManager: "system",
	}

	if !o.reconcileRunningSessions(s) {
		t.Fatal("expected dead pane reconciliation")
	}
	if stopCalls != 1 {
		t.Fatalf("process lease teardown calls = %d, want 1", stopCalls)
	}
	if got := s.Sessions["sup-920"]; got.ProcessLeaseUnit != "" || got.Status != state.StatusDead {
		t.Fatalf("reconciled session = %+v, want released lease and dead status", got)
	}
}

func TestReconcile_ProcessLeaseCleanupRetriesExactlyUntilConfirmed(t *testing.T) {
	resumeCount := 0
	o := restartResumeOrchestrator(t, &resumeCount)
	stopCalls := 0
	o.workerStopProcessFn = func(_ string, sess *state.Session) error {
		stopCalls++
		if stopCalls == 1 {
			return errors.New("system manager temporarily unavailable")
		}
		sess.ProcessLeaseUnit = ""
		sess.ProcessLeaseManager = ""
		return nil
	}

	s := state.NewState()
	s.Sessions["sup-920"] = &state.Session{
		IssueNumber:         920,
		Status:              state.StatusFailed,
		ProcessLeaseUnit:    "maestro-worker-0123456789abcdef0123456789abcdef-g2.scope",
		ProcessLeaseManager: "system",
	}

	if o.reconcileRunningSessions(s) {
		t.Fatal("failed lease cleanup must not claim reconciliation completed")
	}
	if s.Sessions["sup-920"].ProcessLeaseUnit == "" {
		t.Fatal("failed cleanup lost the durable retry receipt")
	}
	if !o.reconcileRunningSessions(s) {
		t.Fatal("successful retry should report reconciliation")
	}
	if stopCalls != 2 || s.Sessions["sup-920"].ProcessLeaseUnit != "" {
		t.Fatalf("cleanup calls=%d lease=%q, want two calls and released metadata", stopCalls, s.Sessions["sup-920"].ProcessLeaseUnit)
	}
	if o.reconcileRunningSessions(s) {
		t.Fatal("released lease must not be cleaned a third time")
	}
	if stopCalls != 2 {
		t.Fatalf("cleanup replayed after confirmation: %d calls", stopCalls)
	}
}
