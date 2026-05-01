package worker

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

type searchGuardrailHarness struct {
	guardDir string
	fakeDir  string
	worktree string
}

func newSearchGuardrailHarness(t *testing.T) searchGuardrailHarness {
	t.Helper()

	baseDir := t.TempDir()
	worktree := filepath.Join(baseDir, "worktree")
	fakeDir := filepath.Join(baseDir, "fake-bin")
	if err := os.MkdirAll(worktree, 0755); err != nil {
		t.Fatalf("create worktree: %v", err)
	}
	if err := os.MkdirAll(fakeDir, 0755); err != nil {
		t.Fatalf("create fake bin: %v", err)
	}

	guardDir, err := ensureSearchGuardrailWrappers(filepath.Join(baseDir, "state"))
	if err != nil {
		t.Fatalf("ensureSearchGuardrailWrappers: %v", err)
	}

	fakeScript := `#!/bin/sh
printf 'real:%s:%s\n' "$0" "$PWD"
printf 'args:'
for arg in "$@"; do
  printf '<%s>' "$arg"
done
printf '\n'
`
	for _, name := range searchGuardedCommands {
		if err := os.WriteFile(filepath.Join(fakeDir, name), []byte(fakeScript), 0755); err != nil {
			t.Fatalf("write fake %s: %v", name, err)
		}
	}

	return searchGuardrailHarness{
		guardDir: guardDir,
		fakeDir:  fakeDir,
		worktree: worktree,
	}
}

func (h searchGuardrailHarness) run(t *testing.T, name, cwd string, allowBroad bool, args ...string) (string, int) {
	t.Helper()

	cmd := exec.Command(filepath.Join(h.guardDir, name), args...)
	cmd.Dir = cwd
	cmd.Env = []string{
		"MAESTRO_WORKTREE=" + h.worktree,
		"MAESTRO_ORIGINAL_PATH=" + h.fakeDir,
		"PATH=" + h.guardDir + string(os.PathListSeparator) + h.fakeDir,
	}
	if allowBroad {
		cmd.Env = append(cmd.Env, "MAESTRO_ALLOW_BROAD_SEARCH=1")
	} else {
		cmd.Env = append(cmd.Env, "MAESTRO_ALLOW_BROAD_SEARCH=")
	}

	output, err := cmd.CombinedOutput()
	if err == nil {
		return string(output), 0
	}
	exitErr, ok := err.(*exec.ExitError)
	if !ok {
		t.Fatalf("run guarded %s: %v", name, err)
	}
	return string(output), exitErr.ExitCode()
}

func TestSearchGuardrailWrapperAllowsBroadLookingPatterns(t *testing.T) {
	h := newSearchGuardrailHarness(t)
	tests := []struct {
		name    string
		command string
		args    []string
	}{
		{
			name:    "rg positional pattern",
			command: "rg",
			args:    []string{"/home/", "."},
		},
		{
			name:    "rg regexp option pattern",
			command: "rg",
			args:    []string{"--regexp", "/tmp/", "."},
		},
		{
			name:    "grep positional pattern",
			command: "grep",
			args:    []string{"/tmp/", "file.txt"},
		},
		{
			name:    "grep regexp option pattern",
			command: "grep",
			args:    []string{"-e", "/home/", "file.txt"},
		},
		{
			name:    "find expression pattern",
			command: "find",
			args:    []string{".", "-name", "/tmp/*"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			output, code := h.run(t, tt.command, h.worktree, false, tt.args...)
			if code != 0 {
				t.Fatalf("guarded %s exited %d, output:\n%s", tt.command, code, output)
			}
			if !strings.Contains(output, "real:") {
				t.Fatalf("guarded %s did not run real command, output:\n%s", tt.command, output)
			}
		})
	}
}

