package worker

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWritePromptFileAtomicallyEnforcesOwnerOnlyMode(t *testing.T) {
	dir := t.TempDir()
	prompt := filepath.Join(dir, "slot-a-prompt.md")
	if err := os.WriteFile(prompt, []byte("old"), 0o644); err != nil {
		t.Fatalf("write old prompt: %v", err)
	}
	if err := os.Chmod(prompt, 0o644); err != nil {
		t.Fatalf("chmod old prompt: %v", err)
	}

	if err := writePromptFile(prompt, "new prompt"); err != nil {
		t.Fatalf("write prompt: %v", err)
	}
	data, err := os.ReadFile(prompt)
	if err != nil {
		t.Fatalf("read prompt: %v", err)
	}
	if string(data) != "new prompt" {
		t.Fatalf("prompt = %q, want replacement content", data)
	}
	if info, err := os.Stat(prompt); err != nil {
		t.Fatalf("stat prompt: %v", err)
	} else if info.Mode().Perm() != workerPromptFileMode {
		t.Fatalf("prompt mode = %o, want %o", info.Mode().Perm(), workerPromptFileMode)
	}
}

func TestWritePromptFileReplacesSymlinkWithoutTouchingTarget(t *testing.T) {
	dir := t.TempDir()
	victim := filepath.Join(t.TempDir(), "victim")
	if err := os.WriteFile(victim, []byte("untouched"), 0o600); err != nil {
		t.Fatalf("write victim: %v", err)
	}
	prompt := filepath.Join(dir, "slot-a-prompt.md")
	if err := os.Symlink(victim, prompt); err != nil {
		t.Fatalf("symlink prompt: %v", err)
	}

	if err := writePromptFile(prompt, "private prompt"); err != nil {
		t.Fatalf("write prompt: %v", err)
	}
	if data, err := os.ReadFile(victim); err != nil || string(data) != "untouched" {
		t.Fatalf("victim changed through prompt symlink: data=%q err=%v", data, err)
	}
	if info, err := os.Lstat(prompt); err != nil {
		t.Fatalf("lstat prompt: %v", err)
	} else if !info.Mode().IsRegular() || info.Mode().Perm() != workerPromptFileMode {
		t.Fatalf("prompt was not replaced by an owner-only regular file: %v", info.Mode())
	}
}

func TestEnsureWorkerLogDirRepairsModeAndRejectsSymlink(t *testing.T) {
	logDir := filepath.Join(t.TempDir(), "logs")
	if err := os.Mkdir(logDir, 0o755); err != nil {
		t.Fatalf("mkdir logs: %v", err)
	}
	if err := os.Chmod(logDir, 0o755); err != nil {
		t.Fatalf("chmod logs: %v", err)
	}
	if err := ensureWorkerLogDir(logDir); err != nil {
		t.Fatalf("ensure logs: %v", err)
	}
	if info, err := os.Stat(logDir); err != nil {
		t.Fatalf("stat logs: %v", err)
	} else if info.Mode().Perm() != workerLogDirMode {
		t.Fatalf("log dir mode = %o, want %o", info.Mode().Perm(), workerLogDirMode)
	}

	target := t.TempDir()
	linked := filepath.Join(t.TempDir(), "logs")
	if err := os.Symlink(target, linked); err != nil {
		t.Fatalf("symlink logs: %v", err)
	}
	if err := ensureWorkerLogDir(linked); err == nil {
		t.Fatal("symlinked worker log directory was accepted")
	}
}
