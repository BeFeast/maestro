package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/state"
)

// TestHandleFleet_CostObservabilityExposed proves the /api/v1/fleet
// snapshot carries the per-project and fleet-wide cost rollup (#619)
// so MC does not compute pricing client-side. Acceptance:
// "/api/v1/fleet (or a new endpoint) returns token + $ aggregates per
// backend/project/day".
func TestHandleFleet_CostObservabilityExposed(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()
	stateDir := filepath.Join(dir, "state")

	saveFleetTestState(t, stateDir, map[string]*state.Session{
		"slot-1": {
			IssueNumber:     7,
			IssueTitle:      "Cost issue",
			Status:          state.StatusDone,
			StartedAt:       now.Add(-2 * time.Hour),
			FinishedAt:      timePtr(now.Add(-1 * time.Hour)),
			Backend:         "claude",
			TokensUsedTotal: 1_000_000,
		},
		"slot-2": {
			IssueNumber:     8,
			IssueTitle:      "Codex issue",
			Status:          state.StatusDone,
			StartedAt:       now.Add(-3 * time.Hour),
			FinishedAt:      timePtr(now.Add(-2 * time.Hour)),
			Backend:         "codex",
			TokensUsedTotal: 500_000,
		},
	})

	cfg := &config.Config{
		Repo:        "owner/proj",
		StateDir:    stateDir,
		MaxParallel: 2,
		Server:      config.ServerConfig{ReadOnly: true},
		Model: config.ModelConfig{
			Default: "claude",
			Backends: map[string]config.BackendDef{
				"claude": {Pricing: config.BackendPricing{InputUSDPerMtok: 10, OutputUSDPerMtok: 30}},
				"codex":  {},
			},
		},
	}

	projects := []FleetProject{NewFleetProject("Proj", "/tmp/proj.yaml", "", cfg)}
	srv := NewFleet(projects, "127.0.0.1", 0, true)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/fleet", nil)
	w := httptest.NewRecorder()
	srv.handleFleet(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var resp fleetResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if len(resp.Projects) != 1 {
		t.Fatalf("projects = %d, want 1", len(resp.Projects))
	}
	projCost := resp.Projects[0].CostObservability
	if projCost.Lifetime.Tokens != 1_500_000 {
		t.Errorf("project lifetime tokens = %d, want 1500000", projCost.Lifetime.Tokens)
	}
	// claude 1M * 20/Mtok = $20.00; codex unpriced = 0; total $20.00
	if got := projCost.Lifetime.USD; got < 19.99 || got > 20.01 {
		t.Errorf("project lifetime USD = %f, want ~20.00", got)
	}
	if projCost.Lifetime.PricedTokens != 1_000_000 {
		t.Errorf("project lifetime priced tokens = %d, want 1000000", projCost.Lifetime.PricedTokens)
	}
	if projCost.Lifetime.UnpricedTokens != 500_000 {
		t.Errorf("project lifetime unpriced tokens = %d, want 500000", projCost.Lifetime.UnpricedTokens)
	}

	if len(projCost.PerBackend) == 0 {
		t.Fatalf("project per_backend missing")
	}
	if len(projCost.PerIssue) != 2 {
		t.Fatalf("project per_issue rows = %d, want 2", len(projCost.PerIssue))
	}

	// The fleet-wide rollup mirrors the single project.
	if resp.CostObservability.Lifetime.Tokens != 1_500_000 {
		t.Errorf("global lifetime tokens = %d, want 1500000", resp.CostObservability.Lifetime.Tokens)
	}
	if len(resp.CostObservability.PerProject) != 1 {
		t.Errorf("global per_project = %d, want 1", len(resp.CostObservability.PerProject))
	}

	// Per-worker $ estimate flows through to the fleet worker row.
	var claudeWorker fleetWorkerState
	for _, w := range resp.Workers {
		if w.Slot == "slot-1" {
			claudeWorker = w
		}
	}
	if claudeWorker.Slot == "" {
		t.Fatalf("slot-1 worker missing from response")
	}
	if got := claudeWorker.CostUSDEstimate; got < 19.99 || got > 20.01 {
		t.Errorf("slot-1 cost_usd_estimate = %f, want ~20.00", got)
	}

	// And the unpriced backend reports tokens but no dollars.
	var codexWorker fleetWorkerState
	for _, w := range resp.Workers {
		if w.Slot == "slot-2" {
			codexWorker = w
		}
	}
	if codexWorker.Slot == "" {
		t.Fatalf("slot-2 worker missing from response")
	}
	if codexWorker.CostUSDEstimate != 0 {
		t.Errorf("slot-2 cost_usd_estimate = %f, want 0 (codex unpriced)", codexWorker.CostUSDEstimate)
	}
	if codexWorker.TokensUsedTotal != 500_000 {
		t.Errorf("slot-2 tokens_used_total = %d, want 500000", codexWorker.TokensUsedTotal)
	}
}
