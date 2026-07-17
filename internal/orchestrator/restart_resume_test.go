package orchestrator

// #877: a self-deploy/operator restart must not turn an in-flight worker into a
// false running->dead transition that strands a dirty worktree and forces a
// manual resume. The daemon deliberately checkpoints each still-running session
// on shutdown (state.Session.RestartCheckpointAt); this reconcile must then
// resume the SAME logical session in place EXACTLY ONCE, preserving the
// worktree, and never dispatch a duplicate.

import (
	"fmt"
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
	o.tmuxPanePIDFn = func(session string) (int, error) {
		if session == slotTmux {
			return adoptedPID, nil
		}
		return 0, fmt.Errorf("no such session %q", session)
	}

	worktree := t.TempDir()
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

// If a replacement's tmux is alive but its live pane pid can't be read, the
// marker must be PRESERVED (not consumed) so the next cycle retries the
// non-destructive adoption — the session must never be respawned over or falsely
// marked dead while a live replacement is running.
func TestReconcile_RestartCheckpoint_LiveReplacementUnreadablePID_RetriesNextCycle(t *testing.T) {
	resumeCount := 0
	o := restartResumeOrchestrator(t, &resumeCount)

	const slotTmux = "maestro-sup-310"
	o.tmuxSessionExistsFn = func(name string) bool { return name == slotTmux }
	o.tmuxPanePIDFn = func(session string) (int, error) { return 0, fmt.Errorf("pane gone") }

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
