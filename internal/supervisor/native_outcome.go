package supervisor

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/befeast/maestro/internal/admissioncontrol"
	"github.com/befeast/maestro/internal/aiexecution"
	"github.com/befeast/maestro/internal/config"
	"github.com/google/uuid"
)

var sealNativeConsultation = func(client admissioncontrol.Client, request admissioncontrol.SealRequest) (admissioncontrol.NativeOutcome, error) {
	return client.SealNative(request)
}

func nativeHash(value any) string {
	body, _ := json.Marshal(value)
	hash := sha256.Sum256(body)
	return hex.EncodeToString(hash[:])
}
func nativeAuthorityPin(cfg *config.Config) string {
	r := cfg.Supervisor.NativeSessionRegistration
	if r == nil || r.AuthorityUID == nil {
		return ""
	}
	return nativeHash(struct {
		Socket string
		UID    uint32
	}{r.ControlSocket, *r.AuthorityUID})
}
func nativeInputDigest(cfg *config.Config, identity ConsultationIdentity, prompt string) string {
	return nativeHash(struct {
		Identity                       ConsultationIdentity
		Prompt, Model, Backend, Effort string
	}{identity, prompt, cfg.Supervisor.Model, cfg.Supervisor.Backend, cfg.Supervisor.Effort})
}
func hasNativeInvocations(receipt *ConsultationReceipt) bool {
	for _, inv := range receipt.Invocations {
		if inv.NativeSession != nil {
			return true
		}
	}
	return false
}

func hasNativeSession(receipt *ConsultationReceipt) bool {
	if hasNativeInvocations(receipt) {
		return true
	}
	if receipt.PlannedInvocation != nil && receipt.PlannedInvocation.NativeSession != nil {
		for _, candidate := range receipt.Candidates {
			if candidate.IntentID == receipt.PlannedInvocation.ID && candidate.Status == "launch_intent" {
				return true
			}
		}
	}
	return false
}

func validNativeInvocation(receipt *ConsultationReceipt, inv *InvocationReceipt) bool {
	native := inv.NativeSession
	if native == nil || !native.Request.Valid() || native.Request.NativeSessionID != inv.ID || uuid.Validate(inv.ID) != nil ||
		native.Request.ProjectID != receipt.Identity.ProjectID || native.Request.Role != receipt.Identity.Role || native.Acknowledgement == nil ||
		native.Acknowledgement.Binding != native.Request.Binding || native.Acknowledgement.Revoked ||
		native.Acknowledgement.RegistrationVersion <= 0 || native.Acknowledgement.RegistrationVersion > native.Request.ExpectedVersion ||
		!nativeDigest(native.AuthorityPin) || inv.EndedAt.IsZero() || inv.StartedAt.IsZero() || inv.EndedAt.Before(inv.StartedAt) {
		return false
	}
	switch inv.Status {
	case "succeeded", "failed", "timed_out", "cancelled", "output_limit", "local_output_unknown":
	default:
		return false
	}
	if inv.ProcessLease != nil && (inv.ProcessLease.Unit != "maestro-native-"+strings.ReplaceAll(inv.ID, "-", "")+".service" ||
		inv.ProcessLease.Manager != "system" || !inv.ProcessTerminationVerified || inv.ProcessTermination == nil ||
		inv.ProcessTerminationDigest != inv.ProcessTermination.Digest ||
		aiexecution.ValidateNativeProcessTermination(*inv.ProcessTermination, inv.ProcessLease.Profile, inv.ID, inv.ProcessLease.Unit) != nil ||
		inv.Status == "succeeded" && inv.ProcessTermination.LocalStatus != "succeeded") {
		return false
	}
	if native.OutcomeIntent != nil && (native.OutcomeIntent.Binding != native.Request.Binding || native.OutcomeIntent.RegistrationVersion != native.Acknowledgement.RegistrationVersion) {
		return false
	}
	if native.Outcome != nil && (native.OutcomeIntent == nil || admissioncontrol.ValidateNativeOutcome(*native.Outcome, *native.OutcomeIntent) != nil) {
		return false
	}
	return true
}

