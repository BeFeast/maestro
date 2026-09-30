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
	"github.com/befeast/maestro/internal/tmuxsession"
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

func TestNativeWorkerHeldPhaseReleasesOnlyProvenOSTerminalCapacity(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(bin, 0700); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{"claude": "#!/bin/sh\nexit 91\n", "tmux": "#!/bin/sh\nexit 0\n", "systemctl": "#!/bin/sh\ncase \"$NATIVE_FIXTURE_STATE\" in unknown) exit 2;; *) printf '%s\\n' \"$NATIVE_FIXTURE_STATE\";; esac\n"} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(body), 0700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("NATIVE_FIXTURE_STATE", "active")
	uid := uint32(os.Getuid())
	cfg := &config.Config{ProjectID: "fixture-project", StateDir: dir, Model: config.ModelConfig{Default: "claude", Backends: map[string]config.BackendDef{"claude": {Cmd: "claude"}}}, WorkerNativeSessionRegistration: &config.NativeSessionRegistrationConfig{ControlSocket: filepath.Join(dir, "control.sock"), AuthorityUID: &uid, ExpectedPolicyVersion: 1, FleetID: "fixture-fleet", GatewayScope: "fixture-gateway", BudgetRunID: "fixture-budget", TTLSeconds: 100}}
	slot := "slot-1"
	receiptDir := filepath.Join(dir, "worker-native-sessions", slot)
	if err := os.MkdirAll(receiptDir, 0700); err != nil {
		t.Fatal(err)
	}
	request := admissioncontrol.RegistrationRequest{Binding: admissioncontrol.Binding{GatewayScope: "fixture-gateway", NativeSessionID: uuid.NewString(), FleetID: "fixture-fleet", ProjectID: "fixture-project", RunID: "fixture-budget", Role: "planner", ExpiresAt: time.Now().Unix() + 100}, ExpectedVersion: 1}
	lease, err := tmuxsession.WorkerProcessLease(cfg.ProjectID, slot, 1)
	if err != nil {
		t.Fatal(err)
	}
	lease.Manager = tmuxsession.ProcessLeaseManagerUser
	r := &worker.NativeWorkerReceipt{SchemaVersion: 1, ProjectID: cfg.ProjectID, Slot: slot, Generation: 1, IssueNumber: 1207, RoleRunID: uuid.NewString(), Status: "launched", LogFile: filepath.Join(dir, "fixture.log"), Worktree: filepath.Join(dir, "worktree"), Branch: "fixture", Backend: "claude", Request: request, Acknowledgement: &admissioncontrol.Acknowledgement{Binding: request.Binding, RegistrationVersion: 1}, ProcessLeaseUnit: lease.Unit, ProcessLeaseManager: lease.Manager, PID: 4242}
	b, _ := json.Marshal(r)
	if err := os.WriteFile(filepath.Join(receiptDir, "generation-1.json"), b, 0600); err != nil {
		t.Fatal(err)
	}
	sess := &state.Session{IssueNumber: 1207, Status: state.StatusRunning, WorkerGeneration: 1, NativeSessionID: request.NativeSessionID, NativeRoleRunID: r.RoleRunID, NativeRole: "planner", NativeReceiptDir: receiptDir, Worktree: r.Worktree, Branch: r.Branch, Backend: "claude", ProcessLeaseUnit: lease.Unit, ProcessLeaseManager: lease.Manager, PID: 4242, Phase: state.PhaseAdvisor}
	st := state.NewState()
	st.Sessions[slot] = sess
	err = worker.StartPhase(cfg, sess, slot, "accepted planner artifacts", "claude")
	hold, ok := worker.NativeHold(err)
	if !ok || hold.Code != "previous_outcome_unknown" {
		t.Fatalf("phase error=%v", err)
	}
	if err := state.Save(dir, st); err != nil {
		t.Fatal(err)
	}
	limiter := newFleetSpawnLimiter(&fleetConcurrencyTestStore{settings: config.FleetConcurrencySettings{MinLiveWorkers: 1, MaxLiveWorkers: 1}})
	limiter.RegisterStateDir(dir)
	if err := worker.ReconcileNativeWorkerTermination(cfg, slot, sess); err == nil {
		t.Fatal("active process released")
	}
	if _, _, ok := limiter.Reserve(dir); ok {
		t.Fatal("active held generation yielded neighbor permit")
	}
	t.Setenv("NATIVE_FIXTURE_STATE", "unknown")
	if err := worker.ReconcileNativeWorkerTermination(cfg, slot, sess); err == nil {
		t.Fatal("unknown process released")
	}
	if _, _, ok := limiter.Reserve(dir); ok {
		t.Fatal("unknown held generation yielded neighbor permit")
	}
	t.Setenv("NATIVE_FIXTURE_STATE", "inactive")
	if err := worker.ReconcileNativeWorkerTermination(cfg, slot, sess); err != nil {
		t.Fatal(err)
	}
	// Do not save the new StatusDead: the limiter must also defeat the stale
	// on-disk StatusRunning projection using the exact terminal receipt.
	if sess.NativeRegistrationHold != "previous_outcome_unknown" {
		t.Fatal("OS terminal erased financial hold")
	}
	_, release, ok := limiter.Reserve(dir)
	if !ok {
		t.Fatal("proven OS-terminal held phase denied neighbor permit")
	}
	release()
	live, _, _, below, err := limiter.FloorStatus()
	if err != nil || live != 0 || !below {
		t.Fatalf("stale held Running masked floor: %d %v %v", live, below, err)
	}
}
