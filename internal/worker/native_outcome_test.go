package worker

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/befeast/maestro/internal/admissioncontrol"
	"github.com/befeast/maestro/internal/state"
)

func fixtureNativeOutcome(request admissioncontrol.SealRequest, held bool) admissioncontrol.NativeOutcome {
	outcome := admissioncontrol.NativeOutcome{Binding: request.Binding, RegistrationVersion: request.RegistrationVersion,
		Sealed: true, Outcome: "no_dispatch", NextGenerationAllowed: true, AttemptsDigest: strings.Repeat("a", 64)}
	if held {
		code := "outcome_unknown"
		outcome.Outcome = "held"
		outcome.NextGenerationAllowed = false
		outcome.HoldCode = &code
		outcome.PhysicalAttempts = 1
		outcome.UnresolvedAttempts = 1
	}
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
	outcome.EvidenceID = "native-outcome-v1:" + outcome.SnapshotDigest
	return outcome
}

func nativeOutcomeFixture(t *testing.T) *nativeFixture {
	f := nativeTestFixture(t)
	if _, err := f.start(); err != nil {
		t.Fatal(err)
	}
	previousNativeGenerationOutcome = persistedNativeGenerationOutcome
	old := sealNativeWorker
	t.Cleanup(func() { sealNativeWorker = old })
	return f
}

func TestNativeWorkerOutcomeIntentLostReplyCannotAdoptOrMintThenExactReconcile(t *testing.T) {
	f := nativeOutcomeFixture(t)
	sess := f.st.Sessions[f.slot]
	var saved admissioncontrol.SealRequest
	calls := 0
	sealNativeWorker = func(_ admissioncontrol.Client, request admissioncontrol.SealRequest) (admissioncontrol.NativeOutcome, error) {
		calls++
		receipt, err := readNativeWorkerReceipt(sess.NativeReceiptDir, 1)
		if err != nil || receipt.OutcomeIntent == nil || *receipt.OutcomeIntent != request {
			t.Fatal("seal preceded durable exact intent", err)
		}
		if calls == 1 {
			saved = request
			return admissioncontrol.NativeOutcome{}, errors.New("lost reply")
		}
		if request != saved {
			t.Fatal("seal changed identity")
		}
		return fixtureNativeOutcome(request, false), nil
	}
	_, err := ReconcileNativeWorkerOutcome(f.cfg, f.slot, 1)
	expectNativeHold(t, err, "outcome_authority_unavailable", false)
	if _, err := f.start(); err == nil {
		t.Fatal("adopted possibly sealed generation")
	}
	if f.spawned != 1 || len(f.registered) != 1 {
		t.Fatal("lost reply dispatched")
	}
	receipt, err := ReconcileNativeWorkerOutcome(f.cfg, f.slot, 1)
	if err != nil || receipt.Outcome == nil || !receipt.Outcome.NextGenerationAllowed {
		t.Fatal(err)
	}
	disk, err := readNativeWorkerReceipt(sess.NativeReceiptDir, 1)
	if err != nil || persistedNativeGenerationOutcome(nil, disk) != nil {
		t.Fatal("durable proof not usable", err)
	}
	// Explicit reentry reuses the saved allowed proof without another RPC.
	if _, err := ReconcileNativeWorkerOutcome(f.cfg, f.slot, 1); err != nil || calls != 2 {
		t.Fatal(err, calls)
	}
}

func TestNativeWorkerOutcomeHeldLateSettlementAndSuccessfulPhase(t *testing.T) {
	f := nativeOutcomeFixture(t)
	sess := f.st.Sessions[f.slot]
	f.cfg.WorkerLaunchContext = nil
	sess.Phase = state.PhaseAdvisor
	held := true
	sealNativeWorker = func(_ admissioncontrol.Client, request admissioncontrol.SealRequest) (admissioncontrol.NativeOutcome, error) {
		return fixtureNativeOutcome(request, held), nil
	}
	err := StartPhase(f.cfg, sess, f.slot, "accepted plan", "claude")
	expectNativeHold(t, err, "previous_outcome_unknown", false)
	receipt, err := readNativeWorkerReceipt(sess.NativeReceiptDir, 1)
	if err != nil || receipt.Outcome == nil || receipt.Outcome.NextGenerationAllowed || f.spawned != 1 || len(f.registered) != 1 {
		t.Fatal("held outcome dispatched", err)
	}
	oldID, oldRun := sess.NativeSessionID, sess.NativeRoleRunID
	held = false
	if err := StartPhase(f.cfg, sess, f.slot, "accepted plan", "claude"); err != nil {
		t.Fatal(err)
	}
	if f.spawned != 2 || len(f.registered) != 2 || sess.NativeSessionID == oldID || sess.NativeRoleRunID == oldRun || sess.NativeParentRoleRunID != oldRun {
		t.Fatal("late proof failed exact next generation")
	}
}

