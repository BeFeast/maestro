package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestWriteDigestReportToDirectory(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 6, 12, 7, 0, 0, 0, time.UTC)

	path, err := writeDigestReport(dir, "# report\n", now)
	if err != nil {
		t.Fatalf("writeDigestReport: %v", err)
	}
	want := filepath.Join(dir, "maestro-digest-2026-06-12.md")
	if path != want {
		t.Errorf("path = %q, want %q", path, want)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read report: %v", err)
	}
	if string(data) != "# report\n" {
		t.Errorf("unexpected content: %q", data)
	}
}

func TestWriteDigestReportToExplicitFile(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 6, 12, 7, 0, 0, 0, time.UTC)
	target := filepath.Join(dir, "nested", "digest.md")

	path, err := writeDigestReport(target, "# report\n", now)
	if err != nil {
		t.Fatalf("writeDigestReport: %v", err)
	}
	if path != target {
		t.Errorf("path = %q, want %q", path, target)
	}
	if _, err := os.Stat(target); err != nil {
		t.Errorf("report file not written: %v", err)
	}
}
