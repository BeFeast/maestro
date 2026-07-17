package server

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/state"
	"github.com/befeast/maestro/internal/statestore"
)

func TestFleetProjectSnapshotSurfacesRepairReservation(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{Repo: "owner/repo", StateDir: dir, MaxParallel: 1}
	st := state.NewState()
	st.Sessions["txc-1"] = &state.Session{
		IssueNumber: 1,
		Status:      state.StatusDead,
		PRNumber:    7,
		Worktree:    "/work/txc-1",
	}
	st.Approvals = []state.Approval{{
		ID:     "repair-1",
		Action: "spawn_repair_worker",
		Status: state.ApprovalStatusAwaitingDispatch,
		Target: &state.SupervisorTarget{Issue: 1, PR: 7, Session: "txc-1"},
	}}
	if err := state.Save(dir, st); err != nil {
		t.Fatalf("save state: %v", err)
	}

	project := NewFleetProject("repo", "", "", cfg)
	fleet := NewFleet([]FleetProject{project}, "127.0.0.1", 0, false)
	snapshot, _ := fleet.projectSnapshot(project, time.Now().UTC())
	if len(snapshot.IssueClaims) != 1 {
		t.Fatalf("issue claims = %+v, want one repair reservation", snapshot.IssueClaims)
	}
	claim := snapshot.IssueClaims[0]
	if claim.Kind != state.IssueClaimRepairDispatch || claim.IssueNumber != 1 || claim.Session != "txc-1" || claim.PRNumber != 7 || claim.ApprovalID != "repair-1" {
		t.Fatalf("repair claim = %+v", claim)
	}
}

// TestFleetStateStoreCrossProject proves the #760 wiring end to end at the
// fleet layer: SetStateStore(sqlite) opens a shared maestro.db, mirrorState
// write-through-mirrors each project's freshly-loaded state, and the resulting
// mirror answers "every running session across the fleet" in ONE query instead
// of N state.Load calls.
func TestFleetStateStoreCrossProject(t *testing.T) {
	db := filepath.Join(t.TempDir(), "maestro.db")
	fleet := NewFleet(nil, "127.0.0.1", 0, false)
	if err := fleet.SetStateStore(statestore.ModeSQLite, db); err != nil {
		t.Fatalf("set state store: %v", err)
	}
	// SetStateStore installs a process-global state.Save hook; clear it so it
	// cannot fire into this closed store from a later test in the package.
	t.Cleanup(func() { state.SetSaveHook(nil) })
	store := fleet.StateStore()
	if store == nil {
		t.Fatal("expected a state store handle in sqlite mode")
	}

	// Two projects, each with one running and one pr_open session.
	cfgA := &config.Config{Repo: "owner/a", StateDir: filepath.Join(t.TempDir(), "a")}
	cfgB := &config.Config{Repo: "owner/b", StateDir: filepath.Join(t.TempDir(), "b")}
	for _, p := range []struct {
		cfg   *config.Config
		issue int
	}{{cfgA, 11}, {cfgB, 22}} {
		st := state.NewState()
		st.Sessions["1"] = &state.Session{IssueNumber: p.issue, Status: state.StatusRunning}
		st.Sessions["2"] = &state.Session{IssueNumber: p.issue + 1, Status: state.StatusPROpen}
		fleet.mirrorState(p.cfg, st)
	}

	running, err := store.RunningSessions(context.Background())
	if err != nil {
		t.Fatalf("running sessions: %v", err)
	}
	if len(running) != 2 {
		t.Fatalf("want 2 running sessions across the fleet, got %d", len(running))
	}
	byProject := map[string]int{}
	for _, ps := range running {
		if ps.Session.Status != state.StatusRunning {
			t.Fatalf("non-running session leaked: %+v", ps.Session)
		}
		byProject[ps.Project] = ps.Session.IssueNumber
	}
	if byProject["owner/a"] != 11 || byProject["owner/b"] != 22 {
		t.Fatalf("cross-project running sessions mis-tagged: %+v", byProject)
	}
}

// TestFleetStateStoreWriteThroughOnSave is the #760 P1 fix: with sqlite mode on,
// a plain state.Save (the orchestrator/supervisor/watchdog/server write path)
// mirrors into the shared maestro.db through the installed save hook — WITHOUT
// the fleet read path ever polling the project. The cross-project query then
// reflects the save immediately instead of being stuck at the startup snapshot.
func TestFleetStateStoreWriteThroughOnSave(t *testing.T) {
	db := filepath.Join(t.TempDir(), "maestro.db")
	cfg := &config.Config{Repo: "owner/a", StateDir: filepath.Join(t.TempDir(), "a")}
	proj := NewFleetProject("a", "", "", cfg)
	fleet := NewFleet([]FleetProject{proj}, "127.0.0.1", 0, false)
	if err := fleet.SetStateStore(statestore.ModeSQLite, db); err != nil {
		t.Fatalf("set state store: %v", err)
	}
	// SetStateStore installs a process-global hook; drop it so it cannot fire into
	// this closed store from a later test in the package.
	t.Cleanup(func() { state.SetSaveHook(nil) })
	store := fleet.StateStore()
	if store == nil {
		t.Fatal("expected a state store handle in sqlite mode")
	}

	// A direct save — no fleet read path involved — must reach the mirror.
	st := state.NewState()
	st.Sessions["1"] = &state.Session{IssueNumber: 99, Status: state.StatusRunning}
	if err := state.Save(cfg.StateDir, st); err != nil {
		t.Fatalf("save: %v", err)
	}

	running, err := store.RunningSessions(context.Background())
	if err != nil {
		t.Fatalf("running sessions: %v", err)
	}
	if len(running) != 1 {
		t.Fatalf("write-through did not mirror the save: got %d running sessions, want 1", len(running))
	}
	if running[0].Project != "owner/a" || running[0].Session.IssueNumber != 99 {
		t.Fatalf("write-through row mis-tagged: %+v", running[0])
	}
}

// TestFleetStateStoreJSONModeNoop asserts the default (json) mode opens no
// handle and mirrorState is inert — observable behavior is unchanged until the
// sqlite mirror is explicitly enabled.
func TestFleetStateStoreJSONModeNoop(t *testing.T) {
	fleet := NewFleet(nil, "127.0.0.1", 0, false)
	if err := fleet.SetStateStore(statestore.ModeJSON, ""); err != nil {
		t.Fatalf("set state store: %v", err)
	}
	if fleet.StateStore() != nil {
		t.Fatal("json mode must not open a state store handle")
	}
	// mirrorState must not panic or write when no handle exists.
	st := state.NewState()
	st.Sessions["1"] = &state.Session{IssueNumber: 1, Status: state.StatusRunning}
	fleet.mirrorState(&config.Config{Repo: "owner/a", StateDir: "/sd/a"}, st)
}
