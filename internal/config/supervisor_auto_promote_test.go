package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// #1240: supervisor.auto_promote_ready gates default-policy ready-label
// promotion. It is off unless set, and validated against the queue policies
// that already own promotion and against a missing ready label.

func TestParse_SupervisorAutoPromoteReadyDefaultsOff(t *testing.T) {
	cfg, err := parse([]byte(`
repo: owner/repo
issue_labels: [pilot-ready]
supervisor:
  enabled: false
  safe_actions: [add_ready_label]
`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if cfg.Supervisor.AutoPromoteReady {
		t.Fatal("Supervisor.AutoPromoteReady = true, want false when the key is absent")
	}
}

func TestParseStrict_SupervisorAutoPromoteReadyAccepted(t *testing.T) {
	cfg, err := ParseStrict([]byte(`
repo: owner/repo
issue_labels: [pilot-ready]
supervisor:
  auto_promote_ready: true
`))
	if err != nil {
		t.Fatalf("ParseStrict: %v", err)
	}
	if !cfg.Supervisor.AutoPromoteReady {
		t.Fatal("Supervisor.AutoPromoteReady = false, want true")
	}
}

func TestParse_SupervisorAutoPromoteReadyValidation(t *testing.T) {
	cases := []struct {
		name string
		yaml string
		want string
	}{
		{
			name: "dynamic wave owns promotion",
			yaml: `
repo: owner/repo
issue_labels: [pilot-ready]
supervisor:
  auto_promote_ready: true
  dynamic_wave:
    enabled: true
`,
			want: "cannot be combined with supervisor.dynamic_wave",
		},
		{
			name: "ordered queue owns promotion",
			yaml: `
repo: owner/repo
issue_labels: [pilot-ready]
supervisor:
  auto_promote_ready: true
  ordered_queue:
    issues: [12]
`,
			want: "cannot be combined with supervisor.ordered_queue",
		},
		{
			name: "no ready label to promote with",
			yaml: `
repo: owner/repo
supervisor:
  auto_promote_ready: true
`,
			want: "requires supervisor.ready_label or issue_labels",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parse([]byte(tc.yaml))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("parse error = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}

func TestParse_SupervisorAutoPromoteReadyAcceptsSupervisorReadyLabel(t *testing.T) {
	cfg, err := parse([]byte(`
repo: owner/repo
supervisor:
  auto_promote_ready: true
  ready_label: maestro-ready
  ordered_queue:
    enabled: false
    issues: []
  dynamic_wave:
    enabled: false
`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !cfg.Supervisor.AutoPromoteReady {
		t.Fatal("Supervisor.AutoPromoteReady = false, want true")
	}
}

func TestLoadFrom_SupervisorPolicyFileAutoPromoteRequiresReadyLabel(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "maestro.yaml")
	policyDir := filepath.Join(dir, ".maestro")
	if err := os.Mkdir(policyDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, []byte("repo: owner/repo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	policy := []byte("supervisor:\n  auto_promote_ready: true\n")
	if err := os.WriteFile(filepath.Join(policyDir, "supervisor.yaml"), policy, 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := LoadFrom(configPath)
	if err == nil || !strings.Contains(err.Error(), "requires supervisor.ready_label or issue_labels") {
		t.Fatalf("LoadFrom error = %v, want missing ready label error", err)
	}
}
