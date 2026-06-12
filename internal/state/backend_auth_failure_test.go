package state

import (
	"strings"
	"testing"
	"time"
)

// #693: a session that died on a backend auth failure with no fallback must
// render as backend_auth_failure (not dead/retry_exhausted, and not the
// rate-limit token) so operators see a credential outage, not a code failure.
func TestSessionDisplayStatus_BackendAuthFailure(t *testing.T) {
	now := time.Now().UTC()
	sess := &Session{
		Status:               StatusDead,
		Backend:              "claude",
		RateLimitHit:         true,
		ProviderLimitBackend: "claude",
		ProviderLimitReason:  BackendBlockAuthFailure,
	}
	if got := SessionDisplayStatusForAt(sess, nil, now); got != string(DisplayBackendAuthFailure) {
		t.Fatalf("display status = %q, want %q", got, DisplayBackendAuthFailure)
	}

	// A provider capacity limit keeps the existing token.
	sess.ProviderLimitReason = BackendBlockProviderLimit
	if got := SessionDisplayStatusForAt(sess, nil, now); got != string(DisplayBackendRateLimited) {
		t.Fatalf("display status = %q, want %q", got, DisplayBackendRateLimited)
	}

	// A scheduled retry takes precedence over the backend-block token.
	sess.ProviderLimitReason = BackendBlockAuthFailure
	retryAt := now.Add(time.Minute)
	sess.NextRetryAt = &retryAt
	if got := SessionDisplayStatusForAt(sess, nil, now); got != string(StatusDead) {
		t.Fatalf("display status = %q, want %q", got, StatusDead)
	}
}

func TestSessionAttention_BackendAuthFailure(t *testing.T) {
	sess := &Session{
		Status:               StatusDead,
		Backend:              "claude",
		RateLimitHit:         true,
		ProviderLimitBackend: "claude",
		ProviderLimitReason:  BackendBlockAuthFailure,
	}
	attention := SessionAttentionFor(sess, nil)
	if !attention.NeedsAttention {
		t.Fatal("auth-failed session with no fallback must need attention")
	}
	if !strings.Contains(attention.Reason, "failed authentication") {
		t.Fatalf("attention reason = %q, want authentication wording", attention.Reason)
	}
	if !strings.Contains(attention.NextAction, "retry budget was not consumed") {
		t.Fatalf("next action = %q, want retry-budget note", attention.NextAction)
	}
}
