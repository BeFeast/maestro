package worker

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/befeast/maestro/internal/admissioncontrol"
	"github.com/befeast/maestro/internal/aiexecution"
	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/state"
	"github.com/befeast/maestro/internal/tmuxsession"
)

func registeredPrelaunchFixture(t *testing.T) (*nativeFixture, *NativeWorkerReceipt, *int) {
	t.Helper()
	f := nativeTestFixture(t)
	f.cfg.Hooks.BeforeRun = "exit 23"
	_, err := f.start()
	expectNativeHold(t, err, "setup_failed", false)
	f.cfg.Hooks.BeforeRun = ""
	r, err := readNativeWorkerReceipt(nativeReceiptDir(f.cfg.StateDir, f.slot), 1)
	if err != nil || r.Status != "registered" || f.spawned != 0 {
		t.Fatalf("prelaunch fixture: receipt=%+v error=%v", r, err)
	}
	calls := 0
	registerNativeWorker = func(_ admissioncontrol.Client, request admissioncontrol.RegistrationRequest) (admissioncontrol.Acknowledgement, error) {
		calls++
		if request != r.Request {
			t.Fatal("recovery changed the saved registration request")
		}
		return *r.Acknowledgement, nil
	}
	return f, r, &calls
}

func TestNativeHostRecoveryRejectsOrphanedExactRunner(t *testing.T) {
	path := filepath.Join(t.TempDir(), "prior-run.sh")
	cmd := exec.Command("/bin/sleep", "30")
	cmd.Args[0] = path
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	err := verifyHostRunnerAbsent(path)
	expectNativeHold(t, err, "native_host_process_exists", true)
}

