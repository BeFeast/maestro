package state

import (
	"strings"
	"testing"
	"time"
)

// #713: a session that died because its backend's configured model was
// unavailable (with no fallback) must render as backend_model_unavailable —
// distinct from backend_auth_failure (credentials) and the rate-limit token —
// so operators see "the model is gone" (swap the model id) rather than a code
// failure or an auth outage.
func TestSessionDisplayStatus_BackendModelUnavailable(t *testing.T) {
	now := time.Now().UTC()
	sess := &Session{
		Status:               StatusDead,
		Backend:              "claude",
		RateLimitHit:         true,
		ProviderLimitBackend: "claude",
		ProviderLimitReason:  BackendBlockModelUnavailable,
	}
	if got := SessionDisplayStatusForAt(sess, nil, now); got != string(DisplayBackendModelUnavailable) {
		t.Fatalf("display status = %q, want %q", got, DisplayBackendModelUnavailable)
	}

	// An auth failure keeps its own distinct token.
	sess.ProviderLimitReason = BackendBlockAuthFailure
	if got := SessionDisplayStatusForAt(sess, nil, now); got != string(DisplayBackendAuthFailure) {
		t.Fatalf("display status = %q, want %q", got, DisplayBackendAuthFailure)
	}

	// A scheduled retry takes precedence over the backend-block token.
	sess.ProviderLimitReason = BackendBlockModelUnavailable
	retryAt := now.Add(time.Minute)
	sess.NextRetryAt = &retryAt
	if got := SessionDisplayStatusForAt(sess, nil, now); got != string(StatusDead) {
		t.Fatalf("display status = %q, want %q", got, StatusDead)
	}
}

func TestSessionAttention_BackendModelUnavailable(t *testing.T) {
	sess := &Session{
		Status:               StatusDead,
		Backend:              "claude",
		RateLimitHit:         true,
		ProviderLimitBackend: "claude",
		ProviderLimitReason:  BackendBlockModelUnavailable,
	}
	attention := SessionAttentionFor(sess, nil)
	if !attention.NeedsAttention {
		t.Fatal("model-unavailable session with no fallback must need attention")
	}
	if !strings.Contains(attention.Reason, "configured model") {
		t.Fatalf("attention reason = %q, want model wording", attention.Reason)
	}
	if !strings.Contains(attention.NextAction, "retry budget was not consumed") {
		t.Fatalf("next action = %q, want retry-budget note", attention.NextAction)
	}
}
