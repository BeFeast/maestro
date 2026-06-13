package worker

import (
	"os"
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
// log echoing the words "model" / "not exist" / "404" from a prompt or issue
// body must NOT match (the false-positive class from #663). 404 in
// particular is anchored to an HTTP / status / code / error marker exactly
// like http_401 — a bare "404" or an issue reference "#404" is never a hit.
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
//   - HTTP 404 paired with an HTTP / status / code / error / response marker
//     (bare "404" is NOT matched — same discipline as http_401)
var modelUnavailablePatterns = []struct {
	label string
	re    *regexp.Regexp
}{
	{"model_access_denied", regexp.MustCompile(`(?i)it may not exist or you may not have access to it`)},
	{"model_does_not_exist", regexp.MustCompile(`(?i)\bthe model\b[^\n]{0,80}?\b(?:does not|doesn'?t) exist\b`)},
	{"model_not_found", regexp.MustCompile(`(?i)\bmodel[_ ]not[_ ]found\b|\bmodel\b[\s:="'` + "`" + `-]+[^\n]{0,80}?\bnot found\b`)},
	{"not_found_error", regexp.MustCompile(`(?i)(?:not_found_error\b[^\n]{0,80}?\bmodel\b|\bmodel\b[^\n]{0,80}?not_found_error\b)`)},
	// HTTP 404 with a status/code/error context word. Requires the literal
	// 404 to appear next to an HTTP / status / code / error / response marker
	// so issue references ("#404"), counters ("404 records") and a bare "404
	// Not Found" line do not false-positive — only an anchored 404 (e.g. "API
	// Error: 404", "status: 404") is treated as a model-unavailable signal.
	{"http_404", regexp.MustCompile(`(?i)\b(?:http|https|status|code|err(?:or)?|response|received)\b[ \t]*[:=]?[ \t]*404\b`)},
}

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
	return DetectModelUnavailable(strings.Join(lines, "\n"))
}
