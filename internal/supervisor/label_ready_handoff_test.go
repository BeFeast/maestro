package supervisor

import (
	"testing"

	"github.com/befeast/maestro/internal/approver"
	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/github"
	"github.com/befeast/maestro/internal/state"
)

// #736 reproduction: in cautious+LLM mode the deterministic guardrail plans a
// SAFE add_ready_label mutation (add_ready_label ∈ safe_actions) and computes
// risk=safe, but the supervisor LLM is allowed to RAISE the headline risk to
// approval_gated. Before the fix RunOnce keyed the safe-mutation apply on
// decision.Risk == RiskSafe, so the inflated risk dropped the mutation, AND
// the approval fallback refused to mint because label_issue_ready was not in
// the executor registry — the ready label was never applied and the
// orchestrator starved. The safe mutation must STILL be applied (recorded
// succeeded, no approval minted).
func TestRunOnce_SafeReadyLabelAppliedDespiteLLMRiskInflation(t *testing.T) {
	cfg := testConfig(t)
	cfg.IssueLabels = []string{"maestro-ready"}
	enableDynamicWave(cfg)
	// Operator whitelisted the ready label as a safe action ...
	cfg.Supervisor.SafeActions = []string{config.SupervisorActionAddReadyLabel}
	// ... and did NOT gate it behind approval_required (only delete_worktree).
	cfg.Supervisor.ApprovalRequired = []string{config.SupervisorActionDeleteWorktree}
	// Force the LLM path so we can still prove inflation cannot block the safe
	// mutation (hands-off short-circuits risk=safe by default).
	cfg.Supervisor.AlwaysConsultLLM = true

	reader := &fakeReader{issues: []github.Issue{testIssue(266, "auth handoff")}}

	// LLM accepts the guardrail action/target but inflates the risk from
	// safe -> approval_gated (and must mark requires_approval to satisfy the
	// policy, since label_issue_ready is approval-required by default).
	llm := &fakeLLM{output: `{
  "summary": "Gate the ready label behind approval out of caution.",
  "recommended_action": "label_issue_ready",
  "target": {"issue": 266, "pr": 0, "session": ""},
  "risk": "approval_gated",
  "confidence": 0.7,
  "reasons": ["being cautious about labeling"],
  "requires_approval": true
}`}

	st := state.NewState()
	decision, err := testLLMEngine(cfg, reader, llm).Decide(st)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}

	// Sanity: this is the exact regression shape — label_issue_ready carrying
	// the planned safe mutation, but with an LLM-inflated headline risk.
	if decision.RecommendedAction != ActionLabelIssueReady {
		t.Fatalf("action = %q, want %q", decision.RecommendedAction, ActionLabelIssueReady)
	}
	if decision.Risk != RiskApprovalGated {
		t.Fatalf("risk = %q, want %q (LLM must have inflated the headline risk)", decision.Risk, RiskApprovalGated)
	}
	if len(decision.Mutations) != 1 || decision.Mutations[0].Type != MutationAddReadyLabel {
		t.Fatalf("mutations = %#v, want one planned add_ready_label", decision.Mutations)
	}

	// RunOnce's side-effect stage must apply the safe mutation directly.
	applyOrMintDecision(cfg, st, reader, &decision)

	if decision.Status != DecisionStatusSucceeded {
		t.Fatalf("status = %q, want %q (safe mutation must be applied, not refused)", decision.Status, DecisionStatusSucceeded)
	}
	if decision.Mode != ModeSafeActions {
		t.Fatalf("mode = %q, want %q", decision.Mode, ModeSafeActions)
	}
	if got, want := joinLabels(reader.addedLabels), "#266:maestro-ready"; got != want {
		t.Fatalf("added labels = %q, want %q — ready label never applied (orchestrator starves)", got, want)
	}
	if len(st.Approvals) != 0 {
		t.Fatalf("approvals = %d, want 0 — the safe mutation must not ALSO mint/refuse an approval", len(st.Approvals))
	}
	if decision.ApprovalID != "" {
		t.Fatalf("ApprovalID = %q, want empty (no approval for an applied safe mutation)", decision.ApprovalID)
	}
}

// #736 acceptance criterion 3: when the operator DOES gate the ready-label
// verb behind approval_required (and does not whitelist add_ready_label as a
// safe action), the supervisor must NOT auto-apply — it mints a pending
// approval, and the verb is now in the executor registry so the mint is not
// refused ("not in the executor registry").
func TestRunOnce_OperatorGatedReadyLabelMintsApproval(t *testing.T) {
	cfg := testConfig(t)
	cfg.IssueLabels = []string{"maestro-ready"}
	enableDynamicWave(cfg)
	// add_ready_label is NOT a safe action; the operator gated it instead.
	cfg.Supervisor.ApprovalRequired = []string{config.SupervisorActionAddReadyLabel}

	reader := &fakeReader{issues: []github.Issue{testIssue(266, "auth handoff")}}

	llm := &fakeLLM{output: `{
  "summary": "Request approval to label issue #266 ready.",
  "recommended_action": "label_issue_ready",
  "target": {"issue": 266, "pr": 0, "session": ""},
  "risk": "approval_gated",
  "confidence": 0.7,
  "reasons": ["operator gated the ready label"],
  "requires_approval": true
}`}

	st := state.NewState()
	decision, err := testLLMEngine(cfg, reader, llm).Decide(st)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if decision.RecommendedAction != ActionLabelIssueReady {
		t.Fatalf("action = %q, want %q", decision.RecommendedAction, ActionLabelIssueReady)
	}
	if len(decision.Mutations) != 0 {
		t.Fatalf("mutations = %#v, want none (add_ready_label is not a safe action)", decision.Mutations)
	}

	applyOrMintDecision(cfg, st, reader, &decision)

	if len(reader.addedLabels) != 0 {
		t.Fatalf("added labels = %#v, want none (operator-gated verb must not auto-apply)", reader.addedLabels)
	}
	if len(st.Approvals) != 1 {
		t.Fatalf("approvals = %d, want 1 pending approval (mint, not refuse)", len(st.Approvals))
	}
	a := st.Approvals[0]
	if a.Action != ActionLabelIssueReady {
		t.Fatalf("approval action = %q, want %q", a.Action, ActionLabelIssueReady)
	}
	if a.Status != state.ApprovalStatusPending {
		t.Fatalf("approval status = %q, want %q", a.Status, state.ApprovalStatusPending)
	}
	if a.Target == nil || a.Target.Issue != 266 {
		t.Fatalf("approval target = %#v, want issue 266", a.Target)
	}
	if decision.ApprovalID != a.ID {
		t.Fatalf("decision.ApprovalID = %q, want %q", decision.ApprovalID, a.ID)
	}
	// The minted verb must be executable — the regression was the executor
	// having no case for it. Pin the registry contract here too.
	if !approver.IsKnownApprovalAction(a.Action) {
		t.Fatalf("minted approval action %q is not in the executor registry %v — mint would be refused (#736)", a.Action, approver.KnownApprovalActionList())
	}
}

func joinLabels(labels []string) string {
	out := ""
	for i, l := range labels {
		if i > 0 {
			out += ","
		}
		out += l
	}
	return out
}
