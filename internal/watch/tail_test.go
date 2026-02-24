package watch

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTailFiltered_FiltersDotsAndSpinners(t *testing.T) {
	dir := t.TempDir()
	logFile := filepath.Join(dir, "test.log")

	// Write a log file with mixed content: dots, spinners, and real output
	content := strings.Join([]string{
		".",
		"..",
		"...",
		"⠋",
		"⣾",
		"◐",
		"Running tool: Read file.go",
		"...",
		"\x1b[31mcolored output\x1b[0m",
		"Created PR #42",
		".",
		"",
		"   ",
		"Building project...",
	}, "\n") + "\n"

	if err := os.WriteFile(logFile, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	// Capture stdout by redirecting to a pipe
	oldStdout := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w

	// Use a nonexistent tmux session so TailFiltered reads the whole file and exits
	TailFiltered(logFile, "nonexistent-session-that-does-not-exist")

	w.Close()
	os.Stdout = oldStdout

	var buf [4096]byte
	n, _ := r.Read(buf[:])
	output := string(buf[:n])
	r.Close()

	// Should contain meaningful lines
	if !strings.Contains(output, "Running tool: Read file.go") {
		t.Errorf("output should contain tool call line, got:\n%s", output)
	}
	if !strings.Contains(output, "colored output") {
		t.Errorf("output should contain ANSI-stripped line, got:\n%s", output)
	}
	if !strings.Contains(output, "Created PR #42") {
		t.Errorf("output should contain PR creation line, got:\n%s", output)
	}
	if !strings.Contains(output, "Building project...") {
		t.Errorf("output should contain 'Building project...' (not just dots), got:\n%s", output)
	}

	// Should NOT contain spinner/dot-only lines
	lines := strings.Split(strings.TrimSpace(output), "\n")
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "." || trimmed == ".." || trimmed == "..." {
			t.Errorf("output should not contain dot-only line %q", trimmed)
		}
		if trimmed == "⠋" || trimmed == "⣾" || trimmed == "◐" {
			t.Errorf("output should not contain spinner character %q", trimmed)
		}
	}
}

func TestTailFiltered_NoLogFile(t *testing.T) {
	// Capture stdout
	oldStdout := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w

	// Nonexistent log file + nonexistent session → should print message and exit
	TailFiltered("/tmp/nonexistent-maestro-test-log-file", "nonexistent-session")

	w.Close()
	os.Stdout = oldStdout

	var buf [4096]byte
	n, _ := r.Read(buf[:])
	output := string(buf[:n])
	r.Close()

	if !strings.Contains(output, "No log file found") {
		t.Errorf("expected 'No log file found' message, got: %q", output)
	}
}
