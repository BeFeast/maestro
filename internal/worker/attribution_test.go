package worker

import (
	"testing"
	"time"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/state"
)

func attribCfgWithBackends() *config.Config {
	return &config.Config{
		Model: config.ModelConfig{
			Default: "claude",
			Backends: map[string]config.BackendDef{
				"claude": {
					Cmd:      "claude",
					Provider: "anthropic",
					Model:    "opus-4.8",
					Variant:  "opus[1m]",
					Effort:   "xhigh",
				},
				"codex": {
					Cmd:      "codex",
					Provider: "openai",
					Model:    "gpt-5.5",
					Effort:   "medium",
				},
				"freellm": {
					Cmd: "/srv/example-home/.maestro/bin/maestro-freellm",
					// Intentionally no metadata — backends without
					// declared attribution still work; UI shows "—".
				},
			},
		},
	}
}

func TestRecordBackendAttribution_FirstCall_SnapshotsMetadata(t *testing.T) {
	cfg := attribCfgWithBackends()
	sess := &state.Session{}
	now := time.Date(2026, 5, 31, 17, 0, 0, 0, time.UTC)

	recordBackendAttribution(cfg, sess, "claude", "initial_spawn", "", now)

	if got := len(sess.Attribution); got != 1 {
		t.Fatalf("len(Attribution) = %d, want 1", got)
	}
	seg := sess.Attribution[0]
	if seg.Backend != "claude" {
		t.Fatalf("Backend = %q, want claude", seg.Backend)
	}
	if seg.Provider != "anthropic" || seg.Model != "opus-4.8" || seg.Variant != "opus[1m]" || seg.Effort != "xhigh" {
		t.Fatalf("snapshot = %+v, want anthropic/opus-4.8/opus[1m]/xhigh", seg)
	}
	if seg.Reason != "initial_spawn" {
		t.Fatalf("Reason = %q, want initial_spawn", seg.Reason)
	}
	if seg.EndedAt != nil {
		t.Fatalf("EndedAt = %v, want nil for active segment", seg.EndedAt)
	}
	if !seg.StartedAt.Equal(now) {
		t.Fatalf("StartedAt = %v, want %v", seg.StartedAt, now)
	}
}

func TestBeginSessionAttemptClearsScheduledRetryMarker(t *testing.T) {
	cfg := attribCfgWithBackends()
	retryAt := time.Date(2026, 7, 18, 5, 43, 24, 0, time.UTC)
	sess := &state.Session{Status: state.StatusDead, NextRetryAt: &retryAt, WorkerGeneration: 4}
	now := retryAt.Add(time.Second)

	beginSessionAttempt(cfg, sess, "claude", "in_place_respawn", "ci_failure", now)

	if sess.Status != state.StatusRunning {
		t.Fatalf("status = %q, want %q", sess.Status, state.StatusRunning)
	}
	if sess.NextRetryAt != nil {
		t.Fatalf("next_retry_at = %v, want nil once replacement attempt is running", sess.NextRetryAt)
	}
	if sess.WorkerGeneration != 5 {
		t.Fatalf("worker_generation = %d, want 5", sess.WorkerGeneration)
	}
}

func TestRecordBackendAttribution_SecondCall_ClosesPreviousAndAppends(t *testing.T) {
	cfg := attribCfgWithBackends()
	sess := &state.Session{}

	t0 := time.Date(2026, 5, 31, 17, 0, 0, 0, time.UTC)
	t1 := t0.Add(12 * time.Minute)

	recordBackendAttribution(cfg, sess, "claude", "initial_spawn", "", t0)
	recordBackendAttribution(cfg, sess, "codex", "fallover", "fallover", t1)

	if got := len(sess.Attribution); got != 2 {
		t.Fatalf("len(Attribution) = %d, want 2", got)
	}

	prev := sess.Attribution[0]
	if prev.EndedAt == nil || !prev.EndedAt.Equal(t1) {
		t.Fatalf("prev.EndedAt = %v, want %v", prev.EndedAt, t1)
	}
	if prev.EndReason != "fallover" {
		t.Fatalf("prev.EndReason = %q, want fallover", prev.EndReason)
	}

	next := sess.Attribution[1]
	if next.Backend != "codex" || next.Provider != "openai" || next.Model != "gpt-5.5" || next.Effort != "medium" {
		t.Fatalf("next segment = %+v, want codex/openai/gpt-5.5/medium", next)
	}
	if next.Reason != "fallover" {
		t.Fatalf("next.Reason = %q, want fallover", next.Reason)
	}
	if next.EndedAt != nil {
		t.Fatalf("next.EndedAt should still be open")
	}
}

func TestRecordBackendAttribution_BackendWithoutMetadata_StillWorks(t *testing.T) {
	cfg := attribCfgWithBackends()
	sess := &state.Session{}
	now := time.Date(2026, 5, 31, 17, 0, 0, 0, time.UTC)

	recordBackendAttribution(cfg, sess, "freellm", "fallover", "fallover", now)

	if got := len(sess.Attribution); got != 1 {
		t.Fatalf("len(Attribution) = %d, want 1", got)
	}
	seg := sess.Attribution[0]
	if seg.Backend != "freellm" {
		t.Fatalf("Backend = %q, want freellm", seg.Backend)
	}
	// All metadata fields are absent -> empty strings -> omitempty in JSON.
	if seg.Provider != "" || seg.Model != "" || seg.Variant != "" || seg.Effort != "" {
		t.Fatalf("expected empty metadata, got %+v", seg)
	}
}

func TestRecordBackendAttribution_NilSession_NoOp(t *testing.T) {
	cfg := attribCfgWithBackends()
	// Should not panic.
	recordBackendAttribution(cfg, nil, "claude", "initial_spawn", "", time.Now())
}

func TestRecordBackendAttribution_EmptyBackendName_NoOp(t *testing.T) {
	cfg := attribCfgWithBackends()
	sess := &state.Session{}
	recordBackendAttribution(cfg, sess, "", "initial_spawn", "", time.Now())
	if got := len(sess.Attribution); got != 0 {
		t.Fatalf("len(Attribution) = %d, want 0 (empty backendName must be no-op)", got)
	}
}

func TestRecordBackendAttribution_UnknownBackend_NoMetadata(t *testing.T) {
	cfg := attribCfgWithBackends()
	sess := &state.Session{}
	now := time.Date(2026, 5, 31, 17, 0, 0, 0, time.UTC)

	// Backend name not in cfg.Model.Backends — segment is still added,
	// just without metadata snapshot.
	recordBackendAttribution(cfg, sess, "ghost", "initial_spawn", "", now)

	if got := len(sess.Attribution); got != 1 {
		t.Fatalf("len(Attribution) = %d, want 1 (unknown backend still records segment)", got)
	}
	seg := sess.Attribution[0]
	if seg.Backend != "ghost" {
		t.Fatalf("Backend = %q, want ghost", seg.Backend)
	}
	if seg.Provider != "" || seg.Model != "" {
		t.Fatalf("unknown backend should have empty metadata, got %+v", seg)
	}
}
