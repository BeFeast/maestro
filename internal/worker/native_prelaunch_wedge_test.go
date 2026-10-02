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
	"github.com/gofrs/flock"
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
	// The exact termination is observed once, before the seal, and that proof
	// is the evidence recorded with the marker.
	if seals != 1 || verified != 1 || len(f.registered) != 2 || f.spawned != 0 || f.stopped != 0 {
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
	// The settlement of the projected generation is read from the receipt, not
	// stubbed: a held parent outcome must keep the wedge held.
	previousNativeGenerationOutcome = persistedNativeGenerationOutcome
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
				// The terminal parent is sealed by the hold path itself; a
				// settlement that denies a next generation keeps the
				// successor registration untouched.
				fx.parent.OutcomeIntent, fx.parent.Outcome = nil, nil
				if err := writeNativeWorkerReceipt(fx.dir, fx.parent); err != nil {
					t.Fatal(err)
				}
				sealNativeWorker = func(_ admissioncontrol.Client, request admissioncontrol.SealRequest) (admissioncontrol.NativeOutcome, error) {
					fx.seals++
					if request.NativeSessionID != fx.parent.Request.NativeSessionID {
						t.Fatal("successor sealed before the parent settlement was known")
					}
					return fixtureNativeOutcome(request, true), nil
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
			if mode != "attempts_recorded" && mode != "authority_unavailable" && mode != "parent_unsealed" && fx.seals != 0 {
				t.Fatal("authority sealed before local absence was proven")
			}
			if mode == "parent_unsealed" {
				if parent, err := readNativeWorkerReceipt(fx.dir, 1); err != nil || fx.seals != 1 || parent.Outcome == nil || parent.Outcome.NextGenerationAllowed {
					t.Fatalf("terminal unsealed parent not sealed by the hold path: seals=%d err=%v", fx.seals, err)
				}
			}
		})
	}
}

