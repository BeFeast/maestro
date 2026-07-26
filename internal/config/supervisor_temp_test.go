package config

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestSupervisorTempDirDefaultsToDiskBackedPath(t *testing.T) {
	var supervisor SupervisorConfig
	got := supervisor.EffectiveTempDir()
	if !strings.HasPrefix(got, "/var/tmp/maestro-supervisor-") {
		t.Fatalf("temp dir = %q, want disk-backed /var/tmp default", got)
	}
}

func TestParseSupervisorTempDir(t *testing.T) {
	home := filepath.Join("/var/tmp", "maestro-supervisor-temp-test-home")
	t.Setenv("HOME", home)
	cfg, err := Parse([]byte(`
repo: owner/repo
local_path: /srv/repo
worktree_base: /srv/worktrees
supervisor:
  temp_dir: ~/supervisor-temp
`))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := cfg.Supervisor.EffectiveTempDir(), filepath.Join(home, "supervisor-temp"); got != want {
		t.Fatalf("temp dir = %q, want %q", got, want)
	}
}

func TestParseSupervisorTempDirRejectsGlobalTmp(t *testing.T) {
	tests := []struct {
		name    string
		value   string
		wantErr string
	}{
		{name: "relative", value: "relative", wantErr: "must be absolute"},
		{name: "global tmp", value: "/tmp/maestro-supervisor", wantErr: "must be disk-backed"},
		{name: "global tmp itself", value: "/tmp", wantErr: "must be disk-backed"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte("repo: owner/repo\nlocal_path: /srv/repo\nworktree_base: /srv/worktrees\nsupervisor:\n  temp_dir: " + tc.value + "\n"))
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %v, want %q", err, tc.wantErr)
			}
		})
	}
}
