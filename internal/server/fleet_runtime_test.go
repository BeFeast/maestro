package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/befeast/maestro/internal/config"
)

// The daemon hot-adds and hot-removes flows at runtime (#757); the fleet must
// reflect that in /api/v1/fleet without a rebuild, and the by-name addressing
// must stay unambiguous.
func TestFleetAddRemoveProjectVisibleInAPI(t *testing.T) {
	s := NewFleet(nil, "127.0.0.1", 0, false)

	fleetNames := func() []string {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/fleet", nil)
		rec := httptest.NewRecorder()
		s.HandlerForTest().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /api/v1/fleet = %d, want 200", rec.Code)
		}
		var resp struct {
			Projects []struct {
				Name string `json:"name"`
			} `json:"projects"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode fleet response: %v", err)
		}
		names := make([]string, 0, len(resp.Projects))
		for _, p := range resp.Projects {
			names = append(names, p.Name)
		}
		return names
	}

	if got := fleetNames(); len(got) != 0 {
		t.Fatalf("empty fleet should list 0 projects, got %v", got)
	}

	s.AddProject(NewFleetProjectWithGitHubNamed("alpha", &config.Config{Repo: "owner/alpha"}))
	if got := fleetNames(); len(got) != 1 || got[0] != "alpha" {
		t.Fatalf("after add, fleet = %v, want [alpha]", got)
	}

	// Re-adding the same name replaces in place rather than duplicating.
	s.AddProject(NewFleetProjectWithGitHubNamed("alpha", &config.Config{Repo: "owner/alpha"}))
	if got := fleetNames(); len(got) != 1 {
		t.Fatalf("re-adding same name should not duplicate, fleet = %v", got)
	}

	if !s.RemoveProject("alpha") {
		t.Fatal("RemoveProject(alpha) = false, want true")
	}
	if got := fleetNames(); len(got) != 0 {
		t.Fatalf("after remove, fleet = %v, want []", got)
	}

	if s.RemoveProject("alpha") {
		t.Fatal("RemoveProject of a missing project should report false")
	}
}