// A transient failure inside the termination fence (receipt lock contention,
// authority unavailable at the seal) must not wedge the slot: the respawn
// holds with nothing minted, and the daemon's hold reconciliation proves,
// seals and records the projected generation on a later cycle and clears the
// hold, after which the respawn mints the successor normally. Before this the
// fence left native_process_identity_missing on an UNSEALED generation that
// nothing sealed afterwards.
func TestTerminationFenceTransientFailureIsReconciledByHoldPath(t *testing.T) {
	for _, mode := range []string{"lock_contention", "authority_unavailable"} {
		t.Run(mode, func(t *testing.T) {
			f, parent, pin := cleanExitWithoutMarker(t)
			sess := f.st.Sessions[f.slot]
			dir := sess.NativeReceiptDir
			previousNativeGenerationOutcome = persistedNativeGenerationOutcome
			seals, verified := 0, 0
			authorityDown := mode == "authority_unavailable"
			sealNativeWorker = func(_ admissioncontrol.Client, request admissioncontrol.SealRequest) (admissioncontrol.NativeOutcome, error) {
				seals++
				if request.NativeSessionID != parent.Request.NativeSessionID {
					t.Fatal("sealed a different registration")
				}
				if verified == 0 {
					t.Fatal("sealed before the exact OS termination was observed")
				}
				if authorityDown {
					return admissioncontrol.NativeOutcome{}, errors.New("socket down")
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
			var lock *flock.Flock
			if mode == "lock_contention" {
				lock = flock.New(filepath.Join(dir, ".lock"))
				if ok, err := lock.TryLock(); err != nil || !ok {
					t.Fatal("could not hold the receipt lock", err)
				}
				t.Cleanup(func() { _ = lock.Unlock() })
			}
			f.cfg.WorkerLaunchContext = &config.WorkerLaunchContext{Role: "repair", ParentRoleRunID: sess.NativeRoleRunID}
			err := RespawnInPlace(f.cfg, f.slot, sess, f.cfg.Repo, f.issue, "repair prompt", "claude")
			expectNativeHold(t, err, "native_process_identity_missing", true)
			// Nothing minted, nothing marked; the registration fixture would have
			// failed the test had a successor reached the authority.
			if _, statErr := os.Lstat(filepath.Join(dir, nativeReceiptName(2))); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatal("successor registered during a transient fence failure", statErr)
			}
			if terminal, err := NativeSessionProcessTerminal(f.cfg.StateDir, f.slot, sess); err != nil || terminal {
				t.Fatal("terminal marker written without a completed seal", err)
			}
			held, err := readNativeWorkerReceipt(dir, 1)
			if err != nil || held.Outcome != nil || held.NativeProcessEvidence != nil {
				t.Fatalf("receipt advanced past the failure: %+v %v", held, err)
			}
			switch mode {
			case "lock_contention":
				if seals != 0 || verified != 0 || held.OutcomeIntent != nil {
					t.Fatalf("lock contention observed or sealed anyway: seals=%d verified=%d intent=%v", seals, verified, held.OutcomeIntent)
				}
			case "authority_unavailable":
				// The seal intent is durable so the retry re-seals the same binding.
				if seals != 1 || verified != 1 || held.OutcomeIntent == nil {
					t.Fatalf("seals=%d verified=%d intent=%v", seals, verified, held.OutcomeIntent)
				}
			}
			if sess.WorkerGeneration != 1 || sess.Status != state.StatusPROpen || sess.PRNumber != 20 || len(f.registered) != 1 || f.spawned != 0 || f.stopped != 0 {
				t.Fatalf("projection changed by a held fence: %+v registered=%d", sess, len(f.registered))
			}
			// The daemon retains the hold on the restored snapshot and routes it to
			// the runtime reconciliation on the next cycle.
			sess.NativeRegistrationHold = "native_process_identity_missing"
			if err := state.Save(f.cfg.StateDir, f.st); err != nil {
				t.Fatal(err)
			}
			// Still transient: held again, nothing persisted beyond the intent.
			err = ReconcileNativeWorkerRuntime(f.cfg, f.st, f.slot)
			if mode == "lock_contention" {
				expectNativeHold(t, err, "generation_in_progress", false)
			} else {
				expectNativeHold(t, err, "outcome_authority_unavailable", false)
			}
			if terminal, err := NativeSessionProcessTerminal(f.cfg.StateDir, f.slot, sess); err != nil || terminal || sess.NativeRegistrationHold != "native_process_identity_missing" {
				t.Fatal("hold path recorded termination without a completed seal", err)
			}
			// Transient condition gone: the hold path proves, seals, records and clears.
			if lock != nil {
				if err := lock.Unlock(); err != nil {
					t.Fatal(err)
				}
			}
			authorityDown = false
			if err := ReconcileNativeWorkerRuntime(f.cfg, f.st, f.slot); err != nil {
				t.Fatal("hold path did not reconcile once the transient failure cleared", err)
			}
			if sess.NativeRegistrationHold != "" || sess.WorkerGeneration != 1 || sess.Status != state.StatusPROpen || sess.PRNumber != 20 || sess.NativeRoleRunID != parent.RoleRunID {
				t.Fatalf("hold not cleared cleanly: %+v", sess)
			}
			if terminal, err := NativeSessionProcessTerminal(f.cfg.StateDir, f.slot, sess); err != nil || !terminal {
				t.Fatal("terminal marker not recorded by the hold path", err)
			}
			sealed, err := readNativeWorkerReceipt(dir, 1)
			if err != nil || sealed.Outcome == nil || !sealed.Outcome.NextGenerationAllowed || sealed.NativeProcessEvidence == nil || sealed.NativeProcessEvidence.LocalStatus == "launch_intent" {
				t.Fatalf("projected generation not sealed with termination evidence: %+v %v", sealed, err)
			}
			if saved, err := state.Load(f.cfg.StateDir); err != nil || saved.Sessions[f.slot].NativeRegistrationHold != "" {
				t.Fatal("cleared hold not durable", err)
			}
			if _, statErr := os.Lstat(filepath.Join(dir, nativeReceiptName(2))); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatal("hold path minted a successor", statErr)
			}
			// The ordinary respawn now mints generation 2 on top of the proven
			// terminal generation and stops at the synthetic route's execution
			// proof, as in TestRespawnInPlaceProvesTerminationBeforeRegisteringSuccessor.
			registerNativeWorker = func(_ admissioncontrol.Client, request admissioncontrol.RegistrationRequest) (admissioncontrol.Acknowledgement, error) {
				f.registered = append(f.registered, request)
				return admissioncontrol.Acknowledgement{Binding: request.Binding, RegistrationVersion: 1}, nil
			}
			err = RespawnInPlace(f.cfg, f.slot, sess, f.cfg.Repo, f.issue, "repair prompt", "claude")
			hold, ok := NativeHold(err)
			if !ok || hold.Code == "native_process_identity_missing" || hold.LaunchUncertain {
				t.Fatalf("respawn after reconciliation held with %+v (%v)", hold, err)
			}
			if next, err := readNativeWorkerReceipt(dir, 2); err != nil || next.Status != "registered" || next.ParentRoleRunID != parent.RoleRunID {
				t.Fatalf("successor missing or malformed: %+v %v", next, err)
			}
			// lock_contention: observed and sealed once, by the successful hold
			// path. authority_unavailable: the fence and the first hold-path
			// attempt each observed and lost the seal reply before the third
			// attempt sealed. The respawn itself re-sealed nothing.
			wantSeals := 1
			if mode == "authority_unavailable" {
				wantSeals = 3
			}
			if seals != wantSeals || verified != wantSeals || len(f.registered) != 2 {
				t.Fatalf("seals=%d verified=%d registered=%d want %d", seals, verified, len(f.registered), wantSeals)
			}
		})
	}
}