func TestNativeWorkerOutcomePersistenceFailureCannotAuthorizeRecovery(t *testing.T) {
	for _, failIntent := range []bool{true, false} {
		t.Run(map[bool]string{true: "intent", false: "snapshot"}[failIntent], func(t *testing.T) {
			f := nativeOutcomeFixture(t)
			sess := f.st.Sessions[f.slot]
			calls := 0
			sealNativeWorker = func(_ admissioncontrol.Client, request admissioncontrol.SealRequest) (admissioncontrol.NativeOutcome, error) {
				calls++
				return fixtureNativeOutcome(request, false), nil
			}
			prior := persistNativeWorkerReceipt
			persistNativeWorkerReceipt = func(dir string, r *NativeWorkerReceipt) error {
				if r.OutcomeIntent != nil && (failIntent || r.Outcome != nil) {
					return errors.New("disk full")
				}
				return prior(dir, r)
			}
			_, err := ReconcileNativeWorkerOutcome(f.cfg, f.slot, 1)
			expectNativeHold(t, err, "outcome_persistence_failed", false)
			receipt, err := readNativeWorkerReceipt(sess.NativeReceiptDir, 1)
			if err != nil || persistedNativeGenerationOutcome(nil, receipt) == nil {
				t.Fatal("non-durable reply granted")
			}
			if failIntent && calls != 0 || !failIntent && calls != 1 {
				t.Fatal("wrong RPC count", calls)
			}
		})
	}
}

func TestNativeWorkerOutcomeInvalidBindingConfigAndReceiptCannotRelease(t *testing.T) {
	f := nativeOutcomeFixture(t)
	sess := f.st.Sessions[f.slot]
	calls := 0
	sealNativeWorker = func(_ admissioncontrol.Client, request admissioncontrol.SealRequest) (admissioncontrol.NativeOutcome, error) {
		calls++
		o := fixtureNativeOutcome(request, false)
		o.Binding.Role = "foreign"
		return o, nil
	}
	_, err := ReconcileNativeWorkerOutcome(f.cfg, f.slot, 1)
	expectNativeHold(t, err, "outcome_response_invalid", false)
	f.cfg.WorkerNativeSessionRegistration.GatewayScope = "other"
	_, err = ReconcileNativeWorkerOutcome(f.cfg, f.slot, 1)
	expectNativeHold(t, err, "native_identity_conflict", false)
	if calls != 1 {
		t.Fatal("rebound authority")
	}
	f.cfg.WorkerNativeSessionRegistration.GatewayScope = "fixture-gateway"
	sealNativeWorker = func(_ admissioncontrol.Client, request admissioncontrol.SealRequest) (admissioncontrol.NativeOutcome, error) {
		return fixtureNativeOutcome(request, false), nil
	}
	receipt, err := ReconcileNativeWorkerOutcome(f.cfg, f.slot, 1)
	if err != nil {
		t.Fatal(err)
	}
	receipt.Outcome.EvidenceID = "forged"
	if err := writeNativeWorkerReceipt(sess.NativeReceiptDir, receipt); err != nil {
		t.Fatal(err)
	}
	if err := nativeWorkerDestructiveOutcome(nil, f.slot, sess); err == nil {
		t.Fatal("forged receipt allowed cleanup")
	}
	if _, err := os.Stat(filepath.Join(sess.NativeReceiptDir, nativeReceiptName(1))); err != nil {
		t.Fatal(err)
	}
}

func TestNativeWorkerOutcomePolicyRolloverAndExactLeaseRemainSeparate(t *testing.T) {
	f := nativeOutcomeFixture(t)
	sess := f.st.Sessions[f.slot]
	f.cfg.WorkerNativeSessionRegistration.ExpectedPolicyVersion = 2
	sealNativeWorker = func(_ admissioncontrol.Client, request admissioncontrol.SealRequest) (admissioncontrol.NativeOutcome, error) {
		if request.RegistrationVersion != 1 {
			t.Fatal("sealed current policy instead of saved binding")
		}
		return fixtureNativeOutcome(request, false), nil
	}
	if _, err := ReconcileNativeWorkerOutcome(f.cfg, f.slot, 1); err != nil {
		t.Fatal(err)
	}
	if !f.live {
		t.Fatal("financial seal signalled local process")
	}
	if terminal, err := NativeSessionProcessTerminal(f.cfg.StateDir, f.slot, sess); err != nil || terminal {
		t.Fatal("financial seal released OS occupancy", err)
	}
	if err := StopProcess(f.slot, sess); err != nil {
		t.Fatal(err)
	}
	if err := nativeWorkerDestructiveOutcome(nil, f.slot, sess); err != nil {
		t.Fatal(err)
	}
}
