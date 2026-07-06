package webhook

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/befeast/maestro/internal/webhookstore"
)

const testSecret = "webhook-shared-secret"

func newTestIngestor(t *testing.T) (*Ingestor, *webhookstore.Store) {
	t.Helper()
	db := filepath.Join(t.TempDir(), "maestro.db")
	store, err := webhookstore.Open(db)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return NewIngestor(store, testSecret), store
}

// signedRequest builds a POST with a valid (or, when sign=false, a bogus)
// GitHub signature over body.
func signedRequest(t *testing.T, path, event, delivery string, body []byte, sign bool) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(string(body)))
	req.Header.Set(HeaderEvent, event)
	req.Header.Set(HeaderDelivery, delivery)
	req.Header.Set(HeaderHookID, "42")
	if sign {
		req.Header.Set(HeaderSignature, Sign(testSecret, body))
	} else {
		req.Header.Set(HeaderSignature, Sign("wrong-secret", body))
	}
	return req
}

func TestIngestValidStoredOnce(t *testing.T) {
	in, store := newTestIngestor(t)
	body := readFixture(t, "issues_opened.json")

	rec := httptest.NewRecorder()
	in.ServeHTTP(rec, signedRequest(t, DefaultPath, "issues", "del-1", body, true))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("valid delivery: status=%d body=%s", rec.Code, rec.Body.String())
	}

	got, ok, err := store.Get(context.Background(), "del-1")
	if err != nil || !ok {
		t.Fatalf("delivery not stored: ok=%v err=%v", ok, err)
	}
	if got.EventType != "issues" || got.Action != "opened" || got.Repo != "BeFeast/maestro" {
		t.Fatalf("envelope not denormalised: %+v", got)
	}
	if string(got.Payload) != string(body) {
		t.Fatal("raw payload not stored verbatim")
	}

	stats := in.Stats()
	if stats.Accepted != 1 || stats.ByEventType["issues"] != 1 {
		t.Fatalf("counters after one delivery: %+v", stats)
	}
	if stats.LastDeliveryAt.IsZero() {
		t.Fatal("last delivery time not recorded")
	}
}

func TestIngestReplayIsNoOp(t *testing.T) {
	in, store := newTestIngestor(t)
	body := readFixture(t, "issues_opened.json")

	first := httptest.NewRecorder()
	in.ServeHTTP(first, signedRequest(t, DefaultPath, "issues", "dup-1", body, true))
	if first.Code != http.StatusAccepted {
		t.Fatalf("first delivery status=%d", first.Code)
	}

	// Replaying the same X-GitHub-Delivery must ack (200) without duplicating.
	replay := httptest.NewRecorder()
	in.ServeHTTP(replay, signedRequest(t, DefaultPath, "issues", "dup-1", body, true))
	if replay.Code != http.StatusOK {
		t.Fatalf("replay status=%d body=%s", replay.Code, replay.Body.String())
	}
	if !strings.Contains(replay.Body.String(), "duplicate") {
		t.Fatalf("replay body should note duplicate: %s", replay.Body.String())
	}

	n, err := store.Count(context.Background())
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Fatalf("replay duplicated a delivery: count=%d", n)
	}
	stats := in.Stats()
	if stats.Accepted != 1 || stats.Duplicates != 1 {
		t.Fatalf("want accepted=1 duplicates=1, got %+v", stats)
	}
}

func TestIngestInvalidSignatureRejectedAndNotStored(t *testing.T) {
	in, store := newTestIngestor(t)
	body := readFixture(t, "issues_opened.json")

	rec := httptest.NewRecorder()
	in.ServeHTTP(rec, signedRequest(t, DefaultPath, "issues", "bad-sig", body, false))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("invalid signature: status=%d want 401", rec.Code)
	}

	if _, ok, _ := store.Get(context.Background(), "bad-sig"); ok {
		t.Fatal("payload was stored despite invalid signature")
	}
	n, _ := store.Count(context.Background())
	if n != 0 {
		t.Fatalf("store should be empty after rejected delivery, count=%d", n)
	}
	if stats := in.Stats(); stats.SignatureFailures != 1 {
		t.Fatalf("signature failure not counted: %+v", stats)
	}
}

func TestIngestMissingDeliveryID(t *testing.T) {
	in, _ := newTestIngestor(t)
	body := readFixture(t, "issues_opened.json")
	req := signedRequest(t, DefaultPath, "issues", "", body, true)
	req.Header.Del(HeaderDelivery)
	rec := httptest.NewRecorder()
	in.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("missing delivery id: status=%d want 400", rec.Code)
	}
}

