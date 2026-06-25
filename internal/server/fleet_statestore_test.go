package server

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/state"
	"github.com/befeast/maestro/internal/statestore"
)

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
