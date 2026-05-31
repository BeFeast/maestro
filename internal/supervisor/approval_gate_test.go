package supervisor

import (
	"testing"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/state"
)

// #545: the supervise approval-gate must mint approvals for the four
// operator-configured mutating verbs. Before the fix, decisionRequiresApproval
// consulted ApprovalRequiredActions (the default list, which lacks these verbs)
// and canonicalAction returned "" for them, so a merge_pr/RiskMutating decision
// was silently dropped and hands-off merge never completed. The pre-existing
// tests only asserted the decision (action + risk), never that an approval was
// minted — which is how this slipped through.

func cautiousApprovalCfg() *config.Config {
	cfg := &config.Config{}
	cfg.Supervisor.Mode = "cautious"
	cfg.Supervisor.ApprovalRequired = []string{
		config.SupervisorActionMergePR,
		config.SupervisorActionCloseIssue,
		config.SupervisorActionDeleteWorktree,
		config.SupervisorActionChangeGlobalConfig,
	}
	return cfg
}

func TestCanonicalAction_MutatingVerbsPassthrough(t *testing.T) {
	verbs := []string{
		config.SupervisorActionMergePR,
		config.SupervisorActionCloseIssue,
		config.SupervisorActionDeleteWorktree,
		config.SupervisorActionChangeGlobalConfig,
	}
	for _, v := range verbs {
		if got := canonicalAction(v); got != v {
			t.Fatalf("canonicalAction(%q) = %q, want %q", v, got, v)
		}
		// case-insensitive + trimmed, matching the other verbs' contract
		if got := canonicalAction("  " + v + "  "); got != v {
			t.Fatalf("canonicalAction(padded %q) = %q, want %q", v, got, v)
		}
	}
}

func TestDecisionRequiresApproval_GatesMutatingVerbs(t *testing.T) {
	cfg := cautiousApprovalCfg()
	verbs := []string{
		config.SupervisorActionMergePR,
		config.SupervisorActionCloseIssue,
		config.SupervisorActionDeleteWorktree,
		config.SupervisorActionChangeGlobalConfig,
	}
	for _, v := range verbs {
		d := state.SupervisorDecision{RecommendedAction: v, Risk: RiskMutating}
		if !decisionRequiresApproval(cfg, d) {
			t.Fatalf("decisionRequiresApproval(%q, mutating) = false, want true (cautious gate must mint)", v)
		}
	}
}

func TestDecisionRequiresApproval_SafeAndNoneNotGated(t *testing.T) {
	cfg := cautiousApprovalCfg()
	safe := state.SupervisorDecision{RecommendedAction: ActionMonitorOpenPR, Risk: RiskSafe}
	if decisionRequiresApproval(cfg, safe) {
		t.Fatal("RiskSafe decision must not require approval")
	}
	none := state.SupervisorDecision{RecommendedAction: ActionNone, Risk: RiskMutating}
	if decisionRequiresApproval(cfg, none) {
		t.Fatal("ActionNone must not require approval")
	}
}

func TestNewSupervisorPolicy_ApprovalRequiredFoldedIn(t *testing.T) {
	cfg := cautiousApprovalCfg()
	p := newSupervisorPolicy(cfg)
	if !p.requiresApproval(config.SupervisorActionMergePR) {
		t.Fatal("newSupervisorPolicy must mark merge_pr approval-required from ApprovalRequired")
	}
}
