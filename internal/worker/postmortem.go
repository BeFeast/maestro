package worker

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"
	"unicode/utf8"
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

// pemRedactionMarker replaces a redacted PEM private-key block, whether the
// complete-block regex or the tail-clipped scrubber (redactClippedPEM) caught it.
const pemRedactionMarker = "[REDACTED_PRIVATE_KEY_BLOCK]"

// postmortemMultilineRedactions collapse credential shapes whose body spans
// several lines. The per-line postmortemRedactions above only mask the first
// line of such a value — a `KEY="-----BEGIN … KEY-----` assignment redacts the
// assignment line but leaves the PEM body and footer lines, which are still
// collected as recent actions and reach the prompt / persisted file (#835
// review). These run first, over the whole assembled text, so a multiline
// secret is redacted in full before the single-line patterns run.
var postmortemMultilineRedactions = []struct {
	re   *regexp.Regexp
	repl string
}{
	// PEM blocks: -----BEGIN … KEY----- … -----END … KEY----- (also CERTIFICATE).
	// (?s) lets . span the base64 body/footer lines — including any per-line
	// bullet prefixes the post-mortem adds — and the lazy .*? stops at the first
	// matching END so adjacent blocks are not merged. Catches a bare PEM key
	// dumped to the log as well as one embedded in a KEY="…" assignment. A block
	// whose BEGIN banner fell outside the scanned tail is handled separately by
	// redactClippedPEM.
	{regexp.MustCompile(`(?is)-----BEGIN[ A-Z0-9]+-----.*?-----END[ A-Z0-9]+-----`), pemRedactionMarker},
	// KEY="…" whose closing quote lands on a later line (any multiline secret
	// value). The embedded \n forces a genuine multiline match; single-line
	// assignments are left to the per-line patterns. [^"] spans newlines, so the
	// whole value through its closing quote — and the bullet-prefixed body lines
	// inside it — is masked.
	{regexp.MustCompile(`(?i)\b([A-Z0-9_]*(?:TOKEN|SECRET|PASSWORD|PASSWD|API[_-]?KEY|PRIVATE[_-]?KEY)[A-Z0-9_]*)\s*[:=]\s*"[^"]*\n[^"]*"`), "${1}=[REDACTED]"},
}

// pemMarkerRe matches a PEM banner line (-----BEGIN … KEY----- / -----END …
// -----, also CERTIFICATE), after any list-bullet prefix the post-mortem adds.
var pemMarkerRe = regexp.MustCompile(`^-{5}(?:BEGIN|END)[ A-Z0-9]+-{5}$`)

// pemStrongBodyRe matches a full-width PEM/DER base64 body line: >=44 base64
// characters with optional '=' padding. 44 is above a bare 40-char git SHA, so a
// lone hash is never mistaken for key material; canonical PEM body lines are 64.
var pemStrongBodyRe = regexp.MustCompile(`^[A-Za-z0-9+/]{44,}={0,3}$`)

// pemWeakBodyRe matches a short base64 fragment (>=8 chars) — a clipped final
// body line. A short fragment is ambiguous: it may be a PEM body tail whose
// banners and full-width lines were clipped out of the tail, or a benign lone
// token (a git SHA, a digest). It is disambiguated by pemHexHashRe below.
var pemWeakBodyRe = regexp.MustCompile(`^[A-Za-z0-9+/]{8,}={0,3}$`)

// pemHexHashRe matches a fragment that is plausibly a git SHA / digest and so may
// be benign: purely LOWERCASE hex digits, with no uppercase A–F, no base64-only
// character (g-z, '+', '/'), and no '=' padding. The lowercase restriction is
// what separates a hash from key material: a PEM/DER base64 body line draws on the
// full base64 alphabet, so it routinely carries uppercase letters, '+', '/', or
// padding — none of which survive here — whereas git SHAs, checksums, and hex
// digests are conventionally lowercase. A short fragment that is NOT lowercase hex
// is therefore treated as clipped key material and redacted even in isolation.
// This is where an uppercase-bearing hex fragment such as `DEADBEEF…` — a
// plausible clipped PEM body tail — is caught: the earlier `[0-9a-fA-F]` form
// accepted it as a hash and left it weak, leaking the short final body line of a
// tail-clipped key (#835 review — short PEM tail leak). A lowercase-hex fragment
// stays weak and is only swept into a redaction run when adjacent to a marker or
// full-width body line, so a lone git SHA is still not over-redacted.
var pemHexHashRe = regexp.MustCompile(`^[0-9a-f]+$`)

