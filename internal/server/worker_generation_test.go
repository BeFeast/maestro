package server

import (
	"testing"
	"time"

	"github.com/befeast/maestro/internal/state"
)

func TestFleetWorkerCarriesCanonicalWorkerGeneration(t *testing.T) {
	sess := &state.Session{
		IssueNumber:      963,
		Status:           state.StatusRunning,
		StartedAt:        time.Now().UTC(),
		WorkerGeneration: 8,
	}
	info := makeSessionInfo("owner/repo", "sup-963", sess)
	worker := makeFleetWorkerState(fleetProjectState{Name: "repo"}, info)
	if info.WorkerGeneration != 8 || worker.WorkerGeneration != 8 {
		t.Fatalf("generation projection lost: info=%d fleet=%d", info.WorkerGeneration, worker.WorkerGeneration)
	}
}