func TestSearchGuardrailWrapperRejectsBroadSearchScopes(t *testing.T) {
	h := newSearchGuardrailHarness(t)
	tests := []struct {
		name    string
		command string
		args    []string
	}{
		{
			name:    "rg path operand",
			command: "rg",
			args:    []string{"needle", "/"},
		},
		{
			name:    "rg files mode path operand",
			command: "rg",
			args:    []string{"--files", "/tmp"},
		},
		{
			name:    "grep file operand",
			command: "grep",
			args:    []string{"needle", "/tmp"},
		},
		{
			name:    "find path operand",
			command: "find",
			args:    []string{"/tmp", "-name", "needle"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			output, code := h.run(t, tt.command, h.worktree, false, tt.args...)
			if code != 2 {
				t.Fatalf("guarded %s exited %d, want 2, output:\n%s", tt.command, code, output)
			}
			if !strings.Contains(output, "broad filesystem path") || !strings.Contains(output, h.worktree) {
				t.Fatalf("guarded %s did not point back to worktree, output:\n%s", tt.command, output)
			}
			if strings.Contains(output, "real:") {
				t.Fatalf("guarded %s ran real command after rejection, output:\n%s", tt.command, output)
			}
		})
	}
}

func TestSearchGuardrailWrapperRejectsBroadCWD(t *testing.T) {
	h := newSearchGuardrailHarness(t)
	output, code := h.run(t, "rg", "/tmp", false, "needle")
	if code != 2 {
		t.Fatalf("guarded rg exited %d, want 2, output:\n%s", code, output)
	}
	if !strings.Contains(output, "broad filesystem root") || !strings.Contains(output, h.worktree) {
		t.Fatalf("guarded rg did not point back to worktree, output:\n%s", output)
	}
	if strings.Contains(output, "real:") {
		t.Fatalf("guarded rg ran real command after rejection, output:\n%s", output)
	}
}

func TestSearchGuardrailWrapperAllowsExplicitBroadSearch(t *testing.T) {
	h := newSearchGuardrailHarness(t)
	output, code := h.run(t, "rg", "/tmp", true, "needle", "/")
	if code != 0 {
		t.Fatalf("guarded rg exited %d, output:\n%s", code, output)
	}
	if !strings.Contains(output, "real:") {
		t.Fatalf("guarded rg did not run real command, output:\n%s", output)
	}
}

func TestBuildWorkerRunnerScriptIncludesSearchGuardrails(t *testing.T) {
	script := buildWorkerRunnerScript(
		[]string{"codex", "exec", "-"},
		"/tmp/prompt.md",
		"/tmp/worker.log",
		"/tmp/worktree",
		"/tmp/state/search-guardrails",
	)

	for _, want := range []string{
		"export MAESTRO_WORKTREE='/tmp/worktree'",
		"export MAESTRO_SEARCH_GUARDRAIL_DIR='/tmp/state/search-guardrails'",
		"export PATH=\"$MAESTRO_SEARCH_GUARDRAIL_DIR:$MAESTRO_ORIGINAL_PATH\"",
		"cd \"$MAESTRO_WORKTREE\" || exit 1",
		"[maestro] worker worktree:",
		"exec codex exec - < '/tmp/prompt.md' 2>&1 | tee -a '/tmp/worker.log'",
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("runner script missing %q\nscript:\n%s", want, script)
		}
	}
}

func TestEnsureSearchGuardrailWrappers(t *testing.T) {
	guardDir, err := ensureSearchGuardrailWrappers(t.TempDir())
	if err != nil {
		t.Fatalf("ensureSearchGuardrailWrappers: %v", err)
	}

	for _, name := range searchGuardedCommands {
		path := filepath.Join(guardDir, name)
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("missing wrapper %s: %v", name, err)
		}
		if info.Mode()&0111 == 0 {
			t.Fatalf("wrapper %s is not executable: %v", name, info.Mode())
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read wrapper %s: %v", name, err)
		}
		content := string(data)
		for _, want := range []string{"MAESTRO_WORKTREE", "MAESTRO_ALLOW_BROAD_SEARCH", "broad filesystem"} {
			if !strings.Contains(content, want) {
				t.Fatalf("wrapper %s missing %q\n%s", name, want, content)
			}
		}
	}
}
