package worker

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// #805 live signature (BeFeast/ok-folio fleet, 2026-07-02): the codex CLI
// printed this and exited; the daemon saw only "pid dead, tmux missing" and
// burned the whole per-issue retry budget respawning on the same dead backend.
const codexUsageLimitDeath = "You've hit your usage limit. Upgrade to Pro (https://chatgpt.com/codex/settings/usage) or try again at 12:30 PM."

func TestDetectUsageLimit_KnownSignatures(t *testing.T) {
	cases := []struct {
		name  string
		input string
		label string
	}{
		{"codex hit usage limit", codexUsageLimitDeath, "hit_limit"},
		{"claude hit limit", "You've hit your limit", "hit_limit"},
		{"codex usage url only", "See chatgpt.com/codex/settings/usage for details", "codex_usage_limit"},
		{"claude usage limit reached", "Claude usage limit reached. Your limit will reset at 3am", "usage_limit_reached"},
		{"claude 5-hour window", "5-hour limit reached ∙ resets 3am", "usage_limit_reached"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hit, label := DetectUsageLimit(tc.input, nil)
			if !hit {
				t.Fatalf("DetectUsageLimit(%q) = false, want hit", tc.input)
			}
			if label != tc.label {
				t.Errorf("label = %q, want %q", label, tc.label)
			}
		})
	}
}

// The generic capacity signals stay excluded: they are transient (the #663
// false-positive class), not an account-level quota exhaustion. They remain
// covered by the provider-limit path, which requires a parseable reset.
func TestDetectUsageLimit_GenericRateLimitSignalsNotClassified(t *testing.T) {
	cases := []string{
		"Error: rate limit exceeded.",
		"HTTP 429 too many requests",
		"status: 429",
		"resource_exhausted",
		"quota exceeded",
		codexWriteStdinClosed,
		"processed 14290 records",
	}
	for _, in := range cases {
		if hit, label := DetectUsageLimit(in, nil); hit {
			t.Errorf("DetectUsageLimit(%q) incorrectly hit (label=%q)", in, label)
		}
	}
}

// #805: operators can extend the classifier per backend via
// model.backends.<name>.usage_limit_patterns; the matched label is the regex
// source so BackendHealth.Pattern names which entry fired.
func TestDetectUsageLimit_ExtraPatterns(t *testing.T) {
	extra := []string{`(?i)monthly spend cap reached`}
	hit, label := DetectUsageLimit("ERROR: Monthly spend cap reached for org acme", extra)
	if !hit {
		t.Fatal("DetectUsageLimit should match an operator-supplied extra pattern")
	}
	if label != extra[0] {
		t.Errorf("label = %q, want the regex source %q", label, extra[0])
	}
	if hit, _ := DetectUsageLimit("ordinary worker output", extra); hit {
		t.Error("extra pattern must not match ordinary output")
	}
	// An invalid extra (config parse rejects these, but stay safe) is skipped
	// without breaking the built-in signatures.
	if hit, _ := DetectUsageLimit(codexUsageLimitDeath, []string{"(unclosed"}); !hit {
		t.Error("built-in signatures must keep matching when an extra pattern fails to compile")
	}
}

// IsUsageLimit scans only the log tail: a prompt echo of quota phrasing at the
// top of a long log (a worker legitimately WORKING ON a quota issue) must not
// classify, while the same phrasing as terminal output must.
func TestIsUsageLimit_TailOnly(t *testing.T) {
	dir := t.TempDir()

	tail := filepath.Join(dir, "tail.log")
	if err := os.WriteFile(tail, []byte("Starting worker\nProcessing issue #217\n"+codexUsageLimitDeath+"\n"), 0644); err != nil {
		t.Fatalf("write log: %v", err)
	}
	if hit, label := IsUsageLimit(tail, nil); !hit || label != "hit_limit" {
		t.Fatalf("IsUsageLimit(tail) = %v/%q, want true/hit_limit", hit, label)
	}

	promptEcho := filepath.Join(dir, "echo.log")
	content := "Prompt: fix the handler for the provider message \"" + codexUsageLimitDeath + "\"\n" +
		strings.Repeat("worker made progress on an ordinary line\n", authFailureTailLines+20) +
		"worker exited normally\n"
	if err := os.WriteFile(promptEcho, []byte(content), 0644); err != nil {
		t.Fatalf("write log: %v", err)
	}
	if hit, label := IsUsageLimit(promptEcho, nil); hit {
		t.Fatalf("IsUsageLimit must not classify a prompt echo outside the tail (label=%q)", label)
	}

	if hit, _ := IsUsageLimit("", nil); hit {
		t.Error("empty path must not classify")
	}
	if hit, _ := IsUsageLimit(filepath.Join(dir, "missing.log"), nil); hit {
		t.Error("unreadable file must not classify")
	}
}
