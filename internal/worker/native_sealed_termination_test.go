package worker

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/befeast/maestro/internal/admissioncontrol"
	"github.com/befeast/maestro/internal/aiexecution"
	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/state"
)

func sealedTerminationGapFixture(t *testing.T) (*nativeFixture, *NativeWorkerReceipt, aiexecution.FileProof) {
	t.Helper()
	f, r, pin := runtimeGapFixture(t)
	f.cfg.WorkerNativeSessionRegistration.AdmissionBasis = "requests"
	r.Request.AdmissionBasis = "requests"
	r.Acknowledgement.Binding.AdmissionBasis = "requests"
	r.Status = "launched"
	r.NativeProcessEvidence = nativeRuntimeProof(r, pin, false)
	seal := admissioncontrol.SealRequest{Binding: r.Request.Binding, RegistrationVersion: r.Acknowledgement.RegistrationVersion}
	outcome := fixtureNativeOutcome(seal, false)
	r.OutcomeIntent, r.Outcome = &seal, &outcome
	dir := nativeReceiptDir(f.cfg.StateDir, f.slot)
	if err := writeNativeWorkerReceipt(dir, r); err != nil {
		t.Fatal(err)
	}
	proof := workerExecutionProof{Version: 2, Policy: f.cfg.AIExecution, RuntimeKey: r.Slot, Worktree: r.Worktree, ProcessLeaseUnit: r.ProcessLeaseUnit, Spec: aiexecution.LaunchSpec{ProjectID: r.ProjectID, Role: r.Request.Role, Registration: r.Acknowledgement}}
	b, _ := json.Marshal(proof)
	if err := os.WriteFile(filepath.Join(f.cfg.StateDir, r.Slot+"-run.sh.execution.json"), b, 0600); err != nil {
		t.Fatal(err)
	}
	sess := f.st.Sessions[f.slot]
	(&nativeWorkerLaunch{dir: dir, receipt: r}).stamp(sess)
	sess.WorkerGeneration, sess.PRNumber = r.Generation, 20
	sess.Status, sess.NativeRegistrationHold = state.StatusRetryExhausted, "native_generation_sealed"
	sess.RetryCount, sess.UnexpectedExitRetries = 1, 1
	sess.PID, sess.TmuxSession = 0, ""
	clearSessionProcessLease(sess)
	return f, r, pin
}

func TestSealedNativeTerminationRecoversOnlyOSProjectionAndPreservesRepairIdentity(t *testing.T) {
	f, r, pin := sealedTerminationGapFixture(t)
	sess := f.st.Sessions[f.slot]
	before := *sess
	verified := 0
	verifyNativeWorkerTermination = func(got aiexecution.FileProof, nativeID, unit string) (*aiexecution.NativeProcessTermination, error) {
		verified++
		if got != pin || nativeID != r.Request.NativeSessionID || unit != r.ProcessLeaseUnit {
			t.Fatal("wrong original process identity")
		}
		return nativeRuntimeProof(r, pin, true), nil
	}
	oldSeal := sealNativeWorker
	t.Cleanup(func() { sealNativeWorker = oldSeal })
	sealNativeWorker = func(admissioncontrol.Client, admissioncontrol.SealRequest) (admissioncontrol.NativeOutcome, error) {
		t.Fatal("OS reconciliation must not reseal authority outcome")
		return admissioncontrol.NativeOutcome{}, nil
	}
	for i := 0; i < 2; i++ {
		if err := ReconcileNativeWorkerTermination(f.cfg, f.slot, sess); err != nil {
			t.Fatal(err)
		}
	}
	if verified != 1 || !reflect.DeepEqual(*sess, before) || f.spawned != 0 || f.stopped != 0 {
		t.Fatal("reconciliation changed session/history or was not idempotent")
	}
	after, err := readNativeWorkerReceipt(sess.NativeReceiptDir, sess.WorkerGeneration)
	if err != nil {
		t.Fatal(err)
	}
	want := *r
	want.NativeProcessEvidence = after.NativeProcessEvidence
	if !reflect.DeepEqual(*after, want) || after.NativeProcessEvidence.LocalStatus != "local_output_unknown" || after.NativeProcessEvidence.EndedAt.IsZero() {
		t.Fatal("receipt changed beyond exact OS termination evidence")
	}
	if terminal, err := NativeSessionProcessTerminal(f.cfg.StateDir, f.slot, sess); err != nil || !terminal {
		t.Fatal("terminal marker missing", err)
	}
	if started, err := NativeWorkerSealStarted(f.cfg.StateDir, f.slot, sess); err != nil || started {
		t.Fatal("normal sealed generation still held", err)
	}
	// The existing explicit repair path can now register a successor in the
	// same retained slot and budget run; OS reconciliation never does so itself.
	f.cfg.WorkerRuntime = config.WorkerRuntimeConfig{Mode: config.WorkerRuntimeModeIsolated, Scope: config.WorkerRuntimeScopeSystem}
	f.cfg.WorkerLaunchContext = &config.WorkerLaunchContext{Role: "repair", ParentRoleRunID: r.RoleRunID}
	previousNativeGenerationOutcome = persistedNativeGenerationOutcome
	registerNativeWorker = func(_ admissioncontrol.Client, req admissioncontrol.RegistrationRequest) (admissioncontrol.Acknowledgement, error) {
		return admissioncontrol.Acknowledgement{Binding: req.Binding, RegistrationVersion: 1}, nil
	}
	next, err := prepareNativeWorker(f.cfg, sess, f.slot, "claude", workerBackendConfig(f.cfg.Model.Backends["claude"]), 2, r.IssueNumber, r.Worktree, r.Branch)
	if err != nil {
		t.Fatal("retained repair successor blocked", err)
	}
	defer next.close()
	if next.receipt.Request.RunID != r.Request.RunID || next.receipt.ParentRoleRunID != r.RoleRunID || next.receipt.Slot != r.Slot || next.receipt.Worktree != r.Worktree || next.receipt.Branch != r.Branch || next.receipt.Request.Role != "repair" || sess.PRNumber != 20 || sess.RetryCount != 1 || f.spawned != 0 {
		t.Fatal("repair successor did not retain ancestry, worktree, PR and budget")
	}
}

