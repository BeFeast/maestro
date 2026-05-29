package worker

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// codexUsageLimitSignature is the exact OpenAI/Codex usage-limit message that
// issue #458 reported slipping past detection. The inserted word "usage" breaks
// the contiguous "you've hit your limit" match, so it must be caught by the
// "(usage )?" group and/or the codex/settings/usage marker.
const codexUsageLimitSignature = "ERROR: You've hit your usage limit. Visit https://chatgpt.com/codex/settings/usage to see your usage. You can try again at May 30th, 2026 8:13 PM."

func TestDetectRateLimit(t *testing.T) {
	tests := []struct {
		name      string
		input     string
		wantHit   bool
		wantLabel string
	}{
		{
			name:      "hit your limit",
			input:     "Error: You've hit your limit for the day.",
			wantHit:   true,
			wantLabel: "hit_limit",
		},
		{
			name:      "hit your limit without apostrophe",
			input:     "Youve hit your limit",
			wantHit:   true,
			wantLabel: "hit_limit",
		},
		{
			name:      "codex usage limit signature (issue #458)",
			input:     codexUsageLimitSignature,
			wantHit:   true,
			wantLabel: "hit_limit",
		},
		{
			name:      "hit your usage limit standalone",
			input:     "You've hit your usage limit.",
			wantHit:   true,
			wantLabel: "hit_limit",
		},
		{
			name:      "HTTP 429 status code",
			input:     "HTTP error 429: Too Many Requests",
			wantHit:   true,
			wantLabel: "http_429",
		},
		{
			name:      "429 in error message",
			input:     "Error: received status 429 from API",
			wantHit:   true,
			wantLabel: "http_429",
		},
		{
			name:      "rate limit exceeded",
			input:     "Rate limit exceeded. Please retry after 60 seconds.",
			wantHit:   true,
			wantLabel: "rate_limit_exceeded",
		},
		{
			name:      "rate limit exceeded case insensitive",
			input:     "RATE LIMIT EXCEEDED",
			wantHit:   true,
			wantLabel: "rate_limit_exceeded",
		},
		{
			name:      "quota exceeded",
			input:     "Error: Quota exceeded for project xyz",
			wantHit:   true,
			wantLabel: "quota_exceeded",
		},
		{
			name:      "too many requests",
			input:     "Too many requests, please slow down",
			wantHit:   true,
			wantLabel: "too_many_requests",
		},
		{
			name:      "resource exhausted gRPC",
			input:     "rpc error: code = ResourceExhausted desc = request limit",
			wantHit:   true,
			wantLabel: "resource_exhausted",
		},
		{
			name:      "multiline with rate limit on later line",
			input:     "Starting task...\nProcessing...\nError: rate limit exceeded\nRetrying...",
			wantHit:   true,
			wantLabel: "rate_limit_exceeded",
		},
		{
			name:    "normal output no rate limit",
			input:   "tokens 50000 (in 10000 / out 40000)\nTask completed successfully.",
			wantHit: false,
		},
		{
			name:    "empty string",
			input:   "",
			wantHit: false,
		},
		{
			name:    "429 as part of larger number — no match",
			input:   "processed 14290 records",
			wantHit: false,
		},
		{
			name:      "429 standalone in error context",
			input:     "status: 429",
			wantHit:   true,
			wantLabel: "http_429",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gotHit, gotLabel := DetectRateLimit(tc.input)
			if gotHit != tc.wantHit {
				t.Errorf("DetectRateLimit() hit = %v, want %v", gotHit, tc.wantHit)
			}
			if tc.wantHit && gotLabel != tc.wantLabel {
				t.Errorf("DetectRateLimit() label = %q, want %q", gotLabel, tc.wantLabel)
			}
			if !tc.wantHit && gotLabel != "" {
				t.Errorf("DetectRateLimit() label = %q, want empty when no hit", gotLabel)
			}
		})
	}
}

func TestOutputContainsRateLimit_ClaudeMessage(t *testing.T) {
	output := "Error: You've hit your limit for Claude. Please wait before trying again."
	if !OutputContainsRateLimit(output) {
		t.Error("should detect Claude rate limit message")
	}
}

