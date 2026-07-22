package state

import (
	"strings"
	"testing"
	"time"
)

func TestReconcileBackendHealth_ClearsOnlyExpiredProviderModelRoute(t *testing.T) {
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	expired := now.Add(-time.Second)
	active := now.Add(time.Hour)
	s := NewState()
	s.ProviderModelHealth["claude"] = map[string]BackendHealth{
		"claude-fable-5": {
			State:      BackendHealthCooldown,
			Reason:     BackendBlockModelCooldown,
			RetryAfter: &expired,
		},
		"claude-sonnet-4-6": {
			State:      BackendHealthCooldown,
			Reason:     BackendBlockModelCooldown,
			RetryAfter: &active,
		},
	}

	if !ReconcileBackendHealth(s, now) {
		t.Fatal("expected expired route to clear")
	}
	if _, ok := s.ProviderModelHealth["claude"]["claude-fable-5"]; ok {
		t.Fatal("expired Fable route survived reconciliation")
	}
	if _, ok := s.ProviderModelHealth["claude"]["claude-sonnet-4-6"]; !ok {
		t.Fatal("active Sonnet route was cleared with Fable")
	}
}

func TestSessionDisplayStatus_BackendModelCooldown(t *testing.T) {
	sess := &Session{
		Status:                StatusDead,
		RateLimitHit:          true,
		ProviderLimitBackend:  "fable",
		ProviderLimitProvider: "claude",
		ProviderLimitModel:    "claude-fable-5",
		ProviderLimitReason:   BackendBlockModelCooldown,
	}
	if got := SessionDisplayStatusForAt(sess, nil, time.Now()); got != string(DisplayBackendModelCooldown) {
		t.Fatalf("display status = %q, want %q", got, DisplayBackendModelCooldown)
	}
	attention := SessionAttentionFor(sess, nil)
	if !attention.NeedsAttention || attention.Reason == "" {
		t.Fatalf("attention = %+v", attention)
	}
}

func TestSessionDisplayStatus_BackendModelOverloaded(t *testing.T) {
	sess := &Session{
		Status:                StatusDead,
		RateLimitHit:          true,
		ProviderLimitBackend:  "fable",
		ProviderLimitProvider: "claude",
		ProviderLimitModel:    "claude-fable-5",
		ProviderLimitReason:   BackendBlockModelOverloaded,
	}
	if got := SessionDisplayStatusForAt(sess, nil, time.Now()); got != string(DisplayBackendModelOverloaded) {
		t.Fatalf("display status = %q, want %q", got, DisplayBackendModelOverloaded)
	}
	attention := SessionAttentionFor(sess, nil)
	if !attention.NeedsAttention || !strings.Contains(attention.Reason, "temporarily overloaded") {
		t.Fatalf("attention = %+v", attention)
	}
}

func TestProviderModelHealthPersistence(t *testing.T) {
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	retry := now.Add(5 * time.Minute)
	s := NewState()
	s.ProviderModelHealth["claude"] = map[string]BackendHealth{
		"claude-fable-5": {
			State:                     BackendHealthCooldown,
			Reason:                    BackendBlockModelCooldown,
			CredentialCandidates:      2,
			CredentialCandidatesKnown: true,
			CredentialUsableKnown:     true,
			AggregateReason:           "all_model_credentials_cooling_down",
			RetryAfter:                &retry,
		},
	}
	dir := t.TempDir()
	if err := Save(dir, s); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	health, ok := loaded.ProviderModelHealth["claude"]["claude-fable-5"]
	if !ok || health.CredentialCandidates != 2 || health.RetryAfter == nil || !health.RetryAfter.Equal(retry) {
		t.Fatalf("persisted health = %+v ok=%v", health, ok)
	}
}
