package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/state"
)

// TestFleetAPIExposesPausedFlag pins #683 AC5: GET /api/v1/fleet carries the
// per-project paused flag so Mission Control and the operator brief can tell
// an intentional pause from an outage. The flag is an explicit boolean on
// every project (false when not paused) and the paused project's operator
// state reads "Paused" instead of dead/idle.
func TestFleetAPIExposesPausedFlag(t *testing.T) {
	dir := t.TempDir()
	pausedStateDir := filepath.Join(dir, "paused")
	normalStateDir := filepath.Join(dir, "normal")

	pausedAt := time.Date(2026, 6, 11, 9, 0, 0, 0, time.UTC)
	pausedState := state.NewState()
	pausedState.SetPaused(pausedAt)
	if err := state.Save(pausedStateDir, pausedState); err != nil {
		t.Fatalf("save paused state: %v", err)
	}
	if err := state.Save(normalStateDir, state.NewState()); err != nil {
		t.Fatalf("save normal state: %v", err)
	}

	projects := []FleetProject{
		NewFleetProject("Paused", "/tmp/paused.yaml", "", &config.Config{
			Repo:        "owner/paused",
			StateDir:    pausedStateDir,
			MaxParallel: 1,
		}),
		NewFleetProject("Normal", "/tmp/normal.yaml", "", &config.Config{
			Repo:        "owner/normal",
			StateDir:    normalStateDir,
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
	if len(resp.Projects) != 2 {
		t.Fatalf("projects len = %d, want 2", len(resp.Projects))
	}

	byName := make(map[string]fleetProjectState, len(resp.Projects))
	for _, project := range resp.Projects {
		byName[project.Name] = project
	}
	paused, ok := byName["Paused"]
	if !ok {
		t.Fatalf("project Paused missing from response: %+v", resp.Projects)
	}
	if !paused.Paused {
		t.Fatal("paused project missing paused=true in fleet API")
	}
	if !paused.PausedAt.Equal(pausedAt) {
		t.Fatalf("paused_at = %v, want %v", paused.PausedAt, pausedAt)
	}
	if paused.OperatorState.Kind != "paused" || paused.OperatorState.Label != "Paused" {
		t.Fatalf("operator state = %+v, want kind=paused label=Paused", paused.OperatorState)
	}

	normal, ok := byName["Normal"]
	if !ok {
		t.Fatalf("project Normal missing from response: %+v", resp.Projects)
	}
	if normal.Paused {
		t.Fatal("non-paused project reports paused=true")
	}

	// The JSON wire format must carry an explicit boolean for every
	// project (no omitempty), so the SPA never reads undefined.
	if got := strings.Count(w.Body.String(), `"paused":`); got != 2 {
		t.Fatalf(`found %d "paused" keys in response, want one per project (2)`, got)
	}
}

// #888: Fleet serializes state/config projections, never the private service
// credential file. A synthetic canary in the one approved boundary must not
// appear in the API response.
func TestFleetAPIDoesNotExposeWorkerCredentialBoundary(t *testing.T) {
	stateDir := t.TempDir()
	if err := state.Save(stateDir, state.NewState()); err != nil {
		t.Fatalf("save state: %v", err)
	}
	credentialDir := t.TempDir()
	if err := os.Chmod(credentialDir, 0o700); err != nil {
		t.Fatalf("mkdir credential dir: %v", err)
	}
	canary := "CANARY-" + "fleet-zero-copy-" + "0888"
	credentialFile := filepath.Join(credentialDir, "worker-proxy.env")
	if err := os.WriteFile(
		credentialFile,
		[]byte("OPENAI_API_KEY='"+canary+"'\n"),
		0o600,
	); err != nil {
		t.Fatalf("write synthetic credential boundary: %v", err)
	}
	t.Setenv("MAESTRO_WORKER_CREDENTIALS_FILE", credentialFile)

	srv := NewFleet([]FleetProject{
		NewFleetProject("Safe", "/tmp/safe.yaml", "", &config.Config{
			Repo:        "owner/safe",
			StateDir:    stateDir,
			MaxParallel: 1,
		}),
	}, "127.0.0.1", 8786, true)
	w := httptest.NewRecorder()
	srv.handleFleet(w, httptest.NewRequest(http.MethodGet, "/api/v1/fleet", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}
	if strings.Contains(w.Body.String(), canary) {
		t.Fatal("Fleet API exposed the private worker credential value")
	}
}
