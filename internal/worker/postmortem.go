package worker

import (
	"bufio"
	"encoding/json"
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

// workerLogAttemptMarker is the banner buildWorkerRunnerScript writes to
// slot.log at the start of every (re)spawn ("[maestro] worker worktree: …").
// Respawns append to the same slot.log, so this line is the per-attempt
// boundary: the tail can otherwise carry an older attempt's failures/files
// whenever the just-failed attempt produced fewer than the scanned lines, and
// the prompt would mislabel that stale content as the current attempt (#835
// review). isolateCurrentAttempt slices the tail to the region after the last
// marker before extraction.
const workerLogAttemptMarker = "[maestro] worker worktree:"

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
	regexp.MustCompile(`^\s*\[codex\]\s+exit=[1-9]`), // codex stream-splitter's rendered non-zero exit
	regexp.MustCompile(`(?i)\bcommand (?:failed|not found)\b`),
	regexp.MustCompile(`(?i)^\s*error[:\s]`),
	regexp.MustCompile(`(?i)\berror:`),
	regexp.MustCompile(`(?i)\bfatal:`),
	regexp.MustCompile(`(?i)\bnpm ERR!`),
	regexp.MustCompile(`(?i)\bTraceback \(most recent call last\)`),
	regexp.MustCompile(`(?i)\bassertion failed\b`),
	regexp.MustCompile(`(?i)\b(?:build|tests?)\s+failed\b`),
}

// editedFilePatterns capture the path a line reports as edited/written from the
// human-readable slot.log. They cover raw claude stream-json tool inputs
// ("file_path": "…") on non-stream-split logs, the codex stream-splitter's
// rendered file-change form ("[codex] <kind> <path>"), common human-style verbs
// some CLIs print, and unified-diff headers. claude's stream-splitter renders
// tool_use without its file_path input, so those edits are recovered from the
// raw .jsonl side channel instead (see filesFromSideChannel).
var editedFilePatterns = []*regexp.Regexp{
	regexp.MustCompile(`"file_path"\s*:\s*"([^"]+)"`),
	// codex `exec --json` file_change rendered by the stream-splitter as
	// "[codex] <kind> <path>" (renderCodexItem). The two whitespace-separated
	// tokens after the tag distinguish it from "[codex] $ …", "[codex] exit=…",
	// and "[codex] usage: …", none of which present as <word> <token>.
	regexp.MustCompile(`^\s*\[codex\]\s+\w+\s+(\S+)`),
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
	// Bound extraction to the just-failed attempt: respawns append to the same
	// slot.log, so a short final attempt would otherwise inherit an older
	// attempt's failures/files (#835 review).
	tail = isolateCurrentAttempt(tail)
	if strings.TrimSpace(tail) == "" {
		return "" // empty log ⇒ no section
	}
	// The claude stream-splitter renders tool_use without its file_path input,
	// so the "files touched" section would be empty for stream-split logs.
	// Recover those paths from the raw NDJSON side channel, likewise bounded to
	// the current attempt. Best-effort: a missing/unreadable jsonl yields none.
	sideChannelFiles := filesFromSideChannel(JSONLPathForLog(path), tailLines)
	return extractPostmortemBody(tail, sideChannelFiles)
}

