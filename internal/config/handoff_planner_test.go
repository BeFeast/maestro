package config

import (
	"os"
	"path/filepath"
	"testing"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "maestro.yaml")
	if err := os.WriteFile(path, []byte(body), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
	return path
}

func TestSupervisorHandoffPlanner_DefaultsAndActive(t *testing.T) {
	path := writeConfig(t, `repo: owner/repo
supervisor:
  handoff_planner:
    enabled: true
    issue_template: .maestro/templates/design-handoff-child.md
    parse_sections:
      - "## Issue Wave"
      - "## Route Replacement Map"
    preflight_command: /home/x/.maestro/bin/preflight.sh
    require_preflight_before_create: true
`)
	cfg, err := LoadFrom(path)
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}

	hp := cfg.Supervisor.HandoffPlanner
	if !hp.Active() {
		t.Fatalf("Active() = false, want true")
	}
	if hp.IssueTemplate != ".maestro/templates/design-handoff-child.md" {
		t.Errorf("IssueTemplate = %q", hp.IssueTemplate)
	}
	if len(hp.ParseSections) != 2 {
		t.Errorf("ParseSections = %#v", hp.ParseSections)
	}
	if !hp.RequirePreflightBeforeCreate {
		t.Errorf("RequirePreflightBeforeCreate = false, want true")
	}
	got := hp.EffectiveSourceLabels()
	if len(got) != 2 || got[0] != "epic" || got[1] != "design-handoff" {
		t.Errorf("EffectiveSourceLabels = %#v, want [epic design-handoff]", got)
	}
}

func TestSupervisorHandoffPlanner_DisabledByDefault(t *testing.T) {
	path := writeConfig(t, "repo: owner/repo\n")
	cfg, err := LoadFrom(path)
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	if cfg.Supervisor.HandoffPlanner.Active() {
		t.Errorf("Active() = true, want false (no enabled config)")
	}
}

func TestSupervisorPreflightCommand_TrimmedAndStored(t *testing.T) {
	path := writeConfig(t, `repo: owner/repo
supervisor:
  preflight_command: "  /opt/maestro/bin/preflight.sh  "
`)
	cfg, err := LoadFrom(path)
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	if got := cfg.Supervisor.PreflightCommand; got != "/opt/maestro/bin/preflight.sh" {
		t.Fatalf("PreflightCommand = %q, want trimmed", got)
	}
}

func TestSupervisorCompletionGates_IssueRequiresLiveVerification(t *testing.T) {
	gates := SupervisorCompletionGatesConfig{
		RequiredLabels:    []string{"needs-visual-verification", "ui"},
		BodyMarkers:       []string{"## Live Visual Verification"},
		VerificationLabel: "awaiting-verification",
	}
	if !gates.Active() {
		t.Fatalf("Active() = false, want true")
	}
	cases := []struct {
		name   string
		labels []string
		body   string
		want   bool
	}{
		{name: "label match", labels: []string{"bug", "UI"}, body: "", want: true},
		{name: "body marker match", labels: []string{"bug"}, body: "Please run\n## Live Visual Verification\nbefore closing.", want: true},
		{name: "verification label", labels: []string{"awaiting-verification"}, body: "", want: true},
		{name: "no match", labels: []string{"bug"}, body: "ordinary issue", want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := gates.IssueRequiresLiveVerification(tc.labels, tc.body)
			if got != tc.want {
				t.Fatalf("got=%v want=%v", got, tc.want)
			}
		})
	}
}

func TestSupervisorCompletionGates_InactiveWhenEmpty(t *testing.T) {
	gates := SupervisorCompletionGatesConfig{}
	if gates.Active() {
		t.Fatalf("Active() = true, want false")
	}
	if gates.IssueRequiresLiveVerification([]string{"foo"}, "bar") {
		t.Fatalf("IssueRequiresLiveVerification returned true on inactive gates")
	}
}

func TestSupervisorDefaults_IncludeHandoffActions(t *testing.T) {
	path := writeConfig(t, "repo: owner/repo\n")
	cfg, err := LoadFrom(path)
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	seen := map[string]bool{}
	for _, a := range cfg.Supervisor.AllowedActions {
		seen[a] = true
	}
	if !seen["open_child_issue"] {
		t.Errorf("AllowedActions missing open_child_issue: %#v", cfg.Supervisor.AllowedActions)
	}
	if !seen["preflight_failed"] {
		t.Errorf("AllowedActions missing preflight_failed: %#v", cfg.Supervisor.AllowedActions)
	}
	approval := map[string]bool{}
	for _, a := range cfg.Supervisor.ApprovalRequiredActions {
		approval[a] = true
	}
	if !approval["open_child_issue"] {
		t.Errorf("ApprovalRequiredActions missing open_child_issue: %#v", cfg.Supervisor.ApprovalRequiredActions)
	}
	for _, action := range []string{"spawn_worker", "spawn_repair_worker", "spawn_review_repair"} {
		if approval[action] {
			t.Errorf("mechanical action %q must be autonomous by default: %#v", action, cfg.Supervisor.ApprovalRequiredActions)
		}
	}
}