// Fail-closed: a wedge hold on an unsealed generation whose termination cannot
// be proven stays held with nothing sealed, persisted or marked.
func TestNativeRuntimeReconcileKeepsUnsealedWedgeHeldWithoutTerminationProof(t *testing.T) {
	for _, mode := range []string{"lease_active", "pane_present", "proof_unverified", "verified_route_off"} {
		t.Run(mode, func(t *testing.T) {
			f, parent, _ := cleanExitWithoutMarker(t)
			sess := f.st.Sessions[f.slot]
			dir := sess.NativeReceiptDir
			previousNativeGenerationOutcome = persistedNativeGenerationOutcome
			sealNativeWorker = func(admissioncontrol.Client, admissioncontrol.SealRequest) (admissioncontrol.NativeOutcome, error) {
				t.Fatal("sealed a generation whose termination was not proven")
				return admissioncontrol.NativeOutcome{}, nil
			}
			verifyNativeWorkerTermination = func(aiexecution.FileProof, string, string) (*aiexecution.NativeProcessTermination, error) {
				return nil, errors.New("journal unavailable")
			}
			switch mode {
			case "lease_active":
				workerProcessLeaseActive = func(tmuxsession.ProcessLease) (bool, error) { return true, nil }
			case "pane_present":
				nativeWorkerPaneAbsent = func(string) (bool, error) { return false, nil }
			case "verified_route_off":
				f.cfg.AIExecution.RequireVerifiedRoute = false
			}
			sess.NativeRegistrationHold = "native_process_identity_missing"
			sess.Status = state.StatusDead
			if err := state.Save(f.cfg.StateDir, f.st); err != nil {
				t.Fatal(err)
			}
			err := ReconcileNativeWorkerRuntime(f.cfg, f.st, f.slot)
			if mode == "verified_route_off" {
				if err != nil {
					t.Fatal("runtime reconciliation ran without the verified route", err)
				}
			} else {
				expectNativeHold(t, err, "native_process_identity_missing", true)
			}
			if terminal, err := NativeSessionProcessTerminal(f.cfg.StateDir, f.slot, sess); err != nil || terminal {
				t.Fatal("terminal marker written without proof", err)
			}
			if r, err := readNativeWorkerReceipt(dir, 1); err != nil || r.OutcomeIntent != nil || r.Outcome != nil || r.NativeProcessEvidence != nil {
				t.Fatalf("receipt changed without proof: %+v %v", r, err)
			}
			if sess.NativeRegistrationHold != "native_process_identity_missing" || sess.Status != state.StatusDead || sess.WorkerGeneration != 1 || sess.NativeRoleRunID != parent.RoleRunID {
				t.Fatalf("projection changed without proof: %+v", sess)
			}
		})
	}
}

