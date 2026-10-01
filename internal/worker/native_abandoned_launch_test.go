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

	"github.com/befeast/maestro/internal/admissioncontrol"
	"github.com/befeast/maestro/internal/aiexecution"
	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/state"
	"github.com/befeast/maestro/internal/tmuxsession"
	"github.com/befeast/maestro/internal/workerlease"
	"github.com/google/uuid"
)

// syntheticVerifiedRoutePolicy pins a synthetic containment manifest so the
// receipt/proof identity checks run without a root-owned host profile. Every
// host observation behind the pin is replaced by package seams in the tests.
func syntheticVerifiedRoutePolicy(t *testing.T, dir string) aiexecution.Policy {
	t.Helper()
	profile := filepath.Join(dir, "synthetic-profile.json")
	if err := os.WriteFile(profile, []byte(`{"synthetic":true}`), 0600); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(profile)
	sum := sha256.Sum256(b)
	m := aiexecution.Manifest{Version: 1, EvidenceKind: "synthetic-no-launch", Runtime: aiexecution.RuntimeExpectation{ManagementKeyEnv: "MAESTRO_TEST_HOST_KEY"}, Containment: map[string]aiexecution.FileProof{"worker": {Path: profile, SHA256: hex.EncodeToString(sum[:])}}}
	b, _ = json.Marshal(m)
	path := filepath.Join(dir, "manifest.json")
	if err := os.WriteFile(path, b, 0600); err != nil {
		t.Fatal(err)
	}
	sum = sha256.Sum256(b)
	return aiexecution.Policy{RequireVerifiedRoute: true, ManifestPath: path, ManifestSHA256: hex.EncodeToString(sum[:])}
}

type abandonedLaunchFixture struct {
	f            *nativeFixture
	dir          string
	parent, next *NativeWorkerReceipt
	proof        []byte
	scratch      workerlease.Lease
	seals        int
	absence      int
}

