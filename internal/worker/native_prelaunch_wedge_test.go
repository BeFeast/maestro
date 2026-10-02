package worker

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/befeast/maestro/internal/admissioncontrol"
	"github.com/befeast/maestro/internal/aiexecution"
	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/state"
	"github.com/befeast/maestro/internal/tmuxsession"
	"github.com/google/uuid"
)

// cleanExitWithoutMarker reproduces the state the live respawns started from
// (ret-halenote-1 2026-10-02 05:57:20, ret-halenote-2 05:46:20): generation 1
// was launched and exited cleanly, the scratch-lease reconciler released its
// exact lease from the session without writing the native terminal marker,
// and the session projects the launched, unsealed generation with no lease.
// The slot's execution proof still describes that generation.
func cleanExitWithoutMarker(t *testing.T) (*nativeFixture, *NativeWorkerReceipt, aiexecution.FileProof) {
	t.Helper()
	f, parent, _ := registeredPrelaunchFixture(t)
	f.cfg.AIExecution = syntheticVerifiedRoutePolicy(t, f.cfg.StateDir)
	f.cfg.WorkerRuntime = isolatedRuntimeConfig(t, f.cfg.ProjectID).WorkerRuntime
	dir := nativeReceiptDir(f.cfg.StateDir, f.slot)
	logDir := state.LogDir(f.cfg.StateDir)
	if err := os.MkdirAll(logDir, 0700); err != nil {
		t.Fatal(err)
	}
	lease, err := workerProcessLease(f.cfg, f.slot, 1)
	if err != nil {
		t.Fatal(err)
	}
	parent.Status = "launched"
	parent.PID = 4040
	parent.ProcessLeaseUnit, parent.ProcessLeaseManager = lease.Unit, lease.Manager
	parent.LogFile = filepath.Join(logDir, f.slot+".log")
	if err := writeNativeWorkerReceipt(dir, parent); err != nil {
		t.Fatal(err)
	}
	proof := workerExecutionProof{Version: 2, Policy: f.cfg.AIExecution, RuntimeKey: f.slot, Worktree: parent.Worktree, ProcessLeaseUnit: parent.ProcessLeaseUnit, Spec: aiexecution.LaunchSpec{ProjectID: parent.ProjectID, Role: parent.Request.Role, Registration: parent.Acknowledgement}}
	proofBytes, _ := json.Marshal(proof)
	if err := os.WriteFile(filepath.Join(f.cfg.StateDir, f.slot+"-run.sh.execution.json"), proofBytes, 0600); err != nil {
		t.Fatal(err)
	}
	pin, err := nativeProfileFromReceipt(f.cfg, parent)
	if err != nil {
		t.Fatal(err)
	}
	sess := f.st.Sessions[f.slot]
	sess.Status = state.StatusPROpen
	sess.PRNumber = 20
	sess.WorkerGeneration = 1
	sess.NativeSessionID = parent.Request.NativeSessionID
	sess.NativeRoleRunID = parent.RoleRunID
	sess.NativeRole = parent.Request.Role
	sess.NativeReceiptDir = dir
	sess.NativeRegistrationHold = ""
	sess.PID, sess.TmuxSession = 0, ""
	clearSessionProcessLease(sess)
	finished := time.Now().Add(-time.Hour).UTC()
	sess.FinishedAt = &finished
	if err := state.Save(f.cfg.StateDir, f.st); err != nil {
		t.Fatal(err)
	}
	oldSeal, oldTerminate, oldObserve := sealNativeWorker, verifyNativeWorkerTermination, observeNativeWorkerLaunch
	t.Cleanup(func() {
		sealNativeWorker = oldSeal
		verifyNativeWorkerTermination = oldTerminate
		observeNativeWorkerLaunch = oldObserve
	})
	return f, parent, pin
}

