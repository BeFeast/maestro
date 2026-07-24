package orchestrator

import (
	"testing"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/notify"
)

func millTestOrchestrator() *Orchestrator {
	return &Orchestrator{
		cfg:      &config.Config{Repo: "owner/repo", WorkerMaxTokens: 120000},
		notifier: &notify.Notifier{},
		repo:     "owner/repo",
	}
}

// Below the streak limit dispatch proceeds; at or above it the issue is held.
func TestTokenBudgetMillHold_HoldsAtStreakLimit(t *testing.T) {
	o := millTestOrchestrator()
	if o.tokenBudgetMillHold(628, tokenBudgetKillStreakLimit-1) {
		t.Fatal("held below the streak limit")
	}
	if !o.tokenBudgetMillHold(628, tokenBudgetKillStreakLimit) {
		t.Fatal("did not hold at the streak limit")
	}
}

// The same streak length alerts once; an escalation alerts again.
func TestTokenBudgetMillHold_AlertsOncePerEscalation(t *testing.T) {
	o := millTestOrchestrator()
	o.tokenBudgetMillHold(628, 2)
	if got := o.tokenBudgetMillNotified[628]; got != 2 {
		t.Fatalf("notified = %d, want 2", got)
	}
	o.tokenBudgetMillHold(628, 2) // same streak — no new alert
	if got := o.tokenBudgetMillNotified[628]; got != 2 {
		t.Fatalf("notified = %d after repeat, want 2", got)
	}
	o.tokenBudgetMillHold(628, 3) // escalation — alerts again
	if got := o.tokenBudgetMillNotified[628]; got != 3 {
		t.Fatalf("notified = %d after escalation, want 3", got)
	}
}

// A cleared streak must clear the alert memory, so a later wall on the same
// issue alerts instead of being held silently.
func TestTokenBudgetMillHold_ClearedStreakReArms(t *testing.T) {
	o := millTestOrchestrator()
	o.tokenBudgetMillHold(628, 2)

	// A worker ended some other way: the streak is gone, dispatch resumes.
	if o.tokenBudgetMillHold(628, 0) {
		t.Fatal("held with a cleared streak")
	}
	if _, remembered := o.tokenBudgetMillNotified[628]; remembered {
		t.Fatal("alert memory survived a cleared streak — a later wall would hold the issue silently")
	}

	// The wall returns: it must hold AND alert again.
	if !o.tokenBudgetMillHold(628, 2) {
		t.Fatal("did not hold on the returning wall")
	}
	if got := o.tokenBudgetMillNotified[628]; got != 2 {
		t.Fatalf("notified = %d on the returning wall, want a fresh alert at 2", got)
	}
}

// Streaks are tracked per issue.
func TestTokenBudgetMillHold_PerIssue(t *testing.T) {
	o := millTestOrchestrator()
	o.tokenBudgetMillHold(628, 2)
	if o.tokenBudgetMillHold(629, 1) {
		t.Fatal("issue 629 held by issue 628's streak")
	}
	if got := o.tokenBudgetMillNotified[628]; got != 2 {
		t.Fatalf("issue 628 memory = %d, want 2 (unaffected by 629)", got)
	}
}
