package worker

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestExtractPostmortem_FailedCommands(t *testing.T) {
	log := strings.Join([]string{
		"running go test ./...",
		"--- FAIL: TestFoo (0.01s)",
		"    foo_test.go:42: expected 3, got 4",
		"FAIL\tgithub.com/befeast/maestro/internal/foo\t0.02s",
		"internal/bar/bar.go:10:2: undefined: doStuff",
		"exit status 1",
		"some unrelated chatter that is fine",
	}, "\n")

	path := writeTempLog(t, log)
	out := ExtractPostmortem(path, PostmortemTailLines)

	if out == "" {
		t.Fatal("expected a non-empty post-mortem")
	}
	for _, want := range []string{
		"--- FAIL: TestFoo",
		"undefined: doStuff",
		"exit status 1",
		"Errors / failed commands observed",
		"Last actions before the attempt ended",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("post-mortem missing %q:\n%s", want, out)
		}
	}
	// A benign line must not be pulled into the failures section, but it is the
	// last line so it appears under "last actions".
	if strings.Count(out, "unrelated chatter") != 1 {
		t.Errorf("benign line should appear once (as a last action), got:\n%s", out)
	}
}

func TestExtractPostmortem_EditedFiles(t *testing.T) {
	log := strings.Join([]string{
		`{"type":"tool_use","name":"Edit","input":{"file_path":"/repo/internal/a.go"}}`,
		`Wrote internal/b.go`,
		`+++ b/internal/c.go`,
		`diff --git a/internal/d.go b/internal/d.go`,
		"done",
	}, "\n")

	out := ExtractPostmortem(writeTempLog(t, log), PostmortemTailLines)
	for _, want := range []string{
		"Files the previous attempt touched",
		"/repo/internal/a.go",
		"internal/b.go",
		"internal/c.go",
		"internal/d.go",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("post-mortem missing edited file %q:\n%s", want, out)
		}
	}
}

func TestExtractPostmortem_EmptyLogNoSection(t *testing.T) {
	// Empty file.
	if got := ExtractPostmortem(writeTempLog(t, ""), PostmortemTailLines); got != "" {
		t.Errorf("empty log should yield no post-mortem, got %q", got)
	}
	// Whitespace-only file.
	if got := ExtractPostmortem(writeTempLog(t, "   \n\t\n  "), PostmortemTailLines); got != "" {
		t.Errorf("whitespace-only log should yield no post-mortem, got %q", got)
	}
	// Missing file.
	if got := ExtractPostmortem(filepath.Join(t.TempDir(), "does-not-exist.log"), PostmortemTailLines); got != "" {
		t.Errorf("missing log should yield no post-mortem, got %q", got)
	}
	// Empty path.
	if got := ExtractPostmortem("", PostmortemTailLines); got != "" {
		t.Errorf("empty path should yield no post-mortem, got %q", got)
	}
}

func TestExtractPostmortem_RedactsSecrets(t *testing.T) {
	// Fabricated credentials are assembled via concatenation so secret
	// scanners (agent-lint) don't match them in the diff.
	fakeBearer := "sk-" + "abcdefghijklmnopqrstuvwxyz012345"
	fakeGHToken := "ghp_" + "abcdefghijklmnopqrstuvwxyz0123456789"
	log := strings.Join([]string{
		"error: request failed",
		"Authorization: Bearer " + fakeBearer,
		"exporting GITHUB_TOKEN=" + fakeGHToken,
		"API_KEY=supersecretvalue123",
		"last line",
	}, "\n")

	out := ExtractPostmortem(writeTempLog(t, log), PostmortemTailLines)

	for _, secret := range []string{
		fakeBearer,
		fakeGHToken,
		"supersecretvalue123",
	} {
		if strings.Contains(out, secret) {
			t.Errorf("post-mortem leaked secret %q:\n%s", secret, out)
		}
	}
	if !strings.Contains(out, "[REDACTED") && !strings.Contains(out, "=[REDACTED]") {
		t.Errorf("expected a redaction marker in output:\n%s", out)
	}
}

