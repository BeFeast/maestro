package supervisor

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/befeast/maestro/internal/admissioncontrol"
	"github.com/befeast/maestro/internal/aiexecution"
	"github.com/befeast/maestro/internal/config"
)

func plannedNativeFixture(t *testing.T) (*config.Config, string, *auxiliaryFixture, ConsultationIdentity, *nativeTermination) {
	t.Helper()
	cfg, count, aux := outcomeTestConfig(t)
	sealNativeConsultation = func(_ admissioncontrol.Client, request admissioncontrol.SealRequest) (admissioncontrol.NativeOutcome, error) {
		return nativeOutcomeFixture(request, true), nil
	}
	id := newConsultationIdentity(cfg, "crashed-cycle")
	_, _ = NewBackendLLMClient(cfg).(*backendLLMClient).CompleteConsultation(id, "prompt")
	receipt := loadReceipt(t, cfg)
	inv := receipt.Invocations[0]
	inv.NativeSession.Outcome, inv.NativeSession.OutcomeIntent = nil, nil
	inv.ProcessLease = &NativeInvocationProcessLease{Unit: "maestro-native-" + strings.ReplaceAll(inv.ID, "-", "") + ".service", Manager: "system", Profile: aiexecution.FileProof{Path: "/fixture/original-profile", SHA256: strings.Repeat("b", 64)}}
	proof := &nativeTermination{Profile: inv.ProcessLease.Profile, NativeSessionID: inv.ID, Unit: inv.ProcessLease.Unit, StartedAt: inv.StartedAt, EndedAt: inv.EndedAt, LocalStatus: "succeeded", Digest: strings.Repeat("c", 64)}
	inv.StartedAt, inv.EndedAt, inv.Status, inv.OutputCheckpoint = time.Time{}, time.Time{}, "", nil
	receipt.PlannedInvocation = &inv
	receipt.Invocations = nil
	for i := range receipt.Candidates {
		if receipt.Candidates[i].IntentID == inv.ID {
			receipt.Candidates[i].Status = "launch_intent"
		}
	}
	store := &consultationStore{dir: filepath.Join(cfg.StateDir, "supervisor-consultations")}
	if err := store.save(&receipt); err != nil {
		t.Fatal(err)
	}
	old := verifyNativeTermination
	t.Cleanup(func() { verifyNativeTermination = old })
	verifyNativeTermination = func(profile aiexecution.FileProof, nativeID, unit string) (*nativeTermination, error) {
		if profile != proof.Profile || nativeID != proof.NativeSessionID || unit != proof.Unit {
			t.Fatal("termination queried a different incarnation")
		}
		return proof, nil
	}
	return cfg, count, aux, id, proof
}

func TestNativePlannedCrashRestoresExactOrphanCheckpoint(t *testing.T) {
	cfg, count, aux, id, _ := plannedNativeFixture(t)
	seals := 0
	sealNativeConsultation = func(_ admissioncontrol.Client, request admissioncontrol.SealRequest) (admissioncontrol.NativeOutcome, error) {
		seals++
		saved := loadReceipt(t, cfg)
		if saved.PlannedInvocation != nil || len(saved.Invocations) != 1 || saved.Invocations[0].ProcessTerminationDigest == "" || saved.Invocations[0].OutputCheckpoint == nil || aux.released.Load() != 0 {
			t.Fatal("seal preceded recovered durable process/output evidence")
		}
		return nativeOutcomeFixture(request, false), nil
	}
	result, err := ReconcileNativeConsultation(cfg, id, "prompt")
	if err != nil || result.Output != "done" || seals != 1 || nativeCalls(t, count) != 1 || aux.released.Load() != 1 {
		t.Fatalf("recovery output=%q err=%v seals=%d", result.Output, err, seals)
	}
}

func TestNativePlannedCrashMissingOutputReleasesSettledOccupancyWithoutSuccess(t *testing.T) {
	cfg, count, aux, id, proof := plannedNativeFixture(t)
	proof.LocalStatus = "local_output_unknown"
	path := filepath.Join(cfg.StateDir, "supervisor-consultations", proof.NativeSessionID+".output.json")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	sealNativeConsultation = func(_ admissioncontrol.Client, request admissioncontrol.SealRequest) (admissioncontrol.NativeOutcome, error) {
		saved := loadReceipt(t, cfg)
		inv := saved.Invocations[0]
		if inv.Status != "local_output_unknown" || inv.OutputCheckpoint == nil || inv.OutputCheckpoint.Complete || aux.released.Load() != 0 {
			t.Fatal("output loss was not durable before seal")
		}
		return nativeOutcomeFixture(request, false), nil
	}
	result, err := ReconcileNativeConsultation(cfg, id, "prompt")
	if err == nil || result.Output != "" || nativeCalls(t, count) != 1 || aux.released.Load() != 1 || !result.Receipt.NativeOutcomeComplete || result.Receipt.Status != "failed" {
		t.Fatal("missing stdout became success or blocked financially settled occupancy", err)
	}
	if err := NativeAuxiliaryOutcomeComplete(cfg.StateDir, id.ID); err != nil {
		t.Fatal(err)
	}
}