// pemInlineBodyRe finds a candidate PEM/DER base64 run embedded anywhere in a
// line, such as command output (`$ echo MIIEv…`, `cat id_rsa: MIIEv…`).
// redactClippedPEM only recognizes a body that is the whole line (after any list
// bullet), so an inline clipped body carrying a command or label prefix would
// otherwise survive into the prompt and persisted file (#835 review).
//
// It matches any base64 run of >=8 characters with optional '=' padding and
// hands the accept/reject decision to inlineRunIsKeyMaterial. The floor is a
// deliberately broad >=8 whether or not the run carries '=' padding: an earlier
// >=16-only bar for UNPADDED runs skipped short fragments such as
// `$ echo XyZ12aBc` before the classifier could see them, leaking the raw key
// tail into both sinks (#835 review — unpadded inline PEM tail leak). 8 is the
// shortest run the classifier can still tell apart from the identifiers and
// paths a post-mortem records, so a lower floor would only feed it fragments it
// must preserve anyway.
//
// The run is unanchored and bounded by non-base64 characters, so the surrounding
// prose ("$ echo", "cat id_rsa:") is preserved. Greedy matching grabs the whole
// run plus any trailing '=' padding, so a full-width padded body is masked whole
// rather than split.
var pemInlineBodyRe = regexp.MustCompile(`[A-Za-z0-9+/]{8,}={0,3}`)

// listBulletPrefixRe captures the leading whitespace and optional list bullet
// ("- ", "* ", "+ ") of a post-mortem line so classification ignores the bullet
// and a redacted run can be re-emitted with the same one.
var listBulletPrefixRe = regexp.MustCompile(`^\s*(?:[-*+]\s+)?`)

// redactPostmortemSecrets masks common credential shapes in place. Multiline
// patterns run first so a PEM key or other multiline secret value is collapsed
// to a single redacted token; redactClippedPEM then catches key material whose
// BEGIN banner fell outside the scanned tail; the per-line patterns run last.
func redactPostmortemSecrets(text string) string {
	for _, r := range postmortemMultilineRedactions {
		text = r.re.ReplaceAllString(text, r.repl)
	}
	text = redactClippedPEM(text)
	text = redactInlinePEMBody(text)
	for _, r := range postmortemRedactions {
		text = r.re.ReplaceAllString(text, r.repl)
	}
	return text
}

// redactInlinePEMBody masks a base64 body run embedded inside a larger line —
// the inline complement to redactClippedPEM's whole-line scrubber. It runs after
// redactClippedPEM (whole-line body runs are already collapsed to a marker there,
// so this only catches genuinely embedded runs like `$ echo <body>` or `cat
// id_rsa: <body>`) and masks just the base64 run inlineRunIsKeyMaterial flags as
// key material, keeping the command or label prefix so the post-mortem still
// records what the attempt tried (#835 review).
func redactInlinePEMBody(text string) string {
	return pemInlineBodyRe.ReplaceAllStringFunc(text, func(run string) string {
		if inlineRunIsKeyMaterial(run) {
			return pemRedactionMarker
		}
		return run
	})
}