// TestExtractPostmortem_RedactsMultilineSecrets covers PEM/multiline secret
// bodies: the per-line patterns only mask the assignment line, so the base64
// body and footer lines of a `KEY="-----BEGIN … KEY-----` value survived into
// the prompt and persisted file (#835 review).
func TestExtractPostmortem_RedactsMultilineSecrets(t *testing.T) {
	// Fabricated PEM body and headers assembled at runtime so secret
	// scanners (agent-lint private-key rule) don't match them in the diff.
	body1 := "b3BlbnNzaC1rZXktdjEAAAAABG5vbmUAAAAEbm9uZQAAAAAAAAAB"
	body2 := "AAAAMwAAAAtzc2gtZWQyNTUxOQAAACDddeadbeefdeadbeefdeadbe"
	dashes := strings.Repeat("-", 5)
	beginOpenSSH := dashes + "BEGIN OPENSSH PRIVATE " + "KEY" + dashes
	endOpenSSH := dashes + "END OPENSSH PRIVATE " + "KEY" + dashes
	log := strings.Join([]string{
		"error: ssh auth failed",
		`SSH_PRIVATE_KEY="` + beginOpenSSH,
		body1,
		body2,
		endOpenSSH + `"`,
		"last line",
	}, "\n")

	out := ExtractPostmortem(writeTempLog(t, log), PostmortemTailLines)

	for _, secret := range []string{body1, body2, "BEGIN OPENSSH PRIVATE " + "KEY", "END OPENSSH PRIVATE " + "KEY"} {
		if strings.Contains(out, secret) {
			t.Errorf("post-mortem leaked multiline secret content %q:\n%s", secret, out)
		}
	}
	if !strings.Contains(out, "REDACTED") {
		t.Errorf("expected a redaction marker in output:\n%s", out)
	}
}

// TestExtractPostmortem_RedactsBarePEMBlock covers a private key dumped to the
// log outside any KEY=… assignment (e.g. a `cat key.pem` echo).
func TestExtractPostmortem_RedactsBarePEMBlock(t *testing.T) {
	// Headers assembled at runtime to avoid secret-scanner diff matches.
	body := "MIIBOgIBAAJBAKj34GkxFhD90vcNLYLInFEX6Ppy1tPf9Cnzj4p4WGeKLs1Pt8Q"
	dashes := strings.Repeat("-", 5)
	log := strings.Join([]string{
		"error: reading deploy key",
		dashes + "BEGIN RSA PRIVATE " + "KEY" + dashes,
		body,
		dashes + "END RSA PRIVATE " + "KEY" + dashes,
		"build failed",
	}, "\n")

	out := ExtractPostmortem(writeTempLog(t, log), PostmortemTailLines)

	if strings.Contains(out, body) {
		t.Errorf("post-mortem leaked bare PEM body %q:\n%s", body, out)
	}
	if !strings.Contains(out, "REDACTED_PRIVATE_KEY_BLOCK") {
		t.Errorf("expected PEM block redaction marker in output:\n%s", out)
	}
}

func TestCapPostmortem_Enforced(t *testing.T) {
	// A body far larger than the cap.
	var sb strings.Builder
	for i := 0; i < 500; i++ {
		sb.WriteString("- this is a reasonably long line of post-mortem content\n")
	}
	big := sb.String()
	if len(big) <= PostmortemPromptCapBytes {
		t.Fatalf("test setup: body should exceed cap, got %d", len(big))
	}

	capped := CapPostmortem(big, PostmortemPromptCapBytes)
	// The marker is reserved out of the budget: content plus marker must never
	// exceed capBytes (#835 review: marker previously overshot the cap).
	if len(capped) > PostmortemPromptCapBytes {
		t.Errorf("capped output exceeds cap: %d > %d bytes", len(capped), PostmortemPromptCapBytes)
	}
	if !strings.Contains(capped, "truncated") {
		t.Errorf("expected truncation marker in capped output")
	}

	// A small body is returned unchanged.
	small := "- short\n- body"
	if got := CapPostmortem(small, PostmortemPromptCapBytes); got != small {
		t.Errorf("small body should be unchanged, got %q", got)
	}
	// capBytes <= 0 disables the cap.
	if got := CapPostmortem(big, 0); got != big {
		t.Errorf("capBytes<=0 should disable cap")
	}
}

// TestCapPostmortem_MarkerWithinCap exercises a range of caps — including ones
// smaller than the truncation marker itself — and asserts the result stays
// within budget and remains valid UTF-8 (#835 review: marker overshoot).
func TestCapPostmortem_MarkerWithinCap(t *testing.T) {
	var sb strings.Builder
	for i := 0; i < 500; i++ {
		sb.WriteString("- a reasonably long line of post-mortem content ☃\n")
	}
	big := sb.String()

	for _, capBytes := range []int{10, 40, len(postmortemTruncationMarker), 100, 512, PostmortemPromptCapBytes} {
		got := CapPostmortem(big, capBytes)
		if len(got) > capBytes {
			t.Errorf("cap=%d: output %d bytes exceeds cap", capBytes, len(got))
		}
		if !utf8.ValidString(got) {
			t.Errorf("cap=%d: output is not valid UTF-8: %q", capBytes, got)
		}
	}
}

