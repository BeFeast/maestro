package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/befeast/maestro/internal/admissioncontrol"
	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/state"
	"github.com/befeast/maestro/internal/worker"
	"github.com/google/uuid"
)

// exitedNativeSlot persists the #1243 shape: a launched generation whose
// worker exited (session no longer running, lease released) without a
// recorded terminal marker, so NativePendingSlots still counts it.
func exitedNativeSlot(t *testing.T, dir, slot string, sess *state.Session) *worker.NativeWorkerReceipt {
	t.Helper()
	receiptDir := filepath.Join(dir, "worker-native-sessions", slot)
	if err := os.MkdirAll(receiptDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(dir, "worker-native-sessions"), 0700); err != nil {
		t.Fatal(err)
	}
	request := admissioncontrol.RegistrationRequest{Binding: admissioncontrol.Binding{GatewayScope: "fixture-gateway", NativeSessionID: uuid.NewString(), FleetID: "fixture-fleet", ProjectID: "fixture-project", RunID: "fixture-budget", Role: "repair", ExpiresAt: time.Now().Unix() + 100}, ExpectedVersion: 1}
	r := &worker.NativeWorkerReceipt{SchemaVersion: 1, ProjectID: request.ProjectID, Slot: slot, Generation: 1, IssueNumber: sess.IssueNumber, RoleRunID: uuid.NewString(), Status: "launched", LogFile: filepath.Join(dir, slot+".log"), Worktree: filepath.Join(dir, "wt", slot), Branch: slot, Backend: "claude", Request: request, Acknowledgement: &admissioncontrol.Acknowledgement{Binding: request.Binding, RegistrationVersion: 1}, ProcessLeaseUnit: "fixture-" + slot + ".service", ProcessLeaseManager: "system", PID: 4040}
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(receiptDir, "generation-1.json"), b, 0600); err != nil {
		t.Fatal(err)
	}
	sess.WorkerGeneration = 1
	sess.NativeSessionID = request.NativeSessionID
	sess.NativeRoleRunID = r.RoleRunID
	sess.NativeRole = "repair"
	sess.NativeReceiptDir = receiptDir
	sess.Worktree, sess.Branch = r.Worktree, r.Branch
	return r
}

// recordNativeTerminal writes the terminal marker the exit seal records once
// the exact OS termination is proven and the binding is settled.
func recordNativeTerminal(t *testing.T, dir string, r *worker.NativeWorkerReceipt) {
	t.Helper()
	b, err := json.Marshal(map[string]any{"role_run_id": r.RoleRunID, "native_session_id": r.Request.NativeSessionID, "generation": r.Generation, "unit": r.ProcessLeaseUnit, "manager": r.ProcessLeaseManager})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "worker-native-sessions", r.Slot, "generation-1.json.terminated"), b, 0600); err != nil {
		t.Fatal(err)
	}
}

// #1243 live shape: with no worker running anywhere, two exited but unsealed
// native generations closed the ceiling every cycle and the daemon never said
// why. The stall diagnostic names both slots and their session projections;
// once the exit seal records one termination the ceiling reopens, and a
// ceiling that still holds a genuinely running worker is not a stall.
func TestFleetCeilingStallDiagnosticNamesExitedNativeSlots(t *testing.T) {
	dir := t.TempDir()
	finished := time.Now().Add(-3 * time.Hour).UTC()
	st := state.NewState()
	done := &state.Session{IssueNumber: 17, Status: state.StatusDone, PRNumber: 20, FinishedAt: &finished}
	dead := &state.Session{IssueNumber: 7, Status: state.StatusDead, NativeRegistrationHold: "previous_outcome_unknown", FinishedAt: &finished}
	st.Sessions["ret-halenote-1"] = done
	st.Sessions["ret-halenote-2"] = dead
	sealedLater := exitedNativeSlot(t, dir, "ret-halenote-1", done)
	exitedNativeSlot(t, dir, "ret-halenote-2", dead)
	if err := state.Save(dir, st); err != nil {
		t.Fatal(err)
	}
	limiter := newFleetSpawnLimiter(&fleetConcurrencyTestStore{settings: config.FleetConcurrencySettings{MinLiveWorkers: 2, MaxLiveWorkers: 2}})
	limiter.RegisterProject(dir, "owner/halenote", time.Minute)

	if !limiter.CeilingReached() {
		t.Fatal("exited, unrecorded native generations must keep counting until their termination is recorded")
	}
	detail, stalled := limiter.CeilingStallDiagnostic()
	if !stalled {
		t.Fatalf("no worker runs in the fleet, but the ceiling is not reported as a stall: %q", detail)
	}
	for _, want := range []string{
		"live=2 min=2 max=2 fleet_running=0",
		"owner/halenote/ret-halenote-1: native launch not recorded terminal (session done)",
		"owner/halenote/ret-halenote-2: native launch not recorded terminal (session dead, hold previous_outcome_unknown)",
	} {
		if !strings.Contains(detail, want) {
			t.Fatalf("diagnostic %q lacks %q", detail, want)
		}
	}

	// The exit seal records ret-halenote-1's termination: capacity reopens.
	recordNativeTerminal(t, dir, sealedLater)
	if limiter.CeilingReached() {
		t.Fatal("ceiling still counts a generation whose termination is recorded")
	}
	if detail, stalled := limiter.CeilingStallDiagnostic(); detail != "" || stalled {
		t.Fatalf("diagnostic with free capacity: %q %v", detail, stalled)
	}

	// A running worker elsewhere fills the ceiling again; that is capacity in
	// use, not a stall.
	other := t.TempDir()
	saveFleetRunningState(t, other, 1)
	limiter.RegisterProject(other, "owner/product", time.Minute)
	if !limiter.CeilingReached() {
		t.Fatal("running worker not counted")
	}
	detail, stalled = limiter.CeilingStallDiagnostic()
	if stalled || !strings.Contains(detail, "fleet_running=1") || !strings.Contains(detail, "owner/product/slot-1: running") {
		t.Fatalf("detail=%q stalled=%v", detail, stalled)
	}
}

// An in-flight reservation is named as such and, with no running worker, is
// part of a stall report rather than hidden.
func TestFleetCeilingStallDiagnosticListsReservations(t *testing.T) {
	dir := t.TempDir()
	saveFleetRunningState(t, dir, 0)
	limiter := newFleetSpawnLimiter(&fleetConcurrencyTestStore{settings: config.FleetConcurrencySettings{MaxLiveWorkers: 1}})
	limiter.RegisterProject(dir, "owner/product", time.Minute)
	if detail, _ := limiter.CeilingStallDiagnostic(); detail != "" {
		t.Fatalf("diagnostic with free capacity: %q", detail)
	}
	_, release, ok := limiter.Reserve(dir)
	if !ok {
		t.Fatal("reservation failed")
	}
	defer release()
	detail, stalled := limiter.CeilingStallDiagnostic()
	if !stalled || !strings.Contains(detail, "owner/product/(uncommitted): spawn reservation") {
		t.Fatalf("detail=%q stalled=%v", detail, stalled)
	}
	unlimited := newFleetSpawnLimiter(&fleetConcurrencyTestStore{settings: config.FleetConcurrencySettings{}})
	unlimited.RegisterProject(dir, "owner/product", time.Minute)
	if detail, stalled := unlimited.CeilingStallDiagnostic(); detail != "" || stalled {
		t.Fatalf("disabled ceiling produced a diagnostic: %q %v", detail, stalled)
	}
}
