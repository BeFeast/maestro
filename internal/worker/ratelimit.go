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
// Patterns are intentionally high-precision: they must NOT match transient
// tool/exec errors (e.g. codex `write_stdin failed: stdin is closed`) or
// stray digit sequences like "429" embedded in unrelated text — that
// misclassification triggered a false backend fallback on apertune (issue
// #663), where the orchestrator respawned a healthy codex worker on a more
// expensive backend chasing a phantom quota block.
//
// Supported patterns (case-insensitive):
//   - "You've hit your <qualifier> limit" (Claude / Codex provider phrasing).
//     The optional qualifier word matches "usage" (Codex) and "session"
//     (Claude subscription: "You've hit your session limit · resets 9am
//     (UTC)", #808) as well as the bare "You've hit your limit".
//   - "You've reached your <plan> limit" (Claude/Fable subscription cap, #808)
//   - "You're out of (extra )?usage" (Claude extra-usage exhaustion, #808) —
//     paired with a parseable "resets <t>" hint this is the high-confidence
//     provider-limit signal the fallover selector acts on.
//   - "chatgpt.com/codex/settings/usage" marker (Codex usage-limit URL)
//   - HTTP 429 paired with a status/code/error context word — bare 429
//     embedded in unrelated text is NOT matched
//   - "<NNN> too many requests" (HTTP reason phrase)
//   - "rate limit exceeded|reached|hit" / "rate_limit_error|exceeded" — the
//     bare phrase "rate limit" alone is NOT matched (too noisy: appears in
//     prompt context, runbook docs, etc.)
//   - "quota exceeded" (Google / generic)
//   - "too many requests" (HTTP 429 reason)
//   - "resource_exhausted" (gRPC status)
var rateLimitPatterns = []struct {
	label string
	re    *regexp.Regexp
}{
	{"hit_limit", regexp.MustCompile(`(?i)you'?ve hit your (?:\w+ )?limit`)},
	{"reached_limit", regexp.MustCompile(`(?i)you'?ve reached your\b[^.\n]{0,40}?\blimit\b`)},
	{"out_of_usage", regexp.MustCompile(`(?i)you'?re out of (?:extra )?usage\b`)},
	{"codex_usage_limit", regexp.MustCompile(`(?i)(chatgpt\.com/)?codex/settings/usage`)},
	// HTTP 429 with a rate-limit context word. Requires the literal 429 to
	// appear next to an HTTP / status / code / error / response marker so
	// "1.0.429" or "processed 14290 records" do not false-positive.
	{"http_429", regexp.MustCompile(`(?i)\b(?:http|https|status|code|err(?:or)?|response|received)\b[ \t]*[:=]?[ \t]*429\b`)},
	{"http_429_reason", regexp.MustCompile(`(?i)\b429\b[ \t:,-]*too[ \t]+many[ \t]+requests`)},
	{"rate_limit_exceeded", regexp.MustCompile(`(?i)rate[ _.-]?limit[ _.-]?(?:exceeded|reached|hit|error)`)},
	{"quota_exceeded", regexp.MustCompile(`(?i)quota[ _.-]?exceeded`)},
	{"too_many_requests", regexp.MustCompile(`(?i)too\s+many\s+requests`)},
	{"resource_exhausted", regexp.MustCompile(`(?i)resource[_.\s]?exhausted`)},
}

// rateLimitResetRe extracts the human-readable reset hint emitted after a
// provider usage-limit error, e.g.
//
//	"... try again at May 30th, 2026 8:13 PM."   (Codex / OpenAI)
//	"You've hit your session limit · resets 9am (UTC)"   (Claude, #808)
//	"You're out of extra usage · resets 4:10pm (UTC)"    (Claude, #808)
//
// The capture is greedy to the end of the line; parseResetTimestamp /
// parseTimeOnlyReset trim a trailing sentence period, a trailing
// parenthesised timezone ("(UTC)"), and surrounding whitespace before
// parsing. The "resets" alternative is anchored on a word boundary so it does
// not fire inside an unrelated token (e.g. "core-resets").
var rateLimitResetRe = regexp.MustCompile(`(?i)(?:try again (?:at|after)|\bresets)\s+([^\n]+)`)

// trailingTZParenRe strips a trailing parenthesised timezone marker from a
// reset capture: "9am (UTC)" -> "9am", "4:10pm (UTC)" -> "4:10pm". The
// timezone is dropped rather than honoured — parseTimeOnlyReset already
// documents its result as a best-effort UTC lower bound, and every live
// Claude signature states "(UTC)".
var trailingTZParenRe = regexp.MustCompile(`\s*\([^)]*\)\s*$`)

