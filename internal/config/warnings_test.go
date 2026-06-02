package config

import (
	"strings"
	"testing"
)

// #425 (sup-98): a hands-off project that gates merge_pr in
// approval_required will mint an approval for every green PR and stop
// merging until a human acts. Warnings() must surface this loud at load
// time so the operator notices without grepping the daemon journal.
func TestConfig_Warnings_HandsOffMergeApprovalRequired(t *testing.T) {
	cfg := &Config{ReviewGate: "greptile"}
	cfg.Supervisor.ApprovalRequired = []string{
		SupervisorActionMergePR,
		SupervisorActionCloseIssue,
		SupervisorActionDeleteWorktree,
	}
	warnings := cfg.Warnings()
	if len(warnings) == 0 {
		t.Fatalf("Warnings() = nil, want at least one warning for hands-off + merge_pr in approval_required")
	}
	found := false
	for _, msg := range warnings {
		if strings.Contains(msg, "merge_pr") && strings.Contains(msg, "approval_required") && strings.Contains(msg, "review_gate=greptile") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("Warnings() = %v, want a message naming merge_pr, approval_required, and review_gate", warnings)
	}
}

func TestConfig_Warnings_NoApprovalRequired(t *testing.T) {
	cfg := &Config{ReviewGate: "greptile"}
	if warnings := cfg.Warnings(); len(warnings) != 0 {
		t.Fatalf("Warnings() = %v, want none when merge_pr is not in approval_required", warnings)
	}
}

// delete_worktree is approval_required but does not gate the green-PR
// completion path, so it must not trigger the hands-off warning.
func TestConfig_Warnings_NonMergeVerbDoesNotWarn(t *testing.T) {
	cfg := &Config{ReviewGate: "greptile"}
	cfg.Supervisor.ApprovalRequired = []string{SupervisorActionDeleteWorktree, SupervisorActionChangeGlobalConfig}
	if warnings := cfg.Warnings(); len(warnings) != 0 {
		t.Fatalf("Warnings() = %v, want none for non-merge-gating verbs", warnings)
	}
}

// Nil config must be safe to call (callers iterate freshly-loaded configs).
func TestConfig_Warnings_NilSafe(t *testing.T) {
	var cfg *Config
	if warnings := cfg.Warnings(); warnings != nil {
		t.Fatalf("Warnings() on nil cfg = %v, want nil", warnings)
	}
}

// #427: when a project has multiple backends but routing.mode is manual (or
// empty) and no per-role backend is set, backend selection is pinned by
// model:* labels / default — not by task content. Warn at config load so
// operators do not mistake "model:codex on every issue" for task-based routing.
func TestConfig_Warnings_ManualRoutingWithLabelPinning(t *testing.T) {
	cfg := &Config{
		Model: ModelConfig{
			Default: "codex",
			Backends: map[string]BackendDef{
				"codex":  {Cmd: "codex"},
				"claude": {Cmd: "claude"},
			},
		},
		Routing: RoutingConfig{Mode: "manual"},
	}
	warnings := cfg.Warnings()
	found := false
	for _, msg := range warnings {
		if strings.Contains(msg, "routing.mode") && strings.Contains(msg, "label") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("Warnings() = %v, want a warning naming routing.mode and label pinning for the #427 misconfiguration", warnings)
	}
}

// #427: an empty routing block (the actual scribe-service shape from the bug
// report) must trigger the warning — manual is the implicit default.
func TestConfig_Warnings_EmptyRoutingTreatedAsManual(t *testing.T) {
	cfg := &Config{
		Model: ModelConfig{
			Default: "codex",
			Backends: map[string]BackendDef{
				"codex":  {Cmd: "codex"},
				"claude": {Cmd: "claude"},
			},
		},
	}
	warnings := cfg.Warnings()
	found := false
	for _, msg := range warnings {
		if strings.Contains(msg, "routing.mode") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("Warnings() = %v, want a warning when routing block is absent and multiple backends exist", warnings)
	}
}

// Single-backend projects have nothing to route — warning must stay silent.
func TestConfig_Warnings_SingleBackendNoWarning(t *testing.T) {
	cfg := &Config{
		Model: ModelConfig{
			Default: "codex",
			Backends: map[string]BackendDef{
				"codex": {Cmd: "codex"},
			},
		},
		Routing: RoutingConfig{Mode: "manual"},
	}
	for _, msg := range cfg.Warnings() {
		if strings.Contains(msg, "routing.mode") {
			t.Fatalf("Warnings() = %v, single-backend setup should not warn about routing", cfg.Warnings())
		}
	}
}

// Auto-routing opts the operator into task-based selection — no warning.
func TestConfig_Warnings_AutoRoutingNoWarning(t *testing.T) {
	cfg := &Config{
		Model: ModelConfig{
			Default: "codex",
			Backends: map[string]BackendDef{
				"codex":  {Cmd: "codex"},
				"claude": {Cmd: "claude"},
			},
		},
		Routing: RoutingConfig{Mode: "auto", RouterModel: "claude"},
	}
	for _, msg := range cfg.Warnings() {
		if strings.Contains(msg, "routing.mode") {
			t.Fatalf("Warnings() = %v, auto routing should not trigger the #427 warning", cfg.Warnings())
		}
	}
}

// Per-role backends are legitimate role-based routing — no warning.
func TestConfig_Warnings_RoleBackendsNoWarning(t *testing.T) {
	cfg := &Config{
		Model: ModelConfig{
			Default: "codex",
			Backends: map[string]BackendDef{
				"codex":  {Cmd: "codex"},
				"claude": {Cmd: "claude"},
			},
		},
		Routing: RoutingConfig{
			Mode:                  "manual",
			ImplementationBackend: "claude",
		},
	}
	for _, msg := range cfg.Warnings() {
		if strings.Contains(msg, "routing.mode") {
			t.Fatalf("Warnings() = %v, role backends should not trigger the #427 warning", cfg.Warnings())
		}
	}
}
