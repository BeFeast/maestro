package orchestrator

import (
	"strings"
	"testing"
	"time"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/notify"
	"github.com/befeast/maestro/internal/state"
	"github.com/befeast/maestro/internal/worker"
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

// recordBudgetKill appends a PR-less budget stop for the issue, as the live
// dispatch path records one.
func recordBudgetKill(s *state.State, slot string, issue int, finished time.Time, ceiling, observed int) {
	at := finished
	s.Sessions[slot] = &state.Session{
		IssueNumber:              issue,
		Status:                   state.StatusFailed,
		WorkerOutcome:            worker.TokenBudgetExceededOutcome,
		TokenBudgetMaxTokens:     ceiling,
		TokenBudgetTokensAttempt: observed,
		StartedAt:                finished.Add(-2 * time.Minute),
		FinishedAt:               &at,
	}
}

// #1124 end to end: two stops at budget X hold the issue, raising the budget to
// Y > X makes it dispatchable again with no state.json edit, and two fresh stops
// at Y hold it again.
func TestTokenBudgetMill_RaisedBudgetReleasesThenReHolds(t *testing.T) {
	o := millTestOrchestrator() // worker_max_tokens = 120000
	s := state.NewState()
	now := time.Now().UTC()

	recordBudgetKill(s, "sup-1", 628, now.Add(-40*time.Minute), 120000, 157000)
	recordBudgetKill(s, "sup-2", 628, now.Add(-30*time.Minute), 120000, 158000)
	kills := s.ConsecutiveTokenBudgetKillsForIssue(628, o.cfg.WorkerMaxTokens)
	if !o.tokenBudgetMillHold(628, kills) {
		t.Fatalf("two stops at the configured budget did not hold (kills=%d)", kills)
	}

	// The operator applies the remedy the alert names, and nothing else.
	o.cfg.WorkerMaxTokens = 400000
	kills = s.ConsecutiveTokenBudgetKillsForIssue(628, o.cfg.WorkerMaxTokens)
	if kills != 0 {
		t.Fatalf("streak after the raise = %d, want 0", kills)
	}
	if o.tokenBudgetMillHold(628, kills) {
		t.Fatal("raising worker_max_tokens did not release the hold")
	}
	if _, remembered := o.tokenBudgetMillNotified[628]; remembered {
		t.Fatal("alert memory survived the release — the returning wall would hold silently")
	}

	// The issue walls into the new ceiling too: one stop is not enough, two are.
	recordBudgetKill(s, "sup-3", 628, now.Add(-10*time.Minute), 400000, 430000)
	kills = s.ConsecutiveTokenBudgetKillsForIssue(628, o.cfg.WorkerMaxTokens)
	if kills != 1 || o.tokenBudgetMillHold(628, kills) {
		t.Fatalf("held after a single stop at the new ceiling (kills=%d)", kills)
	}
	recordBudgetKill(s, "sup-4", 628, now.Add(-5*time.Minute), 400000, 440000)
	kills = s.ConsecutiveTokenBudgetKillsForIssue(628, o.cfg.WorkerMaxTokens)
	if kills != 2 {
		t.Fatalf("streak at the new ceiling = %d, want 2", kills)
	}
	if !o.tokenBudgetMillHold(628, kills) {
		t.Fatal("did not hold again after two stops at the raised budget")
	}
}

// A budget stop must record the ceiling it hit, otherwise a later raise has
// nothing to compare against.
func TestMarkTokenBudgetExceeded_RecordsCeiling(t *testing.T) {
	o := millTestOrchestrator()
	sess := &state.Session{IssueNumber: 628, Status: state.StatusRunning, StartedAt: time.Now().UTC()}
	o.markTokenBudgetExceeded("sup-1", sess, worker.TokenBudgetMarker{
		Outcome:        worker.TokenBudgetExceededOutcome,
		TokensObserved: 157000,
		MaxTokens:      120000,
		Measure:        worker.TokenBudgetMeasureUncached,
	}, time.Now().UTC())

	if sess.TokenBudgetMaxTokens != 120000 {
		t.Fatalf("recorded ceiling = %d, want 120000", sess.TokenBudgetMaxTokens)
	}
}

// The alert must name the escape route that actually works.
func TestTokenBudgetMillAlertBody_NamesTheEscapeRoute(t *testing.T) {
	body := tokenBudgetMillAlertBody(628, 2, 120000)
	for _, want := range []string{"worker_max_tokens is raised above 120000", "clears itself"} {
		if !strings.Contains(body, want) {
			t.Fatalf("alert body %q does not mention %q", body, want)
		}
	}
}
