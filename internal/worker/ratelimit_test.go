package worker

import (
	"os"
	"path/filepath"
	"testing"
)

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
