package worker

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/befeast/maestro/internal/config"
)

func TestRunHook_EmptyScript(t *testing.T) {
	cfg := &config.Config{
		Repo:  "owner/repo",
		Hooks: config.HooksConfig{TimeoutMs: 5000},
	}
	env := HookEnv{IssueNumber: 42, WorkspacePath: t.TempDir()}
	if err := RunHook(cfg, "test", "", env); err != nil {
		t.Fatalf("expected nil error for empty script, got: %v", err)
	}
	if err := RunHook(cfg, "test", "   ", env); err != nil {
		t.Fatalf("expected nil error for whitespace script, got: %v", err)
	}
}

func TestRunHook_Success(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "marker.txt")

	cfg := &config.Config{
		Repo:  "owner/repo",
		Hooks: config.HooksConfig{TimeoutMs: 5000},
	}
	env := HookEnv{IssueID: "owner/repo#42", IssueNumber: 42, WorkspacePath: dir}

	script := "echo hello > " + marker
	if err := RunHook(cfg, "after_create", script, env); err != nil {
		t.Fatalf("RunHook failed: %v", err)
	}

	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("expected marker file to exist: %v", err)
	}
}

func TestRunHook_Failure(t *testing.T) {
	cfg := &config.Config{
		Repo:  "owner/repo",
		Hooks: config.HooksConfig{TimeoutMs: 5000},
	}
	env := HookEnv{IssueNumber: 42, WorkspacePath: t.TempDir()}

	err := RunHook(cfg, "before_run", "exit 1", env)
	if err == nil {
		t.Fatal("expected error for failing script")
	}
}

func TestRunHook_Timeout(t *testing.T) {
	cfg := &config.Config{
		Repo:  "owner/repo",
		Hooks: config.HooksConfig{TimeoutMs: 100}, // 100ms timeout
	}
	env := HookEnv{IssueNumber: 42, WorkspacePath: t.TempDir()}

	err := RunHook(cfg, "test", "sleep 10", env)
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if got := err.Error(); !strings.Contains(got, "timed out") {
		t.Fatalf("expected timeout error, got: %v", err)
	}
}

func TestRunHook_EnvVars(t *testing.T) {
	dir := t.TempDir()
	outFile := filepath.Join(dir, "env.txt")

	cfg := &config.Config{
		Repo:  "owner/repo",
		Hooks: config.HooksConfig{TimeoutMs: 5000},
	}
	env := HookEnv{
		IssueID:       "owner/repo#42",
		IssueNumber:   42,
		WorkspacePath: dir,
	}

	script := `echo "ID=$ISSUE_ID NUM=$ISSUE_NUMBER WS=$WORKSPACE_PATH" > ` + outFile
	if err := RunHook(cfg, "test", script, env); err != nil {
		t.Fatalf("RunHook failed: %v", err)
	}

	data, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatalf("read output: %v", err)
	}
	got := string(data)
	if !strings.Contains(got, "ID=owner/repo#42") {
		t.Errorf("ISSUE_ID not set correctly, got: %s", got)
	}
	if !strings.Contains(got, "NUM=42") {
		t.Errorf("ISSUE_NUMBER not set correctly, got: %s", got)
	}
	if !strings.Contains(got, "WS="+dir) {
		t.Errorf("WORKSPACE_PATH not set correctly, got: %s", got)
	}
}

func TestRunHook_WorkingDirectory(t *testing.T) {
	dir := t.TempDir()
	outFile := filepath.Join(dir, "pwd.txt")

	cfg := &config.Config{
		Repo:  "owner/repo",
		Hooks: config.HooksConfig{TimeoutMs: 5000},
	}
	env := HookEnv{IssueNumber: 1, WorkspacePath: dir}

	script := "pwd > " + outFile
	if err := RunHook(cfg, "test", script, env); err != nil {
		t.Fatalf("RunHook failed: %v", err)
	}

	data, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatalf("read output: %v", err)
	}
	got := string(data)
	if !strings.Contains(got, dir) {
		t.Errorf("expected working dir %s, got: %s", dir, got)
	}
}
