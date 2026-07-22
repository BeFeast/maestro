package worker

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/befeast/maestro/internal/config"
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
	// Prompt is delivered via stdin (not argv) to stay under the Linux
	// single-argument size limit; the prompt file is returned as stdinFile.
	if stdinFile != promptFile {
		t.Errorf("stdinFile = %q, want %q", stdinFile, promptFile)
	}
	args := strings.Join(cmd.Args, " ")
	if !strings.Contains(args, "--dangerously-skip-permissions") {
		t.Errorf("expected --dangerously-skip-permissions in args, got: %s", args)
	}
	if strings.Contains(args, "do the thing") {
		t.Errorf("prompt content must not appear in argv (stdin delivery): %s", args)
	}
	if !strings.Contains(args, "--model") {
		t.Errorf("expected extra args in command, got: %s", args)
	}
	if cmd.Dir != worktree {
		t.Errorf("expected Dir=%s, got %s", worktree, cmd.Dir)
	}
}

// TestBuildWorkerCmd_ClaudeUsageStream verifies the #737 opt-in: when
// UsageStream is set the claude worker runs in stream-json mode, and an
// operator-pinned --output-format in extra_args overrides it (no duplicate).
func TestBuildWorkerCmd_ClaudeUsageStream(t *testing.T) {
	dir := t.TempDir()
	promptFile := filepath.Join(dir, "prompt.md")
	if err := os.WriteFile(promptFile, []byte("do the thing"), 0644); err != nil {
		t.Fatal(err)
	}

	t.Run("opt-in appends stream-json flags", func(t *testing.T) {
		cfg := BackendConfig{Cmd: "claude", UsageStream: true}
		cmd, _, err := BuildWorkerCmd("claude", cfg, promptFile, "/tmp/wt")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		args := strings.Join(cmd.Args, " ")
		for _, want := range []string{"--output-format stream-json", "--verbose"} {
			if !strings.Contains(args, want) {
				t.Errorf("expected %q in args, got: %s", want, args)
			}
		}
	})

	t.Run("disabled by default", func(t *testing.T) {
		cfg := BackendConfig{Cmd: "claude"}
		cmd, _, err := BuildWorkerCmd("claude", cfg, promptFile, "/tmp/wt")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if strings.Contains(strings.Join(cmd.Args, " "), "stream-json") {
			t.Errorf("stream-json must not appear unless UsageStream is set: %v", cmd.Args)
		}
	})

	t.Run("extra_args output-format overrides", func(t *testing.T) {
		cfg := BackendConfig{Cmd: "claude", UsageStream: true, ExtraArgs: []string{"--output-format", "json"}}
		cmd, _, err := BuildWorkerCmd("claude", cfg, promptFile, "/tmp/wt")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if strings.Contains(strings.Join(cmd.Args, " "), "stream-json") {
			t.Errorf("operator --output-format must win over the stream-json default: %v", cmd.Args)
		}
	})
}

// TestBuildWorkerCmd_ClaudeLargePromptViaStdin guards against the latent E2BIG
// regression from #454: a worker claude prompt larger than the Linux
// MAX_ARG_STRLEN single-argument limit (128 KiB) must be delivered via stdin,
// never as a CLI argument, so fork/exec never sees "argument list too long".
// TestBuildWorkerCmd_CodexUsageStream verifies the #738 opt-in: when
// UsageStream is set the codex worker runs `exec --json`, and an
// operator-pinned --json in extra_args overrides it (no duplicate flag).
func TestBuildWorkerCmd_CodexUsageStream(t *testing.T) {
	dir := t.TempDir()
	promptFile := filepath.Join(dir, "prompt.md")
	if err := os.WriteFile(promptFile, []byte("do the thing"), 0644); err != nil {
		t.Fatal(err)
	}

	t.Run("opt-in appends --json", func(t *testing.T) {
		cfg := BackendConfig{Cmd: "codex", UsageStream: true}
		cmd, _, err := BuildWorkerCmd("codex", cfg, promptFile, "/tmp/wt")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		args := strings.Join(cmd.Args, " ")
		if !strings.Contains(args, "--json") {
			t.Errorf("expected --json in args, got: %s", args)
		}
		// The stdin prompt marker must remain the final argument.
		if cmd.Args[len(cmd.Args)-1] != "-" {
			t.Errorf("expected trailing '-' stdin marker, got: %v", cmd.Args)
		}
	})

	t.Run("disabled by default", func(t *testing.T) {
		cfg := BackendConfig{Cmd: "codex"}
		cmd, _, err := BuildWorkerCmd("codex", cfg, promptFile, "/tmp/wt")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if strings.Contains(strings.Join(cmd.Args, " "), "--json") {
			t.Errorf("--json must not appear unless UsageStream is set: %v", cmd.Args)
		}
	})

	t.Run("extra_args --json is not duplicated", func(t *testing.T) {
		cfg := BackendConfig{Cmd: "codex", UsageStream: true, ExtraArgs: []string{"--json"}}
		cmd, _, err := BuildWorkerCmd("codex", cfg, promptFile, "/tmp/wt")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		n := 0
		for _, a := range cmd.Args {
			if a == "--json" {
				n++
			}
		}
		if n != 1 {
			t.Errorf("operator --json must not be duplicated, found %d in: %v", n, cmd.Args)
		}
	})
}

