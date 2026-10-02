package worker

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/befeast/maestro/internal/admissioncontrol"
	"github.com/befeast/maestro/internal/aiexecution"
	"github.com/befeast/maestro/internal/state"
	"github.com/befeast/maestro/internal/tmuxsession"
)

func refuseNativeRegistration(t *testing.T) {
	t.Helper()
	registerNativeWorker = func(admissioncontrol.Client, admissioncontrol.RegistrationRequest) (admissioncontrol.Acknowledgement, error) {
		t.Fatal("exit reconciliation minted a successor registration")
		return admissioncontrol.Acknowledgement{}, nil
	}
}

func expectNativeSlotPending(t *testing.T, f *nativeFixture, pending bool) {
	t.Helper()
	slots, err := NativePendingSlots(f.cfg.StateDir, f.st.Sessions)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(slots) == 1 && slots[0] == f.slot; got != pending || (!pending && len(slots) != 0) {
		t.Fatalf("native pending slots=%v, want pending=%v", slots, pending)
	}
}

func expectCleanupNativeOutcome(t *testing.T, f *nativeFixture, sess *state.Session, settled bool) {
	t.Helper()
	lease := CaptureCleanupLease(f.slot, sess)
	probes := CleanupProbes{PIDAlive: func(int) bool { return false }, TmuxAlive: func(string) bool { return false }}
	err := ValidateCleanupLease(lease, sess, probes, CleanupPolicy{})
	if settled {
		if err != nil {
			t.Fatal("stale-worktree cleanup still refused after the exit seal", err)
		}
		return
	}
	if err == nil || !strings.Contains(err.Error(), "native physical outcome unresolved") {
		t.Fatalf("cleanup error=%v, want native physical outcome unresolved", err)
	}
}

