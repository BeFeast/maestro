package worker

import (
	"os"
	"regexp"
	"strings"
)

// rateLimitPatterns holds compiled regexes for detecting rate-limit / quota
// errors in AI CLI output. Each entry pairs a human-readable label with
// its regex.
//
// Supported patterns (case-insensitive):
//   - "You've hit your limit" (Claude web / API)
//   - HTTP 429 status codes
//   - "rate limit exceeded" (generic)
//   - "quota exceeded" (Google / generic)
//   - "too many requests" (HTTP 429 reason)
//   - "resource_exhausted" (gRPC status)
var rateLimitPatterns = []struct {
	label string
	re    *regexp.Regexp
}{
	{"hit_limit", regexp.MustCompile(`(?i)you'?ve hit your limit`)},
	{"http_429", regexp.MustCompile(`\b429\b`)},
	{"rate_limit_exceeded", regexp.MustCompile(`(?i)rate.limit.exceeded`)},
	{"quota_exceeded", regexp.MustCompile(`(?i)quota.exceeded`)},
	{"too_many_requests", regexp.MustCompile(`(?i)too many requests`)},
	{"resource_exhausted", regexp.MustCompile(`(?i)resource[_.\s]?exhausted`)},
}

// rateLimitSubstrings is a flat list of case-insensitive substrings used for
// quick log-file scanning (OutputContainsRateLimit / IsRateLimited).
var rateLimitSubstrings = []string{
	"you've hit your limit",
	"you have hit your limit",
	"rate limit",
	"rate_limit",
	"too many requests",
	"quota exceeded",
	"resource_exhausted",
	"429",
}

// DetectRateLimit scans multi-line output for known rate-limit / quota error
// patterns. Returns true and the matching pattern label on the first match,
// or false and "" if no rate-limit pattern is found.
// Used for real-time detection from live tmux output (running workers).
func DetectRateLimit(output string) (bool, string) {
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		for _, p := range rateLimitPatterns {
			if p.re.MatchString(line) {
				return true, p.label
			}
		}
	}
	return false, ""
}

// OutputContainsRateLimit checks if the given output text contains
// any known rate-limit error patterns (case-insensitive).
// Used for post-mortem detection from log files (dead workers).
func OutputContainsRateLimit(output string) bool {
	lower := strings.ToLower(output)
	for _, pattern := range rateLimitSubstrings {
		if strings.Contains(lower, pattern) {
			return true
		}
	}
	return false
}

// IsRateLimited checks if a log file contains rate-limit error messages.
// It reads the entire file and looks for known rate-limit patterns.
// Used to detect rate-limiting in dead workers for fallback logic.
func IsRateLimited(logFile string) bool {
	if logFile == "" {
		return false
	}
	data, err := os.ReadFile(logFile)
	if err != nil {
		return false
	}
	return OutputContainsRateLimit(string(data))
}
