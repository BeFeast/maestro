package supervisor

import (
	"testing"
	"time"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/github"
	"github.com/befeast/maestro/internal/state"
	"github.com/befeast/maestro/internal/worker"
)

// idleLLM is an LLM decision that agrees with an idle (action=none) guardrail.
// The short-circuit means it should never be consulted on a safe, mutation-free
// cycle — the tests below assert calls==0 — but it is a valid response so the
// always_consult_llm escape hatch has something to parse when it does call.
func idleLLM() *fakeLLM {
	return &fakeLLM{output: `{
  "summary": "Nothing to do; the project is idle.",
  "recommended_action": "none",
  "target": {"issue": 0, "pr": 0, "session": ""},
  "risk": "safe",
  "confidence": 0.8,
  "reasons": ["no eligible issue", "no worker running"],
  "requires_approval": false
}`}
}

// TestDecideWithLLM_SafeIdleDecision_SkipsLLM pins #837 AC: an idle project
// (deterministic action=none, risk=safe, no planned mutations) completes the
// supervise cycle without invoking the supervisor backend at all.
func TestDecideWithLLM_SafeIdleDecision_SkipsLLM(t *testing.T) {
	cfg := testConfig(t)
	cfg.IssueLabels = []string{"maestro-ready"}
	// No open issues, no sessions → deterministic action=none / risk=safe / no mutations.
	reader := &fakeReader{}
	llm := idleLLM()

	decision, err := testLLMEngine(cfg, reader, llm).Decide(state.NewState())
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if llm.calls != 0 {
		t.Fatalf("LLM calls = %d, want 0 (safe idle decision must short-circuit the backend)", llm.calls)
	}
	if decision.RecommendedAction != ActionNone {
		t.Fatalf("action = %q, want %q", decision.RecommendedAction, ActionNone)
	}
	if decision.Risk != RiskSafe {
		t.Fatalf("risk = %q, want %q", decision.Risk, RiskSafe)
	}
	if len(decision.Mutations) != 0 {
		t.Fatalf("mutations = %#v, want none", decision.Mutations)
	}
}

// TestDecideWithLLM_MonitorOpenPR_SkipsLLM pins the monitor_open_pr arm of the
// AC: a safe, mutation-free "watch this PR" decision does not spend an LLM call.
func TestDecideWithLLM_MonitorOpenPR_SkipsLLM(t *testing.T) {
	cfg := testConfig(t)
	cfg.ReviewGate = "none"
	reader := &fakeReader{
		prs:        []github.PR{{Number: 100, HeadRefName: "feat/ci-pending", Mergeable: "MERGEABLE"}},
		ciStatuses: map[int]string{100: "pending"},
	}
	st := state.NewState()
	st.Sessions["slot-1"] = &state.Session{
		IssueNumber: 201,
		Status:      state.StatusPROpen,
		Branch:      "feat/ci-pending",
		PRNumber:    100,
		StartedAt:   time.Now().UTC().Add(-10 * time.Minute),
	}
	llm := idleLLM()

	decision, err := testLLMEngine(cfg, reader, llm).Decide(st)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if llm.calls != 0 {
		t.Fatalf("LLM calls = %d, want 0 (monitor_open_pr is safe + mutation-free)", llm.calls)
	}
	if decision.RecommendedAction != ActionMonitorOpenPR {
		t.Fatalf("action = %q, want %q", decision.RecommendedAction, ActionMonitorOpenPR)
	}
}

// TestDecideWithLLM_WaitForRunningWorker_SkipsLLM pins the wait_* arm of the AC:
// waiting on an in-flight worker is a safe, mutation-free decision → no LLM call.
func TestDecideWithLLM_WaitForRunningWorker_SkipsLLM(t *testing.T) {
	cfg := testConfig(t)
	reader := &fakeReader{prs: []github.PR{{Number: 55, HeadRefName: "untracked-open-pr", State: "OPEN"}}}
	st := state.NewState()
	st.Sessions["slot-1"] = &state.Session{
		IssueNumber: 42,
		IssueTitle:  "work in progress",
		Status:      state.StatusRunning,
		PID:         12345,
		StartedAt:   time.Now().UTC(),
	}
	llm := idleLLM()

	decision, err := testLLMEngine(cfg, reader, llm).Decide(st)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if llm.calls != 0 {
		t.Fatalf("LLM calls = %d, want 0 (wait_for_running_worker is safe + mutation-free)", llm.calls)
	}
	if decision.RecommendedAction != ActionWaitForRunningWorker {
		t.Fatalf("action = %q, want %q", decision.RecommendedAction, ActionWaitForRunningWorker)
	}
}