func nativeInvocationsAllowed(receipt *ConsultationReceipt) bool {
	if receipt.PlannedInvocation != nil || len(receipt.Invocations) == 0 || len(receipt.Invocations) > 32 {
		return false
	}
	seen := map[string]bool{}
	for i := range receipt.Invocations {
		inv := &receipt.Invocations[i]
		if inv.Number != i+1 || seen[inv.ID] || !validNativeInvocation(receipt, inv) || inv.NativeSession.Outcome == nil || !inv.NativeSession.Outcome.NextGenerationAllowed {
			return false
		}
		seen[inv.ID] = true
	}
	return true
}

func sealNativeInvocations(cfg *config.Config, receipt *ConsultationReceipt, persist func() error) error {
	client, err := registrationClient(cfg)
	if err != nil {
		return aiexecution.Held("native_outcome_unverified")
	}
	if receipt.PlannedInvocation != nil || len(receipt.Invocations) == 0 || len(receipt.Invocations) > 32 {
		return aiexecution.Held("native_outcome_unverified")
	}
	configured := cfg.Supervisor.NativeSessionRegistration
	seen := map[string]bool{}
	for i := range receipt.Invocations {
		inv := &receipt.Invocations[i]
		if !validNativeInvocation(receipt, inv) || inv.Number != i+1 || seen[inv.ID] || cfg.AIExecution.RequireVerifiedRoute && inv.ProcessLease == nil {
			return aiexecution.Held("native_outcome_unverified")
		}
		seen[inv.ID] = true
		if inv.ProcessLease != nil {
			if _, err := checkNativeTermination(*inv); err != nil {
				return err
			}
		}
		native := inv.NativeSession
		if native.AuthorityPin != nativeAuthorityPin(cfg) || native.Request.GatewayScope != configured.GatewayScope ||
			native.Request.FleetID != configured.FleetID || native.Request.RunID != configured.BudgetRunID {
			return aiexecution.Held("native_outcome_unverified")
		}
		if native.Outcome != nil && native.Outcome.NextGenerationAllowed {
			continue
		}
		if native.OutcomeIntent == nil {
			native.OutcomeIntent = &admissioncontrol.SealRequest{Binding: native.Request.Binding, RegistrationVersion: native.Acknowledgement.RegistrationVersion}
			if err := persist(); err != nil {
				return &ConsultationHold{Code: "receipt_persistence_failed"}
			}
		}
		outcome, err := sealNativeConsultation(client, *native.OutcomeIntent)
		if err != nil || admissioncontrol.ValidateNativeOutcome(outcome, *native.OutcomeIntent) != nil {
			return aiexecution.Held("native_outcome_unverified")
		}
		native.Outcome = &outcome
		if err := persist(); err != nil {
			return &ConsultationHold{Code: "receipt_persistence_failed"}
		}
	}
	if !nativeInvocationsAllowed(receipt) {
		return aiexecution.Held("native_outcome_unverified")
	}
	return nil
}

func (s *consultationStore) loadNativeOutput(identity ConsultationIdentity, inv InvocationReceipt) ([]byte, error) {
	hold := aiexecution.Held("native_output_unverified")
	cp := inv.OutputCheckpoint
	if cp == nil || cp.Filename != inv.ID+".output.json" || cp.Bytes < 0 || cp.Bytes > 4<<20 || cp.Complete != (inv.Status == "succeeded") || cp.Truncated != (inv.Status == "output_limit") {
		return nil, hold
	}
	record, err := s.readNativeOutputRecord(identity, inv)
	if err != nil || record.Status != inv.Status || record.SHA256 != cp.SHA256 || len(record.Data) != cp.Bytes || record.Complete != cp.Complete || record.Truncated != cp.Truncated {
		return nil, hold
	}
	return record.Data, nil
}

