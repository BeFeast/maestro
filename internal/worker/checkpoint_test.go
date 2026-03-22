package worker

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSaveAndLoadCheckpoint(t *testing.T) {
	dir := t.TempDir()

	err := SaveCheckpoint(dir, 42, 85000)
	if err != nil {
		t.Fatalf("SaveCheckpoint: %v", err)
	}

	// Verify file exists
	path := filepath.Join(dir, checkpointFile)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read checkpoint: %v", err)
	}

	content := string(data)
	if !strings.Contains(content, "Issue #42") {
		t.Error("checkpoint should contain issue number")
	}
	if !strings.Contains(content, "85000") {
		t.Error("checkpoint should contain token count")
	}
	if !strings.Contains(content, "Instructions for continuation") {
		t.Error("checkpoint should contain continuation instructions")
	}

	// Test LoadCheckpoint
	loaded := LoadCheckpoint(dir)
	if loaded != content {
		t.Error("LoadCheckpoint should return same content as written")
	}
}

func TestLoadCheckpoint_NoFile(t *testing.T) {
	dir := t.TempDir()

	loaded := LoadCheckpoint(dir)
	if loaded != "" {
		t.Errorf("LoadCheckpoint should return empty string for missing file, got %q", loaded)
	}
}

func TestSaveCheckpoint_EmptyWorktree(t *testing.T) {
	err := SaveCheckpoint("", 42, 1000)
	if err == nil {
		t.Error("SaveCheckpoint should fail with empty worktree path")
	}
}
