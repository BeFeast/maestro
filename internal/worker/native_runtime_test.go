package worker

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/befeast/maestro/internal/admissioncontrol"
	"github.com/befeast/maestro/internal/aiexecution"
	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/state"
)

func nativeRuntimeProof(r *NativeWorkerReceipt, pin aiexecution.FileProof, terminal bool) *aiexecution.NativeProcessTermination {
	p := &aiexecution.NativeProcessTermination{Version: 1, Profile: pin, NativeSessionID: r.Request.NativeSessionID, Unit: r.ProcessLeaseUnit, Cgroup: "/test/" + r.ProcessLeaseUnit, BootID: "00000000-0000-4000-8000-000000000001", InvocationID: strings.Repeat("a", 32), StartedAt: time.Now().Add(-time.Minute).UTC(), LocalStatus: "launch_intent", ExitCode: -1}
	if terminal {
		p.EndedAt = time.Now().UTC()
		p.LocalStatus = "local_output_unknown"
	}
	b, _ := json.Marshal(p)
	sum := sha256.Sum256(b)
	p.Digest = hex.EncodeToString(sum[:])
	return p
}

func runtimeGapFixture(t *testing.T) (*nativeFixture, *NativeWorkerReceipt, aiexecution.FileProof) {
	f, r, _ := registeredPrelaunchFixture(t)
	f.cfg.AIExecution = observerTestPolicy(t, f.cfg.StateDir)
	r.Status = "launch_intent"
	r.ProcessLeaseUnit = "maestro-worker-0123456789abcdef0123456789abcdef-g1.service"
	r.ProcessLeaseManager = "system"
	r.LogFile = filepath.Join(f.cfg.StateDir, "logs", f.slot+".log")
	proof := workerExecutionProof{Version: 2, Policy: f.cfg.AIExecution, RuntimeKey: r.Slot, Worktree: r.Worktree, ProcessLeaseUnit: r.ProcessLeaseUnit, Spec: aiexecution.LaunchSpec{ProjectID: r.ProjectID, Role: r.Request.Role, Registration: r.Acknowledgement}}
	b, _ := json.Marshal(proof)
	if err := os.WriteFile(filepath.Join(f.cfg.StateDir, r.Slot+"-run.sh.execution.json"), b, 0600); err != nil {
		t.Fatal(err)
	}
	if err := writeNativeWorkerReceipt(nativeReceiptDir(f.cfg.StateDir, f.slot), r); err != nil {
		t.Fatal(err)
	}
	pin, err := nativeProfileFromReceipt(f.cfg, r)
	if err != nil {
		t.Fatal(err)
	}
	oldObserve, oldTerminate := observeNativeWorkerLaunch, verifyNativeWorkerTermination
	t.Cleanup(func() { observeNativeWorkerLaunch = oldObserve; verifyNativeWorkerTermination = oldTerminate })
	return f, r, pin
}

func TestNativeRuntimeReconcileRecoversKilledLaunchWithoutRespawnOrInventedSuccess(t *testing.T) {
	f, r, pin := runtimeGapFixture(t)
	observeNativeWorkerLaunch = func(aiexecution.FileProof, string, string, string) (*aiexecution.NativeProcessTermination, int, error) {
		return nil, 0, errors.New("inactive")
	}
	verifyNativeWorkerTermination = func(got aiexecution.FileProof, id, unit string) (*aiexecution.NativeProcessTermination, error) {
		if got != pin || id != r.Request.NativeSessionID || unit != r.ProcessLeaseUnit {
			t.Fatal("wrong native identity")
		}
		return nativeRuntimeProof(r, pin, true), nil
	}
	if err := ReconcileNativeWorkerRuntime(f.cfg, f.st, f.slot); err != nil {
		t.Fatal(err)
	}
	after, err := readNativeWorkerReceipt(nativeReceiptDir(f.cfg.StateDir, f.slot), 1)
	if err != nil {
		t.Fatal(err)
	}
	sess := f.st.Sessions[f.slot]
	if after.Status != "launched" || after.NativeProcessEvidence.LocalStatus != "local_output_unknown" || after.PID != 0 || after.Outcome != nil || after.OutcomeIntent != nil || after.Request != r.Request || f.spawned != 0 || f.stopped != 0 || sess.Status != state.StatusDead || sess.WorkerGeneration != 1 || sess.NativeSessionID != r.Request.NativeSessionID || sess.NativeRegistrationHold != "" {
		t.Fatal("incorrect gap recovery")
	}
	if terminal, err := NativeSessionProcessTerminal(f.cfg.StateDir, f.slot, sess); err != nil || !terminal {
		t.Fatal("termination not recovered", terminal, err)
	}
	f.cfg.MaxRetriesPerIssue = 1
	if err := ScheduleNativeWorkerRecovery(f.cfg, f.st, f.slot, r.Request.NativeSessionID); err == nil || sess.NextRetryAt != nil {
		t.Fatal("unsettled generation scheduled")
	}
	seal := admissioncontrol.SealRequest{Binding: after.Request.Binding, RegistrationVersion: after.Acknowledgement.RegistrationVersion}
	outcome := fixtureNativeOutcome(seal, false)
	after.OutcomeIntent = &seal
	after.Outcome = &outcome
	if err := writeNativeWorkerReceipt(sess.NativeReceiptDir, after); err != nil {
		t.Fatal(err)
	}
	if err := ScheduleNativeWorkerRecovery(f.cfg, f.st, f.slot, r.Request.NativeSessionID); err != nil {
		t.Fatal(err)
	}
	if sess.NextRetryAt == nil || sess.RetryCount != 1 || sess.UnexpectedExitRetries != 1 || sess.Status != state.StatusDead || f.spawned != 0 {
		t.Fatal("bounded retry not queued")
	}
	if err := ScheduleNativeWorkerRecovery(f.cfg, f.st, f.slot, r.Request.NativeSessionID); err != nil || sess.RetryCount != 1 {
		t.Fatal("queue decision not idempotent")
	}
	f.cfg.WorkerRuntime = config.WorkerRuntimeConfig{Mode: config.WorkerRuntimeModeIsolated, Scope: config.WorkerRuntimeScopeSystem}
	f.cfg.WorkerLaunchContext = nil
	previousNativeGenerationOutcome = persistedNativeGenerationOutcome
	registerNativeWorker = func(_ admissioncontrol.Client, request admissioncontrol.RegistrationRequest) (admissioncontrol.Acknowledgement, error) {
		saved, err := readNativeWorkerReceipt(sess.NativeReceiptDir, 2)
		if err != nil || saved.Status != "registration_intent" || saved.Request != request {
			t.Fatal("successor registration was not durable", err)
		}
		return admissioncontrol.Acknowledgement{Binding: request.Binding, RegistrationVersion: 1}, nil
	}
	next, err := prepareNativeWorker(f.cfg, sess, f.slot, "claude", workerBackendConfig(f.cfg.Model.Backends["claude"]), 2, r.IssueNumber, r.Worktree, r.Branch)
	if err != nil {
		t.Fatal("settled generation could not register successor", err)
	}
	defer next.close()
	if next.receipt.Generation != 2 || next.receipt.Request.NativeSessionID == r.Request.NativeSessionID || next.receipt.Request.RunID != r.Request.RunID || next.receipt.Request.GatewayScope != r.Request.GatewayScope || next.receipt.Status != "registered" || f.spawned != 0 {
		t.Fatal("successor changed budget run or relaunched old identity")
	}
}