// stripResetDecorations removes the sentence punctuation and parenthesised
// timezone a provider may append to a "<when>" reset capture, in EITHER order:
// "9am (UTC)", "9am (UTC).", "8:13 PM.", and "9am. (UTC)" all reduce to the
// bare timestamp. The trailing period is trimmed FIRST so a "(UTC)." tail
// (period after the closing paren, as a sentence-terminated Claude reset
// emits) does not defeat trailingTZParenRe's "$" anchor and leave an
// unparseable "9am (UTC)"; the strip and trim are then repeated so residual
// punctuation exposed by removing the paren is dropped too (#808 review).
func stripResetDecorations(raw string) string {
	cleaned := strings.TrimRight(strings.TrimSpace(raw), ". ")
	cleaned = trailingTZParenRe.ReplaceAllString(cleaned, "")
	return strings.TrimRight(strings.TrimSpace(cleaned), ". ")
}

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

// timeOnlyResetLayouts are the clock-only layouts tried when no date-bearing
// layout in resetLayouts matches the "try again at <when>" capture. Codex
// emits this shape when the quota resets later the same day — the live #805
// signature was "You've hit your usage limit ... try again at 12:30 PM."
// with no date at all, which the date-bearing layouts reject; the reset then
// went unparsed, the rate-limit signal stayed low-confidence (#663), and no
// failover fired. A clock-only hint is resolved against a reference time to
// the NEXT occurrence of that wall-clock time (see parseTimeOnlyReset).
// The hour-only meridiem shapes ("3PM"/"3 PM") cover the Claude subscription
// phrasing "resets 9am (UTC)" (#808), which states an hour with no minutes; the
// date-bearing and minute-bearing layouts reject it.
var timeOnlyResetLayouts = []string{
	"3:04 PM",
	"3:04PM",
	"15:04",
	"3PM",
	"3 PM",
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

// OutputContainsRateLimit checks if the given output text contains any known
// rate-limit error patterns (case-insensitive). It uses the same regex set as
// DetectRateLimit so live and post-mortem detection share one source of truth
// and neither false-positives on transient tool/exec errors.
func OutputContainsRateLimit(output string) bool {
	hit, _ := DetectRateLimit(output)
	return hit
}

// logTail reads a dead worker's log file and returns its last n lines. It is
// the shared read for every post-mortem log classifier in this package: the
// error that killed a CLI is its terminal output, while earlier log content
// can echo the same strings as prompt or work content (a worker fixing a
// quota classifier, a runbook quoting the provider message). ok=false means
// the path is empty or the file is unreadable.
func logTail(logFile string, n int) (string, bool) {
	if logFile == "" {
		return "", false
	}
	data, err := os.ReadFile(logFile)
	if err != nil {
		return "", false
	}
	lines := strings.Split(string(data), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n"), true
}

// IsRateLimited checks whether a dead worker's log file ends with a
// rate-limit error message. Only the last authFailureTailLines lines are
// scanned — a long-running worker can echo live quota text as prompt or work
// output and later die for an unrelated reason; scanning the whole log let
// that echo classify the death as a provider limit (#805 review). The
// terminal error that actually killed the CLI is always within the tail.
// Used to detect rate-limiting in dead workers for fallback logic.
func IsRateLimited(logFile string) bool {
	tail, ok := logTail(logFile, authFailureTailLines)
	if !ok {
		return false
	}
	return OutputContainsRateLimit(tail)
}

// ClassifyRateLimit inspects output for a rate-limit signal and reports both
// the match and its confidence. confidence is "high" when the output carries a
// parseable provider-stated reset window (e.g. the codex "try again at
// <when>" suffix), and "low" otherwise.
//
// Callers that switch backends on a rate-limit signal MUST require confidence
// == "high". A low-confidence detection is too easy to false-positive — a
// pattern match without a reset hint is often a stale prompt-context echo, a
// transient tool/exec error, or a codex tools-router error that looks superficially
// like a quota message — and switching backends on it burns the more-expensive
// fallback for no reason, masks the real worker error in operator signal, and
// can wedge the session in a respawn loop when no fallback is configured.
// See issue #663.
func ClassifyRateLimit(output string) (hit bool, label string, confidence string, resetAt time.Time) {
	return ClassifyRateLimitAt(output, time.Now().UTC())
}

// ClassifyRateLimitAt is ClassifyRateLimit with an explicit reference time,
// used to resolve time-only reset hints ("try again at 12:30 PM", #805) to an
// absolute instant. Callers on the live path pass time.Now().UTC(); tests pass
// a fixed clock.
func ClassifyRateLimitAt(output string, now time.Time) (hit bool, label string, confidence string, resetAt time.Time) {
	hit, label = DetectRateLimit(output)
	if !hit {
		return false, "", "", time.Time{}
	}
	if reset, ok := ParseRateLimitResetAt(output, now); ok {
		return true, label, "high", reset
	}
	return true, label, "low", time.Time{}
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
	return ParseRateLimitResetAt(output, time.Now().UTC())
}

// ParseRateLimitResetAt is ParseRateLimitReset with an explicit reference
// time. Date-bearing hints parse as before; a time-only hint ("try again at
// 12:30 PM" — the live codex phrasing from #805, emitted when the quota
// resets later the same day) is resolved to the next occurrence of that
// wall-clock time strictly after now. Without this, the reset went unparsed,
// the death stayed a low-confidence rate-limit signal (#663), and the
// orchestrator burned the per-issue retry budget on a quota-dead backend.
//
// When the output carries several "try again at" hints, the LAST parseable
// one wins: the hint the provider printed as the CLI died is the most recent
// output, while an earlier occurrence can be an echo of prompt or work
// content quoting the same message (#805 review).
func ParseRateLimitResetAt(output string, now time.Time) (time.Time, bool) {
	matches := rateLimitResetRe.FindAllStringSubmatch(output, -1)
	for i := len(matches) - 1; i >= 0; i-- {
		if ts, ok := parseResetTimestamp(matches[i][1]); ok {
			return ts, true
		}
		if ts, ok := parseTimeOnlyReset(matches[i][1], now); ok {
			return ts, true
		}
	}
	return time.Time{}, false
}

// ParseRateLimitResetFromLog extracts the provider-stated reset hint from a
// dead worker's log file, resolving time-only hints against the given
// reference time. Like IsRateLimited it scans only the last
// authFailureTailLines lines — a "try again at ..." echoed mid-log as prompt
// or work content must not lend a later, unrelated death a high-confidence
// reset window (#805 review). ok=false when the file is unreadable or the
// tail carries no parseable hint.
func ParseRateLimitResetFromLog(logFile string, now time.Time) (time.Time, bool) {
	tail, ok := logTail(logFile, authFailureTailLines)
	if !ok {
		return time.Time{}, false
	}
	return ParseRateLimitResetAt(tail, now)
}

// parseTimeOnlyReset resolves a clock-only "<when>" capture ("12:30 PM",
// "12:30pm", "09:15") against the reference time: the result is today's
// occurrence of that wall-clock time in UTC, rolled to tomorrow when it has
// already passed. Like the date-bearing layouts the provider states no
// timezone, so the result is a best-effort lower bound for the cooldown.
func parseTimeOnlyReset(raw string, now time.Time) (time.Time, bool) {
	if now.IsZero() {
		return time.Time{}, false
	}
	// Drop the trailing sentence period and parenthesised timezone
	// ("9am (UTC)." -> "9am") so the "(UTC)" the Claude signature appends — and
	// any period after it — does not defeat the clock layouts (#808).
	cleaned := stripResetDecorations(raw)
	if cleaned == "" {
		return time.Time{}, false
	}
	// Collapse whitespace and uppercase so "12:30pm" / "8:13  pm" parse with
	// Go's reference layouts (which only accept "PM").
	cleaned = strings.ToUpper(strings.Join(strings.Fields(cleaned), " "))
	now = now.UTC()
	for _, layout := range timeOnlyResetLayouts {
		clock, err := time.ParseInLocation(layout, cleaned, time.UTC)
		if err != nil {
			continue
		}
		candidate := time.Date(now.Year(), now.Month(), now.Day(), clock.Hour(), clock.Minute(), 0, 0, time.UTC)
		if !candidate.After(now) {
			candidate = candidate.Add(24 * time.Hour)
		}
		return candidate, true
	}
	return time.Time{}, false
}

// parseResetTimestamp normalizes and parses a single "<when>" capture extracted
// from a "try again at <when>" hint. Day ordinals are stripped and whitespace
// is collapsed before trying each layout in resetLayouts.
func parseResetTimestamp(raw string) (time.Time, bool) {
	// Drop the trailing sentence period and parenthesised timezone in either
	// order ("... 8:13 PM (UTC)." -> "... 8:13 PM").
	cleaned := stripResetDecorations(raw)
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