// The in-place respawn right after a clean exit must prove and record the
// projected generation's termination before it registers a successor. The old
// order (register generation 2, then StopProcess) wedged the slot with a
// registered generation-2 receipt under native_process_identity_missing.
func TestRespawnInPlaceProvesTerminationBeforeRegisteringSuccessor(t *testing.T) {
	f, parent, pin := cleanExitWithoutMarker(t)
	sess := f.st.Sessions[f.slot]
	dir := sess.NativeReceiptDir
	previousNativeGenerationOutcome = persistedNativeGenerationOutcome
	seals, verified := 0, 0
	sealNativeWorker = func(_ admissioncontrol.Client, request admissioncontrol.SealRequest) (admissioncontrol.NativeOutcome, error) {
		seals++
		if request.NativeSessionID != parent.Request.NativeSessionID {
			t.Fatal("sealed a different registration")
		}
		if verified == 0 {
			t.Fatal("sealed before the exact OS termination was observed")
		}
		return fixtureNativeOutcome(request, false), nil
	}
	verifyNativeWorkerTermination = func(got aiexecution.FileProof, nativeID, unit string) (*aiexecution.NativeProcessTermination, error) {
		verified++
		if got != pin || nativeID != parent.Request.NativeSessionID || unit != parent.ProcessLeaseUnit {
			t.Fatal("wrong original process identity")
		}
		return nativeRuntimeProof(parent, pin, true), nil
	}
	registerNativeWorker = func(_ admissioncontrol.Client, request admissioncontrol.RegistrationRequest) (admissioncontrol.Acknowledgement, error) {
		// Ordering fence: by the time generation 2 reaches the authority the
		// projected generation is sealed and its terminal marker is durable.
		if terminal, err := nativeWorkerTerminated(dir, parent); err != nil || !terminal {
			t.Fatal("successor registered before the terminal marker", err)
		}
		if r, err := readNativeWorkerReceipt(dir, 1); err != nil || r.Outcome == nil || r.NativeProcessEvidence == nil {
			t.Fatal("successor registered before the projected generation was sealed with termination evidence", err)
		}
		if saved, err := readNativeWorkerReceipt(dir, 2); err != nil || saved.Status != "registration_intent" || saved.Request != request {
			t.Fatal("successor registration was not durable", err)
		}
		f.registered = append(f.registered, request)
		return admissioncontrol.Acknowledgement{Binding: request.Binding, RegistrationVersion: 1}, nil
	}
	f.cfg.WorkerLaunchContext = &config.WorkerLaunchContext{Role: "repair", ParentRoleRunID: sess.NativeRoleRunID}
	err := RespawnInPlace(f.cfg, f.slot, sess, f.cfg.Repo, f.issue, "repair prompt", "claude")
	hold, ok := NativeHold(err)
	if !ok {
		t.Fatalf("expected a native hold from the synthetic verified route after registration, got %v", err)
	}
	// The synthetic policy has no controller pin, so the launch stops at the
	// execution proof. What matters is that the stop happened AFTER the
	// successor was minted on top of a proven terminal generation, not at
	// StopProcess with an unproven one.
	if hold.Code == "native_process_identity_missing" || hold.LaunchUncertain {
		t.Fatalf("respawn wedged on the missing marker: %+v", hold)
	}
	t.Logf("launch held after registration with %s", hold.Code)
	// f.registered already carries generation 1 from the fixture's own start.
	if seals != 1 || verified != 2 || len(f.registered) != 2 || f.spawned != 0 || f.stopped != 0 {
		t.Fatalf("seals=%d verified=%d registered=%d spawned=%d stopped=%d", seals, verified, len(f.registered), f.spawned, f.stopped)
	}
	if terminal, err := NativeSessionProcessTerminal(f.cfg.StateDir, f.slot, sess); err != nil || !terminal {
		t.Fatal("terminal marker not recorded", err)
	}
	next, err := readNativeWorkerReceipt(dir, 2)
	if err != nil || next.Status != "registered" || next.ParentRoleRunID != parent.RoleRunID {
		t.Fatalf("successor missing or malformed: %+v %v", next, err)
	}
	if sess.WorkerGeneration != 1 || sess.NativeRoleRunID != parent.RoleRunID || sess.PRNumber != 20 {
		t.Fatalf("projection advanced without a launch: %+v", sess)
	}
	// StopProcess on the lease-less projection now accepts the proven terminal generation.
	if err := StopProcess(f.slot, sess); err != nil {
		t.Fatal("proven terminal generation still held by StopProcess", err)
	}
}

