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

// ParseTokensFromOutput scans multi-line text for token-usage lines and
// returns the maximum total count found (0 if none matched).
// Commas in numbers are stripped before parsing.
func ParseTokensFromOutput(text string) int {
	max := 0
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if n := parseTokensFromLine(line); n > max {
			max = n
		}
	}
	return max
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
