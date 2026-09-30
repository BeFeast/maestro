package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/befeast/maestro/internal/admissioncontrol"
	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/state"
	"github.com/befeast/maestro/internal/worker"
	"github.com/google/uuid"
)

func TestNativeWorkerOccupancySurvivesRestartWithoutMaskingFloor(t *testing.T) {
	dir := t.TempDir()
	saveFleetRunningState(t, dir, 0)
	slot := "slot-1"
	receiptDir := filepath.Join(dir, "worker-native-sessions", slot)
	if err := os.MkdirAll(receiptDir, 0700); err != nil {
		t.Fatal(err)
	}
	request := admissioncontrol.RegistrationRequest{Binding: admissioncontrol.Binding{GatewayScope: "fixture-gateway", NativeSessionID: uuid.NewString(), FleetID: "fixture-fleet", ProjectID: "fixture-project", RunID: "fixture-budget", Role: "planner", ExpiresAt: time.Now().Unix() + 100}, ExpectedVersion: 1}
	r := &worker.NativeWorkerReceipt{SchemaVersion: 1, ProjectID: request.ProjectID, Slot: slot, Generation: 1, IssueNumber: 1207, RoleRunID: uuid.NewString(), Status: "launch_intent", LogFile: filepath.Join(dir, "fixture.log"), Request: request, Acknowledgement: &admissioncontrol.Acknowledgement{Binding: request.Binding, RegistrationVersion: 1}, ProcessLeaseUnit: "fixture.scope", ProcessLeaseManager: "system"}
	write := func() {
		t.Helper()
		b, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(receiptDir, "generation-1.json"), b, 0600); err != nil {
			t.Fatal(err)
		}
	}
	write()
	store := &fleetConcurrencyTestStore{settings: config.FleetConcurrencySettings{MinLiveWorkers: 1, MaxLiveWorkers: 1}}
	limiter := newFleetSpawnLimiter(store)
	limiter.RegisterStateDir(dir)
	if _, _, ok := limiter.Reserve(dir); ok {
		t.Fatal("unknown launched process lost capacity across restart")
	}
	live, _, _, below, err := limiter.FloorStatus()
	if err != nil || live != 0 || !below {
		t.Fatalf("unknown launch masked floor: %d %v %v", live, below, err)
	}
	saveFleetRunningState(t, dir, 1)
	occupied, err := limiter.workerOccupancyLocked(true)
	if err != nil || len(occupied) != 1 {
		t.Fatalf("receipt+session double counted: %v %v", occupied, err)
	}
	live, _, _, below, err = limiter.FloorStatus()
	if err != nil || live != 1 || below {
		t.Fatal("actual live session not counted")
	}
	if err := state.Update(dir, func(st *state.State) error { st.Sessions = map[string]*state.Session{}; return nil }); err != nil {
		t.Fatal(err)
	}
	r.Status = "registered"
	write()
	_, release, ok := limiter.Reserve(dir)
	if !ok {
		t.Fatal("registration-only receipt consumed process capacity")
	}
	release()
	// Even a terminal-looking state row cannot erase an unresolved OS launch.
	r.Status = "launch_intent"
	write()
	st := state.NewState()
	st.Sessions[slot] = &state.Session{IssueNumber: 1207, Status: state.StatusDone, WorkerGeneration: 5, NativeRoleRunID: "newer"}
	if err := state.Save(dir, st); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := limiter.Reserve(dir); ok {
		t.Fatal("stale state erased launch uncertainty")
	}
}
