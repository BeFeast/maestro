package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/state"
)

func TestLoadFleetProjects(t *testing.T) {
	dir := t.TempDir()
	stateDir := filepath.Join(dir, "state")
	configPath := filepath.Join(dir, "project.yaml")
	if err := os.WriteFile(configPath, []byte("repo: owner/project\nstate_dir: "+stateDir+"\nsession_prefix: prj\n"), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	fleetPath := filepath.Join(dir, "fleet.yaml")
	if err := os.WriteFile(fleetPath, []byte("projects:\n  - name: Project\n    config: project.yaml\n    dashboard_url: http://127.0.0.1:8787\n"), 0o644); err != nil {
		t.Fatalf("write fleet: %v", err)
	}

	projects, err := LoadFleetProjects(fleetPath)
	if err != nil {
		t.Fatalf("LoadFleetProjects failed: %v", err)
	}
	if len(projects) != 1 {
		t.Fatalf("projects len = %d, want 1", len(projects))
	}
	if projects[0].Name != "Project" {
		t.Fatalf("project name = %q", projects[0].Name)
	}
	if projects[0].cfg == nil || projects[0].cfg.Repo != "owner/project" {
		t.Fatalf("resolved config = %+v", projects[0].cfg)
	}
	if projects[0].DashboardURL != "http://127.0.0.1:8787" {
		t.Fatalf("dashboard url = %q", projects[0].DashboardURL)
	}
}

func TestFleetAPIAggregatesProjects(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()
	firstStateDir := filepath.Join(dir, "one")
	secondStateDir := filepath.Join(dir, "two")
	saveFleetTestState(t, firstStateDir, map[string]*state.Session{
		"one-1": {
			IssueNumber: 1,
			IssueTitle:  "Build thing",
			Status:      state.StatusRunning,
			StartedAt:   now.Add(-time.Minute),
			PID:         999999,
			Backend:     "opencode",
		},
		"one-2": {
			IssueNumber: 2,
			IssueTitle:  "Review thing",
			Status:      state.StatusPROpen,
			StartedAt:   now.Add(-2 * time.Minute),
			PRNumber:    12,
		},
	})
	saveFleetTestState(t, secondStateDir, map[string]*state.Session{
		"two-1": {
			IssueNumber: 3,
			IssueTitle:  "Broken thing",
			Status:      state.StatusRetryExhausted,
			StartedAt:   now.Add(-3 * time.Minute),
		},
	})

	projects := []FleetProject{
		NewFleetProject("One", "/tmp/one.yaml", "http://127.0.0.1:8787", &config.Config{
			Repo:        "owner/one",
			StateDir:    firstStateDir,
			MaxParallel: 2,
			Server:      config.ServerConfig{ReadOnly: true},
		}),
		NewFleetProject("Two", "/tmp/two.yaml", "", &config.Config{
			Repo:        "owner/two",
			StateDir:    secondStateDir,
			MaxParallel: 1,
		}),
	}
	srv := NewFleet(projects, "127.0.0.1", 8786, true)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/fleet", nil)
	w := httptest.NewRecorder()
	srv.handleFleet(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}
	var resp fleetResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Summary.Projects != 2 || resp.Summary.Running != 1 || resp.Summary.PROpen != 1 || resp.Summary.Failed != 1 || resp.Summary.Sessions != 3 {
		t.Fatalf("unexpected summary: %+v", resp.Summary)
	}
	if resp.Projects[0].Name != "One" {
		t.Fatalf("first project = %q, want One", resp.Projects[0].Name)
	}
	if len(resp.Projects[0].Active) != 2 {
		t.Fatalf("project active len = %d, want 2", len(resp.Projects[0].Active))
	}
}

func TestFleetDashboard(t *testing.T) {
	srv := NewFleet(nil, "127.0.0.1", 8786, true)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	srv.handleFleetDashboard(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}
	body := w.Body.String()
	if ct := w.Header().Get("Content-Type"); ct != "text/html; charset=utf-8" {
		t.Errorf("content-type = %q, want text/html", ct)
	}
	for _, want := range []string{"Maestro Fleet", "/api/v1/fleet", "renderProject"} {
		if !contains(body, want) {
			t.Fatalf("dashboard should contain %q", want)
		}
	}
}

func saveFleetTestState(t *testing.T, dir string, sessions map[string]*state.Session) {
	t.Helper()
	st := state.NewState()
	for name, sess := range sessions {
		st.Sessions[name] = sess
	}
	if err := state.Save(dir, st); err != nil {
		t.Fatalf("save state: %v", err)
	}
}