// inlineRunIsKeyMaterial reports whether an embedded base64 run (>=8 chars,
// pemInlineBodyRe) is (clipped) PEM/DER key material worth redacting. It mirrors
// the whole-line classification in redactClippedPEM but is tuned to spare the
// identifiers and file paths that legitimately fill a post-mortem:
//
//   - a '='-padded run, or a full-width run (>=44 non-padding chars), is key
//     material outright — the same shapes the padded whole-line tail and
//     pemStrongBodyRe already redact ('=' padding never appears in a git SHA /
//     hex digest, and 44 is above a bare 40-char SHA);
//   - a lowercase-hex fragment (pemHexHashRe) is a plausible git SHA / digest and
//     stays intact, matching redactClippedPEM's carve-out;
//   - a shorter UNPADDED run is key material when it carries BOTH an uppercase
//     letter and a digit — the encoded-byte mix a clipped final PEM body line such
//     as `$ echo XyZ12aBc` has and the benign runs reaching this matcher do not: a
//     lowercase file path keeps its digits but has no uppercase
//     (`internal/attempt2`, `stream/01`), and a camelCase identifier keeps its case
//     but has no digit (`handleUserRequest`);
//   - a run carrying a '+' is key material: '+' is a base64-only character that
//     never appears in a file path, a Go identifier, or a hex digest, so its
//     presence marks an encoded blob (#835 review — mixed-case inline PEM tail leak,
//     `$ echo abc+DefG`);
//   - an ALL-UPPERCASE run (no lowercase, no digit) at or above the >=8 inline
//     floor is likewise key material. The base64 alphabet spans all 26 uppercase
//     letters, so a clipped DER body window can land entirely in A–Z with no other
//     signal, and an 8–15 char fragment such as `$ echo ABCDEFGH` is
//     indistinguishable by shape from a bare-caps clipped key tail. A previous
//     >=16-char floor left the 8–15 band weak and leaked the raw tail into the
//     prompt and persisted file (#835 review — short all-caps inline PEM tail
//     leak); a secrets-hygiene extractor resolves the ambiguity toward masking. The
//     rare legitimate all-caps word of the same width (`HOSTNAME`, `PASSWORD`) is
//     over-redacted, while shorter all-caps tokens (`SIGKILL`, HTTP verbs) fall
//     below the >=8 floor and survive;
//   - a MIXED-CASE, digit-free run of >=8 chars is key material when it holds no
//     same-case letter run of >=4 — the "word" or "acronym" signal of a real
//     identifier. A camelCase/PascalCase name the post-mortem records
//     (`handleUserRequest`, `HTTPSConn`) always carries such a run, while a clipped
//     base64 key tail (`$ echo AbCdEfGh`) alternates case almost every character
//     with no readable word to lean on (#835 review — mixed-case inline PEM tail
//     leak). The rare short-word identifier (`getUrlFor`) is over-redacted, the safe
//     direction for a secrets-hygiene extractor.
//
// A run whose only signal is LOWERCASE — a file path or lowercase word
// (`internal/attempt2`, `something`) — is indistinguishable from ordinary
// post-mortem prose and is left intact, so the extractor does not garble the
// identifiers and paths it exists to record. The residual key shapes it declines
// here are covered by the padded / full-width / whole-line-clipped and
// complete-block branches.
func inlineRunIsKeyMaterial(run string) bool {
	body := strings.TrimRight(run, "=")
	if len(body) < len(run) { // carried '=' padding — an encoded blob, not a hash
		return true
	}
	if len(body) >= 44 { // full-width PEM/DER body line
		return true
	}
	if pemHexHashRe.MatchString(body) { // a lone lowercase git SHA / digest
		return false
	}
	if strings.Contains(body, "+") { // '+' is base64-only: never a path/identifier/hash
		return true
	}
	hasUpper := strings.ContainsAny(body, "ABCDEFGHIJKLMNOPQRSTUVWXYZ")
	hasLower := strings.ContainsAny(body, "abcdefghijklmnopqrstuvwxyz")
	hasDigit := strings.ContainsAny(body, "0123456789")
	if hasUpper && hasDigit { // encoded-byte mix of a clipped key body tail
		return true
	}
	if hasUpper && !hasLower && len(body) >= 8 {
		// An all-uppercase base64 run has no lowercase or digit to lean on; mask it at
		// the >=8 inline floor rather than leak a clipped 8–15 char key tail.
		return true
	}
	// A mixed-case, digit-free run: a camelCase identifier or PascalCase type carries
	// a word/acronym run of >=4 same-case letters; a clipped base64 key tail
	// (`AbCdEfGh`) alternates case densely with no such run. Mask the
	// dense-alternation shape at the >=8 inline floor while the identifier survives.
	return hasUpper && hasLower && len(body) >= 8 && longestSameCaseRun(body) < 4
}

