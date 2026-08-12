package webhook

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestGiteaOriginDetectorsEachHeaderAlone asserts every documented detector
// header is sufficient on its own. Forgejo/Gitea send the whole family on every
// delivery, but the guard must not depend on that — a proxy that strips some
// headers must still be caught by whichever one survives.
func TestGiteaOriginDetectorsEachHeaderAlone(t *testing.T) {
	// The exact list the spec fixes, plus the siblings verified against Forgejo
	// services/webhook/shared/payloader.go and Gitea services/webhook/deliver.go.
	want := []string{
		"X-Gitea-Event",
		"X-Forgejo-Event",
		"X-Gitea-Event-Type",
		"X-Gitea-Hook-Installation-Target-Type",
		"X-Gitea-Signature",
		"X-Gogs-Event",
		"X-Forgejo-Event-Type",
		"X-Forgejo-Delivery",
		"X-Forgejo-Signature",
		"X-Gitea-Delivery",
		"X-Gogs-Event-Type",
		"X-Gogs-Delivery",
		"X-Gogs-Signature",
	}
	detectors := GiteaOriginHeaderNames()
	have := make(map[string]bool, len(detectors))
	for _, name := range detectors {
		have[name] = true
	}
	for _, name := range want {
		if !have[name] {
			t.Errorf("detector list is missing %q", name)
		}
	}
	if len(detectors) != len(want) {
		t.Errorf("detector list = %d entries, want %d: %v", len(detectors), len(want), detectors)
	}

	// The accessor hands back a copy: mutating it must not disarm the guard.
	if len(detectors) > 0 {
		detectors[0] = "X-Not-A-Detector"
		h := http.Header{}
		h.Set(HeaderForgejoEvent, "push")
		if _, ok := GiteaOriginHeader(h); !ok {
			t.Fatal("mutating the returned detector list disarmed the guard — accessor must return a copy")
		}
		detectors = GiteaOriginHeaderNames()
	}

	for _, name := range detectors {
		t.Run(name, func(t *testing.T) {
			h := http.Header{}
			// Every delivery also carries the GitHub-aliased headers; the detector
			// must fire despite them.
			h.Set(HeaderEvent, "push")
			h.Set(HeaderDelivery, "uuid-1")
			h.Set(name, "value")
			got, ok := GiteaOriginHeader(h)
			if !ok {
				t.Fatalf("GiteaOriginHeader(%s alone) = not detected", name)
			}
			if got != name {
				t.Fatalf("GiteaOriginHeader reported %q, want %q", got, name)
			}
			if !IsGiteaOrigin(h) {
				t.Fatalf("IsGiteaOrigin(%s alone) = false", name)
			}
		})
	}
}

// TestGiteaOriginIgnoresGitHubOnlyDelivery pins the false-positive side: a real
// GitHub delivery (including the sha1 X-Hub-Signature and the GitHub-prefixed
// event-type header, both deliberately NOT detectors) must not be flagged.
func TestGiteaOriginIgnoresGitHubOnlyDelivery(t *testing.T) {
	h := http.Header{}
	h.Set(HeaderEvent, "pull_request")
	h.Set(HeaderDelivery, "b1f0e2ac-0000-4000-8000-000000000000")
	h.Set(HeaderSignature, "sha256=deadbeef")
	h.Set(HeaderHookID, "42")
	h.Set("X-Hub-Signature", "sha1=deadbeef")
	h.Set("X-GitHub-Event-Type", "pull_request")
	h.Set("X-GitHub-Hook-Installation-Target-Type", "repository")
	h.Set("User-Agent", "GitHub-Hookshot/044aadd")
	if name, ok := GiteaOriginHeader(h); ok {
		t.Fatalf("GitHub-only delivery flagged as gitea-origin via %q", name)
	}

	if _, ok := GiteaOriginHeader(nil); ok {
		t.Fatal("nil header flagged as gitea-origin")
	}
	if _, ok := GiteaOriginHeader(http.Header{}); ok {
		t.Fatal("empty header flagged as gitea-origin")
	}
}

// TestGiteaOriginHeaderLookupIsCaseInsensitive pins http.Header.Get semantics:
// both Set and Get canonicalise the name they are given, so however a caller
// spells the header the detector still fires. This is a property of the map
// accessors, not of the wire: a header parsed off a real request is already
// canonical, whatever capitalisation Forgejo/Gitea put on the wire. A
// present-but-empty header is not a detector.
func TestGiteaOriginHeaderLookupIsCaseInsensitive(t *testing.T) {
	for _, spelling := range []string{"x-gitea-event", "X-GITEA-EVENT", "X-Gitea-Event"} {
		h := http.Header{}
		h.Set(spelling, "push")
		if _, ok := GiteaOriginHeader(h); !ok {
			t.Errorf("spelling %q not detected", spelling)
		}
	}

	// Documented limit of Get-based lookup: a map key written RAW (bypassing Set)
	// in non-canonical form is invisible to Get. Unreachable on the ingest path —
	// net/http canonicalises every header it parses, so r.Header never holds such
	// a key — but pinned here so nobody reads the case-insensitivity above as a
	// promise about hand-built maps.
	raw := http.Header{"x-gitea-event": {"push"}}
	if _, ok := GiteaOriginHeader(raw); ok {
		t.Fatal("raw non-canonical map key detected — behaviour changed; update this test and the ingest-path reasoning")
	}

	blank := http.Header{}
	blank.Set(HeaderGiteaEvent, "   ")
	if _, ok := GiteaOriginHeader(blank); ok {
		t.Fatal("blank X-Gitea-Event counted as a detector")
	}
}