// The terminal, still unsealed parent of a registered successor (the seal was
// lost after the marker was written) is sealed by the hold path before the
// successor is abandoned.
func TestNativeRuntimeReconcileSealsTerminalUnsealedParentBeforeAbandoningSuccessor(t *testing.T) {
	fx := prelaunchWedge(t, "native_process_identity_missing", state.StatusDead)
	f := fx.f
	fx.parent.OutcomeIntent, fx.parent.Outcome = nil, nil
	if err := writeNativeWorkerReceipt(fx.dir, fx.parent); err != nil {
		t.Fatal(err)
	}
	previousNativeGenerationOutcome = persistedNativeGenerationOutcome
	var order []string
	sealNativeWorker = func(_ admissioncontrol.Client, request admissioncontrol.SealRequest) (admissioncontrol.NativeOutcome, error) {
		fx.seals++
		switch request.NativeSessionID {
		case fx.parent.Request.NativeSessionID:
			order = append(order, "parent")
		case fx.next.Request.NativeSessionID:
			order = append(order, "successor")
		default:
			t.Fatal("sealed an unknown registration")
		}
		return fixtureNativeOutcome(request, false), nil
	}
	verifyNativeWorkerTermination = func(aiexecution.FileProof, string, string) (*aiexecution.NativeProcessTermination, error) {
		t.Fatal("re-observed a generation that already carries the terminal marker")
		return nil, nil
	}
	if err := ReconcileNativeWorkerRuntime(f.cfg, f.st, f.slot); err != nil {
		t.Fatal(err)
	}
	sess := f.st.Sessions[f.slot]
	if sess.NativeRegistrationHold != "" || !reflect.DeepEqual(order, []string{"parent", "successor"}) || fx.absence != 1 || sess.WorkerGeneration != 1 || sess.Status != state.StatusDead {
		t.Fatalf("hold=%q order=%v absence=%d sess=%+v", sess.NativeRegistrationHold, order, fx.absence, sess)
	}
	if parent, err := readNativeWorkerReceipt(fx.dir, 1); err != nil || parent.Outcome == nil || !parent.Outcome.NextGenerationAllowed {
		t.Fatalf("parent not sealed: %+v %v", parent, err)
	}
	if _, err := os.Lstat(filepath.Join(fx.dir, nativeReceiptName(2))); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("successor registration not archived", err)
	}
	if _, err := os.Lstat(filepath.Join(fx.dir, nativeLaunchAbandonmentPrefix(2, 1)+".receipt.json")); err != nil {
		t.Fatal("abandonment archive missing", err)
	}
}

