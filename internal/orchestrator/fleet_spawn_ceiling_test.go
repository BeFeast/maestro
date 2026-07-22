package orchestrator

import (
	"testing"

	"github.com/befeast/maestro/internal/github"
	"github.com/befeast/maestro/internal/state"
)

func TestStartNewWorkers_FleetSpawnCeilingBlocksBeforeGitHubList(t *testing.T) {
	cfg := cfgWithBackends("claude", "claude")
	issues := []github.Issue{makeIssue(1081, "ready issue")}
	o, started, _ := newStartWorkersOrchestrator(cfg, issues)
	listed := false
	o.listOpenIssuesFn = func(labels []string) ([]github.Issue, error) {
		listed = true
		return issues, nil
	}
	o.SetFleetSpawnCeiling(func() bool { return true })

	o.startNewWorkers(state.NewState(), 5)

	if listed {
		t.Fatal("startNewWorkers listed GitHub issues after the fleet ceiling closed")
	}
	if len(*started) != 0 {
		t.Fatalf("started = %v, want no workers", *started)
	}
}

func TestStartNewWorkers_FleetReservationCapsBatch(t *testing.T) {
	cfg := cfgWithBackends("claude", "claude")
	issues := []github.Issue{
		makeIssue(1081, "first"),
		makeIssue(1082, "second"),
		makeIssue(1083, "third"),
	}
	o, started, _ := newStartWorkersOrchestrator(cfg, issues)
	remaining := 1
	o.SetFleetSpawnReserve(func() (func(string), func(), bool) {
		if remaining == 0 {
			return nil, nil, false
		}
		remaining--
		done := false
		commit := func(string) { done = true }
		release := func() {
			if !done {
				remaining++
			}
		}
		return commit, release, true
	})

	o.startNewWorkers(state.NewState(), 5)

	if len(*started) != 1 || (*started)[0] != 1081 {
		t.Fatalf("started = %v, want only the one reserved worker", *started)
	}
}