// A missing record is distinct from a corrupt record only for planned-launch
// recovery. The former can become explicit output loss after proven termination;
// the latter must never be replaced with newly invented evidence.
func (s *consultationStore) readNativeOutputRecord(identity ConsultationIdentity, inv InvocationReceipt) (*nativeOutputRecord, error) {
	hold := aiexecution.Held("native_output_unverified")
	if uuid.Validate(inv.ID) != nil || inv.NativeSession == nil {
		return nil, hold
	}
	path := filepath.Join(s.dir, inv.ID+".output.json")
	before, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if err != nil || !before.Mode().IsRegular() || before.Mode().Perm()&0077 != 0 || before.Size() > 6<<20 {
		return nil, hold
	}
	owner, ok := before.Sys().(*syscall.Stat_t)
	if !ok || owner.Uid != uint32(os.Geteuid()) {
		return nil, hold
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, hold
	}
	defer f.Close()
	after, err := f.Stat()
	if err != nil || !os.SameFile(before, after) {
		return nil, hold
	}
	body, err := io.ReadAll(io.LimitReader(f, (6<<20)+1))
	if err != nil || len(body) > 6<<20 {
		return nil, hold
	}
	var record nativeOutputRecord
	if aiexecution.DecodeStrict(body, &record) != nil {
		return nil, hold
	}
	canonical, _ := json.Marshal(record)
	sum := sha256.Sum256(record.Data)
	if !bytes.Equal(canonical, body) || record.Version != 1 || record.RoleRunID != identity.ID || record.InvocationID != inv.ID ||
		record.NativeSessionID != inv.NativeSession.Request.NativeSessionID || len(record.Data) > 4<<20 ||
		record.SHA256 != hex.EncodeToString(sum[:]) || record.Complete != (record.Status == "succeeded") || record.Truncated != (record.Status == "output_limit") {
		return nil, hold
	}
	return &record, nil
}

func readNativeConsultation(stateDir, roleRunID string) (*ConsultationReceipt, bool, error) {
	for _, name := range []string{"current.json", roleRunID + ".json"} {
		data, err := readAuxiliaryReceipt(stateDir, name)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, false, err
		}
		var receipt ConsultationReceipt
		if aiexecution.DecodeStrict(data, &receipt) != nil || receipt.SchemaVersion != 1 || uuid.Validate(receipt.Identity.ID) != nil {
			return nil, false, aiexecution.Held("native_receipt_invalid")
		}
		if receipt.Identity.ID == roleRunID {
			return &receipt, name == "current.json", nil
		}
	}
	return nil, false, os.ErrNotExist
}

func nativeLaunchMarker(store *consultationStore, receipt *ConsultationReceipt) (bool, error) {
	data, err := readAuxiliaryReceipt(filepath.Dir(store.dir), "launch.json")
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var marker struct {
		Identity ConsultationIdentity `json:"identity"`
		IntentID string               `json:"intent_id"`
	}
	if aiexecution.DecodeStrict(data, &marker) != nil || uuid.Validate(marker.Identity.ID) != nil || uuid.Validate(marker.IntentID) != nil {
		return false, aiexecution.Held("native_launch_identity_conflict")
	}
	if marker.Identity.ID != receipt.Identity.ID {
		return false, nil
	}
	if marker.Identity != receipt.Identity {
		return false, aiexecution.Held("native_launch_identity_conflict")
	}
	expectedIntent := ""
	if receipt.PlannedInvocation != nil {
		expectedIntent = receipt.PlannedInvocation.ID
	} else if len(receipt.Invocations) > 0 {
		expectedIntent = receipt.Invocations[len(receipt.Invocations)-1].ID
	}
	if marker.IntentID != expectedIntent {
		return false, aiexecution.Held("native_launch_identity_conflict")
	}
	found := false
	for _, candidate := range receipt.Candidates {
		if candidate.IntentID == marker.IntentID {
			found = true
		}
	}
	if !found || uuid.Validate(marker.IntentID) != nil {
		return false, aiexecution.Held("native_launch_identity_conflict")
	}
	return true, nil
}

// ReconcileNativeConsultation restores only the same role-run and input. It
// never acquires another permit, starts a process, or walks the fallback chain.
func ReconcileNativeConsultation(cfg *config.Config, identity ConsultationIdentity, prompt string) (ConsultationResult, error) {
	result, found, err := replayNativeConsultation(cfg, identity, prompt)
	if !found && err == nil {
		err = aiexecution.Held("native_receipt_missing")
	}
	return result, err
}

