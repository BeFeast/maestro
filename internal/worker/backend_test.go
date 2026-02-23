package worker

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBuildWorkerCmd_Claude(t *testing.T) {
	// Create a temp prompt file
	dir := t.TempDir()
	promptFile := filepath.Join(dir, "prompt.md")
	if err := os.WriteFile(promptFile, []byte("do the thing"), 0644); err != nil {
		t.Fatal(err)
	}
	worktree := "/tmp/test-worktree"

	cfg := BackendConfig{Cmd: "claude", ExtraArgs: []string{"--model", "opus"}}
	cmd, err := BuildWorkerCmd("claude", cfg, promptFile, worktree)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cmd.Path == "" {
		t.Fatal("cmd.Path is empty")
	}
	args := strings.Join(cmd.Args, " ")
	if !strings.Contains(args, "--dangerously-skip-permissions") {
		t.Errorf("expected --dangerously-skip-permissions in args, got: %s", args)
	}
	if !strings.Contains(args, "do the thing") {
		t.Errorf("expected prompt content in args, got: %s", args)
	}
	if !strings.Contains(args, "--model") {
		t.Errorf("expected extra args in command, got: %s", args)
	}
	if cmd.Dir != worktree {
		t.Errorf("expected Dir=%s, got %s", worktree, cmd.Dir)
	}
}

func TestBuildWorkerCmd_ClaudeDefault(t *testing.T) {
	dir := t.TempDir()
	promptFile := filepath.Join(dir, "prompt.md")
	if err := os.WriteFile(promptFile, []byte("hello"), 0644); err != nil {
		t.Fatal(err)
	}

	// Empty backend name should default to claude
	cfg := BackendConfig{}
	cmd, err := BuildWorkerCmd("", cfg, promptFile, "/tmp/wt")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Should use "claude" as default cmd
	if !strings.HasSuffix(cmd.Path, "claude") && !strings.Contains(cmd.Args[0], "claude") {
		t.Errorf("expected claude command, got: %v", cmd.Args)
	}
}

func TestBuildWorkerCmd_Codex(t *testing.T) {
	dir := t.TempDir()
	promptFile := filepath.Join(dir, "prompt.md")
	if err := os.WriteFile(promptFile, []byte("implement feature X"), 0644); err != nil {
		t.Fatal(err)
	}
	worktree := "/tmp/codex-worktree"

	cfg := BackendConfig{Cmd: "/usr/local/bin/codex"}
	cmd, err := BuildWorkerCmd("codex", cfg, promptFile, worktree)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	args := strings.Join(cmd.Args, " ")
	if !strings.Contains(args, "exec") {
		t.Errorf("expected 'exec' subcommand in args, got: %s", args)
	}
	if !strings.Contains(args, "--dangerously-bypass-approvals-and-sandbox") {
		t.Errorf("expected --dangerously-bypass-approvals-and-sandbox in args, got: %s", args)
	}
	if !strings.Contains(args, "-C") {
		t.Errorf("expected -C flag in args, got: %s", args)
	}
	if !strings.Contains(args, worktree) {
		t.Errorf("expected worktree path in args, got: %s", args)
	}
	if cmd.Stdin == nil {
		t.Error("expected stdin to be set for codex backend")
	}
	if cmd.Dir != worktree {
		t.Errorf("expected Dir=%s, got %s", worktree, cmd.Dir)
	}
}

func TestBuildWorkerCmd_Unknown(t *testing.T) {
	_, err := BuildWorkerCmd("gemini", BackendConfig{}, "/tmp/prompt.md", "/tmp/wt")
	if err == nil {
		t.Fatal("expected error for unknown backend")
	}
	if !strings.Contains(err.Error(), "unknown backend") {
		t.Errorf("expected 'unknown backend' error, got: %v", err)
	}
}
