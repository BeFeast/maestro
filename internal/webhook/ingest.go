package webhook

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/befeast/maestro/internal/webhookstore"
)

// maxBodyBytes caps the request body the ingestor reads. GitHub documents a
// 25 MiB maximum webhook payload; anything larger is refused (413) rather than
// read into memory unbounded. The request is rejected as OVERSIZE before the
// signature is checked — otherwise a truncated body would fail signature
// validation and surface a misleading "401 invalid signature" for what is really
// a size problem, which GitHub would then retry indefinitely.
const maxBodyBytes = 25 << 20

// DefaultPath is the endpoint path the fleet daemon serves webhook ingestion on
// when the operator does not override it. It lives under the existing /api/v1
// namespace so it shares the fleet server's port (:8786) without a second
// listener (#824 scope: "HTTP endpoint under the existing fleet daemon, path
// configurable").
const DefaultPath = "/api/v1/webhooks/github"

// Store is the persistence surface the Ingestor needs. *webhookstore.Store
// satisfies it; the interface keeps the handler testable with an in-memory fake
// and documents exactly the store methods ingestion touches.
type Store interface {
	Insert(ctx context.Context, d webhookstore.Delivery) (bool, error)
	Count(ctx context.Context) (int, error)
	CountsByEventType(ctx context.Context) (map[string]int, error)
	// LastDelivery returns the time and event type of the most recent stored
	// delivery so restart seeding can restore both diagnostics, not just the time.
	LastDelivery(ctx context.Context) (time.Time, string, error)
}

// Ingestor is the HTTP handler that authenticates, deduplicates and persists
// inbound GitHub webhook deliveries. It is safe for concurrent use: the store
// serialises writes through a single SQLite connection, and the in-memory
// observability counters are guarded by a mutex, so the endpoint keeps working
// while the daemon is under normal fleet load (#824 acceptance).
type Ingestor struct {
	store  Store
	secret string
	now    func() time.Time

	mu sync.Mutex
	// Session counters. accepted/duplicates/byEventType are seeded from the
	// durable store on construction so a daemon restart reports the persisted
	// totals, not zero; signatureFailures and badRequests are transient
	// per-process (an unstored, unauthenticated delivery has nothing durable to
	// count). lastDeliveryAt tracks the most recent accepted or duplicate
	// delivery for the "last webhook delivery time" diagnostic (#824 AC 5/10).
	accepted          int64
	duplicates        int64
	signatureFailures int64
	badRequests       int64
	byEventType       map[string]int64
	lastDeliveryAt    time.Time
	lastEventType     string
}

// NewIngestor builds an Ingestor over store with the given webhook secret and
// seeds its counters from the durable store so observability survives a restart.
// A seed failure is non-fatal (counters simply start from zero) — the endpoint
// must come up even if the initial reconcile query hiccups.
func NewIngestor(store Store, secret string) *Ingestor {
	in := &Ingestor{
		store:       store,
		secret:      strings.TrimSpace(secret),
		now:         func() time.Time { return time.Now().UTC() },
		byEventType: make(map[string]int64),
	}
	in.seed(context.Background())
	return in
}

// seed loads durable totals from the store into the in-memory counters so a
// restart does not reset the "total deliveries" and per-event-type diagnostics.
func (in *Ingestor) seed(ctx context.Context) {
	if in.store == nil {
		return
	}
	total, err := in.store.Count(ctx)
	if err != nil {
		log.Printf("[webhook] seed delivery count failed (starting counters from zero): %v", err)
		return
	}
	byType, err := in.store.CountsByEventType(ctx)
	if err != nil {
		log.Printf("[webhook] seed per-event counts failed: %v", err)
		byType = nil
	}
	last, lastEvent, err := in.store.LastDelivery(ctx)
	if err != nil {
		log.Printf("[webhook] seed last-delivery time failed: %v", err)
	}
	in.mu.Lock()
	defer in.mu.Unlock()
	in.accepted = int64(total)
	in.byEventType = make(map[string]int64, len(byType))
	for k, v := range byType {
		in.byEventType[k] = int64(v)
	}
	if !last.IsZero() {
		in.lastDeliveryAt = last.UTC()
		// Restore the event type alongside the time so a restart does not report a
		// last-delivery timestamp with an empty event type (#824 observability).
		in.lastEventType = lastEvent
	}
}

// SecretConfigured reports whether the ingestor has a webhook secret. Without
// one every delivery fails signature validation (fail-closed), so the daemon
// logs a loud warning rather than silently accepting nothing.
func (in *Ingestor) SecretConfigured() bool {
	return in != nil && in.secret != ""
}

