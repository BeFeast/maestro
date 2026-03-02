package worker

import (
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

// DetectRateLimit scans multi-line output for known rate-limit / quota error
// patterns. Returns true and the matching pattern label on the first match,
// or false and "" if no rate-limit pattern is found.
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
