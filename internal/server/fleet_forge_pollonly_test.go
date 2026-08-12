package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/webhook"
	"github.com/befeast/maestro/internal/webhookstore"
)

// A forgejo row must advertise its forge and that the fleet-global `webhooks`
// block does not cover it (#1172 M5) — otherwise an operator reads healthy
// global webhook stats as if they applied to a poll-only project.
func TestProjectSnapshotMarksForgejoRowPollOnly(t *testing.T) {
	cfg := &config.Config{
		Repo:     "owner/svc",
		StateDir: t.TempDir(),
		Forge: config.ForgeConfig{
			Kind:    config.ForgeKindForgejo,
			BaseURL: "https://forge.example.com",
		},
	}
	proj := NewFleetProject("svc", "", "", cfg)
	fleet := NewFleet([]FleetProject{proj}, "127.0.0.1", 0, false)

	item, _ := fleet.projectSnapshot(proj, time.Now())
	if item.Forge != "forgejo" {
		t.Fatalf("forge = %q, want forgejo", item.Forge)
	}
	if item.WebhooksApplicable {
		t.Fatal("webhooks_applicable = true for a forgejo row, want false (poll-only)")
	}

	blob, err := json.Marshal(item)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	js := string(blob)
	for _, want := range []string{`"forge":"forgejo"`, `"webhooks_applicable":false`} {
		if !strings.Contains(js, want) {
			t.Fatalf("payload JSON missing %s:\n%s", want, js)
		}
	}
}

// The github arm: both the absent forge block (every legacy row) and an explicit
// kind: github report the historical shape — forge "github", webhooks applicable.
// The fields are never omitted, so the SPA never has to infer poll-only from a
// missing key.
func TestProjectSnapshotGitHubRowShapeUnchanged(t *testing.T) {
	for name, cfg := range map[string]*config.Config{
		"absent forge block": {Repo: "owner/legacy", StateDir: t.TempDir()},
		"explicit github": {
			Repo:     "owner/explicit",
			StateDir: t.TempDir(),
			Forge:    config.ForgeConfig{Kind: config.ForgeKindGitHub},
		},
	} {
		t.Run(name, func(t *testing.T) {
			proj := NewFleetProject("p", "", "", cfg)
			fleet := NewFleet([]FleetProject{proj}, "127.0.0.1", 0, false)

			item, _ := fleet.projectSnapshot(proj, time.Now())
			if item.Forge != "github" {
				t.Fatalf("forge = %q, want github", item.Forge)
			}
			if !item.WebhooksApplicable {
				t.Fatal("webhooks_applicable = false for a github row, want true")
			}
			blob, _ := json.Marshal(item)
			js := string(blob)
			for _, want := range []string{`"forge":"github"`, `"webhooks_applicable":true`} {
				if !strings.Contains(js, want) {
					t.Fatalf("payload JSON missing %s:\n%s", want, js)
				}
			}
		})
	}
}

// A row whose config failed to resolve must not ASSERT webhook coverage: the
// forge kind is a display fallback there, so claiming webhooks_applicable would
// be a guess presented as fact on the one row an operator is already worried
// about (a broken forgejo row would render as github + covered).
func TestProjectSnapshotUnresolvedConfigDoesNotClaimWebhookCoverage(t *testing.T) {
	proj := NewFleetProject("broken", "", "", nil)
	fleet := NewFleet([]FleetProject{proj}, "127.0.0.1", 0, false)

	item, workers := fleet.projectSnapshot(proj, time.Now())
	if workers != nil {
		t.Fatalf("workers = %v, want nil for an unresolved row", workers)
	}
	if item.Error == "" {
		t.Fatal("unresolved row has no error field")
	}
	if item.WebhooksApplicable {
		t.Fatal("webhooks_applicable = true for a row with no resolved config, want false (coverage unknowable)")
	}
	if item.Forge == "" {
		t.Fatal("forge is empty — the field must always be present")
	}
}

// forgeRejectedStore is the minimal webhook.Store the ingestor needs; the
// rejection path never touches it, which the zero counts assert.
type forgeRejectedStore struct{}

func (forgeRejectedStore) Insert(context.Context, webhookstore.Delivery) (bool, error) {
	return false, errors.New("insert must not be reached for a gitea-origin delivery")
}
func (forgeRejectedStore) Count(context.Context) (int, error) { return 0, nil }
func (forgeRejectedStore) CountsByEventType(context.Context) (map[string]int, error) {
	return map[string]int{}, nil
}
func (forgeRejectedStore) LastDelivery(context.Context) (time.Time, string, error) {
	return time.Time{}, "", nil
}

// The new rejection counter must reach the fleet snapshot's `webhooks` block
// under a stable key, so a misconfigured Forgejo hook is visible rather than
// invisible (#1172 M5 / G3).
func TestFleetWebhookStatsSurfaceForgeRejected(t *testing.T) {
	in := webhook.NewIngestor(forgeRejectedStore{}, "shared-secret")
	fleet := NewFleet(nil, "127.0.0.1", 0, false)
	fleet.SetWebhookIngestor(in, webhook.DefaultPath)

	if got := fleet.webhookStatsSnapshot(); got == nil || got.ForgeRejected != 0 {
		t.Fatalf("baseline forge_rejected = %+v, want 0", got)
	}

	body := []byte(`{"action":"completed","repository":{"full_name":"owner/svc"}}`)
	req := httptest.NewRequest(http.MethodPost, webhook.DefaultPath, strings.NewReader(string(body)))
	req.Header.Set(webhook.HeaderEvent, "check_run")
	req.Header.Set(webhook.HeaderDelivery, "fj-1")
	req.Header.Set(webhook.HeaderSignature, webhook.Sign("shared-secret", body))
	req.Header.Set(webhook.HeaderForgejoEvent, "check_run")

	rec := httptest.NewRecorder()
	in.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("gitea-origin delivery status = %d, want 422 (body=%s)", rec.Code, rec.Body.String())
	}

	stats := fleet.webhookStatsSnapshot()
	if stats == nil {
		t.Fatal("webhook stats block missing")
	}
	if stats.ForgeRejected != 1 {
		t.Fatalf("forge_rejected = %d, want 1", stats.ForgeRejected)
	}
	if stats.TotalDeliveries != 0 {
		t.Fatalf("total_deliveries = %d, want 0 — a rejected delivery must not read as healthy ingestion", stats.TotalDeliveries)
	}

	blob, err := json.Marshal(stats)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(blob), `"forge_rejected":1`) {
		t.Fatalf("webhooks block missing forge_rejected key:\n%s", blob)
	}
}
