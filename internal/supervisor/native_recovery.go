package supervisor

import (
	"encoding/hex"
	"errors"
	"os"
	"strings"
	"time"

	"github.com/befeast/maestro/internal/aiexecution"
)

type nativeTermination struct {
	Profile               aiexecution.FileProof
	NativeSessionID, Unit string
	StartedAt, EndedAt    time.Time
	LocalStatus, Digest   string
}

// No local exit, missing unit, saved bool, or authority financial outcome can
// substitute for the original containment verifier's kernel observation.
var verifyNativeTermination = func(profile aiexecution.FileProof, nativeID, unit string) (*nativeTermination, error) {
	proof, err := aiexecution.VerifyNativeProcessTermination(profile, nativeID, unit)
	if err != nil {
		return nil, err
	}
	return &nativeTermination{Profile: proof.Profile, NativeSessionID: proof.NativeSessionID, Unit: proof.Unit, StartedAt: proof.StartedAt, EndedAt: proof.EndedAt, LocalStatus: proof.LocalStatus, Digest: proof.Digest}, nil
}

func nativeDigest(value string) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == 32 && strings.ToLower(value) == value
}

func checkNativeTermination(inv InvocationReceipt) (*nativeTermination, error) {
	hold := aiexecution.Held("native_process_termination_unverified")
	lease := inv.ProcessLease
	if lease == nil || lease.Unit != "maestro-native-"+strings.ReplaceAll(inv.ID, "-", "")+".service" || lease.Manager != "system" || !nativeDigest(lease.Profile.SHA256) {
		return nil, hold
	}
	proof, err := verifyNativeTermination(lease.Profile, inv.ID, lease.Unit)
	if err != nil || proof == nil || proof.Profile != lease.Profile || proof.NativeSessionID != inv.ID || proof.Unit != lease.Unit ||
		proof.StartedAt.IsZero() || proof.EndedAt.Before(proof.StartedAt) || !nativeDigest(proof.Digest) ||
		(proof.LocalStatus != "succeeded" && proof.LocalStatus != "failed" && proof.LocalStatus != "local_output_unknown") ||
		(inv.ProcessTerminationDigest != "" && proof.Digest != inv.ProcessTerminationDigest) {
		return nil, hold
	}
	return proof, nil
}

func recoverPlannedNativeInvocation(store *consultationStore, receipt *ConsultationReceipt, persist func() error) error {
	hold := aiexecution.Held("native_outcome_unverified")
	if receipt.PlannedInvocation == nil || len(receipt.Invocations) >= 32 {
		return hold
	}
	inv := *receipt.PlannedInvocation
	if inv.Number != len(receipt.Invocations)+1 || inv.NativeSession == nil {
		return hold
	}
	candidateIndex := -1
	for i, c := range receipt.Candidates {
		if c.IntentID == inv.ID {
			if c.Status != "launch_intent" || candidateIndex != -1 {
				return hold
			}
			candidateIndex = i
		}
	}
	if candidateIndex == -1 {
		return hold
	}
	if err := recoverNativeInvocationOutput(store, receipt, &inv); err != nil {
		return err
	}
	receipt.Invocations = append(receipt.Invocations, inv)
	receipt.PlannedInvocation = nil
	receipt.Candidates[candidateIndex].Status = inv.Status
	if err := persist(); err != nil {
		return &ConsultationHold{Code: "receipt_persistence_failed"}
	}
	return nil
}

func recoverNativeInvocationOutput(store *consultationStore, receipt *ConsultationReceipt, inv *InvocationReceipt) error {
	proof, err := checkNativeTermination(*inv)
	if err != nil {
		return err
	}
	inv.StartedAt, inv.EndedAt = proof.StartedAt, proof.EndedAt
	inv.ProcessTerminationVerified = true
	inv.ProcessTerminationDigest = proof.Digest
	record, err := store.readNativeOutputRecord(receipt.Identity, *inv)
	if errors.Is(err, os.ErrNotExist) {
		if inv.OutputCheckpoint != nil {
			return aiexecution.Held("native_output_unverified")
		}
		// The monitor proves termination, not stdout completeness. Fsync an
		// explicit loss record before any seal/marker/permit release.
		inv.Status = "local_output_unknown"
		inv.OutputCheckpoint, err = store.saveNativeOutput(receipt.Identity, *inv, nil)
		if err != nil {
			return &ConsultationHold{Code: "receipt_persistence_failed"}
		}
	} else if err != nil {
		return err
	} else {
		if cp := inv.OutputCheckpoint; cp != nil && (cp.Filename != inv.ID+".output.json" || cp.Bytes != len(record.Data) || cp.SHA256 != record.SHA256 || cp.Complete != record.Complete || cp.Truncated != record.Truncated ||
			(record.Status != inv.Status && !(inv.Status == "containment_unresolved" && record.Status == "local_output_unknown"))) {
			return aiexecution.Held("native_output_unverified")
		}
		if record.Status == "succeeded" && proof.LocalStatus != "succeeded" {
			return aiexecution.Held("native_output_unverified")
		}
		inv.Status = record.Status
		inv.OutputCheckpoint = &NativeOutputCheckpoint{Filename: inv.ID + ".output.json", SHA256: record.SHA256, Bytes: len(record.Data), Complete: record.Complete, Truncated: record.Truncated}
		if inv.Status == "containment_unresolved" {
			inv.Status = "local_output_unknown"
			inv.OutputCheckpoint, err = store.saveNativeOutput(receipt.Identity, *inv, record.Data)
			if err != nil {
				return &ConsultationHold{Code: "receipt_persistence_failed"}
			}
		}
	}
	if !validNativeInvocation(receipt, inv) {
		return aiexecution.Held("native_outcome_unverified")
	}
	return nil
}