// Without the strict verified route the lease-less termination cannot be
// proven at all: the respawn must hold BEFORE any successor identity exists
// instead of registering generation 2 and holding at StopProcess.
func TestRespawnHoldsBeforeMintingSuccessorWhenTerminationUnprovable(t *testing.T) {
	for _, kind := range []string{"in_place", "respawn"} {
		t.Run(kind, func(t *testing.T) {
			f := nativeTestFixture(t)
			if _, err := f.start(); err != nil {
				t.Fatal(err)
			}
			sess := f.st.Sessions[f.slot]
			// Clean exit, then the scratch reconciler released the exact lease
			// from the projection without the native terminal marker.
			f.live = false
			sess.PID, sess.TmuxSession = 0, ""
			clearSessionProcessLease(sess)
			sess.Status = state.StatusDead
			previousNativeGenerationOutcome = persistedNativeGenerationOutcome
			oldSeal := sealNativeWorker
			t.Cleanup(func() { sealNativeWorker = oldSeal })
			sealNativeWorker = func(admissioncontrol.Client, admissioncontrol.SealRequest) (admissioncontrol.NativeOutcome, error) {
				t.Fatal("sealed a generation whose termination cannot be observed")
				return admissioncontrol.NativeOutcome{}, nil
			}
			f.cfg.WorkerLaunchContext = nil
			var err error
			switch kind {
			case "in_place":
				err = RespawnInPlace(f.cfg, f.slot, sess, f.cfg.Repo, f.issue, "prompt", "claude")
			case "respawn":
				err = Respawn(f.cfg, f.slot, sess, f.cfg.Repo, f.issue, "prompt", "claude")
			}
			expectNativeHold(t, err, "native_process_identity_missing", true)
			if _, statErr := os.Lstat(filepath.Join(nativeReceiptDir(f.cfg.StateDir, f.slot), nativeReceiptName(2))); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatal("successor registered on top of an unproven termination", statErr)
			}
			if len(f.registered) != 1 || f.spawned != 1 || f.stopped != 0 || sess.WorkerGeneration != 1 || sess.Status != state.StatusDead {
				t.Fatalf("registered=%d spawned=%d stopped=%d gen=%d status=%s", len(f.registered), f.spawned, f.stopped, sess.WorkerGeneration, sess.Status)
			}
		})
	}
}

type prelaunchWedgeFixture struct {
	f            *nativeFixture
	dir          string
	parent, next *NativeWorkerReceipt
	proof        []byte
	seals        int
	absence      int
}

