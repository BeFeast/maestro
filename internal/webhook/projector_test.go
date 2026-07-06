package webhook

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// recordingProjector captures every ProjectWebhook call so a test can assert the
// ingestor projects accepted deliveries (and only those).
type recordingProjector struct {
	mu    sync.Mutex
	calls []string // event types projected
	err   error
}

func (p *recordingProjector) ProjectWebhook(_ context.Context, eventType string, _ []byte, _ time.Time) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, eventType)
	return p.err
}

func (p *recordingProjector) events() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]string, len(p.calls))
	copy(out, p.calls)
	return out
}

// TestProjectorRunsOnAcceptedDeliveryOnce confirms an accepted delivery is
// projected into the mirror exactly once, and a redelivery (already stored) is
// NOT re-projected — the projection rides the first-time store, so GitHub retries
// do not re-run it.
func TestProjectorRunsOnAcceptedDeliveryOnce(t *testing.T) {
	in, _ := newTestIngestor(t)
	proj := &recordingProjector{}
	in.SetProjector(proj)

	body := readFixture(t, "issues_opened.json")

	first := httptest.NewRecorder()
	in.ServeHTTP(first, signedRequest(t, DefaultPath, "issues", "del-1", body, true))
	if first.Code != http.StatusAccepted {
		t.Fatalf("first delivery status=%d", first.Code)
	}

	// Redelivery of the same X-GitHub-Delivery: acknowledged but not re-projected.
	replay := httptest.NewRecorder()
	in.ServeHTTP(replay, signedRequest(t, DefaultPath, "issues", "del-1", body, true))
	if replay.Code != http.StatusOK {
		t.Fatalf("replay status=%d, want 200 duplicate", replay.Code)
	}

	if got := proj.events(); len(got) != 1 || got[0] != "issues" {
		t.Fatalf("projector calls = %v, want exactly one [issues]", got)
	}
}

// TestProjectorErrorIsNonFatal confirms a projection failure does not fail the
// delivery: the raw payload is already durably stored, so the ingestor still
// acknowledges with 202 and the mirror can be reconciled later.
func TestProjectorErrorIsNonFatal(t *testing.T) {
	in, store := newTestIngestor(t)
	in.SetProjector(&recordingProjector{err: errors.New("projection boom")})

	body := readFixture(t, "issues_opened.json")
	rec := httptest.NewRecorder()
	in.ServeHTTP(rec, signedRequest(t, DefaultPath, "issues", "del-err", body, true))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status=%d, want 202 despite projection error", rec.Code)
	}
	if _, ok, _ := store.Get(context.Background(), "del-err"); !ok {
		t.Fatal("raw delivery must still be stored when projection fails")
	}
}

// TestNoProjectorIsPurePhaseB confirms an ingestor without a projector still
// accepts and stores deliveries — the mirror is optional.
func TestNoProjectorIsPurePhaseB(t *testing.T) {
	in, store := newTestIngestor(t)
	body := readFixture(t, "issues_opened.json")
	rec := httptest.NewRecorder()
	in.ServeHTTP(rec, signedRequest(t, DefaultPath, "issues", "del-2", body, true))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status=%d", rec.Code)
	}
	if _, ok, _ := store.Get(context.Background(), "del-2"); !ok {
		t.Fatal("delivery not stored without a projector")
	}
}