func replayNativeConsultation(cfg *config.Config, identity ConsultationIdentity, prompt string) (ConsultationResult, bool, error) {
	result := ConsultationResult{}
	if uuid.Validate(identity.ID) != nil || cfg == nil || identity.ProjectID != cfg.ProjectID || (identity.Role != "supervisor" && identity.Role != "reviewer") {
		return result, false, aiexecution.Held("native_receipt_invalid")
	}
	store, unlock, err := lockConsultationStoreMode(cfg.StateDir, true)
	if err != nil {
		return result, false, err
	}
	defer unlock()
	receipt, current, err := readNativeConsultation(cfg.StateDir, identity.ID)
	priorRun := false
	if errors.Is(err, os.ErrNotExist) {
		data, readErr := readAuxiliaryReceipt(cfg.StateDir, "current.json")
		if errors.Is(readErr, os.ErrNotExist) {
			return result, false, nil
		}
		if readErr != nil {
			return result, false, readErr
		}
		var prior ConsultationReceipt
		if aiexecution.DecodeStrict(data, &prior) != nil || prior.SchemaVersion != 1 || uuid.Validate(prior.Identity.ID) != nil {
			return result, false, aiexecution.Held("native_receipt_invalid")
		}
		if !hasNativeSession(&prior) {
			return result, false, nil
		}
		if prior.Identity.ProjectID != cfg.ProjectID || prior.Identity.Role != identity.Role {
			return result, false, aiexecution.Held("native_input_conflict")
		}
		if prior.NativeOutcomeComplete && nativeInvocationsAllowed(&prior) {
			marker, markerErr := nativeLaunchMarker(store, &prior)
			if markerErr != nil {
				return result, false, markerErr
			}
			if !marker {
				if err := releaseNativeAuxiliary(cfg, prior.Identity.ID); err != nil {
					return result, false, err
				}
				return result, false, nil
			}
		}
		receipt = &prior
		current = true
		priorRun = true
		err = nil
	}
	if err != nil {
		return result, false, err
	}
	result.Receipt = receipt
	if !hasNativeSession(receipt) {
		return result, false, nil
	}
	if !nativeDigest(receipt.InputDigest) || (!priorRun && (receipt.Identity != identity || receipt.InputDigest != nativeInputDigest(cfg, identity, prompt))) {
		return result, true, aiexecution.Held("native_input_conflict")
	}
	if len(receipt.Invocations) > 32 || len(receipt.Invocations) == 32 && receipt.PlannedInvocation != nil {
		return result, true, aiexecution.Held("native_outcome_unverified")
	}
	if _, err := readAuxiliaryReceipt(cfg.StateDir, "registration.json"); err == nil || !errors.Is(err, os.ErrNotExist) {
		return result, true, &ConsultationHold{Code: "unresolved_registration_intent"}
	}
	marker, err := nativeLaunchMarker(store, receipt)
	if err != nil {
		return result, true, err
	}
	persist := func() error {
		if !current {
			return aiexecution.Held("native_archive_incomplete")
		}
		return store.save(receipt)
	}
	if receipt.PlannedInvocation != nil {
		if !current || !marker {
			return result, true, aiexecution.Held("native_launch_receipt_missing")
		}
		if err := recoverPlannedNativeInvocation(store, receipt, persist); err != nil {
			return result, true, err
		}
	}
	for i := range receipt.Invocations {
		inv := &receipt.Invocations[i]
		if inv.ProcessLease != nil && (!inv.ProcessTerminationVerified || inv.ProcessTermination == nil || inv.ProcessTerminationDigest == "" || inv.Status == "containment_unresolved") {
			if !current || !marker || receipt.NativeOutcomeComplete {
				return result, true, aiexecution.Held("native_process_termination_unverified")
			}
			if err := recoverNativeInvocationOutput(store, receipt, inv); err != nil {
				return result, true, err
			}
			for j := range receipt.Candidates {
				if receipt.Candidates[j].IntentID == inv.ID {
					receipt.Candidates[j].Status = inv.Status
				}
			}
			if err := persist(); err != nil {
				return result, true, &ConsultationHold{Code: "receipt_persistence_failed"}
			}
		}
	}
	output := []byte(nil)
	for _, inv := range receipt.Invocations {
		if !validNativeInvocation(receipt, &inv) {
			return result, true, aiexecution.Held("native_outcome_unverified")
		}
		if inv.ProcessLease != nil && !receipt.NativeOutcomeComplete {
			if _, err := checkNativeTermination(inv); err != nil {
				return result, true, err
			}
		}
		saved, err := store.loadNativeOutput(receipt.Identity, inv)
		if err != nil {
			return result, true, err
		}
		output = saved
	}
	if !receipt.NativeOutcomeComplete {
		if !current || !marker {
			return result, true, aiexecution.Held("native_launch_receipt_missing")
		}
		if err := sealNativeInvocations(cfg, receipt, persist); err != nil {
			return result, true, err
		}
		receipt.NativeOutcomeComplete = true
		end := time.Now().UTC()
		receipt.EndedAt = &end
		receipt.Status = "failed"
		if receipt.Invocations[len(receipt.Invocations)-1].Status == "succeeded" {
			receipt.Status = "succeeded"
		}
		if err := persist(); err != nil {
			return result, true, &ConsultationHold{Code: "receipt_persistence_failed"}
		}
	}
	if !nativeInvocationsAllowed(receipt) {
		return result, true, aiexecution.Held("native_outcome_unverified")
	}
	if (receipt.Status == "succeeded") != (receipt.Invocations[len(receipt.Invocations)-1].Status == "succeeded") {
		return result, true, aiexecution.Held("native_output_unverified")
	}
	if marker {
		if err := store.finishLaunch(); err != nil {
			return result, true, &ConsultationHold{Code: "receipt_persistence_failed"}
		}
	}
	if err := releaseNativeAuxiliary(cfg, receipt.Identity.ID); err != nil {
		return result, true, err
	}
	if priorRun {
		return result, true, aiexecution.Held("native_prior_outcome_reconciled")
	}
	if receipt.Status != "succeeded" {
		return result, true, fmt.Errorf("native consultation completed with local failure")
	}
	result.Output = strings.TrimSpace(string(output))
	return result, true, nil
}