// The fence never touches the session projection: a running session that
// reaches a phase transition without its exact lease (StartPhase) keeps its
// status, timestamps and runtime fields, and only the receipt directory
// records the proven termination.
func TestTerminationFenceLeavesSessionProjectionUntouched(t *testing.T) {
	f, parent, pin := cleanExitWithoutMarker(t)
	sess := f.st.Sessions[f.slot]
	previousNativeGenerationOutcome = persistedNativeGenerationOutcome
	sealNativeWorker = func(_ admissioncontrol.Client, request admissioncontrol.SealRequest) (admissioncontrol.NativeOutcome, error) {
		return fixtureNativeOutcome(request, false), nil
	}
	verifyNativeWorkerTermination = func(aiexecution.FileProof, string, string) (*aiexecution.NativeProcessTermination, error) {
		return nativeRuntimeProof(parent, pin, true), nil
	}
	sess.Status = state.StatusRunning
	sess.PID, sess.TmuxSession = 4040, TmuxSessionName(f.slot)
	sess.FinishedAt, sess.WorkerEndedAt = nil, nil
	started := time.Now().Add(-2 * time.Hour).UTC()
	sess.StartedAt = started
	before := *sess
	if err := ensureNativeGenerationTerminalBeforeSuccessor(f.cfg, f.slot, sess); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, *sess) {
		t.Fatalf("fence modified the session projection:\n before=%+v\n after=%+v", before, *sess)
	}
	if terminal, err := NativeSessionProcessTerminal(f.cfg.StateDir, f.slot, sess); err != nil || !terminal {
		t.Fatal("terminal marker not recorded", err)
	}
	// Idempotent: a recorded generation is not re-observed or re-sealed.
	verifyNativeWorkerTermination = func(aiexecution.FileProof, string, string) (*aiexecution.NativeProcessTermination, error) {
		t.Fatal("re-observed a recorded generation")
		return nil, nil
	}
	if err := ensureNativeGenerationTerminalBeforeSuccessor(f.cfg, f.slot, sess); err != nil {
		t.Fatal(err)
	}
}

