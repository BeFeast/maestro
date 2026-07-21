package state

import (
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
