package supervisor

import (
	"strings"
	"testing"
	"time"

	"github.com/befeast/maestro/internal/github"
	"github.com/befeast/maestro/internal/state"
	"github.com/befeast/maestro/internal/worker"
)

func TestDecide_OpenPRDoesNotStarveSpareSlotBacklog(t *testing.T) {
	cfg := testConfig(t)
	cfg.MaxParallel = 4
	cfg.IssueLabels = []string{"maestro-ready"}
	reader := &fakeReader{
		prs: []github.PR{{Number: 88, HeadRefName: "feat/old", State: "OPEN", IsDraft: true}},
		issues: []github.Issue{
			testIssue(10, "already has open PR", "maestro-ready"),
			testIssue(20, "next backlog", "maestro-ready"),
		},
		openPRIssues: map[int]bool{10: true},
		ciStatuses:   map[int]string{88: "pending"},
	}
	st := state.NewState()
	st.Sessions["sup-1"] = &state.Session{
		IssueNumber: 10, IssueTitle: "old", Status: state.StatusPROpen, PRNumber: 88,
		Branch: "feat/old", StartedAt: time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC),
	}

	decision, err := testEngine(cfg, reader).Decide(st)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if decision.RecommendedAction != ActionSpawnWorker {
		t.Fatalf("action = %q, want spawn_worker (spare slots must fill backlog)", decision.RecommendedAction)
	}
	if decision.Target == nil || decision.Target.Issue != 20 {
		t.Fatalf("target = %#v, want issue #20", decision.Target)
	}
	reasons := strings.Join(decision.Reasons, "\n")
	if !strings.Contains(reasons, "Prefer backlog label/spawn") {
		t.Fatalf("reasons = %q, want spare-capacity fill rationale", reasons)
	}
}

func TestDecide_TokenBudgetDoesNotStarveSpareSlotBacklog(t *testing.T) {
	cfg := testConfig(t)
	cfg.MaxParallel = 4
	cfg.IssueLabels = []string{"maestro-ready"}
	reader := &fakeReader{
		issues: []github.Issue{testIssue(30, "next backlog", "maestro-ready")},
	}
	st := state.NewState()
	st.Sessions["sup-old"] = &state.Session{
		IssueNumber: 9, IssueTitle: "budgeted out", Status: state.StatusFailed,
		WorkerOutcome: worker.TokenBudgetExceededOutcome,
		StartedAt:     time.Date(2026, 7, 22, 11, 0, 0, 0, time.UTC),
	}

	decision, err := testEngine(cfg, reader).Decide(st)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if decision.RecommendedAction != ActionSpawnWorker {
		t.Fatalf("action = %q, want spawn_worker despite prior token-budget stop", decision.RecommendedAction)
	}
	if decision.Target == nil || decision.Target.Issue != 30 {
		t.Fatalf("target = %#v, want issue #30", decision.Target)
	}
}