// The fence must hold, never fail, when the projected generation's receipt is
// absent, or the receipt or its terminal marker cannot be trusted: the daemon
// records a non-hold respawn error as a failed session, while a hold retains
// the session and parks the slot for the operator. Each fault keeps its own
// code: a marker that decodes but names another identity is the existing
// native_identity_conflict, a marker failing the receipt integrity check is the
// existing receipt_invalid, and only an unreadable or undecodable file is
// projected_receipt_undecodable. A receipt whose outcome, operator recovery or
// process evidence section fails validation keeps the receipt read's own hold.
// Nothing is observed, sealed or minted meanwhile. Together the modes produce
// every code in NativeProjectedReceiptHoldCodes, the set the orchestrator parks
// and reports.
func TestTerminationFenceHoldsWhenProjectedReceiptMissingOrUndecodable(t *testing.T) {
	produced := map[string]bool{}
	for _, mode := range []string{"receipt_missing", "receipt_undecodable", "terminal_marker_undecodable", "terminal_marker_identity_conflict", "terminal_marker_invalid_mode", "outcome_invalid", "operator_recovery_invalid", "process_evidence_invalid"} {
		t.Run(mode, func(t *testing.T) {
			f, parent, _ := cleanExitWithoutMarker(t)
			sess := f.st.Sessions[f.slot]
			dir := sess.NativeReceiptDir
			previousNativeGenerationOutcome = persistedNativeGenerationOutcome
			sealNativeWorker = func(admissioncontrol.Client, admissioncontrol.SealRequest) (admissioncontrol.NativeOutcome, error) {
				t.Fatal("sealed a generation whose receipt cannot be read")
				return admissioncontrol.NativeOutcome{}, nil
			}
			verifyNativeWorkerTermination = func(aiexecution.FileProof, string, string) (*aiexecution.NativeProcessTermination, error) {
				t.Fatal("observed a generation whose receipt cannot be read")
				return nil, nil
			}
			var want string
			switch mode {
			case "receipt_missing":
				if err := os.Remove(filepath.Join(dir, nativeReceiptName(1))); err != nil {
					t.Fatal(err)
				}
				want = "projected_receipt_missing"
			case "receipt_undecodable":
				// Strict decoding of the receipt itself already reports the
				// generic receipt hold; it must keep reaching the caller as one.
				if err := os.WriteFile(filepath.Join(dir, nativeReceiptName(1)), []byte(`{"schema_version":`), 0600); err != nil {
					t.Fatal(err)
				}
				want = "receipt_invalid"
			case "terminal_marker_undecodable":
				if err := os.WriteFile(filepath.Join(dir, nativeReceiptName(1)+".terminated"), []byte(`{"generation":`), 0600); err != nil {
					t.Fatal(err)
				}
				want = "projected_receipt_undecodable"
			case "terminal_marker_identity_conflict":
				// A well-formed marker for another native session of the same
				// generation is not proof that this generation ended.
				other := terminationFor(parent)
				other.NativeSessionID = uuid.NewString()
				b, _ := json.Marshal(other)
				if err := os.WriteFile(filepath.Join(dir, nativeReceiptName(1)+".terminated"), b, 0600); err != nil {
					t.Fatal(err)
				}
				want = "native_identity_conflict"
			case "terminal_marker_invalid_mode":
				// The exact marker, but not owner-only: the receipt integrity
				// check fails, as it would for the receipt itself.
				path := filepath.Join(dir, nativeReceiptName(1)+".terminated")
				b, _ := json.Marshal(terminationFor(parent))
				if err := os.WriteFile(path, b, 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(path, 0644); err != nil {
					t.Fatal(err)
				}
				want = "receipt_invalid"
			case "outcome_invalid":
				// A settled outcome without the intent it answers.
				corrupt := *parent
				corrupt.Outcome = &admissioncontrol.NativeOutcome{}
				if err := writeNativeWorkerReceipt(dir, &corrupt); err != nil {
					t.Fatal(err)
				}
				want = "outcome_receipt_invalid"
			case "operator_recovery_invalid":
				// An operator recovery record without the retired outcome it
				// consumes.
				corrupt := *parent
				corrupt.OperatorRecovery = &NativeOperatorRecoveryRecord{NextGeneration: 2, ScheduledAt: time.Now().UTC()}
				if err := writeNativeWorkerReceipt(dir, &corrupt); err != nil {
					t.Fatal(err)
				}
				want = "operator_recovery_receipt_invalid"
			case "process_evidence_invalid":
				// Process evidence that proves neither a launch nor a termination.
				corrupt := *parent
				corrupt.NativeProcessEvidence = &aiexecution.NativeProcessTermination{Version: 1, NativeSessionID: parent.Request.NativeSessionID, Unit: parent.ProcessLeaseUnit, LocalStatus: "terminated"}
				if err := writeNativeWorkerReceipt(dir, &corrupt); err != nil {
					t.Fatal(err)
				}
				want = "native_process_evidence_invalid"
			}
			produced[want] = true
			before := *sess
			err := ensureNativeGenerationTerminalBeforeSuccessor(f.cfg, f.slot, sess)
			expectNativeHold(t, err, want, true)
			if hold, _ := NativeHold(err); hold.Slot != f.slot {
				t.Fatalf("hold not attributed to the slot: %+v", hold)
			}
			if !reflect.DeepEqual(before, *sess) {
				t.Fatalf("held fence modified the session projection:\n before=%+v\n after=%+v", before, *sess)
			}
			// The respawn entry point returns the same hold, so the daemon
			// retains the held session instead of failing it.
			f.cfg.WorkerLaunchContext = &config.WorkerLaunchContext{Role: "repair", ParentRoleRunID: sess.NativeRoleRunID}
			err = RespawnInPlace(f.cfg, f.slot, sess, f.cfg.Repo, f.issue, "repair prompt", "claude")
			expectNativeHold(t, err, want, true)
			if _, statErr := os.Lstat(filepath.Join(dir, nativeReceiptName(2))); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatal("successor registered on top of an unreadable projected receipt", statErr)
			}
			if len(f.registered) != 1 || f.spawned != 0 || f.stopped != 0 || sess.WorkerGeneration != 1 || sess.Status != state.StatusPROpen || sess.PRNumber != 20 {
				t.Fatalf("registered=%d spawned=%d stopped=%d gen=%d status=%s", len(f.registered), f.spawned, f.stopped, sess.WorkerGeneration, sess.Status)
			}
		})
	}
	for _, code := range NativeProjectedReceiptHoldCodes() {
		if !produced[code] {
			t.Errorf("no fence fault produces parked code %s", code)
		}
	}
}

