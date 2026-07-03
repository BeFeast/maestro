package worker

import (
	"regexp"
	"strings"
)

// authFailurePatterns holds compiled regexes for detecting backend
// auth/credential failures in AI CLI output. Each entry pairs a
// human-readable label with its regex.
//
// Like rateLimitPatterns above, these are intentionally high-precision:
// a bare "401" or the word "unauthorized" embedded in unrelated text must
// NOT match, because worker logs can legitimately contain such strings as
// work content (a worker writing auth middleware, test output asserting
// 401 responses, or a prompt echo of an auth-related issue body — the
// false-positive class from #663).
//
// Supported signatures (case-insensitive):
//   - "Failed to authenticate" (claude CLI, observed live in #693:
//     "Failed to authenticate. API Error: 401 Invalid authentication
//     credentials")
//   - "Invalid authentication credentials"
//   - "authentication_error" / "authentication error" (Anthropic API error
//     type embedded in CLI error JSON)
//   - HTTP 401 paired with a status/code/error context word — bare 401
//     embedded in unrelated text is NOT matched
//   - "401 Unauthorized" (HTTP reason phrase)
//   - "OAuth token has expired/been revoked" and "Please run /login"
//     (claude CLI logged-out states)
//   - invalid / incorrect / expired / revoked API key (OpenAI & Gemini
//     phrasings: "Incorrect API key provided", "API key not valid")
var authFailurePatterns = []struct {
	label string
	re    *regexp.Regexp
}{
	{"failed_to_authenticate", regexp.MustCompile(`(?i)failed to authenticate`)},
	{"invalid_auth_credentials", regexp.MustCompile(`(?i)invalid authentication credentials`)},
	{"authentication_error", regexp.MustCompile(`(?i)\bauthentication[_ ]error\b`)},
	// HTTP 401 with an auth context word. Requires the literal 401 to appear
	// next to an HTTP / status / code / error / response marker so issue
	// numbers ("#401") or counters ("4012 records") do not false-positive.
	{"http_401", regexp.MustCompile(`(?i)\b(?:http|https|status|code|err(?:or)?|response|received)\b[ \t]*[:=]?[ \t]*401\b`)},
	{"http_401_reason", regexp.MustCompile(`(?i)\b401\b[ \t:,-]*unauthorized`)},
	{"oauth_token_expired", regexp.MustCompile(`(?i)oauth token (?:has )?(?:expired|been revoked)`)},
	{"login_required", regexp.MustCompile(`(?i)please run /login`)},
	{"invalid_api_key", regexp.MustCompile(`(?i)(?:invalid|incorrect|expired|revoked)[ _-]?api[ _-]?key|api key (?:is )?(?:not valid|invalid|expired|revoked)`)},
}

// authFailureTailLines bounds post-mortem auth detection to the end of a
// dead worker's log. The auth error that killed the CLI is its terminal
// output, while earlier log content can echo the worker prompt — which for
// auth-related issues legitimately contains strings like "401" or "Failed
// to authenticate". Scanning only the tail keeps a prompt echo at the top
// of a long log from being misread as a backend outage.
const authFailureTailLines = 100

// DetectAuthFailure scans multi-line output for known backend
// auth/credential failure signatures. Returns true and the matching pattern
// label on the first match, or false and "" if none is found.
func DetectAuthFailure(output string) (bool, string) {
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		for _, p := range authFailurePatterns {
			if p.re.MatchString(line) {
				return true, p.label
			}
		}
	}
	return false, ""
}

// IsAuthFailure checks whether a dead worker's log file ends with a backend
// auth/credential failure signature (e.g. the claude CLI 401 from #693).
// Only the last authFailureTailLines lines are scanned — see the constant's
// comment for why. Returns the verdict and the matched pattern label.
//
// Callers MUST combine this with an early-death window (the worker died
// shortly after spawn) before classifying the death as a backend failure:
// an unauthenticated CLI dies on its first API call, while an auth
// signature in a long-lived worker's log is more likely incidental work
// content than a backend outage.
func IsAuthFailure(logFile string) (bool, string) {
	tail, ok := logTail(logFile, authFailureTailLines)
	if !ok {
		return false, ""
	}
	return DetectAuthFailure(tail)
}