// prelaunchWedge reproduces the live wedge on candidate 2: generation 1 was
// launched, exited cleanly, and (one cycle after the respawn held) was sealed
// and proven terminal by the hold path; generation 2 is a registered receipt
// (no log file, no execution proof, no lease, no claim, no pane) that the
// respawn minted before StopProcess held; the session still projects
// generation 1 under the given hold.
func prelaunchWedge(t *testing.T, hold string, status state.SessionStatus) *prelaunchWedgeFixture {
	t.Helper()
	f, parent, _ := registeredPrelaunchFixture(t)
	f.cfg.AIExecution = syntheticVerifiedRoutePolicy(t, f.cfg.StateDir)
	f.cfg.WorkerRuntime = isolatedRuntimeConfig(t, f.cfg.ProjectID).WorkerRuntime
	dir := nativeReceiptDir(f.cfg.StateDir, f.slot)
	logDir := state.LogDir(f.cfg.StateDir)
	if err := os.MkdirAll(logDir, 0700); err != nil {
		t.Fatal(err)
	}
	parent.Status = "launched"
	parent.PID = 4040
	parent.ProcessLeaseUnit = "maestro-worker-0123456789abcdef0123456789abcdef-g1.service"
	parent.ProcessLeaseManager = "system"
	parent.LogFile = filepath.Join(logDir, f.slot+".log")
	// The slot's execution proof still describes the projected generation:
	// the respawn held before rewriting it for generation 2.
	proof := workerExecutionProof{Version: 2, Policy: f.cfg.AIExecution, RuntimeKey: f.slot, Worktree: parent.Worktree, ProcessLeaseUnit: parent.ProcessLeaseUnit, Spec: aiexecution.LaunchSpec{ProjectID: parent.ProjectID, Role: parent.Request.Role, Registration: parent.Acknowledgement}}
	proofBytes, _ := json.Marshal(proof)
	if err := os.WriteFile(filepath.Join(f.cfg.StateDir, f.slot+"-run.sh.execution.json"), proofBytes, 0600); err != nil {
		t.Fatal(err)
	}
	pin, err := nativeProfileFromReceipt(f.cfg, parent)
	if err != nil {
		t.Fatal(err)
	}
	parent.NativeProcessEvidence = nativeRuntimeProof(parent, pin, true)
	seal := admissioncontrol.SealRequest{Binding: parent.Request.Binding, RegistrationVersion: parent.Acknowledgement.RegistrationVersion}
	outcome := fixtureNativeOutcome(seal, false)
	parent.OutcomeIntent, parent.Outcome = &seal, &outcome
	if err := writeNativeWorkerReceipt(dir, parent); err != nil {
		t.Fatal(err)
	}
	terminated, _ := json.Marshal(terminationFor(parent))
	if err := os.WriteFile(filepath.Join(dir, nativeReceiptName(1)+".terminated"), terminated, 0600); err != nil {
		t.Fatal(err)
	}
	next := *parent
	next.Generation = 2
	next.RoleRunID = uuid.NewString()
	next.ParentRoleRunID = parent.RoleRunID
	next.Request.NativeSessionID = uuid.NewString()
	next.Request.Role = "repair"
	ack := admissioncontrol.Acknowledgement{Binding: next.Request.Binding, RegistrationVersion: 1}
	next.Acknowledgement = &ack
	next.Status = "registered"
	next.PID = 0
	next.LogFile = ""
	next.ProcessLeaseUnit = "maestro-worker-0123456789abcdef0123456789abcdef-g2.service"
	next.OutcomeIntent, next.Outcome, next.NativeProcessEvidence = nil, nil, nil
	if err := writeNativeWorkerReceipt(dir, &next); err != nil {
		t.Fatal(err)
	}
	sess := f.st.Sessions[f.slot]
	sess.Status = status
	sess.PRNumber = 20
	sess.WorkerGeneration = 1
	sess.NativeSessionID = parent.Request.NativeSessionID
	sess.NativeRoleRunID = parent.RoleRunID
	sess.NativeRole = parent.Request.Role
	sess.NativeReceiptDir = dir
	sess.NativeRegistrationHold = hold
	sess.RetryCount, sess.UnexpectedExitRetries = 1, 1
	sess.PID, sess.TmuxSession = 0, ""
	clearSessionProcessLease(sess)
	finished := time.Now().Add(-time.Hour).UTC()
	sess.FinishedAt = &finished
	if status == state.StatusDead {
		retryAt := time.Now().Add(-time.Minute).UTC()
		sess.NextRetryAt = &retryAt
	}
	if err := state.Save(f.cfg.StateDir, f.st); err != nil {
		t.Fatal(err)
	}
	fx := &prelaunchWedgeFixture{f: f, dir: dir, parent: parent, next: &next, proof: proofBytes}
	oldSeal, oldAbsence, oldObserve := sealNativeWorker, verifyNativeWorkerPrelaunchAbsence, observeNativeWorkerLaunch
	t.Cleanup(func() {
		sealNativeWorker = oldSeal
		verifyNativeWorkerPrelaunchAbsence = oldAbsence
		observeNativeWorkerLaunch = oldObserve
	})
	sealNativeWorker = func(_ admissioncontrol.Client, request admissioncontrol.SealRequest) (admissioncontrol.NativeOutcome, error) {
		fx.seals++
		if request.NativeSessionID != next.Request.NativeSessionID || request.RegistrationVersion != 1 {
			t.Fatal("sealed a different registration")
		}
		return fixtureNativeOutcome(request, false), nil
	}
	verifyNativeWorkerPrelaunchAbsence = func(_ aiexecution.FileProof, projectID, nativeID, unit string) error {
		fx.absence++
		if projectID != next.ProjectID || nativeID != next.Request.NativeSessionID || unit != next.ProcessLeaseUnit {
			t.Fatal("absence checked for a different native identity")
		}
		return nil
	}
	observeNativeWorkerLaunch = func(aiexecution.FileProof, string, string, string) (*aiexecution.NativeProcessTermination, int, error) {
		t.Fatal("wedge reconciliation observed a live launch")
		return nil, 0, nil
	}
	return fx
}