func TestOutputContainsRateLimit_CaseInsensitive(t *testing.T) {
	output := "ERROR: YOU'VE HIT YOUR LIMIT"
	if !OutputContainsRateLimit(output) {
		t.Error("should detect rate limit case-insensitively")
	}
}

func TestOutputContainsRateLimit_TooManyRequests(t *testing.T) {
	output := "HTTP 429 Too Many Requests"
	if !OutputContainsRateLimit(output) {
		t.Error("should detect 'too many requests'")
	}
}

func TestOutputContainsRateLimit_QuotaExceeded(t *testing.T) {
	output := "API error: quota exceeded for this billing period"
	if !OutputContainsRateLimit(output) {
		t.Error("should detect 'quota exceeded'")
	}
}

func TestOutputContainsRateLimit_RateLimitUnderscore(t *testing.T) {
	output := `{"error": {"type": "rate_limit_error", "message": "rate limited"}}`
	if !OutputContainsRateLimit(output) {
		t.Error("should detect 'rate_limit'")
	}
}

func TestOutputContainsRateLimit_NoMatch(t *testing.T) {
	output := "Worker completed successfully. All tests passing."
	if OutputContainsRateLimit(output) {
		t.Error("should not detect rate limit in normal output")
	}
}

func TestOutputContainsRateLimit_EmptyString(t *testing.T) {
	if OutputContainsRateLimit("") {
		t.Error("should not detect rate limit in empty string")
	}
}

func TestIsRateLimited_FromFile(t *testing.T) {
	dir := t.TempDir()
	logFile := filepath.Join(dir, "worker.log")
	content := "Starting worker...\nProcessing issue #42\nError: You've hit your limit for Claude.\n"
	if err := os.WriteFile(logFile, []byte(content), 0644); err != nil {
		t.Fatalf("write log file: %v", err)
	}
	if !IsRateLimited(logFile) {
		t.Error("should detect rate limit from log file")
	}
}

func TestIsRateLimited_NoRateLimit(t *testing.T) {
	dir := t.TempDir()
	logFile := filepath.Join(dir, "worker.log")
	content := "Starting worker...\nDone.\n"
	if err := os.WriteFile(logFile, []byte(content), 0644); err != nil {
		t.Fatalf("write log file: %v", err)
	}
	if IsRateLimited(logFile) {
		t.Error("should not detect rate limit in normal log file")
	}
}

func TestIsRateLimited_EmptyPath(t *testing.T) {
	if IsRateLimited("") {
		t.Error("should return false for empty path")
	}
}

func TestIsRateLimited_NonexistentFile(t *testing.T) {
	if IsRateLimited("/nonexistent/path/worker.log") {
		t.Error("should return false for nonexistent file")
	}
}

func TestOutputContainsRateLimit_HTTP429(t *testing.T) {
	output := "Request failed with status 429"
	if !OutputContainsRateLimit(output) {
		t.Error("should detect HTTP 429 status code")
	}
}

func TestOutputContainsRateLimit_ResourceExhausted(t *testing.T) {
	output := "gRPC error: RESOURCE_EXHAUSTED: quota exceeded"
	if !OutputContainsRateLimit(output) {
		t.Error("should detect 'resource_exhausted'")
	}
}

// TestRateLimit_CodexSignature exercises every detection entry point against
// the exact Codex/OpenAI usage-limit string from issue #458, ensuring the
// "usage" variant is recognised end to end.
func TestRateLimit_CodexSignature(t *testing.T) {
	if hit, label := DetectRateLimit(codexUsageLimitSignature); !hit {
		t.Errorf("DetectRateLimit should recognise the codex usage-limit signature, got hit=false")
	} else if label != "hit_limit" {
		t.Errorf("DetectRateLimit label = %q, want %q", label, "hit_limit")
	}

	if !OutputContainsRateLimit(codexUsageLimitSignature) {
		t.Error("OutputContainsRateLimit should recognise the codex usage-limit signature")
	}

	dir := t.TempDir()
	logFile := filepath.Join(dir, "codex.log")
	content := "Starting worker...\nProcessing issue #247\n" + codexUsageLimitSignature + "\n"
	if err := os.WriteFile(logFile, []byte(content), 0644); err != nil {
		t.Fatalf("write log file: %v", err)
	}
	if !IsRateLimited(logFile) {
		t.Error("IsRateLimited should recognise the codex usage-limit signature from a log file")
	}
}

