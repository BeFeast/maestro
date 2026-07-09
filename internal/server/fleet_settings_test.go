package server

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/befeast/maestro/internal/configstore"
)

// The real config store must satisfy the fleet's settings write API (#839).
var _ FleetSettingsStore = (*configstore.Store)(nil)

// fakeSettingsStore implements BOTH the project-CRUD and settings write APIs, so
// the settings endpoint (which reaches the store by type-asserting the wired
// project store) can be exercised without a real SQLite store.
type fakeSettingsStore struct {
	fakeProjectStore
	mu       sync.Mutex
	fleetSet map[string]string
	projSet  map[string]string // "project/key" -> value
	deleted  []string
}

func (f *fakeSettingsStore) SetFleetSetting(_ context.Context, key, value, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fleetSet == nil {
		f.fleetSet = map[string]string{}
	}
	f.fleetSet[key] = value
	return nil
}

func (f *fakeSettingsStore) SetProjectSetting(_ context.Context, project, key, value, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.projSet == nil {
		f.projSet = map[string]string{}
	}
	f.projSet[project+"/"+key] = value
	return nil
}

func (f *fakeSettingsStore) DeleteFleetSetting(_ context.Context, key, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted = append(f.deleted, key)
	return nil
}

func postSettings(t *testing.T, srv *FleetServer, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/fleet/settings", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.handleFleetSettings(w, req)
	return w
}

func TestFleetSettingsSetFleetDefault(t *testing.T) {
	store := &fakeSettingsStore{}
	srv := NewFleet(nil, "127.0.0.1", 8786, false)
	srv.SetProjectStore(store)

	w := postSettings(t, srv, `{"key":"supervisor.enabled","value":"false","reason":"idle burn"}`)
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%s", w.Code, w.Body.String())
	}
	if store.fleetSet["supervisor.enabled"] != "false" {
		t.Fatalf("fleetSet = %v, want supervisor.enabled=false", store.fleetSet)
	}
}

func TestFleetSettingsSetProjectOverride(t *testing.T) {
	store := &fakeSettingsStore{}
	srv := NewFleet(nil, "127.0.0.1", 8786, false)
	srv.SetProjectStore(store)

	w := postSettings(t, srv, `{"key":"worker_max_tokens","value":"400000","project":"svc"}`)
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%s", w.Code, w.Body.String())
	}
	if store.projSet["svc/worker_max_tokens"] != "400000" {
		t.Fatalf("projSet = %v, want svc/worker_max_tokens=400000", store.projSet)
	}
	if len(store.fleetSet) != 0 {
		t.Fatal("a per-project override must not touch the fleet defaults")
	}
}

func TestFleetSettingsUnsetFleetDefault(t *testing.T) {
	store := &fakeSettingsStore{}
	srv := NewFleet(nil, "127.0.0.1", 8786, false)
	srv.SetProjectStore(store)

	w := postSettings(t, srv, `{"key":"supervisor.enabled","unset":true}`)
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%s", w.Code, w.Body.String())
	}
	if len(store.deleted) != 1 || store.deleted[0] != "supervisor.enabled" {
		t.Fatalf("deleted = %v, want [supervisor.enabled]", store.deleted)
	}
}

func TestFleetSettingsUnsetProjectRejected(t *testing.T) {
	store := &fakeSettingsStore{}
	srv := NewFleet(nil, "127.0.0.1", 8786, false)
	srv.SetProjectStore(store)

	w := postSettings(t, srv, `{"key":"supervisor.enabled","project":"svc","unset":true}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (unset is fleet-only); body=%s", w.Code, w.Body.String())
	}
}

func TestFleetSettingsRejectsBadValue(t *testing.T) {
	store := &fakeSettingsStore{}
	srv := NewFleet(nil, "127.0.0.1", 8786, false)
	srv.SetProjectStore(store)

	w := postSettings(t, srv, `{"key":"worker_max_tokens","value":"banana"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 on bad value; body=%s", w.Code, w.Body.String())
	}
	if len(store.fleetSet) != 0 {
		t.Fatal("a rejected value must not be written")
	}
}

func TestFleetSettingsRejectsUnknownKey(t *testing.T) {
	store := &fakeSettingsStore{}
	srv := NewFleet(nil, "127.0.0.1", 8786, false)
	srv.SetProjectStore(store)

	w := postSettings(t, srv, `{"key":"not.a.key","value":"1"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 on unknown key; body=%s", w.Code, w.Body.String())
	}
}

func TestFleetSettingsRequiresKey(t *testing.T) {
	store := &fakeSettingsStore{}
	srv := NewFleet(nil, "127.0.0.1", 8786, false)
	srv.SetProjectStore(store)

	w := postSettings(t, srv, `{"value":"false"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 when key missing; body=%s", w.Code, w.Body.String())
	}
}

func TestFleetSettingsReadOnlyForbidden(t *testing.T) {
	store := &fakeSettingsStore{}
	srv := NewFleet(nil, "127.0.0.1", 8786, true) // read-only
	srv.SetProjectStore(store)

	w := postSettings(t, srv, `{"key":"supervisor.enabled","value":"false"}`)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", w.Code, w.Body.String())
	}
	if len(store.fleetSet) != 0 {
		t.Fatal("read-only fleet must not write the store")
	}
}

func TestFleetSettingsWithoutStoreReturns501(t *testing.T) {
	srv := NewFleet(nil, "127.0.0.1", 8786, false) // no SetProjectStore

	w := postSettings(t, srv, `{"key":"supervisor.enabled","value":"false"}`)
	if w.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501; body=%s", w.Code, w.Body.String())
	}
}

// A project store that only does CRUD (no settings) must not accidentally accept
// settings writes.
func TestFleetSettingsProjectOnlyStoreReturns501(t *testing.T) {
	srv := NewFleet(nil, "127.0.0.1", 8786, false)
	srv.SetProjectStore(&fakeProjectStore{}) // no settings methods

	w := postSettings(t, srv, `{"key":"supervisor.enabled","value":"false"}`)
	if w.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501 for a CRUD-only store; body=%s", w.Code, w.Body.String())
	}
}

func TestFleetSettingsRequiresAuth(t *testing.T) {
	store := &fakeSettingsStore{}
	srv := NewFleet(nil, "127.0.0.1", 8786, false)
	srv.SetProjectStore(store)
	srv.SetAuthForTest("s3cr3t", "ci")

	// No credential → 401, store untouched.
	w := postSettings(t, srv, `{"key":"supervisor.enabled","value":"false"}`)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %d, want 401; body=%s", w.Code, w.Body.String())
	}
	if len(store.fleetSet) != 0 {
		t.Fatal("unauthenticated request must not write the store")
	}

	// Bearer token → accepted.
	req := httptest.NewRequest(http.MethodPost, "/api/v1/fleet/settings", bytes.NewBufferString(`{"key":"supervisor.enabled","value":"false"}`))
	req.Header.Set("Authorization", "Bearer s3cr3t")
	wa := httptest.NewRecorder()
	srv.handleFleetSettings(wa, req)
	if wa.Code != http.StatusAccepted {
		t.Fatalf("authenticated status = %d, want 202; body=%s", wa.Code, wa.Body.String())
	}
}

func TestFleetSettingsMethodNotAllowed(t *testing.T) {
	store := &fakeSettingsStore{}
	srv := NewFleet(nil, "127.0.0.1", 8786, false)
	srv.SetProjectStore(store)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/fleet/settings", nil)
	w := httptest.NewRecorder()
	srv.handleFleetSettings(w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405; body=%s", w.Code, w.Body.String())
	}
}