// longestSameCaseRun returns the length of the longest maximal run of adjacent
// same-case ASCII letters in s; digits and any other character break a run. A
// readable identifier or type name carries a word or acronym of several letters in
// one case (`handle`, `HTTPS`); a base64 key fragment alternates case almost every
// character, so its longest same-case run is short.
func longestSameCaseRun(s string) int {
	best, cur := 0, 0
	var prevUpper, have bool
	for i := 0; i < len(s); i++ {
		c := s[i]
		upper := c >= 'A' && c <= 'Z'
		lower := c >= 'a' && c <= 'z'
		if !upper && !lower {
			cur, have = 0, false
			continue
		}
		if have && upper == prevUpper {
			cur++
		} else {
			cur = 1
		}
		prevUpper, have = upper, true
		if cur > best {
			best = cur
		}
	}
	return best
}

// redactClippedPEM masks PEM private-key material whose -----BEGIN----- banner
// fell outside the scanned log tail, so the complete-block regex in
// postmortemMultilineRedactions cannot see it (#835 review). When a 400-line
// tail starts inside a key dump, the orphaned base64 body lines (and any END
// footer) otherwise survive into the "last actions" section and reach the prompt
// and persisted file.
//
// It is line-oriented over the assembled body: classify each line (after any
// list bullet) as a PEM marker, a full-width base64 body line, a short base64
// body tail (a short fragment that is not a plausible hex hash — see
// pemHexHashRe), or a short hex fragment, then redact each maximal contiguous run
// of such lines that either contains a marker or holds at least one full-width /
// short-body-tail anchor line. A single anchor is enough because a tail clipped
// so that both PEM banners fall outside it can leave one genuine key body line
// alone — full-width or, when the clip lands on the final line, short (#835
// review). A run of only short hex fragments (no anchor, no marker) is left alone
// — a lone hex token may be a git SHA or digest, not key material.
func redactClippedPEM(text string) string {
	lines := strings.Split(text, "\n")
	n := len(lines)
	const (
		other = iota
		weak
		strong
		marker
	)
	class := make([]int, n)
	for i, ln := range lines {
		content := ln[len(listBulletPrefixRe.FindString(ln)):]
		switch {
		case pemMarkerRe.MatchString(content):
			class[i] = marker
		case pemStrongBodyRe.MatchString(content):
			class[i] = strong
		case pemWeakBodyRe.MatchString(content):
			// A short base64 fragment. Anchor it as key material (a clipped PEM
			// body tail) unless it is a plausible git SHA / digest: only a
			// LOWERCASE-hex fragment (no uppercase A–F, no base64-only character,
			// no '=' padding — see pemHexHashRe) stays weak, since a real PEM body
			// line almost always carries a character outside lowercase hex. Any
			// other short fragment — uppercase hex included — is redacted even in
			// isolation (#835 review — short PEM tail leak).
			if pemHexHashRe.MatchString(content) {
				class[i] = weak
			} else {
				class[i] = strong
			}
		}
	}
	// A line joins a PEM run if it is a marker or full-width body line, or a weak
	// fragment transitively adjacent to one (so a clipped final body line next to
	// the block is swept in, but an isolated short token is not).
	inRun := make([]bool, n)
	for i, c := range class {
		if c == marker || c == strong {
			inRun[i] = true
		}
	}
	for changed := true; changed; {
		changed = false
		for i := 0; i < n; i++ {
			if inRun[i] || class[i] != weak {
				continue
			}
			if (i > 0 && inRun[i-1]) || (i+1 < n && inRun[i+1]) {
				inRun[i] = true
				changed = true
			}
		}
	}
	// Walk maximal runs; redact those with a marker or any anchor body line
	// (full-width, or a short non-hex body tail), collapsing each to a single
	// marker that keeps the run's first bullet. One anchor is enough: when a PEM
	// dump is clipped so both banners fall outside the scanned tail, a genuine key
	// body line can remain alone (#835 review) — requiring >=2 leaked it, and a
	// tail clipped on the final line leaves only a short body tail. The guard is
	// also a defensive floor: it never collapses a run that carries no key-material
	// evidence, so a run of only short hex fragments (a lone SHA or digest) is
	// left intact.
	var out []string
	for i := 0; i < n; {
		if !inRun[i] {
			out = append(out, lines[i])
			i++
			continue
		}
		j := i
		hasMarker, strongCount := false, 0
		for j < n && inRun[j] {
			switch class[j] {
			case marker:
				hasMarker = true
			case strong:
				strongCount++
			}
			j++
		}
		if hasMarker || strongCount >= 1 {
			prefix := listBulletPrefixRe.FindString(lines[i])
			out = append(out, prefix+pemRedactionMarker)
		} else {
			out = append(out, lines[i:j]...)
		}
		i = j
	}
	return strings.Join(out, "\n")
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

// postmortemTruncationMarker is appended when CapPostmortem has to cut. Its
// byte length is reserved out of the caller's budget so the returned excerpt —
// content plus marker — never exceeds capBytes (#835 review).
const postmortemTruncationMarker = "\n\n… (post-mortem truncated for the prompt; full version saved to the state dir)"

// CapPostmortem hard-caps a post-mortem so the returned string is at most
// capBytes and appends a short truncation marker when it had to cut. The marker
// is counted against capBytes, so the total never exceeds the budget. Truncation
// lands on a line boundary where possible and always yields valid UTF-8.
// capBytes <= 0 disables the cap.
func CapPostmortem(s string, capBytes int) string {
	if capBytes <= 0 || len(s) <= capBytes {
		return s
	}
	// Make the text valid UTF-8 up front so the subsequent byte-slice can only
	// ever remove bytes (sanitizing after the slice could expand a split rune to
	// U+FFFD and push content+marker back over capBytes).
	s = sanitizePromptUTF8(s)
	if len(s) <= capBytes {
		return s
	}
	// Reserve room for the marker so content+marker stays within capBytes. When
	// the cap is too small to fit the marker at all, fall back to a marker-free
	// excerpt bounded to a rune boundary (never grows past capBytes).
	budget := capBytes - len(postmortemTruncationMarker)
	if budget <= 0 {
		return truncateToRuneBoundary(s, capBytes)
	}
	truncated := truncateToRuneBoundary(s, budget)
	if idx := strings.LastIndexByte(truncated, '\n'); idx > 0 {
		truncated = truncated[:idx]
	}
	return strings.TrimRight(truncated, "\n") + postmortemTruncationMarker
}

// truncateToRuneBoundary returns the longest prefix of s that is at most
// capBytes bytes and does not split a UTF-8 rune. It only ever removes bytes,
// so the result is guaranteed <= capBytes (unlike sanitizePromptUTF8, which can
// grow a string by expanding invalid bytes to U+FFFD). s is assumed valid
// UTF-8, so the only possible invalid tail is a rune the byte-slice split.
func truncateToRuneBoundary(s string, capBytes int) string {
	if capBytes <= 0 {
		return ""
	}
	if len(s) <= capBytes {
		return s
	}
	b := s[:capBytes]
	for len(b) > 0 {
		if r, size := utf8.DecodeLastRuneInString(b); r == utf8.RuneError && size <= 1 {
			b = b[:len(b)-1] // drop the partial trailing byte left by the slice
			continue
		}
		break
	}
	return b
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