func TestNativeFailedHostRecoveryRetainsIntentAndArchivesProof(t *testing.T) {
	f, receipt, calls := registeredPrelaunchFixture(t)
	f.cfg.AIExecution = observerTestPolicy(t, f.cfg.StateDir)
	f.cfg.WorkerRuntime = config.WorkerRuntimeConfig{Mode: config.WorkerRuntimeModeIsolated, Scope: config.WorkerRuntimeScopeSystem}
	profileBytes, err := os.ReadFile(os.Getenv("MAESTRO_TEST_HOST_OBSERVER_PROFILE"))
	if err != nil {
		t.Fatal(err)
	}
	var profile aiexecution.NativeContainmentProfile
	if err := json.Unmarshal(profileBytes, &profile); err != nil {
		t.Fatal(err)
	}
	f.cfg.ProjectID, receipt.ProjectID, receipt.Request.ProjectID, receipt.Acknowledgement.Binding.ProjectID = profile.ProjectID, profile.ProjectID, profile.ProjectID, profile.ProjectID
	receipt.Status = "launch_intent"
	receipt.LogFile = filepath.Join(state.LogDir(f.cfg.StateDir), f.slot+".log")
	lease, err := workerProcessLease(f.cfg, f.slot, 1)
	if err != nil {
		t.Fatal(err)
	}
	receipt.ProcessLeaseUnit = lease.Unit
	controller, err := f.cfg.AIExecution.LiveControllerLease()
	if err != nil {
		t.Fatal(err)
	}
	controller.StartTicks = "prior-process-identity"
	proof := workerExecutionProof{Version: 1, Policy: f.cfg.AIExecution, ControllerLease: controller, RuntimeKey: f.slot, Worktree: receipt.Worktree, ProcessLeaseUnit: lease.Unit, Spec: aiexecution.LaunchSpec{ProjectID: receipt.ProjectID, Registration: receipt.Acknowledgement}}
	b, _ := json.Marshal(proof)
	proofPath := filepath.Join(f.cfg.StateDir, f.slot+"-run.sh.execution.json")
	if err := os.WriteFile(proofPath, b, 0600); err != nil {
		t.Fatal(err)
	}
	dir := nativeReceiptDir(f.cfg.StateDir, f.slot)
	if err := writeNativeWorkerReceipt(dir, receipt); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(filepath.Join(dir, nativeReceiptName(1)))
	client, err := nativeClient(f.cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := authorizeRegisteredWorkerRecovery(f.cfg, client, dir, receipt, lease, receipt.Request.NativeSessionID); err != nil {
		t.Fatal(err)
	}
	if *calls != 1 || receipt.Status != "launch_intent" || len(receipt.PrelaunchRecoveries) != 1 {
		t.Fatal("history reset", receipt.Status, *calls)
	}
	sum := sha256.Sum256(before)
	if receipt.PrelaunchRecoveries[0].ReceiptSHA256 != hex.EncodeToString(sum[:]) {
		t.Fatal("receipt hash mismatch")
	}
	archived, _ := os.ReadFile(filepath.Join(dir, "prelaunch-recovery-g1-1.receipt.json"))
	if !reflect.DeepEqual(archived, before) {
		t.Fatal("prior receipt not archived")
	}
	archived, _ = os.ReadFile(filepath.Join(dir, "prelaunch-recovery-g1-1.execution.json"))
	if !reflect.DeepEqual(archived, b) {
		t.Fatal("prior proof not archived")
	}
	n := &nativeWorkerLaunch{dir: dir, receipt: receipt, recoverPrelaunch: true}
	if err := n.beginLaunch(receipt.LogFile); err != nil {
		t.Fatal(err)
	}
	if receipt.Status != "launch_intent" || receipt.Generation != 1 {
		t.Fatal("recovery changed generation/status")
	}
	if slots, err := NativePendingSlots(f.cfg.StateDir, nil); err != nil || len(slots) != 1 || slots[0] != f.slot {
		t.Fatal("archives changed pending occupancy", slots, err)
	}
}

func TestNativeWorkerExplicitPrelaunchRecoveryPreservesIdentity(t *testing.T) {
	f, before, calls := registeredPrelaunchFixture(t)
	_, err := f.start()
	expectNativeHold(t, err, "unresolved_registration", false)
	if *calls != 0 || f.spawned != 0 {
		t.Fatal("ordinary start replayed the held registration")
	}
	got, err := RecoverRegisteredWorkerStart(f.cfg, f.st, f.cfg.Repo, f.issue, "fixture task", f.slot, before.Request.NativeSessionID)
	if err != nil || got != f.slot || f.spawned != 1 || *calls != 1 {
		t.Fatalf("recovery slot=%s error=%v spawned=%d checks=%d", got, err, f.spawned, *calls)
	}
	after, err := readNativeWorkerReceipt(nativeReceiptDir(f.cfg.StateDir, f.slot), 1)
	if err != nil || after.Status != "launched" || after.RoleRunID != before.RoleRunID || after.Request != before.Request || after.Generation != 1 || after.ConfigDigest != before.ConfigDigest {
		t.Fatalf("recovery replaced registered identity: %+v, %v", after, err)
	}
	sess := f.st.Sessions[f.slot]
	if sess.Status != state.StatusRunning || sess.NativeRegistrationHold != "" || sess.WorkerGeneration != 1 || sess.NativeSessionID != before.Request.NativeSessionID || sess.NativeRoleRunID != before.RoleRunID || sess.RetryCount != 0 {
		t.Fatalf("wrong recovered projection: %+v", sess)
	}
	if _, err := RecoverRegisteredWorkerStart(f.cfg, f.st, f.cfg.Repo, f.issue, "fixture task", f.slot, before.Request.NativeSessionID); err == nil || f.spawned != 1 {
		t.Fatal("explicit recovery launched twice")
	}
}

func TestNativeWorkerExplicitPrelaunchRecoveryRefusesUncertainOrChangedEvidence(t *testing.T) {
	for _, kind := range []string{"foreign_uuid", "registration_intent", "reconciled", "launch_intent", "launched", "expired", "pid", "log", "terminal", "lease_active", "lease_unknown", "pane_present", "pane_unknown", "authority_unknown", "revoked", "ack_changed", "scope_changed", "projection_running", "sibling", "missing_receipt"} {
		t.Run(kind, func(t *testing.T) {
			f, r, _ := registeredPrelaunchFixture(t)
			expected := r.Request.NativeSessionID
			dir := nativeReceiptDir(f.cfg.StateDir, f.slot)
			switch kind {
			case "foreign_uuid":
				expected = "00000000-0000-4000-8000-000000000001"
			case "registration_intent":
				r.Status, r.Acknowledgement = "registration_intent", nil
			case "reconciled":
				r.Status = "registration_reconciled_not_launched"
			case "launch_intent", "launched":
				r.Status, r.LogFile = kind, filepath.Join(f.cfg.StateDir, "worker.log")
			case "expired":
				r.Request.ExpiresAt = time.Now().Unix() - 1
				r.Acknowledgement.Binding.ExpiresAt = r.Request.ExpiresAt
			case "pid":
				r.PID = 4242
			case "log":
				r.LogFile = filepath.Join(f.cfg.StateDir, "worker.log")
			case "terminal":
				if err := os.WriteFile(filepath.Join(dir, nativeReceiptName(1)+".terminated"), []byte("invalid"), 0600); err != nil {
					t.Fatal(err)
				}
			case "lease_active":
				workerProcessLeaseActive = func(tmuxsession.ProcessLease) (bool, error) { return true, nil }
			case "lease_unknown":
				workerProcessLeaseActive = func(tmuxsession.ProcessLease) (bool, error) { return false, errors.New("unknown") }
			case "pane_present":
				nativeWorkerPaneAbsent = func(string) (bool, error) { return false, nil }
			case "pane_unknown":
				nativeWorkerPaneAbsent = func(string) (bool, error) { return false, errors.New("unknown") }
			case "authority_unknown":
				registerNativeWorker = func(admissioncontrol.Client, admissioncontrol.RegistrationRequest) (admissioncontrol.Acknowledgement, error) {
					return admissioncontrol.Acknowledgement{}, errors.New("unknown")
				}
			case "revoked", "ack_changed":
				registerNativeWorker = func(admissioncontrol.Client, admissioncontrol.RegistrationRequest) (admissioncontrol.Acknowledgement, error) {
					ack := *r.Acknowledgement
					if kind == "revoked" {
						ack.Revoked = true
					} else {
						ack.RegistrationVersion++
					}
					return ack, nil
				}
			case "scope_changed":
				f.cfg.WorkerNativeSessionRegistration.GatewayScope = "foreign"
			case "projection_running":
				f.st.Sessions[f.slot].Status = state.StatusRunning
			case "sibling":
				f.st.Sessions["other-slot"] = &state.Session{IssueNumber: f.issue.Number}
			}
			if err := writeNativeWorkerReceipt(dir, r); err != nil {
				t.Fatal(err)
			}
			if kind == "missing_receipt" {
				if err := os.Remove(filepath.Join(dir, nativeReceiptName(1))); err != nil {
					t.Fatal(err)
				}
			}
			before, _ := os.ReadFile(filepath.Join(dir, nativeReceiptName(1)))
			_, err := RecoverRegisteredWorkerStart(f.cfg, f.st, f.cfg.Repo, f.issue, "fixture task", f.slot, expected)
			if err == nil || f.spawned != 0 || f.stopped != 0 {
				t.Fatalf("unsafe recovery: %v spawned=%d stopped=%d", err, f.spawned, f.stopped)
			}
			after, _ := os.ReadFile(filepath.Join(dir, nativeReceiptName(1)))
			if !reflect.DeepEqual(before, after) {
				t.Fatal("refused recovery modified durable receipt")
			}
			if _, err := os.Stat(filepath.Join(dir, nativeReceiptName(2))); !os.IsNotExist(err) {
				t.Fatal("refused recovery created a generation")
			}
		})
	}
}
