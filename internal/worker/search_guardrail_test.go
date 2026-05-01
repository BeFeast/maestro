package worker

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestClassifySearchGuardrail(t *testing.T) {
	worktree := "/mnt/storage/worktrees/repo/sup-27"
	tests := []struct {
		name       string
		command    string
		cwd        string
		args       []string
		allowBroad bool
		wantWarn   bool
		wantReason searchGuardrailReason
	}{
		{
			name:       "rg from root warns",
			command:    "rg",
			cwd:        "/",
			wantWarn:   true,
			wantReason: searchGuardrailBroadCWD,
		},
		{
			name:       "find from mnt warns",
			command:    "find",
			cwd:        "/mnt",
			wantWarn:   true,
			wantReason: searchGuardrailBroadCWD,
		},
		{
			name:       "grep from home user warns",
			command:    "grep",
			cwd:        "/home/dev",
			wantWarn:   true,
			wantReason: searchGuardrailBroadCWD,
		},
		{
			name:       "search inside worktree is allowed",
			command:    "rg",
			cwd:        "/mnt/storage/worktrees/repo/sup-27/internal/worker",
			wantReason: searchGuardrailNone,
		},
		{
			name:       "explicit broad search path warns",
			command:    "rg",
			cwd:        worktree,
			args:       []string{"guardrail", "/"},
			wantWarn:   true,
			wantReason: searchGuardrailBroadArg,
		},
		{
			name:       "explicit allow suppresses warning",
			command:    "rg",
			cwd:        "/",
			allowBroad: true,
			wantReason: searchGuardrailAllowed,
		},
		{
			name:       "non search command ignored",
			command:    "go",
			cwd:        "/",
			wantReason: searchGuardrailNone,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := classifySearchGuardrail(tt.command, tt.cwd, worktree, tt.args, tt.allowBroad)
			if got.Warn != tt.wantWarn || got.Reason != tt.wantReason {
				t.Fatalf("classifySearchGuardrail() = %+v, want warn=%v reason=%s", got, tt.wantWarn, tt.wantReason)
			}
		})
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