// TestIngestOversizePayloadRejected proves an oversize body is rejected as 413
// (payload too large) BEFORE signature validation — not silently truncated and
// then surfaced as a misleading 401 invalid signature. The signature here is
// valid over the full body, so a 401 would mean the size check ran too late.
func TestIngestOversizePayloadRejected(t *testing.T) {
	in, store := newTestIngestor(t)
	body := bytes.Repeat([]byte("a"), maxBodyBytes+1)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, DefaultPath, bytes.NewReader(body))
	req.Header.Set(HeaderEvent, "issues")
	req.Header.Set(HeaderDelivery, "too-big")
	req.Header.Set(HeaderSignature, Sign(testSecret, body))
	in.ServeHTTP(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversize payload: status=%d want 413 body=%s", rec.Code, rec.Body.String())
	}
	if _, ok, _ := store.Get(context.Background(), "too-big"); ok {
		t.Fatal("oversize payload should not be stored")
	}
	if stats := in.Stats(); stats.SignatureFailures != 0 {
		t.Fatalf("oversize should not count as a signature failure: %+v", stats)
	}
}

func TestIngestMethodNotAllowed(t *testing.T) {
	in, _ := newTestIngestor(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, DefaultPath, nil)
	in.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET: status=%d want 405", rec.Code)
	}
}

// TestIngestEventTypeRouting drives one delivery of each fixture event type and
// asserts the per-event-type counters route correctly (#824 AC: event-type
// routing; integration test with sample GitHub payload fixtures).
func TestIngestEventTypeRouting(t *testing.T) {
	in, store := newTestIngestor(t)
	fixtures := []struct {
		event   string
		file    string
		action  string
		delivID string
	}{
		{"issues", "issues_opened.json", "opened", "r-1"},
		{"pull_request", "pull_request_synchronize.json", "synchronize", "r-2"},
		{"check_run", "check_run_completed.json", "completed", "r-3"},
	}
	for _, f := range fixtures {
		body := readFixture(t, f.file)
		rec := httptest.NewRecorder()
		in.ServeHTTP(rec, signedRequest(t, DefaultPath, f.event, f.delivID, body, true))
		if rec.Code != http.StatusAccepted {
			t.Fatalf("%s: status=%d", f.event, rec.Code)
		}
		got, ok, err := store.Get(context.Background(), f.delivID)
		if err != nil || !ok {
			t.Fatalf("%s not stored: ok=%v err=%v", f.event, ok, err)
		}
		if got.Action != f.action {
			t.Fatalf("%s action=%q want %q", f.event, got.Action, f.action)
		}
	}

	stats := in.Stats()
	for _, f := range fixtures {
		if stats.ByEventType[f.event] != 1 {
			t.Fatalf("event %q counter=%d want 1 (stats=%+v)", f.event, stats.ByEventType[f.event], stats)
		}
	}
	if stats.Accepted != 3 {
		t.Fatalf("accepted=%d want 3", stats.Accepted)
	}
}

// TestSeedFromDurableStore proves a fresh ingestor over a store that already
// holds deliveries reports the persisted totals — the "restart does not lose
// acknowledged deliveries" observability (#824 AC).
func TestSeedFromDurableStore(t *testing.T) {
	db := filepath.Join(t.TempDir(), "maestro.db")
	store, err := webhookstore.Open(db)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer store.Close()

	// Land two deliveries directly, then build a new ingestor as if the daemon
	// restarted.
	for _, id := range []string{"seed-a", "seed-b"} {
		if _, err := store.Insert(context.Background(), webhookstore.Delivery{
			DeliveryID: id, EventType: "issues", Payload: []byte("{}"),
		}); err != nil {
			t.Fatalf("seed insert: %v", err)
		}
	}

	in := NewIngestor(store, testSecret)
	stats := in.Stats()
	if stats.Accepted != 2 {
		t.Fatalf("seeded accepted=%d want 2", stats.Accepted)
	}
	if stats.ByEventType["issues"] != 2 {
		t.Fatalf("seeded per-event=%d want 2", stats.ByEventType["issues"])
	}
	// A restart must restore BOTH the last-delivery time and its event type — a
	// non-zero time with an empty event type is the bug this guards.
	if stats.LastDeliveryAt.IsZero() {
		t.Fatal("seeded last delivery time not restored")
	}
	if stats.LastEventType != "issues" {
		t.Fatalf("seeded last event type=%q want issues", stats.LastEventType)
	}
}
