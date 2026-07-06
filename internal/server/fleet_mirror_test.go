package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/befeast/maestro/internal/github"
	"github.com/befeast/maestro/internal/mirrorstore"
)

// stubReconcileAPI is a minimal mirrorstore.ReconcileReader for driving a real
// reconcile pass so the fleet diagnostics reconcile block can be asserted.
type stubReconcileAPI struct {
	issues []github.Issue
	prs    []github.PR
}

func (s stubReconcileAPI) ListOpenIssues(labels []string) ([]github.Issue, error) {
	return s.issues, nil
}
func (s stubReconcileAPI) ListOpenPRs() ([]github.PR, error)     { return s.prs, nil }
func (s stubReconcileAPI) IsPRMerged(prNumber int) (bool, error) { return false, nil }

func newMirrorStore(t *testing.T) *mirrorstore.Store {
	t.Helper()
	db := filepath.Join(t.TempDir(), "maestro.db")
	store, err := mirrorstore.Open(db)
	if err != nil {
		t.Fatalf("open mirror store: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

// TestFleetSnapshotMirrorStats proves the GitHub mirror diagnostics surface on
// the fleet snapshot (#825 AC 4): per-entity counts and a stale subset against
// the configured horizon.
func TestFleetSnapshotMirrorStats(t *testing.T) {
	store := newMirrorStore(t)
	ctx := context.Background()
	now := time.Now().UTC()

	// One fresh issue and one issue last seen well beyond the horizon.
	if _, err := store.UpsertIssue(ctx, mirrorstore.Issue{Repo: "o/r", Number: 1, LastSeenAt: now, Source: mirrorstore.SourceWebhook}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpsertIssue(ctx, mirrorstore.Issue{Repo: "o/r", Number: 2, LastSeenAt: now.Add(-72 * time.Hour), Source: mirrorstore.SourceWebhook}); err != nil {
		t.Fatal(err)
	}

	fleet := NewFleet(nil, "127.0.0.1", 0, false)
	fleet.SetMirrorStore(store, mirrorstore.DefaultStaleHorizon)

	snap := fleet.snapshot()
	if snap.Mirror == nil {
		t.Fatal("fleet snapshot missing mirror block")
	}
	if !snap.Mirror.Enabled {
		t.Fatal("mirror block should be enabled")
	}
	if snap.Mirror.Counts.Issues != 2 || snap.Mirror.TotalRows != 2 {
		t.Fatalf("unexpected mirror counts: %+v", snap.Mirror)
	}
	if snap.Mirror.Stale.Issues != 1 || snap.Mirror.TotalStale != 1 {
		t.Fatalf("unexpected mirror stale counts: %+v", snap.Mirror)
	}
	if snap.Mirror.StaleHorizon != mirrorstore.DefaultStaleHorizon.String() {
		t.Fatalf("stale horizon = %q, want %q", snap.Mirror.StaleHorizon, mirrorstore.DefaultStaleHorizon.String())
	}

	// The block is also present in the JSON the fleet API serves.
	handler := fleet.HandlerForTest()
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/fleet", nil))
	var payload struct {
		Mirror *fleetMirrorStats `json:"mirror"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode fleet json: %v", err)
	}
	if payload.Mirror == nil || payload.Mirror.TotalRows != 2 || payload.Mirror.TotalStale != 1 {
		t.Fatalf("fleet JSON missing mirror stats: %+v", payload.Mirror)
	}
}

// TestFleetSnapshotReconcileStats proves the reconciliation status surfaces on the
// mirror diagnostics block (#827): a pass that repaired drift shows up per repo,
// with a fleet-wide drift-repair total.
func TestFleetSnapshotReconcileStats(t *testing.T) {
	store := newMirrorStore(t)
	ctx := context.Background()

	// A reconcile that discovers a GitHub-open issue the mirror never saw repairs
	// one row and records a successful pass for the repo.
	api := stubReconcileAPI{issues: []github.Issue{{Number: 1, Title: "hi", State: "open"}}}
	if _, err := mirrorstore.NewReconciler(store, api, "o/r").Reconcile(ctx); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	fleet := NewFleet(nil, "127.0.0.1", 0, false)
	fleet.SetMirrorStore(store, mirrorstore.DefaultStaleHorizon)

	snap := fleet.snapshot()
	if snap.Mirror == nil {
		t.Fatal("fleet snapshot missing mirror block")
	}
	if snap.Mirror.DriftRepairs != 1 {
		t.Fatalf("drift repairs = %d, want 1", snap.Mirror.DriftRepairs)
	}
	var found bool
	for _, rs := range snap.Mirror.Reconcile {
		if rs.Repo == "o/r" {
			found = true
			if rs.Runs != 1 || rs.Repairs != 1 || rs.LastSuccessAt == "" {
				t.Fatalf("unexpected reconcile stat: %+v", rs)
			}
		}
	}
	if !found {
		t.Fatalf("reconcile block missing repo o/r: %+v", snap.Mirror.Reconcile)
	}
}

// TestFleetNoMirrorOmitsStats confirms the block is omitted when the mirror is
// not configured.
func TestFleetNoMirrorOmitsStats(t *testing.T) {
	fleet := NewFleet(nil, "127.0.0.1", 0, false)
	if snap := fleet.snapshot(); snap.Mirror != nil {
		t.Fatalf("mirror block should be nil when unconfigured: %+v", snap.Mirror)
	}
}

// TestFleetMirrorHorizonFallback confirms a non-positive horizon falls back to
// the default rather than making every row look fresh.
func TestFleetMirrorHorizonFallback(t *testing.T) {
	store := newMirrorStore(t)
	fleet := NewFleet(nil, "127.0.0.1", 0, false)
	fleet.SetMirrorStore(store, 0)
	snap := fleet.snapshot()
	if snap.Mirror == nil || snap.Mirror.StaleHorizon != mirrorstore.DefaultStaleHorizon.String() {
		t.Fatalf("expected default horizon fallback, got %+v", snap.Mirror)
	}
}