// #1243: a cleanly exited generation (lease released by the scratch reconciler,
// no terminal marker, no seal) is proven, sealed and recorded by the per-cycle
// exit reconciliation instead of waiting for a respawn the fleet ceiling can
// block. The slot then stops counting as a live worker and stale-worktree
// cleanup accepts the settled outcome.
func TestReconcileNativeWorkerExitSealsCleanExitAndReleasesCapacity(t *testing.T) {
	for _, status := range []state.SessionStatus{state.StatusDone, state.StatusPROpen, state.StatusRetryExhausted} {
		t.Run(string(status), func(t *testing.T) {
			f, parent, pin := cleanExitWithoutMarker(t)
			sess := f.st.Sessions[f.slot]
			sess.Status = status
			dir := sess.NativeReceiptDir
			previousNativeGenerationOutcome = persistedNativeGenerationOutcome
			refuseNativeRegistration(t)
			seals, verified := 0, 0
			sealNativeWorker = func(_ admissioncontrol.Client, request admissioncontrol.SealRequest) (admissioncontrol.NativeOutcome, error) {
				seals++
				if request.NativeSessionID != parent.Request.NativeSessionID || request.RegistrationVersion != parent.Acknowledgement.RegistrationVersion {
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
			expectNativeSlotPending(t, f, true)
			expectCleanupNativeOutcome(t, f, sess, false)
			before := *sess

			sealed, err := ReconcileNativeWorkerExit(f.cfg, f.st, f.slot)
			if err != nil || !sealed {
				t.Fatalf("sealed=%v err=%v", sealed, err)
			}
			if seals != 1 || verified != 1 {
				t.Fatalf("seals=%d verified=%d", seals, verified)
			}
			if !reflect.DeepEqual(before, *sess) {
				t.Fatalf("exit reconciliation modified the session projection:\n before=%+v\n after=%+v", before, *sess)
			}
			r, err := readNativeWorkerReceipt(dir, 1)
			if err != nil || r.Outcome == nil || !r.Outcome.NextGenerationAllowed || r.NativeProcessEvidence == nil || r.NativeProcessEvidence.LocalStatus == "launch_intent" {
				t.Fatalf("generation not sealed with termination evidence: %+v %v", r, err)
			}
			if terminal, err := NativeSessionProcessTerminal(f.cfg.StateDir, f.slot, sess); err != nil || !terminal {
				t.Fatal("terminal marker not recorded", err)
			}
			if started, err := NativeWorkerSealStarted(f.cfg.StateDir, f.slot, sess); err != nil || started {
				t.Fatal("settled generation still reports an unfinished seal", started, err)
			}
			if _, err := os.Lstat(filepath.Join(dir, nativeReceiptName(2))); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("exit reconciliation registered a successor", err)
			}
			expectNativeSlotPending(t, f, false)
			expectCleanupNativeOutcome(t, f, sess, true)

			// Idempotent: a recorded, settled generation is neither re-observed
			// nor re-sealed on later cycles.
			sealNativeWorker = func(admissioncontrol.Client, admissioncontrol.SealRequest) (admissioncontrol.NativeOutcome, error) {
				t.Fatal("re-sealed a settled generation")
				return admissioncontrol.NativeOutcome{}, nil
			}
			verifyNativeWorkerTermination = func(aiexecution.FileProof, string, string) (*aiexecution.NativeProcessTermination, error) {
				t.Fatal("re-observed a recorded generation")
				return nil, nil
			}
			if sealed, err := ReconcileNativeWorkerExit(f.cfg, f.st, f.slot); err != nil || sealed {
				t.Fatalf("second pass sealed=%v err=%v", sealed, err)
			}
		})
	}
}

// A generation whose terminal marker was written by the lease termination path
// but whose binding was never sealed is sealed without re-observing the
// process, which unblocks stale-worktree cleanup.
func TestReconcileNativeWorkerExitSealsTerminalUnsealedGeneration(t *testing.T) {
	f, parent, _ := cleanExitWithoutMarker(t)
	sess := f.st.Sessions[f.slot]
	sess.Status = state.StatusDone
	previousNativeGenerationOutcome = persistedNativeGenerationOutcome
	refuseNativeRegistration(t)
	marked := *sess
	setSessionProcessLease(&marked, tmuxsession.ProcessLease{Unit: parent.ProcessLeaseUnit, Manager: parent.ProcessLeaseManager})
	if err := markNativeWorkerTerminated(&marked); err != nil {
		t.Fatal(err)
	}
	expectNativeSlotPending(t, f, false)
	expectCleanupNativeOutcome(t, f, sess, false)
	seals := 0
	sealNativeWorker = func(_ admissioncontrol.Client, request admissioncontrol.SealRequest) (admissioncontrol.NativeOutcome, error) {
		seals++
		return fixtureNativeOutcome(request, false), nil
	}
	verifyNativeWorkerTermination = func(aiexecution.FileProof, string, string) (*aiexecution.NativeProcessTermination, error) {
		t.Fatal("re-observed a generation that already carries the terminal marker")
		return nil, nil
	}
	if sealed, err := ReconcileNativeWorkerExit(f.cfg, f.st, f.slot); err != nil || !sealed || seals != 1 {
		t.Fatalf("sealed=%v err=%v seals=%d", sealed, err, seals)
	}
	expectCleanupNativeOutcome(t, f, sess, true)
}

// Fail-closed: without proven OS termination the exit reconciliation reports a
// typed hold and persists nothing, so the slot keeps counting as a live worker
// and cleanup stays refused. Sessions that still run or still own their exact
// lease are not touched at all.
func TestReconcileNativeWorkerExitKeepsUncertainTerminationHeld(t *testing.T) {
	for _, mode := range []string{"lease_active", "pane_present", "proof_unverified", "session_running", "session_owns_lease"} {
		t.Run(mode, func(t *testing.T) {
			f, parent, _ := cleanExitWithoutMarker(t)
			sess := f.st.Sessions[f.slot]
			sess.Status = state.StatusDone
			dir := sess.NativeReceiptDir
			previousNativeGenerationOutcome = persistedNativeGenerationOutcome
			refuseNativeRegistration(t)
			sealNativeWorker = func(admissioncontrol.Client, admissioncontrol.SealRequest) (admissioncontrol.NativeOutcome, error) {
				t.Fatal("sealed a generation whose termination was not proven")
				return admissioncontrol.NativeOutcome{}, nil
			}
			observed := 0
			verifyNativeWorkerTermination = func(aiexecution.FileProof, string, string) (*aiexecution.NativeProcessTermination, error) {
				observed++
				return nil, errors.New("journal unavailable")
			}
			untouched := false
			switch mode {
			case "lease_active":
				workerProcessLeaseActive = func(tmuxsession.ProcessLease) (bool, error) { return true, nil }
			case "pane_present":
				nativeWorkerPaneAbsent = func(string) (bool, error) { return false, nil }
			case "session_running":
				sess.Status = state.StatusRunning
				untouched = true
			case "session_owns_lease":
				setSessionProcessLease(sess, tmuxsession.ProcessLease{Unit: parent.ProcessLeaseUnit, Manager: parent.ProcessLeaseManager})
				untouched = true
			}
			before := *sess
			sealed, err := ReconcileNativeWorkerExit(f.cfg, f.st, f.slot)
			if sealed {
				t.Fatal("sealed without proven termination")
			}
			if untouched {
				if err != nil || observed != 0 {
					t.Fatalf("exit reconciliation acted on a session that still owns its process: err=%v observed=%d", err, observed)
				}
			} else {
				expectNativeHold(t, err, "native_process_identity_missing", true)
				if h, _ := NativeHold(err); h.Slot != f.slot {
					t.Fatalf("hold does not name the slot: %+v", h)
				}
			}
			if mode == "proof_unverified" && observed != 1 {
				t.Fatalf("termination proof consulted %d times", observed)
			}
			if !reflect.DeepEqual(before, *sess) {
				t.Fatalf("projection changed without proof:\n before=%+v\n after=%+v", before, *sess)
			}
			if r, err := readNativeWorkerReceipt(dir, 1); err != nil || r.OutcomeIntent != nil || r.Outcome != nil || r.NativeProcessEvidence != nil {
				t.Fatalf("receipt changed without proof: %+v %v", r, err)
			}
			if terminal, err := NativeSessionProcessTerminal(f.cfg.StateDir, f.slot, sess); err != nil || terminal {
				t.Fatal("terminal marker written without proof", err)
			}
			expectNativeSlotPending(t, f, true)
			if !untouched {
				expectCleanupNativeOutcome(t, f, sess, false)
			}
		})
	}
}

// An authority that cannot settle keeps the generation unsealed and counted:
// unavailable leaves only the durable seal intent, a held settlement denies a
// next generation. Once the authority settles, the next cycle seals it even
// after the cycle's seal-started guard stamped native_generation_sealed, which
// the existing hold path would otherwise never clear without the marker.
func TestReconcileNativeWorkerExitHoldsUntilAuthoritySettles(t *testing.T) {
	for _, mode := range []string{"authority_unavailable", "settlement_held"} {
		t.Run(mode, func(t *testing.T) {
			f, parent, pin := cleanExitWithoutMarker(t)
			sess := f.st.Sessions[f.slot]
			sess.Status = state.StatusDone
			dir := sess.NativeReceiptDir
			previousNativeGenerationOutcome = persistedNativeGenerationOutcome
			refuseNativeRegistration(t)
			settle := false
			seals := 0
			sealNativeWorker = func(_ admissioncontrol.Client, request admissioncontrol.SealRequest) (admissioncontrol.NativeOutcome, error) {
				seals++
				if settle {
					return fixtureNativeOutcome(request, false), nil
				}
				if mode == "authority_unavailable" {
					return admissioncontrol.NativeOutcome{}, errors.New("socket down")
				}
				return fixtureNativeOutcome(request, true), nil
			}
			verifyNativeWorkerTermination = func(aiexecution.FileProof, string, string) (*aiexecution.NativeProcessTermination, error) {
				return nativeRuntimeProof(parent, pin, true), nil
			}
			sealed, err := ReconcileNativeWorkerExit(f.cfg, f.st, f.slot)
			if sealed {
				t.Fatal("sealed without a settlement allowing a next generation")
			}
			if mode == "authority_unavailable" {
				expectNativeHold(t, err, "outcome_authority_unavailable", false)
			} else {
				expectNativeHold(t, err, "previous_outcome_unknown", false)
			}
			r, err := readNativeWorkerReceipt(dir, 1)
			if err != nil || r.OutcomeIntent == nil || r.NativeProcessEvidence != nil {
				t.Fatalf("seal intent not durable or evidence recorded early: %+v %v", r, err)
			}
			if (mode == "authority_unavailable") != (r.Outcome == nil) {
				t.Fatalf("unexpected persisted outcome %+v", r.Outcome)
			}
			if terminal, err := NativeSessionProcessTerminal(f.cfg.StateDir, f.slot, sess); err != nil || terminal {
				t.Fatal("terminal marker written without a settlement", err)
			}
			expectNativeSlotPending(t, f, true)
			expectCleanupNativeOutcome(t, f, sess, false)

			// The cycle's seal-started guard projects the unfinished seal.
			started, err := NativeWorkerSealStarted(f.cfg.StateDir, f.slot, sess)
			if err != nil || !started {
				t.Fatal("unfinished seal not reported", started, err)
			}
			sess.NativeRegistrationHold = "native_generation_sealed"

			settle = true
			if sealed, err := ReconcileNativeWorkerExit(f.cfg, f.st, f.slot); err != nil || !sealed {
				t.Fatalf("settled authority not sealed on the next cycle: sealed=%v err=%v", sealed, err)
			}
			if seals != 2 {
				t.Fatalf("seals=%d", seals)
			}
			if terminal, err := NativeSessionProcessTerminal(f.cfg.StateDir, f.slot, sess); err != nil || !terminal {
				t.Fatal("terminal marker not recorded once settled", err)
			}
			if started, err := NativeWorkerSealStarted(f.cfg.StateDir, f.slot, sess); err != nil || started {
				t.Fatal("seal still reported unfinished; the hold path cannot clear native_generation_sealed", started, err)
			}
			expectNativeSlotPending(t, f, false)
		})
	}
}

// Holds owned by other reconciliations, launch_intent receipts and native
// sessions without the verified route are left alone.
func TestReconcileNativeWorkerExitIgnoresForeignShapes(t *testing.T) {
	for _, mode := range []string{"wedge_hold", "launch_intent", "verified_route_off", "registration_removed"} {
		t.Run(mode, func(t *testing.T) {
			f, parent, _ := cleanExitWithoutMarker(t)
			sess := f.st.Sessions[f.slot]
			sess.Status = state.StatusDone
			dir := sess.NativeReceiptDir
			previousNativeGenerationOutcome = persistedNativeGenerationOutcome
			refuseNativeRegistration(t)
			sealNativeWorker = func(admissioncontrol.Client, admissioncontrol.SealRequest) (admissioncontrol.NativeOutcome, error) {
				t.Fatal("sealed a foreign shape")
				return admissioncontrol.NativeOutcome{}, nil
			}
			verifyNativeWorkerTermination = func(aiexecution.FileProof, string, string) (*aiexecution.NativeProcessTermination, error) {
				t.Fatal("observed a foreign shape")
				return nil, nil
			}
			switch mode {
			case "wedge_hold":
				sess.NativeRegistrationHold = "native_process_identity_missing"
			case "launch_intent":
				parent.Status = "launch_intent"
				if err := writeNativeWorkerReceipt(dir, parent); err != nil {
					t.Fatal(err)
				}
			case "verified_route_off":
				f.cfg.AIExecution.RequireVerifiedRoute = false
			case "registration_removed":
				f.cfg.WorkerNativeSessionRegistration = nil
			}
			if sealed, err := ReconcileNativeWorkerExit(f.cfg, f.st, f.slot); err != nil || sealed {
				t.Fatalf("sealed=%v err=%v", sealed, err)
			}
			expectNativeSlotPending(t, f, true)
		})
	}
}
