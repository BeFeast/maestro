package supervisor

import (
	"strings"
	"testing"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/github"
	"github.com/befeast/maestro/internal/state"
)

// Helpers ---------------------------------------------------------------

func enableHandoffPlanner(cfg *config.Config) {
	on := true
	enableDynamicWave(cfg)
	cfg.Supervisor.HandoffPlanner.Enabled = &on
	cfg.Supervisor.HandoffPlanner.SourceIssueLabels = []string{"design-handoff", "epic"}
}

// epicIssue returns an open issue that the handoff planner should pick as
// the parent epic by either title prefix or label.
func epicIssue(number int) github.Issue {
	return testIssue(number, "Epic: Scribe design redesign")
}

func handoffLabeledIssue(number int, title string) github.Issue {
	return testIssue(number, title, "design-handoff")
}

// Test: dynamic wave eligible=0 + open handoff epic => actionable warning + open_child_issue ----

func TestDecide_HandoffPlannerRecommendsOpenChildIssueWhenQueueExhausted(t *testing.T) {
	cfg := testConfig(t)
	cfg.IssueLabels = []string{"maestro-ready"}
	enableHandoffPlanner(cfg)
	// Only an epic remains open; no concrete runnable issues.
	reader := &fakeReader{issues: []github.Issue{epicIssue(146)}}

	decision, err := testEngine(cfg, reader).Decide(state.NewState())
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if decision.RecommendedAction != ActionOpenChildIssue {
		t.Fatalf("action = %q, want %q", decision.RecommendedAction, ActionOpenChildIssue)
	}
	if decision.Risk != RiskApprovalGated {
		t.Errorf("risk = %q, want approval_gated", decision.Risk)
	}
	if !decision.RequiresApproval {
		t.Errorf("RequiresApproval = false, want true")
	}
	if decision.Target == nil || decision.Target.Issue != 146 {
		t.Fatalf("target = %#v, want issue 146", decision.Target)
	}
	requireStuckState(t, decision, state.StuckHandoffEpicNeedsChild)
}

// Test: ordered_queue_exhausted gets promoted to warning when handoff epic is open
func TestDecide_OrderedQueueExhaustedPromotedToWarningWithHandoffEpic(t *testing.T) {
	cfg := testConfig(t)
	cfg.IssueLabels = []string{"maestro-ready"}
	enableHandoffPlanner(cfg)
	// Mission-parent epic at #146 plus one wontfix-skipped issue so the
	// queue-exhausted finding fires alongside the planner stuck-state.
	excluded := testIssue(99, "old work", "wontfix")
	reader := &fakeReader{issues: []github.Issue{epicIssue(146), excluded}}

	decision, err := testEngine(cfg, reader).Decide(state.NewState())
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	stuck := requireStuckState(t, decision, "ordered_queue_exhausted")
	if stuck.Severity != SeverityWarning {
		t.Fatalf("severity = %q, want %q", stuck.Severity, SeverityWarning)
	}
	if !stuck.SupervisorCanAct {
		t.Error("SupervisorCanAct = false, want true (planner can open child issue)")
	}
}

// Test: handoff planner disabled keeps the legacy info severity.
func TestDecide_OrderedQueueExhaustedStaysInfoWhenHandoffPlannerDisabled(t *testing.T) {
	cfg := testConfig(t)
	cfg.IssueLabels = []string{"maestro-ready"}
	enableDynamicWave(cfg)
	excluded := testIssue(99, "old work", "wontfix")
	reader := &fakeReader{issues: []github.Issue{epicIssue(146), excluded}}

	decision, err := testEngine(cfg, reader).Decide(state.NewState())
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	stuck := requireStuckState(t, decision, "ordered_queue_exhausted")
	if stuck.Severity != SeverityInfo {
		t.Fatalf("severity = %q, want %q (legacy behaviour unchanged)", stuck.Severity, SeverityInfo)
	}
	if decision.RecommendedAction != ActionNone {
		t.Fatalf("action = %q, want %q (planner inactive)", decision.RecommendedAction, ActionNone)
	}
}

// Test: handoff planner picks the open handoff-labeled issue, not a runnable child.
func TestDecide_HandoffPlannerOnlyFiresWhenNoEligibleCandidates(t *testing.T) {
	cfg := testConfig(t)
	cfg.IssueLabels = []string{"maestro-ready"}
	enableHandoffPlanner(cfg)
	reader := &fakeReader{issues: []github.Issue{
		epicIssue(146),
		testIssue(170, "concrete work", "maestro-ready"),
	}}

	decision, err := testEngine(cfg, reader).Decide(state.NewState())
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if decision.RecommendedAction != ActionSpawnWorker {
		t.Fatalf("action = %q, want %q (eligible child should win)", decision.RecommendedAction, ActionSpawnWorker)
	}
	if decision.Target == nil || decision.Target.Issue != 170 {
		t.Fatalf("target = %#v, want issue 170", decision.Target)
	}
}

// Test: max_open_children limits planner activation.
func TestDecide_HandoffPlannerRespectsMaxOpenChildren(t *testing.T) {
	cfg := testConfig(t)
	cfg.IssueLabels = []string{"maestro-ready"}
	enableHandoffPlanner(cfg)
	cfg.Supervisor.HandoffPlanner.MaxOpenChildren = 1
	// Two non-epic issues are open; both are skipped (wontfix label),
	// but max_open_children=1 means the planner refuses to add more
	// children. Expect ActionNone (no planner recommendation).
	reader := &fakeReader{issues: []github.Issue{
		epicIssue(146),
		testIssue(170, "open child A", "wontfix"),
		testIssue(171, "open child B", "wontfix"),
	}}

	decision, err := testEngine(cfg, reader).Decide(state.NewState())
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if decision.RecommendedAction == ActionOpenChildIssue {
		t.Fatalf("action = %q, want planner NOT to fire (max_open_children=1)", decision.RecommendedAction)
	}
}