// ServeHTTP handles POST <path>: validate signature, dedupe on
// X-GitHub-Delivery, persist. The ordering is deliberate — signature validation
// fires BEFORE the body is parsed or stored, so an invalid-signature delivery is
// counted and rejected with 401 without ever touching the store (#824 AC:
// "Invalid signature → 401, counted, payload not stored").
func (in *Ingestor) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}

	// Read one byte past the cap so an oversize body is DETECTED (not silently
	// truncated). A truncated body would fail signature validation and be reported
	// as a bogus 401; reject it up front as 413 so operators see the real cause and
	// GitHub gets a distinct, non-retryable-as-signature-failure status.
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes+1))
	if err != nil {
		in.countBadRequest()
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": fmt.Sprintf("read body: %v", err)})
		return
	}
	if int64(len(body)) > maxBodyBytes {
		in.countBadRequest()
		log.Printf("[webhook] rejected delivery: payload exceeds %d bytes (event=%q delivery=%q)",
			maxBodyBytes, r.Header.Get(HeaderEvent), r.Header.Get(HeaderDelivery))
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "payload too large"})
		return
	}

	// Signature first: an attacker on the LAN must not be able to enumerate
	// delivery-id state or plant payloads. Fail closed on any mismatch.
	sig := r.Header.Get(HeaderSignature)
	if !VerifySignature(in.secret, body, sig) {
		in.countSignatureFailure()
		log.Printf("[webhook] rejected delivery: invalid or missing %s (event=%q delivery=%q)",
			HeaderSignature, r.Header.Get(HeaderEvent), r.Header.Get(HeaderDelivery))
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid signature"})
		return
	}

	deliveryID := strings.TrimSpace(r.Header.Get(HeaderDelivery))
	if deliveryID == "" {
		in.countBadRequest()
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": HeaderDelivery + " header is required"})
		return
	}
	eventType := strings.TrimSpace(r.Header.Get(HeaderEvent))

	env := ParseEnvelope(body)
	delivery := webhookstore.Delivery{
		DeliveryID: deliveryID,
		EventType:  eventType,
		Action:     env.Action,
		Repo:       env.Repo,
		Sender:     env.Sender,
		HookID:     strings.TrimSpace(r.Header.Get(HeaderHookID)),
		ReceivedAt: in.now(),
		Payload:    body,
	}

	stored, err := in.store.Insert(r.Context(), delivery)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": fmt.Sprintf("store delivery: %v", err)})
		return
	}

	if !stored {
		// Redelivery of an already-stored X-GitHub-Delivery. Ack with 200 and no
		// side effect so GitHub stops retrying (#824 AC: "replaying the same
		// X-GitHub-Delivery does not duplicate").
		in.countDuplicate(eventType, delivery.ReceivedAt)
		writeJSON(w, http.StatusOK, map[string]string{
			"status":      "duplicate",
			"delivery_id": deliveryID,
			"event":       eventType,
		})
		return
	}

	in.countAccepted(eventType, delivery.ReceivedAt)
	// One journal line per accepted delivery gives the "last webhook delivery
	// time and counts" diagnostic straight from journalctl (#824 AC 10). The
	// payload itself is never logged.
	log.Printf("[webhook] stored delivery event=%q action=%q repo=%q delivery=%s",
		eventType, env.Action, env.Repo, deliveryID)
	writeJSON(w, http.StatusAccepted, map[string]string{
		"status":      "stored",
		"delivery_id": deliveryID,
		"event":       eventType,
	})
}

func (in *Ingestor) countSignatureFailure() {
	in.mu.Lock()
	in.signatureFailures++
	in.mu.Unlock()
}

func (in *Ingestor) countBadRequest() {
	in.mu.Lock()
	in.badRequests++
	in.mu.Unlock()
}

func (in *Ingestor) countAccepted(eventType string, at time.Time) {
	in.mu.Lock()
	in.accepted++
	if in.byEventType == nil {
		in.byEventType = make(map[string]int64)
	}
	in.byEventType[eventType]++
	in.lastDeliveryAt = at.UTC()
	in.lastEventType = eventType
	in.mu.Unlock()
}

func (in *Ingestor) countDuplicate(eventType string, at time.Time) {
	in.mu.Lock()
	in.duplicates++
	in.lastDeliveryAt = at.UTC()
	in.lastEventType = eventType
	in.mu.Unlock()
}

// Stats is the observability snapshot surfaced to diagnostics (fleet API and
// journal). All counts are cumulative for the process except Accepted /
// ByEventType, which are seeded from the durable store so they reflect the
// persisted total across restarts.
type Stats struct {
	LastDeliveryAt    time.Time
	LastEventType     string
	Accepted          int64
	Duplicates        int64
	SignatureFailures int64
	BadRequests       int64
	ByEventType       map[string]int64
}

// Stats returns a snapshot of the observability counters.
func (in *Ingestor) Stats() Stats {
	in.mu.Lock()
	defer in.mu.Unlock()
	byType := make(map[string]int64, len(in.byEventType))
	for k, v := range in.byEventType {
		byType[k] = v
	}
	return Stats{
		LastDeliveryAt:    in.lastDeliveryAt,
		LastEventType:     in.lastEventType,
		Accepted:          in.accepted,
		Duplicates:        in.duplicates,
		SignatureFailures: in.signatureFailures,
		BadRequests:       in.badRequests,
		ByEventType:       byType,
	}
}

// SortedEventTypes returns the event types in a deterministic order, for callers
// that render the per-event-type counters.
func (s Stats) SortedEventTypes() []string {
	types := make([]string, 0, len(s.ByEventType))
	for k := range s.ByEventType {
		types = append(types, k)
	}
	sort.Strings(types)
	return types
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	// Best-effort; a broken client connection is not actionable here.
	_ = json.NewEncoder(w).Encode(v)
}