// TestCapPostmortem_MultibyteBoundary ensures a cap landing mid-rune does not
// split a multibyte character or overshoot the byte budget — across both the
// marker-free small-cap branch and the main branch (cap larger than the marker,
// no newline in the budget window, where an after-slice sanitize could
// previously expand a split rune and push content+marker back over the cap).
func TestCapPostmortem_MultibyteBoundary(t *testing.T) {
	s := strings.Repeat("☃", 200) // 3 bytes each, no newlines
	m := len(postmortemTruncationMarker)
	caps := []int{1, 2, 3, 5, 20, m - 1, m, m + 1, m + 2, m + 5, m + 10, m + 100}
	for _, capBytes := range caps {
		got := CapPostmortem(s, capBytes)
		if len(got) > capBytes {
			t.Fatalf("cap=%d: output %d bytes exceeds cap", capBytes, len(got))
		}
		if !utf8.ValidString(got) {
			t.Fatalf("cap=%d: output not valid UTF-8: %q", capBytes, got)
		}
	}
}

func TestExtractPostmortem_TailBounded(t *testing.T) {
	// A failure line far above the tail window must be dropped; a recent one kept.
	var lines []string
	lines = append(lines, "FAIL\tancient/package\t0.01s") // line 0, way above the tail
	for i := 0; i < PostmortemTailLines+50; i++ {
		lines = append(lines, "noise line filler")
	}
	lines = append(lines, "error: recent failure near the end")

	out := ExtractPostmortem(writeTempLog(t, strings.Join(lines, "\n")), PostmortemTailLines)
	if strings.Contains(out, "ancient/package") {
		t.Errorf("failure outside the tail window should be dropped:\n%s", out)
	}
	if !strings.Contains(out, "recent failure near the end") {
		t.Errorf("recent failure should be captured:\n%s", out)
	}
}

// #835 review comment 1: respawns append to the same slot.log, so the tail can
// carry an older attempt's output. The extractor isolates the region after the
// last worker-worktree banner (the per-(re)spawn marker) so the prior attempt's
// failures/files are not mislabeled as the current one — and the banner's host
// path never leaks into the post-mortem.
func TestExtractPostmortem_IsolatesCurrentAttempt(t *testing.T) {
	log := strings.Join([]string{
		"[maestro] worker worktree: /home/wt/slot-1",
		"--- FAIL: TestOld (0.00s)",
		"Wrote internal/old.go",
		"exit status 1",
		"[maestro] worker worktree: /home/wt/slot-1", // attempt 2 begins here
		"running go test ./...",
		"--- FAIL: TestNew (0.00s)",
		"Wrote internal/new.go",
	}, "\n")

	out := ExtractPostmortem(writeTempLog(t, log), PostmortemTailLines)

	for _, stale := range []string{"TestOld", "internal/old.go"} {
		if strings.Contains(out, stale) {
			t.Errorf("prior attempt content %q should be isolated out:\n%s", stale, out)
		}
	}
	for _, want := range []string{"TestNew", "internal/new.go"} {
		if !strings.Contains(out, want) {
			t.Errorf("current attempt content %q missing:\n%s", want, out)
		}
	}
	// The worker-worktree banner is a host path and must not reach the prompt.
	if strings.Contains(out, "worker worktree") {
		t.Errorf("worktree banner should not appear in the post-mortem:\n%s", out)
	}
}

// #835 review comment 2: the codex stream-splitter renders file changes as
// "[codex] <kind> <path>", which the earlier patterns ignored, so the edited
// files were dropped for stream-split codex logs. Command/exit lines under the
// same tag must not be misread as files.
func TestExtractPostmortem_CodexRenderedFileChange(t *testing.T) {
	log := strings.Join([]string{
		"[codex] $ /bin/bash -lc go test ./...",
		"--- FAIL: TestFoo (0.00s)",
		"[codex] exit=1",
		"[codex] modified internal/codexedit.go",
		"[codex] add newpkg/newfile.go",
	}, "\n")

	out := ExtractPostmortem(writeTempLog(t, log), PostmortemTailLines)

	for _, want := range []string{"internal/codexedit.go", "newpkg/newfile.go"} {
		if !strings.Contains(out, want) {
			t.Errorf("codex-rendered edit %q not captured:\n%s", want, out)
		}
	}
	// "[codex] $ …" (a command, not a file change) must not leak into the files
	// section — it legitimately appears under "last actions".
	if files := postmortemSection(out, "Files the previous attempt touched"); strings.Contains(files, "/bin/bash") {
		t.Errorf("codex command token misparsed as a file:\n%s", files)
	}
	// A rendered non-zero codex exit is a failure signal.
	if !strings.Contains(out, "[codex] exit=1") {
		t.Errorf("codex non-zero exit should be flagged as a failure:\n%s", out)
	}
}