func TestNativeRuntimeReconcileAdoptsActualMonitorSeparatelyFromHostPane(t *testing.T) {
	f, r, pin := runtimeGapFixture(t)
	f.live = true
	observeNativeWorkerLaunch = func(aiexecution.FileProof, string, string, string) (*aiexecution.NativeProcessTermination, int, error) {
		return nativeRuntimeProof(r, pin, false), 9898, nil
	}
	verifyNativeWorkerTermination = func(aiexecution.FileProof, string, string) (*aiexecution.NativeProcessTermination, error) {
		t.Fatal("live worker queried for termination")
		return nil, nil
	}
	if err := ReconcileNativeWorkerRuntime(f.cfg, f.st, f.slot); err != nil {
		t.Fatal(err)
	}
	after, err := readNativeWorkerReceipt(nativeReceiptDir(f.cfg.StateDir, f.slot), 1)
	if err != nil {
		t.Fatal(err)
	}
	if after.PID != 9898 || f.st.Sessions[f.slot].PID != 4242 || f.st.Sessions[f.slot].Status != state.StatusRunning || f.spawned != 0 || f.stopped != 0 {
		t.Fatal("monitor/pane identities conflated")
	}
}

func TestNativeLaunchHandshakeWaitsForDurableClaim(t *testing.T) {
	f, r, pin := runtimeGapFixture(t)
	calls := 0
	observeNativeWorkerLaunch = func(aiexecution.FileProof, string, string, string) (*aiexecution.NativeProcessTermination, int, error) {
		calls++
		if calls == 1 {
			return nil, 0, errors.New("claim not written yet")
		}
		return nativeRuntimeProof(r, pin, false), 9898, nil
	}
	if err := waitNativeWorkerLaunch(f.cfg, f.slot, 1, time.Second); err != nil || calls != 2 {
		t.Fatal("handshake did not wait for native claim", calls, err)
	}
}

func TestNativeLeaseCleanupProtectsFailedAndMissingProjection(t *testing.T) {
	for _, mode := range []string{"failed", "missing", "malformed_receipt", "terminated_unsettled"} {
		t.Run(mode, func(t *testing.T) {
			f, r, _ := registeredPrelaunchFixture(t)
			runtimeCfg := isolatedRuntimeConfig(t, f.cfg.ProjectID)
			f.cfg.WorkerRuntime = runtimeCfg.WorkerRuntime
			lease := prepareRuntimeLease(t, f.cfg, f.slot)
			r.Status = "launch_intent"
			r.ProcessLeaseUnit = lease.Unit
			r.ProcessLeaseManager = lease.Scope
			r.LogFile = filepath.Join(f.cfg.StateDir, "logs", f.slot+".log")
			dir := nativeReceiptDir(f.cfg.StateDir, f.slot)
			if err := writeNativeWorkerReceipt(dir, r); err != nil {
				t.Fatal(err)
			}
			if mode == "missing" {
				delete(f.st.Sessions, f.slot)
			}
			if mode == "malformed_receipt" {
				_ = os.WriteFile(filepath.Join(dir, nativeReceiptName(1)), []byte("bad"), 0600)
			}
			if mode == "terminated_unsettled" {
				b, _ := json.Marshal(terminationFor(r))
				_ = os.WriteFile(filepath.Join(dir, nativeReceiptName(1)+".terminated"), b, 0600)
			}
			ops := &fakeWorkerLeaseOps{active: map[string]bool{lease.Unit: true}}
			result := reconcileWorkerLeasesWithOps(f.cfg, f.st, time.Now(), ops)
			if len(ops.stopped) != 0 || len(ops.cleaned) != 0 || len(result.Cleaned) != 0 || result.Attention == 0 {
				t.Fatal("native launch uncertainty was killed/cleaned", result)
			}
		})
	}
}
