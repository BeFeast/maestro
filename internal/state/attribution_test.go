package state

import (
	"strings"
	"testing"
	"time"
)

func TestFormatAttributionTimelineSingleSegment(t *testing.T) {
	t0 := time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC)
	got := FormatAttributionTimeline([]BackendAttribution{{
		Backend:   "codex",
		Provider:  "openai",
		Model:     "gpt-5.5",
		Effort:    "medium",
		StartedAt: t0,
	}}, t0.Add(12*time.Minute))

	want := "codex openai gpt-5.5 medium (0-end)"
	if got != want {
		t.Fatalf("FormatAttributionTimeline() = %q, want %q", got, want)
	}
}

func TestFormatAttributionTimelineMarksUsageUnreliable(t *testing.T) {
	started := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	got := FormatAttributionTimeline([]BackendAttribution{{
		Backend:         "grok",
		Model:           "grok-4.5",
		UsageUnreliable: true,
		StartedAt:       started,
	}}, started.Add(time.Minute))
	if !strings.Contains(got, "usage-unreliable") {
		t.Fatalf("FormatAttributionTimeline() = %q, want usage-unreliable marker", got)
	}
}

func TestMarkActiveAttributionUsageUnreliableIsMonotonic(t *testing.T) {
	sess := &Session{Attribution: []BackendAttribution{{Backend: "grok"}}}
	if !MarkActiveAttributionUsageUnreliable(sess, "live_assistant_zero_input_or_output", UsageUnreliableScopeLiveBudget) {
		t.Fatal("first mark did not report a durable change")
	}
	if !SessionUsageUnreliable(sess) {
		t.Fatal("session did not report unreliable usage")
	}
	if got := sess.Attribution[0].UsageUnreliableReason; got != "live_assistant_zero_input_or_output" {
		t.Fatalf("reason = %q, want live assistant degradation", got)
	}
	if SessionUsageAccountingUnreliable(sess) {
		t.Fatal("live-budget-only degradation marked accounting totals unreliable")
	}
	if !MarkActiveAttributionUsageUnreliable(sess, "terminal_result_zero_input_or_output", UsageUnreliableScopeAccounting) {
		t.Fatal("accounting degradation did not upgrade the live-budget marker")
	}
	if got := sess.Attribution[0].UsageUnreliableReason; got != "terminal_result_zero_input_or_output" {
		t.Fatalf("reason = %q, want upgraded accounting reason", got)
	}
	if !SessionUsageAccountingUnreliable(sess) {
		t.Fatal("accounting degradation was not reported")
	}
	if MarkActiveAttributionUsageUnreliable(sess, "later_reason", UsageUnreliableScopeLiveBudget) {
		t.Fatal("weaker repeat mark replaced accounting degradation")
	}
}

func TestMarkActiveAttributionUsageUnreliableCreatesLegacySegment(t *testing.T) {
	started := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	sess := &Session{Backend: "claude", Model: "proxy-model", StartedAt: started}
	if !MarkActiveAttributionUsageUnreliable(sess, "terminal_result_missing_usage", UsageUnreliableScopeAccounting) {
		t.Fatal("legacy session was not marked")
	}
	if len(sess.Attribution) != 1 {
		t.Fatalf("attribution len = %d, want 1", len(sess.Attribution))
	}
	seg := sess.Attribution[0]
	if seg.Backend != "claude" || seg.Model != "proxy-model" || seg.Reason != "usage_observation" || !seg.UsageUnreliable || seg.UsageUnreliableScope != UsageUnreliableScopeAccounting {
		t.Fatalf("legacy attribution = %+v", seg)
	}
}

func TestFormatAttributionTimelineTwoSegmentsInOrder(t *testing.T) {
	t0 := time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC)
	t1 := t0.Add(12 * time.Minute)
	got := FormatAttributionTimeline([]BackendAttribution{
		{
			Backend:   "codex",
			Provider:  "openai",
			Model:     "gpt-5.5",
			Effort:    "medium",
			StartedAt: t0,
			EndedAt:   &t1,
			EndReason: "fallover",
		},
		{
			Backend:   "claude",
			Provider:  "anthropic",
			Model:     "opus-4.8",
			Effort:    "xhigh",
			StartedAt: t1,
		},
	}, t1.Add(4*time.Minute))

	want := "codex openai gpt-5.5 medium (0-12m); claude anthropic opus-4.8 xhigh (12m-end, fallover)"
	if got != want {
		t.Fatalf("FormatAttributionTimeline() = %q, want %q", got, want)
	}
}