func TestNativeRuntimeReconcileAbandonsRegisteredSuccessorOfPrelaunchWedge(t *testing.T) {
	for _, tc := range []struct {
		name   string
		hold   string
		status state.SessionStatus
	}{
		{"retry_exhausted_identity_missing", "native_process_identity_missing", state.StatusRetryExhausted},
		{"dead_retry_identity_missing", "native_process_identity_missing", state.StatusDead},
		{"dead_retry_previous_outcome_unknown", "previous_outcome_unknown", state.StatusDead},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := prelaunchWedge(t, tc.hold, tc.status)
			f := fx.f
			before, _ := os.ReadFile(filepath.Join(fx.dir, nativeReceiptName(2)))
			sess := f.st.Sessions[f.slot]
			retryAt := sess.NextRetryAt
			if err := ReconcileNativeWorkerRuntime(f.cfg, f.st, f.slot); err != nil {
				t.Fatal(err)
			}
			if fx.seals != 1 || fx.absence != 1 || f.spawned != 0 || f.stopped != 0 {
				t.Fatalf("seals=%d absence=%d spawned=%d stopped=%d", fx.seals, fx.absence, f.spawned, f.stopped)
			}
			if _, err := os.Lstat(filepath.Join(fx.dir, nativeReceiptName(2))); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("abandoned receipt still occupies generation 2", err)
			}
			archived, _ := os.ReadFile(filepath.Join(fx.dir, "launch-abandoned-g2-1.receipt.json"))
			if !reflect.DeepEqual(archived, before) {
				t.Fatal("receipt was not archived byte-for-byte")
			}
			// A registered receipt never had an execution proof; nothing is
			// invented for the archive and the slot's own proof is untouched.
			if _, err := os.Lstat(filepath.Join(fx.dir, "launch-abandoned-g2-1.execution.json")); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("an execution proof was archived for a receipt that never had one", err)
			}
			if current, err := os.ReadFile(filepath.Join(f.cfg.StateDir, f.slot+"-run.sh.execution.json")); err != nil || !reflect.DeepEqual(current, fx.proof) {
				t.Fatal("slot execution proof changed", err)
			}
			if _, err := os.Lstat(filepath.Join(fx.dir, "launch-abandoned-g2-1.intent.json")); err != nil {
				t.Fatal("seal intent sidecar missing", err)
			}
			recordBytes, err := os.ReadFile(filepath.Join(fx.dir, "launch-abandoned-g2-1.outcome.json"))
			if err != nil {
				t.Fatal(err)
			}
			var record NativeLaunchAbandonmentRecord
			if err := json.Unmarshal(recordBytes, &record); err != nil {
				t.Fatal(err)
			}
			sum := sha256.Sum256(before)
			if record.Generation != 2 || record.PreviousStatus != "registered" || record.ClearedHold != tc.hold || record.ParentRoleRunID != fx.parent.RoleRunID ||
				record.NativeSessionID != fx.next.Request.NativeSessionID || record.ReceiptSHA256 != hex.EncodeToString(sum[:]) || record.ExecutionProofSHA256 != "" ||
				record.Outcome.PhysicalAttempts != 0 || record.Outcome.Outcome != "no_dispatch" || !record.Outcome.NextGenerationAllowed || record.WorkerExecHold != "" {
				t.Fatalf("abandonment record incomplete: %+v", record)
			}
			// Parent generation and projection history are untouched; only the hold is gone.
			parent, err := readNativeWorkerReceipt(fx.dir, 1)
			if err != nil || parent.Status != "launched" || parent.Outcome == nil || parent.NativeProcessEvidence == nil {
				t.Fatal("parent generation changed", err)
			}
			if terminal, err := nativeWorkerTerminated(fx.dir, parent); err != nil || !terminal {
				t.Fatal("parent terminal marker changed", err)
			}
			if sess.NativeRegistrationHold != "" || sess.WorkerGeneration != 1 || sess.NativeRoleRunID != fx.parent.RoleRunID || sess.RetryCount != 1 ||
				sess.UnexpectedExitRetries != 1 || sess.Status != tc.status || sess.PRNumber != 20 || !reflect.DeepEqual(sess.NextRetryAt, retryAt) {
				t.Fatalf("projection altered beyond the hold: %+v", sess)
			}
			saved, err := state.Load(f.cfg.StateDir)
			if err != nil || saved.Sessions[f.slot].NativeRegistrationHold != "" || saved.Sessions[f.slot].RetryCount != 1 {
				t.Fatal("cleared hold not durable", err)
			}
			if pending, err := NativePendingSlots(f.cfg.StateDir, f.st.Sessions); err != nil || len(pending) != 0 {
				t.Fatal("archived registration still consumes fleet capacity", pending, err)
			}
			// A crash between the archive and the projection save replays from
			// the sealed, terminal parent alone: nothing is re-sealed.
			sess.NativeRegistrationHold = tc.hold
			if err := ReconcileNativeWorkerRuntime(f.cfg, f.st, f.slot); err != nil || sess.NativeRegistrationHold != "" || fx.seals != 1 {
				t.Fatal("archived abandonment was not replayed", err, fx.seals)
			}
			// The ordinary retry path now mints a fresh generation-2 identity.
			f.cfg.WorkerLaunchContext = &config.WorkerLaunchContext{Role: "repair", ParentRoleRunID: sess.NativeRoleRunID}
			previousNativeGenerationOutcome = persistedNativeGenerationOutcome
			registerNativeWorker = func(_ admissioncontrol.Client, request admissioncontrol.RegistrationRequest) (admissioncontrol.Acknowledgement, error) {
				return admissioncontrol.Acknowledgement{Binding: request.Binding, RegistrationVersion: 1}, nil
			}
			launch, err := prepareNativeWorker(f.cfg, sess, f.slot, "claude", workerBackendConfig(f.cfg.Model.Backends["claude"]), 2, sess.IssueNumber, sess.Worktree, sess.Branch)
			if err != nil {
				t.Fatal("slot could not relaunch after abandonment", err)
			}
			defer launch.close()
			if launch.adopt || launch.receipt.Generation != 2 || launch.receipt.Status != "registered" || launch.receipt.Request.NativeSessionID == fx.next.Request.NativeSessionID || launch.receipt.ParentRoleRunID != fx.parent.RoleRunID {
				t.Fatalf("relaunch reused abandoned identity: %+v", launch.receipt)
			}
		})
	}
}

