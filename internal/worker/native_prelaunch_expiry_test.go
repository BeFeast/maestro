package worker

import (
	"bytes"
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
	"github.com/befeast/maestro/internal/state"
	"github.com/befeast/maestro/internal/tmuxsession"
)

type expiredPrelaunchFixture struct {
	f        *nativeFixture
	dir      string
	receipt  *NativeWorkerReceipt
	before   []byte
	hold     string
	seals    int
	absence  int
	register *int
}

// expiredPrelaunch reproduces the live 2026-10-02 shape: a first-generation
// start registered its native session and then failed setup before launch
// intent (live: the gateway binding observation held with
// binding_inventory_incomplete during a binding outage longer than the
// registration TTL). The session is failed under the hold with no
// native generation stamped on it; the generation-1 receipt is registered with
// no PID, log file, outcome or terminal marker; no lease is active and no pane
// exists. The clock is moved past the registration expiry.
func expiredPrelaunch(t *testing.T) *expiredPrelaunchFixture {
	t.Helper()
	f, receipt, calls := registeredPrelaunchFixture(t)
	f.cfg.AIExecution = syntheticVerifiedRoutePolicy(t, f.cfg.StateDir)
	f.cfg.WorkerRuntime = isolatedRuntimeConfig(t, f.cfg.ProjectID).WorkerRuntime
	dir := nativeReceiptDir(f.cfg.StateDir, f.slot)
	before, err := os.ReadFile(filepath.Join(dir, nativeReceiptName(1)))
	if err != nil {
		t.Fatal(err)
	}
	const hold = "binding_inventory_incomplete"
	sess := f.st.Sessions[f.slot]
	if !NativePrelaunchExpiryCandidate(sess) || sess.Worktree != filepath.Join(f.cfg.WorktreeBase, f.slot) || sess.LogFile != "" {
		t.Fatalf("fixture is not the failed first-generation prelaunch projection: %+v", sess)
	}
	sess.NativeRegistrationHold = hold
	if err := state.Save(f.cfg.StateDir, f.st); err != nil {
		t.Fatal(err)
	}
	fx := &expiredPrelaunchFixture{f: f, dir: dir, receipt: receipt, before: before, hold: hold, register: calls}
	oldSeal, oldAbsence, oldObserve, oldClock := sealNativeWorker, verifyNativeWorkerPrelaunchAbsence, observeNativeWorkerLaunch, nativePrelaunchExpiryClock
	t.Cleanup(func() {
		sealNativeWorker = oldSeal
		verifyNativeWorkerPrelaunchAbsence = oldAbsence
		observeNativeWorkerLaunch = oldObserve
		nativePrelaunchExpiryClock = oldClock
	})
	expiresAt := time.Unix(receipt.Request.ExpiresAt, 0)
	nativePrelaunchExpiryClock = func() time.Time { return expiresAt.Add(time.Minute) }
	sealNativeWorker = func(_ admissioncontrol.Client, request admissioncontrol.SealRequest) (admissioncontrol.NativeOutcome, error) {
		fx.seals++
		if request.Binding != receipt.Request.Binding || request.RegistrationVersion != receipt.Acknowledgement.RegistrationVersion {
			t.Fatal("sealed a different registration")
		}
		return fixtureNativeOutcome(request, false), nil
	}
	verifyNativeWorkerPrelaunchAbsence = func(_ aiexecution.FileProof, projectID, nativeID, unit string) error {
		fx.absence++
		if projectID != receipt.ProjectID || nativeID != receipt.Request.NativeSessionID || unit != receipt.ProcessLeaseUnit {
			t.Fatal("absence checked for a different native identity")
		}
		return nil
	}
	observeNativeWorkerLaunch = func(aiexecution.FileProof, string, string, string) (*aiexecution.NativeProcessTermination, int, error) {
		t.Fatal("expired prelaunch reconciliation observed a live launch")
		return nil, 0, nil
	}
	return fx
}

