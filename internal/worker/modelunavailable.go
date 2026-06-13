package worker

import (
	"os"
	"regexp"
	"strings"
)

// modelUnavailablePatterns holds compiled regexes for detecting that a backend
// CLI failed because its configured model is unavailable — pulled from the
// plan, renamed, or not accessible to the account — as opposed to a
// credential/auth failure (see authFailurePatterns). This is the #713 class:
// when Fable was pulled from Pro/Max subscriptions early, the claude CLI
// (cmd `claude --model claude-fable-5 ...`) returned
//
//	There's an issue with the selected model (claude-fable-5). It may not
//	exist or you may not have access to it. Run --model to pick a different
//	model.
//
// Workers spawned against that config died ~2 minutes in; the orchestrator
// treated each death as a work failure, respawned on the SAME broken backend,
// and burned max_retries_per_issue until the issue wedged on retry_exhausted —
// fallback_backends was never consulted (karaoke #192 / scribe-service #339).
//
// Like authFailurePatterns these are intentionally high-precision: a worker
// log echoing the words "model" / "not exist" / "404" from a prompt or issue
// body must NOT match (the #663 false-positive class). Each signature anchors
// on a stable, provider-specific phrase, and IsModelUnavailable restricts the
// scan to the log tail where the CLI's terminal error actually lands.
//
// Supported signatures (case-insensitive):
//   - "it may not exist or you may not have access to it" (claude CLI, the
//     stable part of the live #713 message) and the equivalent OpenAI phrasing
//     "does not exist or you do not have access"
//   - "The model ... does not exist" (OpenAI/Gemini)
//   - Anthropic API "not_found_error" error type embedded in CLI error JSON
//   - "model_not_found" / "model: ... not found" (the model token adjacent, so
//     an unrelated "not found" elsewhere in the line does not match)
//   - HTTP 404 paired with a status/code/error context word — a bare "404"
//     (counters, issue refs) is NOT matched, mirroring the http_401 anchoring
var modelUnavailablePatterns = []struct {
	label string
	re    *regexp.Regexp
}{
	// claude CLI (live #713) and OpenAI share the "...exist or you ... access"
	// tail; require both halves of the phrase so a bare "may not exist" does
	// not match.
	{"model_no_access", regexp.MustCompile(`(?i)(?:may not exist or you may not have access|does not exist or you do not have access)`)},
	// OpenAI/Gemini: "The model `x` does not exist".
	{"model_does_not_exist", regexp.MustCompile(`(?i)\bthe model\b[^\n]{0,60}\bdoes not exist\b`)},
	// Anthropic API error type embedded in CLI error JSON (the missing-model
	// counterpart of authentication_error).
	{"not_found_error", regexp.MustCompile(`(?i)\bnot_found_error\b`)},
	// "model_not_found" or "model: ... not found" with the model token adjacent.
	{"model_not_found", regexp.MustCompile(`(?i)\bmodel_not_found\b|\bmodel(?:[ _-]?id)?\b[^\n]{0,40}?\bnot[ _]found\b`)},
	// HTTP 404 next to an HTTP/status/code/error marker so issue numbers
	// ("#404") or counters ("404 records") do not false-positive.
	{"http_404", regexp.MustCompile(`(?i)\b(?:http|https|status|code|err(?:or)?|response|received)\b[ \t]*[:=]?[ \t]*404\b`)},
}

// DetectModelUnavailable scans multi-line output for known model-unavailable
// signatures. Returns true and the matching pattern label on the first match,
// or false and "" if none is found. Auth/credential signatures are NOT matched
// here — those belong to DetectAuthFailure, a distinct class with different
// remediation (swap the model id vs fix credentials).
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
// model-unavailable signature (e.g. the claude CLI "It may not exist or you
// may not have access to it" from #713). Only the last authFailureTailLines
// lines are scanned — see that constant's comment for why a prompt echo at the
// top of a long log must not be misread as a backend outage. Returns the
// verdict and the matched pattern label.
//
// Callers MUST combine this with an early-death window (the worker died
// shortly after spawn) before classifying the death as a backend failure: a
// CLI pointed at a missing model dies on its first API call, while a
// model-unavailable phrase in a long-lived worker's log is more likely
// incidental work content than a backend outage.
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
