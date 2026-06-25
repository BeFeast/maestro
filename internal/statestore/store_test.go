package statestore

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/befeast/maestro/internal/state"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "maestro.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func bindingFor(stateDir, project, repo string) RowBinding {
	return RowBinding{Project: project, Repo: repo, StateDir: stateDir}
}

// sampleState builds a non-trivial state.State with one running and one pr_open
// session plus a decision, backend-health entry and mission.
func sampleState() *state.State {
	now := time.Date(2026, 6, 25, 12, 0, 0, 0, time.UTC)
	st := state.NewState()
	st.Sessions["1"] = &state.Session{
		IssueNumber: 101,
		IssueTitle:  "running issue",
		Status:      state.StatusRunning,
		Backend:     "claude",
		StartedAt:   now,
	}
	st.Sessions["2"] = &state.Session{
		IssueNumber: 102,
		IssueTitle:  "pr open issue",
		Status:      state.StatusPROpen,
		PRNumber:    55,
		Backend:     "codex",
		StartedAt:   now,
	}
	st.SupervisorDecisions = []state.SupervisorDecision{{
		ID:                "dec-1",
		CreatedAt:         now,
		Summary:           "looks good",
		RecommendedAction: "merge",
		Risk:              "low",
	}}
	st.BackendHealth["claude"] = state.BackendHealth{State: state.BackendHealthAvailable, Since: now}
	st.BackendHealth["codex"] = state.BackendHealth{State: state.BackendHealthCooldown, Reason: "quota", Since: now}
	st.Missions[200] = &state.Mission{ParentIssue: 200, ChildIssues: []int{201, 202}, Status: "active"}
	return st
}

func TestImportStateRoundTrip(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	b := bindingFor("/sd/a", "owner/a", "owner/a")
	if err := s.ImportState(ctx, b, sampleState()); err != nil {
		t.Fatalf("import: %v", err)
	}

	sessions, err := s.Sessions(ctx, b.StateDir)
	if err != nil {
		t.Fatalf("sessions: %v", err)
	}
	if len(sessions) != 2 {
		t.Fatalf("want 2 sessions, got %d", len(sessions))
	}
	if sessions["1"].IssueNumber != 101 || sessions["1"].Status != state.StatusRunning {
		t.Fatalf("session 1 round-trip mismatch: %+v", sessions["1"])
	}
	if sessions["2"].PRNumber != 55 {
		t.Fatalf("session 2 pr round-trip mismatch: %+v", sessions["2"])
	}

	decisions, err := s.Decisions(ctx, b.StateDir)
	if err != nil {
		t.Fatalf("decisions: %v", err)
	}
	if len(decisions) != 1 || decisions[0].ID != "dec-1" {
		t.Fatalf("decision round-trip mismatch: %+v", decisions)
	}

	health, err := s.BackendHealth(ctx, b.StateDir)
	if err != nil {
		t.Fatalf("backend health: %v", err)
	}
	if health["claude"].State != state.BackendHealthAvailable || health["codex"].Reason != "quota" {
		t.Fatalf("backend health round-trip mismatch: %+v", health)
	}

	missions, err := s.Missions(ctx, b.StateDir)
	if err != nil {
		t.Fatalf("missions: %v", err)
	}
	if len(missions) != 1 || missions[200].Status != "active" || len(missions[200].ChildIssues) != 2 {
		t.Fatalf("mission round-trip mismatch: %+v", missions)
	}
}