func TestDecideWithLLM_TokenBudgetExceeded_SkipsLLM(t *testing.T) {
	cfg := testConfig(t)
	st := state.NewState()
	st.Sessions["slot-budget"] = &state.Session{
		IssueNumber:       906,
		Status:            state.StatusFailed,
		WorkerOutcome:     worker.TokenBudgetExceededOutcome,
		TokensUsedAttempt: 80_000,
		StartedAt:         time.Now().UTC(),
	}
	llm := idleLLM()

	decision, err := testLLMEngine(cfg, &fakeReader{}, llm).Decide(st)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if llm.calls != 0 {
		t.Fatalf("LLM calls = %d, want 0 for deterministic token budget state", llm.calls)
	}
	if decision.RecommendedAction != ActionNone || decision.Target == nil || decision.Target.Session != "slot-budget" {
		t.Fatalf("decision = %+v, want action=none targeting slot-budget", decision)
	}
}

// TestDecideWithLLM_SpawnCandidate_CallsLLM pins the counterpart AC: a mutating
// spawn_worker candidate still goes through the LLM path exactly as before.
func TestDecideWithLLM_SpawnCandidate_CallsLLM(t *testing.T) {
	cfg := testConfig(t)
	cfg.IssueLabels = []string{"maestro-ready"}
	reader := &fakeReader{issues: []github.Issue{testIssue(42, "ready work", "maestro-ready")}}
	llm := validSpawnLLM()

	decision, err := testLLMEngine(cfg, reader, llm).Decide(state.NewState())
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if llm.calls != 1 {
		t.Fatalf("LLM calls = %d, want 1 (spawn_worker is mutating; the LLM must be consulted)", llm.calls)
	}
	if decision.RecommendedAction != ActionSpawnWorker {
		t.Fatalf("action = %q, want %q", decision.RecommendedAction, ActionSpawnWorker)
	}
}

// TestDecideWithLLM_LabelIssueReadyWithMutations_SkipsLLM: risk=safe label
// mutations must not block the control loop on a hung supervisor backend —
// the guardrail already owns the decision.
func TestDecideWithLLM_LabelIssueReadyWithMutations_SkipsLLM(t *testing.T) {
	cfg := testConfig(t)
	cfg.IssueLabels = []string{"maestro-ready"}
	cfg.Supervisor.SafeActions = []string{config.SupervisorActionAddReadyLabel}
	reader := &fakeReader{issues: []github.Issue{testIssue(308, "implement supervisor")}}
	llm := idleLLM()

	decision, err := testLLMEngine(cfg, reader, llm).Decide(state.NewState())
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if llm.calls != 0 {
		t.Fatalf("LLM calls = %d, want 0 (safe label mutations short-circuit)", llm.calls)
	}
	if decision.RecommendedAction != ActionLabelIssueReady {
		t.Fatalf("action = %q, want %q", decision.RecommendedAction, ActionLabelIssueReady)
	}
	if len(decision.Mutations) == 0 {
		t.Fatalf("mutations = %#v, want a planned add_ready_label mutation", decision.Mutations)
	}
}

// TestDecideWithLLM_AlwaysConsultLLM_IdleStillCallsLLM pins the escape hatch:
// supervisor.always_consult_llm=true restores the pre-#837 behavior of calling
// the backend on every enabled cycle, even for a safe, mutation-free idle state.
func TestDecideWithLLM_AlwaysConsultLLM_IdleStillCallsLLM(t *testing.T) {
	cfg := testConfig(t)
	cfg.IssueLabels = []string{"maestro-ready"}
	cfg.Supervisor.AlwaysConsultLLM = true
	reader := &fakeReader{}
	llm := idleLLM()

	decision, err := testLLMEngine(cfg, reader, llm).Decide(state.NewState())
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if llm.calls != 1 {
		t.Fatalf("LLM calls = %d, want 1 (always_consult_llm restores the old always-call path)", llm.calls)
	}
	if decision.RecommendedAction != ActionNone {
		t.Fatalf("action = %q, want %q", decision.RecommendedAction, ActionNone)
	}
}

// TestRunOnce_SafeIdleDecision_RecordedWithoutBackendCall proves the AC
// end-to-end through the daemon seam: RunOnce builds its own real backend
// client (Supervisor.Enabled, no injected LLM), so a real Complete would fail —
// no backend is configured in the test config. A clean, recorded action=none
// decision is therefore proof the LLM path was short-circuited.
func TestRunOnce_SafeIdleDecision_RecordedWithoutBackendCall(t *testing.T) {
	cfg := testConfig(t)
	cfg.Supervisor.Enabled = true
	cfg.IssueLabels = []string{"maestro-ready"}
	reader := &fakeReader{}
	if err := state.Save(cfg.StateDir, state.NewState()); err != nil {
		t.Fatalf("seed state: %v", err)
	}

	decision, err := RunOnce(cfg, reader)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if decision.RecommendedAction != ActionNone {
		t.Fatalf("action = %q, want %q", decision.RecommendedAction, ActionNone)
	}

	st, err := state.Load(cfg.StateDir)
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	if latest := st.LatestSupervisorDecision(); latest == nil || latest.RecommendedAction != ActionNone {
		t.Fatalf("latest recorded decision = %#v, want an action=none decision", latest)
	}
}