// Test: preflight failure blocks spawn_worker recommendation.
func TestDecide_PreflightFailureBlocksSpawnWorker(t *testing.T) {
	cfg := testConfig(t)
	cfg.IssueLabels = []string{"maestro-ready"}
	enableDynamicWave(cfg)
	cfg.Supervisor.PreflightCommand = "false"
	reader := &fakeReader{issues: []github.Issue{testIssue(42, "ready work", "maestro-ready")}}

	eng := testEngine(cfg, reader)
	eng.preflight = func(string) PreflightResult {
		return PreflightResult{Ok: false, Reason: "design zip missing", ExitCode: 2}
	}
	decision, err := eng.Decide(state.NewState())
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if decision.RecommendedAction != ActionPreflightFailed {
		t.Fatalf("action = %q, want %q", decision.RecommendedAction, ActionPreflightFailed)
	}
	if decision.Risk != RiskApprovalGated {
		t.Errorf("risk = %q, want approval_gated", decision.Risk)
	}
	stuck := requireStuckState(t, decision, state.StuckPreflightFailed)
	joined := strings.Join(stuck.Evidence, "\n")
	if !strings.Contains(joined, "design zip missing") {
		t.Fatalf("stuck evidence = %#v, want preflight reason", stuck.Evidence)
	}
}

// Test: preflight failure blocks open_child_issue planner.
func TestDecide_PreflightFailureBlocksHandoffPlanner(t *testing.T) {
	cfg := testConfig(t)
	cfg.IssueLabels = []string{"maestro-ready"}
	enableHandoffPlanner(cfg)
	cfg.Supervisor.HandoffPlanner.PreflightCommand = "false"
	cfg.Supervisor.HandoffPlanner.RequirePreflightBeforeCreate = true
	reader := &fakeReader{issues: []github.Issue{epicIssue(146)}}

	eng := testEngine(cfg, reader)
	eng.preflight = func(string) PreflightResult {
		return PreflightResult{Ok: false, Reason: "ssh god@10.10.0.13 unreachable", ExitCode: 1}
	}
	decision, err := eng.Decide(state.NewState())
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if decision.RecommendedAction != ActionPreflightFailed {
		t.Fatalf("action = %q, want %q", decision.RecommendedAction, ActionPreflightFailed)
	}
	requireStuckState(t, decision, state.StuckPreflightFailed)
}

// Test: race protection — closed issue not dispatched.
func TestDecide_PostMergeRaceClosedIssueNotSpawned(t *testing.T) {
	cfg := testConfig(t)
	cfg.IssueLabels = []string{"maestro-ready"}
	enableDynamicWave(cfg)
	reader := &fakeReader{
		issues:       []github.Issue{testIssue(168, "scribe redesign concrete", "maestro-ready")},
		closedIssues: map[int]bool{168: true},
	}

	decision, err := testEngine(cfg, reader).Decide(state.NewState())
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if decision.RecommendedAction == ActionSpawnWorker {
		t.Fatalf("action = %q, want NOT spawn_worker for closed issue (race)", decision.RecommendedAction)
	}
}

// Test: race protection — issue already Done in state not spawned again.
func TestDecide_AlreadyDoneIssueNotSpawned(t *testing.T) {
	cfg := testConfig(t)
	cfg.IssueLabels = []string{"maestro-ready"}
	enableDynamicWave(cfg)
	reader := &fakeReader{issues: []github.Issue{testIssue(168, "scribe", "maestro-ready")}}
	st := state.NewState()
	st.Sessions["scr-85"] = &state.Session{
		IssueNumber: 168,
		Status:      state.StatusDone,
	}

	decision, err := testEngine(cfg, reader).Decide(st)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if decision.RecommendedAction == ActionSpawnWorker {
		t.Fatalf("action = %q, want NOT spawn_worker (issue already Done in state)", decision.RecommendedAction)
	}
}

// Test: open_child_issue is in default allowed + approval-required action list.
func TestDefaultAllowedActions_IncludesOpenChildIssue(t *testing.T) {
	seen := map[string]bool{}
	for _, a := range defaultAllowedActions() {
		seen[a] = true
	}
	if !seen[ActionOpenChildIssue] {
		t.Errorf("defaultAllowedActions missing %q", ActionOpenChildIssue)
	}
	if !seen[ActionPreflightFailed] {
		t.Errorf("defaultAllowedActions missing %q", ActionPreflightFailed)
	}
	seenApproval := map[string]bool{}
	for _, a := range defaultApprovalRequiredActions() {
		seenApproval[a] = true
	}
	if !seenApproval[ActionOpenChildIssue] {
		t.Errorf("defaultApprovalRequiredActions missing %q", ActionOpenChildIssue)
	}
	for _, action := range []string{ActionSpawnWorker, ActionSpawnRepairWorker, ActionSpawnReviewRepair} {
		if seenApproval[action] {
			t.Errorf("defaultApprovalRequiredActions unexpectedly gates mechanical action %q", action)
		}
	}
}

// Test: canonicalAction maps "create_issue" / "create_child_issue" → open_child_issue.
func TestCanonicalAction_AliasesForOpenChildIssue(t *testing.T) {
	cases := []string{"open_child_issue", "create_issue", "create_child_issue"}
	for _, in := range cases {
		if got := canonicalAction(in); got != ActionOpenChildIssue {
			t.Errorf("canonicalAction(%q) = %q, want %q", in, got, ActionOpenChildIssue)
		}
	}
}
