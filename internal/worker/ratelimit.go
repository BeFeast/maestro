package worker

import (
	"os"
	"regexp"
	"strings"
	"time"
)

// rateLimitPatterns holds compiled regexes for detecting rate-limit / quota
// errors in AI CLI output. Each entry pairs a human-readable label with
// its regex.
//
// Supported patterns (case-insensitive):
//   - "You've hit your limit" (Claude web / API)
//   - "You've hit your usage limit" (Codex / OpenAI) — the inserted "usage"
//     word is matched by the optional "(usage )?" group, so both Claude and
//     Codex signatures hit the same label
//   - "chatgpt.com/codex/settings/usage" marker (Codex usage-limit URL)
//   - HTTP 429 status codes
//   - "rate limit exceeded" (generic)
//   - "quota exceeded" (Google / generic)
//   - "too many requests" (HTTP 429 reason)
//   - "resource_exhausted" (gRPC status)
var rateLimitPatterns = []struct {
	label string
	re    *regexp.Regexp
}{
	{"hit_limit", regexp.MustCompile(`(?i)you'?ve hit your (usage )?limit`)},
	{"codex_usage_limit", regexp.MustCompile(`(?i)(chatgpt\.com/)?codex/settings/usage`)},
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
	"you've hit your usage limit",
	"you have hit your usage limit",
	"chatgpt.com/codex/settings/usage",
	"codex/settings/usage",
	"rate limit",
	"rate_limit",
	"too many requests",
	"quota exceeded",
	"resource_exhausted",
	"429",
}

// rateLimitResetRe extracts the human-readable reset hint emitted after a
// provider usage-limit error, e.g.
//
//	"... try again at May 30th, 2026 8:13 PM."
//
// The capture is greedy to the end of the line; parseResetTimestamp trims a
// trailing sentence period and surrounding whitespace before parsing.
var rateLimitResetRe = regexp.MustCompile(`(?i)try again (?:at|after)\s+([^\n]+)`)

// resetLayouts are the timestamp layouts ParseRateLimitReset attempts, in
// order, against the cleaned "try again at <when>" capture. The Codex signature
// uses a "May 30th, 2026 8:13 PM" shape; ordinal suffixes (st/nd/rd/th) are
// stripped before parsing so the standard reference layouts apply.
var resetLayouts = []string{
	"January 2, 2006 3:04 PM",
	"January 2, 2006 3:04PM",
	"January 2, 2006 15:04",
	"Jan 2, 2006 3:04 PM",
	"Jan 2, 2006 15:04",
	"2006-01-02 15:04:05",
	time.RFC3339,
}

// ordinalSuffixRe matches day ordinals like "30th", "1st", "2nd", "3rd" so we
// can drop the suffix before parsing ("May 30th" -> "May 30").
var ordinalSuffixRe = regexp.MustCompile(`(?i)(\d{1,2})(st|nd|rd|th)`)

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

// ParseRateLimitReset extracts the provider-stated reset time from output that
// contains a "try again at <date/time>" hint (as emitted by the Codex/OpenAI
// usage-limit error). It returns the parsed time and ok=true when a hint is
// present and parseable, or ok=false when no hint is found or the timestamp
// cannot be parsed.
//
// The provider message carries no timezone, so the returned time is
// interpreted in UTC. Callers that need wall-clock precision should treat the
// result as a best-effort lower bound for the cooldown, not an exact instant.
func ParseRateLimitReset(output string) (time.Time, bool) {
	m := rateLimitResetRe.FindStringSubmatch(output)
	if m == nil {
		return time.Time{}, false
	}
	return parseResetTimestamp(m[1])
}

// parseResetTimestamp normalizes and parses a single "<when>" capture extracted
// from a "try again at <when>" hint. Day ordinals are stripped and whitespace
// is collapsed before trying each layout in resetLayouts.
func parseResetTimestamp(raw string) (time.Time, bool) {
	// Drop a trailing sentence period and surrounding whitespace ("... 8:13 PM."
	// -> "... 8:13 PM").
	cleaned := strings.TrimRight(strings.TrimSpace(raw), ". ")
	if cleaned == "" {
		return time.Time{}, false
	}
	// Drop day ordinals: "May 30th, 2026" -> "May 30, 2026".
	cleaned = ordinalSuffixRe.ReplaceAllString(cleaned, "$1")
	// Collapse runs of whitespace to single spaces so "8:13  PM" parses.
	cleaned = strings.Join(strings.Fields(cleaned), " ")
	for _, layout := range resetLayouts {
		if ts, err := time.ParseInLocation(layout, cleaned, time.UTC); err == nil {
			return ts.UTC(), true
		}
	}
	return time.Time{}, false
}
