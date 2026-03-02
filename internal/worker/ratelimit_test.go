package worker

import (
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
