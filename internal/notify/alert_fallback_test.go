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

// Codex review catch (P1): a failed send must NOT record the dedup state, or
// every later cycle reporting the same condition is silenced and the alert is
// lost for good.
func TestAlert_TransportFailureStaysRetryable(t *testing.T) {
	var mu sync.Mutex
	attempts := 0
	fail := true
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		attempts++
		shouldFail := fail
		mu.Unlock()
		if shouldFail {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	n := New(srv.URL, "operator")
	if err := n.Alert(AlertFloorBreach, "proj:floor", "breach", "live=0"); err == nil {
		t.Fatal("expected the transport error to surface")
	}

	mu.Lock()
	fail = false
	mu.Unlock()

	// Same condition next cycle: it must be retried, not deduped away.
	if err := n.Alert(AlertFloorBreach, "proj:floor", "breach", "live=0"); err != nil {
		t.Fatalf("retry failed: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if attempts != 2 {
		t.Fatalf("attempts = %d, want 2 — a failed alert must stay eligible for retry", attempts)
	}
}

// Codex review catch (P2): concurrent identical alerts must still collapse to
// one send — the retry fix must not lose the atomic dedup reservation.
func TestAlert_ConcurrentIdenticalAlertsSendOnce(t *testing.T) {
	var mu sync.Mutex
	sends := 0
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		sends++
		mu.Unlock()
		<-release // hold the first send open so the second call races it
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	n := New(srv.URL, "operator")
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = n.Alert(AlertFloorBreach, "proj:floor", "breach", "live=0")
		}()
	}
	// Give both goroutines time to pass the dedup check before releasing.
	for i := 0; i < 100; i++ {
		mu.Lock()
		started := sends
		mu.Unlock()
		if started > 0 {
			break
		}
	}
	close(release)
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	if sends != 1 {
		t.Fatalf("sends = %d, want 1 — concurrent identical alerts must collapse", sends)
	}
}