func TestBuildWorkerCmd_Kimi(t *testing.T) {
	dir := t.TempDir()
	promptFile := filepath.Join(dir, "prompt.md")
	if err := os.WriteFile(promptFile, []byte("implement the issue"), 0644); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name        string
		backendName string
		cfg         BackendConfig
	}{
		{
			name:        "provider resolves custom backend",
			backendName: "kimi-k2",
			cfg:         BackendConfig{Cmd: "kimi", Provider: "moonshot"},
		},
		{
			name:        "command basename resolves custom backend",
			backendName: "fast",
			cfg:         BackendConfig{Cmd: "/usr/local/bin/kimi --verbose"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd, stdinFile, err := BuildWorkerCmd(tt.backendName, tt.cfg, promptFile, "/tmp/kimi-worktree")
			if err != nil {
				t.Fatalf("BuildWorkerCmd: %v", err)
			}
			if stdinFile != promptFile {
				t.Fatalf("stdinFile = %q, want %q", stdinFile, promptFile)
			}
			if cmd.Dir != "/tmp/kimi-worktree" {
				t.Fatalf("Dir = %q, want /tmp/kimi-worktree", cmd.Dir)
			}
			args := cmd.Args[1:]
			if !containsArg(args, "--print") {
				t.Errorf("args = %v, want --print", args)
			}
			if !containsArg(args, "--output-format=stream-json") {
				t.Errorf("args = %v, want --output-format=stream-json", args)
			}
			if strings.Contains(strings.Join(args, " "), "implement the issue") {
				t.Errorf("prompt content must not appear in argv: %v", args)
			}
		})
	}

	t.Run("operator output format is not duplicated", func(t *testing.T) {
		cfg := BackendConfig{
			Cmd:       "kimi --print",
			Provider:  "moonshot",
			ExtraArgs: []string{"--output-format", "stream-json"},
		}
		cmd, _, err := BuildWorkerCmd("kimi-k2", cfg, promptFile, "/tmp/kimi-worktree")
		if err != nil {
			t.Fatal(err)
		}
		if countFlag(cmd.Args, "--print") != 1 {
			t.Fatalf("--print duplicated in args: %v", cmd.Args)
		}
		if countFlag(cmd.Args, "--output-format") != 1 {
			t.Fatalf("--output-format duplicated in args: %v", cmd.Args)
		}
	})
}

func TestStreamSplitForBackend_KimiAlwaysEnabled(t *testing.T) {
	split := streamSplitForBackend("kimi-k2", BackendConfig{Cmd: "kimi", Provider: "moonshot"}, "/tmp/kimi.log")
	if split == nil {
		t.Fatal("Kimi must use stream-split without a usage_stream opt-in")
	}
	if split.Backend != config.BackendKindKimi || split.JSONLPath != "/tmp/kimi.jsonl" {
		t.Fatalf("split = %+v, want kimi backend and sibling JSONL path", split)
	}
}

