// Package webhook implements the fleet daemon's inbound GitHub webhook
// ingestion (epic #811, phase B — issue #824): HMAC-SHA256 signature
// validation, envelope parsing, and an idempotent HTTP handler that lands every
// valid delivery in the shared maestro.db via internal/webhookstore.
//
// This package does not interpret or mutate orchestration state. The handler
// authenticates a delivery, keys it by X-GitHub-Delivery for idempotency, and
// persists it durably; optional projector and after-accept hooks let the daemon
// update its read model and wake the matching reconciliation loop.
package webhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
)

// GitHub delivery header names. Canonicalised (net/http canonicalises header
// keys), so callers read them via http.Header.Get with these exact strings.
const (
	HeaderEvent     = "X-GitHub-Event"
	HeaderDelivery  = "X-GitHub-Delivery"
	HeaderSignature = "X-Hub-Signature-256"
	HeaderHookID    = "X-GitHub-Hook-ID"
)

// Gitea/Forgejo-origin delivery header names (#1172 M5). Forgejo and Gitea
// deliver GitHub-ALIASED headers — X-GitHub-Event, X-GitHub-Delivery and
// X-Hub-Signature-256 with the identical "sha256="+hex HMAC scheme — so a
// Forgejo hook pointed at this endpoint with the same secret is otherwise
// indistinguishable from GitHub and would be stored and projected into the
// GitHub-keyed mirror tables under the bare owner/name key. These headers are
// the unambiguous discriminators: GitHub never sends any of them.
//
// Verified against source, not guessed. The two forges emit OVERLAPPING but not
// identical sets, so the list is their union — do not trim it on the assumption
// that both send all thirteen:
//   - Forgejo services/webhook/shared/payloader.go (AddDefaultHeaders) sends
//     X-Forgejo-{Delivery,Event,Event-Type,Signature},
//     X-Gitea-{Delivery,Event,Event-Type,Signature} and
//     X-Gogs-{Delivery,Event,Event-Type,Signature} on EVERY delivery.
//   - Gitea services/webhook/deliver.go (addDefaultHeaders) sends the X-Gitea-*
//     and X-Gogs-* quartets plus X-Gitea-Hook-Installation-Target-Type, and
//     NO X-Forgejo-* header at all (that prefix is Forgejo-only).
//
// Deliberately NOT detectors: X-Hub-Signature (sha1) and X-GitHub-Event-Type —
// the first is a real historical GitHub header, and the second is GitHub-prefixed,
// so treating either as Gitea-origin risks rejecting genuine GitHub traffic. The
// list errs toward MORE detectors only where the header prefix makes the origin
// unambiguous.
const (
	HeaderForgejoEvent     = "X-Forgejo-Event"
	HeaderForgejoEventType = "X-Forgejo-Event-Type"
	HeaderForgejoDelivery  = "X-Forgejo-Delivery"
	HeaderForgejoSignature = "X-Forgejo-Signature"
	HeaderGiteaEvent       = "X-Gitea-Event"
	HeaderGiteaEventType   = "X-Gitea-Event-Type"
	HeaderGiteaDelivery    = "X-Gitea-Delivery"
	HeaderGiteaSignature   = "X-Gitea-Signature"
	HeaderGiteaHookTarget  = "X-Gitea-Hook-Installation-Target-Type"
	HeaderGogsEvent        = "X-Gogs-Event"
	HeaderGogsEventType    = "X-Gogs-Event-Type"
	HeaderGogsDelivery     = "X-Gogs-Delivery"
	HeaderGogsSignature    = "X-Gogs-Signature"
)

// giteaOriginHeaders is the exact detector list. Named (rather than inlined as
// string literals at the call site) so tests and any future diagnostic enumerate
// the same set the handler enforces. Deliberately UNexported and read only
// through GiteaOriginHeaderNames: an exported slice is mutable package state, so
// any importer could disable the write-side guard with a single assignment and
// no test would fail.
var giteaOriginHeaders = []string{
	HeaderForgejoEvent,
	HeaderForgejoEventType,
	HeaderForgejoDelivery,
	HeaderForgejoSignature,
	HeaderGiteaEvent,
	HeaderGiteaEventType,
	HeaderGiteaDelivery,
	HeaderGiteaSignature,
	HeaderGiteaHookTarget,
	HeaderGogsEvent,
	HeaderGogsEventType,
	HeaderGogsDelivery,
	HeaderGogsSignature,
}

// GiteaOriginHeaderNames returns a copy of the detector list for tests and
// diagnostics. A copy, not the backing slice, so a caller cannot mutate what the
// handler enforces.
func GiteaOriginHeaderNames() []string {
	out := make([]string, len(giteaOriginHeaders))
	copy(out, giteaOriginHeaders)
	return out
}

