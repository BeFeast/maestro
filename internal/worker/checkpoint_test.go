package worker

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestSaveCheckpoint_WritesCheckpointFile(t *testing.T) {
	// Create a temporary directory simulating a git worktree
	dir := t.TempDir()

	// Initialize a git repo so git commands work
	if err := runCmd(dir, "git", "init"); err != nil {
		t.Fatalf("git init: %v", err)
	}
	if err := runCmd(dir, "git", "commit", "--allow-empty", "-m", "init"); err != nil {
		t.Fatalf("git commit: %v", err)
	}

	// Create a file to simulate work
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n"), 0644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	content, err := SaveCheckpoint(dir, 75000)
	if err != nil {
		t.Fatalf("SaveCheckpoint: %v", err)
	}

	// Verify CHECKPOINT.md was created
	cpPath := filepath.Join(dir, checkpointFile)
	data, err := os.ReadFile(cpPath)
	if err != nil {
		t.Fatalf("read checkpoint: %v", err)
	}
	if string(data) != content {
		t.Errorf("file content doesn't match returned content")
	}

	// Verify checkpoint mentions tokens
	if !strings.Contains(content, "75000") {
		t.Errorf("checkpoint should mention token count, got: %s", content)
	}

	// Verify the file was committed
	if err := runCmd(dir, "git", "diff", "--cached", "--quiet"); err != nil {
		t.Errorf("expected clean staging area after checkpoint commit")
	}
}

func TestReadCheckpoint_ExistingFile(t *testing.T) {
	dir := t.TempDir()
	cpContent := "# Checkpoint\nTest checkpoint content\n"
	if err := os.WriteFile(filepath.Join(dir, checkpointFile), []byte(cpContent), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
	got := ReadCheckpoint(dir)
	if got != cpContent {
		t.Errorf("ReadCheckpoint = %q, want %q", got, cpContent)
	}
}

func TestReadCheckpoint_NoFile(t *testing.T) {
	dir := t.TempDir()
	got := ReadCheckpoint(dir)
	if got != "" {
		t.Errorf("ReadCheckpoint = %q, want empty", got)
	}
}

func runCmd(dir string, name string, args ...string) error {
	cmd := newCmd(dir, name, args...)
	return cmd.Run()
}

func newCmd(dir string, name string, args ...string) *exec.Cmd {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=test",
		"GIT_AUTHOR_EMAIL=test@test.com",
		"GIT_COMMITTER_NAME=test",
		"GIT_COMMITTER_EMAIL=test@test.com",
	)
	return cmd
}
