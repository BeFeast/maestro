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
	"github.com/befeast/maestro/internal/aiexecution"
	"github.com/befeast/maestro/internal/state"
)

func TestObserveNativeWorkerTerminationIsExactAndPreservesReceipt(t *testing.T) {
	for _, mode := range []string{"terminated", "live", "wrong_native", "invalid_proof", "missing_proof"} {
		t.Run(mode, func(t *testing.T) {
			f, r, pin := runtimeGapFixture(t)
			path := filepath.Join(nativeReceiptDir(f.cfg.StateDir, f.slot), nativeReceiptName(r.Generation))
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			calls := 0
			verifyNativeWorkerTermination = func(got aiexecution.FileProof, native, unit string) (*aiexecution.NativeProcessTermination, error) {
				calls++
				if got != pin || native != r.Request.NativeSessionID || unit != r.ProcessLeaseUnit {
					t.Fatal("termination identity drift")
				}
				if mode == "missing_proof" {
					return nil, errors.New("missing")
				}
				proof := nativeRuntimeProof(r, pin, true)
				if mode == "invalid_proof" {
					proof.Digest = "invalid"
				}
				return proof, nil
			}
			native := r.Request.NativeSessionID
			if mode == "wrong_native" {
				native = "00000000-0000-4000-8000-000000000002"
			}
			if mode == "live" {
				f.live = true
			}
			proof, err := ObserveNativeWorkerTermination(f.cfg, f.slot, r.Generation, native)
			if mode == "terminated" && (err != nil || proof == nil) {
				t.Fatal("termination not observed", err)
			}
			if mode != "terminated" && (err == nil || proof != nil) {
				t.Fatal("unsafe termination accepted")
			}
			if (mode == "live" || mode == "wrong_native") && calls != 0 {
				t.Fatal("queried termination for wrong/live process")
			}
			after, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(before, after) || f.spawned != 0 || f.stopped != 0 {
				t.Fatal("observation mutated receipt or process")
			}
		})
	}
}

func operatorRecoveryDigest(t *testing.T, value any, omit ...string) string {
	t.Helper()
	b, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	var object map[string]any
	if err := d.Decode(&object); err != nil {
		t.Fatal(err)
	}
	for _, key := range omit {
		delete(object, key)
	}
	var canonical bytes.Buffer
	e := json.NewEncoder(&canonical)
	e.SetEscapeHTML(false)
	if err := e.Encode(object); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(bytes.TrimSuffix(canonical.Bytes(), []byte("\n")))
	return hex.EncodeToString(sum[:])
}

func refreshOperatorRecoveryOutcome(t *testing.T, outcome *admissioncontrol.NativeOutcome) {
	p := outcome.OperatorRetirement
	p.RequestDigest = operatorRecoveryDigest(t, p.Request())
	p.RetirementDigest = operatorRecoveryDigest(t, p, "retirement_digest")
	outcome.SnapshotDigest = operatorRecoveryDigest(t, outcome, "snapshot_digest", "evidence_id")
	outcome.EvidenceID = "native-outcome-v2:" + outcome.SnapshotDigest
}

func operatorRecoveryFixture(t *testing.T) (*nativeFixture, *NativeWorkerReceipt) {
	t.Helper()
	f, r, pin := runtimeGapFixture(t)
	r.Status = "launched"
	r.Request.AdmissionBasis = "requests"
	r.Acknowledgement.Binding = r.Request.Binding
	r.NativeProcessEvidence = nativeRuntimeProof(r, pin, true)
	seal := admissioncontrol.SealRequest{Binding: r.Request.Binding, RegistrationVersion: r.Acknowledgement.RegistrationVersion}
	prior := fixtureNativeOutcome(seal, true)
	proof := r.NativeProcessEvidence
	p := &admissioncontrol.OperatorRetirement{SchemaVersion: 1, OperatorUID: uint32(os.Geteuid()), Reason: "operator_reviewed_unknown_usage", PriorOutcome: prior,
		AttemptObservations:         []admissioncontrol.AttemptObservation{{PhysicalAttemptID: "11111111-1111-4111-8111-111111111111", ObservationDigest: strings.Repeat("b", 64)}},
		LocalTerminationAttestation: admissioncontrol.LocalTerminationAttestation{SchemaVersion: 1, NativeSessionID: r.Request.NativeSessionID, Unit: proof.Unit, Cgroup: proof.Cgroup, BootID: proof.BootID, InvocationID: proof.InvocationID, ProofDigest: proof.Digest}}
	outcome := prior
	outcome.OperatorRetirement = p
	outcome.Outcome, outcome.NextGenerationAllowed, outcome.HoldCode = "operator_retired_unknown", true, nil
	refreshOperatorRecoveryOutcome(t, &outcome)
	if err := admissioncontrol.ValidateNativeOutcome(outcome, seal); err != nil {
		t.Fatal("retirement fixture", err)
	}
	r.OutcomeIntent, r.Outcome = &seal, &outcome
	dir := nativeReceiptDir(f.cfg.StateDir, f.slot)
	if err := writeNativeWorkerReceipt(dir, r); err != nil {
		t.Fatal(err)
	}
	sess := f.st.Sessions[f.slot]
	sess.Status, sess.PID, sess.TmuxSession = state.StatusFailed, 0, ""
	sess.WorkerGeneration = r.Generation
	sess.ProcessLeaseUnit, sess.ProcessLeaseManager = r.ProcessLeaseUnit, r.ProcessLeaseManager
	(&nativeWorkerLaunch{dir: dir, receipt: r}).stamp(sess)
	sess.RetryCount, sess.UnexpectedExitRetries = 9, 8
	sess.WorkerOutcome = state.WorkerOutcomeRepeatedUnexpectedExit
	sess.RetryReason = state.RetryReasonStalledProgress
	sess.NativeRegistrationHold = "previous_outcome_unknown"
	f.cfg.MaxRetriesPerIssue = 1
	if err := markNativeWorkerTerminated(sess); err != nil {
		t.Fatal(err)
	}
	return f, r
}

