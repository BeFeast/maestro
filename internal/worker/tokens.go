package worker

import (
	"regexp"
	"strconv"
	"strings"
)

// tokenPatterns holds compiled regexes for extracting total token counts
// from various AI CLI output formats.
//
// Supported formats (case-insensitive):
//   - openclaw/claude: "tokens 12345 (in 1000 / out 11345)"
//   - codex-style:     "Tokens: 1234 input, 5678 output"
//   - generic total:   "total tokens: 12345"  |  "total_tokens: 12345"
var tokenPatterns = []*regexp.Regexp{
	// openclaw format: "tokens 12345 (in 1000 / out 11345)"
	regexp.MustCompile(`(?i)\btokens\s+(\d[\d,]*)(?:\s+\(in\s+[\d,]+\s*/\s*out\s+[\d,]+\))?`),
	// codex-style: "Tokens: 1234 input, 5678 output" — sum input+output
	regexp.MustCompile(`(?i)\bTokens:\s*(\d[\d,]*)\s+input,\s*(\d[\d,]*)\s+output`),
	// generic "total tokens: N" or "total_tokens: N"
	regexp.MustCompile(`(?i)\btotal[_ ]tokens[:\s]+(\d[\d,]*)`),
}

// codexTwoLineKeyword matches the codex "tokens used" header line that
// precedes a number-only line (the modern codex CLI splits the total
// across two lines, e.g. "tokens used\n89,655"). The keyword line is
// allowed to carry a leading prefix (timestamps, log tags) but must end
// with the phrase.
var codexTwoLineKeyword = regexp.MustCompile(`(?i)(?:^|[\s\]])tokens used\s*$`)

// numericLine matches a line that is just a (possibly comma-grouped)
// integer, used as the value pairing for codexTwoLineKeyword.
var numericLine = regexp.MustCompile(`^(\d[\d,]*)\s*$`)

// jsonInputTokens / jsonOutputTokens extract the per-call usage totals
// from claude --output-format json (and stream-json) result objects.
// The result frame carries a single "usage" block with both fields; in
// stream-json the trailing result frame still has them, so taking the
// last match of each on the buffered output reflects the run total.
var jsonInputTokens = regexp.MustCompile(`"input_tokens"\s*:\s*(\d+)`)
var jsonOutputTokens = regexp.MustCompile(`"output_tokens"\s*:\s*(\d+)`)

// ParseTokensFromOutput scans multi-line text for token-usage lines and
// returns the maximum total count found (0 if none matched).
// Commas in numbers are stripped before parsing.
//
// Three families of producers are recognised:
//
//  1. Single-line totals — see tokenPatterns above.
//
//  2. The codex "two-line" form, where the keyword and the count live on
//     adjacent lines:
//
//     tokens used
//     89,655
//
//  3. JSON "usage" objects (claude --output-format json / stream-json):
//
//     "usage": {"input_tokens": 123, "output_tokens": 456}
//
// claude's default text mode (`claude -p ...`) does NOT print a
// parseable token total to stdout — token capture for claude requires
// running with `--output-format json` (and `--verbose` for stream-json).
// Workers that run claude in default mode degrade to tokens=0; the
// cost-observability rollup already skips zero-token sessions and
// reports "price not configured" rather than a misleading total.
func ParseTokensFromOutput(text string) int {
	maxTokens := 0
	lines := strings.Split(text, "\n")

	for i, raw := range lines {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		if n := parseTokensFromLine(line); n > maxTokens {
			maxTokens = n
		}
		if codexTwoLineKeyword.MatchString(line) {
			if n := nextNumericValue(lines, i+1); n > maxTokens {
				maxTokens = n
			}
		}
	}

	if n := parseJSONUsage(text); n > maxTokens {
		maxTokens = n
	}

	return maxTokens
}

// nextNumericValue returns the integer on the first non-empty line at or
// after start that consists solely of a (comma-grouped) number, or 0 if
// the next non-empty line is something else. Used to pair a codex
// "tokens used" header with its count line.
func nextNumericValue(lines []string, start int) int {
	for j := start; j < len(lines); j++ {
		next := strings.TrimSpace(lines[j])
		if next == "" {
			continue
		}
		if m := numericLine.FindStringSubmatch(next); m != nil {
			return parseNum(m[1])
		}
		return 0
	}
	return 0
}

// parseJSONUsage extracts the run-total token count from a claude
// `--output-format json` (or stream-json) buffer by summing the last
// `"input_tokens"` and `"output_tokens"` values found. Returns 0 when
// neither field appears.
func parseJSONUsage(text string) int {
	in := lastIntMatch(jsonInputTokens, text)
	out := lastIntMatch(jsonOutputTokens, text)
	return in + out
}

func lastIntMatch(re *regexp.Regexp, s string) int {
	ms := re.FindAllStringSubmatch(s, -1)
	if len(ms) == 0 {
		return 0
	}
	n, _ := strconv.Atoi(ms[len(ms)-1][1])
	return n
}

func parseTokensFromLine(line string) int {
	// Try codex-style first: captures two groups (input + output)
	codexRe := tokenPatterns[1]
	if m := codexRe.FindStringSubmatch(line); m != nil {
		in := parseNum(m[1])
		out := parseNum(m[2])
		return in + out
	}

	// Try the remaining patterns — each yields a single total
	for i, re := range tokenPatterns {
		if i == 1 {
			continue // already handled
		}
		if m := re.FindStringSubmatch(line); m != nil {
			return parseNum(m[1])
		}
	}
	return 0
}

// parseNum converts a numeric string (with optional commas) to int.
func parseNum(s string) int {
	s = strings.ReplaceAll(s, ",", "")
	n, _ := strconv.Atoi(s)
	return n
}

// FormatTokens formats a token count as a compact human-readable string.
//
//	0       → "-"
//	999     → "999"
//	1500    → "1.5k"
//	1200000 → "1.2M"
func FormatTokens(n int) string {
	switch {
	case n <= 0:
		return "-"
	case n < 1000:
		return strconv.Itoa(n)
	case n < 1_000_000:
		k := float64(n) / 1000.0
		return formatFloat(k) + "k"
	default:
		m := float64(n) / 1_000_000.0
		return formatFloat(m) + "M"
	}
}

// formatFloat renders a float with one decimal place, stripping trailing ".0".
func formatFloat(f float64) string {
	s := strconv.FormatFloat(f, 'f', 1, 64)
	s = strings.TrimRight(s, "0")
	s = strings.TrimRight(s, ".")
	return s
}
