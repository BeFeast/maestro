package worker

import (
	"os"
	"regexp"
	"strings"
)

// usageLimitPatterns holds compiled regexes for detecting that a backend CLI
// died because the account's usage quota is exhausted (#805; live: codex
// "You've hit your usage limit ... try again at 12:30 PM" killed every
// dispatch on the then-default backend). This is deliberately a high-precision
// SUBSET of rateLimitPatterns: an account-level quota exhaustion lasts until
// the provider window resets, so a dead worker matching one of these is a
// backend failure worth gating and falling over even when no reset time is
// parseable. The generic capacity signals (HTTP 429, "too many requests",
// "rate limit exceeded", "resource_exhausted") are intentionally EXCLUDED —
// they are transient, and acting on them without a provider-stated reset is
// the false-positive class from #663.
//
// Supported signatures (case-insensitive):
//   - "You've hit your (usage )?limit" (Codex / Claude provider phrasing)
//   - "chatgpt.com/codex/settings/usage" marker (Codex usage-limit URL)
//   - "usage/5-hour/weekly limit reached" (claude CLI subscription-window
//     phrasings, e.g. "Claude usage limit reached ...")
var usageLimitPatterns = []struct {
	label string
	re    *regexp.Regexp
}{
	{"hit_limit", regexp.MustCompile(`(?i)you'?ve hit your (usage )?limit`)},
	{"codex_usage_limit", regexp.MustCompile(`(?i)(chatgpt\.com/)?codex/settings/usage`)},
	{"usage_limit_reached", regexp.MustCompile(`(?i)\b(?:usage|5-hour|weekly)[ _-]limit reached\b`)},
}

// DetectUsageLimit scans multi-line output for known account-quota exhaustion
// signatures, plus any operator-supplied per-backend extra patterns
// (config: model.backends.<name>.usage_limit_patterns; validated to compile
// at config parse, so an entry failing to compile here is silently skipped).
// Returns true and the matching pattern label on the first match — for an
// extra pattern the label is its regex source. Returns false and "" when
// nothing matches.
func DetectUsageLimit(output string, extraPatterns []string) (bool, string) {
	extras := make([]*regexp.Regexp, 0, len(extraPatterns))
	for _, p := range extraPatterns {
		if re, err := regexp.Compile(p); err == nil {
			extras = append(extras, re)
		}
	}
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		for _, p := range usageLimitPatterns {
			if p.re.MatchString(line) {
				return true, p.label
			}
		}
		for _, re := range extras {
			if re.MatchString(line) {
				return true, re.String()
			}
		}
	}
	return false, ""
}

// IsUsageLimit checks whether a dead worker's log file ends with an
// account-quota exhaustion signature (e.g. the codex "You've hit your usage
// limit" from #805). Only the last authFailureTailLines lines are scanned —
// the quota error that killed the CLI is its terminal output, while earlier
// log content can echo the worker prompt (which for a quota-related issue
// legitimately quotes these exact strings). Returns the verdict and the
// matched pattern label.
//
// Callers MUST combine this with an early-death window (the worker died
// shortly after spawn) before classifying the death as a backend failure: a
// quota-dead CLI dies on its first API call, while a usage-limit signature in
// a long-lived worker's log is more likely incidental work content than a
// backend outage. A quota death that also states a parseable reset is handled
// earlier by the provider-limit path (any worker age) with the provider's own
// RetryAfter.
func IsUsageLimit(logFile string, extraPatterns []string) (bool, string) {
	if logFile == "" {
		return false, ""
	}
	data, err := os.ReadFile(logFile)
	if err != nil {
		return false, ""
	}
	lines := strings.Split(string(data), "\n")
	if len(lines) > authFailureTailLines {
		lines = lines[len(lines)-authFailureTailLines:]
	}
	return DetectUsageLimit(strings.Join(lines, "\n"), extraPatterns)
}
