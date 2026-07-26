package orchestrator

import (
	"testing"

	"github.com/befeast/maestro/internal/github"
	"github.com/befeast/maestro/internal/state"
)

// The host-resource precondition must stop dispatch before the GitHub list, so
// a held cycle also costs no API quota (#1128).
func TestStartNewWorkers_ResourceHoldBlocksBeforeGitHubList(t *testing.T) {
	cfg := cfgWithBackends("claude", "claude")
	issues := []github.Issue{makeIssue(1128, "ready issue")}
	o, started, labels := newStartWorkersOrchestrator(cfg, issues)
	listed := false
	o.listOpenIssuesFn = func([]string) ([]github.Issue, error) {
		listed = true
		return issues, nil
	}
	o.SetSpawnResourceHold(func() (bool, string) { return true, "/tmp free=2.0GiB below the spawn floor 4.0GiB" })

	s := state.NewState()
	o.startNewWorkers(s, 5)

	if listed {
		t.Fatal("startNewWorkers listed GitHub issues while dispatch was held on host resources")
	}
	if len(*started) != 0 {
		t.Fatalf("started = %v, want no workers", *started)
	}
	if len(*labels) != 0 {
		t.Fatalf("labels = %v, want no label churn for a held cycle", *labels)
	}
}

// The hold is a throughput pause, not a freeze: it burns no retry budget, needs
// no operator action, and the very next cycle dispatches the same issue.
func TestStartNewWorkers_ResourceHoldSpendsNoRetryBudgetAndSelfClears(t *testing.T) {
	cfg := cfgWithBackends("claude", "claude")
	issues := []github.Issue{makeIssue(1128, "ready issue")}
	o, started, _ := newStartWorkersOrchestrator(cfg, issues)
	held := true
	o.SetSpawnResourceHold(func() (bool, string) { return held, "host tmpfs below the spawn floor" })

	s := state.NewState()
	o.startNewWorkers(s, 5)
	if len(s.Sessions) != 0 || s.FailedAttemptsForIssue(1128) != 0 {
		t.Fatalf("state after a held cycle = %+v, want the issue's retry budget untouched", s)
	}
	if s.PauseActive() || s.DrainActive() {
		t.Fatal("the resource hold persisted an operator-visible pause/drain state")
	}

	held = false
	o.startNewWorkers(s, 5)
	if len(*started) != 1 || (*started)[0] != 1128 {
		t.Fatalf("started = %v, want the held issue to dispatch as soon as the host recovers", *started)
	}
}
