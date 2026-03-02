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
	cmd, stdinFile, err := BuildWorkerCmd("claude", cfg, promptFile, worktree)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cmd.Path == "" {
		t.Fatal("cmd.Path is empty")
	}
	if stdinFile != "" {
		t.Errorf("expected empty stdinFile for claude, got: %s", stdinFile)
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
	cmd, stdinFile, err := BuildWorkerCmd("", cfg, promptFile, "/tmp/wt")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Should use "claude" as default cmd
	if !strings.HasSuffix(cmd.Path, "claude") && !strings.Contains(cmd.Args[0], "claude") {
		t.Errorf("expected claude command, got: %v", cmd.Args)
	}
	if stdinFile != "" {
		t.Errorf("expected empty stdinFile for default claude, got: %s", stdinFile)
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
	cmd, stdinFile, err := BuildWorkerCmd("codex", cfg, promptFile, worktree)
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
	if stdinFile != promptFile {
		t.Errorf("expected stdinFile=%s, got %s", promptFile, stdinFile)
	}
	if cmd.Stdin != nil {
		t.Error("expected cmd.Stdin to be nil (stdin handled by runner script)")
	}
	if cmd.Dir != worktree {
		t.Errorf("expected Dir=%s, got %s", worktree, cmd.Dir)
	}
}

func TestBuildWorkerCmd_Gemini(t *testing.T) {
	dir := t.TempDir()
	promptFile := filepath.Join(dir, "prompt.md")
	if err := os.WriteFile(promptFile, []byte("analyze this codebase"), 0644); err != nil {
		t.Fatal(err)
	}
	worktree := "/tmp/gemini-worktree"

	cfg := BackendConfig{Cmd: "gemini-cli", ExtraArgs: []string{"--yolo"}}
	cmd, stdinFile, err := BuildWorkerCmd("gemini", cfg, promptFile, worktree)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if stdinFile != "" {
		t.Errorf("expected empty stdinFile for gemini, got: %s", stdinFile)
	}
	args := strings.Join(cmd.Args, " ")
	if !strings.Contains(args, "-p") {
		t.Errorf("expected -p flag in args, got: %s", args)
	}
	if !strings.Contains(args, "analyze this codebase") {
		t.Errorf("expected prompt content in args, got: %s", args)
	}
	if !strings.Contains(args, "--yolo") {
		t.Errorf("expected extra args in command, got: %s", args)
	}
	if cmd.Dir != worktree {
		t.Errorf("expected Dir=%s, got %s", worktree, cmd.Dir)
	}
}

func TestBuildWorkerCmd_GeminiDefaultCmd(t *testing.T) {
	dir := t.TempDir()
	promptFile := filepath.Join(dir, "prompt.md")
	if err := os.WriteFile(promptFile, []byte("hello"), 0644); err != nil {
		t.Fatal(err)
	}

	cfg := BackendConfig{}
	cmd, _, err := BuildWorkerCmd("gemini", cfg, promptFile, "/tmp/wt")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Should use "gemini" as default cmd when none specified
	if !strings.HasSuffix(cmd.Path, "gemini") && !strings.Contains(cmd.Args[0], "gemini") {
		t.Errorf("expected gemini command, got: %v", cmd.Args)
	}
}

func TestBuildWorkerCmd_Cline(t *testing.T) {
	dir := t.TempDir()
	promptFile := filepath.Join(dir, "prompt.md")
	if err := os.WriteFile(promptFile, []byte("fix the login flow"), 0644); err != nil {
		t.Fatal(err)
	}
	worktree := "/tmp/cline-worktree"

	cfg := BackendConfig{Cmd: "cline", ExtraArgs: []string{"--verbose"}}
	cmd, stdinFile, err := BuildWorkerCmd("cline", cfg, promptFile, worktree)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if stdinFile != "" {
		t.Errorf("expected empty stdinFile for cline, got: %s", stdinFile)
	}
	args := strings.Join(cmd.Args, " ")
	if !strings.Contains(args, "-y") {
		t.Errorf("expected -y flag in args, got: %s", args)
	}
	if !strings.Contains(args, "fix the login flow") {
		t.Errorf("expected prompt content in args, got: %s", args)
	}
	if !strings.Contains(args, "--verbose") {
		t.Errorf("expected extra args in command, got: %s", args)
	}
	if cmd.Dir != worktree {
		t.Errorf("expected Dir=%s, got %s", worktree, cmd.Dir)
	}
}

func TestBuildWorkerCmd_ClineDefaultCmd(t *testing.T) {
	dir := t.TempDir()
	promptFile := filepath.Join(dir, "prompt.md")
	if err := os.WriteFile(promptFile, []byte("hello"), 0644); err != nil {
		t.Fatal(err)
	}

	cfg := BackendConfig{}
	cmd, _, err := BuildWorkerCmd("cline", cfg, promptFile, "/tmp/wt")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.HasSuffix(cmd.Path, "cline") && !strings.Contains(cmd.Args[0], "cline") {
		t.Errorf("expected cline command, got: %v", cmd.Args)
	}
}

func TestBuildWorkerCmd_GenericArgMode(t *testing.T) {
	dir := t.TempDir()
	promptFile := filepath.Join(dir, "prompt.md")
	if err := os.WriteFile(promptFile, []byte("do custom task"), 0644); err != nil {
		t.Fatal(err)
	}
	worktree := "/tmp/custom-worktree"

	cfg := BackendConfig{
		Cmd:        "my-custom-cli",
		ExtraArgs:  []string{"--flag1", "val1"},
		PromptMode: "arg",
	}
	cmd, stdinFile, err := BuildWorkerCmd("my-custom-backend", cfg, promptFile, worktree)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if stdinFile != "" {
		t.Errorf("expected empty stdinFile for arg mode, got: %s", stdinFile)
	}
	args := strings.Join(cmd.Args, " ")
	if !strings.Contains(args, "--flag1") {
		t.Errorf("expected extra args in command, got: %s", args)
	}
	if !strings.Contains(args, "do custom task") {
		t.Errorf("expected prompt content as last arg, got: %s", args)
	}
	if cmd.Dir != worktree {
		t.Errorf("expected Dir=%s, got %s", worktree, cmd.Dir)
	}
}

func TestBuildWorkerCmd_GenericStdinMode(t *testing.T) {
	dir := t.TempDir()
	promptFile := filepath.Join(dir, "prompt.md")
	if err := os.WriteFile(promptFile, []byte("stdin prompt"), 0644); err != nil {
		t.Fatal(err)
	}
	worktree := "/tmp/stdin-worktree"

	cfg := BackendConfig{
		Cmd:        "stdin-cli",
		ExtraArgs:  []string{"--auto"},
		PromptMode: "stdin",
	}
	cmd, stdinFile, err := BuildWorkerCmd("stdin-backend", cfg, promptFile, worktree)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if stdinFile != promptFile {
		t.Errorf("expected stdinFile=%s, got %s", promptFile, stdinFile)
	}
	args := strings.Join(cmd.Args, " ")
	if !strings.Contains(args, "--auto") {
		t.Errorf("expected extra args in command, got: %s", args)
	}
	// Prompt content should NOT be in args for stdin mode
	if strings.Contains(args, "stdin prompt") {
		t.Errorf("prompt content should not be in args for stdin mode, got: %s", args)
	}
	if cmd.Dir != worktree {
		t.Errorf("expected Dir=%s, got %s", worktree, cmd.Dir)
	}
}

func TestBuildWorkerCmd_GenericFileMode(t *testing.T) {
	dir := t.TempDir()
	promptFile := filepath.Join(dir, "prompt.md")
	if err := os.WriteFile(promptFile, []byte("file prompt"), 0644); err != nil {
		t.Fatal(err)
	}
	worktree := "/tmp/file-worktree"

	cfg := BackendConfig{
		Cmd:        "file-cli",
		PromptMode: "file",
	}
	cmd, stdinFile, err := BuildWorkerCmd("file-backend", cfg, promptFile, worktree)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if stdinFile != "" {
		t.Errorf("expected empty stdinFile for file mode, got: %s", stdinFile)
	}
	args := strings.Join(cmd.Args, " ")
	if !strings.Contains(args, promptFile) {
		t.Errorf("expected prompt file path in args, got: %s", args)
	}
	// Prompt content should NOT be in args
	if strings.Contains(args, "file prompt") {
		t.Errorf("prompt content should not be in args for file mode, got: %s", args)
	}
	if cmd.Dir != worktree {
		t.Errorf("expected Dir=%s, got %s", worktree, cmd.Dir)
	}
}

func TestBuildWorkerCmd_GenericDefaultArgMode(t *testing.T) {
	dir := t.TempDir()
	promptFile := filepath.Join(dir, "prompt.md")
	if err := os.WriteFile(promptFile, []byte("default mode"), 0644); err != nil {
		t.Fatal(err)
	}

	// No PromptMode set — should default to "arg"
	cfg := BackendConfig{Cmd: "some-cli"}
	cmd, stdinFile, err := BuildWorkerCmd("unknown-backend", cfg, promptFile, "/tmp/wt")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if stdinFile != "" {
		t.Errorf("expected empty stdinFile for default arg mode, got: %s", stdinFile)
	}
	args := strings.Join(cmd.Args, " ")
	if !strings.Contains(args, "default mode") {
		t.Errorf("expected prompt content in args, got: %s", args)
	}
}

func TestBuildWorkerCmd_GenericNoCmdError(t *testing.T) {
	cfg := BackendConfig{} // no Cmd set
	_, _, err := BuildWorkerCmd("no-cmd-backend", cfg, "/tmp/prompt.md", "/tmp/wt")
	if err == nil {
		t.Fatal("expected error for generic backend with no cmd")
	}
	if !strings.Contains(err.Error(), "requires cmd") {
		t.Errorf("expected 'requires cmd' error, got: %v", err)
	}
}

func TestBuildWorkerCmd_GenericInvalidPromptMode(t *testing.T) {
	dir := t.TempDir()
	promptFile := filepath.Join(dir, "prompt.md")
	if err := os.WriteFile(promptFile, []byte("test"), 0644); err != nil {
		t.Fatal(err)
	}

	cfg := BackendConfig{Cmd: "some-cli", PromptMode: "invalid"}
	_, _, err := BuildWorkerCmd("bad-mode-backend", cfg, promptFile, "/tmp/wt")
	if err == nil {
		t.Fatal("expected error for invalid prompt_mode")
	}
	if !strings.Contains(err.Error(), "unknown prompt_mode") {
		t.Errorf("expected 'unknown prompt_mode' error, got: %v", err)
	}
}

func TestSplitCmd(t *testing.T) {
	tests := []struct {
		input      string
		wantBinary string
		wantArgs   []string
	}{
		{"claude", "claude", nil},
		{"claude --model claude-opus-4-6", "claude", []string{"--model", "claude-opus-4-6"}},
		{"/usr/local/bin/codex --flag", "/usr/local/bin/codex", []string{"--flag"}},
		{"  gemini  --fast  ", "gemini", []string{"--fast"}},
		{"", "", nil},
	}
	for _, tt := range tests {
		binary, args := splitCmd(tt.input)
		if binary != tt.wantBinary {
			t.Errorf("splitCmd(%q) binary = %q, want %q", tt.input, binary, tt.wantBinary)
		}
		if len(args) != len(tt.wantArgs) {
			t.Errorf("splitCmd(%q) args = %v, want %v", tt.input, args, tt.wantArgs)
			continue
		}
		for i := range args {
			if args[i] != tt.wantArgs[i] {
				t.Errorf("splitCmd(%q) args[%d] = %q, want %q", tt.input, i, args[i], tt.wantArgs[i])
			}
		}
	}
}

func TestBuildWorkerCmd_CmdWithArgs(t *testing.T) {
	dir := t.TempDir()
	promptFile := filepath.Join(dir, "prompt.md")
	if err := os.WriteFile(promptFile, []byte("do work"), 0644); err != nil {
		t.Fatal(err)
	}

	// Claude backend: cmd contains arguments
	cfg := BackendConfig{Cmd: "claude --model claude-opus-4-6"}
	cmd, _, err := BuildWorkerCmd("claude", cfg, promptFile, "/tmp/wt")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Binary should be just "claude", not the full string
	if cmd.Args[0] != "claude" {
		t.Errorf("expected Args[0]=%q, got %q", "claude", cmd.Args[0])
	}
	args := strings.Join(cmd.Args, " ")
	if !strings.Contains(args, "--model") {
		t.Errorf("expected --model in args, got: %s", args)
	}
	if !strings.Contains(args, "claude-opus-4-6") {
		t.Errorf("expected claude-opus-4-6 in args, got: %s", args)
	}
	if !strings.Contains(args, "--dangerously-skip-permissions") {
		t.Errorf("expected --dangerously-skip-permissions in args, got: %s", args)
	}

	// Codex backend: cmd contains arguments
	cfg = BackendConfig{Cmd: "codex --some-flag"}
	cmd, _, err = BuildWorkerCmd("codex", cfg, promptFile, "/tmp/wt")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cmd.Args[0] != "codex" {
		t.Errorf("expected Args[0]=%q, got %q", "codex", cmd.Args[0])
	}
	args = strings.Join(cmd.Args, " ")
	if !strings.Contains(args, "--some-flag") {
		t.Errorf("expected --some-flag in args, got: %s", args)
	}

	// Generic backend: cmd contains arguments
	cfg = BackendConfig{Cmd: "my-cli --verbose --debug", PromptMode: "arg"}
	cmd, _, err = BuildWorkerCmd("custom", cfg, promptFile, "/tmp/wt")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cmd.Args[0] != "my-cli" {
		t.Errorf("expected Args[0]=%q, got %q", "my-cli", cmd.Args[0])
	}
	args = strings.Join(cmd.Args, " ")
	if !strings.Contains(args, "--verbose") {
		t.Errorf("expected --verbose in args, got: %s", args)
	}
	if !strings.Contains(args, "--debug") {
		t.Errorf("expected --debug in args, got: %s", args)
	}
}

func TestKnownBackends(t *testing.T) {
	backends := KnownBackends()
	expected := map[string]bool{"claude": false, "codex": false, "gemini": false, "cline": false}
	for _, name := range backends {
		if _, ok := expected[name]; ok {
			expected[name] = true
		}
	}
	for name, found := range expected {
		if !found {
			t.Errorf("expected %q in KnownBackends(), got: %v", name, backends)
		}
	}
}
