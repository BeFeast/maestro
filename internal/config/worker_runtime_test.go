package config

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestWorkerRuntimeDefaultsToLegacyRollbackPath(t *testing.T) {
	var runtime WorkerRuntimeConfig
	if got := runtime.EffectiveMode(); got != WorkerRuntimeModeLegacy {
		t.Fatalf("mode = %q, want legacy", got)
	}
	if runtime.IsolatedEnabled() {
		t.Fatal("zero-value worker runtime unexpectedly enabled isolation")
	}
	if got := runtime.EffectiveScope(); got != WorkerRuntimeScopeSystem {
		t.Fatalf("scope = %q, want system", got)
	}
	if got := runtime.EffectiveScratchRoot(); !strings.HasPrefix(got, "/var/tmp/maestro-workers-") {
		t.Fatalf("scratch root = %q, want disk-backed /var/tmp default", got)
	}
}

func TestParseWorkerRuntimeIsolated(t *testing.T) {
	home := filepath.Join("/var/tmp", "maestro-worker-runtime-test-home")
	t.Setenv("HOME", home)
	cfg, err := Parse([]byte(`
repo: owner/repo
local_path: /srv/repo
worktree_base: /srv/worktrees
worker_runtime:
  mode: isolated
  scope: user
  scratch_root: ~/worker-scratch
  memory_max_mb: 4096
`))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.WorkerRuntime.IsolatedEnabled() || cfg.WorkerRuntime.EffectiveScope() != WorkerRuntimeScopeUser {
		t.Fatalf("runtime = %+v", cfg.WorkerRuntime)
	}
	if got, want := cfg.WorkerRuntime.EffectiveScratchRoot(), filepath.Join(home, "worker-scratch"); got != want {
		t.Fatalf("scratch root = %q, want %q", got, want)
	}
	if cfg.WorkerRuntime.MemoryMaxMB != 4096 {
		t.Fatalf("memory max = %d, want 4096", cfg.WorkerRuntime.MemoryMaxMB)
	}
}

func TestParseWorkerRuntimeRejectsGlobalTmpAndInvalidSettings(t *testing.T) {
	tests := []struct {
		name    string
		stanza  string
		wantErr string
	}{
		{name: "mode", stanza: "  mode: other\n", wantErr: "worker_runtime.mode"},
		{name: "scope", stanza: "  mode: isolated\n  scope: session\n", wantErr: "worker_runtime.scope"},
		{name: "relative", stanza: "  mode: isolated\n  scratch_root: relative\n", wantErr: "must be absolute"},
		{name: "global tmp", stanza: "  mode: isolated\n  scratch_root: /tmp/maestro-workers\n", wantErr: "must be disk-backed"},
		{name: "negative memory", stanza: "  memory_max_mb: -1\n", wantErr: "memory_max_mb"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte("repo: owner/repo\nlocal_path: /srv/repo\nworktree_base: /srv/worktrees\nworker_runtime:\n" + tc.stanza))
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %v, want %q", err, tc.wantErr)
			}
		})
	}
}