// TestRateLimit_CodexUsageURLOnly verifies the codex/settings/usage marker is
// detected even when the "you've hit your ... limit" phrasing is absent (e.g.
// only the help URL is logged).
func TestRateLimit_CodexUsageURLOnly(t *testing.T) {
	output := "See https://chatgpt.com/codex/settings/usage for details."
	if hit, label := DetectRateLimit(output); !hit {
		t.Error("DetectRateLimit should match the codex/settings/usage marker")
	} else if label != "codex_usage_limit" {
		t.Errorf("DetectRateLimit label = %q, want %q", label, "codex_usage_limit")
	}
	if !OutputContainsRateLimit(output) {
		t.Error("OutputContainsRateLimit should match the codex/settings/usage marker")
	}
}

// TestRateLimit_ClaudeStillMatches guards against regression: the original
// Claude "You've hit your limit" phrasing (no "usage" word) must still match.
func TestRateLimit_ClaudeStillMatches(t *testing.T) {
	output := "You've hit your limit"
	if hit, label := DetectRateLimit(output); !hit {
		t.Error("DetectRateLimit should still match the Claude 'You've hit your limit' phrasing")
	} else if label != "hit_limit" {
		t.Errorf("DetectRateLimit label = %q, want %q", label, "hit_limit")
	}
	if !OutputContainsRateLimit(output) {
		t.Error("OutputContainsRateLimit should still match the Claude phrasing")
	}
}

func TestParseRateLimitReset_CodexSignature(t *testing.T) {
	got, ok := ParseRateLimitReset(codexUsageLimitSignature)
	if !ok {
		t.Fatal("ParseRateLimitReset should parse the reset hint from the codex signature")
	}
	want := time.Date(2026, time.May, 30, 20, 13, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("ParseRateLimitReset = %v, want %v", got, want)
	}
}

func TestParseRateLimitReset_Variants(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  time.Time
		ok    bool
	}{
		{
			name:  "ordinal with PM",
			input: "try again at May 30th, 2026 8:13 PM.",
			want:  time.Date(2026, time.May, 30, 20, 13, 0, 0, time.UTC),
			ok:    true,
		},
		{
			name:  "ordinal AM",
			input: "Please try again at January 1st, 2027 9:05 AM",
			want:  time.Date(2027, time.January, 1, 9, 5, 0, 0, time.UTC),
			ok:    true,
		},
		{
			name:  "try again after wording",
			input: "Rate limited; try again after June 3rd, 2026 12:00 PM.",
			want:  time.Date(2026, time.June, 3, 12, 0, 0, 0, time.UTC),
			ok:    true,
		},
		{
			name:  "iso 8601",
			input: "you've hit your limit; try again at 2026-05-30T20:13:00Z",
			want:  time.Date(2026, time.May, 30, 20, 13, 0, 0, time.UTC),
			ok:    true,
		},
		{
			name:  "no reset hint",
			input: "You've hit your usage limit.",
			ok:    false,
		},
		{
			name:  "unparseable timestamp",
			input: "try again at some point soon.",
			ok:    false,
		},
		{
			name:  "empty",
			input: "",
			ok:    false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := ParseRateLimitReset(tc.input)
			if ok != tc.ok {
				t.Fatalf("ParseRateLimitReset ok = %v, want %v (got %v)", ok, tc.ok, got)
			}
			if tc.ok && !got.Equal(tc.want) {
				t.Errorf("ParseRateLimitReset = %v, want %v", got, tc.want)
			}
		})
	}
}
