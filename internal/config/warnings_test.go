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