// TestIngestRejectsGiteaOriginDelivery is the G1 acceptance: a correctly SIGNED
// Forgejo delivery (Forgejo uses the identical sha256= HMAC scheme, so the
// signature is genuinely valid) is refused with 422 — not stored, not projected,
// no after-accepted wake — and counted in ForgeRejected.
func TestIngestRejectsGiteaOriginDelivery(t *testing.T) {
	in, store := newTestIngestor(t)
	proj := &recordingProjector{}
	in.SetProjector(proj)
	var woke []string
	in.SetAfterAccepted(func(eventType, repo string) { woke = append(woke, eventType) })

	body := []byte(`{"action":"completed","repository":{"full_name":"BeFeast/maestro"}}`)
	req := signedRequest(t, DefaultPath, "check_run", "forgejo-del-1", body, true)
	req.Header.Set(HeaderForgejoEvent, "check_run")
	req.Header.Set(HeaderGiteaEvent, "check_run")
	req.Header.Set(HeaderGiteaSignature, "0f0f0f")

	rec := httptest.NewRecorder()
	in.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("gitea-origin delivery: status=%d body=%s, want 422", rec.Code, rec.Body.String())
	}
	var payload map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("response body is not JSON: %v (%s)", err, rec.Body.String())
	}
	if payload["origin"] == "" || payload["error"] == "" || payload["reason"] == "" || payload["remedy"] == "" {
		t.Fatalf("422 body must name cause and remedy, got %v", payload)
	}

	if _, ok, err := store.Get(context.Background(), "forgejo-del-1"); err != nil || ok {
		t.Fatalf("gitea-origin delivery was stored: ok=%v err=%v", ok, err)
	}
	if n, err := store.Count(context.Background()); err != nil || n != 0 {
		t.Fatalf("store count = %d (err=%v), want 0", n, err)
	}
	if events := proj.events(); len(events) != 0 {
		t.Fatalf("gitea-origin delivery was projected: %v", events)
	}
	if len(woke) != 0 {
		t.Fatalf("gitea-origin delivery ran the after-accepted hook: %v", woke)
	}

	stats := in.Stats()
	if stats.ForgeRejected != 1 {
		t.Fatalf("ForgeRejected = %d, want 1 (stats=%+v)", stats.ForgeRejected, stats)
	}
	if stats.Accepted != 0 || stats.Duplicates != 0 || stats.BadRequests != 0 || stats.SignatureFailures != 0 {
		t.Fatalf("rejection leaked into other counters: %+v", stats)
	}
}

// TestIngestGiteaOriginWithBadSignatureIs401 pins the ordering: the origin check
// runs AFTER signature validation, so an unauthenticated caller cannot probe
// which forges this endpoint distinguishes.
func TestIngestGiteaOriginWithBadSignatureIs401(t *testing.T) {
	in, store := newTestIngestor(t)
	body := []byte(`{"action":"opened","repository":{"full_name":"BeFeast/maestro"}}`)
	req := signedRequest(t, DefaultPath, "issues", "forgejo-del-2", body, false)
	req.Header.Set(HeaderGiteaEvent, "issues")

	rec := httptest.NewRecorder()
	in.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("bad-signature gitea-origin delivery: status=%d, want 401", rec.Code)
	}
	stats := in.Stats()
	if stats.SignatureFailures != 1 {
		t.Fatalf("SignatureFailures = %d, want 1", stats.SignatureFailures)
	}
	if stats.ForgeRejected != 0 {
		t.Fatalf("ForgeRejected = %d, want 0 — signature must be checked first", stats.ForgeRejected)
	}
	if n, err := store.Count(context.Background()); err != nil || n != 0 {
		t.Fatalf("store count = %d (err=%v), want 0", n, err)
	}
}

// TestIngestGitHubDeliveryUnaffectedByOriginGuard is the regression arm: adding
// the guard must leave the GitHub happy path byte-identical.
func TestIngestGitHubDeliveryUnaffectedByOriginGuard(t *testing.T) {
	in, store := newTestIngestor(t)
	body := readFixture(t, "issues_opened.json")

	rec := httptest.NewRecorder()
	in.ServeHTTP(rec, signedRequest(t, DefaultPath, "issues", "github-del-1", body, true))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("github delivery: status=%d body=%s, want 202", rec.Code, rec.Body.String())
	}
	if _, ok, err := store.Get(context.Background(), "github-del-1"); err != nil || !ok {
		t.Fatalf("github delivery not stored: ok=%v err=%v", ok, err)
	}
	if stats := in.Stats(); stats.ForgeRejected != 0 || stats.Accepted != 1 {
		t.Fatalf("github delivery counters: %+v", stats)
	}
}