// GiteaOriginHeader returns the first detector header present on h and whether
// the delivery is Gitea/Forgejo-origin. Lookup goes through http.Header.Get,
// which canonicalises the name it is given, so the caller's capitalisation of
// the constants does not matter. A header present but empty does NOT count as a
// detector — Gitea/Forgejo always populate these with a non-empty event name,
// UUID or digest.
func GiteaOriginHeader(h http.Header) (string, bool) {
	if h == nil {
		return "", false
	}
	for _, name := range giteaOriginHeaders {
		if strings.TrimSpace(h.Get(name)) != "" {
			return name, true
		}
	}
	return "", false
}

// IsGiteaOrigin reports whether h carries any Gitea/Forgejo-origin marker.
func IsGiteaOrigin(h http.Header) bool {
	_, ok := GiteaOriginHeader(h)
	return ok
}

// signaturePrefix is the algorithm tag GitHub prepends to the hex digest in the
// X-Hub-Signature-256 header ("sha256=<hexdigest>"). The older X-Hub-Signature
// (sha1) header is intentionally NOT accepted — GitHub recommends validating
// the sha256 variant exclusively.
const signaturePrefix = "sha256="

// SupportedEvents is the set of GitHub event types this phase captures (#824
// scope: issues, labels, issue comments, pull requests, PR reviews / review
// comments, check runs / suites, statuses, and Projects v2 item events). "ping"
// is included so the delivery GitHub sends when a webhook is first created is
// acknowledged rather than 400'd. A valid-signature delivery of any OTHER event
// type is still stored (Supported gates only observability/routing labels, never
// whether a signed delivery is persisted) so no acknowledged event is lost.
var SupportedEvents = map[string]bool{
	"ping":                        true,
	"issues":                      true,
	"label":                       true,
	"issue_comment":               true,
	"pull_request":                true,
	"pull_request_review":         true,
	"pull_request_review_comment": true,
	"check_run":                   true,
	"check_suite":                 true,
	"status":                      true,
	"projects_v2_item":            true,
}

// Supported reports whether eventType is in the phase-B captured set. It labels
// observability and routing; it does NOT gate persistence — the ingestor stores
// every valid-signature delivery so a not-yet-modelled event type is never
// dropped on the floor.
func Supported(eventType string) bool {
	return SupportedEvents[strings.TrimSpace(eventType)]
}

// VerifySignature reports whether sigHeader is a valid HMAC-SHA256 signature of
// body under secret. sigHeader is the raw X-Hub-Signature-256 value
// ("sha256=<hexdigest>"). The comparison is constant-time (hmac.Equal) so a
// caller cannot time-probe the expected digest, and an empty secret or a
// malformed / missing header always fails closed — an unsigned or
// wrongly-signed delivery is never accepted.
func VerifySignature(secret string, body []byte, sigHeader string) bool {
	if secret == "" {
		return false
	}
	sigHeader = strings.TrimSpace(sigHeader)
	if !strings.HasPrefix(sigHeader, signaturePrefix) {
		return false
	}
	presented := strings.TrimPrefix(sigHeader, signaturePrefix)
	// Decode the presented hex first so hmac.Equal compares raw bytes, not the
	// hex encodings (which differ in length for an invalid header and would let
	// ConstantTimeCompare short-circuit on length).
	presentedMAC, err := hex.DecodeString(presented)
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	expectedMAC := mac.Sum(nil)
	return hmac.Equal(presentedMAC, expectedMAC)
}

// Sign returns the X-Hub-Signature-256 header value GitHub would send for body
// under secret. Used by tests (and any operator diagnostic) to produce a valid
// signature; the ingestion path only ever verifies.
func Sign(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return signaturePrefix + hex.EncodeToString(mac.Sum(nil))
}

// Envelope holds the common fields extracted from a webhook payload for
// indexing and observability. Every GitHub event payload is a JSON object; the
// fields below are present on most event types (repository/sender on all
// repo-scoped events, action on the action-carrying ones). Missing fields
// simply stay empty — parsing never fails the ingestion, since the raw payload
// is stored verbatim regardless.
type Envelope struct {
	Action string
	Repo   string
	Sender string
}

// envelopeShape is the minimal subset of a GitHub webhook payload the ingestor
// denormalises into indexed columns.
type envelopeShape struct {
	Action     string `json:"action"`
	Repository struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
	Sender struct {
		Login string `json:"login"`
	} `json:"sender"`
}

// ParseEnvelope extracts the indexed envelope fields from a raw payload. A body
// that is not a JSON object (or is empty) yields a zero Envelope with no error:
// the raw bytes are still stored, so a malformed or unusual payload degrades to
// "stored, just not denormalised" rather than a rejected delivery.
func ParseEnvelope(body []byte) Envelope {
	var shape envelopeShape
	if err := json.Unmarshal(body, &shape); err != nil {
		return Envelope{}
	}
	return Envelope{
		Action: strings.TrimSpace(shape.Action),
		Repo:   strings.TrimSpace(shape.Repository.FullName),
		Sender: strings.TrimSpace(shape.Sender.Login),
	}
}
