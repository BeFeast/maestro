package worker

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/befeast/maestro/internal/admissioncontrol"
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