func releaseNativeAuxiliary(cfg *config.Config, identity string) error {
	if releaser, ok := cfg.RuntimeAuxiliaryLimiter.(interface{ ReconcileAuxiliary(string, string) error }); ok {
		return releaser.ReconcileAuxiliary(cfg.StateDir, identity)
	}
	if cfg.RuntimeAuxiliaryLimiter != nil {
		return aiexecution.Held("auxiliary_reconciliation_unavailable")
	}
	return NativeAuxiliaryOutcomeComplete(cfg.StateDir, identity)
}

// NativeAuxiliaryOutcomeComplete lets the controller independently validate
// durable financial proof before dropping a retained in-memory permit.
func NativeAuxiliaryOutcomeComplete(stateDir, roleRunID string) error {
	if uuid.Validate(roleRunID) != nil {
		return aiexecution.Held("native_receipt_invalid")
	}
	receipt, _, err := readNativeConsultation(stateDir, roleRunID)
	if err != nil {
		return err
	}
	if !receipt.NativeOutcomeComplete || !nativeDigest(receipt.InputDigest) ||
		(receipt.Identity.Role != "supervisor" && receipt.Identity.Role != "reviewer") || receipt.Identity.ProjectID == "" ||
		receipt.EndedAt == nil || (receipt.Status != "succeeded" && receipt.Status != "failed") || !nativeInvocationsAllowed(receipt) {
		return aiexecution.Held("native_outcome_unverified")
	}
	if (receipt.Status == "succeeded") != (receipt.Invocations[len(receipt.Invocations)-1].Status == "succeeded") {
		return aiexecution.Held("native_output_unverified")
	}
	store := &consultationStore{dir: filepath.Join(stateDir, "supervisor-consultations")}
	if marker, err := nativeLaunchMarker(store, receipt); err != nil || marker {
		return aiexecution.Held("native_outcome_unverified")
	}
	for _, inv := range receipt.Invocations {
		if _, err := store.loadNativeOutput(receipt.Identity, inv); err != nil {
			return err
		}
	}
	return nil
}
