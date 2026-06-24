package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
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

// #768: hot-adding a project whose config sets server.auth.token_env must make
// the fleet start enforcing auth — on both the global read gate (the middleware
// reads the live checker) and the mutating endpoints (which read it directly) —
// without a server rebuild. Removing the last token-bearing project disables it
// again. s.auth reads/writes are synchronized, so this is also a -race probe of
// the AddProject/RemoveProject re-derivation against the handler reads.
func TestFleetHotAddReDerivesAuth(t *testing.T) {
	const tokenEnv = "MAESTRO_TEST_FLEET_TOKEN_768"
	t.Setenv(tokenEnv, "good-token")

	s := NewFleet(nil, "127.0.0.1", 0, false)

	// status hits the global read gate (GET /api/v1/fleet) with the given
	// Authorization header (empty = none).
	status := func(authz string) int {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/fleet", nil)
		if authz != "" {
			req.Header.Set("Authorization", authz)
		}
		rec := httptest.NewRecorder()
		s.HandlerForTest().ServeHTTP(rec, req)
		return rec.Code
	}
	// mutatingStatus hits a mutating endpoint (POST /api/v1/fleet/actions).
	mutatingStatus := func(authz string) int {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/fleet/actions", nil)
		if authz != "" {
			req.Header.Set("Authorization", authz)
		}
		rec := httptest.NewRecorder()
		s.HandlerForTest().ServeHTTP(rec, req)
		return rec.Code
	}

	// No token-bearing project yet → auth disabled: reads are open and the
	// mutating endpoint does not 401.
	if got := status(""); got != http.StatusOK {
		t.Fatalf("read with auth disabled = %d, want 200", got)
	}
	if got := mutatingStatus(""); got == http.StatusUnauthorized {
		t.Fatalf("mutating endpoint 401 with auth disabled, want non-401 (got %d)", got)
	}

	// Hot-add a token-bearing project. AddProject must re-derive auth.
	s.AddProject(NewFleetProjectWithGitHubNamed("secure", &config.Config{
		Repo: "owner/secure",
		Server: config.ServerConfig{
			Auth: config.ServerAuthConfig{TokenEnv: tokenEnv},
		},
	}))

	if got := status(""); got != http.StatusUnauthorized {
		t.Fatalf("read after hot-add of token-bearing project = %d, want 401", got)
	}
	if got := mutatingStatus(""); got != http.StatusUnauthorized {
		t.Fatalf("mutating endpoint after hot-add = %d, want 401", got)
	}
	if got := status("Bearer good-token"); got != http.StatusOK {
		t.Fatalf("read with valid bearer = %d, want 200", got)
	}
	if got := mutatingStatus("Bearer good-token"); got == http.StatusUnauthorized {
		t.Fatalf("mutating endpoint 401 with valid bearer, want non-401 (got %d)", got)
	}

	// Removing the last token-bearing project disables auth again.
	if !s.RemoveProject("secure") {
		t.Fatal("RemoveProject(secure) = false, want true")
	}
	if got := status(""); got != http.StatusOK {
		t.Fatalf("read after removing token-bearing project = %d, want 200 (auth disabled)", got)
	}
	if got := mutatingStatus(""); got == http.StatusUnauthorized {
		t.Fatalf("mutating endpoint 401 after auth disabled, want non-401 (got %d)", got)
	}
}

// #768: AddProject/RemoveProject re-derive s.auth while HTTP handlers read it.
// Run the two concurrently so `go test -race` flags any unsynchronized access.
func TestFleetAuthReadWriteRaceFree(t *testing.T) {
	const tokenEnv = "MAESTRO_TEST_FLEET_TOKEN_768_RACE"
	t.Setenv(tokenEnv, "good-token")

	s := NewFleet(nil, "127.0.0.1", 0, false)
	tokenCfg := &config.Config{
		Repo:   "owner/secure",
		Server: config.ServerConfig{Auth: config.ServerAuthConfig{TokenEnv: tokenEnv}},
	}

	var wg sync.WaitGroup
	// Writers: churn the token-bearing project in and out, re-deriving auth.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			s.AddProject(NewFleetProjectWithGitHubNamed("secure", tokenCfg))
			s.RemoveProject("secure")
		}
	}()
	// Readers: hammer both the read gate and a mutating endpoint.
	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				get := httptest.NewRequest(http.MethodGet, "/api/v1/fleet", nil)
				s.HandlerForTest().ServeHTTP(httptest.NewRecorder(), get)
				post := httptest.NewRequest(http.MethodPost, "/api/v1/fleet/actions", nil)
				s.HandlerForTest().ServeHTTP(httptest.NewRecorder(), post)
			}
		}()
	}
	wg.Wait()
}