func TestSealedNativeTerminationFailsClosedWithoutChangingEvidence(t *testing.T) {
	for _, mode := range []string{"active", "missing_proof", "invalid_proof", "unsettled", "held", "worktree_drift", "partial_lease", "unverified_route", "receipt_write_failure"} {
		t.Run(mode, func(t *testing.T) {
			f, r, pin := sealedTerminationGapFixture(t)
			sess := f.st.Sessions[f.slot]
			verifyNativeWorkerTermination = func(aiexecution.FileProof, string, string) (*aiexecution.NativeProcessTermination, error) {
				if mode == "missing_proof" {
					return nil, errors.New("original runtime unobservable")
				}
				p := nativeRuntimeProof(r, pin, true)
				if mode == "invalid_proof" {
					p.Unit = "another.service"
				}
				return p, nil
			}
			switch mode {
			case "active":
				f.live = true
			case "unsettled":
				r.Outcome = nil
			case "held":
				o := fixtureNativeOutcome(*r.OutcomeIntent, true)
				r.Outcome = &o
			case "worktree_drift":
				sess.Worktree += "-other"
			case "partial_lease":
				sess.ProcessLeaseManager = "system"
			case "unverified_route":
				f.cfg.AIExecution.RequireVerifiedRoute = false
			case "receipt_write_failure":
				persistNativeWorkerReceipt = func(string, *NativeWorkerReceipt) error { return errors.New("disk full") }
			}
			if err := writeNativeWorkerReceipt(sess.NativeReceiptDir, r); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(sess.NativeReceiptDir, nativeReceiptName(r.Generation))
			beforeBytes, _ := os.ReadFile(path)
			beforeSession := *sess
			if err := ReconcileNativeWorkerTermination(f.cfg, f.slot, sess); err == nil {
				t.Fatal("unsafe termination accepted")
			}
			afterBytes, _ := os.ReadFile(path)
			if !bytes.Equal(beforeBytes, afterBytes) || !reflect.DeepEqual(*sess, beforeSession) || f.spawned != 0 || f.stopped != 0 {
				t.Fatal("failed reconciliation changed evidence or session")
			}
			if _, err := os.Lstat(path + ".terminated"); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("failed reconciliation wrote a marker")
			}
		})
	}
}

func TestSealedNativeTerminationMarkerFailureIsRetryable(t *testing.T) {
	f, r, pin := sealedTerminationGapFixture(t)
	sess := f.st.Sessions[f.slot]
	before := *sess
	verifyNativeWorkerTermination = func(aiexecution.FileProof, string, string) (*aiexecution.NativeProcessTermination, error) {
		return nativeRuntimeProof(r, pin, true), nil
	}
	marker := filepath.Join(sess.NativeReceiptDir, nativeReceiptName(r.Generation)+".terminated")
	// Fail marker rename after the verified receipt has been persisted. This
	// models the partial-write boundary without weakening either durable check.
	persistNativeWorkerReceipt = func(dir string, receipt *NativeWorkerReceipt) error {
		if err := writeNativeWorkerReceipt(dir, receipt); err != nil {
			return err
		}
		return os.Mkdir(marker, 0700)
	}
	if err := ReconcileNativeWorkerTermination(f.cfg, f.slot, sess); err == nil {
		t.Fatal("marker failure was ignored")
	}
	if !reflect.DeepEqual(*sess, before) {
		t.Fatal("partial persistence changed held session")
	}
	after, err := readNativeWorkerReceipt(sess.NativeReceiptDir, r.Generation)
	if err != nil || after.NativeProcessEvidence.EndedAt.IsZero() || !reflect.DeepEqual(after.Outcome, r.Outcome) {
		t.Fatal("verified proof or sealed outcome lost", err)
	}
	if terminal, err := NativeSessionProcessTerminal(f.cfg.StateDir, f.slot, sess); err == nil || terminal {
		t.Fatal("incomplete marker accepted")
	}
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	persistNativeWorkerReceipt = writeNativeWorkerReceipt
	if err := ReconcileNativeWorkerTermination(f.cfg, f.slot, sess); err != nil {
		t.Fatal("partial persistence cannot be retried", err)
	}
	if terminal, err := NativeSessionProcessTerminal(f.cfg.StateDir, f.slot, sess); err != nil || !terminal || !reflect.DeepEqual(*sess, before) {
		t.Fatal("retry did not preserve session and restore marker", err)
	}
}
