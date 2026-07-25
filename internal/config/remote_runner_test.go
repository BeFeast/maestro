package config

import (
	"strings"
	"testing"
)

func TestParseRemoteRunnerDefaultsOff(t *testing.T) {
	cfg, err := Parse([]byte("repo: owner/repo\nremote_runner:\n  target: ignored.example\n"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.RemoteRunner.Enabled {
		t.Fatal("remote runner must default off")
	}
}

func TestParseRemoteRunnerOptIn(t *testing.T) {
	cfg, err := Parse([]byte(`
repo: owner/repo
auto_rebase: false
remote_runner:
  enabled: true
  target: runner@example.internal
  repo_path: /srv/maestro/repos/repo
  worktree_base: /srv/maestro/worktrees/repo
  credentials_file: /run/user/1000/maestro/worker.env
  ssh_args: [-o, ConnectTimeout=10]
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	r := cfg.RemoteRunner
	if !r.Enabled || r.SSHCommand != "ssh" || r.MaestroCommand != "maestro" || r.BaseBranch != "main" {
		t.Fatalf("remote runner defaults = %+v", r)
	}
	if r.Target != "runner@example.internal" || r.RepoPath != "/srv/maestro/repos/repo" || r.WorktreeBase != "/srv/maestro/worktrees/repo" {
		t.Fatalf("remote runner fields = %+v", r)
	}
}

func TestParseRemoteRunnerRejectsUnsafeOrUnsupportedConfig(t *testing.T) {
	base := `
repo: owner/repo
auto_rebase: false
remote_runner:
  enabled: true
  target: runner@example.internal
  repo_path: /srv/maestro/repos/repo
  worktree_base: /srv/maestro/worktrees/repo
`
	tests := []struct {
		name string
		yaml string
		want string
	}{
		{name: "missing target", yaml: strings.Replace(base, "  target: runner@example.internal\n", "", 1), want: "remote_runner.target"},
		{name: "relative repo", yaml: strings.Replace(base, "/srv/maestro/repos/repo", "repos/repo", 1), want: "remote_runner.repo_path"},
		{name: "root worktree", yaml: strings.Replace(base, "/srv/maestro/worktrees/repo", "/", 1), want: "remote_runner.worktree_base"},
		{name: "unsafe branch", yaml: base + "  base_branch: main..old\n", want: "remote_runner.base_branch"},
		{name: "auto rebase", yaml: strings.Replace(base, "auto_rebase: false", "auto_rebase: true", 1), want: "auto_rebase: false"},
		{name: "validation contract", yaml: base + "validation_contract: true\n", want: "validation_contract"},
		{name: "phase pipeline", yaml: base + "pipeline:\n  enabled: true\n", want: "pipeline.enabled"},
		{name: "hook", yaml: base + "hooks:\n  before_run: ./scripts/setup.sh\n", want: "lifecycle or tool hooks"},
		{name: "newline ssh arg", yaml: base + "  ssh_args: [\"bad\\narg\"]\n", want: "ssh_args"},
		{name: "positional ssh arg", yaml: base + "  ssh_args: [unexpected-target]\n", want: "not a target"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse([]byte(tt.yaml))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Parse error = %v, want substring %q", err, tt.want)
			}
		})
	}
}
