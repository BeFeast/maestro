package watch

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/befeast/maestro/internal/state"
)

var ansiRegex = regexp.MustCompile(`\x1b\[[0-9;]*[a-zA-Z]|\x1b\][^\x07]*\x07|\x1b\(B`)

// FormatPaneTitle creates a rich status line for a watch pane border.
// Format: " slot │ #num title │ backend │ elapsed │ status "
func FormatPaneTitle(slotName string, sess *state.Session) string {
	elapsed := FormatElapsed(time.Since(sess.StartedAt))
	title := TruncateTitle(sess.IssueTitle, 40)
	backend := sess.Backend
	if backend == "" {
		backend = "?"
	}
	return fmt.Sprintf(" %s │ #%d %s │ %s │ %s │ %s ",
		slotName, sess.IssueNumber, title, backend, elapsed, sess.Status)
}

// FormatElapsed formats a duration as a compact human-readable string.
func FormatElapsed(d time.Duration) string {
	d = d.Truncate(time.Second)
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	s := int(d.Seconds()) % 60
	if h > 0 {
		return fmt.Sprintf("%dh%02dm%02ds", h, m, s)
	}
	return fmt.Sprintf("%dm%02ds", m, s)
}

// TruncateTitle truncates a string to maxLen runes, appending "…" if truncated.
func TruncateTitle(s string, maxLen int) string {
	runes := []rune(s)
	if len(runes) <= maxLen {
		return s
	}
	return string(runes[:maxLen-1]) + "…"
}

// StripANSI removes ANSI escape sequences from a string.
func StripANSI(s string) string {
	return ansiRegex.ReplaceAllString(s, "")
}

// CleanOutputLine strips ANSI codes, trims whitespace, and filters spinner frames.
// Returns empty string for lines that are just spinners or whitespace.
func CleanOutputLine(s string) string {
	s = StripANSI(s)
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}

	// Filter common spinner characters and progress dots
	spinnerPatterns := []string{
		"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏",
		"⣾", "⣽", "⣻", "⢿", "⡿", "⣟", "⣯", "⣷",
		"◐", "◓", "◑", "◒",
		".", "..", "...",
	}
	for _, sp := range spinnerPatterns {
		if s == sp {
			return ""
		}
	}

	return s
}