func (fx *expiredPrelaunchFixture) reconcile() (bool, error) {
	return ReconcileExpiredNativePrelaunch(fx.f.cfg, fx.f.st, fx.f.slot)
}

// useRequestsBasis rewrites the registration to the request-accounted
// admission basis the live fleet uses.
func (fx *expiredPrelaunchFixture) useRequestsBasis(t *testing.T) {
	t.Helper()
	fx.receipt.Request.AdmissionBasis = "requests"
	ack := *fx.receipt.Acknowledgement
	ack.Binding = fx.receipt.Request.Binding
	fx.receipt.Acknowledgement = &ack
	if err := writeNativeWorkerReceipt(fx.dir, fx.receipt); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(fx.dir, nativeReceiptName(1)))
	if err != nil {
		t.Fatal(err)
	}
	fx.before = b
}

// resealFixtureOutcome recomputes the snapshot digest and evidence ID of an
// edited fixture outcome so it passes ValidateNativeOutcome.
func resealFixtureOutcome(outcome admissioncontrol.NativeOutcome) admissioncontrol.NativeOutcome {
	body, _ := json.Marshal(outcome)
	var object map[string]any
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	_ = decoder.Decode(&object)
	delete(object, "snapshot_digest")
	delete(object, "evidence_id")
	var canonical bytes.Buffer
	encoder := json.NewEncoder(&canonical)
	encoder.SetEscapeHTML(false)
	_ = encoder.Encode(object)
	digest := sha256.Sum256(bytes.TrimSuffix(canonical.Bytes(), []byte("\n")))
	outcome.SnapshotDigest = hex.EncodeToString(digest[:])
	prefix := "native-outcome-v1:"
	if outcome.AdmissionBasis == "requests" {
		prefix = "native-outcome-v2:"
	}
	outcome.EvidenceID = prefix + outcome.SnapshotDigest
	return outcome
}

// zeroAttemptOutcome is the authority settlement of a binding no request ever
// used: no_dispatch, zero physical attempts, next generation allowed.
func zeroAttemptOutcome(request admissioncontrol.SealRequest) admissioncontrol.NativeOutcome {
	outcome := fixtureNativeOutcome(request, false)
	outcome.Outcome, outcome.PhysicalAttempts, outcome.TerminalAttempts = "no_dispatch", 0, 0
	return resealFixtureOutcome(outcome)
}

