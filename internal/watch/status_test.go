package watch

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/befeast/maestro/internal/state"
)

func TestFormatPaneTitle(t *testing.T) {
	sess := &state.Session{
		IssueNumber: 59,
		IssueTitle:  "feat: rich status display",
		Backend:     "claude",
		StartedAt:   time.Now().Add(-15*time.Minute - 23*time.Second),
		Status:      state.StatusRunning,
	}

	title := FormatPaneTitle("pan-1", sess)

	checks := []string{"pan-1", "#59", "feat: rich status display", "claude", "running"}
	for _, want := range checks {
		if !strings.Contains(title, want) {
			t.Errorf("title %q should contain %q", title, want)
		}
	}

	if !strings.Contains(title, "15m") {
		t.Errorf("title %q should contain elapsed time ~15m", title)
	}
}

func TestFormatPaneTitle_EmptyBackend(t *testing.T) {
	sess := &state.Session{
		IssueNumber: 10,
		IssueTitle:  "test",
		Backend:     "",
		StartedAt:   time.Now(),
		Status:      state.StatusRunning,
	}

	title := FormatPaneTitle("pan-1", sess)
	if !strings.Contains(title, "?") {
		t.Errorf("title %q should show '?' for empty backend", title)
	}
}

func TestFormatElapsed(t *testing.T) {
	tests := []struct {
		d    time.Duration
		want string
	}{
		{0, "0m00s"},
		{30 * time.Second, "0m30s"},
		{5*time.Minute + 10*time.Second, "5m10s"},
		{1*time.Hour + 2*time.Minute + 3*time.Second, "1h02m03s"},
		{10*time.Hour + 0*time.Minute + 5*time.Second, "10h00m05s"},
	}
	for _, tt := range tests {
		got := FormatElapsed(tt.d)
		if got != tt.want {
			t.Errorf("FormatElapsed(%v) = %q, want %q", tt.d, got, tt.want)
		}
	}
}

func TestTruncateTitle(t *testing.T) {
	tests := []struct {
		s      string
		maxLen int
		want   string
	}{
		{"short", 10, "short"},
		{"exactly ten", 11, "exactly ten"},
		{"this is a long title", 10, "this is a…"},
	}
	for _, tt := range tests {
		got := TruncateTitle(tt.s, tt.maxLen)
		if got != tt.want {
			t.Errorf("TruncateTitle(%q, %d) = %q, want %q", tt.s, tt.maxLen, got, tt.want)
		}
	}
}

func TestStripANSI(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"hello", "hello"},
		{"\x1b[31mred\x1b[0m", "red"},
		{"\x1b[1;32mbold green\x1b[0m", "bold green"},
		{"\x1b]0;title\x07content", "content"},
		{"no\x1b(Bansi", "noansi"},
	}
	for _, tt := range tests {
		got := StripANSI(tt.input)
		if got != tt.want {
			t.Errorf("StripANSI(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestCleanOutputLine(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"", ""},
		{"   ", ""},
		{"hello world", "hello world"},
		{"\x1b[31mcolored text\x1b[0m", "colored text"},
		{"⠋", ""},
		{"⣾", ""},
		{"...", ""},
		{".", ""},
		{"Building project...", "Building project..."},
	}
	for _, tt := range tests {
		got := CleanOutputLine(tt.input)
		if got != tt.want {
			t.Errorf("CleanOutputLine(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestPaneMappingRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "panes.json")

	mappings := []PaneMapping{
		{PaneIndex: 0, SlotName: "pan-1", StateDir: "/tmp/state1"},
		{PaneIndex: 1, SlotName: "pan-2", StateDir: "/tmp/state2"},
	}

	if err := WritePaneMap(path, mappings); err != nil {
		t.Fatalf("write: %v", err)
	}

	loaded, err := ReadPaneMap(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	if len(loaded) != 2 {
		t.Fatalf("expected 2 mappings, got %d", len(loaded))
	}
	if loaded[0].SlotName != "pan-1" || loaded[0].PaneIndex != 0 {
		t.Errorf("first mapping: got %+v", loaded[0])
	}
	if loaded[1].SlotName != "pan-2" || loaded[1].PaneIndex != 1 {
		t.Errorf("second mapping: got %+v", loaded[1])
	}
}
