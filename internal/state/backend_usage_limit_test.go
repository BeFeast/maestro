package state

import (
	"strings"
	"testing"
	"time"
)

// #805: a session that died on an account-quota exhaustion with no fallback
// must render as backend_usage_limit (not dead/retry_exhausted, and not the
// generic rate-limit token) so operators see "quota exhausted, wait for the
// window", not a code failure.
func TestSessionDisplayStatus_BackendUsageLimit(t *testing.T) {
	now := time.Now().UTC()
	sess := &Session{
		Status:               StatusDead,
		Backend:              "codex",
		RateLimitHit:         true,
		ProviderLimitBackend: "codex",
		ProviderLimitReason:  BackendBlockUsageLimit,
	}
	if got := SessionDisplayStatusForAt(sess, nil, now); got != string(DisplayBackendUsageLimit) {
		t.Fatalf("display status = %q, want %q", got, DisplayBackendUsageLimit)
	}

	// A scheduled retry takes precedence over the backend-block token.
	retryAt := now.Add(time.Minute)
	sess.NextRetryAt = &retryAt
	if got := SessionDisplayStatusForAt(sess, nil, now); got != string(StatusDead) {
		t.Fatalf("display status = %q, want %q", got, StatusDead)
	}
}

func TestSessionAttention_BackendUsageLimit(t *testing.T) {
	sess := &Session{
		Status:               StatusDead,
		Backend:              "codex",
		RateLimitHit:         true,
		ProviderLimitBackend: "codex",
		ProviderLimitReason:  BackendBlockUsageLimit,
	}
	attention := SessionAttentionFor(sess, nil)
	if !attention.NeedsAttention {
		t.Fatal("quota-dead session with no fallback must need attention")
	}
	if !strings.Contains(attention.Reason, "usage quota") {
		t.Fatalf("attention reason = %q, want usage-quota wording", attention.Reason)
	}
	if !strings.Contains(attention.NextAction, "retry budget was not consumed") {
		t.Fatalf("next action = %q, want retry-budget note", attention.NextAction)
	}
}
