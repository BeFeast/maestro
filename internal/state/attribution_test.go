package state

import (
	"strings"
	"testing"
	"time"
)

func TestFormatAttributionTrailerSingleSegment(t *testing.T) {
	t0 := time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC)
	got := FormatAttributionTrailer([]BackendAttribution{{
		Backend:   "codex",
		Provider:  "openai",
		Model:     "gpt-5.5",
		Effort:    "medium",
		StartedAt: t0,
	}}, t0.Add(12*time.Minute))

	want := "Maestro-Backend: codex openai gpt-5.5 medium (0-end)"
	if got != want {
		t.Fatalf("FormatAttributionTrailer() = %q, want %q", got, want)
	}
}

func TestFormatAttributionTrailerTwoSegmentsInOrder(t *testing.T) {
	t0 := time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC)
	t1 := t0.Add(12 * time.Minute)
	got := FormatAttributionTrailer([]BackendAttribution{
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

	want := "Maestro-Backend: codex openai gpt-5.5 medium (0-12m); claude anthropic opus-4.8 xhigh (12m-end, fallover)"
	if got != want {
		t.Fatalf("FormatAttributionTrailer() = %q, want %q", got, want)
	}
}

func TestAppendAttributionTrailerDoesNotDuplicate(t *testing.T) {
	t0 := time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC)
	attribution := []BackendAttribution{{
		Backend:   "codex",
		Model:     "gpt-5.5",
		StartedAt: t0,
	}}
	once := AppendAttributionTrailer("Refs #656\n", attribution, t0.Add(time.Minute))
	twice := AppendAttributionTrailer(once, attribution, t0.Add(2*time.Minute))

	if twice != once {
		t.Fatalf("trailer duplicated:\nonce=%q\ntwice=%q", once, twice)
	}
	if count := strings.Count(once, AttributionTrailerKey+":"); count != 1 {
		t.Fatalf("trailer count = %d, want 1 in %q", count, once)
	}
}
