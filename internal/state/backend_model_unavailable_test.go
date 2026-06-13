package state

import (
	"strings"
	"testing"
	"time"
)

// #713: a session that died because its backend's model is unavailable (with
// no fallback) must render as backend_model_unavailable — distinct from the
// auth-failure and rate-limit tokens — so operators see "the model is gone"
// and reach for the right fix (swap the model id), not a credential refresh.
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

	// An auth failure keeps its own token.
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
	if !strings.Contains(attention.Reason, "model is unavailable") {
		t.Fatalf("attention reason = %q, want model-unavailable wording", attention.Reason)
	}
	if !strings.Contains(attention.NextAction, "Swap the model id") {
		t.Fatalf("next action = %q, want swap-the-model-id remediation", attention.NextAction)
	}
	if !strings.Contains(attention.NextAction, "retry budget was not consumed") {
		t.Fatalf("next action = %q, want retry-budget note", attention.NextAction)
	}
}

// #713: model-unavailable deaths set RateLimitHit, so FailedAttemptsForIssue
// must exclude them — N such deaths leave the issue fully retryable.
func TestFailedAttemptsForIssue_ExcludesModelUnavailable(t *testing.T) {
	s := NewState()
	for i := 0; i < 4; i++ {
		s.Sessions[strings.Repeat("x", i+1)] = &Session{
			IssueNumber:          192,
			Status:               StatusDead,
			Backend:              "claude",
			RateLimitHit:         true,
			ProviderLimitBackend: "claude",
			ProviderLimitReason:  BackendBlockModelUnavailable,
		}
	}
	if got := s.FailedAttemptsForIssue(192); got != 0 {
		t.Fatalf("FailedAttemptsForIssue(192) = %d, want 0 — model-unavailable deaths must not burn the retry budget", got)
	}
}
