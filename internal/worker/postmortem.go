package worker

import (
	"bufio"
	"fmt"
	"os"
	"regexp"
	"strings"
)

// Prior-attempt post-mortem extraction (#835).
//
// When a worker attempt fails and the orchestrator respawns it, the new
// attempt's prompt otherwise only carries GitHub-side feedback (CI output,
// review comments, rebase conflicts). The failed attempt's own trajectory —
// everything the CLI streamed into <state_dir>/logs/<slot>.log — is discarded,
// so attempt N+1 has no idea what attempt N tried and repeats the same dead
// ends. ExtractPostmortem distills a compact, secret-redacted summary from the
// tail of that log so the retry prompt can carry it forward.
//
// This is a deterministic, backend-agnostic heuristic pass (no LLM call): it
// scans the tail for failure/error signatures, files the attempt touched, and
// the last actions before termination. Extraction is best-effort — a missing,
// empty, or unreadable log yields an empty string (no section), never an error.

const (
	// PostmortemTailLines bounds how many trailing log lines the extractor
	// scans. Worker logs can be enormous; the failure signal lives near the
	// end, so scanning the tail keeps the pass cheap and focused.
	PostmortemTailLines = 400

	// PostmortemPromptCapBytes hard-caps the post-mortem excerpt placed in a
	// retry prompt. Logs can be huge and prompt bloat is a real token cost;
	// the full post-mortem is persisted to disk uncapped by the caller.
	PostmortemPromptCapBytes = 2048

	maxPostmortemFailureLines = 20
	maxPostmortemEditedFiles  = 15
	maxPostmortemLastActions  = 15
	// maxPostmortemLineRunes trims individual selected lines so a single
	// pathological line (a giant JSON blob, a base64 payload) cannot dominate
	// the budget.
	maxPostmortemLineRunes = 400
)

// postmortemRedactions mirrors internal/supervisor's sensitiveRedactions so
// obvious credential shapes never reach the retry prompt or the persisted
// post-mortem file. The worker package cannot import supervisor (supervisor
// already imports worker), so the patterns are mirrored here rather than
// shared; keep the two lists in sync.
var postmortemRedactions = []struct {
	re   *regexp.Regexp
	repl string
}{
	{regexp.MustCompile(`(?i)\bAuthorization\s*:\s*[^\n\r]+`), "Authorization: [REDACTED]"},
	{regexp.MustCompile(`(?i)\bBearer\s+[A-Za-z0-9._~+/=-]+`), "Bearer [REDACTED]"},
	{regexp.MustCompile(`(?i)\b([A-Z0-9_]*(?:TOKEN|SECRET|PASSWORD|PASSWD|API[_-]?KEY|PRIVATE[_-]?KEY)[A-Z0-9_]*)\s*[:=]\s*("[^"\n]*"|'[^'\n]*'|[^\s\n]+)`), "${1}=[REDACTED]"},
	{regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9_]{20,}\b`), "[REDACTED_GITHUB_TOKEN]"},
	{regexp.MustCompile(`\bsk-[A-Za-z0-9]{20,}\b`), "[REDACTED_API_KEY]"},
	{regexp.MustCompile(`\bxox[baprs]-[A-Za-z0-9-]{20,}\b`), "[REDACTED_SLACK_TOKEN]"},
}

// redactPostmortemSecrets masks common credential shapes in place.
func redactPostmortemSecrets(text string) string {
	for _, r := range postmortemRedactions {
		text = r.re.ReplaceAllString(text, r.repl)
	}
	return text
}

// failureSignatures identify lines that report a failed command, build error,
// or test failure across the CLIs a worker may drive (Go tooling, git, npm,
// python, generic shells). The set is intentionally curated rather than
// matching every mention of "error" — a cap plus dedup bound any noise.
var failureSignatures = []*regexp.Regexp{
	regexp.MustCompile(`^\s*(?:--- FAIL|=== FAIL|FAIL\b)`), // go test failures
	regexp.MustCompile(`^\s*ok\s+\S+\s+\S+\s+\[build failed\]`),
	regexp.MustCompile(`^\s*panic:`),     // go / runtime panic
	regexp.MustCompile(`\.go:\d+:\d+:`),  // go compiler diagnostics
	regexp.MustCompile(`^\s*#\s+\S`),     // go build package error header
	regexp.MustCompile(`\bundefined:\s`), // go link/compile
	regexp.MustCompile(`(?i)\bexit status [1-9]`),
	regexp.MustCompile(`(?i)\bexit code [1-9]`),
	regexp.MustCompile(`(?i)\bcommand (?:failed|not found)\b`),
	regexp.MustCompile(`(?i)^\s*error[:\s]`),
	regexp.MustCompile(`(?i)\berror:`),
	regexp.MustCompile(`(?i)\bfatal:`),
	regexp.MustCompile(`(?i)\bnpm ERR!`),
	regexp.MustCompile(`(?i)\bTraceback \(most recent call last\)`),
	regexp.MustCompile(`(?i)\bassertion failed\b`),
	regexp.MustCompile(`(?i)\b(?:build|tests?)\s+failed\b`),
}

