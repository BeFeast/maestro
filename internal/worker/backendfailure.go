package worker

import (
	"regexp"
	"strings"
)

// modelUnavailablePatterns holds compiled regexes for detecting that a
// backend CLI failed because its configured model is unavailable — pulled
// from the plan, renamed, or not accessible to the account — rather than an
// auth/credential outage (those live in authFailurePatterns). A
// model-unavailable death is still a backend failure, not a work failure:
// respawning on the same backend dies the same way and burns the per-issue
// retry budget, so the orchestrator must gate the backend and fall over to a
// fallback (#713).
//
// Like authFailurePatterns these are intentionally high-precision: a worker
// log echoing model-error wording from a prompt or issue body must NOT match
// (the false-positive class from #663). In particular, a generic HTTP 404 is
// not evidence that the configured AI model is unavailable: worker output can
// legitimately contain application, test, documentation, and tool 404s.
//
// Supported signatures (case-insensitive):
//   - "it may not exist or you may not have access to it" — the stable tail
//     of the claude CLI message observed live when Fable was pulled from
//     Pro/Max subscriptions (#713): "There's an issue with the selected
//     model (claude-fable-5). It may not exist or you may not have access to
//     it. Run --model to pick a different model."
//   - "The model ... does not exist" (OpenAI phrasing)
//   - "model not found" / "model_not_found" / "model: <id> not found"
//   - Anthropic API error type "not_found_error" paired with a model context
//     word (either order) so a not_found_error about some other resource does
//     not false-positive
//   - HTTP/API 529 paired with an overloaded marker. A terminal overload is
//     model-scoped transient capacity, not credential exhaustion or evidence
//     that the configured model id is invalid.
var modelUnavailablePatterns = []struct {
	label string
	re    *regexp.Regexp
}{
	{"model_access_denied", regexp.MustCompile(`(?i)it may not exist or you may not have access to it`)},
	{"model_does_not_exist", regexp.MustCompile(`(?i)\bthe model\b[^\n]{0,80}?\b(?:does not|doesn'?t) exist\b`)},
	{"model_not_found", regexp.MustCompile(`(?i)\bmodel[_ ]not[_ ]found\b|\bmodel\b[\s:="'` + "`" + `-]+[^\n]{0,80}?\bnot found\b`)},
	{"not_found_error", regexp.MustCompile(`(?i)(?:not_found_error\b[^\n]{0,80}?\bmodel\b|\bmodel\b[^\n]{0,80}?not_found_error\b)`)},
	// A bare 529 or the word "overloaded" is not enough: both must occur on
	// the same terminal line, and 529 must be anchored to an HTTP/API status
	// context. A separately wrapped structured body is handled below, where it
	// must carry the stronger overloaded_error marker.
	{"model_overloaded", regexp.MustCompile(`(?i)(?:\b(?:http|https|status|code|err(?:or)?|response|received)\b[ \t]*[:=]?[ \t]*529\b[^\n]{0,160}\boverloaded(?:_error)?\b|\boverloaded(?:_error)?\b[^\n]{0,160}\b(?:http|https|status|code|err(?:or)?|response|received)\b[ \t]*[:=]?[ \t]*529\b)`)},
}

// modelOverloadedWrappedPattern covers CLI wrappers that print the status and
// structured provider body on adjacent lines. Requiring overloaded_error in
// the second line keeps an unrelated mention of an overloaded queue from being
// paired with a bare 529 status in ordinary worker output.
var modelOverloadedWrappedPattern = regexp.MustCompile(`(?i)\b(?:http|https|status|code|err(?:or)?|response|received)\b[ \t]*[:=]?[ \t]*529\b[^\n]*\r?\n[^\n]{0,320}\boverloaded_error\b`)

// DetectModelUnavailable scans multi-line output for known model-unavailable
// signatures. Returns true and the matching pattern label on the first match,
// or false and "" if none is found.
func DetectModelUnavailable(output string) (bool, string) {
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		for _, p := range modelUnavailablePatterns {
			if p.re.MatchString(line) {
				return true, p.label
			}
		}
	}
	if modelOverloadedWrappedPattern.MatchString(output) {
		return true, "model_overloaded"
	}
	return false, ""
}

// IsModelUnavailable checks whether a dead worker's log file ends with a
// model-unavailable signature (e.g. the claude CLI "it may not exist or you
// may not have access to it" from #713). Only the last authFailureTailLines
// lines are scanned — the model error that killed the CLI is its terminal
// output, while earlier log content can echo the worker prompt (which for a
// model-related issue legitimately quotes these exact strings). Returns the
// verdict and the matched pattern label.
//
// Callers MUST combine this with an early-death window (the worker died
// shortly after spawn) before classifying the death as a backend failure: an
// unusable model dies on the CLI's first API call, while a model-unavailable
// signature in a long-lived worker's log is more likely incidental work
// content than a backend outage.
func IsModelUnavailable(logFile string) (bool, string) {
	tail, ok := logTail(logFile, authFailureTailLines)
	if !ok {
		return false, ""
	}
	return DetectModelUnavailable(tail)
}