// #835 review comment 2: the claude stream-splitter renders tool_use without
// its file_path input, so the edited files live only in the raw .jsonl side
// channel. The extractor reads that side channel to recover them.
func TestExtractPostmortem_ClaudeJSONLSideChannel(t *testing.T) {
	// slot.log — the rendered stream — drops file_path from tool_use.
	logContent := strings.Join([]string{
		"[claude] model: claude-opus-4",
		"Let me edit the file.",
		"[tool_use: Edit]",
		"--- FAIL: TestBar (0.00s)",
		"[tool_use: Write]",
	}, "\n")
	// slot.jsonl — the raw NDJSON side channel — keeps file_path.
	jsonlContent := strings.Join([]string{
		`{"type":"system","subtype":"init","model":"claude-opus-4"}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Edit","input":{"file_path":"internal/edited.go"}}]}}`,
		`{"type":"user","message":{"content":"tool result string, not an array"}}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Write","input":{"file_path":"internal/written.go"}}]}}`,
	}, "\n")

	logPath := writeTempLogAndJSONL(t, logContent, jsonlContent)
	out := ExtractPostmortem(logPath, PostmortemTailLines)

	for _, want := range []string{"Files the previous attempt touched", "internal/edited.go", "internal/written.go"} {
		if !strings.Contains(out, want) {
			t.Errorf("claude jsonl side-channel edit %q not captured:\n%s", want, out)
		}
	}
}

// The .jsonl side channel also accumulates across respawns; its per-attempt
// boundary is the backend's session-start frame (claude system/init here), so a
// prior attempt's edits must not surface for the just-failed attempt.
func TestExtractPostmortem_JSONLIsolatesCurrentAttempt(t *testing.T) {
	logContent := strings.Join([]string{
		"[maestro] worker worktree: /home/wt/slot-1",
		"[tool_use: Edit]",
	}, "\n")
	jsonlContent := strings.Join([]string{
		`{"type":"system","subtype":"init","model":"m"}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Edit","input":{"file_path":"internal/attempt1.go"}}]}}`,
		`{"type":"system","subtype":"init","model":"m"}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Edit","input":{"file_path":"internal/attempt2.go"}}]}}`,
	}, "\n")

	logPath := writeTempLogAndJSONL(t, logContent, jsonlContent)
	out := ExtractPostmortem(logPath, PostmortemTailLines)

	if strings.Contains(out, "internal/attempt1.go") {
		t.Errorf("prior attempt's jsonl edit should be isolated out:\n%s", out)
	}
	if !strings.Contains(out, "internal/attempt2.go") {
		t.Errorf("current attempt's jsonl edit missing:\n%s", out)
	}
}

// postmortemSection returns the body of the "### <heading>" section of a
// post-mortem, up to the next "### " heading or the end of the text.
func postmortemSection(out, heading string) string {
	marker := "### " + heading
	idx := strings.Index(out, marker)
	if idx < 0 {
		return ""
	}
	rest := out[idx+len(marker):]
	if next := strings.Index(rest, "\n### "); next >= 0 {
		return rest[:next]
	}
	return rest
}

func writeTempLog(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "worker.log")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write temp log: %v", err)
	}
	return path
}

// writeTempLogAndJSONL writes slot.log and its paired slot.jsonl side channel
// into the same directory so JSONLPathForLog resolves the pair.
func writeTempLogAndJSONL(t *testing.T, logContent, jsonlContent string) string {
	t.Helper()
	dir := t.TempDir()
	logPath := filepath.Join(dir, "slot.log")
	if err := os.WriteFile(logPath, []byte(logContent), 0o644); err != nil {
		t.Fatalf("write log: %v", err)
	}
	if err := os.WriteFile(JSONLPathForLog(logPath), []byte(jsonlContent), 0o644); err != nil {
		t.Fatalf("write jsonl: %v", err)
	}
	return logPath
}
