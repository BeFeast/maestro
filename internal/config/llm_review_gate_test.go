package config

import (
	"reflect"
	"testing"
)

// review_gate: llm-review (#1148) normalizes to the pair of model streams.
func TestParse_ReviewGateLLMReviewDefaultsToPair(t *testing.T) {
	yaml := `
repo: owner/repo
review_gate: llm-review
`
	cfg, err := parse([]byte(yaml))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if cfg.ReviewGate != "llm-review" {
		t.Fatalf("ReviewGate = %q, want llm-review", cfg.ReviewGate)
	}
	want := []string{"llm-review-opus", "llm-review-terra"}
	if got := cfg.EffectiveReviewGateStreams(); !reflect.DeepEqual(got, want) {
		t.Fatalf("EffectiveReviewGateStreams() = %#v, want %v", got, want)
	}
}

// The underscore/compact spellings normalize to the canonical gate value.
func TestParse_ReviewGateLLMReviewSpellings(t *testing.T) {
	for _, spelling := range []string{"llm-review", "llm_review", "llmreview", "LLM-Review"} {
		cfg, err := parse([]byte("repo: owner/repo\nreview_gate: " + spelling + "\n"))
		if err != nil {
			t.Fatalf("parse(%q): %v", spelling, err)
		}
		if cfg.ReviewGate != "llm-review" {
			t.Errorf("ReviewGate for %q = %q, want llm-review", spelling, cfg.ReviewGate)
		}
	}
}

// Explicit streams may mix the llm pair alias with other streams; the alias
// expands and dedupes.
func TestParse_ReviewGateStreamsLLMReviewAlias(t *testing.T) {
	yaml := `
repo: owner/repo
review_gate: llm-review
review_gate_streams:
  - llm-review
  - greptile
`
	cfg, err := parse([]byte(yaml))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	want := []string{"llm-review-opus", "llm-review-terra", "greptile"}
	if got := cfg.EffectiveReviewGateStreams(); !reflect.DeepEqual(got, want) {
		t.Fatalf("EffectiveReviewGateStreams() = %#v, want %v", got, want)
	}
}

// Greptile stays the default and existing rows keep working unchanged.
func TestParse_ReviewGateDefaultStillGreptile(t *testing.T) {
	cfg, err := parse([]byte("repo: owner/repo\n"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if cfg.ReviewGate != "greptile" {
		t.Fatalf("ReviewGate = %q, want greptile (default unchanged)", cfg.ReviewGate)
	}
	if got := cfg.EffectiveReviewGateStreams(); !reflect.DeepEqual(got, []string{"greptile"}) {
		t.Fatalf("EffectiveReviewGateStreams() = %#v, want [greptile]", got)
	}
}

// review_gate: none still disables every stream, llm-review included.
func TestParse_ReviewGateNoneDisablesLLMStreams(t *testing.T) {
	yaml := `
repo: owner/repo
review_gate: none
review_gate_streams:
  - llm-review
`
	cfg, err := parse([]byte(yaml))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got := cfg.EffectiveReviewGateStreams(); got != nil {
		t.Fatalf("EffectiveReviewGateStreams() = %#v, want nil for gate none", got)
	}
}

// Unknown stream names are dropped; an all-unknown list falls back to the
// gate's default set — the pair for llm-review.
func TestParse_ReviewGateLLMReviewUnknownStreamsFallBackToPair(t *testing.T) {
	yaml := `
repo: owner/repo
review_gate: llm-review
review_gate_streams:
  - not-a-stream
`
	cfg, err := parse([]byte(yaml))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	want := []string{"llm-review-opus", "llm-review-terra"}
	if got := cfg.EffectiveReviewGateStreams(); !reflect.DeepEqual(got, want) {
		t.Fatalf("EffectiveReviewGateStreams() = %#v, want %v", got, want)
	}
}
