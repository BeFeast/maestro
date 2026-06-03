package worker

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/befeast/maestro/internal/config"
)

func TestSetupWorkerToolHooksDefaultOff(t *testing.T) {
	setup, err := setupWorkerToolHooks(t.TempDir(), t.TempDir(), "claude", config.HooksConfig{TimeoutMs: 1000})
	if err != nil {
		t.Fatalf("setupWorkerToolHooks: %v", err)
	}
	if setup.RunnerPath != "" || setup.Claude {
		t.Fatalf("setup = %+v, want empty/default off", setup)
	}
}

func TestSetupWorkerToolHooksWritesClaudeSettingsAndExcludesLocalFile(t *testing.T) {
	worktree := newGitRepo(t)
	stateDir := t.TempDir()
	hooks := config.HooksConfig{
		TimeoutMs: 2000,
		PreTool: config.ToolHookConfig{
			Command:        "echo pre",
			BlockOnFailure: true,
		},
		PostEdit: config.ToolHookConfig{
			Command: "echo post",
			Matcher: "Write|Edit",
		},
	}

	setup, err := setupWorkerToolHooks(stateDir, worktree, "claude", hooks)
	if err != nil {
		t.Fatalf("setupWorkerToolHooks: %v", err)
	}
	if !setup.Claude {
		t.Fatal("setup.Claude = false, want true")
	}
	if setup.RunnerPath == "" {
		t.Fatal("RunnerPath should be set")
	}
	if _, err := os.Stat(setup.RunnerPath); err != nil {
		t.Fatalf("runner missing: %v", err)
	}

	settingsData, err := os.ReadFile(filepath.Join(worktree, ".claude", "settings.local.json"))
	if err != nil {
		t.Fatalf("read settings: %v", err)
	}
	var settings map[string]any
	if err := json.Unmarshal(settingsData, &settings); err != nil {
		t.Fatalf("settings JSON: %v\n%s", err, settingsData)
	}
	content := string(settingsData)
	for _, want := range []string{
		`"PreToolUse"`,
		`"PostToolUse"`,
		`"matcher": "*"`,
		`"matcher": "Write|Edit"`,
		"MAESTRO_WORKER_HOOK_EVENT",
		setup.RunnerPath,
	} {
		if !strings.Contains(content, want) {
			t.Fatalf("settings missing %q\n%s", want, content)
		}
	}

	excludePath := gitPath(t, worktree, "info/exclude")
	excludeData, err := os.ReadFile(excludePath)
	if err != nil {
		t.Fatalf("read git exclude: %v", err)
	}
	if !strings.Contains(string(excludeData), ".claude/settings.local.json") {
		t.Fatalf("git exclude missing local settings entry:\n%s", excludeData)
	}
}

func TestSetupWorkerToolHooksNonClaudeWritesRunnerOnly(t *testing.T) {
	stateDir := t.TempDir()
	worktree := t.TempDir()
	hooks := config.HooksConfig{PostEdit: config.ToolHookConfig{Command: "echo post"}}

	setup, err := setupWorkerToolHooks(stateDir, worktree, "codex", hooks)
	if err != nil {
		t.Fatalf("setupWorkerToolHooks: %v", err)
	}
	if setup.Claude {
		t.Fatal("setup.Claude = true, want false for codex")
	}
	if setup.RunnerPath == "" {
		t.Fatal("RunnerPath should be set for prompt fallback")
	}
	if _, err := os.Stat(filepath.Join(worktree, ".claude", "settings.local.json")); !os.IsNotExist(err) {
		t.Fatalf("codex setup should not write Claude settings, err=%v", err)
	}
}

func TestWorkerToolHookPromptSection(t *testing.T) {
	hooks := config.HooksConfig{
		PreTool:  config.ToolHookConfig{Command: "./check-pre.sh", BlockOnFailure: true},
		PostEdit: config.ToolHookConfig{Command: "gofmt -w ."},
	}
	section := workerToolHookPromptSection(hooks, "codex", workerToolHookSetup{RunnerPath: "/tmp/hook.py"})

	for _, want := range []string{
		"## Worker Tool Hooks",
		"does not expose a supported automatic hook installer",
		"./check-pre.sh",
		"gofmt -w .",
		"/tmp/hook.py",
	} {
		if !strings.Contains(section, want) {
			t.Fatalf("section missing %q\n%s", want, section)
		}
	}
}

func TestWorkerToolHookRunnerFeedsFailureOutputAsContext(t *testing.T) {
	stateDir := t.TempDir()
	hooks := config.HooksConfig{
		TimeoutMs: 5000,
		PostEdit: config.ToolHookConfig{
			Command:        `echo out; echo err >&2; exit 3`,
			BlockOnFailure: true,
		},
	}
	runnerPath, err := writeWorkerToolHookRunner(stateDir, hooks)
	if err != nil {
		t.Fatalf("writeWorkerToolHookRunner: %v", err)
	}

	cmd := exec.Command(runnerPath)
	cmd.Env = append(os.Environ(), "MAESTRO_WORKER_HOOK_EVENT=post_edit")
	cmd.Stdin = strings.NewReader(`{"tool_name":"Write","tool_input":{"file_path":"main.go"}}`)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("runner should return JSON with exit 0, got %v\n%s", err, out)
	}
	got := string(out)
	for _, want := range []string{
		`"decision": "block"`,
		"Maestro post_edit hook failed (exit 3)",
		"stdout:",
		"out",
		"stderr:",
		"err",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("runner output missing %q\n%s", want, got)
		}
	}
}

func newGitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	if out, err := exec.Command("git", "init", dir).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("test\n"), 0644); err != nil {
		t.Fatalf("write README: %v", err)
	}
	run("add", "README.md")
	run("-c", "user.name=Test", "-c", "user.email=test@example.invalid", "commit", "-m", "init")
	return dir
}

func gitPath(t *testing.T, worktree, path string) string {
	t.Helper()
	out, err := exec.Command("git", "-C", worktree, "rev-parse", "--git-path", path).CombinedOutput()
	if err != nil {
		t.Fatalf("git rev-parse --git-path %s: %v\n%s", path, err, out)
	}
	got := strings.TrimSpace(string(out))
	if filepath.IsAbs(got) {
		return got
	}
	return filepath.Join(worktree, got)
}
