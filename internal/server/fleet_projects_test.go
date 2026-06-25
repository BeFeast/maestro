package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/befeast/maestro/internal/configstore"
)

// The real config store must satisfy the fleet's project-CRUD write API so the
// daemon can wire it (#761). A signature drift breaks this at compile time.
var _ FleetProjectStore = (*configstore.Store)(nil)

// fakeProjectStore is an in-memory FleetProjectStore so the dashboard
// project-CRUD endpoint (#761) can be exercised without a real SQLite store.
type fakeProjectStore struct {
	mu        sync.Mutex
	upserts   map[string]string
	deleted   []string
	upsertErr error
	deleteErr error
}

func (f *fakeProjectStore) UpsertProject(_ context.Context, name, configYAML string) error {
	if f.upsertErr != nil {
		return f.upsertErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.upserts == nil {
		f.upserts = map[string]string{}
	}
	f.upserts[name] = configYAML
	return nil
}

func (f *fakeProjectStore) DeleteProject(_ context.Context, name string) error {
	if f.deleteErr != nil {
		return f.deleteErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted = append(f.deleted, name)
	return nil
}

const fleetProjectYAML = "repo: owner/svc\nmax_parallel: 2\n"

func TestFleetProjectUpsertWritesStore(t *testing.T) {
	store := &fakeProjectStore{}
	srv := NewFleet(nil, "127.0.0.1", 8786, false)
	srv.SetProjectStore(store)

	body := `{"config_yaml":"repo: owner/svc\nmax_parallel: 2\n","reason":"add svc from UI"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/fleet/projects", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.handleFleetProjects(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%s", w.Code, w.Body.String())
	}
	// The name is derived the same way the config store does: sanitised repo.
	if got, ok := store.upserts["owner-svc"]; !ok {
		t.Fatalf("store did not receive owner-svc; got %v", store.upserts)
	} else if got == "" {
		t.Fatal("store received empty config_yaml")
	}
}

func TestFleetProjectUpsertHonoursExplicitName(t *testing.T) {
	store := &fakeProjectStore{}
	srv := NewFleet(nil, "127.0.0.1", 8786, false)
	srv.SetProjectStore(store)

	body := `{"name":"custom","config_yaml":"repo: owner/svc\nmax_parallel: 2\n"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/fleet/projects", bytes.NewBufferString(body))
	w := httptest.NewRecorder()
	srv.handleFleetProjects(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%s", w.Code, w.Body.String())
	}
	if _, ok := store.upserts["custom"]; !ok {
		t.Fatalf("store did not receive explicit name 'custom'; got %v", store.upserts)
	}
}

func TestFleetProjectUpsertRequiresConfigYAML(t *testing.T) {
	store := &fakeProjectStore{}
	srv := NewFleet(nil, "127.0.0.1", 8786, false)
	srv.SetProjectStore(store)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/fleet/projects", bytes.NewBufferString(`{}`))
	w := httptest.NewRecorder()
	srv.handleFleetProjects(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", w.Code, w.Body.String())
	}
	if len(store.upserts) != 0 {
		t.Fatalf("store should not have been written; got %v", store.upserts)
	}
}

func TestFleetProjectUpsertRejectsInvalidConfig(t *testing.T) {
	store := &fakeProjectStore{}
	srv := NewFleet(nil, "127.0.0.1", 8786, false)
	srv.SetProjectStore(store)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/fleet/projects", bytes.NewBufferString(`{"config_yaml":"::: not yaml :::"}`))
	w := httptest.NewRecorder()
	srv.handleFleetProjects(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", w.Code, w.Body.String())
	}
}

func TestFleetProjectDeleteRemovesFromStore(t *testing.T) {
	store := &fakeProjectStore{}
	srv := NewFleet(nil, "127.0.0.1", 8786, false)
	srv.SetProjectStore(store)

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/fleet/projects", bytes.NewBufferString(`{"name":"owner-svc"}`))
	w := httptest.NewRecorder()
	srv.handleFleetProjects(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%s", w.Code, w.Body.String())
	}
	if len(store.deleted) != 1 || store.deleted[0] != "owner-svc" {
		t.Fatalf("deleted = %v, want [owner-svc]", store.deleted)
	}
}

func TestFleetProjectDeleteRequiresName(t *testing.T) {
	store := &fakeProjectStore{}
	srv := NewFleet(nil, "127.0.0.1", 8786, false)
	srv.SetProjectStore(store)

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/fleet/projects", bytes.NewBufferString(`{}`))
	w := httptest.NewRecorder()
	srv.handleFleetProjects(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", w.Code, w.Body.String())
	}
}

func TestFleetProjectCRUDWithoutStoreReturns501(t *testing.T) {
	srv := NewFleet(nil, "127.0.0.1", 8786, false) // no SetProjectStore

	req := httptest.NewRequest(http.MethodPost, "/api/v1/fleet/projects", bytes.NewBufferString(`{"config_yaml":"`+`repo: owner/svc\n`+`"}`))
	w := httptest.NewRecorder()
	srv.handleFleetProjects(w, req)

	if w.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501; body=%s", w.Code, w.Body.String())
	}
}

func TestFleetProjectCRUDReadOnlyForbidden(t *testing.T) {
	store := &fakeProjectStore{}
	srv := NewFleet(nil, "127.0.0.1", 8786, true) // read-only
	srv.SetProjectStore(store)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/fleet/projects", bytes.NewBufferString(`{"config_yaml":"repo: owner/svc\n"}`))
	w := httptest.NewRecorder()
	srv.handleFleetProjects(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", w.Code, w.Body.String())
	}
	if len(store.upserts) != 0 {
		t.Fatal("read-only fleet must not write the store")
	}
}

func TestFleetProjectCRUDMethodNotAllowed(t *testing.T) {
	store := &fakeProjectStore{}
	srv := NewFleet(nil, "127.0.0.1", 8786, false)
	srv.SetProjectStore(store)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/fleet/projects", nil)
	w := httptest.NewRecorder()
	srv.handleFleetProjects(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405; body=%s", w.Code, w.Body.String())
	}
}

// Auth fires before the read-only / availability gates so an unauthenticated
// mutating request always sees 401 (#487 posture, mirrored for project CRUD).
func TestFleetProjectCRUDRequiresAuth(t *testing.T) {
	store := &fakeProjectStore{}
	srv := NewFleet(nil, "127.0.0.1", 8786, false)
	srv.SetProjectStore(store)
	srv.SetAuthForTest("s3cr3t", "ci")

	// No credential → 401, store untouched.
	unauth := httptest.NewRequest(http.MethodPost, "/api/v1/fleet/projects", bytes.NewBufferString(`{"config_yaml":"repo: owner/svc\n"}`))
	wu := httptest.NewRecorder()
	srv.handleFleetProjects(wu, unauth)
	if wu.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %d, want 401; body=%s", wu.Code, wu.Body.String())
	}
	if len(store.upserts) != 0 {
		t.Fatal("unauthenticated request must not write the store")
	}

	// Bearer token → accepted.
	authReq := httptest.NewRequest(http.MethodPost, "/api/v1/fleet/projects", bytes.NewBufferString(`{"config_yaml":"repo: owner/svc\nmax_parallel: 2\n"}`))
	authReq.Header.Set("Authorization", "Bearer s3cr3t")
	wa := httptest.NewRecorder()
	srv.handleFleetProjects(wa, authReq)
	if wa.Code != http.StatusAccepted {
		t.Fatalf("authenticated status = %d, want 202; body=%s", wa.Code, wa.Body.String())
	}
}

func TestFleetProjectUpsertSurfacesStoreError(t *testing.T) {
	store := &fakeProjectStore{upsertErr: errors.New("disk full")}
	srv := NewFleet(nil, "127.0.0.1", 8786, false)
	srv.SetProjectStore(store)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/fleet/projects", bytes.NewBufferString(`{"config_yaml":"repo: owner/svc\nmax_parallel: 2\n"}`))
	w := httptest.NewRecorder()
	srv.handleFleetProjects(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 on store error; body=%s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if _, ok := resp["error"]; !ok {
		t.Fatalf("expected error field in body; got %s", w.Body.String())
	}
}