func TestBuildWorkerCmd_ClaudeLargePromptViaStdin(t *testing.T) {
	dir := t.TempDir()
	promptFile := filepath.Join(dir, "prompt.md")
	// 1 MiB + a little, far beyond the 128 KiB single-argument ceiling.
	largePrompt := strings.Repeat("WORKER_PROMPT_PAYLOAD\n", (1<<20)/21+16)
	if len(largePrompt) <= 1<<20 {
		t.Fatalf("prompt should exceed 1 MiB, got %d bytes", len(largePrompt))
	}
	if err := os.WriteFile(promptFile, []byte(largePrompt), 0644); err != nil {
		t.Fatal(err)
	}
	worktree := "/tmp/large-prompt-worktree"

	cfg := BackendConfig{Cmd: "claude", ExtraArgs: []string{"--model", "opus"}}
	cmd, stdinFile, err := BuildWorkerCmd("claude", cfg, promptFile, worktree)
	if err != nil {
		t.Fatalf("unexpected error building worker claude cmd with large prompt: %v", err)
	}
	// (a) prompt content must not appear anywhere in argv.
	for i, a := range cmd.Args {
		if strings.Contains(a, "WORKER_PROMPT_PAYLOAD") {
			t.Fatalf("prompt content leaked into argv at index %d (E2BIG risk)", i)
		}
	}
	// (b) the prompt file is returned as the stdin source.
	if stdinFile != promptFile {
		t.Errorf("stdinFile = %q, want %q", stdinFile, promptFile)
	}
	// (c) cmd.Stdin stays nil — stdin redirection is handled by the runner script.
	if cmd.Stdin != nil {
		t.Error("expected cmd.Stdin to be nil (stdin handled by runner script)")
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
	// claude delivers the prompt via stdin, so the prompt file is returned.
	if stdinFile != promptFile {
		t.Errorf("stdinFile = %q, want %q", stdinFile, promptFile)
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

func TestBuildWorkerCmd_CodexPromptFileIsValidUTF8(t *testing.T) {
	dir := t.TempDir()
	promptFile := filepath.Join(dir, "prompt.md")
	prompt := "assembled prompt with invalid byte: " + string([]byte{0xff}) + " end"
	if err := writePromptFile(promptFile, prompt); err != nil {
		t.Fatal(err)
	}

	_, stdinFile, err := BuildWorkerCmd("codex", BackendConfig{Cmd: "codex"}, promptFile, "/tmp/codex-worktree")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	data, err := os.ReadFile(stdinFile)
	if err != nil {
		t.Fatal(err)
	}

	if !utf8.Valid(data) {
		t.Fatalf("codex stdin prompt is not valid UTF-8: %q", data)
	}
	if !strings.Contains(string(data), "\uFFFD") {
		t.Fatalf("expected invalid byte to be replaced with U+FFFD, got %q", string(data))
	}
}

func TestBuildWorkerCmd_ClaudeWithMCPConfig(t *testing.T) {
	dir := t.TempDir()
	promptFile := filepath.Join(dir, "prompt.md")
	if err := os.WriteFile(promptFile, []byte("use docs"), 0644); err != nil {
		t.Fatal(err)
	}

	cfg := BackendConfig{
		Cmd: "claude",
		MCP: config.MCPConfig{
			Strict:  true,
			Configs: []string{"/tmp/project-mcp.json"},
			Servers: map[string]config.MCPServerDef{
				"docs": {
					Command: "npx",
					Args:    []string{"-y", "@example/docs-mcp"},
					Env:     map[string]string{"DOCS_ENV": "test"},
				},
			},
		},
	}
	cmd, stdinFile, err := BuildWorkerCmd("claude", cfg, promptFile, "/tmp/wt")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if stdinFile != promptFile {
		t.Errorf("stdinFile = %q, want %q", stdinFile, promptFile)
	}
	args := strings.Join(cmd.Args, " ")
	for _, want := range []string{
		"--mcp-config",
		"/tmp/project-mcp.json",
		"--strict-mcp-config",
		`"mcpServers"`,
		`"docs"`,
		`"command":"npx"`,
		`"args":["-y","@example/docs-mcp"]`,
	} {
		if !strings.Contains(args, want) {
			t.Errorf("expected %q in args, got: %s", want, args)
		}
	}
}

func TestBuildWorkerCmd_CodexWithMCPServers(t *testing.T) {
	dir := t.TempDir()
	promptFile := filepath.Join(dir, "prompt.md")
	if err := os.WriteFile(promptFile, []byte("use docs"), 0644); err != nil {
		t.Fatal(err)
	}

	cfg := BackendConfig{
		Cmd: "codex",
		MCP: config.MCPConfig{
			Servers: map[string]config.MCPServerDef{
				"docs": {
					Command:          "npx",
					Args:             []string{"-y", "@example/docs-mcp"},
					AllowedTools:     []string{"search_docs"},
					StartupTimeoutMs: 15000,
				},
				"symbols": {
					URL:               "https://mcp.example.invalid/mcp",
					BearerTokenEnvVar: "SYMBOLS_MCP_TOKEN",
				},
			},
		},
	}
	cmd, stdinFile, err := BuildWorkerCmd("codex", cfg, promptFile, "/tmp/wt")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if stdinFile != promptFile {
		t.Errorf("stdinFile = %q, want %q", stdinFile, promptFile)
	}
	args := strings.Join(cmd.Args, " ")
	for _, want := range []string{
		`mcp_servers."docs".command="npx"`,
		`mcp_servers."docs".args=["-y","@example/docs-mcp"]`,
		`mcp_servers."docs".allowed_tools=["search_docs"]`,
		`mcp_servers."docs".startup_timeout_ms=15000`,
		`mcp_servers."symbols".url="https://mcp.example.invalid/mcp"`,
		`mcp_servers."symbols".bearer_token_env_var="SYMBOLS_MCP_TOKEN"`,
	} {
		if !strings.Contains(args, want) {
			t.Errorf("expected %q in args, got: %s", want, args)
		}
	}
	if cmd.Args[len(cmd.Args)-1] != "-" {
		t.Errorf("codex stdin prompt marker should remain last, got args: %v", cmd.Args)
	}
}

func TestBuildWorkerCmd_NoMCPByDefault(t *testing.T) {
	dir := t.TempDir()
	promptFile := filepath.Join(dir, "prompt.md")
	if err := os.WriteFile(promptFile, []byte("plain worker"), 0644); err != nil {
		t.Fatal(err)
	}

	for _, backendName := range []string{"claude", "codex"} {
		cmd, _, err := BuildWorkerCmd(backendName, BackendConfig{Cmd: backendName}, promptFile, "/tmp/wt")
		if err != nil {
			t.Fatalf("%s: unexpected error: %v", backendName, err)
		}
		args := strings.Join(cmd.Args, " ")
		if strings.Contains(args, "mcp") || strings.Contains(args, "MCP") {
			t.Errorf("%s: expected no MCP args by default, got: %s", backendName, args)
		}
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

func TestBuildSupervisorCmd_ClaudeReadOnly(t *testing.T) {
	dir := t.TempDir()
	promptFile := filepath.Join(dir, "prompt.md")
	if err := os.WriteFile(promptFile, []byte("decide safely"), 0644); err != nil {
		t.Fatal(err)
	}

	cfg := BackendConfig{Cmd: "claude", Model: "sonnet", Effort: "medium"}
	cmd, stdinFile, err := BuildSupervisorCmd("claude", cfg, promptFile, "/tmp/wt")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Prompt is delivered via stdin (not argv) to stay under the Linux
	// single-argument size limit; the prompt file is returned as stdinFile.
	if stdinFile != promptFile {
		t.Errorf("stdinFile = %q, want %q", stdinFile, promptFile)
	}
	args := strings.Join(cmd.Args, " ")
	if strings.Contains(args, "dangerously") || strings.Contains(args, "bypass") {
		t.Errorf("supervisor command should not include worker permission bypass flags: %s", args)
	}
	if strings.Contains(args, "decide safely") {
		t.Errorf("prompt content must not appear in argv (stdin delivery): %s", args)
	}
	for _, want := range []string{"-p", "--model", "sonnet", "--effort", "medium"} {
		if !strings.Contains(args, want) {
			t.Errorf("expected %q in args, got: %s", want, args)
		}
	}
}

func TestBuildSupervisorCmd_CodexReadOnly(t *testing.T) {
	dir := t.TempDir()
	promptFile := filepath.Join(dir, "prompt.md")
	if err := os.WriteFile(promptFile, []byte("decide safely"), 0644); err != nil {
		t.Fatal(err)
	}

	cmd, stdinFile, err := BuildSupervisorCmd("codex", BackendConfig{Cmd: "codex"}, promptFile, "/tmp/wt")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if stdinFile != promptFile {
		t.Errorf("stdinFile = %q, want %q", stdinFile, promptFile)
	}
	args := strings.Join(cmd.Args, " ")
	if !strings.Contains(args, "exec") || !strings.Contains(args, "-C") {
		t.Errorf("expected codex exec args, got: %s", args)
	}
	if strings.Contains(args, "dangerously") || strings.Contains(args, "bypass") {
		t.Errorf("supervisor command should not include worker permission bypass flags: %s", args)
	}
}

func TestBuildSupervisorCmd_Kimi(t *testing.T) {
	dir := t.TempDir()
	promptFile := filepath.Join(dir, "prompt.md")
	if err := os.WriteFile(promptFile, []byte("decide safely"), 0644); err != nil {
		t.Fatal(err)
	}
	cfg := BackendConfig{Cmd: "kimi", Provider: "moonshot", Model: "kimi-k2"}
	cmd, stdinFile, err := BuildSupervisorCmd("kimi", cfg, promptFile, "/tmp/wt")
	if err != nil {
		t.Fatal(err)
	}
	if stdinFile != promptFile {
		t.Fatalf("stdinFile = %q, want %q", stdinFile, promptFile)
	}
	args := strings.Join(cmd.Args, " ")
	for _, want := range []string{"--print", "--final-message-only", "--model kimi-k2"} {
		if !strings.Contains(args, want) {
			t.Errorf("Kimi supervisor args missing %q: %s", want, args)
		}
	}
	if strings.Contains(args, "decide safely") {
		t.Errorf("supervisor prompt leaked into argv: %s", args)
	}
}

func TestBuildWorkerCmd_GeminiPromptFileError(t *testing.T) {
	cfg := BackendConfig{Cmd: "gemini"}
	_, _, err := BuildWorkerCmd("gemini", cfg, "/nonexistent/prompt.md", "/tmp/wt")
	if err == nil {
		t.Fatal("expected error for missing prompt file")
	}
	if !strings.Contains(err.Error(), "read prompt file") {
		t.Errorf("expected 'read prompt file' error, got: %v", err)
	}
}

func TestBuildWorkerCmd_GeminiArgOrder(t *testing.T) {
	dir := t.TempDir()
	promptFile := filepath.Join(dir, "prompt.md")
	if err := os.WriteFile(promptFile, []byte("test prompt"), 0644); err != nil {
		t.Fatal(err)
	}

	cfg := BackendConfig{Cmd: "gemini", ExtraArgs: []string{"--sandbox", "none"}}
	cmd, _, err := BuildWorkerCmd("gemini", cfg, promptFile, "/tmp/wt")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify exact argument structure: gemini -p <prompt> <extra_args...>
	// cmd.Args[0] is the command itself
	args := cmd.Args[1:] // skip command name
	if len(args) < 4 {
		t.Fatalf("expected at least 4 args, got %d: %v", len(args), args)
	}
	if args[0] != "-p" {
		t.Errorf("args[0] = %q, want %q", args[0], "-p")
	}
	if args[1] != "test prompt" {
		t.Errorf("args[1] = %q, want %q", args[1], "test prompt")
	}
	if args[2] != "--sandbox" {
		t.Errorf("args[2] = %q, want %q", args[2], "--sandbox")
	}
	if args[3] != "none" {
		t.Errorf("args[3] = %q, want %q", args[3], "none")
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
		// multiple consecutive spaces between tokens collapse per strings.Fields
		{"claude   --model   opus", "claude", []string{"--model", "opus"}},
		// leading and trailing whitespace around a multi-token command is stripped
		{"  claude --model opus  ", "claude", []string{"--model", "opus"}},
		// tabs are valid field separators, just like spaces
		{"claude\t--model\topus", "claude", []string{"--model", "opus"}},
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

// #684: the Anthropic CLI registered under a custom backend key (the sup-175
// `fable:` shape) must build the exact same exec invocation as the `claude:`
// key — skip-permissions flag, -p, prompt via stdin — resolved via the
// per-backend provider field.
func TestBuildWorkerCmd_CustomNameProviderAnthropic(t *testing.T) {
	dir := t.TempDir()
	promptFile := filepath.Join(dir, "prompt.md")
	if err := os.WriteFile(promptFile, []byte("implement the issue"), 0644); err != nil {
		t.Fatal(err)
	}
	worktree := "/tmp/fable-worktree"

	cfg := BackendConfig{Cmd: "claude --model claude-fable-5 --effort xhigh", Provider: "anthropic"}
	cmd, stdinFile, err := BuildWorkerCmd("fable", cfg, promptFile, worktree)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	args := strings.Join(cmd.Args, " ")
	if !strings.Contains(args, "--dangerously-skip-permissions") {
		t.Errorf("expected --dangerously-skip-permissions in args, got: %s", args)
	}
	if !strings.Contains(args, "-p") {
		t.Errorf("expected -p in args, got: %s", args)
	}
	if strings.Contains(args, "implement the issue") {
		t.Errorf("prompt content must not appear in argv (stdin delivery): %s", args)
	}
	if stdinFile != promptFile {
		t.Errorf("stdinFile = %q, want %q", stdinFile, promptFile)
	}

	// Same invocation as the claude: key with the same cmd.
	direct, directStdin, err := BuildWorkerCmd("claude", cfg, promptFile, worktree)
	if err != nil {
		t.Fatalf("unexpected error building claude-key cmd: %v", err)
	}
	if !reflect.DeepEqual(cmd.Args, direct.Args) {
		t.Errorf("custom-name args = %v, want same as claude-key args %v", cmd.Args, direct.Args)
	}
	if stdinFile != directStdin {
		t.Errorf("custom-name stdinFile = %q, want same as claude-key %q", stdinFile, directStdin)
	}
}

// #684: provider: openai under a custom key must build the codex exec shape
// (exec subcommand, bypass flag, -C worktree, stdin prompt via "-").
func TestBuildWorkerCmd_CustomNameProviderOpenAI(t *testing.T) {
	dir := t.TempDir()
	promptFile := filepath.Join(dir, "prompt.md")
	if err := os.WriteFile(promptFile, []byte("implement the issue"), 0644); err != nil {
		t.Fatal(err)
	}
	worktree := "/tmp/fast-worktree"

	cfg := BackendConfig{Cmd: "codex --profile fast", Provider: "openai"}
	cmd, stdinFile, err := BuildWorkerCmd("fast", cfg, promptFile, worktree)
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
	if !strings.Contains(args, "-C "+worktree) {
		t.Errorf("expected -C %s in args, got: %s", worktree, args)
	}
	if cmd.Args[len(cmd.Args)-1] != "-" {
		t.Errorf("expected stdin prompt marker '-' as last arg, got: %v", cmd.Args)
	}
	if stdinFile != promptFile {
		t.Errorf("stdinFile = %q, want %q", stdinFile, promptFile)
	}

	// Same invocation as the codex: key with the same cmd.
	direct, _, err := BuildWorkerCmd("codex", cfg, promptFile, worktree)
	if err != nil {
		t.Fatalf("unexpected error building codex-key cmd: %v", err)
	}
	if !reflect.DeepEqual(cmd.Args, direct.Args) {
		t.Errorf("custom-name args = %v, want same as codex-key args %v", cmd.Args, direct.Args)
	}
}

// #684 second heuristic: no provider set, but the cmd binary basename is a
// known CLI — must still resolve to the CLI-specific path.
func TestBuildWorkerCmd_CustomNameCmdBasenameFallback(t *testing.T) {
	dir := t.TempDir()
	promptFile := filepath.Join(dir, "prompt.md")
	if err := os.WriteFile(promptFile, []byte("do the thing"), 0644); err != nil {
		t.Fatal(err)
	}

	cfg := BackendConfig{Cmd: "/usr/local/bin/claude --model opus"}
	cmd, stdinFile, err := BuildWorkerCmd("mymodel", cfg, promptFile, "/tmp/wt")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	args := strings.Join(cmd.Args, " ")
	if !strings.Contains(args, "--dangerously-skip-permissions") {
		t.Errorf("expected --dangerously-skip-permissions in args, got: %s", args)
	}
	if stdinFile != promptFile {
		t.Errorf("stdinFile = %q, want %q", stdinFile, promptFile)
	}
	if strings.Contains(args, "do the thing") {
		t.Errorf("prompt content must not appear in argv (stdin delivery): %s", args)
	}
}

// #684: a genuinely custom CLI (unknown provider, unknown binary) keeps the
// generic path and its prompt_mode semantics.
func TestBuildWorkerCmd_UnknownProviderStaysGeneric(t *testing.T) {
	dir := t.TempDir()
	promptFile := filepath.Join(dir, "prompt.md")
	if err := os.WriteFile(promptFile, []byte("stdin prompt"), 0644); err != nil {
		t.Fatal(err)
	}

	cfg := BackendConfig{Cmd: "groq-cli --auto", Provider: "groq", PromptMode: "stdin"}
	cmd, stdinFile, err := BuildWorkerCmd("helper", cfg, promptFile, "/tmp/wt")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if stdinFile != promptFile {
		t.Errorf("stdinFile = %q, want %q", stdinFile, promptFile)
	}
	args := strings.Join(cmd.Args, " ")
	if strings.Contains(args, "dangerously") {
		t.Errorf("generic backend must not gain CLI permission-bypass flags: %s", args)
	}
}

// #684: supervisor commands for custom-named claude backends keep stdin
// prompt delivery and still avoid worker-only bypass flags.
func TestBuildSupervisorCmd_CustomNameProviderAnthropic(t *testing.T) {
	dir := t.TempDir()
	promptFile := filepath.Join(dir, "prompt.md")
	if err := os.WriteFile(promptFile, []byte("decide safely"), 0644); err != nil {
		t.Fatal(err)
	}

	cfg := BackendConfig{Cmd: "claude --model claude-fable-5", Provider: "anthropic"}
	cmd, stdinFile, err := BuildSupervisorCmd("fable", cfg, promptFile, "/tmp/wt")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if stdinFile != promptFile {
		t.Errorf("stdinFile = %q, want %q", stdinFile, promptFile)
	}
	args := strings.Join(cmd.Args, " ")
	if !strings.Contains(args, "-p") {
		t.Errorf("expected -p in args, got: %s", args)
	}
	if strings.Contains(args, "dangerously") || strings.Contains(args, "bypass") {
		t.Errorf("supervisor command should not include worker permission bypass flags: %s", args)
	}
	if strings.Contains(args, "decide safely") {
		t.Errorf("prompt content must not appear in argv (stdin delivery): %s", args)
	}
}

func TestKnownBackends(t *testing.T) {
	backends := KnownBackends()
	expected := map[string]bool{"claude": false, "codex": false, "gemini": false, "cline": false, "kimi": false, "pi": false}
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

func containsArg(args []string, want string) bool {
	for _, arg := range args {
		if arg == want {
			return true
		}
	}
	return false
}

func countFlag(args []string, flag string) int {
	count := 0
	for _, arg := range args {
		if arg == flag || strings.HasPrefix(arg, flag+"=") {
			count++
		}
	}
	return count
}

// #730: the Pi backend runs headless in JSON event-stream mode with the
// prompt delivered via stdin (-p with no prompt argument) so a large worker
// prompt never hits the Linux MAX_ARG_STRLEN single-argument limit.
func TestBuildWorkerCmd_Pi(t *testing.T) {
	dir := t.TempDir()
	promptFile := filepath.Join(dir, "prompt.md")
	if err := os.WriteFile(promptFile, []byte("implement the issue"), 0644); err != nil {
		t.Fatal(err)
	}
	worktree := "/tmp/pi-worktree"

	cfg := BackendConfig{Cmd: "pi", Provider: "ollama", Model: "glm-5.2:cloud", ExtraArgs: []string{"--verbose"}}
	cmd, stdinFile, err := BuildWorkerCmd("pi", cfg, promptFile, worktree)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if stdinFile != promptFile {
		t.Errorf("stdinFile = %q, want %q (Pi -p reads prompt from stdin)", stdinFile, promptFile)
	}
	args := strings.Join(cmd.Args, " ")
	for _, want := range []string{"--mode json", "--no-session", "--provider ollama", "--model glm-5.2:cloud", "--tools", "-p", "--verbose"} {
		if !strings.Contains(args, want) {
			t.Errorf("expected %q in args, got: %s", want, args)
		}
	}
	if strings.Contains(args, "implement the issue") {
		t.Errorf("prompt content must not appear in argv (stdin delivery): %s", args)
	}
	// -p must be the last token so extra_args flags stay flags.
	if cmd.Args[len(cmd.Args)-1] != "-p" {
		t.Errorf("expected -p as last arg, got: %v", cmd.Args)
	}
	if cmd.Dir != worktree {
		t.Errorf("expected Dir=%s, got %s", worktree, cmd.Dir)
	}
}

// #730: a large Pi prompt is delivered via stdin, never as an argument.
func TestBuildWorkerCmd_PiLargePromptViaStdin(t *testing.T) {
	dir := t.TempDir()
	promptFile := filepath.Join(dir, "prompt.md")
	largePrompt := strings.Repeat("PI_PROMPT_PAYLOAD\n", (1<<20)/16+16)
	if err := os.WriteFile(promptFile, []byte(largePrompt), 0644); err != nil {
		t.Fatal(err)
	}
	cfg := BackendConfig{Cmd: "pi", Provider: "ollama", Model: "glm-5.2:cloud"}
	cmd, stdinFile, err := BuildWorkerCmd("pi", cfg, promptFile, "/tmp/wt")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for i, a := range cmd.Args {
		if strings.Contains(a, "PI_PROMPT_PAYLOAD") {
			t.Fatalf("prompt content leaked into argv at index %d (E2BIG risk): %s", i, a)
		}
	}
	if stdinFile != promptFile {
		t.Errorf("stdinFile = %q, want %q", stdinFile, promptFile)
	}
}

// #730: a custom-named Pi backend resolves via provider: ollama to the pi
// exec path, mirroring the claude/codex custom-name behaviour from #684.
func TestBuildWorkerCmd_CustomNameProviderOllama(t *testing.T) {
	dir := t.TempDir()
	promptFile := filepath.Join(dir, "prompt.md")
	if err := os.WriteFile(promptFile, []byte("do the thing"), 0644); err != nil {
		t.Fatal(err)
	}
	cfg := BackendConfig{Cmd: "pi", Provider: "ollama", Model: "glm-5.2:cloud"}
	cmd, stdinFile, err := BuildWorkerCmd("pi-ollama", cfg, promptFile, "/tmp/wt")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	args := strings.Join(cmd.Args, " ")
	if !strings.Contains(args, "--mode json") || !strings.Contains(args, "--provider ollama") {
		t.Errorf("expected pi json+provider args, got: %s", args)
	}
	if stdinFile != promptFile {
		t.Errorf("stdinFile = %q, want %q", stdinFile, promptFile)
	}
	direct, _, err := BuildWorkerCmd("pi", cfg, promptFile, "/tmp/wt")
	if err != nil {
		t.Fatalf("unexpected error building pi-key cmd: %v", err)
	}
	if !reflect.DeepEqual(cmd.Args, direct.Args) {
		t.Errorf("custom-name args = %v, want same as pi-key args %v", cmd.Args, direct.Args)
	}
}

// #730: the Pi supervisor command runs read-only (--no-tools) and delivers
// the prompt via stdin, never via argv, and never with worker bypass flags.
func TestBuildSupervisorCmd_PiReadOnly(t *testing.T) {
	dir := t.TempDir()
	promptFile := filepath.Join(dir, "prompt.md")
	if err := os.WriteFile(promptFile, []byte("decide safely"), 0644); err != nil {
		t.Fatal(err)
	}
	cfg := BackendConfig{Cmd: "pi", Provider: "ollama", Model: "glm-5.2:cloud"}
	cmd, stdinFile, err := BuildSupervisorCmd("pi", cfg, promptFile, "/tmp/wt")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if stdinFile != promptFile {
		t.Errorf("stdinFile = %q, want %q", stdinFile, promptFile)
	}
	args := strings.Join(cmd.Args, " ")
	for _, want := range []string{"--mode json", "--no-session", "--provider ollama", "--model glm-5.2:cloud", "--no-tools", "-p"} {
		if !strings.Contains(args, want) {
			t.Errorf("expected %q in args, got: %s", want, args)
		}
	}
	if strings.Contains(args, "decide safely") {
		t.Errorf("prompt content must not appear in argv (stdin delivery): %s", args)
	}
	if strings.Contains(args, "dangerously") || strings.Contains(args, "bypass") {
		t.Errorf("supervisor command should not include worker permission bypass flags: %s", args)
	}
	if cmd.Args[len(cmd.Args)-1] != "-p" {
		t.Errorf("expected -p as last arg, got: %v", cmd.Args)
	}
}
