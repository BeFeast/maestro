package worker

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// initBareRemoteAndWorktree creates a bare "origin" repo and a clone that
// acts as the worktree so that git push -u origin HEAD succeeds.
func initBareRemoteAndWorktree(t *testing.T) string {
	t.Helper()
	base := t.TempDir()
	bare := filepath.Join(base, "origin.git")
	work := filepath.Join(base, "work")

	for _, args := range [][]string{
		{"git", "init", "--bare", "-b", "main", bare},
		{"git", "clone", bare, work},
	} {
		if out, err := exec.Command(args[0], args[1:]...).CombinedOutput(); err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
	}
	// Create an initial commit on main so origin/main exists
	for _, args := range [][]string{
		{"git", "-C", work, "commit", "--allow-empty", "-m", "init"},
		{"git", "-C", work, "push", "-u", "origin", "main"},
		{"git", "-C", work, "checkout", "-b", "feat/test"},
		// Create a tracked change so diff --stat has output
		{"git", "-C", work, "commit", "--allow-empty", "-m", "work commit"},
	} {
		if out, err := exec.Command(args[0], args[1:]...).CombinedOutput(); err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
	}
	return work
}

func TestSaveAndLoadCheckpoint(t *testing.T) {
	dir := initBareRemoteAndWorktree(t)

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
