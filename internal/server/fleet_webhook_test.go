package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/befeast/maestro/internal/webhook"
	"github.com/befeast/maestro/internal/webhookstore"
)

const fleetWebhookSecret = "fleet-webhook-secret"

func newFleetWithWebhook(t *testing.T) (*FleetServer, *webhookstore.Store) {
	t.Helper()
	db := filepath.Join(t.TempDir(), "maestro.db")
	store, err := webhookstore.Open(db)
	if err != nil {
		t.Fatalf("open webhook store: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	fleet := NewFleet(nil, "127.0.0.1", 0, false)
	fleet.SetWebhookIngestor(webhook.NewIngestor(store, fleetWebhookSecret), webhook.DefaultPath)
	return fleet, store
}

// TestFleetWebhookBypassesAuth proves the webhook endpoint authenticates by
// signature, not the fleet bearer token: with fleet auth ENABLED, a bearer-less
// but correctly-signed delivery still lands (#824), while a normal read endpoint
// is 401'd.
func TestFleetWebhookBypassesAuth(t *testing.T) {
	fleet, _ := newFleetWithWebhook(t)
	fleet.SetAuthForTest("operator-token", "operator")
	handler := fleet.HandlerForTest()

	// A normal endpoint without the bearer token is rejected.
	readRec := httptest.NewRecorder()
	handler.ServeHTTP(readRec, httptest.NewRequest(http.MethodGet, "/api/v1/fleet", nil))
	if readRec.Code != http.StatusUnauthorized {
		t.Fatalf("read endpoint without token: status=%d want 401", readRec.Code)
	}

	// The webhook endpoint with a valid signature but NO bearer token succeeds.
	body := []byte(`{"action":"opened","repository":{"full_name":"BeFeast/maestro"}}`)
	req := httptest.NewRequest(http.MethodPost, webhook.DefaultPath, strings.NewReader(string(body)))
	req.Header.Set(webhook.HeaderEvent, "issues")
	req.Header.Set(webhook.HeaderDelivery, "fleet-del-1")
	req.Header.Set(webhook.HeaderSignature, webhook.Sign(fleetWebhookSecret, body))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("signed webhook delivery: status=%d body=%s", rec.Code, rec.Body.String())
	}
}

// TestFleetWebhookInvalidSignature401 confirms an unsigned/bad-signature
// delivery is 401'd by the ingestor even though it bypassed the fleet auth
// middleware.
func TestFleetWebhookInvalidSignature401(t *testing.T) {
	fleet, store := newFleetWithWebhook(t)
	handler := fleet.HandlerForTest()

	body := []byte(`{"action":"opened"}`)
	req := httptest.NewRequest(http.MethodPost, webhook.DefaultPath, strings.NewReader(string(body)))
	req.Header.Set(webhook.HeaderEvent, "issues")
	req.Header.Set(webhook.HeaderDelivery, "fleet-bad")
	req.Header.Set(webhook.HeaderSignature, webhook.Sign("nope", body))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("bad signature via fleet: status=%d want 401", rec.Code)
	}
	if n, _ := store.Count(req.Context()); n != 0 {
		t.Fatalf("payload stored despite bad signature: count=%d", n)
	}
}

// TestFleetSnapshotWebhookStats proves the ingestion diagnostics surface on the
// fleet snapshot (#824 AC 5/10).
func TestFleetSnapshotWebhookStats(t *testing.T) {
	fleet, _ := newFleetWithWebhook(t)
	handler := fleet.HandlerForTest()

	body := []byte(`{"action":"labeled","repository":{"full_name":"BeFeast/maestro"}}`)
	req := httptest.NewRequest(http.MethodPost, webhook.DefaultPath, strings.NewReader(string(body)))
	req.Header.Set(webhook.HeaderEvent, "issues")
	req.Header.Set(webhook.HeaderDelivery, "stats-1")
	req.Header.Set(webhook.HeaderSignature, webhook.Sign(fleetWebhookSecret, body))
	handler.ServeHTTP(httptest.NewRecorder(), req)

	snap := fleet.snapshot()
	if snap.Webhooks == nil {
		t.Fatal("fleet snapshot missing webhooks block")
	}
	if !snap.Webhooks.Enabled {
		t.Fatal("webhooks block should be enabled")
	}
	if snap.Webhooks.TotalDeliveries != 1 || snap.Webhooks.ByEventType["issues"] != 1 {
		t.Fatalf("unexpected webhook stats: %+v", snap.Webhooks)
	}
	if snap.Webhooks.LastDeliveryAt == "" {
		t.Fatal("webhook stats missing last delivery time")
	}

	// The block is also present in the JSON the fleet API serves.
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/fleet", nil))
	var payload struct {
		Webhooks *fleetWebhookStats `json:"webhooks"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode fleet json: %v", err)
	}
	if payload.Webhooks == nil || payload.Webhooks.TotalDeliveries != 1 {
		t.Fatalf("fleet JSON missing webhook stats: %+v", payload.Webhooks)
	}
}

// TestFleetNoWebhookOmitsStats confirms the block is omitted when ingestion is
// not configured (nil ingestor).
func TestFleetNoWebhookOmitsStats(t *testing.T) {
	fleet := NewFleet(nil, "127.0.0.1", 0, false)
	if snap := fleet.snapshot(); snap.Webhooks != nil {
		t.Fatalf("webhooks block should be nil when ingestion is unconfigured: %+v", snap.Webhooks)
	}
}