// A launch_intent projected generation under a wedge hold never had its launch
// adopted by the session, so no lease path observes it. The hold reconciliation
// must take the ordinary launch/state gap recovery for it instead of
// re-reporting previous_outcome_unknown every cycle: adopt the exact live
// launch, or record its verified termination, and clear the hold.
func TestNativeRuntimeReconcileObservesLaunchIntentGenerationUnderWedgeHold(t *testing.T) {
	for _, mode := range []string{"live", "terminated"} {
		for _, hold := range []string{"native_process_identity_missing", "previous_outcome_unknown"} {
			t.Run(mode+"/"+hold, func(t *testing.T) {
				f, parent, pin := cleanExitWithoutMarker(t)
				sess := f.st.Sessions[f.slot]
				dir := sess.NativeReceiptDir
				previousNativeGenerationOutcome = persistedNativeGenerationOutcome
				parent.Status, parent.PID = "launch_intent", 0
				if err := writeNativeWorkerReceipt(dir, parent); err != nil {
					t.Fatal(err)
				}
				sess.Status, sess.PRNumber, sess.FinishedAt = state.StatusRunning, 0, nil
				sess.NativeRegistrationHold = hold
				if err := state.Save(f.cfg.StateDir, f.st); err != nil {
					t.Fatal(err)
				}
				sealNativeWorker = func(admissioncontrol.Client, admissioncontrol.SealRequest) (admissioncontrol.NativeOutcome, error) {
					t.Fatal("gap recovery sealed the generation")
					return admissioncontrol.NativeOutcome{}, nil
				}
				live := mode == "live"
				f.live = live
				observeNativeWorkerLaunch = func(got aiexecution.FileProof, projectID, nativeID, unit string) (*aiexecution.NativeProcessTermination, int, error) {
					if got != pin || projectID != parent.ProjectID || nativeID != parent.Request.NativeSessionID || unit != parent.ProcessLeaseUnit {
						t.Fatal("observed a different native identity")
					}
					if !live {
						return nil, 0, errors.New("inactive")
					}
					return nativeRuntimeProof(parent, pin, false), 9898, nil
				}
				verifyNativeWorkerTermination = func(got aiexecution.FileProof, nativeID, unit string) (*aiexecution.NativeProcessTermination, error) {
					if live {
						t.Fatal("live launch queried for termination")
					}
					if got != pin || nativeID != parent.Request.NativeSessionID || unit != parent.ProcessLeaseUnit {
						t.Fatal("wrong original process identity")
					}
					return nativeRuntimeProof(parent, pin, true), nil
				}
				if err := ReconcileNativeWorkerRuntime(f.cfg, f.st, f.slot); err != nil {
					t.Fatal("launch_intent generation under a wedge hold was not observed:", err)
				}
				after, err := readNativeWorkerReceipt(dir, 1)
				if err != nil || after.Status != "launched" || after.NativeProcessEvidence == nil || after.OutcomeIntent != nil {
					t.Fatalf("receipt not projected from the observation: %+v %v", after, err)
				}
				if sess.NativeRegistrationHold != "" || sess.WorkerGeneration != 1 || sess.NativeRoleRunID != parent.RoleRunID || sess.ProcessLeaseUnit != parent.ProcessLeaseUnit || f.spawned != 0 || f.stopped != 0 {
					t.Fatalf("hold not cleared by the observation: %+v", sess)
				}
				terminal, err := NativeSessionProcessTerminal(f.cfg.StateDir, f.slot, sess)
				if err != nil {
					t.Fatal(err)
				}
				if live {
					if sess.Status != state.StatusRunning || sess.PID != 4242 || after.PID != 9898 || terminal {
						t.Fatalf("live launch not adopted: %+v terminal=%v", sess, terminal)
					}
				} else if sess.Status != state.StatusDead || sess.PID != 0 || !terminal {
					t.Fatalf("termination not recorded: %+v terminal=%v", sess, terminal)
				}
				if saved, err := state.Load(f.cfg.StateDir); err != nil || saved.Sessions[f.slot].NativeRegistrationHold != "" {
					t.Fatal("cleared hold not durable", err)
				}
			})
		}
	}
}
