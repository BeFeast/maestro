package supervisor

import (
	"strings"
	"testing"
	"time"

	"github.com/befeast/maestro/internal/github"
	"github.com/befeast/maestro/internal/state"
)

// #689 regression (karaoke 2026-06-11): a merge-ready PR with a
// retry_exhausted worker makes the deterministic guardrail compute merge_pr
// (risk=mutating), while the supervisor LLM keeps answering monitor_open_pr
// or none because it sees pending review feedback. The disagreement is
// stable, so treating it as a fatal error crash-looped systemd (13+
// restarts in ~20 minutes). The cycle must instead succeed with an explicit
// no-op: neither side executes, the conflict is recorded as a
// guardrail_conflict stuck state, and the loop stays alive.
func TestDecideWithLLM_GuardrailConflictOnMutatingAction_HoldsNoOp(t *testing.T) {
	llmOutputs := map[string]string{
		"monitor_open_pr": `{
  "summary": "Keep monitoring PR #137 while review feedback is pending.",
  "recommended_action": "monitor_open_pr",
  "target": {"issue": 130, "pr": 137, "session": "karaoke-1"},
  "risk": "safe",
  "confidence": 0.8,
  "reasons": ["unresolved review feedback on the PR"],
  "requires_approval": false
}`,
		"none": `{
  "summary": "No action while review feedback is pending.",
  "recommended_action": "none",
  "risk": "safe",
  "confidence": 0.7,
  "reasons": ["unresolved review feedback on the PR"],
  "requires_approval": false
}`,
	}

	for name, output := range llmOutputs {
		t.Run(name, func(t *testing.T) {
			cfg := testConfig(t)
			cfg.ReviewGate = "none"
			reader := &fakeReader{
				prs:        []github.PR{{Number: 137, HeadRefName: "feat/standoff", Mergeable: "MERGEABLE"}},
				ciStatuses: map[int]string{137: "success"},
			}
			st := state.NewState()
			st.Sessions["karaoke-1"] = &state.Session{
				IssueNumber: 130,
				IssueTitle:  "standoff PR",
				Status:      state.StatusRetryExhausted,
				Branch:      "feat/standoff",
				PRNumber:    137,
				StartedAt:   time.Now().UTC().Add(-time.Hour),
			}

			decision, err := testLLMEngine(cfg, reader, &fakeLLM{output: output}).Decide(st)
			if err != nil {
				t.Fatalf("Decide: %v (stable disagreement must not fail the cycle — #689)", err)
			}
			if decision.RecommendedAction != ActionNone {
				t.Fatalf("action = %q, want %q (mutating guardrail side must not execute without LLM agreement)", decision.RecommendedAction, ActionNone)
			}
			if decision.Risk != RiskSafe {
				t.Fatalf("risk = %q, want safe", decision.Risk)
			}
			if decision.RequiresApproval {
				t.Fatal("RequiresApproval = true, want false for the held no-op")
			}
			if len(decision.Mutations) != 0 {
				t.Fatalf("mutations = %#v, want none for the held no-op", decision.Mutations)
			}
			stuck := requireStuckState(t, decision, state.StuckGuardrailConflict)
			if stuck.Severity != SeverityWarning {
				t.Errorf("severity = %q, want warning (operator must see the standoff)", stuck.Severity)
			}
			if !strings.Contains(stuck.Summary, "disagrees with deterministic guardrail") {
				t.Errorf("summary = %q, want it to name the disagreement", stuck.Summary)
			}
			if stuck.Target == nil || stuck.Target.PR != 137 {
				t.Errorf("stuck target = %#v, want PR=137 so the operator can find the standoff", stuck.Target)
			}
		})
	}
}

// #689: a target-only disagreement is the same decision-layer class as an
// action disagreement and must also resolve instead of failing the cycle.
// The guardrail side (monitor_open_pr) is risk=safe, so the deterministic
// decision wins the tie-break and keeps its own target.
func TestDecideWithLLM_GuardrailTargetDisagreement_ResolvesToDeterministic(t *testing.T) {
	cfg := testConfig(t)
	cfg.ReviewGate = "none"
	reader := &fakeReader{
		prs:        []github.PR{{Number: 117, HeadRefName: "feat/pending", Mergeable: "MERGEABLE"}},
		ciStatuses: map[int]string{117: "pending"},
	}
	st := state.NewState()
	st.Sessions["slot-1"] = &state.Session{
		IssueNumber: 202,
		Status:      state.StatusPROpen,
		Branch:      "feat/pending",
		PRNumber:    117,
		StartedAt:   time.Now().UTC().Add(-10 * time.Minute),
	}
	llm := &fakeLLM{output: `{
  "summary": "Monitor a different PR.",
  "recommended_action": "monitor_open_pr",
  "target": {"pr": 999},
  "risk": "safe",
  "confidence": 0.8,
  "reasons": ["LLM picked the wrong target"],
  "requires_approval": false
}`}

	decision, err := testLLMEngine(cfg, reader, llm).Decide(st)
	if err != nil {
		t.Fatalf("Decide: %v (target disagreement must not fail the cycle — #689)", err)
	}
	if decision.RecommendedAction != ActionMonitorOpenPR {
		t.Fatalf("action = %q, want deterministic %q", decision.RecommendedAction, ActionMonitorOpenPR)
	}
	if decision.Target == nil || decision.Target.PR != 117 {
		t.Fatalf("target = %#v, want deterministic PR=117", decision.Target)
	}
	requireStuckState(t, decision, state.StuckGuardrailConflict)
}

// validateLLMDecision must keep returning the typed conflict error for the
// guardrail comparisons and a plain error for policy violations — the
// resolver only catches the former; a disallowed action stays a hard error.
func TestValidateLLMDecision_ConflictErrorTyping(t *testing.T) {
	policy := newSupervisorPolicy(testConfig(t))
	deterministic := state.SupervisorDecision{
		RecommendedAction: ActionMonitorOpenPR,
		Risk:              RiskSafe,
		Target:            &state.SupervisorTarget{PR: 117},
	}
	llm := LLMDecision{
		Summary:           "monitor",
		RecommendedAction: ActionNone,
		Risk:              RiskSafe,
		Confidence:        0.8,
		Reasons:           []string{"r"},
	}

	_, err := validateLLMDecision(llm, deterministic, policy)
	if _, ok := err.(*guardrailConflictError); !ok {
		t.Fatalf("action disagreement error = %T (%v), want *guardrailConflictError", err, err)
	}

	llm.RecommendedAction = "delete_repo"
	_, err = validateLLMDecision(llm, deterministic, policy)
	if err == nil {
		t.Fatal("disallowed action must stay an error")
	}
	if _, ok := err.(*guardrailConflictError); ok {
		t.Fatalf("policy violation must NOT be a guardrail conflict: %v", err)
	}
}
