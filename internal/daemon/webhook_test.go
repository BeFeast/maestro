package daemon

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/befeast/maestro/internal/server"
	"github.com/befeast/maestro/internal/webhook"
)

func TestReadWebhookSecret(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "secret")
	if err := os.WriteFile(path, []byte("  top-secret\n"), 0o600); err != nil {
		t.Fatalf("write secret: %v", err)
	}
	got, err := readWebhookSecret(path)
	if err != nil {
		t.Fatalf("read secret: %v", err)
	}
	if got != "top-secret" {
		t.Fatalf("secret = %q, want trimmed %q", got, "top-secret")
	}
}

func TestReadWebhookSecretEmptyAndMissing(t *testing.T) {
	dir := t.TempDir()
	empty := filepath.Join(dir, "empty")
	if err := os.WriteFile(empty, []byte("   \n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := readWebhookSecret(empty); err == nil {
		t.Fatal("expected error for empty secret file")
	}
	if _, err := readWebhookSecret(filepath.Join(dir, "does-not-exist")); err == nil {
		t.Fatal("expected error for missing secret file")
	}
}

// TestConfigureWebhookIngestionWiresEndpoint proves the daemon-level wiring:
// given a secret file, configureWebhookIngestion opens a store and registers a
// live ingestion endpoint on the fleet server that accepts a signed delivery.
func TestConfigureWebhookIngestionWiresEndpoint(t *testing.T) {
	dir := t.TempDir()
	secretPath := filepath.Join(dir, "secret")
	const secret = "wire-me"
	if err := os.WriteFile(secretPath, []byte(secret), 0o600); err != nil {
		t.Fatalf("write secret: %v", err)
	}

	fleet := server.NewFleet(nil, "127.0.0.1", 0, false)
	configureWebhookIngestion(fleet, Options{
		WebhookSecretFile: secretPath,
		WebhookDBPath:     filepath.Join(dir, "maestro.db"),
		WebhookPath:       webhook.DefaultPath,
	})

	body := []byte(`{"action":"opened","repository":{"full_name":"BeFeast/maestro"}}`)
	req := httptest.NewRequest(http.MethodPost, webhook.DefaultPath, strings.NewReader(string(body)))
	req.Header.Set(webhook.HeaderEvent, "issues")
	req.Header.Set(webhook.HeaderDelivery, "wire-1")
	req.Header.Set(webhook.HeaderSignature, webhook.Sign(secret, body))
	rec := httptest.NewRecorder()
	fleet.HandlerForTest().ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("wired endpoint: status=%d body=%s", rec.Code, rec.Body.String())
	}
}

// TestConfigureWebhookIngestionDisabledWithoutSecret confirms no endpoint is
// registered when no secret file is configured — the delivery path 404s / falls
// through to the dashboard rather than accepting unsigned input.
func TestConfigureWebhookIngestionDisabledWithoutSecret(t *testing.T) {
	fleet := server.NewFleet(nil, "127.0.0.1", 0, false)
	configureWebhookIngestion(fleet, Options{})

	req := httptest.NewRequest(http.MethodPost, webhook.DefaultPath, strings.NewReader("{}"))
	req.Header.Set(webhook.HeaderDelivery, "nope")
	rec := httptest.NewRecorder()
	fleet.HandlerForTest().ServeHTTP(rec, req)
	if rec.Code == http.StatusAccepted {
		t.Fatal("delivery accepted despite no configured webhook secret")
	}
}
