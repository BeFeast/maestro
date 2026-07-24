package notify

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// Codex review catch: without ntfy configured, Alert used to return nil without
// sending — every classified alert (including the CRITICAL floor breach) was
// silently dropped on installs that only use the Telegram/OpenClaw transport.
func TestAlert_FallsBackToBaseTransportWithoutNtfy(t *testing.T) {
	var mu sync.Mutex
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, r.ContentLength)
		r.Body.Read(buf)
		mu.Lock()
		bodies = append(bodies, string(buf))
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	n := New(srv.URL, "operator")
	if n.NtfyConfigured() {
		t.Fatal("precondition: ntfy must be unconfigured")
	}
	if err := n.Alert(AlertFloorBreach, "proj:floor", "maestro floor breach", "live=0 min=5"); err != nil {
		t.Fatalf("Alert: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(bodies) != 1 {
		t.Fatalf("sent %d message(s), want 1 — the alert was dropped", len(bodies))
	}
	if !strings.Contains(bodies[0], string(AlertFloorBreach)) || !strings.Contains(bodies[0], "live=0 min=5") {
		t.Fatalf("payload = %q, want the class and body preserved", bodies[0])
	}
}

// The dedup contract is unchanged on the fallback path.
func TestAlert_FallbackKeepsDedup(t *testing.T) {
	var mu sync.Mutex
	count := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		count++
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	n := New(srv.URL, "operator")
	_ = n.Alert(AlertIdleStall, "proj:idle", "stall", "same body")
	_ = n.Alert(AlertIdleStall, "proj:idle", "stall", "same body")

	mu.Lock()
	defer mu.Unlock()
	if count != 1 {
		t.Fatalf("sent %d, want 1 — dedup must survive the fallback path", count)
	}
}

func TestAlertFallbackText(t *testing.T) {
	if got := alertFallbackText(AlertEmergency, "", ""); got != "[emergency]" {
		t.Fatalf("empty = %q", got)
	}
	if got := alertFallbackText(AlertEmergency, "title", ""); got != "[emergency] title" {
		t.Fatalf("title-only = %q", got)
	}
	if got := alertFallbackText(AlertEmergency, "", "body"); got != "[emergency] body" {
		t.Fatalf("body-only = %q", got)
	}
	if got := alertFallbackText(AlertEmergency, "title", "body"); got != "[emergency] title — body" {
		t.Fatalf("both = %q", got)
	}
}
