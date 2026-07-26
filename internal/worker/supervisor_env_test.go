package worker

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/befeast/maestro/internal/config"
)

// envValues returns every value assigned to key in a child environment. More
// than one entry is a defect: the last assignment wins on Linux, so a leftover
// inherited TMPDIR next to the injected one is only accidentally harmless.
func envValues(env []string, key string) []string {
	var out []string
	for _, kv := range env {
		name, value, found := strings.Cut(kv, "=")
		if found && name == key {
			out = append(out, value)
		}
	}
	return out
}

// TestBuildSupervisorCmd_TempEnvPerBackendKind covers every kind
// resolveBackendKind can return plus the generic fallback (#1127). Supervisor
// probes are daemon children outside any worker lease, so this env assignment
// is the only thing keeping them off the RAM-backed host /tmp.
func TestBuildSupervisorCmd_TempEnvPerBackendKind(t *testing.T) {
	dir := t.TempDir()
	promptFile := filepath.Join(dir, "prompt.md")
	if err := os.WriteFile(promptFile, []byte("decide safely"), 0644); err != nil {
		t.Fatal(err)
	}
	tempDir := filepath.Join(dir, "supervisor-temp")
	// The daemon's own environment is what the probes used to inherit; point it
	// at the RAM-backed /tmp so the assertion proves an override, not an append.
	t.Setenv("TMPDIR", "/tmp")
	t.Setenv("TMP", "/tmp")
	t.Setenv("TEMP", "/tmp")

	tests := []struct {
		name    string
		backend string
		cfg     BackendConfig
	}{
		{name: "claude", backend: "claude", cfg: BackendConfig{Cmd: "claude"}},
		{name: "codex", backend: "codex", cfg: BackendConfig{Cmd: "codex"}},
		{name: "gemini", backend: "gemini", cfg: BackendConfig{Cmd: "gemini"}},
		{name: "kimi", backend: "kimi", cfg: BackendConfig{Cmd: "kimi"}},
		{name: "cline", backend: "cline", cfg: BackendConfig{Cmd: "cline"}},
		{name: "pi", backend: "pi", cfg: BackendConfig{Cmd: "pi"}},
		{name: "opencode", backend: "opencode", cfg: BackendConfig{Cmd: "opencode"}},
		{name: "custom name resolves to claude", backend: "fable", cfg: BackendConfig{Cmd: "claude", Provider: "anthropic"}},
		{name: "generic", backend: "some-cli", cfg: BackendConfig{Cmd: "some-cli", PromptMode: "stdin"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := tc.cfg
			cfg.TempDir = tempDir
			cmd, _, err := BuildSupervisorCmd(tc.backend, cfg, promptFile, dir)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if cmd.Env == nil {
				t.Fatal("cmd.Env is nil: the child inherits the daemon environment and its RAM-backed TMPDIR")
			}
			for _, key := range []string{"TMPDIR", "TMP", "TEMP"} {
				values := envValues(cmd.Env, key)
				if len(values) != 1 {
					t.Fatalf("%s assigned %d times (%v), want exactly once", key, len(values), values)
				}
				if values[0] != tempDir {
					t.Errorf("%s = %q, want %q", key, values[0], tempDir)
				}
			}
			// The child still needs the rest of the daemon environment (PATH,
			// provider credentials); only the temp variables are replaced.
			if got := envValues(cmd.Env, "PATH"); len(got) != 1 || got[0] != os.Getenv("PATH") {
				t.Errorf("PATH = %v, want the inherited %q", got, os.Getenv("PATH"))
			}
		})
	}

	info, err := os.Stat(tempDir)
	if err != nil {
		t.Fatalf("supervisor temp dir was not created: %v", err)
	}
	if !info.IsDir() {
		t.Fatalf("supervisor temp dir %s is not a directory", tempDir)
	}
	if perm := info.Mode().Perm(); perm != 0700 {
		t.Errorf("supervisor temp dir mode = %o, want 700", perm)
	}
}

// TestBuildSupervisorCmd_TempEnvDefaultsToDiskBacked pins the fallback used when
// a caller does not thread supervisor.temp_dir through: it must still be a
// disk-backed path outside global /tmp.
func TestBuildSupervisorCmd_TempEnvDefaultsToDiskBacked(t *testing.T) {
	dir := t.TempDir()
	promptFile := filepath.Join(dir, "prompt.md")
	if err := os.WriteFile(promptFile, []byte("decide safely"), 0644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", "/tmp")

	cmd, _, err := BuildSupervisorCmd("claude", BackendConfig{Cmd: "claude"}, promptFile, dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := config.DefaultSupervisorTempDir()
	values := envValues(cmd.Env, "TMPDIR")
	if len(values) != 1 || values[0] != want {
		t.Fatalf("TMPDIR = %v, want [%s]", values, want)
	}
	if want == "/tmp" || strings.HasPrefix(want, "/tmp/") {
		t.Fatalf("default supervisor temp dir %q is under the RAM-backed global /tmp", want)
	}
}
