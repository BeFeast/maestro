package orchestrator

import (
	"bytes"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/github"
	"github.com/befeast/maestro/internal/notify"
	"github.com/befeast/maestro/internal/state"
	"github.com/befeast/maestro/internal/worker"
)

func captureOrchestratorLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prevOut, prevFlags := log.Writer(), log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(prevOut)
		log.SetFlags(prevFlags)
	})
	return &buf
}

// #1243: the only seal of a cleanly exited native generation used to be the
// next respawn of its slot, which the fleet ceiling (counting that unsealed
// generation as a live worker) itself blocked. The cycle now seals exited
// generations before any step consults the ceiling, so a ceiling held only by
// such a generation reopens in the same cycle.
func TestRunOnceSealsNativeExitBeforeFleetCeiling(t *testing.T) {
	cfg := &config.Config{
		Repo:              "owner/repo",
		StateDir:          t.TempDir(),
		MaxParallel:       1,
		MaxRuntimeMinutes: 60,
	}
	cfg.AIExecution.RequireVerifiedRoute = true
	finished := time.Now().Add(-3 * time.Hour).UTC()
	s := state.NewState()
	s.Sessions["ret-1"] = &state.Session{IssueNumber: 17, IssueTitle: "issue 17", Status: state.StatusDone, PRNumber: 20,
		WorkerGeneration: 8, NativeRoleRunID: "role-run", StartedAt: finished.Add(-time.Hour), FinishedAt: &finished}
	s.Sessions["ret-2"] = &state.Session{IssueNumber: 7, IssueTitle: "issue 7", Status: state.StatusPROpen, PRNumber: 21,
		StartedAt: finished.Add(-time.Hour), FinishedAt: &finished}
	if err := state.Save(cfg.StateDir, s); err != nil {
		t.Fatal(err)
	}

	var order []string
	sealed := false
	listed := false
	o := &Orchestrator{
		cfg:                 cfg,
		repo:                cfg.Repo,
		notifier:            &notify.Notifier{},
		listOpenPRsFn:       func() ([]github.PR, error) { return nil, nil },
		pidAliveFn:          func(int) bool { return false },
		tmuxSessionExistsFn: func(string) bool { return false },
		isIssueClosedFn:     func(int) (bool, error) { return false, nil },
		isPRMergedFn:        func(int) (bool, error) { return false, nil },
		captureTmuxFn:       func(string) (string, error) { return "", nil },
		listOpenIssuesFn: func([]string) ([]github.Issue, error) {
			listed = true
			return nil, nil
		},
	}
	o.nativeExitReconcileFn = func(gotCfg *config.Config, st *state.State, slot string) (bool, error) {
		if gotCfg != cfg || st.Sessions[slot] == nil {
			t.Fatalf("exit reconciliation got the wrong cycle inputs for %s", slot)
		}
		order = append(order, "exit:"+slot)
		if slot == "ret-1" {
			sealed = true
			return true, nil
		}
		return false, nil
	}
	o.SetFleetSpawnCeiling(func() bool {
		order = append(order, "ceiling")
		// The only counted unit is ret-1's exited, still unsealed generation.
		return !sealed
	})
	o.SetFleetSpawnStallDiagnostic(func() (string, bool) {
		t.Fatal("stall diagnostic consulted although the ceiling reopened")
		return "", false
	})

	if err := o.RunOnce(); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if len(order) < 3 || order[0] != "exit:ret-1" || order[1] != "exit:ret-2" || order[2] != "ceiling" {
		t.Fatalf("cycle order=%v, want every exit reconciled before the fleet ceiling", order)
	}
	if !listed {
		t.Fatal("ceiling held by the sealed exit still blocked fresh dispatch")
	}
}

// Without the verified route there is no observable termination proof, so the
// cycle leaves exit sealing to the existing lease and respawn paths.
func TestRunOnceSkipsNativeExitReconcileWithoutVerifiedRoute(t *testing.T) {
	cfg := &config.Config{Repo: "owner/repo", StateDir: t.TempDir(), MaxParallel: 1, MaxRuntimeMinutes: 60}
	s := state.NewState()
	finished := time.Now().UTC()
	s.Sessions["ret-1"] = &state.Session{IssueNumber: 17, Status: state.StatusDone, WorkerGeneration: 1, NativeRoleRunID: "role-run", FinishedAt: &finished}
	if err := state.Save(cfg.StateDir, s); err != nil {
		t.Fatal(err)
	}
	o := &Orchestrator{
		cfg:                 cfg,
		repo:                cfg.Repo,
		notifier:            &notify.Notifier{},
		listOpenPRsFn:       func() ([]github.PR, error) { return nil, nil },
		pidAliveFn:          func(int) bool { return false },
		tmuxSessionExistsFn: func(string) bool { return false },
		isIssueClosedFn:     func(int) (bool, error) { return false, nil },
		captureTmuxFn:       func(string) (string, error) { return "", nil },
		listOpenIssuesFn:    func([]string) ([]github.Issue, error) { return nil, nil },
	}
	o.nativeExitReconcileFn = func(*config.Config, *state.State, string) (bool, error) {
		t.Fatal("exit reconciliation ran without the verified route")
		return false, nil
	}
	if err := o.RunOnce(); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
}