// assertUntouched requires the receipt, the session projection and the issue
// claim to be exactly as before a reconciliation that must not act.
func (fx *expiredPrelaunchFixture) assertUntouched(t *testing.T, sessBefore state.Session) {
	t.Helper()
	current, err := os.ReadFile(filepath.Join(fx.dir, nativeReceiptName(1)))
	if err != nil || !reflect.DeepEqual(current, fx.before) {
		t.Fatal("generation-1 receipt changed", err)
	}
	if _, err := os.Lstat(filepath.Join(fx.dir, "launch-abandoned-g1-1.receipt.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("receipt archived", err)
	}
	if got := *fx.f.st.Sessions[fx.f.slot]; !reflect.DeepEqual(got, sessBefore) {
		t.Fatalf("session projection changed:\n got %+v\nwant %+v", got, sessBefore)
	}
	if !fx.f.st.IssueHasNonFreshClaim(fx.f.issue.Number) {
		t.Fatal("issue claim released without a retired registration")
	}
	if fx.f.spawned != 0 || fx.f.stopped != 0 {
		t.Fatalf("process touched: spawned=%d stopped=%d", fx.f.spawned, fx.f.stopped)
	}
}

func TestExpiredNativePrelaunchIsSealedArchivedAndReleased(t *testing.T) {
	fx := expiredPrelaunch(t)
	f := fx.f
	if !f.st.IssueHasNonFreshClaim(f.issue.Number) {
		t.Fatal("fixture: the held slot must claim the issue")
	}
	released, err := fx.reconcile()
	if err != nil || !released {
		t.Fatalf("released=%v err=%v", released, err)
	}
	if fx.seals != 1 || fx.absence != 1 || *fx.register != 0 || f.spawned != 0 || f.stopped != 0 {
		t.Fatalf("seals=%d absence=%d register=%d spawned=%d stopped=%d", fx.seals, fx.absence, *fx.register, f.spawned, f.stopped)
	}
	// The receipt is archived byte-for-byte with its seal intent and outcome.
	if _, err := os.Lstat(filepath.Join(fx.dir, nativeReceiptName(1))); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("abandoned registration still occupies generation 1", err)
	}
	archived, _ := os.ReadFile(filepath.Join(fx.dir, "launch-abandoned-g1-1.receipt.json"))
	if !reflect.DeepEqual(archived, fx.before) {
		t.Fatal("receipt was not archived byte-for-byte")
	}
	if _, err := os.Lstat(filepath.Join(fx.dir, "launch-abandoned-g1-1.intent.json")); err != nil {
		t.Fatal("seal intent sidecar missing", err)
	}
	if _, err := os.Lstat(filepath.Join(fx.dir, "launch-abandoned-g1-1.execution.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("an execution proof was archived for a registered receipt", err)
	}
	recordBytes, err := os.ReadFile(filepath.Join(fx.dir, "launch-abandoned-g1-1.outcome.json"))
	if err != nil {
		t.Fatal(err)
	}
	var record NativeLaunchAbandonmentRecord
	if err := json.Unmarshal(recordBytes, &record); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(fx.before)
	if record.Generation != 1 || record.Slot != f.slot || record.PreviousStatus != "registered" || record.ClearedHold != fx.hold || record.ParentRoleRunID != "" ||
		record.RoleRunID != fx.receipt.RoleRunID || record.NativeSessionID != fx.receipt.Request.NativeSessionID || record.ReceiptSHA256 != hex.EncodeToString(sum[:]) ||
		record.ExecutionProofSHA256 != "" || record.Outcome.PhysicalAttempts != 0 || record.Outcome.Outcome != "no_dispatch" || !record.Outcome.NextGenerationAllowed {
		t.Fatalf("abandonment record incomplete: %+v", record)
	}
	// Hold cleared, issue released, attempt kept as an audit record that does
	// not consume the per-issue retry budget.
	sess := f.st.Sessions[f.slot]
	if sess.NativeRegistrationHold != "" || !sess.ReleasedForRedispatch || sess.WorkerOutcome != state.WorkerOutcomeNativePrelaunchAbandoned ||
		sess.Status != state.StatusFailed || sess.FinishedAt == nil || sess.WorkerGeneration != 0 || sess.NativeRoleRunID != "" {
		t.Fatalf("projection not released: %+v", sess)
	}
	if _, claimed := f.st.IssueClaimFor(f.issue.Number); claimed {
		t.Fatal("issue still claimed after the registration was retired")
	}
	if n := f.st.FailedAttemptsForIssue(f.issue.Number); n != 0 {
		t.Fatalf("never-launched attempt consumed retry budget: %d", n)
	}
	saved, err := state.Load(f.cfg.StateDir)
	if err != nil || saved.Sessions[f.slot].NativeRegistrationHold != "" || !saved.Sessions[f.slot].ReleasedForRedispatch {
		t.Fatal("released projection not durable", err)
	}
	if pending, err := NativePendingSlots(f.cfg.StateDir, f.st.Sessions); err != nil || len(pending) != 0 {
		t.Fatal("archived registration still consumes fleet capacity", pending, err)
	}
	// Normal selection now claims the issue under a fresh slot identity. The
	// fixture placed its slot directly; a real fresh claim advances the slot
	// sequence past it.
	if saved.NextSlot < 2 {
		saved.NextSlot = 2
	}
	claim, acquired, err := saved.ClaimFreshDispatch(f.issue.Number, f.cfg.SessionPrefix, "dispatch:fixture", time.Minute, time.Now())
	if err != nil || !acquired || claim == nil || claim.Slot == f.slot {
		t.Fatalf("issue not redispatchable: claim=%+v acquired=%v err=%v", claim, acquired, err)
	}

	// Idempotent across cycles and across a daemon restart: the released
	// session is no longer a candidate and nothing is sealed again.
	if released, err := fx.reconcile(); err != nil || released || fx.seals != 1 {
		t.Fatalf("second cycle acted: released=%v err=%v seals=%d", released, err, fx.seals)
	}
	restarted, err := state.Load(f.cfg.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	if released, err := ReconcileExpiredNativePrelaunch(f.cfg, restarted, f.slot); err != nil || released || fx.seals != 1 {
		t.Fatalf("restart acted: released=%v err=%v seals=%d", released, err, fx.seals)
	}
}

// A crash (or failed save) between archiving the receipt and saving the
// released projection is replayed from the exact archived record without
// another seal.
func TestExpiredNativePrelaunchReplaysArchivedAbandonmentAfterCrash(t *testing.T) {
	fx := expiredPrelaunch(t)
	f := fx.f
	if released, err := fx.reconcile(); err != nil || !released || fx.seals != 1 {
		t.Fatalf("released=%v err=%v seals=%d", released, err, fx.seals)
	}
	// Put the held projection back on disk, as if the save never happened,
	// and reload it as a restarted daemon would.
	restoreHeld := func(hold string) *state.State {
		t.Helper()
		disk, err := state.Load(f.cfg.StateDir)
		if err != nil {
			t.Fatal(err)
		}
		sess := disk.Sessions[f.slot]
		sess.NativeRegistrationHold, sess.ReleasedForRedispatch, sess.WorkerOutcome, sess.FinishedAt = hold, false, "", nil
		if err := state.Save(f.cfg.StateDir, disk); err != nil {
			t.Fatal(err)
		}
		restarted, err := state.Load(f.cfg.StateDir)
		if err != nil {
			t.Fatal(err)
		}
		return restarted
	}
	restarted := restoreHeld(fx.hold)
	if released, err := ReconcileExpiredNativePrelaunch(f.cfg, restarted, f.slot); err != nil || !released {
		t.Fatalf("archived abandonment was not replayed: released=%v err=%v", released, err)
	}
	if fx.seals != 1 || fx.absence != 1 {
		t.Fatalf("replay re-sealed: seals=%d absence=%d", fx.seals, fx.absence)
	}
	saved, err := state.Load(f.cfg.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	if sess := saved.Sessions[f.slot]; sess.NativeRegistrationHold != "" || !sess.ReleasedForRedispatch || sess.WorkerOutcome != state.WorkerOutcomeNativePrelaunchAbandoned {
		t.Fatalf("replay did not release durably: %+v", sess)
	}
	// An archive recorded under a different hold never replays.
	other := restoreHeld("setup_failed")
	if released, err := ReconcileExpiredNativePrelaunch(f.cfg, other, f.slot); err != nil || released || other.Sessions[f.slot].NativeRegistrationHold != "setup_failed" {
		t.Fatalf("archive of a different hold replayed: released=%v err=%v", released, err)
	}
	// A tampered archive is reported, not replayed.
	restarted = restoreHeld(fx.hold)
	if err := os.WriteFile(filepath.Join(fx.dir, "launch-abandoned-g1-1.receipt.json"), []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	released, err := ReconcileExpiredNativePrelaunch(f.cfg, restarted, f.slot)
	if released || restarted.Sessions[f.slot].NativeRegistrationHold != fx.hold {
		t.Fatal("tampered archive replayed")
	}
	expectNativeHold(t, err, "receipt_invalid", true)
}

// The live fleet registers with the request-accounted admission basis; its
// zero-attempt settlement is no_dispatch as well.
func TestExpiredNativePrelaunchRequestsBasisSealsWithZeroAttempts(t *testing.T) {
	fx := expiredPrelaunch(t)
	fx.useRequestsBasis(t)
	sealNativeWorker = func(_ admissioncontrol.Client, request admissioncontrol.SealRequest) (admissioncontrol.NativeOutcome, error) {
		fx.seals++
		if request.AdmissionBasis != "requests" || request.Binding != fx.receipt.Request.Binding {
			t.Fatal("sealed a different registration")
		}
		return zeroAttemptOutcome(request), nil
	}
	if released, err := fx.reconcile(); err != nil || !released || fx.seals != 1 {
		t.Fatalf("released=%v err=%v seals=%d", released, err, fx.seals)
	}
	b, err := os.ReadFile(filepath.Join(fx.dir, "launch-abandoned-g1-1.outcome.json"))
	if err != nil {
		t.Fatal(err)
	}
	var record NativeLaunchAbandonmentRecord
	if err := json.Unmarshal(b, &record); err != nil || record.Outcome.SchemaVersion != 2 || record.Outcome.PhysicalAttempts != 0 || record.Outcome.Outcome != "no_dispatch" {
		t.Fatalf("record=%+v err=%v", record, err)
	}
}

func TestExpiredNativePrelaunchLeavesValidRegistrationUntouched(t *testing.T) {
	fx := expiredPrelaunch(t)
	nativePrelaunchExpiryClock = time.Now
	sessBefore := *fx.f.st.Sessions[fx.f.slot]
	released, err := fx.reconcile()
	if err != nil || released {
		t.Fatalf("valid registration reconciled: released=%v err=%v", released, err)
	}
	// Revocation is probed by replaying the exact persisted request, the same
	// idempotent call explicit recovery makes; a valid acknowledgement keeps
	// the slot for explicit recovery.
	if *fx.register != 1 || fx.seals != 0 || fx.absence != 0 {
		t.Fatalf("register=%d seals=%d absence=%d", *fx.register, fx.seals, fx.absence)
	}
	fx.assertUntouched(t, sessBefore)
	for _, code := range []string{"authority_unavailable", "policy_conflict"} {
		registerNativeWorker = func(admissioncontrol.Client, admissioncontrol.RegistrationRequest) (admissioncontrol.Acknowledgement, error) {
			return admissioncontrol.Acknowledgement{}, &admissioncontrol.Hold{Code: code}
		}
		if released, err := fx.reconcile(); err != nil || released || fx.seals != 0 {
			t.Fatalf("%s: unknown registration status reconciled: released=%v err=%v", code, released, err)
		}
		fx.assertUntouched(t, sessBefore)
	}
}

func TestExpiredNativePrelaunchRetiresRevokedRegistration(t *testing.T) {
	for _, code := range []string{"registration_revoked", "registration_expired"} {
		t.Run(code, func(t *testing.T) {
			fx := expiredPrelaunch(t)
			nativePrelaunchExpiryClock = time.Now
			probes := 0
			registerNativeWorker = func(_ admissioncontrol.Client, request admissioncontrol.RegistrationRequest) (admissioncontrol.Acknowledgement, error) {
				probes++
				if request != fx.receipt.Request {
					t.Fatal("revocation probe changed the persisted registration request")
				}
				return admissioncontrol.Acknowledgement{}, &admissioncontrol.Hold{Code: code}
			}
			released, err := fx.reconcile()
			if err != nil || !released || probes != 1 || fx.seals != 1 {
				t.Fatalf("released=%v err=%v probes=%d seals=%d", released, err, probes, fx.seals)
			}
			if _, err := os.Lstat(filepath.Join(fx.dir, "launch-abandoned-g1-1.receipt.json")); err != nil {
				t.Fatal("revoked registration not archived", err)
			}
		})
	}
}

// Uncertain launches keep their existing fencing, and a registration that
// never received an acknowledgement has nothing to seal.
func TestExpiredNativePrelaunchLeavesNonRegisteredReceiptsUntouched(t *testing.T) {
	for _, status := range []string{"launch_intent", "launched", "registration_intent", "registration_reconciled_not_launched"} {
		t.Run(status, func(t *testing.T) {
			fx := expiredPrelaunch(t)
			r := *fx.receipt
			r.Status = status
			switch status {
			case "launch_intent", "launched":
				r.LogFile = filepath.Join(state.LogDir(fx.f.cfg.StateDir), fx.f.slot+".log")
			case "registration_intent":
				r.Acknowledgement = nil
			}
			if err := writeNativeWorkerReceipt(fx.dir, &r); err != nil {
				t.Fatal(err)
			}
			b, err := os.ReadFile(filepath.Join(fx.dir, nativeReceiptName(1)))
			if err != nil {
				t.Fatal(err)
			}
			fx.before = b
			sessBefore := *fx.f.st.Sessions[fx.f.slot]
			if released, err := fx.reconcile(); err != nil || released {
				t.Fatalf("released=%v err=%v", released, err)
			}
			if fx.seals != 0 || fx.absence != 0 || *fx.register != 0 {
				t.Fatalf("seals=%d absence=%d register=%d", fx.seals, fx.absence, *fx.register)
			}
			fx.assertUntouched(t, sessBefore)
		})
	}
}

func TestExpiredNativePrelaunchKeepsHoldWhenAuthorityCannotSettle(t *testing.T) {
	for _, mode := range []string{"authority_unavailable", "attempts_held", "attempt_settled", "request_accounted", "invalid_response"} {
		t.Run(mode, func(t *testing.T) {
			fx := expiredPrelaunch(t)
			want := "native_launch_abandonment_contradicted"
			switch mode {
			case "authority_unavailable":
				sealNativeWorker = func(admissioncontrol.Client, admissioncontrol.SealRequest) (admissioncontrol.NativeOutcome, error) {
					fx.seals++
					return admissioncontrol.NativeOutcome{}, errors.New("socket down")
				}
				want = "outcome_authority_unavailable"
			case "attempts_held":
				sealNativeWorker = func(_ admissioncontrol.Client, request admissioncontrol.SealRequest) (admissioncontrol.NativeOutcome, error) {
					fx.seals++
					return fixtureNativeOutcome(request, true), nil
				}
			case "attempt_settled":
				// A settled physical attempt on the binding contradicts the
				// local "never launched" evidence even though a next
				// generation would be allowed.
				sealNativeWorker = func(_ admissioncontrol.Client, request admissioncontrol.SealRequest) (admissioncontrol.NativeOutcome, error) {
					fx.seals++
					outcome := fixtureNativeOutcome(request, false)
					outcome.Outcome, outcome.PhysicalAttempts, outcome.TerminalAttempts = "settled", 1, 1
					return resealFixtureOutcome(outcome), nil
				}
			case "request_accounted":
				fx.useRequestsBasis(t)
				sealNativeWorker = func(_ admissioncontrol.Client, request admissioncontrol.SealRequest) (admissioncontrol.NativeOutcome, error) {
					fx.seals++
					return fixtureNativeOutcome(request, false), nil
				}
			case "invalid_response":
				sealNativeWorker = func(_ admissioncontrol.Client, request admissioncontrol.SealRequest) (admissioncontrol.NativeOutcome, error) {
					fx.seals++
					outcome := zeroAttemptOutcome(request)
					outcome.EvidenceID = "forged"
					return outcome, nil
				}
				want = "outcome_response_invalid"
			}
			sessBefore := *fx.f.st.Sessions[fx.f.slot]
			released, err := fx.reconcile()
			if released {
				t.Fatal("released without an authority settlement")
			}
			expectNativeHold(t, err, want, true)
			if fx.seals != 1 {
				t.Fatalf("seals=%d", fx.seals)
			}
			// The seal intent is durable before the authority call; nothing else moved.
			if _, err := os.Lstat(filepath.Join(fx.dir, "launch-abandoned-g1-1.intent.json")); err != nil {
				t.Fatal("seal intent not persisted before the authority call", err)
			}
			if _, err := os.Lstat(filepath.Join(fx.dir, "launch-abandoned-g1-1.outcome.json")); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("outcome archived without a settlement", err)
			}
			fx.assertUntouched(t, sessBefore)
			saved, err := state.Load(fx.f.cfg.StateDir)
			if err != nil || saved.Sessions[fx.f.slot].NativeRegistrationHold != fx.hold {
				t.Fatal("hold not kept durably", err)
			}
			if mode != "authority_unavailable" {
				return
			}
			// Once the authority answers, the next cycle reuses the same
			// intent and settles the exact registration.
			sealNativeWorker = func(_ admissioncontrol.Client, request admissioncontrol.SealRequest) (admissioncontrol.NativeOutcome, error) {
				fx.seals++
				return zeroAttemptOutcome(request), nil
			}
			if released, err := fx.reconcile(); err != nil || !released || fx.seals != 2 {
				t.Fatalf("recovered authority not used: released=%v err=%v seals=%d", released, err, fx.seals)
			}
			if _, err := os.Lstat(filepath.Join(fx.dir, "launch-abandoned-g1-1.receipt.json")); err != nil {
				t.Fatal("persisted intent was not reused for the archive", err)
			}
		})
	}
}

func TestExpiredNativePrelaunchKeepsHoldWithoutNeverLaunchedProof(t *testing.T) {
	for _, mode := range []string{"lease_active", "pane_present", "claim_present", "terminal_marker", "receipt_log_file", "receipt_pid", "receipt_issue_conflict", "projection_started", "projection_log_file"} {
		t.Run(mode, func(t *testing.T) {
			fx := expiredPrelaunch(t)
			f := fx.f
			want, uncertain := "", true
			rewrite := func(edit func(*NativeWorkerReceipt)) {
				r := *fx.receipt
				edit(&r)
				if err := writeNativeWorkerReceipt(fx.dir, &r); err != nil {
					t.Fatal(err)
				}
				b, err := os.ReadFile(filepath.Join(fx.dir, nativeReceiptName(1)))
				if err != nil {
					t.Fatal(err)
				}
				fx.before = b
			}
			switch mode {
			case "lease_active":
				workerProcessLeaseActive = func(tmuxsession.ProcessLease) (bool, error) { return true, nil }
				want = "native_process_unknown"
			case "pane_present":
				nativeWorkerPaneAbsent = func(string) (bool, error) { return false, nil }
				want = "native_host_runtime_unknown"
			case "claim_present":
				verifyNativeWorkerPrelaunchAbsence = func(aiexecution.FileProof, string, string, string) error {
					return aiexecution.Held("containment_prelaunch_claim_conflict")
				}
			case "terminal_marker":
				if err := os.WriteFile(filepath.Join(fx.dir, nativeReceiptName(1)+".terminated"), []byte("x"), 0600); err != nil {
					t.Fatal(err)
				}
				want = "native_recovery_terminal_conflict"
			case "receipt_log_file":
				rewrite(func(r *NativeWorkerReceipt) { r.LogFile = filepath.Join(state.LogDir(f.cfg.StateDir), f.slot+".log") })
				want, uncertain = "native_launch_abandonment_unproven", false
			case "receipt_pid":
				rewrite(func(r *NativeWorkerReceipt) { r.PID = 7 })
				want, uncertain = "native_launch_abandonment_unproven", false
			case "receipt_issue_conflict":
				rewrite(func(r *NativeWorkerReceipt) { r.IssueNumber++ })
				want, uncertain = "native_identity_conflict", false
			case "projection_started":
				f.st.Sessions[f.slot].StartedAt = time.Now().UTC()
				want, uncertain = "native_recovery_projection_conflict", false
			case "projection_log_file":
				f.st.Sessions[f.slot].LogFile = filepath.Join(state.LogDir(f.cfg.StateDir), f.slot+".log")
				want, uncertain = "native_recovery_projection_conflict", false
			}
			sessBefore := *f.st.Sessions[f.slot]
			released, err := fx.reconcile()
			if released || err == nil {
				t.Fatalf("unproven abandonment reconciled: released=%v err=%v", released, err)
			}
			if want != "" {
				expectNativeHold(t, err, want, uncertain)
			} else {
				var execHold *aiexecution.Hold
				if !errors.As(err, &execHold) || execHold.Code != "containment_prelaunch_claim_conflict" {
					t.Fatalf("err=%v", err)
				}
			}
			if fx.seals != 0 {
				t.Fatal("authority sealed before local absence was proven")
			}
			fx.assertUntouched(t, sessBefore)
		})
	}
}

// A hold raised before any registration has no receipt to retire: the slot is
// left untouched and no receipt directory is created for it.
func TestExpiredNativePrelaunchIgnoresHoldWithoutRegistration(t *testing.T) {
	fx := expiredPrelaunch(t)
	if err := os.RemoveAll(fx.dir); err != nil {
		t.Fatal(err)
	}
	if released, err := fx.reconcile(); err != nil || released || fx.seals != 0 {
		t.Fatalf("released=%v err=%v seals=%d", released, err, fx.seals)
	}
	if _, err := os.Lstat(fx.dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("receipt directory created for a pre-registration hold", err)
	}
	if fx.f.st.Sessions[fx.f.slot].NativeRegistrationHold != fx.hold {
		t.Fatal("pre-registration hold cleared")
	}
}

// The strict verified route is the only route whose containment pin proves
// pre-launch absence; without it nothing is reconciled.
func TestExpiredNativePrelaunchRequiresVerifiedRoute(t *testing.T) {
	fx := expiredPrelaunch(t)
	fx.f.cfg.AIExecution = aiexecution.Policy{}
	sessBefore := *fx.f.st.Sessions[fx.f.slot]
	if released, err := fx.reconcile(); err != nil || released || fx.seals != 0 {
		t.Fatalf("released=%v err=%v seals=%d", released, err, fx.seals)
	}
	fx.assertUntouched(t, sessBefore)
}

func TestNativePrelaunchExpiryCandidate(t *testing.T) {
	base := state.Session{IssueNumber: 21, Status: state.StatusFailed, NativeRegistrationHold: "binding_inventory_incomplete"}
	for name, tc := range map[string]struct {
		edit func(*state.Session)
		want bool
	}{
		"failed_first_generation_hold": {func(*state.Session) {}, true},
		"no_hold":                      {func(s *state.Session) { s.NativeRegistrationHold = "" }, false},
		"dead":                         {func(s *state.Session) { s.Status = state.StatusDead }, false},
		"retry_exhausted":              {func(s *state.Session) { s.Status = state.StatusRetryExhausted }, false},
		"launched_generation":          {func(s *state.Session) { s.WorkerGeneration = 1 }, false},
		"role_run_stamped":             {func(s *state.Session) { s.NativeRoleRunID = "00000000-0000-4000-8000-000000000001" }, false},
		"native_session_stamped":       {func(s *state.Session) { s.NativeSessionID = "00000000-0000-4000-8000-000000000002" }, false},
		"already_released":             {func(s *state.Session) { s.ReleasedForRedispatch = true }, false},
	} {
		sess := base
		tc.edit(&sess)
		if got := NativePrelaunchExpiryCandidate(&sess); got != tc.want {
			t.Fatalf("%s: got %v want %v", name, got, tc.want)
		}
	}
	if NativePrelaunchExpiryCandidate(nil) {
		t.Fatal("nil session is a candidate")
	}
}
