package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/befeast/maestro/internal/outcome"
)

func TestFleetOutcomeProbeSkipsSnapshot(t *testing.T) {
	srv := NewFleet([]FleetProject{{Name: "snapshot-would-include-this-project"}}, "127.0.0.1", 8786, false)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/fleet", nil)
	req.Header.Set(outcome.HealthProbeHeader, outcome.HealthProbeHeaderValue)
	rec := httptest.NewRecorder()

	srv.handleFleet(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal probe response: %v", err)
	}
	if string(body["status"]) != `"ok"` {
		t.Fatalf("status body = %s, want ok", body["status"])
	}
	if _, ok := body["projects"]; ok {
		t.Fatal("outcome probe built the full Fleet snapshot")
	}
}

func TestFleetOutcomeProbeStillRequiresAuth(t *testing.T) {
	srv := NewFleet(nil, "127.0.0.1", 8786, false)
	srv.SetAuthForTest("good-token", "alice")
	req := httptest.NewRequest(http.MethodGet, "/api/v1/fleet", nil)
	req.Header.Set(outcome.HealthProbeHeader, outcome.HealthProbeHeaderValue)
	rec := httptest.NewRecorder()

	srv.HandlerForTest().ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}