// TestImportStateIdempotent asserts a re-import of the same snapshot does not
// duplicate rows (one-shot startup import must be safe to repeat) and that a
// removed entry disappears (full per-project snapshot replace).
func TestImportStateIdempotent(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	b := bindingFor("/sd/a", "owner/a", "owner/a")
	if err := s.ImportState(ctx, b, sampleState()); err != nil {
		t.Fatalf("import: %v", err)
	}
	if err := s.ImportState(ctx, b, sampleState()); err != nil {
		t.Fatalf("re-import: %v", err)
	}
	sessions, err := s.Sessions(ctx, b.StateDir)
	if err != nil {
		t.Fatalf("sessions: %v", err)
	}
	if len(sessions) != 2 {
		t.Fatalf("re-import duplicated sessions: got %d, want 2", len(sessions))
	}

	// Drop a session and re-import: the mirror must shrink to match JSON.
	shrunk := sampleState()
	delete(shrunk.Sessions, "2")
	if err := s.ImportState(ctx, b, shrunk); err != nil {
		t.Fatalf("import shrunk: %v", err)
	}
	sessions, err = s.Sessions(ctx, b.StateDir)
	if err != nil {
		t.Fatalf("sessions after shrink: %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("removed session still present: got %d, want 1", len(sessions))
	}
	if _, ok := sessions["2"]; ok {
		t.Fatalf("session 2 should be gone after re-import without it")
	}
}

// TestCrossProjectRunningSessions is the #760 acceptance example: every running
// session across the whole fleet answered in one query, each tagged with its
// originating project.
func TestCrossProjectRunningSessions(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	a := bindingFor("/sd/a", "owner/a", "owner/a")
	bb := bindingFor("/sd/b", "owner/b", "owner/b")
	if err := s.ImportState(ctx, a, sampleState()); err != nil {
		t.Fatalf("import a: %v", err)
	}
	if err := s.ImportState(ctx, bb, sampleState()); err != nil {
		t.Fatalf("import b: %v", err)
	}

	running, err := s.RunningSessions(ctx)
	if err != nil {
		t.Fatalf("running sessions: %v", err)
	}
	// Each project contributes exactly one running session (session "1"); the
	// pr_open session "2" must be excluded.
	if len(running) != 2 {
		t.Fatalf("want 2 running sessions across the fleet, got %d", len(running))
	}
	seen := map[string]bool{}
	for _, ps := range running {
		if ps.Session.Status != state.StatusRunning {
			t.Fatalf("non-running session leaked into RunningSessions: %+v", ps.Session)
		}
		seen[ps.Project] = true
	}
	if !seen["owner/a"] || !seen["owner/b"] {
		t.Fatalf("running sessions not tagged with both projects: %+v", running)
	}
}

// TestStateDirScopingIsolation asserts the same slot under two state dirs does
// not collide — the composite (state_dir, slot) key keeps projects disjoint.
func TestStateDirScopingIsolation(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	a := bindingFor("/sd/a", "owner/a", "owner/a")
	bb := bindingFor("/sd/b", "owner/b", "owner/b")

	stA := state.NewState()
	stA.Sessions["1"] = &state.Session{IssueNumber: 1, Status: state.StatusRunning}
	stB := state.NewState()
	stB.Sessions["1"] = &state.Session{IssueNumber: 999, Status: state.StatusRunning}

	if err := s.ImportState(ctx, a, stA); err != nil {
		t.Fatalf("import a: %v", err)
	}
	if err := s.ImportState(ctx, bb, stB); err != nil {
		t.Fatalf("import b: %v", err)
	}

	gotA, err := s.Sessions(ctx, a.StateDir)
	if err != nil {
		t.Fatalf("sessions a: %v", err)
	}
	gotB, err := s.Sessions(ctx, bb.StateDir)
	if err != nil {
		t.Fatalf("sessions b: %v", err)
	}
	if gotA["1"].IssueNumber != 1 || gotB["1"].IssueNumber != 999 {
		t.Fatalf("same-slot rows collided across projects: a=%+v b=%+v", gotA["1"], gotB["1"])
	}
	// Re-importing project a must not disturb project b's identically-slotted row.
	if err := s.ImportState(ctx, a, stA); err != nil {
		t.Fatalf("re-import a: %v", err)
	}
	gotB, err = s.Sessions(ctx, bb.StateDir)
	if err != nil {
		t.Fatalf("sessions b after re-import a: %v", err)
	}
	if gotB["1"].IssueNumber != 999 {
		t.Fatalf("re-import of a mutated b's row: %+v", gotB["1"])
	}
}

// TestImportNilClears asserts ImportState(nil) clears a project's rows (a
// deleted project leaves no stale rows in a cross-project query).
func TestImportNilClears(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	b := bindingFor("/sd/a", "owner/a", "owner/a")
	if err := s.ImportState(ctx, b, sampleState()); err != nil {
		t.Fatalf("import: %v", err)
	}
	if err := s.ImportState(ctx, b, nil); err != nil {
		t.Fatalf("import nil: %v", err)
	}
	sessions, err := s.Sessions(ctx, b.StateDir)
	if err != nil {
		t.Fatalf("sessions: %v", err)
	}
	if len(sessions) != 0 {
		t.Fatalf("nil import did not clear sessions: got %d", len(sessions))
	}
	running, err := s.RunningSessions(ctx)
	if err != nil {
		t.Fatalf("running: %v", err)
	}
	if len(running) != 0 {
		t.Fatalf("cleared project still surfaced in cross-project query: %d", len(running))
	}
}

// TestRetainStateDirs proves the startup reconcile drops rows for projects no
// longer in the fleet while preserving the rows of projects still present — the
// #760 fix for stale cross-project rows after a project is removed.
func TestRetainStateDirs(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	a := bindingFor("/sd/a", "owner/a", "owner/a")
	b := bindingFor("/sd/b", "owner/b", "owner/b")
	c := bindingFor("/sd/c", "owner/c", "owner/c")
	for _, bind := range []RowBinding{a, b, c} {
		if err := s.ImportState(ctx, bind, sampleState()); err != nil {
			t.Fatalf("import %s: %v", bind.StateDir, err)
		}
	}

	// Keep only a and c; b was removed from the config store while stopped.
	if err := s.RetainStateDirs(ctx, []string{"/sd/a", "", "/sd/a", "/sd/c"}); err != nil {
		t.Fatalf("retain: %v", err)
	}

	running, err := s.RunningSessions(ctx)
	if err != nil {
		t.Fatalf("running: %v", err)
	}
	seen := map[string]bool{}
	for _, ps := range running {
		seen[ps.Project] = true
	}
	if seen["owner/b"] {
		t.Fatalf("removed project still surfaced in cross-project query: %+v", running)
	}
	if !seen["owner/a"] || !seen["owner/c"] {
		t.Fatalf("retained projects missing after prune: %+v", running)
	}
	// b's rows must be gone from every table, not just sessions.
	if h, _ := s.BackendHealth(ctx, "/sd/b"); len(h) != 0 {
		t.Fatalf("removed project's backend_health rows not pruned: %d", len(h))
	}
	if m, _ := s.Missions(ctx, "/sd/b"); len(m) != 0 {
		t.Fatalf("removed project's mission rows not pruned: %d", len(m))
	}
	if d, _ := s.Decisions(ctx, "/sd/b"); len(d) != 0 {
		t.Fatalf("removed project's decision rows not pruned: %d", len(d))
	}
	// a's rows must be untouched.
	if sess, _ := s.Sessions(ctx, "/sd/a"); len(sess) != 2 {
		t.Fatalf("retained project's sessions disturbed: %d", len(sess))
	}
}

// TestRetainStateDirsEmptyClearsAll asserts an empty keep set (no projects)
// clears every row.
func TestRetainStateDirsEmptyClearsAll(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	if err := s.ImportState(ctx, bindingFor("/sd/a", "owner/a", "owner/a"), sampleState()); err != nil {
		t.Fatalf("import: %v", err)
	}
	if err := s.RetainStateDirs(ctx, nil); err != nil {
		t.Fatalf("retain empty: %v", err)
	}
	running, err := s.RunningSessions(ctx)
	if err != nil {
		t.Fatalf("running: %v", err)
	}
	if len(running) != 0 {
		t.Fatalf("empty keep set did not clear all rows: %d", len(running))
	}
}

// TestClearStateDir asserts the runtime-removal counterpart drops one project's
// rows without touching another's.
func TestClearStateDir(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	a := bindingFor("/sd/a", "owner/a", "owner/a")
	b := bindingFor("/sd/b", "owner/b", "owner/b")
	if err := s.ImportState(ctx, a, sampleState()); err != nil {
		t.Fatalf("import a: %v", err)
	}
	if err := s.ImportState(ctx, b, sampleState()); err != nil {
		t.Fatalf("import b: %v", err)
	}
	if err := s.ClearStateDir(ctx, "/sd/a"); err != nil {
		t.Fatalf("clear a: %v", err)
	}
	if sess, _ := s.Sessions(ctx, "/sd/a"); len(sess) != 0 {
		t.Fatalf("cleared project still has sessions: %d", len(sess))
	}
	if sess, _ := s.Sessions(ctx, "/sd/b"); len(sess) != 2 {
		t.Fatalf("clearing a disturbed b: %d", len(sess))
	}
}

func TestMirrorModeJSONIsNoop(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	b := Binding{Mode: ModeJSON, Handle: s, StateDir: "/sd/a", Project: "owner/a", Repo: "owner/a"}
	if err := Mirror(b, sampleState()); err != nil {
		t.Fatalf("mirror json: %v", err)
	}
	sessions, err := s.Sessions(ctx, "/sd/a")
	if err != nil {
		t.Fatalf("sessions: %v", err)
	}
	if len(sessions) != 0 {
		t.Fatalf("ModeJSON Mirror wrote rows: got %d", len(sessions))
	}
}

func TestMirrorModeSQLiteWritesThrough(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	b := Binding{Mode: ModeSQLite, Handle: s, StateDir: "/sd/a", Project: "owner/a", Repo: "owner/a"}
	if err := Mirror(b, sampleState()); err != nil {
		t.Fatalf("mirror sqlite: %v", err)
	}
	sessions, err := s.Sessions(ctx, "/sd/a")
	if err != nil {
		t.Fatalf("sessions: %v", err)
	}
	if len(sessions) != 2 {
		t.Fatalf("ModeSQLite Mirror did not write through: got %d, want 2", len(sessions))
	}
}

func TestParseMode(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want Mode
		ok   bool
	}{
		{"", ModeJSON, true},
		{"json", ModeJSON, true},
		{"SQLite", ModeSQLite, true},
		{"sqlite", ModeSQLite, true},
		{"nope", "", false},
	} {
		got, err := ParseMode(tc.in)
		if tc.ok && (err != nil || got != tc.want) {
			t.Fatalf("ParseMode(%q) = (%q, %v), want (%q, nil)", tc.in, got, err, tc.want)
		}
		if !tc.ok && err == nil {
			t.Fatalf("ParseMode(%q) expected error", tc.in)
		}
	}
}
