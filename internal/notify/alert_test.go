package notify

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// capturedPost records one ntfy POST for assertions.
type capturedPost struct {
	topic    string
	title    string
	priority string
	tags     string
	auth     string
	body     string
}

// ntfyRecorder is an httptest server that captures every POST for a topic path.
func ntfyRecorder(t *testing.T) (*httptest.Server, *[]capturedPost, *sync.Mutex) {
	t.Helper()
	var mu sync.Mutex
	var posts []capturedPost
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		buf := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(buf)
		mu.Lock()
		posts = append(posts, capturedPost{
			topic:    strings.TrimPrefix(r.URL.Path, "/"),
			title:    r.Header.Get("Title"),
			priority: r.Header.Get("Priority"),
			tags:     r.Header.Get("Tags"),
			auth:     r.Header.Get("Authorization"),
			body:     string(buf),
		})
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return srv, &posts, &mu
}

func TestRouteFor_ClassToPriorityMapping(t *testing.T) {
	cases := []struct {
		class    AlertClass
		priority int
		wantTag  string
	}{
		{AlertGateFailStreak, 4, "warning"},
		{AlertIdleStall, 4, "hourglass"},
		{AlertFutileRecovery, 4, "recycle"},
		{AlertBackendCooldownExhausted, 4, "battery"},
		{AlertDeliveryAdvance, 3, "rocket"},
		{AlertEmergency, 5, "rotating_light"},
	}
	for _, c := range cases {
		r := RouteFor(c.class)
		if r.Priority != c.priority {
			t.Errorf("RouteFor(%s).Priority = %d, want %d", c.class, r.Priority, c.priority)
		}
		found := false
		for _, tag := range r.Tags {
			if tag == c.wantTag {
				found = true
			}
		}
		if !found {
			t.Errorf("RouteFor(%s).Tags = %v, want to contain %q", c.class, r.Tags, c.wantTag)
		}
	}
}

func TestRouteFor_UnknownClassFallsBackToDefault(t *testing.T) {
	r := RouteFor(AlertClass("does_not_exist"))
	if r.Priority != defaultRoute.Priority {
		t.Errorf("unknown class priority = %d, want default %d", r.Priority, defaultRoute.Priority)
	}
}

func TestAlert_SendsOnePostWithRoutedHeaders(t *testing.T) {
	srv, posts, mu := ntfyRecorder(t)
	n := (&Notifier{}).WithNtfy(srv.URL, "proj-alerts", "")

	if err := n.Alert(AlertDeliveryAdvance, "proj:feed", "feed advanced", "issue #42 delivered"); err != nil {
		t.Fatalf("Alert: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(*posts) != 1 {
		t.Fatalf("got %d posts, want 1", len(*posts))
	}
	p := (*posts)[0]
	if p.topic != "proj-alerts" {
		t.Errorf("topic = %q, want proj-alerts", p.topic)
	}
	if p.title != "feed advanced" {
		t.Errorf("title = %q", p.title)
	}
	if p.priority != strconv.Itoa(RouteFor(AlertDeliveryAdvance).Priority) {
		t.Errorf("priority = %q, want %d", p.priority, RouteFor(AlertDeliveryAdvance).Priority)
	}
	if !strings.Contains(p.tags, "rocket") {
		t.Errorf("tags = %q, want to contain rocket", p.tags)
	}
	if p.body != "issue #42 delivered" {
		t.Errorf("body = %q", p.body)
	}
	if p.auth != "" {
		t.Errorf("auth header set without a token: %q", p.auth)
	}
}

func TestAlert_TokenSetsBearerHeader(t *testing.T) {
	srv, posts, mu := ntfyRecorder(t)
	n := (&Notifier{}).WithNtfy(srv.URL, "topic", "secret-from-env")

	if err := n.Alert(AlertEmergency, "k", "t", "b"); err != nil {
		t.Fatalf("Alert: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if got := (*posts)[0].auth; got != "Bearer secret-from-env" {
		t.Errorf("auth = %q, want Bearer secret-from-env", got)
	}
}

func TestAlert_DedupPerStateChange(t *testing.T) {
	srv, posts, mu := ntfyRecorder(t)
	n := (&Notifier{}).WithNtfy(srv.URL, "topic", "")

	// Same (class,key,body) three times -> one POST.
	for i := 0; i < 3; i++ {
		if err := n.Alert(AlertGateFailStreak, "proj:gate", "gate failing", "3 consecutive failures"); err != nil {
			t.Fatalf("Alert %d: %v", i, err)
		}
	}
	mu.Lock()
	after3 := len(*posts)
	mu.Unlock()
	if after3 != 1 {
		t.Fatalf("repeated identical state produced %d posts, want 1", after3)
	}

	// State changes (body differs) -> a second POST.
	if err := n.Alert(AlertGateFailStreak, "proj:gate", "gate failing", "5 consecutive failures"); err != nil {
		t.Fatalf("Alert changed state: %v", err)
	}
	mu.Lock()
	after4 := len(*posts)
	mu.Unlock()
	if after4 != 2 {
		t.Fatalf("changed state produced total %d posts, want 2", after4)
	}
}

func TestAlert_DifferentKeysAreIndependent(t *testing.T) {
	srv, posts, mu := ntfyRecorder(t)
	n := (&Notifier{}).WithNtfy(srv.URL, "topic", "")

	// Two different projects, same class + same body: distinct dedup identities.
	if err := n.Alert(AlertGateFailStreak, "projA:gate", "gate", "streak"); err != nil {
		t.Fatalf("Alert A: %v", err)
	}
	if err := n.Alert(AlertGateFailStreak, "projB:gate", "gate", "streak"); err != nil {
		t.Fatalf("Alert B: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(*posts) != 2 {
		t.Errorf("distinct keys produced %d posts, want 2", len(*posts))
	}
}

func TestAlert_NoNtfyConfig_IsNoOp(t *testing.T) {
	n := &Notifier{} // no ntfy configured
	if err := n.Alert(AlertEmergency, "k", "t", "b"); err != nil {
		t.Errorf("Alert without ntfy config should return nil, got %v", err)
	}
}

func TestAcceptance_DeliveryAdvanceAndGateFailEachOnePost(t *testing.T) {
	// Mirrors the issue acceptance: with ntfy configured, a manufactured
	// delivery_advance and a gate_fail_streak each produce exactly one POST;
	// repeating the same state produces none.
	srv, posts, mu := ntfyRecorder(t)
	n := (&Notifier{}).WithNtfy(srv.URL, "proj", "")

	_ = n.Alert(AlertDeliveryAdvance, "proj:feed", "advance", "delivered #1")
	_ = n.Alert(AlertGateFailStreak, "proj:gate", "gate", "streak=3")
	// Repeat identical states: no new POSTs.
	_ = n.Alert(AlertDeliveryAdvance, "proj:feed", "advance", "delivered #1")
	_ = n.Alert(AlertGateFailStreak, "proj:gate", "gate", "streak=3")

	mu.Lock()
	defer mu.Unlock()
	if len(*posts) != 2 {
		t.Fatalf("got %d posts, want exactly 2 (one per class, repeats deduped)", len(*posts))
	}
}