func TestOperatorRetirementRecoveryPreservesAutomaticBudgetsAndIsOneGeneration(t *testing.T) {
	f, r := operatorRecoveryFixture(t)
	sess := f.st.Sessions[f.slot]
	oldOutcome := operatorRecoveryDigest(t, r.Outcome)
	if err := ScheduleNativeOperatorRecovery(f.cfg, f.st, f.slot, r.Request.NativeSessionID); err != nil {
		t.Fatal(err)
	}
	if sess.Status != state.StatusDead || sess.RetryCount != 9 || sess.UnexpectedExitRetries != 8 || sess.WorkerOutcome != state.WorkerOutcomeRepeatedUnexpectedExit || sess.NextRetryAt == nil || sess.RetryReason != state.RetryReasonOperatorRestart || sess.NativeRegistrationHold != "" || f.spawned != 0 {
		t.Fatal("operator recovery changed budgets/history or launched", sess)
	}
	after, err := readNativeWorkerReceipt(sess.NativeReceiptDir, r.Generation)
	if err != nil || after.OperatorRecovery == nil || after.OperatorRecovery.NextGeneration != r.Generation+1 || operatorRecoveryDigest(t, after.Outcome) != oldOutcome {
		t.Fatal("retirement/outcome changed", err)
	}
	queued := operatorRecoverySessionDigest(sess)
	if err := ScheduleNativeOperatorRecovery(f.cfg, f.st, f.slot, r.Request.NativeSessionID); err != nil || operatorRecoverySessionDigest(sess) != queued {
		t.Fatal("retry not idempotent", err)
	}
	// The ordinary dispatch path consumes NextRetryAt before attempting launch.
	sess.NextRetryAt = nil
	if err := ScheduleNativeOperatorRecovery(f.cfg, f.st, f.slot, r.Request.NativeSessionID); err == nil {
		t.Fatal("consumed one-off authorization reused")
	}
}

func TestOperatorRetirementRecoveryReplaysOnlyDurableUnconsumedIntent(t *testing.T) {
	f, r := operatorRecoveryFixture(t)
	sess := f.st.Sessions[f.slot]
	before := operatorRecoverySessionDigest(sess)
	oldPersist := persistNativeWorkerReceipt
	persistNativeWorkerReceipt = func(dir string, receipt *NativeWorkerReceipt) error {
		if err := oldPersist(dir, receipt); err != nil {
			return err
		}
		return errors.New("lost local reply after durable intent")
	}
	if err := ScheduleNativeOperatorRecovery(f.cfg, f.st, f.slot, r.Request.NativeSessionID); err == nil || operatorRecoverySessionDigest(sess) != before {
		t.Fatal("uncertain intent changed session")
	}
	persistNativeWorkerReceipt = oldPersist
	if err := ScheduleNativeOperatorRecovery(f.cfg, f.st, f.slot, r.Request.NativeSessionID); err != nil || sess.NextRetryAt == nil {
		t.Fatal("durable scheduling intent not recoverable", err)
	}
}

func TestOperatorRetirementRecoveryRejectsUnprovenOrWrongTermination(t *testing.T) {
	for _, mode := range []string{"held", "ordinary_allowed", "wrong_attestation", "missing_terminal_marker", "live_projection", "wrong_native"} {
		t.Run(mode, func(t *testing.T) {
			f, r := operatorRecoveryFixture(t)
			sess := f.st.Sessions[f.slot]
			native := r.Request.NativeSessionID
			switch mode {
			case "held":
				prior := r.Outcome.OperatorRetirement.PriorOutcome
				r.Outcome = &prior
			case "ordinary_allowed":
				outcome := fixtureNativeOutcome(*r.OutcomeIntent, false)
				r.Outcome = &outcome
			case "wrong_attestation":
				r.Outcome.OperatorRetirement.LocalTerminationAttestation.ProofDigest = strings.Repeat("c", 64)
				refreshOperatorRecoveryOutcome(t, r.Outcome)
			case "missing_terminal_marker":
				if err := os.Remove(filepath.Join(sess.NativeReceiptDir, nativeReceiptName(r.Generation)+".terminated")); err != nil {
					t.Fatal(err)
				}
			case "live_projection":
				sess.PID = 4321
			case "wrong_native":
				native = "22222222-2222-4222-8222-222222222222"
			}
			if err := writeNativeWorkerReceipt(sess.NativeReceiptDir, r); err != nil {
				t.Fatal(err)
			}
			before := operatorRecoverySessionDigest(sess)
			if err := ScheduleNativeOperatorRecovery(f.cfg, f.st, f.slot, native); err == nil || operatorRecoverySessionDigest(sess) != before || f.spawned != 0 {
				t.Fatal("unproven retirement scheduled")
			}
		})
	}
}
