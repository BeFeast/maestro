package worker

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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
	log := strings.Join([]string{
		"error: request failed",
		"Authorization: Bearer sk-abcdefghijklmnopqrstuvwxyz012345",
		"exporting GITHUB_TOKEN=ghp_abcdefghijklmnopqrstuvwxyz0123456789",
		"API_KEY=supersecretvalue123",
		"last line",
	}, "\n")

	out := ExtractPostmortem(writeTempLog(t, log), PostmortemTailLines)

	for _, secret := range []string{
		"sk-abcdefghijklmnopqrstuvwxyz012345",
		"ghp_abcdefghijklmnopqrstuvwxyz0123456789",
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
	// Content is capped to capBytes; a short marker may be appended.
	if len(capped) > PostmortemPromptCapBytes+160 {
		t.Errorf("capped output too large: %d bytes", len(capped))
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

func writeTempLog(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "worker.log")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write temp log: %v", err)
	}
	return path
}