// A wedge hold raised by the termination fence (nothing registered yet) is
// cleared once the projected generation is sealed and terminal.
func TestNativeRuntimeReconcileClearsWedgeHoldWithoutSuccessor(t *testing.T) {
	fx := prelaunchWedge(t, "native_process_identity_missing", state.StatusDead)
	f := fx.f
	if err := os.Remove(filepath.Join(fx.dir, nativeReceiptName(2))); err != nil {
		t.Fatal(err)
	}
	if err := ReconcileNativeWorkerRuntime(f.cfg, f.st, f.slot); err != nil {
		t.Fatal(err)
	}
	sess := f.st.Sessions[f.slot]
	if sess.NativeRegistrationHold != "" || fx.seals != 0 || fx.absence != 0 || sess.WorkerGeneration != 1 || sess.RetryCount != 1 || sess.Status != state.StatusDead {
		t.Fatalf("hold not cleared cleanly: %+v seals=%d", sess, fx.seals)
	}
	saved, err := state.Load(f.cfg.StateDir)
	if err != nil || saved.Sessions[f.slot].NativeRegistrationHold != "" {
		t.Fatal("cleared hold not durable", err)
	}
	// unresolved_launch without a successor keeps its existing semantics.
	sess.NativeRegistrationHold = "unresolved_launch"
	expectNativeHold(t, ReconcileNativeWorkerRuntime(f.cfg, f.st, f.slot), "native_generation_sealed", true)
}