// editedFilePatterns capture the path a line reports as edited/written. They
// cover claude stream-json tool inputs ("file_path": "…"), common human-style
// verbs some CLIs print, and unified-diff headers.
var editedFilePatterns = []*regexp.Regexp{
	regexp.MustCompile(`"file_path"\s*:\s*"([^"]+)"`),
	regexp.MustCompile(`(?i)^\s*(?:Edited|Editing|Wrote|Writing|Created|Creating|Modified|Updated)\s+(?:file\s+)?(\S+)`),
	regexp.MustCompile(`^\+\+\+ b/(\S+)`),
	regexp.MustCompile(`^diff --git a/\S+ b/(\S+)`),
}

// ExtractPostmortem reads up to tailLines trailing lines of the worker log at
// path and returns a compact, secret-redacted post-mortem of the failed
// attempt (or "" when the log is missing, empty, or unreadable — callers treat
// "" as "no section"). Pass tailLines <= 0 to use PostmortemTailLines.
func ExtractPostmortem(path string, tailLines int) string {
	if strings.TrimSpace(path) == "" {
		return ""
	}
	if tailLines <= 0 {
		tailLines = PostmortemTailLines
	}
	tail, err := readLogTail(path, tailLines)
	if err != nil {
		return "" // missing / unreadable ⇒ graceful degradation
	}
	if strings.TrimSpace(tail) == "" {
		return "" // empty log ⇒ no section
	}
	return extractPostmortemBody(tail)
}

// readLogTail returns the last limit lines of the file at path joined by
// newlines. It mirrors the orchestrator's readLastLines reader.
func readLogTail(path string, limit int) (string, error) {
	if limit <= 0 {
		return "", nil
	}
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	lines := make([]string, 0, limit)
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
		if len(lines) > limit {
			lines = lines[1:]
		}
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}
	return strings.Join(lines, "\n"), nil
}

// extractPostmortemBody runs the heuristic pass over already-read log text and
// returns the redacted markdown body (or "" when nothing useful was found).
func extractPostmortemBody(logText string) string {
	rawLines := strings.Split(logText, "\n")

	var failures, files, actions []string
	seenFailure := map[string]bool{}
	seenFile := map[string]bool{}

	for _, raw := range rawLines {
		line := trimPostmortemLine(raw)
		if line == "" {
			continue
		}
		if len(failures) < maxPostmortemFailureLines && matchesAny(line, failureSignatures) && !seenFailure[line] {
			seenFailure[line] = true
			failures = append(failures, line)
		}
		if len(files) < maxPostmortemEditedFiles {
			if path := extractEditedFile(raw); path != "" && !seenFile[path] {
				seenFile[path] = true
				files = append(files, path)
			}
		}
	}

	// Last actions: the final non-empty lines, in chronological order.
	for i := len(rawLines) - 1; i >= 0 && len(actions) < maxPostmortemLastActions; i-- {
		line := trimPostmortemLine(rawLines[i])
		if line == "" {
			continue
		}
		actions = append(actions, line)
	}
	reverse(actions)

	if len(failures) == 0 && len(files) == 0 && len(actions) == 0 {
		return ""
	}

	var b strings.Builder
	if len(failures) > 0 {
		b.WriteString("### Errors / failed commands observed\n")
		for _, f := range failures {
			fmt.Fprintf(&b, "- %s\n", f)
		}
		b.WriteString("\n")
	}
	if len(files) > 0 {
		b.WriteString("### Files the previous attempt touched\n")
		for _, f := range files {
			fmt.Fprintf(&b, "- %s\n", f)
		}
		b.WriteString("\n")
	}
	if len(actions) > 0 {
		b.WriteString("### Last actions before the attempt ended\n")
		for _, a := range actions {
			fmt.Fprintf(&b, "- %s\n", a)
		}
	}

	return redactPostmortemSecrets(strings.TrimRight(b.String(), "\n"))
}

// CapPostmortem hard-caps a post-mortem to at most capBytes of content and
// appends a short truncation marker when it had to cut. Truncation lands on a
// line boundary where possible and always yields valid UTF-8. capBytes <= 0
// disables the cap.
func CapPostmortem(s string, capBytes int) string {
	if capBytes <= 0 || len(s) <= capBytes {
		return s
	}
	truncated := s[:capBytes]
	if idx := strings.LastIndexByte(truncated, '\n'); idx > 0 {
		truncated = truncated[:idx]
	}
	truncated = sanitizePromptUTF8(truncated)
	return strings.TrimRight(truncated, "\n") +
		"\n\n… (post-mortem truncated for the prompt; full version saved to the state dir)"
}

func trimPostmortemLine(raw string) string {
	line := strings.TrimRight(raw, " \t\r")
	line = strings.TrimSpace(line)
	if line == "" {
		return ""
	}
	runes := []rune(line)
	if len(runes) > maxPostmortemLineRunes {
		line = string(runes[:maxPostmortemLineRunes]) + " …"
	}
	return line
}

func matchesAny(line string, patterns []*regexp.Regexp) bool {
	for _, re := range patterns {
		if re.MatchString(line) {
			return true
		}
	}
	return false
}

func extractEditedFile(raw string) string {
	for _, re := range editedFilePatterns {
		if m := re.FindStringSubmatch(raw); m != nil {
			path := strings.TrimSpace(m[1])
			path = strings.Trim(path, `"',`)
			if path != "" {
				return path
			}
		}
	}
	return ""
}

func reverse(s []string) {
	for i, j := 0, len(s)-1; i < j; i, j = i+1, j-1 {
		s[i], s[j] = s[j], s[i]
	}
}