// isolateCurrentAttempt returns the slice of log text after the last line whose
// trimmed form begins with workerLogAttemptMarker — the banner the worker
// runner writes at the start of every (re)spawn. The final marker delimits the
// most recent attempt, so everything after it is that attempt's own output. The
// marker line itself is dropped (it carries a host worktree path). When no
// marker is present — a non-stream-split/legacy log, or an attempt longer than
// the scanned tail — the whole tail is already current-attempt output and is
// returned unchanged.
func isolateCurrentAttempt(text string) string {
	lines := strings.Split(text, "\n")
	last := -1
	for i, l := range lines {
		if strings.HasPrefix(strings.TrimSpace(l), workerLogAttemptMarker) {
			last = i
		}
	}
	if last < 0 {
		return text
	}
	return strings.Join(lines[last+1:], "\n")
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
// sideChannelFiles are paths recovered from the raw .jsonl side channel (edits
// the rendered slot.log dropped); they are merged after the in-log matches so
// log order is preserved.
func extractPostmortemBody(logText string, sideChannelFiles []string) string {
	rawLines := strings.Split(logText, "\n")

	var failures, files, actions []string
	seenFailure := map[string]bool{}
	seenFile := map[string]bool{}

	addFile := func(path string) {
		path = strings.TrimSpace(path)
		if path == "" || seenFile[path] || len(files) >= maxPostmortemEditedFiles {
			return
		}
		seenFile[path] = true
		files = append(files, path)
	}

	for _, raw := range rawLines {
		line := trimPostmortemLine(raw)
		if line == "" {
			continue
		}
		if len(failures) < maxPostmortemFailureLines && matchesAny(line, failureSignatures) && !seenFailure[line] {
			seenFailure[line] = true
			failures = append(failures, line)
		}
		if path := extractEditedFile(raw); path != "" {
			addFile(path)
		}
	}
	for _, path := range sideChannelFiles {
		addFile(path)
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

// filesFromSideChannel recovers the paths of files the attempt edited from the
// raw NDJSON side channel (slot.jsonl) the stream-splitter appends alongside the
// rendered slot.log. This is where claude tool_use edits keep their file_path
// (the rendered log drops it) and where codex file_change items keep their
// paths. Best-effort: an empty path, a missing/unreadable jsonl, or a log that
// never used stream-split all yield no extra files. Like the log tail it is
// bounded to the current attempt so a respawn's earlier edits are not attributed
// to the just-failed one.
func filesFromSideChannel(jsonlPath string, tailLines int) []string {
	if strings.TrimSpace(jsonlPath) == "" {
		return nil
	}
	if tailLines <= 0 {
		tailLines = PostmortemTailLines
	}
	tail, err := readLogTail(jsonlPath, tailLines)
	if err != nil || strings.TrimSpace(tail) == "" {
		return nil
	}
	return filesFromRawFrames(isolateCurrentAttemptJSONL(tail))
}

// isolateCurrentAttemptJSONL slices raw NDJSON to the frames at and after the
// last session-start frame. The side channel carries no worker-worktree banner
// (that goes straight to slot.log, not through the splitter), so the backend's
// own run-start frame — claude's system/init, codex's thread.started — is the
// per-attempt boundary. When none is present the whole tail is returned.
func isolateCurrentAttemptJSONL(text string) string {
	lines := strings.Split(text, "\n")
	last := -1
	for i, l := range lines {
		if isSessionStartFrame(l) {
			last = i
		}
	}
	if last < 0 {
		return text
	}
	return strings.Join(lines[last:], "\n")
}

// isSessionStartFrame reports whether a raw NDJSON line is a backend's
// session-start frame: claude emits {"type":"system","subtype":"init",…} and
// codex emits {"type":"thread.started",…} at the start of each (re)spawn.
func isSessionStartFrame(line string) bool {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" || trimmed[0] != '{' {
		return false
	}
	var f struct {
		Type    string `json:"type"`
		Subtype string `json:"subtype"`
	}
	if json.Unmarshal([]byte(trimmed), &f) != nil {
		return false
	}
	switch f.Type {
	case "system":
		return f.Subtype == "init"
	case "thread.started":
		return true
	}
	return false
}

// rawFileFrame is the subset of a raw NDJSON frame that carries an edited path:
// a claude assistant/user message (tool_use blocks live in Content) or a codex
// item envelope (file_change items carry Changes).
type rawFileFrame struct {
	Message *struct {
		Content json.RawMessage `json:"content"`
	} `json:"message"`
	Item *struct {
		Changes []struct {
			Path string `json:"path"`
		} `json:"changes"`
	} `json:"item"`
}

// rawToolBlock is the subset of a claude message content block needed to
// recover an edit target: a tool_use block whose input names a file.
type rawToolBlock struct {
	Type  string `json:"type"`
	Input struct {
		FilePath     string `json:"file_path"`
		NotebookPath string `json:"notebook_path"`
	} `json:"input"`
}

// filesFromRawFrames extracts edited-file paths from raw NDJSON frames: claude
// tool_use file_path/notebook_path inputs and codex file_change item paths.
// Paths are de-duplicated and capped at maxPostmortemEditedFiles.
func filesFromRawFrames(text string) []string {
	var files []string
	seen := map[string]bool{}
	add := func(path string) {
		path = strings.TrimSpace(path)
		if path == "" || seen[path] || len(files) >= maxPostmortemEditedFiles {
			return
		}
		seen[path] = true
		files = append(files, path)
	}
	for _, raw := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(raw)
		if trimmed == "" || trimmed[0] != '{' {
			continue
		}
		var fr rawFileFrame
		if json.Unmarshal([]byte(trimmed), &fr) != nil {
			continue
		}
		if fr.Message != nil {
			for _, p := range claudeEditPaths(fr.Message.Content) {
				add(p)
			}
		}
		if fr.Item != nil {
			for _, c := range fr.Item.Changes {
				add(c.Path)
			}
		}
	}
	return files
}

// claudeEditPaths returns the file paths of the tool_use blocks in a claude
// message content field. Only blocks that actually carry a file_path or
// notebook_path input contribute, so Read/Grep/Bash tool_use blocks add
// nothing; content that is a plain string (some user messages) yields none.
func claudeEditPaths(content json.RawMessage) []string {
	if len(content) == 0 {
		return nil
	}
	var blocks []rawToolBlock
	if json.Unmarshal(content, &blocks) != nil {
		return nil
	}
	var paths []string
	for _, b := range blocks {
		if b.Type != "tool_use" {
			continue
		}
		if p := strings.TrimSpace(b.Input.FilePath); p != "" {
			paths = append(paths, p)
		} else if p := strings.TrimSpace(b.Input.NotebookPath); p != "" {
			paths = append(paths, p)
		}
	}
	return paths
}

func reverse(s []string) {
	for i, j := 0, len(s)-1; i < j; i, j = i+1, j-1 {
		s[i], s[j] = s[j], s[i]
	}
}