func TestNativeRuntimeReconcileKeepsPrelaunchWedgeHeldWithoutProof(t *testing.T) {
	for _, mode := range []string{"parent_not_terminal", "parent_unsealed", "lease_active", "pane_present", "attempts_recorded", "successor_launched", "log_file_set", "parent_mismatch", "terminal_marker", "claim_present", "authority_unavailable", "unresolved_launch_sees_only_launch_intent"} {
		t.Run(mode, func(t *testing.T) {
			hold := "native_process_identity_missing"
			if mode == "unresolved_launch_sees_only_launch_intent" {
				hold = "unresolved_launch"
			}
			fx := prelaunchWedge(t, hold, state.StatusRetryExhausted)
			f := fx.f
			want, uncertain := "", true
			switch mode {
			case "parent_not_terminal":
				if err := os.Remove(filepath.Join(fx.dir, nativeReceiptName(1)+".terminated")); err != nil {
					t.Fatal(err)
				}
				want = "native_process_identity_missing"
			case "parent_unsealed":
				fx.parent.OutcomeIntent, fx.parent.Outcome = nil, nil
				if err := writeNativeWorkerReceipt(fx.dir, fx.parent); err != nil {
					t.Fatal(err)
				}
				want, uncertain = "previous_outcome_unknown", false
			case "lease_active":
				workerProcessLeaseActive = func(tmuxsession.ProcessLease) (bool, error) { return true, nil }
				want = "native_process_unknown"
			case "pane_present":
				nativeWorkerPaneAbsent = func(string) (bool, error) { return false, nil }
				want = "native_host_runtime_unknown"
			case "attempts_recorded":
				sealNativeWorker = func(_ admissioncontrol.Client, request admissioncontrol.SealRequest) (admissioncontrol.NativeOutcome, error) {
					fx.seals++
					return fixtureNativeOutcome(request, true), nil
				}
				want = "native_launch_abandonment_contradicted"
			case "successor_launched":
				fx.next.Status = "launched"
				fx.next.PID = 7
				fx.next.LogFile = fx.parent.LogFile
				if err := writeNativeWorkerReceipt(fx.dir, fx.next); err != nil {
					t.Fatal(err)
				}
				want = "native_launch_abandonment_unproven"
			case "log_file_set":
				fx.next.LogFile = fx.parent.LogFile
				if err := writeNativeWorkerReceipt(fx.dir, fx.next); err != nil {
					t.Fatal(err)
				}
				want = "native_launch_abandonment_unproven"
			case "parent_mismatch":
				fx.next.ParentRoleRunID = uuid.NewString()
				if err := writeNativeWorkerReceipt(fx.dir, fx.next); err != nil {
					t.Fatal(err)
				}
				want = "native_identity_conflict"
			case "terminal_marker":
				if err := os.WriteFile(filepath.Join(fx.dir, nativeReceiptName(2)+".terminated"), []byte("x"), 0600); err != nil {
					t.Fatal(err)
				}
				want = "native_recovery_terminal_conflict"
			case "claim_present":
				verifyNativeWorkerPrelaunchAbsence = func(aiexecution.FileProof, string, string, string) error {
					return aiexecution.Held("containment_prelaunch_claim_conflict")
				}
			case "authority_unavailable":
				sealNativeWorker = func(admissioncontrol.Client, admissioncontrol.SealRequest) (admissioncontrol.NativeOutcome, error) {
					return admissioncontrol.NativeOutcome{}, errors.New("socket down")
				}
				want = "outcome_authority_unavailable"
			case "unresolved_launch_sees_only_launch_intent":
				want = "native_launch_abandonment_unproven"
			}
			err := ReconcileNativeWorkerRuntime(f.cfg, f.st, f.slot)
			if err == nil {
				t.Fatal("unproven abandonment was reconciled")
			}
			if want != "" {
				expectNativeHold(t, err, want, uncertain)
			} else {
				var execHold *aiexecution.Hold
				if !errors.As(err, &execHold) || execHold.Code != "containment_prelaunch_claim_conflict" {
					t.Fatalf("err=%v", err)
				}
			}
			if _, statErr := os.Lstat(filepath.Join(fx.dir, nativeReceiptName(2))); statErr != nil {
				t.Fatal("held receipt was archived or removed", statErr)
			}
			if f.st.Sessions[f.slot].NativeRegistrationHold != hold || f.spawned != 0 || f.stopped != 0 {
				t.Fatal("hold cleared or process touched without proof")
			}
			if mode != "attempts_recorded" && mode != "authority_unavailable" && fx.seals != 0 {
				t.Fatal("authority sealed before local absence was proven")
			}
		})
	}
}
