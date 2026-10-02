package worker

import (
	"github.com/befeast/maestro/internal/admissioncontrol"
	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/pipeline"
	"github.com/befeast/maestro/internal/state"
)

var sealNativeWorker = func(client admissioncontrol.Client, request admissioncontrol.SealRequest) (admissioncontrol.NativeOutcome, error) {
	return client.SealNative(request)
}

func validateNativeWorkerOutcome(receipt *NativeWorkerReceipt) error {
	invalid := &NativeRegistrationHold{Code: "outcome_receipt_invalid", LaunchUncertain: true}
	if receipt.OutcomeIntent == nil {
		if receipt.Outcome != nil {
			return invalid
		}
		return nil
	}
	if receipt.Status != "launched" || receipt.Acknowledgement == nil ||
		receipt.OutcomeIntent.Binding != receipt.Request.Binding ||
		receipt.OutcomeIntent.RegistrationVersion != receipt.Acknowledgement.RegistrationVersion ||
		!receipt.OutcomeIntent.Valid() {
		return invalid
	}
	if receipt.Outcome != nil && admissioncontrol.ValidateNativeOutcome(*receipt.Outcome, *receipt.OutcomeIntent) != nil {
		return invalid
	}
	return nil
}

func persistedNativeGenerationOutcome(_ *config.Config, receipt *NativeWorkerReceipt) error {
	if validateNativeWorkerOutcome(receipt) != nil || receipt.Outcome == nil || !receipt.Outcome.NextGenerationAllowed {
		return &NativeRegistrationHold{Code: "previous_outcome_unknown"}
	}
	return nil
}

// NativeWorkerSealStarted repairs a lost session projection from the fsynced
// receipt. The intent may have committed remotely even when no reply survived.
func NativeWorkerSealStarted(stateDir, slot string, sess *state.Session) (bool, error) {
	if sess == nil || sess.NativeRoleRunID == "" {
		return false, nil
	}
	if sess.NativeReceiptDir != nativeReceiptDir(stateDir, slot) {
		return false, &NativeRegistrationHold{Code: "native_identity_conflict"}
	}
	receipt, err := readNativeWorkerReceipt(sess.NativeReceiptDir, sess.WorkerGeneration)
	if err != nil {
		return false, err
	}
	if receipt.Slot != slot || receipt.RoleRunID != sess.NativeRoleRunID || receipt.Request.NativeSessionID != sess.NativeSessionID {
		return false, &NativeRegistrationHold{Code: "native_identity_conflict"}
	}
	if receipt.OutcomeIntent == nil {
		return false, nil
	}
	if receipt.Outcome != nil && receipt.Outcome.NextGenerationAllowed {
		terminal, err := nativeWorkerTerminated(sess.NativeReceiptDir, receipt)
		if err != nil {
			return false, err
		}
		if terminal {
			return false, nil
		}
	}
	return true, nil
}

// ReconcileNativeWorkerOutcome seals only the exact saved native registration.
// It never launches, signals, removes a worktree, releases an OS lease, or
// invents a new identity. Late ledger evidence can replace a held snapshot;
// every result is fsynced before any next-generation authorization is returned.
func ReconcileNativeWorkerOutcome(cfg *config.Config, slot string, generation uint64) (*NativeWorkerReceipt, error) {
	if _, err := nativeClient(cfg); err != nil {
		return nil, err
	}
	dir, unlock, err := lockNativeWorker(cfg, slot)
	if err != nil {
		return nil, err
	}
	defer unlock()
	receipt, err := readNativeWorkerReceipt(dir, generation)
	if err != nil {
		return nil, err
	}
	if receipt.Slot != slot {
		return nil, &NativeRegistrationHold{Code: "native_identity_conflict"}
	}
	return reconcileNativeWorkerOutcomeLocked(cfg, dir, receipt)
}

func reconcileNativeWorkerOutcomeLocked(cfg *config.Config, dir string, receipt *NativeWorkerReceipt) (*NativeWorkerReceipt, error) {
	client, err := nativeClient(cfg)
	if err != nil {
		return nil, err
	}
	definition, ok := cfg.Model.Backends[receipt.Backend]
	if !ok {
		return nil, &NativeRegistrationHold{Code: "backend_unknown"}
	}
	backend := workerBackendConfig(definition)
	backend.TokenBudget = cfg.WorkerMaxTokens
	// The endpoint, caller scope, backend and project remain pinned. A policy
	// rollover can change the current expected version; sealing echoes the old
	// installed registration version and never re-registers against new policy.
	oldConfig := *cfg
	oldRegistration := *cfg.WorkerNativeSessionRegistration
	oldRegistration.ExpectedPolicyVersion = receipt.Request.ExpectedVersion
	oldConfig.WorkerNativeSessionRegistration = &oldRegistration
	matchedConfig := receipt.ConfigDigest == nativeConfigDigest(&oldConfig, receipt.Backend, backend, receipt.Request.Role)
	phase := map[string]state.Phase{"planner": state.PhasePlan, "advisor": state.PhaseAdvisor,
		"implementer": state.PhaseImplement, "validator": state.PhaseValidate}[receipt.Request.Role]
	if phase != state.PhaseNone {
		if effort := pipeline.EffortForPhase(&oldConfig, phase); effort != "" {
			backend.TierEffort = effort
			matchedConfig = matchedConfig || receipt.ConfigDigest == nativeConfigDigest(&oldConfig, receipt.Backend, backend, receipt.Request.Role)
		}
	}
	if receipt.Status != "launched" || receipt.ProjectID != cfg.ProjectID || receipt.Acknowledgement == nil ||
		dir != nativeReceiptDir(cfg.StateDir, receipt.Slot) ||
		!matchedConfig {
		return nil, &NativeRegistrationHold{Code: "native_identity_conflict"}
	}
	if err := validateNativeWorkerOutcome(receipt); err != nil {
		return nil, err
	}
	if receipt.Outcome != nil && receipt.Outcome.NextGenerationAllowed {
		return receipt, nil
	}
	if receipt.OutcomeIntent == nil {
		request := admissioncontrol.SealRequest{Binding: receipt.Request.Binding, RegistrationVersion: receipt.Acknowledgement.RegistrationVersion}
		receipt.OutcomeIntent = &request
		if err := persistNativeWorkerReceipt(dir, receipt); err != nil {
			return nil, &NativeRegistrationHold{Code: "outcome_persistence_failed"}
		}
	}
	outcome, err := sealNativeWorker(client, *receipt.OutcomeIntent)
	if err != nil {
		return nil, &NativeRegistrationHold{Code: "outcome_authority_unavailable"}
	}
	if admissioncontrol.ValidateNativeOutcome(outcome, *receipt.OutcomeIntent) != nil {
		return nil, &NativeRegistrationHold{Code: "outcome_response_invalid"}
	}
	receipt.Outcome = &outcome
	if err := persistNativeWorkerReceipt(dir, receipt); err != nil {
		return nil, &NativeRegistrationHold{Code: "outcome_persistence_failed"}
	}
	return receipt, nil
}
