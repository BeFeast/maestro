package orchestrator

import (
	"testing"

	"github.com/befeast/maestro/internal/github"
	"github.com/befeast/maestro/internal/state"
)

func TestReturnPilotProjectCapAndEmptyQueue(t *testing.T) {
	for _, repo := range []string{"BeFeast/hedroom", "BeFeast/halenote", "BeFeast/maestro"} {
		t.Run(repo, func(t *testing.T) {
			cfg := cfgWithBackends("codex", "codex")
			cfg.Repo = repo
			cfg.MaxParallel = 1
			o, started, _ := newStartWorkersOrchestrator(cfg, []github.Issue{makeIssue(1, "first"), makeIssue(2, "second")})
			st := state.NewState()
			o.startNewWorkers(st, availableSlots(cfg, st, len(st.ActiveSessions())))
			if len(*started) != 1 || len(st.ActiveSessions()) != 1 {
				t.Fatalf("one-slot project launched %d workers", len(*started))
			}
			o.startNewWorkers(st, availableSlots(cfg, st, len(st.ActiveSessions())))
			if len(*started) != 1 {
				t.Fatal("second cycle exceeded local one-worker cap")
			}

			empty, emptyStarts, _ := newStartWorkersOrchestrator(cfg, nil)
			emptyState := state.NewState()
			empty.startNewWorkers(emptyState, availableSlots(cfg, emptyState, 0))
			if len(*emptyStarts) != 0 || len(emptyState.Sessions) != 0 {
				t.Fatal("empty queue created synthetic work")
			}
		})
	}
}