func TestNativePlannedCrashCorruptionOrUnprovenTerminationCannotSeal(t *testing.T) {
	for _, mode := range []string{"corrupt_output", "no_kernel_proof", "foreign_proof", "unknown_exit_with_success", "wrong_marker"} {
		t.Run(mode, func(t *testing.T) {
			cfg, count, aux, id, proof := plannedNativeFixture(t)
			switch mode {
			case "corrupt_output":
				_ = os.WriteFile(filepath.Join(cfg.StateDir, "supervisor-consultations", proof.NativeSessionID+".output.json"), []byte("{}"), 0600)
			case "no_kernel_proof":
				verifyNativeTermination = func(aiexecution.FileProof, string, string) (*nativeTermination, error) {
					return nil, errors.New("unknown cgroup")
				}
			case "foreign_proof":
				proof.NativeSessionID = "different"
				verifyNativeTermination = func(aiexecution.FileProof, string, string) (*nativeTermination, error) { return proof, nil }
			case "unknown_exit_with_success":
				proof.LocalStatus = "local_output_unknown"
			case "wrong_marker":
				receipt := loadReceipt(t, cfg)
				receipt.Candidates[0].IntentID = id.ID
				_ = (&consultationStore{dir: filepath.Join(cfg.StateDir, "supervisor-consultations")}).save(&receipt)
			}
			sealNativeConsultation = func(admissioncontrol.Client, admissioncontrol.SealRequest) (admissioncontrol.NativeOutcome, error) {
				t.Fatal("unverified planned invocation reached financial seal")
				return admissioncontrol.NativeOutcome{}, nil
			}
			result, err := ReconcileNativeConsultation(cfg, id, "prompt")
			if err == nil || result.Output != "" || aux.released.Load() != 0 || nativeCalls(t, count) != 1 {
				t.Fatal("unproven recovery released", err)
			}
		})
	}
}

func TestNativePlannedCrashPersistenceFailureRetainsIntent(t *testing.T) {
	cfg, _, _, id, proof := plannedNativeFixture(t)
	_ = os.Remove(filepath.Join(cfg.StateDir, "supervisor-consultations", proof.NativeSessionID+".output.json"))
	store := &consultationStore{dir: filepath.Join(cfg.StateDir, "supervisor-consultations")}
	receipt := loadReceipt(t, cfg)
	err := recoverPlannedNativeInvocation(store, &receipt, func() error { return errors.New("fsync failed") })
	if err == nil || loadReceipt(t, cfg).PlannedInvocation == nil {
		t.Fatal("failed recovered receipt write lost launch intent")
	}
	if _, err := os.Stat(filepath.Join(store.dir, "launch.json")); err != nil {
		t.Fatal(err)
	}
	sealNativeConsultation = func(_ admissioncontrol.Client, request admissioncontrol.SealRequest) (admissioncontrol.NativeOutcome, error) {
		return nativeOutcomeFixture(request, false), nil
	}
	result, err := ReconcileNativeConsultation(cfg, id, "prompt")
	if err == nil || result.Output != "" || !result.Receipt.NativeOutcomeComplete {
		t.Fatal("durable loss checkpoint did not recover", err)
	}
}

func TestNativeKnownTerminationPreservesPartialTimeoutAndContainmentOutputAsFailure(t *testing.T) {
	for _, status := range []string{"timed_out", "containment_unresolved"} {
		t.Run(status, func(t *testing.T) {
			cfg, count, aux, id, proof := plannedNativeFixture(t)
			proof.LocalStatus = "failed"
			receipt := loadReceipt(t, cfg)
			inv := *receipt.PlannedInvocation
			inv.StartedAt, inv.EndedAt, inv.Status = proof.StartedAt, proof.EndedAt, status
			store := &consultationStore{dir: filepath.Join(cfg.StateDir, "supervisor-consultations")}
			checkpoint, err := store.saveNativeOutput(receipt.Identity, inv, []byte("partial prefix"))
			if err != nil {
				t.Fatal(err)
			}
			if status == "containment_unresolved" {
				inv.OutputCheckpoint = checkpoint
				receipt.Invocations = []InvocationReceipt{inv}
				receipt.PlannedInvocation = nil
				receipt.Candidates[0].Status = status
			}
			if err := store.save(&receipt); err != nil {
				t.Fatal(err)
			}
			sealNativeConsultation = func(_ admissioncontrol.Client, request admissioncontrol.SealRequest) (admissioncontrol.NativeOutcome, error) {
				return nativeOutcomeFixture(request, false), nil
			}
			result, err := ReconcileNativeConsultation(cfg, id, "prompt")
			if err == nil || result.Output != "" || !result.Receipt.NativeOutcomeComplete || aux.released.Load() != 1 || nativeCalls(t, count) != 1 {
				t.Fatal("partial output became success or failed to reconcile", err)
			}
			saved := result.Receipt.Invocations[0]
			out, err := store.loadNativeOutput(id, saved)
			if err != nil || string(out) != "partial prefix" || saved.OutputCheckpoint.Complete || status == "containment_unresolved" && saved.Status != "local_output_unknown" {
				t.Fatal("partial prefix was lost or upgraded", err)
			}
		})
	}
}