// A typed hold from the exit reconciliation is journaled with its cause and
// does not stop the cycle; the generation stays counted by the ceiling.
func TestReconcileNativeWorkerExitsJournalsTypedHold(t *testing.T) {
	buf := captureOrchestratorLog(t)
	s := state.NewState()
	s.Sessions["ret-1"] = &state.Session{Status: state.StatusDone}
	s.Sessions["ret-2"] = &state.Session{Status: state.StatusDone}
	calls := 0
	o := &Orchestrator{cfg: &config.Config{}}
	o.nativeExitReconcileFn = func(_ *config.Config, _ *state.State, slot string) (bool, error) {
		calls++
		if slot == "ret-1" {
			return false, &worker.NativeRegistrationHold{Code: "native_process_identity_missing", LaunchUncertain: true, Slot: slot}
		}
		return true, nil
	}
	o.reconcileNativeWorkerExits(s)
	out := buf.String()
	if calls != 2 {
		t.Fatalf("calls=%d, a hold on one slot stopped the others", calls)
	}
	if !strings.Contains(out, "native exit seal held for ret-1: worker native registration held: native_process_identity_missing") {
		t.Fatalf("typed hold not journaled:\n%s", out)
	}
	if !strings.Contains(out, "native exit sealed for ret-2") {
		t.Fatalf("seal not journaled:\n%s", out)
	}
}

// #1243: when the ceiling blocks a project that runs no worker, the cycle
// journals which slots hold the ceiling and why, once per cycle, and marks it
// CRITICAL when no worker runs anywhere in the fleet.
func TestFleetCeilingStallDiagnosticJournaledOncePerCycle(t *testing.T) {
	buf := captureOrchestratorLog(t)
	cfg := cfgWithBackends("claude", "claude")
	o, started, _ := newStartWorkersOrchestrator(cfg, []github.Issue{makeIssue(17, "repair")})
	o.SetFleetSpawnCeiling(func() bool { return true })
	consulted := 0
	stalled := true
	o.SetFleetSpawnStallDiagnostic(func() (string, bool) {
		consulted++
		return "live=2 min=2 max=2 fleet_running=0 occupants=[owner/halenote/ret-1: native launch not recorded terminal (session done)]", stalled
	})
	s := state.NewState()

	o.beginCycle()
	o.startNewWorkers(s, 1)
	o.startNewWorkers(s, 1)
	o.noteFleetCeilingStall(s)
	o.endCycle()
	if consulted != 1 || strings.Count(buf.String(), "CRITICAL fleet live-worker ceiling stall") != 1 {
		t.Fatalf("consulted=%d, want one CRITICAL stall line per cycle:\n%s", consulted, buf.String())
	}
	if !strings.Contains(buf.String(), "owner/halenote/ret-1: native launch not recorded terminal (session done)") {
		t.Fatalf("stall line does not name the occupying slot:\n%s", buf.String())
	}
	if len(*started) != 0 {
		t.Fatalf("started=%v under a closed ceiling", *started)
	}

	// The next cycle reports again; a fleet that still runs a worker elsewhere
	// is reported without the CRITICAL marker.
	buf.Reset()
	stalled = false
	o.beginCycle()
	o.startNewWorkers(s, 1)
	o.endCycle()
	if consulted != 2 || strings.Contains(buf.String(), "CRITICAL") || !strings.Contains(buf.String(), "ceiling held with no local worker running") {
		t.Fatalf("consulted=%d, second cycle:\n%s", consulted, buf.String())
	}

	// A project with its own running worker is not stalled by the ceiling.
	buf.Reset()
	s.Sessions["slot-1"] = &state.Session{IssueNumber: 1, Status: state.StatusRunning}
	o.beginCycle()
	o.startNewWorkers(s, 1)
	o.endCycle()
	if consulted != 2 || strings.Contains(buf.String(), "ceiling held") || strings.Contains(buf.String(), "stall") {
		t.Fatalf("consulted=%d, diagnostic with a local worker running:\n%s", consulted, buf.String())
	}
}