// abandonedLaunchGap reproduces #1235: generation 1 is launched and sealed,
// the session projection was restored to it with native_registration_hold
// unresolved_launch, and the generation-2 receipt (child of generation 1)
// is still launch_intent with no PID, claim, unit or pane because the host
// runner refused before creating any process.
func abandonedLaunchGap(t *testing.T) *abandonedLaunchFixture {
	t.Helper()
	f, parent, _ := registeredPrelaunchFixture(t)
	f.cfg.AIExecution = syntheticVerifiedRoutePolicy(t, f.cfg.StateDir)
	runtimeCfg := isolatedRuntimeConfig(t, f.cfg.ProjectID)
	f.cfg.WorkerRuntime = runtimeCfg.WorkerRuntime
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
	scratch := prepareRuntimeLease(t, f.cfg, f.slot)
	next := *parent
	next.Generation = 2
	next.RoleRunID = uuid.NewString()
	next.ParentRoleRunID = parent.RoleRunID
	next.Request.NativeSessionID = uuid.NewString()
	next.Request.Role = "repair"
	ack := admissioncontrol.Acknowledgement{Binding: next.Request.Binding, RegistrationVersion: 1}
	next.Acknowledgement = &ack
	next.Status = "launch_intent"
	next.PID = 0
	next.ProcessLeaseUnit, next.ProcessLeaseManager = scratch.Unit, scratch.Scope
	next.OutcomeIntent, next.Outcome, next.NativeProcessEvidence = nil, nil, nil
	if err := writeNativeWorkerReceipt(dir, &next); err != nil {
		t.Fatal(err)
	}
	proof := workerExecutionProof{Version: 2, Policy: f.cfg.AIExecution, RuntimeKey: f.slot, Worktree: next.Worktree, ProcessLeaseUnit: next.ProcessLeaseUnit, Spec: aiexecution.LaunchSpec{ProjectID: next.ProjectID, Role: next.Request.Role, Registration: next.Acknowledgement}}
	proofBytes, _ := json.Marshal(proof)
	if err := os.WriteFile(filepath.Join(f.cfg.StateDir, f.slot+"-run.sh.execution.json"), proofBytes, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(parent.LogFile, []byte("[maestro] worker exec: starting\n[maestro] worker exec: AI execution held: containment_forgejo_authorization_unverified\n"), 0600); err != nil {
		t.Fatal(err)
	}
	sess := f.st.Sessions[f.slot]
	sess.Status = state.StatusFailed
	sess.WorkerGeneration = 1
	sess.NativeSessionID = parent.Request.NativeSessionID
	sess.NativeRoleRunID = parent.RoleRunID
	sess.NativeRole = parent.Request.Role
	sess.NativeReceiptDir = dir
	sess.NativeRegistrationHold = "unresolved_launch"
	sess.RetryCount, sess.UnexpectedExitRetries = 1, 1
	sess.PID, sess.TmuxSession = 0, ""
	if err := state.Save(f.cfg.StateDir, f.st); err != nil {
		t.Fatal(err)
	}
	fx := &abandonedLaunchFixture{f: f, dir: dir, parent: parent, next: &next, proof: proofBytes, scratch: scratch}
	oldSeal, oldAbsence := sealNativeWorker, verifyNativeWorkerPrelaunchAbsence
	t.Cleanup(func() { sealNativeWorker = oldSeal; verifyNativeWorkerPrelaunchAbsence = oldAbsence })
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
	return fx
}

func TestNativeRuntimeReconcileAbandonsNeverLaunchedSuccessorAndUnwedgesSlot(t *testing.T) {
	fx := abandonedLaunchGap(t)
	f := fx.f
	before, _ := os.ReadFile(filepath.Join(fx.dir, nativeReceiptName(2)))
	if err := ReconcileNativeWorkerRuntime(f.cfg, f.st, f.slot); err != nil {
		t.Fatal(err)
	}
	if fx.seals != 1 || fx.absence != 1 || f.spawned != 0 {
		t.Fatalf("seals=%d absence=%d spawned=%d", fx.seals, fx.absence, f.spawned)
	}
	if _, err := os.Lstat(filepath.Join(fx.dir, nativeReceiptName(2))); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("abandoned receipt still occupies generation 2", err)
	}
	archived, _ := os.ReadFile(filepath.Join(fx.dir, "launch-abandoned-g2-1.receipt.json"))
	if !reflect.DeepEqual(archived, before) {
		t.Fatal("receipt was not archived byte-for-byte")
	}
	archived, _ = os.ReadFile(filepath.Join(fx.dir, "launch-abandoned-g2-1.execution.json"))
	if !reflect.DeepEqual(archived, fx.proof) {
		t.Fatal("execution proof was not archived")
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
	if record.Generation != 2 || record.ParentRoleRunID != fx.parent.RoleRunID || record.NativeSessionID != fx.next.Request.NativeSessionID || record.ReceiptSHA256 != hex.EncodeToString(sum[:]) ||
		record.Outcome.PhysicalAttempts != 0 || !record.Outcome.NextGenerationAllowed || record.WorkerExecHold != "containment_forgejo_authorization_unverified" {
		t.Fatalf("abandonment record incomplete: %+v", record)
	}
	if _, err := os.Lstat(fx.scratch.ManifestPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("orphaned scratch lease was not released", err)
	}
	// Parent generation and projection history are untouched; only the hold is gone.
	parent, err := readNativeWorkerReceipt(fx.dir, 1)
	if err != nil || parent.Status != "launched" || parent.Outcome == nil {
		t.Fatal("parent generation changed", err)
	}
	sess := f.st.Sessions[f.slot]
	if sess.NativeRegistrationHold != "" || sess.WorkerGeneration != 1 || sess.NativeRoleRunID != fx.parent.RoleRunID || sess.RetryCount != 1 || sess.UnexpectedExitRetries != 1 || sess.Status != state.StatusFailed {
		t.Fatalf("projection altered beyond the hold: %+v", sess)
	}
	saved, err := state.Load(f.cfg.StateDir)
	if err != nil || saved.Sessions[f.slot].NativeRegistrationHold != "" || saved.Sessions[f.slot].RetryCount != 1 {
		t.Fatal("cleared hold not durable", err)
	}
	if pending, err := NativePendingSlots(f.cfg.StateDir, f.st.Sessions); err != nil || len(pending) != 0 {
		t.Fatal("archived launch still consumes fleet capacity", pending, err)
	}
	// The ordinary relaunch path now mints a fresh generation-2 identity.
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
}

func TestNativeRuntimeReconcileReplaysAbandonmentAfterCrashBeforeProjectionSave(t *testing.T) {
	fx := abandonedLaunchGap(t)
	f := fx.f
	if err := ReconcileNativeWorkerRuntime(f.cfg, f.st, f.slot); err != nil {
		t.Fatal(err)
	}
	// Simulate a crash between the archive rename and the state save.
	f.st.Sessions[f.slot].NativeRegistrationHold = "unresolved_launch"
	if err := ReconcileNativeWorkerRuntime(f.cfg, f.st, f.slot); err != nil || f.st.Sessions[f.slot].NativeRegistrationHold != "" || fx.seals != 1 {
		t.Fatal("archived abandonment was not replayed", err, fx.seals)
	}
}

func TestNativeRuntimeReconcileKeepsUnprovenSuccessorHeld(t *testing.T) {
	for _, mode := range []string{"no_hold", "lease_active", "pane_present", "attempts_recorded", "parent_mismatch", "pid_recorded", "terminal_marker", "claim_present", "authority_unavailable"} {
		t.Run(mode, func(t *testing.T) {
			fx := abandonedLaunchGap(t)
			f := fx.f
			want, uncertain := "", true
			switch mode {
			case "no_hold":
				f.st.Sessions[f.slot].NativeRegistrationHold = ""
				want = "native_generation_sealed"
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
			case "parent_mismatch":
				fx.next.ParentRoleRunID = uuid.NewString()
				if err := writeNativeWorkerReceipt(fx.dir, fx.next); err != nil {
					t.Fatal(err)
				}
				want = "native_identity_conflict"
			case "pid_recorded":
				fx.next.PID = 7
				if err := writeNativeWorkerReceipt(fx.dir, fx.next); err != nil {
					t.Fatal(err)
				}
				want = "native_launch_abandonment_unproven"
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
			}
			err := ReconcileNativeWorkerRuntime(f.cfg, f.st, f.slot)
			if err == nil {
				t.Fatal("unproven abandonment was reconciled")
			}
			if want != "" {
				expectNativeHold(t, err, want, uncertain)
			} else {
				var hold *aiexecution.Hold
				if !errors.As(err, &hold) || hold.Code != "containment_prelaunch_claim_conflict" {
					t.Fatalf("err=%v", err)
				}
			}
			if _, statErr := os.Lstat(filepath.Join(fx.dir, nativeReceiptName(2))); statErr != nil {
				t.Fatal("held receipt was archived or removed", statErr)
			}
			if _, statErr := os.Lstat(fx.scratch.ManifestPath); statErr != nil {
				t.Fatal("held scratch lease was released", statErr)
			}
			if f.st.Sessions[f.slot].NativeRegistrationHold != map[bool]string{true: "", false: "unresolved_launch"}[mode == "no_hold"] || f.spawned != 0 || f.stopped != 0 {
				t.Fatal("hold cleared or process touched without proof")
			}
			if mode != "attempts_recorded" && mode != "authority_unavailable" && fx.seals != 0 {
				t.Fatal("authority sealed before local absence was proven")
			}
		})
	}
}

func TestNativeWorkerExecHoldSurfacesOnlyBoundedHoldCode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "slot.log")
	if NativeWorkerExecHold(path) != "" || NativeWorkerExecHold("relative.log") != "" {
		t.Fatal("missing log produced a hold")
	}
	if err := os.WriteFile(path, []byte("noise\n[maestro] worker exec: AI execution held: first_code\nmore\n[maestro] worker exec: AI execution held: containment_forgejo_authorization_unverified extra words\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if got := NativeWorkerExecHold(path); got != "containment_forgejo_authorization_unverified" {
		t.Fatalf("got %q", got)
	}
	if err := os.WriteFile(path, []byte("AI execution held: Not-A-Code; rm -rf\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if got := NativeWorkerExecHold(path); got != "" {
		t.Fatalf("unsafe token surfaced: %q", got)
	}
}
