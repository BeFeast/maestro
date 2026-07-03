package config

import (
	"strings"
	"testing"
)

// #805: usage_limit_patterns extends the built-in quota-death classifier per
// backend; entries are plain regexes validated at parse.
func TestParse_UsageLimitPatterns(t *testing.T) {
	yaml := `
repo: owner/repo
model:
  default: codex
  backends:
    codex:
      cmd: codex
      usage_limit_patterns:
        - "(?i)monthly spend cap reached"
        - "org quota exhausted"
    claude:
      cmd: claude
`
	cfg, err := parse([]byte(yaml))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	got := cfg.Model.Backends["codex"].UsageLimitPatterns
	if len(got) != 2 || got[0] != "(?i)monthly spend cap reached" || got[1] != "org quota exhausted" {
		t.Fatalf("UsageLimitPatterns = %v, want the two configured entries in order", got)
	}
	if len(cfg.Model.Backends["claude"].UsageLimitPatterns) != 0 {
		t.Fatal("claude has no usage_limit_patterns; want empty")
	}
}

// #805: an entry that does not compile fails config parse, so the operator
// learns immediately instead of the classifier silently skipping it.
func TestParse_UsageLimitPatterns_InvalidRegexRejected(t *testing.T) {
	yaml := `
repo: owner/repo
model:
  default: codex
  backends:
    codex:
      cmd: codex
      usage_limit_patterns:
        - "(unclosed"
`
	_, err := parse([]byte(yaml))
	if err == nil {
		t.Fatal("parse should reject an invalid usage_limit_patterns regex")
	}
	if !strings.Contains(err.Error(), "usage_limit_patterns") || !strings.Contains(err.Error(), "(unclosed") {
		t.Fatalf("error should name the field and the offending entry, got: %v", err)
	}
}
